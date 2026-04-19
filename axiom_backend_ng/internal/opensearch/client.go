// Package opensearch owns the BM25 fulltext search layer for axiom-ng.
//
// The Python backend stores one chunk per document segment in a shared
// index (default `axiom_chunks`) with a fixed set of `_source` fields.
// axiom-ng preserves the exact query shape so the frontend's document
// search page sees byte-identical responses regardless of which
// backend served the request.
//
// Parity references:
//   - axiom_backend/ai_researcher/core_rag/opensearch_store.py
//   - axiom_backend/api/documents.py (search/fulltext handler)
package opensearch

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	opensearchgo "github.com/opensearch-project/opensearch-go/v4"
	opensearchapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// Config captures the deploy-time settings for OpenSearch access.
// Mirrors axiom_backend/ai_researcher/config.py:583-589.
type Config struct {
	Enabled  bool
	Host     string
	Port     int
	UseSSL   bool
	Username string
	Password string
	Index    string
}

// DefaultConfig returns parity defaults — matches the Python backend's
// config.py values. Callers still need to read env vars for Host / Port
// / credentials.
func DefaultConfig() Config {
	return Config{
		Enabled: true,
		Host:    "localhost",
		Port:    9200,
		Index:   "axiom_chunks",
	}
}

// FromEnv builds a Config from the same env vars the Python backend
// reads. lookup may be nil (falls back to os.Getenv).
func FromEnv(lookup func(string) string) Config {
	cfg := DefaultConfig()
	if v := readEnv(lookup, "ENABLE_OPENSEARCH"); v != "" {
		// Python treats any value other than literal 'false' (case
		// insensitive) as enabled; match that.
		cfg.Enabled = !strings.EqualFold(v, "false")
	}
	if v := readEnv(lookup, "OPENSEARCH_HOST"); v != "" {
		cfg.Host = v
	}
	if v := readEnv(lookup, "OPENSEARCH_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Port = n
		}
	}
	if v := readEnv(lookup, "OPENSEARCH_USE_SSL"); v != "" {
		cfg.UseSSL = strings.EqualFold(v, "true")
	}
	if v := readEnv(lookup, "OPENSEARCH_USERNAME"); v != "" {
		cfg.Username = v
	}
	if v := readEnv(lookup, "OPENSEARCH_PASSWORD"); v != "" {
		cfg.Password = v
	}
	if v := readEnv(lookup, "OPENSEARCH_INDEX"); v != "" {
		cfg.Index = v
	}
	return cfg
}

func readEnv(lookup func(string) string, key string) string {
	if lookup == nil {
		return osGetenv(key)
	}
	return lookup(key)
}

// URL returns the computed base URL for the OpenSearch endpoint.
func (c Config) URL() string {
	scheme := "http"
	if c.UseSSL {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, c.Host, c.Port)
}

// Client wraps opensearch-go with the subset axiom-ng uses.
type Client struct {
	cfg Config
	os  *opensearchapi.Client
}

// ErrDisabled is returned by NewClient when Config.Enabled is false.
// Callers should treat this as "OpenSearch subsystem intentionally
// off" and return HTTP 503 from /api/documents/search/fulltext.
var ErrDisabled = errors.New("opensearch: subsystem disabled via ENABLE_OPENSEARCH=false")

// NewClient constructs a Client. Returns ErrDisabled when the
// subsystem is opted out.
func NewClient(cfg Config) (*Client, error) {
	if !cfg.Enabled {
		return nil, ErrDisabled
	}
	if cfg.Host == "" {
		return nil, errors.New("opensearch: host is required")
	}

	transport := &http.Transport{}
	if cfg.UseSSL {
		transport.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	osCfg := opensearchapi.Config{
		Client: opensearchgo.Config{
			Addresses: []string{cfg.URL()},
			Transport: transport,
		},
	}
	if cfg.Username != "" {
		osCfg.Client.Username = cfg.Username
		osCfg.Client.Password = cfg.Password
	}
	client, err := opensearchapi.NewClient(osCfg)
	if err != nil {
		return nil, fmt.Errorf("opensearch: new client: %w", err)
	}
	return &Client{cfg: cfg, os: client}, nil
}

// Config returns the deployed config (useful for tests and logging).
func (c *Client) Config() Config { return c.cfg }
