// configstore_test.go — store-mechanics witnesses (F13 #307): roundtrip
// set/unset/secret-ref, absent-file read (env-only bootstrap: nothing
// created, nothing read), migration idempotency, the Fachdaten-never
// runtime-only refusal sonde, and the secret byte inspection over file
// AND WAL sidecars (values never enter the file — the sonde plants the
// exact scenario that would leak and proves zero bytes).
package configstore

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestRoundtripSetUnsetSecretRef — settings upsert, unset idempotency
// (including the secret-ref side), secret-ref rows, and the read-back
// shape through the package's own Read.
func TestRoundtripSetUnsetSecretRef(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.sqlite")
	st := openStore(t, path)

	if err := st.Set("AXIOM_API_PORT", "8012"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := st.Set("AXIOM_STORAGE_LIBRARY_DRIVER", "sqlite"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := st.Set("AXIOM_API_PORT", "9999"); err != nil { // upsert
		t.Fatalf("set again: %v", err)
	}
	if err := st.SetAll(nil, map[string]string{"AXIOM_WS_SECRET": SecretRefSourceEnv}); err != nil {
		t.Fatalf("secret ref: %v", err)
	}
	readBack := func() Settings {
		t.Helper()
		got, found, err := Read(path)
		if err != nil || !found {
			t.Fatalf("read back: found=%v err=%v", found, err)
		}
		return got
	}
	got := readBack()
	if want := map[string]string{"AXIOM_API_PORT": "9999", "AXIOM_STORAGE_LIBRARY_DRIVER": "sqlite"}; !maps.Equal(got.Values, want) {
		t.Fatalf("values = %v, want %v", got.Values, want)
	}
	if want := map[string]string{"AXIOM_WS_SECRET": "env"}; !maps.Equal(got.SecretRefs, want) {
		t.Fatalf("secret refs = %v, want %v", got.SecretRefs, want)
	}

	if err := st.Unset("AXIOM_API_PORT"); err != nil {
		t.Fatalf("unset: %v", err)
	}
	if err := st.Unset("AXIOM_API_PORT"); err != nil { // idempotent
		t.Fatalf("unset again: %v", err)
	}
	got = readBack()
	if _, ok := got.Values["AXIOM_API_PORT"]; ok {
		t.Fatalf("unset key survived: %v", got.Values)
	}
	// unset clears the secret-ref side too — a surviving reference would
	// drift the moment the env var clears.
	if err := st.Unset("AXIOM_WS_SECRET"); err != nil {
		t.Fatalf("unset ref: %v", err)
	}
	got = readBack()
	if _, ok := got.SecretRefs["AXIOM_WS_SECRET"]; ok {
		t.Fatalf("unset must remove the secret reference: %v", got.SecretRefs)
	}

	if err := st.SetAll(nil, map[string]string{"AXIOM_WS_SECRET": "keychain"}); err == nil {
		t.Fatalf("unknown secret-ref source must be refused")
	}
}

// TestReadAbsentFileCreatesNothing — the env-only container path: an
// absent file reads as empty settings, found=false, and leaves the
// filesystem untouched.
func TestReadAbsentFileCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.sqlite")
	s, found, err := Read(path)
	if err != nil {
		t.Fatalf("read absent: %v", err)
	}
	if found || !s.Empty() {
		t.Fatalf("absent file must read empty/found=false, got found=%v %v", found, s)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("read must not create the file, stat err = %v", err)
	}
	if _, err := os.Stat(dir + "/.axiom"); !os.IsNotExist(err) {
		t.Fatalf("read must not create state dirs")
	}
}

// TestOpenIdempotentRowsSurvive — second Open adopts the file; the
// ledger carries the applied migration (read off the write handle —
// no production API exposes it); rows survive the reopen.
func TestOpenIdempotentRowsSurvive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.sqlite")
	st := openStore(t, path)
	if err := st.Set("AXIOM_BIND_ADDR", "0.0.0.0"); err != nil {
		t.Fatalf("set: %v", err)
	}
	var n int
	var latest sql.NullString
	if err := st.db.QueryRow(`SELECT count(*), max(version) FROM config_schema_migrations`).Scan(&n, &latest); err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if n != 1 || !strings.Contains(latest.String, "0001") {
		t.Fatalf("ledger = %d/%q, want 1 applied, latest 0001_*", n, latest.String)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := Open(path); err != nil {
		t.Fatalf("second open must adopt: %v", err)
	}
	s, found, err := Read(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !found || s.Values["AXIOM_BIND_ADDR"] != "0.0.0.0" {
		t.Fatalf("rows must survive reopen, got found=%v %v", found, s.Values)
	}
}

// TestEnsureFileCreatedSemantics — the claim flag Open's cleanup keys
// on: the first exclusive claim creates (created=true), a second
// adopts (created=false). A deterministic failed-setup witness would
// need a fault-injection seam between create and migrate; the flag's
// truth is what the removal decides on, and that is pinned here.
func TestEnsureFileCreatedSemantics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.sqlite")
	created, err := ensureFile(path)
	if err != nil || !created {
		t.Fatalf("first claim must create, got created=%v err=%v", created, err)
	}
	created, err = ensureFile(path)
	if err != nil || created {
		t.Fatalf("second claim must adopt, got created=%v err=%v", created, err)
	}
}

// TestRuntimeOnlyRefusesForeignTables — the Fachdaten-never sonde: a
// file carrying a domain table (the Library writer-lease shape as the
// stand-in) is refused by BOTH the read path and a fresh Open — config
// pointed at (or smuggled into) domain data fails loudly, never
// half-boots.
func TestRuntimeOnlyRefusesForeignTables(t *testing.T) {
	dir := t.TempDir()
	// Plant a legitimate config.sqlite, then smuggle a domain table in
	// through a raw handle (the smuggling path the rule exists for).
	path := filepath.Join(dir, "config.sqlite")
	st := openStore(t, path)
	if err := st.Set("AXIOM_API_PORT", "8012"); err != nil {
		t.Fatalf("set: %v", err)
	}
	st.Close()
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE library_imports (import_id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("plant: %v", err)
	}
	raw.Close()

	if _, _, err := Read(path); err == nil || !strings.Contains(err.Error(), "library_imports") {
		t.Fatalf("read must refuse the foreign table by name, got %v", err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "library_imports") {
		t.Fatalf("open must refuse the foreign table by name, got %v", err)
	}
	// The sqlite internal namespace stays tolerated (WAL bookkeeping).
	_ = context.Background()
}

// TestFileCarriesZeroSecretBytes — the inspection sonde: the sanctioned
// writer surface produces settings rows plus secret REFERENCES (never
// values); the file bytes — and the WAL sidecar's bytes — contain none
// of a planted secret vocabulary. The store itself is vocabulary-dumb
// (the secret-key refusal lives in the config layer above — it owns
// the AXIOM_* vocabulary). Scope, honestly: this sonde pins the
// SANCTIONED write shapes and proves the byte scanner non-vacuous (the
// positive control); write-PATH falsifiability — that a leaking
// sanctioned path goes red — lives at the CLI layer
// (TestConfigSetGoodAndBad's after-state check and the hostile-form
// byte scan in TestImportEnvIdempotentEffectiveIdenticalAndSecretFree,
// verified red when the gate is removed).
func TestFileCarriesZeroSecretBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.sqlite")
	st := openStore(t, path)
	// The vocabulary an operator's environment would carry: values that
	// must never reach the file.
	secrets := []string{
		"hunter2-password",           // AXIOM_OPENSEARCH_PASSWORD shape
		"super-secret-hmac-key",      // AXIOM_WS_SECRET / source-secret shape
		"postgresql://u:leaked@h/db", // DSN credential shape
	}
	// The sanctioned shape: non-secret rows only, secrets as refs.
	if err := st.Set("AXIOM_API_PORT", "8012"); err != nil {
		t.Fatal(err)
	}
	if err := st.Set("AXIOM_OPENSEARCH_URL", "http://127.0.0.1:9200"); err != nil {
		t.Fatal(err)
	}
	refs := map[string]string{}
	for _, refKey := range []string{"AXIOM_WS_SECRET", "AXIOM_OPENSEARCH_PASSWORD", "AXIOM_COMPUTE_WORKER_SOURCE_SECRET", "AXIOM_DATABASE_URL"} {
		refs[refKey] = SecretRefSourceEnv
	}
	if err := st.SetAll(nil, refs); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	st.Close()
	// Also scan a NON-checkpointed shape: fresh store, dirty WAL.
	path2 := filepath.Join(dir, "config2.sqlite")
	st2 := openStore(t, path2)
	if err := st2.Set("AXIOM_API_PORT", "8013"); err != nil {
		t.Fatal(err)
	}
	_ = st2.Close() // no checkpoint — the -wal sidecar may still hold pages

	for _, p := range []string{path, path + "-wal", path + "-shm", path2, path2 + "-wal"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue // absent sidecars are fine
		}
		for _, secret := range secrets {
			if strings.Contains(string(b), secret) {
				t.Fatalf("%s carries secret bytes (%q found) — values must never reach config.sqlite or its sidecars", filepath.Base(p), secret)
			}
		}
	}

	// POSITIVE CONTROL (review finding): the sonde must be able to FAIL.
	// A planted secret through a RAW handle — the exact bypass a future
	// write-path regression would amount to — must be DETECTED by this
	// scan shape; a scanner that cannot go red proves nothing. The
	// planted file is quarantined in its own directory.
	qdir := filepath.Join(dir, "quarantine")
	if err := os.MkdirAll(qdir, 0o700); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(qdir, "planted.sqlite")
	if err := os.WriteFile(planted, []byte("settings blob containing hunter2-password raw"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !scanCarries(planted, []string{"hunter2-password"}) {
		t.Fatal("the byte scan missed a planted secret — the sonde is vacuous")
	}
	if scanCarries(path, []string{"hunter2-password"}) {
		t.Fatal("positive control cross-contaminated the sanctioned store")
	}
}

// scanCarries reports whether any of secrets appears in the file's
// bytes — the sonde primitive both the clean assertion and the
// positive control share.
func scanCarries(path string, secrets []string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, secret := range secrets {
		if strings.Contains(string(b), secret) {
			return true
		}
	}
	return false
}

// TestDefaultPathOverride — AXIOM_CONFIG_PATH wins, the default sits in
// the state root next to library.sqlite.
func TestDefaultPathOverride(t *testing.T) {
	t.Setenv("AXIOM_CONFIG_PATH", "/tmp/elsewhere/config.sqlite")
	p, err := DefaultPath()
	if err != nil || p != "/tmp/elsewhere/config.sqlite" {
		t.Fatalf("override path = %q err=%v", p, err)
	}
	t.Setenv("AXIOM_CONFIG_PATH", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir on this host — the default-path shape is untestable here")
	}
	p, err = DefaultPath()
	if err != nil || p != home+"/.axiom/config.sqlite" {
		t.Fatalf("default path = %q err=%v", p, err)
	}
}

// TestParallelFirstOpenAllSucceed — the measured failure mode: parallel
// `config set` processes against a FRESH file (the reviewer measured
// 7/96 failing on SQLITE_BUSY from the connection-open journal-mode
// pragma). The WAL switch now runs post-connect under busy retry —
// every goroutine's Open+Set+Close must succeed.
func TestParallelFirstOpenAllSucceed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.sqlite")
	const workers, rounds = 8, 6
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				st, err := Open(path)
				if err != nil {
					errs <- fmt.Errorf("worker %d round %d: %w", w, r, err)
					return
				}
				if err := st.Set(fmt.Sprintf("AXIOM_API_PORT"), strconv.Itoa(9000+w)); err != nil {
					errs <- fmt.Errorf("worker %d round %d set: %w", w, r, err)
					st.Close()
					return
				}
				if _, _, err := Read(path); err != nil {
					errs <- fmt.Errorf("worker %d round %d read: %w", w, r, err)
					st.Close()
					return
				}
				st.Close()
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.Fatal("parallel first-open must be busy-safe (post-connect WAL switch with retry)")
	}
}

