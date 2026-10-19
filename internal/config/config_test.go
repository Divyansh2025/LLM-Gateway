package config

import "testing"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadEnablesOnlyProvidersWithKeys(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"GROQ_API_KEY":  "g",
		"GROQ_BASE_URL": "http://localhost:9999/v1/",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("default addr = %q", cfg.Addr)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("providers = %v, want only groq", cfg.Providers)
	}
	g := cfg.Providers["groq"]
	if g.APIKey != "g" || g.BaseURL != "http://localhost:9999/v1" {
		t.Errorf("groq = %+v", g)
	}
}

func TestLoadFailsWithNoProviders(t *testing.T) {
	if _, err := Load(env(nil)); err == nil {
		t.Error("want error when no API keys are set")
	}
}

func TestLoadParsesRoutes(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"GROQ_API_KEY":   "g",
		"GEMINI_API_KEY": "m",
		"GATEWAY_ROUTES": "fast = groq/llama-3.1-8b-instant, gemini/gemini-2.0-flash ; cheap=gemini/gemini-2.0-flash-lite",
	}))
	if err != nil {
		t.Fatal(err)
	}
	fast := cfg.Routes["fast"]
	if len(fast) != 2 || fast[0] != (Target{"groq", "llama-3.1-8b-instant"}) || fast[1] != (Target{"gemini", "gemini-2.0-flash"}) {
		t.Errorf("fast = %+v", fast)
	}
	if len(cfg.Routes["cheap"]) != 1 {
		t.Errorf("cheap = %+v", cfg.Routes["cheap"])
	}
}

func TestLoadRejectsBadRoutes(t *testing.T) {
	cases := map[string]string{
		"no equals":             "fast",
		"bad target":            "fast=llama",
		"unconfigured provider": "fast=openai/gpt-4o",
		"slash in name":         "a/b=groq/m",
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(map[string]string{"GROQ_API_KEY": "g", "GATEWAY_ROUTES": spec}))
			if err == nil {
				t.Errorf("want error for %q", spec)
			}
		})
	}
}
