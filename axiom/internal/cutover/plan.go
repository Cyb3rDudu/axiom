// Package cutover is the DM09 (#318) window machinery: a gated,
// manifest-led, resumable cutover (`axiom data cutover`) and its
// rollback (`axiom data rollback`), orchestrating the landed DM03–DM08
// tooling (bundle export/import/verify, shadow-read) plus the in-window
// schema steps (target migration BEFORE data — asserted, not assumed —
// and the DM06 FK drops on the adopted Store database).
//
// # Shape
//
// An operator authors a PLAN (JSON, 0600 — it carries privileged DSNs
// and service commands; it never leaves the host). The plan names the
// pull-point source, the new Library target (PostgreSQL or SQLite),
// the baseline bundle the pre-window rehearsal imported, the
// maintenance lease policy, the config switch, and the staged worker
// re-enable. Every run materializes a RUN DIRECTORY: run.json (the
// gate state machine — the resume mechanism), the freeze delta bundle,
// the delta report, the freeze anchor (the reverse-delta base), the
// config backup, the shadow report, and an append-only audit log.
//
// # Teeth
//
//   - --require-confirmation is load-bearing: without it the command
//     VALIDATES only (plan, connectivity, baseline) and mutates
//     nothing — no run directory, no DDL, no config row, no import.
//   - Gate order is asserted in code: the data import refuses to run
//     unless the target-schema gate completed in THIS run (DM08
//     lesson: the import is data-only; the schema must have been
//     migrated first).
//   - Every lease decision (wait/cancel/abort) is recorded with what
//     was observed, not just what was decided.
//   - The final in-window check is the DM08 shadow-read; a red shadow
//     fails the run before the worker re-enable.
package cutover

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom/internal/databundle"
)

// Plan identity.
const (
	PlanFormat        = "axiom-cutover-plan"
	PlanFormatVersion = 1
)

// StuckLease choices — the documented maintenance-gate decision for an
// active lease found during the freeze.
const (
	LeaseWait   = "wait"   // poll until the lease clears (bounded by WaitTimeoutS)
	LeaseCancel = "cancel" // request cancellation, converge expired rows, wait for drain
	LeaseAbort  = "abort"  // abort the cutover run, listing the leases
)

// Target is the new Library home: exactly one engine.
type Target struct {
	DSN        string `json:"dsn"`         // PostgreSQL target
	SQLitePath string `json:"sqlite_path"` // library.sqlite target
}

// Check is one post-switch verification: an HTTP GET that must return
// ExpectStatus (default 200) within RetryS seconds (poll ~1s), body
// optionally containing ExpectContains.
type Check struct {
	Kind           string `json:"kind"`          // "http"
	URL            string `json:"url"`           //
	ExpectStatus   int    `json:"expect_status"` // 0 = 200
	ExpectContains string `json:"expect_contains,omitempty"`
	RetryS         int    `json:"retry_s,omitempty"` // total budget; 0 = 60
}

// Maintenance is the gate-1 policy: what to stop, where leases live,
// and what to do with a stuck one.
type Maintenance struct {
	DSN           string   `json:"dsn"`             // lease home; "" = StoreDSN = SourceDSN
	StopCommands  []string `json:"stop_commands"`   // sync/dispatcher/repair/backfills
	StuckLease    string   `json:"stuck_lease"`     // wait | cancel | abort
	WaitTimeoutS  int      `json:"wait_timeout_s"`  // drain budget; 0 = 900
	PollIntervalS int      `json:"poll_interval_s"` // 0 = 5
}

// SchemaPlan gates the in-window DDL (both idempotent runners).
type SchemaPlan struct {
	MigrateTarget        bool `json:"migrate_target"`         // core+library migrations on a PG target
	ApplyStoreMigrations bool `json:"apply_store_migrations"` // store set on StoreDSN (the DM06 FK drops)
}

