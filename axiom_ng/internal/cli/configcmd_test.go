// configcmd_test.go — the F13 #307 CLI-surface witnesses: config
// set/unset with validation teeth (unknown key, type violation, secret
// refusal — each exit 1 and never touch the file), get --effective
// with the full source column, the one-shot env importer (idempotent,
// effective-identical, secrets as references with the log sonde
// proving zero secret bytes in any output line), and the doctor
// config-file check.
package cli

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config/configstore"
)

// backedConfig points the package's chain at a fresh config.sqlite the
// test writes through the real store.
func backedConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.sqlite")
	t.Setenv("AXIOM_CONFIG_PATH", path)
	return path
}

// captureRunTo runs Run with both streams captured and returns
// exit, stdout, stderr — with the process environment snapshotted and
// restored around the run (the chain MATERIALIZES file/flag values
// into it; a test's materializations must not leak into the next).
func captureRunTo(args ...string) (int, string, string) {
	defer restoreProcessEnv(snapshotEnv())()
	var errBuf strings.Builder
	exit, out := runTo(&errBuf, args)
	return exit, out, errBuf.String()
}

func snapshotEnv() []string { return os.Environ() }

// restoreProcessEnv rebuilds the exact snapshot — including UNSETTING
// keys the wrapped run created (chain materializations), which the
// snapshot itself cannot know.
func restoreProcessEnv(snapshot []string) func() {
	return func() {
		for _, kv := range snapshot {
			k, _, _ := strings.Cut(kv, "=")
			os.Unsetenv(k)
		}
		for _, kv := range snapshot {
			k, v, _ := strings.Cut(kv, "=")
			os.Setenv(k, v)
		}
		inSnapshot := map[string]bool{}
		for _, kv := range snapshot {
			k, _, _ := strings.Cut(kv, "=")
			inSnapshot[k] = true
		}
		for _, kv := range os.Environ() {
			k, _, _ := strings.Cut(kv, "=")
			if !inSnapshot[k] {
				os.Unsetenv(k)
			}
		}
	}
}

// TestConfigSetGoodAndBad — the set teeth: a good write lands in the
// file (exit 0); unknown key, type violation, and secret key each exit
// 1, name the key, and leave the file UNTOUCHED.
func TestConfigSetGoodAndBad(t *testing.T) {
	path := backedConfig(t)

	exit, out, _ := captureRunTo("config", "set", "AXIOM_API_PORT", "8012")
	if exit != exitOK {
		t.Fatalf("good set exit = %d, out=%s", exit, out)
	}
	settings, found, err := configstore.Read(path)
	if err != nil || !found || settings.Values["AXIOM_API_PORT"] != "8012" {
		t.Fatalf("set must persist, found=%v err=%v values=%v", found, err, settings.Values)
	}

	for _, tc := range []struct {
		key, value, wantMention string
	}{
		{"AXIOM_NO_SUCH_KEY", "x", "unknown key"},
		{"AXIOM_API_PORT", "eighty", "falls back"},
		{"AXIOM_API_PORT", "70000", "out of range"},
		{"AXIOM_SEARCH_RERANK", "maybe", "not a boolean"},
		{"AXIOM_STORAGE_LIBRARY_DRIVER", "oracle", "unknown driver"},
		{"AXIOM_WS_SECRET", "the-ws-secret", "secret"},
		{"AXIOM_LIBRARY_DATABASE_URL", "postgresql://u:pw@h/lib", "inline credential"},
	} {
		exit, out, _ := captureRunTo("config", "set", tc.key, tc.value)
		if exit != exitFailure {
			t.Fatalf("set %s must exit 1, got %d", tc.key, exit)
		}
		if !strings.Contains(out, tc.key) || !strings.Contains(out, tc.wantMention) {
			t.Fatalf("set %s must name key+reason, got: %s", tc.key, out)
		}
		if strings.Contains(out, "the-ws-secret") {
			t.Fatalf("set refusal echoed a secret value: %s", out)
		}
	}
	// the refusals wrote nothing beyond the good row
	settings, _, err = configstore.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Values) != 1 || len(settings.SecretRefs) != 0 {
		t.Fatalf("refused sets must not touch the file, got %v %v", settings.Values, settings.SecretRefs)
	}
}

