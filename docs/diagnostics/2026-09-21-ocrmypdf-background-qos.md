# OCRmyPDF uses only about four cores on the M2 Max

Date: 2026-09-21  
Scope: production case `ITXG8RX9`, 658-page Bartscher scan  
Result: root cause identified; no production code changed

## Finding

The OCR pipeline is not serialized by Python, the GIL, `Popen`, Ghostscript,
or OCRmyPDF. It inherits Darwin's background resource classification from the
launchd service that starts it.

The installed service is:

```text
~/Library/LaunchAgents/org.nix-community.home.axiom-rag.plist
ProcessType = Background
```

`launchctl print pid/55812` placed the running `axiom-ng` process in the
`org.nix-community.home.axiom-rag` resource and jetsam coalitions. The fixer,
OCRmyPDF, and all Tesseract children descend from that process. On Apple
silicon, background QoS influences placement on efficiency rather than
performance cores. Apple's launchd documentation also states that
`ProcessType=Background` applies resource limits intended to avoid disrupting
the user experience.

The repository contains the same setting in
`deploy/launchd/com.axiom.rag.plist`.

## Decisive controlled experiment

The same Fixer Tesseract binary, the same German BEST model, the same copied
page PNG, `OMP_THREAD_LIMIT=1`, and exactly 12 concurrent processes were run as
temporary launchd jobs. The only changed field was `ProcessType`.

| ProcessType | Wall time | User CPU | Mean user cores | Relative wall time |
|---|---:|---:|---:|---:|
| Background | 82.56 s | 202.52 s | 2.45 | 11.08x |
| Standard | 8.81 s | 80.81 s | 9.17 | 1.18x |
| Interactive | 7.45 s | 78.03 s | 10.47 | 1.00x |

All three jobs exited successfully. The background classification alone made
the identical workload 9.4x slower than Standard and 11.1x slower than
Interactive. Standard already recovered nearly all throughput; Interactive is
not necessary for this workload.

This is also consistent with the end-to-end measurements:

| Run | Start/end evidence | Wall time | Ratio |
|---|---|---:|---:|
| Owner foreground reference | reported `real 10m14.288s`; output mtime 23:38 | 614.3 s | 1.00x |
| Production Background run | log start 12:11:34; OCR output mtime 14:00:18 | 6,524 s | 10.62x |

The independently measured 10.62x production slowdown matches the controlled
11.08x Background/Interactive slowdown.

## Live production evidence

During the OCR stage:

- Twelve Tesseract children existed simultaneously and were all runnable.
- Each child received about 22-25% CPU while the machine remained about 63%
  idle. This is not a lack of queued work.
- An eight-second `sample` of one Tesseract obtained only 2,478 samples rather
  than approximately 8,000. Every captured stack was useful LSTM/NEON OCR
  computation; there was no lock, I/O wait, or OpenMP spin bottleneck.
- The sampled Tesseract had accumulated 27.32 CPU seconds over 119 seconds of
  wall time, consistent with the process receiving roughly one quarter of a
  core.
- The OCRmyPDF parent had accumulated 12:12 CPU over 67 minutes at the first
  detailed snapshot, about 0.18 core on average. A ten-second parent sample
  showed worker threads mostly in `poll()` waiting for Tesseract. One worker
  periodically encoded a PNG in PIL/zlib, but parent CPU was far below one
  saturated core.
- `top` and `iostat` showed 62-64% idle CPU and only 0.1-7.6 MB/s disk traffic.
  The controlled jobs reported zero swaps and zero block I/O.

## Hypotheses checked

### GIL or Python-side serialization: rejected

The parent was not CPU-bound, and the workers were waiting for already-running
Tesseract children. The Python 3.11 parent could not be withholding eight cores
while those independent child processes were runnable. The controlled
ProcessType experiment also used the exact same Python-independent Tesseract
workload and reproduced the whole effect.

The host installation today is OCRmyPDF 17.10.0 under Python 3.14.7, not Python
3.13. A Nix-store OCRmyPDF 17.11.0 installation is also built for Python 3.14.7;
the Fixer is OCRmyPDF 17.11.0 under Python 3.11.16. The A/B/C test held the
Fixer stack constant, so interpreter version is not causal.

### `Popen` or an OCRmyPDF pipeline stage serializes pages: rejected

OCRmyPDF 17.11's OCR stage computes `max_workers = min(page_count, jobs)` and
submits every page to a `ThreadPoolExecutor`. Twelve simultaneous Tesseract
children were directly observed. The main-thread graft step runs as completed
pages return, but the parent CPU measurement shows that it does not starve the
children.

