# syntax=docker/dockerfile:1
# forge-fake: a throwaway GitLab stand-in for the M6 E2E harness only. Never
# built or shipped as part of the product. Runs as root so it can bind :443 (the
# api's SSRF allowlist normalizes the base URL to the default https port).
#
# Debian (not alpine) because the optional git smart-HTTP endpoint shells out to
# `git http-backend`, which alpine's git package does not ship but debian's does
# (/usr/lib/git-core/git-http-backend).
FROM node:24-bookworm-slim@sha256:3638d9a6fe4030bd716be989438248074489337ba3275657f93595428be4fc03
WORKDIR /app
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY forge-fake.mjs ./
ENTRYPOINT ["node", "forge-fake.mjs"]