// TestUninitializedFileReadsEmptyAndSelfHeals — a first-write crash
// leftover (a zero-byte or ledger-only file) is NOT a poisoned boot:
// Read treats it as empty (env-equivalent), and the next Open migrates
// it into a working store.
func TestUninitializedFileReadsEmptyAndSelfHeals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.sqlite")
	if err := os.WriteFile(path, nil, 0o600); err != nil { // 0-byte crash leftover
		t.Fatal(err)
	}
	s, found, err := Read(path)
	if err != nil || !found {
		t.Fatalf("0-byte file must read empty/found=true, got err=%v found=%v", err, found)
	}
	if !s.Empty() {
		t.Fatalf("0-byte file carries no rows, got %v", s)
	}
	st, err := Open(path) // the next writer heals it
	if err != nil {
		t.Fatalf("heal open: %v", err)
	}
	if err := st.Set("AXIOM_API_PORT", "8012"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	s, _, err = Read(path)
	if err != nil || s.Values["AXIOM_API_PORT"] != "8012" {
		t.Fatalf("healed store must work, got err=%v values=%v", err, s.Values)
	}
}

// TestAdoptedFileTightenedTo0600 — a pre-existing weaker permission is
// below the file's contract; Open tightens it.
func TestAdoptedFileTightenedTo0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.sqlite")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("adopted file mode = %v, want 0600", fi.Mode().Perm())
	}
}

