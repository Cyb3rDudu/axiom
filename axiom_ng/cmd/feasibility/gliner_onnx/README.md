# GLiNER-ONNX zero-shot parity test (#171, Block 7)

`urchade/gliner_multi-v2.1`, gliner 0.2.28 (runner-pinned). Proves that the
zero-shot NER used by the runner can move to ONNX with exact parity.

```python
m = GLiNER.from_pretrained('urchade/gliner_multi-v2.1')
m.export_to_onnx('OUT')                          # 2.3 s -> model.onnx + tokenizer files
m_onnx = GLiNER.from_pretrained('OUT', load_onnx_model=True)
# predict_entities(...) → entity set identical, max |score diff| 1e-05 (German sample)
```

Verified with the runner's own labels
[person,organization,location,concept,"book or journal","research method"] on a
German sample: PyTorch vs ONNX entity set identical, max |score diff| = 1e-05.
See `../../docs/research/07-gliner-mrebel.md`.
