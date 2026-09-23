// Package cli is the shared command surface of the runtime binaries
// (F05, #299): `axiom` is the canonical entrypoint, `axiom-ng` the
// functional compatibility alias (ADR 0001 §3) delegating to the same
// Run. The subcommand surface is the role-CLI contract F06–F10 dock
// onto; the legacy KG mode flags (#244) stay accepted through both
// names — one binary family, one dispatch.
//
// Exit-code contract (documented, tested):
//
//	0   success (serve shut down gracefully after signal/fatal-free run,
//	    doctor fully healthy, config valid, version printed)
//	1   runtime failure (startup diagnosis, unhealthy doctor result,
//	    invalid env combination)
//	2   usage error (unknown subcommand/role; not-yet-extracted roles;
//	    commands reserved for later steps)
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/composition"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/version"
)

// Run dispatches argv (including argv[0]) and returns the process exit
// code. name is the invocation identity ("axiom" canonical, "axiom-ng"
// alias) — it prefixes log lines and keeps the alias's output
// byte-identical to the pre-F05 binary.
func Run(name string, args []string) int {
	if len(args) < 2 {
		return serve(name, nil) // no-arg: the compat boot = serve all
	}
	switch args[1] {
	case "serve":
		return cmdServe(name, args[2:])
	case "version":
		return cmdVersion(hasFlag(args[2:], "--json"))
	case "doctor":
		return cmdDoctor(hasFlag(args[2:], "--json"))
	case "config":
		return cmdConfig(args[2:])
	case "--version":
		return cmdVersion(false)
	case "-help", "--help", "help":
		// The new surface docs plus the legacy mode block (#202 contract
		// text stays the operator reference for the mode flags).
		fmt.Print(help(name))
		fmt.Print(modeHelp)
		return 0
	default:
		// Legacy mode flags (#244) keep working under both names.
		if runCLIMode(args) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "axiom: unknown command %q\n\n%s", args[1], help(name))
		return 2
	}
}

// Exit codes (the contract documented in the package comment).
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// cmdServe is the role CLI: which slice of the composition this process
// runs. `all` is the compat full stack; `api` the API-serving selection;
// `library`/`store` exist as vocabulary only until their components are
// extracted (F06/F09) — they refuse loudly instead of fake-splitting.
func cmdServe(name string, args []string) int {
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: %s serve all|api|library|store\n", name)
		return exitUsage
	}
	var roles []composition.Role
	switch args[0] {
	case "all":
		// nil = Full: the config-derived full stack, byte-identical to the
		// pre-F05 boot (RolesFromConfig inside).
	case "api":
		roles = apiRoles(config.Load())
		if len(roles) == 1 {
			fmt.Fprintln(os.Stderr, name+": WARNING: AXIOM_DATABASE_URL not set; serving api-only (degraded)")
		}
	case "library":
		fmt.Fprintf(os.Stderr, "%s serve library: component not yet extracted — the library role arrives with F06 (#300); until then `serve all` serves the compiled-in library\n", name)
		return exitUsage
	case "store":
		fmt.Fprintf(os.Stderr, "%s serve store: component not yet extracted — the store role arrives with F09 (#303); until then `serve all` serves the compiled-in store\n", name)
		return exitUsage
	default:
		fmt.Fprintf(os.Stderr, "%s serve: unknown role %q (known: all api library store)\n", name, args[0])
		return exitUsage
	}
	return serve(name, roles)
}

// apiRoles is the API-relevant selection out of the F04 registry: every
// role that serves the HTTP surface (store, events, sync, repair API,
// search, ingest wiring) — minus the background claim/fixer loops. A
// db-less config degrades to api-only with today's warning (the degraded
// shape is legal; Select would otherwise refuse the unstartable store).
func apiRoles(cfg config.Config) []composition.Role {
	if cfg.DatabaseURL == "" {
		return []composition.Role{composition.RoleAPI}
	}
	return []composition.Role{
		composition.RoleAPI, composition.RoleStore, composition.RoleEvents,
		composition.RoleSync, composition.RoleRepair, composition.RoleSearch,
		composition.RoleIngest,
	}
}

