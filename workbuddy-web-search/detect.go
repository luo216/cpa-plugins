package main

import (
	"strings"

	"github.com/tidwall/gjson"
)

const (
	claudeWebSearchToolTypeA = "web_search_20250305"
	claudeWebSearchToolTypeB = "web_search_20260209"
)

// isClaudeSourceFormat 判断入站协议是否为 Claude / Anthropic Messages。
func isClaudeSourceFormat(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "claude", "anthropic":
		return true
	default:
		return false
	}
}

func isClaudeTypedWebSearchToolType(toolType string) bool {
	return toolType == claudeWebSearchToolTypeA || toolType == claudeWebSearchToolTypeB
}

// isClaudeCodeWebSearchRequest 判定 Claude Code 的 web_search 子请求：
// 工具列表里声明了 web_search 类型工具（Claude Code 的搜索子请求只带这一个工具）。
func isClaudeCodeWebSearchRequest(body []byte) bool {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() || len(tools.Array()) == 0 {
		return false
	}
	hasWebSearch := false
	for _, tool := range tools.Array() {
		if isClaudeTypedWebSearchToolType(tool.Get("type").String()) {
			hasWebSearch = true
			continue
		}
		// 出现任何其他工具：不是纯搜索子请求，交回默认路由。
		if tool.Get("type").String() != "" || tool.Get("name").String() != "" {
			return false
		}
	}
	return hasWebSearch
}

// extractQuery 从 Claude Code 搜索子请求里还原搜索词。
// 约定文本为 "Perform a web search for the query: <query>"；若客户端
// 形态不同则退化为取最后一条 user 消息文本。
func extractQuery(body []byte) string {
	if q := extractQueryFromPerformPrefix(body); q != "" {
		return q
	}
	return extractQueryFromUserMessages(body)
}

func extractQueryFromPerformPrefix(body []byte) string {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return ""
	}
	const prefix = "perform a web search for the query:"
	for _, message := range messages.Array() {
		if message.Get("role").String() != "user" {
			continue
		}
		text := strings.TrimSpace(extractClaudeMessageText(message.Get("content")))
		lower := strings.ToLower(text)
		if strings.HasPrefix(lower, prefix) {
			return strings.TrimSpace(text[len(prefix):])
		}
	}
	return ""
}

func extractQueryFromUserMessages(body []byte) string {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return ""
	}
	arr := messages.Array()
	for i := len(arr) - 1; i >= 0; i-- {
		message := arr[i]
		role := message.Get("role").String()
		if role != "" && role != "user" {
			continue
		}
		if query := strings.TrimSpace(extractClaudeMessageText(message.Get("content"))); query != "" {
			return query
		}
	}
	return ""
}

func extractClaudeMessageText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	var parts []string
	for _, block := range content.Array() {
		if block.Get("type").String() != "text" {
			continue
		}
		if text := strings.TrimSpace(block.Get("text").String()); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

// extractMaxUses 读 web_search 工具的 max_uses（默认 5）。
func extractMaxUses(body []byte, defaultMax int) int {
	if defaultMax <= 0 {
		defaultMax = 5
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return defaultMax
	}
	for _, tool := range tools.Array() {
		if !isClaudeTypedWebSearchToolType(tool.Get("type").String()) {
			continue
		}
		if maxUses := int(tool.Get("max_uses").Int()); maxUses > 0 {
			return maxUses
		}
	}
	return defaultMax
}
