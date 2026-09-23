// configcmd.go — `axiom config` (F05 #299): the read-only view over the
// env-based configuration plus the consistency validate. `config set`
// (and every persistent store) arrives with F13 — refusing loudly now
// instead of shipping a pseudo-store.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/composition"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
)

func cmdConfig(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: axiom config get --effective [--json] | validate | set (F13)")
		return exitUsage
	}
	switch args[0] {
	case "get":
		return cmdConfigGet(args[1:])
	case "validate":
		return cmdConfigValidate()
	case "set":
		fmt.Fprintln(os.Stderr, "axiom config set: arrives with F13 (persistent runtime configuration store) — no pseudo-store before that")
		return exitUsage
	default:
		fmt.Fprintf(os.Stderr, "axiom config: unknown subcommand %q\n", args[0])
		return exitUsage
	}
}

// cmdConfigGet — only the --effective view exists (the raw store IS the
// environment today). One row per key: effective value (secrets
// redacted), source env|default. Flags join the precedence with F13.
func cmdConfigGet(args []string) int {
	effective, jsonOut := false, false
	for _, a := range args {
		switch a {
		case "--effective":
			effective = true
		case "--json":
			jsonOut = true
		default:
			fmt.Fprintf(os.Stderr, "axiom config get: unknown flag %q (known: --effective --json)\n", a)
			return exitUsage
		}
	}
	if !effective {
		fmt.Fprintln(os.Stderr, "axiom config get: only --effective exists today — the configuration source is the environment (flags/store arrive with F13)")
		return exitUsage
	}
	entries := config.Effective(config.Load())
	if jsonOut {
		out, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return exitFailure
		}
		fmt.Println(string(out))
		return exitOK
	}
	for _, e := range entries {
		fmt.Printf("%-32s %-8s %v\n", e.Env, e.Source, e.Value)
	}
	return exitOK
}

// cmdConfigValidate — the env combination consistency check: (a) every
// set value parses for its target kind (nothing the loader would
// silently ignore), (b) the derived role set wires without a missing
// port (the composition Select diagnosis, without starting anything).
func cmdConfigValidate() int {
	failed := false
	if problems := config.ValidateEnv(); len(problems) > 0 {
		failed = true
		for _, p := range problems {
			fmt.Println("invalid:", p)
		}
	}
	cfg := config.Load()
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

// discardLogger swallows the Select precedence notes (they document
// legal env/role interplay, not validation problems).
func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }
