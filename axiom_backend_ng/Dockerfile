# syntax=docker/dockerfile:1.7
#
# Multi-arch build strategy: the builder stage is pinned to the *build*
# platform (native runner) so `go build` never runs under QEMU. Go is a
# native cross-compiler; we pass TARGETOS/TARGETARCH into the go toolchain
# and produce the right binary for each leg of the manifest.
#
# Result: a multi-arch (linux/amd64 + linux/arm64) build takes ~the same
# wall time as a single-arch build, instead of 2× with QEMU.

ARG GO_VERSION=1.25

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder
RUN apk add --no-cache ca-certificates
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
ENV CGO_ENABLED=0 GOFLAGS=-trimpath

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
        -ldflags "-s -w \
            -X github.com/Cyb3rDudu/axiom/axiom-ng/internal/version.Version=${VERSION} \
            -X github.com/Cyb3rDudu/axiom/axiom-ng/internal/version.Commit=${COMMIT} \
            -X github.com/Cyb3rDudu/axiom/axiom-ng/internal/version.Date=${DATE}" \
        -o /out/axiom-ng ./cmd/axiom-ng

# Final stage: scratch + CA certs + non-root user.
FROM scratch AS runtime
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/axiom-ng /axiom-ng

EXPOSE 8010
USER 65532:65532
ENTRYPOINT ["/axiom-ng"]
