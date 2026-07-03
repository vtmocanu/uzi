# syntax=docker/dockerfile:1

# --- build stage ---
FROM golang:1.26@sha256:f96cc555eb8db430159a3aa6797cd5bae561945b7b0fe7d0e284c63a3b291609 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static, VCS-stamp-free build. Migrations are embedded via go:embed, so the
# runtime image needs nothing but the binary.
RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -ldflags="-s -w" -o /out/server ./cmd/server

# --- runtime stage ---
FROM gcr.io/distroless/static-debian12:nonroot@sha256:d093aa3e30dbadd3efe1310db061a14da60299baff8450a17fe0ccc514a16639
COPY --from=build /out/server /server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]
