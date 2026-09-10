# syntax=docker/dockerfile:1
#
# PRD #1171 m6 (item 9) — a throwaway container image for api/cmd/codexm3btestserver, the
# minimal REAL codex worker-route API test server. run-lifecycle.sh builds it and runs it on an
# INTERNAL (no-egress) docker network so the packaged worker container can drive the REAL
# WorkerClient Bearer release/refresh routes against a migrated throwaway Postgres.
#
# BUILD CONTEXT is the repo's api/ module (the same context api/Dockerfile uses), so the whole
# module — including the go:embed'd internal/store/migrations the server runs via store.Migrate —
# is present. It ships NO production behavior and no real credential: every seeded value is a
# canary and the codex refresh client is an in-process fake (see cmd/codexm3btestserver/main.go).
#
# The base digest is pinned to match api/Dockerfile's golang:1.26 so the two build with the same
# toolchain. -buildvcs=false: the build context is api/ with no .git to stamp from.

# --- build stage ---
FROM golang:1.26@sha256:26326682769ca980f8f1d3b1f52be2dd1c1d25270e3de3fe0c97d6bb65df3556 AS build
WORKDIR /src

COPY go.mod go.sum ./
# Retry the module fetch: proxy.golang.org blips transiently and a single INTERNAL_ERROR should
# not redden the packaged proof build (mirrors api/Dockerfile). 5 attempts, n*3s backoff.
RUN n=0; until go mod download; do n=$((n+1)); [ "$n" -ge 5 ] && { echo "go mod download: giving up after $n attempts" >&2; exit 1; }; echo "go mod download failed (attempt $n/5); retrying in $((n*3))s" >&2; sleep "$((n*3))"; done

COPY . .
# Static, VCS-stamp-free build; migrations are embedded via go:embed, so the runtime image needs
# nothing but the binary.
RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -ldflags="-s -w" -o /out/codexm3btestserver ./cmd/codexm3btestserver

# --- runtime stage ---
FROM gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a
LABEL org.opencontainers.image.source="https://github.com/vtmocanu/uzi"
COPY --from=build /out/codexm3btestserver /codexm3btestserver
# The server reads its DSN + bind/advertise from env (UZI_TEST_DATABASE_URL, CODEX_M3B_LISTEN,
# CODEX_M3B_ADVERTISE_URL) run-lifecycle.sh passes; it prints ONE JSON contract line on stdout.
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/codexm3btestserver"]
