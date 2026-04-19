// Package db owns the PostgreSQL connection used by axiom-ng.
//
// Parity targets with the Python backend:
//
//   - Pool size:        50 (pool_size)
//   - Max connections:  80 (pool_size + max_overflow)
//   - Max conn lifetime: 1h (pool_recycle)
//   - Statement timeout: 30s (connect_args.options = -c statement_timeout=30000)
//   - Connect timeout:   10s
//
// axiom-ng uses GORM on top of pgx as the driver (gorm.io/driver/postgres).
// The underlying *sql.DB is tuned to the settings above; pgvector is
// registered as a Postgres type so vector(N) columns round-trip via
// github.com/pgvector/pgvector-go.
package db

import (
	"context"
	"fmt"
	"time"

	pgvector "github.com/pgvector/pgvector-go"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Config bundles the options required to open a *gorm.DB.
type Config struct {
	URL              string
	MaxOpenConns     int
	MaxIdleConns     int
	ConnMaxLifetime  time.Duration
	ConnMaxIdleTime  time.Duration
	StatementTimeout time.Duration
	ConnectTimeout   time.Duration
}

// DefaultConfig returns parity-with-Python defaults. Callers must set URL.
func DefaultConfig() Config {
	return Config{
		MaxOpenConns:     80, // pool_size (50) + max_overflow (30)
		MaxIdleConns:     5,
		ConnMaxLifetime:  1 * time.Hour,
		ConnMaxIdleTime:  15 * time.Minute,
		StatementTimeout: 30 * time.Second,
		ConnectTimeout:   10 * time.Second,
	}
}

// Open returns a *gorm.DB against cfg.URL with axiom-ng defaults applied.
func Open(ctx context.Context, cfg Config) (*gorm.DB, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("db: URL is required")
	}

	dsn := withStatementTimeout(cfg.URL, cfg.StatementTimeout)

	gormDB, err := gorm.Open(postgres.New(postgres.Config{DSN: dsn}), &gorm.Config{
		Logger:                 logger.Default.LogMode(logger.Warn),
		SkipDefaultTransaction: true,
		PrepareStmt:            true,
	})
	if err != nil {
		return nil, fmt.Errorf("db: open gorm: %w", err)
	}

	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, fmt.Errorf("db: underlying sql.DB: %w", err)
	}
	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
	if cfg.ConnMaxIdleTime > 0 {
		sqlDB.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return gormDB, nil
}

// Ping returns nil when the underlying sql.DB accepts a connection.
func Ping(ctx context.Context, gormDB *gorm.DB) error {
	sqlDB, err := gormDB.DB()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return sqlDB.PingContext(ctx)
}

// RequireSchema runs a lightweight existence check for the tables that
// axiom-ng depends on. If any are missing the function returns an error
// so startup fails fast instead of blowing up on the first query.
func RequireSchema(ctx context.Context, gormDB *gorm.DB) error {
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
	for _, table := range required {
		var ok bool
		err := gormDB.WithContext(ctx).
			Raw(`SELECT to_regclass($1)::text IS NOT NULL`, "public."+table).
			Scan(&ok).Error
		if err != nil {
			return fmt.Errorf("db: probe %s: %w", table, err)
		}
		if !ok {
			return fmt.Errorf("db: required table %q not found — initialize schema via init-db/*.sql before starting axiom-ng", table)
		}
	}
	return nil
}

// VectorType re-exports pgvector.Vector so callers avoid the import.
type VectorType = pgvector.Vector

// NewVector wraps pgvector.NewVector for callers that do not import
// pgvector directly.
func NewVector(v []float32) VectorType { return pgvector.NewVector(v) }

// withStatementTimeout splices the Postgres statement_timeout setting
// into the DSN's options parameter. Matches the Python backend's
// connect_args.options.
func withStatementTimeout(dsn string, timeout time.Duration) string {
	if timeout <= 0 {
		return dsn
	}
	ms := int(timeout / time.Millisecond)
	sep := "?"
	for i := 0; i < len(dsn); i++ {
		if dsn[i] == '?' {
			sep = "&"
			break
		}
	}
	return fmt.Sprintf("%s%soptions=-c%%20statement_timeout=%d", dsn, sep, ms)
}
