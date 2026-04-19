package config

import "github.com/knadh/koanf/v2"

// structDefaultsProvider adapts a Config value into a koanf.Provider so
// defaults merge into the loader chain like any other source.
type structDefaultsProvider Config

func (p structDefaultsProvider) ReadBytes() ([]byte, error) {
	return nil, nil
}

func (p structDefaultsProvider) Read() (map[string]interface{}, error) {
	return map[string]interface{}{
		"port":              p.Port,
		"log_level":         p.LogLevel,
		"database_url":      p.DatabaseURL,
		"gpu_worker_socket": p.GPUWorkerSocket,
		"opensearch_url":    p.OpenSearchURL,
	}, nil
}

// compile-time check that structDefaultsProvider satisfies koanf.Provider.
var _ koanf.Provider = structDefaultsProvider{}
