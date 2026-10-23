package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func call[T any](t *testing.T, l *limiter, method string, value any) T {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	response, err := l.handle(method, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(response, &env); err != nil || !env.OK {
		t.Fatalf("invalid envelope %s: %v", response, err)
	}
	var result T
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRegistrationAndVisualConfigurationContract(t *testing.T) {
	l, _ := testLimiter()
	reg := call[registration](t, l, pluginabi.MethodPluginRegister, registerRequest{SchemaVersion: 6})
	if reg.SchemaVersion != 2 || reg.Metadata.Name != pluginName || reg.Capabilities["scheduler"] || !reg.Capabilities["request_lifecycle_plugin"] {
		t.Fatalf("unexpected registration: %+v", reg)
	}
	fields := make(map[string]bool)
	for _, field := range reg.Metadata.ConfigFields {
		fields[field.Name] = true
	}
	for _, key := range []string{"requests_per_minute", "max_concurrency", "auth_indexes", "account_groups", "account_limits"} {
		if !fields[key] {
			t.Fatalf("missing visual field %s", key)
		}
	}
	encoded, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Metadata struct {
			Name         string
			ConfigFields []struct {
				Name    string
				Type    string
				Default json.RawMessage
			}
		}
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	wantDefaults := map[string]string{"requests_per_minute": "5", "max_concurrency": "1", "auth_indexes": "[]", "account_groups": "{}", "account_limits": "{}"}
	if wire.Metadata.Name != pluginName || len(wire.Metadata.ConfigFields) != len(wantDefaults) {
		t.Fatalf("metadata fields were lost on the wire: %s", encoded)
	}
	for _, field := range wire.Metadata.ConfigFields {
		if string(field.Default) != wantDefaults[field.Name] {
			t.Fatalf("configuration editor received wrong default for %s: %s", field.Name, field.Default)
		}
	}
	call[registration](t, l, pluginabi.MethodPluginReconfigure, registerRequest{SchemaVersion: 6, ConfigYAML: []byte("requests_per_minute: 9\nmax_concurrency: 2\nauth_indexes: [a]\naccount_groups: {a: shared}\naccount_limits: {shared: {max_concurrency: 3}}\nenabled: true\npriority: 100\n")})
	if l.cfg.RequestsPerMinute != 9 || l.cfg.accountLimits("shared").MaxConcurrency != 3 || !l.cfg.includes("a", "") || l.cfg.includes("b", "") {
		t.Fatalf("visual configuration was not applied: %+v", l.cfg)
	}
}

func TestVisualEditorBlankValuesRestoreDefaults(t *testing.T) {
	l, _ := testLimiter()
	call[registration](t, l, pluginabi.MethodPluginReconfigure, registerRequest{SchemaVersion: 6, ConfigYAML: []byte("requests_per_minute: 9\nmax_concurrency: 3\nauth_indexes: [a]\naccount_groups: {a: shared}\naccount_limits: {shared: {max_concurrency: 2}}")})
	call[registration](t, l, pluginabi.MethodPluginReconfigure, registerRequest{SchemaVersion: 6, ConfigYAML: []byte("requests_per_minute: null\nmax_concurrency: null\nauth_indexes: null\naccount_groups: null\naccount_limits: null\n")})
	if !reflect.DeepEqual(l.cfg, defaultConfig()) {
		t.Fatalf("clearing fields in the visual editor did not restore defaults: %+v", l.cfg)
	}
}

func TestMonitorSummaryReflectsRuntimeAndKeepsCompletedRejections(t *testing.T) {
	l, now := testLimiter()
	cfg := defaultConfig()
	cfg.AccountGroups = map[string]string{"private-a": "private-group", "private-b": "private-group"}
	cfg.AuthIndexes = []string{"private-a", "private-b"}
	configure(t, l, cfg)
	admit(t, l, "first", "private-a")
	begin(t, l, "busy")
	if result := l.admit("busy", "private-b", ""); result.code != "account_concurrency_exceeded" {
		t.Fatal(result)
	}
	initial := l.snapshot()
	if initial.ActiveSlots != 1 || initial.WindowRequests != 1 || initial.BusyRejected != 1 || initial.RateRejected != 0 || initial.Defaults != cfg.limits {
		t.Fatalf("incorrect monitoring totals: %+v", initial)
	}
	if initial.Scope.AllCredentials || initial.Scope.SelectedCount != 2 || initial.Scope.GroupCount != 1 || initial.Scope.OverrideCount != 0 {
		t.Fatalf("incorrect monitoring scope: %+v", initial.Scope)
	}
	l.complete("first")
	l.complete("busy")
	for i := 0; i < 4; i++ {
		id := fmt.Sprint(i)
		admit(t, l, id, "private-a")
		l.complete(id)
	}
	begin(t, l, "rate")
	if result := l.admit("rate", "private-a", ""); result.code != "account_rate_limit_exceeded" {
		t.Fatal(result)
	}
	l.complete("rate")
	*now = now.Add(time.Minute)
	result := l.snapshot()
	if result.ActiveSlots != 0 || result.WindowRequests != 0 || result.ActiveRequests != 0 || len(result.Items) != 0 || result.Rejected != 2 || result.BusyRejected != 1 || result.RateRejected != 1 {
		t.Fatalf("expired account discarded lifetime monitoring totals: %+v", result)
	}
	raw, err := json.Marshal(result)
	if err != nil || bytes.Contains(raw, []byte("private-")) {
		t.Fatalf("scope summary exposed configured identities: %s / %v", raw, err)
	}
}

func TestInvalidConfigurationDoesNotReplaceActiveSettings(t *testing.T) {
	l, _ := testLimiter()
	for _, cfg := range []string{
		"requests_per_minute: -1", "max_concurrency: -1", "requests_per_minute: 10001", "max_concurrency: 1.5",
		"account_limits: {a: {requests_per_minute: -1}}", "auth_indexes: [a, a]", "auth_indexes: ['']",
		"account_groups: {a: ''}", "account_limits: {'': {max_concurrency: 1}}", "requests_per_minute: invalid",
		"defaults: &limits {max_concurrency: 1.5}\n<<: *limits",
		"account_limits: {a: {requests_per_minute: 1.5}}",
	} {
		t.Run(cfg, func(t *testing.T) {
			raw, _ := json.Marshal(registerRequest{SchemaVersion: 6, ConfigYAML: []byte(cfg)})
			if _, err := l.handle(pluginabi.MethodPluginReconfigure, raw); err == nil {
				t.Fatal("invalid configuration accepted")
			}
			if !reflect.DeepEqual(l.cfg, defaultConfig()) {
				t.Fatal("invalid configuration changed active limits")
			}
		})
	}
	for _, schema := range []uint32{0, 1} {
		raw, _ := json.Marshal(registerRequest{SchemaVersion: schema})
		if _, err := parseConfig(raw); err == nil {
			t.Fatal("host without lifecycle support accepted")
		}
	}
}

func intercept(t *testing.T, l *limiter, method, id, account string) pluginapi.RequestInterceptResponse {
	t.Helper()
	return call[pluginapi.RequestInterceptResponse](t, l, method, pluginapi.RequestInterceptRequest{
		RequestID: id, SourceFormat: "openai-response", Model: "model",
		Headers: http.Header{"X-Session-Id": {"session"}}, Body: []byte(`{"prompt_cache_key":"stable","input":[]}`),
		Metadata: map[string]any{"selected_auth_index": account, "canonical_session_id": "session"},
	})
}

func TestRPCAdmissionPreservesAffinityAndReturnsRetryable429(t *testing.T) {
	l, _ := testLimiter()
	for _, method := range []string{pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter} {
		resp := intercept(t, l, method, "first", "account")
		if resp.Terminate || len(resp.Body) > 0 || len(resp.Headers) > 0 || len(resp.ClearHeaders) > 0 {
			t.Fatalf("allowed request was rewritten: %+v", resp)
		}
	}
	intercept(t, l, pluginabi.MethodRequestInterceptBefore, "second", "account")
	resp := intercept(t, l, pluginabi.MethodRequestInterceptAfter, "second", "account")
	if !resp.Terminate || resp.StatusCode != 429 || resp.ResponseHeaders.Get("Retry-After") != "1" {
		t.Fatalf("incorrect refusal contract: %+v", resp)
	}
	var body struct{ Error struct{ Code, Type string } }
	if err := json.Unmarshal(resp.ResponseBody, &body); err != nil || body.Error.Code != "account_concurrency_exceeded" || body.Error.Type != "rate_limit_error" {
		t.Fatalf("incorrect error body %s: %v", resp.ResponseBody, err)
	}
	if bytes.Contains(resp.ResponseBody, []byte("stable")) || bytes.Contains(resp.ResponseBody, []byte("selected_auth_index")) {
		t.Fatal("refusal exposed request data")
	}
}

func TestRPCReleasesSlotsForEveryTerminalOutcome(t *testing.T) {
	for _, outcome := range []pluginapi.RequestCompletionOutcome{
		pluginapi.RequestCompletionSucceeded, pluginapi.RequestCompletionFailed, pluginapi.RequestCompletionRejected, pluginapi.RequestCompletionCanceled,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			l, _ := testLimiter()
			intercept(t, l, pluginabi.MethodRequestInterceptBefore, "request", "account")
			intercept(t, l, pluginabi.MethodRequestInterceptAfter, "request", "account")
			call[struct{}](t, l, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: "request", Outcome: outcome})
			intercept(t, l, pluginabi.MethodRequestInterceptBefore, "next", "account")
			if resp := intercept(t, l, pluginabi.MethodRequestInterceptAfter, "next", "account"); resp.Terminate {
				t.Fatalf("terminal outcome leaked a slot: %+v", resp)
			}
		})
	}
}

