# syntax=docker/dockerfile:1

# The build context is the REPO ROOT (see docker-compose.yml `web.build.context`),
# not web/, so the docs viewer can bundle docs/ as a sibling of web/. The Vite
# raw-glob in web/src/lib/docs.ts resolves `../../../docs`, which only works if
# that web/ <-> docs/ sibling layout is preserved here (web at /app/web, docs at
# /app/docs). The root .dockerignore keeps the context lean and, crucially, does
# NOT blanket-exclude *.md — those are the files this image bundles.

# --- build stage ---
FROM node:24-alpine@sha256:d32cdf619f63fe0471182d08996dd516c6275bb5fd31ae06e55a570bd9e1ad43 AS build
WORKDIR /app/web

COPY web/package.json web/package-lock.json ./
# --ignore-scripts: no dependency in this closure needs a lifecycle script to
# build the SPA, and this stage produces the bundle served to users. Verified
# 2026-08-03 at 5ea4d2f8 by building the image both ways: 16 files, 1829546
# bytes, tree hash dadcc2d7 either way. The control that makes that mean
# something: injecting one file moves the hash to e497e256, so the instrument
# is sensitive rather than reading nothing.
# esbuild is the only dependency here that ships a postinstall, and its platform
# binary arrives as one of 26 optional deps (`@esbuild/linux-x64`), which npm
# installs without running anything -- the script only validates a binary that
# is already in place. This repo has already been bitten by the general
# mechanism: `agent-browser`'s postinstall rewrites a HOST-GLOBAL symlink
# (see CLAUDE.md). Today zero packages in web's lockfile-tree that this stage
# installs would execute; the flag is what keeps that true after the next
# lockfile refresh, which is when nobody will be looking.
# NOT to be copied to agent/templates/base/Dockerfile: that image WANTS its
# postinstall, which is what bakes the agent-browser CLI into the worker.
RUN npm ci --ignore-scripts

COPY web/ ./
COPY docs/ /app/docs/
RUN npm run build

# --- runtime stage ---
# nginx-unprivileged runs as a non-root user (uid 101) and listens on 8080.
FROM nginxinc/nginx-unprivileged:1.27-alpine@sha256:65e3e85dbaed8ba248841d9d58a899b6197106c23cb0ff1a132b7bfe0547e4c0
COPY web/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/web/dist /usr/share/nginx/html
EXPOSE 8080
