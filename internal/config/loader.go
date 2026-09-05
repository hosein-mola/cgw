package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Load parses the deliberately small, auditable YAML subset used by config.yaml.
// It supports mappings, quoted/plain scalars, booleans, integers and indented lists.
func Load(path string) (Config, error) {
	c := Default()
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()

	section, entry, listKey := "", "", ""
	s := bufio.NewScanner(f)
	lineNo := 0
	for s.Scan() {
		lineNo++
		raw := strings.TrimRight(s.Text(), " \t\r")
		trim := strings.TrimSpace(stripComment(raw))
		if trim == "" {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		if strings.HasPrefix(trim, "- ") {
			if section != "models" || entry == "" || listKey != "failover" {
				return c, fmt.Errorf("config line %d: unexpected list item", lineNo)
			}
			m := c.Models[entry]
			m.Failover = append(m.Failover, scalar(strings.TrimSpace(strings.TrimPrefix(trim, "- "))))
			c.Models[entry] = m
			continue
		}
		key, value, ok := strings.Cut(trim, ":")
		if !ok {
			return c, fmt.Errorf("config line %d: expected key: value", lineNo)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if indent == 0 {
			if value != "" {
				return c, fmt.Errorf("config line %d: top-level key must be a mapping", lineNo)
			}
			section, entry, listKey = key, "", ""
			continue
		}
		if (section == "models" || section == "providers") && indent == 2 {
			if value != "" {
				return c, fmt.Errorf("config line %d: entry must be a mapping", lineNo)
			}
			entry, listKey = key, ""
			if section == "models" {
				if _, ok := c.Models[entry]; !ok {
					c.Models[entry] = ModelConfig{}
				}
			} else if _, ok := c.Providers[entry]; !ok {
				c.Providers[entry] = ProviderConfig{UpstreamStream: true}
			}
			continue
		}
		if err := assign(&c, section, entry, key, value, &listKey); err != nil {
			return c, fmt.Errorf("config line %d: %w", lineNo, err)
		}
	}
	if err := s.Err(); err != nil {
		return c, err
	}
	c = ArvanOnly(c)
	return c, c.Validate()
}

func assign(c *Config, section, entry, key, value string, listKey *string) error {
	val := scalar(value)
	switch section {
	case "server":
		switch key {
		case "host":
			c.Server.Host = val
		case "port":
			c.Server.Port = mustInt(val)
		default:
			return fmt.Errorf("unknown server field %q", key)
		}
	case "auth":
		if key != "proxy_api_key_env" {
			return fmt.Errorf("unknown auth field %q", key)
		}
		c.Auth.ProxyAPIKeyEnv = val
	case "timeouts":
		switch key {
		case "connect_seconds":
			c.Timeouts.ConnectSeconds = mustInt(val)
		case "upstream_seconds":
			c.Timeouts.UpstreamSeconds = mustInt(val)
		case "idle_stream_seconds":
			c.Timeouts.IdleStreamSeconds = mustInt(val)
		default:
			return fmt.Errorf("unknown timeouts field %q", key)
		}
	case "limits":
		switch key {
		case "max_request_bytes":
			c.Limits.MaxRequestBytes = int64(mustInt(val))
		case "max_header_bytes":
			c.Limits.MaxHeaderBytes = mustInt(val)
		default:
			return fmt.Errorf("unknown limits field %q", key)
		}
	case "models":
		if entry == "" {
			return fmt.Errorf("model entry missing")
		}
		m := c.Models[entry]
		switch key {
		case "preferred_provider":
			m.PreferredProvider = val
		case "provider":
			m.Provider = val
		case "upstream_model":
			m.UpstreamModel = val
		case "upstream_api":
			m.UpstreamAPI = val
		case "failover":
			m.Failover = nil
			*listKey = "failover"
		default:
			return fmt.Errorf("unknown model field %q", key)
		}
		c.Models[entry] = m
	case "providers":
		if entry == "" {
			return fmt.Errorf("provider entry missing")
		}
		p := c.Providers[entry]
		switch key {
		case "base_url":
			p.BaseURL = val
		case "api_key_env":
			p.APIKeyEnv = val
		case "upstream_stream":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return fmt.Errorf("invalid boolean %q", val)
			}
			p.UpstreamStream = b
		default:
			return fmt.Errorf("unknown provider field %q", key)
		}
		c.Providers[entry] = p
	default:
		return fmt.Errorf("unknown section %q", section)
	}
	return nil
}

func scalar(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		if s[0] == '"' {
			if v, err := strconv.Unquote(s); err == nil {
				return v
			}
		}
		return s[1 : len(s)-1]
	}
	return s
}

func mustInt(s string) int { v, _ := strconv.Atoi(s); return v }

func stripComment(s string) string {
	quoted := byte(0)
	for i := 0; i < len(s); i++ {
		if (s[i] == '\'' || s[i] == '"') && (i == 0 || s[i-1] != '\\') {
			if quoted == 0 {
				quoted = s[i]
			} else if quoted == s[i] {
				quoted = 0
			}
		}
		if s[i] == '#' && quoted == 0 {
			return s[:i]
		}
	}
	return s
}
