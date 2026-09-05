package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("server:\n  host: \"127.0.0.1\"\n  port: 3999\nauth:\n  proxy_api_key_env: TEST_KEY\nmodels:\n  deepseek-v4-pro:\n    provider: arvan\n    upstream_model: DeepSeek-V4-Pro\nproviders:\n  arvan:\n    base_url: \"https://example.com/v1\"\n    api_key_env: ARVAN_API_KEY\n    upstream_stream: false\n")
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Port != 3999 || c.Auth.ProxyAPIKeyEnv != "TEST_KEY" || c.Providers["arvan"].UpstreamStream {
		t.Fatalf("bad config: %#v", c)
	}
	if model := c.Models["deepseek-v4-pro"]; model.Provider != "arvan" || model.UpstreamModel != "DeepSeek-V4-Pro" || len(model.Failover) != 0 {
		t.Fatalf("bad Arvan route: %#v", model)
	}
	if got := DefaultUpstreamAPI("GPT-5.2-Codex"); got != "responses" {
		t.Fatalf("GPT-5.2-Codex API = %q", got)
	}
}

func TestLoadNativeResponsesModel(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("server:\n  host: 127.0.0.1\n  port: 3002\nauth:\n  proxy_api_key_env: PROXY_API_KEY\nmodels:\n  gpt-5-2-codex:\n    provider: arvan\n    upstream_model: GPT-5.2-Codex\n    upstream_api: responses\nproviders:\n  arvan:\n    base_url: https://example.com/v1\n    api_key_env: ARVAN_API_KEY\n    upstream_stream: true\n")
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Models["gpt-5-2-codex"].UpstreamAPI; got != "responses" {
		t.Fatalf("upstream_api = %q", got)
	}
}

func TestArvanOnlyDropsOtherProvidersAndRoutes(t *testing.T) {
	c := Default()
	c.Providers["other"] = ProviderConfig{BaseURL: "https://example.com/v1", APIKeyEnv: "OTHER_KEY"}
	c.Models["other-model"] = ModelConfig{Provider: "other", UpstreamModel: "other"}
	c.Models["fallback-model"] = ModelConfig{PreferredProvider: "arvan", Failover: []string{"arvan", "other"}}

	c = ArvanOnly(c)
	if len(c.Providers) != 1 {
		t.Fatalf("unexpected providers: %#v", c.Providers)
	}
	if _, ok := c.Providers["arvan"]; !ok {
		t.Fatal("Arvan provider was removed")
	}
	if _, ok := c.Models["other-model"]; ok {
		t.Fatal("model for removed provider remains")
	}
	if _, ok := c.Models["fallback-model"]; ok {
		t.Fatal("multi-provider route remains")
	}
}
