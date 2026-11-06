package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAccountHeadersIdentifyEachSelectedStreamAccount(t *testing.T) {
	l, _ := testLimiter()
	lookups := 0
	l.directory.setLoader(func() ([]credentialEmail, error) {
		lookups++
		return []credentialEmail{
			{ID: "auth-a", AuthIndex: "index-a", Email: "alice@example.com"},
			{ID: "auth-b", AuthIndex: "index-b", Email: "bob@example.com"},
		}, nil
	})
	for _, mode := range []string{routingCapacity, routingGuard} {
		cfg := defaultConfig()
		cfg.RoutingMode, cfg.AccountDisplay = mode, displayFingerprint
		configure(t, l, cfg)
		for _, account := range []struct{ id, index, email string }{
			{"auth-a", "index-a", "alice@example.com"},
			{"auth-b", "index-b", "bob@example.com"},
		} {
			response := call[pluginapi.StreamChunkInterceptResponse](t, l, pluginabi.MethodResponseInterceptStreamChunk, pluginapi.StreamChunkInterceptRequest{
				RequestID: "same-cpa-request", ChunkIndex: pluginapi.StreamChunkHeaderInitIndex,
				ResponseHeaders: http.Header{"Content-Type": {"text/event-stream"}, accountEmailHeader: {"stale@example.com"}},
				Metadata:        map[string]any{"selected_auth_id": account.id, "selected_auth_index": account.index},
			})
			if response.Headers.Get(accountEmailHeader) != account.email || response.Headers.Get(authIndexHeader) != account.index {
				t.Fatalf("wrong selected account in %s: %+v", mode, response)
			}
			if len(response.Headers) != 2 || !reflect.DeepEqual(response.ClearHeaders, accountResponseHeaderNames()) || len(response.Body) != 0 || response.DropChunk {
				t.Fatalf("stream data or unrelated headers were modified: %+v", response)
			}
		}
	}
	if lookups != 1 || l.snapshot().Admitted != 0 {
		t.Fatalf("header hooks bypassed caching or affected limits: lookups=%d", lookups)
	}
}

func TestAccountHeadersNonStreamingAndIdentityFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]any
		wantMail string
		wantIdx  string
	}{
		{"selected index", map[string]any{"selected_auth_index": "index-a"}, "alice@example.com", "index-a"},
		{"ID resolves index", map[string]any{"selected_auth_id": "auth-a"}, "alice@example.com", "index-a"},
		{"runtime ID without index", map[string]any{"selected_auth_id": "runtime"}, "runtime@example.com", ""},
		{"missing mailbox", map[string]any{"selected_auth_index": "unknown"}, "", "unknown"},
		{"missing selection", nil, "", ""},
		{"unrelated metadata", map[string]any{"email": "caller@example.com", "account": "private-secret"}, "", ""},
		{"conflicting selection", map[string]any{"selected_auth_index": "index-b", "selected_auth_id": "auth-a"}, "", "index-b"},
		{"invalid index", map[string]any{"selected_auth_index": "bad\r\nInjected: yes"}, "", ""},
		{"oversized index", map[string]any{"selected_auth_index": strings.Repeat("a", 513)}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := testLimiter()
			l.directory.setLoader(func() ([]credentialEmail, error) {
				return []credentialEmail{
					{ID: "auth-a", AuthIndex: "index-a", Email: "alice@example.com"},
					{ID: "auth-b", AuthIndex: "index-b", Email: "bob@example.com"},
					{ID: "runtime", Email: "runtime@example.com"},
				}, nil
			})
			for _, protocol := range []string{"openai-response", "openai", "claude"} {
				response := call[pluginapi.ResponseInterceptResponse](t, l, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
					SourceFormat: protocol, Metadata: tc.metadata, Body: []byte(`{"content":"unchanged"}`),
					ResponseHeaders: http.Header{accountEmailHeader: {"stale@example.com"}, authIndexHeader: {"stale-index"}},
				})
				if response.Headers.Get(accountEmailHeader) != tc.wantMail || response.Headers.Get(authIndexHeader) != tc.wantIdx || len(response.Body) != 0 {
					t.Fatalf("wrong account or modified body for %s: %+v", protocol, response)
				}
				if !reflect.DeepEqual(response.ClearHeaders, accountResponseHeaderNames()) {
					t.Fatalf("stale account headers were not cleared: %+v", response)
				}
			}
		})
	}
}

