# syntax=docker/dockerfile:1

# The `base` worker template (PRD #18): the minimal image every worker starts
# from. Heavy/system-dependency variants (e.g. templates/jvm) FROM this same node
# base and add layers on top; they MUST keep every guardrail-relevant layer here
# intact (the PRD #51 worker/runner uid split + root-entry setpriv drop, tini, no
# secrets). `docker compose` selects the template via the WORKER_TEMPLATE
# build/compose var (default `base`).
#
# Unlike the api image (distroless), the worker CANNOT be distroless: M3's Claude
# Agent SDK spawns a real Bash tool and the git flow needs git + coreutils. This
# is a minimal Node 24 + git + bash base (digest shared with web/Dockerfile), run
# as a non-root user (PRD §Worker runtime).
FROM node:24-alpine@sha256:d32cdf619f63fe0471182d08996dd516c6275bb5fd31ae06e55a570bd9e1ad43
WORKDIR /app

# git for clone/fetch/worktree; bash for the SDK Bash tool (M3); tini as PID 1 so
# SIGTERM reaches the worker and it shuts down cleanly. curl + xz + tar for the
# pinned nix/devbox downloads below. setpriv (util-linux) is the PRD #51 A1 drop
# wrapper the root entrypoint uses to drop root -> worker keeping only
# CAP_SETUID/SETGID ambient; the busybox setpriv applet CANNOT `--reuid`, so the
# grep asserts the util-linux binary is the one on /bin/setpriv (fails the build
# otherwise).
#
# docker-cli + docker-cli-compose + docker-cli-buildx (Alpine community, PRD #83
# Decision 1): the docker CLIENT only — there is NO `dockerd` in any image. It is
# INERT without a daemon: DOCKER_HOST is NOT baked (Q3/keystone set it at run time
# only when a sidecar is wired), so `docker …` on a sidecar-less worker fails loudly
# ("cannot connect to the Docker daemon") rather than reaching for a host socket, and
# the Bash guardrail denies docker outright until a daemon is wired. compose ships the
# v2 subcommand plugin (`docker compose`), buildx the build plugin.
# Retry apk on a transient Alpine-mirror blip (issue #739). 5 attempts, n*3s backoff;
# the setpriv assertion stays outside the loop (it is not a network step).
RUN n=0; until apk add --no-cache git bash tini curl xz tar setpriv docker-cli docker-cli-compose docker-cli-buildx; do n=$((n+1)); [ "$n" -ge 5 ] && { echo "apk add: giving up after $n attempts" >&2; exit 1; }; echo "apk add failed (attempt $n/5); retrying in $((n*3))s" >&2; sleep "$((n*3))"; done \
    && /bin/setpriv --help 2>&1 | grep -q -- --reuid

# Two runtime uids (PRD #51 A1). `worker` (uid 10001) holds the run credentials
# (join token, forge PAT, Anthropic token) and does all credentialed work;
# `runner` (uid 10002) is the distinct, cap-less uid the worker spawns the
# untrusted code-execution surfaces under (M4). Numbered to match the k8s
# remote-worker design intent (worker 10001 / runner 10002). `worker` joins group
# `runner` so it can reach runner-group trees. Created EARLY so /nix + /data can be
# chowned to them below; the entrypoint drops root -> worker at run time.
RUN addgroup -g 10001 worker && adduser -u 10001 -G worker -h /home/worker -D worker \
    && addgroup -g 10002 runner && adduser -u 10002 -G runner -h /home/runner -D runner \
    && addgroup worker runner
# The worker's HOME: the image default is /root (setpriv does not set HOME), so pin
# a worker-owned home for the worker's git/npm metadata. Per-run SDK HOME is
# overridden onto /data (buildSdkEnv) regardless.
ENV HOME=/home/worker

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
# re-runs the live worker-user provision + warm-start smoke against this image.

# TARGETARCH drives the arch of both downloads below: nix-installer uses the
# GNU-triple arch (x86_64/aarch64), devbox the Go arch (amd64/arm64). Buildkit
# auto-populates it, so a compose build is native on x86_64 AND Apple-Silicon
# hosts — but ONLY buildkit does. Kaniko implements none of buildkit's automatic
# platform args, so the retired GitLab pipeline's kaniko jobs passed --build-arg
# TARGETARCH explicitly. GitHub Actions builds with buildx, which auto-populates it,
# so no --build-arg is needed there (see .github/workflows/ci.yml).
#
# Deliberately NO default: a builder that forgets to set this must fail loudly on
# the empty value, not silently bake an image for the wrong architecture.
ARG TARGETARCH

