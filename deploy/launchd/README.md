# launchd service templates (#205 §3)

Templates for `~/Library/LaunchAgents/` (user LaunchAgents, `axiom` group).
G3's install script copies these and substitutes placeholders.

## Conventions

- **Env files, not env stanzas.** Every service sources its env file via a
  `sh -c` wrapper before exec:
  `. "$HOME/.config/axiom/<service>.env" && exec <cmd>`
  Env files live in `~/.config/axiom/*.env`, mode 0700, secrets ONLY there —
  never in `/tmp`, never inline in the plist (reboot survival + no secret in
  launchd-visible config).
- **Logs** go to `~/.local/state/axiom/logs/<service>.log`.
- **KeepAlive policy:** `com.axiom.runner` runs standing (KeepAlive true);
  `com.axiom.fixer` is on-demand by default (KeepAlive false — enable per
  runbook decision in G3).
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
