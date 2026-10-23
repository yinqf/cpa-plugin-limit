package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestMonitorEmailModesPreserveRedactionAndAccounting(t *testing.T) {
	l, now := testLimiter()
	lookups := 0
	l.directory.setLoader(func() ([]credentialEmail, error) {
		lookups++
		return decodeCredentialEmails([]byte(`{"files":[{"id":"private-id","auth_index":"private-index","email":"alice@example.com","account":"private-secret","label":"private-label","name":"private-name.json"}]}`))
	})
	begin(t, l, "request")
	if result := l.admit("request", "private-index", "private-id"); result.status != 0 {
		t.Fatal(result)
	}
	for _, test := range []struct{ mode, label string }{
		{displayMaskedEmail, "al***@example.com"}, {displayEmail, "alice@example.com"}, {displayFingerprint, ""},
	} {
		call[registration](t, l, pluginabi.MethodPluginReconfigure, registerRequest{SchemaVersion: 6, ConfigYAML: []byte("account_display: " + test.mode)})
		response := call[pluginapi.ManagementResponse](t, l, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: "GET", Path: "/status"})
		var result snapshot
		if err := json.Unmarshal(response.Body, &result); err != nil {
			t.Fatal(err)
		}
		if result.AccountDisplay != test.mode || len(result.Items) != 1 || result.Items[0].Label != test.label || len(result.Items[0].Account) != 16 {
			t.Fatalf("incorrect account display: %s", response.Body)
		}
		if result.Admitted != 1 || result.ActiveSlots != 1 || result.WindowRequests != 1 {
			t.Fatal("display mode changed admission accounting")
		}
		if bytes.Contains(response.Body, []byte("private-")) || test.mode != displayEmail && bytes.Contains(response.Body, []byte("alice@example.com")) {
			t.Fatalf("monitor exposed unrequested account data: %s", response.Body)
		}
	}
	*now = now.Add(emailCacheTTL)
	l.monitorSnapshot()
	if lookups != 1 {
		t.Fatalf("cache was bypassed or fingerprint mode queried credentials: %d", lookups)
	}
	begin(t, l, "busy")
	if result := l.admit("busy", "private-index", "private-id"); result.code != "account_concurrency_exceeded" {
		t.Fatal("display reconfiguration released the active account slot")
	}
}

func TestEmailLabelsFollowAliasesAndGroupsWithoutMergingAccounts(t *testing.T) {
	l, _ := testLimiter()
	cfg := defaultConfig()
	cfg.AccountGroups = map[string]string{"a": "shared", "b": "shared"}
	configure(t, l, cfg)
	l.directory.setLoader(func() ([]credentialEmail, error) {
		return []credentialEmail{
			{AuthIndex: "a", Email: "same@example.com"},
			{ID: "b-id", AuthIndex: "b", Email: "same@example.com"},
			{AuthIndex: "separate", Email: "same@example.com"},
			{ID: "runtime-id", Email: "runtime@example.com"},
			{AuthIndex: "invalid", Email: "Display Name <other@example.com>"},
		}, nil
	})
	admit(t, l, "a", "a")
	begin(t, l, "b")
	if result := l.admit("b", "b", "b-id"); result.code != "account_concurrency_exceeded" {
		t.Fatal(result)
	}
	admit(t, l, "other-account", "separate")
	admit(t, l, "invalid-email", "invalid")
	begin(t, l, "runtime")
	if result := l.admit("runtime", "", "runtime-id"); result.status != 0 {
		t.Fatal(result)
	}
	result := l.monitorSnapshot()
	labels := make(map[string]int)
	for _, row := range result.Items {
		labels[row.Label]++
	}
	if result.AccountDisplay != displayEmail || len(result.Items) != 4 || result.ActiveSlots != 4 || labels["same@example.com"] != 2 || labels["runtime@example.com"] != 1 || labels[""] != 1 {
		t.Fatalf("email identity changed grouping or missing-email fallback: %+v", result)
	}
	if label := emailLabel([]string{"index:a", "index:b"}, map[string]string{"index:a": "a@example.com", "index:b": "b@example.com"}, displayEmail); label != "a@example.com、b@example.com" {
		t.Fatalf("multiple real accounts were mislabeled as one email: %s", label)
	}
}

