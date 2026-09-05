package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/local/codex-deepseek-proxy/internal/catalog"
	"github.com/local/codex-deepseek-proxy/internal/chat"
	"github.com/local/codex-deepseek-proxy/internal/config"
	"github.com/local/codex-deepseek-proxy/internal/ids"
	"github.com/local/codex-deepseek-proxy/internal/providers"
	"github.com/local/codex-deepseek-proxy/internal/responses"
	"github.com/local/codex-deepseek-proxy/internal/routing"
)

type Server struct {
	cfg      config.Config
	registry *providers.Registry
	router   *routing.Router
	logger   *slog.Logger
	handler  http.Handler
}

func New(cfg config.Config, logger *slog.Logger) *Server {
	cfg = config.ArvanOnly(cfg)
	registry := providers.NewRegistry(cfg)
	s := &Server{cfg: cfg, registry: registry, router: routing.New(cfg, registry), logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /health/providers", s.providerHealth)
	mux.Handle("GET /v1/models", s.auth(http.HandlerFunc(s.models)))
	mux.Handle("POST /v1/responses", s.auth(http.HandlerFunc(s.createResponse)))
	s.handler = s.requestID(s.recoverer(mux))
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) HTTPServer() *http.Server {
	return &http.Server{
		Addr: fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port), Handler: s.handler,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second,
		MaxHeaderBytes: s.cfg.Limits.MaxHeaderBytes,
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) providerHealth(w http.ResponseWriter, _ *http.Request) {
	result := make(map[string]any)
	for id, p := range s.registry.All() {
		result[id] = map[string]any{"configured": p.Configured()}
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": result})
}

func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	data := make([]any, 0, len(s.cfg.Models))
	catalogModels := make([]any, 0, len(s.cfg.Models))
	names := make([]string, 0, len(s.cfg.Models))
	for name := range s.cfg.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, id := range names {
		m, ok := s.cfg.Models[id]
		if !ok {
			continue
		}
		owner := "codex-deepseek-proxy"
		if m.Provider != "" {
			owner = m.Provider
		}
		data = append(data, map[string]any{"id": id, "object": "model", "created": 0, "owned_by": owner})
		catalogModels = append(catalogModels, catalog.Metadata(id, m.UpstreamModel))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data, "models": catalogModels})
}

