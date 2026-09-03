package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elucid/gateway-platform/internal/events"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

type disconnectWriter struct {
	header http.Header
	status int
	buf    bytes.Buffer
	cancel context.CancelFunc
	once   sync.Once
}

func (w *disconnectWriter) Header() http.Header    { return w.header }
func (w *disconnectWriter) WriteHeader(status int) { w.status = status }
func (w *disconnectWriter) Write(p []byte) (int, error) {
	w.once.Do(func() {
		if w.cancel != nil {
			w.cancel()
		}
	})
	if w.buf.Len() > 0 {
		return 0, errors.New("client disconnected")
	}
	return w.buf.Write(p)
}
func (w *disconnectWriter) Flush() {}

func latestRelease(t *testing.T, rdb *redis.Client) contracts.Release {
	t.Helper()
	messages, err := rdb.XRange(context.Background(), events.StreamKey, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) == 0 {
		t.Fatal("expected stream events")
	}
	payload, ok := messages[len(messages)-1].Values["payload"].(string)
	if !ok {
		t.Fatalf("invalid event payload: %#v", messages[len(messages)-1].Values["payload"])
	}
	var release contracts.Release
	if err := json.Unmarshal([]byte(payload), &release); err != nil {
		t.Fatal(err)
	}
	return release
}

func TestOpenAIChatStreamingIncludesUsageAndWritesRelease(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		options, ok := request["stream_options"].(map[string]any)
		if !ok || options["include_usage"] != true || request["stream"] != true {
			t.Fatalf("stream usage was not forced: %#v", request)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"id\":\"s1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: {\"id\":\"s1\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "apikey")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL}}})
	recorder := httptest.NewRecorder()
	status, err := server.HandleStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "m", Stream: true, Messages: []map[string]any{{"role": "user", "content": "hello"}}}, recorder)
	if err != nil || status != http.StatusOK || recorder.Code != http.StatusOK {
		t.Fatalf("stream failed: status=%d recorder=%d err=%v body=%s", status, recorder.Code, err, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "data: [DONE]") || recorder.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("unexpected SSE response: headers=%v body=%s", recorder.Header(), recorder.Body.String())
	}
	release := latestRelease(t, rdb)
	if release.TenantID != "tenant" || release.UsageSource != contracts.UsageSourceUpstream || release.TokensIn != 2 || release.TokensOut != 3 || release.Partial {
		t.Fatalf("unexpected streaming release: %+v", release)
	}
}

func TestStreamingDrainsAfterClientDisconnect(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstreamDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(upstreamDone)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "apikey")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dst := &disconnectWriter{header: make(http.Header), cancel: cancel}
	status, err := server.HandleStreaming(ctx, "tenant", "default", ChatCompletionRequest{Model: "m", Stream: true, Messages: []map[string]any{{"role": "user", "content": "hello"}}}, dst)
	if err != nil || status != http.StatusOK {
		t.Fatalf("disconnect stream failed: status=%d err=%v", status, err)
	}
	select {
	case <-upstreamDone:
	case <-time.After(time.Second):
		t.Fatal("upstream was not drained after client disconnect")
	}
	release := latestRelease(t, rdb)
	if release.UsageSource != contracts.UsageSourceUpstream || release.TokensOut != 1 || !release.Partial {
		t.Fatalf("disconnect release was not terminal and accounted: %+v", release)
	}
}

func TestStreamingMissingUsageIsPartialRelease(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]\n\n")
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "apikey")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL}}})
	recorder := httptest.NewRecorder()
	status, err := server.HandleStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "m", Stream: true, Messages: []map[string]any{{"role": "user", "content": "hello"}}}, recorder)
	if status != http.StatusBadGateway || !errors.Is(err, ErrUsageUnavailable) {
		t.Fatalf("missing usage result: status=%d err=%v", status, err)
	}
	release := latestRelease(t, rdb)
	if release.UsageSource != contracts.UsageSourceMissing || !release.Partial {
		t.Fatalf("missing usage release classification: %+v", release)
	}
}

func TestAnthropicStreamingAccumulatesNativeUsage(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "claude-key" || r.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected Anthropic stream request: path=%s x-api-key=%q authorization=%q", r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"usage\":{\"input_tokens\":4,\"cache_read_input_tokens\":1}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":6}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "claude")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "claude", Platform: "claude", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "claude-key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "anthropic_messages", InferencePath: "/v1/messages"}}})
	recorder := httptest.NewRecorder()
	status, err := server.HandleMessagesStreaming(context.Background(), "tenant", "default", []byte(`{"model":"claude","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hello"}]}`), recorder)
	if err != nil || status != http.StatusOK || recorder.Code != http.StatusOK {
		t.Fatalf("Anthropic stream failed: status=%d recorder=%d err=%v body=%s", status, recorder.Code, err, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "event: message_stop") {
		t.Fatalf("Anthropic SSE response was not forwarded: %s", recorder.Body.String())
	}
	release := latestRelease(t, rdb)
	if release.UsageSource != contracts.UsageSourceUpstream || release.TokensIn != 4 || release.TokensOut != 6 || release.CacheReadTokens != 1 || release.Partial {
		t.Fatalf("unexpected Anthropic streaming release: %+v", release)
	}
}

