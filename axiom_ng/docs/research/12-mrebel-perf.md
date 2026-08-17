# 12 — mREBEL-Decoder-Performance (G2 #179 Vorforschung, #171 Fortführung)

Zielhierarchie (dudu): **p50 ≤ 0,2 s pro Chunk = "kein Rückschritt" (Endbar)**; 0,5 s war nur ein
Meilenstein. Sidecar = allerletzte datenbasierte Reserve. Regression-Gate nach JEDEM Schritt:
Triple-Set-Parität ≥ 95 % gegen `mrebel_ref_50.json` (n=50) + 2×-Determinismus (byte-gleich).
Messumgebung: Carrier 192.168.1.2, RTX 3090 GPU 0, Container `study-mrebel`, alles GPU.

## Schritt 0 — GPU-Hygiene & Exklusivität (VOR den Messungen)

- `study-minirunner` (8 h up, 6,5 GB auf GPU 0, durchgehend **0 % Util** — R7 verifiziert, obsolet)
  und `runner-carrier-gpu1` (Python, 4 GB auf GPU 1; vor dem Stopp 3× 0 % Util nachgewiesen)
  **gestoppt** (`podman stop`, nicht entfernt — `podman start` belebt wieder). Danach: alle GPUs
  1 MiB, keine Compute-Prozesse → **mrebelgo exklusiv auf GPU 0**.
- Kontrollbefund: die Baseline mit residentem (idle) Minirunner (p50 4,45 s) ist nach
  Exklusivität **identisch** (4,459 s) — die alten Zahlen waren nicht kontaminiert; der
  Flaschenhals lag eh CPU-seitig (s. Schritt 1).

## Schritt 1 — Diagnose (messen, bevor bauen)

Instrumentierung (`MRBEL_TRACE=1` → `/tmp/mrebel_trace.jsonl`, ein JSONL pro ORT-Call;
`MRBEL_DUMP_STEPS=1` → Beam-Schritt-Dump; `analyze_trace.py` / `cmp_steps.py`).