func (s *Server) createResponse(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID := RequestID(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Limits.MaxRequestBytes)
	rawRequest, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		s.requestError(w, http.StatusBadRequest, "invalid_request_error", friendlyJSONError(readErr))
		return
	}
	dec := json.NewDecoder(bytes.NewReader(rawRequest))
	var rr responses.Request
	if err := dec.Decode(&rr); err != nil {
		s.requestError(w, http.StatusBadRequest, "invalid_request_error", friendlyJSONError(err))
		return
	}
	if err := ensureEOF(dec); err != nil {
		s.requestError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	candidates, err := s.router.Candidates(rr.Model)
	if err != nil {
		s.requestError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if len(candidates) == 1 && candidates[0].UpstreamAPI == "responses" {
		s.createNativeResponse(w, r, rr, rawRequest, candidates[0], started, requestID)
		return
	}
	chatReq, err := responses.ToChat(rr)
	if err != nil {
		s.requestError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	var lastErr error
	lastStatus := 0
	retries := 0
candidateLoop:
	for ci, candidate := range candidates {
		for attempt := 0; attempt < 2; attempt++ {
			lastStatus = 0
			if attempt > 0 {
				retries++
				if !waitContext(r.Context(), time.Duration(250*(1<<(attempt-1)))*time.Millisecond) {
					return
				}
			}
			chatReq.Model = candidate.UpstreamModel
			chatReq.Stream = candidate.Provider.UpstreamStream()
			attemptCtx, cancel := context.WithCancel(r.Context())
			upstream, callErr := candidate.Provider.CreateChatCompletion(attemptCtx, &chatReq)
			if callErr != nil {
				cancel()
				lastErr = callErr
				if !isTransientError(callErr) && !errors.Is(callErr, providers.ErrAPIKeyMissing) {
					break
				}
				continue
			}
			if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
				body := readLimited(upstream.Body, 1<<20)
				upstream.Body.Close()
				cancel()
				lastStatus = upstream.StatusCode
				lastErr = fmt.Errorf("%s", upstreamMessage(body, upstream.Status))
				if !isTransientStatus(upstream.StatusCode) {
					break
				}
				continue
			}

			failover := ci > 0
			if chatReq.Stream {
				committed, streamErr := s.consumeStream(r.Context(), w, rr, candidate, upstream.Body, cancel)
				upstream.Body.Close()
				cancel()
				if committed {
					s.logResult(started, requestID, rr.Model, candidate, streamErr, retries, failover)
					return
				}
				lastErr = streamErr
				if !isRetryableStreamError(streamErr) {
					break candidateLoop
				}
				continue
			}
			completion, decodeErr := decodeCompletion(upstream.Body)
			upstream.Body.Close()
			cancel()
			if decodeErr != nil {
				lastErr = decodeErr
				break candidateLoop
			}
			streamErr := s.sendCompletion(w, rr, completion)
			s.logResult(started, requestID, rr.Model, candidate, streamErr, retries, failover)
			return
		}
		// A non-transient HTTP response must not be sent to another provider.
		if lastStatus != 0 && !isTransientStatus(lastStatus) {
			break
		}
	}
	status := http.StatusBadGateway
	if lastStatus == http.StatusTooManyRequests {
		status = http.StatusTooManyRequests
	} else if errors.Is(lastErr, providers.ErrAPIKeyMissing) || isTransientStatus(lastStatus) {
		status = http.StatusServiceUnavailable
	}
	message := "upstream provider unavailable"
	if lastErr != nil {
		message = lastErr.Error()
	}
	s.logger.Error("response failed", "request_id", requestID, "proxy_model", rr.Model, "status", status, "duration_ms", time.Since(started).Milliseconds(), "error", message, "retry_count", retries)
	s.requestError(w, status, "upstream_error", message)
}

func (s *Server) createNativeResponse(w http.ResponseWriter, r *http.Request, rr responses.Request, rawRequest []byte, candidate routing.Candidate, started time.Time, requestID string) {
	body, err := nativeRequestBody(rawRequest, candidate.UpstreamModel)
	if err != nil {
		s.requestError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var lastErr error
	lastStatus, retries := 0, 0
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			retries++
			if !waitContext(r.Context(), time.Duration(250*(1<<(attempt-1)))*time.Millisecond) {
				return
			}
		}
		attemptCtx, cancel := context.WithCancel(r.Context())
		upstream, callErr := candidate.Provider.CreateResponse(attemptCtx, body, rr.Stream)
		if callErr != nil {
			cancel()
			lastErr = callErr
			if !isTransientError(callErr) && !errors.Is(callErr, providers.ErrAPIKeyMissing) {
				break
			}
			continue
		}
		if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
			payload := readLimited(upstream.Body, 1<<20)
			cancel()
			lastStatus = upstream.StatusCode
			lastErr = fmt.Errorf("%s", upstreamMessage(payload, upstream.Status))
			if !isTransientStatus(upstream.StatusCode) {
				break
			}
			continue
		}
		committed, relayErr := s.relayNativeResponse(attemptCtx, w, rr, upstream.Body)
		upstream.Body.Close()
		cancel()
		if committed {
			s.logResult(started, requestID, rr.Model, candidate, relayErr, retries, false)
			return
		}
		lastErr = relayErr
		if !isRetryableStreamError(relayErr) {
			break
		}
	}
	status := http.StatusBadGateway
	if lastStatus == http.StatusTooManyRequests {
		status = http.StatusTooManyRequests
	} else if errors.Is(lastErr, providers.ErrAPIKeyMissing) || isTransientStatus(lastStatus) {
		status = http.StatusServiceUnavailable
	}
	message := "upstream provider unavailable"
	if lastErr != nil {
		message = lastErr.Error()
	}
	s.logger.Error("native response failed", "request_id", requestID, "proxy_model", rr.Model, "status", status, "duration_ms", time.Since(started).Milliseconds(), "error", message, "retry_count", retries)
	s.requestError(w, status, "upstream_error", message)
}

func nativeRequestBody(raw []byte, upstreamModel string) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("invalid native Responses request: %w", err)
	}
	model, _ := json.Marshal(upstreamModel)
	request["model"] = model
	return json.Marshal(request)
}

type nativeChunk struct {
	data []byte
	err  error
}

func nativeChunks(ctx context.Context, r io.Reader) <-chan nativeChunk {
	ch := make(chan nativeChunk, 1)
	go func() {
		defer close(ch)
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				data := append([]byte(nil), buf[:n]...)
				select {
				case ch <- nativeChunk{data: data}:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				select {
				case ch <- nativeChunk{err: err}:
				case <-ctx.Done():
				}
				return
			}
		}
	}()
	return ch
}

