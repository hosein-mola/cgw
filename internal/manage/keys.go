package manage

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/local/codex-deepseek-proxy/internal/config"
)

type Secrets map[string]string

func loadSecrets(home string) (Secrets, error) {
	s := Secrets{}
	path := filepath.Join(home, "credentials.json")
	if err := noLinks(path); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, errors.New("cannot read credentials store (invalid JSON or permissions)")
	}
	b, err = unseal(b)
	if err != nil {
		return nil, errors.New("cannot decrypt credential store for this OS user")
	}
	if err = json.Unmarshal(b, &s); err != nil {
		return nil, errors.New("invalid credential store")
	}
	if s == nil {
		s = Secrets{}
	}
	return s, nil
}
func saveSecrets(home string, s Secrets) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	b, err = seal(b)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(home, "credentials.json"), b)
}
func randomSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func validKey(key string) bool {
	return key != "" && len(key) <= 16384 && !strings.ContainsAny(key, "\r\n\x00")
}

// Stored credentials take precedence so CLI updates reliably take effect after restart.
func applySecrets(home string, c config.Config) error {
	s, err := loadSecrets(home)
	if err != nil {
		return err
	}
	if v := s["proxy"]; v != "" {
		if err = os.Setenv(c.Auth.ProxyAPIKeyEnv, v); err != nil {
			return err
		}
	}
	for id, p := range c.Providers {
		if v := s[id]; v != "" {
			if err = os.Setenv(p.APIKeyEnv, v); err != nil {
				return err
			}
		}
	}
	if !validKey(os.Getenv(c.Auth.ProxyAPIKeyEnv)) {
		return fmt.Errorf("proxy key missing; run cgw init or set %s", c.Auth.ProxyAPIKeyEnv)
	}
	return nil
}

func redact(home, text string) string {
	s, _ := loadSecrets(home)
	if s == nil {
		s = Secrets{}
	}
	for _, key := range []string{"PROXY_API_KEY", "ARVAN_API_KEY", "ARVANAI_KEY"} {
		if v := os.Getenv(key); v != "" {
			s[key] = v
		}
	}
	for _, v := range s {
		if v != "" {
			text = strings.ReplaceAll(text, v, "[REDACTED]")
			encoded, _ := json.Marshal(v)
			if len(encoded) > 2 {
				text = strings.ReplaceAll(text, string(encoded[1:len(encoded)-1]), "[REDACTED]")
			}
		}
	}
	return text
}
