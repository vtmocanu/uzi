# syntax=docker/dockerfile:1

# The `base` worker template (PRD #18): the minimal image every worker starts
# from. Heavy/system-dependency variants (e.g. templates/jvm) FROM this same node
# base and add layers on top; they MUST keep every guardrail-relevant layer here
# intact (non-root `uzi` user, tini, no secrets). `docker compose` selects the
# template via the WORKER_TEMPLATE build/compose var (default `base`).
#
# Unlike the api image (distroless), the worker CANNOT be distroless: M3's Claude
# Agent SDK spawns a real Bash tool and the git flow needs git + coreutils. This
# is a minimal Node 22 + git + bash base (digest shared with web/Dockerfile), run
# as a non-root user (PRD §Worker runtime).
FROM node:22-alpine@sha256:16e22a550f3863206a3f701448c45f7912c6896a62de43add43bb9c86130c3e2
WORKDIR /app

# git for clone/fetch/worktree; bash for the SDK Bash tool (M3); tini as PID 1 so
# SIGTERM reaches the worker and it shuts down cleanly. curl + xz + tar for the
# pinned nix/devbox downloads below.
RUN apk add --no-cache git bash tini curl xz tar

# The non-root runtime user, created EARLY so the nix store can be chowned to it
# below (the worker realizes packages as this user at run time).
RUN addgroup -S uzi && adduser -S -G uzi -h /home/uzi uzi

# --- PRD #18 M3/M4: nix + devbox for per-run tool provisioning ------------------
# The worker installs a run's tier-1 tool packages with devbox (nix under the hood)
# before the SDK starts (see agent/src/provision.ts). Alpine/musl is CONFIRMED
# working for nix (tester live smoke: nix closures ship their own glibc). Two hard
# rules learned from that smoke:
#   * Everything is installed at BUILD time and PINNED — no floating install
#     script piped to a shell, no runtime downloads (both were blockers: the
#     self-updating launcher fetched an unpinned devbox + tried to sudo-bootstrap
#     nix at first run).
#   * The /nix store stays at its DEFAULT path — relocating it (NIX_STORE_DIR)
#     changes store-path hashes and forfeits cache.nixos.org binary substitution.
#     Warm-start across container recreation is instead provided by a named volume
#     at /nix (docker-compose `agentnix`), which Docker seeds from this image's /nix
#     on first use.
#
# NOTE: the exact installer invocation + store ownership below are implemented per
# the tester's B1/B2/B3 findings but are NOT built in this workspace — the tester
# re-runs the live uzi-user provision + warm-start smoke against this image.

# TARGETARCH is auto-populated by buildkit (amd64 / arm64), so both templates build
# natively on x86_64 AND Apple-Silicon/arm64 hosts. nix-installer uses the
# GNU-triple arch (x86_64/aarch64); devbox uses the Go arch (amd64/arm64).
ARG TARGETARCH

# nix, single-user, installed as root then chowned to uzi (the lead-approved path)
# so the non-root worker can realize packages without a daemon. Determinate
# nix-installer, pinned by release tag; `--init none` = no systemd (container),
# sandbox off (can't nest in a build), flakes on (devbox needs them).
ARG NIX_INSTALLER_VERSION=3.21.5
RUN NIX_ARCH="$(case "${TARGETARCH}" in amd64) echo x86_64 ;; arm64) echo aarch64 ;; *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; esac)" \
    && curl -fsSL -o /tmp/nix-installer \
      "https://github.com/DeterminateSystems/nix-installer/releases/download/v${NIX_INSTALLER_VERSION}/nix-installer-${NIX_ARCH}-linux" \
    && chmod +x /tmp/nix-installer \
    && /tmp/nix-installer install linux --init none --no-confirm \
        --extra-conf "sandbox = false" \
        --extra-conf "experimental-features = nix-command flakes" \
    && rm -f /tmp/nix-installer \
    && chown -R uzi:uzi /nix
# Single-user runtime (no daemon in the container): empty NIX_REMOTE = direct store
# access, and the default profile bin is on PATH so `nix` resolves for uzi. /sbin
# must remain on PATH — nix build tooling shells out to it (see provision.ts).
ENV NIX_REMOTE=""
ENV PATH="/nix/var/nix/profiles/default/bin:${PATH}"