// TestConfigUnsetGoodAndUnknown — unset removes (idempotent), unknown
// vocabulary keys are refused with the same teeth.
func TestConfigUnsetGoodAndUnknown(t *testing.T) {
	path := backedConfig(t)
	if exit, _, _ := captureRunTo("config", "set", "AXIOM_BIND_ADDR", "10.1.2.3"); exit != exitOK {
		t.Fatal("seed set failed")
	}
	if exit, _, _ := captureRunTo("config", "unset", "AXIOM_BIND_ADDR"); exit != exitOK {
		t.Fatal("unset failed")
	}
	if exit, _, _ := captureRunTo("config", "unset", "AXIOM_BIND_ADDR"); exit != exitOK {
		t.Fatal("unset must be idempotent")
	}
	settings, _, err := configstore.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := settings.Values["AXIOM_BIND_ADDR"]; ok {
		t.Fatal("unset row survived")
	}
	if exit, out, _ := captureRunTo("config", "unset", "AXIOM_NOPE"); exit != exitFailure || !strings.Contains(out, "AXIOM_NOPE") {
		t.Fatalf("unknown unset must exit 1 naming the key, got %d: %s", exit, out)
	}
}

// TestConfigGetShowsSourceColumn — get --effective renders the chain
// sources: default, file, env, flag — one probe key per stage, both in
// text and JSON.
func TestConfigGetShowsSourceColumn(t *testing.T) {
	path := backedConfig(t)
	st, err := configstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("AXIOM_API_PORT", "9101"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	t.Setenv("AXIOM_BIND_ADDR", "10.9.9.9")

	exit, out, _ := captureRunTo("config", "get", "--effective")
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	for _, tc := range []struct{ key, source string }{
		{"AXIOM_API_PORT", "file"},
		{"AXIOM_BIND_ADDR", "env"},
		{"AXIOM_QUERY_RUNNER_URL", "default"},
	} {
		line := findRowLine(out, tc.key)
		if !strings.Contains(line, tc.source) {
			t.Fatalf("%s row must show source=%s, got %q", tc.key, tc.source, line)
		}
	}

	exit, out, _ = captureRunTo("config", "get", "--effective", "--json", "--set", "AXIOM_SEARCH_MAX_PER_BOOK=7")
	if exit != exitOK {
		t.Fatalf("json exit = %d", exit)
	}
	var entries []config.Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &entries); err != nil {
		t.Fatalf("json: %v", err)
	}
	byEnv := map[string]config.Entry{}
	for _, e := range entries {
		byEnv[e.Env] = e
	}
	if e := byEnv["AXIOM_SEARCH_MAX_PER_BOOK"]; e.Source != "flag" || e.Value != float64(7) { // JSON numbers unmarshal as float64
		t.Fatalf("flag row = %+v, want source=flag value=7", e)
	}
	if e := byEnv["AXIOM_API_PORT"]; e.Source != "file" || e.Value != float64(9101) {
		t.Fatalf("file row = %+v", e)
	}
}

func findRowLine(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), key) {
			return line
		}
	}
	return ""
}

// TestConfigUsageArityAndDanglingSet — the exit-2 teeth: wrong arity on
// the config subcommands and a dangling --set anywhere in Run are
// usage errors, not runtime failures.
func TestConfigUsageArityAndDanglingSet(t *testing.T) {
	backedConfig(t)
	for _, args := range [][]string{
		{"config", "set", "AXIOM_API_PORT"},
		{"config", "set"},
		{"config", "unset"},
		{"config", "get"},
		{"config", "get", "--effective", "--bogus"},
		{"serve", "--set"},
		{"doctor", "--set"},
		{"config", "--set"},
	} {
		exit, _, _ := captureRunTo(args...)
		if exit != exitUsage {
			t.Fatalf("%v must exit 2, got %d", args, exit)
		}
	}
}

