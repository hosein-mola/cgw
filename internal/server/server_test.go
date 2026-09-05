package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/local/codex-deepseek-proxy/internal/config"
)

func TestHealthAndAuth(t *testing.T) {
	t.Setenv("PROXY_API_KEY", "secret")
	s := New(config.Default(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != 200 {
		t.Fatalf("health: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != 401 {
		t.Fatalf("unauthenticated models: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "deepseek-v4-pro") {
		t.Fatalf("models: %d %s", rec.Code, rec.Body)
	}
	var models struct {
		Models []struct {
			Slug          string `json:"slug"`
			ContextWindow int    `json:"context_window"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &models); err != nil {
		t.Fatal(err)
	}
	if len(models.Models) != 1 || models.Models[0].Slug != "deepseek-v4-pro" || models.Models[0].ContextWindow != 1048576 {
		t.Fatalf("bad Codex model catalog: %#v", models.Models)
	}
}

func TestClientCancellationReachesUpstream(t *testing.T) {
	t.Setenv("PROXY_API_KEY", "proxy-secret")
	t.Setenv("ARVAN_API_KEY", "arvan-secret")
	started, canceled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	cfg := config.Default()
	a := cfg.Providers["arvan"]
	a.BaseURL = upstream.URL + "/v1"
	cfg.Providers["arvan"] = a
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"deepseek-v4-pro","input":"hi","stream":true}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer proxy-secret")
	done := make(chan struct{})
	go func() { s.Handler().ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not start")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream context was not canceled")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy handler did not return")
	}
}

func TestMidstreamDisconnectEmitsFailed(t *testing.T) {
	t.Setenv("PROXY_API_KEY", "proxy-secret")
	t.Setenv("ARVAN_API_KEY", "arvan-secret")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()
	cfg := config.Default()
	a := cfg.Providers["arvan"]
	a.BaseURL = upstream.URL + "/v1"
	cfg.Providers["arvan"] = a
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"deepseek-v4-pro","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer proxy-secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "event: response.failed") || strings.Contains(rec.Body.String(), "event: response.completed") {
		t.Fatalf("bad failure lifecycle: %s", rec.Body)
	}
}

func TestCodexResponsesRoundTripThroughArvan(t *testing.T) {
	t.Setenv("PROXY_API_KEY", "proxy-secret")
	t.Setenv("ARVAN_API_KEY", "arvan-secret")
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer arvan-secret" {
			t.Errorf("wrong Arvan credential")
		}
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if got["model"] != "DeepSeek-V4-Pro" || got["stream"] != true {
			t.Errorf("bad Arvan request: %#v", got)
		}
		messages, _ := got["messages"].([]any)
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			if len(messages) < 2 || messages[0].(map[string]any)["role"] != "system" {
				t.Errorf("Codex instructions were not converted to Arvan messages: %#v", messages)
			}
			_, _ = io.WriteString(w, "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_123\",\"type\":\"function\",\"function\":{\"name\":\"run_command\",\"arguments\":\"{\\\"cmd\\\":\\\"go test ./...\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		last := messages[len(messages)-1].(map[string]any)
		if last["role"] != "tool" || last["tool_call_id"] != "call_123" || last["content"] != "ok" {
			t.Errorf("Codex tool output was not converted for Arvan: %#v", last)
		}
		_, _ = io.WriteString(w, "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	cfg := config.Default()
	a := cfg.Providers["arvan"]
	a.BaseURL = upstream.URL + "/v1"
	cfg.Providers["arvan"] = a
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer proxy-secret")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		return rec
	}

	first := request(`{"model":"deepseek-v4-pro","instructions":"You are a coding agent.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Run tests"}]}],"tools":[{"type":"function","name":"run_command","description":"Run a command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}],"stream":true}`)
	if body := first.Body.String(); !strings.Contains(body, "event: response.function_call_arguments.done") || !strings.Contains(body, `"call_id":"call_123"`) || !strings.Contains(body, "event: response.completed") {
		t.Fatalf("bad tool-call lifecycle: %s", body)
	}

	second := request(`{"model":"deepseek-v4-pro","input":[{"type":"function_call","call_id":"call_123","name":"run_command","arguments":"{\"cmd\":\"go test ./...\"}"},{"type":"function_call_output","call_id":"call_123","output":"ok"}],"stream":true}`)
	if body := second.Body.String(); !strings.Contains(body, "event: response.output_text.delta") || !strings.Contains(body, "done") || !strings.Contains(body, "event: response.completed") {
		t.Fatalf("bad continuation lifecycle: %s", body)
	}
	if calls.Load() != 2 {
		t.Fatalf("Arvan calls = %d, want 2", calls.Load())
	}
}

func TestNonStreamingUpstreamSynthesizesSSE(t *testing.T) {
	t.Setenv("PROXY_API_KEY", "proxy-secret")
	t.Setenv("ARVAN_API_KEY", "arvan-secret")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got["stream"] != false {
			t.Errorf("expected stream=false, got %#v", got["stream"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"synthetic"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()
	cfg := config.Default()
	a := cfg.Providers["arvan"]
	a.BaseURL = upstream.URL + "/v1"
	a.UpstreamStream = false
	cfg.Providers["arvan"] = a
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"deepseek-v4-pro","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer proxy-secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "response.output_text.delta") || !strings.Contains(rec.Body.String(), "response.completed") {
		t.Fatalf("response %d: %s", rec.Code, rec.Body)
	}
}

func TestNativeResponsesModelPassThrough(t *testing.T) {
	t.Setenv("PROXY_API_KEY", "proxy-secret")
	t.Setenv("ARVAN_API_KEY", "arvan-secret")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got["model"] != "GPT-5.2-Codex" || got["reasoning"] == nil {
			t.Errorf("native request fields were not preserved: %#v", got)
		}
		tools, _ := got["tools"].([]any)
		if len(tools) != 1 || tools[0].(map[string]any)["type"] != "custom" {
			t.Errorf("native custom tool was not preserved: %#v", got["tools"])
		}
		if stream, _ := got["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_native","object":"response","status":"completed","model":"gpt-5.2-codex","output":[{"type":"custom_tool_call","name":"apply_patch","call_id":"call_1","input":"patch"}]}`)
	}))
	defer upstream.Close()
	cfg := config.Default()
	a := cfg.Providers["arvan"]
	a.BaseURL = upstream.URL + "/v1"
	cfg.Providers["arvan"] = a
	cfg.Models["gpt-5-2-codex"] = config.ModelConfig{Provider: "arvan", UpstreamModel: "GPT-5.2-Codex", UpstreamAPI: "responses"}
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	request := func(stream bool) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"model":"gpt-5-2-codex","input":"edit","instructions":"use tools","reasoning":{"effort":"low"},"tools":[{"type":"custom","name":"apply_patch"}],"stream":%t}`, stream)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer proxy-secret")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	nonstream := request(false)
	if nonstream.Code != http.StatusOK || !strings.Contains(nonstream.Body.String(), `"model":"gpt-5-2-codex"`) || !strings.Contains(nonstream.Body.String(), `"custom_tool_call"`) {
		t.Fatalf("bad native JSON response: %d %s", nonstream.Code, nonstream.Body)
	}
	stream := request(true)
	if stream.Code != http.StatusOK || !strings.Contains(stream.Body.String(), "event: response.created") || !strings.Contains(stream.Body.String(), "event: response.completed") {
		t.Fatalf("bad native SSE response: %d %s", stream.Code, stream.Body)
	}
}
