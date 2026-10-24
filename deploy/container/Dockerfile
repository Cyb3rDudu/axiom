# Dockerfile — the F14 topology image (#308): ONE image, roles per
# argument (`serve all|api|library|store` via the axiom binary, `worker`
# via the reference-mode compute service; see entrypoint.sh). The build
# needs the repository root as context:
#
#   docker build -f deploy/container/Dockerfile -t axiom-topology .
#
# The image carries ONLY the light/reference stack (fastapi, pymupdf) —
# the heavy GPU deps of the real backend are out of scope for the
# topology proof (the processor contract is backend-transparent).
FROM golang:1.26 AS build
# Version stamp + port guard: without BuildType=release the binary is a
# debug build and refuses the production ports (AXIOM_ALLOW_DEBUG_BIND would
# only mask that). Same ldflags as the Makefile `rag` target.
ARG VERSION=dev
ARG COMMIT=none
WORKDIR /src
# layer-cache friendliness: modules first, then sources
WORKDIR /src/axiom
COPY axiom/go.mod axiom/go.sum ./
RUN go mod download
COPY axiom ./
RUN CGO_ENABLED=0 go build \
    -ldflags "-X github.com/Cyb3rDudu/axiom/axiom/internal/version.Version=${VERSION} -X github.com/Cyb3rDudu/axiom/axiom/internal/version.Commit=${COMMIT} -X github.com/Cyb3rDudu/axiom/axiom/internal/version.BuildType=release" \
    -o /out/axiom ./cmd/axiom

FROM python:3.11-slim
# pandoc: the EPUB lane shells out to it (same as the CI runner job).
# tesseract/ghostscript (+deu): the repair worker's OCR path (ocrmypdf,
# T2 class); the label-heal path (T1, pikepdf) needs none of them.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        pandoc tesseract-ocr tesseract-ocr-deu ghostscript \
    && rm -rf /var/lib/apt/lists/*

# The repair worker (axiom-fixer / pdf_repair_agent, F08 #302) is exec'd by
# the repair role IN THIS CONTAINER (AXIOM_REPAIR_WORKER_CMD, default
# /opt/axiom/bin/axiom-repair-worker — hence that exact path). Autarkic deps
# per axiom-fixer/requirements.txt: its own pinned set, never shared with the
# worker image's stack.
COPY axiom-fixer/requirements.txt /tmp/fixer-requirements.txt
RUN pip install --no-cache-dir -r /tmp/fixer-requirements.txt \
    && mkdir -p /opt/axiom/bin /opt/axiom/fixer
COPY --chmod=0755 deploy/container/repair-worker.sh /opt/axiom/bin/axiom-repair-worker
COPY axiom-fixer /opt/axiom/fixer/app
WORKDIR /opt/axiom/worker
COPY axiom-compute-worker/requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -r /tmp/requirements.txt
COPY axiom-compute-worker/axiom_compute_worker ./axiom_compute_worker
COPY --from=build /out/axiom /usr/local/bin/axiom
COPY --chmod=0755 deploy/container/entrypoint.sh /usr/local/bin/entrypoint.sh
# State roots pre-created and owned by the runtime user: fresh named
# volumes mount root-owned otherwise, and every role writes under
# /var/lib/axiom (artifacts, quarantine, worker roots) — uid 1100 would
# EACCES on Linux (F14 review finding; podman's auto-chown masked it).
RUN useradd --system --create-home --uid 1100 axiom \
    && mkdir -p /var/lib/axiom \
    && chown -R axiom:axiom /var/lib/axiom
USER axiom
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
