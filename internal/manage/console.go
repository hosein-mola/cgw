package manage

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/local/codex-deepseek-proxy/internal/config"
)

const selectedModelFile = "selected-model"

type modelTier struct {
	Name   string
	Models []string
}

type modelChoice struct {
	ID   string
	Tier string
}

// Keep the selector focused on strong coding and agent models. The remote
// catalog is still authoritative: entries are shown only while Arvan offers
// them.
var codingModelTiers = []modelTier{
	{Name: "Cheap", Models: []string{
		"GPT-5.6-Luna",
		"Gemini-3-Flash-Preview",
		"Claude-Haiku-4.5",
		"DeepSeek-V4-Flash",
	}},
	{Name: "Medium", Models: []string{
		"GPT-5.6-Terra",
		"Claude-Sonnet-4.6",
		"GPT-5.2-Codex",
		"Kimi-K2.7-Code",
		"Qwen3-Coder-480b-A35B-Instruct",
	}},
	{Name: "Frontier", Models: []string{
		"GPT-5.6-Sol",
		"Claude-Fable-5",
		"Claude-Opus-4.7",
		"DeepSeek-V4-Pro",
		"Gemini-3.1-Pro-Preview",
		"Kimi-K3",
	}},
}

func interactiveConsole() error {
	fmt.Print(Help)
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\ncgw> ")
		if !scanner.Scan() {
			return scanner.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			return nil
		}
		if err := Run(strings.Fields(line)); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
		}
	}
}

func modelsCommand(home, cfgPath string) error {
	c, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	available, err := fetchArvanModels(home, c)
	if err != nil {
		return err
	}
	choices := curatedCodingModels(available)
	upstream, err := chooseModel(os.Stdin, os.Stdout, choices)
	if err != nil {
		return err
	}
	model, changed, err := configureCuratedRoutes(&c, choices, upstream)
	if err != nil {
		return err
	}
	if changed {
		if err = writeConfig(cfgPath, c); err != nil {
			return err
		}
	}
	if err = codexCommand(home, cfgPath, []string{"use", model}); err != nil {
		return err
	}
	codexConfigPath, err := codexPath("")
	if err != nil {
		return err
	}
	if err = writeCodexModelCatalog(codexConfigPath, c, modelChoiceIDs(choices)); err != nil {
		return err
	}
	if err = atomicWrite(filepath.Join(home, selectedModelFile), []byte(model+"\n")); err != nil {
		return err
	}
	if changed {
		if _, statusErr := control(home, "GET", "/status"); statusErr == nil {
			unlock, lockErr := lock(filepath.Join(home, "operation.lock"))
			if lockErr != nil {
				return lockErr
			}
			if err = stop(home); err == nil {
				err = start(home, cfgPath)
			}
			unlock()
			if err != nil {
				return err
			}
		}
	}
	fmt.Printf("Selected %s. Run it with: cgw run\n", upstream)
	return nil
}

func curatedCodingModels(available []string) []modelChoice {
	remote := make(map[string]string, len(available))
	for _, id := range available {
		remote[strings.ToLower(id)] = id
	}
	var choices []modelChoice
	for _, tier := range codingModelTiers {
		for _, id := range tier.Models {
			if actual, ok := remote[strings.ToLower(id)]; ok {
				choices = append(choices, modelChoice{ID: actual, Tier: tier.Name})
			}
		}
	}
	return choices
}

func configureCuratedRoutes(c *config.Config, choices []modelChoice, selectedUpstream string) (string, bool, error) {
	before := c.Models
	c.Models = make(map[string]config.ModelConfig, len(choices))
	selected := ""
	for _, choice := range choices {
		name, _, err := addModelRoute(c, choice.ID)
		if err != nil {
			return "", false, err
		}
		if strings.EqualFold(choice.ID, selectedUpstream) {
			selected = name
		}
	}
	if selected == "" {
		return "", false, fmt.Errorf("selected model %q is not in the curated catalog", selectedUpstream)
	}
	return selected, !sameModelRoutes(before, c.Models), nil
}

