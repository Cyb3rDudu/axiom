package config

import (
	"os"
	"testing"
	"time"
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

func TestLoadReadsYAMLFile(t *testing.T) {
	t.Parallel()
	path := writeYAML(t, `
port: 7777
log_level: warn
database_url: postgres://yaml/test
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 7777 {
		t.Errorf("port: got %d, want 7777", cfg.Port)
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("log level: got %q", cfg.LogLevel)
	}
	if cfg.DatabaseURL != "postgres://yaml/test" {
		t.Errorf("database_url: got %q", cfg.DatabaseURL)
	}
}

func TestLoadEnvOverridesYAML(t *testing.T) {
	path := writeYAML(t, "port: 7777\n")
	t.Setenv("AXIOM_NG_PORT", "8888")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 8888 {
		t.Errorf("port: got %d, want 8888", cfg.Port)
	}
}

func TestLoadRejectsMissingConfigFile(t *testing.T) {
	t.Parallel()
	_, err := Load("/nonexistent/axiom-ng.yaml")
	if err == nil {
		t.Fatal("expected error for missing config file")
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	t.Parallel()
	path := writeYAML(t, "port: [unclosed\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for malformed YAML")
	}
}

func TestLoadRejectsTypeMismatchInConfig(t *testing.T) {
	t.Parallel()
	// port must be an integer; a string value should fail unmarshal.
	path := writeYAML(t, "port: not-a-number\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected unmarshal error for non-integer port")
	}
}

func TestLoadIngestFlagsFromEnv(t *testing.T) {
	t.Setenv("AXIOM_NG_INGEST_ENABLED", "true")
	t.Setenv("AXIOM_NG_INGEST_POOL_SIZE", "3")
	t.Setenv("AXIOM_NG_INGEST_POLL_INTERVAL", "250ms")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.IngestEnabled {
		t.Errorf("IngestEnabled: want true")
	}
	if cfg.IngestPoolSize != 3 {
		t.Errorf("IngestPoolSize: want 3, got %d", cfg.IngestPoolSize)
	}
	if cfg.IngestPollInterval != 250*time.Millisecond {
		t.Errorf("IngestPollInterval: want 250ms, got %s", cfg.IngestPollInterval)
	}
}

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "axiom-ng-*.yaml")
	if err != nil {
		t.Fatalf("tempfile: %v", err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = f.Close()
	return f.Name()
}
