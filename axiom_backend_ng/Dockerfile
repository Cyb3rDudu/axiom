# syntax=docker/dockerfile:1.7
# Multi-stage build: static Go binary, then minimal scratch image with CA certs.

ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
ENV CGO_ENABLED=0 GOFLAGS=-trimpath

RUN go build \
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
