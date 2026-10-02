// Package configstore is config.sqlite — the persistent runtime
// configuration store (F13, #307): a small SQLite file per host
// installation under the state root, read by the config resolution
// chain (flag > env > file > default) BEFORE the env overlay so a
// value set in the file applies exactly where the environment does not
// override it.
//
// # Model
//
//   - settings: non-secret overrides. The key vocabulary is the
//     canonical AXIOM_* environment names — one vocabulary across env,
//     file, and --set flags; values in their exact environment spelling.
//   - secret_refs: secret REFERENCES, never values. A row records that
//     a secret key's value is expected from the process environment;
//     the value itself stays in the OS secret store / env. Secret keys
//     are never writable into settings (the CLI refuses; the inspection
//     sonde proves zero secret bytes in the file).
//   - config_schema_migrations: the versioning ledger (F12 pattern —
//     applied migration files under schema/, ordered by name).
//
// # Operating rules (F12 standard, startup-asserted)
//
//   - journal_mode=WAL, foreign_keys=ON, busy_timeout=5s — asserted at
//     Open; a pragma that did not take is a loud failure, never a silent
//     degradation.
//   - Atomic creation: exclusive O_EXCL create at 0600 (never clobbers a
//     concurrent creator — adopt its file), parent dir 0700.
//   - Single handle, _txlock=immediate, MaxOpenConns=1: one writer slot
//     per process; cross-process writers park in the busy handler.
//   - Fachdaten NEVER: Read refuses a file carrying tables outside the
//     runtime-only vocabulary (CheckRuntimeOnly) — pointing config at a
//     library.sqlite, or smuggling domain tables into config.sqlite,
//     fails loudly instead of half-booting.
//
// # Boundary
//
// Pure runtime configuration. No domain (Library/Store) table ever lives
// here; env-only operation (no file) is the fully supported container
// path — Read treats an absent file as empty settings and creates
// nothing. Only this package and the config/cli layers above it touch
// the file (import-confinement lint in this package's tests).
package configstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// busyTimeout is the SQLite-side wait a competing writer (another
// process, e.g. `axiom config set` against a booting server) parks in
// before erroring.
const busyTimeout = 5 * time.Second

// schemaFS embeds the ordered SQLite-dialect config migrations.
//
//go:embed schema/*.sql
var schemaFS embed.FS

// runtimeTables is the CLOSED table vocabulary of config.sqlite — the
// Fachdaten-never rule's allowlist. sqlite internal tables (sqlite_*)
// are the engine's own and always tolerated.
var runtimeTables = map[string]bool{
	"settings":                 true,
	"secret_refs":              true,
	"config_schema_migrations": true,
}

// SecretRefSourceEnv is the one secret-ref source today: the value is
// expected from the process environment (the OS-secret-store path
// arrives with a later step; the reference, not the value, is what the
// file records). Shared with the config layer's validation so the
// vocabulary lives in exactly one place.
const SecretRefSourceEnv = "env"

// Settings is the file's complete content.
type Settings struct {
	// Values: non-secret overrides (key = canonical AXIOM_* name).
	Values map[string]string
	// SecretRefs: secret references — key -> source ("env").
	SecretRefs map[string]string
}

// Empty reports whether the settings carry no row at all.
func (s Settings) Empty() bool {
	return len(s.Values) == 0 && len(s.SecretRefs) == 0
}

// DefaultPath resolves the config.sqlite location: AXIOM_CONFIG_PATH
// (absolute file path) wins, the default is the state root
// ~/.axiom-ng/config.sqlite next to library.sqlite and the write key
// (one file per host installation).
func DefaultPath() (string, error) {
	if p := os.Getenv("AXIOM_CONFIG_PATH"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("config sqlite: no home dir for the default config.sqlite path — set AXIOM_CONFIG_PATH")
	}
	return home + "/.axiom-ng/config.sqlite", nil
}

