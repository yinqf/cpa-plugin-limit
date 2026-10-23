package main

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"sort"
	"sync"
	"time"
)

const (
	rateWindow           = time.Minute
	maxTrackedAccounts   = 4096
	maxTrackedRequests   = 16384
	maxCompletedRequests = 16384
)

// history retains the newest maxLimit attempts, including while RPM is disabled.
// Keeping that many is enough to enforce every supported limit after a reload.
type history struct {
	times []time.Time
	head  int
}

func (h *history) prune(now time.Time) {
	cutoff := now.Add(-rateWindow)
	for h.head < len(h.times) && !h.times[h.head].After(cutoff) {
		h.head++
	}
	if h.head == len(h.times) {
		h.times, h.head = nil, 0
	}
}

func (h *history) count() int { return len(h.times) - h.head }

func (h *history) add(now time.Time) {
	if h.count() == maxLimit {
		h.head++
	}
	if h.head > 0 && h.head >= len(h.times)/2 {
		h.times = h.times[:copy(h.times, h.times[h.head:])]
		h.head = 0
	}
	h.times = append(h.times, now)
}

func (h *history) retryAfter(now time.Time, limit int) time.Duration {
	if limit == 0 || h.count() < limit {
		return 0
	}
	// After lowering a limit, enough attempts must expire, not just the oldest.
	return h.times[len(h.times)-limit].Add(rateWindow).Sub(now)
}

type accountState struct {
	name                string
	aliases             map[string]struct{}
	history             history
	active              int
	admitted            uint64
	rateRejected        uint64
	concurrencyRejected uint64
}

type requestState struct {
	accounts map[string]struct{}
}

type decision struct {
	status     int
	code       string
	message    string
	retryAfter time.Duration
}

func unavailable(code, message string) decision {
	return decision{status: 503, code: code, message: message}
}

type limiter struct {
	mu              sync.Mutex
	now             func() time.Time
	cfg             config
	quiescing       bool
	accounts        map[string]*accountState
	aliases         map[string]string
	requests        map[string]*requestState
	completed       map[string]*list.Element
	completionOrder list.List
	nextCleanup     time.Time
	admitted        uint64
	rejected        uint64
	rateRejected    uint64
	busyRejected    uint64
	directory       emailDirectory
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{
		now: now, cfg: defaultConfig(), accounts: make(map[string]*accountState),
		aliases: make(map[string]string), requests: make(map[string]*requestState),
		completed: make(map[string]*list.Element),
	}
}

func (l *limiter) configure(cfg config) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanup(l.now())
	if !maps.Equal(cfg.AccountGroups, l.cfg.AccountGroups) && len(l.accounts) > 0 {
		return fmt.Errorf("account_groups cannot change while accounts have active requests or attempts in the last 60 seconds; drain traffic before changing identity groups")
	}
	l.cfg = cfg
	return nil
}

func (l *limiter) begin(id string) decision {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.quiescing {
		return unavailable("limiter_stopping", "The account limiter is stopping; retry on an active instance.")
	}
	if id == "" {
		return unavailable("limiter_missing_request_id", "The host did not provide a request ID required for account admission.")
	}
	if l.completed[id] != nil {
		return unavailable("limiter_request_completed", "The request has already completed.")
	}
	if l.requests[id] != nil {
		return decision{}
	}
	if len(l.requests) >= maxTrackedRequests {
		return unavailable("limiter_capacity", "The account limiter has reached its request tracking capacity.")
	}
	l.requests[id] = &requestState{accounts: make(map[string]struct{})}
	return decision{}
}

