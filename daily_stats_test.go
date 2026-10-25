package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func checkDailyTotals(t *testing.T, result snapshot, date string, admitted, rate, busy uint64) {
	t.Helper()
	if result.StatisticsDate != date || result.Admitted != admitted || result.RateRejected != rate || result.BusyRejected != busy || result.Rejected != rate+busy {
		t.Fatalf("incorrect daily totals: %+v", result)
	}
	var rowAdmitted, rowRate, rowBusy uint64
	for _, row := range result.Items {
		rowAdmitted += row.Admitted
		rowRate += row.RateRejected
		rowBusy += row.ConcurrencyRejected
	}
	if rowAdmitted != admitted || rowRate != rate || rowBusy != busy {
		t.Fatalf("daily account counters differ from global counters: %+v", result)
	}
}

func TestBeijingMidnightResetsCountersWithoutReleasingLimits(t *testing.T) {
	l, now := testLimiter()
	*now = time.Date(2026, 10, 25, 15, 59, 30, 0, time.UTC)
	cfg := defaultConfig()
	cfg.RequestsPerMinute = 1
	configure(t, l, cfg)
	admit(t, l, "yesterday-idle", "idle")
	l.complete("yesterday-idle")
	admit(t, l, "yesterday-rate", "rate")
	l.complete("yesterday-rate")
	admit(t, l, "overnight-stream", "busy")
	begin(t, l, "yesterday-rate-rejected")
	if result := l.admit("yesterday-rate-rejected", "rate", ""); result.status != 429 {
		t.Fatal(result)
	}
	l.complete("yesterday-rate-rejected")
	begin(t, l, "yesterday-busy-rejected")
	if result := l.admit("yesterday-busy-rejected", "busy", ""); result.status != 429 {
		t.Fatal(result)
	}
	l.complete("yesterday-busy-rejected")

	*now = time.Date(2026, 10, 25, 15, 59, 59, 999999999, time.UTC)
	checkDailyTotals(t, l.snapshot(), "2026-10-25", 3, 1, 1)
	*now = now.Add(time.Nanosecond)
	// The ordinary cleanup is not due yet; admission itself must advance the day.
	begin(t, l, "today-busy-rejected")
	if result := l.admit("today-busy-rejected", "busy", ""); result.code != "account_concurrency_exceeded" {
		t.Fatalf("midnight released an overnight stream's slot: %+v", result)
	}
	l.complete("today-busy-rejected")
	result := l.snapshot()
	checkDailyTotals(t, result, "2026-10-26", 0, 0, 1)
	if result.ActiveSlots != 1 || result.WindowRequests != 3 || len(result.Items) != 3 {
		t.Fatalf("midnight reset enforcement state: %+v", result)
	}
	begin(t, l, "today-rate-rejected")
	if result := l.admit("today-rate-rejected", "rate", ""); result.code != "account_rate_limit_exceeded" || result.retryAfter != 30*time.Second {
		t.Fatalf("midnight refunded yesterday's rolling-window quota: %+v", result)
	}
	l.complete("today-rate-rejected")
	checkDailyTotals(t, l.snapshot(), "2026-10-26", 0, 1, 1)

	l.complete("overnight-stream")
	*now = now.Add(30 * time.Second)
	admit(t, l, "today-rate", "rate")
	l.complete("today-rate")
	admit(t, l, "today-busy", "busy")
	l.complete("today-busy")
	result = l.snapshot()
	checkDailyTotals(t, result, "2026-10-26", 2, 1, 1)
	if result.ActiveSlots != 0 || result.WindowRequests != 2 || len(result.Items) != 2 {
		t.Fatalf("completion leaked a slot or yesterday-only idle row remained: %+v", result)
	}
}

