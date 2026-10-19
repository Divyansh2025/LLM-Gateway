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

// Config is everything the gateway needs to start.
type Config struct {
	Addr      string
	Providers map[string]Provider
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
	return cfg, nil
}

// FromEnv is Load using the process environment.
func FromEnv() (Config, error) { return Load(os.Getenv) }