// TestGarbageConfigFileRefusesLoudly — a non-SQLite file at
// AXIOM_CONFIG_PATH makes the chain refuse loudly (exit 1 with a
// diagnosis) — never a silent boot on defaults.
func TestGarbageConfigFileRefusesLoudly(t *testing.T) {
	path := backedConfig(t)
	if err := os.WriteFile(path, []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer restoreProcessEnv(snapshotEnv())()
	exit, _, errOut := captureRunTo("config", "get", "--effective")
	if exit != exitFailure || !strings.Contains(errOut, "config sqlite") {
		t.Fatalf("garbage file must exit 1 with a diagnosis, got %d: %s", exit, errOut)
	}
}

// TestConfigGetFlagTeeth — a bad --set key/value on get is exit 1 with
// the diagnosis (the chain validates before resolving).
func TestConfigGetFlagTeeth(t *testing.T) {
	backedConfig(t)
	exit, _, errOut := captureRunTo("config", "get", "--effective", "--set", "AXIOM_NOPE=1")
	if exit != exitFailure || !strings.Contains(errOut, "AXIOM_NOPE") {
		t.Fatalf("unknown --set key must exit 1, got %d: %s", exit, errOut)
	}
	exit, _, errOut = captureRunTo("config", "get", "--effective", "--set", "AXIOM_WS_SECRET=x")
	if exit != exitFailure || !strings.Contains(errOut, "AXIOM_WS_SECRET") {
		t.Fatalf("secret --set key must exit 1, got %d: %s", exit, errOut)
	}
	if strings.Contains(errOut, "AXIOM_WS_SECRET=x") {
		t.Fatal("the refusal must not echo the flagged pair verbatim beyond the key name")
	}
	// an inline credential on the flag surface is refused like the file
	// surface (ps/history would carry it)
	exit, _, errOut = captureRunTo("config", "get", "--effective", "--set", "AXIOM_QUERY_RUNNER_URL=http://u:pw@runner:8012")
	if exit != exitFailure || !strings.Contains(errOut, "inline credential") {
		t.Fatalf("credential-bearing --set must exit 1, got %d: %s", exit, errOut)
	}
}

// TestImportEnvIdempotentEffectiveIdenticalAndSecretFree — the importer
// witnesses: (a) the effective view does not move across the import
// (env still owns every imported key), (b) replay is idempotent — the
// file state after two imports equals the state after one, (c) secrets
// land as references and the FILE plus every output line carry zero
// secret bytes (the log sonde).
func TestImportEnvIdempotentEffectiveIdenticalAndSecretFree(t *testing.T) {
	defer restoreProcessEnv(snapshotEnv())() // direct LoadResolved calls materialize nothing here today, but the sonde's shape must not depend on that
	path := backedConfig(t)
	secrets := map[string]string{
		"AXIOM_WS_SECRET":                    "ws-secret-value-xyz",
		"AXIOM_OPENSEARCH_PASSWORD":          "os-password-value-xyz",
		"AXIOM_DATABASE_URL":                 "postgresql://u:db-password-xyz@h/db",
		"AXIOM_COMPUTE_WORKER_SOURCE_SECRET": "hmac-source-secret-xyz",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	t.Setenv("AXIOM_API_PORT", "8222")
	t.Setenv("AXIOM_SEARCH_RERANK", "false")
	t.Setenv("AXIOM_PROCESSOR_URL", "http://legacy-runner:8012") // legacy spelling feeds

	before, _, err := config.LoadResolved(nil)
	if err != nil {
		t.Fatal(err)
	}

	exit, out, errOut := captureRunTo("config", "import-env")
	if exit != exitOK {
		t.Fatalf("import-env exit = %d, out=%s err=%s", exit, out, errOut)
	}

	// (c) log sonde: every output line is free of the secret vocabulary.
	for _, secret := range secrets {
		for _, stream := range []string{out, errOut} {
			if strings.Contains(stream, secret) {
				t.Fatalf("import-env output carries a secret value (%q)", secret)
			}
		}
	}
	// (b) replay idempotency at the file level — CONTENT, not counts.
	first, found, err := configstore.Read(path)
	if err != nil || !found {
		t.Fatalf("read after import: %v", err)
	}
	exit, _, _ = captureRunTo("config", "import-env")
	if exit != exitOK {
		t.Fatal("replay failed")
	}
	second, _, err := configstore.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(first.Values, second.Values) || !maps.Equal(first.SecretRefs, second.SecretRefs) {
		t.Fatalf("replay must be idempotent: %v/%v then %v/%v", first.Values, first.SecretRefs, second.Values, second.SecretRefs)
	}
	// (a) effective identical: env owns the keys, the file shadows
	// nothing — the WHOLE configuration, not a spot check.
	after, _, err := config.LoadResolved(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("effective moved across import:\n%+v\nvs\n%+v", before, after)
	}
	// secrets as references only; values never in the file bytes.
	if len(first.SecretRefs) != len(secrets) {
		t.Fatalf("secret refs = %v, want one per secret env", first.SecretRefs)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(b), secret) {
			t.Fatalf("config.sqlite carries a secret value (%q)", secret)
		}
	}
	// the DSN's credential never entered the settings either.
	if v, ok := first.Values["AXIOM_DATABASE_URL"]; ok {
		t.Fatalf("secret-valued key must not be a settings row, got %q", v)
	}
}

// TestImportEnvValidatesBeforeWrite — the importer validates the mapped
// rows against the file surface's rules FIRST: a value the file would
// refuse (here: a type violation the env carries) refuses the whole
// import — exit 1, nothing written, no file created.
func TestImportEnvValidatesBeforeWrite(t *testing.T) {
	path := backedConfig(t)
	defer restoreProcessEnv(snapshotEnv())()
	t.Setenv("AXIOM_API_PORT", "not-a-port")
	exit, out, _ := captureRunTo("config", "import-env")
	if exit != exitFailure || !strings.Contains(out, "AXIOM_API_PORT") {
		t.Fatalf("invalid env must refuse the import, got %d: %s", exit, out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a refused import must create nothing, stat err = %v", err)
	}
}

// TestUnsetRemovesSecretRef — unset clears the secret-ref side too:
// import-env creates the reference, config unset removes it, and the
// drift check is clean afterwards (a surviving ref would fail the
// next doctor the moment the env var clears).
func TestUnsetRemovesSecretRef(t *testing.T) {
	path := backedConfig(t)
	defer restoreProcessEnv(snapshotEnv())()
	t.Setenv("AXIOM_WS_SECRET", "ws-secret-value-abc")
	if exit, out, errOut := captureRunTo("config", "import-env"); exit != exitOK {
		t.Fatalf("seed import failed: %d %s %s", exit, out, errOut)
	}
	settings, found, err := configstore.Read(path)
	if err != nil || !found || len(settings.SecretRefs) != 1 {
		t.Fatalf("import must create the ref, found=%v err=%v refs=%v", found, err, settings.SecretRefs)
	}
	if exit, _, _ := captureRunTo("config", "unset", "AXIOM_WS_SECRET"); exit != exitOK {
		t.Fatal("unset of a secret-ref key failed")
	}
	settings, _, err = configstore.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := settings.SecretRefs["AXIOM_WS_SECRET"]; ok {
		t.Fatalf("unset must remove the secret reference, got %v", settings.SecretRefs)
	}
	if drift := config.SecretRefDrift(); len(drift) != 0 {
		t.Fatalf("drift after unset: %v", drift)
	}
}

// TestDoctorConfigFileCheck — the doctor row: absent file = ok (the
// container path), present file = ok with its shape, broken file =
// fail with the whole report still rendered.
func TestDoctorConfigFileCheck(t *testing.T) {
	t.Run("absent file is ok", func(t *testing.T) {
		backedConfig(t)
		defer restoreProcessEnv(snapshotEnv())() // doctor resolves through the chain
		rep := runDoctor(nil)
		if rep.Checks["config-file"].Status != "ok" || !strings.Contains(rep.Checks["config-file"].Detail, "absent") {
			t.Fatalf("config-file: %+v", rep.Checks["config-file"])
		}
	})
	t.Run("present file is ok with shape", func(t *testing.T) {
		path := backedConfig(t)
		st, err := configstore.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Set("AXIOM_API_PORT", "8012"); err != nil {
			t.Fatal(err)
		}
		st.Close()
		defer restoreProcessEnv(snapshotEnv())()
		rep := runDoctor(nil)
		if rep.Checks["config-file"].Status != "ok" || !strings.Contains(rep.Checks["config-file"].Detail, "1 settings") {
			t.Fatalf("config-file: %+v", rep.Checks["config-file"])
		}
	})
	t.Run("broken file fails but reports", func(t *testing.T) {
		path := backedConfig(t)
		st, err := configstore.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Set("AXIOM_API_PORT", "not-a-port"); err != nil {
			t.Fatal(err)
		}
		st.Close()
		defer restoreProcessEnv(snapshotEnv())()
		rep := runDoctor(nil)
		if rep.Checks["config-file"].Status != "fail" {
			t.Fatalf("config-file must fail, got %+v", rep.Checks["config-file"])
		}
		if rep.Checks["database"].Status == "" || !strings.Contains(rep.Checks["database"].Detail, "skipped") {
			t.Fatalf("cfg-dependent checks must degrade honestly, got %+v", rep.Checks["database"])
		}
		if rep.OK {
			t.Fatal("report must be unhealthy")
		}
	})
}

// TestServeRoutesThroughTheChain — serve resolves through the file
// layer: a file-fed port in the #205 debug-bind guard range (8013 —
// NOT the 8011 default) makes the debug build refuse BEFORE anything
// else, naming the file-fed port — proof the chain-fed configuration
// reached the serve boot (and no listener ever starts: the refusal is
// the fast exit).
func TestServeRoutesThroughTheChain(t *testing.T) {
	path := backedConfig(t)
	st, err := configstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("AXIOM_API_PORT", "8013"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	defer restoreProcessEnv(snapshotEnv())() // the boot materializes the file row into env
	var buf strings.Builder
	exit := serveTo(&buf, []string{"serve", "api"})
	if exit != exitFailure {
		t.Fatalf("debug build on a guard-range port must exit 1, got %d", exit)
	}
	if !strings.Contains(buf.String(), "8013") || !strings.Contains(buf.String(), "refusing to bind") {
		t.Fatalf("serve must name the FILE-fed port in the guard refusal, got exit=%d: %s", exit, buf.String())
	}
}