The command-line differences are non-causal:

- Omitting `--jobs` uses `available_cpu_count()`, which is 12 on this machine.
- `-q` changes verbosity and disables the progress bar only.
- `--sidecar` merges and copies accumulated page text after the concurrent page
  stage; it does not change page scheduling.
- Working directory does not alter the executor or QoS classification.

### OCRmyPDF's `nice(5)`: rejected as the differentiator

OCRmyPDF 17.10 and 17.11 both call `os.nice(5)`. On the same copied page, direct
Tesseract took 5.35 s at nice 0 and 5.42 s at nice 5, with 5.21 and 5.27 user
CPU seconds respectively. Nice alone did not reproduce the slowdown.

### Low Power Mode, thermal throttling, swap, or disk: rejected

- `pmset -g custom`: `lowpowermode 0` on AC and battery.
- The machine was on AC at 100% charge.
- `pmset -g therm`: no thermal or performance warning recorded.
- The kernel exposed all 12 recommended cores: 8 Performance and 4 Efficiency.
- No swap I/O occurred during the controlled runs; disk throughput was low.
- Most importantly, changing only `ProcessType` recovered 9-10.5 user cores
  without changing power, memory, binaries, models, input, or environment.

### Exact full reference rerun: no longer possible after successful custody

The requested original path
`~/Zotero/storage/UJVARCDJ/Bartscher...pdf` disappeared when the successful
repair replaced the attachment. The run copy was then replaced by the healed
OCR output: it now has 1,856,460 characters rather than the zero characters
recorded in the pre-repair evidence. Therefore a byte-equivalent full rerun
would require retrieving the quarantined original attachment. The controlled
same-binary/same-input ProcessType experiment is stricter for isolating the
scheduler variable than another end-to-end run would have been.

## Confirmation and falsification criteria

Confirmed by:

1. The production process tree was demonstrably inside a launchd Background
   resource coalition.
2. Twelve runnable Tesseracts received only about a quarter core each while
   most system CPU was idle.
3. Changing only `ProcessType` from Background to Standard reduced identical
   12-process work from 82.56 s to 8.81 s.
4. The controlled slowdown factor agrees with the production/reference factor.

The diagnosis would have been falsified if either:

- the Background and Standard/Interactive jobs had comparable wall time and
  core occupancy, or
- the production descendants were outside the Background coalition and still
  showed the same runnable-but-unscheduled behavior.

Neither falsifier occurred.

## Fix recommendation

### Minimal and recommended now

Change the Axiom RAG launchd job from:

```xml
<key>ProcessType</key><string>Background</string>
```

to:

```xml
<key>ProcessType</key><string>Standard</string>
```

Apply this both to the actual Home Manager/Nix source that generates
`org.nix-community.home.axiom-rag.plist` and to the repository deployment
template. Rebuild/reload the agent normally. Standard achieved 9.17 mean user
cores in the controlled test, close to Interactive's 10.47, without marking a
long-running server as latency-critical.

Keep `--jobs 12` and `OMP_THREAD_LIMIT=1`. They are correct once the process is
not in a Background coalition.

### Cleaner architecture if the server should remain Background

Keep the API/queue service Background, but run CPU-bound fixer invocations in a
separate launchd job with `ProcessType=Standard`. A normal child process cannot
escape the resource classification inherited from the server's launchd
coalition merely by changing Python, `nice`, cwd, `-q`, or `--jobs`.

`ProcessType=Adaptive` only helps when the service uses XPC transactions that
carry a priority boost. The current direct subprocess path does not do that.
`Interactive` works, but Apple's guidance reserves it for work whose
responsiveness is critical; the measurement shows Standard is sufficient.

## Post-fix acceptance test

Run a 12-page or 50-page OCR through the real service and require:

- aggregate OCR occupancy above eight cores during the Tesseract phase;
- individual Tesseract processes no longer pinned near 25% while the machine
  is mostly idle;
- service-path wall time within about 20% of the same foreground command;
- identical page count, geometry, and text-quality gates.

For the full Bartscher original, the observed foreground reference supports an
expectation near 10-12 minutes on this machine, not 1 hour 49 minutes.

## External references

- Apple, [Tuning your code's performance for Apple silicon](https://developer.apple.com/documentation/apple-silicon/tuning-your-code-s-performance-for-apple-silicon/)
- Apple, [Optimize for Apple Silicon with performance and efficiency cores](https://developer.apple.com/news/?id=vk3m204o)
- Apple launchd.plist manual, [ProcessType resource classifications](https://keith.github.io/xcode-man-pages/launchd.plist.5.html)

