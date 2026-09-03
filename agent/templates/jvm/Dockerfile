# syntax=docker/dockerfile:1

# The `jvm` worker template (PRD #18): the `base` image plus a JDK, for users
# whose repos build/test Java. It mirrors templates/base/Dockerfile layer-for-
# layer (same pinned node base, git+bash+tini, the PRD #51 worker/runner uid split
# + root-entry setpriv drop, tini as PID 1, no secrets, the SAME pinned nix +
# devbox provisioning stack) and only ADDS the JDK — templates/base is the source
# of truth for the common layers; keep this in lockstep with it (a test pins the
# devbox install + DEVBOX_VERSION match), and never drop a guardrail-relevant layer
# to add a dependency.
FROM node:24-alpine@sha256:d32cdf619f63fe0471182d08996dd516c6275bb5fd31ae06e55a570bd9e1ad43
WORKDIR /app

# git for clone/fetch/worktree; bash for the SDK Bash tool; tini as PID 1; curl +
# xz + tar for the pinned nix/devbox downloads. openjdk17 is this template's reason
# to exist — a full JDK (java + javac) from Alpine's community repo. setpriv
# (util-linux) is the PRD #51 A1 drop wrapper (busybox setpriv cannot `--reuid`);
# the grep fails the build if /bin/setpriv is not the util-linux binary. docker-cli +
# docker-cli-compose + docker-cli-buildx (PRD #83 Decision 1): the docker CLIENT only,
# NO dockerd, inert without a wired daemon (DOCKER_HOST not baked). MIRRORED from
# templates/base (jvm is NOT `FROM base`; keep in lockstep).
# Retry apk on a transient Alpine-mirror blip (issue #739); see templates/base for rationale.
RUN n=0; until apk add --no-cache git bash tini curl xz tar openjdk17 setpriv docker-cli docker-cli-compose docker-cli-buildx; do n=$((n+1)); [ "$n" -ge 5 ] && { echo "apk add: giving up after $n attempts" >&2; exit 1; }; echo "apk add failed (attempt $n/5); retrying in $((n*3))s" >&2; sleep "$((n*3))"; done \
    && /bin/setpriv --help 2>&1 | grep -q -- --reuid

# JAVA_HOME + PATH so `java`/`javac` and Maven/Gradle wrappers resolve for the
# agent's Bash tool. Alpine installs openjdk17 under this fixed prefix.
ENV JAVA_HOME=/usr/lib/jvm/java-17-openjdk
ENV PATH="${JAVA_HOME}/bin:${PATH}"

# Two runtime uids (PRD #51 A1), MIRRORED from templates/base. `worker` (uid 10001)
# holds the run credentials and does all credentialed work; `runner` (uid 10002) is
# the distinct, cap-less uid the worker spawns untrusted code-execution under (M4).
# Numbered to match the k8s design intent (worker 10001 / runner 10002); `worker`
# joins group `runner`. Created EARLY so /nix + /data can be chowned; the entrypoint
# drops root -> worker at run time.
RUN addgroup -g 10001 worker && adduser -u 10001 -G worker -h /home/worker -D worker \
    && addgroup -g 10002 runner && adduser -u 10002 -G runner -h /home/runner -D runner \
    && addgroup worker runner
# The worker's HOME (the image default is /root; setpriv does not set HOME). Per-run
# SDK HOME is overridden onto /data (buildSdkEnv) regardless. MIRRORED from base.
ENV HOME=/home/worker

