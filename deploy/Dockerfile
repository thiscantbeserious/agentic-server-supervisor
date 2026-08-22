# deploy/Dockerfile — R1. Two stages, CGO_ENABLED=0. Sentinel installs
# nothing beyond the packages listed in stage 2 and executes only
# journalctl, sensors -j and agy — but the debian:trixie-slim base still
# ships /bin/bash and coreutils (measured — contracts/runtime.md R1
# states this precisely). Build context is the repo root:
#
#   docker buildx build -f deploy/Dockerfile -t sentinel:dev \
#     --platform linux/amd64,linux/arm64 \
#     $(deploy/agy-build-args.sh) \
#     .
#
# deploy/agy-build-args.sh resolves the current per-architecture AGY_URL/
# AGY_SHA512 pairs (one for amd64, one for arm64) plus AGY_VERSION from
# the vendor's own manifest (never hand-assembled, never guessed) — see
# R1 for why the vendor installer itself is not run at build time
# instead. The build itself resolves nothing from the network: both
# pairs are ops input, and the one matching TARGETARCH (which buildx
# supplies per target) is selected inside the build.
#
# The binding spec is contracts/runtime.md R1.

# ---------------------------------------------------------------------------
# Stage 1 — builder
# ---------------------------------------------------------------------------
ARG GO_IMAGE=golang:1.25-trixie
# --platform=$BUILDPLATFORM (a buildx-supplied automatic ARG: the
# architecture actually running `docker build`) pins this stage to build
# NATIVELY regardless of which target(s) buildx is asked for. Without it,
# `--platform linux/amd64,linux/arm64` runs this ENTIRE stage twice — once
# per target — so the non-native pass runs gofmt/go vet/go test ./... (and
# later the agy download + sha512sum + tar over ~205MB) under qemu-user
# emulation: measured 5-10x slower, on every push, plus a real history of
# `go test` timing flakiness under emulation that would land as
# intermittent red CI blamed on the tests rather than on qemu.
# CGO_ENABLED=0 already makes cross-compilation free, so building natively
# here costs nothing — GOARCH=${TARGETARCH} below still produces the
# correct target binary regardless of what this stage itself runs on.
#
# The distinction that matters here is which stage a platform pin sits
# on, not just that one exists. A CONSTANT platform on a stage that
# EXECUTES target-architecture code (agy, sentinel, in the runtime
# stage) produces a userland that disagrees with the binaries inside it
# whenever the constant doesn't match the actual target. A VARIABLE like
# $BUILDPLATFORM, always equal to the build host's own architecture, on
# a stage that only COMPILES and runs this repo's own tests — never the
# target binaries — decides WHERE compilation happens, not what
# architecture the output is; GOARCH=${TARGETARCH} alone decides that,
# unchanged either way. Do not delete this pin by pattern-matching it
# against a platform-pinning mistake elsewhere: the two are the opposite
# fix, on the opposite kind of stage.
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS builder

# One URL/digest pair PER ARCHITECTURE, resolved from the vendor manifest
# via deploy/agy-build-args.sh — never a guessed URL, never a
# self-computed digest (R1: the vendor manifest publishes sha512, not
# sha256; pinning OUR OWN sha256 of whatever we downloaded proves
# nothing about what the vendor published). The one matching TARGETARCH
# is selected below — this keeps the pin (the operator supplies exact
# URLs and vendor digests for BOTH architectures; the build resolves
# nothing from the network) while producing a correct image for
# whichever target buildx is building. Declared after FROM so they
# survive into this stage (build args declared before FROM are out of
# scope past it).
ARG AGY_URL_AMD64
ARG AGY_SHA512_AMD64
ARG AGY_URL_ARM64
ARG AGY_SHA512_ARM64
ARG AGY_VERSION=unknown
ARG VERSION=dev
# TARGETARCH is a predefined buildx arg: the architecture actually being
# built (amd64/arm64), independent of the host running `docker build`.
# Declared WITHOUT a default, deliberately: giving it a literal default
# (`=amd64`) stops BuildKit from auto-populating it with the real target
# — measured against a real GHCR push, where every platform silently
# built as amd64 because of exactly that default. An undefaulted ARG
# with this exact name is what BuildKit auto-populates; anything else
# makes it an ordinary build arg that just happens to share the name.
ARG TARGETARCH

WORKDIR /src

# Own layer, cached independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CI's gate re-run inside the image: a red suite fails the build (C9).
RUN test -z "$(gofmt -l .)" || (echo "ERROR: gofmt found unformatted files:" >&2; gofmt -l . >&2; exit 1)
RUN go vet ./...
RUN go test ./...

