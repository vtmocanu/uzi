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
# SIGTERM reaches the worker and it shuts down cleanly.
RUN apk add --no-cache git bash tini

# Install deps first for layer caching. Only runtime deps (tsx) are needed — the
# worker runs its TypeScript directly via tsx, matching the bottega reference.
COPY package.json package-lock.json ./
RUN npm ci --omit=dev

COPY tsconfig.json ./
COPY src ./src

# UZI_DATA_DIR holds the bare-clone cache, worktrees, and (M3) the pinned SDK
# session dir; it is expected to be a persistent volume.
ENV UZI_DATA_DIR=/data

# Bake the template identity into the image so the worker self-reports the image
# it was actually built from (PRD #18 register `template`). This is a HARDCODED
# literal per template — deliberately NOT the WORKER_TEMPLATE build arg echoed
# back (that would make reported==declared always and kill the drift signal). The
# server compares this reported value with the declared choice and badges drift.
# Soft signal only (observability, not security — a hostile worker can lie); the
# join token stays the trust anchor.
ENV UZI_WORKER_TEMPLATE=base
RUN mkdir -p /data \
    && addgroup -S uzi && adduser -S -G uzi -h /home/uzi uzi \
    && chown -R uzi:uzi /app /data
USER uzi:uzi
VOLUME ["/data"]

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["npm", "run", "start"]