func TestAccountHeadersDoNotInspectOrChangePayloadChunks(t *testing.T) {
	l, _ := testLimiter()
	l.directory.setLoader(func() ([]credentialEmail, error) {
		t.Fatal("payload chunk queried the credential directory")
		return nil, nil
	})
	// Invalid base64 proves the lightweight decoder never interprets stream bodies.
	for _, body := range []string{"server_is_overloaded", "response.completed", "request_timeout"} {
		raw := []byte(fmt.Sprintf(`{"ChunkIndex":0,"Body":%q,"Metadata":{"selected_auth_index":"account"}}`, body))
		encoded, err := l.handle(pluginabi.MethodResponseInterceptStreamChunk, raw)
		if err != nil {
			t.Fatal(err)
		}
		var env pluginabi.Envelope
		if err := json.Unmarshal(encoded, &env); err != nil || !env.OK {
			t.Fatalf("invalid envelope: %s / %v", encoded, err)
		}
		var response pluginapi.StreamChunkInterceptResponse
		if err := json.Unmarshal(env.Result, &response); err != nil || len(response.Headers) != 0 || len(response.ClearHeaders) != 0 || len(response.Body) != 0 || response.DropChunk {
			t.Fatalf("payload chunk was modified: %+v / %v", response, err)
		}
	}
}

func TestAccountHeaderLookupFailureDoesNotBreakResponses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []credentialEmail
		err   error
	}{
		{name: "unavailable", err: fmt.Errorf("host unavailable")},
		{name: "ambiguous", files: []credentialEmail{{AuthIndex: "index", Email: "alice@example.com"}, {AuthIndex: "index", Email: "bob@example.com"}}},
		{name: "invalid email", files: []credentialEmail{{AuthIndex: "index", Email: "alice@example.com\r\nInjected: yes"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := testLimiter()
			l.directory.setLoader(func() ([]credentialEmail, error) { return tc.files, tc.err })
			response := call[pluginapi.StreamChunkInterceptResponse](t, l, pluginabi.MethodResponseInterceptStreamChunk, pluginapi.StreamChunkInterceptRequest{
				ChunkIndex: pluginapi.StreamChunkHeaderInitIndex, Metadata: map[string]any{"selected_auth_index": "index"},
			})
			if response.Headers.Get(accountEmailHeader) != "" || response.Headers.Get(authIndexHeader) != "index" || response.DropChunk || len(response.Body) != 0 {
				t.Fatalf("failed lookup broke response or invented an email: %+v", response)
			}
		})
	}
}

// CPA's executor copies selection metadata into a private attempt map. Response
// hooks can receive only the original metadata, but keep the same RequestID.
func TestAccountHeadersRecoverSelectionFromRequestLifecycle(t *testing.T) {
	for _, mode := range []string{routingCapacity, routingGuard} {
		for _, stream := range []bool{false, true} {
			for _, excluded := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/excluded=%t", mode, stream, excluded), func(t *testing.T) {
					l, _ := testLimiter()
					cfg := defaultConfig()
					cfg.RoutingMode = mode
					if excluded {
						cfg.AuthIndexes = []string{"another-index"}
					}
					configure(t, l, cfg)
					lookups := 0
					l.directory.setLoader(func() ([]credentialEmail, error) {
						lookups++
						return []credentialEmail{{ID: "auth-a", AuthIndex: "index-a", Email: "alice@example.com"}}, nil
					})
					startAccountHeaderRequest(t, l, "request-a", "index-a", "auth-a")
					if lookups != 0 {
						t.Fatal("admission queried the account directory")
					}
					var headers http.Header
					if stream {
						response := call[pluginapi.StreamChunkInterceptResponse](t, l, pluginabi.MethodResponseInterceptStreamChunk, pluginapi.StreamChunkInterceptRequest{
							RequestID: "request-a", ChunkIndex: pluginapi.StreamChunkHeaderInitIndex, Metadata: map[string]any{"requested_model": "test-model"},
						})
						headers = response.Headers
						if len(response.Body) != 0 || response.DropChunk {
							t.Fatalf("modified stream: %+v", response)
						}
					} else {
						response := call[pluginapi.ResponseInterceptResponse](t, l, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
							RequestID: "request-a", Metadata: map[string]any{"requested_model": "test-model"}, Body: []byte(`{"output":[]}`),
						})
						headers = response.Headers
						if len(response.Body) != 0 {
							t.Fatalf("modified response: %+v", response)
						}
					}
					if headers.Get(accountEmailHeader) != "alice@example.com" || headers.Get(authIndexHeader) != "index-a" || lookups != 1 {
						t.Fatalf("lost admitted account: headers=%v lookups=%d", headers, lookups)
					}
				})
			}
		}
	}
}

