# Feasibility Study — Block 7 · GLiNER-ONNX + mREBEL-Options (#171)

## GLiNER-ONNX: zero-shot parity PROVEN

The runner uses `urchade/gliner_multi-v2.1` (DeBERTa-v3 backbone + span head),
labels → internal types (person/organization/location/concept/work/method). The
installed `gliner 0.2.28` ships its own ONNX path (`load_onnx_model=True` +
`export_to_onnx`).

Measured (real German sample, 2.3 s export):

```
text: "Das St. Galler Management-Modell … Prof. Andreas Müller von der Universität St. Gallen
      forscht zu Nachhaltigkeit und Controlling."
PyTorch  → person(Prof. Andreas Müller,0.975) org(Universität St. Gallen,0.832)
           concept(Nachhaltigkeit,0.706) concept(Controlling,0.595)
ONNX     → identical entity set, max |score diff| = 1e-05
```

**Crux satisfied:** GLiNER zero-shot NER parity across PyTorch↔ONNX holds exactly
(entity set identical, scores ≤1e-5 apart). Because GLiNER is now ONNX-runnable, the
entity extraction can move to `onnxruntime` — either as the Python runner's own
`load_onnx_model=True` path (drop torch for this component) or, in a Go runner, via
`onnxruntime_go` on the exported `model.onnx` (same engine as dense/rerank) —
note that parity so far is proven PyTorch↔ONNX **in Python**; the Go
`onnxruntime_go` run itself is still pending. The DeBERTa tokenizer is
XLM-R-family (SPM unigram + reindex, same mechanism) but **NOT the same vocab**
as the Block-2 BGE-M3 XLM-R pin — a Go GLiNER port needs its own tokenizer pin.

**Device note:** DeBERTa backbone is `microsoft/deberta-v3-base` — ONNX CPU/GPU
portable. On the 3090 farm, GLiNER-ONNX via onnxruntime CUDA-EP is the natural
ingest acceleration (the runner currently pins `DEVICE_GLINER=cpu`, the L8 lesson).

## mREBEL options (options only — nothing built, per Nicht-Ziele #3)

`Babelscape/mrebel-large` (BART-large, Seq2Seq) extracts entity *relationships*.
It is the runner's heaviest-to-port component (autoregressive decode). Three options:

| Option | Effort | Device matrix | Verdict |
|---|---|---|---|
| **1. Sidecar (keep Python mREBEL service)** | Low — reuse `relation_extractor.py` over an HTTP sidecar | CUDA-farm / MPS; Python stays for this one component | **Recommended for a Go runner** — isolates the ONLY non-Go port |
| **2. ONNX-Decoding-Loop** | High — BART encoder + autoregressive decoder ONNX; attention-cache/beam handling in Go; triple-parser; ~1.5GB model | CPU slow; CUDA needed; ONNX BART decode is notoriously fiddly | Not worth it: high effort, fragile decode, marginal gain |
| **3. Triple-Mining replacement** | Moderate — rule/co-occurrence relation miner in Go | portable (pure compute) | Loses relationship quality / multi-hop expressiveness; only if mREBEL value is low in practice |

**Recommendation:** **Sidecar** (option 1). mREBEL is the single weak corner; a Go
runner keeps it as a Python HTTP sidecar on the farm (exactly how the current
runner already separates it), and Go handles everything else natively. Do NOT
attempt BART ONNX decode in Go (option 2) — high effort, fragile. Option 3 only if
a follow-up shows mREBEL's contribution is marginal for the KGs this library needs.

(Other options seen: LLM-side triple extraction — ollama/localai — is out of scope
per Nicht-Ziele #4, only named here as a possible future replacement.)

## Device matrix addendum (Block 7)

| Component | Proven path (this study) | Runs on CUDA? |
|---|---|---|
| GLiNER-ONNX | **ONNX export + load_onnx_model, parity ≤1e-5** | yes (onnxruntime CUDA-EP) |
| mREBEL | **sidecar only (recommended)** | yes via Python service on farm |
