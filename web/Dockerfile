# syntax=docker/dockerfile:1

# The build context is the REPO ROOT (see docker-compose.yml `web.build.context`),
# not web/, so the docs viewer can bundle docs/ as a sibling of web/. The Vite
# raw-glob in web/src/lib/docs.ts resolves `../../../docs`, which only works if
# that web/ <-> docs/ sibling layout is preserved here (web at /app/web, docs at
# /app/docs). The root .dockerignore keeps the context lean and, crucially, does
# NOT blanket-exclude *.md — those are the files this image bundles.

# --- build stage ---
FROM node:24-alpine@sha256:e67514e5d0f6c46656005e1b693b2ec9d52e80b641307de684d4a015ba7a4eaf AS build
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
# Retry the registry fetch on a transient npm-registry blip (issue #739): a single reset
# should not redden a CI validation or release-publish build. 5 attempts, n*3s backoff.
RUN n=0; until npm ci --ignore-scripts; do n=$((n+1)); [ "$n" -ge 5 ] && { echo "npm ci: giving up after $n attempts" >&2; exit 1; }; echo "npm ci failed (attempt $n/5); retrying in $((n*3))s" >&2; sleep "$((n*3))"; done

COPY web/ ./
COPY docs/ /app/docs/
# The in-app changelog bundles the repo-root CHANGELOG.md the same way the docs
# viewer bundles docs/: web/src/lib/changelog.ts resolves `../../../CHANGELOG.md`,
# which lands at /app/CHANGELOG.md only if this sibling layout is preserved here.
COPY CHANGELOG.md /app/CHANGELOG.md
# The API-contract tests (web/src/lib/apiContract.test.ts) import the recorded wire
# fixtures as typed JSON modules (compile-time keyof/typeof checks), resolving
# `../../../fixtures/api-contract/*.json`, which lands at /app/fixtures/api-contract/
# only if this sibling layout is preserved here. `tsc --noEmit` (in `npm run build`)
# typechecks the test files, so a missing fixtures dir fails the build with TS2307.
COPY fixtures/api-contract/ /app/fixtures/api-contract/
RUN npm run build

# --- runtime stage ---
# nginx-unprivileged runs as a non-root user (uid 101) and listens on 8080.
FROM nginxinc/nginx-unprivileged:1.30-alpine@sha256:44e36330f74d4f3a1d4e222acca9e23b401fb87811a7597024502bb759c4dd49
# Links this package to the repository on the registry (GHCR reads this label).
LABEL org.opencontainers.image.source="https://github.com/vtmocanu/uzi"
COPY web/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/web/dist /usr/share/nginx/html
EXPOSE 8080