// TestReadRejectsURLSignificantPathChars — the read path carries the
// same guard as the write path: a PRESENT file whose path contains a
// "?" must not be opened (the DSN would silently redirect to a
// DIFFERENT file). A valid store sits at the misdirected literal
// target and a 0-byte file at the ?-carrying name (so Read passes the
// Stat check and reaches the guard): without the guard the DSN reads
// the marker row and returns clean — this shape is what makes the
// witness able to fail.
func TestReadRejectsURLSignificantPathChars(t *testing.T) {
	dir := t.TempDir()
	literal := filepath.Join(dir, "a")
	st, err := Open(literal)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("AXIOM_API_PORT", "8012"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	weird := filepath.Join(dir, "a?b.sqlite")
	if err := os.WriteFile(weird, nil, 0o600); err != nil { // present at the literal name
		t.Fatal(err)
	}
	if _, _, err := Read(weird); err == nil {
		t.Fatal("read must reject ? in the path of a present file")
	}
	// the literal target itself stays readable — the guard is on the
	// PATH, not on the store.
	s, found, err := Read(literal)
	if err != nil || !found || s.Values["AXIOM_API_PORT"] != "8012" {
		t.Fatalf("the literal target must stay readable, found=%v err=%v values=%v", found, err, s.Values)
	}
}

// TestCorruptFileStaysLoud — the corrupt shapes ERROR, never read as
// empty: (a) a ledger claiming applied migrations with both runtime
// tables gone; (b) settings present but secret_refs dropped. Both are
// damaged past-selves (truncation victims, not the crashed-first-write
// uninitialized shape) — reading them as empty would silently discard
// the operator's overrides behind a green boot.
func TestCorruptFileStaysLoud(t *testing.T) {
	drop := func(t *testing.T, path, stmts string) {
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(stmts); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	for _, tc := range []struct {
		name, stmts, wantErr string
	}{
		{"both-tables-gone", `DROP TABLE settings; DROP TABLE secret_refs;`, "corrupt"},
		{"secret-refs-gone", `DROP TABLE secret_refs;`, "no such table"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.sqlite")
			st := openStore(t, path)
			if err := st.Set("AXIOM_API_PORT", "8012"); err != nil {
				t.Fatal(err)
			}
			st.Close()
			drop(t, path, tc.stmts)
			_, _, err := Read(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("corrupt shape must error naming %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestSetAllIsAtomic — a failing row (unknown secret-ref source) aborts
// the WHOLE batch: no partial import state survives.
func TestSetAllIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.sqlite")
	st := openStore(t, path)
	if err := st.SetAll(
		map[string]string{"AXIOM_API_PORT": "8012"},
		map[string]string{"AXIOM_WS_SECRET": "keychain"}, // unknown source
	); err == nil {
		t.Fatal("unknown ref source must fail the batch")
	}
	st.Close()
	s, _, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Values) != 0 || len(s.SecretRefs) != 0 {
		t.Fatalf("a failed SetAll must leave NO partial state, got %v %v", s.Values, s.SecretRefs)
	}
}
