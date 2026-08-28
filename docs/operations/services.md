# Services & Deployment (#205)

Standard installation is the product. Nix is the documented exception case,
not the reference path. `make` is a dev surface only.

## Components

| Name | What | Artifact | Install |
| --- | --- | --- | --- |
| `rag` | Go binary (API + dispatcher are modes of one binary) | `axiom-ng-<version>-darwin-arm64` | `/opt/axiom/rag/<version>/` |
| `runner` | axiom_ng_runner (query embed/rerank + ingest, MPS) | `axiom-runner-<version>-macos-arm64.tar.zst` (conda-pack) | `/opt/axiom/runner/<version>/{env,app}` |
| `fixer` | pdf_repair_agent (autarkic, event runner) | `axiom-fixer-<version>-macos-arm64.tar.zst` | `/opt/axiom/fixer/<version>/{env,app}` |

## Port map

| Port | Service |
| --- | --- |
| 8011 | `com.axiom.rag` API (bind via `rag-api.env`, LAN needs 0.0.0.0) |
| 8012 | `com.axiom.runner` processor (`AXIOM_PROCESSOR_COMPUTE=real`) |
| 8013–8015 | `com.axiom.rag-dispatch-gpu0/1/2` dispatcher instances |
| 19542–19544 | Carrier runners (or carrier-bridge on 127.0.0.1) |

## Install layout & staging rule

```text
/opt/axiom/<component>/<version>/    + current -> <version> (atomic symlink)
/opt/axiom/bin/                      stable shims: axiom-ng, axiom-runner, axiom-fixer
~/.config/axiom/*.env                env files, 0700, SECRETS ONLY HERE
~/.local/state/axiom/{logs,runs,models}/   state OUT of /opt and OUT of the repo
```

Staging rule: builds stage in `dist/` (repo-ignored). Only finished,
checksum-verified artifacts reach `/opt` — via `make install` (local build)
or `scripts/install_release.sh` (GitHub release). **Production installs come
from GitHub releases, never from a developer's /tmp.** Old artifacts in
`dist/` go stale — `make clean` before releases.

## Production install (from GitHub)

```sh
scripts/install_release.sh rag <tag>        # download + checksum + gated /opt install
scripts/install_release.sh runner <tag>     # includes one-time conda-unpack
scripts/install_release.sh fixer <tag>      # includes one-time conda-unpack fixup (bundled interpreter, #208)
```

Both paths verify the sha256 sidecar BEFORE the operator prompt and before
anything under `/opt` moves. Rollback = flip the `current` symlink back and
`launchctl kickstart -k gui/$(id -u)/com.axiom.<service>`.

## launchd service group

Installed by `scripts/install_services.sh` (confirmation prompt; substitutes
`__HOME__` — launchd does not expand `$HOME` in `Standard*Path`):

- `com.axiom.rag` — RunAtLoad + KeepAlive (reboot survival)
- `com.axiom.rag-dispatch-gpu0/1/2` — one per Carrier runner
- `com.axiom.runner` — standing service
- `com.axiom.carrier-bridge` — TEMPLATE (opt-in `--with-bridge`), the
  Apple-python bridge workaround for TCC-blocked unsigned binaries; may
  retire once binaries are TCC-granted
- fixer: **NO service** — event runner, see below

```sh
launchctl print  gui/$(id -u)/com.axiom.rag        # status
launchctl kickstart -k gui/$(id -u)/com.axiom.rag  # restart
launchctl bootout    gui/$(id -u)/com.axiom.rag    # stop
```

Conventions in `deploy/launchd/README.md`: env-file sourcing via `sh -c`
wrapper, runner exec'd as `env/bin/python -m axiom_ng_runner` (conda-pack
shebang ceiling).

