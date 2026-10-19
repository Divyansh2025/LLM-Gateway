// Package config loads gateway settings from environment variables.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Provider is one upstream LLM API that speaks the OpenAI chat completions format.
type Provider struct {
	Name    string // prefix clients use in the model field, e.g. "groq" in "groq/llama-3.1-8b-instant"
	BaseURL string // everything before /chat/completions
	APIKey  string
}

// Target is one provider and the model name to send it.
type Target struct {
	Provider string
	Model    string
}

// Config is everything the gateway needs to start.
type Config struct {
	Addr      string
	Providers map[string]Provider
	// Routes maps a route name clients can use as the model (e.g. "fast") to
	// targets tried in order until one succeeds.
	Routes map[string][]Target
}

// knownProviders are the upstreams the gateway can talk to today. A provider is
// enabled only when its API key is set.
var knownProviders = []struct {
	name, keyEnv, baseURL string
}{
	{"openai", "OPENAI_API_KEY", "https://api.openai.com/v1"},
	{"groq", "GROQ_API_KEY", "https://api.groq.com/openai/v1"},
	{"gemini", "GEMINI_API_KEY", "https://generativelanguage.googleapis.com/v1beta/openai"},
}

// Load reads config from the environment. getenv is injected so tests don't
// depend on the real environment.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Addr:      getenv("GATEWAY_ADDR"),
		Providers: map[string]Provider{},
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}

	for _, p := range knownProviders {
		key := getenv(p.keyEnv)
		if key == "" {
			continue
		}
		// <NAME>_BASE_URL lets you point a provider at a mock server or a proxy.
		base := getenv(strings.ToUpper(p.name) + "_BASE_URL")
		if base == "" {
			base = p.baseURL
		}
		cfg.Providers[p.name] = Provider{Name: p.name, BaseURL: strings.TrimRight(base, "/"), APIKey: key}
	}

	if len(cfg.Providers) == 0 {
		return Config{}, fmt.Errorf("no providers configured: set at least one of OPENAI_API_KEY, GROQ_API_KEY, GEMINI_API_KEY")
	}

	routes, err := parseRoutes(getenv("GATEWAY_ROUTES"), cfg.Providers)
	if err != nil {
		return Config{}, err
	}
	cfg.Routes = routes
	return cfg, nil
}

// ParseTarget splits "provider/model" into its parts.
func ParseTarget(s string) (Target, bool) {
	provider, model, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok || provider == "" || model == "" {
		return Target{}, false
	}
	return Target{Provider: provider, Model: model}, true
}

// parseRoutes reads routes written as
//
//	fast=groq/llama-3.1-8b-instant,gemini/gemini-2.0-flash;smart=openai/gpt-4o
//
// Every target must name a configured provider, so typos fail at startup
// rather than on the first request.
func parseRoutes(spec string, providers map[string]Provider) (map[string][]Target, error) {
	routes := map[string][]Target{}
	for _, def := range strings.Split(spec, ";") {
		if strings.TrimSpace(def) == "" {
			continue
		}
		name, list, ok := strings.Cut(def, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("GATEWAY_ROUTES: %q should look like name=provider/model,provider/model", def)
		}
		for _, item := range strings.Split(list, ",") {
			t, ok := ParseTarget(item)
			if !ok {
				return nil, fmt.Errorf("GATEWAY_ROUTES: route %q has bad target %q, want provider/model", name, item)
			}
			if _, ok := providers[t.Provider]; !ok {
				return nil, fmt.Errorf("GATEWAY_ROUTES: route %q uses provider %q, which has no API key set", name, t.Provider)
			}
			routes[name] = append(routes[name], t)
		}
	}
	return routes, nil
}

// FromEnv is Load using the process environment.
func FromEnv() (Config, error) { return Load(os.Getenv) }
