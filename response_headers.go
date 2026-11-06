package main

import "net/http"

const (
	accountEmailHeader = "X-CPA-Account-Email"
	authIndexHeader    = "X-CPA-Auth-Index"
)

// Ignore bodies and history: diagnostics never transform or buffer response data.
type accountResponseRequest struct {
	ChunkIndex int
	Metadata   map[string]any
}

func accountResponseHeaderNames() []string {
	return []string{accountEmailHeader, authIndexHeader}
}

func (l *limiter) accountResponseHeaders(metadata map[string]any) http.Header {
	headers := make(http.Header)
	index := accountHeaderValue(metadataString(metadata, "selected_auth_index"))
	id := metadataString(metadata, "selected_auth_id")
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
