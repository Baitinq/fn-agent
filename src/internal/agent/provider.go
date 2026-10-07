package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

type historyItem struct {
	Type             string          `json:"type"`
	Role             string          `json:"role,omitempty"`
	Text             string          `json:"text,omitempty"`
	CallID           string          `json:"call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
	Arguments        json.RawMessage `json:"arguments,omitempty"`
	Provider         string          `json:"provider,omitempty"`
	Model            string          `json:"model,omitempty"`
	ThoughtSignature string          `json:"thought_signature,omitempty"`
	ProviderData     json.RawMessage `json:"provider_data,omitempty"`
	RedactedThinking string          `json:"redacted_thinking,omitempty"`
	ToolError        bool            `json:"tool_error,omitempty"`
	REPLCheckpoint   string          `json:"repl_checkpoint,omitempty"`
	transient        bool
}

type modelRequest struct {
	Instructions    string
	History         []historyItem
	Tools           bool
	ReasoningEffort string
	MaxOutputTokens int64
}
type modelResponse struct {
	Items     []historyItem
	Text      string
	ToolCalls []historyItem
	Usage     Usage
}

func (a *Agent) streamModel(ctx context.Context, request modelRequest, emit func(ToolEvent)) (modelResponse, error) {
	switch a.provider {
	case "openai-completions":
		return a.streamCompletions(ctx, request, emit)
	case "gemini":
		return a.streamGemini(ctx, request, emit)
	case "anthropic":
		return a.streamAnthropic(ctx, request, emit)
	default:
		return a.streamOpenAI(ctx, request, emit)
	}
}

func openAIInput(history []historyItem, model string) responses.ResponseInputParam {
	input := make(responses.ResponseInputParam, 0, len(history))
	for _, h := range history {
		var item responses.ResponseInputItemUnionParam
		switch h.Type {
		case "message":
			if h.Role == "user" {
				item = responses.ResponseInputItemParamOfMessage(h.Text, responses.EasyInputMessageRoleUser)
			} else if h.Provider == "openai" && h.Model == model && len(h.ProviderData) > 0 {
				item.OfOutputMessage = &responses.ResponseOutputMessageParam{}
				_ = json.Unmarshal(h.ProviderData, item.OfOutputMessage)
			} else {
				item.OfOutputMessage = &responses.ResponseOutputMessageParam{Content: []responses.ResponseOutputMessageContentUnionParam{{OfOutputText: &responses.ResponseOutputTextParam{Text: h.Text}}}}
			}
		case "reasoning":
			if h.Provider == "openai" && h.Model == model && len(h.ProviderData) > 0 {
				item.OfReasoning = &responses.ResponseReasoningItemParam{}
				_ = json.Unmarshal(h.ProviderData, item.OfReasoning)
			} else if h.Text != "" {
				item.OfOutputMessage = &responses.ResponseOutputMessageParam{Content: []responses.ResponseOutputMessageContentUnionParam{{OfOutputText: &responses.ResponseOutputTextParam{Text: h.Text}}}}
			} else {
				continue
			}
		case "tool_call":
			item = responses.ResponseInputItemParamOfFunctionCall(string(h.Arguments), h.CallID, h.Name)
		case "tool_result":
			item = responses.ResponseInputItemParamOfFunctionCallOutput(h.CallID, h.Text)
		}
		input = append(input, item)
	}
	return input
}

func (a *Agent) streamOpenAI(ctx context.Context, request modelRequest, emit func(ToolEvent)) (modelResponse, error) {
	params := responses.ResponseNewParams{Model: a.modelName, Input: responses.ResponseNewParamsInputUnion{OfInputItemList: openAIInput(request.History, a.modelName)}, Store: openai.Bool(false), PromptCacheKey: openai.String(a.sessionID)}
	if request.Instructions != "" {
		params.Instructions = openai.String(request.Instructions)
	}
	params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffort(request.ReasoningEffort)}
	if request.Tools {
		params.Tools = []responses.ToolUnionParam{replTool}
		params.Reasoning.Summary = shared.ReasoningSummaryAuto
	}
	if request.MaxOutputTokens > 0 {
		params.MaxOutputTokens = openai.Int(request.MaxOutputTokens)
	}
	resp, err := a.streamResponse(ctx, params, emit)
	if err != nil {
		return modelResponse{}, err
	}
	result := modelResponse{Text: resp.OutputText(), Usage: Usage{InputTokens: resp.Usage.InputTokens, CachedInputTokens: resp.Usage.InputTokensDetails.CachedTokens, OutputTokens: resp.Usage.OutputTokens, ReasoningOutputTokens: resp.Usage.OutputTokensDetails.ReasoningTokens, TotalTokens: resp.Usage.TotalTokens}}
	for _, output := range resp.Output {
		switch output.Type {
		case "message":
			message := output.AsMessage()
			text := ""
			for _, c := range message.Content {
				if c.Type == "output_text" {
					text += c.AsOutputText().Text
				}
				if c.Type == "refusal" {
					text += c.AsRefusal().Refusal
				}
			}
			data, _ := json.Marshal(message.ToParam())
			result.Items = append(result.Items, historyItem{Type: "message", Role: "assistant", Text: text, Provider: "openai", Model: a.modelName, ProviderData: data})
		case "reasoning":
			reasoning := output.AsReasoning()
			var textParts []string
			for _, summary := range reasoning.Summary {
				textParts = append(textParts, summary.Text)
			}
			for _, content := range reasoning.Content {
				textParts = append(textParts, content.Text)
			}
			text := strings.Join(textParts, "\n")
			data, _ := json.Marshal(reasoning.ToParam())
			result.Items = append(result.Items, historyItem{Type: "reasoning", Text: text, Provider: "openai", Model: a.modelName, ProviderData: data})
		case "function_call":
			fc := output.AsFunctionCall()
			item := historyItem{Type: "tool_call", CallID: fc.CallID, Name: fc.Name, Arguments: json.RawMessage(fc.Arguments), Provider: "openai", Model: a.modelName}
			result.Items = append(result.Items, item)
			result.ToolCalls = append(result.ToolCalls, item)
		default:
			return modelResponse{}, fmt.Errorf("unsupported response item type %q", output.Type)
		}
	}
	return result, nil
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	Thought          bool                    `json:"thought,omitempty"`
	ThoughtSignature string                  `json:"thoughtSignature,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}
type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
	ID   string          `json:"id,omitempty"`
}
type geminiFunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
	ID       string         `json:"id,omitempty"`
}
type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}
type geminiRequest struct {
	Contents          []geminiContent `json:"contents"`
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	Tools             []any           `json:"tools,omitempty"`
	GenerationConfig  map[string]any  `json:"generationConfig,omitempty"`
}
type geminiAPIError struct {
	StatusCode int
	Message    string
}

