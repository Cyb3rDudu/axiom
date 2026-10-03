// Package sqlite is the Library component's SQLite repository engine
// (F12, #306) — the second implementation of library.Repository (the
// PostgreSQL twin lives in internal/library/pglib). Same contract, same
// repository contract suite, one file: library.sqlite.
//
// # Operating rules (startup-asserted, test-pinned)
//
//   - journal_mode=WAL, foreign_keys=ON, busy_timeout=5s — asserted on
//     BOTH handles at Open; a pragma that did not take is a loud start
//     failure, never a silent degradation.
//   - Restrictive file permissions: the file is claimed with an
//     exclusive O_EXCL create (atomic — never a half-visible or
//     clobbered database; a concurrent creator's file is adopted) at
//     0600, the parent directory 0700 when this code has to create it.
//   - Single writer: every mutating statement runs on the write handle
//     (BEGIN IMMEDIATE via the driver's _txlock, MaxOpenConns=1). The
//     per-import advisory locks of the PostgreSQL engine map onto the
//     IMMEDIATE transaction — the single writer slot IS the lock.
//   - Contention latency ceiling: a competing writer parks inside the
//     engine's busy handler for up to busy_timeout (5s). A cancelled
//     context does NOT interrupt the park (SQLite's busy handler is not
//     ctx-aware) — callers that must bound latency cancel ABOVE the
//     repository or accept the 5s ceiling.
//   - NO ATTACH, no cross-component queries: the file carries the
//     library_* namespace only. The Store's tables are a different
//     database; the Zotero-mirror strangler reads answer absence.
//
// # Operating boundary (honest, documented)
//
// Single-host profile: the WAL lives next to the file, ownership is one
// host's process set (the writer lease guards the Library's single
// writer across them). Multi-replica or network-filesystem deployments
// need the PostgreSQL profile — SQLite on NFS is corruption, and two
// hosts cannot share one file lock domain. config.sqlite (F13) is a
// SEPARATE file: runtime configuration only, never Fachdaten.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/library"
	"modernc.org/sqlite"
)

// busyTimeout is the SQLite-side wait a second writer (another process
// inside the single-host boundary) parks in before erroring — the
// analog of a pool waiting on a PostgreSQL lock.
const busyTimeout = 5 * time.Second

// schemaFS embeds the ordered SQLite-dialect Library migrations.
//
//go:embed schema/*.sql
var schemaFS embed.FS

// Repo is the Library SQLite repository (a library.Repository).
type Repo struct {
	write *sql.DB // _txlock=immediate, one connection — the single writer slot
	read  *sql.DB // WAL reader
}

// compile-time interface conformance — the SQLite engine implements the
// WHOLE neutral contract.
var _ library.Repository = (*Repo)(nil)

// Close releases both handles.
func (r *Repo) Close() error {
	return errors.Join(r.write.Close(), r.read.Close())
}

// dsn builds the modernc DSN: pragmas ride the DSN so EVERY pooled
// connection carries them (per-connection state in SQLite); _txlock
// makes every BEGIN on the write handle an IMMEDIATE one.
func dsn(path string, write bool) string {
	d := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)",
		path, busyTimeout.Milliseconds())
	if write {
		d += "&_txlock=immediate"
	}
	return d
}

// Open atomically creates (exclusive O_EXCL create, 0600) or opens the
// Library's SQLite file, asserts the operating pragmas on both handles,
// and applies the embedded migrations (own ledger). A second call against
// the same file is a normal open (idempotent).
func Open(ctx context.Context, path string) (*Repo, error) {
	if path == "" {
		return nil, fmt.Errorf("library sqlite: path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("library sqlite: %w", err)
	}
	if strings.ContainsAny(abs, "?#%") {
		return nil, fmt.Errorf("library sqlite: path %q contains URL-significant characters (?/#/%%) that would corrupt or redirect the file DSN", abs)
	}
	if err := ensureFile(abs); err != nil {
		return nil, fmt.Errorf("library sqlite: %w", err)
	}
	w, err := sql.Open("sqlite", dsn(abs, true))
	if err != nil {
		return nil, fmt.Errorf("library sqlite: open write handle: %w", err)
	}
	w.SetMaxOpenConns(1) // the single writer slot — no intra-process contention
	rd, err := sql.Open("sqlite", dsn(abs, false))
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("library sqlite: open read handle: %w", err)
	}
	r := &Repo{write: w, read: rd}
	if err := r.assertPragmas(ctx); err != nil {
		_errors := r.Close()
		return nil, errors.Join(fmt.Errorf("library sqlite: pragma assert: %w", err), _errors)
	}
	if err := r.migrate(ctx); err != nil {
		_errors := r.Close()
		return nil, errors.Join(fmt.Errorf("library sqlite: migrate: %w", err), _errors)
	}
	return r, nil
}

