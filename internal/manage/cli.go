package manage

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	assets "github.com/local/codex-deepseek-proxy"
	"github.com/local/codex-deepseek-proxy/internal/config"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const Help = `Codex ArvanCloud Proxy

Usage:
  cgw COMMAND [options] [arguments]

Available commands:
  list                         Show this command list
  clear                        Clear the terminal screen
  init                         Create private settings and a local proxy key
  models                       Choose a Cheap, Medium, or Frontier coding model
  history                      List all Codex session IDs without prompt contents
  resume SESSION_ID [PROMPT]   Resume a Codex session by its full ID
  run [-- ...]                 Start Codex with the saved ArvanCloud model
  run codex [-- ...]           Start Codex with your ChatGPT subscription

Server:
  start                        Start the proxy in the background
  stop                         Stop the background proxy
  restart                      Restart the background proxy
  status                       Show process, URL, uptime, and configuration
  serve                        Run the proxy in the foreground
  logs [options]               Read logs; supports --errors, --lines, and --follow
  doctor                       Check local configuration and dependencies
  check                        Test every coding model with a tiny paid tool call

Credentials:
  set-key APIKEY               Save or replace the Arvan API key
  del-key                      Delete the stored Arvan API key
  ls-key                       Show whether the Arvan API key is stored

Advanced Codex commands:
  codex install                Install the Arvan provider and model profiles
  codex use MODEL              Install profiles and select a Codex default
  codex chatgpt                Select the built-in OpenAI provider
  codex backups                List exact configuration snapshots
  codex restore [options]      Restore configuration; supports --backup and --force
  codex run [options]          Run a specific proxy model with --model MODEL

Global options:
  --home DIR                   Override the private state directory
  --config FILE                Override the proxy YAML configuration

Run without arguments to open this interactive console. Type exit or quit to close it.
`

