package main

/*
#include "cliproxy_plugin.h"

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
extern void store_host_api_go(cliproxy_host_api*);
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const pluginIdentifier = "workbuddy-web-search"

var currentConfig atomic.Value

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

// pluginConfig 与 config.yaml 里 plugins.configs.workbuddy-web-search 对应。
type pluginConfig struct {
	Enabled    bool   `yaml:"enabled"`
	// Endpoint 是 2api 网关的 Anthropic Messages 搜索端点，形如
	// http://127.0.0.1:7863/v1/search/messages。任何实现该协议壳的
	// "2api"（WorkBuddy 桌面端逆向、其他中转）都可接。
	Endpoint   string `yaml:"endpoint"`
	APIKey     string `yaml:"api_key"`
	// TimeoutSeconds 单次搜索超时。
	TimeoutSeconds int `yaml:"timeout_seconds"`
}

func (c pluginConfig) timeout() time.Duration {
	if c.TimeoutSeconds <= 0 {
		return 30 * time.Second
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcModelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	store_host_api_go(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)

	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodModelRoute:
		return routeModel(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginIdentifier})
	case pluginabi.MethodExecutorExecute:
		return execute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return executeStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":0}`)})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg := defaultPluginConfig()
	if len(req.ConfigYAML) > 0 {
		decoded, errDecode := decodeConfig(req.ConfigYAML)
		if errDecode != nil {
			return errDecode
		}
		cfg = decoded
	}
	currentConfig.Store(cfg)
	return nil
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:        true,
		TimeoutSeconds: 30,
	}
}

func decodeConfig(raw []byte) (pluginConfig, error) {
	cfg := defaultPluginConfig()
	if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
		return pluginConfig{}, errUnmarshal
	}
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	return cfg, nil
}

func loadedConfig() pluginConfig {
	raw := currentConfig.Load()
	if cfg, ok := raw.(pluginConfig); ok {
		return cfg
	}
	return defaultPluginConfig()
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginIdentifier,
			Version:          "0.1.0",
			Author:           "steve",
			GitHubRepository: "https://github.com/steve/cpa-plugin-workbuddy-web-search",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "When false, the router declines all Claude web_search requests."},
				{Name: "endpoint", Type: pluginapi.ConfigFieldTypeString, Description: "Anthropic Messages-compatible search endpoint of the 2api gateway, e.g. http://127.0.0.1:7863/v1/search/messages."},
				{Name: "api_key", Type: pluginapi.ConfigFieldTypeString, Description: "Bearer token for the 2api gateway endpoint."},
				{Name: "timeout_seconds", Type: pluginapi.ConfigFieldTypeNumber, Description: "Per-search timeout in seconds (default 30)."},
			},
		},
		Capabilities: registrationCapability{
			ModelRouter:           true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeStatic),
			ExecutorInputFormats:  []string{"claude"},
			ExecutorOutputFormats: []string{"claude"},
		},
	}
}

// routeModel 拦截判定：Claude 协议入站、声明了 web_search 工具、后端配置可用。
// 只在「工具列表里只有 web_search」时接管（Claude Code 的搜索子请求满足这一点），
// 混合普通工具的请求仍走模型对话路径，避免误伤。
func routeModel(raw []byte) ([]byte, error) {
	var req rpcModelRouteRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	if !cfg.Enabled || cfg.Endpoint == "" {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	if !isClaudeSourceFormat(req.SourceFormat) {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	if !isClaudeCodeWebSearchRequest(req.Body) {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	return okEnvelope(pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "claude_code_web_search_workbuddy",
	})
}

func execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	payload, headers, errRun := runSearch(context.Background(), req.ExecutorRequest)
	if errRun != nil {
		return errorEnvelope("executor_error", errRun.Error()), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: payload, Headers: headers})
}

func executeStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream"), nil
	}
	body, headers, errRun := runSearch(context.Background(), req.ExecutorRequest)
	if errRun != nil {
		closePluginStream(streamID, errRun.Error())
		return errorEnvelope("executor_error", errRun.Error()), nil
	}
	if errEmit := emitPluginStreamChunk(streamID, body); errEmit != nil {
		closePluginStream(streamID, errEmit.Error())
		return errorEnvelope("executor_error", errEmit.Error()), nil
	}
	closePluginStream(streamID, "")
	_ = headers
	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

// runSearch 调 2api 网关的 Messages 兼容搜索端点，返回可直接透传给
// Claude Code 的 SSE 事件流（或非流式 JSON）。
func runSearch(ctx context.Context, req pluginapi.ExecutorRequest) ([]byte, http.Header, error) {
	cfg := loadedConfig()
	query := extractQuery(req.OriginalRequest)
	if query == "" {
		query = extractQuery(req.Payload)
	}
	if strings.TrimSpace(query) == "" {
		return nil, nil, fmt.Errorf("no web search query found in request")
	}

	maxResults := extractMaxUses(req.OriginalRequest, 5)
	body := buildSearchMessagesRequest(query, maxResults, strings.TrimSpace(req.Model))
	reqCtx, cancel := context.WithTimeout(ctx, cfg.timeout())
	defer cancel()
	httpReq, errNew := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.Endpoint, strings.NewReader(body))
	if errNew != nil {
		return nil, nil, errNew
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	httpReq.Header.Set("User-Agent", claudeCodeUserAgent)

	resp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		return nil, nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, nil, errRead
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("workbuddy search http %d: %s", resp.StatusCode, truncate(string(raw), 512))
	}
	// 网关返回的是单条 Messages JSON；流式请求需要转成 SSE 事件序列。
	if req.Stream {
		sse := messagesJSONToClaudeSSE(raw)
		return sse, http.Header{"Content-Type": []string{"text/event-stream"}}, nil
	}
	return raw, http.Header{"Content-Type": []string{"application/json"}}, nil
}

var httpClient = &http.Client{}

// claudeCodeUserAgent 与 Claude Code CLI 的 UA 保持一致，便于下游网关
// 按客户端指纹做兼容处理。
const claudeCodeUserAgent = "claude-cli/2.1.310 (external, cli)"

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