# nix, single-user, installed as root then chowned to the worker (group `runner`,
# the M4 forward hook) so the non-root worker can realize packages without a
# daemon. The worker owns /nix (nix's owner check) and does all provisioning
# pre-split; group-write for the runner is set up in M4 with a live nix test.
# Determinate
# nix-installer, pinned by release tag; `--init none` = no systemd (container),
# sandbox off (can't nest in a build), flakes on (devbox needs them).
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
# Single-user runtime (no daemon in the container): empty NIX_REMOTE = direct store
# access, and the default profile bin is on PATH so `nix` resolves for worker. /sbin
# must remain on PATH — nix build tooling shells out to it (see provision.ts).
ENV NIX_REMOTE=""
ENV PATH="/nix/var/nix/profiles/default/bin:${PATH}"

# devbox: the pinned release BINARY (not the self-updating launcher), verified
# against the release's own checksums, installed mode 0755 so a non-owner runtime
# user (worker/runner) can exec it (the launcher shipped 0711 root-owned → "Permission denied").
# The tarball is saved under its EXACT release filename so `sha256sum -c` (which
# opens the filename named in checksums.txt) finds it.
ARG DEVBOX_VERSION=0.17.5
# set -o pipefail: BusyBox `sha256sum -c -` exits 0 on EMPTY stdin, so without it a
# grep that matches no checksum line would SKIP verification silently. pipefail
# makes the grep no-match (exit 1) fail the build.
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

# --- PRD #83 M1 (Q2): go + python default toolchain via devbox-global (build-time) ------
# The owner's "the default worker should have docker/python/go available" set, delivered
# through the ONE nix/devbox path (PRD #18 — no second apk package path, no version drift)
# and PRE-WARMED into the baked /nix so first use is instant instead of paying the cold
# closure fetch. The committed pinned manifest (agent/devbox-global/devbox.json) is the
# single source of truth for WHICH packages (13 as of 0.11.11 -- read the file, do not
# trust a count quoted here); `devbox global install` realizes their closures into the
# DEFAULT /nix (baked into this image layer) and builds the global profile. Runs AFTER the
# nix chown (line ~88) and the devbox binary install above, per the M1 spec.
#
# UNVERIFIED in this workspace (the Dockerfile-header convention, lines 57-59): the tester
# re-runs the live worker-user build and confirms the EXACT `devbox global` invocation
# (`devbox global install` from the copied manifest, or the equivalent
# `devbox global add go python3 python3Packages.pip`), the resulting `devbox.lock`, and
# that the manifest's packages land on the AGENT PATH. HOME=/home/worker (set above), so devbox's
# global config resolves under $HOME/.local/share/devbox/global/default; ENV PATH below
# adds that profile's bin dir. jvm mirrors this stanza (it is NOT `FROM base`).
RUN mkdir -p /home/worker/.local/share/devbox/global/default
COPY agent/devbox-global/devbox.json /home/worker/.local/share/devbox/global/default/devbox.json
# The committed lock PINS the nixpkgs revision for EVERY package. Without this COPY the
# image never sees it and the pin FLOATS per rebuild — it must sit BEFORE the install.
COPY agent/devbox-global/devbox.lock /home/worker/.local/share/devbox/global/default/devbox.lock
RUN devbox global install \
    && chown -R worker:runner /home/worker/.local/share/devbox /nix
