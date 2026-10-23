package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func startRouting(t *testing.T, l *limiter, id, session string) pluginapi.SchedulerPickRequest {
	t.Helper()
	before := call[pluginapi.RequestInterceptResponse](t, l, pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{RequestID: id, Headers: http.Header{"Session-Id": {session}, "Authorization": {"Bearer dummy-caller"}}, Body: []byte(`{"input":[]}`)})
	return pluginapi.SchedulerPickRequest{Provider: "codex", Model: "model", Options: pluginapi.SchedulerOptions{Headers: before.Headers}, Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a"}, {ID: "b"}}}
}

func TestHotSessionStaysStickyUntilFullThenUsesOtherAccount(t *testing.T) {
	l, _ := testLimiter()
	cfg := defaultConfig()
	cfg.RequestsPerMinute, cfg.MaxConcurrency = 2, 0
	configure(t, l, cfg)
	var selections []string
	for i := 0; i < 5; i++ {
		id := fmt.Sprint(i)
		input := startRouting(t, l, id, "private-hot-session")
		picked := call[pluginapi.SchedulerPickResponse](t, l, pluginabi.MethodSchedulerPick, input)
		if !picked.Handled {
			t.Fatal("scheduler did not handle a tracked request")
		}
		result := l.admit(id, "", picked.AuthID)
		if i < 4 {
			if result.status != 0 {
				t.Fatalf("rejected with another account available: %+v", result)
			}
			selections = append(selections, picked.AuthID)
		} else if result.status != 429 {
			t.Fatalf("full pool did not return 429: %+v", result)
		}
		l.complete(id)
	}
	if strings.Join(selections, ",") != "a,a,b,b" {
		t.Fatalf("hot session stayed on a full account or lost affinity early: %v", selections)
	}
	snap := l.snapshot()
	if snap.Admitted != 4 || snap.Rejected != 1 || snap.PendingAttempts != 0 || snap.ActiveSlots != 0 || snap.SchedulerLastSeen == nil {
		t.Fatalf("incorrect routed accounting: %+v", snap)
	}
}

func TestConcurrentPicksReserveCapacityBeforeAfterAuth(t *testing.T) {
	l, _ := testLimiter()
	cfg := defaultConfig()
	cfg.RequestsPerMinute, cfg.MaxConcurrency = 0, 2
	configure(t, l, cfg)
	const count = 80
	inputs := make([]pluginapi.SchedulerPickRequest, count)
	selected := make([]string, count)
	for i := range inputs {
		inputs[i] = startRouting(t, l, fmt.Sprint(i), "same-session")
	}
	var group sync.WaitGroup
	for i := range inputs {
		group.Add(1)
		go func() { defer group.Done(); selected[i] = l.pick(inputs[i]).AuthID }()
	}
	group.Wait()
	if snap := l.snapshot(); snap.PendingAttempts != 4 || snap.ActiveSlots != 4 {
		t.Fatalf("parallel selection overbooked or failed to use both accounts: %+v", snap)
	}
	admitted := make(chan string, count)
	for i := range inputs {
		group.Add(1)
		go func() {
			defer group.Done()
			result := l.admit(fmt.Sprint(i), "", selected[i])
			if result.status == 0 {
				admitted <- selected[i]
			} else if result.status != 429 {
				t.Errorf("unexpected result: %+v", result)
			}
		}()
	}
	group.Wait()
	close(admitted)
	counts := map[string]int{}
	for id := range admitted {
		counts[id]++
	}
	if counts["a"] != 2 || counts["b"] != 2 {
		t.Fatalf("requests concentrated on one account: %v", counts)
	}
	for i := range inputs {
		l.complete(fmt.Sprint(i))
	}
	if snap := l.snapshot(); snap.PendingAttempts != 0 || snap.ActiveSlots != 0 || snap.Admitted != 4 {
		t.Fatalf("reserved capacity leaked: %+v", snap)
	}
}

func TestPendingRPMReservationsAlsoPreventOverbooking(t *testing.T) {
	l, _ := testLimiter()
	cfg := defaultConfig()
	cfg.MaxConcurrency, cfg.RequestsPerMinute = 0, 1
	configure(t, l, cfg)
	first := l.pick(startRouting(t, l, "first", "hot"))
	second := l.pick(startRouting(t, l, "second", "hot"))
	if first.AuthID == second.AuthID {
		t.Fatal("pending RPM quota was ignored")
	}
	l.complete("first")
	if result := l.admit("second", "", second.AuthID); result.status != 0 {
		t.Fatal(result)
	}
	if snap := l.snapshot(); snap.Admitted != 1 || snap.PendingAttempts != 0 {
		t.Fatalf("unexecuted picks consumed RPM: %+v", snap)
	}
}

func TestSchedulerRespectsIndexGroupsScopesAndPendingCancellation(t *testing.T) {
	l, _ := testLimiter()
	cfg := defaultConfig()
	cfg.AuthIndexes = []string{"index-a", "index-c"}
	cfg.AccountGroups = map[string]string{"index-a": "shared", "index-b": "shared"}
	configure(t, l, cfg)
	l.directory.setLoader(func() ([]credentialEmail, error) {
		return []credentialEmail{{ID: "a", AuthIndex: "index-a"}, {ID: "b", AuthIndex: "index-b"}, {ID: "c", AuthIndex: "index-c"}}, nil
	})
	firstInput := startRouting(t, l, "first", "hot")
	first := l.pick(firstInput)
	secondInput := startRouting(t, l, "second", "hot")
	secondInput.Candidates = append(secondInput.Candidates, pluginapi.SchedulerAuthCandidate{ID: "c"})
	second := l.pick(secondInput)
	if first.AuthID != "a" || second.AuthID != "c" {
		t.Fatalf("duplicate credentials escaped their shared reservation: %s %s", first.AuthID, second.AuthID)
	}
	l.complete("first")
	l.complete("first")
	if result := l.admit("second", "index-c", "c"); result.status != 0 {
		t.Fatal(result)
	}
	l.complete("second")
	if snap := l.snapshot(); snap.ActiveSlots != 0 || snap.PendingAttempts != 0 || snap.Admitted != 1 {
		t.Fatalf("cancellation leaked pending slots or counted an unexecuted pick: %+v", snap)
	}
}

func TestSchedulerPriorityEligibilityAffinityExpiryAndPrivacy(t *testing.T) {
	l, now := testLimiter()
	cfg := defaultConfig()
	cfg.MaxConcurrency = 0
	cfg.SessionAffinityTTL = 2
	configure(t, l, cfg)
	first := l.pick(startRouting(t, l, "first", "private-session"))
	l.admit("first", "", first.AuthID)
	l.complete("first")
	second := l.pick(startRouting(t, l, "second", "private-session"))
	if second.AuthID != first.AuthID {
		t.Fatal("affinity did not survive a completed request")
	}
	l.complete("second")
	*now = now.Add(2 * time.Second)
	third := l.pick(startRouting(t, l, "third", "private-session"))
	if third.AuthID != "b" {
		t.Fatal("expired affinity remained pinned")
	}
	l.complete("third")
	priority := startRouting(t, l, "priority", "other")
	priority.Candidates = []pluginapi.SchedulerAuthCandidate{{ID: "a", Priority: 10}, {ID: "b", Priority: 0}, {ID: "disabled", Priority: 100, Attributes: map[string]string{"weight": "0"}}}
	if picked := l.pick(priority); picked.AuthID != "a" {
		t.Fatalf("priority or zero-weight exclusion was ignored: %+v", picked)
	}
	l.complete("priority")
	pinned := startRouting(t, l, "pinned", "private-session")
	pinned.Candidates = []pluginapi.SchedulerAuthCandidate{{ID: "a"}}
	if picked := l.pick(pinned); picked.AuthID != "a" {
		t.Fatal("picked outside the host's candidate set")
	}
	l.complete("pinned")
	raw, _ := json.Marshal(l.snapshot())
	if strings.Contains(string(raw), "private-session") || strings.Contains(string(raw), "dummy-caller") {
		t.Fatal("monitor leaked affinity identities")
	}
	*now = now.Add(2 * time.Second)
	if l.snapshot().AffinitySessions != 0 {
		t.Fatal("expired bindings retained indefinitely")
	}
}

func TestGuardModeDelegatesAndUsesNoPrivateHeader(t *testing.T) {
	l, _ := testLimiter()
	reg := call[registration](t, l, pluginabi.MethodPluginRegister, registerRequest{SchemaVersion: 6, ConfigYAML: []byte("routing_mode: guard-only")})
	if reg.Capabilities["scheduler"] {
		t.Fatal("guard-only mode still claims the scheduler")
	}
	input := startRouting(t, l, "guard", "session")
	if len(input.Options.Headers) > 0 || l.pick(input).Handled {
		t.Fatal("guard-only mode changed routing")
	}
}

func TestRepeatedSelectionReleasesUnexecutedReservation(t *testing.T) {
	l, _ := testLimiter()
	input := startRouting(t, l, "request", "session")
	l.pick(input)
	input.Candidates = []pluginapi.SchedulerAuthCandidate{{ID: "b"}}
	if picked := l.pick(input); picked.AuthID != "b" {
		t.Fatal("retry did not move to a remaining candidate")
	}
	if snap := l.snapshot(); snap.PendingAttempts != 1 || snap.ActiveSlots != 1 {
		t.Fatal("unexecuted selection leaked capacity")
	}
	l.complete("request")
	if snap := l.snapshot(); snap.PendingAttempts != 0 || snap.ActiveSlots != 0 || len(snap.Items) != 0 || len(l.totals) != 0 {
		t.Fatal("completion leaked a pending reservation or created historical counters")
	}
}

func TestMissingDirectoryPreservesGroupAndIndexOverrideGuard(t *testing.T) {
	for _, grouped := range []bool{false, true} {
		l, _ := testLimiter()
		cfg := defaultConfig()
		one := 1
		name := "index-a"
		if grouped {
			cfg.AccountGroups = map[string]string{"index-a": "shared"}
			name = "shared"
		}
		cfg.AccountLimits = map[string]limitOverride{name: {RequestsPerMinute: &one}}
		configure(t, l, cfg)
		input := startRouting(t, l, "first", "session")
		picked := l.pick(input)
		if result := l.admit("first", "index-a", picked.AuthID); result.status != 0 {
			t.Fatalf("directory fallback broke the guard: %+v", result)
		}
		l.complete("first")
		input = startRouting(t, l, "next", "session")
		if picked := l.pick(input); picked.AuthID != "b" {
			t.Fatal("index override was lost when the initial pick knew only the ID")
		}
		l.complete("next")
	}
}

func TestAffinityNamespacesAndBoundedCache(t *testing.T) {
	base := pluginapi.RequestInterceptRequest{Headers: http.Header{"Session-Id": {"same-session"}, "Authorization": {"Bearer dummy-a"}}}
	first := requestSession(base)
	base.Headers.Set("Authorization", "Bearer dummy-b")
	if requestSession(base) == first {
		t.Fatal("different callers shared an affinity namespace")
	}
	base.Headers.Del("Session-Id")
	if requestSession(base) != "" {
		t.Fatal("a caller key alone was mistaken for a session")
	}
	l, now := testLimiter()
	for i := 0; i <= maxTrackedRequests; i++ {
		l.bindSession(fmt.Sprint(i), "a", *now)
	}
	if len(l.bindings) != maxTrackedRequests || l.bindings["0"] != nil {
		t.Fatal("affinity cache exceeded its bound")
	}
	*now = now.Add(time.Duration(l.cfg.SessionAffinityTTL) * time.Second)
	l.pruneBindings(*now)
	if len(l.bindings) != 0 {
		t.Fatal("expired affinity entries retained")
	}
}
