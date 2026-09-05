package manage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/local/codex-deepseek-proxy/internal/catalog"
	"github.com/local/codex-deepseek-proxy/internal/config"
	"github.com/pelletier/go-toml/v2"
)

const providerName = "arvan_proxy"

type backupState struct {
	Original string
	Existed  bool
	LastHash string
	Owned    bool
	Profiles []string
}

func codexPath(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	dir := os.Getenv("CODEX_HOME")
	if dir == "" {
		h, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		dir = filepath.Join(h, ".codex")
	}
	return filepath.Abs(filepath.Join(dir, "config.toml"))
}
func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func backupDir(path string) string {
	return filepath.Join(filepath.Dir(path), ".deepseek-proxy-backups", filepath.Base(path))
}

func loadTOML(path string) ([]byte, map[string]any, bool, error) {
	if err := noLinks(path); err != nil {
		return nil, nil, false, err
	}
	b, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, false, err
	}
	m := map[string]any{}
	if err = toml.Unmarshal(b, &m); err != nil {
		return nil, nil, exists, fmt.Errorf("invalid Codex TOML; no changes made: %w", err)
	}
	return b, m, exists, nil
}
func table(m map[string]any, key string) (map[string]any, error) {
	if v, ok := m[key]; ok {
		t, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Codex %s is not a table", key)
		}
		return t, nil
	}
	t := map[string]any{}
	m[key] = t
	return t, nil
}
func snapshot(path string, b []byte) (string, error) {
	dir := backupDir(path)
	if err := privateDir(dir); err != nil {
		return "", err
	}
	name := time.Now().UTC().Format("20060102T150405.000000000") + "-" + randomSecret()[:8] + ".toml"
	if err := atomicWrite(filepath.Join(dir, name), b); err != nil {
		return "", err
	}
	return name, nil
}

// Every mutation gets an exact byte backup; the original baseline is never overwritten.
func editCodex(path string, own bool, edit func(map[string]any, bool) error) error {
	if err := noLinks(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	unlock, err := lock(path + ".deepseek-proxy.lock")
	if err != nil {
		return err
	}
	defer unlock()
	before, m, exists, err := loadTOML(path)
	if err != nil {
		return err
	}
	dir := backupDir(path)
	manifest := filepath.Join(dir, "state.json")
	var state backupState
	stateErr := readJSON(manifest, &state)
	managed := stateErr == nil
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return stateErr
	}
	if err = edit(m, state.Owned); err != nil {
		return err
	}
	after, err := toml.Marshal(m)
	if err != nil {
		return err
	}
	if bytes.Equal(before, after) {
		return nil
	}
	backup, err := snapshot(path, before)
	if err != nil {
		return err
	}
	if !managed {
		state = backupState{Original: backup, Existed: exists, LastHash: hash(before)}
		if err = writeJSON(manifest, state); err != nil {
			return err
		}
	}
	current, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !bytes.Equal(current, before) {
		return errors.New("Codex config changed during update; retry")
	}
	if err = atomicWrite(path, after); err != nil {
		return err
	}
	state.LastHash = hash(after)
	state.Owned = state.Owned || own
	if err = writeJSON(manifest, state); err != nil {
		return fmt.Errorf("config updated but backup index failed; backup %s: %w", backup, err)
	}
	fmt.Printf("Updated %s\nBackup: %s\n", path, filepath.Join(dir, backup))
	return nil
}