// ConfigSwitch is the atomic DSN/config flip: rows written into
// config.sqlite (validated against the AXIOM_* vocabulary — secret
// keys are refused; DSN VALUES stay env/OS-store, only references and
// non-secret rows live here).
type ConfigSwitch struct {
	Path  string            `json:"path"`            // config.sqlite; "" = configstore.DefaultPath()
	Set   map[string]string `json:"set,omitempty"`   // rows to write (e.g. AXIOM_LIBRARY_SQLITE_PATH, AXIOM_DISPATCHER_ENABLED=0)
	Unset []string          `json:"unset,omitempty"` // rows to remove first
}

// Stage is one staged worker re-enable step: optional config rows,
// optional command (e.g. a service restart), then checks.
type Stage struct {
	Name      string            `json:"name"`
	ConfigSet map[string]string `json:"config_set,omitempty"`
	Command   string            `json:"command,omitempty"`
	Checks    []Check           `json:"checks,omitempty"`
}

// RollbackPlan is the pre-decided reverse path: stop the new stack's
// mutators, restart the old one, and where reverse-delta shadow tables
// land (the legacy side).
type RollbackPlan struct {
	StopCommands   []string `json:"stop_commands,omitempty"`
	ShadowDSN      string   `json:"shadow_dsn,omitempty"` // "" = SourceDSN
	RestartCommand string   `json:"restart_command,omitempty"`
	Checks         []Check  `json:"checks,omitempty"`
}

// Plan is the whole window, operator-authored.
type Plan struct {
	Format          string       `json:"format"`
	FormatVersion   int          `json:"format_version"`
	Component       string       `json:"component"`
	SourceDSN       string       `json:"source_dsn"`
	StoreDSN        string       `json:"store_dsn,omitempty"` // "" = SourceDSN
	Target          Target       `json:"target"`
	BaselineBundle  string       `json:"baseline_bundle"`
	RunsDir         string       `json:"runs_dir"`
	FinalShadow     bool         `json:"final_shadow"`
	CommandTimeoutS int          `json:"command_timeout_s,omitempty"` // 0 = 300
	Maintenance     Maintenance  `json:"maintenance"`
	Schema          SchemaPlan   `json:"schema"`
	ConfigSwitch    ConfigSwitch `json:"config_switch"`
	RestartCommand  string       `json:"restart_command,omitempty"`
	RestartChecks   []Check      `json:"restart_checks,omitempty"`
	ReenableStages  []Stage      `json:"reenable_stages,omitempty"`
	Rollback        RollbackPlan `json:"rollback"`
}

