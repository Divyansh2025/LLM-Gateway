// Command gateway runs the LLM gateway HTTP server.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Divyansh2025/llm-gateway/internal/config"
	"github.com/Divyansh2025/llm-gateway/internal/proxy"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.FromEnv()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	// No overall client timeout: streamed responses can legitimately run for
	// minutes. Connecting and waiting for the first response header are bounded.
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       90 * time.Second,
		},
	}

	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", proxy.NewHandler(cfg, client, log, proxy.Options{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		providers := make([]string, 0, len(cfg.Providers))
		for name := range cfg.Providers {
			providers = append(providers, name)
		}
		routes := make([]string, 0, len(cfg.Routes))
		for name := range cfg.Routes {
			routes = append(routes, name)
		}
		log.Info("listening", "addr", cfg.Addr, "providers", providers, "routes", routes)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	// Give in-flight requests (including streams) a chance to finish.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
}