func TestOpenAIStreamingConvertsAnthropicUpstreamEvents(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "claude-key" || r.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected Anthropic cross-protocol request: path=%s x-api-key=%q authorization=%q", r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"))
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["stream"] != true || request["max_tokens"] != float64(32) {
			t.Fatalf("request was not converted for streaming: %#v", request)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"converted\"}}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":6}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "claude")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "claude", Platform: "claude", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "claude-key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "anthropic_messages"}}})
	recorder := httptest.NewRecorder()
	maxTokens := 32
	status, err := server.HandleStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "m", Stream: true, MaxTokens: &maxTokens, Messages: []map[string]any{{"role": "user", "content": "hello"}}}, recorder)
	if err != nil || status != http.StatusOK || recorder.Code != http.StatusOK {
		t.Fatalf("cross-protocol stream failed: status=%d recorder=%d err=%v body=%s", status, recorder.Code, err, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"content":"converted"`) || !strings.Contains(recorder.Body.String(), "data: [DONE]") {
		t.Fatalf("Anthropic SSE was not converted to Chat SSE: %s", recorder.Body.String())
	}
	release := latestRelease(t, rdb)
	if release.UsageSource != contracts.UsageSourceUpstream || release.TokensIn != 4 || release.TokensOut != 6 || release.Partial {
		t.Fatalf("unexpected cross-protocol streaming release: %+v", release)
	}
}

func TestResponsesStreamingReadsCompletedUsage(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer codex-key" {
			t.Fatalf("unexpected Responses stream request: path=%s authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":7,\"total_tokens\":12}}}\n\n")
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "codex")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "codex", Platform: "codex", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "codex-key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "openai_responses", InferencePath: "/responses"}}})
	recorder := httptest.NewRecorder()
	status, err := server.HandleResponsesStreaming(context.Background(), "tenant", "default", []byte(`{"model":"gpt-5","stream":true,"input":"hello"}`), recorder)
	if err != nil || status != http.StatusOK || recorder.Code != http.StatusOK {
		t.Fatalf("Responses stream failed: status=%d recorder=%d err=%v body=%s", status, recorder.Code, err, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatalf("Responses SSE response was not forwarded: %s", recorder.Body.String())
	}
	release := latestRelease(t, rdb)
	if release.UsageSource != contracts.UsageSourceUpstream || release.TokensIn != 5 || release.TokensOut != 7 || release.Partial {
		t.Fatalf("unexpected Responses streaming release: %+v", release)
	}
}

func TestOpenAIStreamingConvertsResponsesUpstreamEvents(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer codex-key" {
			t.Fatalf("unexpected Responses cross-protocol request: path=%s authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["stream"] != true || request["model"] != "m" {
			t.Fatalf("request was not converted for Responses streaming: %#v", request)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"converted\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":7}}}\n\n")
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "codex")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "codex", Platform: "codex", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "codex-key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "openai_responses"}}})
	recorder := httptest.NewRecorder()
	maxTokens := 32
	status, err := server.HandleStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "m", Stream: true, MaxTokens: &maxTokens, Messages: []map[string]any{{"role": "user", "content": "hello"}}}, recorder)
	if err != nil || status != http.StatusOK || recorder.Code != http.StatusOK {
		t.Fatalf("cross-protocol Responses stream failed: status=%d recorder=%d err=%v body=%s", status, recorder.Code, err, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"content":"converted"`) || !strings.Contains(recorder.Body.String(), "data: [DONE]") {
		t.Fatalf("Responses SSE was not converted to Chat SSE: %s", recorder.Body.String())
	}
	release := latestRelease(t, rdb)
	if release.UsageSource != contracts.UsageSourceUpstream || release.TokensIn != 5 || release.TokensOut != 7 || release.Partial {
		t.Fatalf("unexpected Responses cross-protocol release: %+v", release)
	}
}

