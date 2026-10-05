// cutover_test.go — the DB-free teeth: plan validation, delta
// arithmetic (added/changed/tombstoned), catalog-drift refusal, the
// pre/post-write discriminator, env compatibility, and the config
// backup→switch→revert convergence over a real (temp) config.sqlite.
package cutover

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom/internal/config/configstore"
	"github.com/Cyb3rDudu/axiom/axiom/internal/databundle"
)

func TestPlanValidationTeeth(t *testing.T) {
	base := func() Plan {
		return Plan{
			Format: PlanFormat, FormatVersion: PlanFormatVersion, Component: "library",
			SourceDSN: "postgresql://x/y", Target: Target{DSN: "postgresql://x/z"},
			BaselineBundle: "/tmp/b", RunsDir: t.TempDir(),
			Maintenance: Maintenance{StuckLease: LeaseAbort},
		}
	}
	cases := []struct {
		name string
		mut  func(*Plan)
		want string
	}{
		{"format", func(p *Plan) { p.Format = "nope" }, "format"},
		{"version", func(p *Plan) { p.FormatVersion = 2 }, "format_version"},
		{"component", func(p *Plan) { p.Component = "store" }, "component"},
		{"no source", func(p *Plan) { p.SourceDSN = "" }, "source_dsn"},
		{"two targets", func(p *Plan) { p.Target.SQLitePath = "/tmp/x.sqlite" }, "exactly one"},
		{"no baseline", func(p *Plan) { p.BaselineBundle = "" }, "baseline_bundle"},
		{"no runs dir", func(p *Plan) { p.RunsDir = "" }, "runs_dir"},
		{"no lease choice", func(p *Plan) { p.Maintenance.StuckLease = "" }, "stuck_lease"},
		{"bad lease choice", func(p *Plan) { p.Maintenance.StuckLease = "pray" }, "wait | cancel | abort"},
		{"set+unset overlap", func(p *Plan) {
			p.ConfigSwitch = ConfigSwitch{Set: map[string]string{"AXIOM_DISPATCHER_ENABLED": "0"}, Unset: []string{"AXIOM_DISPATCHER_ENABLED"}}
		}, "both set and unset"},
		{"unknown config key", func(p *Plan) {
			p.ConfigSwitch = ConfigSwitch{Set: map[string]string{"AXIOM_NOT_A_THING": "1"}}
		}, "config_switch.set invalid"},
		{"secret key refused", func(p *Plan) {
			p.ConfigSwitch = ConfigSwitch{Set: map[string]string{"AXIOM_STORE_DATABASE_URL": "postgresql://secret"}}
		}, "config_switch.set invalid"},
		{"bad check kind", func(p *Plan) {
			p.RestartChecks = []Check{{Kind: "tcp", URL: "http://x"}}
		}, `kind "tcp" is not "http"`},
		{"bad check url", func(p *Plan) {
			p.RestartChecks = []Check{{Kind: "http", URL: "ftp://x"}}
		}, "http(s) URL"},
		{"stage without name", func(p *Plan) {
			p.ReenableStages = []Stage{{}}
		}, "name is required"},
		{"duplicate stage", func(p *Plan) {
			p.ReenableStages = []Stage{{Name: "a"}, {Name: "a"}}
		}, "appears twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base()
			tc.mut(&p)
			err := (&p).Validate()
			if err == nil {
				t.Fatalf("expected a validation error mentioning %q", tc.want)
			}
			if !contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
	if bp := base(); bp.Validate() != nil {
		t.Fatalf("the base plan must validate: %v", bp.Validate())
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// idx builds a synthetic table index.
func idx(table string, rows map[string]string) *databundle.SourceIndex {
	return &databundle.SourceIndex{Tables: map[string]*databundle.TableIndex{
		table: {
			Table: table, Key: []string{"id"},
			Columns: []databundle.ColumnRef{{Name: "id", Type: "text"}, {Name: "v", Type: "text"}},
			Rows:    rows,
		},
	}}
}

func TestTableDeltaArithmetic(t *testing.T) {
	base := idx("t", map[string]string{
		`"1"`: `{"id":"1","v":"a"}`, // unchanged
		`"2"`: `{"id":"2","v":"b"}`, // changed
		`"3"`: `{"id":"3","v":"c"}`, // tombstoned
	})
	cur := idx("t", map[string]string{
		`"1"`: `{"id":"1","v":"a"}`,
		`"2"`: `{"id":"2","v":"B"}`,
		`"4"`: `{"id":"4","v":"d"}`, // added
	})
	carry, tombs, td, err := tableDelta("t", base.Tables["t"], cur.Tables["t"])
	if err != nil {
		t.Fatal(err)
	}
	if td.Added != 1 || td.Changed != 1 || td.Tombstoned != 1 || td.Unchanged != 1 {
		t.Fatalf("counts wrong: %+v", td)
	}
	if len(carry) != 2 || len(tombs) != 1 || tombs[0] != `"3"` {
		t.Fatalf("carry=%v tombs=%v", carry, tombs)
	}
}

func TestTableDeltaCatalogDriftRefused(t *testing.T) {
	a := idx("t", nil)
	b := idx("t", nil)
	b.Tables["t"].Columns = []databundle.ColumnRef{{Name: "id", Type: "uuid"}, {Name: "w", Type: "text"}}
	if _, _, _, err := tableDelta("t", a.Tables["t"], b.Tables["t"]); err == nil || !contains(err.Error(), "drifted") {
		t.Fatalf("catalog drift must abort, got %v", err)
	}
}

// TestReverseDeltaAgainstAnchor — the pre/post-write discriminator and
// the reverse-delta row selection, entirely over synthetic indexes.
func TestReverseDeltaAgainstAnchor(t *testing.T) {
	dir := t.TempDir()
	// baseline manifest with one table (columns/key), so writeDeltaBundle
	// can produce a manifest-complete bundle.
	man := &databundle.Manifest{
		Format: databundle.FormatName, FormatVersion: databundle.FormatVersion, Component: "library",
		Tables: []databundle.TableManifest{{
			Name: "t", Key: []string{"id"},
			Columns: []databundle.ColumnManifest{
				{Name: "id", Type: "text"}, {Name: "v", Type: "text"},
			},
		}},
	}
	if err := databundle.WriteManifestDir(dir, man); err != nil {
		t.Fatal(err)
	}
	anchorIdx := idx("t", map[string]string{
		`"1"`: `{"id":"1","v":"a"}`,
		`"2"`: `{"id":"2","v":"b"}`,
		`"3"`: `{"id":"3","v":"c"}`, // dropped post-cutover — tombstone
	})
	anchor, err := writeAnchor(filepath.Join(dir, "anchor.json"), "run-x", anchorIdx)
	if err != nil {
		t.Fatal(err)
	}

	// Pre-write: identical target → zero diff.
	if n := diffAgainstAnchor(anchor, anchorIdx); n != 0 {
		t.Fatalf("identical target must diff zero, got %d", n)
	}

	// Post-write: one changed, one added, one deleted.
	post := idx("t", map[string]string{
		`"1"`: `{"id":"1","v":"A"}`, // changed
		`"2"`: `{"id":"2","v":"b"}`, // unchanged
		`"9"`: `{"id":"9","v":"z"}`, // added
	})
	if n := diffAgainstAnchor(anchor, post); n != 2 {
		t.Fatalf("changed+added must diff 2 (tombstones do not count as target drift), got %d", n)
	}
	revDir := filepath.Join(dir, "rev")
	rep, err := reverseDelta(revDir, anchor, post, man, "run-x", nil)
	if err != nil {
		t.Fatal(err)
	}
	var td *TableDelta
	for i := range rep.Tables {
		if rep.Tables[i].Table == "t" {
			td = &rep.Tables[i]
		}
	}
	if td == nil {
		t.Fatal("reverse report lacks table t")
	}
	if td.Added != 1 || td.Changed != 1 || td.Tombstoned != 1 {
		t.Fatalf("reverse delta counts wrong: %+v", td)
	}
	// The reverse bundle carries exactly the changed+added rows.
	revIdx, _, err := databundle.IndexBundle(context.Background(), revDir)
	if err != nil {
		t.Fatal(err)
	}
	rows := revIdx.Tables["t"].Rows
	if len(rows) != 2 {
		t.Fatalf("reverse bundle carries %d rows, want 2", len(rows))
	}
	if _, ok := rows[`"1"`]; !ok {
		t.Fatal("changed row missing from reverse bundle")
	}
	if _, ok := rows[`"9"`]; !ok {
		t.Fatal("added row missing from reverse bundle")
	}
}

func TestSwitchEnvCompat(t *testing.T) {
	t.Setenv("AXIOM_DISPATCHER_ENABLED", "1")
	t.Setenv("AXIOM_ZOTERO_BASE", "https://zotero.example")
	s := ConfigSwitch{Set: map[string]string{
		"AXIOM_DISPATCHER_ENABLED": "0",                      // conflict
		"AXIOM_ZOTERO_BASE":        "https://zotero.example", // equal — harmless overlap
	}, Unset: []string{"AXIOM_F14_SPLIT"}} // absent
	verdicts, err := switchEnvCompat(s)
	if err == nil {
		t.Fatal("conflicting env must abort the switch")
	}
	byKey := map[string]EnvVerdict{}
	for _, v := range verdicts {
		byKey[v.Key] = v
	}
	if byKey["AXIOM_DISPATCHER_ENABLED"].Env != "conflict" ||
		byKey["AXIOM_ZOTERO_BASE"].Env != "equal" ||
		byKey["AXIOM_F14_SPLIT"].Env != "absent" {
		t.Fatalf("verdicts wrong: %+v", verdicts)
	}
}

func TestConfigBackupSwitchRevertConverges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.sqlite")
	runDir := t.TempDir()

	// Pre-state: one row.
	st, err := configstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("AXIOM_ZOTERO_BASE", "prod-idx"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	backupPath, backup, err := backupConfig(runDir, path)
	if err != nil {
		t.Fatal(err)
	}
	if !backup.Found || backup.Values["AXIOM_ZOTERO_BASE"] != "prod-idx" {
		t.Fatalf("backup wrong: %+v", backup)
	}

	// Switch: change one, add one.
	sw := ConfigSwitch{Path: path,
		Set:   map[string]string{"AXIOM_ZOTERO_BASE": "new-idx", "AXIOM_DISPATCHER_ENABLED": "0"},
		Unset: []string{}}
	if err := applySwitch(sw); err != nil {
		t.Fatal(err)
	}
	rec := &SwitchRecord{Set: map[string]string{"AXIOM_ZOTERO_BASE": "x", "AXIOM_DISPATCHER_ENABLED": "y"}, Unset: sw.Unset}
	if err := revertConfig(backup, rec); err != nil {
		t.Fatal(err)
	}

	after, found, err := configstore.Read(path)
	if err != nil || !found {
		t.Fatal(err)
	}
	if after.Values["AXIOM_ZOTERO_BASE"] != "prod-idx" {
		t.Fatalf("revert did not restore the original value: %+v", after.Values)
	}
	if _, ok := after.Values["AXIOM_DISPATCHER_ENABLED"]; ok {
		t.Fatal("revert left a row the backup did not carry")
	}
	_ = backupPath
}

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "f.json")
	if err := writeAtomic(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "x" {
		t.Fatalf("atomic write failed: %v %q", err, b)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

func TestRunManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	raw, _ := json.Marshal(Plan{Format: PlanFormat})
	r, err := newRun("/plan.json", raw, dir, RunKindCutover)
	if err != nil {
		t.Fatal(err)
	}
	r.begin(GateMaintenance)
	r.finish(GateMaintenance, "completed", "note")
	r2, err := loadRun(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.completed(GateMaintenance) {
		t.Fatal("completed gate did not survive the round trip")
	}
	if r2.startedBefore(GateMaintenance) != true {
		t.Fatal("startedAtLoad must include the pre-load gate")
	}
	if r2.startedBefore(GateFreeze) {
		t.Fatal("startedAtLoad must not include an untouched gate")
	}
}
