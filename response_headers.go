package main

import (
	"net/http"
	"strings"
)

const (
	accountEmailHeader = "X-CPA-Account-Email"
	authIndexHeader    = "X-CPA-Auth-Index"
)

// Ignore bodies and history: diagnostics never transform or buffer response data.
type accountResponseRequest struct {
	RequestID  string
	ChunkIndex int
	Metadata   map[string]any
}

type responseAccountIdentity struct {
	index string
	id    string
}

func accountResponseHeaderNames() []string {
	return []string{accountEmailHeader, authIndexHeader}
}

func (l *limiter) accountResponseHeaders(req accountResponseRequest) http.Header {
	headers := make(http.Header)
	index := metadataString(req.Metadata, "selected_auth_index")
	id := metadataString(req.Metadata, "selected_auth_id")
	_, hasIndex := req.Metadata["selected_auth_index"]
	_, hasID := req.Metadata["selected_auth_id"]
	// Some hosts omit selection metadata from response hooks. The after-auth
	// admission belongs to this lifecycle ID and tracks the latest attempt,
	// including credentials outside the limiter's scope. Never mix identities
	// from explicit metadata with the remembered pair.
	if !hasIndex && !hasID {
		l.mu.Lock()
		if tracked := l.requests[strings.TrimSpace(req.RequestID)]; tracked != nil {
			index, id = tracked.selected.index, tracked.selected.id
		}
		l.mu.Unlock()
	}
	index = accountHeaderValue(index)
	if index == "" && id == "" {
		return headers
	}
	now := l.now()
	emails, _ := l.directory.lookup(now)
	var idIndex string
	if id != "" {
		idIndex = accountHeaderValue(l.directory.lookupIndexes(now)[id])
	}
	if index == "" {
		index = idIndex
	}
	if index != "" {
		headers.Set(authIndexHeader, index)
	}
	email := emails["index:"+index]
	if email == "" && id != "" && (index == "" || idIndex == index) {
		email = emails["id:"+id]
	}
	// Conflicting identity metadata must never label a response with another account.
	if idIndex != "" && index != idIndex {
		email = ""
	}
	if email != "" {
		headers.Set(accountEmailHeader, email)
	}
	return headers
}

func accountHeaderValue(value string) string {
	if len(value) > 512 {
		return ""
	}
	for i := range value {
		if value[i] < 32 || value[i] == 127 {
			return ""
		}
	}
	return value
}