// serve is the shared boot: debug-bind guard, composition build, signal
// context, ordered start, fatal/signal select, budgeted stop. The body is
// the pre-F05 main.go sequence — one place, two entry names.
func serve(name string, roles []composition.Role) int {
	cfg := config.Load()
	logger := log.New(os.Stderr, name+": ", log.LstdFlags)
	logger.Printf("starting %s", version.Banner())
	// #202: heartbeat sink for long mutating KG passes.
	repo.SetKGProgressLogger(logger.Printf)

	// #205 §5: a debug build must never serve production ports.
	if version.DebugBindRefused(version.BuildType, cfg.APIPort, os.Getenv) {
		logger.Printf("refusing to bind production port %d with %s — build a release artifact (make rag) or set AXIOM_ALLOW_DEBUG_BIND=1 for local dev",
			cfg.APIPort, version.Banner())
		return exitFailure
	}

	var root *composition.Root
	var err error
	if roles == nil {
		root, err = composition.Full(cfg, logger, composition.Ports{})
	} else {
		root, err = composition.Select(cfg, logger, composition.Ports{}, roles...)
	}
	if err != nil {
		logger.Printf("startup: %v", err)
		return exitFailure
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := root.Start(sigCtx); err != nil {
		logger.Printf("startup: %v", err)
		return exitFailure
	}

	select {
	case <-sigCtx.Done():
		logger.Printf("signal received; shutting down")
		stop() // idempotent; release the signal handler
	case err := <-root.Fatal():
		logger.Printf("%v", err)
		return exitFailure
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root.Stop(shutdownCtx)
	return exitOK
}

// cmdVersion prints the single identity line (#205: the banner agrees with
// /api/health); --json adds the structured form for automation.
func cmdVersion(asJSON bool) int {
	if !asJSON {
		fmt.Println(version.Banner())
		return exitOK
	}
	out, err := json.Marshal(map[string]string{
		"banner":     version.Banner(),
		"version":    version.Version,
		"commit":     version.Commit,
		"build_type": version.BuildType,
	})
	if err != nil {
		return exitFailure
	}
	fmt.Println(string(out))
	return exitOK
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// help documents the surface and the exit-code contract.
func help(name string) string {
	return fmt.Sprintf(`%[1]s — the axiom runtime (F05 #299; ADR 0001)

Usage: %[1]s <command> [args]

Commands:
  serve all                     full stack through the composition root
                                (config-derived: dispatcher/fixer opt-in envs)
  serve api                     the API-serving roles (no claim/fixer loops)
  serve library|store           NOT YET: components arrive with F06 (#300)
                                and F09 (#303) — refuses loudly until then
  version [--json]              version banner (agrees with /api/health)
  doctor [--json]               config/DB/OpenSearch/artifact-root health;
                                exit 0 only when fully healthy; never prints
                                secret values
  config get --effective [--json]
                                resolved config with source per key
                                (precedence: flag > env > default; flags
                                arrive with F13)
  config validate               env combination consistency check
  config set                    arrives with F13 (persistent store) — no
                                pseudo-store before that
  (KG mode flags)               the legacy one-shot modes (#244) keep
                                working: -cleanup-frontmatter-kg,
                                -consolidate-relations, … (see -help output
                                of axiom-ng)
  (no command)                  start the full stack (compat boot)

Exit codes:
  0  success
  1  runtime failure (startup diagnosis, unhealthy doctor, invalid env)
  2  usage error (unknown command/role, not-yet-extracted or reserved
     commands)

The versioned API edge is /api/v1/{health,search,passage/{id}}; the
unversioned compat routes stay byte-identical through 0.2.x (F01 baseline).
`, name)
}