func TestOpenAIStreamingConvertsGeminiUpstreamEvents(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gemini-2.5-pro:streamGenerateContent" || r.URL.RawQuery != "alt=sse" || r.Header.Get("x-goog-api-key") != "gemini-key" {
			t.Fatalf("unexpected Gemini streaming request: path=%s query=%s key=%q", r.URL.Path, r.URL.RawQuery, r.Header.Get("x-goog-api-key"))
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if _, present := request["stream"]; present {
			t.Fatalf("Gemini request must not use OpenAI stream field: %#v", request)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"converted\"}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":7}}\n\n")
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "gemini")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "gemini", Platform: "gemini", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "gemini-key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "gemini_generate"}}})
	recorder := httptest.NewRecorder()
	status, err := server.HandleStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "gemini-2.5-pro", Stream: true, Messages: []map[string]any{{"role": "user", "content": "hello"}}}, recorder)
	if err != nil || status != http.StatusOK || recorder.Code != http.StatusOK {
		t.Fatalf("cross-protocol Gemini stream failed: status=%d recorder=%d err=%v body=%s", status, recorder.Code, err, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"content":"converted"`) || !strings.Contains(recorder.Body.String(), "data: [DONE]") {
		t.Fatalf("Gemini SSE was not converted to Chat SSE: %s", recorder.Body.String())
	}
	release := latestRelease(t, rdb)
	if release.UsageSource != contracts.UsageSourceUpstream || release.TokensIn != 5 || release.TokensOut != 7 || release.Partial {
		t.Fatalf("unexpected Gemini cross-protocol release: %+v", release)
	}
}

func TestStreamingRejectsToolCallsBeforeUpstream(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "apikey")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL}}})
	recorder := httptest.NewRecorder()
	status, err := server.HandleResponsesStreaming(context.Background(), "tenant", "default", []byte(`{"model":"m","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"input":"hello"}`), recorder)
	if status != http.StatusBadRequest || !errors.Is(err, contracts.ErrUnsupportedCapability) || called {
		t.Fatalf("streaming tools result: status=%d err=%v called=%v", status, err, called)
	}
}

func TestResolveInferencePathEscapesModelTemplate(t *testing.T) {
	got := resolveInferencePath("/v1beta/models/{model}:generateContent", "gemini/2.5 pro")
	if got != "/v1beta/models/gemini%2F2.5%20pro:generateContent" {
		t.Fatalf("escaped inference path=%q", got)
	}
}

func TestClaudeUpstreamConvertsOpenAIRequestAndResponse(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "claude-key" || r.Header.Get("Authorization") != "" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Fatalf("unexpected Claude request: path=%s x-api-key=%q authorization=%q anthropic-version=%q", r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), r.Header.Get("anthropic-version"))
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["system"] != "be brief" || request["max_tokens"] != float64(32) {
			t.Fatalf("request was not converted: %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg-1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":6}}`))
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "claude")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "claude", Platform: "claude", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "claude-key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "anthropic_messages", InferencePath: "/v1/messages"}}})
	maxTokens := 32
	body, status, err := server.HandleNonStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "m", MaxTokens: &maxTokens, Messages: []map[string]any{{"role": "system", "content": "be brief"}, {"role": "user", "content": "hello"}}})
	if err != nil || status != http.StatusOK {
		t.Fatalf("request failed: status=%d err=%v body=%s", status, err, body)
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response["object"] != "chat.completion" || response["id"] != "msg-1" {
		t.Fatalf("response was not converted to OpenAI: %s", body)
	}
	usage := response["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(4) || usage["completion_tokens"] != float64(6) {
		t.Fatalf("usage was not preserved: %s", body)
	}
}

func TestMessagesInboundConvertsOpenAIResponse(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected OpenAI path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat-1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`))
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "apikey")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL}}})
	body, status, err := server.HandleMessages(context.Background(), "tenant", "default", []byte(`{"model":"m","max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`))
	if err != nil || status != http.StatusOK {
		t.Fatalf("request failed: status=%d err=%v body=%s", status, err, body)
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response["type"] != "message" || response["role"] != "assistant" || response["stop_reason"] != "stop_sequence" {
		t.Fatalf("response was not converted to Anthropic: %s", body)
	}
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != float64(2) || usage["output_tokens"] != float64(3) {
		t.Fatalf("usage was not preserved: %s", body)
	}
}

func TestMessagesInboundConvertsResponsesFixture(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer codex-key" {
			t.Fatalf("unexpected Responses request: path=%s authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["model"] != "gpt-5" || request["max_output_tokens"] != float64(32) || request["instructions"] != "be brief" {
			t.Fatalf("request was not converted to Responses: %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp-1","object":"response","model":"gpt-5","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10,"input_tokens_details":{"cached_tokens":2}}}`)
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "codex")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "codex", Platform: "codex", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "codex-key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "openai_responses", InferencePath: "/responses"}}})
	body, status, err := server.HandleMessages(context.Background(), "tenant", "default", []byte(`{"model":"gpt-5","system":"be brief","max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`))
	if err != nil || status != http.StatusOK {
		t.Fatalf("Messages to Responses fixture failed: status=%d err=%v body=%s", status, err, body)
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	content := response["content"].([]any)
	if response["type"] != "message" || content[0].(map[string]any)["text"] != "done" {
		t.Fatalf("Responses response was not converted to Messages: %s", body)
	}
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(6) || usage["cache_read_input_tokens"] != float64(2) {
		t.Fatalf("Responses usage was not converted: %s", body)
	}
	release := latestRelease(t, rdb)
	if release.UsageSource != contracts.UsageSourceUpstream || release.TokensIn != 4 || release.TokensOut != 6 || release.CacheReadTokens != 2 {
		t.Fatalf("unexpected Responses Messages release: %+v", release)
	}
}

func TestMessagesInboundConvertsGeminiFixture(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generate" || r.Header.Get("x-goog-api-key") != "gemini-key" || r.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected Gemini request: path=%s key=%q authorization=%q", r.URL.Path, r.Header.Get("x-goog-api-key"), r.Header.Get("Authorization"))
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["contents"].([]any)[0].(map[string]any)["role"] != "user" || request["generationConfig"].(map[string]any)["maxOutputTokens"] != float64(32) {
			t.Fatalf("request was not converted to Gemini: %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":6,"totalTokenCount":10,"cachedContentTokenCount":2}}`)
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "gemini")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "gemini", Platform: "gemini", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "gemini-key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "gemini_generate", InferencePath: "/generate"}}})
	body, status, err := server.HandleMessages(context.Background(), "tenant", "default", []byte(`{"model":"gemini-2.5-pro","max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`))
	if err != nil || status != http.StatusOK {
		t.Fatalf("Messages to Gemini fixture failed: status=%d err=%v body=%s", status, err, body)
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	content := response["content"].([]any)
	if response["type"] != "message" || content[0].(map[string]any)["text"] != "done" {
		t.Fatalf("Gemini response was not converted to Messages: %s", body)
	}
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(6) || usage["cache_read_input_tokens"] != float64(2) {
		t.Fatalf("Gemini usage was not converted: %s", body)
	}
	release := latestRelease(t, rdb)
	if release.UsageSource != contracts.UsageSourceUpstream || release.TokensIn != 4 || release.TokensOut != 6 || release.CacheReadTokens != 2 {
		t.Fatalf("unexpected Gemini Messages release: %+v", release)
	}
}

func TestResponsesInboundAndUpstreamRoundTrip(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer key" {
			t.Fatalf("unexpected Responses request: path=%s authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["model"] != "m" || request["max_output_tokens"] != float64(32) || request["instructions"] != "be brief" {
			t.Fatalf("request was not converted to Responses: %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","model":"m","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10}}`))
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "codex")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "codex", Platform: "codex", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "openai_responses", InferencePath: "/responses"}}})
	body, status, err := server.HandleResponses(context.Background(), "tenant", "default", []byte(`{"model":"m","instructions":"be brief","input":"hello","max_output_tokens":32}`))
	if err != nil || status != http.StatusOK {
		t.Fatalf("Responses request failed: status=%d err=%v body=%s", status, err, body)
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response["object"] != "response" || response["id"] != "resp-1" {
		t.Fatalf("response was not preserved as Responses: %s", body)
	}
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(6) {
		t.Fatalf("usage was not preserved: %s", body)
	}
}

func TestResponsesInboundConvertsAnthropicFixture(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "claude-key" || r.Header.Get("Authorization") != "" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Fatalf("unexpected Anthropic request: path=%s x-api-key=%q authorization=%q anthropic-version=%q", r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), r.Header.Get("anthropic-version"))
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["model"] != "claude" || request["max_tokens"] != float64(32) || request["system"] != "be brief" {
			t.Fatalf("request was not converted to Anthropic: %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg-1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":6,"cache_read_input_tokens":2}}`)
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "claude")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "claude", Platform: "claude", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "claude-key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "anthropic_messages", InferencePath: "/v1/messages"}}})
	body, status, err := server.HandleResponses(context.Background(), "tenant", "default", []byte(`{"model":"claude","instructions":"be brief","input":"hello","max_output_tokens":32}`))
	if err != nil || status != http.StatusOK {
		t.Fatalf("Responses to Anthropic fixture failed: status=%d err=%v body=%s", status, err, body)
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response["object"] != "response" || response["status"] != "completed" {
		t.Fatalf("Anthropic response was not converted to Responses: %s", body)
	}
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(6) || usage["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(2) {
		t.Fatalf("Anthropic usage was not converted: %s", body)
	}
	release := latestRelease(t, rdb)
	if release.UsageSource != contracts.UsageSourceUpstream || release.TokensIn != 4 || release.TokensOut != 6 || release.CacheReadTokens != 2 {
		t.Fatalf("unexpected Anthropic Responses release: %+v", release)
	}
}

func TestGeminiFixtureUpstreamConvertsTextAndUsage(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generate" || r.Header.Get("x-goog-api-key") != "gemini-key" {
			t.Fatalf("unexpected Gemini request: path=%s key=%q", r.URL.Path, r.Header.Get("x-goog-api-key"))
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["contents"].([]any)[0].(map[string]any)["role"] != "user" {
			t.Fatalf("Gemini contents missing: %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":6,"totalTokenCount":10}}`)
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "gemini")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "gemini", Platform: "gemini", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "gemini-key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "gemini_generate", InferencePath: "/generate", ExtraHeaders: map[string]string{"x-goog-api-key": "gemini-key"}}}})
	body, status, err := server.HandleNonStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "gemini-2.5-pro", Messages: []map[string]any{{"role": "user", "content": "hello"}}})
	if err != nil || status != http.StatusOK {
		t.Fatalf("Gemini fixture request failed: status=%d err=%v body=%s", status, err, body)
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != "done" {
		t.Fatalf("Gemini response was not converted: %s", body)
	}
	usage := response["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(4) || usage["completion_tokens"] != float64(6) {
		t.Fatalf("Gemini usage was not converted: %s", body)
	}
}

