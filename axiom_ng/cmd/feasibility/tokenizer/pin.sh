#!/bin/bash
# Tokenizer-pin (#171, Block 2): assert Go (tggo/goSentencePiece + HF reindex)
# token IDs == Python XLMRobertaTokenizerFast on a German sample incl. umlauts
# + NFC/NFD. Exits 0 only if every sample's Go IDs match Python exactly.
set -u
SP=/Users/dudu/.cache/huggingface/hub/models--BAAI--bge-m3/snapshots/5617a9f61b028005a4858fdac845db406aefb181/sentencepiece.bpe.model
DIR="$(cd "$(dirname "$0")" && pwd)"
TOK="$DIR/build/tokpin"
PY=/Users/dudu/Code/axiom/axiom_ng_runner/.venv/bin/python
[ -x "$TOK" ] || (cd "$DIR" && go build -o build/tokpin ./tokpin)
[ -f "$SP" ] || { echo "SP model not found"; exit 2; }

SAMPLES="$(mktemp)"
cat > "$SAMPLES" <<'EOF'
Öffentlichkeit Straße
Marktanteile des Unternehmens
Gänsefüßchen Hügelstraße Bäume
Zuege Äpfel Oeffnung große Haeuser
MASSNAHME Verordnung Fuehrungskraefte
Straße grossmasstabliche Karte pruefung
Testfälle mit Schrägstrich und Klammern (2026)
EOF
$PY - > /tmp/nfdpairs.txt <<'PYEOF'
import unicodedata
for s in ["Öffentlichkeit Straße","Gänsefüßchen","große Maßnahme","Äpfel übrigens Straße"]:
    print(s); print(unicodedata.normalize("NFD", s))
PYEOF
cat /tmp/nfdpairs.txt >> "$SAMPLES"

fail=0; n=0
while IFS= read -r -u 3 line; do
    [ -z "$line" ] && continue
    n=$((n+1))
    go_ids=$("$TOK" "$SP" "$line" | sed -n 's/^ids=\[\(.*\)\]/\1/p')
    py_ids=$($PY - "$line" <<'PYEOF' 2>/dev/null
import sys
from transformers import AutoTokenizer
t=AutoTokenizer.from_pretrained('BAAI/bge-m3')
print(",".join(str(i) for i in t.encode(sys.argv[1], add_special_tokens=True)))
PYEOF
)
    if [ -z "$py_ids" ]; then echo "PYERROR #$n: $line"; fail=1; continue; fi
    go_norm=$(echo "$go_ids" | tr -cd '0-9')
    py_norm=$(echo "$py_ids" | tr -cd '0-9')
    if [ "$go_norm" = "$py_norm" ]; then
        echo "OK   #$n <- $line (ids ["$go_norm"])"
    else
        echo "FAIL #$n <- $line"; echo "  go [$go_ids]"; echo "  py [$py_ids]"; fail=1
    fi
done 3< "$SAMPLES"
rm -f "$SAMPLES"

echo "---"
echo "PIN RESULT: $([ $fail -eq 0 ] && echo PASS || echo FAIL) over $n samples"
exit $fail