func (l *limiter) admit(requestID, index, id string) decision {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.quiescing {
		return unavailable("limiter_stopping", "The account limiter is stopping; retry on an active instance.")
	}
	req := l.requests[requestID]
	if req == nil {
		return unavailable("limiter_request_inactive", "The host did not begin this request, or it has already completed.")
	}
	if index == "" && id == "" {
		return unavailable("limiter_missing_account", "The host did not provide the selected upstream credential identity.")
	}
	now := l.now()
	if !now.Before(l.nextCleanup) {
		l.cleanup(now)
	}
	key, name, identities, err := l.identity(index, id)
	if err != nil {
		return unavailable("limiter_identity_conflict", "The host linked conflicting credential identities; drain these credentials before retrying.")
	}
	account := l.accounts[key]
	included := l.cfg.includes(index, id)
	if !included && len(key) > 6 && key[:6] == "group:" {
		for _, selected := range l.cfg.AuthIndexes {
			included = included || l.cfg.AccountGroups[selected] == name
		}
	}
	if !included && account != nil {
		for _, selected := range l.cfg.AuthIndexes {
			_, byIndex := account.aliases["index:"+selected]
			_, byID := account.aliases["id:"+selected]
			included = included || byIndex || byID
		}
	}
	if !included {
		return decision{}
	}
	if account == nil {
		if len(l.accounts) >= maxTrackedAccounts {
			return unavailable("limiter_capacity", "The account limiter has reached its account tracking capacity.")
		}
		account = &accountState{name: name, aliases: make(map[string]struct{})}
		l.accounts[key] = account
	}
	for _, identity := range identities {
		l.aliases[identity] = key
		account.aliases[identity] = struct{}{}
	}
	account.history.prune(now)
	policy := l.cfg.accountLimits(account.name)
	_, reserved := req.accounts[key]
	if !reserved && policy.MaxConcurrency > 0 && account.active >= policy.MaxConcurrency {
		account.concurrencyRejected++
		l.rejected++
		l.busyRejected++
		return decision{status: 429, code: "account_concurrency_exceeded", message: "The selected upstream account is busy; retry without changing the session.", retryAfter: time.Second}
	}
	if wait := account.history.retryAfter(now, policy.RequestsPerMinute); wait > 0 {
		account.rateRejected++
		l.rejected++
		l.rateRejected++
		return decision{status: 429, code: "account_rate_limit_exceeded", message: "The selected upstream account has reached its rolling 60-second request limit.", retryAfter: wait}
	}
	account.history.add(now)
	account.admitted++
	l.admitted++
	if !reserved {
		req.accounts[key] = struct{}{}
		account.active++
	}
	return decision{}
}

// Prefer a stable index, but remember both identities so fallback-ID requests
// cannot open a second bucket after this credential has been identified.
func (l *limiter) identity(index, id string) (key, name string, identities []string, err error) {
	if index != "" {
		identities = append(identities, "index:"+index)
		key, name = "index:"+index, index
	}
	if id != "" {
		identities = append(identities, "id:"+id)
		if key == "" {
			key, name = "id:"+id, id
		}
	}
	group := l.cfg.AccountGroups[index]
	if other := l.cfg.AccountGroups[id]; group != "" && other != "" && other != group {
		return "", "", nil, fmt.Errorf("conflicting account groups")
	}
	if group == "" {
		group = l.cfg.AccountGroups[id]
	}
	if group != "" {
		key, name = "group:"+group, group
	}
	existingKey := ""
	for _, identity := range identities {
		if existing, ok := l.aliases[identity]; ok {
			if existingKey != "" && existingKey != existing || group != "" && key != existing {
				return "", "", nil, fmt.Errorf("conflicting credential aliases")
			}
			existingKey = existing
		}
	}
	if existingKey != "" {
		return existingKey, l.accounts[existingKey].name, identities, nil
	}
	return key, name, identities, nil
}

func (l *limiter) complete(id string) {
	if id == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if req := l.requests[id]; req != nil {
		for key := range req.accounts {
			l.accounts[key].active--
		}
		delete(l.requests, id)
	}
	if l.completed[id] == nil {
		l.completed[id] = l.completionOrder.PushFront(id)
		if len(l.completed) > maxCompletedRequests {
			last := l.completionOrder.Back()
			delete(l.completed, last.Value.(string))
			l.completionOrder.Remove(last)
		}
	}
}

