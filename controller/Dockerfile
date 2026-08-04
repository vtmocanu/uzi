# syntax=docker/dockerfile:1

# The hosted-worker controller (PRD #58). Build context is controller/ — this
# module has no dependency on the api's source, only on its wire contract, so the
# context stays this directory (the same shape api/Dockerfile has).

# --- build stage ---
FROM golang:1.26@sha256:3aff6657219a4d9c14e27fb1d8976c49c29fddb70ba835014f477e1c70636647 AS build
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
FROM gcr.io/distroless/static-debian12:nonroot@sha256:d093aa3e30dbadd3efe1310db061a14da60299baff8450a17fe0ccc514a16639
COPY --from=build /out/controller /controller
# No EXPOSE: the controller listens on nothing. Every exchange is outbound to the
# api (Decision 2), which is what lets it run with no inbound surface at all.
USER nonroot:nonroot
ENTRYPOINT ["/controller"]
