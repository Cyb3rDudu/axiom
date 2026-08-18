import numpy as np, torch, onnxruntime as ort
from transformers import AutoModelForSeq2SeqLM, AutoTokenizer
import sys as _sys, os as _os; _sys.path.insert(0, _os.path.dirname(_os.path.dirname(_os.path.abspath(__file__))))
from _device import pick_device as _pd
dev = _pd("auto", no_fp16=True, label="mrebel-export")[0]
m=AutoModelForSeq2SeqLM.from_pretrained("Babelscape/mrebel-large").to(dev).eval()
tok=AutoTokenizer.from_pretrained("Babelscape/mrebel-large")
enc=tok(["Teilung."],return_tensors="pt").to(dev)
with torch.no_grad():
    eh=m.model.encoder(input_ids=enc["input_ids"],attention_mask=enc["attention_mask"])[0]
eh=eh.detach().cpu().numpy(); em=enc["attention_mask"].detach().cpu().numpy()
dm=ort.InferenceSession("/models/mrebel_onnx/decoder_model.onnx",providers=["CUDAExecutionProvider","CPUExecutionProvider"])
wp=ort.InferenceSession("/models/mrebel_onnx/decoder_with_past_model.onnx",providers=["CUDAExecutionProvider","CPUExecutionProvider"])
tp=tok.convert_tokens_to_ids("tp_XX")
r1=dm.run(None,{"input_ids":np.array([[tp]],dtype=np.int64),
                "encoder_hidden_states":eh,"encoder_attention_mask":em})
r1d={o.name:v for o,v in zip(dm.get_outputs(),r1)}
nid=int(np.argmax(r1d["logits"][0,0]))
print("step1 argmax:", repr(tok.decode(nid)))
win=[i.name for i in wp.get_inputs()]
dec_names=[n for n in win if n.startswith("past_key_values") and ".decoder.key" in n or (n.startswith("past_key_values") and ".decoder.value" in n)]
enc_names=[n for n in win if n.startswith("past_key_values") and ".encoder." in n]
# present name -> tensor; input name -> present name (present.{L}.decoder.key == past_key_values.{L}.decoder.key)
f={"input_ids":np.array([[nid]],dtype=np.int64)}
for nm in [n for n in win if n.startswith("past_key_values")]:
    target_key=nm.replace("past_key_values","present")
    f[nm]=r1d[target_key]
f["encoder_attention_mask"]=em
r2=wp.run(None,f); r2d={o.name:v for o,v in zip(wp.get_outputs(),r2)}
ids2=torch.tensor([[tp,nid]],dtype=torch.long,device=dev)
with torch.no_grad():
    ref=m.model.decoder(input_ids=ids2,encoder_hidden_states=torch.from_numpy(eh).cuda(),encoder_attention_mask=torch.from_numpy(em).cuda())
    rl=m.lm_head(ref[0]).detach().cpu().numpy()
print("step1 ORT vs torch ref[0,0] max|d|:", float(np.abs(r1d["logits"][0,0]-rl[0,0]).max()))
print("step2 ORT(cached) vs torch full-2-token ref[0,1] max|d|:", float(np.abs(r2d["logits"][0,0]-rl[0,1]).max()))
