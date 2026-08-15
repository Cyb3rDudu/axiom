# Doku-Inventar & Migrations-Zuordnung

Bestandsinventur für D2 (Epic #138). Jede existierende Doc-Datei des
Repositories hat hier ein dokumentiertes Schicksal: **migriert + überarbeitet**,
**aufgelöst** (Inhalt lebt verteilt woanders) oder **im Git-Archiv verbleibend**
(aus der Site-Nav). Die kanonische Quelle bleibt pro Eintrag benannt.

> **Grundsatz:** Kanonische Quellen liegen in `axiom_ng/docs/` und
> `axiom_ng_runner/`. Die Site rendert **abgeleitete, allgemeingültige Seiten**
> aus `docs/`; die kanonischen Dateien werden nicht verschoben oder verändert.
> Abweichungen sind in den Zielzeilen explizit ausgewiesen.

## Zuordnungstabelle

| Quelldatei | Schicksal | Ziel in der Site | Überarbeitungsbedarf |
| --- | --- | --- | --- |
| `axiom_ng/docs/PROCESSOR_CONTRACT.md` | **Kanonisch bleiben** — nicht verschoben, nicht umgeschrieben | [Developer Guide → PROCESSOR_CONTRACT v1](developer-guide/processor-contract.md) (Referenz-Seite) | Site-Seite fasst zusammen + verweist auf die kanonische Datei; kein Verbatim-Render, um keine privaten Beispielwerte der kanonischen Datei öffentlich zu spiegeln. Kanonische Quelle benannt. |
| `axiom_ng/docs/EXTERNAL_RUNNER_DEPLOYMENT.md` | **Migriert + stark überarbeitet** (Allgemeingültigkeit) | [Operations → Deployment](operations/deployment.md) | Private Infra (Rechnernamen, IPs, `/Users/…`-Pfade) → Platzhalter `<runner-host>`, `<port>`; die gemessenen Muster (host-network, CDI, GPU-Pinning, MPS) bleiben als Anforderungen/Regeln. |
| `axiom_ng/docs/L8_DURCHSTICHS_ANALYSE.md` | **Migriert als datierter Messbericht** | [Referenzen → Benchmarks](references/benchmarks.md) | Rahmung „Messbericht“, Datum; Zahlensubstanz erhalten, Erzählstruktur neutralisiert; Betriebs-/Verlaufs-Storys → Troubleshooting-Muster-Verweis. |
| `axiom_ng/docs/TC2_PARALLEL_BENCHMARK.md` | **Migriert als datierter Messbericht** | [Referenzen → Benchmarks](references/benchmarks.md) | Rahmung „Messbericht“, Datum; private Infra entfernt, Messwerte erhalten. |
| `axiom_ng/docs/MASS_CHUNKING_BENCHMARK.md` | **Migriert als datierter Messbericht** | [Referenzen → Benchmarks](references/benchmarks.md) | Rahmung „Messbericht“, Datum; private Infra/IPs entfernt, Zahlen erhalten. |
| `axiom_ng/docs/CHUNK_QUALITY_ASSESSMENT.md` | **Migriert als datierter Messbericht** | [Referenzen → Benchmarks](references/benchmarks.md) | Rahmung „Messbericht“, Datum; Inhalte weitgehend allgemeingültig, Verdichtung bei Bedarf. |
| `axiom_ng/docs/LEASE_DISPATCHER_PROCESSOR_ADAPTER_WORK_ORDER.md` | **Aufgelöst** — Inhalt lebt verteilt in den Developer-Docs (Architektur-Übersicht, Go-, Runner-, Contract-Kapitel). Datei bleibt im Git als historische Quelle, **nicht** in der Nav. | aus der Nav → [Developer Guide](developer-guide/architecture.md), [Runner](developer-guide/axiom-ng-runner.md) | Inhalt extrahiert; die Arbeitsauftrags-/Session-Verwaltung (Checklisten, Reviewer-Instruktionen) gehört nicht in eine System-Doku. |
| `axiom_ng_runner/README.md` | **Migriert + erweitert** in Developer/Runner-Kapitel; README bleibt Kurz-Datei am Code (sie verweist auf die Site). | [Developer Guide → axiom_ng_runner](developer-guide/axiom-ng-runner.md) | Gate-5-blockers → „Bekannte Einschränkungen", Herausforderungen neutral formuliert. |
| root `README.md` | **Bleibt Schaufenster** (D3 erneuert es). | [Willkommen-Site](index.md) | Site-Link prominent (erledigt in D1). D3 schreibt Inhalt neu. |
| `axiom_ng/docs/plans/*` (AXIOM_NG_GO_MIGRATION, ML_RUNTIME_ARCHITECTURE, ZOTERO_DESKTOP) | **Nicht in der Site** — verbleiben als Archiv-Referenz. | — | Reicht: Verweis auf Archiv in [About/Archiv](about/index.md). |

## Regeln (aus #140 / Epic-Kommentar)

- **Kanonische Quelle des Processor-Contracts bleibt `axiom_ng/docs/PROCESSOR_CONTRACT.md`.** Die Site-Seite
  rendert davon (Zusammenfassung + kanonischer Verweis), verschiebt die Datei aber nicht.
- **D5 (#143) wird NICHT vorgezogen** — Developer-Guide-Vertiefung (Configure-Referenz, Testing,
  Datenmodell, Architektur-Diagramme) gehört zu D5, nicht zu D2. D2 sichert nur Bestand und
  neutralisiert, erfindet keine neuen Inhalte.

## Link-Garantie

Jeder Nav-Punkt von `mkdocs.yml` zeigt auf eine existierende Seite; der
`mkdocs build --strict` ist der obligatorische Link-Check. Seiten, die D3–D6
vertiefen, existieren mit „In Arbeit"-Hinweis.