# devbox: the pinned release BINARY (not the self-updating launcher), verified
# against the release's own checksums, installed mode 0755 so the non-owner uzi
# user can exec it (the launcher shipped 0711 root-owned → "Permission denied").
# The tarball is saved under its EXACT release filename so `sha256sum -c` (which
# opens the filename named in checksums.txt) finds it.
ARG DEVBOX_VERSION=0.17.5
# set -o pipefail: BusyBox `sha256sum -c -` exits 0 on EMPTY stdin, so without it a
# grep that matches no checksum line would SKIP verification silently. pipefail
# makes the grep no-match (exit 1) fail the build.
RUN set -o pipefail \
    && cd /tmp \
    && TARBALL="devbox_${DEVBOX_VERSION}_linux_${TARGETARCH}.tar.gz" \
    && curl -fsSL -o "${TARBALL}" \
      "https://github.com/jetify-com/devbox/releases/download/${DEVBOX_VERSION}/${TARBALL}" \
    && curl -fsSL -o checksums.txt \
      "https://github.com/jetify-com/devbox/releases/download/${DEVBOX_VERSION}/checksums.txt" \
    && grep " ${TARBALL}\$" checksums.txt | sha256sum -c - \
    && mkdir -p devbox-extract \
    && tar -xzf "${TARBALL}" -C devbox-extract \
    && install -m 0755 "$(find devbox-extract -type f -name devbox | head -n1)" /usr/local/bin/devbox \
    && rm -rf "${TARBALL}" checksums.txt devbox-extract

# Install deps first for layer caching. Only runtime deps (tsx) are needed — the
# worker runs its TypeScript directly via tsx, matching the bottega reference.
# The build context is the REPO ROOT (PRD #39 Decision 5), so the worker's own
# files are under agent/ — the per-Dockerfile ignore (Dockerfile.dockerignore next
# to this file) trims the context to source only.
COPY agent/package.json agent/package-lock.json ./
RUN npm ci --omit=dev

COPY agent/tsconfig.json ./
COPY agent/src ./src

# --- PRD #39 M2: bake uzi's own source, read-only, for the `chat` run kind --------
# The chat agent answers questions about uzi itself from a read-only snapshot of the
# repo baked here at build time — never a clone, no PAT, matched exactly to the
# deployed image. Copied as root (the final USER is the non-root uzi) and never
# chowned to uzi below, so the agent can read it but never mutate it. The context is
# the repo root and the per-Dockerfile Dockerfile.dockerignore hard-excludes .env*,
# .git, inspiration/, node_modules and build artifacts — its patterns resolve
# relative to the CONTEXT ROOT, which is why the compose build context is the root.
# BUILD_INFO stamps the deployed commit (UZI_SRC_SHA build arg, empty acceptable)
# and build date; the chat system prompt points the model at it.
ARG UZI_SRC_SHA=
COPY . /opt/uzi-src
RUN printf 'uzi source baked into the worker image (PRD #39 chat)\ncommit: %s\nbuilt:  %s\n' \
      "${UZI_SRC_SHA:-unknown}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > /opt/uzi-src/BUILD_INFO

# UZI_DATA_DIR holds the bare-clone cache, worktrees, and (M3) the pinned SDK
# session dir; it is expected to be a persistent volume. devbox/nix per-user
# metadata (profiles, GC roots) lands HOME-derived under here too; the big store is
# on the separate /nix volume.
ENV UZI_DATA_DIR=/data

# Bake the template identity into the image so the worker self-reports the image
# it was actually built from (PRD #18 register `template`). This is a HARDCODED
# literal per template — deliberately NOT the WORKER_TEMPLATE build arg echoed
# back (that would make reported==declared always and kill the drift signal). The
# server compares this reported value with the declared choice and badges drift.
# Soft signal only (observability, not security — a hostile worker can lie); the
# join token stays the trust anchor.
ENV UZI_WORKER_TEMPLATE=base
RUN mkdir -p /data && chown -R uzi:uzi /app /data
# A ROOT-owned, non-writable (0555) empty dir the worker points git's core.hooksPath at
# on every invocation (agent/src/git.ts, M10 audit). The uzi user cannot create a child
# inside a root-owned 0555 dir, so a self_improve agent — which shares the worker uid
# until the k8s uid-split — cannot plant a pre-push (or any) hook there. MUST be created
# as root, BEFORE the USER line below. Each template bakes this stanza EXPLICITLY: jvm
# builds from the same upstream node image, NOT `FROM base`, so it carries its own copy.
RUN mkdir -p /usr/share/uzi-git-nohooks && chmod 0555 /usr/share/uzi-git-nohooks
USER uzi:uzi
# /data persists the clone cache + devbox/nix per-user metadata; /nix persists the
# nix store (warm-start). Compose backs these with the agentdata + agentnix volumes.
VOLUME ["/data", "/nix"]

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["npm", "run", "start"]