func TestIdleAccountKeepsEmailAndCumulativeCountersInStatus(t *testing.T) {
	l, now := testLimiter()
	l.directory.setLoader(func() ([]credentialEmail, error) {
		return []credentialEmail{{ID: "private-id", AuthIndex: "private-index", Email: "alice@example.com"}}, nil
	})
	for cycle := 1; cycle <= 2; cycle++ {
		id := fmt.Sprint(cycle)
		begin(t, l, id)
		if result := l.admit(id, "private-index", "private-id"); result.status != 0 {
			t.Fatal(result)
		}
		l.complete(id)
		*now = now.Add(24 * time.Hour)
		response := call[pluginapi.ManagementResponse](t, l, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: "GET", Path: "/status"})
		var result snapshot
		if err := json.Unmarshal(response.Body, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Items) != 1 || result.Items[0].Label != "alice@example.com" || result.Items[0].Admitted != uint64(cycle) || result.Items[0].WindowRequests != 0 || result.Items[0].Active != 0 || bytes.Contains(response.Body, []byte("private-")) {
			t.Fatalf("idle status lost the email or counters, or exposed identities: %s", response.Body)
		}
	}
}

func TestEmailCacheRefreshFailureAndAmbiguousIdentityFallback(t *testing.T) {
	l, now := testLimiter()
	admit(t, l, "request", "account")
	lookups := 0
	l.directory.setLoader(func() ([]credentialEmail, error) {
		lookups++
		if lookups == 1 {
			return nil, fmt.Errorf("private callback failure")
		}
		if lookups == 2 {
			return []credentialEmail{{AuthIndex: "account", Email: "first@example.com"}}, nil
		}
		return []credentialEmail{{AuthIndex: "account", Email: "first@example.com"}, {AuthIndex: "account", Email: "other@example.com"}}, nil
	})
	first := l.monitorSnapshot()
	if first.EmailAvailable || first.Items[0].Label != "" {
		t.Fatal("lookup failure returned an email label")
	}
	*now = now.Add(emailCacheTTL - time.Nanosecond)
	l.monitorSnapshot()
	if lookups != 1 {
		t.Fatal("failed lookup was not cached")
	}
	*now = now.Add(time.Nanosecond)
	second := l.monitorSnapshot()
	if !second.EmailAvailable || second.Items[0].Label != "first@example.com" {
		t.Fatalf("cache did not retry after expiry: %+v", second)
	}
	*now = now.Add(emailCacheTTL)
	if third := l.monitorSnapshot(); third.Items[0].Label != "" {
		t.Fatal("conflicting host identities displayed an arbitrary email")
	}
}

func TestEmailLookupDoesNotBlockAdmission(t *testing.T) {
	l, _ := testLimiter()
	admit(t, l, "first", "account")
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	l.directory.setLoader(func() ([]credentialEmail, error) {
		close(started)
		<-release
		return nil, nil
	})
	go func() { l.monitorSnapshot(); close(done) }()
	<-started
	admitted := make(chan decision, 1)
	go func() {
		l.begin("independent")
		admitted <- l.admit("independent", "other-account", "")
	}()
	select {
	case result := <-admitted:
		close(release)
		<-done
		if result.status != 0 {
			t.Fatal(result)
		}
	case <-time.After(5 * time.Second):
		close(release)
		<-done
		t.Fatal("host email lookup held the admission mutex")
	}
}

func TestEmailValidationRejectsNamesSecretsAndControlCharacters(t *testing.T) {
	for _, value := range []string{"", "not-an-email", "Name <user@example.com>", "a@example.com\r\nX: value", strings.Repeat("a", 255) + "@example.com"} {
		if validEmail(value) != "" {
			t.Fatalf("invalid email accepted: %q", value)
		}
	}
	for email, want := range map[string]string{"a@example.com": "a***@example.com", "ab@example.com": "a***@example.com", "alice@example.com": "al***@example.com", "用户甲@example.com": "用户***@example.com"} {
		if validEmail(email) == "" || maskEmail(email) != want {
			t.Fatalf("email was not correctly masked: %q", email)
		}
	}
}
