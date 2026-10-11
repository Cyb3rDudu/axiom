package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
)

// #368 (ADR 0001 executed, v0.2.4): the dispatcher-side legacy environment
// names are GONE. Every removed name gets its own probe here (the strand's
// DoD): the loader ignores it (canonical value/default applies), and every
// refusal surface — ValidateEnv (environment), ValidateFlags (--set),
// ValidateSettings (config.sqlite rows) — names the canonical successor.

// legacyProbeNames is the complete removed-name set (mirrors
// LegacyEnvSuccessor; kept explicit so a map drift fails loudly below).
var legacyProbeNames = map[string]string{
	"AXIOM_DATABASE_URL":              "AXIOM_STORE_DATABASE_URL",
	"AXIOM_FIXER_CMD":                 "AXIOM_REPAIR_WORKER_CMD",
	"AXIOM_PROCESSOR_URL":             "AXIOM_COMPUTE_WORKER_URL",
	"AXIOM_PROCESSOR_URLS":            "AXIOM_COMPUTE_WORKER_URLS",
	"AXIOM_PROCESSOR_RUNNER_NAME":     "AXIOM_COMPUTE_WORKER_NAME",
	"AXIOM_PROCESSOR_TIMEOUT":         "AXIOM_COMPUTE_WORKER_TIMEOUT",
	"AXIOM_PROCESSOR_SOURCE_SECRET":   "AXIOM_COMPUTE_WORKER_SOURCE_SECRET",
	"AXIOM_PROCESSOR_SOURCE_BASE_URL": "AXIOM_COMPUTE_WORKER_SOURCE_BASE_URL",
	"AXIOM_RUNNER_HEALTH_INTERVAL":    "AXIOM_COMPUTE_WORKER_HEALTH_INTERVAL",
}

func TestLegacyEnvSuccessorTableComplete(t *testing.T) {
	if len(LegacyEnvSuccessor) != len(legacyProbeNames) {
		t.Fatalf("LegacyEnvSuccessor has %d entries, probe set has %d — keep both in sync", len(LegacyEnvSuccessor), len(legacyProbeNames))
	}
	for name, succ := range legacyProbeNames {
		if LegacyEnvSuccessor[name] != succ {
			t.Errorf("LegacyEnvSuccessor[%s] = %q, want %q", name, LegacyEnvSuccessor[name], succ)
		}
	}
}