func TestGrokFixtureUsesOpenAICompatibleBearerAndUsage(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer grok-key" || r.Header.Get("x-api-key") != "" {
			t.Fatalf("unexpected Grok request: path=%s authorization=%q x-api-key=%q", r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"grok-1","object":"chat.completion","model":"grok","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`)
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "grok")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "grok", Platform: "grok", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "grok-key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "openai_chat", InferencePath: "/v1/chat/completions"}}})
	body, status, err := server.HandleNonStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "grok", Messages: []map[string]any{{"role": "user", "content": "hello"}}})
	if err != nil || status != http.StatusOK {
		t.Fatalf("Grok fixture request failed: status=%d err=%v body=%s", status, err, body)
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response["id"] != "grok-1" || response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != "done" {
		t.Fatalf("unexpected Grok response: %s", body)
	}
	release := latestRelease(t, rdb)
	if release.UsageSource != contracts.UsageSourceUpstream || release.TokensIn != 4 || release.TokensOut != 6 {
		t.Fatalf("unexpected Grok release: %+v", release)
	}
}

func TestCopilotFixtureUsesOfficialHeadersAndUsage(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer copilot-token" {
			t.Fatalf("unexpected Copilot request: path=%s authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		for key, want := range map[string]string{
			"User-Agent":                          "GitHubCopilotChat/0.44.0",
			"copilot-integration-id":              "vscode-chat",
			"editor-version":                      "vscode/1.109.3",
			"editor-plugin-version":               "copilot-chat/0.44.0",
			"openai-intent":                       "conversation-panel",
			"x-github-api-version":                "2025-05-01",
			"x-vscode-user-agent-library-version": "electron-fetch",
			"X-Initiator":                         "agent",
		} {
			if got := r.Header.Get(key); got != want {
				t.Fatalf("header %s=%q want=%q", key, got, want)
			}
		}
		requestID := r.Header.Get("x-request-id")
		if requestID == "" || r.Header.Get("x-interaction-id") != requestID || r.Header.Get("x-agent-task-id") != requestID {
			t.Fatalf("Copilot request identifiers are inconsistent: %#v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"copilot-1","object":"chat.completion","model":"gpt-5.1","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "copilot")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "copilot", Platform: "copilot", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "oauth", AccessToken: "copilot-token", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "openai_chat", InferencePath: "/chat/completions"}}})
	body, status, err := server.HandleNonStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "gpt-5.1", Messages: []map[string]any{{"role": "user", "content": "hello"}, {"role": "assistant", "content": "hi"}, {"role": "user", "content": "continue"}}})
	if err != nil || status != http.StatusOK {
		t.Fatalf("Copilot fixture request failed: status=%d err=%v body=%s", status, err, body)
	}
	release := latestRelease(t, rdb)
	if release.UsageSource != contracts.UsageSourceUpstream || release.TokensIn != 7 || release.TokensOut != 3 {
		t.Fatalf("unexpected Copilot release: %+v", release)
	}
}

