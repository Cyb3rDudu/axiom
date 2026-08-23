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
scripts/install_release.sh fixer <tag>      # includes one-time fix-env fixup
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
  list (e.g. `http://192.168.1.2:19542,http://127.0.0.1:8012` — Carrier
  first, local floor last). The SAME env file works at home and on the
  road: without the Carrier, the health probe skips it and ingest runs
  locally — no reconfiguration, no failover timeout per submit.
  The legacy pair `AXIOM_PROCESSOR_URL` + `AXIOM_INGEST_FALLBACK_URL` keeps
  working (folded into a two-entry chain); plural wins when both are set.
  Optional: `AXIOM_RUNNER_HEALTH_INTERVAL` (default 60s).
- `runner.env` — `AXIOM_PROCESSOR_COMPUTE=real`, `AXIOM_PROCESSOR_PORT=8012`

Two operational traps with env files (both hit during the v0.1.11
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
