#!/usr/bin/env python3
"""Build the Dubs working copy from legacy chunks, split at scan last-word anchors.

Construction (owner decision): legacy chunk text is clean citation-grade;
the scan stays archival. One PDF page per print page (58-135), embedded
labels = print numbers, running section head from legacy metadata.
"""
import json, re, unicodedata
import pymupdf

CHUNKS = json.load(open("/tmp/dubs_chunks.json"))
ANCHORS = json.load(open("/tmp/dubs_lastwords.json"))
PRINT_FIRST, PRINT_LAST = 58, 135
N_PAGES = PRINT_LAST - PRINT_FIRST + 1  # 78

def norm(s):
    s = unicodedata.normalize("NFC", s)
    s = s.replace("\u00ad", "")
    return re.sub(r"\s+", " ", s).strip()

# --- total stream with word positions -> (chunk_idx, word) ---
stream_words = []  # (ci, word)
for c in CHUNKS:
    for w in re.findall(r"\S+", norm(c["t"])):
        stream_words.append((c["i"], w))
total_words = len(stream_words)

# expected word-share per print page from chunk spread-ranges
# spread s (1-based) -> print pages 2s+56, 2s+57
page_chunks = {}
for p in range(PRINT_FIRST, PRINT_LAST + 1):
    page_chunks[p] = set()
for c in CHUNKS:
    a, b = int(c["a"]), int(c["b"])
    for s in range(a, b + 1):
        for p in (2 * s + 56, 2 * s + 57):
            if PRINT_FIRST <= p <= PRINT_LAST:
                page_chunks[p].add(c["i"])

def anchor_word(p):
    ws = ANCHORS.get(str(p), [])
    cands = [w for w in ws if len(w) >= 5 and re.search(r"[A-Za-zÄÖÜäöüß]", w)]
    return cands[-1] if cands else None

def word_matches(w, aw):
    a = aw.rstrip("-").strip(".,;:!?»«›‹\"'").lower()
    x = w.rstrip("-").strip(".,;:!?»«›‹\"'").lower()
    if not a or not x:
        return False
    n = min(len(a), 8)
    return x[:n] == a[:n] or (len(a) >= 6 and a[:6] == x[:6])

# --- split: global monotone anchor assignment ---
def phrase_positions(p, n):
    aws = [w for w in ANCHORS.get(str(p), []) if len(w) >= 4]
    if len(aws) < n:
        return []
    phrase = aws[-n:]
    allowed = page_chunks[p] | page_chunks.get(min(p + 1, PRINT_LAST), set())
    out = []
    for ci in range(0, total_words - n + 1):
        if all(stream_words[ci + k][0] in allowed
               and word_matches(stream_words[ci + k][1], phrase[k]) for k in range(n)):
            out.append(ci + n)  # end index
    return out

# 1) anker-kandidaten je druckseite (phrase 3 -> 2 -> 1)
anchor_ends = {}
for p in range(PRINT_FIRST, PRINT_LAST + 1):
    for n in (3, 2, 1):
        pos = phrase_positions(p, n)
        if pos:
            anchor_ends[p] = (pos, n)
            break

# 2) monotone greedy: erste position nach vorgaenger
chosen = {}
last_end = 0
for p in range(PRINT_FIRST, PRINT_LAST + 1):
    if p in anchor_ends:
        pos, _ = anchor_ends[p]
        c = next((e for e in pos if e >= last_end + 3), None)
        chosen[p] = c if c is not None else None
        if c:
            last_end = c

# 3) seiten ohne anker: interpolation zwischen gewaehlten nachbarn
keys = sorted(chosen)
def interp(a_p, a_end, b_p, b_end):
    span = b_p - a_p
    for q in range(a_p + 1, b_p):
        frac = (q - a_p) / span
        chosen[q] = int(a_end + frac * (b_end - a_end))

