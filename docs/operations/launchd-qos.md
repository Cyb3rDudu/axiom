# launchd scheduling (QoS): keep CPU-bound services off Background

Date: 2026-09-21 diagnosis, applied to the shipped templates with the 0.2.0
deployment
Scope: `deploy/launchd/*` service templates; operator measurement guide

## The trap

On macOS, a service's `ProcessType` decides which CPU cores its whole
process tree gets. `ProcessType=Background` places descendants largely on
efficiency cores and applies resource limits — and **a child process
cannot escape the coalition it inherits**: not by `nice`, not by changing
the interpreter, the working directory, or the tool flags.

That classification made an identical 12-process OCR workload 9.4× slower
than the same work under `ProcessType=Standard`:

| ProcessType | Wall time | Mean user cores | Relative wall time |
| --- | ---: | ---: | ---: |
| Background | 82.56 s | 2.45 | 11.08x |
| Standard | 8.81 s | 9.17 | 1.18x |
| Interactive | 7.45 s | 10.47 | 1.00x |

Controlled experiment, evidence, and falsification criteria:
[diagnostics 2026-09-21](../diagnostics/2026-09-21-ocrmypdf-background-qos.md).

## The policy

Every service that spawns CPU-bound work ships with
`ProcessType=Standard`:

| Service | Why |
| --- | --- |
| `com.axiom.rag` | spawns the repair/OCR worker children — the class must be fixed at the service level, the children cannot opt out |
| `com.axiom.compute-worker` | ML inference is CPU/GPU-hungry |
| `com.axiom.rag-dispatch-gpu0/1/2` | dispatcher instances share the role surface; compute can descend from them |

The one deliberate exception: `com.axiom.carrier-bridge` stays
`Background` — a pure network forwarder with no CPU-bound children, where
the Background resource limits are the polite fit.

`Interactive` is not needed: Standard already recovers nearly all
throughput, and Apple reserves Interactive for latency-critical work.

## Measure it yourself

`deploy/launchd/qos-probe.sh` runs the same style of experiment as the
diagnosis — an identical CPU-bound workload (12 parallel sha256 children)
under a temporary launchd job whose ONLY variable is `ProcessType`:

```bash
deploy/launchd/qos-probe.sh --launchd Background
deploy/launchd/qos-probe.sh --launchd Standard
```

Each leg prints `wall`, summed `cpu`, and `mean-cores`. On an 8P+4E
Apple-silicon host expect roughly:

- Standard: ~8-10 mean cores
- Background: ~2-3 mean cores — the same ~9× wall-time gap at equal work

An in-place `deploy/launchd/qos-probe.sh` (no launchd) shows the current
shell's coalition as a baseline.

## Post-change acceptance

After an operator applies `ProcessType=Standard` to an installed agent
(reload = `bootout` + `bootstrap` of the changed plist — an operator
deployment step, not part of this repository), require for a real CPU
stage (e.g. an OCR rebuild):

- aggregate occupancy well above eight cores during the hot phase;
- no child pinned near ~25% CPU while the machine is mostly idle;
- service-path wall time within ~20% of the same command run in a
  foreground terminal.

The probe's launchd legs are the cheap pre-check: run them on any host
before trusting a scheduling change.
