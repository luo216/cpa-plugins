package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// buildSearchMessagesRequest 构造发给 2api 网关 /v1/search/messages 的
// Anthropic Messages 形态请求。网关侧按 messages.go 的约定解析：
// tools 里声明 web_search，user 消息为固定的指令前缀 + query。
func buildSearchMessagesRequest(query string, maxResults int, model string) string {
	if model == "" {
		model = "web-search"
	}
	var buf bytes.Buffer
	buf.WriteString(`{"model":`)
	writeJSONString(&buf, model)
	buf.WriteString(`,"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":`)
	fmt.Fprintf(&buf, "%d", clampMaxResults(maxResults))
	buf.WriteString(`}],"messages":[{"role":"user","content":[{"type":"text","text":"Perform a web search for the query: `)
	// query 走 JSON 字符串转义，前缀是固定文本不含需转义字符。
	buf.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(query))
	buf.WriteString(`"}]}]}`)
	return buf.String()
}

func clampMaxResults(n int) int {
	if n <= 0 {
		return 5
	}
	if n > 10 {
		return 10
	}
	return n
}

func writeJSONString(buf *bytes.Buffer, s string) {
	encoded, _ := json.Marshal(s)
	buf.Write(encoded)
}

// messagesJSONToClaudeSSE 把网关返回的单条 Messages JSON 转成
// content_block_* / message_* 的 SSE 事件序列，供流式客户端消费。
// 逐块转发：server_tool_use（若网关带了）、web_search_tool_result、
// text 块按原顺序输出。
func messagesJSONToClaudeSSE(raw []byte) []byte {
	model := gjson.GetBytes(raw, "model").String()
	if model == "" {
		model = "web-search"
	}
	msgID := gjson.GetBytes(raw, "id").String()
	if msgID == "" {
		msgID = "msg_wbsearch_1"
	}
	var chunks []string
	chunks = append(chunks, sseEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "content": []any{},
			"model": model, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}))

	var outputTokens int
	var stopReason any = "end_turn"
	content := gjson.GetBytes(raw, "content")
	if content.IsArray() {
		for idx, block := range content.Array() {
			blockType := block.Get("type").String()
			switch blockType {
			case "text":
				text := block.Get("text").String()
				outputTokens += estimateTokens(text)
				chunks = append(chunks, sseEvent("content_block_start", map[string]any{
					"type": "content_block_start", "index": idx,
					"content_block": map[string]any{"type": "text", "text": ""},
				}))
				chunks = append(chunks, sseEvent("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": idx,
					"delta": map[string]any{"type": "text_delta", "text": text},
				}))
				chunks = append(chunks, sseEvent("content_block_stop", map[string]any{
					"type": "content_block_stop", "index": idx,
				}))
			case "web_search_tool_result", "server_tool_use":
				// content 里已是完整的 Anthropic 块，直接作为 content_block_start 的载荷。
				var blockPayload any
				if errUnmarshal := json.Unmarshal([]byte(block.Raw), &blockPayload); errUnmarshal == nil {
					chunks = append(chunks, sseEvent("content_block_start", map[string]any{
						"type": "content_block_start", "index": idx,
						"content_block": blockPayload,
					}))
				}
				chunks = append(chunks, sseEvent("content_block_stop", map[string]any{
					"type": "content_block_stop", "index": idx,
				}))
			default:
				// 未知块（如 citations 载体）也原样透出，客户端自行忽略。
				var blockPayload any
				if errUnmarshal := json.Unmarshal([]byte(block.Raw), &blockPayload); errUnmarshal == nil {
					chunks = append(chunks, sseEvent("content_block_start", map[string]any{
						"type": "content_block_start", "index": idx,
						"content_block": blockPayload,
					}))
					chunks = append(chunks, sseEvent("content_block_stop", map[string]any{
						"type": "content_block_stop", "index": idx,
					}))
				}
			}
		}
	}
	if sr := gjson.GetBytes(raw, "stop_reason"); sr.Exists() {
		stopReason = sr.Value()
	}
	chunks = append(chunks, sseEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{
			"input_tokens": 0, "output_tokens": outputTokens,
			"server_tool_use": map[string]any{"web_search_requests": 1},
		},
	}))
	chunks = append(chunks, sseEvent("message_stop", map[string]any{"type": "message_stop"}))
	return []byte(strings.Join(chunks, ""))
}

func sseEvent(eventType string, data map[string]any) string {
	raw, _ := json.Marshal(data)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, string(raw))
}

func estimateTokens(text string) int {
	n := len([]rune(text)) / 4
	if n < 1 {
		return 1
	}
	return n
}

type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func emitPluginStreamChunk(streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return fmt.Errorf("plugin stream id is required")
	}
	_, errCall := callHost(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{
		StreamID: streamID,
		Payload:  payload,
	})
	return errCall
}

func closePluginStream(streamID, errMsg string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	_, _ = callHost(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{
		StreamID: streamID,
		Error:    strings.TrimSpace(errMsg),
	})
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

var _ = http.MethodPost