func nextNativeChunk(ctx context.Context, chunks <-chan nativeChunk, idle time.Duration) (nativeChunk, error) {
	timer := time.NewTimer(idle)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nativeChunk{}, ctx.Err()
	case <-timer.C:
		return nativeChunk{}, errors.New("upstream stream idle timeout")
	case chunk, ok := <-chunks:
		if !ok {
			return nativeChunk{}, io.EOF
		}
		return chunk, nil
	}
}

func (s *Server) relayNativeResponse(ctx context.Context, w http.ResponseWriter, rr responses.Request, body io.Reader) (bool, error) {
	if !rr.Stream {
		payload, err := io.ReadAll(io.LimitReader(body, 64<<20))
		if err != nil {
			return false, err
		}
		var response map[string]any
		if err = json.Unmarshal(payload, &response); err != nil {
			return false, fmt.Errorf("invalid upstream Responses JSON: %w", err)
		}
		response["model"] = rr.Model
		writeJSON(w, http.StatusOK, response)
		return true, nil
	}
	chunks := nativeChunks(ctx, body)
	idle := time.Duration(s.cfg.Timeouts.IdleStreamSeconds) * time.Second
	first, err := nextNativeChunk(ctx, chunks, idle)
	if err != nil {
		return false, err
	}
	if first.err != nil {
		return false, first.err
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	committed := true
	write := func(chunk nativeChunk) error {
		if chunk.err != nil {
			if errors.Is(chunk.err, io.EOF) {
				return io.EOF
			}
			return chunk.err
		}
		if _, err := w.Write(chunk.data); err != nil {
			return err
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return nil
	}
	if err = write(first); err != nil {
		return committed, err
	}
	for {
		chunk, readErr := nextNativeChunk(ctx, chunks, idle)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return committed, nil
			}
			return committed, readErr
		}
		if err = write(chunk); errors.Is(err, io.EOF) {
			return committed, nil
		} else if err != nil {
			return committed, err
		}
	}
}

func (s *Server) consumeStream(ctx context.Context, w http.ResponseWriter, rr responses.Request, c routing.Candidate, body io.Reader, cancel context.CancelFunc) (bool, error) {
	items := parseStream(body)
	idle := time.Duration(s.cfg.Timeouts.IdleStreamSeconds) * time.Second
	first, err := nextStreamItem(ctx, items, idle)
	if err != nil {
		cancel()
		return false, err
	}
	var writer *responses.SSEWriter
	if rr.Stream {
		writer, err = responses.NewSSEWriter(w)
		if err != nil {
			return false, err
		}
	}
	state := responses.NewOutputState(rr.Model, writer)
	state.ConfigureRequest(rr)
	if rr.Stream {
		if err = state.Start(); err != nil {
			return true, err
		}
	}
	committed := rr.Stream
	apply := func(item streamItem) error {
		if item.done {
			return io.EOF
		}
		if item.err != nil {
			return item.err
		}
		return state.ApplyChunk(item.chunk)
	}
	if err = apply(first); err == io.EOF {
		return s.finishState(w, rr, state, committed)
	} else if err != nil {
		if committed {
			_ = state.Fail("upstream_error", err.Error())
			return true, err
		}
		return false, err
	}
	for {
		item, readErr := nextStreamItem(ctx, items, idle)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) && state.FinishReason != "" {
				return s.finishState(w, rr, state, committed)
			}
			cancel()
			if committed {
				_ = state.Fail("upstream_error", readErr.Error())
				return true, readErr
			}
			return false, readErr
		}
		err = apply(item)
		if err == io.EOF {
			return s.finishState(w, rr, state, committed)
		}
		if err != nil {
			if committed {
				_ = state.Fail("upstream_error", err.Error())
				return true, err
			}
			return false, err
		}
	}
}

func (s *Server) finishState(w http.ResponseWriter, rr responses.Request, state *responses.OutputState, committed bool) (bool, error) {
	resp, err := state.Complete()
	if err != nil {
		return committed, err
	}
	if !rr.Stream {
		writeJSON(w, http.StatusOK, resp)
		committed = true
	}
	return committed, nil
}

