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
- **KeepAlive policy:** `com.axiom.runner` runs standing (KeepAlive true).
  The fixer has NO plist at all — it is an event runner (owner decision):
  one process per Zotero attachment key, invoked via `scripts/fix.sh <key>`
  (per-key lock + 30-min timeout). Two concurrent runs on the same key
  would corrupt the agent's working directory; the wrapper serializes.
- **`$HOME` is NOT expanded by launchd.** `$HOME` works inside the
  `sh -c` ProgramArguments string (the shell expands it), but NOT in
  `StandardOutPath`/`StandardErrorPath` — the G3 installer substitutes the
  real home directory into those keys at install time.
- **Runner entry:** exec
  `/opt/axiom/runner/current/env/bin/python -m axiom_ng_runner`, NOT the
  `env/bin/axiom-runner` console script. Ceiling of conda-pack: it does not
  rewrite shebangs of pip-installed console scripts (they keep
  `#!/usr/bin/env python`), and launchd's default PATH has no `python` —
  a console-script entry would crash-loop. The `/opt/axiom/bin/axiom-runner`
  shim uses the same `python -m` form.
- **Fixer host deps (NOT bundled in the artifact):** `tesseract5` (with
  `deu` traineddata) and `ghostscript` must be on PATH for the OCR lane —
  see `docs/operations/services.md`.