func TestAccountHeaderFallbackTracksLatestAttemptWithoutMixingRequests(t *testing.T) {
	l, _ := testLimiter()
	cfg := defaultConfig()
	cfg.MaxConcurrency = 0
	cfg.AccountGroups = map[string]string{"index-a": "same-group", "index-b": "same-group"}
	configure(t, l, cfg)
	l.directory.setLoader(func() ([]credentialEmail, error) {
		return []credentialEmail{
			{ID: "auth-a", AuthIndex: "index-a", Email: "alice@example.com"},
			{ID: "auth-b", AuthIndex: "index-b", Email: "bob@example.com"},
		}, nil
	})
	startAccountHeaderRequest(t, l, "first", "index-a", "auth-a")
	startAccountHeaderRequest(t, l, "second", "index-a", "auth-a")
	startAccountHeaderRequest(t, l, "first", "index-b", "auth-b")
	for _, tc := range []struct{ request, index, email string }{
		{"first", "index-b", "bob@example.com"},
		{"second", "index-a", "alice@example.com"},
		{"unrelated", "", ""},
	} {
		headers := accountHeaderFallback(t, l, tc.request, nil)
		if headers.Get(accountEmailHeader) != tc.email || headers.Get(authIndexHeader) != tc.index {
			t.Fatalf("wrong last attempt for %s: %v", tc.request, headers)
		}
	}
	l.complete("first")
	if headers := accountHeaderFallback(t, l, "first", nil); len(headers) != 0 {
		t.Fatalf("completed request retained its account: %v", headers)
	}
	if headers := accountHeaderFallback(t, l, "second", nil); headers.Get(accountEmailHeader) != "alice@example.com" {
		t.Fatalf("completion changed another request: %v", headers)
	}
}

func TestAccountHeaderFallbackDoesNotMergeExplicitSelection(t *testing.T) {
	l, _ := testLimiter()
	l.directory.setLoader(func() ([]credentialEmail, error) {
		return []credentialEmail{
			{ID: "auth-a", AuthIndex: "index-a", Email: "alice@example.com"},
			{ID: "auth-b", AuthIndex: "index-b", Email: "bob@example.com"},
		}, nil
	})
	startAccountHeaderRequest(t, l, "request", "index-a", "auth-a")
	for _, tc := range []struct {
		name     string
		metadata map[string]any
		index    string
		email    string
	}{
		{"index only", map[string]any{"selected_auth_index": "index-b"}, "index-b", "bob@example.com"},
		{"ID only", map[string]any{"selected_auth_id": "auth-b"}, "index-b", "bob@example.com"},
		{"conflicting pair", map[string]any{"selected_auth_id": "auth-a", "selected_auth_index": "index-b"}, "index-b", ""},
		{"empty selection", map[string]any{"selected_auth_index": ""}, "", ""},
		{"invalid selection", map[string]any{"selected_auth_index": "bad\r\nInjected: yes"}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := accountHeaderFallback(t, l, "request", tc.metadata)
			if headers.Get(accountEmailHeader) != tc.email || headers.Get(authIndexHeader) != tc.index {
				t.Fatalf("mixed explicit and remembered identity: %v", headers)
			}
		})
	}
}

func TestAccountHeaderFallbackClearsRejectedSelection(t *testing.T) {
	l, _ := testLimiter()
	startAccountHeaderRequest(t, l, "request", "index-a", "auth-a")
	response := call[pluginapi.RequestInterceptResponse](t, l, pluginabi.MethodRequestInterceptAfter, pluginapi.RequestInterceptRequest{RequestID: "request"})
	if !response.Terminate {
		t.Fatal("missing selection was admitted")
	}
	if headers := accountHeaderFallback(t, l, "request", nil); len(headers) != 0 {
		t.Fatalf("rejected attempt retained an earlier account: %v", headers)
	}
}

func startAccountHeaderRequest(t *testing.T, l *limiter, requestID, index, id string) {
	t.Helper()
	before := call[pluginapi.RequestInterceptResponse](t, l, pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{RequestID: requestID})
	after := call[pluginapi.RequestInterceptResponse](t, l, pluginabi.MethodRequestInterceptAfter, pluginapi.RequestInterceptRequest{
		RequestID: requestID, Metadata: map[string]any{"selected_auth_index": index, "selected_auth_id": id},
	})
	if before.Terminate || after.Terminate {
		t.Fatalf("request admission failed: before=%+v after=%+v", before, after)
	}
}

func accountHeaderFallback(t *testing.T, l *limiter, requestID string, metadata map[string]any) http.Header {
	t.Helper()
	return call[pluginapi.StreamChunkInterceptResponse](t, l, pluginabi.MethodResponseInterceptStreamChunk, pluginapi.StreamChunkInterceptRequest{
		RequestID: requestID, ChunkIndex: pluginapi.StreamChunkHeaderInitIndex, Metadata: metadata,
	}).Headers
}
