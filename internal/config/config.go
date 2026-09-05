package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

type Config struct {
	Server    ServerConfig
	Auth      AuthConfig
	Models    map[string]ModelConfig
	Providers map[string]ProviderConfig
	Timeouts  TimeoutConfig
	Limits    LimitConfig
}

type ServerConfig struct {
	Host string
	Port int
}

type AuthConfig struct {
	ProxyAPIKeyEnv string
}

type ModelConfig struct {
	PreferredProvider string
	Failover          []string
	Provider          string
	UpstreamModel     string
	UpstreamAPI       string
}

type ProviderConfig struct {
	BaseURL        string
	APIKeyEnv      string
	UpstreamStream bool
}

type TimeoutConfig struct {
	ConnectSeconds    int
	UpstreamSeconds   int
	IdleStreamSeconds int
}

type LimitConfig struct {
	MaxRequestBytes int64
	MaxHeaderBytes  int
}

func Default() Config {
	return Config{
		Server: ServerConfig{Host: "127.0.0.1", Port: 3002},
		Auth:   AuthConfig{ProxyAPIKeyEnv: "PROXY_API_KEY"},
		Models: map[string]ModelConfig{
			"deepseek-v4-pro": {
				Provider: "arvan", UpstreamModel: "DeepSeek-V4-Pro", UpstreamAPI: "chat_completions",
			},
		},
		Providers: map[string]ProviderConfig{
			"arvan": {
				BaseURL: "https://api.arvancloudai.ir/v1", APIKeyEnv: "ARVAN_API_KEY", UpstreamStream: true,
			},
		},
		Timeouts: TimeoutConfig{ConnectSeconds: 15, UpstreamSeconds: 300, IdleStreamSeconds: 300},
		Limits:   LimitConfig{MaxRequestBytes: 32 << 20, MaxHeaderBytes: 1 << 20},
	}
}

// ArvanOnly also normalizes configurations saved by older multi-provider builds.
// A stale failover list must never send requests to a different provider.
func ArvanOnly(c Config) Config {
	arvan, ok := c.Providers["arvan"]
	c.Providers = make(map[string]ProviderConfig, 1)
	if ok {
		c.Providers["arvan"] = arvan
	}

	models := make(map[string]ModelConfig)
	for name, model := range c.Models {
		if model.Provider != "arvan" {
			continue
		}
		model.PreferredProvider = ""
		model.Failover = nil
		if model.UpstreamAPI == "" {
			model.UpstreamAPI = DefaultUpstreamAPI(model.UpstreamModel)
		}
		models[name] = model
	}
	deepSeek := ModelConfig{Provider: "arvan", UpstreamModel: "DeepSeek-V4-Pro", UpstreamAPI: "chat_completions"}
	models["deepseek-v4-pro"] = deepSeek
	delete(models, "deepseek-v4-pro-arvan")
	c.Models = models
	return c
}

func (c Config) Validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535")
	}
	if strings.TrimSpace(c.Auth.ProxyAPIKeyEnv) == "" {
		return errors.New("auth.proxy_api_key_env is required")
	}
	if len(c.Models) == 0 || len(c.Providers) == 0 {
		return errors.New("at least one model and provider are required")
	}
	for id, p := range c.Providers {
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("providers.%s.base_url must be an absolute HTTPS URL", id)
		}
		if p.APIKeyEnv == "" {
			return fmt.Errorf("providers.%s.api_key_env is required", id)
		}
	}
	for name, m := range c.Models {
		if m.UpstreamAPI != "" && m.UpstreamAPI != "chat_completions" && m.UpstreamAPI != "responses" {
			return fmt.Errorf("models.%s.upstream_api must be chat_completions or responses", name)
		}
		if m.PreferredProvider != "" {
			if _, ok := c.Providers[m.PreferredProvider]; !ok {
				return fmt.Errorf("models.%s references unknown preferred provider %q", name, m.PreferredProvider)
			}
		}
		if m.Provider != "" {
			if _, ok := c.Providers[m.Provider]; !ok {
				return fmt.Errorf("models.%s references unknown provider %q", name, m.Provider)
			}
			if m.UpstreamModel == "" {
				return fmt.Errorf("models.%s.upstream_model is required", name)
			}
			continue
		}
		if len(m.Failover) == 0 {
			return fmt.Errorf("models.%s needs provider or failover list", name)
		}
		for _, p := range m.Failover {
			if _, ok := c.Providers[p]; !ok {
				return fmt.Errorf("models.%s references unknown provider %q", name, p)
			}
		}
	}
	if c.Timeouts.ConnectSeconds <= 0 || c.Timeouts.UpstreamSeconds <= 0 || c.Timeouts.IdleStreamSeconds <= 0 {
		return errors.New("timeouts must be positive")
	}
	if c.Limits.MaxRequestBytes <= 0 || c.Limits.MaxHeaderBytes <= 0 {
		return errors.New("limits must be positive")
	}
	return nil
}

func DefaultUpstreamAPI(upstreamModel string) string {
	if strings.EqualFold(strings.TrimSpace(upstreamModel), "GPT-5.2-Codex") {
		return "responses"
	}
	return "chat_completions"
}
