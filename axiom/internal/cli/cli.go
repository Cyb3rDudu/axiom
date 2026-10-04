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

	"github.com/Cyb3rDudu/axiom/axiom/internal/composition"
	"github.com/Cyb3rDudu/axiom/axiom/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom/internal/statehome"
	"github.com/Cyb3rDudu/axiom/axiom/internal/version"
)

// Run dispatches argv (including argv[0]) and returns the process exit
// code. name is the invocation identity ("axiom" canonical, "axiom-ng"
// alias): it prefixes serve log lines and error messages. Alias-output
// contract: the legacy serve log lines (prefix "axiom-ng: ") and the KG
// mode flag surface behave as pre-F05. Deliberate deltas on the alias:
// exactly one deprecation warning line per process (emitted once in
// cmd/axiom-ng), -help gained the canonical command block ahead of the
// legacy mode text, and unknown argv is now a usage error (exit 2)
// instead of falling through to a server boot.
//
// --set KEY=VALUE (repeatable) is the CLI-flag stage of the F13 #307
// resolution chain (flag > env > config.sqlite > default), honored by
// serve, doctor, and config — validated once here against the AXIOM_*
// vocabulary (unknown key / bad value / secret key = loud exit). The
// legacy KG mode flags never see it (their own flag surface is frozen).
func Run(name string, args []string) int {
	// #352: one-time legacy state migration at process start, before any
	// path resolution; best-effort — a failure logs, never blocks boot.
	if err := statehome.Migrate(); err != nil {
		log.Printf("statehome: legacy state migration failed (legacy root untouched): %v", err)
	}
	if len(args) < 2 {
		cfg, code := loadRuntime(name, nil)
		if code != exitOK {
			return code
		}
		return serve(name, cfg, nil) // no-arg: the compat boot = serve all
	}
	switch args[1] {
	case "serve":
		flags, rest, code := splitSetFlags(name, args[2:])
		if code != exitOK {
			return code
		}
		return cmdServe(name, rest, flags)
	case "version":
		return cmdVersion(hasFlag(args[2:], "--json"))
	case "doctor":
		flags, rest, code := splitSetFlags(name, args[2:])
		if code != exitOK {
			return code
		}
		return cmdDoctor(hasFlag(rest, "--json"), flags)
	case "config":
		flags, rest, code := splitSetFlags(name, args[2:])
		if code != exitOK {
			return code
		}
		return cmdConfig(name, rest, flags)
	case "data":
		return cmdData(name, args[2:])
	case "--version":
		return cmdVersion(false)
	case "-help", "--help", "help":
		// The new surface docs plus the legacy mode block (#202 contract
		// text stays the operator reference for the mode flags).
		fmt.Print(help(name))
		fmt.Println("--- legacy mode block (documented under the alias name, #202) ---")
		fmt.Print(modeHelp)
		return 0
	default:
		// Legacy mode flags (#244) keep working under both names.
		if runCLIMode(args) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "%s: unknown command %q\n\n%s", name, args[1], help(name))
		return 2
	}
}

// splitSetFlags parses the --set stage for one dispatch case and
// reports the usage error itself (three dispatch sites, one shape).
func splitSetFlags(name string, args []string) (flags map[string]string, rest []string, code int) {
	var err error
	flags, rest, err = config.ParseSetFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		return nil, nil, exitUsage
	}
	return flags, rest, exitOK
}

// loadRuntime resolves the full chain (flag > env > config.sqlite >
// default) for the runtime surfaces. A broken file or flag is a LOUD
// one-line diagnosis + exit 1 — never a silent boot on defaults.
func loadRuntime(name string, flags map[string]string) (config.Config, int) {
	cfg, _, err := config.LoadResolved(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: configuration: %v\n", name, err)
		return config.Config{}, exitFailure
	}
	return cfg, exitOK
}