func installProfiles(path string, c config.Config, active string) error {
	if active != "" {
		if _, ok := c.Models[active]; !ok {
			return fmt.Errorf("unknown proxy model %q", active)
		}
	}
	type profile struct{ name, model string }
	var profiles []profile
	seen := make(map[string]string, len(c.Models))
	for model := range c.Models {
		name, nameErr := codexProfileName(model)
		if nameErr != nil {
			return nameErr
		}
		if previous, exists := seen[name]; exists {
			return fmt.Errorf("models %q and %q map to the same Codex profile %q", previous, model, name)
		}
		seen[name] = model
		profiles = append(profiles, profile{name: name, model: model})
		p := filepath.Join(filepath.Dir(path), name+".config.toml")
		var st backupState
		if _, err := os.Lstat(p); err == nil {
			if err = readJSON(filepath.Join(backupDir(p), "state.json"), &st); err != nil || !st.Owned {
				return fmt.Errorf("profile file already exists: %s; refusing to overwrite", p)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].name < profiles[j].name })
	if err := writeCodexModelCatalog(path, c, nil); err != nil {
		return err
	}
	err := editCodex(path, true, func(m map[string]any, managed bool) error {
		ps, err := table(m, "model_providers")
		if err != nil {
			return err
		}
		if !managed {
			if _, ok := ps[providerName]; ok {
				return fmt.Errorf("Codex already defines %s; rename that existing provider before installing", providerName)
			}
		}
		delete(ps, "parspack")
		delete(ps, "deepseek_proxy")
		host := c.Server.Host
		if host == "0.0.0.0" || host == "::" || host == "" {
			host = "127.0.0.1"
		}
		ps[providerName] = map[string]any{"name": "ArvanCloud Proxy", "base_url": "http://" + net.JoinHostPort(host, strconv.Itoa(c.Server.Port)) + "/v1", "env_key": c.Auth.ProxyAPIKeyEnv, "wire_api": "responses", "requires_openai_auth": false, "supports_websockets": false}
		if active != "" {
			delete(m, "profile")
			m["model"] = active
			m["model_provider"] = providerName
			m["model_catalog_json"] = modelCatalogPath(path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	manifest := filepath.Join(backupDir(path), "state.json")
	var st backupState
	if err = readJSON(manifest, &st); err != nil {
		return err
	}
	current := make(map[string]bool, len(profiles))
	for _, profile := range profiles {
		current[profile.name] = true
	}
	for _, old := range st.Profiles {
		if current[old] {
			continue
		}
		if !validProfile(old) {
			return errors.New("invalid profile name in backup index")
		}
		oldPath := filepath.Join(filepath.Dir(path), old+".config.toml")
		if _, statErr := os.Lstat(oldPath); errors.Is(statErr, os.ErrNotExist) {
			continue
		} else if statErr != nil {
			return statErr
		}
		var oldState backupState
		if err = readJSON(filepath.Join(backupDir(oldPath), "state.json"), &oldState); err != nil || !oldState.Owned {
			return fmt.Errorf("obsolete profile is not managed by this proxy: %s", oldPath)
		}
		if err = restoreOne(oldPath, "original", true); err != nil {
			return fmt.Errorf("cannot remove obsolete profile %s: %w", old, err)
		}
	}
	st.Profiles = st.Profiles[:0]
	for _, profile := range profiles {
		st.Profiles = append(st.Profiles, profile.name)
	}
	if err = writeJSON(manifest, st); err != nil {
		return err
	}
	for _, profile := range profiles {
		p := filepath.Join(filepath.Dir(path), profile.name+".config.toml")
		if err = editCodex(p, true, func(m map[string]any, _ bool) error {
			m["model"] = profile.model
			m["model_provider"] = providerName
			m["model_catalog_json"] = modelCatalogPath(path)
			return nil
		}); err != nil {
			return fmt.Errorf("profile installation incomplete; use codex restore to undo: %w", err)
		}
	}
	return nil
}

func modelCatalogPath(codexConfigPath string) string {
	return filepath.Join(filepath.Dir(codexConfigPath), "arvan-models.json")
}

func writeCodexModelCatalog(codexConfigPath string, c config.Config, remote []string) error {
	routes := config.ArvanOnly(c)
	for _, upstream := range remote {
		if _, _, err := addModelRoute(&routes, upstream); err != nil {
			return err
		}
	}
	models := make([]any, 0, len(routes.Models))
	for _, id := range configuredModelNames(routes) {
		models = append(models, catalog.Metadata(id, routes.Models[id].UpstreamModel))
	}
	b, err := json.MarshalIndent(map[string]any{"models": models}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(modelCatalogPath(codexConfigPath), append(b, '\n'))
}

func validProfile(s string) bool {
	return regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`).MatchString(s)
}

func codexProfileName(model string) (string, error) {
	if !validProfile(model) {
		return "", fmt.Errorf("model %q cannot be used as a Codex profile", model)
	}
	name := strings.ReplaceAll(model, ".", "-")
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(name) {
		return "", fmt.Errorf("model %q cannot be used as a Codex profile", model)
	}
	return name, nil
}

func chatGPT(path string) error {
	// Catalog selection is a top-level/profile setting, not a provider setting.
	// Resolve it before launching Codex so the first process sees the right list.
	catalogPath := filepath.Join(filepath.Dir(path), "codex.json")
	info, err := os.Stat(catalogPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("Codex model catalog is not a regular file: %s", catalogPath)
	}
	hasCatalog := err == nil
	return editCodex(path, false, func(m map[string]any, _ bool) error {
		delete(m, "profile")
		delete(m, "model")
		m["model_provider"] = "openai"
		delete(m, "openai_base_url")
		delete(m, "model_catalog_json")
		if hasCatalog {
			m["model_catalog_json"] = catalogPath
		}
		return nil
	})
}

func restoreOne(path, name string, force bool) error {
	unlock, err := lock(path + ".deepseek-proxy.lock")
	if err != nil {
		return err
	}
	defer unlock()
	var st backupState
	dir := backupDir(path)
	if err = readJSON(filepath.Join(dir, "state.json"), &st); err != nil {
		return fmt.Errorf("no readable backup index: %w", err)
	}
	if err = noLinks(path); err != nil {
		return err
	}
	current, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if hash(current) != st.LastHash && !force {
		return errors.New("Codex config changed outside this CLI; restore refused. Review it first, then use --force (current file is backed up)")
	}
	original := name == "" || name == "original"
	if original {
		name = st.Original
	}
	if filepath.Base(name) != name || filepath.Ext(name) != ".toml" {
		return errors.New("backup must be a filename from codex backups")
	}
	target := filepath.Join(dir, name)
	if err = noLinks(target); err != nil {
		return err
	}
	b, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	var check map[string]any
	if err = toml.Unmarshal(b, &check); err != nil {
		return fmt.Errorf("invalid backup TOML: %w", err)
	}
	if _, err = snapshot(path, current); err != nil {
		return err
	}
	latest, e := os.ReadFile(path)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if !bytes.Equal(latest, current) {
		return errors.New("Codex config changed during restore; retry")
	}
	if original && !st.Existed {
		if exists {
			if err = os.Remove(path); err != nil {
				return err
			}
		}
		st.LastHash = hash(nil)
	} else {
		if err = atomicWrite(path, b); err != nil {
			return err
		}
		st.LastHash = hash(b)
	}
	if err = writeJSON(filepath.Join(dir, "state.json"), st); err != nil {
		return err
	}
	fmt.Println("Restored Codex configuration; ChatGPT authentication was not changed.")
	return nil
}

func listBackups(path string) error {
	entries, err := os.ReadDir(backupDir(path))
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".toml" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Println(name)
	}
	return nil
}

func restoreCodex(path, name string, force bool) error {
	if name != "" && name != "original" {
		return restoreOne(path, name, force)
	}
	var st backupState
	if err := readJSON(filepath.Join(backupDir(path), "state.json"), &st); err != nil {
		return err
	}
	paths := []string{path}
	for _, name := range st.Profiles {
		if !validProfile(name) {
			return errors.New("invalid profile name in backup index")
		}
		p := filepath.Join(filepath.Dir(path), name+".config.toml")
		if _, err := os.Stat(filepath.Join(backupDir(p), "state.json")); err == nil {
			paths = append(paths, p)
		}
	}
	for _, p := range paths {
		var s backupState
		if err := readJSON(filepath.Join(backupDir(p), "state.json"), &s); err != nil {
			return err
		}
		if err := noLinks(p); err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if !force && hash(b) != s.LastHash {
			return fmt.Errorf("outside changes in %s; review then use --force (a new backup is created)", p)
		}
		if filepath.Base(s.Original) != s.Original || filepath.Ext(s.Original) != ".toml" {
			return errors.New("invalid original backup filename")
		}
		originalPath := filepath.Join(backupDir(p), s.Original)
		if err := noLinks(originalPath); err != nil {
			return err
		}
		original, e := os.ReadFile(originalPath)
		if e != nil {
			return e
		}
		var doc map[string]any
		if e = toml.Unmarshal(original, &doc); e != nil {
			return fmt.Errorf("invalid original backup for %s: %w", p, e)
		}
	}
	for _, p := range paths {
		if err := restoreOne(p, "original", force); err != nil {
			return err
		}
	}
	return nil
}
