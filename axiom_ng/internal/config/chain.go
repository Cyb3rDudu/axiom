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
	"sort"
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
// renders the provenance-free view — every key default; the env-only
// truth (set-vs-default per the loader's own semantics) lives in
// envOnlySource, a deliberately different rule (see its comment).
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
// Field-level decision (dual-fed pairs): the chain binds the LOGICAL
// knob — flag > env > file holds per FIELD, and the resolver's
// canonical-over-legacy spelling precedence breaks ties WITHIN one
// stage, never across stages. Materialization is therefore pair-aware:
// a file row on either spelling of a pair is skipped when the
// environment owns the other spelling (env beats file per field), and
// a --set naming either spelling materializes onto the CANONICAL key
// (the flag beats env and file per field).
//
// An unresolvable DEFAULT path (no home dir, no override) degrades to
// the env-only bootstrap instead of aborting — the pre-F13 boot worked
// without $HOME and the chain must not regress that. Writers (config
// set / import-env) keep the loud path error: they NEED a path.
//
// Contract: ONE resolution per process (every runtime surface resolves
// exactly once at its entry). Materialized values persist in the
// process environment by design — a second LoadResolved in the same
// process would read the first one's materializations as env. Tests
// that resolve repeatedly snapshot/restore the environment.
func LoadResolved(flags map[string]string) (Config, Chain, error) {
	path, err := configstore.DefaultPath()
	if err != nil {
		path = "" // no resolvable default path → env-only bootstrap
	}
	settings, found := configstore.Settings{}, false
	if path != "" {
		settings, found, err = configstore.Read(path)
		if err != nil {
			return Config{}, Chain{}, err
		}
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
	// File stage. Legacy spellings of dual-fed pairs materialize FIRST,
	// canonical spellings (and single-fed keys) SECOND — when the file
	// carries both spellings of a pair the canonical row lands last and
	// owns the field deterministically (the resolver's own spelling
	// rule). A pair whose EITHER spelling the environment owns is
	// skipped entirely: materializing the file's spelling of the other
	// row would let file beat env across spellings.
	for _, canonicalPass := range []bool{false, true} {
		for key, value := range settings.Values {
			pair, dual := pairOf(key)
			if dual && (key == pair[0]) != canonicalPass {
				continue
			}
			if ch.envSet[key] || (dual && (ch.envSet[pair[0]] || ch.envSet[pair[1]])) {
				continue // the environment owns this key or its field
			}
			if err := os.Setenv(key, value); err != nil {
				return Config{}, Chain{}, fmt.Errorf("config: materialize %s: %w", key, err)
			}
			ch.File[key] = true
		}
	}
	// Flag stage. A flag naming EITHER spelling of a pair owns the whole
	// field: the value materializes onto the CANONICAL key, so the
	// resolver's spelling rule cannot invert flag > env across spellings.
	// Legacy-named flags apply first — a canonical-named flag on the same
	// pair deterministically wins when both are set.
	for _, canonicalPass := range []bool{false, true} {
		for key, value := range flags {
			pair, dual := pairOf(key)
			if dual && (key == pair[0]) != canonicalPass {
				continue
			}
			target := key
			if dual {
				target = pair[0]
			}
			if err := os.Setenv(target, value); err != nil {
				return Config{}, Chain{}, fmt.Errorf("config: materialize flag %s: %w", key, err)
			}
			ch.Flag[target] = true
			if target != key {
				ch.Flag[key] = true
			}
		}
	}
	return Load(), ch, nil
}

// pairOf returns the dual-fed (canonical, legacy) env pair key
// participates in, if any — the pair-aware materialization's lookup.
func pairOf(key string) (pair [2]string, dual bool) {
	row, known := rowsByKey[key]
	if !known {
		return [2]string{}, false
	}
	p, dual := dualFedEnv[row.field]
	return p, dual
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
// ValidateFlags. A pair naming BOTH spellings of a dual-fed field
// resolves to the canonical-named flag (LoadResolved materializes the
// legacy one first).
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

// UnknownKeyProblem is the shared unknown-key refusal — the teeth
// text of set/unset/flag validation in one place.
const UnknownKeyProblem = "unknown key (not part of the AXIOM_* vocabulary — refused, never silently ignored)"

// Stage labels parameterizing validateKV's messages.
const (
	stageFile = "config.sqlite"
	stageFlag = "--set"
)

// validateKV runs the shared per-key rules for the value-carrying
// stages — config.sqlite rows and --set flags. The environment
// deliberately does NOT route here: credentials legitimately live in
// the environment, and env empties follow the loader's own semantics.
func validateKV(key, value, stage string) []string {
	row, known := rowsByKey[key]
	switch {
	case !known:
		return []string{fmt.Sprintf("%s: %s", key, UnknownKeyProblem)}
	case row.secret:
		if stage == stageFlag {
			return []string{fmt.Sprintf("%s: secret keys never ride a command line (history/ps) — keep the value in the environment / OS secret store", key)}
		}
		return []string{fmt.Sprintf("%s: secret keys carry references, never values — the value stays in the OS secret store / environment", key)}
	case value == "" && !emptyDisablesKeys[key]:
		if stage == stageFlag {
			return []string{fmt.Sprintf("%s: empty value is unset semantics here", key)}
		}
		return []string{fmt.Sprintf("%s: empty value is unset semantics here — unset the key instead", key)}
	}
	problems := checkRawValue(row, value)
	// No credentials on the value-carrying surfaces: a URL with inline
	// userinfo (scheme://user:pass@host) is a credential the operator
	// typed — config.sqlite (and ps/history for --set) must never hold
	// it. The credential-free URL is the storable form; the credential
	// rides the environment / OS secret store (the render side already
	// strips userinfo from every OUTPUT — this keeps it out of the
	// INPUT).
	if stripURLUserinfo(value) != value {
		problems = append(problems, fmt.Sprintf("%s: carries an inline credential (userinfo) — store the credential-free URL in %s and feed the credential via env / OS secret store", key, stage))
	}
	return problems
}

// ValidateSettings checks config.sqlite rows against the vocabulary:
// known key, parseable value, no secret VALUES (references only), no
// inline credentials, legal secret-ref sources, and no empty values
// where empty is unset semantics. The shared per-value rules live in
// validateKV/checkRawValue.
func ValidateSettings(values, secretRefs map[string]string) []string {
	var problems []string
	for key, value := range values {
		problems = append(problems, validateKV(key, value, stageFile)...)
	}
	for key, source := range secretRefs {
		row, known := rowsByKey[key]
		switch {
		case !known:
			problems = append(problems, fmt.Sprintf("%s: %s", key, UnknownKeyProblem))
		case !row.secret:
			problems = append(problems, fmt.Sprintf("%s: not a secret key — set the value, not a reference", key))
		case source != configstore.SecretRefSourceEnv:
			problems = append(problems, fmt.Sprintf("%s: unknown secret-ref source %q (known: %s)", key, source, configstore.SecretRefSourceEnv))
		}
	}
	return problems
}

// ValidateFlags checks --set pairs: known key, parseable value, no
// inline credentials — and refuses SECRET keys outright (a command
// line is visible in shell history and process listings; secrets stay
// in the environment).
func ValidateFlags(flags map[string]string) []string {
	var problems []string
	for key, value := range flags {
		problems = append(problems, validateKV(key, value, stageFlag)...)
	}
	return problems
}

// ModeEnv is the legacy KG-mode surfaces' env read, relocated behind
// the sanctioned config package (the usage-lint abatement): the mode
// knobs (AXIOM_RETENTION_*) are not part of the envRows vocabulary and
// stay env-only; the read itself lives where env reads live.
func ModeEnv(key string) string { return os.Getenv(key) }

// KnownKey reports whether key is part of the AXIOM_* vocabulary (the
// set/unset teeth: unknown keys are refused, never ignored).
func KnownKey(key string) bool {
	_, ok := rowsByKey[key]
	return ok
}

// SecretRefDrift checks every secret reference in config.sqlite
// against the environment: a row declaring source=env whose env var is
// unset has lost its value source — reported as a problem (the
// reference's teeth: it makes the expectation checkable).
func SecretRefDrift() []string {
	path, err := configstore.DefaultPath()
	if err != nil {
		return nil // no path resolvable → no refs to check
	}
	settings, found, err := configstore.Read(path)
	if err != nil || !found {
		return nil // read problems surface through LoadResolved
	}
	var problems []string
	for key, source := range settings.SecretRefs {
		if source == configstore.SecretRefSourceEnv && os.Getenv(key) == "" {
			problems = append(problems, fmt.Sprintf("%s: secret reference expects the environment to feed it, but %s is unset — the default (empty) applies", key, key))
		}
	}
	sort.Strings(problems)
	return problems
}

// EnvImportRows maps the environment onto file rows for the one-shot
// importer: every key the environment FEEDS becomes a settings row
// (non-secret) or a secret reference (secret keys — never values).
// Dual-fed fields import only the spelling that feeds the value (the
// shadowed spelling would drift into the file as dead config); the
// set-empty-disable key imports its empty value (a meaningful state).
// Values are read ONCE — no secret value ever leaves this function.
func EnvImportRows() (values, secretRefs map[string]string) {
	values = map[string]string{}
	secretRefs = map[string]string{}
	fed := func(key string) (string, bool) {
		v, ok := os.LookupEnv(key)
		if !ok {
			return "", false
		}
		if v == "" && !emptyDisablesKeys[key] {
			return "", false // set-but-empty is unset semantics for this key
		}
		return v, true
	}
	for _, row := range envRows {
		if pair, dual := dualFedEnv[row.field]; dual {
			if row.env == pair[0] {
				// the canonical spelling feeds → import it, skip the legacy row
				if v, ok := fed(pair[0]); ok {
					importRow(row, v, values, secretRefs)
				}
			} else if _, canFed := fed(pair[0]); !canFed {
				if v, ok := fed(pair[1]); ok {
					importRow(row, v, values, secretRefs)
				}
			}
			continue
		}
		if v, ok := fed(row.env); ok {
			importRow(row, v, values, secretRefs)
		}
	}
	return values, secretRefs
}

func importRow(row envRow, value string, values, secretRefs map[string]string) {
	if row.secret {
		secretRefs[row.env] = configstore.SecretRefSourceEnv // reference only — the value stays put
		return
	}
	values[row.env] = value
}
