# deploy/Dockerfile — R1. Two stages, CGO_ENABLED=0. Sentinel installs
# nothing beyond the packages listed in stage 2 and executes only
# journalctl, sensors -j and agy — but the debian:trixie-slim base still
# ships /bin/bash and coreutils (measured; contracts/runtime.md R1 was
# corrected to say so, f95693c). Build context is the repo root:
#
#   docker build -f deploy/Dockerfile -t sentinel:dev \
#     $(deploy/agy-build-args.sh) \
#     .
#
# deploy/agy-build-args.sh resolves the current AGY_URL/AGY_SHA512/
# AGY_VERSION triple from the vendor's own manifest (never hand-assembled,
# never guessed) — see R1 for why the vendor installer itself is not run
# at build time instead.
#
# The binding spec is contracts/runtime.md R1.

# ---------------------------------------------------------------------------
# Stage 1 — builder
# ---------------------------------------------------------------------------
ARG GO_IMAGE=golang:1.25-trixie
FROM ${GO_IMAGE} AS builder

# AGY_URL/AGY_SHA512 are ops input, resolved from the vendor manifest via
# deploy/agy-build-args.sh — never a guessed URL, never a self-computed
# digest (R1: the vendor manifest publishes sha512, not sha256; pinning
# OUR OWN sha256 of whatever we downloaded proves nothing about what the
# vendor published). Declared after FROM so they survive into this stage
# (build args declared before FROM are out of scope past it).
ARG AGY_URL
ARG AGY_SHA512
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

# GOARCH is hardcoded, not a build knob. Not because agy lacks an arm64
# artifact — it does not: the vendor also publishes
# manifests/linux_arm64.json, a distinct tarball with its own sha512.
# The reason is narrower: this image targets bam, which is amd64; CI
# pins platforms: linux/amd64 (R6); and AGY_URL/AGY_SHA512 are a SINGLE
# pinned pair resolved for one architecture at a time via
# deploy/agy-build-args.sh. A multi-arch image would need the agy URL
# and digest resolved per target (manifests/linux_<arch>.json, one
# extra --build-arg pair per platform) — the vendor supports it, this
# project has not been asked to, and pinning one architecture because
# one artifact is resolved is a different, correct claim from pinning
# it because no alternative exists.
#
# A round-3 review made this a TARGETARCH-driven ARG on the (at the
# time reasonable, later-disproven) theory that hardcoding amd64 would
# break a hypothetical arm64 build. The ARG did not enable multi-arch
# support — nothing here resolves an arch-specific AGY_URL/AGY_SHA512 —
# it just silently decoupled stage 1's cross-compile target from stage
# 2's --platform pin. Verified by round-4 review: `docker build
# --platform linux/arm64` succeeded, producing an arm64 Go binary in an
# amd64-userland, arm64-labeled image, and the build-time
# `sentinel --version` check could not catch it — an arm64 host
# executes an arm64 static ELF regardless of the image's declared
# platform (platform is manifest metadata, not an execution sandbox).
# That image could not exec on a real amd64 host. Deletion is the fix;
# C1's architecture assertion below is what would have caught it.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/sentinel ./cmd/sentinel

# agy: download, verify, unpack. AGY_URL/AGY_SHA512 are required — an empty
# value fails the build with the exact message R1 specifies, never a
# guessed URL. The tarball does NOT contain a file literally named "agy"
# (contracts/runtime.md R1, amended 7547238) — the real artifact (verified
# against version 1.1.15) is a single ELF executable named "antigravity";
# the vendor's own installer is what renames it to "agy" on install. So
# this locates THE executable in the archive by permission bit, never by
# a hardcoded name (a future vendor rename must not break this build
# again), and fails loudly if that search is ambiguous — zero matches or
# more than one — rather than silently picking the wrong file.
RUN set -e; \
    if [ -z "$AGY_URL" ]; then echo "ERROR: AGY_URL build-arg is required" >&2; exit 1; fi; \
    if [ -z "$AGY_SHA512" ]; then echo "ERROR: AGY_SHA512 build-arg is required" >&2; exit 1; fi; \
    mkdir -p /out /tmp/agy; \
    wget -q -O /tmp/agy.tar.gz "$AGY_URL"; \
    echo "${AGY_SHA512}  /tmp/agy.tar.gz" | sha512sum -c -; \
    tar -xzf /tmp/agy.tar.gz -C /tmp/agy; \
    agy_candidates="$(find /tmp/agy -type f -perm -u+x)"; \
    agy_count="$(printf '%s\n' "$agy_candidates" | grep -c . || true)"; \
    if [ "$agy_count" -eq 0 ]; then \
      echo "ERROR: no executable file found in the ${AGY_URL} tarball" >&2; exit 1; \
    fi; \
    if [ "$agy_count" -gt 1 ]; then \
      echo "ERROR: ${agy_count} executable files found in the ${AGY_URL} tarball, expected exactly 1:" >&2; \
      printf '%s\n' "$agy_candidates" >&2; \
      exit 1; \
    fi; \
    cp "$agy_candidates" /out/agy; \
    chmod +x /out/agy

# ---------------------------------------------------------------------------
# Stage 2 — runtime
# ---------------------------------------------------------------------------
# Pinned to linux/amd64 explicitly (not left to the build host's native
# arch): this image targets bam (amd64) and is built from a SINGLE
# resolved AGY_URL/AGY_SHA512 pair (deploy/agy-build-args.sh resolves
# one architecture per run) — not because the vendor lacks an arm64
# artifact (it does not, see the GOARCH comment above). This stage is
# where agy actually EXECUTES (the build-time verification below runs
# it directly, not just copies it), so it must match the architecture
# the pinned pair was resolved for, regardless of what machine runs
# `docker build`. Stage 1 deliberately
# does NOT get this pin: it only needs to cross-compile a static Go
# binary (GOARCH=amd64, hardcoded — see that RUN step's comment), and
# forcing stage 1 to amd64 too would put the ENTIRE go vet/test suite
# under qemu-user emulation on a non-amd64 build host — measured:
# 5-10x slower, with no correctness benefit since nothing in stage 1
# executes target-architecture code.
FROM --platform=linux/amd64 debian:trixie-slim AS runtime

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
