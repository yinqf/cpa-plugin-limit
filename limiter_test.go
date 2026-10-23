package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLimiter() (*limiter, *time.Time) {
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	return newLimiter(func() time.Time { return now }), &now
}

func begin(t *testing.T, l *limiter, request string) {
	t.Helper()
	if result := l.begin(request); result.status != 0 {
		t.Fatalf("begin %s: %+v", request, result)
	}
}

func admit(t *testing.T, l *limiter, request, account string) {
	t.Helper()
	begin(t, l, request)
	if result := l.admit(request, account, ""); result.status != 0 {
		t.Fatalf("admit %s/%s: %+v", request, account, result)
	}
}

func configure(t *testing.T, l *limiter, cfg config) {
	t.Helper()
	if err := l.configure(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestRollingWindowAndRejectedRequestsDoNotConsumeQuota(t *testing.T) {
	l, now := testLimiter()
	for i := 0; i < 5; i++ {
		id := fmt.Sprint(i)
		admit(t, l, id, "account-a")
		l.complete(id)
		*now = now.Add(10 * time.Second)
	}
	begin(t, l, "sixth")
	result := l.admit("sixth", "account-a", "")
	if result.status != 429 || result.code != "account_rate_limit_exceeded" || result.retryAfter != 10*time.Second {
		t.Fatalf("expected rolling-window rejection: %+v", result)
	}
	if l.snapshot().Items[0].WindowRequests != 5 {
		t.Fatal("rejected attempt consumed quota")
	}
	*now = now.Add(10*time.Second - time.Nanosecond)
	if result := l.admit("sixth", "account-a", ""); result.status != 429 || retrySeconds(result.retryAfter) != 1 {
		t.Fatalf("premature expiry or rounded-down retry: %+v", result)
	}
	*now = now.Add(time.Nanosecond)
	if result := l.admit("sixth", "account-a", ""); result.status != 0 {
		t.Fatalf("attempt at exactly 60 seconds should pass: %+v", result)
	}
	l.complete("sixth")
	begin(t, l, "seventh")
	if result := l.admit("seventh", "account-a", ""); result.status != 429 || result.retryAfter != 10*time.Second {
		t.Fatalf("window reset instead of sliding: %+v", result)
	}
}

func TestConcurrencySharedAcrossRequestsAndIsolatedAcrossAccounts(t *testing.T) {
	l, _ := testLimiter()
	admit(t, l, "client-a/session-a/model-a", "account-a")
	begin(t, l, "client-b/session-b/model-b")
	result := l.admit("client-b/session-b/model-b", "account-a", "")
	if result.code != "account_concurrency_exceeded" || result.status != 429 {
		t.Fatalf("requests did not share account capacity: %+v", result)
	}
	admit(t, l, "client-c", "account-b")
	l.complete("client-a/session-a/model-a")
	if result := l.admit("client-b/session-b/model-b", "account-a", ""); result.status != 0 {
		t.Fatalf("completion did not release capacity: %+v", result)
	}
	for _, row := range l.snapshot().Items {
		if row.Active != 1 {
			t.Fatalf("unexpected account concurrency: %+v", row)
		}
	}
}

func TestConcurrentAdmissionsAreAtomic(t *testing.T) {
	for _, dimension := range []string{"concurrency", "rpm"} {
		t.Run(dimension, func(t *testing.T) {
			l, _ := testLimiter()
			cfg := defaultConfig()
			cfg.MaxConcurrency, cfg.RequestsPerMinute = 3, 0
			if dimension == "rpm" {
				cfg.MaxConcurrency, cfg.RequestsPerMinute = 0, 5
			}
			configure(t, l, cfg)
			var admitted atomic.Int64
			var group sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < 100; i++ {
				id := fmt.Sprint(i)
				begin(t, l, id)
				group.Add(1)
				go func() {
					defer group.Done()
					<-start
					if result := l.admit(id, "same-account", ""); result.status == 0 {
						admitted.Add(1)
					} else if result.status != 429 {
						t.Errorf("unexpected rejection: %+v", result)
					}
				}()
			}
			close(start)
			group.Wait()
			want := int64(3)
			if dimension == "rpm" {
				want = 5
			}
			if admitted.Load() != want {
				t.Fatalf("admitted %d, want %d", admitted.Load(), want)
			}
		})
	}
}

func TestRetryAttemptsConsumeRPMWithoutDoubleCountingConcurrency(t *testing.T) {
	l, _ := testLimiter()
	admit(t, l, "request", "account-a")
	for i := 0; i < 4; i++ {
		if result := l.admit("request", "account-a", ""); result.status != 0 {
			t.Fatalf("retry exhausted its own concurrency slot: %+v", result)
		}
	}
	if row := l.snapshot().Items[0]; row.Active != 1 || row.WindowRequests != 5 {
		t.Fatalf("incorrect retry accounting: %+v", row)
	}
	if result := l.admit("request", "account-a", ""); result.code != "account_rate_limit_exceeded" {
		t.Fatalf("retry escaped RPM: %+v", result)
	}
	l.complete("request")
	l.complete("request")
	l.complete("unknown")
	if row := l.snapshot().Items[0]; row.Active != 0 || row.WindowRequests != 5 {
		t.Fatalf("completion underflowed concurrency or refunded RPM: %+v", row)
	}
}

func TestFailoverHoldsEveryAdmittedAccountUntilRequestCompletion(t *testing.T) {
	l, _ := testLimiter()
	admit(t, l, "request", "account-a")
	if result := l.admit("request", "account-b", ""); result.status != 0 {
		t.Fatal(result)
	}
	begin(t, l, "another")
	for _, account := range []string{"account-a", "account-b"} {
		if result := l.admit("another", account, ""); result.code != "account_concurrency_exceeded" {
			t.Fatalf("failover released an account too early: %+v", result)
		}
	}
	l.complete("request")
	for _, row := range l.snapshot().Items {
		if row.Active != 0 || row.WindowRequests != 1 {
			t.Fatalf("failover leaked an account or refunded RPM: %+v", row)
		}
	}
}

func TestCompletionRacingAdmissionDoesNotLeak(t *testing.T) {
	l, _ := testLimiter()
	cfg := defaultConfig()
	cfg.MaxConcurrency, cfg.RequestsPerMinute = 0, 0
	configure(t, l, cfg)
	for i := 0; i < 300; i++ {
		id := fmt.Sprint(i)
		begin(t, l, id)
		var group sync.WaitGroup
		group.Add(2)
		go func() { defer group.Done(); l.admit(id, "account", "") }()
		go func() { defer group.Done(); l.complete(id) }()
		group.Wait()
		if result := l.admit(id, "account", ""); result.status != 503 {
			t.Fatal("late admission was allowed")
		}
		if result := l.begin(id); result.status != 503 {
			t.Fatal("completed request restarted")
		}
	}
	for _, row := range l.snapshot().Items {
		if row.Active != 0 {
			t.Fatalf("racing completion leaked a slot: %+v", row)
		}
	}
	l.complete("completed-before-begin")
	if result := l.begin("completed-before-begin"); result.status != 503 {
		t.Fatal("completion before the interceptor started was ignored")
	}
}

func TestCredentialAliasesAndExplicitAccountGroupsShareLimits(t *testing.T) {
	for _, order := range []string{"index-first", "id-first", "grouped"} {
		t.Run(order, func(t *testing.T) {
			l, _ := testLimiter()
			cfg := defaultConfig()
			if order == "grouped" {
				cfg.AccountGroups = map[string]string{"index-a": "real-account", "index-b": "real-account"}
				configure(t, l, cfg)
			}
			begin(t, l, "first")
			index := "index-a"
			if order == "id-first" {
				index = ""
			}
			if result := l.admit("first", index, "credential-a"); result.status != 0 {
				t.Fatal(result)
			}
			begin(t, l, "second")
			index = "index-a"
			id := "credential-a"
			if order == "index-first" {
				index = ""
			}
			if order == "grouped" {
				index, id = "index-b", "credential-b"
			}
			if result := l.admit("second", index, id); result.code != "account_concurrency_exceeded" {
				t.Fatalf("identity opened a second bucket: %+v", result)
			}
			if len(l.snapshot().Items) != 1 {
				t.Fatal("multiple buckets for a linked account")
			}
		})
	}
}

func TestGroupedWhitelistAndAliasFallbackCannotBypassProtection(t *testing.T) {
	l, _ := testLimiter()
	cfg := defaultConfig()
	cfg.AuthIndexes = []string{"index-a"}
	cfg.AccountGroups = map[string]string{"index-a": "shared", "index-b": "shared"}
	configure(t, l, cfg)
	begin(t, l, "first")
	if result := l.admit("first", "index-b", "credential-b"); result.status != 0 {
		t.Fatal(result)
	}
	begin(t, l, "second")
	if result := l.admit("second", "", "credential-b"); result.code != "account_concurrency_exceeded" {
		t.Fatalf("group member or fallback escaped whitelist: %+v", result)
	}
	if len(l.snapshot().Items) != 1 {
		t.Fatal("grouped credentials did not share one bucket")
	}
}

func TestConflictingCredentialIdentitiesFailClosed(t *testing.T) {
	l, _ := testLimiter()
	admit(t, l, "index-request", "index-a")
	begin(t, l, "id-request")
	if result := l.admit("id-request", "", "credential-a"); result.status != 0 {
		t.Fatal(result)
	}
	begin(t, l, "conflict")
	if result := l.admit("conflict", "index-a", "credential-a"); result.status != 503 {
		t.Fatalf("conflicting identity silently discarded a bucket: %+v", result)
	}
	if len(l.snapshot().Items) != 2 {
		t.Fatal("identity conflict discarded rate or concurrency state")
	}
	grouped, _ := testLimiter()
	cfg := defaultConfig()
	cfg.AccountGroups = map[string]string{"index-a": "a", "credential-a": "b"}
	configure(t, grouped, cfg)
	begin(t, grouped, "conflict")
	if result := grouped.admit("conflict", "index-a", "credential-a"); result.status != 503 {
		t.Fatalf("conflicting group configuration ignored: %+v", result)
	}
}

func TestGroupingChangesMustDrainButThresholdReloadKeepsHistory(t *testing.T) {
	l, now := testLimiter()
	cfg := defaultConfig()
	cfg.MaxConcurrency = 0
	configure(t, l, cfg)
	for i := 0; i < 5; i++ {
		id := fmt.Sprint(i)
		admit(t, l, id, "account")
		l.complete(id)
		*now = now.Add(10 * time.Second)
	}
	cfg.RequestsPerMinute = 2
	configure(t, l, cfg)
	begin(t, l, "blocked")
	if result := l.admit("blocked", "account", ""); result.retryAfter != 40*time.Second {
		t.Fatalf("reload erased history or calculated expiry incorrectly: %+v", result)
	}
	cfg.AccountGroups = map[string]string{"account": "group"}
	if err := l.configure(cfg); err == nil {
		t.Fatal("group change discarded live rate history")
	}
	*now = now.Add(60 * time.Second)
	configure(t, l, cfg)
	admit(t, l, "after-drain", "account")
	if result := l.snapshot(); len(result.Items) != 2 || result.Admitted != 6 {
		t.Fatalf("changing groups erased historical totals: %+v", result)
	}
	*now = now.Add(time.Hour)
	if err := l.configure(defaultConfig()); err == nil {
		t.Fatal("group change discarded a long-running request")
	}
}

func TestOverridesWhitelistAndIndependentLimitDisabling(t *testing.T) {
	l, _ := testLimiter()
	zero, two := 0, 2
	cfg := defaultConfig()
	cfg.AuthIndexes = []string{"included", "other"}
	cfg.AccountLimits = map[string]limitOverride{
		"included": {RequestsPerMinute: &two, MaxConcurrency: &zero},
		"other":    {RequestsPerMinute: &zero},
	}
	configure(t, l, cfg)
	admit(t, l, "one", "included")
	admit(t, l, "two", "included")
	begin(t, l, "three")
	if result := l.admit("three", "included", ""); result.code != "account_rate_limit_exceeded" {
		t.Fatal(result)
	}
	admit(t, l, "bypass-one", "excluded")
	admit(t, l, "bypass-two", "excluded")
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("other-%d", i)
		admit(t, l, id, "other")
		l.complete(id)
	}
	if rows := l.snapshot().Items; len(rows) != 2 {
		t.Fatalf("excluded account was tracked: %+v", rows)
	}
}

func TestActiveRequestsNeverExpireWithTheRateWindow(t *testing.T) {
	l, now := testLimiter()
	admit(t, l, "stream", "account")
	*now = now.Add(24 * time.Hour)
	begin(t, l, "next")
	if result := l.admit("next", "account", ""); result.code != "account_concurrency_exceeded" {
		t.Fatalf("long-running stream lost its slot: %+v", result)
	}
	l.complete("stream")
	if result := l.admit("next", "account", ""); result.status != 0 {
		t.Fatal(result)
	}
	l.complete("next")
	*now = now.Add(time.Minute)
	result := l.snapshot()
	if len(l.accounts) != 0 || len(l.aliases) != 0 {
		t.Fatal("idle enforcement state or identity mappings were not evicted")
	}
	if len(result.Items) != 1 || result.Items[0].Admitted != 2 || result.Items[0].ConcurrencyRejected != 1 || result.Items[0].Active != 0 || result.Items[0].WindowRequests != 0 {
		t.Fatalf("idle account lost cumulative counters: %+v", result)
	}
}

func TestAccountTotalsSurviveIdleWindowsAndReconfigurationUntilRestart(t *testing.T) {
	l, now := testLimiter()
	cfg := defaultConfig()
	cfg.RequestsPerMinute = 2
	configure(t, l, cfg)
	var fingerprint string
	for cycle := 1; cycle <= 3; cycle++ {
		first, busy, second, rate := fmt.Sprintf("first-%d", cycle), fmt.Sprintf("busy-%d", cycle), fmt.Sprintf("second-%d", cycle), fmt.Sprintf("rate-%d", cycle)
		admit(t, l, first, "account")
		begin(t, l, busy)
		if result := l.admit(busy, "account", ""); result.code != "account_concurrency_exceeded" {
			t.Fatalf("cumulative totals changed concurrency protection: %+v", result)
		}
		l.complete(busy)
		l.complete(first)
		admit(t, l, second, "account")
		l.complete(second)
		begin(t, l, rate)
		if result := l.admit(rate, "account", ""); result.code != "account_rate_limit_exceeded" {
			t.Fatalf("cumulative totals changed rate protection: %+v", result)
		}
		l.complete(rate)
		*now = now.Add(time.Minute)
		// Reconfiguration and repeated reads must not reset archived counters.
		cfg.AccountDisplay = displayMaskedEmail
		configure(t, l, cfg)
		l.snapshot()
		result := l.snapshot()
		if len(result.Items) != 1 {
			t.Fatalf("idle account disappeared: %+v", result)
		}
		row := result.Items[0]
		if cycle == 1 {
			fingerprint = row.Account
		}
		if row.Account != fingerprint || row.Admitted != uint64(2*cycle) || row.RateRejected != uint64(cycle) || row.ConcurrencyRejected != uint64(cycle) || row.Active != 0 || row.WindowRequests != 0 || row.RateRetryAfter != 0 {
			t.Fatalf("counters reset or expired quota remained in cycle %d: %+v", cycle, row)
		}
		if result.Admitted != row.Admitted || result.RateRejected != row.RateRejected || result.BusyRejected != row.ConcurrencyRejected || result.Rejected != row.RateRejected+row.ConcurrencyRejected {
			t.Fatalf("account and global cumulative counters diverged: %+v", result)
		}
	}
	restarted := newLimiter(l.now).snapshot()
	if len(restarted.Items) != 0 || restarted.Admitted != 0 || restarted.Rejected != 0 || restarted.RateRejected != 0 || restarted.BusyRejected != 0 {
		t.Fatalf("new plugin instance retained previous counters: %+v", restarted)
	}
}

func TestHistoricalTotalsDoNotConsumeEnforcementCapacity(t *testing.T) {
	l, now := testLimiter()
	for i := 0; i < maxTrackedAccounts; i++ {
		id := fmt.Sprint(i)
		admit(t, l, id, id)
		l.complete(id)
	}
	*now = now.Add(time.Minute)
	if result := l.snapshot(); len(result.Items) != maxTrackedAccounts || len(l.accounts) != 0 {
		t.Fatalf("idle buckets did not retain totals and release enforcement capacity: rows=%d, buckets=%d", len(result.Items), len(l.accounts))
	}
	admit(t, l, "new-account", "new-account")
	l.complete("new-account")
	admit(t, l, "returning-account", "0")
	l.complete("returning-account")
	if result := l.snapshot(); len(result.Items) != maxTrackedAccounts+1 || result.Admitted != maxTrackedAccounts+2 || l.totals["index:0"].admitted != 2 {
		t.Fatal("historical counters blocked or reset new admissions")
	}
}

func TestTrackingBoundsAndQuiesce(t *testing.T) {
	l, _ := testLimiter()
	for i := 0; i < maxTrackedRequests; i++ {
		begin(t, l, fmt.Sprint(i))
	}
	if result := l.begin("overflow"); result.status != 503 {
		t.Fatal("request tracking exceeded its bound")
	}
	for i := 0; i <= maxCompletedRequests; i++ {
		l.complete(fmt.Sprint(i))
	}
	if len(l.completed) != maxCompletedRequests || l.completionOrder.Len() != maxCompletedRequests || len(l.requests) != 0 {
		t.Fatal("completion bookkeeping is unbounded")
	}
	if result := l.admit("0", "account", ""); result.status != 503 {
		t.Fatal("evicted tombstone allowed a late after-auth call")
	}
	for i := 0; i < maxTrackedAccounts; i++ {
		admit(t, l, fmt.Sprintf("new-%d", i), fmt.Sprint(i))
	}
	begin(t, l, "account-overflow")
	if result := l.admit("account-overflow", "overflow", ""); result.status != 503 {
		t.Fatal("account tracking exceeded its bound")
	}
	l.quiesce()
	if result := l.begin("after-quiesce"); result.status != 503 {
		t.Fatal("quiescing accepted a new request")
	}
	if result := l.admit("new-0", "0", ""); result.status != 503 {
		t.Fatal("quiescing admitted an existing request")
	}
	l.complete("new-0")
}
