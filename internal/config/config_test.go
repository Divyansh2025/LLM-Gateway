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
