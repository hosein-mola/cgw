package manage

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/local/codex-deepseek-proxy/internal/config"
)

// macOS commonly exposes its temporary directory through /var -> /private/var.
func testDir(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if runtime.GOOS != "darwin" {
		return path
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestCodexInstallSwitchRestore(t *testing.T) {
	dir := testDir(t)
	path := filepath.Join(dir, "config.toml")
	original := []byte("# Keep this comment byte-for-byte on rollback\r\nmodel = \"original-model\"\r\nmodel_provider = \"openai\"\r\n[projects.\"C:/work/project\"]\r\ntrust_level = \"trusted\"\r\n[mcp_servers.demo]\r\ncommand = \"demo\"\r\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(auth, []byte("do-not-touch"), 0600); err != nil {
		t.Fatal(err)
	}
	c := config.Default()
	c.Models["deepseek-v4-flash"] = config.ModelConfig{Provider: "arvan", UpstreamModel: "DeepSeek-V4-Flash"}
	if err := installProfiles(path, c, ""); err != nil {
		t.Fatal(err)
	}
	_, m, _, err := loadTOML(path)
	if err != nil {
		t.Fatal(err)
	}
	if m["model"] != "original-model" || m["projects"] == nil || m["mcp_servers"] == nil {
		t.Fatalf("unrelated settings lost: %#v", m)
	}
	providers, ok := m["model_providers"].(map[string]any)
	if !ok {
		t.Fatalf("missing Codex provider table: %#v", m)
	}
	proxy, ok := providers[providerName].(map[string]any)
	if !ok || proxy["base_url"] != "http://127.0.0.1:3002/v1" || proxy["env_key"] != "PROXY_API_KEY" || proxy["wire_api"] != "responses" || proxy["requires_openai_auth"] != false || proxy["supports_websockets"] != false {
		t.Fatalf("bad Codex provider configuration: %#v", proxy)
	}
	for name := range c.Models {
		profile, e := codexProfileName(name)
		if e != nil {
			t.Fatal(e)
		}
		_, p, _, e := loadTOML(filepath.Join(dir, profile+".config.toml"))
		if e != nil {
			t.Fatal(e)
		}
		if p["model"] != name || p["model_provider"] != providerName || p["model_catalog_json"] != modelCatalogPath(path) {
			t.Fatalf("bad profile: %#v", p)
		}
	}
	if err = installProfiles(path, c, "deepseek-v4-flash"); err != nil {
		t.Fatal(err)
	}
	_, m, _, _ = loadTOML(path)
	if m["model"] != "deepseek-v4-flash" || m["model_provider"] != providerName || m["model_catalog_json"] != modelCatalogPath(path) {
		t.Fatal("model not selected")
	}
	if err = chatGPT(path); err != nil {
		t.Fatal(err)
	}
	_, m, _, _ = loadTOML(path)
	if m["model_provider"] != "openai" || m["model"] != nil || m["profile"] != nil || m["model_catalog_json"] != nil {
		t.Fatal("default ChatGPT config not restored")
	}
	if err = restoreCodex(path, "original", false); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(path)
	if !bytes.Equal(original, restored) {
		t.Fatalf("restore was not exact: %s", restored)
	}
	b, _ := os.ReadFile(auth)
	if string(b) != "do-not-touch" {
		t.Fatal("authentication changed")
	}
	for name := range c.Models {
		profile, _ := codexProfileName(name)
		if _, err = os.Stat(filepath.Join(dir, profile+".config.toml")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("created profile not removed by original restore")
		}
	}
}

func TestChatGPTSwitchesCatalogOnFirstCall(t *testing.T) {
	for _, localCatalog := range []bool{false, true} {
		t.Run(fmt.Sprintf("codex_json_%t", localCatalog), func(t *testing.T) {
			dir := testDir(t)
			path := filepath.Join(dir, "config.toml")
			if err := installProfiles(path, config.Default(), "deepseek-v4-pro"); err != nil {
				t.Fatal(err)
			}
			catalogPath := filepath.Join(dir, "codex.json")
			if localCatalog {
				if err := os.WriteFile(catalogPath, []byte(`{"models":[]}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for attempt := 0; attempt < 2; attempt++ {
				if err := chatGPT(path); err != nil {
					t.Fatal(err)
				}
				_, m, _, err := loadTOML(path)
				if err != nil {
					t.Fatal(err)
				}
				if m["model_provider"] != "openai" || m["model"] != nil || m["profile"] != nil {
					t.Fatalf("attempt %d did not switch to OpenAI: %#v", attempt+1, m)
				}
				if localCatalog && m["model_catalog_json"] != catalogPath || !localCatalog && m["model_catalog_json"] != nil {
					t.Fatalf("attempt %d retained the wrong catalog: %v", attempt+1, m["model_catalog_json"])
				}
			}
			// Switching back to Arvan must restore its catalog immediately too.
			if err := installProfiles(path, config.Default(), "deepseek-v4-pro"); err != nil {
				t.Fatal(err)
			}
			_, m, _, err := loadTOML(path)
			if err != nil || m["model_catalog_json"] != modelCatalogPath(path) {
				t.Fatalf("Arvan catalog not restored: %v, %v", m, err)
			}
		})
	}
}

func TestCodexProfileNameSupportsDottedModelIDs(t *testing.T) {
	name, err := codexProfileName("gemini-2.5-pro")
	if err != nil || name != "gemini-2-5-pro" {
		t.Fatalf("unexpected profile mapping: name=%q err=%v", name, err)
	}
	if _, err = codexProfileName("../escape"); err == nil {
		t.Fatal("unsafe model ID accepted as a profile")
	}
}

func TestNumberedModelSelectionAndSavedModel(t *testing.T) {
	c := config.Default()
	c.Models["deepseek-v4-flash"] = config.ModelConfig{Provider: "arvan", UpstreamModel: "DeepSeek-V4-Flash"}
	choices := []modelChoice{{ID: "GPT-5.6-Luna", Tier: "Cheap"}, {ID: "GPT-5.6-Terra", Tier: "Medium"}}
	var output bytes.Buffer
	model, err := chooseModel(strings.NewReader("2\n"), &output, choices)
	if err != nil || model != "GPT-5.6-Terra" {
		t.Fatalf("numbered selection failed: model=%q err=%v output=%q", model, err, output.String())
	}
	model, _, err = addModelRoute(&c, model)
	if err != nil {
		t.Fatal(err)
	}
	home := testDir(t)
	if err = atomicWrite(filepath.Join(home, selectedModelFile), []byte(model+"\n")); err != nil {
		t.Fatal(err)
	}
	selected, err := savedModel(home, c)
	if err != nil || selected != model {
		t.Fatalf("saved selection failed: model=%q err=%v", selected, err)
	}
}

func TestCuratedCodingModelsFiltersAndCategorizes(t *testing.T) {
	choices := curatedCodingModels([]string{
		"Whisper-1", "Gemini-3-Pro-Image-Preview", "GLM-5-Code",
		"DeepSeek-V4-Pro", "gpt-5.6-luna", "Claude-Sonnet-4.6", "Claude-Fable-5",
	})
	want := []modelChoice{
		{ID: "gpt-5.6-luna", Tier: "Cheap"},
		{ID: "Claude-Sonnet-4.6", Tier: "Medium"},
		{ID: "Claude-Fable-5", Tier: "Frontier"},
		{ID: "DeepSeek-V4-Pro", Tier: "Frontier"},
	}
	if fmt.Sprint(choices) != fmt.Sprint(want) {
		t.Fatalf("unexpected curated choices: %#v", choices)
	}
}

func TestConfigureCuratedRoutesRemovesOtherModels(t *testing.T) {
	c := config.Default()
	c.Models["obsolete"] = config.ModelConfig{Provider: "arvan", UpstreamModel: "Obsolete"}
	choices := []modelChoice{{ID: "GPT-5.6-Luna", Tier: "Cheap"}, {ID: "DeepSeek-V4-Pro", Tier: "Frontier"}}
	name, changed, err := configureCuratedRoutes(&c, choices, "GPT-5.6-Luna")
	if err != nil || !changed || name != "gpt-5-6-luna" {
		t.Fatalf("route configuration failed: name=%q changed=%t err=%v", name, changed, err)
	}
	path := filepath.Join(testDir(t), "config.yaml")
	if err = writeConfig(path, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Models) != 2 {
		t.Fatalf("expected only curated routes: %#v", loaded.Models)
	}
	if route := loaded.Models[name]; route.Provider != "arvan" || route.UpstreamModel != "GPT-5.6-Luna" {
		t.Fatalf("curated route was not persisted: %#v", route)
	}
	if _, ok := loaded.Models["obsolete"]; ok {
		t.Fatal("obsolete route remains")
	}
}

func TestHistoryListsIDsWithoutPrompts(t *testing.T) {
	dir := testDir(t)
	sessions := filepath.Join(dir, "sessions", "2026", "09", "05")
	if err := os.MkdirAll(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	id := "01a070f9-d0cd-7bd1-930c-f65967f7598e"
	rollout := `{"type":"session_meta","payload":{"id":"` + id + `","timestamp":"2026-09-05T09:50:15Z","cwd":"C:\\work","model_provider":"arvan_proxy"}}` + "\n" + `{"type":"event_msg","payload":{"message":"private prompt"}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessions, "rollout.jsonl"), []byte(rollout), 0600); err != nil {
		t.Fatal(err)
	}
	olderID := "01a0707d-6f2f-7040-8a2d-b853f3661061"
	older := `{"type":"session_meta","payload":{"session_id":"` + olderID + `","timestamp":"2026-09-05T07:34:23Z","cwd":"C:\\older","model_provider":"openai"}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessions, "older.jsonl"), []byte(older), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := historyCommand(dir, &output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, id) || !strings.Contains(text, "proxy resume "+id) {
		t.Fatalf("proxy session ID or resume command missing: %s", text)
	}
	if !strings.Contains(text, "codex resume "+olderID) {
		t.Fatalf("OpenAI resume command missing: %s", text)
	}
	if strings.Index(text, olderID) > strings.Index(text, id) {
		t.Fatalf("history does not place the newest session last: %s", text)
	}
	if strings.Contains(text, "private prompt") {
		t.Fatal("history exposed prompt contents")
	}
}

func TestToolCallCheckUsesCustomTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"max_output_tokens"`) || !strings.Contains(string(body), `"tool_choice":"required"`) || !strings.Contains(string(body), `"type":"custom"`) {
			t.Fatalf("unexpected check request: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"output":[{"type":"custom_tool_call","name":"ping","input":"ok"}]}`)
	}))
	defer server.Close()
	client := server.Client()
	if err := checkCustomToolCall(client, server.URL, "proxy-key", "test-model"); err != nil {
		t.Fatal(err)
	}
}

func TestCodexCatalogIncludesRemoteModels(t *testing.T) {
	dir := testDir(t)
	configPath := filepath.Join(dir, "config.toml")
	if err := writeCodexModelCatalog(configPath, config.Default(), []string{"Claude-Sonnet-4.6"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(modelCatalogPath(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"slug": "claude-sonnet-4-6"`)) {
		t.Fatalf("remote model metadata missing: %s", b)
	}
}

func TestFetchArvanModelsUsesStoredKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer arvan-test-key" {
			t.Fatalf("unexpected request: path=%q authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"z-model"},{"id":"a-model"},{"id":"a-model"}]}`)
	}))
	defer server.Close()
	home := testDir(t)
	if err := saveSecrets(home, Secrets{"arvan": "arvan-test-key", "proxy": "proxy-test-key"}); err != nil {
		t.Fatal(err)
	}
	c := config.Default()
	provider := c.Providers["arvan"]
	provider.BaseURL = server.URL
	c.Providers["arvan"] = provider
	models, err := fetchArvanModels(home, c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(models, ",") != "a-model,z-model" {
		t.Fatalf("unexpected remote catalog: %#v", models)
	}
}

func TestSavedModelDefaultsToDeepSeek(t *testing.T) {
	model, err := savedModel(testDir(t), config.Default())
	if err != nil || model != "deepseek-v4-pro" {
		t.Fatalf("unexpected default model: model=%q err=%v", model, err)
	}
}

func TestCodexReinstallPreservesProfileSettingsAndRemovesObsoleteProvider(t *testing.T) {
	dir := testDir(t)
	path := filepath.Join(dir, "config.toml")
	legacy := config.Default()
	legacy.Providers["parspack"] = config.ProviderConfig{BaseURL: "https://ai.parspack.com/v1", APIKeyEnv: "PARSPACK_API_KEY"}
	legacy.Models["deepseek-v4-pro-parspack"] = config.ModelConfig{Provider: "parspack", UpstreamModel: "deepseek/deepseek-v4-pro"}
	if err := installProfiles(path, legacy, ""); err != nil {
		t.Fatal(err)
	}
	if err := editCodex(path, false, func(m map[string]any, _ bool) error {
		providers, err := table(m, "model_providers")
		if err != nil {
			return err
		}
		providers["parspack"] = map[string]any{"name": "ParsPack", "base_url": "https://ai.parspack.com/v1"}
		providers["deepseek_proxy"] = map[string]any{"name": "DeepSeek Proxy", "base_url": "http://127.0.0.1:3002/v1"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	profile := filepath.Join(dir, "deepseek-v4-pro.config.toml")
	b, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, []byte("\n[tui.model_availability_nux]\ngpt = 1\n")...)
	if err = os.WriteFile(profile, b, 0600); err != nil {
		t.Fatal(err)
	}

	if err = installProfiles(path, config.Default(), ""); err != nil {
		t.Fatal(err)
	}
	_, base, _, err := loadTOML(path)
	if err != nil {
		t.Fatal(err)
	}
	providers := base["model_providers"].(map[string]any)
	if _, ok := providers["parspack"]; ok {
		t.Fatal("obsolete provider remains in Codex config")
	}
	if _, ok := providers["deepseek_proxy"]; ok {
		t.Fatal("obsolete DeepSeek provider alias remains in Codex config")
	}
	_, refreshed, _, err := loadTOML(profile)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed["tui"] == nil || refreshed["model"] != "deepseek-v4-pro" || refreshed["model_provider"] != providerName {
		t.Fatalf("profile settings were not preserved: %#v", refreshed)
	}
	if _, err = os.Stat(filepath.Join(dir, "deepseek-v4-pro-parspack.config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("obsolete generated profile was not removed")
	}
}

func TestRestoreDetectsOutsideChanges(t *testing.T) {
	path := filepath.Join(testDir(t), "config.toml")
	c := config.Default()
	if err := installProfiles(path, c, ""); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	b = append(b, []byte("\n# user edit\n")...)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := restoreCodex(path, "original", false); err == nil {
		t.Fatal("restore should reject drift")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(b, after) {
		t.Fatal("failed restore changed file")
	}
	if err := restoreCodex(path, "original", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("originally absent config must be removed")
	}
	entries, _ := os.ReadDir(backupDir(path))
	found := false
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".toml") {
			saved, _ := os.ReadFile(filepath.Join(backupDir(path), e.Name()))
			if bytes.Equal(saved, b) {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("forced restore did not back up user changes")
	}
}

func TestInvalidConfigAndProfileCollisionAreNonMutating(t *testing.T) {
	path := filepath.Join(testDir(t), "config.toml")
	bad := []byte("model = [ this is invalid")
	_ = os.WriteFile(path, bad, 0600)
	if err := installProfiles(path, config.Default(), ""); err == nil {
		t.Fatal("invalid TOML accepted")
	}
	b, _ := os.ReadFile(path)
	if !bytes.Equal(b, bad) {
		t.Fatal("invalid TOML overwritten")
	}
	_ = os.WriteFile(path, []byte("# untouched\n"), 0600)
	_ = os.WriteFile(filepath.Join(filepath.Dir(path), "deepseek-v4-pro.config.toml"), []byte("model=\"user-model\""), 0600)
	if err := installProfiles(path, config.Default(), ""); err == nil {
		t.Fatal("profile collision overwritten")
	}
	b, _ = os.ReadFile(path)
	if string(b) != "# untouched\n" {
		t.Fatal("base changed before conflict detected")
	}
}

func TestCredentialRoundTripUpdateDelete(t *testing.T) {
	dir := testDir(t)
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	s := Secrets{"arvan": "test-secret-one", "proxy": "local-secret"}
	if err := saveSecrets(dir, s); err != nil {
		t.Fatal(err)
	}
	got, err := loadSecrets(dir)
	if err != nil || got["arvan"] != s["arvan"] {
		t.Fatalf("roundtrip: %v", err)
	}
	s["arvan"] = "test-secret-two"
	if err = saveSecrets(dir, s); err != nil {
		t.Fatal(err)
	}
	got, err = loadSecrets(dir)
	if err != nil || got["arvan"] != "test-secret-two" {
		t.Fatal("update failed")
	}
	if redact(dir, "error contains test-secret-two") != "error contains [REDACTED]" {
		t.Fatal("redaction failed")
	}
	if runtime.GOOS == "windows" {
		b, _ := os.ReadFile(filepath.Join(dir, "credentials.json"))
		if bytes.Contains(b, []byte("test-secret-two")) {
			t.Fatal("Windows key is not encrypted")
		}
	}
	delete(s, "arvan")
	if err = saveSecrets(dir, s); err != nil {
		t.Fatal(err)
	}
	got, err = loadSecrets(dir)
	if err != nil || got["arvan"] != "" {
		t.Fatal("delete failed")
	}
}

func TestSimpleKeyCommands(t *testing.T) {
	home := testDir(t)
	if err := keyCommand(home, []string{"set-key", "arvan-test-key"}); err != nil {
		t.Fatal(err)
	}
	stored, err := loadSecrets(home)
	if err != nil || stored["arvan"] != "arvan-test-key" {
		t.Fatalf("set-key failed: stored=%q err=%v", stored["arvan"], err)
	}
	if err = keyCommand(home, []string{"del-key"}); err != nil {
		t.Fatal(err)
	}
	stored, err = loadSecrets(home)
	if err != nil || stored["arvan"] != "" {
		t.Fatalf("del-key failed: stored=%q err=%v", stored["arvan"], err)
	}
}

func TestCredentialPrecedenceAndEnvironmentFiltering(t *testing.T) {
	dir := testDir(t)
	t.Setenv("ARVAN_API_KEY", "old")
	t.Setenv("PROXY_API_KEY", "old-proxy")
	if err := saveSecrets(dir, Secrets{"arvan": "new", "proxy": "new-proxy"}); err != nil {
		t.Fatal(err)
	}
	if err := applySecrets(dir, config.Default()); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ARVAN_API_KEY") != "new" || os.Getenv("PROXY_API_KEY") != "new-proxy" {
		t.Fatal("stored keys did not override environment")
	}
	env := setEnv([]string{"CODEX_HOME=old", "PATH=path"}, "CODEX_HOME", "new")
	if strings.Join(env, "|") != "PATH=path|CODEX_HOME=new" {
		t.Fatal("environment was not replaced")
	}
}

func TestControlDoesNotTrustArbitraryAddress(t *testing.T) {
	dir := testDir(t)
	if err := writeJSON(filepath.Join(dir, "runtime.json"), runtimeState{Control: "example.com:80", Token: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := control(dir, "POST", "/shutdown"); err == nil {
		t.Fatal("non-loopback control endpoint accepted")
	}
}

func TestForcedRestoreRecoversMalformedConfig(t *testing.T) {
	path := filepath.Join(testDir(t), "config.toml")
	if err := installProfiles(path, config.Default(), ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("broken = ["), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restoreCodex(path, "original", false); err == nil {
		t.Fatal("malformed outside edit should be protected")
	}
	if err := restoreCodex(path, "original", true); err != nil {
		t.Fatal(err)
	}
}

func TestProfileDriftPreventsPartialRollback(t *testing.T) {
	path := filepath.Join(testDir(t), "config.toml")
	c := config.Default()
	c.Models["deepseek-v4-flash"] = config.ModelConfig{Provider: "arvan", UpstreamModel: "DeepSeek-V4-Flash"}
	if err := installProfiles(path, c, ""); err != nil {
		t.Fatal(err)
	}
	base, _ := os.ReadFile(path)
	p := filepath.Join(filepath.Dir(path), "deepseek-v4-flash.config.toml")
	b, _ := os.ReadFile(p)
	if err := os.WriteFile(p, append(b, []byte("\n# edited\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restoreCodex(path, "original", false); err == nil {
		t.Fatal("profile drift ignored")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(base, after) {
		t.Fatal("base was changed before profile conflict was detected")
	}
}
