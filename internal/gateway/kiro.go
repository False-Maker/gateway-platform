package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

const maxKiroEventBytes = 1 << 20

type kiroTokenUsage struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
	Present    bool
}

type kiroStreamResult struct {
	Content     string
	lastContent string
	Usage       kiroTokenUsage
}

type kiroJSONStreamParser struct {
	buffer []byte
}

func prepareKiroPayload(profile contracts.UpstreamProfile, model string, payload []byte) ([]byte, error) {
	profileARN := strings.TrimSpace(profile.Metadata["profile_arn"])
	if profileARN == "" {
		return nil, errors.New("Kiro profile requires profile_arn metadata")
	}
	var request struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, fmt.Errorf("decode Kiro chat request: %w", err)
	}
	if len(request.Tools) > 0 {
		return nil, contracts.ErrUnsupportedCapability
	}
	system := []string{}
	messages := make([]map[string]any, 0, len(request.Messages))
	for _, message := range request.Messages {
		content, ok := message.Content.(string)
		if !ok {
			return nil, contracts.ErrUnsupportedCapability
		}
		switch message.Role {
		case "system", "developer":
			if content != "" {
				system = append(system, content)
			}
		case "user":
			messages = append(messages, map[string]any{"userInputMessage": map[string]any{"content": content, "modelId": model, "origin": "AI_EDITOR"}})
		case "assistant":
			messages = append(messages, map[string]any{"assistantResponseMessage": map[string]any{"content": content}})
		default:
			return nil, contracts.ErrUnsupportedCapability
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("%w: Kiro request has no user message", contracts.ErrInvalidContract)
	}
	last := messages[len(messages)-1]
	current, ok := last["userInputMessage"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: Kiro request must end with a user message", contracts.ErrInvalidContract)
	}
	if len(system) > 0 {
		current["content"] = strings.Join(system, "\n\n") + "\n\n" + current["content"].(string)
	}
	conversation := map[string]any{
		"chatTriggerType": "MANUAL",
		"conversationId":  newID("kiro"),
		"currentMessage":  map[string]any{"userInputMessage": current},
	}
	if len(messages) > 1 {
		conversation["history"] = messages[:len(messages)-1]
	}
	return json.Marshal(map[string]any{"conversationState": conversation, "profileArn": profileARN})
}

func normalizeKiroResponse(body []byte, model string) ([]byte, error) {
	parser := &kiroJSONStreamParser{}
	result := kiroStreamResult{}
	for _, event := range parser.Feed(body) {
		applyKiroEvent(&result, event)
	}
	if strings.TrimSpace(result.Content) == "" {
		return nil, errors.New("Kiro event stream has no text response")
	}
	response := map[string]any{
		"id":      newID("chatcmpl"),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": result.Content}, "finish_reason": "stop"}},
	}
	if result.Usage.Present {
		response["usage"] = kiroOpenAIUsage(result.Usage)
	}
	return json.Marshal(response)
}

func (p *kiroJSONStreamParser) Feed(chunk []byte) []map[string]any {
	if len(chunk) > 0 {
		p.buffer = append(p.buffer, chunk...)
	}
	events := []map[string]any{}
	for {
		start := bytes.IndexByte(p.buffer, '{')
		if start < 0 {
			if len(p.buffer) > maxKiroEventBytes {
				p.buffer = p.buffer[len(p.buffer)-maxKiroEventBytes:]
			}
			return events
		}
		if start > 0 {
			p.buffer = p.buffer[start:]
		}
		end := matchingJSONBrace(p.buffer)
		if end < 0 {
			if len(p.buffer) > maxKiroEventBytes {
				p.buffer = nil
			}
			return events
		}
		raw := p.buffer[:end+1]
		p.buffer = p.buffer[end+1:]
		var event map[string]any
		if json.Unmarshal(raw, &event) == nil {
			events = append(events, event)
		}
	}
}

