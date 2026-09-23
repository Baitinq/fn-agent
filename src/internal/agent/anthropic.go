package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

type anthropicContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  *string         `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	Data      string          `json:"data,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicMessage struct {
	Role    string             `json:"role"`
	Content []anthropicContent `json:"content"`
}

type anthropicRequest struct {
	Model        string             `json:"model"`
	MaxTokens    int64              `json:"max_tokens"`
	System       string             `json:"system,omitempty"`
	Messages     []anthropicMessage `json:"messages"`
	Tools        []any              `json:"tools,omitempty"`
	Thinking     map[string]any     `json:"thinking,omitempty"`
	OutputConfig map[string]any     `json:"output_config,omitempty"`
	Stream       bool               `json:"stream"`
}

type anthropicEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message,omitempty"`
	ContentBlock *anthropicContent `json:"content_block,omitempty"`
	Delta        struct {
		Type        string `json:"type"`
		Text        string `json:"text,omitempty"`
		Thinking    string `json:"thinking,omitempty"`
		Signature   string `json:"signature,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
	} `json:"delta,omitempty"`
	Usage anthropicUsage `json:"usage,omitempty"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

type anthropicAPIError struct {
	StatusCode int
	Message    string
}

func (e *anthropicAPIError) Error() string { return e.Message }

func anthropicMessages(history []historyItem, model string) []anthropicMessage {
	var messages []anthropicMessage
	appendContent := func(role string, content anthropicContent) {
		if len(messages) > 0 && messages[len(messages)-1].Role == role {
			messages[len(messages)-1].Content = append(messages[len(messages)-1].Content, content)
			return
		}
		messages = append(messages, anthropicMessage{Role: role, Content: []anthropicContent{content}})
	}
	for _, item := range history {
		sameModel := item.Provider == "anthropic" && item.Model == model
		switch item.Type {
		case "message":
			role := "user"
			if item.Role == "assistant" {
				role = "assistant"
			}
			appendContent(role, anthropicContent{Type: "text", Text: item.Text})
		case "reasoning":
			if sameModel && item.RedactedThinking != "" {
				appendContent("assistant", anthropicContent{Type: "redacted_thinking", Data: item.RedactedThinking})
			} else if sameModel && item.ThoughtSignature != "" {
				appendContent("assistant", anthropicContent{Type: "thinking", Thinking: &item.Text, Signature: item.ThoughtSignature})
			} else if item.Text != "" {
				appendContent("assistant", anthropicContent{Type: "text", Text: item.Text})
			}
		case "tool_call":
			appendContent("assistant", anthropicContent{Type: "tool_use", ID: anthropicToolID(item), Name: item.Name, Input: item.Arguments})
		case "tool_result":
			appendContent("user", anthropicContent{Type: "tool_result", ToolUseID: anthropicToolID(item), Content: item.Text, IsError: item.ToolError})
		}
	}
	return messages
}

func anthropicToolID(item historyItem) string {
	var id strings.Builder
	for _, r := range item.CallID {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			id.WriteRune(r)
		}
		if id.Len() == 64 {
			break
		}
	}
	if id.Len() == 0 {
		return "tool_call"
	}
	return id.String()
}

