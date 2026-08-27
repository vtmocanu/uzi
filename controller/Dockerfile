# syntax=docker/dockerfile:1

# The hosted-worker controller (PRD #58). Build context is controller/ — this
# module has no dependency on the api's source, only on its wire contract, so the
# context stays this directory (the same shape api/Dockerfile has).

# --- build stage ---
FROM golang:1.26@sha256:26326682769ca980f8f1d3b1f52be2dd1c1d25270e3de3fe0c97d6bb65df3556 AS build
WORKDIR /src

# M3 brought in the kube client, so the go.sum + `go mod download` this comment
# previously deferred are now here — copied before the source so the dependency
# layer caches independently of a code change (the same shape api/Dockerfile has).
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static, VCS-stamp-free build, matching api/Dockerfile.
RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -ldflags="-s -w" -o /out/controller ./cmd/controller

# --- runtime stage ---
FROM gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a
# Links this package to the repository on the registry (GHCR reads this label).
LABEL org.opencontainers.image.source="https://github.com/vtmocanu/uzi"
COPY --from=build /out/controller /controller
# No EXPOSE: the controller listens on nothing. Every exchange is outbound to the
# api (Decision 2), which is what lets it run with no inbound surface at all.
USER nonroot:nonroot
ENTRYPOINT ["/controller"]