// Read loads the settings from path. An ABSENT file is the documented
// env-only bootstrap: empty settings, found=false, no file created. A
// PRESENT file is read through the runtime-only table check — a file
// with foreign (domain) tables is refused loudly, never half-read.
func Read(path string) (Settings, bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return Settings{}, false, nil
		}
		return Settings{}, false, fmt.Errorf("config sqlite: %w", err)
	}
	abs, err := checkPath(path)
	if err != nil {
		return Settings{}, true, err
	}
	var s Settings
	err = withDB(abs, func(ctx context.Context, db *sql.DB) error {
		if err := checkRuntimeOnly(ctx, db); err != nil {
			return err
		}
		var err error
		s, err = readAll(ctx, db)
		return err
	})
	if err != nil {
		return Settings{}, true, fmt.Errorf("config sqlite: read %s: %w", path, err)
	}
	return s, true, nil
}

// Store is the write handle onto config.sqlite.
type Store struct {
	db *sql.DB
}

// Open atomically creates (exclusive O_EXCL claim, 0600) or adopts the
// config.sqlite file, switches the journal to WAL under busy handling,
// asserts the operating pragmas, and applies the embedded migrations.
// Idempotent: a second Open against the same file is a normal open.
// ADOPTED files are tightened to 0600 (a pre-existing weaker permission
// is below the file's contract). A setup failure never unlinks the
// file — between this process's create and its failure another process
// may have adopted and opened it, and unlinking would divert its writes
// to a dead inode; instead an abandoned file reads as UNINITIALIZED
// (see Read) and the next successful writer migrates it.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("config sqlite: path is required")
	}
	abs, err := checkPath(path)
	if err != nil {
		return nil, err
	}
	created, err := ensureFile(abs)
	if err != nil {
		return nil, fmt.Errorf("config sqlite: %w", err)
	}
	if !created { // adopted: enforce the restrictive permission
		if fi, serr := os.Stat(abs); serr == nil && fi.Mode().Perm() != 0o600 {
			_ = os.Chmod(abs, 0o600)
		}
	}
	db, err := sql.Open("sqlite", dsn(abs, true))
	if err != nil {
		return nil, fmt.Errorf("config sqlite: open: %w", err)
	}
	db.SetMaxOpenConns(1) // one writer slot — no intra-process contention
	st := &Store{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), busyTimeout)
	defer cancel()
	// The WAL switch runs POST-connect under explicit busy retry: as a
	// connection-opening DSN pragma it races parallel first-openers with
	// SQLITE_BUSY (the journal-mode change takes locks the busy handler
	// does not cover on that path — measured: parallel `config set`
	// processes failed 7/96 on a fresh file's first boot).
	if err := setWAL(ctx, db); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("config sqlite: journal_mode: %w", err)
	}
	if err := st.assertPragmas(ctx); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("config sqlite: pragma assert: %w", err)
	}
	if err := checkRuntimeOnly(ctx, st.db); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("config sqlite: %w", err)
	}
	if err := st.migrate(ctx); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("config sqlite: migrate: %w", err)
	}
	return st, nil
}

// Close releases the handle.
func (s *Store) Close() error { return s.db.Close() }

// Set writes (upserts) one non-secret override row.
func (s *Store) Set(key, value string) error {
	return s.SetAll(map[string]string{key: value}, nil)
}

