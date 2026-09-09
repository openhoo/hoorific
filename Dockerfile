# syntax=docker/dockerfile:1

ARG HOORIFIC_CACHE_NAMESPACE=hoorific

FROM docker.io/library/golang:1.27-bookworm AS go-deps
ARG HOORIFIC_CACHE_NAMESPACE
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN --mount=type=cache,id=${HOORIFIC_CACHE_NAMESPACE}-go-mod,target=/go/pkg/mod,sharing=locked \
    go mod download

FROM go-deps AS schema
ARG HOORIFIC_CACHE_NAMESPACE
COPY tools/schema ./tools/schema
COPY internal/admin ./internal/admin
COPY internal/core ./internal/core
RUN --mount=type=cache,id=${HOORIFIC_CACHE_NAMESPACE}-go-mod,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,id=${HOORIFIC_CACHE_NAMESPACE}-go-build,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go run -trimpath ./tools/schema --output /out/admin-openapi.json

FROM docker.io/oven/bun:1.3.14 AS frontend
ARG HOORIFIC_CACHE_NAMESPACE
USER root
WORKDIR /src/web
ENV BUN_INSTALL_CACHE_DIR=/root/.bun/install/cache
COPY web/package.json web/bun.lock ./
RUN --mount=type=cache,id=${HOORIFIC_CACHE_NAMESPACE}-bun,target=/root/.bun/install/cache,sharing=locked \
    bun install --frozen-lockfile
COPY web/index.html web/tsconfig.json web/vite.config.ts ./
COPY web/src ./src
COPY --from=schema /out/admin-openapi.json /src/.artifacts/admin-openapi.json
RUN mkdir -p src/generated && bun run generate-api && bun run build

FROM docker.io/library/rust:1.95.0-alpine3.23@sha256:606fd313a0f49743ee2a7bd49a0914bab7deedb12791f3a846a34a4711db7ed2 AS native-build
ARG HOORIFIC_CACHE_NAMESPACE
WORKDIR /src
ENV OPENSSL_STATIC=1 \
    CARGO_TARGET_DIR=/src/target
RUN apk add --no-cache build-base perl
COPY native/codex-wire/Cargo.toml native/codex-wire/Cargo.lock ./native/codex-wire/
COPY native/codex-wire/src ./native/codex-wire/src
RUN --mount=type=cache,id=${HOORIFIC_CACHE_NAMESPACE}-cargo-registry,target=/usr/local/cargo/registry,sharing=locked \
    --mount=type=cache,id=${HOORIFIC_CACHE_NAMESPACE}-cargo-git,target=/usr/local/cargo/git,sharing=locked \
    --mount=type=cache,id=${HOORIFIC_CACHE_NAMESPACE}-cargo-target,target=/src/target,sharing=locked \
    cargo build --locked --release --manifest-path native/codex-wire/Cargo.toml && \
    mkdir -p /out && \
    cp target/release/hoorific-codex-wire /out/hoorific-codex-wire

FROM go-deps AS build
ARG HOORIFIC_CACHE_NAMESPACE
WORKDIR /src
COPY cmd ./cmd
COPY internal ./internal
COPY --from=frontend /src/internal/console/assets ./internal/console/assets
RUN --mount=type=cache,id=${HOORIFIC_CACHE_NAMESPACE}-go-mod,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,id=${HOORIFIC_CACHE_NAMESPACE}-go-build,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/hoorific ./cmd/hoorific

FROM docker.io/library/golang:1.27-bookworm AS runtime-files
RUN set -eux; \
    mkdir -p /runtime/etc/ssl/certs /runtime/etc/hoorific \
        /runtime/run/secrets /runtime/tmp \
        /runtime/usr/share /runtime/var/lib/hoorific; \
    cp /etc/ssl/certs/ca-certificates.crt /runtime/etc/ssl/certs/ca-certificates.crt; \
    cp -a /usr/share/zoneinfo /runtime/usr/share/zoneinfo; \
    printf '%s\n' \
        'root:x:0:0:root:/root:/sbin/nologin' \
        'hoorific:x:10001:10001:Hoorific:/nonexistent:/sbin/nologin' \
        > /runtime/etc/passwd; \
    printf '%s\n' \
        'root:x:0:' \
        'hoorific:x:10001:' \
        > /runtime/etc/group; \
    chown 10001:10001 /runtime/var/lib/hoorific; \
    chmod 0755 /runtime/var/lib/hoorific; \
    chmod 1777 /runtime/tmp

FROM scratch AS runtime
COPY --from=runtime-files /runtime/ /
COPY --from=build /out/hoorific /usr/local/bin/hoorific
COPY --from=native-build /out/hoorific-codex-wire /usr/local/bin/hoorific-codex-wire
USER 10001:10001
ENV TZ=UTC
ENTRYPOINT ["/usr/local/bin/hoorific"]
