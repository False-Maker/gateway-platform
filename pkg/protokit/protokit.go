// Package protokit contains the small, explicit protocol boundary used by the
// gateway. Non-streaming request/response conversion is kept separate from
// the gateway's protocol-aware SSE forwarding.
package protokit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type Protocol string

const (
	OpenAIChat        Protocol = "openai_chat"
	OpenAIResponses   Protocol = "openai_responses"
	AnthropicMessages Protocol = "anthropic_messages"
	GeminiGenerate    Protocol = "gemini_generate"
)

var (
	ErrUnsupportedProtocol = errors.New("unsupported_protocol")
	ErrInvalidMessage      = errors.New("invalid_message")
)

type Usage struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
}

func ConvertRequest(body []byte, from, to Protocol) ([]byte, error) {
	if from == to {
		if err := validateJSON(body); err != nil {
			return nil, err
		}
		return append([]byte(nil), body...), nil
	}
	switch {
	case from == OpenAIResponses && to == OpenAIChat:
		return responsesToOpenAIRequest(body)
	case from == OpenAIChat && to == OpenAIResponses:
		return openAIToResponsesRequest(body)
	case from == OpenAIChat && to == AnthropicMessages:
		return openAIToAnthropicRequest(body)
	case from == AnthropicMessages && to == OpenAIChat:
		return anthropicToOpenAIRequest(body)
	case from == OpenAIChat && to == GeminiGenerate:
		return openAIToGeminiRequest(body)
	default:
		return nil, fmt.Errorf("%w: %s to %s", ErrUnsupportedProtocol, from, to)
	}
}

func ConvertResponse(body []byte, from, to Protocol) ([]byte, error) {
	if from == to {
		if err := validateJSON(body); err != nil {
			return nil, err
		}
		return append([]byte(nil), body...), nil
	}
	switch {
	case from == OpenAIResponses && to == OpenAIChat:
		return responsesToOpenAIResponse(body)
	case from == OpenAIChat && to == OpenAIResponses:
		return openAIToResponsesResponse(body)
	case from == OpenAIChat && to == AnthropicMessages:
		return openAIToAnthropicResponse(body)
	case from == OpenAIResponses && to == AnthropicMessages:
		chat, err := responsesToOpenAIResponse(body)
		if err != nil {
			return nil, err
		}
		return openAIToAnthropicResponse(chat)
	case from == AnthropicMessages && to == OpenAIChat:
		return anthropicToOpenAIResponse(body)
	case from == AnthropicMessages && to == OpenAIResponses:
		chat, err := anthropicToOpenAIResponse(body)
		if err != nil {
			return nil, err
		}
		return openAIToResponsesResponse(chat)
	case from == OpenAIChat && to == GeminiGenerate:
		return openAIToGeminiResponse(body)
	case from == GeminiGenerate && to == OpenAIChat:
		return geminiToOpenAIResponse(body)
	case from == GeminiGenerate && to == OpenAIResponses:
		chat, err := geminiToOpenAIResponse(body)
		if err != nil {
			return nil, err
		}
		return openAIToResponsesResponse(chat)
	case from == GeminiGenerate && to == AnthropicMessages:
		chat, err := geminiToOpenAIResponse(body)
		if err != nil {
			return nil, err
		}
		return openAIToAnthropicResponse(chat)
	default:
		return nil, fmt.Errorf("%w: %s to %s", ErrUnsupportedProtocol, from, to)
	}
}

func ParseUsage(body []byte, protocol Protocol) (Usage, bool, error) {
	if protocol == GeminiGenerate {
		var gemini struct {
			UsageMetadata *struct {
				PromptTokenCount        int `json:"promptTokenCount"`
				CandidatesTokenCount    int `json:"candidatesTokenCount"`
				CachedContentTokenCount int `json:"cachedContentTokenCount"`
			} `json:"usageMetadata"`
		}
		if err := json.Unmarshal(body, &gemini); err != nil {
			return Usage{}, false, err
		}
		if gemini.UsageMetadata == nil {
			return Usage{}, false, nil
		}
		return Usage{Input: gemini.UsageMetadata.PromptTokenCount, Output: gemini.UsageMetadata.CandidatesTokenCount, CacheRead: gemini.UsageMetadata.CachedContentTokenCount}, true, nil
	}
	var envelope struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Usage{}, false, err
	}
	envelope.Usage = bytes.TrimSpace(envelope.Usage)
	if len(envelope.Usage) == 0 || bytes.Equal(envelope.Usage, []byte("null")) {
		return Usage{}, false, nil
	}
	var raw struct {
		PromptTokens          int `json:"prompt_tokens"`
		CompletionTokens      int `json:"completion_tokens"`
		InputTokens           int `json:"input_tokens"`
		OutputTokens          int `json:"output_tokens"`
		CacheReadInputTokens  int `json:"cache_read_input_tokens"`
		CacheWriteInputTokens int `json:"cache_creation_input_tokens"`
		PromptDetails         *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		InputDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	}
	if err := json.Unmarshal(envelope.Usage, &raw); err != nil {
		return Usage{}, false, err
	}
	input, output := raw.PromptTokens, raw.CompletionTokens
	if input == 0 {
		input = raw.InputTokens
	}
	if output == 0 {
		output = raw.OutputTokens
	}
	cacheRead := raw.CacheReadInputTokens
	if cacheRead == 0 && raw.PromptDetails != nil {
		cacheRead = raw.PromptDetails.CachedTokens
	}
	if cacheRead == 0 && raw.InputDetails != nil {
		cacheRead = raw.InputDetails.CachedTokens
	}
	if protocol != OpenAIChat && protocol != OpenAIResponses && protocol != AnthropicMessages {
		return Usage{}, false, fmt.Errorf("%w: %s", ErrUnsupportedProtocol, protocol)
	}
	return Usage{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: raw.CacheWriteInputTokens}, true, nil
}

