// Package proxy forwards OpenAI-format chat requests to the right upstream provider.
package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Divyansh2025/llm-gateway/internal/config"
)

// maxBodyBytes caps request size so one client can't exhaust memory.
const maxBodyBytes = 1 << 20 // 1 MiB

// Handler routes POST /v1/chat/completions by the "provider/model" prefix in the request.
type Handler struct {
	providers map[string]config.Provider
	client    *http.Client
	log       *slog.Logger
}

func NewHandler(providers map[string]config.Provider, client *http.Client, log *slog.Logger) *Handler {
	return &Handler{providers: providers, client: client, log: log}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

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
	providerName, upstreamModel, ok := strings.Cut(model, "/")
	if !ok || upstreamModel == "" {
		writeError(w, http.StatusBadRequest, `model must look like "provider/model", e.g. "groq/llama-3.1-8b-instant"`)
		return
	}
	p, ok := h.providers[providerName]
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown or unconfigured provider: "+providerName)
		return
	}

	req["model"] = upstreamModel
	out, err := json.Marshal(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not encode upstream request")
		return
	}

	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(out))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not build upstream request")
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+p.APIKey)

	resp, err := h.client.Do(upReq)
	if err != nil {
		h.log.Warn("upstream request failed", "provider", p.Name, "model", upstreamModel, "err", err)
		writeError(w, http.StatusBadGateway, "provider "+p.Name+" is unreachable")
		return
	}
	defer resp.Body.Close()

	for _, k := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("X-Gateway-Provider", p.Name)
	w.WriteHeader(resp.StatusCode)

	n, copyErr := copyFlushing(w, resp.Body)
	h.log.Info("request",
		"provider", p.Name,
		"model", upstreamModel,
		"status", resp.StatusCode,
		"bytes", n,
		"latency_ms", time.Since(start).Milliseconds(),
		"err", copyErr,
	)
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