# --- PRD #18 M3/M4: nix + devbox — MIRRORED FROM templates/base (keep in lockstep) ---
# Same build-time pinned install + /nix-at-default-path + chown-to-worker as base; see
# templates/base/Dockerfile for the full rationale (musl-confirmed, no floating
# fetch, no runtime download, store persisted by the agentnix volume). UNVERIFIED
# in this workspace; the tester re-runs the live smoke.
# TARGETARCH → native amd64 + arm64 builds; buildkit auto-populates it, kaniko does
# not (CI passes it explicitly), and it has no default on purpose. See
# templates/base for the full rationale.
ARG TARGETARCH
ARG NIX_INSTALLER_VERSION=3.21.5
RUN NIX_ARCH="$(case "${TARGETARCH}" in amd64) echo x86_64 ;; arm64) echo aarch64 ;; *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; esac)" \
    && curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 --connect-timeout 15 -o /tmp/nix-installer \
      "https://github.com/DeterminateSystems/nix-installer/releases/download/v${NIX_INSTALLER_VERSION}/nix-installer-${NIX_ARCH}-linux" \
    && chmod +x /tmp/nix-installer \
    && /tmp/nix-installer install linux --init none --no-confirm \
        --extra-conf "sandbox = false" \
        --extra-conf "experimental-features = nix-command flakes" \
    && rm -f /tmp/nix-installer \
    && chown -R worker:runner /nix
ENV NIX_REMOTE=""
ENV PATH="/nix/var/nix/profiles/default/bin:${PATH}"

# devbox pinned binary; tarball saved under its EXACT release name so sha256sum -c
# (which opens the name in checksums.txt) finds it. See templates/base.
ARG DEVBOX_VERSION=0.17.5
# set -o pipefail so a grep that matches no checksum line fails the build rather
# than feeding empty stdin to `sha256sum -c -` (which exits 0). See templates/base.
RUN set -o pipefail \
    && cd /tmp \
    && TARBALL="devbox_${DEVBOX_VERSION}_linux_${TARGETARCH}.tar.gz" \
    && curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 --connect-timeout 15 -o "${TARBALL}" \
      "https://github.com/jetify-com/devbox/releases/download/${DEVBOX_VERSION}/${TARBALL}" \
    && curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 --connect-timeout 15 -o checksums.txt \
      "https://github.com/jetify-com/devbox/releases/download/${DEVBOX_VERSION}/checksums.txt" \
    && grep " ${TARBALL}\$" checksums.txt | sha256sum -c - \
    && mkdir -p devbox-extract \
    && tar -xzf "${TARBALL}" -C devbox-extract \
    && install -m 0755 "$(find devbox-extract -type f -name devbox | head -n1)" /usr/local/bin/devbox \
    && rm -rf "${TARBALL}" checksums.txt devbox-extract

# --- PRD #83 M1 (Q2): go + python default toolchain via devbox-global — MIRRORED FROM base
# The same committed pinned global manifest (agent/devbox-global/devbox.json -- read it
# for the package set; do not trust a list quoted here) realized into the baked /nix at
# build time via `devbox global install`, so every run has the toolchain instantly.
# jvm is NOT `FROM base`, so it carries
# its own copy of this stanza + the ENV PATH addition — keep in LOCKSTEP with base. Exact
# `devbox global` invocation + resulting PATH/devbox.lock are tester-confirmed on the live
# build (Dockerfile header convention). See templates/base for the full rationale.
RUN mkdir -p /home/worker/.local/share/devbox/global/default
COPY agent/devbox-global/devbox.json /home/worker/.local/share/devbox/global/default/devbox.json
# The committed lock PINS the nixpkgs revision — must sit BEFORE the install or the image
# never sees it and the pin floats. MIRRORED from base (jvm is NOT `FROM base`).
COPY agent/devbox-global/devbox.lock /home/worker/.local/share/devbox/global/default/devbox.lock
RUN devbox global install \
    && chown -R worker:runner /home/worker/.local/share/devbox /nix
