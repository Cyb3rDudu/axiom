import json

import numpy as np
import onnxruntime as ort
import torch
from transformers import AutoModelForSeq2SeqLM, AutoTokenizer

chunks=json.load(open('/models/sample_chunks.json'))
text=chunks[0]["text"][:1500]
import sys as _sys, os as _os; _sys.path.insert(0, _os.path.dirname(_os.path.dirname(_os.path.abspath(__file__))))
from _device import pick_device as _pd

dev = _pd("auto", no_fp16=True, label="mrebel-export")[0]
tok=AutoTokenizer.from_pretrained("Babelscape/mrebel-large")

tp=tok.convert_tokens_to_ids("tp_XX")
m=AutoModelForSeq2SeqLM.from_pretrained("Babelscape/mrebel-large").to(dev).eval()
enc=tok(text,max_length=512,padding=True,truncation=True,return_tensors="pt").to(dev)
print("enc len", enc["input_ids"].shape[1])
with torch.no_grad():
    eh=m.model.encoder(input_ids=enc["input_ids"],attention_mask=enc["attention_mask"])[0]
eh_np=eh.detach().cpu().numpy(); em_np=enc["attention_mask"].detach().cpu().numpy()
# Python ORT greedy using decoder_model.onnx (no-cache), like Go
from _device import ort_providers as _op

dm=ort.InferenceSession("/models/mrebel_onnx/decoder_model.onnx",providers=_op(dev))
ids=[tp]
for step in range(5):
    out=dm.run(["logits"],{"encoder_attention_mask":em_np,"input_ids":np.array([ids],dtype=np.int64),"encoder_hidden_states":eh_np})
    logits=out[0][0,-1]  # last position
    nid=int(np.argmax(logits))
    ids.append(nid)
print("Python-ORT greedy ids:", ids)
print("Python-ORT decode:", repr(tok.decode(ids,skip_special_tokens=False)))
# torch greedy reference
with torch.no_grad():
    tg=m.generate(**enc,max_length=10,length_penalty=0,num_beams=1,num_return_sequences=1,decoder_start_token_id=tp,do_sample=False)
print("torch greedy:", repr(tok.decode(tg[0][:10],skip_special_tokens=False)))
