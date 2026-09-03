package protokit

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestAnthropicRequestConvertsToOpenAI(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-sonnet","system":"Be concise","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"temperature":0.2,"stop_sequences":["END"]}`)
	converted, err := ConvertRequest(body, AnthropicMessages, OpenAIChat)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(converted, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "claude-3-5-sonnet" || got["max_tokens"] != float64(64) {
		t.Fatalf("unexpected request: %s", converted)
	}
	messages := got["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" || messages[1].(map[string]any)["content"] != "hello" {
		t.Fatalf("system/content mapping lost: %s", converted)
	}
}

func TestOpenAIRequestConvertsToAnthropicAndRejectsMissingMaxTokens(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":32,"messages":[{"role":"system","content":"rules"},{"role":"user","content":"hello"}]}`)
	converted, err := ConvertRequest(body, OpenAIChat, AnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(converted, &got); err != nil {
		t.Fatal(err)
	}
	if got["system"] != "rules" || got["max_tokens"] != float64(32) {
		t.Fatalf("unexpected request: %s", converted)
	}
	if len(got["messages"].([]any)) != 1 {
		t.Fatalf("system message was not moved: %s", converted)
	}
	_, err = ConvertRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`), OpenAIChat, AnthropicMessages)
	if !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("missing max_tokens error=%v", err)
	}
}

func TestResponseConversionPreservesUsageAndFinishReason(t *testing.T) {
	openAI := []byte(`{"id":"chat-1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8,"cache_read_input_tokens":2}}`)
	anthropic, err := ConvertResponse(openAI, OpenAIChat, AnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	usage, ok, err := ParseUsage(anthropic, AnthropicMessages)
	if err != nil || !ok || usage.Input != 3 || usage.Output != 5 || usage.CacheRead != 2 {
		t.Fatalf("usage=%+v present=%v err=%v body=%s", usage, ok, err, anthropic)
	}
	back, err := ConvertResponse(anthropic, AnthropicMessages, OpenAIChat)
	if err != nil {
		t.Fatal(err)
	}
	usage, ok, err = ParseUsage(back, OpenAIChat)
	if err != nil || !ok || usage.Input != 3 || usage.Output != 5 || usage.CacheRead != 2 {
		t.Fatalf("roundtrip usage=%+v present=%v err=%v body=%s", usage, ok, err, back)
	}
}

func TestAnthropicResponseConvertsToResponses(t *testing.T) {
	body := []byte(`{"id":"msg-1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":6,"cache_read_input_tokens":2,"cache_creation_input_tokens":1}}`)
	converted, err := ConvertResponse(body, AnthropicMessages, OpenAIResponses)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(converted, &got); err != nil {
		t.Fatal(err)
	}
	if got["object"] != "response" || got["status"] != "completed" {
		t.Fatalf("unexpected Responses envelope: %s", converted)
	}
	output := got["output"].([]any)
	content := output[0].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["text"] != "done" {
		t.Fatalf("response text was not preserved: %s", converted)
	}
	usage := got["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(6) || usage["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(2) {
		t.Fatalf("response usage was not preserved: %s", converted)
	}
}

func TestConversionRejectsUnsupportedCapabilities(t *testing.T) {
	_, err := ConvertRequest([]byte(`{"model":"m","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"x"}]}`), OpenAIChat, AnthropicMessages)
	if !errors.Is(err, ErrUnsupportedProtocol) {
		t.Fatalf("stream error=%v", err)
	}
	_, err = ConvertResponse([]byte(`{"id":"m","model":"m","choices":[]}`), OpenAIChat, AnthropicMessages)
	if !errors.Is(err, ErrUnsupportedProtocol) {
		t.Fatalf("multi-choice error=%v", err)
	}
}

func TestConvertSameProtocolValidatesJSON(t *testing.T) {
	if _, err := ConvertRequest([]byte(`{"model":"m"} trailing`), OpenAIChat, OpenAIChat); err == nil {
		t.Fatal("expected malformed JSON error")
	}
}

func TestResponsesRequestAndResponseRoundTrip(t *testing.T) {
	responses := []byte(`{"model":"gpt-4.1","instructions":"Be concise","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"hello"}]}],"max_output_tokens":32}`)
	chat, err := ConvertRequest(responses, OpenAIResponses, OpenAIChat)
	if err != nil {
		t.Fatal(err)
	}
	var gotChat map[string]any
	if err := json.Unmarshal(chat, &gotChat); err != nil {
		t.Fatal(err)
	}
	if gotChat["model"] != "gpt-4.1" || gotChat["max_tokens"] != float64(32) {
		t.Fatalf("unexpected canonical request: %s", chat)
	}
	messages := gotChat["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" || messages[1].(map[string]any)["role"] != "system" || messages[1].(map[string]any)["content"] != "hello" {
		t.Fatalf("input/instructions mapping lost: %s", chat)
	}

	response := []byte(`{"id":"resp-1","object":"response","model":"gpt-4.1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10,"input_tokens_details":{"cached_tokens":2}}}`)
	converted, err := ConvertResponse(response, OpenAIResponses, OpenAIChat)
	if err != nil {
		t.Fatal(err)
	}
	usage, ok, err := ParseUsage(converted, OpenAIChat)
	if err != nil || !ok || usage.Input != 4 || usage.Output != 6 || usage.CacheRead != 2 {
		t.Fatalf("response usage=%+v present=%v err=%v body=%s", usage, ok, err, converted)
	}
	back, err := ConvertResponse(converted, OpenAIChat, OpenAIResponses)
	if err != nil {
		t.Fatal(err)
	}
	var gotResponse map[string]any
	if err := json.Unmarshal(back, &gotResponse); err != nil {
		t.Fatal(err)
	}
	if gotResponse["object"] != "response" || gotResponse["status"] != "completed" {
		t.Fatalf("unexpected Responses response: %s", back)
	}
}

func TestOpenAIToolCallConvertsToAnthropicAndBack(t *testing.T) {
	body := []byte(`{"model":"claude","max_tokens":64,"tools":[{"type":"function","function":{"name":"lookup","description":"Find a record","parameters":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}}}],"messages":[{"role":"user","content":"find it"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"id\":\"42\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"record 42"}]}`)
	anthropic, err := ConvertRequest(body, OpenAIChat, AnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(anthropic, &got); err != nil {
		t.Fatal(err)
	}
	tools := got["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "lookup" {
		t.Fatalf("tool definition was not converted: %s", anthropic)
	}
	messages := got["messages"].([]any)
	assistantBlocks := messages[1].(map[string]any)["content"].([]any)
	if assistantBlocks[0].(map[string]any)["type"] != "tool_use" || assistantBlocks[0].(map[string]any)["id"] != "call_1" {
		t.Fatalf("tool_use was not converted: %s", anthropic)
	}
	back, err := ConvertRequest(anthropic, AnthropicMessages, OpenAIChat)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip map[string]any
	if err := json.Unmarshal(back, &roundTrip); err != nil {
		t.Fatal(err)
	}
	roundTripMessages := roundTrip["messages"].([]any)
	call := roundTripMessages[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" || call["function"].(map[string]any)["name"] != "lookup" || call["function"].(map[string]any)["arguments"] != `{"id":"42"}` {
		t.Fatalf("tool call round trip lost identity: %s", back)
	}
	toolResult := roundTripMessages[2].(map[string]any)
	if toolResult["role"] != "tool" || toolResult["tool_call_id"] != "call_1" || toolResult["content"] != "record 42" {
		t.Fatalf("tool result round trip lost identity: %s", back)
	}
}

func TestToolCallResponseConvertsBothDirections(t *testing.T) {
	openAI := []byte(`{"id":"chat-1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"id\":\"42\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":4}}`)
	anthropic, err := ConvertResponse(openAI, OpenAIChat, AnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(anthropic, &got); err != nil {
		t.Fatal(err)
	}
	if got["stop_reason"] != "tool_use" || got["usage"].(map[string]any)["output_tokens"] != float64(4) {
		t.Fatalf("tool response metadata was lost: %s", anthropic)
	}
	back, err := ConvertResponse(anthropic, AnthropicMessages, OpenAIChat)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(back, &response); err != nil {
		t.Fatal(err)
	}
	choice := response["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" || choice["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["id"] != "call_1" {
		t.Fatalf("tool response round trip lost identity: %s", back)
	}
}

func TestToolConversionRejectsMalformedSchemasAndBlocks(t *testing.T) {
	_, err := ConvertRequest([]byte(`{"model":"m","max_tokens":1,"tools":[{"type":"function","function":{"name":"f","parameters":[]}}],"messages":[{"role":"user","content":"x"}]}`), OpenAIChat, AnthropicMessages)
	if !errors.Is(err, ErrUnsupportedProtocol) {
		t.Fatalf("invalid tool schema error=%v", err)
	}
	_, err = ConvertRequest([]byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`), AnthropicMessages, OpenAIChat)
	if !errors.Is(err, ErrUnsupportedProtocol) && !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("unsupported block error=%v", err)
	}
}

func TestGeminiTextFixturePreservesMessagesControlsAndUsage(t *testing.T) {
	maxTokens := 32
	temperature := 0.2
	request := openAIRequest{Model: "gemini-2.5-pro", MaxTokens: &maxTokens, Temperature: &temperature, Messages: []openAIMessage{{Role: "system", Content: json.RawMessage(`"be brief"`)}, {Role: "user", Content: json.RawMessage(`"hello"`)}, {Role: "assistant", Content: json.RawMessage(`"hi"`)}}}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	gemini, err := ConvertRequest(body, OpenAIChat, GeminiGenerate)
	if err != nil {
		t.Fatal(err)
	}
	var converted map[string]any
	if err := json.Unmarshal(gemini, &converted); err != nil {
		t.Fatal(err)
	}
	if converted["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"] != "be brief" || converted["generationConfig"].(map[string]any)["maxOutputTokens"] != float64(32) {
		t.Fatalf("Gemini request mapping lost fields: %s", gemini)
	}
	contents := converted["contents"].([]any)
	if contents[0].(map[string]any)["role"] != "user" || contents[1].(map[string]any)["role"] != "model" {
		t.Fatalf("Gemini role mapping lost: %s", gemini)
	}

	fixture := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":9,"totalTokenCount":16,"cachedContentTokenCount":2}}`)
	chat, err := ConvertResponse(fixture, GeminiGenerate, OpenAIChat)
	if err != nil {
		t.Fatal(err)
	}
	usage, present, err := ParseUsage(fixture, GeminiGenerate)
	if err != nil || !present || usage.Input != 7 || usage.Output != 9 || usage.CacheRead != 2 {
		t.Fatalf("Gemini usage=%+v present=%v err=%v", usage, present, err)
	}
	var response map[string]any
	if err := json.Unmarshal(chat, &response); err != nil {
		t.Fatal(err)
	}
	message := response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "done" {
		t.Fatalf("Gemini response text was lost: %s", chat)
	}
}

func TestGeminiResponseConvertsToResponses(t *testing.T) {
	fixture := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":6,"totalTokenCount":10}}`)
	converted, err := ConvertResponse(fixture, GeminiGenerate, OpenAIResponses)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(converted, &response); err != nil {
		t.Fatal(err)
	}
	if response["object"] != "response" || response["status"] != "completed" {
		t.Fatalf("unexpected Responses response: %s", converted)
	}
	output := response["output"].([]any)[0].(map[string]any)
	content := output["content"].([]any)[0].(map[string]any)
	if content["text"] != "done" {
		t.Fatalf("Gemini text was not converted: %s", converted)
	}
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(6) {
		t.Fatalf("Gemini usage was not converted: %s", converted)
	}
}

func TestGeminiFixtureRejectsUnsupportedShapes(t *testing.T) {
	_, err := ConvertRequest([]byte(`{"model":"gemini","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"x"}]}`), OpenAIChat, GeminiGenerate)
	if !errors.Is(err, ErrUnsupportedProtocol) {
		t.Fatalf("Gemini tool error=%v", err)
	}
	_, err = ConvertResponse([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"one"}]}},{"content":{"role":"model","parts":[{"text":"two"}]}}]}`), GeminiGenerate, OpenAIChat)
	if !errors.Is(err, ErrUnsupportedProtocol) {
		t.Fatalf("Gemini multiple candidate error=%v", err)
	}
}

func TestResponsesFunctionCallRoundTrip(t *testing.T) {
	body := []byte(`{"model":"gpt-5","tools":[{"type":"function","name":"lookup","description":"Find","parameters":{"type":"object","properties":{"id":{"type":"string"}}}}],"input":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"id\":\"42\"}"},{"type":"function_call_output","call_id":"call_1","output":"record 42"}]}`)
	chat, err := ConvertRequest(body, OpenAIResponses, OpenAIChat)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(chat, &request); err != nil {
		t.Fatal(err)
	}
	messages := request["messages"].([]any)
	if messages[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["id"] != "call_1" || messages[1].(map[string]any)["tool_call_id"] != "call_1" {
		t.Fatalf("Responses tool input was not converted: %s", chat)
	}
	back, err := ConvertRequest(chat, OpenAIChat, OpenAIResponses)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(back), `"function_call_output"`) || !strings.Contains(string(back), `"call_id":"call_1"`) {
		t.Fatalf("Responses tool input round trip lost items: %s", back)
	}
	response, err := ConvertResponse([]byte(`{"id":"chat-1","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"id\":\"42\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":4}}`), OpenAIChat, OpenAIResponses)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response), `"type":"function_call"`) || !strings.Contains(string(response), `"arguments":"{\"id\":\"42\"}"`) {
		t.Fatalf("Responses tool output was not converted: %s", response)
	}
}

func TestResponsesResponseConvertsToAnthropic(t *testing.T) {
	body := []byte(`{"id":"resp-1","object":"response","model":"gpt-5","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"id\":\"42\"}"}],"usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10,"input_tokens_details":{"cached_tokens":2}}}`)
	converted, err := ConvertResponse(body, OpenAIResponses, AnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(converted, &response); err != nil {
		t.Fatal(err)
	}
	content := response["content"].([]any)
	if content[0].(map[string]any)["text"] != "done" || content[1].(map[string]any)["type"] != "tool_use" || content[1].(map[string]any)["id"] != "call_1" {
		t.Fatalf("Responses output was not converted: %s", converted)
	}
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(6) || usage["cache_read_input_tokens"] != float64(2) {
		t.Fatalf("Responses usage was not converted: %s", converted)
	}
}

func TestGeminiResponseConvertsToAnthropic(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":6,"totalTokenCount":10,"cachedContentTokenCount":2}}`)
	converted, err := ConvertResponse(body, GeminiGenerate, AnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(converted, &response); err != nil {
		t.Fatal(err)
	}
	content := response["content"].([]any)
	if content[0].(map[string]any)["text"] != "done" || response["stop_reason"] != "max_tokens" {
		t.Fatalf("Gemini output was not converted: %s", converted)
	}
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(6) || usage["cache_read_input_tokens"] != float64(2) {
		t.Fatalf("Gemini usage was not converted: %s", converted)
	}
}

func TestResponsesRejectsUnsupportedInput(t *testing.T) {
	_, err := ConvertRequest([]byte(`{"model":"m","input":"x","stream":true}`), OpenAIResponses, OpenAIChat)
	if !errors.Is(err, ErrUnsupportedProtocol) {
		t.Fatalf("stream error=%v", err)
	}
	_, err = ConvertRequest([]byte(`{"model":"m","input":[{"type":"input_image","role":"user","content":"x"}]}`), OpenAIResponses, OpenAIChat)
	if !errors.Is(err, ErrUnsupportedProtocol) {
		t.Fatalf("image error=%v", err)
	}
}
