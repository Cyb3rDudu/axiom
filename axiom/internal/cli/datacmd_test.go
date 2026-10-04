// datacmd_test.go — `axiom data` surface witnesses (DM03 #312 / DM04
// #313): dispatch, flag discipline (exit 2 usage errors), and the
// export path against a missing source (exit 1 runtime failure with a
// sanitized message — no DSN echo). The functional roundtrips live in
// internal/databundle.
package cli

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestDataUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"bare", []string{}, exitUsage},
		{"unknown verb", []string{"frobnicate"}, exitUsage},
		{"export without component", []string{"export", "--dsn", "x", "--out", "y"}, exitUsage},
		{"export missing dsn", []string{"export", "--component", "library", "--out", "y"}, exitUsage},
		{"export missing out", []string{"export", "--component", "library", "--dsn", "x"}, exitUsage},
		{"import no target", []string{"import", "--component", "library", "--from", "y"}, exitUsage},
		{"import two targets", []string{"import", "--component", "library", "--from", "y", "--dsn", "a", "--sqlite", "b"}, exitUsage},
		{"import missing from", []string{"import", "--component", "library", "--dsn", "a"}, exitUsage},
		{"verify no target", []string{"verify", "--component", "library", "--from", "y"}, exitUsage},
		{"shadow missing source", []string{"shadow", "--component", "library", "--dsn", "a", "--out", "r"}, exitUsage},
		{"shadow no target", []string{"shadow", "--component", "library", "--source-dsn", "s", "--out", "r"}, exitUsage},
		{"shadow two targets", []string{"shadow", "--component", "library", "--source-dsn", "s", "--dsn", "a", "--sqlite", "b", "--out", "r"}, exitUsage},
		{"shadow missing out", []string{"shadow", "--component", "library", "--source-dsn", "s", "--dsn", "a"}, exitUsage},
		{"unknown component", []string{"export", "--component", "store", "--dsn", "x", "--out", "y"}, exitUsage},
		{"positional junk", []string{"verify", "--component", "library", "--from", "y", "--dsn", "a", "junk"}, exitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code := cmdData("axiom", tc.args); code != tc.want {
				t.Fatalf("cmdData(%v) = %d, want %d", tc.args, code, tc.want)
			}
		})
	}
}

// TestDataExportRuntimeFailureSanitized — an unreachable source is a
// runtime failure (exit 1) whose OUTPUT carries NO DSN credential
// material (the sanitized-DSN discipline of the data family).
func TestDataExportRuntimeFailureSanitized(t *testing.T) {
	const secret = "super-secret-password"
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = wr
	code := cmdData("axiom", []string{"export", "--component", "library",
		"--dsn", "postgresql://axiom_user:" + secret + "@127.0.0.1:1/none?sslmode=disable",
		"--out", t.TempDir()})
	os.Stderr = oldStderr
	wr.Close()
	out, _ := io.ReadAll(rd)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if strings.Contains(string(out), secret) {
		t.Fatal("failure output leaked the DSN password")
	}
}

// TestDataShadowRuntimeFailureSanitized — the shadow leg carries the
// same discipline: an unreachable source is a runtime failure (exit 1)
// whose output carries NO DSN credential material.
func TestDataShadowRuntimeFailureSanitized(t *testing.T) {
	const secret = "super-secret-password"
	rds, wrs, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = wrs
	code := cmdData("axiom", []string{"shadow", "--component", "library",
		"--source-dsn", "postgresql://axiom_user:" + secret + "@127.0.0.1:1/none?sslmode=disable",
		"--dsn", "postgresql://axiom_user:x@127.0.0.1:1/none?sslmode=disable",
		"--out", t.TempDir() + "/shadow-report.json"})
	os.Stderr = oldStderr
	wrs.Close()
	out, _ := io.ReadAll(rds)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if strings.Contains(string(out), secret) {
		t.Fatal("failure output leaked the DSN password")
	}
}

// TestDataHelpSurface — the help text documents the data family.
func TestDataHelpSurface(t *testing.T) {
	h := help("axiom")
	for _, want := range []string{"data export", "data import", "data verify", "data shadow", "DM03", "DM04"} {
		if !strings.Contains(h, want) {
			t.Fatalf("help lacks %q", want)
		}
	}
}
