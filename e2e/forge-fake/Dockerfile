# syntax=docker/dockerfile:1
# forge-fake: a throwaway GitLab stand-in for the M6 E2E harness only. Never
# built or shipped as part of the product. Runs as root so it can bind :443 (the
# api's SSRF allowlist normalizes the base URL to the default https port).
#
# Debian (not alpine) because the optional git smart-HTTP endpoint shells out to
# `git http-backend`, which alpine's git package does not ship but debian's does
# (/usr/lib/git-core/git-http-backend).
FROM node:24-bookworm-slim@sha256:ba849c60be29959425b8734d57b8b4b7d56f98edd9504c9af091d5281095a71e
WORKDIR /app
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# The bind-mounted bare under /gitroot is owned by the host/runner uid, which
# differs from this container's (root) uid on a CI runner (they match on a dev
# laptop, which is why the smart-HTTP git phase passed locally but 500s in CI).
# git-http-backend's upload-pack/receive-pack then trips 'detected dubious
# ownership' on /gitroot/repo.git and returns 500. Trust it at SYSTEM scope: those
# backend children read config from FILES (system/global), which is what reaches an
# ownership check — a command-scope `-c`/GIT_CONFIG_COUNT pin would be stripped before
# the child, the same reason the client fix in run-e2e.sh uses a file. /etc/gitconfig
# works because GIT_CONFIG_NOSYSTEM is never set here. This is a throwaway test image,
# so a blanket '*' is fine; it mirrors the client-side fix in the worker GIT_CONFIG_GLOBAL.
RUN git config --system --add safe.directory '*'
COPY forge-fake.mjs ./
ENTRYPOINT ["node", "forge-fake.mjs"]
