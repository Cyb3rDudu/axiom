// Package testutil provides shared testing helpers for axiom-ng.
package testutil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/db"
)

// Postgres is a running pgvector-enabled Postgres container for tests.
type Postgres struct {
	Pool   *pgxpool.Pool
	URL    string
	Resource *dockertest.Resource

	pool *dockertest.Pool
}

// StartPostgres launches pgvector/pgvector:pg15, applies the repository's
// init-db/*.sql in lexical order, and returns a ready-to-use pool. Callers
// should defer Postgres.Close.
//
// Tests are skipped (not failed) when the Docker daemon is unreachable, so
// contributors without Docker can still run the rest of the suite. CI must
// enforce Docker availability.
func StartPostgres(t *testing.T) *Postgres {
	t.Helper()
	dockerPool, err := dockertest.NewPool("")
	if err != nil {
		t.Skipf("dockertest: cannot connect to Docker daemon: %v", err)
	}
	if err := dockerPool.Client.Ping(); err != nil {
		t.Skipf("dockertest: Docker daemon not reachable: %v", err)
	}

	dockerPool.MaxWait = 60 * time.Second

	const (
		user = "axiom_test"
		pass = "axiom_test"
		name = "axiom_test"
	)
	resource, err := dockerPool.RunWithOptions(&dockertest.RunOptions{
		Repository: "pgvector/pgvector",
		Tag:        "pg15",
		Env: []string{
			"POSTGRES_USER=" + user,
			"POSTGRES_PASSWORD=" + pass,
			"POSTGRES_DB=" + name,
		},
		Cmd: []string{"postgres", "-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off"},
	}, func(c *docker.HostConfig) {
		c.AutoRemove = true
		c.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		t.Fatalf("dockertest: run pgvector container: %v", err)
	}

	port := resource.GetPort("5432/tcp")
	url := fmt.Sprintf("postgres://%s:%s@127.0.0.1:%s/%s?sslmode=disable", user, pass, port, name)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var pool *pgxpool.Pool
	if err := dockerPool.Retry(func() error {
		p, perr := pgxpool.New(ctx, url)
		if perr != nil {
			return perr
		}
		if perr := p.Ping(ctx); perr != nil {
			p.Close()
			return perr
		}
		pool = p
		return nil
	}); err != nil {
		_ = dockerPool.Purge(resource)
		t.Fatalf("dockertest: wait for pgvector to become ready: %v", err)
	}
	pool.Close()

	if err := applyInitSchema(ctx, url); err != nil {
		_ = dockerPool.Purge(resource)
		t.Fatalf("dockertest: apply init schema: %v", err)
	}

	cfg := db.DefaultConfig()
	cfg.URL = url
	pgxPool, err := db.NewPool(ctx, cfg)
	if err != nil {
		_ = dockerPool.Purge(resource)
		t.Fatalf("dockertest: open axiom pool: %v", err)
	}

	return &Postgres{Pool: pgxPool, URL: url, Resource: resource, pool: dockerPool}
}

// Close tears down the pool and container.
func (p *Postgres) Close() {
	if p == nil {
		return
	}
	if p.Pool != nil {
		p.Pool.Close()
	}
	if p.pool != nil && p.Resource != nil {
		_ = p.pool.Purge(p.Resource)
	}
}

// Reset truncates every non-catalog table so successive test cases start
// from a clean slate without paying container-startup cost.
func (p *Postgres) Reset(ctx context.Context, t *testing.T) {
	t.Helper()
	const q = `
		SELECT string_agg(format('%I', tablename), ',')
		FROM pg_tables
		WHERE schemaname = 'public'
	`
	var tables *string
	if err := p.Pool.QueryRow(ctx, q).Scan(&tables); err != nil {
		t.Fatalf("pgtest: collect tables: %v", err)
	}
	if tables == nil || *tables == "" {
		return
	}
	_, err := p.Pool.Exec(ctx, "TRUNCATE "+*tables+" RESTART IDENTITY CASCADE")
	if err != nil {
		t.Fatalf("pgtest: truncate: %v", err)
	}
}

// applyInitSchema runs every SQL file under <repoRoot>/init-db/ in lexical
// order against the freshly-started container.
func applyInitSchema(ctx context.Context, url string) error {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer p.Close()

	root, err := repoRoot()
	if err != nil {
		return err
	}
	// The full axiom schema is spread across three directories today. In
	// production the Postgres container runs docker-entrypoint-initdb.d
	// against the first, then the Python backend's init_postgres.py layers
	// the rest on top at startup. We reproduce that sequence here so
	// tests see the same schema the Python backend produces.
	dirs := []string{
		filepath.Join(root, "init-db"),
		filepath.Join(root, "axiom_backend", "init-db"),
		filepath.Join(root, "axiom_backend", "database", "migrations"),
	}
	for _, dir := range dirs {
		files, err := collectSQL(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				return fmt.Errorf("read %s: %w", f, err)
			}
			if _, err := p.Exec(ctx, string(b)); err != nil {
				return fmt.Errorf("exec %s: %w", filepath.Base(f), err)
			}
		}
	}
	// Replicate axiom_backend/database/init_postgres.py:run_column_migrations().
	// api_key is added as a runtime column migration in Python; axiom-ng
	// expects it in place.
	if _, err := p.Exec(ctx, `ALTER TABLE users ADD COLUMN IF NOT EXISTS api_key VARCHAR UNIQUE`); err != nil {
		return fmt.Errorf("column migration users.api_key: %w", err)
	}
	return nil
}

func repoRoot() (string, error) {
	// Walk upward from this file looking for a directory that holds both
	// `init-db/` and `axiom_backend_ng/`. That uniquely identifies the repo.
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	cur := wd
	for {
		a, _ := os.Stat(filepath.Join(cur, "init-db"))
		b, _ := os.Stat(filepath.Join(cur, "axiom_backend_ng"))
		if a != nil && a.IsDir() && b != nil && b.IsDir() {
			return cur, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("repo root not found (starting from %s)", wd)
		}
		cur = parent
	}
}

func collectSQL(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}
