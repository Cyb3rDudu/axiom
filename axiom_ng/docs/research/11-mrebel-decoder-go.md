# Restpunkt 6 — mREBEL-Decoder nativ in Go: Decoder-ONNX, Beam-Search-Loop, Parität

**Fazit vorweg:** Die mREBEL-Zeile im Entscheidungsdokument wechselt von „Sidecar (Option 1)"
auf **„Go-nativ möglich"** — der komplette Seq2Seq-Decoding-Stack (Encoder + autoregressiver
Beam-Search-Decoder + Triple-Parser) läuft nativ in Go auf dem Carrier (3090), mit
**96 % Chunk-Level-Triple-Set-Parität** gegen das Python-Orakel (n=50, Schwelle ≥95 %).
Stolpersteine, genaue Zahlen und die ehrliche Performance-Diskussion unten.

Branch `research/go-runner-feasibility`, Worktree `axiom-research`. Alle Artefakte committet.
Modell-Binaries NICHT committet (Regel). Test-DB unangetastet (kein Zugriff nötig — reine
Modell-Studie auf `sample_chunks.json`).

---

## 1. Ziel 1 — Decoder-ONNX-Export (FIX des optimum-Bugs)

**Befund:** optimum 2.3.0 scheitert beim BART-Decoder an `os.remove(decoder_model.onnx.data)`
→ `FileNotFoundError` (external-data-Cleanup-Bug) — der in 07c dokumentierte Blocker.
Zusätzlich ist das `onnx`-Subcommand in optimum 2.3.0 nicht registriert; und `torch.onnx`
(delegiert an den Dynamo-Exporter in torch 2.13) scheitert an transformers 4.57
`EncoderDecoderCache` (nicht pytree-registriert).

**Lösung (reproduzierbar committet):**
- Eigenes Export-Image `Containerfile.mrebel-export` mit **gepinntem Legacy-Stack**:
  `optimum==1.21.0` + `transformers==4.42.4` (optimum 1.21 braucht `<4.43`) + `torch 2.5.1`
  (pytorch/pytorch-Basis) + `onnx` + `onnxruntime`.
- `optimum-cli export onnx --model Babelscape/mrebel-large --task text2text-generation --no-post-process`
  → `encoder_model.onnx` + `decoder_model.onnx` (ohne present-Outputs, korrekt).
- `optimum-cli export onnx --model Babelscape/mrebel-large --task text2text-generation-with-past --no-post-process`
  → `decoder_with_past_model.onnx` (KV-Cache-Variante) — **Validierung: presente KV matcht
  Referenz (2,16,17,64), logits max-diff 4.6e-5**.
- `--no-post-process` überspringt nur den decoder_model_merged-Schritt (scheitert an
  serialize) und behält die Einzelgraphen.

**Artefakte auf dem Carrier (`~/models/mrebel_onnx/`):**
| Datei | Größe |
|---|---|
| `encoder_model.onnx` (+data) | 1.63 GB |
| `decoder_model.onnx` (+data) | 679 KB / 2.86 GB |
| `decoder_with_past_model.onnx` (+data) | 579 KB / 2.76 GB |

**Korrektheits-Beweis der Graphen (ORT vs torch, 3090):**
- `decoder_model.onnx`: argmax identisch bei Längen 1–4 (`<triplet>`,`Z`,`Z`,`part`), max|d| 1.6e-3 (L=1) bis 2.4e-1 (L=4, FP-Drift durch Fehlen des KV-Cache im Referenzvergleich).
- `decoder_with_past_model.onnx`: 2-Schritt-Loop (step1 + with_past) logits max|d| 2.8e-3 vs torch-Full-Forward — **der KV-Cache-Loop ist numerisch korrekt**.

**Export-Skripte committet:** `mrebel_export/export_decoder_onnx.sh` (Rezept),
`Containerfile.mrebel-export`, `mrebel_export/export_mrebel_decoder.py` (verworfener
manueller Trace — dokumentiert, warum: Dynamo-Exporter-Failure + korrupte Logits).

## 2. Ziel 2 — Go-Decoding-Loop (`cmd/feasibility/mrebelgo/`)

Struktur (eigenes go.mod, wie die anderen PoCs): `yalue/onnxruntime_go` + `tggo/goSentencePiece`.

- **Encoder:** `encoder_model.onnx` (input_ids, attention_mask → last_hidden_state).
  Go-Encoder-Output == torch-Encoder-Output: **cosine 1.0, max-abs-diff 0.0** (byte-identisch).
