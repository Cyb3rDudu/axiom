// run.go — the run directory: the gate state machine (run.json), the
// append-only audit log (audit.ndjson), and the resume mechanics. A
// gate records "started" when it begins mutating and "completed" only
// after its effects are durable (files landed, transactions
// committed) — a crash between the two leaves the gate re-runnable,
// and every gate in this package is idempotent for exactly that.
package cutover

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Run identity.
const (
	RunKindCutover  = "cutover"
	RunKindRollback = "rollback"
)

// Gate names — the runbook's resume-point vocabulary.
const (
	GateMaintenance    = "maintenance"    // stop mutators, drain leases (choice documented)
	GateFreeze         = "freeze"         // final freeze delta + evidence anchor
	GateTargetSchema   = "target-schema"  // migrate the target BEFORE data (asserted order)
	GateDeltaImport    = "delta-import"   // apply the delta bundle + tombstones
	GateStoreAdoption  = "store-adoption" // store migrations on the adopted DB (DM06 FK drops)
	GateConfigSwitch   = "config-switch"  // atomic config.sqlite flip + env compatibility
	GateRestart        = "restart"        // boot without mutating workers + health checks
	GateShadow         = "shadow"         // DM08 shadow re-run — the window's last data check
	GatePrefixReenable = "reenable:"      // staged worker re-enable (":" + stage name)
	GateDone           = "done"

	RBGateMaintenance  = "rb:maintenance"   // stop the new stack's mutators
	RBGateReverse      = "rb:reverse-delta" // pre/post-write decision + shadow apply
	RBGateConfigRevert = "rb:config-revert" // restore the backed-up settings
	RBGateRestart      = "rb:restart"       // restart the old stack + checks
)

// GateRecord is one gate's outcome inside run.json.
type GateRecord struct {
	Gate       string   `json:"gate"`
	Status     string   `json:"status"` // started | completed | failed
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at,omitempty"`
	Note       string   `json:"note,omitempty"`
	Evidence   []string `json:"evidence,omitempty"` // run-dir-relative paths
}

// LeaseDecision documents one maintenance-gate lease encounter: what
// was observed, the plan's choice, and the outcome.
type LeaseDecision struct {
	JobID         string `json:"job_id"`
	Status        string `json:"status"`
	ClaimedBy     string `json:"claimed_by,omitempty"`
	LeaseUntil    string `json:"lease_until,omitempty"`
	HeartbeatAgeS int64  `json:"heartbeat_age_s,omitempty"`
	Choice        string `json:"choice"`  // wait | cancel | abort
	Action        string `json:"action"`  // what the gate did
	Outcome       string `json:"outcome"` // drained | cancelled | aborted | timeout
	At            string `json:"at"`
}

// SwitchRecord documents the config switch: keys and value digests,
// never values (DSN-adjacent rows stay out of artifacts).
type SwitchRecord struct {
	Path       string            `json:"path"`
	Unset      []string          `json:"unset,omitempty"`
	Set        map[string]string `json:"set,omitempty"` // key -> sha256/12 of value
	BackupFile string            `json:"backup_file,omitempty"`
	EnvCompat  []EnvVerdict      `json:"env_compat,omitempty"`
	At         string            `json:"at"`
}

// EnvVerdict is the per-key environment compatibility check.
type EnvVerdict struct {
	Key  string `json:"key"`
	Env  string `json:"env"` // absent | equal | conflict
	Note string `json:"note,omitempty"`
}

