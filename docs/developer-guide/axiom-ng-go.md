# axiom_ng (Go)

`axiom_ng` ist der Go-Orchestrator des Systems. Er besitzt alle durable
Anwendungserwartung: Zotero-Synchronisation, Ingest-Jobs, Leases, Retries,
Cancellation, persistente IDs, versionierte Verarbeitungs-Snapshots, die
durable abgeleitete Artefakte, Chunks/Embeddings/Entities/Beziehungen, die
PostgreSQL/pgvector- und Knowledge-Graph-Schreibpfade sowie die
OpenSearch-Outbox-/Index-Synchronisierung.

Der Python-Runner rechnet; `axiom_ng` orchestriert, validiert und persistiert.

> Dieses Kapitel ist die Zusammenfassung aus `PROCESSOR_CONTRACT` und der
> aufgelösten Work-Order (`LEASE_DISPATCHER_PROCESSOR_ADAPTER_WORK_ORDER.md`,
> im Git als historische Quelle erhalten). Die Vertrag-Strukturen sind in
> [PROCESSOR_CONTRACT v1](processor-contract.md) exakt spezifiziert.

## Kernkomponenten

- **Dispatcher** — werkelt losgelöst von den HTTP-Handlern; stable Worker-ID,
  konfigurierbare Konkurrenz, Poll-Intervall mit Jitter, Lease-Dauer und
  Renewal-Intervall, Graceful-Shutdown, Cancel-Funktion. Er verhandelt beim
  Start die Capabilities gegen den Prozessor, bevor er Claims verschickt.
- **Persistenz** — atomare, als eine Transaktion committete Snapshots; die
  einzige Wahrheit für Verarbeitungsresultate.
- **Fencing** (Claim-Exklusivität) — jeder Post-Claim-Job-Schreibvorgang ist über
  `job_id + worker_id + lease_token` gefeucht: Nur der aktuelle Lease-Besitzer
  darf einen aktiven Job mutieren. Ein stale Worker kann einen reclaimten Job
  weder komplettieren noch failen.
- **Outbox** — OpenSearch-Indexierung ist outbox-getrieben: Im selben
  PostgreSQL-Transaktion wie der Snapshot wird ein Outbox-Eintrag geschrieben;
  ein separater retrybarer Outbox-Worker synchronisiert. Ein OpenSearch-Ausfall
  erzwingt nie erneutes Marker-Running.

## Lease-State-Machine

Zulässige Übergänge:

```text
pending    -> claimed
claimed    -> processing
claimed    -> pending       Retry vor Prozessor-Akzeptanz
claimed    -> failed / cancelled / skipped
processing -> pending       retrybarer Fehler oder abgelaufener Lease
processing -> completed / failed / cancelled

claimed/processing mit abgelaufenem Lease -> von neuem Owner geclaimt
claimed/processing bei max. attempts     -> failed (LEASE_EXHAUSTED)
```

Das Claim ist **eine** PostgreSQL-Anweisung bzw. ein kurzer Transaction mit
`FOR UPDATE SKIP LOCKED`. Beim Claim: neuer Zufalls-`lease_token`, `claimed_by`
setzen, `attempt` genaue einmal erhöhen, `lease_until`/`last_heartbeat_at` aus
der Datenbank-Zeit, Input-Snapshot + Profil + Idempotency-Key frieren.

**Renewal:** deutlich vor Ablauf (z. B. jedes Drittel der Lease-Dauer),
Datenbank-Zeit statt lokaler Wall-Clock, weiter während Status-Poll/Artifakt-
Download/Validierung/Persistenz, stoppen nach einem terminalen gefeuchten
DB-Übergang. Wiederholter Renewal-Fehler bricht die lokale Dispatch-Arbeit ab.

**Fencing-Prädikat für jeden Post-Claim-Update:**

```text
WHERE id = $job_id
  AND claimed_by = $worker_id
  AND lease_token = $lease_token
  AND status IN (...expected states...)
```

Null-affected-Zeilen => als Lost-Lease behandeln, nie als Erfolg.

## Persistenz- und Ersetzungssemantik

Ein erfolgreiches Ergebnis wird als **ein** unveränderlicher Verarbeitungs-
Snapshot persistiert, identifiziert durch mindestens:

```text
attachment_id
content_hash
processor name and version
processing profile hash
```

Nur **einer** Snapshot ist pro Document/Attachment/Profil aktiv. Der
Aktivitätenwechsel passiert in derselben Transaktion wie das Einfügen der
Ersetzung. Die Persistenz-Transaktion:

1. Neuen Snapshot + abhängige Zeilen einfügen.
2. Zeilen-Zahlen und Referenzen verifizieren.
3. Neuen Snapshot aktiv, den vorigen inaktiv markieren.
4. OpenSearch-Outbox-Arbeit einfügen.
5. Ingest-Job gefeucht als `completed` markieren.
6. Einmal committen.

Schlägt ein Schritt fehl, wird alles zurückgerollt; der vorige aktive Snapshot
bleibt unangetastet. Ein Teil-/ungültiges Ergebnis ersetzt nie den letzten gültigen
Snapshot.

Der Prozessor-Resultat ist erst nach diesem Commit dauerhaft persistiert; der
ACK an den Prozessor erfolgt erst nach dem durable Commit und ist idempotent.

Weiter: [PROCESSOR_CONTRACT v1](processor-contract.md) ·
[axiom_ng_runner (Python)](axiom-ng-runner.md) ·
[Referenzen → Datenmodell](../references/data-model.md)