func anthropicAdaptiveThinking(model string) bool {
	for _, prefix := range []string{"claude-fable-5", "claude-opus-4-6", "claude-opus-4-7", "claude-opus-4-8", "claude-opus-5", "claude-sonnet-4-6", "claude-sonnet-5"} {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

func anthropicEffort(effort string) string {
	if effort == "minimal" {
		return "low"
	}
	if effort == "" {
		return "medium"
	}
	return effort
}

func anthropicThinkingBudget(effort string) int64 {
	switch effort {
	case "minimal":
		return 1024
	case "low":
		return 2048
	case "high":
		return 8192
	default:
		return 4096
	}
}

func (a *Agent) streamAnthropic(ctx context.Context, request modelRequest, emit func(ToolEvent)) (modelResponse, error) {
	maxTokens := request.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = 16384
	}
	budget := anthropicThinkingBudget(request.ReasoningEffort)
	if budget >= maxTokens {
		budget = maxTokens - 1
	}
	payload := anthropicRequest{
		Model: a.modelName, MaxTokens: maxTokens, System: request.Instructions,
		Messages: anthropicMessages(request.History, a.modelName), Stream: true,
	}
	if anthropicAdaptiveThinking(a.modelName) {
		payload.Thinking = map[string]any{"type": "adaptive"}
		payload.OutputConfig = map[string]any{"effort": anthropicEffort(request.ReasoningEffort)}
	} else {
		payload.Thinking = map[string]any{"type": "enabled", "budget_tokens": budget}
	}
	if request.Tools {
		payload.Tools = []any{map[string]any{
			"name":        "repl",
			"description": "Execute Python code in a persistent async REPL with preloaded shell(), web_search(), and llm() host functions.",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"code": map[string]any{"type": "string", "description": "Python code to execute. Variables and imports persist across calls."}},
				"required":   []string{"code"},
			},
		}}
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.baseURL, "/")+"/v1/messages", bytes.NewReader(data))
	if err != nil {
		return modelResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", os.Getenv("ANTHROPIC_API_KEY"))
	req.Header.Set("anthropic-version", "2023-06-01")
	if !anthropicAdaptiveThinking(a.modelName) {
		req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	}
	response, err := a.httpClient.Do(req)
	if err != nil {
		return modelResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		body, _ := io.ReadAll(response.Body)
		return modelResponse{}, &anthropicAPIError{StatusCode: response.StatusCode, Message: fmt.Sprintf("anthropic API %s: %s", response.Status, strings.TrimSpace(string(body)))}
	}

	var result modelResponse
	var inputUsage anthropicUsage
	blocks := map[int]int{}
	toolArguments := map[int]string{}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event anthropicEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			return modelResponse{}, err
		}
		if event.Error != nil {
			return modelResponse{}, &anthropicAPIError{Message: fmt.Sprintf("anthropic API %s: %s", event.Error.Type, event.Error.Message)}
		}
		switch event.Type {
		case "message_start":
			if event.Message != nil {
				inputUsage = event.Message.Usage
			}
		case "content_block_start":
			if event.ContentBlock == nil {
				continue
			}
			block := *event.ContentBlock
			item := historyItem{Provider: "anthropic", Model: a.modelName}
			switch block.Type {
			case "text":
				item.Type, item.Role, item.Text = "message", "assistant", block.Text
			case "thinking":
				item.Type, item.Text, item.ThoughtSignature = "reasoning", *block.Thinking, block.Signature
			case "redacted_thinking":
				item.Type, item.RedactedThinking = "reasoning", block.Data
			case "tool_use":
				item.Type, item.CallID, item.Name, item.Arguments = "tool_call", block.ID, block.Name, block.Input
			default:
				continue
			}
			blocks[event.Index] = len(result.Items)
			result.Items = append(result.Items, item)
		case "content_block_delta":
			itemIndex, ok := blocks[event.Index]
			if !ok {
				continue
			}
			item := &result.Items[itemIndex]
			switch event.Delta.Type {
			case "text_delta":
				item.Text += event.Delta.Text
				result.Text += event.Delta.Text
				emit(ToolEvent{Kind: ToolEventTextDelta, Detail: event.Delta.Text})
			case "thinking_delta":
				item.Text += event.Delta.Thinking
				emit(ToolEvent{Kind: ToolEventReasoningDelta, Detail: event.Delta.Thinking})
			case "signature_delta":
				item.ThoughtSignature += event.Delta.Signature
			case "input_json_delta":
				toolArguments[event.Index] += event.Delta.PartialJSON
			}
		case "content_block_stop":
			itemIndex, ok := blocks[event.Index]
			if ok && result.Items[itemIndex].Type == "tool_call" {
				if arguments := toolArguments[event.Index]; arguments != "" {
					result.Items[itemIndex].Arguments = json.RawMessage(arguments)
				}
				result.ToolCalls = append(result.ToolCalls, result.Items[itemIndex])
			}
		case "message_delta":
			input := inputUsage.InputTokens + inputUsage.CacheCreationInputTokens + inputUsage.CacheReadInputTokens
			result.Usage = Usage{InputTokens: input, CachedInputTokens: inputUsage.CacheReadInputTokens, OutputTokens: event.Usage.OutputTokens, TotalTokens: input + event.Usage.OutputTokens}
		}
	}
	if err := scanner.Err(); err != nil {
		return modelResponse{}, err
	}
	if len(result.Items) == 0 {
		return modelResponse{}, fmt.Errorf("stream ended without a completed response")
	}
	a.recordUsage(result.Usage, emit)
	return result, nil
}