# PRD #92 M1 (MIRRORED from base — jvm is NOT `FROM base`): expose the realized toolchain at
# a STABLE, immutable path independent of devbox's mutable `default` profile symlink (which
# advances with every image roll and strands stale-seeded /nix volumes). `/opt/uzi-toolchain`
# is the stable, PATH-facing handle (root-owned image path — deliberately NOT runner-writable);
# an INDIRECT nix GC root keeps the seed tar carrying the profile and stops a runner-triggered
# `nix-collect-garbage` (issue #91) from reaping it; `/etc/uzi-toolchain-profile` is the
# store-hash marker the version-aware seed (M2) keys on to decide reseed — STORE HASH, NOT
# appVersion.
RUN PROFILE="$(readlink -f /home/worker/.local/share/devbox/global/default/.devbox/nix/profile/default)" \
    && ln -sfn "$PROFILE" /opt/uzi-toolchain \
    && mkdir -p /nix/var/nix/gcroots \
    && ln -sfn /opt/uzi-toolchain /nix/var/nix/gcroots/uzi-toolchain \
    && readlink -f /opt/uzi-toolchain > /etc/uzi-toolchain-profile \
    && chown -R worker:runner /nix/var/nix/gcroots \
    && chown -h worker:runner /nix/var/nix/gcroots/uzi-toolchain
# Put the STABLE toolchain bin on the image PATH so the baked toolchain resolves for BOTH the
# worker's provision subprocess and the agent (whose SDK PATH = runnerPath() = the image
# PATH). Points at /opt/uzi-toolchain/bin — the immutable handle above — NOT the mutable
# `.devbox/.../default/bin` symlink. A per-run `devbox shellenv` still PREPENDS run tools,
# so the baked set stays reachable underneath as a durable floor. MIRRORED from base.
ENV PATH="/opt/uzi-toolchain/bin:${PATH}"
# PRD #92 M1 fail-closed guard (the "UNVERIFIED" stanza never had one): a broken toolchain
# fails the BUILD here (resolving against /opt/uzi-toolchain/bin) instead of shipping a
# silent 127 to every subagent at run time. MIRRORED from base.
#
# DRIFT-PROOF SINCE 0.11.11. This was a hardcoded `command -v python3 && ... && openssl`
# line, and it never grew as devbox.json did: chromium/fontconfig/dejavu_fonts (PRD #87)
# and then file/perl/coreutils/kubernetes-helm/kubeconform were all baked WITHOUT guard
# coverage, so the guard passed on 5 of 13 packages while `publish:agent` reported green.
# The guard now cross-checks its own coverage against what devbox actually installed, so
# adding a package to devbox.json without a row in toolchain-guard.tsv FAILS THE BUILD.
COPY agent/devbox-global/toolchain-guard.tsv /tmp/toolchain-guard.tsv
COPY agent/devbox-global/assert-toolchain.sh /tmp/assert-toolchain.sh
RUN bash /tmp/assert-toolchain.sh /tmp/toolchain-guard.tsv \
    && rm -f /tmp/assert-toolchain.sh /tmp/toolchain-guard.tsv

# PRD #974 M2: bake the pinned gitleaks so the finalize pre-push secret scan needs NO run-time
# module fetch (the restricted worker tier blocks proxy.golang.org). `go` resolves via the
# toolchain ENV above; GOBIN=/usr/local/bin puts the binary on the image PATH (agent-browser is
# installed there too and used). GOFLAGS=-buildvcs=false avoids VCS stamping. The trailing
# `gitleaks version` is a fail-closed BUILD-time check (mirrors the toolchain guard idiom) so a
# broken install fails the BUILD, not every run. Pinned to v8.30.1 to match the repo's gate:repo
# scan (Taskfile.yml:913 / scripts/scan-secrets.sh). CI-verified: this image build cannot run in
# the worker itself. MIRRORED from templates/base (jvm is NOT `FROM base`) — keep in lockstep.
RUN GOFLAGS=-buildvcs=false GOBIN=/usr/local/bin go install github.com/zricethezav/gitleaks/v8@v8.30.1 && gitleaks version

# Install deps first for layer caching. Only runtime deps (tsx) are needed — the
# worker runs its TypeScript directly via tsx, matching the bottega reference.
# The build context is the REPO ROOT (PRD #39 Decision 5), so the worker's own
# files are under agent/ — the per-Dockerfile ignore (Dockerfile.dockerignore next
# to this file) trims the context to source only. MIRRORED from templates/base.
COPY agent/package.json agent/package-lock.json ./
# Retry the registry fetch on a transient npm-registry blip (issue #739); see templates/base.
RUN n=0; until npm ci --omit=dev; do n=$((n+1)); [ "$n" -ge 5 ] && { echo "npm ci: giving up after $n attempts" >&2; exit 1; }; echo "npm ci failed (attempt $n/5); retrying in $((n*3))s" >&2; sleep "$((n*3))"; done

