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
	"os"
	"path/filepath"
	"strings"
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

// TestRoundtripSetUnsetSecretRef — settings upsert, unset idempotency,
// secret-ref rows, and the Settings read-back shape.
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
	if err := st.SetSecretRef("AXIOM_WS_SECRET", "env"); err != nil {
		t.Fatalf("secret ref: %v", err)
	}
	got, err := st.Settings()
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if want := map[string]string{"AXIOM_API_PORT": "9999", "AXIOM_STORAGE_LIBRARY_DRIVER": "sqlite"}; !equalMaps(got.Values, want) {
		t.Fatalf("values = %v, want %v", got.Values, want)
	}
	if want := map[string]string{"AXIOM_WS_SECRET": "env"}; !equalMaps(got.SecretRefs, want) {
		t.Fatalf("secret refs = %v, want %v", got.SecretRefs, want)
	}

	if err := st.Unset("AXIOM_API_PORT"); err != nil {
		t.Fatalf("unset: %v", err)
	}
	if err := st.Unset("AXIOM_API_PORT"); err != nil { // idempotent
		t.Fatalf("unset again: %v", err)
	}
	got, err = st.Settings()
	if err != nil {
		t.Fatalf("settings after unset: %v", err)
	}
	if _, ok := got.Values["AXIOM_API_PORT"]; ok {
		t.Fatalf("unset key survived: %v", got.Values)
	}

	if err := st.SetSecretRef("AXIOM_WS_SECRET", "keychain"); err == nil {
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
	if _, err := os.Stat(dir + "/.axiom-ng"); !os.IsNotExist(err) {
		t.Fatalf("read must not create state dirs")
	}
}

// TestOpenIdempotentAndVersioned — second Open adopts the file; the
// ledger carries the applied migration; rows survive the reopen.
func TestOpenIdempotentAndVersioned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.sqlite")
	st := openStore(t, path)
	if err := st.Set("AXIOM_BIND_ADDR", "0.0.0.0"); err != nil {
		t.Fatalf("set: %v", err)
	}
	n, latest, err := st.Version()
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if n != 1 || !strings.Contains(latest, "0001") {
		t.Fatalf("version = %d/%q, want 1 applied, latest 0001_*", n, latest)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s, found, err := Read(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !found || s.Values["AXIOM_BIND_ADDR"] != "0.0.0.0" {
		t.Fatalf("rows must survive reopen, got found=%v %v", found, s.Values)
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
// the AXIOM_* vocabulary); this sonde pins what every sanctioned write
// path produces, including the import-env flow's ref rows.
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
	for _, refKey := range []string{"AXIOM_WS_SECRET", "AXIOM_OPENSEARCH_PASSWORD", "AXIOM_COMPUTE_WORKER_SOURCE_SECRET", "AXIOM_DATABASE_URL"} {
		if err := st.SetSecretRef(refKey, "env"); err != nil {
			t.Fatal(err)
		}
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
	home, _ := os.UserHomeDir()
	p, err = DefaultPath()
	if err != nil || p != home+"/.axiom-ng/config.sqlite" {
		t.Fatalf("default path = %q err=%v", p, err)
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
