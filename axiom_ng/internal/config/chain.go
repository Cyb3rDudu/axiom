// chain.go — the F13 #307 resolution chain: CLI flag > env override >
// config.sqlite > default. LoadResolved is the ONE entry the runtime
// surfaces (serve, doctor, config) resolve through; config.Load stays
// the pure env reader (byte-identical behavior — the env-only
// container path).
//
// Mechanism: the file layer and the flag layer are MATERIALIZED into
// the process environment (os.Setenv) before Load() runs — every typed
// reader in config.go (envInt, envDur, the dual-fed compute-worker
// resolvers, the deprecation witnesses on legacy spellings) then
// resolves exactly as if the operator had exported the value, and the
// chain order is the materialization order: env keys the environment
// already owns are never overwritten by the file; flags overwrite both.
// Materialization is deliberate process state, not a leak: after boot
// the process environment IS the resolved configuration (child
// processes and the legacy mode surface inherit the same truth).
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config/configstore"
)

// emptyDisablesKeys: keys whose loader reads SET-but-empty as a
// meaningful state (envEmptyDisables) — for these, an env var that is
// present and empty IS an override (the documented drainer disable);
// for every other key an empty value is unset semantics. A source-parse
// guard in chain_test.go fails when config.go grows a second
// envEmptyDisables key that this set does not know.
var emptyDisablesKeys = map[string]bool{"AXIOM_OPENSEARCH_URL": true}

// Chain is the per-key provenance of one resolved configuration: which
// stage fed each key (flag > env > file > default). The zero Chain
// renders the env-only view.
type Chain struct {
	// Flag: keys fed by a --set flag.
	Flag map[string]bool
	// File: keys fed by a config.sqlite row (materialized because the
	// environment did not own them).
	File map[string]bool
	// envSet: keys the ENVIRONMENT fed (LookupEnv present AND a
	// meaningful value — non-empty, or the set-empty disable state).
	envSet map[string]bool
}

// Source stage labels (the `config get --effective` source column).
const (
	SourceFlag    = "flag"
	SourceEnv     = "env"
	SourceFile    = "file"
	SourceDefault = "default"
)

// rowsByKey indexes envRows by environment key (vocabulary + kind
// lookup for the file and flag layers — one table drives everything).
var rowsByKey = func() map[string]envRow {
	m := make(map[string]envRow, len(envRows))
	for _, row := range envRows {
		m[row.env] = row
	}
	return m
}()

// LoadResolved resolves the full chain flag > env > file > default and
// returns the configuration plus the provenance chain (for the
// effective view). A missing config.sqlite is the documented env-only
// bootstrap (container path); a PRESENT but invalid file — foreign
// tables, secret VALUES, unknown keys, unparseable values — is a loud
// error, never a silent fall-back-to-defaults.
//
// Contract: ONE resolution per process (every runtime surface resolves
// exactly once at its entry). Materialized values persist in the
// process environment by design — a second LoadResolved in the same
// process would read the first one's materializations as env. Tests
// that resolve repeatedly snapshot/restore the environment.
func LoadResolved(flags map[string]string) (Config, Chain, error) {
	path, err := configstore.DefaultPath()
	if err != nil {
		return Config{}, Chain{}, err
	}
	settings, found, err := configstore.Read(path)
	if err != nil {
		return Config{}, Chain{}, err
	}
	if found {
		if problems := ValidateSettings(settings.Values, settings.SecretRefs); len(problems) > 0 {
			return Config{}, Chain{}, fmt.Errorf("config.sqlite %s invalid:\n\t%s", path, strings.Join(problems, "\n\t"))
		}
	}
	if problems := ValidateFlags(flags); len(problems) > 0 {
		return Config{}, Chain{}, fmt.Errorf("--set invalid:\n\t%s", strings.Join(problems, "\n\t"))
	}

	ch := Chain{
		Flag:   map[string]bool{},
		File:   map[string]bool{},
		envSet: snapshotEnvStages(),
	}
	for key, value := range settings.Values {
		if ch.envSet[key] {
			continue // the environment owns this key — env beats file
		}
		if err := os.Setenv(key, value); err != nil {
			return Config{}, Chain{}, fmt.Errorf("config: materialize %s: %w", key, err)
		}
		ch.File[key] = true
	}
	for key, value := range flags {
		if err := os.Setenv(key, value); err != nil {
			return Config{}, Chain{}, fmt.Errorf("config: materialize flag %s: %w", key, err)
		}
		ch.Flag[key] = true
	}
	return Load(), ch, nil
}