# PRD #92 M1: expose the realized toolchain at a STABLE, immutable path independent of
# devbox's mutable `default` profile symlink (which advances with every image roll and
# strands stale-seeded /nix volumes). `/opt/uzi-toolchain` is the stable, PATH-facing handle
# (root-owned image path — deliberately NOT runner-writable); an INDIRECT nix GC root keeps
# the seed tar carrying the profile and stops a runner-triggered `nix-collect-garbage`
# (issue #91) from reaping it; `/etc/uzi-toolchain-profile` is the store-hash marker the
# version-aware seed (M2) keys on to decide reseed — the STORE HASH, NOT appVersion.
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
# so the baked set stays reachable underneath as a durable floor.
ENV PATH="/opt/uzi-toolchain/bin:${PATH}"
# PRD #92 M1 fail-closed guard (the "UNVERIFIED" stanza never had one): a broken toolchain
# fails the BUILD here (resolving against /opt/uzi-toolchain/bin) instead of shipping a
# silent 127 to every subagent at run time.
#
# DRIFT-PROOF SINCE 0.11.11. This was a hardcoded `command -v python3 && ... && openssl`
# line, and it never grew as devbox.json did: chromium/fontconfig/dejavu_fonts (PRD #87)
# and then file/perl/coreutils/kubernetes-helm/kubeconform were all baked WITHOUT guard
# coverage, so the guard passed on 5 of 13 packages while `publish:agent` reported green.
# The guard now cross-checks its own coverage against what devbox actually installed, so
# adding a package to devbox.json without a row in toolchain-guard.tsv FAILS THE BUILD.
# MIRRORED in jvm (it is NOT `FROM base`) -- keep the two in lockstep.
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
# the worker itself. MIRRORED in templates/jvm (jvm is NOT `FROM base`) — keep in lockstep.
RUN GOFLAGS=-buildvcs=false GOBIN=/usr/local/bin go install github.com/zricethezav/gitleaks/v8@v8.30.1 && gitleaks version

# Install deps first for layer caching. Only runtime deps (tsx) are needed — the
# worker runs its TypeScript directly via tsx, matching the bottega reference.
# The build context is the REPO ROOT (PRD #39 Decision 5), so the worker's own
# files are under agent/ — the per-Dockerfile ignore (Dockerfile.dockerignore next
# to this file) trims the context to source only.
COPY agent/package.json agent/package-lock.json ./
# Retry the registry fetch on a transient npm-registry blip (issue #739). 5 attempts, n*3s backoff.
RUN n=0; until npm ci --omit=dev; do n=$((n+1)); [ "$n" -ge 5 ] && { echo "npm ci: giving up after $n attempts" >&2; exit 1; }; echo "npm ci failed (attempt $n/5); retrying in $((n*3))s" >&2; sleep "$((n*3))"; done

# --- PRD #87: prebaked browser (chromium + agent-browser) for the web-ux role -----------
# The browser rides the SHARED devbox-global toolchain (chromium + fontconfig + dejavu_fonts
# in agent/devbox-global/devbox.json — the owner's chosen delivery), so it is already
# realized into /opt/uzi-toolchain by the stanza above; agent-browser is the pinned npm CLI
# baked by `npm ci` above. This block wires the four things devbox.json cannot express: font
# discovery, the M4 launch ENV, the M1 crash-close shim, and a fail-closed build guard.
# MIRRORED in templates/jvm (jvm is NOT `FROM base` — no ENV inheritance); keep in lockstep.
#
# Fonts: a nix Chromium closure ships NO fonts, so every screenshot renders tofu without a
# font pkg + a fontconfig that can find it. Point fontconfig at the STABLE toolchain font dir
# (#92 handle — never a rolling /nix/store path) via a minimal self-contained config, and
# pre-warm the cache at build time (as root, so the runtime runner uid never needs to write
# it; fc-cache is best-effort — libfontconfig reads the config regardless). UNVERIFIED
# without an image build: legible-vs-tofu is only provable by M7's screenshot.
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

# M4 runtime ENV (the actual M0 fix): devbox.json puts chromium on PATH but cannot express
# launch flags. Point agent-browser at the prebaked chromium via the STABLE
# /opt/uzi-toolchain/bin handle (#92 — a raw /nix/store path rolls per rebuild); force
# --no-sandbox (mandatory under the PRD #51 hardening: unprivileged uid + cap_drop:ALL +
# no-new-privileges make Chromium's setuid sandbox impossible) and --disable-dev-shm-usage
# (compose sets no shm_size => 64MB /dev/shm); AGENT_BROWSER_IDLE_TIMEOUT_MS closes an idle
# daemon so a wedged one can't silently no-op corrected launch options (M0 secondary).
# agent-browser 0.34.0 reads all three env vars (== --executable-path / --args / idle
# timeout). NOTE: the SDK hands the agent a SPARSE env (agent/src/sdk-env.ts REPLACES the
# subprocess env), so these baked vars do NOT reach the agent's Bash tool by themselves —
# the shim below re-establishes them for the agent-browser process. They are kept here for
# the worker/build context, the build guard, and as the documented contract. MIRRORED in jvm.
ENV AGENT_BROWSER_EXECUTABLE_PATH=/opt/uzi-toolchain/bin/chromium
ENV AGENT_BROWSER_ARGS="--no-sandbox,--disable-dev-shm-usage"
ENV AGENT_BROWSER_IDLE_TIMEOUT_MS=60000