// RunManifest is run.json — the whole run's bookkeeping.
type RunManifest struct {
	RunID        string       `json:"run_id"`
	Kind         string       `json:"kind"`
	Status       string       `json:"status"` // running | completed | aborted
	PlanPath     string       `json:"plan_path"`
	PlanDigest   string       `json:"plan_digest"`
	PlanSnapshot string       `json:"plan_snapshot"` // run-dir-relative copy
	CreatedAt    string       `json:"created_at"`
	UpdatedAt    string       `json:"updated_at"`
	Gates        []GateRecord `json:"gates"`

	// Cutover evidence anchors (run-dir-relative).
	FreezeBundleDir string           `json:"freeze_bundle_dir,omitempty"`
	DeltaReportFile string           `json:"delta_report_file,omitempty"`
	AnchorFile      string           `json:"anchor_file,omitempty"`
	ConfigBackup    string           `json:"config_backup,omitempty"`
	ShadowReport    string           `json:"shadow_report,omitempty"`
	Tombstones      map[string]int64 `json:"tombstones,omitempty"`

	// Lease + switch bookkeeping.
	LeaseDecisions []LeaseDecision `json:"lease_decisions,omitempty"`
	Switch         *SwitchRecord   `json:"switch,omitempty"`
	// Witness is the maintenance gate's mutation probe (the freeze
	// re-verifies it; persisted so a resume between the two gates keeps
	// the original observation).
	Witness *WitnessRecord `json:"witness,omitempty"`

	// Rollback bookkeeping (rb runs, same directory).
	Rollback *RollbackRecord `json:"rollback,omitempty"`
}

// WitnessRecord is the persisted job-table mutation witness.
type WitnessRecord struct {
	Count        int64  `json:"count"`
	MaxUpdatedAt string `json:"max_updated_at,omitempty"`
	ActiveLeases int    `json:"active_leases"`
}

// RollbackRecord is the rollback side of a run directory.
type RollbackRecord struct {
	Path          string       `json:"path"` // rollback.json (this record's file)
	Status        string       `json:"status"`
	Mode          string       `json:"mode,omitempty"` // pre-write | post-write
	ReverseBundle string       `json:"reverse_bundle,omitempty"`
	ReverseReport string       `json:"reverse_report,omitempty"`
	Gates         []GateRecord `json:"gates"`
	StartedAt     string       `json:"started_at"`
	UpdatedAt     string       `json:"updated_at"`
}

// run is the live run handle: the manifest, its directory, and the
// serialization around durable state transitions.
type run struct {
	mu      sync.Mutex
	auditMu sync.Mutex
	dir     string
	man     *RunManifest
	audits  *os.File
	// startedAtLoad snapshots which gates had begun when this process
	// loaded the run — the fresh-entry vs resume discriminator.
	startedAtLoad map[string]bool
}

// nowTS is the canonical timestamp form inside run artifacts.
func nowTS() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// newRun creates a fresh run directory (run.json, plan snapshot, audit
// log). Run IDs are unique per invocation.
func newRun(planPath string, planRaw []byte, runsDir, kind string) (*run, error) {
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		return nil, fmt.Errorf("run: create runs dir: %w", err)
	}
	id := fmt.Sprintf("%s-%s", kind, time.Now().UTC().Format("20060102-150405"))
	dir := filepath.Join(runsDir, id)
	for i := 0; ; i++ {
		if err := os.Mkdir(dir, 0o700); err == nil {
			break
		} else if !os.IsExist(err) {
			return nil, fmt.Errorf("run: create run dir: %w", err)
		}
		dir = filepath.Join(runsDir, fmt.Sprintf("%s-%d", id, i))
	}
	r := &run{dir: dir, man: &RunManifest{
		RunID: filepath.Base(dir), Kind: kind, Status: "running",
		PlanPath: planPath, PlanDigest: PlanDigest(planRaw),
		CreatedAt: nowTS(), UpdatedAt: nowTS(),
	}}
	snap := filepath.Join(dir, "plan.json")
	if err := writeAtomic(snap, planRaw, 0o600); err != nil {
		return nil, fmt.Errorf("run: plan snapshot: %w", err)
	}
	r.man.PlanSnapshot = "plan.json"
	f, err := os.OpenFile(filepath.Join(dir, "audit.ndjson"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("run: audit log: %w", err)
	}
	r.audits = f
	if err := r.save(); err != nil {
		return nil, err
	}
	return r, nil
}