// snapshotEnvStages records which keys the environment feeds BEFORE any
// materialization: present AND meaningful (non-empty, or the set-empty
// disable state). A set-but-empty var on any other key is unset
// semantics to the loader and lets the file feed the key.
func snapshotEnvStages() map[string]bool {
	out := map[string]bool{}
	for key := range rowsByKey {
		v, ok := os.LookupEnv(key)
		if ok && (v != "" || emptyDisablesKeys[key]) {
			out[key] = true
		}
	}
	return out
}

// stage resolves which chain stage fed one table row (the source
// column's truth for the chain view).
func (ch Chain) stage(row envRow) string {
	stageOf := func(key string) string {
		switch {
		case ch.Flag[key]:
			return SourceFlag
		case ch.envSet[key]:
			return SourceEnv
		case ch.File[key]:
			return SourceFile
		default:
			return SourceDefault
		}
	}
	// Dual-fed fields generalize the F08/F10 resolver truth: the
	// canonical spelling's stage wins when it fed the value; the legacy
	// row carries its own stage only when canonical stayed default.
	if pair, dual := dualFedEnv[row.field]; dual {
		canonicalStage := stageOf(pair[0])
		if row.env == pair[0] {
			return canonicalStage
		}
		if canonicalStage != SourceDefault {
			return SourceDefault // shadowed legacy spelling
		}
		return stageOf(pair[1])
	}
	return stageOf(row.env)
}

// ParseSetFlags extracts every --set KEY=VALUE pair from args (both
// "--set K=V" and "--set=K=V" shapes) and returns the flag pairs plus
// the remaining arguments in order. Syntax errors (a dangling --set, a
// token without '=') are reported; KEY-level validation is
// ValidateFlags.
func ParseSetFlags(args []string) (map[string]string, []string, error) {
	flags := map[string]string{}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		var pair string
		switch {
		case a == "--set":
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("--set needs a KEY=VALUE argument")
			}
			i++
			pair = args[i]
		case strings.HasPrefix(a, "--set="):
			pair = strings.TrimPrefix(a, "--set=")
		default:
			rest = append(rest, a)
			continue
		}
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return nil, nil, fmt.Errorf("--set %q is not KEY=VALUE", pair)
		}
		flags[key] = value
	}
	return flags, rest, nil
}

// ValidateSettings checks config.sqlite rows against the vocabulary:
// known key, parseable value, no secret VALUES (references only),
// legal secret-ref sources, and no empty values where empty is unset
// semantics. The shared per-value rules live in checkRawValue.
func ValidateSettings(values, secretRefs map[string]string) []string {
	var problems []string
	for key, value := range values {
		row, known := rowsByKey[key]
		switch {
		case !known:
			problems = append(problems, fmt.Sprintf("%s: unknown key (not part of the AXIOM_* vocabulary — refused, never silently ignored)", key))
			continue
		case row.secret:
			problems = append(problems, fmt.Sprintf("%s: secret keys carry references, never values — the value stays in the OS secret store / environment", key))
			continue
		case value == "" && !emptyDisablesKeys[key]:
			problems = append(problems, fmt.Sprintf("%s: empty value is unset semantics here — unset the key instead", key))
			continue
		}
		problems = append(problems, checkRawValue(row, value)...)
	}
	for key, source := range secretRefs {
		row, known := rowsByKey[key]
		switch {
		case !known:
			problems = append(problems, fmt.Sprintf("%s: unknown key (not part of the AXIOM_* vocabulary)", key))
		case !row.secret:
			problems = append(problems, fmt.Sprintf("%s: not a secret key — set the value, not a reference", key))
		case source != "env":
			problems = append(problems, fmt.Sprintf("%s: unknown secret-ref source %q (known: env)", key, source))
		}
	}
	return problems
}

// ValidateFlags checks --set pairs: known key, parseable value — and
// refuses SECRET keys outright (a command line is visible in shell
// history and process listings; secrets stay in the environment).
func ValidateFlags(flags map[string]string) []string {
	var problems []string
	for key, value := range flags {
		row, known := rowsByKey[key]
		switch {
		case !known:
			problems = append(problems, fmt.Sprintf("%s: unknown key (not part of the AXIOM_* vocabulary — refused, never silently ignored)", key))
			continue
		case row.secret:
			problems = append(problems, fmt.Sprintf("%s: secret keys never ride a command line (history/ps) — keep the value in the environment / OS secret store", key))
			continue
		case value == "" && !emptyDisablesKeys[key]:
			problems = append(problems, fmt.Sprintf("%s: empty value is unset semantics here", key))
			continue
		}
		problems = append(problems, checkRawValue(row, value)...)
	}
	return problems
}

// ModeEnv is the legacy KG-mode surfaces' env read, relocated behind
// the sanctioned config package (the usage-lint abatement): the mode
// knobs (AXIOM_RETENTION_*) are not part of the envRows vocabulary and
// stay env-only; the read itself lives where env reads live.
func ModeEnv(key string) string { return os.Getenv(key) }