func sameModelRoutes(a, b map[string]config.ModelConfig) bool {
	if len(a) != len(b) {
		return false
	}
	for name, left := range a {
		right, ok := b[name]
		if !ok || left.Provider != right.Provider || left.UpstreamModel != right.UpstreamModel || left.UpstreamAPI != right.UpstreamAPI {
			return false
		}
	}
	return true
}

func modelChoiceIDs(choices []modelChoice) []string {
	ids := make([]string, 0, len(choices))
	for _, choice := range choices {
		ids = append(ids, choice.ID)
	}
	return ids
}

func chooseModel(r io.Reader, w io.Writer, choices []modelChoice) (string, error) {
	if len(choices) == 0 {
		return "", errors.New("ArvanCloud returned none of the curated coding models")
	}
	fmt.Fprintln(w, "Available ArvanCloud coding models:")
	lastTier := ""
	for i, choice := range choices {
		if choice.Tier != lastTier {
			if lastTier != "" {
				fmt.Fprintln(w)
			}
			fmt.Fprintf(w, "%s:\n", choice.Tier)
			lastTier = choice.Tier
		}
		fmt.Fprintf(w, "  [%d] %s\n", i+1, choice.ID)
	}
	fmt.Fprintf(w, "\nSelect a model [1-%d]: ", len(choices))
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", err
		}
		return "", errors.New("model selection canceled")
	}
	choice := strings.TrimSpace(scanner.Text())
	for i, model := range choices {
		if choice == fmt.Sprint(i+1) || strings.EqualFold(choice, model.ID) {
			return model.ID, nil
		}
	}
	return "", fmt.Errorf("invalid selection %q", choice)
}