type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Stream      bool            `json:"stream"`
	Tools       []openAITool    `json:"tools"`
	MaxTokens   *int            `json:"max_tokens"`
	Temperature *float64        `json:"temperature"`
	TopP        *float64        `json:"top_p"`
	Stop        json.RawMessage `json:"stop"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	Name       string           `json:"name,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Arguments   string          `json:"arguments,omitempty"`
}

type openAIToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type anthropicRequest struct {
	Model        string             `json:"model"`
	Messages     []anthropicMessage `json:"messages"`
	System       json.RawMessage    `json:"system"`
	MaxTokens    int                `json:"max_tokens"`
	Stream       bool               `json:"stream"`
	Tools        []anthropicTool    `json:"tools,omitempty"`
	Temperature  *float64           `json:"temperature"`
	TopP         *float64           `json:"top_p"`
	StopSequence []string           `json:"stop_sequences,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

type geminiRequest struct {
	Contents          []geminiContent         `json:"contents"`
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text,omitempty"`
}

type geminiGenerationConfig struct {
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		TotalTokenCount         int `json:"totalTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
	} `json:"usageMetadata,omitempty"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason,omitempty"`
}

type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions"`
	Stream          bool            `json:"stream"`
	Tools           []responsesTool `json:"tools,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens"`
	Temperature     *float64        `json:"temperature"`
	TopP            *float64        `json:"top_p"`
	Stop            json.RawMessage `json:"stop"`
	PreviousID      string          `json:"previous_response_id"`
}

type responsesInputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    string          `json:"output,omitempty"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type responsesContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func responsesToOpenAIRequest(body []byte) ([]byte, error) {
	var request responsesRequest
	if err := decode(body, &request); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.Model) == "" || len(bytes.TrimSpace(request.Input)) == 0 || bytes.Equal(bytes.TrimSpace(request.Input), []byte("null")) {
		return nil, fmt.Errorf("%w: model and input are required", ErrInvalidMessage)
	}
	if request.Stream || request.PreviousID != "" {
		return nil, fmt.Errorf("%w: streaming and continuation are not supported", ErrUnsupportedProtocol)
	}
	messages := make([]openAIMessage, 0)
	if request.Instructions != "" {
		messages = append(messages, openAIMessage{Role: "system", Content: json.RawMessage(mustJSON(request.Instructions))})
	}
	input := bytes.TrimSpace(request.Input)
	var one string
	if json.Unmarshal(input, &one) == nil {
		messages = append(messages, openAIMessage{Role: "user", Content: json.RawMessage(mustJSON(one))})
	} else {
		var items []responsesInputItem
		if err := json.Unmarshal(input, &items); err != nil || len(items) == 0 {
			return nil, fmt.Errorf("%w: input must be a string or non-empty message array", ErrInvalidMessage)
		}
		for _, item := range items {
			switch item.Type {
			case "function_call":
				if strings.TrimSpace(item.CallID) == "" || strings.TrimSpace(item.Name) == "" {
					return nil, fmt.Errorf("%w: function_call requires call_id and name", ErrInvalidMessage)
				}
				if _, err := normalizeToolArguments(item.Arguments); err != nil {
					return nil, fmt.Errorf("%w: function_call arguments", ErrInvalidMessage)
				}
				messages = append(messages, openAIMessage{Role: "assistant", ToolCalls: []openAIToolCall{{ID: item.CallID, Type: "function", Function: openAIFunction{Name: item.Name, Arguments: item.Arguments}}}})
				continue
			case "function_call_output":
				if strings.TrimSpace(item.CallID) == "" {
					return nil, fmt.Errorf("%w: function_call_output requires call_id", ErrInvalidMessage)
				}
				messages = append(messages, openAIMessage{Role: "tool", ToolCallID: item.CallID, Content: json.RawMessage(mustJSON(item.Output))})
				continue
			}
			if item.Type != "" && item.Type != "message" {
				return nil, fmt.Errorf("%w: input item type %q", ErrUnsupportedProtocol, item.Type)
			}
			if item.Role != "user" && item.Role != "assistant" && item.Role != "system" && item.Role != "developer" {
				return nil, fmt.Errorf("%w: unsupported input role %q", ErrInvalidMessage, item.Role)
			}
			text, err := responsesTextValue(item.Content)
			if err != nil {
				return nil, fmt.Errorf("%w: input content: %s", ErrInvalidMessage, err)
			}
			role := item.Role
			if role == "developer" {
				role = "system"
			}
			messages = append(messages, openAIMessage{Role: role, Content: json.RawMessage(mustJSON(text))})
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("%w: at least one input message is required", ErrInvalidMessage)
	}
	tools := make([]openAITool, 0, len(request.Tools))
	for _, tool := range request.Tools {
		if tool.Type != "function" || strings.TrimSpace(tool.Name) == "" || !isJSONObject(tool.Parameters) {
			return nil, fmt.Errorf("%w: malformed Responses function tool", ErrInvalidMessage)
		}
		tools = append(tools, openAITool{Type: "function", Function: openAIFunction{Name: tool.Name, Description: tool.Description, Parameters: append([]byte(nil), tool.Parameters...)}})
	}
	converted := openAIRequest{Model: request.Model, Messages: messages, Tools: tools, Temperature: request.Temperature, TopP: request.TopP}
	if request.MaxOutputTokens != nil {
		if *request.MaxOutputTokens <= 0 {
			return nil, fmt.Errorf("%w: max_output_tokens must be positive", ErrInvalidMessage)
		}
		converted.MaxTokens = request.MaxOutputTokens
	}
	if len(request.Stop) > 0 && !bytes.Equal(bytes.TrimSpace(request.Stop), []byte("null")) {
		converted.Stop = request.Stop
	}
	return json.Marshal(converted)
}

func openAIToResponsesRequest(body []byte) ([]byte, error) {
	var request openAIRequest
	if err := decode(body, &request); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.Model) == "" || len(request.Messages) == 0 {
		return nil, fmt.Errorf("%w: model and messages are required", ErrInvalidMessage)
	}
	if request.Stream {
		return nil, fmt.Errorf("%w: streaming is not supported", ErrUnsupportedProtocol)
	}
	var instructions string
	items := make([]responsesInputItem, 0, len(request.Messages))
	for _, message := range request.Messages {
		switch message.Role {
		case "system":
			text, err := textValue(message.Content)
			if err != nil {
				return nil, fmt.Errorf("%w: %s", ErrInvalidMessage, err)
			}
			if instructions != "" {
				instructions += "\n\n"
			}
			instructions += text
		case "user", "assistant":
			if len(bytes.TrimSpace(message.Content)) > 0 && !bytes.Equal(bytes.TrimSpace(message.Content), []byte("null")) {
				text, err := textValue(message.Content)
				if err != nil {
					return nil, fmt.Errorf("%w: %s", ErrInvalidMessage, err)
				}
				items = append(items, responsesInputItem{Type: "message", Role: message.Role, Content: json.RawMessage(mustJSON([]responsesContentPart{{Type: "input_text", Text: text}}))})
			}
			for _, call := range message.ToolCalls {
				if call.Type != "function" || strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Function.Name) == "" {
					return nil, fmt.Errorf("%w: malformed tool call", ErrInvalidMessage)
				}
				if _, err := normalizeToolArguments(call.Function.Arguments); err != nil {
					return nil, fmt.Errorf("%w: malformed tool call arguments", ErrInvalidMessage)
				}
				items = append(items, responsesInputItem{Type: "function_call", CallID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
			}
		case "tool":
			if strings.TrimSpace(message.ToolCallID) == "" {
				return nil, fmt.Errorf("%w: tool_call_id is required", ErrInvalidMessage)
			}
			text, err := textValue(message.Content)
			if err != nil {
				return nil, fmt.Errorf("%w: tool output: %s", ErrInvalidMessage, err)
			}
			items = append(items, responsesInputItem{Type: "function_call_output", CallID: message.ToolCallID, Output: text})
		default:
			return nil, fmt.Errorf("%w: unsupported role %q", ErrInvalidMessage, message.Role)
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: at least one user or assistant message is required", ErrInvalidMessage)
	}
	tools := make([]responsesTool, 0, len(request.Tools))
	for _, tool := range request.Tools {
		if tool.Type != "function" || strings.TrimSpace(tool.Function.Name) == "" || !isJSONObject(tool.Function.Parameters) {
			return nil, fmt.Errorf("%w: malformed function tool", ErrInvalidMessage)
		}
		tools = append(tools, responsesTool{Type: "function", Name: tool.Function.Name, Description: tool.Function.Description, Parameters: append([]byte(nil), tool.Function.Parameters...)})
	}
	converted := responsesRequest{Model: request.Model, Input: json.RawMessage(mustJSON(items)), Instructions: instructions, Tools: tools, Temperature: request.Temperature, TopP: request.TopP}
	if request.MaxTokens != nil {
		if *request.MaxTokens <= 0 {
			return nil, fmt.Errorf("%w: max_tokens must be positive", ErrInvalidMessage)
		}
		converted.MaxOutputTokens = request.MaxTokens
	}
	if len(request.Stop) > 0 && !bytes.Equal(bytes.TrimSpace(request.Stop), []byte("null")) {
		converted.Stop = request.Stop
	}
	return json.Marshal(converted)
}

func openAIToAnthropicRequest(body []byte) ([]byte, error) {
	var request openAIRequest
	if err := decode(body, &request); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.Model) == "" || len(request.Messages) == 0 {
		return nil, fmt.Errorf("%w: model and messages are required", ErrInvalidMessage)
	}
	if request.Stream {
		return nil, fmt.Errorf("%w: streaming is not supported", ErrUnsupportedProtocol)
	}
	if request.MaxTokens == nil || *request.MaxTokens <= 0 {
		return nil, fmt.Errorf("%w: max_tokens is required for Anthropic", ErrInvalidMessage)
	}
	var system []string
	messages := make([]anthropicMessage, 0, len(request.Messages))
	tools, err := openAIToolsToAnthropic(request.Tools)
	if err != nil {
		return nil, err
	}
	for _, message := range request.Messages {
		switch message.Role {
		case "system":
			text, err := textValue(message.Content)
			if err != nil {
				return nil, fmt.Errorf("%w: %s", ErrInvalidMessage, err)
			}
			system = append(system, text)
		case "user", "assistant", "tool":
			converted, err := openAIMessageToAnthropic(message)
			if err != nil {
				return nil, err
			}
			messages = append(messages, converted)
		default:
			return nil, fmt.Errorf("%w: unsupported role %q", ErrInvalidMessage, message.Role)
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("%w: at least one user or assistant message is required", ErrInvalidMessage)
	}
	converted := anthropicRequest{Model: request.Model, Messages: messages, Tools: tools, MaxTokens: *request.MaxTokens, Temperature: request.Temperature, TopP: request.TopP}
	if len(system) > 0 {
		converted.System = json.RawMessage(mustJSON(strings.Join(system, "\n\n")))
	}
	if len(request.Stop) > 0 && !bytes.Equal(request.Stop, []byte("null")) {
		if err := json.Unmarshal(request.Stop, &converted.StopSequence); err != nil {
			var one string
			if err := json.Unmarshal(request.Stop, &one); err != nil {
				return nil, fmt.Errorf("%w: stop must be a string or array", ErrInvalidMessage)
			}
			converted.StopSequence = []string{one}
		}
	}
	return json.Marshal(converted)
}

func anthropicToOpenAIRequest(body []byte) ([]byte, error) {
	var request anthropicRequest
	if err := decode(body, &request); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.Model) == "" || len(request.Messages) == 0 || request.MaxTokens <= 0 {
		return nil, fmt.Errorf("%w: model, messages, and positive max_tokens are required", ErrInvalidMessage)
	}
	if request.Stream {
		return nil, fmt.Errorf("%w: streaming is not supported", ErrUnsupportedProtocol)
	}
	messages := make([]openAIMessage, 0, len(request.Messages)+1)
	if len(request.System) > 0 && !bytes.Equal(request.System, []byte("null")) {
		text, err := anthropicTextValue(request.System)
		if err != nil {
			return nil, fmt.Errorf("%w: system: %s", ErrInvalidMessage, err)
		}
		messages = append(messages, openAIMessage{Role: "system", Content: json.RawMessage(mustJSON(text))})
	}
	for _, message := range request.Messages {
		converted, err := anthropicMessageToOpenAI(message)
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
	}
	tools, err := anthropicToolsToOpenAI(request.Tools)
	if err != nil {
		return nil, err
	}
	converted := openAIRequest{Model: request.Model, Messages: messages, Tools: tools, MaxTokens: &request.MaxTokens, Temperature: request.Temperature, TopP: request.TopP}
	if len(request.StopSequence) > 0 {
		converted.Stop = json.RawMessage(mustJSON(request.StopSequence))
	}
	return json.Marshal(converted)
}

// openAIToGeminiRequest is intentionally strict and fixture-oriented. Model
// selection and authentication remain in UpstreamProfile; this converter only
// maps text messages and generation controls.
func openAIToGeminiRequest(body []byte) ([]byte, error) {
	var request openAIRequest
	if err := decode(body, &request); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.Model) == "" || len(request.Messages) == 0 {
		return nil, fmt.Errorf("%w: model and messages are required", ErrInvalidMessage)
	}
	if request.Stream || len(request.Tools) > 0 {
		return nil, fmt.Errorf("%w: streaming and tools are not supported", ErrUnsupportedProtocol)
	}
	converted := geminiRequest{Contents: make([]geminiContent, 0, len(request.Messages))}
	for _, message := range request.Messages {
		text, err := textValue(message.Content)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrInvalidMessage, err)
		}
		switch message.Role {
		case "system":
			if converted.SystemInstruction == nil {
				converted.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: text}}}
			} else {
				converted.SystemInstruction.Parts = append(converted.SystemInstruction.Parts, geminiPart{Text: text})
			}
		case "user":
			converted.Contents = append(converted.Contents, geminiContent{Role: "user", Parts: []geminiPart{{Text: text}}})
		case "assistant":
			converted.Contents = append(converted.Contents, geminiContent{Role: "model", Parts: []geminiPart{{Text: text}}})
		default:
			return nil, fmt.Errorf("%w: unsupported role %q", ErrInvalidMessage, message.Role)
		}
	}
	if len(converted.Contents) == 0 {
		return nil, fmt.Errorf("%w: at least one user or assistant message is required", ErrInvalidMessage)
	}
	config := &geminiGenerationConfig{Temperature: request.Temperature, TopP: request.TopP}
	if request.MaxTokens != nil {
		if *request.MaxTokens <= 0 {
			return nil, fmt.Errorf("%w: max_tokens must be positive", ErrInvalidMessage)
		}
		config.MaxOutputTokens = *request.MaxTokens
	}
	if len(request.Stop) > 0 && !bytes.Equal(bytes.TrimSpace(request.Stop), []byte("null")) {
		if err := json.Unmarshal(request.Stop, &config.StopSequences); err != nil {
			var one string
			if err := json.Unmarshal(request.Stop, &one); err != nil {
				return nil, fmt.Errorf("%w: stop must be a string or array", ErrInvalidMessage)
			}
			config.StopSequences = []string{one}
		}
	}
	if config.MaxOutputTokens > 0 || config.Temperature != nil || config.TopP != nil || len(config.StopSequences) > 0 {
		converted.GenerationConfig = config
	}
	return json.Marshal(converted)
}

func openAIToolsToAnthropic(tools []openAITool) ([]anthropicTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	converted := make([]anthropicTool, 0, len(tools))
	for _, tool := range tools {
		if tool.Type != "function" || strings.TrimSpace(tool.Function.Name) == "" || !isJSONObject(tool.Function.Parameters) {
			return nil, fmt.Errorf("%w: only well-formed function tools are supported", ErrUnsupportedProtocol)
		}
		converted = append(converted, anthropicTool{Name: tool.Function.Name, Description: tool.Function.Description, InputSchema: append([]byte(nil), tool.Function.Parameters...)})
	}
	return converted, nil
}

func anthropicToolsToOpenAI(tools []anthropicTool) ([]openAITool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	converted := make([]openAITool, 0, len(tools))
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) == "" || !isJSONObject(tool.InputSchema) {
			return nil, fmt.Errorf("%w: Anthropic tool requires name and object input_schema", ErrInvalidMessage)
		}
		converted = append(converted, openAITool{Type: "function", Function: openAIFunction{Name: tool.Name, Description: tool.Description, Parameters: append([]byte(nil), tool.InputSchema...)}})
	}
	return converted, nil
}

func openAIMessageToAnthropic(message openAIMessage) (anthropicMessage, error) {
	switch message.Role {
	case "user", "assistant":
		blocks := make([]anthropicContentBlock, 0, len(message.ToolCalls)+1)
		if len(bytes.TrimSpace(message.Content)) > 0 && !bytes.Equal(bytes.TrimSpace(message.Content), []byte("null")) {
			text, err := textValue(message.Content)
			if err != nil {
				return anthropicMessage{}, fmt.Errorf("%w: %s", ErrInvalidMessage, err)
			}
			if text != "" {
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: text})
			}
		}
		for _, call := range message.ToolCalls {
			arguments, err := normalizeToolArguments(call.Function.Arguments)
			if call.Type != "function" || strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Function.Name) == "" || err != nil {
				return anthropicMessage{}, fmt.Errorf("%w: malformed tool call", ErrInvalidMessage)
			}
			var input any
			if err := json.Unmarshal(arguments, &input); err != nil {
				return anthropicMessage{}, fmt.Errorf("%w: tool call arguments: %s", ErrInvalidMessage, err)
			}
			blocks = append(blocks, anthropicContentBlock{Type: "tool_use", ID: call.ID, Name: call.Function.Name, Input: json.RawMessage(mustJSON(input))})
		}
		if len(blocks) == 0 {
			return anthropicMessage{}, fmt.Errorf("%w: message content is required", ErrInvalidMessage)
		}
		return anthropicMessage{Role: message.Role, Content: json.RawMessage(mustJSON(blocks))}, nil
	case "tool":
		if strings.TrimSpace(message.ToolCallID) == "" {
			return anthropicMessage{}, fmt.Errorf("%w: tool_call_id is required", ErrInvalidMessage)
		}
		text, err := textValue(message.Content)
		if err != nil {
			return anthropicMessage{}, fmt.Errorf("%w: tool result: %s", ErrInvalidMessage, err)
		}
		return anthropicMessage{Role: "user", Content: json.RawMessage(mustJSON([]anthropicContentBlock{{Type: "tool_result", ToolUseID: message.ToolCallID, Content: json.RawMessage(mustJSON(text))}}))}, nil
	default:
		return anthropicMessage{}, fmt.Errorf("%w: unsupported role %q", ErrInvalidMessage, message.Role)
	}
}

func anthropicMessageToOpenAI(message anthropicMessage) ([]openAIMessage, error) {
	if message.Role != "user" && message.Role != "assistant" {
		return nil, fmt.Errorf("%w: unsupported role %q", ErrInvalidMessage, message.Role)
	}
	if text, err := textValue(message.Content); err == nil {
		return []openAIMessage{{Role: message.Role, Content: json.RawMessage(mustJSON(text))}}, nil
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(message.Content, &blocks); err != nil || len(blocks) == 0 {
		return nil, fmt.Errorf("%w: only string or supported content blocks are accepted", ErrInvalidMessage)
	}
	var text strings.Builder
	toolCalls := make([]openAIToolCall, 0)
	results := make([]openAIMessage, 0)
	for _, block := range blocks {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			if message.Role != "assistant" || strings.TrimSpace(block.ID) == "" || strings.TrimSpace(block.Name) == "" || !json.Valid(block.Input) {
				return nil, fmt.Errorf("%w: malformed tool_use block", ErrInvalidMessage)
			}
			toolCalls = append(toolCalls, openAIToolCall{ID: block.ID, Type: "function", Function: openAIFunction{Name: block.Name, Arguments: string(block.Input)}})
		case "tool_result":
			if message.Role != "user" || strings.TrimSpace(block.ToolUseID) == "" || len(bytes.TrimSpace(block.Content)) == 0 {
				return nil, fmt.Errorf("%w: malformed tool_result block", ErrInvalidMessage)
			}
			result, err := anthropicTextValue(block.Content)
			if err != nil {
				return nil, fmt.Errorf("%w: tool_result: %s", ErrInvalidMessage, err)
			}
			results = append(results, openAIMessage{Role: "tool", ToolCallID: block.ToolUseID, Content: json.RawMessage(mustJSON(result))})
		default:
			return nil, fmt.Errorf("%w: content block %q", ErrUnsupportedProtocol, block.Type)
		}
	}
	if text.Len() > 0 || len(toolCalls) > 0 {
		content := json.RawMessage(nil)
		if text.Len() > 0 {
			content = json.RawMessage(mustJSON(text.String()))
		}
		results = append([]openAIMessage{{Role: message.Role, Content: content, ToolCalls: toolCalls}}, results...)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("%w: content block list is empty", ErrInvalidMessage)
	}
	return results, nil
}

func isJSONObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' || !json.Valid(raw) {
		return false
	}
	var object map[string]any
	return json.Unmarshal(raw, &object) == nil
}

func normalizeToolArguments(raw string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("arguments are empty")
	}
	value := json.RawMessage(trimmed)
	if !isJSONObject(value) {
		return nil, errors.New("arguments must be a JSON object")
	}
	return value, nil
}

type openAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object,omitempty"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

type openAIChoice struct {
	Index        int                   `json:"index"`
	Message      openAIResponseMessage `json:"message"`
	FinishReason *string               `json:"finish_reason"`
}

type openAIResponseMessage struct {
	Role      string           `json:"role"`
	Content   json.RawMessage  `json:"content"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIUsage struct {
	PromptTokens          int `json:"prompt_tokens"`
	CompletionTokens      int `json:"completion_tokens"`
	TotalTokens           int `json:"total_tokens"`
	CacheReadInputTokens  int `json:"cache_read_input_tokens,omitempty"`
	CacheWriteInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type,omitempty"`
	Model      string                  `json:"model"`
	Role       string                  `json:"role"`
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason,omitempty"`
	Usage      *anthropicUsage         `json:"usage,omitempty"`
}

