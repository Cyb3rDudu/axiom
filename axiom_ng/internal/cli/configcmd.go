// configcmd.go — `axiom config` (F05 #299 surface, made real by F13
// #307): the resolved view (get --effective, source column
// flag/env/file/default), the persistent store writes (set/unset —
// validated BEFORE any file write: unknown key, type violation, and
// secret keys are refused with exit 1, never silently ignored), the
// full consistency check (validate: env values, file rows, secret-ref
// drift, and the composition wiring), and the one-shot env importer
// (import-env: non-secrets as values, secrets as REFERENCES — values
// never enter the file or any output line).
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sort"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/composition"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config/configstore"
)

func cmdConfig(name string, args []string, flags map[string]string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: %s config get --effective [--json] | set <KEY> <VALUE> | unset <KEY> | validate | import-env\n", name)
		return exitUsage
	}
	// The write subcommands take no --set: it would be silently
	// ignored (they write the store, they do not resolve) — a loud
	// usage error instead (exit 2).
	if len(flags) > 0 {
		switch args[0] {
		case "set", "unset", "import-env":
			fmt.Fprintf(os.Stderr, "%s config %s does not take --set (it writes config.sqlite directly; --set resolves a run's configuration)\n", name, args[0])
			return exitUsage
		}
	}
	switch args[0] {
	case "get":
		return cmdConfigGet(name, args[1:], flags)
	case "validate":
		return cmdConfigValidate(flags)
	case "set":
		return cmdConfigSet(name, args[1:])
	case "unset":
		return cmdConfigUnset(name, args[1:])
	case "import-env":
		return cmdConfigImportEnv(name)
	default:
		fmt.Fprintf(os.Stderr, "%s config: unknown subcommand %q\n", name, args[0])
		return exitUsage
	}
}

// cmdConfigGet — the resolved view: one row per key with effective
// value (secrets redacted) and the source column flag | env | file |
// default (the F13 #307 chain; --set flags feed the flag stage).
func cmdConfigGet(name string, args []string, flags map[string]string) int {
	effective, jsonOut := false, false
	for _, a := range args {
		switch a {
		case "--effective":
			effective = true
		case "--json":
			jsonOut = true
		default:
			fmt.Fprintf(os.Stderr, "%s config get: unknown flag %q (known: --effective --json)\n", name, a)
			return exitUsage
		}
	}
	if !effective {
		fmt.Fprintf(os.Stderr, "%s config get: only --effective exists — the view resolves the full chain (flag > env > config.sqlite > default)\n", name)
		return exitUsage
	}
	cfg, ch, err := config.LoadResolved(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s config get: %v\n", name, err)
		return exitFailure
	}
	entries := config.EffectiveChain(cfg, ch)
	if jsonOut {
		out, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return exitFailure
		}
		fmt.Println(string(out))
		return exitOK
	}
	for _, e := range entries {
		fmt.Printf("%-40s %-8s %v\n", e.Env, e.Source, e.Value)
	}
	return exitOK
}

// cmdConfigSet — write one non-secret override into config.sqlite.
// Validation FIRST (vocabulary, type, vocabulary-keyed values, secret
// refusal), file write only after: an invalid set never touches the
// file. Exit 1 names every problem; exit 2 is argument shape only.
func cmdConfigSet(name string, args []string) int {
	if len(args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: %s config set <KEY> <VALUE>\n", name)
		return exitUsage
	}
	key, value := args[0], args[1]
	if problems := config.ValidateSettings(map[string]string{key: value}, nil); len(problems) > 0 {
		for _, p := range problems {
			fmt.Println("invalid:", p)
		}
		fmt.Println("configuration invalid — nothing written")
		return exitFailure
	}
	st, err := openConfigStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s config set: %v\n", name, err)
		return exitFailure
	}
	defer st.Close()
	if err := st.Set(key, value); err != nil {
		fmt.Fprintf(os.Stderr, "%s config set: %v\n", name, err)
		return exitFailure
	}
	fmt.Printf("set %s (config.sqlite; feeds the chain where the environment does not override it — effective after the next start, the running process keeps its resolution)\n", key)
	return exitOK
}

