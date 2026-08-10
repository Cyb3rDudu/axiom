package server

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DBHealthChecker reports Postgres reachability for /api/health.
type DBHealthChecker struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

// CheckDB wraps a pgx pool as a health checker.
func CheckDB(pool *pgxpool.Pool) *DBHealthChecker {
	return &DBHealthChecker{pool: pool, timeout: 2 * time.Second}
}

// Ready pings the database.
func (c *DBHealthChecker) Ready() error {
	if c == nil || c.pool == nil {
		return nil // db not configured yet; treat as ok to keep health informative
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	return c.pool.Ping(ctx)
}