type anthropicUsage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationTokens  int `json:"cache_creation_input_tokens,omitempty"`
}

type responsesResponse struct {
	ID               string                     `json:"id"`
	Object           string                     `json:"object"`
	Model            string                     `json:"model"`
	Status           string                     `json:"status"`
	Output           []responsesOutputItem      `json:"output"`
	Usage            *responsesUsage            `json:"usage,omitempty"`
	IncompleteDetail *responsesIncompleteDetail `json:"incomplete_details,omitempty"`
}

type responsesOutputItem struct {
	Type      string                 `json:"type"`
	Role      string                 `json:"role,omitempty"`
	Content   []responsesContentPart `json:"content,omitempty"`
	CallID    string                 `json:"call_id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Arguments string                 `json:"arguments,omitempty"`
}

type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
}

type responsesIncompleteDetail struct {
	Reason string `json:"reason"`
}

func responsesToOpenAIResponse(body []byte) ([]byte, error) {
	var response responsesResponse
	if err := decode(body, &response); err != nil {
		return nil, err
	}
	if response.Status != "" && response.Status != "completed" && response.Status != "incomplete" {
		return nil, fmt.Errorf("%w: response status %q", ErrUnsupportedProtocol, response.Status)
	}
	var parts []string
	toolCalls := make([]openAIToolCall, 0)
	messageItems := 0
	for _, item := range response.Output {
		switch item.Type {
		case "message":
			if item.Role != "" && item.Role != "assistant" {
				return nil, fmt.Errorf("%w: response output role %q", ErrUnsupportedProtocol, item.Role)
			}
			messageItems++
			for _, part := range item.Content {
				if part.Type != "output_text" {
					return nil, fmt.Errorf("%w: response content block %q", ErrUnsupportedProtocol, part.Type)
				}
				parts = append(parts, part.Text)
			}
		case "function_call":
			if strings.TrimSpace(item.CallID) == "" || strings.TrimSpace(item.Name) == "" {
				return nil, fmt.Errorf("%w: function_call requires call_id and name", ErrInvalidMessage)
			}
			if _, err := normalizeToolArguments(item.Arguments); err != nil {
				return nil, fmt.Errorf("%w: function_call arguments", ErrInvalidMessage)
			}
			toolCalls = append(toolCalls, openAIToolCall{ID: item.CallID, Type: "function", Function: openAIFunction{Name: item.Name, Arguments: item.Arguments}})
		default:
			return nil, fmt.Errorf("%w: response output item %q", ErrUnsupportedProtocol, item.Type)
		}
	}
	if messageItems > 1 || (len(parts) == 0 && len(toolCalls) == 0) {
		if messageItems > 1 {
			return nil, fmt.Errorf("%w: Responses response cannot represent %d message outputs", ErrUnsupportedProtocol, messageItems)
		}
		return nil, fmt.Errorf("%w: response has no text output", ErrInvalidMessage)
	}
	finish := "stop"
	if response.Status == "incomplete" || (response.IncompleteDetail != nil && response.IncompleteDetail.Reason == "max_output_tokens") {
		finish = "length"
	}
	content := json.RawMessage(nil)
	if len(parts) > 0 {
		content = json.RawMessage(mustJSON(strings.Join(parts, "")))
	}
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	converted := openAIResponse{ID: response.ID, Object: "chat.completion", Model: response.Model, Choices: []openAIChoice{{Index: 0, Message: openAIResponseMessage{Role: "assistant", Content: content, ToolCalls: toolCalls}, FinishReason: &finish}}}
	if response.Usage != nil {
		converted.Usage = &openAIUsage{PromptTokens: response.Usage.InputTokens, CompletionTokens: response.Usage.OutputTokens, TotalTokens: response.Usage.TotalTokens}
		if response.Usage.TotalTokens == 0 {
			converted.Usage.TotalTokens = response.Usage.InputTokens + response.Usage.OutputTokens
		}
		if response.Usage.InputTokensDetails != nil {
			converted.Usage.CacheReadInputTokens = response.Usage.InputTokensDetails.CachedTokens
		}
	}
	return json.Marshal(converted)
}

func openAIToResponsesResponse(body []byte) ([]byte, error) {
	var response openAIResponse
	if err := decode(body, &response); err != nil {
		return nil, err
	}
	if len(response.Choices) != 1 {
		return nil, fmt.Errorf("%w: Responses response cannot represent %d choices", ErrUnsupportedProtocol, len(response.Choices))
	}
	choice := response.Choices[0]
	output := make([]responsesOutputItem, 0, len(choice.Message.ToolCalls)+1)
	if len(bytes.TrimSpace(choice.Message.Content)) > 0 && !bytes.Equal(bytes.TrimSpace(choice.Message.Content), []byte("null")) {
		text, err := textValue(choice.Message.Content)
		if err != nil {
			return nil, fmt.Errorf("%w: response content: %s", ErrInvalidMessage, err)
		}
		output = append(output, responsesOutputItem{Type: "message", Role: "assistant", Content: []responsesContentPart{{Type: "output_text", Text: text}}})
	}
	for _, call := range choice.Message.ToolCalls {
		if call.Type != "function" || strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Function.Name) == "" {
			return nil, fmt.Errorf("%w: malformed response tool call", ErrInvalidMessage)
		}
		if _, err := normalizeToolArguments(call.Function.Arguments); err != nil {
			return nil, fmt.Errorf("%w: malformed response tool call arguments", ErrInvalidMessage)
		}
		output = append(output, responsesOutputItem{Type: "function_call", CallID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	if len(output) == 0 {
		return nil, fmt.Errorf("%w: response content is empty", ErrInvalidMessage)
	}
	converted := responsesResponse{ID: response.ID, Object: "response", Model: response.Model, Status: "completed", Output: output}
	if choice.FinishReason != nil && *choice.FinishReason == "length" {
		converted.Status = "incomplete"
		converted.IncompleteDetail = &responsesIncompleteDetail{Reason: "max_output_tokens"}
	}
	if response.Usage != nil {
		converted.Usage = &responsesUsage{InputTokens: response.Usage.PromptTokens, OutputTokens: response.Usage.CompletionTokens, TotalTokens: response.Usage.TotalTokens}
		if converted.Usage.TotalTokens == 0 {
			converted.Usage.TotalTokens = response.Usage.PromptTokens + response.Usage.CompletionTokens
		}
		if response.Usage.CacheReadInputTokens > 0 {
			converted.Usage.InputTokensDetails = &struct {
				CachedTokens int `json:"cached_tokens"`
			}{CachedTokens: response.Usage.CacheReadInputTokens}
		}
	}
	return json.Marshal(converted)
}

func openAIToGeminiResponse(body []byte) ([]byte, error) {
	var response openAIResponse
	if err := decode(body, &response); err != nil {
		return nil, err
	}
	if len(response.Choices) != 1 {
		return nil, fmt.Errorf("%w: Gemini response cannot represent %d choices", ErrUnsupportedProtocol, len(response.Choices))
	}
	choice := response.Choices[0]
	if len(choice.Message.ToolCalls) > 0 {
		return nil, fmt.Errorf("%w: Gemini text fixture does not support tool calls", ErrUnsupportedProtocol)
	}
	text, err := textValue(choice.Message.Content)
	if err != nil {
		return nil, fmt.Errorf("%w: response content: %s", ErrInvalidMessage, err)
	}
	finish := "STOP"
	if choice.FinishReason != nil && *choice.FinishReason == "length" {
		finish = "MAX_TOKENS"
	}
	converted := geminiResponse{Candidates: []geminiCandidate{{Content: geminiContent{Role: "model", Parts: []geminiPart{{Text: text}}}, FinishReason: finish}}}
	if response.Usage != nil {
		converted.UsageMetadata = &struct {
			PromptTokenCount        int `json:"promptTokenCount"`
			CandidatesTokenCount    int `json:"candidatesTokenCount"`
			TotalTokenCount         int `json:"totalTokenCount"`
			CachedContentTokenCount int `json:"cachedContentTokenCount"`
		}{PromptTokenCount: response.Usage.PromptTokens, CandidatesTokenCount: response.Usage.CompletionTokens, TotalTokenCount: response.Usage.TotalTokens, CachedContentTokenCount: response.Usage.CacheReadInputTokens}
	}
	return json.Marshal(converted)
}

func geminiToOpenAIResponse(body []byte) ([]byte, error) {
	var response geminiResponse
	if err := decode(body, &response); err != nil {
		return nil, err
	}
	if len(response.Candidates) != 1 {
		return nil, fmt.Errorf("%w: Gemini response cannot represent %d candidates", ErrUnsupportedProtocol, len(response.Candidates))
	}
	candidate := response.Candidates[0]
	if candidate.Content.Role != "" && candidate.Content.Role != "model" {
		return nil, fmt.Errorf("%w: Gemini response role %q", ErrInvalidMessage, candidate.Content.Role)
	}
	var parts []string
	for _, part := range candidate.Content.Parts {
		if strings.TrimSpace(part.Text) == "" {
			return nil, fmt.Errorf("%w: Gemini response contains a non-text part", ErrUnsupportedProtocol)
		}
		parts = append(parts, part.Text)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("%w: Gemini response has no text output", ErrInvalidMessage)
	}
	finish := "stop"
	if candidate.FinishReason == "MAX_TOKENS" {
		finish = "length"
	}
	converted := openAIResponse{Object: "chat.completion", Choices: []openAIChoice{{Index: 0, Message: openAIResponseMessage{Role: "assistant", Content: json.RawMessage(mustJSON(strings.Join(parts, "")))}, FinishReason: &finish}}}
	if response.UsageMetadata != nil {
		converted.Usage = &openAIUsage{PromptTokens: response.UsageMetadata.PromptTokenCount, CompletionTokens: response.UsageMetadata.CandidatesTokenCount, TotalTokens: response.UsageMetadata.TotalTokenCount, CacheReadInputTokens: response.UsageMetadata.CachedContentTokenCount}
		if converted.Usage.TotalTokens == 0 {
			converted.Usage.TotalTokens = converted.Usage.PromptTokens + converted.Usage.CompletionTokens
		}
	}
	return json.Marshal(converted)
}

func responsesTextValue(raw json.RawMessage) (string, error) {
	if text, err := textValue(raw); err == nil {
		return text, nil
	}
	var parts []responsesContentPart
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) == 0 {
		return "", errors.New("only string or input_text content is supported")
	}
	var text strings.Builder
	for _, part := range parts {
		if part.Type != "input_text" {
			return "", fmt.Errorf("content block %q is not supported", part.Type)
		}
		text.WriteString(part.Text)
	}
	return text.String(), nil
}

func openAIToAnthropicResponse(body []byte) ([]byte, error) {
	var response openAIResponse
	if err := decode(body, &response); err != nil {
		return nil, err
	}
	if len(response.Choices) != 1 {
		return nil, fmt.Errorf("%w: Anthropic response cannot represent %d choices", ErrUnsupportedProtocol, len(response.Choices))
	}
	choice := response.Choices[0]
	if choice.FinishReason != nil && *choice.FinishReason != "" && *choice.FinishReason != "stop" && *choice.FinishReason != "length" && *choice.FinishReason != "tool_calls" {
		return nil, fmt.Errorf("%w: finish_reason %q", ErrUnsupportedProtocol, *choice.FinishReason)
	}
	blocks := make([]anthropicContentBlock, 0, len(choice.Message.ToolCalls)+1)
	if len(bytes.TrimSpace(choice.Message.Content)) > 0 && !bytes.Equal(bytes.TrimSpace(choice.Message.Content), []byte("null")) {
		text, err := textValue(choice.Message.Content)
		if err != nil {
			return nil, fmt.Errorf("%w: response content: %s", ErrInvalidMessage, err)
		}
		blocks = append(blocks, anthropicContentBlock{Type: "text", Text: text})
	}
	for _, call := range choice.Message.ToolCalls {
		arguments, err := normalizeToolArguments(call.Function.Arguments)
		if call.Type != "function" || strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Function.Name) == "" || err != nil {
			return nil, fmt.Errorf("%w: malformed response tool call", ErrInvalidMessage)
		}
		var input any
		if err := json.Unmarshal(arguments, &input); err != nil {
			return nil, fmt.Errorf("%w: response tool arguments: %s", ErrInvalidMessage, err)
		}
		blocks = append(blocks, anthropicContentBlock{Type: "tool_use", ID: call.ID, Name: call.Function.Name, Input: json.RawMessage(mustJSON(input))})
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("%w: response content is empty", ErrInvalidMessage)
	}
	converted := anthropicResponse{ID: response.ID, Type: "message", Model: response.Model, Role: "assistant", Content: blocks, StopReason: anthropicStopReason(choice.FinishReason)}
	if len(choice.Message.ToolCalls) > 0 {
		converted.StopReason = "tool_use"
	}
	if response.Usage != nil {
		converted.Usage = &anthropicUsage{InputTokens: response.Usage.PromptTokens, OutputTokens: response.Usage.CompletionTokens, CacheReadInputTokens: response.Usage.CacheReadInputTokens, CacheCreationTokens: response.Usage.CacheWriteInputTokens}
	}
	return json.Marshal(converted)
}

func anthropicToOpenAIResponse(body []byte) ([]byte, error) {
	var response anthropicResponse
	if err := decode(body, &response); err != nil {
		return nil, err
	}
	if response.Role != "" && response.Role != "assistant" {
		return nil, fmt.Errorf("%w: response role %q", ErrInvalidMessage, response.Role)
	}
	if response.StopReason != "" && response.StopReason != "end_turn" && response.StopReason != "max_tokens" && response.StopReason != "stop_sequence" && response.StopReason != "tool_use" {
		return nil, fmt.Errorf("%w: stop_reason %q", ErrUnsupportedProtocol, response.StopReason)
	}
	var parts []string
	toolCalls := make([]openAIToolCall, 0)
	for _, part := range response.Content {
		switch part.Type {
		case "text":
			parts = append(parts, part.Text)
		case "tool_use":
			if strings.TrimSpace(part.ID) == "" || strings.TrimSpace(part.Name) == "" || !json.Valid(part.Input) {
				return nil, fmt.Errorf("%w: malformed tool_use response", ErrInvalidMessage)
			}
			toolCalls = append(toolCalls, openAIToolCall{ID: part.ID, Type: "function", Function: openAIFunction{Name: part.Name, Arguments: string(part.Input)}})
		default:
			return nil, fmt.Errorf("%w: response content block %q", ErrUnsupportedProtocol, part.Type)
		}
	}
	finish := openAIFinishReason(response.StopReason)
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	content := json.RawMessage(nil)
	if len(parts) > 0 {
		content = json.RawMessage(mustJSON(strings.Join(parts, "")))
	}
	converted := openAIResponse{ID: response.ID, Object: "chat.completion", Model: response.Model, Choices: []openAIChoice{{Index: 0, Message: openAIResponseMessage{Role: "assistant", Content: content, ToolCalls: toolCalls}, FinishReason: &finish}}}
	if response.Usage != nil {
		converted.Usage = &openAIUsage{PromptTokens: response.Usage.InputTokens, CompletionTokens: response.Usage.OutputTokens, TotalTokens: response.Usage.InputTokens + response.Usage.OutputTokens, CacheReadInputTokens: response.Usage.CacheReadInputTokens, CacheWriteInputTokens: response.Usage.CacheCreationTokens}
	}
	return json.Marshal(converted)
}

func anthropicStopReason(reason *string) string {
	if reason == nil || *reason == "" {
		return "end_turn"
	}
	switch *reason {
	case "length":
		return "max_tokens"
	case "stop":
		return "stop_sequence"
	default:
		return *reason
	}
}

func openAIFinishReason(reason string) string {
	switch reason {
	case "max_tokens":
		return "length"
	case "stop_sequence", "end_turn", "":
		return "stop"
	default:
		return reason
	}
}

func textValue(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", errors.New("content is required")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	return "", errors.New("only string text content is supported")
}

func anthropicTextValue(raw json.RawMessage) (string, error) {
	if text, err := textValue(raw); err == nil {
		return text, nil
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil || len(blocks) == 0 {
		return "", errors.New("only string or text content blocks are supported")
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "text" {
			return "", fmt.Errorf("content block %q is not supported", block.Type)
		}
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, ""), nil
}

func hasJSONValue(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null")) && !bytes.Equal(raw, []byte("[]")) && !bytes.Equal(raw, []byte("{}"))
}

func decode(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("request contains multiple JSON values")
		}
		return err
	}
	return nil
}

func validateJSON(body []byte) error {
	var value any
	return decode(body, &value)
}

func mustJSON(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}
