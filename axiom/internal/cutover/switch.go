// switch.go — the atomic DSN/config flip and its revert. Rows land in
// config.sqlite through the same validated writer `axiom config set`
// uses; the pre-switch state is backed up into the run directory ONCE
// (a resume never re-snapshots mid-state); the environment
// compatibility check runs before any write: the chain's own rule
// (env beats file) means a service environment pinning a key we are
// switching would silently mask the switch — the runbook precondition
// is that the cutover executes in the services' env class, and the
// check enforces what that env must look like from here.
package cutover

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"

	"github.com/Cyb3rDudu/axiom/axiom/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom/internal/config/configstore"
)

// ConfigBackup is the pre-switch state snapshot (run-dir artifact).
type ConfigBackup struct {
	Format     string            `json:"format"` // axiom-cutover-config-backup
	Path       string            `json:"path"`
	Found      bool              `json:"found"`
	Values     map[string]string `json:"values,omitempty"`
	SecretRefs map[string]string `json:"secret_refs,omitempty"`
}

// switchEnvCompat verifies the process environment does not conflict
// with the planned rows (equal values are the harmless overlap and
// pass — the LoadResolved rule). Returns per-key verdicts for the run
// manifest; never values.
func switchEnvCompat(s ConfigSwitch) ([]EnvVerdict, error) {
	var out []EnvVerdict
	conflicts := 0
	touched := map[string]bool{}
	for k := range s.Set {
		touched[k] = true
	}
	for _, k := range s.Unset {
		touched[k] = true
	}
	for _, key := range sortedKeysOf(touched) {
		v := EnvVerdict{Key: key, Env: "absent"}
		if cur, ok := os.LookupEnv(key); ok && cur != "" {
			if planned, isSet := s.Set[key]; isSet && planned == cur {
				v.Env = "equal"
				v.Note = "environment already carries the planned value (harmless overlap)"
			} else {
				v.Env = "conflict"
				v.Note = "environment pins this key — the file row would be masked by the env stage (chain: flag > env > file); unset it or align it before the switch"
				conflicts++
			}
		}
		out = append(out, v)
	}
	if conflicts > 0 {
		return out, fmt.Errorf("config switch: %d planned key(s) are pinned by the environment — resolve before the window (see env_compat in run.json)", conflicts)
	}
	return out, nil
}

func sortedKeysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// backupConfig snapshots the pre-switch state (idempotent per run: a
// resume keeps the FIRST snapshot — the honest pre-state).
func backupConfig(runDir, explicitPath string) (string, *ConfigBackup, error) {
	path := explicitPath
	if path == "" {
		p, err := configstore.DefaultPath()
		if err != nil {
			return "", nil, fmt.Errorf("config switch: no config.sqlite path (plan or AXIOM_CONFIG_PATH): %w", err)
		}
		path = p
	}
	backupPath := runDir + "/config-backup.json"
	if _, err := os.Stat(backupPath); err == nil {
		b, err := readBackup(backupPath)
		return backupPath, b, err
	}
	settings, found, err := configstore.Read(path)
	if err != nil {
		return "", nil, fmt.Errorf("config switch: read %s: %w", path, err)
	}
	b := &ConfigBackup{Format: "axiom-cutover-config-backup", Path: path, Found: found,
		Values: settings.Values, SecretRefs: settings.SecretRefs}
	if b.Values == nil {
		b.Values = map[string]string{}
	}
	if b.SecretRefs == nil {
		b.SecretRefs = map[string]string{}
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return "", nil, err
	}
	if err := writeAtomic(backupPath, append(raw, '\n'), 0o600); err != nil {
		return "", nil, err
	}
	return backupPath, b, nil
}

func readBackup(path string) (*ConfigBackup, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b ConfigBackup
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// applySwitch lands the switch: unsets first, then the set rows in one
// transaction (configstore.SetAll). Values are validated (again)
// before any write.
func applySwitch(s ConfigSwitch) error {
	if problems := config.ValidateSettings(s.Set, nil); len(problems) > 0 {
		return fmt.Errorf("config switch: set rows invalid:\n\t%s", joinStrings(problems, "\n\t"))
	}
	st, err := configstore.Open(s.Path)
	if err != nil {
		return fmt.Errorf("config switch: open %s: %w", s.Path, err)
	}
	defer st.Close()
	for _, k := range s.Unset {
		if err := st.Unset(k); err != nil {
			return fmt.Errorf("config switch: unset %s: %w", k, err)
		}
	}
	if len(s.Set) > 0 {
		if err := st.SetAll(s.Set, nil); err != nil {
			return fmt.Errorf("config switch: set rows: %w", err)
		}
	}
	return nil
}

// revertConfig restores the backed-up state: keys the backup carried
// are rewritten; keys the switch touched that the backup lacked are
// removed — the file converges to the pre-switch rows exactly.
func revertConfig(b *ConfigBackup, sw *SwitchRecord) error {
	st, err := configstore.Open(b.Path)
	if err != nil {
		return fmt.Errorf("config revert: open %s: %w", b.Path, err)
	}
	defer st.Close()
	touched := map[string]bool{}
	for k := range sw.Set {
		touched[k] = true
	}
	for _, k := range sw.Unset {
		touched[k] = true
	}
	for _, k := range sortedKeysOf(touched) {
		if _, ok := b.Values[k]; !ok {
			if _, ok := b.SecretRefs[k]; !ok {
				if err := st.Unset(k); err != nil {
					return fmt.Errorf("config revert: unset %s: %w", k, err)
				}
			}
		}
	}
	if len(b.Values) > 0 || len(b.SecretRefs) > 0 {
		if err := st.SetAll(b.Values, b.SecretRefs); err != nil {
			return fmt.Errorf("config revert: restore rows: %w", err)
		}
	}
	return nil
}

// digestValue is the run-manifest spelling of a row value: digest,
// never the value.
func digestValue(v string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(v)))[:12]
}

func joinStrings(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