func (s *Server) sendCompletion(w http.ResponseWriter, rr responses.Request, completion chat.Completion) error {
	var writer *responses.SSEWriter
	var err error
	if rr.Stream {
		writer, err = responses.NewSSEWriter(w)
		if err != nil {
			return err
		}
	}
	state := responses.NewOutputState(rr.Model, writer)
	state.ConfigureRequest(rr)
	if err = state.Start(); err != nil {
		return err
	}
	if err = state.ApplyCompletion(completion); err != nil {
		if rr.Stream {
			_ = state.Fail("upstream_error", err.Error())
		}
		return err
	}
	resp, err := state.Complete()
	if err != nil {
		return err
	}
	if !rr.Stream {
		writeJSON(w, http.StatusOK, resp)
	}
	return nil
}

func (s *Server) logResult(start time.Time, requestID, model string, c routing.Candidate, err error, retries int, failover bool) {
	attrs := []any{"request_id", requestID, "proxy_model", model, "provider", c.Provider.ID(), "upstream_model", c.UpstreamModel, "duration_ms", time.Since(start).Milliseconds(), "retry_count", retries, "failover", failover}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
		s.logger.Error("response stream ended with error", attrs...)
		return
	}
	attrs = append(attrs, "status", "completed")
	s.logger.Info("response completed", attrs...)
}

func (s *Server) requestError(w http.ResponseWriter, status int, typ, message string) {
	writeJSON(w, status, responses.APIError{Error: responses.ErrorBody{Message: message, Type: typ}})
}

func decodeCompletion(r io.Reader) (chat.Completion, error) {
	var c chat.Completion
	dec := json.NewDecoder(io.LimitReader(r, 64<<20))
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid upstream JSON: %w", err)
	}
	if len(c.Choices) == 0 {
		return c, errors.New("upstream response contains no choices")
	}
	return c, nil
}

func ensureEOF(dec *json.Decoder) error {
	var v any
	if err := dec.Decode(&v); err != io.EOF {
		if err == nil {
			return errors.New("request body contains multiple JSON values")
		}
		return err
	}
	return nil
}
func readLimited(r io.ReadCloser, n int64) []byte {
	defer r.Close()
	b, _ := io.ReadAll(io.LimitReader(r, n))
	return b
}
func upstreamMessage(body []byte, fallback string) string {
	var value any
	if json.Unmarshal(body, &value) == nil {
		if message := errorMessage(value); message != "" {
			return message
		}
	}
	return fallback
}

func errorMessage(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		for _, key := range []string{"message", "body", "detail", "error"} {
			if child, ok := v[key]; ok {
				if message := errorMessage(child); message != "" {
					return message
				}
			}
		}
	}
	return ""
}
func waitContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func isTransientStatus(status int) bool {
	switch status {
	case 408, 429, 500, 502, 503, 504:
		return true
	}
	return false
}
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var n interface{ Temporary() bool }
	return errors.As(err, &n) && n.Temporary() || strings.Contains(strings.ToLower(err.Error()), "connection reset") || strings.Contains(strings.ToLower(err.Error()), "timeout")
}
func isRetryableStreamError(err error) bool {
	return errors.Is(err, io.EOF) || isTransientError(err)
}
func friendlyJSONError(err error) string {
	var max *http.MaxBytesError
	if errors.As(err, &max) {
		return fmt.Sprintf("request body exceeds %d bytes", max.Limit)
	}
	return "invalid JSON request: " + err.Error()
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := os.Getenv(s.cfg.Auth.ProxyAPIKeyEnv)
		header := r.Header.Get("Authorization")
		provided := ""
		if strings.HasPrefix(header, "Bearer ") {
			provided = strings.TrimPrefix(header, "Bearer ")
		}
		expectedHash := sha256.Sum256([]byte(expected))
		providedHash := sha256.Sum256([]byte(provided))
		if expected == "" || subtle.ConstantTimeCompare(providedHash[:], expectedHash[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="codex-deepseek-proxy"`)
			s.requestError(w, http.StatusUnauthorized, "authentication_error", "invalid proxy API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type contextKey string

const requestIDKey contextKey = "request-id"

func RequestID(ctx context.Context) string { v, _ := ctx.Value(requestIDKey).(string); return v }
func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if id == "" || len(id) > 128 {
			id = ids.New("req")
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.logger.Error("request panic", "request_id", RequestID(r.Context()), "panic", fmt.Sprint(v))
				s.requestError(w, http.StatusInternalServerError, "server_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
