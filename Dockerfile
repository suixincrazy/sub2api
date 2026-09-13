# syntax=docker/dockerfile:1.7
# =============================================================================
# Sub2API Xray Multi-Stage Dockerfile
# =============================================================================
# Stage 1: Build frontend
# Stage 2: Build Go backend with embedded frontend
# Stage 3: Final minimal image
# =============================================================================

ARG NODE_IMAGE=node:24-alpine
ARG GOLANG_IMAGE=golang:1.27.0-alpine
ARG ALPINE_IMAGE=alpine:3.21
ARG POSTGRES_IMAGE=postgres:18-alpine
ARG XRAY_IMAGE=ghcr.io/xtls/xray-core:26.3.27
ARG SING_BOX_VERSION=1.13.14
ARG GOPROXY=https://goproxy.cn,direct
ARG GOSUMDB=sum.golang.google.cn
ARG NPM_CONFIG_REGISTRY=

# -----------------------------------------------------------------------------
# Stage 1: Frontend Builder
# -----------------------------------------------------------------------------
# --platform=$BUILDPLATFORM: the frontend output is JS (arch-neutral), so build
# it on the native host arch instead of under QEMU emulation for the target.
FROM --platform=${BUILDPLATFORM} ${NODE_IMAGE} AS frontend-builder
ARG NPM_CONFIG_REGISTRY

WORKDIR /app/frontend

# Keep the builder aligned with the lockfile generator.
RUN corepack enable && corepack prepare pnpm@9.15.9 --activate

# Install dependencies first (better caching)
COPY frontend/package.json frontend/pnpm-lock.yaml frontend/pnpm-workspace.yaml ./
RUN --mount=type=cache,id=sub2api-pnpm-store,target=/root/.local/share/pnpm/store \
    if [ -n "${NPM_CONFIG_REGISTRY}" ]; then pnpm config set registry "${NPM_CONFIG_REGISTRY}"; fi && \
    pnpm install --frozen-lockfile --prefer-offline

# Copy frontend source and build.
# LegalDocumentView.vue (admin-compliance gate) build-time imports
# ../../../../docs/legal/*.md?raw, so docs/legal/ must sit beside frontend/
# in the image (WORKDIR /app/frontend -> resolves to /app/docs/legal/*.md).
# Copy only that subtree to keep the build dependency minimal.
COPY frontend/ ./
COPY docs/legal/ /app/docs/legal/
RUN pnpm run build

# -----------------------------------------------------------------------------
# Stage 2: Backend Builder
# -----------------------------------------------------------------------------
# --platform=$BUILDPLATFORM: run the Go toolchain on the native host arch and
# cross-compile to the target arch below. The binary is CGO_ENABLED=0, so this
# is a clean pure-Go cross-compile — no QEMU emulation of go mod download / go
# build (emulated networking here was dropping module fetches with EOF).
FROM --platform=${BUILDPLATFORM} ${GOLANG_IMAGE} AS backend-builder

# Build arguments for version info (set by CI)
ARG VERSION=
ARG COMMIT=docker
ARG DATE
ARG GOPROXY
ARG GOSUMDB
# Populated by buildx from the --platform target (e.g. linux/amd64).
ARG TARGETOS
ARG TARGETARCH

ENV GOPROXY=${GOPROXY}
ENV GOSUMDB=${GOSUMDB}

# Install build dependencies
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /app/backend

# Copy go mod files first (better caching)
COPY backend/go.mod backend/go.sum ./
# Cache mount keeps the module cache across builds so a transient CDN blip on
# retry resumes instead of re-fetching every zip from scratch.
RUN --mount=type=cache,id=sub2api-gomod,target=/go/pkg/mod \
    go mod download

# Copy backend source first
COPY backend/ ./

# Copy frontend dist from previous stage (must be after backend copy to avoid being overwritten)
COPY --from=frontend-builder /app/backend/internal/web/dist ./internal/web/dist

# Build the binary (BuildType=release for CI builds, embed frontend)
# Version precedence: build arg VERSION > exact git tag > cmd/server/VERSION
RUN --mount=type=cache,id=sub2api-gomod,target=/go/pkg/mod \
    --mount=type=cache,id=sub2api-gobuild,target=/root/.cache/go-build \
    VERSION_VALUE="${VERSION}" && \
    if [ -z "${VERSION_VALUE}" ]; then VERSION_VALUE="$(./scripts/resolve-version.sh)"; fi && \
    DATE_VALUE="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -tags embed \
    -ldflags="-s -w -X main.Version=${VERSION_VALUE} -X main.Commit=${COMMIT} -X main.Date=${DATE_VALUE} -X main.BuildType=release" \
    -trimpath \
    -o /app/sub2api \
    ./cmd/server

# -----------------------------------------------------------------------------
# Stage 3: PostgreSQL Client (version-matched with docker-compose)
# -----------------------------------------------------------------------------
FROM ${POSTGRES_IMAGE} AS pg-client

# -----------------------------------------------------------------------------
# Stage 4: Xray Runtime
# -----------------------------------------------------------------------------
FROM ${XRAY_IMAGE} AS xray-runtime

