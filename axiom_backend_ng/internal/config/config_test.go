package config

import (
	"testing"
)

func TestDefaults(t *testing.T) {
	t.Parallel()
	d := Defaults()
	if d.Port != 8010 {
		t.Errorf("default port: got %d, want 8010", d.Port)
	}
	if d.LogLevel != "info" {
		t.Errorf("default log level: got %q, want %q", d.LogLevel, "info")
	}
}

func TestLoadUsesDefaultsWhenNoEnvSet(t *testing.T) {
	t.Parallel()
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 8010 {
		t.Errorf("port: got %d, want 8010 (default)", cfg.Port)
	}
}

func TestLoadOverridesFromEnv(t *testing.T) {
	t.Setenv("AXIOM_NG_PORT", "9999")
	t.Setenv("AXIOM_NG_LOG_LEVEL", "debug")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 9999 {
		t.Errorf("port: got %d, want 9999", cfg.Port)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("log level: got %q, want %q", cfg.LogLevel, "debug")
	}
}
