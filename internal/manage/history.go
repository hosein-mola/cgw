package manage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/local/codex-deepseek-proxy/internal/config"
)

type codexHistoryEntry struct {
	ID        string
	Timestamp time.Time
	Provider  string
	CWD       string
}

func historyCommand(codexHome string, w io.Writer) error {
	entries, err := loadCodexHistory(codexHome)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(w, "No Codex sessions found.")
		return nil
	}
	fmt.Fprintf(w, "Codex session history (%d, newest last)\n", len(entries))
	fmt.Fprintln(w, "Resume with the command shown for each session.")
	for i, entry := range entries {
		stamp := entry.Timestamp.Local().Format("2006-01-02 15:04:05")
		provider := strings.TrimSpace(entry.Provider)
		if provider == "" {
			provider = "unknown provider"
		}
		cwd := strings.TrimSpace(entry.CWD)
		if cwd == "" {
			cwd = "unknown"
		}
		resume := "proxy resume " + entry.ID
		if strings.EqualFold(provider, "openai") {
			resume = "codex resume " + entry.ID
		}
		fmt.Fprintf(w, "\n[%d] %s  %s\n    ID: %s\n    CWD: %s\n    Resume: %s\n", i+1, stamp, provider, entry.ID, cwd, resume)
	}
	return nil
}

func loadCodexHistory(codexHome string) ([]codexHistoryEntry, error) {
	root := filepath.Join(codexHome, "sessions")
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var entries []codexHistoryEntry
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".jsonl") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		var meta struct {
			Type    string `json:"type"`
			Payload struct {
				SessionID     string `json:"session_id"`
				ID            string `json:"id"`
				Timestamp     string `json:"timestamp"`
				CWD           string `json:"cwd"`
				ModelProvider string `json:"model_provider"`
			} `json:"payload"`
		}
		decoder := json.NewDecoder(f)
		decodeErr := decoder.Decode(&meta)
		closeErr := f.Close()
		if closeErr != nil {
			return closeErr
		}
		if decodeErr != nil || meta.Type != "session_meta" {
			return nil
		}
		id := strings.TrimSpace(meta.Payload.ID)
		if id == "" {
			id = strings.TrimSpace(meta.Payload.SessionID)
		}
		if id == "" {
			return nil
		}
		stamp, _ := time.Parse(time.RFC3339Nano, meta.Payload.Timestamp)
		if stamp.IsZero() {
			if info, statErr := entry.Info(); statErr == nil {
				stamp = info.ModTime()
			}
		}
		entries = append(entries, codexHistoryEntry{ID: id, Timestamp: stamp, Provider: meta.Payload.ModelProvider, CWD: meta.Payload.CWD})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Timestamp.Equal(entries[j].Timestamp) {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Timestamp.Before(entries[j].Timestamp)
	})
	return entries, nil
}

func resumeCommand(home, cfgPath string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: proxy resume SESSION_ID [PROMPT]")
	}
	if !regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`).MatchString(args[0]) {
		return errors.New("SESSION_ID must be a full Codex UUID from proxy history")
	}
	path, err := codexPath("")
	if err != nil {
		return err
	}
	entries, err := loadCodexHistory(filepath.Dir(path))
	if err != nil {
		return err
	}
	var session codexHistoryEntry
	found := false
	for _, entry := range entries {
		if strings.EqualFold(entry.ID, args[0]) {
			session = entry
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("Codex session %s was not found", args[0])
	}
	executable, err := codexExecutable()
	if err != nil {
		return err
	}
	cmd := exec.Command(executable, append([]string{"resume"}, args...)...)
	env := setEnv(os.Environ(), "CODEX_HOME", filepath.Dir(path))
	if !strings.EqualFold(strings.TrimSpace(session.Provider), "openai") {
		c, loadErr := config.Load(cfgPath)
		if loadErr != nil {
			return loadErr
		}
		unlock, lockErr := lock(filepath.Join(home, "operation.lock"))
		if lockErr != nil {
			return lockErr
		}
		startErr := start(home, cfgPath)
		unlock()
		if startErr != nil {
			return startErr
		}
		if err = applySecrets(home, c); err != nil {
			return err
		}
		env = setEnv(os.Environ(), "CODEX_HOME", filepath.Dir(path))
		for _, provider := range c.Providers {
			env = withoutEnv(env, provider.APIKeyEnv)
		}
		env = withoutEnv(env, "ARVANAI_KEY")
	}
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func codexExecutable() (string, error) {
	if path, err := exec.LookPath("codex"); err == nil {
		return path, nil
	}
	if runtime.GOOS == "windows" {
		root := filepath.Join(os.Getenv("LOCALAPPDATA"), "OpenAI", "Codex", "bin")
		var newest string
		var newestTime time.Time
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.EqualFold(entry.Name(), "codex.exe") {
				return nil
			}
			if info, infoErr := entry.Info(); infoErr == nil && info.ModTime().After(newestTime) {
				newest, newestTime = path, info.ModTime()
			}
			return nil
		})
		if newest != "" {
			return newest, nil
		}
	}
	return "", errors.New("Codex CLI is not on PATH; install it first")
}