- **Decoder (Standard-Pfad):** `decoder_model.onnx` pro Schritt mit voller Re-Encodierung
  („volle Re-Encodierung pro Schritt"-Fallback aus der Aufgabe, §4.2) — jede Hypothese hat
  ihre wachsende Token-Sequenz, logits am letzten Token.
- **Beam-Search:** width 3, return 3, max_length 256, length_penalty 0, do_sample false,
  decoder_start `tp_XX` (250058). **BeamHypotheses-Semantik repliziert** (best-numBeams
  finished behalten, schlechtere verdrängen; Stopp via `worst_finished >= best_open`).
- **KV-Cache-Pfad (experimentell, `MRBEL_CACHE=1`):** `decoder_with_past_model.onnx` mit
  Cache-Threading (step1 present.encoder = konstante Encoder-KV; stepN re-emittiert nur
  Decoder-KV). Läuft, aber **kein Speed-Gewinn** im Harness (siehe §4) und FP-empfindlicher.
- **2×-Determinismus:** byte-gleich (sha256 `1934fcde…`), beide Läufe identisch.

## 3. Ziele 3+4 — Parser-Port + Parität (n=50)

**Parser** (`decode.go`): `_parse_mrebel_output`-Regex 1:1 nach Go, Typ-Mapping
(per→PERSON, org→ORGANIZATION, loc→LOCATION, media→WORK, sonst CONCEPT), Dedup
first-seen über die 3 Beams. **Unit-Test grün gegen 6 reale Python-Fixtures**
(`parser_fixtures.json`).

**Paritätsmessung (50 Chunks, sample_chunks.json, Carrier 3090, same-device):**
| Metrik | Ergebnis |
|---|---|
| **Chunk-Level-Triple-Set-Gleichheit** (Union über 3 Beams, dedupliziert) | **48/50 = 96.0 %** (Schwelle ≥95 % ✓) |
| String-Gleichheit (roh) | 0/50 — Python hängt `</s><pad>`-Padding an; Go liefert nackte Sequenzen |
| String-Gleichheit (normalisiert: `</s>`/`<pad>` gestrippt, Whitespace kollabiert) | **47/50 = 94 %** |
| Gesamt-Triples | Go 137 vs Python 136 |

**Divergenz-Charakterisierung (identisch zur Sparse-Outlier-Methodik):**
- Chunk 23: Go generiert eine Zusatz-Triple („constant mix subclass of musterportfolio") —
  Python hat sie nicht (Beam-FP-Ordung bei nahezu gleichen Scores).
- Chunk 48: Token-Boundary-Divergenz an seltenem Kompositum (Python spaltet
  „datenquel"/„daten" statt „datenquelle") — **der bekannte seltene-Nicht-Latin/
  Kompositum-Grenztoken-Fall**, der auch die Sparse-Outlier (Chunks 48/195) trieb.
- 1 weiterer String-only-Diff ohne Triple-Auswirkung.

**Kritische Stolpersteine (im Bericht + Code dokumentiert):**
1. **Byte- vs. Rune-Truncation:** Python `text[:1500]` schneidet Zeichen, Go `text[:1500]`
   Bytes → mit Umlauten 21 Zeichen Verlust → völlig anderes Encoder-Input → Müll-Generierung.
   Fix: `truncateRunes`.
2. **HF-Vocab-Offset:** Decoder-Output-ids sind HF-space = raw-sentencepiece + 1; Go-Decode
   muss `id-1` rechnen (sonst dekodiert id 130629 als Oriya-Schrift statt „Verantwortung").
3. **SPM-Run-Lead-Space:** `tggo` Decode strippt das führende `▁`-Leerzeichen eines Runs;
   nach Spezial-Tokens muss es re-addiert werden (HF-Spacing: `Köpfen <concept>`).
4. **Beam-Eviction:** naive „top-3 fertig"-Auswahl behält `[tp_XX, eos]` (Score −8.78);
   transformers' `BeamHypotheses` verdrängt es, sobald 3 echte Beams fertig sind (−3.67,
   −5.64, −6.13). Stopp-Kondition `worst_finished >= best_open` ist Pflicht.

## 4. Ziel 5 — Performance auf der 3090 (ehrlich)

| Pfad | p50 pro Chunk | p95 | mean |
|---|---|---|---|
| **Go no-cache Beam** (Standard) | **4.45 s** | 9.53 s | 5.10 s |
| Go cached (with_past, `MRBEL_CACHE=1`) | 4.38 s | 8.76 s | 5.63 s |
| **Python-Orakel** (torch generate, mit KV-Cache) | **0.14 s** | 0.31 s | 0.16 s |

Erkenntnisse:
- Der **no-cache-Pfad** ist ~30× langsamer als Python (O(L²)-Re-Encodierung: ~136
  Decoder-Forwards pro Beam vs. ~16 KV-Cache-Schritte).
- Der **KV-Cache-Pfad in Go wird NICHT schneller** — die per-call-Kosten dominieren:
  (a) onnxruntime_go legt pro Schritt ~50 Input-/25 Output-Tensoren über cgo an;
  (b) die with_past-Graphen haben dynamische Shapes → ORT re-plannt pro Schritt.
  Beides zusammen frisst den algorithmischen Gewinn. Ein Produktions-Runner müsste
  Tensoren wiederverwenden + Shape-gecachte Sessions nutzen, um an Python heranzukommen.
- Der KV-Cache-Pfad ist zudem FP-empfindlicher (86 % vs 96 % — Score-Akkumulation über
  Cache-Konkatenation vs. Recompute; gleiche Art von cuDNN-Streuung wie bei GLiNER).

**Messwert fürs Epic:** Go-mREBEL ist *funktional* paritätisch (96 % Triple-Set), aber die
Latenz ist der Preis des no-cache-Pfads; die Cache-Pfad-Optimierung (Tensor-Reuse,
Shape-Caching) ist der benannte Epic-Kostenpunkt. Für die Query-Pfade (dense/rerank) ist
das irrelevant — die sind bereits CUDA-paritätisch und schnell.

## 5. Definition of Done — Status

- [x] Decoder-ONNX-Dateien auf dem Carrier (Größen oben; Export-Skript committet, reproduzierbar)
- [x] `mrebelgo` baut (eigenes go.mod), läuft 2× byte-gleich
- [x] Parser-Unit-Test grün gegen 6 committete Python-Fixtures
- [x] Paritätsmessung n=50: **Triple-Set 96.0 % ≥ 95 %** (Zahl + Artefakt committed), String gemessen und berichtet
- [x] Latenzen p50/p95 vs Python auf derselben 3090 im Bericht
- [x] `11-mrebel-decoder-go.md` existiert; Entscheidungsdok-Zeile aktualisiert
- [x] Keine Modell-Binaries committet; Test-DB nicht verändert; kein Merge

**Artefakte:** `carrier_results/go_mrebel_parity.json` (+r2), `go_mrebel_cache.json`,
`mrebel_ref_50.json` (Python-Orakel), `cache_timings2.txt`, `go_timings_r2.txt`,
`parser_fixtures.json`.

**Reproduzierbare Befehle (Carrier):**
```
# Export (einmalig, Image ist committet):
podman run --rm -e HF_HOME=/hf -v ~/.cache/huggingface:/hf:rw -v ~/models:/models:rw \
  -v $PWD/axiom_ng/cmd/feasibility/mrebel_export/export_decoder_onnx.sh:/run.sh:ro \
  localhost/study-mrebel-export:latest bash /run.sh

# Go-Parität (50 Chunks):
podman run --rm --device nvidia.com/gpu=all -e CUDA_VISIBLE_DEVICES=0 -e ORT_CUDA=1 -e ORT_CUDA_DEVICE=0 \
  -v ~/models:/models:ro localhost/study-mrebel:latest bash -c \
  "cd /study/cmd/feasibility/mrebelgo && go build -o /tmp/mg . && /tmp/mg \
   /opt/onnxruntime/lib/libonnxruntime.so.1.29.0 /models/mrebel_onnx /models/sample_chunks.json \
   /study/cmd/feasibility/mrebelgo/parity_idxs.json /tmp/go_mrebel_parity.json"
```

**Entscheidungsdok-Update:** `go-runner-feasibility.md` — mREBEL-Zeile von
„Sidecar (Option 1)" auf „**Go-nativ möglich** (Decoder-ONNX exportiert + validiert;
Go-Beam 96 % Triple-Set-Parität; Parser 1:1; Latenz 4.45 s vs 0.14 s Python — Cache-
Optimierung = Epic-Punkt)"; Fazit-Zeile entsprechend angepasst.