# --- PRD #87: prebaked browser (chromium + agent-browser) for the web-ux role -----------
# MIRRORED from templates/base (jvm is NOT `FROM base` — no ENV inheritance; forgetting this
# block would leave jvm workers with the browser binary but no launch flags and re-create the
# M0 SUID abort on every jvm worker). Keep in LOCKSTEP with base. The browser rides the SHARED
# devbox-global toolchain (chromium + fontconfig + dejavu_fonts in
# agent/devbox-global/devbox.json), already realized into /opt/uzi-toolchain by the stanza
# above; agent-browser is the pinned npm CLI baked by `npm ci`. This block wires font
# discovery, the M4 launch ENV, the M1 crash-close shim, and a fail-closed build guard.
#
# Fonts: a nix Chromium closure ships NO fonts, so screenshots render tofu without a font pkg
# + a fontconfig that can find it. Point fontconfig at the STABLE toolchain font dir (#92
# handle) via a minimal self-contained config and pre-warm the cache at build time (as root;
# fc-cache best-effort — libfontconfig reads the config regardless). UNVERIFIED without an
# image build: legible-vs-tofu is only provable by M7's screenshot. See templates/base.
RUN mkdir -p /etc/fonts /var/cache/fontconfig \
    && printf '%s\n' \
      '<?xml version="1.0"?>' \
      '<!DOCTYPE fontconfig SYSTEM "urn:fontconfig:fonts.dtd">' \
      '<fontconfig>' \
      '  <dir>/opt/uzi-toolchain/share/fonts</dir>' \
      '  <cachedir>/var/cache/fontconfig</cachedir>' \
      '</fontconfig>' > /etc/fonts/fonts.conf
ENV FONTCONFIG_FILE=/etc/fonts/fonts.conf
RUN if command -v fc-cache >/dev/null 2>&1; then fc-cache -f || true; fi

# M4 runtime ENV (the M0 fix): chromium via the STABLE /opt/uzi-toolchain/bin handle (#92 —
# never a raw /nix/store path), --no-sandbox (mandatory under PRD #51 hardening) +
# --disable-dev-shm-usage (64MB /dev/shm), and an idle timeout so a wedged daemon self-closes.
# The SDK hands the agent a SPARSE env (sdk-env.ts REPLACES it), so these baked vars do NOT
# reach the agent's Bash tool alone — the shim below re-establishes them; kept here for the
# worker/build context, the build guard, and the documented contract. MIRRORED from base.
ENV AGENT_BROWSER_EXECUTABLE_PATH=/opt/uzi-toolchain/bin/chromium
ENV AGENT_BROWSER_ARGS="--no-sandbox,--disable-dev-shm-usage"
ENV AGENT_BROWSER_IDLE_TIMEOUT_MS=60000

# M1 crash-close shim on PATH ahead of the real CLI: a browser-driving agent that finds no
# runnable browser exits cleanly (exit 0 + message) instead of a raw Chromium SUID abort (M0),
# keyed on the missing-browser FAILURE not the role name. /usr/local/bin is on PATH; the real
# npm bin (/app/node_modules/.bin/agent-browser) is NOT. The shim execs the real CLI by
# absolute path when a browser resolves and injects the M4 launch config the sparse SDK env
# drops. Root-owned 0755. MIRRORED from base.
#
# That layout alone does NOT make a bare `agent-browser` hit the shim: the CMD is `npm run
# start`, and npm's run-script PREPENDS /app/node_modules/.bin to the worker's PATH, which
# SHADOWED the shim on the non-root (#58 k8s) start until issue #120. The invariant is held
# by agent/templates/entrypoint.sh exporting UZI_RUNNER_PATH = the untouched image PATH on
# BOTH branches — see the shim header for the full account. MIRRORED from base.
COPY --chmod=0755 agent/bin/agent-browser /usr/local/bin/agent-browser

