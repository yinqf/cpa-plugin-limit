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
				{ConfigField: pluginapi.ConfigField{Name: "auth_indexes", Type: pluginapi.ConfigFieldTypeArray, Description: "限制范围：填写凭据索引组成的 JSON 数组。留空或 [] 限制所有供应商凭据；只限制 Codex 时填写对应凭据索引，也支持 selected_auth_id。完整示例见账户限流监控页面的配置填写示例。"}, Default: []string{}},
				{ConfigField: pluginapi.ConfigField{Name: "account_groups", Type: pluginapi.ConfigFieldTypeObject, Description: "账户合并：填写凭据索引到分组名的 JSON 对象，同一真实账户的多份凭据映射到同一分组名，共用计数。留空或 {} 不合并；修改前需等在途请求结束和最近 60 秒记录过期。完整示例见账户限流监控页面的配置填写示例。"}, Default: map[string]string{}},
				{ConfigField: pluginapi.ConfigField{Name: "account_limits", Type: pluginapi.ConfigFieldTypeObject, Description: "单账户阈值：填写以分组名或未分组凭据索引为键的 JSON 对象。未填字段继承全局值，0 关闭对应限制。留空或 {} 使用全局配置。完整示例见账户限流监控页面的配置填写示例。"}, Default: map[string]any{}},
				{ConfigField: pluginapi.ConfigField{Name: "account_display", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{displayEmail, displayMaskedEmail, displayFingerprint}, Description: "监控账户显示方式：email 为完整邮箱，默认值；masked_email 为脱敏邮箱；fingerprint 为账户指纹。邮箱模式不显示指纹，无邮箱时显示未提供邮箱。监控接口无需管理登录，请在网关限制访问。仅影响显示，不改变账户分组和计数。"}, Default: displayEmail},
				{ConfigField: pluginapi.ConfigField{Name: "routing_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{routingCapacity, routingGuard}, Description: "调度模式：capacity-aware 默认在选账户前检查容量，保留可用账户的会话亲和，满额后切换其他可用账户；guard-only 仅在选定账户后拦截，不切换账户。CPA 同时只采用一个调度插件；切换模式后重新加载插件。"}, Default: routingCapacity},
				{ConfigField: pluginapi.ConfigField{Name: "session_affinity_ttl_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "插件会话亲和缓存有效期，默认 3600 秒，范围 0～86400；每次命中刷新有效期，0 关闭亲和。仅 capacity-aware 模式生效；账户满额时切换并更新绑定。"}, Default: 3600},
			},
		},
		Capabilities: map[string]bool{"scheduler": true, "scheduler_across_priorities": true, "request_interceptor": true, "request_lifecycle_plugin": true, "management_api": true},
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
		reg := pluginRegistration()
		reg.Capabilities["scheduler"] = cfg.RoutingMode == routingCapacity
		return ok(reg)
	case pluginabi.MethodSchedulerPick:
		var req pluginapi.SchedulerPickRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("invalid scheduler request")
		}
		return ok(l.pick(req))
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
		response := termination(result, req.SourceFormat)
		if method == pluginabi.MethodRequestInterceptBefore && result.status == 0 {
			session := requestSession(req)
			l.mu.Lock()
			if tracked := l.requests[strings.TrimSpace(req.RequestID)]; tracked != nil && l.cfg.RoutingMode == routingCapacity {
				tracked.session = session
				response.Headers = http.Header{routingHeader: {strings.TrimSpace(req.RequestID)}}
			}
			l.mu.Unlock()
		}
		if method == pluginabi.MethodRequestInterceptAfter {
			response.ClearHeaders = []string{routingHeader}
		}
		return ok(response)
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
