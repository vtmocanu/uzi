# syntax=docker/dockerfile:1

# The build context is the REPO ROOT (see docker-compose.yml `web.build.context`),
# not web/, so the docs viewer can bundle docs/ as a sibling of web/. The Vite
# raw-glob in web/src/lib/docs.ts resolves `../../../docs`, which only works if
# that web/ <-> docs/ sibling layout is preserved here (web at /app/web, docs at
# /app/docs). The root .dockerignore keeps the context lean and, crucially, does
# NOT blanket-exclude *.md — those are the files this image bundles.

# --- build stage ---
FROM node:22-alpine@sha256:16e22a550f3863206a3f701448c45f7912c6896a62de43add43bb9c86130c3e2 AS build
WORKDIR /app/web

COPY web/package.json web/package-lock.json ./
RUN npm ci

COPY web/ ./
COPY docs/ /app/docs/
RUN npm run build

# --- runtime stage ---
# nginx-unprivileged runs as a non-root user (uid 101) and listens on 8080.
FROM nginxinc/nginx-unprivileged:1.27-alpine@sha256:65e3e85dbaed8ba248841d9d58a899b6197106c23cb0ff1a132b7bfe0547e4c0
COPY web/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/web/dist /usr/share/nginx/html
EXPOSE 8080