// LoadPlan reads and validates a plan file.
func LoadPlan(path string) (*Plan, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("plan: read %s: %w", path, err)
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("plan: decode %s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate enforces the plan's self-consistency (structure only —
// connectivity and baseline checks live in the dry run).
func (p *Plan) Validate() error {
	if p.Format != PlanFormat {
		return fmt.Errorf("plan: format %q is not %q", p.Format, PlanFormat)
	}
	if p.FormatVersion != PlanFormatVersion {
		return fmt.Errorf("plan: format_version %d is not supported (this build understands %d)", p.FormatVersion, PlanFormatVersion)
	}
	if p.Component != databundle.ComponentLib {
		return fmt.Errorf("plan: component %q — this build cuts over %q only", p.Component, databundle.ComponentLib)
	}
	if p.SourceDSN == "" {
		return fmt.Errorf("plan: source_dsn is required (the pull point)")
	}
	if (p.Target.DSN == "") == (p.Target.SQLitePath == "") {
		return fmt.Errorf("plan: target needs exactly one of dsn / sqlite_path")
	}
	if p.BaselineBundle == "" {
		return fmt.Errorf("plan: baseline_bundle is required (the delta base — the pre-window rehearsal bundle)")
	}
	if p.RunsDir == "" {
		return fmt.Errorf("plan: runs_dir is required (run artifacts are evidence, never ephemeral)")
	}
	switch p.Maintenance.StuckLease {
	case LeaseWait, LeaseCancel, LeaseAbort:
	case "":
		return fmt.Errorf("plan: maintenance.stuck_lease is required (wait | cancel | abort — the choice is documented, not improvised)")
	default:
		return fmt.Errorf("plan: maintenance.stuck_lease %q is not one of wait | cancel | abort", p.Maintenance.StuckLease)
	}
	if p.Maintenance.WaitTimeoutS < 0 || p.Maintenance.PollIntervalS < 0 {
		return fmt.Errorf("plan: maintenance timeouts must be >= 0")
	}
	if p.CommandTimeoutS < 0 {
		return fmt.Errorf("plan: command_timeout_s must be >= 0")
	}
	// Config switch rows: full vocabulary validation up front (unknown
	// key, type violation, secret key — the same teeth `axiom config
	// set` has), so a bad plan fails at load, never mid-window.
	if len(p.ConfigSwitch.Set) > 0 || len(p.ConfigSwitch.Unset) > 0 {
		if problems := config.ValidateSettings(p.ConfigSwitch.Set, nil); len(problems) > 0 {
			return fmt.Errorf("plan: config_switch.set invalid:\n\t%s", strings.Join(problems, "\n\t"))
		}
		for _, k := range p.ConfigSwitch.Unset {
			if _, ok := p.ConfigSwitch.Set[k]; ok {
				return fmt.Errorf("plan: config_switch key %s is both set and unset — resolve to one", k)
			}
			if problems := config.ValidateSettings(map[string]string{k: ""}, nil); len(problems) > 0 {
				// empty value may fail type validation; vocabulary check only
				if !strings.Contains(problems[0], "unknown configuration key") {
					return fmt.Errorf("plan: config_switch.unset key %s: %s", k, problems[0])
				}
			}
		}
	}
	for i, c := range p.RestartChecks {
		if err := c.validate(fmt.Sprintf("restart_checks[%d]", i)); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for i, s := range p.ReenableStages {
		if s.Name == "" {
			return fmt.Errorf("plan: reenable_stages[%d].name is required", i)
		}
		if seen[s.Name] {
			return fmt.Errorf("plan: reenable stage %q appears twice", s.Name)
		}
		seen[s.Name] = true
		if len(s.ConfigSet) > 0 {
			if problems := config.ValidateSettings(s.ConfigSet, nil); len(problems) > 0 {
				return fmt.Errorf("plan: reenable_stages[%d] (%s) config_set invalid:\n\t%s", i, s.Name, strings.Join(problems, "\n\t"))
			}
		}
		for j, c := range s.Checks {
			if err := c.validate(fmt.Sprintf("reenable_stages[%d].checks[%d]", i, j)); err != nil {
				return err
			}
		}
	}
	for i, c := range p.Rollback.Checks {
		if err := c.validate(fmt.Sprintf("rollback.checks[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

func (c Check) validate(where string) error {
	if c.Kind != "http" {
		return fmt.Errorf("plan: %s: kind %q is not \"http\"", where, c.Kind)
	}
	if !strings.HasPrefix(c.URL, "http://") && !strings.HasPrefix(c.URL, "https://") {
		return fmt.Errorf("plan: %s: url must be an http(s) URL", where)
	}
	if c.ExpectStatus < 0 {
		return fmt.Errorf("plan: %s: expect_status must be >= 0", where)
	}
	if c.RetryS < 0 {
		return fmt.Errorf("plan: %s: retry_s must be >= 0", where)
	}
	return nil
}

// EffectiveStoreDSN resolves the store-adoption DSN default chain.
func (p *Plan) EffectiveStoreDSN() string {
	if p.StoreDSN != "" {
		return p.StoreDSN
	}
	return p.SourceDSN
}

// EffectiveMaintenanceDSN resolves the lease-home default chain.
func (p *Plan) EffectiveMaintenanceDSN() string {
	if p.Maintenance.DSN != "" {
		return p.Maintenance.DSN
	}
	return p.EffectiveStoreDSN()
}

// CommandTimeout resolves the shared command-execution budget.
func (p *Plan) CommandTimeout() time.Duration {
	if p.CommandTimeoutS > 0 {
		return time.Duration(p.CommandTimeoutS) * time.Second
	}
	return 300 * time.Second
}

// Digest returns the plan file's content digest (provenance pin for
// run manifests).
func PlanDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum)
}
