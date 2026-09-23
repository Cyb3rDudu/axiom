// cli_test.go — unit witnesses for the F05 command surface (#299):
// dispatch/exit codes, the not-yet-extracted role refusals, version JSON,
// config get redaction + sources, config validate, doctor check logic,
// and the legacy-mode fallthrough. Binary-level witnesses (alias
// deprecation count, mode-exit discipline) live in cmd/axiom-ng's IT
// suite, which execs the built binaries.
package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
)

func TestServeRefusesUnextractedRoles(t *testing.T) {
	for role, want := range map[string]string{
		"library": "F06 (#300)",
		"store":   "F09 (#303)",
	} {
		var buf strings.Builder
		exit := serveTo(&buf, []string{"serve", role})
		if exit != exitUsage {
			t.Fatalf("serve %s exit = %d, want %d", role, exit, exitUsage)
		}
		if !strings.Contains(buf.String(), "component not yet extracted") || !strings.Contains(buf.String(), want) {
			t.Fatalf("serve %s must refuse loudly naming %s, got: %s", role, want, buf.String())
		}
	}
	var buf strings.Builder
	if exit := serveTo(&buf, []string{"serve", "bogus"}); exit != exitUsage {
		t.Fatalf("unknown role exit = %d, want %d", exit, exitUsage)
	}
	if exit := serveTo(&buf, []string{"serve"}); exit != exitUsage {
		t.Fatalf("missing role exit = %d, want %d", exit, exitUsage)
	}
}

// serveTo runs Run with stderr captured (the refusals write to stderr).
func serveTo(buf *strings.Builder, args []string) int {
	prev := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	exit := Run("axiom", append([]string{"axiom"}, args...))
	w.Close()
	os.Stderr = prev
	b, _ := io.ReadAll(r)
	buf.Write(b)
	return exit
}

func TestVersionJSON(t *testing.T) {
	exit := Run("axiom", []string{"axiom", "version", "--json"})
	if exit != exitOK {
		t.Fatalf("version --json exit %d", exit)
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	if exit := serveTo(&strings.Builder{}, []string{"frobnicate"}); exit != exitUsage {
		t.Fatalf("unknown command exit = %d, want 2", exit)
	}
}

func TestConfigGetEffectiveRedactsSecrets(t *testing.T) {
	t.Setenv("AXIOM_DATABASE_URL", "postgresql://u:secretpw@127.0.0.1:5432/axiom_dev?sslmode=disable")
	t.Setenv("AXIOM_OPENSEARCH_PASSWORD", "hunter2")
	t.Setenv("AXIOM_API_PORT", "8123")

	exit := Run("axiom", []string{"axiom", "config", "get", "--effective", "--json"})
	if exit != exitOK {
		t.Fatalf("config get exit %d", exit)
	}
}

// The JSON content itself is asserted in the config package (Effective);
// here the secret-leak sonde: NO output of the CLI ever contains the
// secret VALUES — run every read-only command with secrets set and grep
// the combined output.
func TestNoSecretValuesInAnyOutput(t *testing.T) {
	const dsnPW = "k9xJc2VjcmV0pw"
	t.Setenv("AXIOM_DATABASE_URL", "postgresql://u:"+dsnPW+"@127.0.0.1:5432/db?sslmode=disable")
	t.Setenv("AXIOM_OPENSEARCH_PASSWORD", "opensearch-hunter2")
	t.Setenv("AXIOM_WS_SECRET", "ws-hunter3")
	t.Setenv("AXIOM_PROCESSOR_SOURCE_SECRET", "src-hunter4")
	t.Setenv("AXIOM_ARTIFACT_ROOT", t.TempDir())

	var outBuf, errBuf strings.Builder
	prevStdout, prevStderr := os.Stdout, os.Stderr
	ro, wo, _ := os.Pipe()
	re, we, _ := os.Pipe()
	os.Stdout, os.Stderr = wo, we
	for _, args := range [][]string{
		{"config", "get", "--effective", "--json"},
		{"config", "get", "--effective"},
		{"config", "validate"},
		{"version", "--json"},
		{"doctor", "--json"},
	} {
		_ = Run("axiom", append([]string{"axiom"}, args...))
	}
	wo.Close()
	we.Close()
	os.Stdout, os.Stderr = prevStdout, prevStderr
	rb, _ := io.ReadAll(ro)
	eb, _ := io.ReadAll(re)
	outBuf.Write(rb)
	errBuf.Write(eb)
	out := outBuf.String() + errBuf.String()
	for _, secret := range []string{dsnPW, "opensearch-hunter2", "ws-hunter3", "src-hunter4"} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret value %q leaked into CLI output:\n%s", secret, out)
		}
	}
	// The sanitized DSN keeps host+db but drops the credential — and the
	// redaction sonde must not pass vacuously: the sanitized form IS there.
	if !strings.Contains(out, "127.0.0.1:5432") {
		t.Fatalf("sanitized DSN projection missing host — redaction removed too much:\n%s", out)
	}
}