// Exit codes (the contract documented in the package comment).
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// cmdServe is the role CLI: which slice of the composition this process
// runs. `all` is the compat full stack; `api` the API-serving selection;
// `library` the Library slice (Zotero provider + import ladder + sync
// mirror, F07); `store` stays vocabulary-only until F09 — it refuses
// loudly instead of fake-splitting.
func cmdServe(name string, args []string, flags map[string]string) int {
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: %s serve [--set KEY=VALUE]... all|api|library|store\n", name)
		return exitUsage
	}
	var roles []composition.Role
	var cfg config.Config
	switch args[0] {
	case "all":
		// nil = Full: the config-derived full stack, byte-identical to the
		// pre-F05 boot (RolesFromConfig inside).
		var code int
		cfg, code = loadRuntime(name, flags)
		if code != exitOK {
			return code
		}
	case "api":
		base, code := loadRuntime(name, flags)
		if code != exitOK {
			return code
		}
		note := func(note string) {
			fmt.Fprintln(os.Stderr, name+": note: "+note)
		}
		cfg = apiServeConfig(base, note)
		cfg = apiServeSplitCredentials(cfg, note)
		// F11 #305: in the split topology (AXIOM_LIBRARY_URL/
		// AXIOM_STORE_URL set) the Library contract surface is served by
		// the library PROCESS — a local provider here would race the
		// single-writer lease and duplicate imports. Suppress it loudly,
		// the serve-store precedent.
		if cfg.LibraryURL != "" && cfg.LibraryImportProviders != "" {
			fmt.Fprintf(os.Stderr, "%s: note: AXIOM_LIBRARY_IMPORT_PROVIDERS=%q set but serve api runs split-mode (AXIOM_LIBRARY_URL set) — the Library surface is served by the library process; local import providers stay off in this process\n", name, cfg.LibraryImportProviders)
			cfg.LibraryImportProviders = ""
		}
		roles = apiRoles(cfg)
		// the api-only warning fires for every api-only shape EXCEPT the
		// full split edge (both component URLs — the credential-free edge,
		// not degraded). A HALF-configured split (one URL, no DSN) is still
		// degraded and keeps the warning.
		if len(roles) == 1 && !fullSplitEdge(cfg) {
			fmt.Fprintln(os.Stderr, name+": WARNING: AXIOM_DATABASE_URL not set; serving api-only (degraded)")
		}
	case "library":
		// F07 #301: the Library slice is real now — the Zotero provider
		// (AXIOM_LIBRARY_IMPORT_PROVIDERS=zotero wires the import ladder
		// behind the single-writer lease) + the sync mirror, without the
		// store-processing roles (F09). F14 #308: the repair track rides
		// this slice in the split topology — AXIOM_FIXER_INVOKER_ENABLED=1
		// arms the supervised repair-worker loop HERE (the api arm keeps
		// its documented "no loops" contract and suppresses the env).
		var code int
		cfg, code = loadRuntime(name, flags)
		if code != exitOK {
			return code
		}
		roles = libraryRoles()
	case "store":
		// F09 #303: the Store slice is real — the processing/retrieval
		// half WITHOUT any Library runtime: no Zotero sync, no provider,
		// no Zotero probe (the composition skips the zotero health check
		// entirely for this role set). Intake is revision-only (POST
		// /api/v1/store/ingest); the legacy sync lane simply has no
		// driver in this process.
		base, code := loadRuntime(name, flags)
		if code != exitOK {
			return code
		}
		cfg = apiServeConfig(base, func(note string) {
			fmt.Fprintln(os.Stderr, name+": note: "+note)
		})
		if cfg.LibraryImportProviders != "" {
			fmt.Fprintf(os.Stderr, "%s: note: AXIOM_LIBRARY_IMPORT_PROVIDERS=%q set but serve store runs no Library — the import routes stay unwired in this process\n", name, cfg.LibraryImportProviders)
			cfg.LibraryImportProviders = ""
		}
		roles = storeRoles()
	default:
		fmt.Fprintf(os.Stderr, "%s serve: unknown role %q (known: all api library store)\n", name, args[0])
		return exitUsage
	}
	return serve(name, cfg, roles)
}

// apiServeConfig enforces the documented serve-api contract ("no
// claim/fixer loops") on the env-derived config: the repair ROLE stays
// selected (the /api/repair/* write surface is API), but the env-gated
// fixer invoker LOOP is suppressed — explicitly, with the same
// never-silently note Select uses for the dispatcher. The claim loop
// needs no adjustment: the dispatcher role is simply not in the api set,
// so Select's own precedence note fires when its env is on.
func apiServeConfig(cfg config.Config, note func(string)) config.Config {
	if cfg.FixerInvokerEnabled {
		note("AXIOM_FIXER_INVOKER_ENABLED=1 but serve api runs no fixer loop — explicit role selection wins (no invoker in this process)")
		cfg.FixerInvokerEnabled = false
	}
	return cfg
}

// fullSplitEdge reports the configured split topology (both component
// URLs) — the shape whose api-only derivation is the credential-free
// public edge, NOT the degraded no-DSN fallback. A half-configured
// split is not an edge; it stays degraded.
func fullSplitEdge(cfg config.Config) bool {
	return cfg.LibraryURL != "" && cfg.StoreURL != ""
}

// apiServeSplitCredentials enforces the DM07 #316 serve-api contract
// for the split topology: the api process is the pure public edge and
// holds NO database credentials. A DSN present in its environment (the
// shared-env-file shape) used to silently boot the local store stack
// and shadow the component proxies; now it is ignored, loudly. The
// loops env follows the same "no loops" contract so the cleared DSN
// cannot trip Select's half-wired guard. Non-split shapes pass through
// unchanged (the single-DSN compat rule).
func apiServeSplitCredentials(cfg config.Config, note func(string)) config.Config {
	if !fullSplitEdge(cfg) {
		return cfg
	}
	if cfg.DatabaseURL != "" {
		note("AXIOM_STORE_DATABASE_URL/AXIOM_DATABASE_URL set but serve api runs the split edge (AXIOM_LIBRARY_URL+AXIOM_STORE_URL) — the api process stays credential-free (#316); the DSN is ignored here")
		cfg.DatabaseURL = ""
	}
	if cfg.DispatcherEnabled {
		note("AXIOM_DISPATCHER_ENABLED=1 but serve api runs no claim loop in the split topology — the store process owns it")
		cfg.DispatcherEnabled = false
	}
	return cfg
}

