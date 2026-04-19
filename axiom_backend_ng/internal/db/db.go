// Package db owns the PostgreSQL connection pool used by axiom-ng.
//
// Parity targets with the Python backend:
//
//   - Pool size:        50 (pool_size)
//   - Max connections:  80 (pool_size + max_overflow)
//   - Max conn lifetime: 1h (pool_recycle)
//   - Health check ping: before use (pool_pre_ping)
//   - Statement timeout: 30s (connect_args.options = -c statement_timeout=30000)
//   - Connect timeout:   10s
//
// pgvector is registered on every new connection so vector(N) columns
// round-trip through pgvector.Vector without manual scanning.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	pgxv5 "github.com/pgvector/pgvector-go/pgx"
)

// Config bundles the options required to open a pool.
type Config struct {
	URL              string
	MaxConns         int32
	MinConns         int32
	MaxConnLifetime  time.Duration
	MaxConnIdleTime  time.Duration
	StatementTimeout time.Duration
	ConnectTimeout   time.Duration
}

// DefaultConfig returns parity-with-Python defaults. Callers must set URL.
func DefaultConfig() Config {
	return Config{
		MaxConns:         80, // pool_size (50) + max_overflow (30)
		MinConns:         5,
		MaxConnLifetime:  1 * time.Hour,
		MaxConnIdleTime:  15 * time.Minute,
		StatementTimeout: 30 * time.Second,
		ConnectTimeout:   10 * time.Second,
	}
}

// NewPool opens a pgx pool against cfg.URL, applies the axiom-ng defaults,
// and registers pgvector on every new connection.
func NewPool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("db: URL is required")
	}

	pcfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("db: parse URL: %w", err)
	}

	if cfg.MaxConns > 0 {
		pcfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		pcfg.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		pcfg.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		pcfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	}
	if cfg.ConnectTimeout > 0 {
		pcfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	}

	// Match Python's per-connection `options = -c statement_timeout=30000`.
	// pgxpool.ParseConfig always initialises RuntimeParams so we can assign
	// directly without a nil guard.
	if stmt := int(cfg.StatementTimeout / time.Millisecond); stmt > 0 {
		pcfg.ConnConfig.RuntimeParams["statement_timeout"] = fmt.Sprintf("%d", stmt)
	}

	pcfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if err := pgxv5.RegisterTypes(ctx, conn); err != nil {
			return fmt.Errorf("db: register pgvector: %w", err)
		}
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("db: new pool: %w", err)
	}
	return pool, nil
}

// Ping verifies the database is reachable.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return pool.Ping(ctx)
}

// RequireSchema runs a lightweight existence check for the tables that
// axiom-ng depends on. If any are missing the function returns an error so
// startup fails fast instead of blowing up on the first query.
func RequireSchema(ctx context.Context, pool *pgxpool.Pool) error {
	required := []string{
		"users",
		"chats",
		"messages",
		"missions",
		"documents",
		"document_groups",
		"document_chunks",
		"writing_sessions",
		"drafts",
		"supported_languages",
		"system_settings",
	}
	const q = `SELECT to_regclass($1)::text IS NOT NULL`
	for _, table := range required {
		var ok bool
		if err := pool.QueryRow(ctx, q, "public."+table).Scan(&ok); err != nil {
			return fmt.Errorf("db: probe %s: %w", table, err)
		}
		if !ok {
			return fmt.Errorf("db: required table %q not found — initialize schema via init-db/*.sql before starting axiom-ng", table)
		}
	}
	return nil
}

// VectorType is a re-export so callers can avoid importing pgvector directly.
type VectorType = pgvector.Vector

// NewVector wraps pgvector.NewVector for callers that do not import pgvector
// directly.
func NewVector(v []float32) VectorType { return pgvector.NewVector(v) }
