# deploy/Dockerfile — R1. Two stages, CGO_ENABLED=0, no bash/jq/curl/
# coreutils in the runtime layer. Build context is the repo root:
#
#   docker build -f deploy/Dockerfile -t sentinel:dev \
#     --build-arg AGY_URL=<https url to the agy linux-amd64 tarball> \
#     --build-arg AGY_SHA256=<hex> \
#     .
#
# The binding spec is contracts/runtime.md R1.

# ---------------------------------------------------------------------------
# Stage 1 — builder
# ---------------------------------------------------------------------------
ARG GO_IMAGE=golang:1.25-trixie
FROM ${GO_IMAGE} AS builder

# AGY_URL/AGY_SHA256 are ops input — never a guessed URL (R1). Declared
# after FROM so they survive into this stage (build args declared before
# FROM are out of scope past it).
ARG AGY_URL
ARG AGY_SHA256
ARG AGY_VERSION=unknown
ARG VERSION=dev

WORKDIR /src

# Own layer, cached independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CI's gate re-run inside the image: a red suite fails the build (C9).
RUN test -z "$(gofmt -l .)" || (echo "ERROR: gofmt found unformatted files:" >&2; gofmt -l . >&2; exit 1)
RUN go vet ./...
RUN go test ./...

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/sentinel ./cmd/sentinel

# agy: download, verify, unpack. AGY_URL/AGY_SHA256 are required — an empty
# value fails the build with the exact message R1 specifies, never a
# guessed URL.
RUN set -e; \
    if [ -z "$AGY_URL" ]; then echo "ERROR: AGY_URL build-arg is required" >&2; exit 1; fi; \
    if [ -z "$AGY_SHA256" ]; then echo "ERROR: AGY_SHA256 build-arg is required" >&2; exit 1; fi; \
    mkdir -p /out /tmp/agy; \
    wget -q -O /tmp/agy.tar.gz "$AGY_URL"; \
    echo "${AGY_SHA256}  /tmp/agy.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/agy.tar.gz -C /tmp/agy; \
    agy_bin="$(find /tmp/agy -type f -name agy -perm -u+x | head -n1)"; \
    if [ -z "$agy_bin" ]; then agy_bin="$(find /tmp/agy -type f -name agy | head -n1)"; fi; \
    if [ -z "$agy_bin" ]; then echo "ERROR: agy binary not found in ${AGY_URL} tarball" >&2; exit 1; fi; \
    cp "$agy_bin" /out/agy; \
    chmod +x /out/agy

# ---------------------------------------------------------------------------
# Stage 2 — runtime
# ---------------------------------------------------------------------------
FROM debian:trixie-slim AS runtime

ARG AGY_VERSION=unknown
ARG VERSION=dev

LABEL org.opencontainers.image.source="https://github.com/thiscantbeserious/agentic-server-supervisor" \
      org.opencontainers.image.revision="${VERSION}" \
      org.opencontainers.image.version="${AGY_VERSION}"

# systemd provides journalctl; matches the target's Debian 13 journal
# format (ARCHITECTURE §2.7). Explicitly NOT installed: jq, curl, bash (as
# a dependency), smartmontools, zfsutils-linux — HTTP is net/http, JSON is
# encoding/json, hashing is crypto/sha256, all in the Go binary.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        systemd \
        lm-sensors \
        ca-certificates \
        tzdata && \
    rm -rf /var/lib/apt/lists/*

RUN groupadd --gid 10001 sentinel && \
    useradd --uid 10001 --gid 10001 --home-dir /home/sentinel --create-home --shell /usr/sbin/nologin sentinel

COPY --from=builder --chown=root:root --chmod=0555 /out/sentinel /usr/local/bin/sentinel
COPY --from=builder --chown=root:root --chmod=0555 /out/agy /usr/local/bin/agy

# No /opt/sentinel, no prompt or schema files — role.md, report.schema.json
# and facts.schema.json are go:embed'ed in their owning packages (C1). The
# image ships two binaries and nothing writable.

ENV LANG=C.UTF-8 \
    TZ=UTC \
    PATH=/usr/local/bin:/usr/bin:/bin

# Build-time verification — any failure fails the build.
RUN /usr/local/bin/sentinel --version && \
    /usr/local/bin/agy --version && \
    journalctl --version && \
    sensors -v

USER sentinel
WORKDIR /
ENTRYPOINT ["/usr/local/bin/sentinel"]
CMD ["tick", "--loop"]

# No HEALTHCHECK here — compose owns it (one owner, R4).
