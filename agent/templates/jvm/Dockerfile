# syntax=docker/dockerfile:1

# The `jvm` worker template (PRD #18): the `base` image plus a JDK, for users
# whose repos build/test Java. It mirrors templates/base/Dockerfile layer-for-
# layer (same pinned node base, git+bash+tini, tsx-only npm ci, non-root `uzi`
# user, tini as PID 1, no secrets) and only ADDS the JDK — templates/base is the
# source of truth for the common layers; keep this in lockstep with it, and never
# drop a guardrail-relevant layer to add a dependency.
FROM node:22-alpine@sha256:16e22a550f3863206a3f701448c45f7912c6896a62de43add43bb9c86130c3e2
WORKDIR /app

# git for clone/fetch/worktree; bash for the SDK Bash tool; tini as PID 1 so
# SIGTERM reaches the worker and it shuts down cleanly. curl + xz for the devbox
# installer below. openjdk17 is this template's reason to exist — a full JDK (java
# + javac) from Alpine's community repo for Java toolchains the base image can't
# provide.
RUN apk add --no-cache git bash tini curl xz openjdk17

# JAVA_HOME + PATH so `java`/`javac` and Maven/Gradle wrappers resolve for the
# agent's Bash tool. Alpine installs openjdk17 under this fixed prefix.
ENV JAVA_HOME=/usr/lib/jvm/java-17-openjdk
ENV PATH="${JAVA_HOME}/bin:${PATH}"

# --- PRD #18 M3: devbox + nix — MIRRORED FROM templates/base (keep in lockstep) ---
# Same install + store-relocation as base; same UNVERIFIED + Alpine/musl risk (see
# templates/base/Dockerfile for the full note). The self-contained-per-template
# mechanism means this block is duplicated; if it grows further, consolidating base
# + jvm into one multi-stage Dockerfile selected by build `target` is the fix.
ENV DEVBOX_VERSION=0.13.0
RUN curl -fsSL https://get.jetify.com/devbox -o /tmp/devbox-install.sh \
    && FORCE=1 DEVBOX_USE_VERSION=${DEVBOX_VERSION} bash /tmp/devbox-install.sh -f \
    && rm -f /tmp/devbox-install.sh
ENV XDG_CACHE_HOME=/data/.cache \
    XDG_DATA_HOME=/data/.local/share \
    XDG_STATE_HOME=/data/.local/state \
    DEVBOX_DEBUG=0

# Install deps first for layer caching. Only runtime deps (tsx) are needed — the
# worker runs its TypeScript directly via tsx, matching the bottega reference.
COPY package.json package-lock.json ./
RUN npm ci --omit=dev

COPY tsconfig.json ./
COPY src ./src

# UZI_DATA_DIR holds the bare-clone cache, worktrees, and the pinned SDK session
# dir; it is expected to be a persistent volume.
ENV UZI_DATA_DIR=/data

# Bake the template identity into the image so the worker self-reports the image
# it was actually built from (PRD #18 register `template`). Hardcoded literal, not
# the build arg echoed back — see templates/base.
ENV UZI_WORKER_TEMPLATE=jvm
RUN mkdir -p /data \
    && addgroup -S uzi && adduser -S -G uzi -h /home/uzi uzi \
    && chown -R uzi:uzi /app /data
USER uzi:uzi
VOLUME ["/data"]

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["npm", "run", "start"]
