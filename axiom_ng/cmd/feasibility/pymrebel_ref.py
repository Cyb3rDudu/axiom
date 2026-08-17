#!/usr/bin/env python3
"""
Restpunkt 6 — Python mREBEL reference (Orakel) for Restpunkt 6 parity.
Mirrors axiom_ng_runner relation_extractor.extract_relations_from_chunks EXACTLY
(num_beams=3, num_return_sequences=3, max_length=256, length_penalty=0,
decoder_start_token_id=tp_XX), then dumps per chunk:
  - the 3 raw decoded return sequences
  - the parsed triples (dedup across beams, first-seen order)
so Go (mrebelgo) can be compared on triple-set equality AND raw-string equality.

Usage: pymrebel_ref.py <chunks.json> <out.json> [max_chunks]
Runs on the study GPU container (torch+transformers matching the runner 4.57.6).
"""
import json
import re
import sys

import torch
from transformers import AutoModelForSeq2SeqLM, AutoTokenizer

_MREBEL_TYPE_MAP = {
    "per": "PERSON", "org": "ORGANIZATION", "loc": "LOCATION",
    "concept": "CONCEPT", "media": "WORK", "event": "CONCEPT", "misc": "CONCEPT",
}


def parse_mrebel_output(decoded):
    triples = []
    decoded = decoded.replace("</s>", "").replace("<pad>", "")
    for m in re.finditer(r'<triplet>\s*(.+?)\s*<(\w+)>\s*(.+?)\s*<(\w+)>\s*([^<]+)', decoded):
        head, head_type, tail, tail_type, relation = m.groups()
        head = head.strip(); tail = tail.strip(); relation = relation.strip()
        if head and tail and relation and len(head) >= 2 and len(tail) >= 2:
            triples.append({"head": head,
                            "head_type": _MREBEL_TYPE_MAP.get(head_type.lower(), "CONCEPT"),
                            "tail": tail,
                            "tail_type": _MREBEL_TYPE_MAP.get(tail_type.lower(), "CONCEPT"),
                            "relation": relation})
    return triples


def main():
    chunks_path, out_path = sys.argv[1], sys.argv[2]
    maxc = int(sys.argv[3]) if len(sys.argv) > 3 else 10**9
    dev = "cuda" if torch.cuda.is_available() else "cpu"
    print(f"device={dev}", flush=True)
    model = AutoModelForSeq2SeqLM.from_pretrained("Babelscape/mrebel-large").to(dev).eval()
    tok = AutoTokenizer.from_pretrained("Babelscape/mrebel-large")
    tp_id = tok.convert_tokens_to_ids("tp_XX")
    chunks = json.load(open(chunks_path))

    results = []
    processed = 0
    for ci, chunk in enumerate(chunks):
        text = chunk.get("text", "")
        if not text or len(text.strip()) < 50:
            results.append({"idx": ci, "skipped_reason": "short"})
            continue
        if processed >= maxc:
            break
        input_text = text[:1500]
        input_ids = tok(input_text, max_length=512, padding=True, truncation=True, return_tensors="pt").to(dev)
        with torch.no_grad():
            tokens = model.generate(
                **input_ids,
                max_length=256,
                length_penalty=0,
                num_beams=3,
                num_return_sequences=3,
                decoder_start_token_id=tp_id,
            )
        seqs_raw = [tok.decode(s, skip_special_tokens=False) for s in tokens]
        # triple set across the 3 beams, dedup first-seen (like the runner)
        seen = set(); all_triples = []
        per_seq = []
        for seqstr in seqs_raw:
            triples = parse_mrebel_output(seqstr)
            per_seq.append(triples)
            for t in triples:
                key = (t["head"].lower(), t["relation"], t["tail"].lower())
                if key not in seen:
                    seen.add(key); all_triples.append(t)
        results.append({
            "idx": ci,
            "raw_sequences": seqs_raw,
            "parsed": [per_seq],
            "triples": all_triples,
        })
        processed += 1
        if (processed % 10 == 0):
            print(f"  ... {processed} chunks done", flush=True)
    json.dump(results, open(out_path, "w"), ensure_ascii=False, indent=1)
    print(f"wrote {processed} chunk results -> {out_path}")


if __name__ == "__main__":
    main()