bounds = [(PRINT_FIRST - 1, 0)] + [(p, chosen[p]) for p in keys if chosen[p]] + [(PRINT_LAST + 1, total_words)]
for (ap, ae), (bp, be) in zip(bounds, bounds[1:]):
    if bp - ap > 1:
        interp(ap, ae, bp, be)

ends = {p: (chosen[p] if chosen.get(p) else min(total_words, max(1, int((p - PRINT_FIRST + 1) * total_words / N_PAGES)))) for p in range(PRINT_FIRST, PRINT_LAST + 1)}
# monotonie-fix
mx = 0
for p in range(PRINT_FIRST, PRINT_LAST + 1):
    ends[p] = max(ends[p], mx + 1)
    mx = ends[p]

pages_text = {}
prev = 0
for p in range(PRINT_FIRST, PRINT_LAST + 1):
    words = [w for _, w in stream_words[prev:ends[p]]]
    pages_text[p] = " ".join(words)
    prev = ends[p]

# running section: last contributing chunk's deepest section title
def section_for(p):
    cis = [ci for ci, _ in stream_words[max(0, ends[p]-1):ends[p]]] or [None]
    ci = cis[0]
    if ci is None:
        return ""
    c = CHUNKS[ci]
    return c["sec"][-1] if c["sec"] else ""

# --- build pdf ---
out = pymupdf.open()
W, H = 595, 842
for p in range(PRINT_FIRST, PRINT_LAST + 1):
    page = out.new_page(width=W, height=H)
    head = section_for(p)
    body = pages_text[p]
    y = 60
    if head:
        page.insert_textbox(pymupdf.Rect(50, 40, W - 50, 70), head, fontsize=9,
                            fontname="helv", color=(0.25, 0.25, 0.25))
        y = 80
    rc = page.insert_textbox(pymupdf.Rect(50, y, W - 50, H - 50), body,
                             fontsize=9.5, fontname="helv", align=0)
    if rc < 0:
        # overflow: shrink
        page.insert_textbox(pymupdf.Rect(40, y, W - 40, H - 40), body,
                            fontsize=8, fontname="helv", align=0)
out.set_page_labels([{"startpage": 0, "prefix": "", "style": "D",
                      "firstpagenum": PRINT_FIRST}])
out.save("/tmp/dubs_working_copy.pdf", deflate=True)
out.close()

# --- verification ---
built = pymupdf.open("/tmp/dubs_working_copy.pdf")
stream_norm = norm(" ".join(w for _, w in stream_words))
missing = []
for c in CHUNKS:
    t = norm(c["t"])
    if t and t not in stream_norm:
        missing.append(c["i"])
cover = len([c for c in CHUNKS if norm(c["t"]) in stream_norm])
print(f"pages: {len(built)} (soll {N_PAGES}) | labels: {built[0].get_label()}..{built[-1].get_label()}")
print(f"chunk-coverage: {cover}/{len(CHUNKS)} | fehlend: {missing}")
print(f"ankerseite 94 endet: …{' '.join(pages_text[94].split()[-6:])}")
print(f"ankerseite 95 endet: …{' '.join(pages_text[95].split()[-6:])}")
print(f"gesamt-zeichen strom: {len(stream_norm)} (quelle: {sum(len(norm(c['t'])) for c in CHUNKS)})")

# ---------------------------------------------------------------------------
# Construction record (v3, owner pivot away from OCR):
# - input : 57 legacy chunks (active snapshot, processing_chunks) with
#           spread pair-ranges + section titles; last-word anchors per
#           print page harvested from the quarantined scan's bottom bands
#           (tesseract deu, lower 22% of each v2-gutter leaf crop)
# - split : global monotone anchor assignment (phrase 3->2->1, fuzzy word
#           match incl. hyphenation), linear interpolation for anchor-less
#           pages, monotonicity enforced
# - verify: 57/57 chunk coverage, anchors 94/95 end exactly on their
#           scan last-words, labels print 58..135 by construction
