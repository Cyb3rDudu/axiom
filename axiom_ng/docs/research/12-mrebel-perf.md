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