# GOARCH follows TARGETARCH — the vendor ships arm64, so hardcoding
# amd64 here silently breaks a real arm64 build. Doing this correctly
# also requires the runtime stage (below) to NOT pin a platform: a
# TARGETARCH-following stage 1 paired with a stage 2 frozen at
# --platform=linux/amd64 produces an arm64 Go binary inside an
# amd64-userland, amd64-labeled runtime image, and the build-time
# `sentinel --version` check cannot catch it — an arm64 host executes an
# arm64 static ELF regardless of the image's declared platform. The safe
# combination is BOTH stages following TARGETARCH consistently — see the
# runtime stage's FROM line, which does not pin a platform either. With
# both stages tracking the same target, this incoherence cannot arise.
# C1's coherence assertion (Go binary ELF machine, agy ELF machine, and
# the image manifest architecture all agreeing with the requested
# platform) is what verifies this held, for both targets, on every
# build.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/sentinel ./cmd/sentinel

# agy: select the URL/digest pair matching TARGETARCH, download, verify,
# unpack. An empty value for the SELECTED pair fails the build naming
# that pair specifically (e.g. "AGY_URL_ARM64"), never a guessed URL.
# The tarball does NOT contain a file literally named "agy" — the real
# artifact (verified against version 1.1.15, both architectures) is a
# single ELF executable named "antigravity" on amd64 and on arm64; the
# vendor's own installer is what renames it to "agy" on install. So this
# locates THE executable in the archive by permission bit, never by a
# hardcoded name (a future vendor rename must not break this build
# again), and fails loudly if that search is ambiguous — zero matches or
# more than one — rather than silently picking the wrong file.
RUN set -e; \
    case "$TARGETARCH" in \
      amd64) AGY_URL="$AGY_URL_AMD64"; AGY_SHA512="$AGY_SHA512_AMD64"; ARCH_TAG=AMD64 ;; \
      arm64) AGY_URL="$AGY_URL_ARM64"; AGY_SHA512="$AGY_SHA512_ARM64"; ARCH_TAG=ARM64 ;; \
      *) echo "ERROR: unsupported TARGETARCH: $TARGETARCH (agy ships amd64 and arm64 only)" >&2; exit 1 ;; \
    esac; \
    if [ -z "$AGY_URL" ]; then echo "ERROR: AGY_URL_${ARCH_TAG} build-arg is required" >&2; exit 1; fi; \
    if [ -z "$AGY_SHA512" ]; then echo "ERROR: AGY_SHA512_${ARCH_TAG} build-arg is required" >&2; exit 1; fi; \
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
# NO --platform pin here, deliberately: buildx sets the platform per
# target it is building (linux/amd64 and linux/arm64, R6), and this
# stage is where agy actually EXECUTES (the build-time verification
# below runs it directly, not just copies it), so it must track
# whatever target is being built, not a hardcoded one. A CONSTANT
# platform here, combined with stage 1 following TARGETARCH, produces
# exactly the three-way mismatch this file exists to prevent: a Go
# binary cross-compiled for one architecture in stage 1, copied into a
# stage 2 frozen at another. Both stages must follow TARGETARCH — stage
# 1 via GOARCH=${TARGETARCH} (see that RUN step), stage 2 by simply not
# overriding the platform buildx already selected for it. Do not
# reintroduce a platform constant on either stage.
FROM debian:trixie-slim AS runtime

ARG AGY_VERSION=unknown
ARG VERSION=dev
# TARGETARCH is buildx's predefined arg for the platform actually being
# built; re-declared here because an ARG's scope ends at the FROM that
# follows its declaration (the builder stage's own TARGETARCH, above,
# does not carry over) and the ELF check below needs to know which
# machine type agy is supposed to be. No default — see the builder
# stage's TARGETARCH comment for why one is never safe here.
ARG TARGETARCH
# TARGETPLATFORM (e.g. "linux/arm64") is a SEPARATE buildx-injected arg
# from TARGETARCH, never touched by the selection logic in the builder
# stage. Declared here so the verification below can derive its
# expectation from a source independent of the one that chose the
# binaries — see that RUN step for why this independence is the point.
ARG TARGETPLATFORM

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

# /state exists in the IMAGE, owned by the user the container runs as, even
# though the compose file mounts a volume over it. That is the entire point:
# Docker initialises a fresh named volume from the image's directory, taking
# its ownership and mode with it. Without this the mountpoint does not exist
# in the image, so the volume is created root-owned, the container runs as
# 10001, and every tick fails with "state dir not writable" on a filesystem
# nothing in the container can chown, because the rest of it is read_only.
#
# It cost a full deployment to learn: the supervisor ran for twenty minutes
# on the target host without completing a single tick.
RUN mkdir -p /state && chown 10001:10001 /state && chmod 0700 /state

