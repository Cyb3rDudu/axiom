# Custody-Runbook — Manuelle Reparatur über `POST /api/repair/custody` (#279)

**Werkzeug:** EIN Aufruf fährt das quarantine-first-Protokoll für eine manuell
geheilte Datei — identische Reihenfolge wie der Fixer-Autoheal-Pfad
(`internal/repair/apply.go`), dieselben Write-Mutationen, dieselbe
Quarantäne. **Kein Geschwister-Upload, kein Papierkorb-Improvisieren mehr.**

```bash
curl -sS -X POST http://<axiom-host>:<port>/api/repair/custody \
  -F attachment_key=DAM2Q443 \
  -F reason="Manuelle Reparatur nach Fixer-HALT: Seiten 47-49 repariert" \
  -F healed_file=@/pfad/zur/geheilten-datei.pdf
```

Optionales Formularfeld `content_type` (Default `application/pdf`,
EPUB-Reparaturen: `application/epub+zip`). Antwort: Schrittreport
(Quarantäne-Pfad, neuer Attachment-Key, Schema-Dateiname) —
`next_step` erinnert an den Sync.

## Wann einsetzen

- Nach einem **Fixer-HALT** (#278-Klasse): der Bibliothekar hat die Datei von
  Hand geheilt ( externes Werkzeug, PDF-Editor) und die Bibliothek soll die
  geheilte Fassung als einzige verarbeitbare nehmen.
- Nach jedem Fall, in dem früher "Geschwister hochladen + altes Attachment in
  den Papierkorb" improvisiert wurde (Geursen-Vorfall 2026-09-17: die
  Projektion hielt das alte Attachment preferred, die Reparatur war
  unsichtbar, bis der Operator den Papierkorb von Hand zog).

## Was der Aufruf treibt (Protokoll)

1. **Quarantäne des Originals** — Datei wird VOR jeder Mutation in den
   Quarantäne-Bereich kopiert (`<quarantine-root>/originals/`), custody
   fail-closed: schlägt das Kopieren fehl, passiert nichts weiter.
2. **Protokoll-Satz** — `<quarantine-root>/manual/<KEY>.json`: Original-Pfad,
   Attachment-Key, Grund, Datum + jeder abgeschlossene Schritt.
3. **Altes Attachment-Item löschen** — version-guarded über die
   Zotero-Write-API (Mutation 1, `DeleteAttachmentItem`).
4. **Geheilte Datei hochladen** — unter dem Parent-Item mit Schema-Dateiname
   (`{Autor|Institution} - {Jahr} - {Titel}.ext`, Mutation 2,
   `CreateAttachmentWithFile`).
5. **Sync auslösen** (`POST /api/zotero/sync`) — das geheilte Attachment ist
   jetzt der einzige verarbeitbare Kandidat → preferred → Preflight →
   reguläres Processing. Der Endpoint antwortet mit `next_step` als Erinnerung.

## Abbruch & Nachlauf (idempotent)

- Jeder Schritt landet sofort im Protokoll-Satz. Bricht der Aufruf mittendrin
  ab (Gateway-Fehler, Netz), zeigt der Satz genau die abgeschlossenen
  Schritte.
- **Einfach erneut aufrufen** (gleicher `attachment_key`, geheilte Datei).
  Der zweite Lauf: quarantiniert erneut (custody-konservativ), behandelt ein
  bereits gelöschtes Item (404) als erledigt und vollendet das Protokoll.
- **Nach Erfolg verweigert das Werkzeug** (409 + vollständiger Report): ein
  zweiter Upload würde ein zweites geheiltes Geschwister erzeugen — genau der
  Zustand, den das Werkzeug verhindert. Erneut reparieren (auch die geheilte
  Fassung ist kaputt) nur über den manuellen Weg unten.

## Guards (vor jeder Mutation)

| Fehler | Bedeutung |
| --- | --- |
| `400` | `attachment_key`/`reason` fehlt, Datei leer/unlesbar, content_type unbekannt |
| `404` | Attachment-Key der Bibliothek unbekannt oder gelöscht |
| `409` | Key wurde bereits erfolgreich geheilt (Report im Body) |
| `502` | Zotero-Write-Gateway-Fehler mittendrin — Satz zeigt den Stand, erneuter Aufruf setzt fort |

## Manuelle Wiederherstellung (kein Un-Quarantine-Automatismus)

Das Original liegt in `<quarantine-root>/originals/<KEY>_<unixns>.pdf`.
Wiederherstellung = Datei zurückkopieren, manuell als Attachment unter das
Parent-Item laden (Zotero UI), Sync. Der Protokoll-Satz
(`<quarantine-root>/manual/<KEY>.json`) dokumentiert Grund und Verlauf.

## Warum kein CLI eigener Bauart

Der RAG ist das einzige Zotero-Gateway (Design-Nagel #184): Credentials und
Write-Key leben im Server. Der Endpoint nutzt den authorisierten
Write-Client des Repair-API-Kontexts — ein separates CLI müsste Credentials
duplizieren. Der Aufruf oben IST der eine Befehl.
