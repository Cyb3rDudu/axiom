# Deployment: Runner-Betrieb (GPU-Compute-Offload)

Dieses Kapitel beschreibt, wie der `axiom_ng_runner` (der Python-Prozessor) auf
einem externen GPU-Host betrieben und wie der `axiom_ng`-Dispatcher an ihn
angeschlossen wird. Der Zweck: schwere Dokument-Verarbeitung (Marker-Konvertierung,
BGE-M3-Embeddings, GLiNER-Entities, mREBEL-Relationships) auf NVIDIA-GPUs statt am
Dispatcher-Host auszuführen.

> **Allgemeingültig:** Konkrete Hostnamen, IPs und Benutzernamen sind durch
> Platzhalter wie `<runner-host>`, `<port>` oder generische Beschreibungen ersetzt.
> Die wiedergegebenen Muster sind **Anforderungen und Betriebsregeln** — sie stammen
> aus Messungen, sind aber unabhängig von einer bestimmten Maschine formulierbar.
> Beispiel-Ports (z. B. `19542`) sind Illustration, keine Vorgabe.

## Architektur

```text
Dispatcher-Host (axiom-ng)             Runner-Host (GPU)
┌──────────────────────────┐  HTTP/JSON ┌──────────────────────────────┐
│ Go-Dispatcher             │ ──────────▶ │ axiom_ng_runner (Python)      │
│ POST /v1/process          │   Port     │ Konvertierung + Embeddings +  │
│ pollt Status/Ergebnis     │ ◀───────── │ Entity/Relation-Extraktion     │
│ persistiert in Postgres   │            └──────────────────────────────┘
└──────────────────────────┘
```

Der Runner ist **reine Compute**: Er greift weder auf Postgres, OpenSearch oder
Zotero zu. Aller durable Zustand bleibt beim Dispatcher. Über die Leitung geht
nur der HTTP-Vertrag (`PROCESSOR_CONTRACT v1`).

## Voraussetzungen auf dem GPU-Host

- NVIDIA-GPU(s) + Treiber (verifizieren: `nvidia-smi`)
- Podman (rootless funktioniert) oder ein äquivalenter OCI-Container-Runner
- NVIDIA-CDI-Integration: die CDI-Spezifikation
  (`/var/run/cdi/nvidia-container-toolkit.json`) muss vorliegen. Läuft bereits ein
  GPU-Container auf dem Host, ist CDI meist eingerichtet — diese Konfiguration kann
  als Referenz übernommen werden.

## 1. Code zum Runner-Host bringen

Der Runner ist selbsttragend (das `compute_core`-Vendor-Verzeichnis ist in
`axiom_ng_runner/` enthalten): Ein einzelner Verzeichnisbaum muss zum
Runner-Host übertragen werden:

```bash
rsync -av --exclude='.venv' --exclude='__pycache__' --exclude='.pytest_cache' \
  axiom_ng_runner/ <user>@<runner-host>:<pfad>/axiom_ng_runner/
```

## 2. Containerfile

Bau-Checks (aus der Betriebserfahrung abgeleitet, als Anforderungen formuliert):