**Restart order is runner → rag → dispatchers.** A rolling restart must bring
up the runner (8012) before the dispatchers (8013–8015): a dispatcher that
starts before the runner boots now retries capability negotiation with backoff
(#214) and exits non-zero (so launchd/KeepAlive restarts it) if the runner
stays unreachable past the ~2min window or proves unusable — so the wrong
order is survivable, but `launchctl kickstart -k` of the
runner first, then the rag, then the dispatchers is the clean sequence.

## PDF preflight — quality gate before chunking (#175)

The ingest dispatcher can run a read-only quality gate on each claimed PDF
BEFORE full processing: with `AXIOM_DISPATCHER_PREFLIGHT=1`, the claimed
source is POSTed as raw bytes to the runner's `/v1/pdf/preflight`, which
returns a structured report — text layer present, page count, per-page text
density, blank-image-only series, and label/folio anomalies (all cheap
pymupdf metrics, no ML; no repair, no upstream mutation). The verdict lands
in `ingest_jobs.quality_state` and is readable on the job listing.

Policy: green/yellow (`ok=true`) proceeds to full processing. A red verdict
(e.g. a textless scan, `🔴 unpaginiert`) is **not** sent into the pipeline —
the job is archived with a clear status (`skipped`, reason
`preflight:<verdacht>`) and the attachment is registered as a **repair-case
candidate**, so the fixer (#206/#203) can later heal it rather than letting
junk chunks pollute the index. Advisory by design: if the source is not
locally readable or the preflight call fails, the job proceeds normally —
preflight never blocks a job it cannot measure.

### EPUB preflight (#220)

The same endpoint serves EPUBs: `Content-Type: application/epub+zip` routes
to the EPUB analyzer (zip integrity, OPF/spine presence, DRM detection —
`META-INF/rights.xml` or non-font-obfuscation `encryption.xml` — and a
text-extraction check), plus the external **epubcheck** conformance stage
(W3C, BSD-3, JSON mode; `AXIOM_PROCESSOR_EPUBCHECK_CMD`, auto-detected on
PATH, reported as `not_available` when absent). Identical policy: red →
skip + repair-case, green/yellow with extractable text → ok. No Java is
bundled into the runner artifact (conda-pack, #208) — the jar lives in the
host environment.

### EPUB page anchors → citable print pages (#220)

When an EPUB ships publisher page anchors (`epub:type="pagebreak"`,
`class="page"` — Jossé/dtv —, `id="page_N"` — Bieger/Springer —, or Adobe
`page-map.xml`), the runner parses them into a uniform map and enriches
`epub_cfi` locators with additive `page_start`/`page_end`, a `chapter`
ordinal (spine parity with PDF locators) and a print-page trust level. The
book's own printed TOC decides (#223): TOC page numbers matching the
chapter-start anchors → `page_source: "print_verified"` (citable print
folios, proven offline); markers without a printable TOC →
`print_unverified` (present as marker pagination, never as print folio).
Anchors are only trusted when their page numbers form a monotone, plausible
sequence in spine order — non-monotone/implausible maps are refused, and a
TOC that systematically diverges (the vendor reader-pagination class,
constant per-chapter drift) is DETECTED and refused too (locators stay
chapter+CFI, `page_source: "none"`). Never silently upgraded.

### EPUB repair toolbelt + repair-case wiring (#220 Stage 2)

Mechanical, model-free EPUB repairs live in `compute_core/epub_repair.py`
(stage-1 capable): `normalize_entry_paths` (the promoted W9/Z3 experiment
— pandoc-safe package view, OPF at the archive root, no `..` references;
the epub worker imports it from there now), `repair_spine` (synthesize a
missing `<spine>` from the manifest) and `remove_entry_corpses` (drop
unreferenced zip entries). `apply_repairs` chains them and proves the
result through the same preflight analyzer (red→green discipline as with
PDF surgery). Dispatcher wiring: preflight bytes now carry the
attachment's content type, so EPUB claims run the EPUB gate and red
verdicts (incl. epubcheck FATAL/ERROR findings, which land in the
quality-state details) create repair cases on the same queue — the #206
invoker path is format-agnostic and picks them up unchanged.

## Fixer: event runner (owner decision)

One process per Zotero attachment key, invoked via the tested wrapper
`scripts/fix.sh <zotero-key> [--apply]` — per-key lock
(`~/.local/state/axiom/runs/fix-<key>.lock`, concurrent same-key runs exit 3)
and a 30-minute hard timeout. Two simultaneous repair instances on the same
PDF would destroy the working directory; the wrapper serializes. The
`--key` contract stays per-invocation — no env file, no static key.

If a run is SIGKILLed or the host loses power, the lockdir may survive with
a dead pid — the wrapper detects that and recovers automatically. Manual
recovery (only if the pid file is unreadable):
`rmdir ~/.local/state/axiom/runs/fix-<key>.lock`.

### Fixer caller — the mail-ingest invoker (#206)

The systematic caller of `fix.sh` is the **fixer invoker**, a loop inside
the axiom binary (NOT a launchd service, NOT KeepAlive — the event-runner
owner decision stands). Enabled with `AXIOM_FIXER_INVOKER_ENABLED=1` in the
env file of an axiom instance that also carries the Zotero write key
(the repair API must be wired — the invoker uploads through it).

- **Who/when:** polls the `repair_cases` queue (status `queued`) every
  30 s, resolves each case to its attachment key, claims it
  (`queued → in_repair`, loop guard included), and invokes
  `AXIOM_FIXER_CMD <key> --apply` (default `/opt/axiom/bin/axiom-fixer`, the
  same wrapper contract as `scripts/fix.sh`). One invocation per key per
  claim — the one-shot `--key` contract is untouched.
- **Timeout:** fix.sh's own 30-min kill (lockdir + timeout binary) does the
  primary work; the invoker runs a 35-min context backstop above it, so a
  wedged wrapper can never hang the invoker.
- **Concurrency:** `AXIOM_FIXER_CONCURRENCY` (default 1, clamped to 1–2)
  parallel fixer runs per host — the per-key lockdir additionally
  serializes against manual operator runs (exit 3 is treated as an
  ordinary retryable failure).
- **Success:** fixer exit 0 AND a healed `work.pdf` under
  `~/.local/state/axiom/runs/<key>/` — exit 0 alone is NOT success (a
  green exit without the artifact fails). The healed pdf runs through the
  audited custody sequence (quarantine original → delete → create/upload
  schema filename → `healed`) and the next sync/preflight GREEN re-ingests
  it — the normal ingest path, no side door.
- **Failure & retry:** non-zero exit, timeout, or missing healed pdf → the
  case goes back to `queued` while attempts remain (max 2 per attachment,
  the `repair_attempts` loop guard), then parks as `failed` with the
  fixer's exit reason; the third claim of a still-broken attachment is
  `blocked_for_dudu` (loop-guard escalation). A case whose attachment
  vanished at the source parks `blocked_for_dudu('attachment-gone')`.
- **Crash safety:** the invoker never dies on a case (per-case recover,
  per-case timeout, process-group kill on the backstop) and a dead invoker
  loses no case: stale `in_repair` claims older than 40 min are requeued by
  the per-tick reaper — the dispatcher lease-recovery pattern; the loop
  guard still caps attempts. The installed `/opt/axiom/bin/axiom-fixer`
  shim execs the fix.sh shipped INSIDE the artifact, so invoker and manual
  operator runs share the same per-key lock. Narrow residual: if the
  attachment disappears between item resolution and claim, the case ends
  `failed` with a Zotero-404-flavored reason instead of
  `blocked_for_dudu('attachment-gone')` — bounded and visible.

The fixer has NO env file in this scheme: its package-local `config.env`
(via its own `load_config_envfile`, inside the artifact) carries the
non-secret settings, and `--key` stays a per-event argument.

### Stufe-2 — autonomous diagnosis & decision (#203)

Inside that one `fix.sh <key> --apply` invocation, the agent is the
**Stufe-2 case driver**: it does not only run a single canned repair, it
*decides* what to do per case within the Stufe-1 toolbelt. Stufe-1 stays
the machine, Stufe-2 the judgment — there is **no new call path or second
wrapper**: the fixer invoker's existing wrapper contract is unchanged
(Key per invocation, per-key lock, 30-min timeout, `--apply`).

**How the agent decides.** Each model turn emits one JSON `step` that the
dispatcher validates and maps onto the deterministic tools (probe,
forensics, spread, ocr, surgery). The loop follows:

1. **Diagnose** — forensics (druckseiten truth map, the normal path) or
   the integrity probe where chunk/zitat evidence is present.
2. **Werkzeugwahl + Operation** — validates the damage class (one of
   `constant-offset` / `reprint-start` / `two-range` / `injection`;
   `unclassifiable` → escalate) and selects the matching Stufe-1 operation
   (surgery / spread / ocr) — or **nothing**: „nichts tun, weil <Grund>" is
   a first-class verdict reported honestly via `stop`, never a silent no-op.
3. **Verifikation** — measure-after (3-Stellen-Beweis where present, else
   Stelle-1 green + open places named) before any „geheilt" claim.
4. **Bericht** — `report.json` under `~/.local/state/axiom/runs/<key>/` with
   `verdict`, `truth_source`, and the mandatory „was unbewiesen blieb"
   section. A `budget` / `time-budget` / `escalate` verdict is a normal
   report, not a crash.

**Budgets (abbruch-sicher, no unbounded case).** Both limits live in the
agent itself, inside the wrapper's external 30-min kill:

- **Operations:** `PDF_REPAIR_BUDGET_MAX_OPS` (default 50) — every non-terminal
  model round-trip (failed attempts included) counts; exceeding ends the
  run with verdict `budget`.
- **Wall-clock (Stufe-2):** `PDF_REPAIR_BUDGET_MAX_SECONDS` (default 900,
  15 min per case) — a hard time cap independent of the step count, so a
  slow/hung model endpoint cannot stretch a case past its slot. On expiry
  the loop terminates with verdict `time-budget`; the agent cannot argue
  its way past it. `0` disables it (hermetic tests / sandbox only).
- **Abort-safety:** 2 failed attempts of the same class → escalate; a
  client error or handler crash is captured as evidence/report, never a
  bare traceback without an audit record.

**Boundaries.** The agent operates ONLY through the Stufe-1 tools — it never
writes PDF bytes directly (the single label-write path is surgery_exec) and
never builds ad-hoc tools. Its freedom is *which* validated Stufe-1
operation to apply (or none), never *whether to bypass* the toolbelt. That
keeps Stufe-2 a judgment layer: deterministic, reproducible tooling below;
autonomous planning on top.

## Env files & secrets

One file per service in `~/.config/axiom/`, mode 0700. Secrets (DSN, HMAC
source secret, DeepSeek keys) live ONLY there — never in `/tmp` (the reboot
wiped /tmp and took the secret with it), never inline in plists.

- `rag.env` — shared: `AXIOM_DATABASE_URL`, `AXIOM_OPENSEARCH_URL`,
  `AXIOM_PROCESSOR_SOURCE_BASE_URL`, `AXIOM_PROCESSOR_SOURCE_SECRET`
- `rag-api.env` — instance: `AXIOM_API_PORT=8011`, `AXIOM_BIND_ADDR`,
  `AXIOM_DISPATCHER_ENABLED=0`
- `rag-dispatch-gpuN.env` — per dispatcher: port 8013+,
  `AXIOM_DISPATCHER_ENABLED=1`, worker id,
  `AXIOM_DISPATCHER_PROFILE` (the empty-profile trap: assert non-empty!)
- Ingest runner selection (#207): `AXIOM_PROCESSOR_URLS` = ordered candidate
  list (e.g. `http://<carrier-host>:<port>,http://127.0.0.1:8012` — Carrier
  first, local floor last). The SAME env file works at home and on the
  road: without the Carrier, the health probe skips it and ingest runs
  locally — no reconfiguration, no failover timeout per submit.
  The legacy pair `AXIOM_PROCESSOR_URL` + `AXIOM_INGEST_FALLBACK_URL` keeps
  working (folded into a two-entry chain); plural wins when both are set.
  Optional: `AXIOM_RUNNER_HEALTH_INTERVAL` (default 60s).
- `runner.env` — `AXIOM_PROCESSOR_COMPUTE=real`, `AXIOM_PROCESSOR_PORT=8012`

Operational traps with env files (first two hit during the v0.1.11
production install, 2026-08-23):

1. **`AXIOM_BIND_ADDR` decides what answers locally.** With
   `AXIOM_BIND_ADDR=<lan-ip>` (the host LAN address, needed when remote
   runners pull source artifacts from this host's API), the RAG does NOT
   answer on `127.0.0.1`/`localhost` — a health call against
   `localhost:<port>` goes nowhere. Local scripting on the host must use
   the LAN IP (or hostname). `0.0.0.0` would answer everywhere but exposes
   the API on all interfaces — deliberate trade-off, documented here so it
   does not bite the next person. Known install findings: #210.
2. **Port changes can span multiple env files.** The API port appears not
   only in `rag-api.env` (`AXIOM_API_PORT`) but also — if the source-pull
   path is active — in `rag.env`
   (`AXIOM_PROCESSOR_SOURCE_BASE_URL=http://<host>:<api-port>`). Changing
   only the former silently breaks remote-runner source fetches (404 on
   every dispatcher job). After any port change:
   `grep -rn <old-port> ~/.config/axiom/` must come back empty (comments
   aside), then `launchctl kickstart -k gui/$(id -u)/com.axiom.rag`.
3. **JSON values in env files must be SINGLE-quoted.** POSIX `sh` strips
   double quotes on assignment: `AXIOM_DISPATCHER_PROFILE={"jobs": …}`
   sources to `{jobs: …}` — unquoted JSON keys reach the config, the
   frozen.go validation rejects every dispatcher claim, and all
   dispatchers spin in an endless loop with jobs pending forever
   (v0.1.12 live finding, 2026-08-24; hot-fixed with single quotes +
   `launchctl kickstart`). A spaced double-quoted variant (`{"jobs": 1}`)
   word-splits and makes sourcing fail outright — under the #210 guard the
   service then crash-loops on start. Recipe:
   `AXIOM_DISPATCHER_PROFILE='{"jobs": …}'`.
   `scripts/install_services.sh` preflights this: it sources every env file
   (files that do not source cleanly are named as preflight errors) and refuses to install when a set `AXIOM_DISPATCHER_PROFILE` does not
   parse as JSON (fail-closed before the confirm prompt, like the #210 DB
   guard and the #211 zstd check).

The fixer has NO env file in this scheme: its package-local `config.env`
(via its own `load_config_envfile`, inside the artifact) carries the
non-secret settings, and `--key` stays a per-event argument.

## Debug vs production (DoD core, #205 §5)

- Production artifacts are release-built, version-stamped, checksummed.
  `axiom-ng --version` AND `/api/health` (field `build`) must report the
  same banner. If the banner says `debug build`, it is NOT production.
- A debug build REFUSES to bind ports 8011–8015
  (`AXIOM_ALLOW_DEBUG_BIND=1` opts out for local dev). The 2026-08-23
  incident — an instrumented debug build serving production for hours —
  is now machine-enforced, not convention.

## Host dependencies (NOT bundled in artifacts)

The fixer's OCR lane shells out to **tesseract5** (with `deu` traineddata)
and **ghostscript** — they must be on PATH. Probes:
`tesseract --list-langs | grep deu` and `gs --version`. Without them the
OCR lane reports unavailability (everything else runs).

zstd is needed to build/install the tar.zst artifacts (macOS: `brew install
zstd`, or the nix store path the Makefile picks up automatically).

## VM clock note (Podman wake freeze)

The Podman VM clock can freeze while the Mac sleeps → dispatcher leases 404
on wake. After a long sleep, restart the VM/containers before trusting
dispatcher state (observed 2026-08-22/23 night). If dispatchers report lease
404s out of nowhere, check VM clock first: `podman machine ssh date`.

## Nix exception (nix-darwin host)

Standard path = launchctl + `/opt` + release artifacts (above). On the
nix-darwin host the same services map to home-manager
`launchd.agents.<name>` (pattern: `launchd.agents.llama-swap` in the owner's
nix-conf): stable config under `~/.config/axiom/`, state under
`~/.local/state/axiom`. Host deps via nix: `pkgs.tesseract5`
(`enableLanguages = ["eng" "deu"]`) + `pkgs.ghostscript` in the system
profile. Nix is a wrapper, not a requirement — axiom itself has no flake
(#205 non-goal).

## CI / Releases

Tag `v*` → `.github/workflows/release.yml`: `make rag runner fixer` on
macos-14 (arm64), checksum verify, artifacts + sidecars attached to the
GitHub Release. No linux artifacts — the Carrier path stays containerized.