func matchingJSONBrace(data []byte) int {
	depth := 0
	inString := false
	escaped := false
	for i, value := range data {
		if escaped {
			escaped = false
			continue
		}
		if inString && value == '\\' {
			escaped = true
			continue
		}
		if value == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch value {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func applyKiroEvent(result *kiroStreamResult, event map[string]any) string {
	content := kiroEventContent(event)
	if content != "" && content != result.lastContent {
		result.Content += content
		result.lastContent = content
	} else {
		content = ""
	}
	if usage, ok := kiroEventTokenUsage(event); ok {
		result.Usage = usage
	}
	return content
}

func kiroEventContent(event map[string]any) string {
	if followup, _ := event["followupPrompt"].(bool); followup {
		return ""
	}
	if content, ok := event["content"].(string); ok {
		return content
	}
	for _, key := range []string{"assistantResponseEvent", "contentEvent"} {
		if nested, ok := event[key].(map[string]any); ok {
			if content := kiroEventContent(nested); content != "" {
				return content
			}
		}
	}
	return ""
}

func kiroEventTokenUsage(event map[string]any) (kiroTokenUsage, bool) {
	if raw, ok := event["tokenUsage"].(map[string]any); ok {
		usage := kiroTokenUsage{Present: true}
		uncached, uncachedOK := exactNonNegativeInt(raw["uncachedInputTokens"])
		cacheRead, cacheReadOK := exactNonNegativeInt(raw["cacheReadInputTokens"])
		cacheWrite, cacheWriteOK := exactNonNegativeInt(raw["cacheWriteInputTokens"])
		output, outputOK := exactNonNegativeInt(raw["outputTokens"])
		// A partial tokenUsage object is not sufficient for admission. Missing
		// fields must remain missing rather than being silently treated as zero.
		if !(uncachedOK || cacheReadOK || cacheWriteOK) || !outputOK {
			return kiroTokenUsage{}, false
		}
		usage.Input = uncached + cacheRead + cacheWrite
		usage.Output = output
		usage.CacheRead = cacheRead
		usage.CacheWrite = cacheWrite
		return usage, true
	}
	for _, key := range []string{"messageMetadataEvent", "metadataEvent", "usageEvent"} {
		if nested, ok := event[key].(map[string]any); ok {
			if usage, found := kiroEventTokenUsage(nested); found {
				return usage, true
			}
		}
	}
	input, inputOK := exactNonNegativeInt(event["inputTokens"])
	output, outputOK := exactNonNegativeInt(event["outputTokens"])
	if inputOK || outputOK {
		return kiroTokenUsage{Input: input, Output: output, Present: true}, true
	}
	return kiroTokenUsage{}, false
}

func exactNonNegativeInt(value any) (int, bool) {
	number, ok := value.(float64)
	if !ok || number < 0 || number != math.Trunc(number) || number > float64(int(^uint(0)>>1)) {
		return 0, false
	}
	return int(number), true
}

func kiroOpenAIUsage(usage kiroTokenUsage) map[string]any {
	return map[string]any{
		"prompt_tokens":               usage.Input,
		"completion_tokens":           usage.Output,
		"total_tokens":                usage.Input + usage.Output,
		"cache_read_input_tokens":     usage.CacheRead,
		"cache_creation_input_tokens": usage.CacheWrite,
	}
}

func drainKiroStream(ctx context.Context, body io.Reader, dst io.Writer, flusher http.Flusher, model string) (*usage, bool, error, error) {
	parser := &kiroJSONStreamParser{}
	result := kiroStreamResult{}
	buffer := make([]byte, 32<<10)
	clientConnected := true
	var writeErr error
	write := func(block []byte) {
		if !clientConnected {
			return
		}
		if ctx.Err() != nil {
			clientConnected = false
			writeErr = ctx.Err()
			return
		}
		if _, err := dst.Write(block); err != nil {
			clientConnected = false
			writeErr = err
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	for {
		count, readErr := body.Read(buffer)
		for _, event := range parser.Feed(buffer[:count]) {
			content := applyKiroEvent(&result, event)
			if content != "" {
				write(openAIStreamBlock(model, map[string]any{"role": "assistant", "content": content}, nil, nil))
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return kiroUsageValue(result.Usage), true, writeErr, readErr
		}
	}
	if result.Content == "" {
		return kiroUsageValue(result.Usage), true, writeErr, errors.New("Kiro event stream has no text response")
	}
	finish := "stop"
	write(openAIStreamBlock(model, map[string]any{}, &finish, kiroUsageMap(result.Usage)))
	write([]byte("data: [DONE]\n\n"))
	return kiroUsageValue(result.Usage), writeErr != nil, writeErr, nil
}

func openAIStreamBlock(model string, delta map[string]any, finish *string, usageMap map[string]any) []byte {
	payload := map[string]any{"id": "stream", "object": "chat.completion.chunk", "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	if usageMap != nil {
		payload["usage"] = usageMap
	}
	return []byte("data: " + string(mustJSON(payload)) + "\n\n")
}

func kiroUsageMap(value kiroTokenUsage) map[string]any {
	if !value.Present {
		return nil
	}
	return kiroOpenAIUsage(value)
}

func kiroUsageValue(value kiroTokenUsage) *usage {
	if !value.Present {
		return nil
	}
	return &usage{Input: value.Input, Output: value.Output, CacheRead: value.CacheRead, CacheWrite: value.CacheWrite}
}
