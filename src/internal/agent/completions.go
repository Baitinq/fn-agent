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

type completionMessage struct {
	Role             string               `json:"role"`
	Content          string               `json:"content,omitempty"`
	ReasoningContent string               `json:"reasoning_content,omitempty"`
	ToolCalls        []completionToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string               `json:"tool_call_id,omitempty"`
	Name             string               `json:"name,omitempty"`
}

type completionToolCall struct {
	Index    int                `json:"index,omitempty"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function completionFunction `json:"function"`
}

type completionFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type completionRequest struct {
	Model           string              `json:"model"`
	Messages        []completionMessage `json:"messages"`
	Tools           []any               `json:"tools,omitempty"`
	ReasoningEffort string              `json:"reasoning_effort,omitempty"`
	MaxTokens       int64               `json:"max_tokens,omitempty"`
	Stream          bool                `json:"stream"`
	StreamOptions   map[string]bool     `json:"stream_options"`
}

type completionChunk struct {
	Choices []struct {
		Delta        completionMessage `json:"delta"`
		FinishReason string            `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		TotalTokens         int64 `json:"total_tokens"`
		PromptTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type completionAPIError struct {
	StatusCode int
	Message    string
}

func (e *completionAPIError) Error() string { return e.Message }

func completionMessages(instructions string, history []historyItem, model string) []completionMessage {
	messages := make([]completionMessage, 0, len(history)+1)
	if instructions != "" {
		messages = append(messages, completionMessage{Role: "system", Content: instructions})
	}
	assistant := func() *completionMessage {
		if len(messages) == 0 || messages[len(messages)-1].Role != "assistant" {
			messages = append(messages, completionMessage{Role: "assistant"})
		}
		return &messages[len(messages)-1]
	}
	for _, item := range history {
		switch item.Type {
		case "message":
			role := "user"
			if item.Role == "assistant" {
				role = "assistant"
			}
			messages = append(messages, completionMessage{Role: role, Content: item.Text})
		case "reasoning":
			if item.Provider == "openai-completions" && item.Model == model {
				assistant().ReasoningContent += item.Text
			}
		case "tool_call":
			message := assistant()
			message.ToolCalls = append(message.ToolCalls, completionToolCall{ID: item.CallID, Type: "function", Function: completionFunction{Name: item.Name, Arguments: string(item.Arguments)}})
		case "tool_result":
			messages = append(messages, completionMessage{Role: "tool", Content: item.Text, ToolCallID: item.CallID, Name: item.Name})
		}
	}
	return messages
}

func (a *Agent) streamCompletions(ctx context.Context, request modelRequest, emit func(ToolEvent)) (modelResponse, error) {
	payload := completionRequest{
		Model:           a.modelName,
		Messages:        completionMessages(request.Instructions, request.History, a.modelName),
		ReasoningEffort: request.ReasoningEffort,
		MaxTokens:       request.MaxOutputTokens,
		Stream:          true,
		StreamOptions:   map[string]bool{"include_usage": true},
	}
	if request.Tools {
		payload.Tools = []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "repl",
				"description": "Execute Python code in a persistent async REPL with preloaded shell(), web_search(), and llm() host functions.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{"code": map[string]any{
						"type":        "string",
						"description": "Python code to execute. Variables and imports persist across calls.",
					}},
					"required":             []string{"code"},
					"additionalProperties": false,
				},
			},
		}}
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.baseURL, "/")+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return modelResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+envOrDefault("FN_API_KEY", os.Getenv("OPENAI_API_KEY")))
	for name, value := range a.headers {
		req.Header.Set(name, value)
	}
	response, err := a.httpClient.Do(req)
	if err != nil {
		return modelResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		body, _ := io.ReadAll(response.Body)
		return modelResponse{}, &completionAPIError{StatusCode: response.StatusCode, Message: fmt.Sprintf("completions API %s: %s", response.Status, strings.TrimSpace(string(body)))}
	}

	var result modelResponse
	var finishReason string
	toolCalls := map[int]int{}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk completionChunk
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			return modelResponse{}, err
		}
		if chunk.Error != nil {
			return modelResponse{}, &completionAPIError{Message: chunk.Error.Message}
		}
		if chunk.Usage != nil {
			result.Usage = Usage{InputTokens: chunk.Usage.PromptTokens, CachedInputTokens: chunk.Usage.PromptTokensDetails.CachedTokens, OutputTokens: chunk.Usage.CompletionTokens, ReasoningOutputTokens: chunk.Usage.CompletionTokensDetails.ReasoningTokens, TotalTokens: chunk.Usage.TotalTokens}
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
			delta := choice.Delta
			if delta.ReasoningContent != "" {
				result.Items = appendCompletionItem(result.Items, historyItem{Type: "reasoning", Text: delta.ReasoningContent, Provider: "openai-completions", Model: a.modelName})
				emit(ToolEvent{Kind: ToolEventReasoningDelta, Detail: delta.ReasoningContent})
			}
			if delta.Content != "" {
				result.Text += delta.Content
				result.Items = appendCompletionItem(result.Items, historyItem{Type: "message", Role: "assistant", Text: delta.Content, Provider: "openai-completions", Model: a.modelName})
				emit(ToolEvent{Kind: ToolEventTextDelta, Detail: delta.Content})
			}
			for _, call := range delta.ToolCalls {
				index, ok := toolCalls[call.Index]
				if !ok {
					result.Items = append(result.Items, historyItem{Type: "tool_call", Provider: "openai-completions", Model: a.modelName})
					index = len(result.Items) - 1
					toolCalls[call.Index] = index
				}
				result.Items[index].CallID += call.ID
				result.Items[index].Name += call.Function.Name
				result.Items[index].Arguments = append(result.Items[index].Arguments, call.Function.Arguments...)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return modelResponse{}, err
	}
	switch finishReason {
	case "":
		return modelResponse{}, fmt.Errorf("completions stream ended before the response completed")
	case "length":
		return modelResponse{}, fmt.Errorf("completions response hit the max_tokens limit before completing")
	}
	for _, item := range result.Items {
		if item.Type == "tool_call" {
			result.ToolCalls = append(result.ToolCalls, item)
		}
	}
	if len(result.Items) == 0 {
		return modelResponse{}, fmt.Errorf("stream ended without a completed response")
	}
	a.recordUsage(result.Usage, emit)
	return result, nil
}

func appendCompletionItem(items []historyItem, item historyItem) []historyItem {
	if len(items) > 0 && items[len(items)-1].Type == item.Type {
		items[len(items)-1].Text += item.Text
		return items
	}
	return append(items, item)
}