func TestMissingIdentityAndMalformedInterceptionFailClosed(t *testing.T) {
	l, _ := testLimiter()
	begin(t, l, "request")
	if result := l.admit("request", "", ""); result.status != 503 {
		t.Fatal("unidentified account was admitted")
	}
	for _, method := range []string{pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter} {
		raw, err := l.handle(method, []byte("invalid-json"))
		if err != nil || !bytes.Contains(raw, []byte(`"Terminate":true`)) || !bytes.Contains(raw, []byte(`"StatusCode":503`)) {
			t.Fatalf("interception error would be skipped by host: %s / %v", raw, err)
		}
		var env pluginabi.Envelope
		if err := json.Unmarshal(rpcFailure(method, "dummy internal error"), &env); err != nil || !env.OK {
			t.Fatal("native interceptor error did not fail closed")
		}
	}
}

func TestStatusUsesModernResourcesAndHidesIdentifiers(t *testing.T) {
	l, _ := testLimiter()
	admit(t, l, "private-request-id", "private-credential-index")
	reg := call[pluginapi.ManagementRegistrationResponse](t, l, pluginabi.MethodManagementRegister, struct{}{})
	if len(reg.Routes) != 0 || len(reg.Resources) != 2 {
		t.Fatalf("unexpected management routes: %+v", reg)
	}
	for _, path := range []string{"/status", "/v0/resource/plugins/cpa-plugin-limit/status"} {
		resp := call[pluginapi.ManagementResponse](t, l, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: "GET", Path: path})
		if resp.StatusCode != 200 || resp.Headers.Get("Cache-Control") != "no-store" || bytes.Contains(resp.Body, []byte("private-")) {
			t.Fatalf("status contract or redaction failed: %+v", resp)
		}
		var result snapshot
		if err := json.Unmarshal(resp.Body, &result); err != nil || len(result.Items) != 1 || len(result.Items[0].Account) != 16 || result.Items[0].Active != 1 {
			t.Fatalf("invalid status: %s / %v", resp.Body, err)
		}
	}
	for _, test := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/ui", 200}, {"POST", "/status", 405}, {"GET", "/unknown", 404}, {"GET", "/evil/status", 404},
	} {
		resp := call[pluginapi.ManagementResponse](t, l, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: test.method, Path: test.path})
		if resp.StatusCode != test.want {
			t.Fatalf("%s %s: %d", test.method, test.path, resp.StatusCode)
		}
	}
}

