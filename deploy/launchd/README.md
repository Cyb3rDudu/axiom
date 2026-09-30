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

- **Scheduling (QoS) — ProcessType policy (diagnostics 2026-09-21):**
  every service that spawns CPU-bound work runs
  `ProcessType=Standard`: `com.axiom.rag` (it spawns the repair/OCR
  worker children — a child process cannot escape the Background
  coalition it inherits, so the class must be fixed at the SERVICE
  level), `com.axiom.compute-worker` (ML inference), and the
  `com.axiom.rag-dispatch-gpu*` dispatcher instances. The single
  documented exception is `com.axiom.carrier-bridge`: a pure network
  forwarder with no CPU-bound children — `Background` is the deliberate,
  polite fit there. Rationale and controlled A/B/C evidence
  (Background 82.56 s vs Standard 8.81 s vs Interactive 7.45 s on the
  identical 12-process workload; production run 10.62× slower than the
  foreground reference): `docs/diagnostics/2026-09-21-ocrmypdf-background-qos.md`.
  Operators can reproduce the measurement on their own host with
  `qos-probe.sh` (below); the operator-side rollout (changing the
  installed agent and reloading) is a deployment step, not part of this
  repository.
- **qos-probe.sh — the scheduling sonde:** a dependency-light CPU-bound
  probe (12 parallel sha256 children, needs only python3). In-place
  measurement shows the CURRENT coalition's core occupancy; `--launchd
  Background|Standard|Interactive` runs the identical workload as a
  temporary launchd job of that class, so back-to-back legs isolate the
  scheduling variable exactly like the diagnostics experiment:

  ```bash
  deploy/launchd/qos-probe.sh --launchd Background
  deploy/launchd/qos-probe.sh --launchd Standard
  # compare the mean-cores lines — the gap is the scheduling class
  ```

  Expected on an 8P+4E Apple-silicon host: Standard ≈ 8-10 mean cores,
  Background ≈ 2-3 (a ~9× wall-time difference at equal work).
  Measurement guide and acceptance criteria:
  `docs/operations/launchd-qos.md`.