**Repeat-Chunk-Test** (Chunk 0 ×10): 2,81 s → 2,64 s (Warm-up ~6 %). **Shape-Replan-Hypothese
damit endgültig widerlegt** — kein first-occurrence-Effekt pro Shape (Trace: 0/118 Shapes
„first >> rest", nur der Encoder 41→8 ms).

**Der eigentliche Befund (Trace, exklusiv gemessen):**

| Größe | Wert |
|---|---|
| ORT-Calls pro Chunk | ~44 (Repeat-Chunk) / 68 (50er-Satz, median) |
| **Zeit pro ORT-Run** | **median 6,7–7,0 ms** — ORT/CUDA ist schnell, dynamische Shapes kosten nichts |
| ORT gesamt pro Chunk | ~456 ms |
| Wall pro Chunk (damals) | 2.650 ms → **~2,2 s lagen in GO-CODE zwischen den Calls** |
| GPU-Util im Lauf | 6–13 % (GPU fast idle → CPU-gebunden) |

**Bottleneck war `topKIndices`: Voll-Sortierung von 250.071 Indizes pro Beam-Call**
(~50–100 ms × 44–68 Calls) plus `logSoftmax`-Doppelpass (250 k Float64). Meine frühere
„90 ms fixer ORT-Per-Call-Overhead"-Deutung war ein Artefakt (Calls falsch gezählt).

**Python-Studie („Blaupause studieren", dudu-Auftrag):** transformers 4.57.6 default
`generate()` für MBART = **DynamicCache mit `torch.cat(key, dim=-2)` pro Schritt**
(`DynamicLayer.update`), eager — die vor-allokierte StaticCache ist opt-in
(`cache_implementation`, default `None`). Fazit für Go: statische Max-Buffer sind NICHT der
Schlüssel (Replan widerlegt), sondern **ein gebatchter Call pro Decoding-Schritt für alle 3
Beams** — genau Pythons Struktur (batch = 3 × 1 Token/Call). → Opt-3.

## Schritt 2 — Opt-1: Logits-only-Graph + Tensor-Reuse (223b4f8)

`onnx`-Trim auf `decoder_logits.onnx` (nur `logits`-Output; die 48 ungenutzten `present.*`
fielen weg) + Wiederverwendung von `enc_mask`/`enc_hidden`-Tensoren pro Chunk.

**Ergebnis: p50 4,153 s (−7 %), Parität 96 % ✓, deterministisch ✓.**
Lehre: Allokations-/Bandbreiten-Overhead war NICHT der Flaschenhals — wertvoller als der
Gewinn war der Falsifizierungs-Befund für die nächste Hypothese.

## Schritt 3 — Opt-2: Fused O(n) topK-Log-Softmax (e3b6218)

`topKIndices`-Vollsortierung + `logSoftmax`-Doppelpass ersetzt durch **einen Scan**
(Selektion der Top-6 nach rohem Logit — monoton zur logprob, kein Sort, kein 250 k-Float64-
Slice) + einen LSE-Pass; `decodeStep` liefert rohe Logits. Tie-Behaviour identisch zum
früheren stabilen Sort (strenger `>`, niedrigerer Index gewinnt).

**Ergebnis (GPU-exklusiv, 50er-Satz):**

| Metrik | Baseline (c174716, exklusiv) | Opt-2 |
|---|---|---|
| **p50** | 4,459 s | **0,543 s** (**8,2×**, −88 %) |
| p95 | 9,531 s | 1,340 s |
| mean | 5,102 s | 0,648 s |
| Parität (Gate) | 96 % | **96,0 % ✓** (Outlier 23/48 = bekannte Tokenizer-Kanten) |
| Determinismus (Gate) | ✓ | **byte-gleich ✓** |
| ORT-Anteil an Wall | — | 456/543 ms = 84 % (Go-Overhead nur noch ~90 ms) |

0,5-s-Meilenstein erreicht; Zwischenziel p50 ≤ 0,2 s offen.

## Schritt 4 — Cached-Pfad-Parität GEKLÄRT (Hivemind-Zusatz b; zwei Logik-Bugs, kein FP)

Neu-Messung nach Opt-2 bestätigte 86 % mit GROSSEN Abweichungen (Chunk 2: 10 Zusatz-
Triples) — kein Near-Tie-Rauschen. First-Divergence-Diff (`cmp_steps.py`): Cached lief
**255 Runden** und expandierte **`[tp_XX, EOS]` als lebenden Beam** (Generierung an EOS
vorbei → Halluzinations-Ketten). Zwei Fixes:
1. **Eviction nicht portiert** beim Cached-Loop-Re-Write (plain `append` statt `addHyp`)
   → 43→47/50.
2. **Loop-Init leitete Schritt-1-Top-6 ungefiltert in `beams`** (EOS-Kandidat wurde
   expandiert) → Init spiegelt Loop-Semantik (EOS→done+Eviction, numBeams-Cap) → **96,0 %**.

**Cached jetzt: GATE PASS (96,0 %, byte-deterministisch, p50 0,583 s)** — kein Tempo-
vorteil unbatched (Per-Call-dominiert), aber korrekt. Die Hivemind-Verdachtsrichtung
„echter Cache-Ordering-Fehler" war richtig (in Go-Semantik: EOS/Beam-Lifecycle, nicht
KV-Reihenfolge — die war korrekt).

## Schritt 5 — Opt-3: Beam-Batching [B, L]

Ein Decoder-Call pro Runde für alle 3 Beams (B∈{1,3}; Encoder-Tensoren batch-
materialisiert) — exakt Pythons generate()-Struktur.

| Metrik | Opt-2 | Opt-3 |
|---|---|---|
| p50 | 0,543 s | **0,364 s** (−33 %) |
| p95 | 1,340 s | 1,037 s |
| ORT-Calls/Chunk | 68 | **24** |
| ORT-ms/Chunk | 456 | 269 (11,2 ms/call) |
| Gate | PASS | **PASS** (96 %, byte-gleich) |

## Schritt 6 — Opt-4: `logits_last`-Graph-Trim

ONNX-Chirurgie auf `decoder_logits.onnx`: Slice-Knoten (Achse 1, starts=−1, ends=INT64_MAX)
→ Output `logits_last` **[B, 1, 250071]**; validiert (max|d| 0.0 vs `logits[:, -1, :]`).
Kills den L-skalierten Logit-Download (~48 MB → 3 MB pro Call bei L=16).

| Metrik | Opt-3 | Opt-4 |
|---|---|---|
| p50 | 0,364 s | **0,249 s** (−32 %) |
| p95 | 1,037 s | 0,582 s |
| ORT-ms/Chunk | 269 | 171 (**7,1 ms/call**) |
| Gate | PASS | **PASS** (96 %, byte-gleich) |

**Kampagnen-Stand:** Baseline 4,459 s → 0,249 s = **17,9×**; Ziel ≤ 0,2 s offen
(Rest: ~78 ms Go-Overhead [72 TopK/LSE-Scans à 250k] + 171 ms ORT à 24 Calls).

## Schritt 7 — Opt-5a/5b/6: die letzten Hebel, gemessen

- **Opt-5a (parallele Beam-Scans):** die 3 TopK/LSE-Scans pro Call parallel (Goroutines,
  indizierte Schreiber — Arithmetik identisch). **p50 0,209 s**, Gate PASS.
- **Opt-5b (Cached+Batched):** with_past-Beam-Search gebatcht ([B,1]-Schritt + Batch-Cache
  [B,16,L,64] inkl. Zeilen-Re-Parenting bei der Selektion). Korrekt (Gate PASS 96 %), aber
  **p50 0,375 s — SCHLECHTER** als no-cache: das 49-Tensor-Interface des with_past-Graphen
  (48 Einzel-KV-Inputs) kostet mehr als die gesparte Präfix-Rechnung einbringt. Lehre: die
  Schnittstelle, nicht die Mathematik, ist der with_past-Flaschenhals in onnxruntime_go.
- **Opt-6 (IOBinding, dudu-Auftrag „erst ausreizen"):** Konstant-Eingänge über Calls gebunden
  (`CreateIoBinding`/`BindInput`/`RunWithBinding`). **p50 0,208 s ≈ kein Gewinn** —
  onnxruntime_go-Tensoren liegen im HOST; ORT kopiert trotzdem pro Run host→device.
  Gemessen, nicht behauptet: IOBinding ist im Host-Tensor-Modell ausgereizt.

## Schritt 8 — Opt-7: In-Graph LogSoftmax+TopK-Fusion (ENDZIEL)

Graph-Chirurgie auf `decoder_logits_last.onnx`: `LogSoftmax(axis=-1)` + `TopK(K=6)`
angehängt, Outputs nur noch `ftk_topk_ids/ftk_topk_logps` **[B,1,6]** → pro Call verlassen
**6×2 Werte** die GPU statt 3 MB Logits; die 250k-Host-Scans entfallen vollständig
(decodeStepB liefert direkt Kandidaten). TopK-IDs gegen numpy exakt validiert.

| Metrik | Opt-5a | Opt-7 |
|---|---|---|
| p50 | 0,209 s | **0,195 s** ✓ ≤ 0,2 s |
| p95 | 0,496 s | 0,473 s |
| mean | 0,252 s | 0,233 s |
| Gate | PASS | **PASS** (96,0 %, byte-gleich; Outlier 23/48 unverändert) |

## Endstand & Einordnung (alles GPU-exklusiv, n=50)

| | Baseline (c174716) | Opt-2 Scan | Opt-3 Batch | Opt-4 last | **Opt-7 fused** | Python (full-chunk) |
|---|---|---|---|---|---|---|
| **p50** | 4,459 s | 0,543 s | 0,364 s | 0,249 s | **0,195 s** | 0,141 s |
| p95 | 9,531 s | 1,340 s | 1,037 s | 0,582 s | 0,473 s | 0,314 s |
| Faktor | 1× | 8,2× | 12,3× | 17,9× | **22,9×** | (Referenz) |

- **Endbar p50 ≤ 0,2 s ERRICHT** (0,195 s) bei durchgehendem Gate (96 % Triple-Parität,
  2×-Determinismus). Go liegt bei **1,38× Python** (0,195 vs 0,141 s).
- Verbleibende Strukturkosten (datenbasiert): ~24 Calls × ~7 ms — Per-Call-Fixkosten des
  Host-Tensor-Modells (Encoder-Hidden [3,317,1024] ≈ 3,9 MB Upload pro Call; ORT-Run-
  Overhead über 3 Inputs/2 Outputs) + Rest-Go (~20 ms: Tokenizer/Decode/Parse). Pythons
  Vorsprung = durchgängige Device-Residenz (KV + logits + topk bleiben auf GPU).
  onnxruntime_go bietet keine Device-Tensor-API; IOBinding gemessen ohne Gewinn. Das
  letzte Drittel (0,195→0,14) wäre cgo/C-API-Device-Buffer-Arbeit — benannter Epic-Punkt,
  nicht mehr Vorforschung.
- **Epic-Empfehlung (G2 #179):** Go-nativ mit Opt-7-Stack (logits-only-Trim + Batch +
  in-Graph-TopK) ist produktionsreif vorqualifiziert: 96 % Parität, deterministisch,
  0,195 s/Chunk auf einer 3090. Sidecar bleibt die Reserve, nicht der Plan.

## Artefakte & Befehle

- `mrebelgo/analyze_trace.py`, `cmp_steps.py`, `parity_gate.py`, `repeat0.json`;
  `carrier_results/opt2_parity{,_r2}.json`; Trace/Logs `/tmp/mrebel_trace*.jsonl`.
- Lauf (Carrier): `podman run --rm --device nvidia.com/gpu=all -e CUDA_VISIBLE_DEVICES=0
  -e ORT_CUDA=1 -e ORT_CUDA_DEVICE=0 [-e MRBEL_TRACE=1] … study-mrebel:latest bash -c
  "cd /study/cmd/feasibility/mrebelgo && go build -o /tmp/mg . && /tmp/mg
  /opt/onnxruntime/lib/libonnxruntime.so.1.29.0 /models/mrebel_onnx /models/sample_chunks.json
  /study/cmd/feasibility/mrebelgo/parity_idxs.json /tmp/out.json"`

## Nächste Schritte

1. **Cached-Pfad-Parität (86 %) als eigener Punkt** (Hivemind-Zusatz b): First-Divergence-
   Diff `nocache` vs `cached` Steps-Dump — Achtung: alte 86 %-Messung enthielt noch die
   Sort-Tie-Breaks; nach Opt-2 neu messen, dann diffen.
2. **Opt-3: Beam-Batching [3, L]** — ein ORT-Call pro Decoding-Schritt für alle 3 Beams
   (68 → ~23 Calls/Chunk; Schätzung p50 ≈ 0,2 s).
3. Falls danach noch > 0,2 s: Provider-Optionen/IOBinding prüfen; sonst ehrliche Abgrenzung.