// ensureFile creates the database file atomically when absent: an
// exclusive O_CREATE|O_EXCL claim — the file either did not exist (we
// own it, 0600) or already exists (the race's winner or the operator's
// file — adopt it). rename(2) was rejected here: it silently REPLACES a
// concurrently-created, already-migrated database (the loser's empty
// file wins, the winner keeps writing to an unlinked inode — silent
// loss, the lease guard bypassed). An existing file is left untouched
// (its permissions are the operator's statement, not ours to rewrite).
func ensureFile(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil // a concurrent creator won — adopt its file
		}
		return fmt.Errorf("atomic create: %w", err)
	}
	return f.Close()
}

// assertPragmas proves the operating rules took effect on BOTH handles:
// WAL journal, enforced foreign keys, the busy timeout. Read back from
// the engine's own state — never trust the DSN string alone.
func (r *Repo) assertPragmas(ctx context.Context) error {
	for _, h := range []*sql.DB{r.write, r.read} {
		var mode string
		var fk, busy int
		if err := h.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
			return fmt.Errorf("journal_mode: %w", err)
		}
		if err := h.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
			return fmt.Errorf("foreign_keys: %w", err)
		}
		if err := h.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
			return fmt.Errorf("busy_timeout: %w", err)
		}
		if mode != "wal" {
			return fmt.Errorf("journal_mode is %q, want wal", mode)
		}
		if fk != 1 {
			return fmt.Errorf("foreign_keys is %d, want 1", fk)
		}
		if time.Duration(busy)*time.Millisecond < busyTimeout {
			return fmt.Errorf("busy_timeout is %dms, want >= %dms", busy, busyTimeout.Milliseconds())
		}
	}
	return nil
}

// migrate applies every embedded migration not yet in the ledger. Each
// migration runs in its own transaction on the write handle.
//
// Concurrent-first-open behavior (single-host boundary, accepted): two
// processes racing the VERY first open of a fresh file can both miss a
// ledger row and both attempt the insert; the loser fails LOUDLY (a
// constraint error from the ledger write, or a pragma/lock assert while
// the winner still migrates) and its retry Open succeeds idempotently —
// loud-and-retryable beats silent interleaving.
func (r *Repo) migrate(ctx context.Context) error {
	if _, err := r.write.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS library_schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return err
	}
	names, err := fs.Glob(schemaFS, "schema/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var exists bool
		if err := r.read.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM library_schema_migrations WHERE version = ?)`, name,
		).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sqlText, err := schemaFS.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := r.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(sqlText)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO library_schema_migrations (version, applied_at) VALUES (?, ?)`,
			name, formatTS(time.Now())); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers

// tsLayout is the fixed DM03-compatible timestamp form: UTC,
// microsecond-aligned, zero-padded — fixed width, so TEXT comparison is
// chronological comparison.
const tsLayout = "2006-01-02T15:04:05.000000Z"

func formatTS(t time.Time) string { return t.UTC().Truncate(time.Microsecond).Format(tsLayout) }

func parseTS(s string) (time.Time, error) {
	t, err := time.Parse(tsLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("sqlite timestamp %q: %w", s, err)
	}
	return t, nil
}

// absent translates the driver's no-rows signal into the neutral
// library.ErrRowAbsent.
func absent(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return library.ErrRowAbsent
	}
	return err
}

// duplicate reports a SQLite UNIQUE violation — extended code 2067
// (SQLITE_CONSTRAINT_UNIQUE; modernc enables extended result codes on
// every connection) — as the neutral library.ErrDuplicateKey. The wider
// constraint family (19/NOT NULL/CHECK/FK) is NOT a duplicate race and
// must not masquerade as one.
func duplicate(err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code() == 2067 {
		return fmt.Errorf("%w: %v", library.ErrDuplicateKey, err)
	}
	return err
}

// newUUID mints a canonical lowercase RFC 4122 v4 uuid (the PG engine's
// gen_random_uuid() twin — app-minted here because SQLite has none).
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("library sqlite: mint import id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var s [36]byte
	hex.Encode(s[0:8], b[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], b[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], b[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], b[8:10])
	s[23] = '-'
	hex.Encode(s[24:36], b[10:16])
	return string(s[:]), nil
}