func (e *geminiAPIError) Error() string { return e.Message }

type geminiChunk struct {
	Candidates []struct {
		Content geminiContent `json:"content"`
	} `json:"candidates"`
	Usage struct {
		Prompt     int64 `json:"promptTokenCount"`
		Cached     int64 `json:"cachedContentTokenCount"`
		Candidates int64 `json:"candidatesTokenCount"`
		Thoughts   int64 `json:"thoughtsTokenCount"`
		Total      int64 `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
}

func geminiContents(history []historyItem, provider, model string, includeToolCallIDs bool) []geminiContent {
	var contents []geminiContent
	appendPart := func(role string, p geminiPart) {
		if len(contents) > 0 && contents[len(contents)-1].Role == role {
			contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, p)
		} else {
			contents = append(contents, geminiContent{Role: role, Parts: []geminiPart{p}})
		}
	}
	for _, h := range history {
		signature := ""
		if h.Provider == provider && h.Model == model {
			signature = h.ThoughtSignature
		}
		switch h.Type {
		case "message":
			role := "user"
			if h.Role == "assistant" {
				role = "model"
			}
			appendPart(role, geminiPart{Text: h.Text, ThoughtSignature: signature})
		case "reasoning":
			if h.Provider == provider && h.Model == model {
				appendPart("model", geminiPart{Text: h.Text, Thought: true, ThoughtSignature: signature})
			}
		case "tool_call":
			id := h.CallID
			if !includeToolCallIDs {
				id = ""
			}
			appendPart("model", geminiPart{ThoughtSignature: signature, FunctionCall: &geminiFunctionCall{Name: h.Name, Args: h.Arguments, ID: id}})
		case "tool_result":
			id := h.CallID
			if !includeToolCallIDs {
				id = ""
			}
			appendPart("user", geminiPart{FunctionResponse: &geminiFunctionResponse{Name: h.Name, ID: id, Response: geminiToolResponse(h)}})
		}
	}
	return contents
}

func geminiToolResponse(item historyItem) map[string]any {
	if item.ToolError {
		return map[string]any{"error": item.Text}
	}
	return map[string]any{"output": item.Text}
}

func geminiThinkingLevel(effort string) string {
	switch effort {
	case "minimal":
		return "MINIMAL"
	case "low":
		return "LOW"
	case "high":
		return "HIGH"
	default:
		return "MEDIUM"
	}
}

func (a *Agent) streamGemini(ctx context.Context, request modelRequest, emit func(ToolEvent)) (modelResponse, error) {
	payload := geminiRequest{Contents: geminiContents(request.History, "gemini", a.modelName, !strings.Contains(a.baseURL, "ai-gateway")), GenerationConfig: map[string]any{"thinkingConfig": map[string]any{"includeThoughts": true, "thinkingLevel": geminiThinkingLevel(request.ReasoningEffort)}}}
	if request.Instructions != "" {
		payload.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: request.Instructions}}}
	}
	if request.MaxOutputTokens > 0 {
		payload.GenerationConfig["maxOutputTokens"] = request.MaxOutputTokens
	}
	if request.Tools {
		declaration := map[string]any{
			"name":        "repl",
			"description": "Execute Python code in a persistent async REPL with preloaded shell(), web_search(), and llm() host functions.",
			"parameters": map[string]any{
				"type":       "OBJECT",
				"properties": map[string]any{"code": map[string]any{"type": "STRING", "description": "Python code to execute. Variables and imports persist across calls."}},
				"required":   []string{"code"},
			},
		}
		payload.Tools = []any{map[string]any{"functionDeclarations": []any{declaration}}}
	}
	data, _ := json.Marshal(payload)
	endpoint := strings.TrimRight(a.baseURL, "/") + "/models/" + url.PathEscape(a.modelName) + ":streamGenerateContent?alt=sse"
	if !a.authHeader {
		endpoint += "&key=" + url.QueryEscape(os.Getenv("GEMINI_API_KEY"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return modelResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.authHeader {
		req.Header.Set("Authorization", "Bearer "+envOrDefault("FN_API_KEY", os.Getenv("GEMINI_API_KEY")))
	}
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
		return modelResponse{}, &geminiAPIError{StatusCode: response.StatusCode, Message: fmt.Sprintf("gemini API %s: %s", response.Status, strings.TrimSpace(string(body)))}
	}
	var result modelResponse
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var chunk geminiChunk
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			return modelResponse{}, err
		}
		if chunk.Error != nil {
			return modelResponse{}, &geminiAPIError{StatusCode: chunk.Error.Code, Message: fmt.Sprintf("gemini API %s: %s", chunk.Error.Status, chunk.Error.Message)}
		}
		if chunk.Usage.Total > 0 {
			result.Usage = Usage{InputTokens: chunk.Usage.Prompt, CachedInputTokens: chunk.Usage.Cached, OutputTokens: chunk.Usage.Candidates, ReasoningOutputTokens: chunk.Usage.Thoughts, TotalTokens: chunk.Usage.Total}
		}
		for _, candidate := range chunk.Candidates {
			for _, part := range candidate.Content.Parts {
				if part.FunctionCall != nil {
					id := part.FunctionCall.ID
					if id == "" {
						id = fmt.Sprintf("call_%d", len(result.ToolCalls)+1)
					}
					item := historyItem{Type: "tool_call", CallID: id, Name: part.FunctionCall.Name, Arguments: part.FunctionCall.Args, Provider: "gemini", Model: a.modelName, ThoughtSignature: part.ThoughtSignature}
					result.Items = append(result.Items, item)
					result.ToolCalls = append(result.ToolCalls, item)
					continue
				}
				if part.Text != "" || part.ThoughtSignature != "" {
					typ, role, event := "message", "assistant", ToolEventTextDelta
					if part.Thought {
						typ, role, event = "reasoning", "", ToolEventReasoningDelta
					} else {
						result.Text += part.Text
					}
					previous := len(result.Items) - 1
					if previous >= 0 && result.Items[previous].Type == typ && (part.ThoughtSignature == "" || result.Items[previous].ThoughtSignature == "") {
						result.Items[previous].Text += part.Text
						if part.ThoughtSignature != "" {
							result.Items[previous].ThoughtSignature = part.ThoughtSignature
						}
					} else {
						result.Items = append(result.Items, historyItem{Type: typ, Role: role, Text: part.Text, Provider: "gemini", Model: a.modelName, ThoughtSignature: part.ThoughtSignature})
					}
					emit(ToolEvent{Kind: event, Detail: part.Text})
				}
			}
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
func (a *Agent) recordUsage(usage Usage, emit func(ToolEvent)) {
	a.usageMu.Lock()
	a.usage = append(a.usage, usage)
	a.tokensUsed += usage.TotalTokens
	a.usageMu.Unlock()
	emit(ToolEvent{Kind: ToolEventContextTokens, ContextTokens: usage.TotalTokens, CacheHitRate: usage.cacheHitRate()})
}
