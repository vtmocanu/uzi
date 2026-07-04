# syntax=docker/dockerfile:1

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
RUN mkdir -p /data \
    && addgroup -S uzi && adduser -S -G uzi -h /home/uzi uzi \
    && chown -R uzi:uzi /app /data
USER uzi:uzi
VOLUME ["/data"]

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["npm", "run", "start"]