// TestLegacyEnvDoesNotFeedLoader: with ONLY the legacy name set (canonical
// unset), every affected field keeps its documented default/empty value —
// the probe has teeth per name, not just against canonical precedence.
func TestLegacyEnvDoesNotFeedLoader(t *testing.T) {
	// the runner-name default derives from the default runner URL's host
	var defaultRunnerHost string
	if u, err := url.Parse(defaultLocalRunner); err == nil {
		defaultRunnerHost = u.Host
	}
	defaultSourceBase := "http://127.0.0.1:" + strconv.Itoa(defaultAPIPort)
	asserts := map[string]func(Config) string{
		"AXIOM_DATABASE_URL":              func(c Config) string { return c.DatabaseURL },
		"AXIOM_FIXER_CMD":                 func(c Config) string { return c.FixerCommand },
		"AXIOM_PROCESSOR_URL":             func(c Config) string { return c.ProcessorURL },
		"AXIOM_PROCESSOR_URLS":            func(c Config) string { return fmt.Sprint(c.ProcessorURLs) },
		"AXIOM_PROCESSOR_RUNNER_NAME":     func(c Config) string { return c.ProcessorRunnerName },
		"AXIOM_PROCESSOR_TIMEOUT":         func(c Config) string { return c.ProcessorRequestTimeout.String() },
		"AXIOM_PROCESSOR_SOURCE_SECRET":   func(c Config) string { return c.ProcessorSourceSecret },
		"AXIOM_PROCESSOR_SOURCE_BASE_URL": func(c Config) string { return c.ProcessorSourceBaseURL },
		"AXIOM_RUNNER_HEALTH_INTERVAL":    func(c Config) string { return c.RunnerHealthInterval.String() },
	}
	defaults := map[string]string{
		"AXIOM_DATABASE_URL":              "",
		"AXIOM_FIXER_CMD":                 "/opt/axiom/bin/axiom-repair-worker",
		"AXIOM_PROCESSOR_URL":             defaultLocalRunner,
		"AXIOM_PROCESSOR_URLS":            "[]",
		"AXIOM_PROCESSOR_RUNNER_NAME":     defaultRunnerHost, // derives from the URL host post-load
		"AXIOM_PROCESSOR_TIMEOUT":         "5m0s",
		"AXIOM_PROCESSOR_SOURCE_SECRET":   "",
		"AXIOM_PROCESSOR_SOURCE_BASE_URL": defaultSourceBase,
		"AXIOM_RUNNER_HEALTH_INTERVAL":    "1m0s",
	}
	// ambient process env (chain tests materialize os.Setenv rows) must
	// not skew the defaults: clear the canonical siblings and the port.
	withRestoredEnv(t, func() {
		for _, k := range []string{
			"AXIOM_API_PORT", "AXIOM_STORE_DATABASE_URL", "AXIOM_REPAIR_WORKER_CMD",
			"AXIOM_COMPUTE_WORKER_URL", "AXIOM_COMPUTE_WORKER_URLS", "AXIOM_COMPUTE_WORKER_NAME",
			"AXIOM_COMPUTE_WORKER_TIMEOUT", "AXIOM_COMPUTE_WORKER_SOURCE_SECRET",
			"AXIOM_COMPUTE_WORKER_SOURCE_BASE_URL", "AXIOM_COMPUTE_WORKER_HEALTH_INTERVAL",
			"AXIOM_QUERY_RUNNER_URL", "AXIOM_INGEST_FALLBACK_URL",
		} {
			os.Unsetenv(k)
		}
		for name := range legacyProbeNames {
			t.Run(name, func(t *testing.T) {
				t.Setenv(name, "legacy-value")
				if _, ok := asserts[name]; !ok {
					t.Fatalf("probe table missing assertion for %s — add field+default", name)
				}
				got := asserts[name](Load())
				if got != defaults[name] {
					t.Fatalf("legacy %s must leave the default in place (got %q, want %q)", name, got, defaults[name])
				}
			})
		}
	})
}

// TestLegacyEnvValidateEnvNamesSuccessor: `axiom config validate` reports
// every set legacy name with its canonical successor.
func TestLegacyEnvValidateEnvNamesSuccessor(t *testing.T) {
	for name, succ := range legacyProbeNames {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "legacy-value")
			problems := ValidateEnv()
			joined := strings.Join(problems, "\n")
			if !strings.Contains(joined, name) || !strings.Contains(joined, succ) {
				t.Fatalf("ValidateEnv must name %s and its successor %s, got:\n%s", name, succ, joined)
			}
		})
	}
}

// TestLegacyFlagRefusedWithSuccessor: a --set (or config.sqlite row)
// carrying a legacy name is refused as unknown key AND names the successor.
func TestLegacyFlagRefusedWithSuccessor(t *testing.T) {
	for name, succ := range legacyProbeNames {
		t.Run(name, func(t *testing.T) {
			problems := ValidateFlags(map[string]string{name: "legacy-value"})
			joined := strings.Join(problems, "\n")
			if !strings.Contains(joined, "unknown key") {
				t.Fatalf("ValidateFlags must refuse %s as unknown key, got:\n%s", name, joined)
			}
			if !strings.Contains(joined, succ) {
				t.Fatalf("refusal for %s must name the successor %s, got:\n%s", name, succ, joined)
			}
			rowProblems := ValidateSettings(map[string]string{name: "legacy-value"}, nil)
			if !strings.Contains(strings.Join(rowProblems, "\n"), succ) {
				t.Fatalf("config.sqlite row for %s must be refused naming %s, got:\n%s", name, succ, strings.Join(rowProblems, "\n"))
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
