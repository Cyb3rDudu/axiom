import json, numpy as np, torch
from transformers import AutoModelForSeq2SeqLM
d=json.load(open('/go_enc.json'))
ids=np.array(d['enc_ids'],dtype=np.int64)
go_hidden=np.array(d['enc_hidden'],dtype=np.float32)
print("go enc_len",len(ids),"hidden shape",go_hidden.shape)
dev="cuda"
m=AutoModelForSeq2SeqLM.from_pretrained("Babelscape/mrebel-large").to(dev).eval()
with torch.no_grad():
    o=m.model.encoder(input_ids=torch.tensor([ids],device=dev),attention_mask=torch.ones(1,len(ids),device=dev))
torch_hidden=o[0].detach().cpu().numpy()[0].astype(np.float32)
print("torch hidden shape",torch_hidden.shape)
a=go_hidden.reshape(-1); b=torch_hidden.reshape(-1)
cos=float(np.dot(a,b)/(np.linalg.norm(a)*np.linalg.norm(b)+1e-9))
print("Go-enc vs torch-enc cosine:", round(cos,6))
print("max abs diff:", round(float(np.abs(a-b).max()),5))
print("go_hidden[0,:3]", go_hidden[0,:3])
print("torch[0,:3]", torch_hidden[0,:3])