# Fail-closed BUILD guard: the image FAILS to build if agent-browser does not resolve, the
# baked CLI is missing OR cannot RUN, OR an exit-bounded headless Chromium launch fails. The
# `agent-browser --version` step runs the CLI end-to-end through the shim and exercises the
# real native musl binary (a file-check alone would miss an unrunnable wrapper — e.g.
# an engines.node floor the base image does not meet, or a failed native-binary postinstall). `timeout` turns
# a hung launch into a build failure; --dump-dom actually starts Chromium (unlike --version).
# CAVEAT: runs in the BUILDER (root, no cap_drop), so it validates the binary + shared libs,
# NOT the runtime hardening posture — only M7 does that. MIRRORED from base.
#
# Issue #1082 (MIRRORED from base — read the account there): (a) the `--version` step is
# repeated behind a fake glibc `ldd` first on PATH, the condition a provisioned nix glibc.bin
# creates at run time and the plain image PATH (musl `ldd`) can never reproduce; (b) one
# `open about:blank` THROUGH the shim, then `close`, under a throwaway HOME and a 2s idle timeout
# so the daemon (which outlives `close`) is gone before the root-owned build state is removed,
# with a trailing assertion that nothing was baked.
RUN command -v agent-browser \
    && command -v chromium \
    && [ -x /app/node_modules/.bin/agent-browser ] \
    && timeout 30 agent-browser --version >/dev/null \
    && mkdir -p /tmp/uzi-ab-fake-glibc \
    && printf '#!/bin/sh\necho "ldd (GNU libc) 2.42"\n' > /tmp/uzi-ab-fake-glibc/ldd \
    && chmod 0755 /tmp/uzi-ab-fake-glibc/ldd \
    && PATH="/tmp/uzi-ab-fake-glibc:$PATH" timeout 30 agent-browser --version >/dev/null \
    && timeout 90 "$AGENT_BROWSER_EXECUTABLE_PATH" \
        --headless --no-sandbox --disable-dev-shm-usage \
        --user-data-dir=/tmp/uzi-chromium-buildcheck --dump-dom about:blank >/dev/null \
    && HOME=/tmp/uzi-ab-home AGENT_BROWSER_IDLE_TIMEOUT_MS=2000 \
        timeout 90 agent-browser --session uzi-buildcheck open about:blank >/dev/null \
    && HOME=/tmp/uzi-ab-home AGENT_BROWSER_IDLE_TIMEOUT_MS=2000 \
        timeout 30 agent-browser --session uzi-buildcheck close >/dev/null \
    && sleep 4 \
    && rm -rf /tmp/uzi-chromium-buildcheck /tmp/uzi-ab-fake-glibc /tmp/uzi-agent-browser-0 /tmp/uzi-ab-home \
    && [ ! -e "$HOME/.agent-browser" ] && [ ! -e /tmp/uzi-ab-home ]

# BUG 1 (issue #114) store-perm normalization — MIRRORED from templates/base (jvm is NOT
# `FROM base`; keep in lockstep). MUST run AFTER the build guard above, not after the earlier
# `chown -R worker:runner /nix`. Root cause: the guard's `--dump-dom` launch runs chromium AS
# ROOT, and chromium creates its bundled `libexec/chromium/extensions` dir INSIDE its own
# read-only (0555) nix store path — a write only root's CAP_DAC_OVERRIDE permits — as
# `root:root 0700`. That happens after the chown, so the chown never covers it (the parent
# `.../libexec/chromium` is worker:runner 0555 from the chown; only this guard-created child is
# root:root — verified live in a 0.11.0 pod). The `seed-nix` init container runs NON-ROOT (uid
# 10001) and tars the store; it cannot open a root:root 0700 dir → tar fails →
# Init:CrashLoopBackOff. Normalize the whole store world-traversable/readable here, where the
# dir now exists. `a+rX` only ADDS bits (read for all; search/execute for dirs and already-
# executable files) so it cannot strip a needed perm; the store is already world-readable
# (0555/0444) by nix default, so in practice this only reopens the guard-created 0700 dirs.
# Safe: the toolchain store holds no secrets. As root here, busybox `chmod -R` does not hit the
# un-chmodable-root abort the non-root seed would.
RUN chmod -R a+rX /nix

