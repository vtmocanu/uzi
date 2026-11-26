# syntax=docker/dockerfile:1

# The example runtime web module as an image (PRD #4686): a node build stage plus a static
# nginx that serves the build at /modules/example/.
#
# NOT SHIPPED. This image is an acceptance fixture for the host: no uzi image, chart or
# workflow builds or publishes it. It exists so the module can be run as its own container
# (docker-compose.modules.yml, and a Deployment in a cluster) behind the web nginx's
# /modules/<id>/ proxy, without rebuilding the web image.
#
# The build context is the REPO ROOT, like web/Dockerfile:
#   docker build -f web/examples/ui-module/Dockerfile -t example-module .
# It builds only the `example` variant (the browser harness builds the others in-process).

# --- build stage ---
FROM --platform=$BUILDPLATFORM node:26-alpine@sha256:40cd011b55415bc894e45b02fdd5a5dfde056921b7d23a64c74f80497af48797 AS build
WORKDIR /app/web

COPY web/package.json web/package-lock.json ./
# --ignore-scripts and the retry loop: the same reasons as web/Dockerfile (no lifecycle
# script is needed to build, and a registry blip should not fail the build).
RUN n=0; until npm ci --ignore-scripts; do n=$((n+1)); [ "$n" -ge 5 ] && { echo "npm ci: giving up after $n attempts" >&2; exit 1; }; echo "npm ci failed (attempt $n/5); retrying in $((n*3))s" >&2; sleep "$((n*3))"; done

# Only what the module build reads: its own sources (whose tsconfig extends the host's) and
# the host's pinned sdk transform.
COPY web/tsconfig.json ./tsconfig.json
COPY web/scripts/vite-mf-no-new-function.mjs ./scripts/vite-mf-no-new-function.mjs
COPY web/examples/ui-module/ ./examples/ui-module/
ARG MODULE_BUILD_TAG=v1
ENV MODULE_BUILD_TAG=$MODULE_BUILD_TAG MODULE_OUT_DIR=/out/modules/example
RUN npx vite build --config examples/ui-module/vite.config.ts

# --- runtime stage ---
# nginx-unprivileged runs as a non-root user (uid 101) and listens on 8080.
FROM nginxinc/nginx-unprivileged:1.30-alpine@sha256:44e36330f74d4f3a1d4e222acca9e23b401fb87811a7597024502bb759c4dd49
COPY web/examples/ui-module/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /out/ /usr/share/nginx/html/
EXPOSE 8080