func TestAntigravityFixtureWrapsGeminiRequestAndResponse(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:generateContent" || r.Header.Get("Authorization") != "Bearer antigravity-token" || r.Header.Get("X-Goog-User-Project") != "project-1" {
			t.Fatalf("unexpected Antigravity request: path=%s headers=%#v", r.URL.Path, r.Header)
		}
		var envelope map[string]any
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		if envelope["model"] != "gemini-2.5-pro" || envelope["project"] != "project-1" || envelope["requestType"] != "agent" || envelope["requestId"] == "" {
			t.Fatalf("invalid Antigravity envelope: %#v", envelope)
		}
		request := envelope["request"].(map[string]any)
		if request["contents"].([]any)[0].(map[string]any)["role"] != "user" {
			t.Fatalf("invalid nested Gemini request: %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":4,"totalTokenCount":9}},"responseId":"ag-1"}`)
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "antigravity")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "antigravity", Platform: "antigravity", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "oauth", AccessToken: "antigravity-token", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "gemini_generate", InferencePath: "/v1internal:generateContent", UserAgent: "antigravity/1.20.5 linux/x64", ExtraHeaders: map[string]string{"X-Goog-User-Project": "project-1"}}}})
	body, status, err := server.HandleNonStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "gemini-2.5-pro", Messages: []map[string]any{{"role": "user", "content": "hello"}}})
	if err != nil || status != http.StatusOK {
		t.Fatalf("Antigravity fixture request failed: status=%d err=%v body=%s", status, err, body)
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != "done" {
		t.Fatalf("unexpected Antigravity response: %s", body)
	}
	release := latestRelease(t, rdb)
	if release.UsageSource != contracts.UsageSourceUpstream || release.TokensIn != 5 || release.TokensOut != 4 {
		t.Fatalf("unexpected Antigravity release: %+v", release)
	}
}

func TestAntigravityStreamingFixtureUnwrapsGeminiSSE(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" {
			t.Fatalf("path=%s query=%s", r.URL.Path, r.URL.RawQuery)
		}
		var envelope map[string]any
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		if envelope["project"] != "project-1" || envelope["request"].(map[string]any)["stream"] != nil {
			t.Fatalf("invalid stream envelope: %#v", envelope)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hello\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":6,\"candidatesTokenCount\":2,\"totalTokenCount\":8}}}\n\n")
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "antigravity")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "antigravity", Platform: "antigravity", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "oauth", AccessToken: "antigravity-token", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "gemini_generate", InferencePath: "/v1internal:generateContent", ExtraHeaders: map[string]string{"X-Goog-User-Project": "project-1"}}}})
	recorder := httptest.NewRecorder()
	status, err := server.HandleStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "gemini-2.5-pro", Stream: true, Messages: []map[string]any{{"role": "user", "content": "hello"}}}, recorder)
	if err != nil || status != http.StatusOK || !strings.Contains(recorder.Body.String(), `"content":"hello"`) || !strings.Contains(recorder.Body.String(), "data: [DONE]") {
		t.Fatalf("status=%d err=%v body=%s", status, err, recorder.Body.String())
	}
	release := latestRelease(t, rdb)
	if release.TokensIn != 6 || release.TokensOut != 2 || release.UsageSource != contracts.UsageSourceUpstream || release.Partial {
		t.Fatalf("unexpected Antigravity stream release: %+v", release)
	}
}

func TestKiroFixtureTransformsTextEventStreamWithExactUsage(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generateAssistantResponse" || r.Header.Get("Authorization") != "Bearer kiro-token" || r.Header.Get("X-Amz-Target") != "AmazonCodeWhispererStreamingService.GenerateAssistantResponse" {
			t.Fatalf("unexpected Kiro request: path=%s headers=%#v", r.URL.Path, r.Header)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["profileArn"] != "arn:aws:codewhisperer:us-east-1:123456789012:profile/test" {
			t.Fatalf("profileArn missing: %#v", request)
		}
		state := request["conversationState"].(map[string]any)
		current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
		if current["content"] != "rules\n\nhello" || current["modelId"] != "claude-sonnet" {
			t.Fatalf("current message=%#v", current)
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(append([]byte{0, 0, 0, 1}, []byte(`{"content":"done"}`)...))
		_, _ = io.WriteString(w, "\x00{\"messageMetadataEvent\":{\"tokenUsage\":{\"uncachedInputTokens\":3,\"cacheReadInputTokens\":2,\"cacheWriteInputTokens\":1,\"outputTokens\":4,\"totalTokens\":10}}}")
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "kiro")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "kiro", Platform: "kiro", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "oauth", AccessToken: "kiro-token", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "openai_chat", InferencePath: "/generateAssistantResponse", Metadata: map[string]string{"profile_arn": "arn:aws:codewhisperer:us-east-1:123456789012:profile/test"}, ExtraHeaders: map[string]string{"X-Amz-Target": "AmazonCodeWhispererStreamingService.GenerateAssistantResponse"}}}})
	body, status, err := server.HandleNonStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "claude-sonnet", Messages: []map[string]any{{"role": "system", "content": "rules"}, {"role": "user", "content": "hello"}}})
	if err != nil || status != http.StatusOK {
		t.Fatalf("Kiro fixture request failed: status=%d err=%v body=%s", status, err, body)
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != "done" {
		t.Fatalf("unexpected Kiro response: %s", body)
	}
	release := latestRelease(t, rdb)
	if release.TokensIn != 6 || release.TokensOut != 4 || release.CacheReadTokens != 2 || release.CacheWriteTokens != 1 || release.UsageSource != contracts.UsageSourceUpstream {
		t.Fatalf("unexpected Kiro release: %+v", release)
	}
}

func TestKiroStreamingFixtureTransformsRawEventsAndUsage(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = io.WriteString(w, "\x00{\"content\":\"hel\"}\x00{\"content\":\"lo\"}\x00{\"messageMetadataEvent\":{\"tokenUsage\":{\"uncachedInputTokens\":5,\"cacheReadInputTokens\":1,\"cacheWriteInputTokens\":0,\"outputTokens\":2}}}")
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "kiro")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "kiro", Platform: "kiro", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "oauth", AccessToken: "kiro-token", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "openai_chat", InferencePath: "/generateAssistantResponse", Metadata: map[string]string{"profile_arn": "arn:aws:codewhisperer:us-east-1:123456789012:profile/test"}}}})
	recorder := httptest.NewRecorder()
	status, err := server.HandleStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "claude-sonnet", Stream: true, Messages: []map[string]any{{"role": "user", "content": "hello"}}}, recorder)
	if err != nil || status != http.StatusOK || !strings.Contains(recorder.Body.String(), `"content":"hel"`) || !strings.Contains(recorder.Body.String(), `"content":"lo"`) || !strings.Contains(recorder.Body.String(), "data: [DONE]") {
		t.Fatalf("status=%d err=%v body=%s", status, err, recorder.Body.String())
	}
	release := latestRelease(t, rdb)
	if release.TokensIn != 6 || release.TokensOut != 2 || release.CacheReadTokens != 1 || release.UsageSource != contracts.UsageSourceUpstream || release.Partial {
		t.Fatalf("unexpected Kiro stream release: %+v", release)
	}
}

func TestKiroPartialTokenUsageIsNotAdmitted(t *testing.T) {
	if usage, ok := kiroEventTokenUsage(map[string]any{
		"tokenUsage": map[string]any{
			"outputTokens": float64(4),
		},
	}); ok || usage.Present {
		t.Fatalf("partial token usage was admitted: usage=%+v ok=%v", usage, ok)
	}
}

func TestKiroJSONParserHandlesFragmentedEscapedContent(t *testing.T) {
	parser := &kiroJSONStreamParser{}
	if events := parser.Feed([]byte{0, '{', '"', 'c', 'o', 'n', 't', 'e', 'n', 't', '"', ':', '"', 'a'}); len(events) != 0 {
		t.Fatalf("incomplete event was emitted: %#v", events)
	}
	events := parser.Feed([]byte(`b\\\"c"}`))
	if len(events) != 1 || events[0]["content"] != `ab\"c` {
		t.Fatalf("fragmented event=%#v", events)
	}
}

