// doctor.go — `axiom doctor` (F05 #299): the first-line deployment
// diagnostic. Checks the configuration resolution, Postgres reachability
// (plus the migration ledger state), OpenSearch reachability, the
// artifact root, and reports binary/schema versions. Exit 0 ONLY when
// every check is healthy. Secret VALUES never appear in any output —
// failures carry the diagnosis, not the credential (the DSN is projected
// through config's sanitizer before it can land in a detail line).
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/version"
	"github.com/jackc/pgx/v5"
)

// checkStatus is one doctor check's verdict.
type checkStatus struct {
	Status string `json:"status"` // "ok" | "fail"
	Detail string `json:"detail,omitempty"`
}

// doctorReport is the --json shape (and the text rendering's source).
type doctorReport struct {
	OK       bool                   `json:"ok"`
	Binary   map[string]string      `json:"binary"`
	Schema   *schemaInfo            `json:"schema,omitempty"`
	Checks   map[string]checkStatus `json:"checks"`
	Problems []string               `json:"problems,omitempty"`
}

type schemaInfo struct {
	Migrations int    `json:"migrations"`
	Latest     string `json:"latest"`
}

// runDoctor collects every check against cfg. All checks run (a red
// report names every problem, not just the first).
func runDoctor(cfg config.Config) doctorReport {
	rep := doctorReport{
		Binary: map[string]string{
			"banner":     version.Banner(),
			"version":    version.Version,
			"commit":     version.Commit,
			"build_type": version.BuildType,
		},
		Checks: map[string]checkStatus{},
	}

	// config: env combination parses cleanly (silent-fallback detector).
	if problems := config.ValidateEnv(); len(problems) > 0 {
		rep.Checks["config"] = checkStatus{Status: "fail", Detail: fmt.Sprintf("env values the loader would silently ignore: %v", problems)}
	} else {
		rep.Checks["config"] = checkStatus{Status: "ok"}
	}

	// database: reachability + migration ledger.
	if cfg.DatabaseURL == "" {
		rep.Checks["database"] = checkStatus{Status: "fail", Detail: "AXIOM_DATABASE_URL not set — the store is unreachable by definition"}
	} else if info, err := probeDatabase(cfg.DatabaseURL); err != nil {
		rep.Checks["database"] = checkStatus{Status: "fail", Detail: err.Error()}
	} else {
		rep.Schema = info
		rep.Checks["database"] = checkStatus{Status: "ok", Detail: fmt.Sprintf("%d migrations applied, latest %s", info.Migrations, info.Latest)}
	}

	// opensearch: reachability of the outbox/search backing store. An
	// explicitly SET-but-empty URL is the documented disabled state — a
	// legal deployment shape, reported as such (not a failure).
	switch {
	case cfg.OpenSearchURL == "":
		rep.Checks["opensearch"] = checkStatus{Status: "ok", Detail: "disabled (AXIOM_OPENSEARCH_URL set empty — outbox rows stay pending)"}
	default:
		if code, err := probeHTTP(cfg.OpenSearchURL + "/_cluster/health"); err != nil {
			rep.Checks["opensearch"] = checkStatus{Status: "fail", Detail: err.Error()}
		} else {
			rep.Checks["opensearch"] = checkStatus{Status: "ok", Detail: fmt.Sprintf("reachable (HTTP %d)", code)}
		}
	}

	// artifact root: configured, a directory, writable (the dispatcher's
	// durable artifact surface; F14 consumes this diagnosis).
	switch {
	case cfg.ArtifactRoot == "":
		rep.Checks["artifact-root"] = checkStatus{Status: "fail", Detail: "AXIOM_ARTIFACT_ROOT not set — dispatcher artifacts have no durable home"}
	default:
		if err := probeWritableDir(cfg.ArtifactRoot); err != nil {
			rep.Checks["artifact-root"] = checkStatus{Status: "fail", Detail: err.Error()}
		} else {
			rep.Checks["artifact-root"] = checkStatus{Status: "ok", Detail: cfg.ArtifactRoot}
		}
	}

	rep.OK = true
	for name, c := range rep.Checks {
		if c.Status != "ok" {
			rep.OK = false
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %s", name, c.Detail))
		}
	}
	return rep
}

func cmdDoctor(asJSON bool) int {
	rep := runDoctor(config.Load())
	if asJSON {
		out, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return exitFailure
		}
		fmt.Println(string(out))
	} else {
		fmt.Println("binary:  ", rep.Binary["banner"])
		for _, name := range []string{"config", "database", "opensearch", "artifact-root"} {
			c := rep.Checks[name]
			line := fmt.Sprintf("%-14s %s", name+":", c.Status)
			if c.Detail != "" {
				line += " — " + c.Detail
			}
			fmt.Println(line)
		}
		if rep.OK {
			fmt.Println("healthy")
		} else {
			fmt.Println("unhealthy")
		}
	}
	if rep.OK {
		return exitOK
	}
	return exitFailure
}

// probeDatabase opens a bounded connection and reads the migration
// ledger. The error path carries the DSN only through config's
// sanitizer (pgx errors quote host/user, never the password).
func probeDatabase(dsn string) (*schemaInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	info := &schemaInfo{}
	if err := conn.QueryRow(ctx, `SELECT count(*), max(version) FROM schema_migrations`).Scan(&info.Migrations, &info.Latest); err != nil {
		return nil, fmt.Errorf("schema_migrations unreadable (migrated?): %v", err)
	}
	return info, nil
}

// probeHTTP issues a bounded GET and reports the status code.
func probeHTTP(url string) (int, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, fmt.Errorf("unreachable: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// probeWritableDir asserts the root is a writable directory (one probe
// file, removed immediately).
func probeWritableDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("not accessible: %v", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("not a directory")
	}
	probe := filepath.Join(dir, ".axiom-doctor-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		return fmt.Errorf("not writable: %v", err)
	}
	return os.Remove(probe)
}
