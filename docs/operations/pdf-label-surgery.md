# PDF-Label-Surgery Runbook — `scripts/pdf_label_surgery.py` (#176)

**Werkzeug:** deterministische In-place-Heilung kaputter `/PageLabels`-Bäume in
Zotero-Storage-PDFs. Messwerkzeug (Auge): `scripts/integrity_probe.py` — die
Dreifachmessung Annotation-Label ≡ Chunk-Seite ≡ PDF-Label ist die einzige
Wahrheitsquelle. Chirurgie (Hand): dieses Tool. **Kein LLM, kein Upload, kein
Attachment-Tausch** (die alte Bauart ist verworfen, siehe #176).

```bash
axiom_ng_runner/.venv/bin/python scripts/pdf_label_surgery.py <KEY>           # Dry-Run (Default)
axiom_ng_runner/.venv/bin/python scripts/pdf_label_surgery.py <KEY> --apply   # einziger Schreibpfad
```

## Wann einsetzen

- Der Sweep (`pdf_analyze.py sweep`) oder die Probe zeigt **ABWEICHUNG**:
  Label weicht von der Chunk-Seite ab (Versatz, reprint-start, zwei Bereiche,
  C1-Block, fehlender Baum) — **und die Chunk-Wahrheit stimmt** (das ist der
  Normalfall: der RAG hatte bei der Label-Klasse schon recht, deshalb kann man
  an der Quelle heilen).
- **Niemals** bei 🟢 gesund oder MATCH — das Tool verweigert dann selbst
  (kein Eingriff indiziert).

## Disziplin (vom Werkzeug erzwungen)

1. **Dry-Run default** — Bericht mit Klasse, Parametern, geplanten Ranges,
   Ziel-Hash (Simulation auf Temp-Kopie) und dem Beweis, dass nichts
   angefasst wurde (fs + DB Fingerprint vor/nach).
2. **`--apply`** = backup (`/tmp/axiom_runs/backups/pdf_labels/<KEY>.pdf`)
   → schreiben → read-back (jede Seite == Erwartung) → **Anker-Wahrheits-Gate**
   (jedes gemessene M == Label, sonst Auto-Rollback + Abbruch) →
   **hash-sync** (`zotero_attachments`: sha256/file_size/mtime_ms) →
   Probe erneut → Dreifach-GRÜN im Bericht.
3. **unclassifizierbar = verweigern** (exit 3), nie raten — stille Falsheit
   ist der einzige verbotene Zustand.
4. **Stil-Politik**: vorhandene Stile bleiben (PRESERVE); römisch wird aus
   der Messung übernommen (ADOPT, inkl. Groß-/Kleinschreibung); Stil-Vakuum
   → römischer Vorspann als markierter offener Entscheidungspunkt, auflösbar
   mit `--style-override roman|arabic|prefix:C`.

## RAG/Sync — das Wichtigste

**Label-Heilung braucht NIEMALS einen Rechunk.** Der Hash-Sync verhindert den
ungewollten Re-Harvest: der nächste Sync sieht content_hash/mtime passend zur
geheilten Datei und tut nichts. Die Chunks (mit ihrer bereits richtigen
Chunk-Seiten-Wahrheit) bleiben in Kraft — Heilung und Korpus fallen dabei in
eins, weil der RAG bei der Label-Klasse schon richtig lag.

**Sync wird nur noch bewusst ausgelöst** — nämlich wenn sich der *Inhalt*
eines PDFs ändert (neue Auflage, OCR-Rebuild, echte Korrektur). Dann
gewollt: Sync läuft, Hash-Delta erzeugt den Rechunk-Job, die neuen Labels
landen im nächsten Snapshot.

## Grenzfälle

- **Variabel/chaotisch** → REFUSE mit Grund im Bericht (meldungspflichtig,
  dudu entscheidet von Hand — die 31-Bücher-Kampagne zeigt die Formen).
- **Folio-Lücke zwischen letzter römischer Messung und Folio-Lauf**: Body-
  Start per arithmetischem Rücklauf (Wert 1), die Lücke wird im Bericht
  markiert — nie verschwiegen.
- **Rot-Fall** (Klassifikator lügt): Anker-Gate kippt rot, Auto-Rollback,
  Datei byte-identisch (in der Suite bewiesen: `test_rot_sonde_…`).