# M1 crash-close shim on PATH ahead of the real CLI: a browser-driving agent that finds no
# runnable browser exits cleanly (exit 0 + a clear message) instead of a raw Chromium SUID
# abort (M0), keyed on the missing-browser FAILURE not the role name. /usr/local/bin is on
# PATH; the real npm bin (/app/node_modules/.bin/agent-browser) is NOT. The shim execs the
# real CLI by absolute path (no PATH recursion) when a browser resolves, and injects the M4
# launch config the sparse SDK env would otherwise drop. Root-owned 0755. MIRRORED in jvm.
#
# The layout above is NOT on its own what makes a bare `agent-browser` hit the shim: the
# CMD below is `npm run start`, and npm's run-script PREPENDS /app/node_modules/.bin to the
# PATH the worker sees, which SHADOWED the shim on the non-root (#58 k8s) start until issue
# #120. The invariant is held by agent/templates/entrypoint.sh exporting UZI_RUNNER_PATH =
# the untouched image PATH on BOTH branches — see the shim header for the full account.
COPY --chmod=0755 agent/bin/agent-browser /usr/local/bin/agent-browser

# Fail-closed BUILD guard: the image FAILS to build if agent-browser does not resolve, the
# baked CLI is missing OR cannot RUN, OR an exit-bounded headless Chromium launch fails — so a
# broken browser/font/lib set is caught here, not shipped as a run-time crash. The
# `agent-browser --version` step runs the CLI end-to-end through the shim and exercises the
# real native musl binary (a file-check alone would miss an unrunnable wrapper — e.g.
# an engines.node floor the base image does not meet, or a failed native-binary postinstall). `timeout` turns
# a hung launch into a build failure; --dump-dom actually starts Chromium (unlike a bare
# --version). CAVEAT: this runs in the BUILDER (root, no cap_drop, builder /dev), so it
# validates the binary + shared libs, NOT the runtime hardening posture — only M7 does that.
# MIRRORED in jvm.
RUN command -v agent-browser \
    && command -v chromium \
    && [ -x /app/node_modules/.bin/agent-browser ] \
    && timeout 30 agent-browser --version >/dev/null \
    && timeout 90 "$AGENT_BROWSER_EXECUTABLE_PATH" \
        --headless --no-sandbox --disable-dev-shm-usage \
        --user-data-dir=/tmp/uzi-chromium-buildcheck --dump-dom about:blank >/dev/null \
    && rm -rf /tmp/uzi-chromium-buildcheck

# BUG 1 (issue #114) store-perm normalization — MUST run AFTER the build guard above, not
# after the earlier `chown -R worker:runner /nix`. Root cause: the guard's `--dump-dom`
# launch runs chromium AS ROOT, and chromium creates its bundled `libexec/chromium/extensions`
# dir INSIDE its own read-only (0555) nix store path — a write only root's CAP_DAC_OVERRIDE
# permits — as `root:root 0700`. That happens after the chown, so the chown never covers it
# (the parent `.../libexec/chromium` is worker:runner 0555 from the chown; only this guard-
# created child is root:root — verified live in a 0.11.0 pod). The `seed-nix` init container
# runs NON-ROOT (uid 10001) and tars the store; it cannot open a root:root 0700 dir → tar
# fails → Init:CrashLoopBackOff. Normalize the whole store world-traversable/readable here,
# where the dir now exists. `a+rX` only ADDS bits (read for all; search/execute for dirs and
# already-executable files) so it cannot strip a needed perm; the store is already world-
# readable (0555/0444) by nix default, so in practice this only reopens the guard-created
# 0700 dirs. Safe: the toolchain store holds no secrets. As root here, busybox `chmod -R`
# does not hit the un-chmodable-root abort the non-root seed would.
RUN chmod -R a+rX /nix

COPY agent/tsconfig.json ./
COPY agent/src ./src