// SetAll writes values and secret references in ONE transaction — a
// crash or failure mid-import never leaves a partial file state (the
// import either lands completely or not at all). Keys are written in
// sorted order for deterministic replay.
func (s *Store) SetAll(values, secretRefs map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit
	for _, key := range sortedKeysOf(values) {
		if key == "" {
			return fmt.Errorf("config sqlite: empty key")
		}
		if _, err := tx.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			key, values[key], dm03Now()); err != nil {
			return err
		}
	}
	for _, key := range sortedKeysOf(secretRefs) {
		if secretRefs[key] != SecretRefSourceEnv {
			return fmt.Errorf("config sqlite: unknown secret-ref source %q (known: %s)", secretRefs[key], SecretRefSourceEnv)
		}
		if _, err := tx.Exec(`INSERT INTO secret_refs (key, source, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET source = excluded.source, updated_at = excluded.updated_at`,
			key, secretRefs[key], dm03Now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// sortedKeysOf returns the map's keys in sorted order.
func sortedKeysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Unset removes one key from BOTH tables — an override row and a
// secret reference (absent key: no-op — unset is idempotent by
// contract) — in ONE transaction. Without the ref delete, `config
// unset` on an imported secret key would report success while leaving
// a reference that drifts the moment the env var clears.
func (s *Store) Unset(key string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit
	if _, err := tx.Exec(`DELETE FROM settings WHERE key = ?`, key); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM secret_refs WHERE key = ?`, key); err != nil {
		return err
	}
	return tx.Commit()
}

// SetSecretRef writes (upserts) one secret REFERENCE row — the source
// the value is expected from; the value itself never enters the file.
func (s *Store) SetSecretRef(key, source string) error {
	if source != SecretRefSourceEnv {
		return fmt.Errorf("config sqlite: unknown secret-ref source %q (known: %s)", source, SecretRefSourceEnv)
	}
	_, err := s.db.Exec(`INSERT INTO secret_refs (key, source, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET source = excluded.source, updated_at = excluded.updated_at`,
		key, source, dm03Now())
	return err
}

// checkRuntimeOnly enforces the Fachdaten-never rule: every table and
// VIEW in the file must be part of the runtime-only vocabulary (or the
// engine's own sqlite_* namespace) — a view over domain data smuggles
// the data just as well as a table. A foreign object — a Library or
// Store schema smuggled in, or config pointed at a domain database —
// is refused by name.
func checkRuntimeOnly(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type IN ('table', 'view')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var foreign []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if !runtimeTables[name] && !strings.HasPrefix(name, "sqlite_") {
			foreign = append(foreign, name)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(foreign) > 0 {
		sort.Strings(foreign)
		return fmt.Errorf("runtime-only rule violated: file carries domain/foreign tables %v — config.sqlite is runtime configuration, never Fachdaten (check AXIOM_CONFIG_PATH)", foreign)
	}
	return nil
}

// checkPath normalizes and guards the file path: URL-significant
// characters (?/#/%) would silently reinterpret the DSN (a "?" turns
// the rest into DSN parameters and the engine opens a DIFFERENT file).
// Both the read and the write path route through this guard.
func checkPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("config sqlite: %w", err)
	}
	if strings.ContainsAny(abs, "?#%") {
		return "", fmt.Errorf("config sqlite: path %q contains URL-significant characters (?/#/%%) that would corrupt the file DSN", abs)
	}
	return abs, nil
}

// dsn builds the modernc DSN: busy_timeout and foreign_keys ride the
// DSN so every connection carries them; _txlock makes every BEGIN on
// the write handle an IMMEDIATE one. journal_mode deliberately does
// NOT ride the DSN — the WAL switch takes locks the busy handler does
// not cover on the connection-open path (parallel first-openers fail
// with SQLITE_BUSY); it runs post-connect in setWAL under retry. The
// read DSN omits _txlock (one-shot autocommit reads need no write
// slot).
func dsn(path string, write bool) string {
	d := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)",
		path, busyTimeout.Milliseconds())
	if write {
		d += "&_txlock=immediate"
	}
	return d
}

// setWAL switches the journal to WAL, retrying on SQLITE_BUSY for up
// to busyTimeout — the measured parallel-first-boot failure mode. The
// pragma's result row IS the new mode; a non-busy failure or a mode
// that did not take is a loud error.
func setWAL(ctx context.Context, db *sql.DB) error {
	deadline := time.Now().Add(busyTimeout)
	for {
		var mode string
		err := db.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode)
		if err == nil && mode == "wal" {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("journal_mode is %q, want wal", mode)
		}
		if isBusy(err) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		return err
	}
}

// isBusy reports whether err is SQLITE_BUSY (primary code 5, extended
// codes included) — the retryable lock conflict.
func isBusy(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code()&0xff == 5 // SQLITE_BUSY primary code
	}
	return false
}

// ensureFile creates the file atomically when absent (F12 pattern):
// exclusive O_CREATE|O_EXCL claim — the file either did not exist (we
// own it, 0600, created=true) or already exists (adopt, created=false).
// rename(2) was rejected for the same reason as in F12: it silently
// replaces a concurrently created file. The created flag lets Open
// clean up ITS OWN failed creation without ever touching an adopted
// file.
func ensureFile(path string) (created bool, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("create dir %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return false, nil // concurrent creator won — adopt its file
		}
		return false, fmt.Errorf("atomic create: %w", err)
	}
	return true, f.Close()
}

// assertPragmas proves the operating rules took effect — read back from
// the engine's own state, never trusted from the DSN string alone. The
// journal mode is asserted by setWAL's own read-back; here it re-checks
// the persisted state so a flipped-back journal cannot pass silently.
func (s *Store) assertPragmas(ctx context.Context) error {
	var mode string
	var fk, busy int
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return fmt.Errorf("journal_mode: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		return fmt.Errorf("foreign_keys: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
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
	return nil
}

// migrate applies every embedded migration not yet in the ledger, each
// in its own transaction (F12 pattern).
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS config_schema_migrations (
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
		sqlText, err := schemaFS.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		// The exists-check rides INSIDE the write transaction: two
		// simultaneous first-boot writers cannot both apply a migration
		// (_txlock=immediate serializes them; the loser sees the winner's
		// ledger row and skips instead of dying on the UNIQUE violation).
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM config_schema_migrations WHERE version = ?)`, name,
		).Scan(&exists); err != nil {
			_ = tx.Rollback()
			return err
		}
		if exists {
			if err := tx.Commit(); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(string(sqlText)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO config_schema_migrations (version, applied_at) VALUES (?, ?)`,
			name, dm03Now()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// withDB opens the file (no create), runs fn on a short-lived handle,
// and always closes — the read path's one-shot shape.
func withDB(path string, fn func(context.Context, *sql.DB) error) error {
	db, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), busyTimeout)
	defer cancel()
	return fn(ctx, db)
}

// readAll reads both tables off an open handle. The UNINITIALIZED
// shape — no settings and no secret_refs table with an empty (or
// absent) migration ledger, the leftover of a crashed first write —
// reads as empty, env-equivalent: booting against it behaves like the
// env-only path, and the next writer migrates it. A file whose ledger
// claims applied migrations but whose runtime tables are gone is
// CORRUPT, not uninitialized: loud.
func readAll(ctx context.Context, db *sql.DB) (Settings, error) {
	s := Settings{Values: map[string]string{}, SecretRefs: map[string]string{}}
	var runtimeTables int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('settings', 'secret_refs')`).Scan(&runtimeTables); err != nil {
		return s, err
	}
	if runtimeTables == 0 {
		var ledgerExists bool
		if err := db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'config_schema_migrations')`).Scan(&ledgerExists); err != nil {
			return s, err
		}
		applied := 0
		if ledgerExists {
			if err := db.QueryRowContext(ctx,
				`SELECT count(*) FROM config_schema_migrations`).Scan(&applied); err != nil {
				return s, err
			}
		}
		if applied == 0 {
			return s, nil // uninitialized — reads empty, next writer migrates
		}
		return s, fmt.Errorf("settings/secret_refs missing but %d migrations applied — the file is corrupt, not uninitialized", applied)
	}
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return s, err
		}
		s.Values[k] = v
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return s, err
	}
	rows.Close()
	rows, err = db.QueryContext(ctx, `SELECT key, source FROM secret_refs`)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var k, src string
		if err := rows.Scan(&k, &src); err != nil {
			rows.Close()
			return s, err
		}
		s.SecretRefs[k] = src
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return s, err
	}
	rows.Close()
	return s, nil
}

// dm03Now renders the fixed UTC microsecond-aligned timestamp (the
// F12/DM03 form — lexicographic order equals chronological order).
func dm03Now() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
}