// loadRun reopens an existing run directory (resume / rollback).
func loadRun(dir string) (*run, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return nil, fmt.Errorf("run: read %s: %w", filepath.Join(dir, "run.json"), err)
	}
	var man RunManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return nil, fmt.Errorf("run: decode run.json: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "audit.ndjson"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("run: audit log: %w", err)
	}
	r := &run{dir: dir, man: &man, audits: f, startedAtLoad: map[string]bool{}}
	for _, g := range man.Gates {
		if g.Status != "" {
			r.startedAtLoad[g.Gate] = true
		}
	}
	return r, nil
}

// loadRollback opens the rollback side of a run directory.
func (r *run) loadRollback() (*RollbackRecord, error) {
	raw, err := os.ReadFile(filepath.Join(r.dir, "rollback.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rec RollbackRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (r *run) saveRollback(rec *RollbackRecord) error {
	rec.UpdatedAt = nowTS()
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(r.dir, "rollback.json"), append(b, '\n'), 0o600)
}

// save lands run.json atomically.
func (r *run) save() error {
	r.man.UpdatedAt = nowTS()
	b, err := json.MarshalIndent(r.man, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(r.dir, "run.json"), append(b, '\n'), 0o600)
}

// audit appends one event line to audit.ndjson.
func (r *run) audit(gate, event, note string, evidence ...string) {
	ev := map[string]any{
		"at": nowTS(), "run": r.man.RunID, "kind": r.man.Kind,
		"gate": gate, "event": event, "note": note, "evidence": evidence,
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	r.auditMu.Lock()
	defer r.auditMu.Unlock()
	if r.audits != nil {
		_, _ = r.audits.Write(append(b, '\n'))
	}
}

// completed reports whether a gate completed in this run.
func (r *run) completed(gate string) bool {
	for _, g := range r.man.Gates {
		if g.Gate == gate && g.Status == "completed" {
			return true
		}
	}
	return false
}

// started reports whether a gate started (even if it failed after).
func (r *run) started(gate string) bool {
	for _, g := range r.man.Gates {
		if g.Gate == gate && g.Status != "" {
			return true
		}
	}
	return false
}

// startedBefore reports whether a gate had already begun when this
// process loaded the run (the precheck discriminator: a resumed gate
// is legitimately mid-state).
func (r *run) startedBefore(gate string) bool {
	return r.startedAtLoad[gate]
}

// begin marks a gate started (re-entry keeps the original start time).
func (r *run) begin(gate string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.man.Gates {
		if r.man.Gates[i].Gate == gate {
			if r.man.Gates[i].Status == "completed" {
				return
			}
			r.man.Gates[i].Status = "started"
			_ = r.save()
			return
		}
	}
	r.man.Gates = append(r.man.Gates, GateRecord{Gate: gate, Status: "started", StartedAt: nowTS()})
	_ = r.save()
}

// finish marks a gate completed/failed with its note + evidence.
func (r *run) finish(gate, status, note string, evidence ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.man.Gates {
		if r.man.Gates[i].Gate == gate {
			r.man.Gates[i].Status = status
			r.man.Gates[i].FinishedAt = nowTS()
			r.man.Gates[i].Note = note
			r.man.Gates[i].Evidence = append(r.man.Gates[i].Evidence, evidence...)
			_ = r.save()
			r.audit(gate, status, note, evidence...)
			return
		}
	}
	rec := GateRecord{Gate: gate, Status: status, StartedAt: nowTS(), FinishedAt: nowTS(), Note: note, Evidence: evidence}
	r.man.Gates = append(r.man.Gates, rec)
	_ = r.save()
	r.audit(gate, status, note, evidence...)
}

// writeAtomic lands file content tmp+rename with the given mode.
func writeAtomic(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// mkdirFor ensures a file's parent directory exists.
func mkdirFor(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o700)
}
