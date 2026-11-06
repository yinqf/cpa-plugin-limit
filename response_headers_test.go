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