func configuredModelNames(c config.Config) []string {
	names := make([]string, 0, len(c.Models))
	for name := range c.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func savedModel(home string, c config.Config) (string, error) {
	b, err := os.ReadFile(filepath.Join(home, selectedModelFile))
	if errors.Is(err, os.ErrNotExist) {
		if _, ok := c.Models["deepseek-v4-pro"]; ok {
			return "deepseek-v4-pro", nil
		}
		names := configuredModelNames(c)
		if len(names) == 0 {
			return "", errors.New("no ArvanCloud models are configured")
		}
		return names[0], nil
	}
	if err != nil {
		return "", err
	}
	model := strings.TrimSpace(string(b))
	if _, ok := c.Models[model]; !ok {
		return "", fmt.Errorf("saved model %q is no longer configured; run cgw models", model)
	}
	return model, nil
}

func fetchArvanModels(home string, c config.Config) ([]string, error) {
	provider, ok := c.Providers["arvan"]
	if !ok {
		return nil, errors.New("ArvanCloud provider is not configured")
	}
	if err := applySecrets(home, c); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(os.Getenv(provider.APIKeyEnv))
	if key == "" {
		key = strings.TrimSpace(os.Getenv("ARVANAI_KEY"))
	}
	if key == "" {
		return nil, errors.New("Arvan API key missing; run cgw set-key APIKEY")
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(provider.BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		detail := strings.TrimSpace(string(body))
		if detail == "" {
			return nil, fmt.Errorf("ArvanCloud models endpoint returned HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("ArvanCloud models endpoint returned HTTP %d: %s", resp.StatusCode, redact(home, detail))
	}
	var catalog struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&catalog); err != nil {
		return nil, fmt.Errorf("invalid ArvanCloud models response: %w", err)
	}
	seen := make(map[string]bool, len(catalog.Data))
	models := make([]string, 0, len(catalog.Data))
	for _, item := range catalog.Data {
		id := strings.TrimSpace(item.ID)
		if id != "" && !seen[id] {
			seen[id] = true
			models = append(models, id)
		}
	}
	sort.Strings(models)
	if len(models) == 0 {
		return nil, errors.New("ArvanCloud returned an empty model catalog")
	}
	return models, nil
}

func addModelRoute(c *config.Config, upstream string) (string, bool, error) {
	for name, route := range c.Models {
		if strings.EqualFold(route.UpstreamModel, upstream) {
			return name, false, nil
		}
	}
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(upstream) {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if valid {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		return "", false, fmt.Errorf("model ID %q cannot be converted to a local name", upstream)
	}
	name := base
	for suffix := 2; ; suffix++ {
		if existing, found := c.Models[name]; !found {
			break
		} else if strings.EqualFold(existing.UpstreamModel, upstream) {
			return name, false, nil
		}
		name = fmt.Sprintf("%s-%d", base, suffix)
	}
	c.Models[name] = config.ModelConfig{Provider: "arvan", UpstreamModel: upstream, UpstreamAPI: config.DefaultUpstreamAPI(upstream)}
	return name, true, nil
}

func writeConfig(path string, c config.Config) error {
	c = config.ArvanOnly(c)
	var b strings.Builder
	fmt.Fprintf(&b, "server:\n  host: %s\n  port: %d\n\n", strconv.Quote(c.Server.Host), c.Server.Port)
	fmt.Fprintf(&b, "auth:\n  proxy_api_key_env: %s\n\n", strconv.Quote(c.Auth.ProxyAPIKeyEnv))
	b.WriteString("models:\n")
	for _, name := range configuredModelNames(c) {
		model := c.Models[name]
		fmt.Fprintf(&b, "  %s:\n    provider: arvan\n    upstream_model: %s\n    upstream_api: %s\n\n", name, strconv.Quote(model.UpstreamModel), model.UpstreamAPI)
	}
	provider := c.Providers["arvan"]
	fmt.Fprintf(&b, "providers:\n  arvan:\n    base_url: %s\n    api_key_env: %s\n    upstream_stream: %t\n\n", strconv.Quote(provider.BaseURL), strconv.Quote(provider.APIKeyEnv), provider.UpstreamStream)
	fmt.Fprintf(&b, "timeouts:\n  connect_seconds: %d\n  upstream_seconds: %d\n  idle_stream_seconds: %d\n\n", c.Timeouts.ConnectSeconds, c.Timeouts.UpstreamSeconds, c.Timeouts.IdleStreamSeconds)
	fmt.Fprintf(&b, "limits:\n  max_request_bytes: %d\n  max_header_bytes: %d\n", c.Limits.MaxRequestBytes, c.Limits.MaxHeaderBytes)
	return atomicWrite(path, []byte(b.String()))
}

func runCommand(home, cfgPath string, args []string) error {
	if len(args) > 0 && args[0] == "codex" {
		return runChatGPTSubscription(home, cfgPath, trimDoubleDash(args[1:]))
	}
	c, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	model, err := savedModel(home, c)
	if err != nil {
		return err
	}
	codexArgs := []string{"run", "--model", model}
	if rest := trimDoubleDash(args); len(rest) > 0 {
		codexArgs = append(codexArgs, "--")
		codexArgs = append(codexArgs, rest...)
	}
	return codexCommand(home, cfgPath, codexArgs)
}

func runChatGPTSubscription(home, cfgPath string, args []string) error {
	c, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	path, err := codexPath("")
	if err != nil {
		return err
	}
	if err = chatGPT(path); err != nil {
		return err
	}
	executable, err := codexExecutable()
	if err != nil {
		return err
	}
	cmd := exec.Command(executable, args...)
	env := setEnv(os.Environ(), "CODEX_HOME", filepath.Dir(path))
	env = withoutEnv(env, c.Auth.ProxyAPIKeyEnv)
	for _, provider := range c.Providers {
		env = withoutEnv(env, provider.APIKeyEnv)
	}
	env = withoutEnv(env, "ARVANAI_KEY")
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func trimDoubleDash(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return args[1:]
	}
	return args
}