func TestConfigValidateCatchesBadEnv(t *testing.T) {
	t.Setenv("AXIOM_API_PORT", "not-a-port")
	t.Setenv("AXIOM_DISPATCHER_LEASE", "not-a-duration")
	if exit := Run("axiom", []string{"axiom", "config", "validate"}); exit != exitFailure {
		t.Fatalf("invalid env must exit 1, got %d", exit)
	}
}

func TestConfigValidateCatchesHalfWiring(t *testing.T) {
	t.Setenv("AXIOM_DATABASE_URL", "")
	t.Setenv("AXIOM_DISPATCHER_ENABLED", "1")
	if exit := Run("axiom", []string{"axiom", "config", "validate"}); exit != exitFailure {
		t.Fatalf("dispatcher without store must exit 1, got %d", exit)
	}
}

func TestConfigSetRefusesUntilF13(t *testing.T) {
	if exit := serveTo(&strings.Builder{}, []string{"config", "set", "a", "b"}); exit != exitUsage {
		t.Fatalf("config set exit = %d, want 2", exit)
	}
}

func TestDoctorChecksLogic(t *testing.T) {
	ossrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ossrv.Close()

	dir := t.TempDir()

	t.Run("healthy without db is still a failure (store unreachable)", func(t *testing.T) {
		t.Setenv("AXIOM_DATABASE_URL", "")
		t.Setenv("AXIOM_OPENSEARCH_URL", ossrv.URL)
		t.Setenv("AXIOM_ARTIFACT_ROOT", dir)
		rep := runDoctor(config.Load())
		if rep.OK {
			t.Fatal("doctor must not be ok without a database")
		}
		if rep.Checks["opensearch"].Status != "ok" {
			t.Fatalf("opensearch: %+v", rep.Checks["opensearch"])
		}
		if rep.Checks["artifact-root"].Status != "ok" {
			t.Fatalf("artifact-root: %+v", rep.Checks["artifact-root"])
		}
	})

	t.Run("broken dsn fails database check", func(t *testing.T) {
		t.Setenv("AXIOM_DATABASE_URL", "postgresql://nope@127.0.0.1:1/db?sslmode=disable&connect_timeout=1")
		t.Setenv("AXIOM_OPENSEARCH_URL", ossrv.URL)
		t.Setenv("AXIOM_ARTIFACT_ROOT", dir)
		rep := runDoctor(config.Load())
		if rep.Checks["database"].Status != "fail" {
			t.Fatalf("database check must fail on unreachable DSN: %+v", rep.Checks["database"])
		}
		if rep.OK {
			t.Fatal("report must be unhealthy")
		}
	})

	t.Run("disabled opensearch is ok", func(t *testing.T) {
		t.Setenv("AXIOM_OPENSEARCH_URL", "")
		rep := runDoctor(config.Load())
		if rep.Checks["opensearch"].Status != "ok" || !strings.Contains(rep.Checks["opensearch"].Detail, "disabled") {
			t.Fatalf("set-empty OS URL is the documented disabled state: %+v", rep.Checks["opensearch"])
		}
	})

	t.Run("missing artifact root fails", func(t *testing.T) {
		t.Setenv("AXIOM_ARTIFACT_ROOT", "")
		rep := runDoctor(config.Load())
		if rep.Checks["artifact-root"].Status != "fail" {
			t.Fatalf("artifact-root: %+v", rep.Checks["artifact-root"])
		}
	})

	t.Run("json serializes", func(t *testing.T) {
		rep := runDoctor(config.Load())
		if _, err := json.Marshal(rep); err != nil {
			t.Fatal(err)
		}
	})
}