func TestHistoryRemainsBoundedAndCanEnableRPMWithoutReset(t *testing.T) {
	l, now := testLimiter()
	cfg := defaultConfig()
	cfg.MaxConcurrency, cfg.RequestsPerMinute = 0, 0
	configure(t, l, cfg)
	admit(t, l, "request", "account")
	for i := 0; i < 3*maxLimit; i++ {
		if result := l.admit("request", "account", ""); result.status != 0 {
			t.Fatal(result)
		}
	}
	row := l.snapshot().Items[0]
	if row.WindowRequests != maxLimit {
		t.Fatalf("unbounded history: %+v", row)
	}
	cfg.RequestsPerMinute = 5
	configure(t, l, cfg)
	if result := l.admit("request", "account", ""); result.retryAfter != time.Minute {
		t.Fatalf("enabling RPM discarded history: %+v", result)
	}
	*now = now.Add(time.Minute)
	if result := l.admit("request", "account", ""); result.status != 0 {
		t.Fatal(result)
	}
}

func TestSelectedCredentialOverridesDoNotPartitionByModelOrClient(t *testing.T) {
	l, _ := testLimiter()
	for i := 0; i < 2; i++ {
		id := fmt.Sprint(i)
		call[pluginapi.RequestInterceptResponse](t, l, pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{RequestID: id})
		resp := call[pluginapi.RequestInterceptResponse](t, l, pluginabi.MethodRequestInterceptAfter, pluginapi.RequestInterceptRequest{
			RequestID: id, Model: fmt.Sprint("model-", i), Headers: http.Header{"Authorization": {fmt.Sprint("Bearer dummy-", i)}},
			Metadata: map[string]any{"selected_auth_index": "same-account", "caller_scope": fmt.Sprint(i), "canonical_session_id": fmt.Sprint(i)},
		})
		if resp.Terminate != (i == 1) {
			t.Fatal("model/client/session partitioned account capacity")
		}
	}
	if output := fmt.Sprint(l.snapshot()); strings.Contains(output, "dummy-") {
		t.Fatal("status retained client credentials")
	}
}
