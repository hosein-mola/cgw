package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/local/codex-deepseek-proxy/internal/chat"
	"github.com/local/codex-deepseek-proxy/internal/config"
)

var ErrAPIKeyMissing = errors.New("provider API key is not configured")

type Provider interface {
	ID() string
	BaseURL() string
	UpstreamModel(proxyModel string) (string, error)
	UpstreamStream() bool
	Configured() bool
	CreateChatCompletion(context.Context, *chat.CompletionRequest) (*http.Response, error)
	CreateResponse(context.Context, []byte, bool) (*http.Response, error)
}

type HTTPProvider struct {
	id        string
	baseURL   string
	apiKeyEnv string
	stream    bool
	client    *http.Client
	models    map[string]string
}

func New(id string, cfg config.ProviderConfig, client *http.Client, models map[string]string) Provider {
	return &HTTPProvider{id: id, baseURL: strings.TrimRight(cfg.BaseURL, "/"), apiKeyEnv: cfg.APIKeyEnv, stream: cfg.UpstreamStream, client: client, models: models}
}

func NewClient(connect, responseHeader time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   connect,
		ResponseHeaderTimeout: responseHeader,
		ExpectContinueTimeout: 1 * time.Second,
	}}
}

func (p *HTTPProvider) ID() string           { return p.id }
func (p *HTTPProvider) BaseURL() string      { return p.baseURL }
func (p *HTTPProvider) UpstreamStream() bool { return p.stream }
func (p *HTTPProvider) Configured() bool     { return p.apiKey() != "" }

func (p *HTTPProvider) UpstreamModel(proxyModel string) (string, error) {
	if m := p.models[proxyModel]; m != "" {
		return m, nil
	}
	for name, m := range p.models {
		if strings.HasPrefix(name, proxyModel+"-") && m != "" {
			return m, nil
		}
	}
	return "", fmt.Errorf("model %q has no mapping for provider %s", proxyModel, p.id)
}

func (p *HTTPProvider) CreateChatCompletion(ctx context.Context, req *chat.CompletionRequest) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return p.create(ctx, "/chat/completions", body, req.Stream)
}

func (p *HTTPProvider) CreateResponse(ctx context.Context, body []byte, stream bool) (*http.Response, error) {
	return p.create(ctx, "/responses", body, stream)
}

func (p *HTTPProvider) create(ctx context.Context, path string, body []byte, stream bool) (*http.Response, error) {
	key := p.apiKey()
	if key == "" {
		return nil, fmt.Errorf("%w: %s", ErrAPIKeyMissing, p.apiKeyEnv)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	httpReq.Header.Set("User-Agent", "codex-deepseek-proxy/1.0")
	return p.client.Do(httpReq)
}

func (p *HTTPProvider) apiKey() string {
	if v := strings.TrimSpace(os.Getenv(p.apiKeyEnv)); v != "" {
		return v
	}
	// Compatibility aliases for the variable names in the original deployment notes.
	if p.id == "arvan" {
		return strings.TrimSpace(os.Getenv("ARVANAI_KEY"))
	}
	return ""
}

type Registry struct{ providers map[string]Provider }

func NewRegistry(cfg config.Config) *Registry {
	client := NewClient(time.Duration(cfg.Timeouts.ConnectSeconds)*time.Second, time.Duration(cfg.Timeouts.UpstreamSeconds)*time.Second)
	mappings := make(map[string]map[string]string)
	for name, m := range cfg.Models {
		if m.Provider != "" && m.UpstreamModel != "" {
			if mappings[m.Provider] == nil {
				mappings[m.Provider] = make(map[string]string)
			}
			mappings[m.Provider][name] = m.UpstreamModel
		}
	}
	r := &Registry{providers: make(map[string]Provider)}
	for id, pc := range cfg.Providers {
		r.providers[id] = New(id, pc, client, mappings[id])
	}
	return r
}

func (r *Registry) Get(id string) (Provider, bool) { p, ok := r.providers[id]; return p, ok }
func (r *Registry) All() map[string]Provider       { return r.providers }