func Run(args []string) error {
	if len(args) == 0 {
		return interactiveConsole()
	}
	fs := flag.NewFlagSet("cgw", flag.ContinueOnError)
	homeArg := fs.String("home", "", "private state directory")
	cfgArg := fs.String("config", "", "server config")
	fs.Usage = func() { fmt.Print(Help) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	args = fs.Args()
	if len(args) == 0 {
		return interactiveConsole()
	}
	if args[0] == "list" {
		fmt.Print(Help)
		return nil
	}
	if args[0] == "clear" {
		if len(args) != 1 {
			return errors.New("usage: cgw clear")
		}
		return clearScreen()
	}
	home := *homeArg
	if home == "" {
		base, e := os.UserConfigDir()
		if e != nil {
			return e
		}
		home = filepath.Join(base, "codex-deepseek-proxy")
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return err
	}
	cfgPath := *cfgArg
	if cfgPath == "" {
		cfgPath = filepath.Join(home, "config.yaml")
		if args[0] == "serve" {
			if _, e := os.Stat(cfgPath); errors.Is(e, os.ErrNotExist) {
				cfgPath = "config.yaml"
			}
		}
	}
	cfgPath, err = filepath.Abs(cfgPath)
	if err != nil {
		return err
	}
	if err = privateDir(home); err != nil {
		return err
	}
	switch args[0] {
	case "init":
		if len(args) != 1 {
			return errors.New("usage: cgw init")
		}
		unlock, e := lock(filepath.Join(home, "operation.lock"))
		if e != nil {
			return e
		}
		defer unlock()
		if _, e = os.Stat(cfgPath); errors.Is(e, os.ErrNotExist) {
			if e = os.MkdirAll(filepath.Dir(cfgPath), 0700); e != nil {
				return e
			}
			if e = atomicWrite(cfgPath, assets.DefaultConfig); e != nil {
				return e
			}
		} else if e != nil {
			return e
		}
		secrets, e := loadSecrets(home)
		if e != nil {
			return e
		}
		if secrets["proxy"] == "" {
			secrets["proxy"] = randomSecret()
			if e = saveSecrets(home, secrets); e != nil {
				return e
			}
		}
		fmt.Printf("Initialized %s\nConfig: %s\nNext: cgw set-key APIKEY; cgw start\n", home, cfgPath)
		return nil
	case "set-key", "del-key", "ls-key":
		return keyCommand(home, args)
	case "logs":
		return logs(home, args[1:])
	case "status":
		st, e := control(home, "GET", "/status")
		if e != nil {
			return fmt.Errorf("stopped or unreachable: %w", e)
		}
		fmt.Printf("Running PID %d\nURL: %s\nUptime: %s\nConfig: %s\n", st.PID, st.URL, time.Since(st.Started).Round(time.Second), st.Config)
		return nil
	case "start", "stop", "restart":
		if len(args) != 1 {
			return fmt.Errorf("usage: cgw %s", args[0])
		}
		unlock, e := lock(filepath.Join(home, "operation.lock"))
		if e != nil {
			return e
		}
		defer unlock()
		if args[0] == "stop" {
			return stop(home)
		}
		if args[0] == "restart" {
			if _, e = control(home, "GET", "/status"); e == nil {
				if e = stop(home); e != nil {
					return e
				}
			}
		}
		return start(home, cfgPath)
	case "serve":
		if len(args) > 2 || (len(args) == 2 && args[1] != "--managed") {
			return errors.New("usage: cgw serve")
		}
		return Serve(home, cfgPath, len(args) == 2)
	case "models":
		if len(args) != 1 {
			return errors.New("usage: cgw models")
		}
		return modelsCommand(home, cfgPath)
	case "history":
		if len(args) != 1 {
			return errors.New("usage: cgw history")
		}
		path, e := codexPath("")
		if e != nil {
			return e
		}
		return historyCommand(filepath.Dir(path), os.Stdout)
	case "resume":
		return resumeCommand(home, cfgPath, args[1:])
	case "run":
		return runCommand(home, cfgPath, args[1:])
	case "doctor":
		return doctor(home, cfgPath)
	case "check":
		return check(home, cfgPath, args[1:])
	case "codex":
		return codexCommand(home, cfgPath, args[1:])
	default:
		return fmt.Errorf("unknown command %q; run cgw list", args[0])
	}
}

func keyCommand(home string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: cgw set-key APIKEY | del-key | ls-key")
	}
	if args[0] == "ls-key" {
		if len(args) != 1 {
			return errors.New("usage: cgw ls-key")
		}
		s, err := loadSecrets(home)
		if err != nil {
			return err
		}
		fmt.Printf("Arvan API key: stored=%t\n", s["arvan"] != "")
		return nil
	}
	if args[0] == "set-key" {
		if len(args) != 2 {
			return errors.New("usage: cgw set-key APIKEY")
		}
		if !validKey(args[1]) {
			return errors.New("API key must be nonempty, single-line, and at most 16 KiB")
		}
	} else if args[0] == "del-key" {
		if len(args) != 1 {
			return errors.New("usage: cgw del-key")
		}
	} else {
		return errors.New("usage: cgw set-key APIKEY | del-key | ls-key")
	}
	unlock, err := lock(filepath.Join(home, "operation.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	s, err := loadSecrets(home)
	if err != nil {
		return err
	}
	if args[0] == "del-key" {
		delete(s, "arvan")
	} else {
		s["arvan"] = args[1]
	}
	if err = saveSecrets(home, s); err != nil {
		return err
	}
	fmt.Println("Credential store updated. Run cgw restart to apply to a running server.")
	return nil
}

func codexCommand(home, cfgPath string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: cgw codex install|use|chatgpt|backups|restore|run")
	}
	action := args[0]
	rest := args[1:]
	model := ""
	if action == "use" {
		if len(rest) == 0 {
			return errors.New("usage: cgw codex use MODEL")
		}
		model = rest[0]
		rest = rest[1:]
	}
	fs := flag.NewFlagSet("codex", flag.ContinueOnError)
	file := fs.String("config-file", "", "Codex TOML path")
	backup := fs.String("backup", "original", "backup filename")
	force := fs.Bool("force", false, "restore despite outside edits")
	runModel := fs.String("model", "deepseek-v4-pro", "model to run")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if action != "run" && fs.NArg() > 0 {
		return errors.New("unexpected arguments")
	}
	path, err := codexPath(*file)
	if err != nil {
		return err
	}
	switch action {
	case "restore":
		return restoreCodex(path, *backup, *force)
	case "backups":
		return listBackups(path)
	case "chatgpt":
		return chatGPT(path)
	case "install", "use":
		c, e := config.Load(cfgPath)
		if e != nil {
			return e
		}
		return installProfiles(path, c, model)
	case "run":
		c, e := config.Load(cfgPath)
		if e != nil {
			return e
		}
		if _, ok := c.Models[*runModel]; !ok {
			return errors.New("unknown proxy model")
		}
		executable, e := codexExecutable()
		if e != nil {
			return e
		}
		if filepath.Base(path) != "config.toml" {
			return errors.New("codex run requires a config.toml filename")
		}
		if e = installProfiles(path, c, ""); e != nil {
			return e
		}
		unlock, e := lock(filepath.Join(home, "operation.lock"))
		if e != nil {
			return e
		}
		e = start(home, cfgPath)
		unlock()
		if e != nil {
			return e
		}
		if e = applySecrets(home, c); e != nil {
			return e
		}
		profile, e := codexProfileName(*runModel)
		if e != nil {
			return e
		}
		cmd := exec.Command(executable, append([]string{"--profile", profile}, fs.Args()...)...)
		env := setEnv(os.Environ(), "CODEX_HOME", filepath.Dir(path))
		for _, p := range c.Providers {
			env = withoutEnv(env, p.APIKeyEnv)
		}
		env = withoutEnv(env, "ARVANAI_KEY")
		cmd.Env = env
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	default:
		return fmt.Errorf("unknown codex command %q", action)
	}
}
func withoutEnv(env []string, key string) []string {
	out := make([]string, 0, len(env))
	for _, s := range env {
		k, _, _ := strings.Cut(s, "=")
		if !strings.EqualFold(k, key) {
			out = append(out, s)
		}
	}
	return out
}
func setEnv(env []string, key, value string) []string {
	return append(withoutEnv(env, key), key+"="+value)
}

func logs(home string, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	onlyErrors := fs.Bool("errors", false, "errors only")
	lines := fs.Int("lines", 100, "tail lines")
	follow := fs.Bool("follow", false, "follow log")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *lines < 1 || *lines > 10000 {
		return errors.New("lines must be 1..10000")
	}
	path := filepath.Join(home, "server.log")
	if err := noLinks(path); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	var offset int64
	first := true
	var previous os.FileInfo
	pending := ""
	for {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		if previous != nil && (!os.SameFile(previous, info) || info.Size() < offset) {
			offset = 0
			pending = ""
		}
		previous = info
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return err
		}
		b, err := io.ReadAll(io.LimitReader(f, 6<<20))
		f.Close()
		if err != nil {
			return err
		}
		offset += int64(len(b))
		parts := strings.Split(pending+string(b), "\n")
		pending = parts[len(parts)-1]
		parts = parts[:len(parts)-1]
		var filtered []string
		for _, line := range parts {
			if line != "" && (!*onlyErrors || strings.Contains(line, `"level":"ERROR"`)) {
				filtered = append(filtered, line)
			}
		}
		if first && len(filtered) > *lines {
			filtered = filtered[len(filtered)-*lines:]
		}
		first = false
		for _, line := range filtered {
			fmt.Println(redact(home, line))
		}
		if !*follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func doctor(home, cfgPath string) error {
	bad := false
	c, err := config.Load(cfgPath)
	if err != nil {
		fmt.Printf("Config: ERROR %v\n", err)
		bad = true
	} else {
		fmt.Println("Config: OK")
	}
	s, e := loadSecrets(home)
	if e != nil {
		return e
	}
	for id, p := range c.Providers {
		available := s[id] != "" || os.Getenv(p.APIKeyEnv) != "" || (id == "arvan" && os.Getenv("ARVANAI_KEY") != "")
		fmt.Printf("Provider %s: configured=%t\n", id, available)
	}
	fmt.Printf("Proxy key: configured=%t\n", s["proxy"] != "" || os.Getenv(c.Auth.ProxyAPIKeyEnv) != "")
	if st, e := control(home, "GET", "/status"); e == nil {
		fmt.Printf("Server: running (%s)\n", st.URL)
	} else {
		fmt.Println("Server: stopped or unreachable")
	}
	if _, e = codexExecutable(); e == nil {
		fmt.Println("Codex CLI: on PATH")
	} else {
		fmt.Println("Codex CLI: not on PATH")
	}
	if p, e := codexPath(""); e == nil {
		if _, _, _, e = loadTOML(p); e != nil {
			fmt.Printf("Codex config: ERROR %v\n", e)
			bad = true
		} else {
			fmt.Printf("Codex config: valid (%s)\n", p)
		}
	}
	if bad {
		return errors.New("diagnostics found invalid configuration")
	}
	return nil
}

func check(home, cfgPath string, requested []string) error {
	c, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if err = applySecrets(home, c); err != nil {
		return err
	}
	st, err := control(home, "GET", "/status")
	if err != nil {
		return errors.New("start the managed server before running check")
	}
	available := make([]string, 0, len(c.Models))
	routes := make(map[string]string, len(c.Models))
	for name, model := range c.Models {
		available = append(available, model.UpstreamModel)
		routes[strings.ToLower(model.UpstreamModel)] = name
	}
	choices := curatedCodingModels(available)
	if len(choices) == 0 {
		return errors.New("no curated coding models are configured; run cgw models")
	}
	if len(requested) > 0 {
		wanted := make(map[string]bool, len(requested))
		for _, name := range requested {
			wanted[strings.ToLower(name)] = true
		}
		filtered := choices[:0]
		for _, choice := range choices {
			proxyModel := routes[strings.ToLower(choice.ID)]
			if wanted[strings.ToLower(choice.ID)] || wanted[strings.ToLower(proxyModel)] {
				filtered = append(filtered, choice)
				delete(wanted, strings.ToLower(choice.ID))
				delete(wanted, strings.ToLower(proxyModel))
			}
		}
		if len(wanted) > 0 {
			unknown := make([]string, 0, len(wanted))
			for name := range wanted {
				unknown = append(unknown, name)
			}
			sort.Strings(unknown)
			return fmt.Errorf("unknown configured coding model(s): %s", strings.Join(unknown, ", "))
		}
		choices = filtered
	}
	client := &http.Client{Timeout: 150 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	failures := 0
	for _, choice := range choices {
		model := routes[strings.ToLower(choice.ID)]
		if err = checkCustomToolCall(client, st.URL, os.Getenv(c.Auth.ProxyAPIKeyEnv), model); err != nil {
			fmt.Printf("FAIL  %-10s %s: %v\n", choice.Tier, choice.ID, err)
			failures++
		} else {
			fmt.Printf("PASS  %-10s %s\n", choice.Tier, choice.ID)
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d models failed the tool-call check", failures, len(choices))
	}
	fmt.Printf("All %d coding models completed a custom tool call.\n", len(choices))
	return nil
}

func checkCustomToolCall(client *http.Client, baseURL, proxyKey, model string) error {
	body := fmt.Sprintf(`{"model":%q,"input":"Call ping once with value ok. Do not answer with text.","tools":[{"type":"custom","name":"ping","description":"Connectivity probe"}],"tool_choice":"required","stream":false}`, model)
	req, err := http.NewRequest("POST", baseURL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+proxyKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var apiErr struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(payload, &apiErr) == nil && apiErr.Error.Message != "" {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, apiErr.Error.Message)
		}
		return fmt.Errorf("HTTP %d; inspect cgw logs --errors", resp.StatusCode)
	}
	var result struct {
		Output []struct {
			Type    string `json:"type"`
			Name    string `json:"name"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return fmt.Errorf("invalid response: %w", err)
	}
	for _, item := range result.Output {
		if item.Type == "custom_tool_call" && item.Name == "ping" {
			return nil
		}
	}
	types := make([]string, 0, len(result.Output))
	message := ""
	for _, item := range result.Output {
		types = append(types, item.Type)
		for _, content := range item.Content {
			if content.Text != "" {
				message = content.Text
			}
		}
	}
	if len(message) > 200 {
		message = message[:200] + "..."
	}
	if message != "" {
		return fmt.Errorf("model returned no ping custom-tool call (output types: %s; text: %q)", strings.Join(types, ", "), message)
	}
	return fmt.Errorf("model returned no ping custom-tool call (output types: %s)", strings.Join(types, ", "))
}