func (l *limiter) cleanup(now time.Time) {
	for key, account := range l.accounts {
		account.history.prune(now)
		if account.active == 0 && account.history.count() == 0 {
			for identity := range account.aliases {
				delete(l.aliases, identity)
			}
			delete(l.accounts, key)
		}
	}
	l.nextCleanup = now.Add(rateWindow)
}

func (l *limiter) quiesce() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.quiescing = true
}

type accountSnapshot struct {
	Account             string   `json:"account"`
	Label               string   `json:"label,omitempty"`
	Active              int      `json:"active"`
	WindowRequests      int      `json:"window_requests"`
	RequestsPerMinute   int      `json:"requests_per_minute"`
	MaxConcurrency      int      `json:"max_concurrency"`
	RateRetryAfter      int      `json:"rate_retry_after_seconds"`
	Admitted            uint64   `json:"admitted"`
	RateRejected        uint64   `json:"rate_rejected"`
	ConcurrencyRejected uint64   `json:"concurrency_rejected"`
	identities          []string `json:"-"`
}

type snapshot struct {
	Version        string            `json:"version"`
	ObservedAt     time.Time         `json:"observed_at"`
	Quiescing      bool              `json:"quiescing"`
	ActiveRequests int               `json:"active_requests"`
	Admitted       uint64            `json:"admitted"`
	Rejected       uint64            `json:"rejected"`
	RateRejected   uint64            `json:"rate_rejected"`
	BusyRejected   uint64            `json:"concurrency_rejected"`
	ActiveSlots    int               `json:"active_slots"`
	WindowRequests int               `json:"window_requests"`
	Defaults       limits            `json:"defaults"`
	Scope          scopeSnapshot     `json:"scope"`
	AccountDisplay string            `json:"account_display"`
	EmailAvailable bool              `json:"email_lookup_available"`
	Items          []accountSnapshot `json:"items"`
}

type scopeSnapshot struct {
	AllCredentials bool `json:"all_credentials"`
	SelectedCount  int  `json:"selected_count"`
	GroupCount     int  `json:"group_count"`
	OverrideCount  int  `json:"override_count"`
}

func (l *limiter) snapshot() snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.cleanup(now)
	groups := make(map[string]struct{})
	for _, group := range l.cfg.AccountGroups {
		groups[group] = struct{}{}
	}
	result := snapshot{
		Version: pluginVersion, ObservedAt: now.UTC(), Quiescing: l.quiescing,
		ActiveRequests: len(l.requests), Admitted: l.admitted, Rejected: l.rejected,
		RateRejected: l.rateRejected, BusyRejected: l.busyRejected, Defaults: l.cfg.limits,
		AccountDisplay: l.cfg.AccountDisplay,
		Scope:          scopeSnapshot{AllCredentials: len(l.cfg.AuthIndexes) == 0, SelectedCount: len(l.cfg.AuthIndexes), GroupCount: len(groups), OverrideCount: len(l.cfg.AccountLimits)},
		Items:          make([]accountSnapshot, 0, len(l.accounts)),
	}
	for key, account := range l.accounts {
		result.ActiveSlots += account.active
		result.WindowRequests += account.history.count()
		policy := l.cfg.accountLimits(account.name)
		digest := sha256.Sum256([]byte(key))
		result.Items = append(result.Items, accountSnapshot{
			Account: hex.EncodeToString(digest[:8]), Active: account.active,
			WindowRequests: account.history.count(), RequestsPerMinute: policy.RequestsPerMinute, MaxConcurrency: policy.MaxConcurrency,
			RateRetryAfter: retrySeconds(account.history.retryAfter(now, policy.RequestsPerMinute)),
			Admitted:       account.admitted, RateRejected: account.rateRejected, ConcurrencyRejected: account.concurrencyRejected,
		})
		for identity := range account.aliases {
			result.Items[len(result.Items)-1].identities = append(result.Items[len(result.Items)-1].identities, identity)
		}
	}
	sort.Slice(result.Items, func(i, j int) bool { return result.Items[i].Account < result.Items[j].Account })
	return result
}

func retrySeconds(wait time.Duration) int {
	if wait <= 0 {
		return 0
	}
	return int((wait + time.Second - 1) / time.Second)
}
