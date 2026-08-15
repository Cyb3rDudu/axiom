# axiom_ng_runner (Python)

Der **Python-Prozessor-Runner** ist ein Loopback-HTTP-Dokumentprozessor, der
`PROCESSOR_CONTRACT` (Transport-Contract v1) implementiert. Er besitzt **nur
Berechnung und temporäre Job-Ausgabe**; aller durable Anwendungszustand lebt in
`axiom_ng`.

> **Kanonische Quellen** für dieses Kapitel sind die Dateien im Paket:
> `README.md`, `config.py` und `PROCESSOR_CONTRACT` (Contract v1). Diese Seite
> ist ihre allgemeingültige Zusammenfassung für die Site.

## Was er tut

```text
POST /v1/process  (202, async)
GET  /v1/health
GET  /v1/capabilities
GET  /v1/jobs/{job_id}
GET  /v1/jobs/{job_id}/result
GET  /v1/jobs/{job_id}/artifacts/{artifact_ref}
POST /v1/jobs/{job_id}/cancel
POST /v1/jobs/{job_id}/ack
```

Die Verarbeitung ist asynchron: `POST /v1/process` validiert die Quelle, akzeptiert
mit `202` und reiht die Compute-Arbeit in einen Hintergrund-Worker ein. Der Client
pollt `GET /v1/jobs/{id}` bis zu einem Endzustand und holt dann das Ergebnis.

## Compute-Backends

| Backend | Verwendung | Abhängigkeiten |
| --- | --- | --- |
| `reference` (Default) | Hermetische Vertrags-Tests; leichte echte Konvertierung | fastapi, uvicorn, pydantic, pymupdf |
| `real` | Vendor-ed `compute_core`: Marker/pdf_worker, epub_worker, Embedder & Extraktoren | torch, FlagEmbedding, gliner, mrebel, marker-pdf |

Das `reference`-Backend konvertiert PDF über PyMuPDF und EPUB über zipfile,
wiederverwendet einen hermetic-deterministischen Chunker und emittiert
vertragsförmige Ergebnisse mit glaubwürdiger Seiten-/Sektions-Provenienz. Es
berührt nie Datenbank, OpenSearch, Graph oder Zotero-Store.

## Start

```bash
.venv/bin/uvicorn axiom_ng_runner.app:app --host 127.0.0.1 --port 8537
# oder
.venv/bin/python -m axiom_ng_runner
```

Konfiguration (siehe `config.py`):

```text
AXIOM_PROCESSOR_BIND_ADDR=127.0.0.1
AXIOM_PROCESSOR_PORT=8537
AXIOM_PROCESSOR_WORK_ROOT=/tmp/axiom_processor_work
AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS=/path/to/zotero/storage
AXIOM_PROCESSOR_MAX_CONCURRENT_JOBS=1
AXIOM_PROCESSOR_COMPUTE=reference   # oder "real"
```

Details zu allen Variablen: [Operations → Deployment](../operations/deployment.md) und
die Env-Referenztabelle dort.

## Tests

```bash
pytest tests/ -v
```

Die Black-Box-Vertrags-Tests (`PROCESSOR_CONTRACT` §19) laufen gegen das
`reference`-Backend und brauchen nur Runtime + pymupdf + fastapi:
Health/Capabilities, Idempotenz, PDF→Markdown+Chunks, Seiten-/Sektions-Provenienz-
Round-Trip, Hash-Mismatch, Referenz-Integrität, Embeddings vs. Capabilities, kein
Durable-Store-Zugriff, Cancellation, ACK-Cleanup+Idempotenz, Restart-Recovery ohne
Fake-Success und keine durable Quell-Kopie.

## Bekannte Einschränkungen

Das `reference`-Backend (Default, vom Vertrags-Suite genutzt) ist vollständig und
vertragskonform. Das `real`-Backend (Marker/GPU-Compute) hat offene Lücken, die
geschlossen werden müssen, bevor es produktiv genutzt wird — hier festgehalten,
damit sie nicht verloren gehen:

- **Subprozess-Cancellation im real-Backend ist funktionslos.** `_real_pipeline`
  startet Marker/pdf_worker über einen blockierenden `subprocess.run` und
  registriert den Prozess-Handle nicht in `_running`; der Terminate-Zweig des
  Cancel-Endpunkts ist für real-Jobs toter Code (Contract §17/§9.2). Fix: den
  `Popen`-Handle in `_running[job_id]["process"]` halten, damit `job_cancel`
  `terminate()` aufrufen kann, und den Subprozess kooperativ machen (Cancel-Poll).
- **Das real-Backend hat GLiNER/mREBEL-Extraktoren nicht angebunden.**
  `_real_pipeline` ruft nur Konverter + Chunker + TextEmbedder; Entities/Relationen
  nutzen in beiden Backends noch den Reference-Regex-Extraktor.
- **`prune_expired` wird nie eingeplant**, daher ist `result_retention_seconds`
  aktuell wirkungslos und ACKed-Job-Tombstones akkumulieren. Periodischen Sweep
  ergänzen.
- **Kein Request-Queue-Cap** (§9.2): Jeder akzeptierte POST startet einen
  unbegrenzten Daemon-Thread; das Semaphor begrenzt nur die Konkurrenz, nicht die
  Queue-Länge.

Weiter: [PROCESSOR_CONTRACT v1](processor-contract.md) ·
[Architektur-Übersicht](architecture.md) · [Operations → Deployment](../operations/deployment.md)
