package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const routingHeader = "X-Cpa-Limit-Request"

type sessionBinding struct {
	key     string
	authID  string
	expires time.Time
}

type routingCandidate struct {
	pluginapi.SchedulerAuthCandidate
	key, name       string
	identities      []string
	included        bool
	slots, attempts int
	weight          float64
	ready           bool
}

func routingHash(parts ...string) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func headerValue(headers map[string][]string, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

// Retain only a digest of the session and caller namespace, never payloads or keys.
func requestSession(req pluginapi.RequestInterceptRequest) string {
	session := metadataString(req.Metadata, "canonical_session_id")
	for _, name := range []string{"X-Claude-Code-Session-Id", "Session-Id", "Session_id", "X-Session-ID", "X-Session-Affinity", "X-Conversation-Id", "X-Thread-Id"} {
		if session == "" {
			session = headerValue(req.Headers, name)
		}
	}
	if session == "" {
		var body struct {
			SessionID      string `json:"session_id"`
			SessionCamel   string `json:"sessionId"`
			CacheKey       string `json:"prompt_cache_key"`
			ConversationID string `json:"conversation_id"`
			ThreadID       string `json:"thread_id"`
		}
		if json.Unmarshal(req.Body, &body) == nil {
			for _, value := range []string{body.SessionID, body.SessionCamel, body.CacheKey, body.ConversationID, body.ThreadID} {
				if session == "" {
					session = strings.TrimSpace(value)
				}
			}
		}
	}
	for _, key := range []string{"execution_session_id", "derived_session_id", "lcp_affinity_session_id"} {
		if session == "" {
			session = metadataString(req.Metadata, key)
		}
	}
	if session == "" {
		return ""
	}
	scope := metadataString(req.Metadata, "caller_scope")
	if scope == "" {
		scope = headerValue(req.Headers, "Authorization")
	}
	return routingHash(scope, session)
}

func (l *limiter) releasePending(req *requestState) {
	if req.pending == "" {
		return
	}
	if account := l.accounts[req.pending]; account != nil {
		account.pendingAttempts--
		if req.pendingSlot {
			account.pendingSlots--
		}
	}
	req.pending, req.pendingSlot = "", false
}

func (l *limiter) included(index, id, key string) bool {
	if l.cfg.includes(index, id) {
		return true
	}
	for _, selected := range l.cfg.AuthIndexes {
		if strings.HasPrefix(key, "group:") && l.cfg.AccountGroups[selected] == strings.TrimPrefix(key, "group:") {
			return true
		}
		if account := l.accounts[key]; account != nil {
			if _, ok := account.aliases["index:"+selected]; ok {
				return true
			}
			if _, ok := account.aliases["id:"+selected]; ok {
				return true
			}
		}
	}
	return false
}

func (l *limiter) pick(input pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	requestID := headerValue(input.Options.Headers, routingHeader)
	l.mu.Lock()
	active := l.requests[requestID] != nil && l.cfg.RoutingMode == routingCapacity && !l.quiescing
	l.mu.Unlock()
	if !active {
		return pluginapi.SchedulerPickResponse{}
	}
	// This is a local host-directory callback, cached with monitoring lookups.
	// Never hold the admission mutex while the host is running a callback.
	indexes := l.directory.lookupIndexes(l.now())
	l.mu.Lock()
	defer l.mu.Unlock()
	req := l.requests[requestID]
	if req == nil || l.quiescing || l.cfg.RoutingMode != routingCapacity {
		return pluginapi.SchedulerPickResponse{}
	}
	now := l.now()
	l.schedulerLastSeen = now.UTC()
	l.releasePending(req)
	l.cleanup(now)
	l.pruneBindings(now)
	var candidates []routingCandidate
	for _, candidate := range input.Candidates {
		if candidate.ID == "" {
			continue
		}
		weight := 1.0
		if parsed, err := strconv.ParseFloat(candidate.Attributes["weight"], 64); err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) {
			weight = parsed
		}
		if weight <= 0 {
			continue
		}
		key, name, identities, err := l.identity(indexes[candidate.ID], candidate.ID)
		if err != nil {
			continue
		}
		choice := routingCandidate{SchedulerAuthCandidate: candidate, key: key, name: name, identities: identities, included: l.included(indexes[candidate.ID], candidate.ID, key), weight: weight, ready: true}
		if indexes[candidate.ID] == "" && l.aliases["id:"+candidate.ID] == "" && len(l.cfg.AccountGroups) > 0 {
			// Without directory resolution, let the final guard learn the index
			// before creating a bucket that might belong to an explicit group.
			choice.included = false
		}
		if choice.included {
			policy := l.cfg.accountLimits(name)
			if account := l.accounts[key]; account != nil {
				account.history.prune(now)
				choice.slots, choice.attempts = account.active+account.pendingSlots, account.history.count()+account.pendingAttempts
			}
			_, reserved := req.accounts[key]
			choice.ready = (reserved || policy.MaxConcurrency == 0 || choice.slots < policy.MaxConcurrency) && (policy.RequestsPerMinute == 0 || choice.attempts < policy.RequestsPerMinute)
		}
		candidates = append(candidates, choice)
	}
	if len(candidates) == 0 {
		return pluginapi.SchedulerPickResponse{}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	// Rotate equal choices so a new session does not always start on the same ID.
	start := int(l.pickSequence % uint64(len(candidates)))
	l.pickSequence++
	sessionKey := ""
	if req.session != "" && l.cfg.SessionAffinityTTL > 0 {
		sessionKey = routingHash(input.Provider, input.Model, req.session)
	}
	bound := ""
	if entry := l.bindings[sessionKey]; entry != nil {
		bound = entry.Value.(sessionBinding).authID
	}
	selected := -1
	for n := range candidates {
		i := (start + n) % len(candidates)
		choice := candidates[i]
		if !choice.ready {
			continue
		}
		if selected < 0 || choice.Priority > candidates[selected].Priority {
			selected = i
			continue
		}
		best := candidates[selected]
		if choice.Priority != best.Priority || best.ID == bound {
			continue
		}
		load, bestLoad := float64(choice.slots)/choice.weight, float64(best.slots)/best.weight
		if choice.ID == bound || load < bestLoad || load == bestLoad && float64(choice.attempts)/choice.weight < float64(best.attempts)/best.weight {
			selected = i
		}
	}
	if selected < 0 {
		// Select a full candidate so the after-auth guard returns the precise 429
		// and Retry-After; do not turn pool exhaustion into a scheduler RPC error.
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: candidates[start].ID}
	}
	choice := candidates[selected]
	if choice.included {
		account := l.accounts[choice.key]
		if account == nil {
			if len(l.accounts) >= maxTrackedAccounts {
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: choice.ID}
			}
			account = &accountState{name: choice.name, aliases: make(map[string]struct{})}
			l.accounts[choice.key] = account
		}
		for _, identity := range choice.identities {
			l.aliases[identity] = choice.key
			account.aliases[identity] = struct{}{}
		}
		req.pending = choice.key
		_, reserved := req.accounts[choice.key]
		req.pendingSlot = !reserved
		account.pendingAttempts++
		if req.pendingSlot {
			account.pendingSlots++
		}
	}
	l.bindSession(sessionKey, choice.ID, now)
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: choice.ID}
}

func (l *limiter) pruneBindings(now time.Time) {
	for entry := l.bindingOrder.Back(); entry != nil; entry = l.bindingOrder.Back() {
		binding := entry.Value.(sessionBinding)
		if now.Before(binding.expires) {
			break
		}
		delete(l.bindings, binding.key)
		l.bindingOrder.Remove(entry)
	}
}

func (l *limiter) bindSession(key, authID string, now time.Time) {
	if key == "" {
		return
	}
	binding := sessionBinding{key: key, authID: authID, expires: now.Add(time.Duration(l.cfg.SessionAffinityTTL) * time.Second)}
	if entry := l.bindings[key]; entry != nil {
		entry.Value = binding
		l.bindingOrder.MoveToFront(entry)
		return
	}
	l.bindings[key] = l.bindingOrder.PushFront(binding)
	if len(l.bindings) > maxTrackedRequests {
		last := l.bindingOrder.Back()
		delete(l.bindings, last.Value.(sessionBinding).key)
		l.bindingOrder.Remove(last)
	}
}
