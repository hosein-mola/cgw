package routing

import (
	"fmt"

	"github.com/local/codex-deepseek-proxy/internal/config"
	"github.com/local/codex-deepseek-proxy/internal/providers"
)

type Candidate struct {
	Provider      providers.Provider
	UpstreamModel string
	UpstreamAPI   string
}

type Router struct {
	cfg      config.Config
	registry *providers.Registry
}

func New(cfg config.Config, registry *providers.Registry) *Router {
	return &Router{cfg: cfg, registry: registry}
}

func (r *Router) Candidates(model string) ([]Candidate, error) {
	m, ok := r.cfg.Models[model]
	if !ok {
		return nil, fmt.Errorf("unknown model %q", model)
	}
	order := m.Failover
	if m.Provider != "" {
		order = []string{m.Provider}
	}
	if m.PreferredProvider != "" {
		order = append([]string{m.PreferredProvider}, order...)
	}
	seen := make(map[string]bool)
	var out []Candidate
	for _, id := range order {
		if seen[id] {
			continue
		}
		seen[id] = true
		p, ok := r.registry.Get(id)
		if !ok {
			continue
		}
		upstream, err := p.UpstreamModel(model)
		if err != nil {
			return nil, err
		}
		out = append(out, Candidate{Provider: p, UpstreamModel: upstream, UpstreamAPI: m.UpstreamAPI})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("model %q has no available provider route", model)
	}
	return out, nil
}

func (r *Router) Models() map[string]config.ModelConfig { return r.cfg.Models }