func TestWindsurfConfiguredEndpointFixtureAddsClientHeaders(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/configured/chat" || r.Header.Get("Authorization") != "Bearer codeium-key" {
			t.Fatalf("unexpected Windsurf request: path=%s authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		for key, want := range map[string]string{
			"User-Agent":                  "Windsurf/1.2.3 Codeium/1.2.4",
			"X-Codeium-IDE-Name":          "windsurf",
			"X-Codeium-IDE-Version":       "1.2.3",
			"X-Codeium-Extension-Name":    "windsurf",
			"X-Codeium-Extension-Version": "1.2.4",
		} {
			if got := r.Header.Get(key); got != want {
				t.Fatalf("header %s=%q want=%q", key, got, want)
			}
		}
		if r.Header.Get("X-Codeium-Request-Id") == "" {
			t.Fatal("X-Codeium-Request-Id is missing")
		}
		_, _ = io.WriteString(w, `{"id":"windsurf-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "windsurf")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "windsurf", Platform: "windsurf", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "codeium-key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "openai_chat", InferencePath: "/configured/chat", UserAgent: "Windsurf/1.2.3 Codeium/1.2.4", ExtraHeaders: map[string]string{"X-Codeium-IDE-Name": "windsurf", "X-Codeium-IDE-Version": "1.2.3", "X-Codeium-Extension-Name": "windsurf", "X-Codeium-Extension-Version": "1.2.4"}}}})
	body, status, err := server.HandleNonStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "codeium-chat", Messages: []map[string]any{{"role": "user", "content": "hello"}}})
	if err != nil || status != http.StatusOK {
		t.Fatalf("Windsurf fixture request failed: status=%d err=%v body=%s", status, err, body)
	}
	release := latestRelease(t, rdb)
	if release.TokensIn != 3 || release.TokensOut != 2 || release.UsageSource != contracts.UsageSourceUpstream {
		t.Fatalf("unexpected Windsurf release: %+v", release)
	}
}

func TestResponsesInboundConvertsGeminiFixture(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generate" || r.Header.Get("x-goog-api-key") != "gemini-key" {
			t.Fatalf("unexpected Gemini request: path=%s key=%q", r.URL.Path, r.Header.Get("x-goog-api-key"))
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["contents"].([]any)[0].(map[string]any)["role"] != "user" {
			t.Fatalf("Gemini contents missing: %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":6,"totalTokenCount":10}}`)
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "gemini")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "gemini", Platform: "gemini", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "gemini-key", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL, Protocol: "gemini_generate", InferencePath: "/generate", ExtraHeaders: map[string]string{"x-goog-api-key": "gemini-key"}}}})
	body, status, err := server.HandleResponses(context.Background(), "tenant", "default", []byte(`{"model":"gemini-2.5-pro","input":"hello","max_output_tokens":32}`))
	if err != nil || status != http.StatusOK {
		t.Fatalf("Responses to Gemini fixture failed: status=%d err=%v body=%s", status, err, body)
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response["object"] != "response" || response["status"] != "completed" {
		t.Fatalf("Gemini response was not converted to Responses: %s", body)
	}
	output := response["output"].([]any)[0].(map[string]any)
	content := output["content"].([]any)[0].(map[string]any)
	if content["text"] != "done" {
		t.Fatalf("Gemini text was not converted: %s", body)
	}
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(6) {
		t.Fatalf("Gemini usage was not converted: %s", body)
	}
}

