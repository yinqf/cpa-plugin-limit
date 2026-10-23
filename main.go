package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const pluginName = "cpa-plugin-limit"

// pluginVersion is injected by the release build.
var pluginVersion = "0.1.0"

var state = newLimiter(time.Now)

func main() {}

type registration struct {
	SchemaVersion uint32          `json:"schema_version"`
	Metadata      pluginMetadata  `json:"metadata"`
	Capabilities  map[string]bool `json:"capabilities"`
}

// Preserve defaults for newer configuration editors without changing the v7 ABI.
type configField struct {
	pluginapi.ConfigField
	Default any
}

type pluginMetadata struct {
	pluginapi.Metadata
	ConfigFields []configField
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: 2,
		Metadata: pluginMetadata{
			Metadata: pluginapi.Metadata{
				Name: pluginName, Version: pluginVersion, Author: "yinqf",
				GitHubRepository: "https://github.com/yinqf/cpa-plugin-limit",
			},
			ConfigFields: []configField{
				{ConfigField: pluginapi.ConfigField{Name: "requests_per_minute", Type: pluginapi.ConfigFieldTypeInteger, Description: "每账户每分钟请求数：滚动 60 秒最多放行的尝试次数。默认 5，范围 0～10000，0 关闭限频；失败或完成不退还次数。留空恢复默认值。"}, Default: 5},
				{ConfigField: pluginapi.ConfigField{Name: "max_concurrency", Type: pluginapi.ConfigFieldTypeInteger, Description: "每账户最大并发数。默认 1，范围 0～10000，0 关闭并发限制；流式请求收到完成通知后释放名额。留空恢复默认值。"}, Default: 1},
				{ConfigField: pluginapi.ConfigField{Name: "auth_indexes", Type: pluginapi.ConfigFieldTypeArray, Description: "限制范围：在此填写 JSON 数组，例如 [\"codex-index-a\",\"codex-index-b\"]。留空或 [] 限制所有供应商凭据；只限制 Codex 时填写对应凭据索引，也支持 selected_auth_id。"}, Default: []string{}},
				{ConfigField: pluginapi.ConfigField{Name: "account_groups", Type: pluginapi.ConfigFieldTypeObject, Description: "账户合并：在此填写 JSON 对象，例如 {\"index-a\":\"codex-a\",\"index-b\":\"codex-a\"}。同一真实账户的多份凭据共用计数。留空或 {} 不合并；修改前需等在途请求结束和最近 60 秒记录过期。"}, Default: map[string]string{}},
				{ConfigField: pluginapi.ConfigField{Name: "account_limits", Type: pluginapi.ConfigFieldTypeObject, Description: "单账户阈值：在此填写 JSON 对象，例如 {\"codex-a\":{\"requests_per_minute\":5,\"max_concurrency\":2}}。键为分组名或未分组凭据索引；未填字段继承全局值，0 关闭对应限制。留空或 {} 使用全局配置。"}, Default: map[string]any{}},
			},
		},
		Capabilities: map[string]bool{"request_interceptor": true, "request_lifecycle_plugin": true, "management_api": true},
	}
}

func handle(method string, raw []byte) ([]byte, error) {
	return state.handle(method, raw)
}

func (l *limiter) handle(method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		cfg, err := parseConfig(raw)
		if err != nil {
			return nil, err
		}
		if err := l.configure(cfg); err != nil {
			return nil, err
		}
		return ok(pluginRegistration())
	case pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter:
		var req pluginapi.RequestInterceptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return ok(termination(unavailable("limiter_invalid_request", "The host supplied an invalid request interception payload."), ""))
		}
		var result decision
		if method == pluginabi.MethodRequestInterceptBefore {
			result = l.begin(strings.TrimSpace(req.RequestID))
		} else {
			result = l.admit(strings.TrimSpace(req.RequestID), metadataString(req.Metadata, "selected_auth_index"), metadataString(req.Metadata, "selected_auth_id"))
		}
		return ok(termination(result, req.SourceFormat))
	case pluginabi.MethodRequestComplete:
		var req pluginapi.RequestCompletion
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("decode request completion: %w", err)
		}
		l.complete(strings.TrimSpace(req.RequestID))
		return ok(struct{}{})
	case pluginabi.MethodManagementRegister:
		return ok(pluginapi.ManagementRegistrationResponse{Resources: []pluginapi.ResourceRoute{
			{Path: "/ui", Menu: "账户限流监控", Description: "上游账户并发、最近一分钟请求数和限流统计"},
			{Path: "/status"},
		}})
	case pluginabi.MethodManagementHandle:
		return l.management(raw)
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		l.quiesce()
		return ok(struct{}{})
	default:
		return nil, fmt.Errorf("unsupported method %q", method)
	}
}

func metadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func termination(result decision, sourceFormat string) pluginapi.RequestInterceptResponse {
	if result.status == 0 {
		// An empty response leaves the body, headers, and affinity metadata intact.
		return pluginapi.RequestInterceptResponse{}
	}
	errType := "server_error"
	if result.status == http.StatusTooManyRequests {
		errType = "rate_limit_error"
	}
	errorDetail := map[string]string{"type": errType, "code": result.code, "message": result.message}
	body := map[string]any{"error": errorDetail}
	if strings.EqualFold(sourceFormat, "claude") {
		body["type"] = "error"
		if result.status != http.StatusTooManyRequests {
			errorDetail["type"] = "api_error"
		}
	}
	encoded, _ := json.Marshal(body)
	headers := http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}}
	if seconds := retrySeconds(result.retryAfter); seconds > 0 {
		headers.Set("Retry-After", strconv.Itoa(seconds))
	}
	return pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: result.status, ResponseHeaders: headers, ResponseBody: encoded}
}

func ok(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

func rpcFailure(method, message string) []byte {
	if method == pluginabi.MethodRequestInterceptBefore || method == pluginabi.MethodRequestInterceptAfter {
		// Returning an RPC error here would make CPA skip the interceptor.
		raw, _ := ok(termination(unavailable("limiter_internal_error", "The account limiter could not safely admit this request."), ""))
		return raw
	}
	raw, _ := pluginabi.NewErrorEnvelope("plugin_error", message)
	return raw
}