COPY agent/tsconfig.json ./
COPY agent/src ./src

# --- PRD #39 M2: bake uzi's own source, read-only, for the `chat` run kind --------
# Read-only snapshot of uzi's own source for the chat agent, matched to the deployed
# image — never a clone, no PAT. Root-owned (the runtime worker/runner uids are non-root), never
# chowned below. Context is the repo root; the per-Dockerfile
# Dockerfile.dockerignore hard-excludes .env*, .git, inspiration/, node_modules and
# build artifacts (patterns relative to the CONTEXT ROOT). MIRRORED from
# templates/base — keep in lockstep. See templates/base/Dockerfile for the rationale.
ARG UZI_SRC_SHA=
COPY . /opt/uzi-src
RUN printf 'uzi source baked into the worker image (PRD #39 chat)\ncommit: %s\nbuilt:  %s\n' \
      "${UZI_SRC_SHA:-unknown}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > /opt/uzi-src/BUILD_INFO

# UZI_DATA_DIR holds the bare-clone cache, worktrees, and the pinned SDK session
# dir; devbox/nix per-user metadata lands HOME-derived under here too; the big
# store is on the separate /nix volume.
ENV UZI_DATA_DIR=/data

# Bake the RELEASE this image was built from (PRD #113 M1) — the SAME stanza as
# templates/base, which jvm must carry itself (it is NOT `FROM base`, so no ENV is
# inherited). CI passes the git tag; the ${...#v} strip yields the bare Model-B
# coordinate; unset stays EMPTY so an unstamped build reports `unknown` rather
# than a fake SemVer. See templates/base for the full rationale, including why this
# ARG must stay BELOW the expensive nix/Chromium layers: its value changes every
# release and a changed ARG invalidates the cache from its declaration down.
ARG UZI_AGENT_VERSION=
ENV UZI_AGENT_VERSION=${UZI_AGENT_VERSION#v}

# Links this package to the repository on the registry (GHCR reads this label).
# Kept below the expensive nix/Chromium layers with the version stamp above.
LABEL org.opencontainers.image.source="https://github.com/vtmocanu/uzi"

# Bake the template identity into the image so the worker self-reports the image
# it was actually built from (PRD #18 register `template`). Hardcoded literal, not
# the build arg echoed back — see templates/base.
ENV UZI_WORKER_TEMPLATE=jvm
RUN mkdir -p /data && chown -R worker:worker /app /data
# Root-owned 0555 empty git-hooks dir — the SAME explicit stanza as templates/base.
# jvm builds from the same upstream node image, NOT `FROM base`, so it must bake this
# itself. core.hooksPath points here (agent/src/git.ts, M10), so neither the worker
# nor the runner uid can plant a git hook. MUST be as root; the entrypoint drops later.
RUN mkdir -p /usr/share/uzi-git-nohooks && chmod 0555 /usr/share/uzi-git-nohooks

# PRD #51 A1 root-entry setpriv drop — the SAME explicit stanza as templates/base (jvm
# is NOT `FROM base`). Image baseline stays ROOT and the entrypoint drops to `worker`
# keeping only CAP_SETUID/SETGID ambient (M4 spawns `runner` children). No `USER` line.
COPY --chmod=0755 agent/templates/entrypoint.sh /usr/local/sbin/uzi-entrypoint
VOLUME ["/data", "/nix"]

# tini stays PID 1 (SIGTERM -> clean shutdown), execed by the entrypoint as the dropped
# worker: uzi-entrypoint -> setpriv (drop) -> tini -> npm run start. MIRRORED from base.
ENTRYPOINT ["/usr/local/sbin/uzi-entrypoint"]
CMD ["npm", "run", "start"]
