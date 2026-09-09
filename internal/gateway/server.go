package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/internal/events"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/elucid/gateway-platform/pkg/protokit"
)

var (
	ErrNoAccount        = errors.New("no schedulable account")
	ErrQuotaExhausted   = errors.New("all eligible accounts have exhausted quota")
	ErrSnapshotStale    = errors.New("gateway snapshot is unavailable or stale")
	ErrReleaseFailed    = errors.New("release event could not be persisted")
	ErrAttemptFailed    = errors.New("attempt_started event could not be persisted")
	ErrNetwork          = errors.New("network_error")
	ErrUsageUnavailable = errors.New("upstream response did not contain usage")
)

const releaseWriteTimeout = 3 * time.Second

type ChatCompletionRequest struct {
	Model       string           `json:"model"`
	Messages    []map[string]any `json:"messages"`
	Stream      bool             `json:"stream,omitempty"`
	Tools       []map[string]any `json:"tools,omitempty"`
	MaxTokens   *int             `json:"max_tokens,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
	TopP        *float64         `json:"top_p,omitempty"`
	Stop        any              `json:"stop,omitempty"`
}

type Server struct {
	Chooser        *Chooser
	Producer       events.Producer
	Limiter        Limiter
	HTTPClient     *http.Client
	RequestTimeout time.Duration
	Provider       string
	snapshotFresh  func() bool
}

func NewServer(producer events.Producer, provider string) *Server {
	if provider == "" {
		provider = "apikey"
	}
	return &Server{Chooser: NewChooser(nil), Producer: producer, Limiter: Limiter{Redis: producer.Redis}, HTTPClient: &http.Client{}, RequestTimeout: 120 * time.Second, Provider: provider}
}

func (s *Server) HandleNonStreaming(ctx context.Context, tenantID, group string, request ChatCompletionRequest) ([]byte, int, error) {
	return s.handleNonStreaming(ctx, tenantID, group, request, protokit.OpenAIChat)
}

// HandleMessages accepts an Anthropic Messages request and returns an
// Anthropic Messages response. The request is normalized before account
// selection, so the existing OpenAI request path remains the canonical flow.
func (s *Server) HandleMessages(ctx context.Context, tenantID, group string, body []byte) ([]byte, int, error) {
	canonical, err := protokit.ConvertRequest(body, protokit.AnthropicMessages, protokit.OpenAIChat)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	var request ChatCompletionRequest
	if err := json.Unmarshal(canonical, &request); err != nil {
		return nil, http.StatusBadRequest, err
	}
	return s.handleNonStreaming(ctx, tenantID, group, request, protokit.AnthropicMessages)
}

func (s *Server) HandleMessagesStreaming(ctx context.Context, tenantID, group string, body []byte, dst http.ResponseWriter) (int, error) {
	model, stream, err := streamingRequestMetadata(body)
	if err != nil {
		return http.StatusBadRequest, err
	}
	if !stream {
		return http.StatusBadRequest, errors.New("stream must be true")
	}
	if err := validateStreamingProtocolBody(body, protokit.AnthropicMessages); err != nil {
		return http.StatusBadRequest, err
	}
	return s.handleProtocolStreaming(ctx, tenantID, group, body, protokit.AnthropicMessages, model, dst)
}

// HandleResponses accepts an OpenAI Responses request and returns an OpenAI
// Responses response. The request is normalized before account selection.
func (s *Server) HandleResponses(ctx context.Context, tenantID, group string, body []byte) ([]byte, int, error) {
	canonical, err := protokit.ConvertRequest(body, protokit.OpenAIResponses, protokit.OpenAIChat)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	var request ChatCompletionRequest
	if err := json.Unmarshal(canonical, &request); err != nil {
		return nil, http.StatusBadRequest, err
	}
	return s.handleNonStreaming(ctx, tenantID, group, request, protokit.OpenAIResponses)
}

func (s *Server) HandleResponsesStreaming(ctx context.Context, tenantID, group string, body []byte, dst http.ResponseWriter) (int, error) {
	model, stream, err := streamingRequestMetadata(body)
	if err != nil {
		return http.StatusBadRequest, err
	}
	if !stream {
		return http.StatusBadRequest, errors.New("stream must be true")
	}
	if err := validateStreamingProtocolBody(body, protokit.OpenAIResponses); err != nil {
		return http.StatusBadRequest, err
	}
	return s.handleProtocolStreaming(ctx, tenantID, group, body, protokit.OpenAIResponses, model, dst)
}

// HandleStreaming proxies an OpenAI Chat SSE response. Streaming attempts are
// intentionally single-account: once bytes are sent, failover cannot preserve
// the response contract. Upstream draining uses a detached deadline so a
// client disconnect still produces a terminal release event.
func (s *Server) HandleStreaming(ctx context.Context, tenantID, group string, request ChatCompletionRequest, dst http.ResponseWriter) (int, error) {
	if tenantID == "" {
		return http.StatusUnauthorized, errors.New("tenant authentication required")
	}
	if request.Model == "" {
		return http.StatusBadRequest, errors.New("model is required")
	}
	if !request.Stream || containsUnsupportedMessageFields(request.Messages) || containsImageMessageFields(request.Messages) {
		return http.StatusBadRequest, contracts.ErrUnsupportedCapability
	}
	if len(request.Tools) > 0 {
		return http.StatusBadRequest, contracts.ErrUnsupportedCapability
	}
	payload, err := marshalStreamingRequest(request)
	if err != nil {
		return http.StatusBadRequest, err
	}
	return s.handleProtocolStreaming(ctx, tenantID, group, payload, protokit.OpenAIChat, request.Model, dst)
}

func streamingRequestMetadata(body []byte) (string, bool, error) {
	var request struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return "", false, err
	}
	if strings.TrimSpace(request.Model) == "" {
		return "", false, errors.New("model is required")
	}
	return request.Model, request.Stream, nil
}

func validateStreamingProtocolBody(body []byte, protocol protokit.Protocol) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		return err
	}
	delete(object, "stream")
	normalized, err := json.Marshal(object)
	if err != nil {
		return err
	}
	canonical, err := protokit.ConvertRequest(normalized, protocol, protokit.OpenAIChat)
	if err != nil {
		return err
	}
	var request struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(canonical, &request); err != nil {
		return err
	}
	if containsImageMessageFields(request.Messages) {
		return fmt.Errorf("%w: image content is not supported for streaming", contracts.ErrUnsupportedCapability)
	}
	return nil
}

func streamingBodyHasTools(body []byte) bool {
	var request struct {
		Tools json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(body, &request) != nil {
		return true
	}
	raw := bytes.TrimSpace(request.Tools)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null")) && !bytes.Equal(raw, []byte("[]"))
}

func convertStreamingRequest(body []byte, from, to protokit.Protocol) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, err
	}
	delete(object, "stream")
	normalized, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	var converted []byte
	if from != protokit.OpenAIChat && to != protokit.OpenAIChat {
		canonical, convertErr := protokit.ConvertRequest(normalized, from, protokit.OpenAIChat)
		if convertErr != nil {
			return nil, convertErr
		}
		converted, err = protokit.ConvertRequest(canonical, protokit.OpenAIChat, to)
	} else {
		converted, err = protokit.ConvertRequest(normalized, from, to)
	}
	if err != nil {
		return nil, err
	}
	var convertedObject map[string]any
	if err := json.Unmarshal(converted, &convertedObject); err != nil {
		return nil, err
	}
	if to != protokit.GeminiGenerate {
		convertedObject["stream"] = true
	}
	if to == protokit.OpenAIChat {
		convertedObject["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(convertedObject)
}

func (s *Server) handleProtocolStreaming(ctx context.Context, tenantID, group string, payload []byte, inboundProtocol protokit.Protocol, model string, dst http.ResponseWriter) (int, error) {
	if tenantID == "" {
		return http.StatusUnauthorized, errors.New("tenant authentication required")
	}
	if strings.TrimSpace(model) == "" {
		return http.StatusBadRequest, errors.New("model is required")
	}
	if streamingBodyHasTools(payload) {
		return http.StatusBadRequest, contracts.ErrUnsupportedCapability
	}
	if group == "" {
		group = "default"
	}
	if s.snapshotFresh != nil && !s.snapshotFresh() {
		return http.StatusServiceUnavailable, ErrSnapshotStale
	}
	deadline := s.RequestTimeout
	if deadline <= 0 {
		deadline = 120 * time.Second
	}
	requestCtx, requestCancel := context.WithTimeout(ctx, deadline)
	defer requestCancel()
	requestID := newID("req")
	lease, err := s.Chooser.Acquire(contracts.Criteria{Platform: s.Provider, Model: model, Group: group})
	if err != nil {
		if errors.Is(err, ErrQuotaExhausted) {
			return http.StatusTooManyRequests, err
		}
		return http.StatusServiceUnavailable, err
	}
	upstreamProtocol, err := selectedProtocol(s.Provider, lease.Profile)
	if err != nil {
		return http.StatusBadGateway, err
	}
	if upstreamProtocol != inboundProtocol {
		payload, err = convertStreamingRequest(payload, inboundProtocol, upstreamProtocol)
		if err != nil {
			return http.StatusBadRequest, err
		}
	}
	payload, err = prepareProviderPayload(s.Provider, lease.Profile, model, payload)
	if err != nil {
		return http.StatusBadGateway, err
	}
	releaseLimiter, err := s.Limiter.Acquire(requestCtx, lease)
	if err != nil {
		return http.StatusTooManyRequests, err
	}
	defer releaseLimiter()
	attemptID := newID("attempt")
	startedAt := time.Now().UTC()
	started := contracts.AttemptStarted{SchemaVersion: contracts.SchemaVersion, EventID: newID("start"), RequestID: requestID, AttemptID: attemptID, AttemptNo: 1, ProducerID: s.Producer.ProducerID, OccurredAt: startedAt, AccountID: lease.AccountID, Provider: s.Provider, Model: model, TenantID: tenantID, DeadlineAt: startedAt.Add(deadline)}
	startedEvent, err := contracts.NewStreamEvent(contracts.EventTypeAttemptStarted, started.EventID, requestID, attemptID, s.Producer.ProducerID, startedAt, started)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	if _, err := s.Producer.Add(requestCtx, startedEvent); err != nil {
		return http.StatusServiceUnavailable, ErrAttemptFailed
	}

	upstreamCtx, upstreamCancel := context.WithTimeout(context.WithoutCancel(ctx), deadline)
	defer upstreamCancel()
	path := lease.Profile.InferencePath
	if path == "" {
		switch upstreamProtocol {
		case protokit.AnthropicMessages:
			path = "/v1/messages"
		case protokit.OpenAIResponses:
			path = "/responses"
		case protokit.GeminiGenerate:
			path = "/v1beta/models/{model}:streamGenerateContent?alt=sse"
		default:
			path = "/v1/chat/completions"
		}
	}
	path = providerStreamingPath(s.Provider, path)
	endpoint := strings.TrimRight(lease.Profile.BaseURL, "/") + resolveInferencePath(path, model)
	upstreamRequest, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return s.finishStreamingRelease(requestCtx, tenantID, requestID, attemptID, startedAt, lease, ChatCompletionRequest{Model: model}, 0, nil, false, true, err)
	}
	upstreamRequest.Header.Set("Content-Type", "application/json")
	if lease.Credential.AccessToken != "" {
		if upstreamProtocol == protokit.AnthropicMessages && lease.Credential.Kind == "static" {
			upstreamRequest.Header.Set("x-api-key", lease.Credential.AccessToken)
		} else if upstreamProtocol == protokit.GeminiGenerate && lease.Credential.Kind == "static" {
			upstreamRequest.Header.Set("x-goog-api-key", lease.Credential.AccessToken)
		} else {
			upstreamRequest.Header.Set("Authorization", "Bearer "+lease.Credential.AccessToken)
		}
	}
	if lease.Profile.UserAgent != "" {
		upstreamRequest.Header.Set("User-Agent", lease.Profile.UserAgent)
	}
	for key, value := range lease.Profile.ExtraHeaders {
		upstreamRequest.Header.Set(key, value)
	}
	applyProviderRequestHeaders(upstreamRequest.Header, s.Provider, payload)
	if upstreamProtocol == protokit.AnthropicMessages {
		upstreamRequest.Header.Set("anthropic-version", "2023-06-01")
	}
	client := s.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	client, err = clientForProxy(client, lease.Profile.Proxy)
	if err != nil {
		return s.finishStreamingRelease(requestCtx, tenantID, requestID, attemptID, startedAt, lease, ChatCompletionRequest{Model: model}, 0, nil, false, true, err)
	}
	response, err := client.Do(upstreamRequest)
	if err != nil {
		return s.finishStreamingRelease(requestCtx, tenantID, requestID, attemptID, startedAt, lease, ChatCompletionRequest{Model: model}, 0, nil, false, true, err)
	}
	defer response.Body.Close()

	if response.StatusCode >= 400 {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<20))
		if readErr != nil {
			return s.finishStreamingRelease(requestCtx, tenantID, requestID, attemptID, startedAt, lease, ChatCompletionRequest{Model: model}, response.StatusCode, nil, false, true, readErr)
		}
		callErr := fmt.Errorf("upstream returned HTTP %d", response.StatusCode)
		releaseErr := s.writeStreamingRelease(requestCtx, tenantID, requestID, attemptID, startedAt, lease, ChatCompletionRequest{Model: model}, response.StatusCode, nil, false, classifyHTTPWithReset(response.StatusCode, body, retryAfter(response.Header, time.Now())), callErr)
		if releaseErr != nil {
			return http.StatusServiceUnavailable, releaseErr
		}
		s.Chooser.MarkFailure(lease.AccountID, model, classifyHTTPWithReset(response.StatusCode, body, retryAfter(response.Header, time.Now())), retryAfter(response.Header, time.Now()))
		return response.StatusCode, callErr
	}

	dst.Header().Set("Content-Type", "text/event-stream")
	dst.Header().Set("Cache-Control", "no-cache")
	dst.Header().Set("X-Accel-Buffering", "no")
	dst.WriteHeader(http.StatusOK)
	flusher, _ := dst.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	var parsedUsage *usage
	var partial bool
	var readErr error
	if strings.EqualFold(s.Provider, "kiro") {
		parsedUsage, partial, _, readErr = drainKiroStream(ctx, response.Body, dst, flusher, model)
	} else {
		parsedUsage, partial, _, readErr = drainSSEProtocol(ctx, response.Body, dst, flusher, upstreamProtocol, inboundProtocol, model, s.Provider)
	}
	if readErr != nil {
		class := contracts.ErrorNetwork
		resultErr := error(ErrNetwork)
		if errors.Is(readErr, contracts.ErrUnsupportedCapability) {
			class = contracts.ErrorForbiddenCapability
			resultErr = readErr
		}
		releaseErr := s.writeStreamingRelease(requestCtx, tenantID, requestID, attemptID, startedAt, lease, ChatCompletionRequest{Model: model}, http.StatusOK, parsedUsage, partial, class, readErr)
		if releaseErr != nil {
			return http.StatusServiceUnavailable, releaseErr
		}
		return http.StatusOK, resultErr
	}
	releaseErr := s.writeStreamingRelease(requestCtx, tenantID, requestID, attemptID, startedAt, lease, ChatCompletionRequest{Model: model}, http.StatusOK, parsedUsage, partial, contracts.ErrorOK, nil)
	if releaseErr != nil {
		return http.StatusServiceUnavailable, releaseErr
	}
	if parsedUsage == nil {
		return http.StatusBadGateway, ErrUsageUnavailable
	}
	return http.StatusOK, nil
}

func marshalStreamingRequest(request ChatCompletionRequest) ([]byte, error) {
	payload := make(map[string]any)
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return nil, err
	}
	payload["stream"] = true
	payload["stream_options"] = map[string]any{"include_usage": true}
	return json.Marshal(payload)
}

func drainSSE(ctx context.Context, body io.Reader, dst io.Writer, flusher http.Flusher) (*usage, bool, error, error) {
	return drainSSEProtocol(ctx, body, dst, flusher, protokit.OpenAIChat, protokit.OpenAIChat, "", "")
}

const maxSSELineBytes = 1 << 20

func drainSSEProtocol(ctx context.Context, body io.Reader, dst io.Writer, flusher http.Flusher, protocol, inboundProtocol protokit.Protocol, model, provider string) (*usage, bool, error, error) {
	reader := bufio.NewReader(body)
	var accumulator streamUsageAccumulator
	converter := newProtocolSSEConverter(protocol, inboundProtocol, model)
	clientConnected := true
	done := false
	var writeErr error
	eventType := ""
	eventData := ""
	write := func(data []byte) {
		if !clientConnected {
			return
		}
		if ctx.Err() != nil {
			clientConnected = false
			writeErr = ctx.Err()
			return
		}
		if _, err := dst.Write(data); err != nil {
			clientConnected = false
			writeErr = err
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	for {
		line, err := reader.ReadString('\n')
		if len(line) > maxSSELineBytes {
			return accumulator.value(), true, writeErr, fmt.Errorf("SSE line exceeds %d bytes", maxSSELineBytes)
		}
		if len(line) > 0 {
			trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"))
			switch {
			case strings.HasPrefix(trimmed, "event:"):
				eventType = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			case strings.HasPrefix(trimmed, "data:"):
				data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if data != "" {
					data = normalizeProviderStreamEvent(provider, data)
					eventData = data
					accumulator.observe(protocol, data)
					if streamEventDone(protocol, eventType, data) {
						done = true
					}
				}
			case trimmed == "":
				if protocol == inboundProtocol {
					write([]byte(eventSSEBlock(eventType, eventData)))
				} else if eventData != "" {
					blocks, convertErr := converter.convert(eventType, eventData)
					if convertErr != nil {
						return accumulator.value(), true, writeErr, convertErr
					}
					for _, block := range blocks {
						write([]byte(block))
					}
				}
				eventType = ""
				eventData = ""
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return accumulator.value(), !done || writeErr != nil, writeErr, err
		}
	}
	if eventData != "" {
		if protocol == inboundProtocol {
			write([]byte(eventSSEBlock(eventType, eventData)))
		} else {
			blocks, convertErr := converter.convert(eventType, eventData)
			if convertErr != nil {
				return accumulator.value(), true, writeErr, convertErr
			}
			for _, block := range blocks {
				write([]byte(block))
			}
		}
	}
	return accumulator.value(), !done || writeErr != nil, writeErr, nil
}

func eventSSEBlock(eventType, data string) string {
	var builder strings.Builder
	if eventType != "" {
		builder.WriteString("event: ")
		builder.WriteString(eventType)
		builder.WriteString("\n")
	}
	builder.WriteString("data: ")
	builder.WriteString(data)
	builder.WriteString("\n\n")
	return builder.String()
}

type protocolSSEConverter struct {
	source         protokit.Protocol
	target         protokit.Protocol
	model          string
	usage          streamUsageAccumulator
	started        bool
	contentStarted bool
	finished       bool
	finishReason   string
	text           strings.Builder
	sequenceNumber int
}

func newProtocolSSEConverter(source, target protokit.Protocol, model string) *protocolSSEConverter {
	return &protocolSSEConverter{source: source, target: target, model: model}
}

func (c *protocolSSEConverter) convert(eventType, data string) ([]string, error) {
	if c.target != protokit.OpenAIChat && c.finished {
		return nil, nil
	}
	text, finishReason, done, err := decodeProtocolStreamEvent(c.source, eventType, data)
	if err != nil {
		return nil, err
	}
	if c.target == protokit.OpenAIChat {
		return convertSSEBlock(c.source, c.target, eventType, data, c.model), nil
	}
	c.usage.observe(c.source, data)
	if finishReason != "" {
		c.finishReason = finishReason
	}
	switch c.target {
	case protokit.AnthropicMessages:
		return c.convertToAnthropic(text, done), nil
	case protokit.OpenAIResponses:
		return c.convertToResponses(text, done), nil
	default:
		return nil, fmt.Errorf("%w: cannot encode streaming protocol %s", contracts.ErrUnsupportedCapability, c.target)
	}
}

func decodeProtocolStreamEvent(protocol protokit.Protocol, eventType, data string) (string, string, bool, error) {
	if protocol == protokit.OpenAIChat && data == "[DONE]" {
		return "", "", true, nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &envelope) != nil {
		return "", "", false, fmt.Errorf("%w: invalid %s stream event", contracts.ErrUnsupportedCapability, protocol)
	}
	switch protocol {
	case protokit.OpenAIChat:
		var response struct {
			Choices []struct {
				Delta        map[string]json.RawMessage `json:"delta"`
				FinishReason string                     `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &response) != nil {
			return "", "", false, unsupportedStreamEvent(protocol, "invalid chunk")
		}
		if len(response.Choices) == 0 {
			return "", "", false, nil
		}
		if len(response.Choices) != 1 {
			return "", "", false, unsupportedStreamEvent(protocol, "multiple choices")
		}
		choice := response.Choices[0]
		for field, raw := range choice.Delta {
			trimmed := bytes.TrimSpace(raw)
			if field != "role" && field != "content" && len(trimmed) != 0 &&
				!bytes.Equal(trimmed, []byte("null")) && !bytes.Equal(trimmed, []byte("[]")) && !bytes.Equal(trimmed, []byte("{}")) {
				return "", "", false, unsupportedStreamEvent(protocol, "delta."+field)
			}
		}
		var text string
		if raw := bytes.TrimSpace(choice.Delta["content"]); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
			if json.Unmarshal(raw, &text) != nil {
				return "", "", false, unsupportedStreamEvent(protocol, "non-text content")
			}
		}
		return text, canonicalStreamFinishReason(choice.FinishReason), false, nil
	case protokit.AnthropicMessages:
		var response struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &response) != nil {
			return "", "", false, unsupportedStreamEvent(protocol, "invalid event")
		}
		typeValue := response.Type
		if typeValue == "" {
			typeValue = eventType
		}
		switch typeValue {
		case "message_start", "content_block_stop", "message_delta", "ping":
			return "", canonicalStreamFinishReason(response.Delta.StopReason), false, nil
		case "content_block_start":
			if response.ContentBlock.Type != "" && response.ContentBlock.Type != "text" {
				return "", "", false, unsupportedStreamEvent(protocol, "content_block_start."+response.ContentBlock.Type)
			}
			return "", "", false, nil
		case "content_block_delta":
			if response.Delta.Type != "" && response.Delta.Type != "text_delta" {
				return "", "", false, unsupportedStreamEvent(protocol, "content_block_delta."+response.Delta.Type)
			}
			return response.Delta.Text, "", false, nil
		case "message_stop":
			return "", "", true, nil
		default:
			return "", "", false, unsupportedStreamEvent(protocol, typeValue)
		}
	case protokit.OpenAIResponses:
		var response struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
			Item  struct {
				Type string `json:"type"`
			} `json:"item"`
			Part struct {
				Type string `json:"type"`
			} `json:"part"`
			Response struct {
				Status            string `json:"status"`
				IncompleteDetails *struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(data), &response) != nil {
			return "", "", false, unsupportedStreamEvent(protocol, "invalid event")
		}
		typeValue := response.Type
		if typeValue == "" {
			typeValue = eventType
		}
		switch typeValue {
		case "response.output_text.delta":
			return response.Delta, "", false, nil
		case "response.created", "response.queued", "response.in_progress", "response.output_text.done", "response.output_item.done":
			return "", "", false, nil
		case "response.output_item.added":
			if response.Item.Type != "" && response.Item.Type != "message" {
				return "", "", false, unsupportedStreamEvent(protocol, "output_item."+response.Item.Type)
			}
			return "", "", false, nil
		case "response.content_part.added", "response.content_part.done":
			if response.Part.Type != "" && response.Part.Type != "output_text" {
				return "", "", false, unsupportedStreamEvent(protocol, "content_part."+response.Part.Type)
			}
			return "", "", false, nil
		case "response.completed", "response.done":
			finishReason := "stop"
			if response.Response.IncompleteDetails != nil {
				finishReason = canonicalStreamFinishReason(response.Response.IncompleteDetails.Reason)
			} else if response.Response.Status == "incomplete" {
				finishReason = "length"
			}
			return "", finishReason, true, nil
		default:
			return "", "", false, unsupportedStreamEvent(protocol, typeValue)
		}
	case protokit.GeminiGenerate:
		var response struct {
			Candidates []struct {
				Content struct {
					Parts []map[string]json.RawMessage `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			UsageMetadata json.RawMessage `json:"usageMetadata"`
		}
		if json.Unmarshal([]byte(data), &response) != nil {
			return "", "", false, unsupportedStreamEvent(protocol, "invalid chunk")
		}
		if len(response.Candidates) == 0 {
			if len(bytes.TrimSpace(response.UsageMetadata)) != 0 {
				return "", "", false, nil
			}
			return "", "", false, unsupportedStreamEvent(protocol, "missing candidate")
		}
		if len(response.Candidates) != 1 {
			return "", "", false, unsupportedStreamEvent(protocol, "multiple candidates")
		}
		candidate := response.Candidates[0]
		var text strings.Builder
		for _, part := range candidate.Content.Parts {
			if len(part) != 1 {
				return "", "", false, unsupportedStreamEvent(protocol, "non-text part")
			}
			raw, ok := part["text"]
			if !ok {
				return "", "", false, unsupportedStreamEvent(protocol, "non-text part")
			}
			var partText string
			if json.Unmarshal(raw, &partText) != nil {
				return "", "", false, unsupportedStreamEvent(protocol, "non-text part")
			}
			text.WriteString(partText)
		}
		finishReason := canonicalStreamFinishReason(candidate.FinishReason)
		return text.String(), finishReason, finishReason != "", nil
	}
	return "", "", false, unsupportedStreamEvent(protocol, eventType)
}

func unsupportedStreamEvent(protocol protokit.Protocol, detail string) error {
	return fmt.Errorf("%w: cannot convert %s stream event %q", contracts.ErrUnsupportedCapability, protocol, detail)
}

func canonicalStreamFinishReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "":
		return ""
	case "length", "max_tokens", "max_output_tokens":
		return "length"
	default:
		return "stop"
	}
}

func (c *protocolSSEConverter) convertToAnthropic(text string, done bool) []string {
	blocks := make([]string, 0, 4)
	if text != "" || done {
		blocks = append(blocks, c.startAnthropic()...)
	}
	if text != "" {
		payload := map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "text_delta", "text": text},
		}
		blocks = append(blocks, eventSSEBlock("content_block_delta", string(mustJSON(payload))))
	}
	if !done {
		return blocks
	}
	if c.contentStarted {
		payload := map[string]any{"type": "content_block_stop", "index": 0}
		blocks = append(blocks, eventSSEBlock("content_block_stop", string(mustJSON(payload))))
	}
	stopReason := "end_turn"
	if c.finishReason == "length" {
		stopReason = "max_tokens"
	}
	payload := map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": anthropicStreamUsage(c.usage.value()),
	}
	blocks = append(blocks, eventSSEBlock("message_delta", string(mustJSON(payload))))
	blocks = append(blocks, eventSSEBlock("message_stop", string(mustJSON(map[string]any{"type": "message_stop"}))))
	c.finished = true
	return blocks
}

func (c *protocolSSEConverter) startAnthropic() []string {
	if c.started {
		return nil
	}
	c.started = true
	c.contentStarted = true
	message := map[string]any{
		"id": "msg_stream", "type": "message", "role": "assistant", "model": c.model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": anthropicStreamUsage(c.usage.value()),
	}
	start := map[string]any{"type": "message_start", "message": message}
	content := map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	}
	return []string{
		eventSSEBlock("message_start", string(mustJSON(start))),
		eventSSEBlock("content_block_start", string(mustJSON(content))),
	}
}

func anthropicStreamUsage(parsed *usage) map[string]any {
	value := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if parsed == nil {
		return value
	}
	value["input_tokens"] = parsed.Input
	value["output_tokens"] = parsed.Output
	if parsed.CacheRead != 0 {
		value["cache_read_input_tokens"] = parsed.CacheRead
	}
	if parsed.CacheWrite != 0 {
		value["cache_creation_input_tokens"] = parsed.CacheWrite
	}
	return value
}

func (c *protocolSSEConverter) convertToResponses(text string, done bool) []string {
	blocks := make([]string, 0, 6)
	if text != "" || done {
		blocks = append(blocks, c.startResponses()...)
	}
	if text != "" {
		c.text.WriteString(text)
		blocks = append(blocks, c.responsesEvent("response.output_text.delta", map[string]any{
			"item_id": "msg_stream", "output_index": 0, "content_index": 0, "delta": text,
		}))
	}
	if !done {
		return blocks
	}
	fullText := c.text.String()
	part := map[string]any{"type": "output_text", "text": fullText, "annotations": []any{}}
	item := map[string]any{
		"id": "msg_stream", "type": "message", "status": "completed", "role": "assistant",
		"content": []any{part},
	}
	blocks = append(blocks, c.responsesEvent("response.output_text.done", map[string]any{
		"item_id": "msg_stream", "output_index": 0, "content_index": 0, "text": fullText,
	}))
	blocks = append(blocks, c.responsesEvent("response.content_part.done", map[string]any{
		"item_id": "msg_stream", "output_index": 0, "content_index": 0, "part": part,
	}))
	blocks = append(blocks, c.responsesEvent("response.output_item.done", map[string]any{
		"output_index": 0, "item": item,
	}))
	status := "completed"
	response := map[string]any{
		"id": "resp_stream", "object": "response", "status": status, "model": c.model,
		"output": []any{item}, "usage": responsesStreamUsage(c.usage.value()),
	}
	if c.finishReason == "length" {
		response["status"] = "incomplete"
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	blocks = append(blocks, c.responsesEvent("response.completed", map[string]any{"response": response}))
	c.finished = true
	return blocks
}

func (c *protocolSSEConverter) startResponses() []string {
	if c.started {
		return nil
	}
	c.started = true
	response := map[string]any{
		"id": "resp_stream", "object": "response", "status": "in_progress", "model": c.model,
		"output": []any{},
	}
	item := map[string]any{
		"id": "msg_stream", "type": "message", "status": "in_progress", "role": "assistant",
		"content": []any{},
	}
	part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
	return []string{
		c.responsesEvent("response.created", map[string]any{"response": response}),
		c.responsesEvent("response.in_progress", map[string]any{"response": response}),
		c.responsesEvent("response.output_item.added", map[string]any{"output_index": 0, "item": item}),
		c.responsesEvent("response.content_part.added", map[string]any{"item_id": "msg_stream", "output_index": 0, "content_index": 0, "part": part}),
	}
}

func (c *protocolSSEConverter) responsesEvent(eventType string, fields map[string]any) string {
	payload := make(map[string]any, len(fields)+2)
	for key, value := range fields {
		payload[key] = value
	}
	payload["type"] = eventType
	payload["sequence_number"] = c.sequenceNumber
	c.sequenceNumber++
	return eventSSEBlock(eventType, string(mustJSON(payload)))
}

func responsesStreamUsage(parsed *usage) map[string]any {
	value := map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	if parsed == nil {
		return value
	}
	value["input_tokens"] = parsed.Input
	value["output_tokens"] = parsed.Output
	value["total_tokens"] = parsed.Input + parsed.Output
	if parsed.CacheRead != 0 {
		value["input_tokens_details"] = map[string]any{"cached_tokens": parsed.CacheRead}
	}
	return value
}

func convertSSEBlock(protocol, inboundProtocol protokit.Protocol, eventType, data, model string) []string {
	if inboundProtocol != protokit.OpenAIChat {
		return nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &envelope) != nil {
		return nil
	}
	chunk := func(delta map[string]any, finish *string) string {
		choices := []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}
		payload := map[string]any{"id": "stream", "object": "chat.completion.chunk", "model": model, "choices": choices}
		return "data: " + string(mustJSON(payload)) + "\n\n"
	}
	usageChunk := func(raw json.RawMessage, usageProtocol protokit.Protocol) string {
		var wrapper struct {
			Usage json.RawMessage `json:"usage"`
		}
		if usageProtocol == protokit.AnthropicMessages {
			var message struct {
				Usage json.RawMessage `json:"usage"`
			}
			if json.Unmarshal(raw, &message) == nil {
				wrapper.Usage = message.Usage
			}
		} else if usageProtocol == protokit.OpenAIResponses {
			var response struct {
				Response struct {
					Usage json.RawMessage `json:"usage"`
				} `json:"response"`
			}
			if json.Unmarshal(raw, &response) == nil {
				wrapper.Usage = response.Response.Usage
			}
		} else {
			wrapper.Usage = envelope["usageMetadata"]
		}
		if len(wrapper.Usage) == 0 || bytes.Equal(bytes.TrimSpace(wrapper.Usage), []byte("null")) {
			return ""
		}
		usageBody := []byte(`{"usage":` + string(wrapper.Usage) + `}`)
		if usageProtocol == protokit.GeminiGenerate {
			usageBody = raw
		}
		parsed := parseUsageForProtocol(usageBody, usageProtocol)
		if parsed == nil {
			return ""
		}
		payload := map[string]any{"id": "stream", "object": "chat.completion.chunk", "model": model, "choices": []any{}, "usage": map[string]any{"prompt_tokens": parsed.Input, "completion_tokens": parsed.Output, "total_tokens": parsed.Input + parsed.Output}}
		return "data: " + string(mustJSON(payload)) + "\n\n"
	}
	switch protocol {
	case protokit.AnthropicMessages:
		if eventType == "message_stop" || envelope["type"] != nil && string(envelope["type"]) == `"message_stop"` {
			finish := "stop"
			return []string{chunk(map[string]any{}, &finish), "data: [DONE]\n\n"}
		}
		var delta struct {
			Delta struct {
				Type         string `json:"type"`
				Text         string `json:"text"`
				StopReason   string `json:"stop_reason"`
				OutputTokens int    `json:"output_tokens"`
			} `json:"delta"`
		}
		_ = json.Unmarshal([]byte(data), &delta)
		blocks := make([]string, 0, 2)
		if delta.Delta.Text != "" {
			blocks = append(blocks, chunk(map[string]any{"content": delta.Delta.Text}, nil))
		}
		if delta.Delta.StopReason != "" {
			finish := "stop"
			if delta.Delta.StopReason == "max_tokens" {
				finish = "length"
			}
			blocks = append(blocks, chunk(map[string]any{}, &finish))
		}
		if usage := usageChunk([]byte(data), protocol); usage != "" {
			blocks = append(blocks, usage)
		}
		return blocks
	case protokit.OpenAIResponses:
		typeValue := ""
		_ = json.Unmarshal(envelope["type"], &typeValue)
		if typeValue == "response.completed" || typeValue == "response.done" || eventType == "response.completed" || eventType == "response.done" {
			blocks := []string{}
			finish := "stop"
			blocks = append(blocks, chunk(map[string]any{}, &finish))
			if usage := usageChunk([]byte(data), protocol); usage != "" {
				blocks = append(blocks, usage)
			}
			blocks = append(blocks, "data: [DONE]\n\n")
			return blocks
		}
		var delta string
		_ = json.Unmarshal(envelope["delta"], &delta)
		if typeValue == "response.output_text.delta" && delta != "" {
			return []string{chunk(map[string]any{"content": delta}, nil)}
		}
	case protokit.GeminiGenerate:
		var response struct {
			Candidates []struct {
				Content      geminiStreamContent `json:"content"`
				FinishReason string              `json:"finishReason"`
			} `json:"candidates"`
		}
		if json.Unmarshal([]byte(data), &response) != nil || len(response.Candidates) == 0 {
			return nil
		}
		candidate := response.Candidates[0]
		blocks := []string{}
		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				blocks = append(blocks, chunk(map[string]any{"content": part.Text}, nil))
			}
		}
		if candidate.FinishReason != "" {
			finish := "stop"
			if candidate.FinishReason == "MAX_TOKENS" {
				finish = "length"
			}
			blocks = append(blocks, chunk(map[string]any{}, &finish))
			if usage := usageChunk([]byte(data), protocol); usage != "" {
				blocks = append(blocks, usage)
			}
			blocks = append(blocks, "data: [DONE]\n\n")
		}
		return blocks
	}
	return nil
}

type geminiStreamContent struct {
	Parts []struct {
		Text string `json:"text"`
	} `json:"parts"`
}

type streamUsageAccumulator struct {
	valueData usage
	present   bool
}

func (a *streamUsageAccumulator) observe(protocol protokit.Protocol, data string) {
	if candidate := parseUsageForProtocol([]byte(data), protocol); candidate != nil {
		a.merge(*candidate)
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &envelope) != nil {
		return
	}
	switch protocol {
	case protokit.AnthropicMessages:
		if message, ok := envelope["message"]; ok {
			var nested map[string]json.RawMessage
			if json.Unmarshal(message, &nested) == nil {
				if raw, ok := nested["usage"]; ok {
					a.mergeRaw(raw, protocol)
				}
			}
		}
	case protokit.OpenAIResponses:
		if response, ok := envelope["response"]; ok {
			var nested map[string]json.RawMessage
			if json.Unmarshal(response, &nested) == nil {
				if raw, ok := nested["usage"]; ok {
					a.mergeRaw(raw, protocol)
				}
			}
		}
	}
}

func (a *streamUsageAccumulator) mergeRaw(raw json.RawMessage, protocol protokit.Protocol) {
	envelope, err := json.Marshal(map[string]json.RawMessage{"usage": raw})
	if err != nil {
		return
	}
	if candidate := parseUsageForProtocol(envelope, protocol); candidate != nil {
		a.merge(*candidate)
	}
}

func (a *streamUsageAccumulator) merge(candidate usage) {
	if candidate.Input != 0 {
		a.valueData.Input = candidate.Input
	}
	if candidate.Output != 0 {
		a.valueData.Output = candidate.Output
	}
	if candidate.CacheRead != 0 {
		a.valueData.CacheRead = candidate.CacheRead
	}
	if candidate.CacheWrite != 0 {
		a.valueData.CacheWrite = candidate.CacheWrite
	}
	a.present = true
}

func (a *streamUsageAccumulator) value() *usage {
	if !a.present {
		return nil
	}
	value := a.valueData
	return &value
}

func streamEventDone(protocol protokit.Protocol, eventType, data string) bool {
	if protocol == protokit.OpenAIChat && data == "[DONE]" {
		return true
	}
	var envelope struct {
		Type       string `json:"type"`
		Candidates []struct {
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}
	if json.Unmarshal([]byte(data), &envelope) != nil {
		return false
	}
	if eventType != "" {
		if protocol == protokit.AnthropicMessages && eventType == "message_stop" {
			return true
		}
		if protocol == protokit.OpenAIResponses && (eventType == "response.completed" || eventType == "response.done") {
			return true
		}
	}
	switch protocol {
	case protokit.AnthropicMessages:
		return envelope.Type == "message_stop"
	case protokit.OpenAIResponses:
		return envelope.Type == "response.completed" || envelope.Type == "response.done"
	case protokit.GeminiGenerate:
		return len(envelope.Candidates) > 0 && envelope.Candidates[0].FinishReason != ""
	default:
		return false
	}
}

func (s *Server) finishStreamingRelease(ctx context.Context, tenantID, requestID, attemptID string, startedAt time.Time, lease contracts.Lease, request ChatCompletionRequest, status int, parsedUsage *usage, partial, networkErr bool, callErr error) (int, error) {
	class := contracts.ErrorOK
	if networkErr {
		class = contracts.ErrorNetwork
	}
	if err := s.writeStreamingRelease(ctx, tenantID, requestID, attemptID, startedAt, lease, request, status, parsedUsage, partial, class, callErr); err != nil {
		return http.StatusServiceUnavailable, err
	}
	if networkErr {
		return http.StatusBadGateway, ErrNetwork
	}
	return status, callErr
}

func (s *Server) writeStreamingRelease(ctx context.Context, tenantID, requestID, attemptID string, startedAt time.Time, lease contracts.Lease, request ChatCompletionRequest, status int, parsedUsage *usage, partial bool, class contracts.ErrorClass, callErr error) error {
	release := contracts.Release{SchemaVersion: contracts.SchemaVersion, EventID: contracts.TerminalEventID(attemptID), RequestID: requestID, AttemptID: attemptID, AttemptNo: 1, ProducerID: s.Producer.ProducerID, OccurredAt: time.Now().UTC(), AccountID: lease.AccountID, Provider: s.Provider, StatusCode: status, LatencyMS: int(time.Since(startedAt).Milliseconds()), Model: request.Model, TenantID: tenantID, ErrorClass: class, UsageSource: contracts.UsageSourceMissing, Partial: partial}
	if parsedUsage != nil {
		release.TokensIn, release.TokensOut, release.CacheReadTokens, release.CacheWriteTokens = parsedUsage.Input, parsedUsage.Output, parsedUsage.CacheRead, parsedUsage.CacheWrite
		release.UsageSource = contracts.UsageSourceUpstream
	}
	releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), releaseWriteTimeout)
	defer releaseCancel()
	_, err := s.Producer.Add(releaseCtx, mustStreamEvent(contracts.EventTypeRelease, release))
	if err != nil {
		return ErrReleaseFailed
	}
	return nil
}

func (s *Server) handleNonStreaming(ctx context.Context, tenantID, group string, request ChatCompletionRequest, inboundProtocol protokit.Protocol) ([]byte, int, error) {
	if tenantID == "" {
		return nil, http.StatusUnauthorized, errors.New("tenant authentication required")
	}
	if request.Model == "" {
		return nil, http.StatusBadRequest, errors.New("model is required")
	}
	if request.Stream || containsUnsupportedMessageFields(request.Messages) {
		return nil, http.StatusBadRequest, contracts.ErrUnsupportedCapability
	}
	if group == "" {
		group = "default"
	}
	if s.snapshotFresh != nil && !s.snapshotFresh() {
		return nil, http.StatusServiceUnavailable, ErrSnapshotStale
	}
	deadline := s.RequestTimeout
	if deadline <= 0 {
		deadline = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	requestID := newID("req")
	var lastStatus int
	var lastErr error
	attemptedAccounts := make(map[string]struct{})
	for attemptNo := 1; attemptNo <= 2; attemptNo++ {
		lease, err := s.Chooser.AcquireExcluding(contracts.Criteria{Platform: s.Provider, Model: request.Model, Group: group}, attemptedAccounts)
		if err != nil {
			if lastErr != nil {
				if lastStatus == 0 {
					lastStatus = http.StatusBadGateway
				}
				return nil, lastStatus, lastErr
			}
			if errors.Is(err, ErrQuotaExhausted) {
				return nil, http.StatusTooManyRequests, err
			}
			return nil, http.StatusServiceUnavailable, err
		}
		attemptedAccounts[lease.AccountID] = struct{}{}
		releaseLimiter, err := s.Limiter.Acquire(ctx, lease)
		if err != nil {
			return nil, http.StatusTooManyRequests, err
		}
		attemptID := newID("attempt")
		startedAt := time.Now().UTC()
		started := contracts.AttemptStarted{SchemaVersion: contracts.SchemaVersion, EventID: newID("start"), RequestID: requestID, AttemptID: attemptID, AttemptNo: attemptNo, ProducerID: s.Producer.ProducerID, OccurredAt: startedAt, AccountID: lease.AccountID, Provider: s.Provider, Model: request.Model, TenantID: tenantID, DeadlineAt: startedAt.Add(deadline)}
		startedEvent, err := contracts.NewStreamEvent(contracts.EventTypeAttemptStarted, started.EventID, requestID, attemptID, s.Producer.ProducerID, startedAt, started)
		if err != nil {
			releaseLimiter()
			return nil, http.StatusInternalServerError, err
		}
		if _, err := s.Producer.Add(ctx, startedEvent); err != nil {
			releaseLimiter()
			return nil, http.StatusServiceUnavailable, ErrAttemptFailed
		}
		body, status, usage, networkErr, reset, callErr := s.callUpstream(ctx, lease, request, inboundProtocol)
		release := contracts.Release{SchemaVersion: contracts.SchemaVersion, EventID: contracts.TerminalEventID(attemptID), RequestID: requestID, AttemptID: attemptID, AttemptNo: attemptNo, ProducerID: s.Producer.ProducerID, OccurredAt: time.Now().UTC(), AccountID: lease.AccountID, Provider: s.Provider, StatusCode: status, LatencyMS: int(time.Since(startedAt).Milliseconds()), Model: request.Model, TenantID: tenantID, ErrorClass: contracts.ErrorOK, UsageSource: contracts.UsageSourceMissing, Partial: false}
		if usage != nil {
			release.TokensIn, release.TokensOut, release.CacheReadTokens, release.CacheWriteTokens = usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite
			release.UsageSource = contracts.UsageSourceUpstream
		}
		if networkErr {
			release.ErrorClass = contracts.ErrorNetwork
			lastErr = ErrNetwork
		} else if status >= 400 {
			release.ErrorClass = classifyHTTPWithReset(status, body, reset)
			if status == http.StatusUnauthorized && lease.Credential.Kind == "static" {
				release.ErrorClass = contracts.ErrorAuthInvalid
			}
			lastErr = callErr
		}
		releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), releaseWriteTimeout)
		_, releaseErr := s.Producer.Add(releaseCtx, mustStreamEvent(contracts.EventTypeRelease, release))
		releaseCancel()
		if releaseErr != nil {
			releaseLimiter()
			return nil, http.StatusServiceUnavailable, ErrReleaseFailed
		}
		releaseLimiter()
		if networkErr {
			return nil, http.StatusBadGateway, ErrNetwork
		}
		if status >= 400 {
			lastStatus, lastErr = status, callErr
			if shouldFailoverStatus(status) && attemptNo < 2 {
				s.Chooser.MarkFailure(lease.AccountID, request.Model, release.ErrorClass, reset)
				continue
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("upstream returned HTTP %d", status)
			}
			if lastStatus == 0 {
				lastStatus = http.StatusBadGateway
			}
			return nil, lastStatus, lastErr
		}
		if usage == nil {
			lastErr = ErrUsageUnavailable
			if attemptNo < 2 {
				continue
			}
			return nil, http.StatusBadGateway, lastErr
		}
		return body, http.StatusOK, nil
	}
	return nil, http.StatusBadGateway, lastErr
}

func containsUnsupportedMessageFields(messages []map[string]any) bool {
	for _, message := range messages {
		for _, key := range []string{"thinking", "images"} {
			if _, exists := message[key]; exists {
				return true
			}
		}
		if content, exists := message["content"]; exists {
			if parts, isArray := content.([]any); isArray {
				for _, rawPart := range parts {
					part, ok := rawPart.(map[string]any)
					if !ok {
						return true
					}
					partType, _ := part["type"].(string)
					if partType != "text" && partType != "image_url" {
						return true
					}
				}
			}
		}
	}
	return false
}

func containsImageMessageFields(messages []map[string]any) bool {
	for _, message := range messages {
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if ok && part["type"] == "image_url" {
				return true
			}
		}
	}
	return false
}

type usage struct{ Input, Output, CacheRead, CacheWrite int }

func (s *Server) callUpstream(ctx context.Context, lease contracts.Lease, request ChatCompletionRequest, inboundProtocol protokit.Protocol) ([]byte, int, *usage, bool, time.Duration, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, http.StatusBadRequest, nil, false, 0, err
	}
	upstreamProtocol, err := selectedProtocol(s.Provider, lease.Profile)
	if err != nil {
		return nil, http.StatusBadGateway, nil, false, 0, err
	}
	if upstreamProtocol != protokit.OpenAIChat {
		payload, err = protokit.ConvertRequest(payload, protokit.OpenAIChat, upstreamProtocol)
		if err != nil {
			return nil, http.StatusBadRequest, nil, false, 0, err
		}
	}
	payload, err = prepareProviderPayload(s.Provider, lease.Profile, request.Model, payload)
	if err != nil {
		return nil, http.StatusBadGateway, nil, false, 0, err
	}
	path := lease.Profile.InferencePath
	if path == "" {
		path = "/v1/chat/completions"
		if upstreamProtocol == protokit.AnthropicMessages {
			path = "/v1/messages"
		} else if upstreamProtocol == protokit.OpenAIResponses {
			path = "/responses"
		} else if upstreamProtocol == protokit.GeminiGenerate {
			return nil, http.StatusBadGateway, nil, false, 0, fmt.Errorf("Gemini inference path must be configured explicitly")
		}
	}
	endpoint := strings.TrimRight(lease.Profile.BaseURL, "/") + resolveInferencePath(path, request.Model)
	upstreamRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, http.StatusBadGateway, nil, false, 0, err
	}
	upstreamRequest.Header.Set("Content-Type", "application/json")
	if lease.Credential.AccessToken != "" {
		if upstreamProtocol == protokit.AnthropicMessages && lease.Credential.Kind == "static" {
			upstreamRequest.Header.Set("x-api-key", lease.Credential.AccessToken)
		} else if upstreamProtocol == protokit.GeminiGenerate && lease.Credential.Kind == "static" {
			upstreamRequest.Header.Set("x-goog-api-key", lease.Credential.AccessToken)
		} else {
			upstreamRequest.Header.Set("Authorization", "Bearer "+lease.Credential.AccessToken)
		}
	}
	if lease.Profile.UserAgent != "" {
		upstreamRequest.Header.Set("User-Agent", lease.Profile.UserAgent)
	}
	for key, value := range lease.Profile.ExtraHeaders {
		upstreamRequest.Header.Set(key, value)
	}
	applyProviderRequestHeaders(upstreamRequest.Header, s.Provider, payload)
	if upstreamProtocol == protokit.AnthropicMessages {
		upstreamRequest.Header.Set("anthropic-version", "2023-06-01")
	}
	client := s.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	client, err = clientForProxy(client, lease.Profile.Proxy)
	if err != nil {
		return nil, 0, nil, true, 0, err
	}
	response, err := client.Do(upstreamRequest)
	if err != nil {
		return nil, 0, nil, true, 0, err
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if readErr != nil {
		return body, response.StatusCode, nil, true, 0, readErr
	}
	reset := retryAfter(response.Header, time.Now())
	if response.StatusCode >= 400 {
		return body, response.StatusCode, nil, false, reset, fmt.Errorf("upstream returned HTTP %d", response.StatusCode)
	}
	body, err = normalizeProviderResponse(s.Provider, request.Model, body)
	if err != nil {
		return nil, http.StatusBadGateway, nil, false, reset, err
	}
	parsedUsage := parseUsageForProtocol(body, upstreamProtocol)
	if upstreamProtocol != inboundProtocol {
		converted, convertErr := protokit.ConvertResponse(body, upstreamProtocol, inboundProtocol)
		if convertErr != nil {
			return nil, http.StatusBadGateway, parsedUsage, false, reset, convertErr
		}
		body = converted
	}
	return body, response.StatusCode, parsedUsage, false, reset, nil
}

func parseUsage(body []byte) *usage {
	return parseUsageForProtocol(body, protokit.OpenAIChat)
}

func clientForProxy(base *http.Client, rawProxy string) (*http.Client, error) {
	rawProxy = strings.TrimSpace(rawProxy)
	if rawProxy == "" {
		return base, nil
	}
	proxyURL, err := url.Parse(rawProxy)
	if err != nil || (proxyURL.Scheme != "http" && proxyURL.Scheme != "https") || proxyURL.Host == "" {
		return nil, fmt.Errorf("%w: invalid proxy URL", contracts.ErrInvalidContract)
	}
	client := *base
	var transport *http.Transport
	switch configured := base.Transport.(type) {
	case nil:
		transport = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		transport = configured.Clone()
	default:
		return nil, fmt.Errorf("%w: proxy requires an HTTP transport", contracts.ErrInvalidContract)
	}
	transport.Proxy = http.ProxyURL(proxyURL)
	client.Transport = transport
	return &client, nil
}

func parseUsageForProtocol(body []byte, protocol protokit.Protocol) *usage {
	parsed, present, err := protokit.ParseUsage(body, protocol)
	if err != nil || !present {
		return nil
	}
	return &usage{Input: parsed.Input, Output: parsed.Output, CacheRead: parsed.CacheRead, CacheWrite: parsed.CacheWrite}
}

func selectedProtocol(provider string, profile contracts.UpstreamProfile) (protokit.Protocol, error) {
	if profile.Protocol != "" {
		switch protokit.Protocol(profile.Protocol) {
		case protokit.OpenAIChat, protokit.OpenAIResponses, protokit.AnthropicMessages, protokit.GeminiGenerate:
			return protokit.Protocol(profile.Protocol), nil
		default:
			return "", fmt.Errorf("unsupported upstream protocol %q", profile.Protocol)
		}
	}
	if strings.EqualFold(provider, "claude") {
		return protokit.AnthropicMessages, nil
	}
	return protokit.OpenAIChat, nil
}

func applyProviderRequestHeaders(header http.Header, provider string, payload []byte) {
	if strings.EqualFold(provider, "windsurf") {
		if header.Get("User-Agent") == "" {
			header.Set("User-Agent", "Windsurf/1.20.9 Codeium/1.20.9")
		}
		header.Set("X-Codeium-Request-Id", newID("windsurf"))
		return
	}
	if strings.EqualFold(provider, "kiro") {
		header.Set("Accept", "*/*")
		header.Set("amz-sdk-invocation-id", newID("kiro"))
		header.Set("amz-sdk-request", "attempt=1; max=3")
		if header.Get("x-amzn-kiro-agent-mode") == "" {
			header.Set("x-amzn-kiro-agent-mode", "vibe")
		}
		if header.Get("x-amzn-codewhisperer-optout") == "" {
			header.Set("x-amzn-codewhisperer-optout", "true")
		}
		if header.Get("X-Amz-Target") == "" {
			header.Set("X-Amz-Target", "AmazonCodeWhispererStreamingService.GenerateAssistantResponse")
		}
		return
	}
	if strings.EqualFold(provider, "antigravity") {
		if header.Get("User-Agent") == "" {
			header.Set("User-Agent", "antigravity/1.20.5 linux/x64")
		}
		return
	}
	if !strings.EqualFold(provider, "copilot") {
		return
	}
	defaults := map[string]string{
		"User-Agent":                          "GitHubCopilotChat/0.44.0",
		"copilot-integration-id":              "vscode-chat",
		"editor-version":                      "vscode/1.109.3",
		"editor-plugin-version":               "copilot-chat/0.44.0",
		"openai-intent":                       "conversation-panel",
		"x-github-api-version":                "2025-05-01",
		"x-vscode-user-agent-library-version": "electron-fetch",
	}
	for key, value := range defaults {
		if header.Get(key) == "" {
			header.Set(key, value)
		}
	}
	requestID := newID("copilot")
	header.Set("x-request-id", requestID)
	header.Set("x-interaction-id", requestID)
	header.Set("x-agent-task-id", requestID)
	header.Set("x-interaction-type", header.Get("openai-intent"))
	header.Set("X-Initiator", copilotInitiator(payload))
}

func prepareProviderPayload(provider string, profile contracts.UpstreamProfile, model string, payload []byte) ([]byte, error) {
	if strings.EqualFold(provider, "kiro") {
		return prepareKiroPayload(profile, model, payload)
	}
	if !strings.EqualFold(provider, "antigravity") {
		return payload, nil
	}
	projectID := strings.TrimSpace(profile.ExtraHeaders["X-Goog-User-Project"])
	if projectID == "" {
		return nil, errors.New("Antigravity profile requires X-Goog-User-Project")
	}
	var request map[string]any
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, fmt.Errorf("decode Antigravity Gemini request: %w", err)
	}
	delete(request, "stream")
	userAgent := profile.UserAgent
	if userAgent == "" {
		userAgent = "antigravity/1.20.5 linux/x64"
	}
	return json.Marshal(map[string]any{
		"model":       model,
		"project":     projectID,
		"requestId":   newID("antigravity"),
		"requestType": "agent",
		"userAgent":   userAgent,
		"request":     request,
	})
}

func normalizeProviderResponse(provider, model string, body []byte) ([]byte, error) {
	if strings.EqualFold(provider, "kiro") {
		return normalizeKiroResponse(body, model)
	}
	if !strings.EqualFold(provider, "antigravity") {
		return body, nil
	}
	var envelope struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode Antigravity response envelope: %w", err)
	}
	if len(bytes.TrimSpace(envelope.Response)) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Response), []byte("null")) {
		return nil, errors.New("Antigravity response envelope has no response")
	}
	return envelope.Response, nil
}

func normalizeProviderStreamEvent(provider, data string) string {
	if !strings.EqualFold(provider, "antigravity") || data == "[DONE]" {
		return data
	}
	response, err := normalizeProviderResponse(provider, "", []byte(data))
	if err != nil {
		return data
	}
	return string(response)
}

func providerStreamingPath(provider, path string) string {
	if !strings.EqualFold(provider, "antigravity") {
		return path
	}
	path = strings.Replace(path, ":generateContent", ":streamGenerateContent", 1)
	if !strings.Contains(path, "?") {
		return path + "?alt=sse"
	}
	if !strings.Contains(path, "alt=") {
		return path + "&alt=sse"
	}
	return path
}

func copilotInitiator(payload []byte) string {
	var request struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if json.Unmarshal(payload, &request) != nil {
		return "user"
	}
	for _, message := range request.Messages {
		if message.Role == "assistant" || message.Role == "tool" {
			return "agent"
		}
	}
	return "user"
}

func resolveInferencePath(path, model string) string {
	return strings.ReplaceAll(path, "{model}", url.PathEscape(model))
}

func mustJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte("null")
	}
	return encoded
}

func classifyHTTP(status int, body []byte) contracts.ErrorClass {
	return classifyHTTPWithReset(status, body, 0)
}

func classifyHTTPWithReset(status int, body []byte, reset time.Duration) contracts.ErrorClass {
	switch status {
	case http.StatusUnauthorized:
		return contracts.ErrorAuthExpired
	case http.StatusForbidden:
		if strings.Contains(strings.ToLower(string(body)), "<html") || strings.Contains(strings.ToLower(string(body)), "<!doctype") {
			return contracts.ErrorForbiddenTransport
		}
		return contracts.ErrorForbiddenCapability
	case http.StatusTooManyRequests:
		if reset > 0 {
			return contracts.ErrorRateLimitedKnown
		}
		return contracts.ErrorRateLimitedUnknown
	default:
		if status >= 500 {
			return contracts.ErrorUpstream5xx
		}
		return contracts.ErrorBlocked
	}
}

func retryAfter(header http.Header, now time.Time) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value != "" {
		if seconds, err := time.ParseDuration(value + "s"); err == nil && seconds > 0 {
			return seconds
		}
		if at, err := http.ParseTime(value); err == nil && at.After(now) {
			return at.Sub(now)
		}
	}
	if value = strings.TrimSpace(header.Get("X-RateLimit-Reset")); value != "" {
		if epoch, err := strconv.ParseInt(value, 10, 64); err == nil {
			at := time.Unix(epoch, 0)
			if at.After(now) {
				return at.Sub(now)
			}
		}
	}
	return 0
}

func shouldFailoverStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests || status >= 500
}

func mustStreamEvent(eventType string, release contracts.Release) contracts.StreamEvent {
	event, err := contracts.NewStreamEvent(eventType, release.EventID, release.RequestID, release.AttemptID, release.ProducerID, release.OccurredAt, release)
	if err != nil {
		panic(err)
	}
	return event
}

func newID(prefix string) string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(raw[:])
}
