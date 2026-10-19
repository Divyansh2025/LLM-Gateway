// Package proxy forwards OpenAI-format chat requests to the right upstream
// provider, retrying and failing over when a provider misbehaves.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"github.com/Divyansh2025/llm-gateway/internal/breaker"
	"github.com/Divyansh2025/llm-gateway/internal/config"
)

// maxBodyBytes caps request size so one client can't exhaust memory.
const maxBodyBytes = 1 << 20 // 1 MiB

// maxErrorBodyBytes caps how much of a failed upstream response we keep to
// return to the client if every target fails.
const maxErrorBodyBytes = 64 << 10

// Options tunes retries and circuit breaking. Zero values get defaults.
type Options struct {
	MaxAttempts      int           // attempts per target, including the first (default 3)
	BaseBackoff      time.Duration // first retry waits up to this long (default 200ms)
	MaxBackoff       time.Duration // cap on any single wait (default 2s)
	BreakerThreshold int           // consecutive failures that open a provider's breaker (default 5)
	BreakerCooldown  time.Duration // how long an open breaker refuses requests (default 30s)
}

func (o *Options) setDefaults() {
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 3
	}
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = 200 * time.Millisecond
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 2 * time.Second
	}
	if o.BreakerThreshold <= 0 {
		o.BreakerThreshold = 5
	}
	if o.BreakerCooldown <= 0 {
		o.BreakerCooldown = 30 * time.Second
	}
}

// Handler serves POST /v1/chat/completions. The request's model is either
// "provider/model" (one target) or a route name from config (several targets
// tried in order).
type Handler struct {
	providers map[string]config.Provider
	routes    map[string][]config.Target
	breakers  map[string]*breaker.Breaker
	client    *http.Client
	log       *slog.Logger
	opts      Options
}

func NewHandler(cfg config.Config, client *http.Client, log *slog.Logger, opts Options) *Handler {
	opts.setDefaults()
	breakers := make(map[string]*breaker.Breaker, len(cfg.Providers))
	for name := range cfg.Providers {
		breakers[name] = breaker.New(opts.BreakerThreshold, opts.BreakerCooldown, time.Now)
	}
	return &Handler{
		providers: cfg.Providers,
		routes:    cfg.Routes,
		breakers:  breakers,
		client:    client,
		log:       log,
		opts:      opts,
	}
}

// failure is the last thing that went wrong, returned to the client if no
// target succeeds.
type failure struct {
	status      int
	contentType string
	body        []byte
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large or unreadable")
		return
	}

	// Decode into a map so every field we don't touch (messages, tools, stream...)
	// is passed through unchanged.
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "request body is not valid JSON")
		return
	}
	model, _ := req["model"].(string)
	targets, errMsg := h.resolve(model)
	if errMsg != "" {
		writeError(w, http.StatusBadRequest, errMsg)
		return
	}

	var last *failure
	attempts := 0
	for _, t := range targets {
		p := h.providers[t.Provider]
		br := h.breakers[t.Provider]

		for try := 0; try < h.opts.MaxAttempts; try++ {
			if try > 0 {
				if err := sleep(ctx, h.backoff(try)); err != nil {
					return // client went away
				}
			}
			if !br.Allow() {
				h.log.Warn("circuit open, skipping provider", "provider", p.Name)
				last = &failure{status: http.StatusServiceUnavailable}
				break // next target
			}

			attempts++
			resp, err := h.send(ctx, p, t.Model, req)
			if err != nil {
				if ctx.Err() != nil {
					return // client went away; not the provider's fault
				}
				br.Failure()
				h.log.Warn("upstream request failed", "provider", p.Name, "model", t.Model, "try", try+1, "err", err)
				last = &failure{status: http.StatusBadGateway}
				continue
			}
			if retryable(resp.StatusCode) {
				br.Failure()
				errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
				resp.Body.Close()
				h.log.Warn("upstream error", "provider", p.Name, "model", t.Model, "try", try+1, "status", resp.StatusCode)
				last = &failure{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: errBody}
				continue
			}

			// Anything else, including 4xx like a bad request or bad key, is the
			// provider answering correctly, so it goes straight back to the client.
			br.Success()
			h.relay(w, resp, p.Name, t.Model, attempts, start)
			return
		}
	}

	h.log.Error("all targets failed", "model", model, "attempts", attempts, "latency_ms", time.Since(start).Milliseconds())
	w.Header().Set("X-Gateway-Attempts", strconv.Itoa(attempts))
	if last != nil && last.body != nil {
		if last.contentType != "" {
			w.Header().Set("Content-Type", last.contentType)
		}
		w.WriteHeader(last.status)
		w.Write(last.body)
		return
	}
	status := http.StatusBadGateway
	if last != nil {
		status = last.status
	}
	writeError(w, status, "no provider could serve model "+model)
}

// resolve turns the request's model field into the targets to try, or returns
// a message explaining why it can't.
func (h *Handler) resolve(model string) ([]config.Target, string) {
	if t, ok := config.ParseTarget(model); ok {
		if _, ok := h.providers[t.Provider]; !ok {
			return nil, "unknown or unconfigured provider: " + t.Provider
		}
		return []config.Target{t}, ""
	}
	if targets, ok := h.routes[model]; ok {
		return targets, ""
	}
	return nil, `model must be "provider/model" (e.g. "groq/llama-3.1-8b-instant") or a configured route name`
}

func (h *Handler) send(ctx context.Context, p config.Provider, model string, req map[string]any) (*http.Response, error) {
	// Copy the request so each target gets its own model name.
	out := make(map[string]any, len(req))
	for k, v := range req {
		out[k] = v
	}
	out["model"] = model
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	return h.client.Do(upReq)
}

// relay streams a successful (or non-retryable) upstream response to the client.
func (h *Handler) relay(w http.ResponseWriter, resp *http.Response, provider, model string, attempts int, start time.Time) {
	defer resp.Body.Close()
	for _, k := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("X-Gateway-Provider", provider)
	w.Header().Set("X-Gateway-Attempts", strconv.Itoa(attempts))
	w.WriteHeader(resp.StatusCode)

	n, copyErr := copyFlushing(w, resp.Body)
	h.log.Info("request",
		"provider", provider,
		"model", model,
		"status", resp.StatusCode,
		"attempts", attempts,
		"bytes", n,
		"latency_ms", time.Since(start).Milliseconds(),
		"err", copyErr,
	)
}

// retryable reports whether a status means "try again, maybe elsewhere":
// rate limits and server-side errors. Other 4xx mean the request itself is
// wrong, and retrying would fail the same way.
func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// backoff returns a random wait in [0, min(MaxBackoff, BaseBackoff*2^(try-1))).
// The randomness ("full jitter") stops many clients that failed together from
// all retrying at the same instant.
func (h *Handler) backoff(try int) time.Duration {
	ceiling := h.opts.BaseBackoff << (try - 1)
	if ceiling <= 0 || ceiling > h.opts.MaxBackoff {
		ceiling = h.opts.MaxBackoff
	}
	return rand.N(ceiling)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// copyFlushing copies src to w and flushes after every chunk, so streamed
// (server-sent event) responses reach the client token by token instead of
// being buffered until the end.
func copyFlushing(w http.ResponseWriter, src io.Reader) (int64, error) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			written, werr := w.Write(buf[:n])
			total += int64(written)
			if werr != nil {
				return total, werr
			}
			if ferr := rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
				return total, ferr
			}
		}
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// writeError returns errors in the same shape OpenAI uses, so existing client
// SDKs can parse them.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": msg, "type": "gateway_error"},
	})
}