COPY --from=builder --chown=root:root --chmod=0555 /out/sentinel /usr/local/bin/sentinel
COPY --from=builder --chown=root:root --chmod=0555 /out/agy /usr/local/bin/agy

# No /opt/sentinel, no prompt or schema files — role.md, report.schema.json
# and facts.schema.json are go:embed'ed in their owning packages (C1). The
# image ships two binaries and nothing writable.

ENV LANG=C.UTF-8 \
    TZ=UTC \
    PATH=/usr/local/bin:/usr/bin:/bin

# Build-time verification — any failure fails the build.
#
# agy is NOT executed here, deliberately. It is a dynamically linked ELF
# (interpreter /lib64/ld-linux-*.so.*), and on a non-native pass (e.g. an
# arm64 build on an amd64 CI runner) qemu-user cannot resolve that
# interpreter, failing with exit 127 — a missing-interpreter symptom that
# looks like a broken artifact but means only "this host is emulating".
# sentinel is a static Go binary, which qemu-user runs natively under
# emulation with no such gap, so `sentinel --version` still executes it
# for real. journalctl and sensors are this stage's OWN architecture,
# native either way, so running them proves nothing about TARGETARCH.
#
# What actually needs proving — both binaries are the RIGHT architecture,
# not that they can be executed on whatever host happens to be building
# this image — is read, not run: their ELF e_machine field (byte offset
# 18, little-endian uint16), the same check C1 performs against a
# running container. A read cannot be defeated by missing emulation
# support, so it is strictly the better check here, not just the
# emulation-safe one.
#
# The expectation is derived from TARGETPLATFORM, never from TARGETARCH
# — TARGETARCH is also what the builder stage used to CHOOSE which
# binaries to embed, so a check that reused it would only prove the
# artifact agrees with itself: whatever TARGETARCH is, right or wrong,
# both the selection and the check would agree, and this exact class of
# bug — TARGETARCH silently wrong on every platform, both stages seeing
# the same wrong value — shipped to GHCR undetected because of exactly
# that. TARGETPLATFORM is populated by BuildKit independently of
# TARGETARCH, so a real ELF read against IT catches a wrong TARGETARCH
# instead of rubber-stamping it. TARGETARCH is still cross-checked
# against it below, so the two are asserted to agree rather than assumed
# to.
RUN /usr/local/bin/sentinel --version && \
    journalctl --version && \
    sensors -v && \
    if [ -z "$TARGETPLATFORM" ]; then \
      echo "ERROR: TARGETPLATFORM is unset — buildx did not inject it" >&2; exit 1; \
    fi && \
    case "$TARGETPLATFORM" in \
      linux/amd64) want="3e 00"; want_arch=amd64 ;; \
      linux/arm64) want="b7 00"; want_arch=arm64 ;; \
      *) echo "ERROR: unsupported TARGETPLATFORM: $TARGETPLATFORM (expected linux/amd64 or linux/arm64)" >&2; exit 1 ;; \
    esac && \
    if [ "$TARGETARCH" != "$want_arch" ]; then \
      echo "ERROR: TARGETARCH ($TARGETARCH) disagrees with TARGETPLATFORM ($TARGETPLATFORM) — these must never diverge" >&2; \
      exit 1; \
    fi && \
    got_sentinel="$(od -An -tx1 -j18 -N2 /usr/local/bin/sentinel | tr -s ' \t' ' ' | sed -e 's/^ *//' -e 's/ *$//')" && \
    if [ "$got_sentinel" != "$want" ]; then \
      echo "ERROR: sentinel ELF e_machine = [$got_sentinel], want [$want] for TARGETPLATFORM=$TARGETPLATFORM" >&2; \
      exit 1; \
    fi && \
    got_agy="$(od -An -tx1 -j18 -N2 /usr/local/bin/agy | tr -s ' \t' ' ' | sed -e 's/^ *//' -e 's/ *$//')" && \
    if [ "$got_agy" != "$want" ]; then \
      echo "ERROR: agy ELF e_machine = [$got_agy], want [$want] for TARGETPLATFORM=$TARGETPLATFORM" >&2; \
      exit 1; \
    fi

USER sentinel
WORKDIR /
ENTRYPOINT ["/usr/local/bin/sentinel"]
CMD ["tick", "--loop"]

# No HEALTHCHECK here — compose owns it (one owner, R4).
