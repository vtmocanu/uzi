# ADR-1720: Agent images are a content-addressed runtime base plus an always-built release stage

**Status**: Accepted (issue #1720, follow-up to #1682)
**Date**: 2026-09-26
**Deciders**: Vlad Mocanu + agent team; design proposed by the #1682 reporter

## Decision (summary)

Each agent template Dockerfile (`agent/templates/<t>/Dockerfile`) has two stages. The `runtime` stage holds the expensive, slow-moving toolchain; the `release` stage holds uzi's source snapshot, `BUILD_INFO`, the version stamp, labels and the entrypoint. `release.yml` publishes the runtime stage as `ghcr.io/vtmocanu/uzi/agent-runtime-<t>:<input key>` only when that key is absent, and builds the release stage on **every** tag on top of the runtime base's verified digest. A release image is never re-tagged from another release.

## Context

`publish-agent` used to skip the build when the agent runtime paths were unchanged since the previous `v*` tag and re-tag the previous release's digest (the PRD #422 cost-saver). The re-tagged image kept its baked `UZI_AGENT_VERSION`, so with RC-first releases (ADR-1265) every stable promote shipped an agent image reporting its last RC (`agent-base:0.83.1` reported `0.83.1-rc.1+g5472f70`). Hosted workers were masked by ADR-738's `settled` softener; a self-managed worker has no controller signal, so its upgrade badge read `outdated` forever (#1682).

Dropping the re-tag alone fixes the stamp but rests the cost control on registry-cache hits and on keeping the release build args below the expensive layers by convention. A cache miss would silently bring back the full nix/Chromium build.

## The decisions

### D1: Release images never alias another release

The release stage is built on every tag, stamped `UZI_AGENT_VERSION=<tag>+g<short sha>`, with matching `org.opencontainers.image.version` / `.revision` labels and `/opt/uzi-src/BUILD_INFO`. The `assert-agent-version` job (`scripts/agent-image-assert.sh`) proves each published image reports exactly the version it is tagged with, on a signed runtime base, and gates `publish-chart`. The publish job itself exports `BUILD_INFO` from the pushed digest and checks its commit.

### D2: The runtime base is addressed by an input key that covers every runtime-stage input

`scripts/agent-runtime-key.sh` hashes, NUL-framed: a schema version, template, platform, the runtime stage's Dockerfile text verbatim (parent digest, `ARG` defaults, `RUN` lines), the template's `Dockerfile.dockerignore`, and each `COPY` source's `mode type oid` at `HEAD`. **Invariant: a change to any input of the runtime stage changes the key.** The script fails closed on anything it cannot account for (`ADD`, `COPY --from`/JSON/heredoc/globs/variables, `RUN --mount`, a second `FROM`, a global `ARG`, an `ARG` without a default other than `TARGETARCH`/`TARGETPLATFORM`, an unknown platform) and on any input that differs from `HEAD` (staged, unstaged, untracked or ignored). Extend the parser for a new form; never widen the refusal. `task test:agent-runtime-key` (in `gate:repo`) pins each class.

The key is an **input identity, not a reproducibility guarantee**: steps that resolve packages at build time can produce different bytes for the same key.

Content addressing was chosen over a committed digest pin because a pin needs a second PR for every base change (publish, then bump); with the key, a PR that changes a runtime input builds both stages in one run and nothing is pinned by hand.

### D3: Reuse requires proof; absence requires the registry to say so

`scripts/agent-runtime-resolve.sh` reuses a base only when the tag resolves, the resolved digest carries a keyless cosign signature from this repo's `release.yml` on a `v*` tag, and its labels name exactly this key, template and platform; the release build then consumes that same digest. It builds only on the registry's exact `ERROR: <ref>: not found` answer (an authenticated lookup returns it for a missing tag and for a not-yet-created repository). Any other failure fails the release.

### D4: Publication is serialized per template; multiple outputs per key are acceptable

`publish-agent` has job concurrency per template, so resolve, build and sign are not interleaved between two release runs (a promote pushes two tags). Should two digests ever exist for one key, that is acceptable by design: each release consumes the digest it verified, and the key tag names one of them.

### D5: Everything else builds the Dockerfile unchanged

The `release` stage is the last stage, so compose, the e2e harness, KinD, `devbox-update.yml` and PR CI build both stages from the same file as before, with no base argument. Only `release.yml` swaps the runtime stage for the published digest (`--build-context runtime=docker-image://<repo>@<digest>`, which replaces the stage outright; verified on buildx 0.33). PR CI stays cache-read-only, so no registry credential reaches a PR build.

### D6: The runtime/release boundary

The runtime stage keeps `npm ci` and the agent-browser build guard: the guard's headless Chromium launch creates a root-only directory inside `/nix` that the following `chmod -R a+rX /nix` repairs, and moving the guard below the boundary would move that recursive chmod over the whole store into every release. A change to `agent/package.json` or its lockfile therefore produces a new base (a warm registry cache limits the rebuild to the npm layer and below; a cold one pays the full stage). That is accurate, since the base changed, and it is the cost of this boundary. Moving npm below it needs a targeted permission fix for the guard-created directory instead of the recursive chmod.

## Consequences

- Every release pays for the thin release stage; a release whose runtime inputs are unchanged builds nothing else, whether or not the layer cache hits.
- `ADR-738`'s `settled` softener stays: images published before this change are still re-tagged aliases, and a hosted fleet may be pinned to one.
- The fleet-roll decision stays separate (`scripts/worker-tag-autobump.sh`, which now also counts `agent/codex`): publishing an accurately stamped image on every release does not roll hosted workers.
- New GHCR packages `agent-runtime-base` and `agent-runtime-jvm` appear on the first release after this change. They are build inputs; workers never pull them.
