# syntax=docker/dockerfile:1

# --- build stage ---
FROM golang:1.26@sha256:f96cc555eb8db430159a3aa6797cd5bae561945b7b0fe7d0e284c63a3b291609 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# UZI_VERSION stamps main.version so GET /api/version reports the release version.
# CI passes the git tag; the ${UZI_VERSION#v} strip drops a leading v so the value
# equals the v-stripped coordinate the rest of Model B uses (chart appVersion ==
# image tag == VERSION, all bare), whether the tag is vX.Y.Z or bare X.Y.Z. An
# unset arg leaves the "dev" default, which is what a local `docker compose build`
# gets (${dev#v} == dev).
ARG UZI_VERSION=dev
# UZI_COMMIT / UZI_BUILT_AT / UZI_COMMIT_COUNT are the rest of the build info GET
# /api/version reports (PRD #175): the full 40-char source SHA, the build time as
# RFC3339 UTC, and the number of commits behind that SHA. All three default EMPTY and
# stay empty on a local `docker compose build` and on the MR/main validation build —
# only the tag publish job passes them. An empty stamp is omitted from the response,
# so a dev build says "unknown" rather than lying.
#
# built_at is passed as a build ARG rather than computed with $(date) inside a RUN,
# which is what agent/templates/base/Dockerfile does for its BUILD_INFO file. That
# trade is fine there and wrong here: a $(date) baked into a layer reports the CACHED
# layer's date when the layer is reused, and for a field named built_at that is a lie.
#
# UZI_COMMIT_COUNT is not computable here at all: the publish build context is api/
# with no .git in it, and the kaniko image has no git either. CI computes it in
# publish:assert-changelog and hands it over as a dotenv report (PRD #175 M3).
ARG UZI_COMMIT=
ARG UZI_BUILT_AT=
ARG UZI_COMMIT_COUNT=
# Static, VCS-stamp-free build. Migrations are embedded via go:embed, so the
# runtime image needs nothing but the binary. -buildvcs=false stays: the publish
# context is api/ with no .git in it (nothing to stamp from), and a VCS-free build
# is the deliberate reproducibility choice — which is why these coordinates arrive
# as ldflags rather than from debug.ReadBuildInfo().
RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -ldflags="-s -w -X main.version=${UZI_VERSION#v} -X main.commit=${UZI_COMMIT} -X main.builtAt=${UZI_BUILT_AT} -X main.commits=${UZI_COMMIT_COUNT}" -o /out/server ./cmd/server

# --- runtime stage ---
FROM gcr.io/distroless/static-debian12:nonroot@sha256:d093aa3e30dbadd3efe1310db061a14da60299baff8450a17fe0ccc514a16639
COPY --from=build /out/server /server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]