func TestStaticAPIKeyReturnsOnlyAfterRelease(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3}}`))
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "apikey")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "k", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL}}})
	body, status, err := server.HandleNonStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "m", Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if err != nil || status != http.StatusOK || len(body) == 0 {
		t.Fatalf("request failed: status=%d err=%v body=%s", status, err, body)
	}
	if got := rdb.XLen(context.Background(), events.StreamKey).Val(); got != 2 {
		t.Fatalf("expected attempt_started and release, got %d events", got)
	}
}

func TestReleaseUsesDetachedContextAfterUpstreamTimeout(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "apikey")
	server.RequestTimeout = 20 * time.Millisecond
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "k", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL}}})
	_, status, err := server.HandleNonStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "m", Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if status != http.StatusBadGateway || !errors.Is(err, ErrNetwork) {
		t.Fatalf("timeout result: status=%d err=%v", status, err)
	}
	if got := rdb.XLen(context.Background(), events.StreamKey).Val(); got != 2 {
		t.Fatalf("terminal release was not written after timeout: %d events", got)
	}
}

func TestHTTP400DoesNotFailOver(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "apikey")
	server.Chooser.Replace([]contracts.Account{
		{ID: "a", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "a", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL}},
		{ID: "b", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "b", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL}},
	})
	_, status, err := server.HandleNonStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "m", Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if status != http.StatusBadRequest || err == nil || calls != 1 {
		t.Fatalf("400 must not fail over: calls=%d status=%d err=%v", calls, status, err)
	}
}

func TestReleaseFailureWithholdsSuccessfulResponse(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mini.Close()
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "apikey")
	server.Chooser.Replace([]contracts.Account{{ID: "a", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "k", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL}}})
	_, status, err := server.HandleNonStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "m", Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if status != http.StatusServiceUnavailable || !errors.Is(err, ErrReleaseFailed) {
		t.Fatalf("release failure result: status=%d err=%v", status, err)
	}
}

func TestHTTPFailoverStopsAtTwoAttemptsBeforeResponse(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"expired"}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "apikey")
	server.Chooser.Replace([]contracts.Account{
		{ID: "a", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "a", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL}},
		{ID: "b", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "b", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL}},
	})
	body, status, err := server.HandleNonStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "m", Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if err != nil || status != http.StatusOK || len(body) == 0 || calls != 2 {
		t.Fatalf("expected one failover then success: calls=%d status=%d err=%v body=%s", calls, status, err, body)
	}
	if got := rdb.XLen(context.Background(), events.StreamKey).Val(); got != 4 {
		t.Fatalf("expected attempt_started and release per attempt, got %d events", got)
	}
}

func TestForbiddenTransportFailoverUsesDifferentAccount(t *testing.T) {
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	var tokens []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Authorization"))
		if len(tokens) == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<!doctype html><title>blocked</title>"))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()
	server := NewServer(events.Producer{Redis: rdb, ProducerID: "gateway-test"}, "apikey")
	server.Chooser.Replace([]contracts.Account{
		{ID: "a", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "a", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL}},
		{ID: "b", Provider: "apikey", Platform: "apikey", Group: "default", Status: "active", Credential: contracts.Credential{Kind: "static", AccessToken: "b", Version: 1}, Profile: contracts.UpstreamProfile{BaseURL: upstream.URL}},
	})
	_, status, err := server.HandleNonStreaming(context.Background(), "tenant", "default", ChatCompletionRequest{Model: "m", Messages: []map[string]any{{"role": "user", "content": "hi"}}})
	if err != nil || status != http.StatusOK {
		t.Fatalf("failover result: status=%d err=%v", status, err)
	}
	if len(tokens) != 2 || tokens[0] != "Bearer a" || tokens[1] != "Bearer b" {
		t.Fatalf("failover reused an account: %v", tokens)
	}
}
