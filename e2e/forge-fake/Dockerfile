# syntax=docker/dockerfile:1
# forge-fake: a throwaway GitLab stand-in for the M6 E2E harness only. Never
# built or shipped as part of the product. Runs as root so it can bind :443 (the
# api's SSRF allowlist normalizes the base URL to the default https port).
#
# Debian (not alpine) because the optional git smart-HTTP endpoint shells out to
# `git http-backend`, which alpine's git package does not ship but debian's does
# (/usr/lib/git-core/git-http-backend).
FROM node:22-bookworm-slim@sha256:813a7480f28fdadac1f7f5c824bcdad435b5bc1322a5968bbbdef8d058f9dff4
WORKDIR /app
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY forge-fake.mjs ./
ENTRYPOINT ["node", "forge-fake.mjs"]
