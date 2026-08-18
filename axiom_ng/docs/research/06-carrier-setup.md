# Carrier-Setup — CUDA measurement container (#171 Nachzug)

Reproducible carrier setup for the CUDA parity re-runs. All ML runs happen on
the carrier (192.168.1.2, RTX 3090s), NOT locally — the local GPU is reserved for other work.

## Host survey (2026-08-17)

- `ssh dudu@192.168.1.2`, NixOS `carrier`, x86_64, `nvidia-smi 595.91.07`, CUDA 13.2.
- **3 GPUs**: GPU 0 = RTX 3090 (24 GiB), GPU 1 = RTX 3090 (24 GiB), GPU 2 = RTX
  A3000 Laptop (12 GiB).
- **No GPU processes running** at survey time (no prod runner, no llama-swap
  active). Both 3090s free → **no contention → pin GPU 0** (`CUDA_VISIBLE_DEVICES=0`).
- Rootless Podman 5.8.4, CDI spec `/var/run/cdi/nvidia-container-toolkit.json`,
  GPU via `--device nvidia.com/gpu=all`.
- Ignored: prod containers `runner-carrier*` (Exited), `llama-swap` image
  (present, not running). **Prod runner untouched**.

## Study image `localhost/study-axiom-cuda:latest`

`Containerfile.study` — extends `localhost/runner-poc:latest` (the POC basis with
the runner-pinned Python torch stack: torch 2.13.0, FlagEmbedding 1.4.0,
gliner 0.2.28, transformers 4.57.6) and adds:
- Go toolchain **1.26.5 linux-amd64** (matches the Mac, keeps PoC builds/
  hashes consistent).
- **onnxruntime `gpu_cuda13` 1.29.0** shared lib + `libonnxruntime_providers_cuda.so`
  (API 29 for `yalue/onnxruntime_go` v1.33). Note: the CPU-only
  `onnxruntime-linux-x64-1.29.0.tgz` has NO CUDA-EP — must use
  `onnxruntime-linux-x64-gpu_cuda13-1.29.0.tgz`, and set
  `LD_LIBRARY_PATH` to the torch-bundled CUDA 13 runtime
  (`/usr/local/lib/python3.11/site-packages/nvidia/cu13/lib`).
- `scipy` (Spearman/Kendall reference).
- Copies `axiom_ng/cmd/` + `axiom_ng/docs/research/` (feasibility tooling only).
- Builds the Go PoCs (godense, gorerank, gosparse, minirunner, ortprobe,
  tokdump, gogliner, cuda_ep_check) into `/study/bin/`.

## Verification (in-container, GPU 0)

```
$ CUDA_VISIBLE_DEVICES=0 podman run --rm --device nvidia.com/gpu=all \
    -e CUDA_VISIBLE_DEVICES=0 localhost/study-axiom-cuda:latest bash -c \
    'python -c "import torch;print(torch.cuda.is_available(),torch.cuda.get_device_name(0))"'
True NVIDIA GeForce RTX 3090
```
```
$ cd /study/cmd/feasibility/cuda_ep_check && go run .
CUDA EP appended OK        # onnxruntime_go CUDA execution provider initializes
```

Container name prefix: `study-...` (here image `study-axiom-cuda`). Runs on GPU 0
via CDI; the two 3090s are free so no prod interference.

## Reproducibility for independent verification

- Containerfile committed: `axiom_ng/cmd/feasibility/Containerfile.study` (build
  context = the repo root, since it COPYs `axiom_ng/cmd/` + `axiom_ng/docs/`):
  `podman build -t localhost/study-axiom-cuda -f axiom_ng/cmd/feasibility/Containerfile.study .`
- CUDA EP smoke test: `axiom_ng/cmd/feasibility/cuda_ep_check/` (own Go module).
- On carrier: clone at `~/Code/axiom-study` tracking the study branch
  `research/go-runner-feasibility` (kept at the evidence-head during the
  verification; `git log -1` there = the commit these docs ship in).
- **Not in the repo:** the base image `localhost/runner-poc:latest` and the
  models (`~/models/` on the carrier: bge-m3, reranker, sparse_head.onnx,
  gliner ONNX, tokenizers, sample/pair corpora). Same-device reconstruction
  from the tree alone is therefore partial — the carrier copy is the full
  environment while it stays up.
- Every parity / R7 run uses this container on the same GPU → clean
  same-device Go-vs-Python parity.