// apiRoles is the API-relevant selection out of the F04 registry: every
// role that serves the HTTP surface (store, events, sync, repair API,
// search, ingest wiring) — minus the background claim/fixer loops. A
// db-less config degrades to api-only with today's warning (the degraded
// shape is legal; Select would otherwise refuse the unstartable store).
// Since DM07 #316 the split topology (AXIOM_LIBRARY_URL + AXIOM_STORE_URL
// set) is the SECOND legal api-only shape — the pure public edge, NOT
// degraded: cmdServe clears the DSN before this derivation so the api
// process never opens a pool it must not hold.
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

// storeRoles is the Store-slice selection of the F04 registry (F09 #303):
// api (the HTTP surface), store (Postgres + repo + both component
// ledgers), events (the live-view bus), search, ingest (the runner
// failover chain) and the dispatcher (the claim loop + outbox drainer).
// NOT included, by design: sync (the Zotero mirror read path — Library's)
// and repair (F08's track). This is the independence topology: the store
// works against explicit source revisions with the Library completely
// stopped.
func storeRoles() []composition.Role {
	return []composition.Role{
		composition.RoleAPI, composition.RoleStore, composition.RoleEvents,
		composition.RoleSearch, composition.RoleIngest, composition.RoleDispatcher,
	}
}

// libraryRoles is the Library-slice selection of the F04 registry (F07
// #301 unlocked it): the Zotero provider + import ladder + the sync
// mirror — api (the HTTP surface), store (the Postgres substrate: the
// Library's OWN tables live there; the store-PROCESSING roles are F09's
// slice), events (the live-view bus), sync (the Zotero mirror read path
// + revision Mitschrieb), and repair (F14 #308: in the split topology
// the Library process owns the repair track — its supervised child
// worker and the Zotero write gateway live behind the Library
// single-writer, exactly where the repair custody chain writes). NOT
// included, by design: search/ingest/dispatcher (the F09 Store slice).
func libraryRoles() []composition.Role {
	return []composition.Role{
		composition.RoleAPI, composition.RoleStore, composition.RoleEvents,
		composition.RoleSync, composition.RoleRepair,
	}
}

// serve is the shared boot: debug-bind guard, composition build, signal
// context, ordered start, fatal/signal select, budgeted stop. The body is
// the pre-F05 main.go sequence — one place, two entry names.
func serve(name string, cfg config.Config, roles []composition.Role) int {
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
  serve api                     the API-serving roles (no claim/fixer loops);
                                split topology when AXIOM_LIBRARY_URL/
                                AXIOM_STORE_URL are set — the Library/Store
                                contract surfaces bind to the component
                                processes' internal edges (F11 #305)
  serve library                 Library slice: Zotero provider + import
                                ladder + sync mirror (F07 #301; no
                                store-processing) + the repair track (F14
                                #308: the supervised repair-worker loop
                                and write gateway live behind the Library
                                single-writer); serves the
                                internal Library edge on
                                AXIOM_INTERNAL_LIBRARY_ADDR when set
  serve store                   the Store slice (F09 #303): revision
                                intake + retrieval + dispatcher; serves the
                                internal Store edge on
                                AXIOM_INTERNAL_STORE_ADDR when set
  version [--json]              version banner (agrees with /api/health)
  doctor [--json]               config/DB/OpenSearch/artifact-root health;
                                exit 0 only when fully healthy; never prints
                                secret values
  config get --effective [--json]
                                resolved config with source per key
                                (precedence: --set flag > env > config.sqlite
                                > default)
  config set <KEY> <VALUE>      write one override into config.sqlite
                                (validated first: unknown key/type error,
                                secret keys refused)
  config unset <KEY>            remove one override (idempotent)
  config validate               env + file + wiring consistency check
  config import-env             one-shot: import the current environment
                                into config.sqlite (secrets as references
                                only, never values)
  data export --component library --dsn URL --out DIR
                                write the backend-neutral bundle (versioned,
                                streamable, hash-verified; DM03)
  data import --component library --from DIR (--dsn URL | --sqlite PATH) [--merge]
                                apply a bundle (idempotent; non-empty
                                targets need --merge; in-flight imports
                                survive verbatim; DM04)
  data verify --component library --from DIR (--dsn URL | --sqlite PATH) [--json]
                                counts, digests, FK invariants, semantic
                                readbacks (+ PRAGMA integrity on SQLite)
  data shadow --component library --source-dsn URL (--dsn URL | --sqlite PATH)
                                --out REPORT [--max-samples N --json]
                                shadow-read: legacy mirror vs imported
                                copy, full data set, explicit allowlist;
                                unexpected deviations red (DM08)
  --set KEY=VALUE               one-shot override for serve/doctor/config
                                (the CLI-flag stage of the chain)
  (KG mode flags)               the legacy one-shot modes (#244) keep
                                working: -cleanup-frontmatter-kg,
                                -consolidate-relations, … (see -help
                                mode listing)
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
