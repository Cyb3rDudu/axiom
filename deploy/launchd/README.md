# launchd service templates (#205 §3)

Templates for `~/Library/LaunchAgents/` (user LaunchAgents, `axiom` group).
G3's install script copies these and substitutes placeholders.

## Conventions

- **Env files, not env stanzas.** Every service sources its env file(s) via
  a `sh -c` wrapper before exec, with `set -a` so `KEY=VALUE` lines in the
  file (no `export`) are exported into the process environment:
  `set -a; . "$HOME/.config/axiom/<service>.env"; set +a; exec <cmd>`
  Plain `. file && exec` leaves `KEY=VALUE` shell-local — the exec'd
  service starts with silently-defaulted env (#210). Sourcing is also
  fail-closed (`. "…/env" || exit 1`) so a malformed/partial env file
  aborts the service start instead of running with a half-parsed env.
  Services that require a database also guard
  `: "${AXIOM_DATABASE_URL:?...}"` after sourcing so a missing DB setting
  aborts rather than defaulting. Env files live in `~/.config/axiom/*.env`,
  mode 0700, secrets ONLY there — never in `/tmp`, never inline in the
  plist (reboot survival + no secret in launchd-visible config).
- **Logs** go to `~/.local/state/axiom/logs/<service>.log`.
- **KeepAlive policy:** `com.axiom.compute-worker` runs standing (KeepAlive
  true; canonical name since F10 #304 / ADR 0001 §4 — the former
  `com.axiom.runner` label retires with the operator-side switch:
  `launchctl bootout gui/$(id -u)/com.axiom.runner` once, then bootstrap
  the new label).
  The fixer has NO plist at all — it is an event runner (owner decision):
  one process per Zotero attachment key, invoked via `scripts/fix.sh <key>`
  (per-key lock + 30-min timeout). Two concurrent runs on the same key
  would corrupt the agent's working directory; the wrapper serializes.
  Systematic caller is the repair orchestrator INSIDE axiom-ng (#206:
  `AXIOM_FIXER_INVOKER_ENABLED=1`, polls the repair queue, one `--apply`
  invocation per claimed key) — still no launchd, no KeepAlive; see
  docs/operations/services.md §Fixer.
- **`$HOME` is NOT expanded by launchd.** `$HOME` works inside the
  `sh -c` ProgramArguments string (the shell expands it), but NOT in
  `StandardOutPath`/`StandardErrorPath` — the G3 installer substitutes the
  real home directory into those keys at install time.
- **Compute-worker entry:** exec
  `/opt/axiom/compute-worker/current/env/bin/python -m axiom_compute_worker`,
  NOT the `env/bin/axiom-compute-worker` console script. Ceiling of
  conda-pack: it does not rewrite shebangs of pip-installed console
  scripts (they keep `#!/usr/bin/env python`), and launchd's default PATH
  has no `python` — a console-script entry would crash-loop. The
  `/opt/axiom/bin/axiom-compute-worker` shim uses the same `python -m`
  form; the legacy `/opt/axiom/bin/axiom-runner` wrapper warns once and
  delegates (0.1.x hosts: `python -m axiom_ng_runner` equally keeps
  working).
- **Fixer OCR toolchain — bundled (#286):** `tesseract5`, `ghostscript`
  and tessdata (deu+eng + the mapped language set) ship INSIDE the fixer
  artifact; the tools resolve them env-relatively, no host PATH needed.
  See `docs/operations/ocr-rebuild-repair.md`.
