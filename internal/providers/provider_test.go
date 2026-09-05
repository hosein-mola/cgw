package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/local/codex-deepseek-proxy/internal/chat"
	"github.com/local/codex-deepseek-proxy/internal/config"
)

func TestCreateChatCompletionUsesProviderCredential(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", "provider-secret")
	var got chat.CompletionRequest
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gateway/models/DeepSeek-V4-Pro/test-gateway/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer provider-secret" {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()
	p := New("test", config.ProviderConfig{BaseURL: up.URL + "/gateway/models/DeepSeek-V4-Pro/test-gateway/v1/", APIKeyEnv: "TEST_PROVIDER_KEY"}, up.Client(), map[string]string{"proxy": "upstream"})
	m, err := p.UpstreamModel("proxy")
	if err != nil || m != "upstream" {
		t.Fatalf("mapping %q %v", m, err)
	}
	resp, err := p.CreateChatCompletion(context.Background(), &chat.CompletionRequest{Model: m, Messages: []chat.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Model != "upstream" {
		t.Fatalf("model %q", got.Model)
	}
}

func TestCreateResponseUsesNativeEndpoint(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", "provider-secret")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer provider-secret" {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("accept %q", r.Header.Get("Accept"))
		}
		_, _ = w.Write([]byte("event: response.completed\ndata: {}\n\n"))
	}))
	defer up.Close()
	p := New("test", config.ProviderConfig{BaseURL: up.URL + "/v1", APIKeyEnv: "TEST_PROVIDER_KEY"}, up.Client(), nil)
	resp, err := p.CreateResponse(context.Background(), []byte(`{"model":"GPT-5.2-Codex","stream":true}`), true)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