# -----------------------------------------------------------------------------
# Stage 5: Sing-box Runtime
# -----------------------------------------------------------------------------
FROM ${ALPINE_IMAGE} AS sing-box-runtime
ARG SING_BOX_VERSION
ARG TARGETARCH
ARG TARGETVARIANT
RUN apk add --no-cache ca-certificates curl tar && \
    case "${TARGETARCH}${TARGETVARIANT}" in \
      amd64) SING_ARCH=amd64; SING_SHA256=d5b46de6498427bccfeb87dbafcde4dbefdfe35680020d07d286ad915f0bfb34 ;; \
      arm64) SING_ARCH=arm64; SING_SHA256=edec18488af35a93cf8b362063146fdd7b557ef9862710ee77a1f4adb5c70118 ;; \
      armv7) SING_ARCH=armv7; SING_SHA256=4d0f9fefd95734c1e9208382a3476c67b54438435e9693bf78b627a69b0ded29 ;; \
      386) SING_ARCH=386; SING_SHA256=0a9a25a91be0c9178224a9419c515d8ab919af8a848935238fb15e819917d262 ;; \
      *) echo "unsupported sing-box architecture: ${TARGETARCH}${TARGETVARIANT}" >&2; exit 1 ;; \
    esac && \
    mkdir -p /opt/sing-box && \
    curl -fL --retry 5 --retry-delay 3 --retry-all-errors --connect-timeout 20 \
      -o /tmp/sing-box.tar.gz \
      "https://github.com/SagerNet/sing-box/releases/download/v${SING_BOX_VERSION}/sing-box-${SING_BOX_VERSION}-linux-${SING_ARCH}-musl.tar.gz" && \
    echo "${SING_SHA256}  /tmp/sing-box.tar.gz" | sha256sum -c - && \
    tar -xzf /tmp/sing-box.tar.gz -C /opt/sing-box --strip-components=1 && \
    rm -f /tmp/sing-box.tar.gz && \
    test -x /opt/sing-box/sing-box

# -----------------------------------------------------------------------------
# Stage 6: Final Runtime Image
# -----------------------------------------------------------------------------
FROM ${ALPINE_IMAGE}

# Labels
LABEL maintainer="smmooooonn <github.com/smmooooonn>"
LABEL description="Sub2API Xray - AI API Gateway Platform"
LABEL org.opencontainers.image.source="https://github.com/smmooooonn/sub2api-xray"

# Install runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    su-exec \
    libpq \
    zstd-libs \
    lz4-libs \
    krb5-libs \
    libldap \
    libedit \
    && rm -rf /var/cache/apk/*

# Copy pg_dump and psql from the same postgres image used in docker-compose
# This ensures version consistency between backup tools and the database server
COPY --from=pg-client /usr/local/bin/pg_dump /usr/local/bin/pg_dump
COPY --from=pg-client /usr/local/bin/psql /usr/local/bin/psql
COPY --from=pg-client /usr/local/lib/libpq.so.5* /usr/local/lib/
COPY --from=xray-runtime /usr/local/bin/xray /usr/local/bin/xray
COPY --from=sing-box-runtime /opt/sing-box/sing-box /usr/local/bin/sing-box

# Create non-root user
RUN addgroup -g 1000 sub2api && \
    adduser -u 1000 -G sub2api -s /bin/sh -D sub2api

# Set working directory
WORKDIR /app

ENV XRAY_BIN=/usr/local/bin/xray \
    XRAY_WORK_DIR=/app/data/xray \
    XRAY_MAX_INSTANCES=64 \
    XRAY_MAX_INSTANCES_PER_USER=16 \
    XRAY_RUNTIME_IDLE_TTL=15m \
    SING_BOX_BIN=/usr/local/bin/sing-box \
    SING_BOX_WORK_DIR=/app/data/sing-box \
    SING_BOX_MAX_INSTANCES=64 \
    SING_BOX_MAX_INSTANCES_PER_USER=16 \
    SING_BOX_RUNTIME_IDLE_TTL=15m

# Copy binary/resources with ownership to avoid extra full-layer chown copy
COPY --from=backend-builder --chown=sub2api:sub2api /app/sub2api /app/sub2api
COPY --from=backend-builder --chown=sub2api:sub2api /app/backend/resources /app/resources

# The updater creates a same-directory temp file and atomically replaces
# /app/sub2api, so the non-root runtime user must own the application directory.
RUN mkdir -p /app/data && chown -R sub2api:sub2api /app

# Copy entrypoint script (fixes volume permissions then drops to sub2api)
COPY deploy/docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh

# Expose port (can be overridden by SERVER_PORT env var)
EXPOSE 8080

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=10s --retries=3 \
    CMD wget -q -T 5 -O /dev/null http://localhost:${SERVER_PORT:-8080}/health || exit 1

# Run the application (entrypoint fixes /app/data ownership then execs as sub2api)
ENTRYPOINT ["/app/docker-entrypoint.sh"]
CMD ["/app/sub2api"]
