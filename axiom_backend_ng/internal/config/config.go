// Package config loads axiom-ng runtime configuration from env vars
// (with optional YAML file override) using koanf.
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// defaultGetenv is the indirection point for osLookupEnv.
var defaultGetenv = os.Getenv

// Config is the full axiom-ng runtime configuration. Fields will grow as the
// migration proceeds; bootstrap only needs Port and LogLevel.
type Config struct {
	Port            int    `koanf:"port"`
	LogLevel        string `koanf:"log_level"`
	DatabaseURL     string `koanf:"database_url"`
	GPUWorkerSocket string `koanf:"gpu_worker_socket"`
	OpenSearchURL   string `koanf:"opensearch_url"`
	// RawFilesDir is where axiom-ng persists uploaded documents.
	// Matches the Python backend's RAW_FILES_PATH.
	RawFilesDir string `koanf:"raw_files_dir"`
}

// Defaults returns the config populated with bootstrap defaults.
func Defaults() Config {
	return Config{
		Port:     8010,
		LogLevel: "info",
	}
}

// Load reads configuration, in order of increasing precedence:
//  1. Defaults
//  2. YAML file at configPath (if non-empty and readable)
//  3. Environment variables prefixed with AXIOM_NG_
//
// AXIOM_NG_PORT → Port, AXIOM_NG_LOG_LEVEL → LogLevel, etc.
func Load(configPath string) (Config, error) {
	k := koanf.New(".")
	cfg := Defaults()
	defaults := map[string]interface{}{
		"port":              cfg.Port,
		"log_level":         cfg.LogLevel,
		"database_url":      cfg.DatabaseURL,
		"gpu_worker_socket": cfg.GPUWorkerSocket,
		"opensearch_url":    cfg.OpenSearchURL,
		"raw_files_dir":     cfg.RawFilesDir,
	}
	// confmap.Provider.Read cannot fail on a plain map literal, so the only
	// error path through koanf here is an internal impossibility; we ignore
	// it on purpose and have a unit test guarding the resulting defaults.
	_ = k.Load(confmap.Provider(defaults, "."), nil)

	if configPath != "" {
		if err := k.Load(file.Provider(configPath), yaml.Parser()); err != nil {
			return Config{}, fmt.Errorf("load config file %q: %w", configPath, err)
		}
	}

	envProvider := env.Provider("AXIOM_NG_", ".", func(s string) string {
		return strings.ToLower(strings.TrimPrefix(s, "AXIOM_NG_"))
	})
	// env.Provider.Read iterates os.Environ() in-memory and never fails.
	_ = k.Load(envProvider, nil)

	var out Config
	if err := k.Unmarshal("", &out); err != nil {
		return Config{}, fmt.Errorf("unmarshal config: %w", err)
	}

	// Compatibility with Python deployments that set DATABASE_URL (no
	// prefix). AXIOM_NG_DATABASE_URL wins when both are set.
	if out.DatabaseURL == "" {
		if v := osLookupEnv("DATABASE_URL"); v != "" {
			out.DatabaseURL = v
		}
	}
	return out, nil
}

// osLookupEnv is a seam so tests can stub os.Getenv deterministically.
var osLookupEnv = func(k string) string { return defaultGetenv(k) }