1. **Selbsttragender Runner:** Das Image kopiert **nur** `axiom_ng_runner/`
   (inkl. `compute_core`). Kein DB-Adapter, kein veraltetes Modul — die
   DB-Treiber-Importkette ist seit dem Compute-Core-Vendor-Split (#118) nicht
   mehr im Runner.
2. **Triton-JIT braucht einen Compiler + libc:** `gcc` und `libc6-dev` explizit
   installieren (sonst schlägt der erste Dense-Embedding-Lauf an fehlendem
   `crti.o` fehl). Kein `--no-install-recommends`, ohne diese Pakete beizulegen.
3. **Versionen identisch zu der Referenz-Venv pinnen** (siehe
   `axiom_ng_runner/requirements-heavy.txt`), allen voran
   `marker-pdf==1.10.2`. Divergente Versionen erzeugen divergente Ergebnisse.
4. **`RUN touch /.dockerenv`** — siehe Falle 10 in der
   [L8-Durchstichs-Analyse](../references/benchmarks/l8-durchstich.md).

```dockerfile
FROM python:3.11-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc libc6-dev pandoc libglib2.0-0 libgl1 \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir -r requirements-heavy.txt
COPY axiom_ng_runner/ /app/axiom_ng_runner/
WORKDIR /app
ENV PYTHONPATH=/app
EXPOSE <port>
RUN touch /.dockerenv
CMD ["python", "-m", "axiom_ng_runner"]
```

## 3. Bauen und mit GPU starten

**`--network=host` verwenden — das ist die kritische Einstellung.** Eine Port-
Weiterleitung (z. B. `-p <mapped>:<port>`) über den Userspace-Port-Forwarder von
rootless Podman zeigt dieselbe Störsignatur wie ein langsamer Tunnel:
Kleine Pakete (Polls, Health-Checks) sind millisekundenschnell, während
mehrere-MB-Ergebnis-JSONs und Artifakt-Bodys kriechen. Ein Loopback-Test
**innerhalb** des Containers misst nur inneren Schnellpfad, nicht den gemappten
Weg. Symptom einer Transportfalle: GPU idle, keine Dispatcher-Fehler, Jobs
hängen „nach Compute fertig". Dann zuerst den Serving-Pfad prüfen, nicht die
Compute.

```bash
podman build -t runner <pfad>/

# Host-Network + CDI-Device-Injektion — bindet den Host-Port direkt; kein -p-Mapping nötig.
podman run -d --name runner \
  --network=host \
  --device nvidia.com/gpu=all \
  -e AXIOM_PROCESSOR_COMPUTE=real \
  -e AXIOM_PROCESSOR_BIND_ADDR=0.0.0.0 \
  -e AXIOM_PROCESSOR_PORT=<port> \
  -e AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS=/nonexistent \
  -e DEVICE_GLINER=cuda \
  localhost/runner
```

- `--device nvidia.com/gpu=all` legt alle Host-GPUs offen. Zum Pinnen auf eine
  GPU den CDI-Gerätenamen der gewünschten Index-Karte statt `all` verwenden
  (Namen stehen in `/var/run/cdi/nvidia-container-toolkit.json`).
- Für parallele Ein-GPU-Runner (ein Runner pro Karte) `CUDA_VISIBLE_DEVICES=<n>`
  je Container setzen — `cuda:0` in PyTorch zeigt dann auf die jeweilige
  physische GPU.
- **Port wählen** einen dedizierten hohen Port und in der Host-Firewall öffnen.
  Beide Richtungen müssen direkt erreichbar sein: Quell-Download (Runner zieht
  vom Dispatcher) und Ergebnis-/Artifakt-Abruf (Dispatcher zieht vom Runner)
  sind beides MB-große Bulk-Flows, die echten LAN-Durchsatz brauchen.
- **GLiNER-Device:** `DEVICE_GLINER=cuda` muss explizit gesetzt werden. Der
  Default ist `cpu` — ein CPU-GLiNER kostet ~1 Stunde pro Buch statt ~5 Minuten
  auf GPU.

**Per-Model-Geräte-Knobs** (Quelle der Wahrheit: `compute_core/devices.py`,
`_MODEL_DEVICE_ENV`):

| Env-Var | Modell | Default |
| --- | --- | --- |
| `DEVICE_EMBEDDER` | BGE-M3 | `auto` |
| `DEVICE_MARKER` | Marker | `auto` |
| `DEVICE_MREBEL` | mREBEL | `auto` |
| `DEVICE_GLINER` | GLiNER | `cpu` |

**Bind-Adresse:** `0.0.0.0` ist für Remote-Zugriff nötig. Nur auf LAN-only-Hosts
verwenden — der Runner hat bewusst keine Authentisierung (er läuft per Design
nur auf Loopback oder vertrauenswürdigem Netz, siehe Contract §18).

### Alternative: Ablauf am Dispatcher-Host (Apple-MPS)

Ein GPU-Lauf ist nicht zwingend extern. Auf einem Apple-Mac mit MPS ist der
vollständige `real`-Pipeline-Ablauf möglich (Validierung #128):

```bash
DEVICE_GLINER=mps PYTORCH_ENABLE_MPS_FALLBACK=1 \
  AXIOM_PROCESSOR_COMPUTE=real .venv/bin/python -m axiom_ng_runner
```

- Device-Auflösung braucht keine Env für Marker/Embedder/mREBEL (`auto` → mps);
  GLiNER will explizit `DEVICE_GLINER=mps`.
- Bekannte MPS-Grenze: suryas Tabellenerkennung (`TableRecEncoderDecoderModel`)
  ist nicht MPS-kompatibel und fällt mit Warnung auf CPU zurück — tabellenlastige
  PDFs zahlen extra.
- MPS ist **vollständig, aber langsam** (gemessen ~13 s/Seite vs. ~0,7–1,2 s/Seite
  auf einer RTX-3090-Klasse). Für Produktions-Massenbetrieb externe GPUs.

## 4. Verifizieren

```bash
# CUDA im Container:
podman exec runner python -c \
  "import torch; print('cuda:', torch.cuda.is_available(), '| devices:', torch.cuda.device_count())"

# GLiNER lädt und sagt voraus:
podman exec runner python -c "
from gliner import GLiNER
m = GLiNER.from_pretrained('urchade/gliner_multi-v2.1')
print(m.predict_entities('Steve Jobs founded Apple.', ['PERSON']))"

# Health/Endpoints:
curl http://<runner-host>:<port>/v1/health
curl http://<runner-host>:<port>/v1/capabilities
```

Der erste Lauf lädt ~3 GB Modellgewichte (Marker/surya + GLiNER + mREBEL);
spätere Läufe sind warm im Cache.

## Transport-Regel (gemessene Lehre)

Zwei Transport-Ebenen nacheinander maskierten ein Problem während eines
Massenlaufs:

1. **Tunnel-Bulk-Kollaps** auf dem Kontroll-Pfad (ms-Latenz auf kleinen Paketen,
   aber ~35–83 KB/s auf MB-Flows trotz „direkter" Verbindung).
2. **Userspace-Port-Weiterleitung** im rootless-Podman — dieselbe Kollaps-Signatur
   auf der Container-Ebene (`--network=host` behebt sie).

**Betriebsregel:** Dispatcher↔Runner-Bulk-Flows (Ergebnis-JSON, Artifakt-Bodies)
brauchen **direkte LAN-Erreichbarkeit in beiden Richtungen** —
`AXIOM_PROCESSOR_URL` auf den Runner-Host-Port, und die `source_url`-Basis des
Runners auf die LAN-Adresse des Dispatchers. Ein Tunnel funktioniert für die
Kontrollebene und ist der Fallback ohne direkten Pfad (mit Durchsatz-Nachteil).
**Symptom-Signatur einer Transportfalle:** GPU idle, keine Dispatcher-Fehler,
Jobs zwischen Compute-Fertig und Persistiert für Minuten aufgehängt — den
Serving-Pfad (Loopback vs. gemappter Port vs. Tunnel) prüfen, bevor die Compute
beschuldigt wird.

## 5. Dispatcher anbinden

Auf dem Dispatcher-Host:

```bash
export AXIOM_DISPATCHER_ENABLED=true
export AXIOM_PROCESSOR_URL=http://<runner-host>:<port>   # direktes LAN — s. Transport-Regel
```

Der Dispatcher verhandelt beim Start die Capabilities gegen den Remote-Runner
und schlägt fehl, wenn er nicht erreichbar oder vertragsinkompatibel ist. Vor
Massenbetrieb einen Test mit einem kleinen Dokument fahren.

### Quell-Lieferung über `source_url` (ohne gemeinsames Zotero-Mount)

Der Remote-Runner braucht **keinen** Zugriff auf den Zotero-Speicher. Der
Dispatcher hängt jedem Process-Request eine HMAC-signierte Download-URL
(`attachment.source_url`) an; der Runner zieht die Bytes per HTTP (Contract §3,
additive v1-Erweiterung). Dispatcher-seitig konfigurieren:

```bash
# Gemeinsamer HMAC-Secret (Dispatcher signiert, .../source verifiziert).
# Leer = Feature auf beiden Seiten aus.
export AXIOM_PROCESSOR_SOURCE_SECRET='<random-hex>'
# Basis-URL, mit der der Runner den Dispatcher erreicht — NICHT 127.0.0.1 (der Runner
# löst auf seinem eigenen Host auf):
export AXIOM_PROCESSOR_SOURCE_BASE_URL=http://<dispatcher-lan-ip>:<dispatcher-port>
# Dispatcher muss auf einer erreichbaren Schnittstelle lauschen:
export AXIOM_BIND_ADDR=0.0.0.0
# POST /v1/process wartet den synchronen Download ab; das Ergebnisse-Budget floor-t die Submit-Call:
export AXIOM_PROCESSOR_TIMEOUT=180s
# Hinweis auf das fast-gleiche Paar (anderer Scope):
#   AXIOM_PROCESSOR_TIMEOUT        — DISPATCHER-seitiges Ergebnis-Fetch-Budget
#   AXIOM_PROCESSOR_SOURCE_TIMEOUT — RUNNER-seitiges Quell-Download-Budget (Default 120s)
```

Runner-seitig `AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS` ungesetzt oder auf einen
nicht existenten Pfad lassen — dann ist lokale Zustellung konstruktiv unmöglich
und jede Quelle kommt über die signierte URL. Die URL verfällt mit der Arbeit des
Jobs; heruntergeladene Bytes laufen durch denselben Hash-Gate wie lokale Dateien
und sterben mit dem ACK (Contract §18/§19-13).

> Ältere Experimente mit rsync-Brücke oder sshfs-Mount sind durch diesen Mechanismus
> abgelöst — keine Zotero-Kopien auf dem GPU-Host anlegen (Contract §15 / §19 test 12).

## 6. Runner-Identität + GPU-Sampler-Labels

Bei mehreren Runnern muss jede Log-Zeile und jede Job-Zeile sagen, welcher Runner
sie erzeugt hat:

```bash
export AXIOM_PROCESSOR_RUNNER_NAME=<runner-label>
```

Das Label landet in der Phasen-Log-Zeile (`phases[ok]: runner=<label> job=…`) und
in `ingest_jobs.runner_name` zum Claim-Zeitpunkt. Die Verteilung ist dann reines SQL:

```sql
SELECT runner_name, count(*), avg(completed_at - started_at)
FROM ingest_jobs WHERE status = 'completed' GROUP BY 1;
```

> Die Spalte ist bewusst **nicht** `processor_name` (die trägt die
> Prozessor-Software-Identität bei Fertigstellung und darf nicht überschrieben
> werden).

GPU-Sampler pro Runner (30-s-Takt), Label zuerst für Zuordenbarkeit nach Log-Merge:

```bash
nohup sh -c 'while true; do echo "<runner-label> $(date +%s) $(nvidia-smi --query-gpu=index,memory.used,utilization.gpu --format=csv,noheader)"; sleep 30; done' \
  > /tmp/gpu_sampler_<runner>.log 2>&1 &
```

Mit `CUDA_VISIBLE_DEVICES`-Pinning je Container identifizieren GPU-Index + Label
den Runner eindeutig.

## Runner-Env-Variablen (Referenz)

| Env-Var | Default | Bedeutung |
| --- | --- | --- |
| `AXIOM_PROCESSOR_BIND_ADDR` | `127.0.0.1` | `0.0.0.0` für Remote-Zugriff |
| `AXIOM_PROCESSOR_PORT` | `8537` | HTTP-Port |
| `AXIOM_PROCESSOR_COMPUTE` | `reference` | `real` für die GPU-Pipeline |
| `AXIOM_PROCESSOR_MAX_CONCURRENT_JOBS` | `1` | Marker+Modelle sind VRAM-lastig; 1 pro GPU |
| `AXIOM_PROCESSOR_WORK_ROOT` | `/tmp/axiom_processor_work` | Temporärer Job-Zustand |
| `AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS` | — | Lokale Host-Pfade, die der Runner lesen darf |
| `AXIOM_PROCESSOR_RESULT_RETENTION` | `3600` | Sekunden, bevor unacknowledged Ergebnisse verfallen |

## Massenverarbeitung mit dem Remote-Runner

1. Dispatcher-`Concurrency=1` lassen (der Runner erzwingt
   `MAX_CONCURRENT_JOBS=1` ohnehin; parallele Jobs stritten um VRAM).
2. Bei kleinen Dokumenten auf warmem Cache auf einer 3090-Klasse ~30 s erwarten;
   große gescannte Bücher skalieren mit der OCR-Last.
3. Der Runner hält Ergebnisse bis zum ACK; der ACK-Retry-Pass des Dispatchers
   erholt sich, wenn der Dispatcher mitten im Batch neu startet.

## Bekannte Grenzen (ohne Verweis auf konkrete Maschinen)

- `capabilities.models.dense_embedding.name` meldet auch im real-Modus
  `reference-bge-m3` (kosmetisch; die Vektoren selbst sind echte 1024-dim-BGE-M3).
  Zurückgestellt.
- Der Runner lädt alle drei Modelle-Familien pro Prozess; der VRAM-Fußabdruck
  liegt ~2,8 GB — passt auf eine 12-GB-Karte, lässt auf 24-GB-Karten Raum für
  einen zweiten gepinnten Runner pro Karte.

Weiter: [Monitoring](monitoring.md) · [Troubleshooting](troubleshooting.md) ·
[PROCESSOR_CONTRACT v1](../developer-guide/processor-contract.md)
