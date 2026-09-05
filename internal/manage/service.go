package manage

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/local/codex-deepseek-proxy/internal/config"
	"github.com/local/codex-deepseek-proxy/internal/server"
)

type runtimeState struct {
	PID     int
	Control string
	Token   string
	URL     string
	Started time.Time
	Config  string
}

func control(home, method, path string) (runtimeState, error) {
	var st runtimeState
	if err := readJSON(filepath.Join(home, "runtime.json"), &st); err != nil {
		return st, err
	}
	host, _, err := net.SplitHostPort(st.Control)
	if err != nil || host != "127.0.0.1" {
		return st, errors.New("invalid control endpoint")
	}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	req, err := http.NewRequest(method, "http://"+st.Control+path, nil)
	if err != nil {
		return st, err
	}
	req.Header.Set("Authorization", "Bearer "+st.Token)
	resp, err := client.Do(req)
	if err != nil {
		return st, errors.New("managed server is not reachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return st, fmt.Errorf("managed server identity check failed (HTTP %d)", resp.StatusCode)
	}
	var reply struct{ Token string }
	if err = json.NewDecoder(resp.Body).Decode(&reply); err != nil || reply.Token != st.Token {
		return st, errors.New("managed server identity mismatch")
	}
	return st, nil
}

func start(home, configPath string) error {
	if st, err := control(home, "GET", "/status"); err == nil {
		if st.Config != configPath {
			return fmt.Errorf("server is already running with %s; stop it before choosing another config", st.Config)
		}
		fmt.Printf("Already running: PID %d at %s\n", st.PID, st.URL)
		return nil
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err = applySecrets(home, cfg); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// The log is opened by the child after it has detached.
	cmd := exec.Command(exe, "--home", home, "--config", configPath, "serve", "--managed")
	detach(cmd)
	if err = cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if st, e := control(home, "GET", "/status"); e == nil {
			fmt.Printf("Started: PID %d at %s\n", st.PID, st.URL)
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return errors.New("server did not become ready; run cgw logs --errors or cgw serve to see the startup error")
}

func stop(home string) error {
	st, err := control(home, "POST", "/shutdown")
	if err != nil {
		return fmt.Errorf("cannot stop managed server: %w", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err = control(home, "GET", "/status"); err != nil {
			fmt.Printf("Stopped managed server PID %d\n", st.PID)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("shutdown still in progress; check logs")
}

type logWriter struct {
	mu         sync.Mutex
	home, path string
}

func (l *logWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st, err := os.Stat(l.path); err == nil && st.Size() > 5<<20 {
		old := l.path + ".1"
		if err = noLinks(old); err != nil {
			return 0, err
		}
		if err = replaceFile(l.path, old); err != nil {
			return 0, err
		}
	}
	if err := noLinks(l.path); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	_, err = io.WriteString(f, redact(l.home, string(p)))
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func Serve(home, configPath string, managed bool) error {
	if managed {
		if _, e := control(home, "GET", "/status"); e == nil {
			return errors.New("a managed server is already running for this home")
		}
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err = applySecrets(home, cfg); err != nil {
		return err
	}
	if host := strings.TrimSpace(os.Getenv("PROXY_HOST")); host != "" {
		cfg.Server.Host = host
	}
	var output io.Writer = os.Stdout
	if managed {
		output = &logWriter{home: home, path: filepath.Join(home, "server.log")}
	}
	logger := slog.New(slog.NewJSONHandler(output, nil))
	srv := server.New(cfg, logger).HTTPServer()
	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		logger.Error("listen failed", "error", err)
		return err
	}
	defer listener.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var admin *http.Server
	if managed {
		ctl, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			return e
		}
		apiHost, apiPort, _ := net.SplitHostPort(listener.Addr().String())
		if apiHost == "0.0.0.0" || apiHost == "::" {
			apiHost = "127.0.0.1"
		}
		st := runtimeState{PID: os.Getpid(), Control: ctl.Addr().String(), Token: randomSecret(), URL: "http://" + net.JoinHostPort(apiHost, apiPort), Started: time.Now().UTC(), Config: configPath}
		mux := http.NewServeMux()
		reply := func(w http.ResponseWriter, r *http.Request) {
			got := sha256.Sum256([]byte(r.Header.Get("Authorization")))
			want := sha256.Sum256([]byte("Bearer " + st.Token))
			if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
				http.Error(w, "unauthorized", 401)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"Token": st.Token})
			if r.URL.Path == "/shutdown" {
				cancel()
			}
		}
		mux.HandleFunc("GET /status", reply)
		mux.HandleFunc("POST /shutdown", reply)
		admin = &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second}
		go admin.Serve(ctl)
		defer admin.Close()
		if e = writeJSON(filepath.Join(home, "runtime.json"), st); e != nil {
			return e
		}
		defer func() {
			var current runtimeState
			if readJSON(filepath.Join(home, "runtime.json"), &current) == nil && current.Token == st.Token {
				_ = os.Remove(filepath.Join(home, "runtime.json"))
			}
		}()
	}
	logger.Info("proxy listening", "address", srv.Addr)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(listener) }()
	select {
	case err = <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	if err = srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
		logger.Warn("shutdown deadline exceeded; closed active connections")
	}
	logger.Info("proxy stopped")
	return nil
}