// cmdConfigUnset — remove one override row or secret reference
// (idempotent; unknown vocabulary keys are refused with the same teeth
// as set).
func cmdConfigUnset(name string, args []string) int {
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: %s config unset <KEY>\n", name)
		return exitUsage
	}
	key := args[0]
	if !config.KnownKey(key) {
		fmt.Printf("invalid: %q: %s\n", key, config.UnknownKeyProblem)
		fmt.Println("configuration invalid — nothing written")
		return exitFailure
	}
	// An ABSENT file is not an error and must not become one by being
	// created: unset with nothing to remove is a no-op, exit 0.
	path, err := configstore.DefaultPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s config unset: %v\n", name, err)
		return exitFailure
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fmt.Printf("config.sqlite absent — nothing to unset (%s)\n", key)
		return exitOK
	}
	st, err := openConfigStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s config unset: %v\n", name, err)
		return exitFailure
	}
	defer st.Close()
	if err := st.Unset(key); err != nil {
		fmt.Fprintf(os.Stderr, "%s config unset: %v\n", name, err)
		return exitFailure
	}
	fmt.Printf("unset %s\n", key)
	return exitOK
}

// cmdConfigValidate — the full consistency pass: (a) env values parse
// (nothing the loader would silently ignore), (b) config.sqlite rows
// are valid (validated inside LoadResolved — a broken file is a loud
// failure, not a warning), (c) secret references whose env source is
// unset (declared-env-fed but the environment dropped it), (d) the
// derived role set wires without a missing port (the composition
// Select diagnosis, without starting anything).
func cmdConfigValidate(flags map[string]string) int {
	failed := false
	if problems := config.ValidateEnv(); len(problems) > 0 {
		failed = true
		for _, p := range problems {
			fmt.Println("invalid:", p)
		}
	}
	cfg, _, err := config.LoadResolved(flags)
	if err != nil {
		fmt.Println("invalid:", err)
		fmt.Println("configuration invalid")
		return exitFailure
	}
	for _, p := range config.SecretRefDrift() {
		failed = true
		fmt.Println("invalid:", p)
	}
	// Network-free by design: Select's build wiring probes the Zotero
	// local API for its start log — validate points that probe at a
	// refused loopback address so the check stays an env/consistency
	// pass, not a reachability one (the probe degrades to the documented
	// "not reachable" warning in the discard logger).
	cfg.ZoteroBaseURL = "http://127.0.0.1:1"
	if _, err := composition.Select(cfg, discardLogger(), composition.Ports{}, composition.RolesFromConfig(cfg)...); err != nil {
		failed = true
		fmt.Println("inconsistent:", err)
	}
	if failed {
		fmt.Println("configuration invalid")
		return exitFailure
	}
	fmt.Println("configuration ok")
	return exitOK
}

// cmdConfigImportEnv — the one-shot importer: every key the environment
// feeds becomes a file row (non-secrets as values, secrets as env
// REFERENCES — no secret value ever enters the file or this command's
// output). The import is validated FIRST: a value the file surface
// would refuse (type violation, inline credential) refuses the whole
// import writing nothing — the file must never become the durable copy
// of a value the chain would reject at the next boot. Idempotent:
// upserts, a replay reports the same state. The effective
// configuration does not move: the env still owns every imported key
// until it is cleared (the file row takes over then).
func cmdConfigImportEnv(name string) int {
	values, refs := config.EnvImportRows()
	// Credential-carrying values are legal IN THE ENVIRONMENT but never
	// enter the file: those rows are SKIPPED loudly (the environment
	// keeps owning them — the effective configuration does not move);
	// everything else is validated before the first write.
	var skipped []string
	clean := make(map[string]string, len(values))
	for _, k := range sortedKeys(values) {
		if form := config.InlineCredential(k, values[k]); form != "" {
			skipped = append(skipped, fmt.Sprintf("%s (%s — left in the environment, not imported)", k, form))
			continue
		}
		clean[k] = values[k]
	}
	if problems := config.ValidateSettings(clean, refs); len(problems) > 0 {
		for _, p := range problems {
			fmt.Println("invalid:", p)
		}
		fmt.Println("configuration invalid — nothing written")
		return exitFailure
	}
	st, err := openConfigStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s config import-env: %v\n", name, err)
		return exitFailure
	}
	defer st.Close()
	// ONE transaction: the import lands completely or not at all.
	if err := st.SetAll(clean, refs); err != nil {
		fmt.Fprintf(os.Stderr, "%s config import-env: %v\n", name, err)
		return exitFailure
	}
	for _, s := range skipped {
		fmt.Println("skipped:", s)
	}
	fmt.Printf("imported %d settings and %d secret references from the environment into config.sqlite (idempotent; secrets stored as references only — values stay in the environment)\n",
		len(clean), len(refs))
	return exitOK
}

// sortedKeys returns the map's keys in sorted order (deterministic
// write and message order).
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// openConfigStore opens (creating atomically on first write) the
// config.sqlite under the resolved path.
func openConfigStore() (*configstore.Store, error) {
	path, err := configstore.DefaultPath()
	if err != nil {
		return nil, err
	}
	return configstore.Open(path)
}

// discardLogger swallows the Select precedence notes (they document
// legal env/role interplay, not validation problems).
func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }
