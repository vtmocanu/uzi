# syntax=docker/dockerfile:1

# The `jvm` worker template (PRD #18): the `base` image plus a JDK, for users
# whose repos build/test Java. It mirrors templates/base/Dockerfile layer-for-
# layer (same pinned node base, git+bash+tini, non-root `uzi` user, tini as PID 1,
# no secrets, the SAME pinned nix + devbox provisioning stack) and only ADDS the
# JDK — templates/base is the source of truth for the common layers; keep this in
# lockstep with it (a test pins the devbox install + DEVBOX_VERSION match), and
# never drop a guardrail-relevant layer to add a dependency.
FROM node:22-alpine@sha256:16e22a550f3863206a3f701448c45f7912c6896a62de43add43bb9c86130c3e2
WORKDIR /app

# git for clone/fetch/worktree; bash for the SDK Bash tool; tini as PID 1; curl +
# xz + tar for the pinned nix/devbox downloads. openjdk17 is this template's reason
# to exist — a full JDK (java + javac) from Alpine's community repo.
RUN apk add --no-cache git bash tini curl xz tar openjdk17

# JAVA_HOME + PATH so `java`/`javac` and Maven/Gradle wrappers resolve for the
# agent's Bash tool. Alpine installs openjdk17 under this fixed prefix.
ENV JAVA_HOME=/usr/lib/jvm/java-17-openjdk
ENV PATH="${JAVA_HOME}/bin:${PATH}"

# The non-root runtime user, created EARLY so the nix store can be chowned to it.
RUN addgroup -S uzi && adduser -S -G uzi -h /home/uzi uzi

# --- PRD #18 M3/M4: nix + devbox — MIRRORED FROM templates/base (keep in lockstep) ---
# Same build-time pinned install + /nix-at-default-path + chown-to-uzi as base; see
# templates/base/Dockerfile for the full rationale (musl-confirmed, no floating
# fetch, no runtime download, store persisted by the agentnix volume). UNVERIFIED
# in this workspace; the tester re-runs the live smoke.
ARG NIX_INSTALLER_VERSION=3.21.5
RUN curl -fsSL -o /tmp/nix-installer \
      "https://github.com/DeterminateSystems/nix-installer/releases/download/v${NIX_INSTALLER_VERSION}/nix-installer-x86_64-linux" \
    && chmod +x /tmp/nix-installer \
    && /tmp/nix-installer install linux --init none --no-confirm \
        --extra-conf "sandbox = false" \
        --extra-conf "experimental-features = nix-command flakes" \
    && rm -f /tmp/nix-installer \
    && chown -R uzi:uzi /nix
ENV NIX_REMOTE=""
ENV PATH="/nix/var/nix/profiles/default/bin:${PATH}"

ARG DEVBOX_VERSION=0.17.5
RUN curl -fsSL -o /tmp/devbox.tar.gz \
      "https://github.com/jetify-com/devbox/releases/download/${DEVBOX_VERSION}/devbox_${DEVBOX_VERSION}_linux_amd64.tar.gz" \
    && curl -fsSL -o /tmp/devbox-checksums.txt \
      "https://github.com/jetify-com/devbox/releases/download/${DEVBOX_VERSION}/checksums.txt" \
    && (cd /tmp && grep "devbox_${DEVBOX_VERSION}_linux_amd64.tar.gz\$" devbox-checksums.txt | sha256sum -c -) \
    && mkdir -p /tmp/devbox-extract \
    && tar -xzf /tmp/devbox.tar.gz -C /tmp/devbox-extract \
    && install -m 0755 "$(find /tmp/devbox-extract -type f -name devbox | head -n1)" /usr/local/bin/devbox \
    && rm -rf /tmp/devbox.tar.gz /tmp/devbox-checksums.txt /tmp/devbox-extract

# Install deps first for layer caching. Only runtime deps (tsx) are needed — the
# worker runs its TypeScript directly via tsx, matching the bottega reference.
COPY package.json package-lock.json ./
RUN npm ci --omit=dev

COPY tsconfig.json ./
COPY src ./src

# UZI_DATA_DIR holds the bare-clone cache, worktrees, and the pinned SDK session
# dir; devbox/nix per-user metadata lands HOME-derived under here too; the big
# store is on the separate /nix volume.
ENV UZI_DATA_DIR=/data

# Bake the template identity into the image so the worker self-reports the image
# it was actually built from (PRD #18 register `template`). Hardcoded literal, not
# the build arg echoed back — see templates/base.
ENV UZI_WORKER_TEMPLATE=jvm
RUN mkdir -p /data && chown -R uzi:uzi /app /data
USER uzi:uzi
VOLUME ["/data", "/nix"]

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["npm", "run", "start"]