func TestStatusReadAdvancesBeijingDayAfterIdlePeriods(t *testing.T) {
	l, now := testLimiter()
	readStatus := func() snapshot {
		t.Helper()
		response := call[pluginapi.ManagementResponse](t, l, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: "GET", Path: "/status"})
		var result snapshot
		if err := json.Unmarshal(response.Body, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	*now = time.Date(2026, 12, 31, 15, 58, 0, 0, time.UTC)
	admit(t, l, "old-day", "account")
	l.complete("old-day")
	checkDailyTotals(t, readStatus(), "2026-12-31", 1, 0, 0)
	*now = time.Date(2026, 12, 31, 16, 0, 0, 0, time.UTC)
	result := readStatus()
	checkDailyTotals(t, result, "2027-01-01", 0, 0, 0)
	if len(result.Items) != 0 {
		t.Fatal("yesterday-only idle accounts remained in today's status")
	}
	admit(t, l, "new-day", "account")
	l.complete("new-day")
	// UTC midnight is 08:00 Beijing and must not cause a second reset.
	*now = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	checkDailyTotals(t, readStatus(), "2027-01-01", 1, 0, 0)
	*now = now.Add(4 * 24 * time.Hour)
	result = readStatus()
	checkDailyTotals(t, result, "2027-01-05", 0, 0, 0)
	if len(result.Items) != 0 {
		t.Fatal("multiple idle days retained old account totals")
	}
}

func TestDailyStatisticsDateIsIndependentOfInputTimeZone(t *testing.T) {
	for _, test := range []struct{ timestamp, date string }{
		{"2026-10-25T15:59:59.999999999Z", "2026-10-25"},
		{"2026-10-25T16:00:00Z", "2026-10-26"},
		{"2026-10-25T09:00:00-07:00", "2026-10-26"},
		{"2026-10-26T01:00:00+09:00", "2026-10-26"},
		{"2026-10-26T00:00:00Z", "2026-10-26"},
	} {
		t.Run(test.timestamp, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339Nano, test.timestamp)
			if err != nil {
				t.Fatal(err)
			}
			l := newLimiter(func() time.Time { return now })
			checkDailyTotals(t, l.snapshot(), test.date, 0, 0, 0)
		})
	}
}

func TestConcurrentAdmissionsAfterMidnightCountInTheNewDay(t *testing.T) {
	l, now := testLimiter()
	*now = time.Date(2026, 10, 25, 15, 59, 59, 0, time.UTC)
	cfg := defaultConfig()
	cfg.MaxConcurrency, cfg.RequestsPerMinute = 0, 0
	configure(t, l, cfg)
	admit(t, l, "overnight", "account")
	const count = 100
	for i := 0; i < count; i++ {
		begin(t, l, fmt.Sprint(i))
	}
	*now = now.Add(time.Second)
	var group sync.WaitGroup
	for i := 0; i < count; i++ {
		group.Go(func() {
			id := fmt.Sprint(i)
			if result := l.admit(id, "account", ""); result.status != 0 {
				t.Errorf("new-day admission failed: %+v", result)
			}
			l.complete(id)
		})
	}
	group.Wait()
	result := l.snapshot()
	checkDailyTotals(t, result, "2026-10-26", count, 0, 0)
	if result.ActiveSlots != 1 || result.WindowRequests != count+1 {
		t.Fatalf("daily reset changed existing slots or rate history: %+v", result)
	}
	l.complete("overnight")
	if result := l.snapshot(); result.ActiveSlots != 0 {
		t.Fatal("overnight completion failed to release its original slot")
	}
}

func TestMidnightPreservesSchedulerReservationsAndAffinity(t *testing.T) {
	l, now := testLimiter()
	*now = time.Date(2026, 10, 25, 15, 59, 59, 0, time.UTC)
	input := startRouting(t, l, "pending", "session")
	first := l.pick(input)
	initial := l.snapshot()
	*now = now.Add(time.Second)
	result := l.snapshot()
	checkDailyTotals(t, result, "2026-10-26", 0, 0, 0)
	if result.PendingAttempts != 1 || result.ActiveSlots != 1 || result.AffinitySessions != initial.AffinitySessions || result.AffinitySessions == 0 {
		t.Fatalf("midnight lost a pending reservation or affinity binding: %+v", result)
	}
	second := l.pick(startRouting(t, l, "second", "session"))
	if second.AuthID == first.AuthID || !second.Handled {
		t.Fatal("new-day scheduler overbooked yesterday's pending reservation")
	}
	for request, authID := range map[string]string{"pending": first.AuthID, "second": second.AuthID} {
		if result := l.admit(request, "", authID); result.status != 0 {
			t.Fatal(result)
		}
		l.complete(request)
	}
	result = l.snapshot()
	checkDailyTotals(t, result, "2026-10-26", 2, 0, 0)
	if result.PendingAttempts != 0 || result.ActiveSlots != 0 {
		t.Fatal("new-day execution leaked pending or active reservations")
	}
}
