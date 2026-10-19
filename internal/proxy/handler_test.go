package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Divyansh2025/llm-gateway/internal/config"
)

// fastOpts keeps retry waits tiny so tests run quickly.
var fastOpts = Options{MaxAttempts: 3, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, BreakerThreshold: 100, BreakerCooldown: time.Minute}

// upstream is a fake provider that counts the requests it receives.
type upstream struct {
	*httptest.Server
	calls atomic.Int32
}

func newUpstream(t *testing.T, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		h(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

// newGateway starts a gateway with the given fake providers (by name) and routes.
func newGateway(t *testing.T, ups map[string]*upstream, routes map[string][]config.Target, opts Options) *httptest.Server {
	t.Helper()
	cfg := config.Config{Providers: map[string]config.Provider{}, Routes: routes}
	for name, u := range ups {
		cfg.Providers[name] = config.Provider{Name: name, BaseURL: u.URL, APIKey: name + "-key"}
	}
	h := NewHandler(cfg, http.DefaultClient, slog.New(slog.NewTextHandler(io.Discard, nil)), opts)
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)
	return gw
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func ok(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		w.Write([]byte(`{"error":{"message":"upstream says ` + http.StatusText(code) + `"}}`))
	}
}

func TestForwardsToProviderWithRewrittenModel(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		ok(w, r)
	})
	gw := newGateway(t, map[string]*upstream{"fake": u}, nil, fastOpts)

	resp := post(t, gw.URL, `{"model":"fake/small-model","messages":[{"role":"user","content":"hello"}],"temperature":0.2}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if gotAuth != "Bearer fake-key" {
		t.Errorf("upstream auth = %q", gotAuth)
	}
	if gotBody["model"] != "small-model" {
		t.Errorf("upstream model = %v, want small-model", gotBody["model"])
	}
	if gotBody["temperature"] != 0.2 {
		t.Errorf("other fields should pass through unchanged, temperature = %v", gotBody["temperature"])
	}
	if got := resp.Header.Get("X-Gateway-Provider"); got != "fake" {
		t.Errorf("X-Gateway-Provider = %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"hi"`) {
		t.Errorf("body = %s", body)
	}
}

func TestRejectsBadRequests(t *testing.T) {
	u := newUpstream(t, ok)
	gw := newGateway(t, map[string]*upstream{"fake": u}, nil, fastOpts)

	cases := map[string]string{
		"not json":         `{`,
		"unknown route":    `{"model":"gpt-4o"}`,
		"unknown provider": `{"model":"nope/gpt-4o"}`,
		"empty model":      `{"model":"fake/"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp := post(t, gw.URL, body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			var e struct {
				Error struct{ Message string } `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&e); err != nil || e.Error.Message == "" {
				t.Errorf("want OpenAI-style error body, got err=%v", err)
			}
		})
	}
	if u.calls.Load() != 0 {
		t.Error("upstream should not be called for bad requests")
	}
}

func TestRetriesTransientErrorsOnSameProvider(t *testing.T) {
	var n atomic.Int32
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			status(http.StatusServiceUnavailable)(w, r)
			return
		}
		ok(w, r)
	})
	gw := newGateway(t, map[string]*upstream{"fake": u}, nil, fastOpts)

	resp := post(t, gw.URL, `{"model":"fake/m"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retries", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Gateway-Attempts"); got != "3" {
		t.Errorf("attempts = %s, want 3", got)
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	u := newUpstream(t, status(http.StatusUnauthorized))
	backup := newUpstream(t, ok)
	routes := map[string][]config.Target{"r": {{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"}}}
	gw := newGateway(t, map[string]*upstream{"a": u, "b": backup}, routes, fastOpts)

	resp := post(t, gw.URL, `{"model":"r"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 passed through", resp.StatusCode)
	}
	if u.calls.Load() != 1 || backup.calls.Load() != 0 {
		t.Errorf("calls = %d/%d, want 1/0: a 401 would fail the same way again", u.calls.Load(), backup.calls.Load())
	}
}

func TestFallsBackToNextTargetInRoute(t *testing.T) {
	var gotModel string
	down := newUpstream(t, status(http.StatusInternalServerError))
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		gotModel, _ = b["model"].(string)
		ok(w, r)
	})
	routes := map[string][]config.Target{"fast": {{Provider: "a", Model: "model-a"}, {Provider: "b", Model: "model-b"}}}
	gw := newGateway(t, map[string]*upstream{"a": down, "b": up}, routes, fastOpts)

	resp := post(t, gw.URL, `{"model":"fast"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from fallback", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Gateway-Provider"); got != "b" {
		t.Errorf("provider = %q, want b", got)
	}
	if gotModel != "model-b" {
		t.Errorf("fallback got model %q, want model-b", gotModel)
	}
	if down.calls.Load() != 3 {
		t.Errorf("first provider calls = %d, want 3 (all attempts)", down.calls.Load())
	}
}

func TestReturnsLastUpstreamErrorWhenAllFail(t *testing.T) {
	u := newUpstream(t, status(http.StatusTooManyRequests))
	gw := newGateway(t, map[string]*upstream{"fake": u}, nil, fastOpts)

	resp := post(t, gw.URL, `{"model":"fake/m"}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Too Many Requests") {
		t.Errorf("body should be the upstream's error, got %s", body)
	}
}

func TestUnreachableProviderIs502(t *testing.T) {
	cfg := config.Config{Providers: map[string]config.Provider{
		"down": {Name: "down", BaseURL: "http://127.0.0.1:1", APIKey: "k"},
	}}
	h := NewHandler(cfg, http.DefaultClient, slog.New(slog.NewTextHandler(io.Discard, nil)), fastOpts)
	gw := httptest.NewServer(h)
	defer gw.Close()

	resp := post(t, gw.URL, `{"model":"down/m"}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestOpenBreakerSkipsProviderWithoutCallingIt(t *testing.T) {
	down := newUpstream(t, status(http.StatusServiceUnavailable))
	up := newUpstream(t, ok)
	routes := map[string][]config.Target{"r": {{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"}}}
	opts := fastOpts
	opts.BreakerThreshold = 3
	gw := newGateway(t, map[string]*upstream{"a": down, "b": up}, routes, opts)

	// First request: 3 failures on "a" open its breaker, then "b" serves it.
	if resp := post(t, gw.URL, `{"model":"r"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("first request status = %d", resp.StatusCode)
	}
	// Second request: "a" is skipped entirely, so it adds no latency.
	resp := post(t, gw.URL, `{"model":"r"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second request status = %d", resp.StatusCode)
	}
	if down.calls.Load() != 3 {
		t.Errorf("broken provider calls = %d, want 3 (none after breaker opened)", down.calls.Load())
	}
	if got := resp.Header.Get("X-Gateway-Attempts"); got != "1" {
		t.Errorf("second request attempts = %s, want 1", got)
	}
}

// The first streamed event must reach the client before the upstream finishes,
// otherwise users see nothing until the whole answer is generated.
func TestStreamsChunksAsTheyArrive(t *testing.T) {
	release := make(chan struct{})
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"delta\":\"first\"}\n\n"))
		w.(http.Flusher).Flush()
		<-release
		w.Write([]byte("data: [DONE]\n\n"))
	})
	gw := newGateway(t, map[string]*upstream{"fake": u}, nil, fastOpts)

	resp := post(t, gw.URL, `{"model":"fake/m","stream":true}`)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	close(release)
	if err != nil || !strings.Contains(line, "first") {
		t.Fatalf("first line = %q, err = %v", line, err)
	}
}
