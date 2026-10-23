package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

//go:embed ui.html
var statusPage []byte

func (l *limiter) management(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return resourceResponse(http.StatusBadRequest, "application/json", []byte(`{"error":"invalid resource request"}`))
	}
	if req.Method != "" && req.Method != http.MethodGet {
		return resourceResponse(http.StatusMethodNotAllowed, "application/json", []byte(`{"error":"method not allowed"}`))
	}
	path := strings.TrimSuffix(req.Path, "/")
	path = strings.TrimPrefix(path, "/v0/resource/plugins/"+pluginName)
	switch path {
	case "/ui":
		return resourceResponse(http.StatusOK, "text/html; charset=utf-8", statusPage)
	case "/status":
		body, err := json.Marshal(l.monitorSnapshot())
		if err != nil {
			return nil, err
		}
		return resourceResponse(http.StatusOK, "application/json; charset=utf-8", body)
	default:
		return resourceResponse(http.StatusNotFound, "application/json", []byte(`{"error":"resource not found"}`))
	}
}

func resourceResponse(status int, contentType string, body []byte) ([]byte, error) {
	return ok(pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{
		"Content-Type": {contentType}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"},
	}, Body: body})
}