# --- PRD #39 M2: bake uzi's own source, read-only, for the `chat` run kind --------
# The chat agent answers questions about uzi itself from a read-only snapshot of the
# repo baked here at build time — never a clone, no PAT, matched exactly to the
# deployed image. Copied as root (the runtime worker/runner uids are non-root) and never
# chowned below, so the agent can read it but never mutate it. The context is
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

# Bake the RELEASE this image was built from, so the worker's self-reported
# `version` means something (PRD #113 M1). CI's publish:agent passes the git tag
# (`--build-arg UZI_AGENT_VERSION=$CI_COMMIT_TAG`) and the ${...#v} strip drops a
# leading v, so the stamped value is the bare Model-B coordinate == image tag ==
# chart appVersion — the same normalization api/Dockerfile applies to
# main.version. UNSET IS EMPTY, deliberately: a local or MR validation build
# stamps nothing, the worker reports no version, and the api classifies it
# `unknown`. That is the honest answer, and the reason the frozen "0.1.0-m4"
# default was retired — a fake SemVer is indistinguishable from a real report.
# MIRRORED in templates/jvm (jvm is NOT `FROM base`); keep in lockstep.
#
# PLACEMENT IS DELIBERATE, not tidiness: this ARG changes value on EVERY release,
# and a changed ARG invalidates the build cache from its declaration onward. Down
# here it only invalidates the trailing cheap layers. Moved up, it would invalidate
# the ~2.6 GiB nix/Chromium layers above, whose cache pressure already OOM'd a
# runner at exit 137 (see --compressed-caching=false in the retired GitLab pipeline) — a
# release-time build failure, not a style regression. ARG UZI_SRC_SHA sits just
# above for the same reason; keep both below the expensive layers.
ARG UZI_AGENT_VERSION=
ENV UZI_AGENT_VERSION=${UZI_AGENT_VERSION#v}

# Links this package to the repository on the registry (GHCR reads this label).
# Kept below the expensive nix/Chromium layers with the version stamp above.
LABEL org.opencontainers.image.source="https://github.com/vtmocanu/uzi"

# Bake the template identity into the image so the worker self-reports the image
# it was actually built from (PRD #18 register `template`). This is a HARDCODED
# literal per template — deliberately NOT the WORKER_TEMPLATE build arg echoed
# back (that would make reported==declared always and kill the drift signal). The
# server compares this reported value with the declared choice and badges drift.
# Soft signal only (observability, not security — a hostile worker can lie); the
# join token stays the trust anchor.
ENV UZI_WORKER_TEMPLATE=base
RUN mkdir -p /data && chown -R worker:worker /app /data
# A ROOT-owned, non-writable (0555) empty dir the worker points git's core.hooksPath at
# on every invocation (agent/src/git.ts, M10 audit). Neither the worker nor the runner
# uid can create a child inside a root-owned 0555 dir, so no code-execution surface
# (self_improve agent, SDK agent) can plant a pre-push (or any) hook there. MUST be
# created as root; the image baseline stays root and the entrypoint drops later. Each
# template bakes this stanza EXPLICITLY: jvm builds from the same upstream node image,
# NOT `FROM base`, so it carries its own copy.
RUN mkdir -p /usr/share/uzi-git-nohooks && chmod 0555 /usr/share/uzi-git-nohooks

# PRD #51 A1: the image runs as ROOT and the entrypoint (a minimal root startup window
# that migrates the persisted volumes to the worker uid and forces the join token to
# 0400 worker) setpriv-drops to the unprivileged `worker` uid, keeping only
# CAP_SETUID/CAP_SETGID (ambient) so the worker can spawn `runner`-uid children (M4).
# There is deliberately NO `USER` line — the drop happens in the entrypoint, under
# compose's cap_add + no-new-privileges. The script is shared by every template (copied
# explicitly; jvm is NOT `FROM base`), root-owned 0755.
COPY --chmod=0755 agent/templates/entrypoint.sh /usr/local/sbin/uzi-entrypoint
# /data persists the clone cache + devbox/nix per-user metadata; /nix persists the
# nix store (warm-start). Compose backs these with the agentdata + agentnix volumes.
VOLUME ["/data", "/nix"]

# tini stays PID 1 (SIGTERM -> clean worker shutdown); the entrypoint execs it as the
# dropped worker: uzi-entrypoint -> setpriv (drop) -> tini -> npm run start.
ENTRYPOINT ["/usr/local/sbin/uzi-entrypoint"]
CMD ["npm", "run", "start"]
