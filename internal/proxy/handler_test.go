package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Divyansh2025/llm-gateway/internal/config"
)

// newGateway starts a gateway whose only provider, "fake", is the given upstream handler.
func newGateway(t *testing.T, upstream http.HandlerFunc) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)

	providers := map[string]config.Provider{
		"fake": {Name: "fake", BaseURL: up.URL, APIKey: "test-key"},
	}
	h := NewHandler(providers, up.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
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

func TestForwardsToProviderWithRewrittenModel(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	gw := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	})

	resp := post(t, gw.URL, `{"model":"fake/small-model","messages":[{"role":"user","content":"hello"}],"temperature":0.2}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if gotAuth != "Bearer test-key" {
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
	called := false
	gw := newGateway(t, func(w http.ResponseWriter, r *http.Request) { called = true })

	cases := map[string]string{
		"not json":         `{`,
		"no provider":      `{"model":"gpt-4o"}`,
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
	if called {
		t.Error("upstream should not be called for bad requests")
	}
}

func TestPassesThroughUpstreamErrors(t *testing.T) {
	gw := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	})
	resp := post(t, gw.URL, `{"model":"fake/m"}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
}

func TestUnreachableProviderIs502(t *testing.T) {
	providers := map[string]config.Provider{
		"down": {Name: "down", BaseURL: "http://127.0.0.1:1", APIKey: "k"},
	}
	h := NewHandler(providers, http.DefaultClient, slog.New(slog.NewTextHandler(io.Discard, nil)))
	gw := httptest.NewServer(h)
	defer gw.Close()

	resp := post(t, gw.URL, `{"model":"down/m"}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

// The first streamed event must reach the client before the upstream finishes,
// otherwise users see nothing until the whole answer is generated.
func TestStreamsChunksAsTheyArrive(t *testing.T) {
	release := make(chan struct{})
	gw := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"delta\":\"first\"}\n\n"))
		w.(http.Flusher).Flush()
		<-release
		w.Write([]byte("data: [DONE]\n\n"))
	})

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
