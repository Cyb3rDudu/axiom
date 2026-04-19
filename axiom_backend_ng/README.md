# axiom-ng

Go implementation of the axiom backend. Drop-in replacement for the Python
FastAPI backend (`axiom_backend/`), the doc-processor, and the CLI — serving
the same REST + WebSocket contract against the same Postgres+pgvector and
OpenSearch stores.

See [`docs/plans/AXIOM_NG_GO_MIGRATION.md`](../docs/plans/AXIOM_NG_GO_MIGRATION.md)
for the full migration plan.

## Status

Bootstrap only. Exposes `/` and `/health` with byte-identical JSON to the
Python backend; no real endpoints yet.

## Layout

```
axiom_backend_ng/
├── cmd/axiom-ng/        # binary entrypoint
├── internal/
│   ├── api/             # HTTP handlers
│   ├── config/          # env/config loader
│   ├── server/          # chi router, middleware, http.Server lifecycle
│   └── version/         # build info (injected via ldflags)
├── Dockerfile           # multi-stage, FROM scratch
└── Makefile             # test / build / lint / run / docker-build
```

## Local development

```sh
make test
make build
./bin/axiom-ng
curl http://localhost:8010/health
```

Default port is **8010** to avoid collision with the Python backend on 8001.
Override with `AXIOM_NG_PORT`.

## Against the live stack

```sh
docker compose -f docker-compose.yml -f docker-compose.override.ng.yml up
```

This starts axiom-ng alongside the Python backend. No Python routes are
hijacked yet; the Go service is reachable at its own port for smoke testing.
