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
WORKDIR /src
# layer-cache friendliness: modules first, then sources
WORKDIR /src/axiom
COPY axiom/go.mod axiom/go.sum ./
RUN go mod download
COPY axiom ./
RUN CGO_ENABLED=0 go build -o /out/axiom ./cmd/axiom

FROM python:3.11-slim
# pandoc: the EPUB lane shells out to it (same as the CI runner job)
RUN apt-get update \
    && apt-get install -y --no-install-recommends pandoc \
    && rm -rf /var/lib/apt/lists/*
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
