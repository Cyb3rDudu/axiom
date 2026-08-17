#!/bin/bash
# Restpunkt 6 / Ziel 1 — mREBEL decoder ONNX export (REPRODUCIBLE recipe).
#
# Root cause fixed: optimum 2.3.0 (current) fails `export onnx` for the BART decoder
# (FileNotFoundError on decoder_model.onnx.data cleanup). The stable path is the
# PINNED legacy stack optimum 1.21.0 + transformers 4.42.4 + torch 2.5.1, run
# inside the committed Containerfile.mrebel-export image. Add --no-post-process so
# the individual decoder_with_past_model.onnx (KV-cache variant) is kept instead of
# trying to merge it into decoder_model_merged.onnx (which fails on serialize).
#
# Produces (in $OUT): encoder_model.onnx, decoder_model.onnx, decoder_with_past_model.onnx
#
# Usage (carrier):
#   scp this to ~ ; podman run --rm -e HF_HOME=/hf -v ~/.cache/huggingface:/hf:rw \
#     -v ~/models:/models:rw -v /tmp/run_mrebel_export.sh:/run.sh:ro \
#     localhost/study-mrebel-export:latest bash /run.sh
set -u
OUT="${OUT:-/models/mrebel_onnx}"
mkdir -p "$OUT"
echo "=== versions ==="
python -c "import optimum,transformers,torch; from importlib.metadata import version; print('optimum',version('optimum'),'transformers',version('transformers'),'torch',torch.__version__)"

echo "[1/3] encoder + decoder (text2text-generation)"
optimum-cli export onnx --model Babelscape/mrebel-large \
  --task text2text-generation --no-post-process "$OUT" > /tmp/export1.log 2>&1
echo "encoder+decoder EXIT=$?"; tail -2 /tmp/export1.log

echo "[2/3] decoder_with_past (text2text-generation-with-past)"
optimum-cli export onnx --model Babelscape/mrebel-large \
  --task text2text-generation-with-past --no-post-process /tmp/mrebel_wp > /tmp/export2.log 2>&1
echo "with-past EXIT=$?"; tail -2 /tmp/export2.log

echo "[3/3] copy with_past decoder into the base export dir"
cp -f /tmp/mrebel_wp/decoder_with_past_model.onnx* "$OUT/" 2>/dev/null && echo "copy ok"

echo "=== final files ==="
ls -la "$OUT" | grep -E "\.onnx"
