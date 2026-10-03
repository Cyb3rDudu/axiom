# ortprobe — onnxruntime_go smoke test (#171)

Proves `github.com/yalue/onnxruntime_go` can load a shared ORT dylib on macOS
arm64 and run the existing BGE-M3 `model.onnx`, producing the 1024-dim
`sentence_embedding` output (L2-norm 1.0 — BGE-M3 normalizes).

**No homebrew:** ORT dylib is a downloaded official release archive, not a brew
install. `yalue/onnxruntime_go` v1.33 targets ORT 1.29 (C API version 29).

- Dylib (local, /tmp): `onnxruntime-osx-arm64-1.29.0/lib/libonnxruntime.1.29.0.dylib`
  from `https://github.com/microsoft/onnxruntime/releases/download/v1.29.0/onnxruntime-osx-arm64-1.29.0.tgz`
- Model: HF-cache BGE-M3 `onnx/model.onnx` (inputs `input_ids`+`attention_mask`,
  outputs `token_embeddings[1,seq,1024]` + `sentence_embedding[1,1024]`).

Verify:
```
go run . <libonnxruntime.dylib> <model.onnx> 0 34618 2733 1340 224 70948 2
```
Expected: token_embeddings [1 7 1024], sentence_embedding dim=1024, L2 norm 1.0.
The toy ids `0 34618 2733 1340 224 70948 2` are from the Python reference for
"Marktanteile des Unternehmens" (Block 2 sample).
