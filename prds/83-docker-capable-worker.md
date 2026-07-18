# PRD #83: Docker-capable worker (rootless DinD) — testing containerized projects without host-root

**GitLab Issue**: [#83](https://gitlab.example.com/vtmocanu/uzi/-/issues/83)
**Status**: Draft (created 2026-07-18)
**Priority**: Medium
**Depends on**: PRD #4 (worker runtime — the `agent` service, register/claim protocol); PRD #18 (worker templates — the `agent/templates/<name>/Dockerfile` mechanism + `WORKER_TEMPLATE` + declared-vs-reported drift badge this PRD reuses); PRD #51 (worker/runner uid split — the containment this design must not weaken); **PRD #84 (capability-aware scheduling & plan-gate — how a docker-needing run is routed to a docker worker and gated when none exists. #83 registers `docker` as a capability and consumes #84's Match/Gate, so a docker worker is only *reachable* once #84 M2–M4 land; see Decision 9).**
**Related / cross-PRD**: PRD #58 (hosted k8s workers — its Decision 6 pins hosted pods to PodSecurity `restricted`. It is **shipped**: fully implemented and released in v0.3.0, hosted workers live on dev-cluster (`release(v0.3.0): #58 … turn hosted workers on for dev-cluster`). The k8s half of this PRD extends that in-production controller, sequenced after the compose track for scope — see Decision 8). PRD #42 (worker concurrency/sizing — a DinD daemon is a new memory/disk/CPU consumer that the sizing formula must account for). Issues #79/#82 (worker `/nix` store + tool-provisioning disk/egress) are adjacent: DinD adds a second large writable store with the same disk-pressure and egress questions.

## Problem

The worker has no Docker, by design. The `base`/`jvm` images ship `git + bash + node` plus nix/devbox for per-run CLI tools (PRD #18), and the runtime is deliberately locked down: `cap_drop: ALL`, `no-new-privileges:true`, non-root `worker` uid (10001), **no `docker.sock`** (`specs/ai.md` §36: the worker *"never listens. No `docker.sock`"*). Tool provisioning is rootless/daemonless (nix/devbox) precisely to need neither Docker nor root.

That is fine until a user's PRD project **is** a Docker/Compose project — and the canonical instance is uzi dogfooding itself: `./e2e/run-e2e.sh` and `./scripts/smoke.sh` both require `docker compose up`. An agent working a uzi PRD in a uzi worker cannot run uzi's own pre-merge gates. More broadly, any repo whose tests shell out to `docker`/`docker compose`/`testcontainers` is unrunnable on a worker today, with no supported path — the agent just hits "command not found".

We want workers that **can** run Docker for those repos, **without** discarding the property that makes uzi safe: the worker runs untrusted, agent-driven code, so it must never gain host-root-equivalent access (on compose: the user's laptop; on k8s: the node).

## Inspiration check

| Aspect | multica | bottega | coder (+ `myorg/k8s/coder`) | uzi today | uzi (this PRD) |
|---|---|---|---|---|---|
| Agent isolation | **None** — daemon on the user's machine runs the host `claude`/`codex` CLI in a host worktree (`SELF_HOSTING_AI.md`; `pkg/protocol/messages.go:74`) | Fixed worker deps; host tools assumed | Workspace = a container/pod; runs arbitrary user code by design | Containerized worker, `cap_drop: ALL`, worker/runner uid split (#51), no `docker.sock` | unchanged — this PRD does not relax the base worker |
| How it gets Docker | **Free** — the agent is a host process with full host docker/PATH/creds | n/a | Sysbox RuntimeClass · Envbox · rootless Podman · privileged sidecar | n/a | **Rootless DinD sidecar** (a separate daemon/container), opt-in per worker |
| Cost of that choice | Zero isolation: docker access == full host access. The model uzi rejected | No sandbox story | Secure options need node-level infra (RuntimeClass/DaemonSet) or a privileged pod | — | Contained to the DinD userns; no host-root; keeps `no docker.sock` |

**Reading of the field.** *Multica proves the cheap path and its price*: you get Docker for free by giving the agent your whole machine — full host docker, PATH, and credentials, no sandbox. Copying it would delete uzi's trust boundary, so it is an anti-pattern here, not a template. *Coder is the only prior art with uzi's actual problem* (an isolated workspace that nonetheless needs Docker) and it documents exactly four ways to solve it, which bound our option space:

- **Privileged DinD sidecar** / **host `docker.sock` mount** — coder's own docs call the privileged sidecar *"insecure … workspaces will be able to gain root access to the host machine"*; the socket mount is host-root by definition. **Both are disqualified for uzi** — the worker runs untrusted code, and `no docker.sock` is a recorded spec invariant.
- **Sysbox RuntimeClass** — the cleanest posture (unprivileged pod, userns isolation) but needs Ubuntu nodes + a `sysbox-deploy-k8s` DaemonSet + a `sysbox-runc` RuntimeClass, and the project is fading (Nestybox folded into Docker; no release since ~2025-05). A heavy, node-level bet on a slowing dependency. Documented here as an **alternative**, not the default.
- **Envbox** — bundles Sysbox but the outer pod is `privileged: true` with host `/lib/modules` mounted (`coder/coder examples/templates/kubernetes-envbox/main.tf:158`). A privileged pod on a shared cluster. Rejected as default.
- **Rootless Podman / rootless DinD** — no host root, no RuntimeClass; needs `/dev/fuse` (coder uses a `smarter-device-manager` DaemonSet), cgroup v2, userns. **This is the option that preserves uzi's "no host root" invariant**, and it is philosophically the same rootless/daemonless stance as our nix/devbox provisioning.

So this PRD takes **rootless DinD as a sidecar** as the primary mechanism on both compose and k8s, with Sysbox as a documented escape hatch for operators who already run it.

## Solution Overview

Docker is an **opt-in worker capability**, never in `base`, delivered by two coupled pieces:

1. **A `docker` worker template** (`agent/templates/docker/Dockerfile`, mirroring `jvm`'s "base + a stack" shape from PRD #18): the base image plus the **Docker CLI + compose/buildx plugins only** — no daemon in the worker image. It reuses #18's declared-vs-reported drift badge; `UZI_WORKER_TEMPLATE=docker` is self-reported at register.
2. **A rootless DinD sidecar** that provides the actual daemon, wired to the worker via `DOCKER_HOST`. It is a **separate container with its own mount namespace**, so containers it runs cannot see the worker's filesystem — in particular they cannot reach the join-token file, the decrypted forge PAT, or the Anthropic token. Rootless ⇒ no host root. No host `docker.sock` is ever mounted.

Delivery is phased so the immediately useful part does not block on unfinished work:

The two are **delivery tracks**, not product/chart versions:

- **Compose track (this PRD's core, ships first).** An opt-in `dind` sidecar service in `docker-compose.yml`, profile-gated, isolated from the user's real Docker daemon. Fully usable on a laptop; unblocks uzi dogfooding. Self-contained — depends on nothing that is not already landed.
- **k8s track (designed here, follows the compose track).** A distinct, opt-in **docker-capable worker type** provisioned by #58's (shipped, in-production) controller into a **separate, non-`restricted` namespace** with a rootless-DinD pod sidecar and a `/dev/fuse` device DaemonSet. Explicitly a weaker, quota-gated tier — never the default, never privileged, never sysbox-required. Extends #58's live controller; held until the compose track ships only because compose delivers the whole user-visible win with no #58 coupling. (Awareness, not a blocker: hosted provisioning has a live bug, #82.)

The base and `jvm` workers, and the default hosted-worker posture, are **unchanged**. A worker only gains Docker if its owner (and an admin quota/policy) opts it in.

## Design Decisions

1. **Docker is a capability of a template, not a tool tier — because it needs a daemon, unlike everything nix provisions.** PRD #18 draws a clean line: per-repo CLI tools come from devbox/nix (Tier 1–3), and image templates exist *"only for heavy or system-level dependencies that devbox/nix handles poorly."* Docker is the archetype of the latter: the CLI is inert without a running `dockerd`, and a daemon is a *process + devices + namespaces*, not a package. So `docker` is a template (like `jvm`), and the daemon rides alongside as a sidecar — it can never be a nix package in the shared provisioning path.

2. **Rootless DinD sidecar, never privileged, never the host socket.** The worker runs untrusted agent-driven code; any host-root-equivalent grant turns a prompt-injected agent into node/laptop compromise. That rules out the privileged sidecar (coder: *"insecure"*) and the `docker.sock` mount (host-root; and `specs/ai.md` §36 records **no `docker.sock`** as an invariant — this PRD keeps it). Rootless DinD confines the daemon and its containers to a userns-mapped unprivileged uid: strong isolation, no host root, and the same rootless stance as our nix/devbox engine.

3. **The DinD daemon is a SEPARATE container (own mount namespace) — this is the load-bearing security property.** If Docker ran inside the worker container, the agent could `docker run -v /run/secrets/worker_token:/x` (or `-v /data:/x`) and read the join token, the decrypted PAT, and the Anthropic token straight out of the worker's filesystem — re-opening exactly what the #51 uid split and the M6 `/proc` hardening closed. Because the daemon lives in its own container, `-v <path>` binds **the DinD container's** filesystem, which holds none of that. A worker-level test pins that a container started through the sidecar cannot read the token path. (Corollary: the DinD sidecar must not itself mount the worker's secret or `/data`.)

4. **Guardrails: deny `docker` everywhere by default; allow it only in the `docker` template.** The PreToolUse deny-hook (`agent/src/guardrails.ts`) is the worker's defense-in-depth layer under `bypassPermissions`. On non-docker workers, `docker` is not installed, but the bash screen denies it anyway (symmetry with the existing push/secret/`/proc` denies, and the tokenizer already defeats `sh -c`/`git -C`-style evasion). On a `docker` worker, `docker` is allowed, but **every existing deny stays** — `git push`, secret-mount reads, `/proc` snooping are unaffected, and Decision 3's container-can't-read-secrets test is what bounds the new surface. Adding Docker is a guardrail change and gets guardrail-review scrutiny (PRD #18 Decision 6's rule: any tool that can push or mutate remotes is reviewed — Docker can `docker push`, so image-registry egress is considered in M3).

5. **Opt-in per worker, gated by admin quota/policy — never silent.** A user does not get Docker by accident: the capability is chosen at join-token issuance (reusing #18's per-worker declared template) and, for hosted workers, bounded by an admin policy/quota (reusing #58's per-user quota + admin settings surface). Rationale: a docker-capable worker is a materially larger attack surface and resource consumer; whether it is even offered is an operator decision, mirroring how #18 gates tool allowlists and #58 gates provisioning.

6. **Portability: one mechanism (rootless-DinD sidecar) on both compose and k8s.** `specs/human.md` requires the worker to *"work the same under docker-compose today and k8s later."* The sidecar shape satisfies that: a compose `dind` service and a k8s pod sidecar container are the same idea, differing only in platform wiring (a compose service + internal network vs. a pod sidecar + a `/dev/fuse` device plugin). We do **not** adopt sysbox/envbox as the default precisely because they are k8s-only and would fork the model.

7. **Hosted docker workers live in their OWN namespace, not a relaxed `restricted` one.** PRD #58 Decision 6 pins default hosted pods to PodSecurity `restricted`, and its threat model leans on the worker namespace being *empty* (nothing but zero-privilege workers) so a compromised controller reaches little. Rootless DinD needs relaxations `restricted` forbids (a `/dev/fuse` device, userns, a looser seccomp/AppArmor profile). Rather than weaken the shared namespace for everyone, docker-capable workers are provisioned into a **second, dedicated `baseline` namespace** with `automountServiceAccountToken: false` preserved and the same "nothing else lives here" discipline. The default `restricted` namespace and its uniform posture are untouched; the weaker tier is bounded, opt-in, and separately blast-radiused. (This is the concrete resolution of #58's noted collision: #58 keeps `restricted`; docker is a distinct tier, not a relaxation of the default.)

8. **The k8s track is sequenced after the compose track — a scope choice, not a missing dependency.** #58 is shipped (released v0.3.0, hosted workers live on dev-cluster), so the docker tier extends a real, in-production controller — nothing about it is blocked on #58. It follows the compose track only because that track already delivers the whole user-visible win — including uzi's own dogfooding — with zero #58 coupling, keeping this PRD's blast radius small. (A live hosted-provisioning bug, #82, is worth tracking but does not gate this work.)

9. **How a docker run *reaches* a docker worker — routing + the "you need docker" gate — is PRD #84, not this PRD.** #83 provides the capability (template + sidecar) and registers `docker` in #84's capability vocabulary; #84 owns Declare → Match → Gate: the static per-repo hint, the lead's plan-time inference, the capability-filtered claim query, and the plan-approval block when no eligible worker exists. Consequence: the compose track can build and run a docker worker image on its own, but a docker-*needing* run is only reliably routed and gated once #84 M2–M4 land — so #83's user-facing completeness depends on #84. The split keeps docker-the-capability separate from matching-any-capability (`jvm` has the same latent routing gap #84 fixes), which is why #84 is its own PRD rather than a milestone here.

## Technical Design

### 1. The `docker` worker template (agent + compose + docs)

- `agent/templates/docker/Dockerfile`: `FROM` the same pinned node base, **mirroring `templates/base` layer-for-layer** (git/bash/tini, the #51 worker/runner uid split + root-entry `setpriv` drop, the pinned nix/devbox stack, no secrets, the root-owned `0555` `/usr/share/uzi-git-nohooks`) and adding **only** `docker-cli`, `docker-cli-compose`, `docker-cli-buildx` (Alpine community). No `dockerd` in this image — the daemon is the sidecar. Same lockstep rule as `jvm`: base is the source of truth for common layers; a test pins the devbox/setpriv layers match.
- `ENV UZI_WORKER_TEMPLATE=docker` (hardcoded literal, per #18 — the drift signal, not the build arg echoed back).
- `DOCKER_HOST` is set at run time by the worker (see §2), not baked, so a `docker` worker with no sidecar fails loudly ("cannot connect to the Docker daemon") rather than silently reaching for a host socket.
- `docs/worker-setup.md`: how to select the `docker` template and bring up the sidecar; the trust note (rootless, isolated from your real Docker, no host socket).

### 2. Compose track: rootless DinD sidecar

- New service `dind` in `docker-compose.yml`, image `docker:<pinned>-dind-rootless`, **profile-gated** (`profiles: ["agent-docker"]`) so it only exists when a docker worker is wanted; the plain `--profile agent` path is unchanged.
- Wiring (choose in M2, both keep the daemon off the host and out of the worker's mount ns):
  - **Shared-socket-volume** (preferred): an `emptyDir`-style named volume mounted into both `dind` (daemon writes its rootless socket there) and `agent` (reads it); `DOCKER_HOST=unix:///…/docker.sock`. No TCP surface at all.
  - **TCP-on-internal-network** fallback: `DOCKER_HOST=tcp://dind:2375` on the compose-internal network only (never published). Simpler, but a listening port; used only if the rootless socket-share proves awkward.
- The `dind` service does **not** mount `agentdata`/`/data`, the `worker_token` secret, or anything of the worker's — Decision 3. Its storage is its own volume (`dinddata`), which is where built images/layers land (disk-pressure note → Risks).
- Sizing (PRD #42): the DinD daemon + build cache is a new slot-like consumer. `docs/worker-setup.md` gains a line: a docker worker budgets an extra ~1–2 GiB / ~1 CPU and a `dinddata` volume for images. `AGENT_MEM_LIMIT`/`AGENT_CPUS` guidance updated.
- The worker CLI (`api/cmd/uzi`) and web worker-registration flow surface the `docker` template choice (per the CLAUDE.md "new functionality ⇒ check the CLI" rule).

### 3. Guardrails (agent)

- `agent/src/guardrails.ts`: add `docker` to the default bash deny-list (defense-in-depth on non-docker workers), gated so the `docker` template's worker allows it. The gate is worker-config-driven (the claim/worker knows its template), not repo-driven.
- Keep all existing denies intact on docker workers (`git push`, secret-mount prefix, `/proc`). Consider `docker push` explicitly: image-registry egress is new; M3 decides whether to allow it (needed for repos that build+push in tests) or deny by default and allowlist per repo, and documents the choice.
- Tests: (a) `docker` denied on a non-docker worker even through `sh -c`/quoting evasion (extends the existing tokenizer tests); (b) **the Decision 3 invariant** — a container launched via the sidecar cannot read the worker's join-token path (the security test that justifies the whole design).

### 4. k8s track: hosted docker-capable worker tier (extends PRD #58's shipped controller)

- A new hosted **worker type** ("docker") the #58 controller can provision, distinct from `base`/`jvm`. Materializes a Deployment whose pod carries: the worker container (`docker` image, `runAsUser: 10001`, `automountServiceAccountToken: false` — all #58 posture kept) **plus** a rootless-DinD sidecar container.
- Target namespace: a **second dedicated namespace** with PodSecurity `baseline` (not `restricted`), containing only docker-capable hosted workers. The controller's Role is extended to it with the same minimal verbs (#58 Decision 1); the empty-namespace discipline is preserved (no CNPG, no app secrets, no privileged SAs).
- `/dev/fuse` for rootless overlay: a `smarter-device-manager` (or equivalent) DaemonSet exposes the fuse device as a schedulable resource (coder's documented approach); the pod requests it. No privileged container, no host `docker.sock`, no RuntimeClass required.
- **Documented operator alternative**: if the cluster already runs Sysbox, an operator may instead provision docker workers as an unprivileged pod with `runtimeClassName: sysbox-runc` (strongest posture). This PRD does not require it and does not install sysbox; it is a config path in the docs.
- Admin policy/quota (reusing #58's per-user quota + admin settings) gates whether docker workers may be provisioned at all, and how many.

### 5. Docs + specs

- New `docs/worker-docker.md` (audience: user): what a docker worker is, that it is rootless and isolated from your real Docker, that it has no host access, resource cost, and the compose bring-up. Leading-fence frontmatter per `docs/README.md`.
- `docs/worker-setup.md`: the `docker` template + sidecar; sizing delta.
- `specs/ai.md`: new section — docker-capable worker (template + rootless-DinD sidecar; the separate-mount-ns secret invariant; the `no docker.sock` invariant explicitly **preserved**). `ARCHITECTURE.md`: worker paragraph (optional DinD sidecar), worker egress (image pulls from registries — a new outbound set to document alongside the nix substituters #18 already added).
- `specs/human.md` is user-stated — **not edited without approval**; if any human-facing requirement needs to change, it is raised, not edited here.

## Milestones

Phase analysis (per the parallel-milestone workflow):

| Phase | Milestone | Depends on | Touches | Parallelizable? |
|---|---|---|---|---|
| 1 | **M1** `docker` template image + guardrail deny-by-default | #18, #51 (landed) | `agent/templates/docker/`, `agent/src/guardrails.ts`, tests | M1 first (foundational) |
| 2 | **M2** compose rootless-DinD sidecar + `DOCKER_HOST` wiring + e2e | M1 | `docker-compose.yml`, `agent/src` (DOCKER_HOST), `e2e/` | parallel with M3 after M1 |
| 2 | **M3** guardrail allow-in-docker-template + Decision-3 secret-reachability test + `docker push` policy | M1 | `agent/src/guardrails.ts`, tests, `ARCHITECTURE.md` | parallel with M2 after M1 |
| 3 | **M4** hosted-k8s docker worker tier (separate `baseline` ns, DinD pod sidecar, fuse DaemonSet, quota) | #58 controller (shipped, v0.3.0); this PRD M1–M3 | `controller/`, `deploy/` chart, admin surface | after compose track (extends #58's shipped controller) |
| 3 | **M5** docs + specs + CLI/web surfacing of the docker capability | M2 (compose usable) | `docs/`, `specs/ai.md`, `api/cmd/uzi`, `web/` | after M2 |
| 3 | **M6** CI validation build of the `docker` template image | M1 | `.gitlab-ci.yml` | after M1 |

- [ ] **M1 — `docker` template + deny-by-default guardrail.** `agent/templates/docker/Dockerfile` (base + docker-cli/compose/buildx, mirroring `jvm`); `UZI_WORKER_TEMPLATE=docker`; register/drift reuse from #18; `docker` denied on non-docker workers with evasion-resistant tests.
- [ ] **M2 — Compose rootless-DinD sidecar.** Profile-gated `dind` service, socket-share (or internal TCP) wiring, `DOCKER_HOST` set by the worker; e2e: a run can `docker compose up` a toy project **and** (security) a container it starts cannot read the worker's token path.
- [ ] **M3 — Guardrail allow-in-template + secret-reachability test + registry-egress policy.** Docker allowed only on docker workers; all existing denies intact; `docker push` decision documented; ARCHITECTURE.md egress updated.
- [ ] **M4 — Hosted k8s docker worker tier (after the compose track).** Extends #58's shipped, in-production controller to provision a docker worker type into a dedicated `baseline` namespace with a rootless-DinD pod sidecar + fuse DaemonSet; admin quota gate; sysbox documented as an operator alternative.
- [ ] **M5 — Docs, specs, surfacing.** `docs/worker-docker.md` (audience: user) + `worker-setup.md` sizing; `specs/ai.md` section; CLI + web expose the `docker` template choice.
- [ ] **M6 — CI.** Kaniko validation build of the `docker` template image in `.gitlab-ci.yml`.

## Risks & Open Questions

- **Rootless DinD limitations.** Needs cgroup v2 and userns; some images that themselves require privileges (nested `--privileged`, certain mounts) will not run. Acceptable — the target is "run this repo's compose/testcontainers tests," not "arbitrary privileged Docker." Documented in `docs/worker-docker.md`. Overlay perf via fuse-overlayfs is slower than native; a known cost.
- **Disk & memory pressure.** A `dinddata` image/layer store is a second large writable volume alongside `/nix` (#79/#82 territory): pulled base images and build cache grow unbounded without GC. M2/M5 must state a retention/GC story and the sizing delta; on k8s (M4) it interacts with #58's PVC presets.
- **Guardrail surface.** Docker is a real second execution surface. The design bounds it (rootless, separate mount ns, containers can't reach worker secrets — Decision 3's test), but `docker push` / registry egress and outbound image pulls widen the network surface; M3 decides the default (deny + per-repo allowlist vs. allow) and documents it.
- **`docker compose` vs Compose V2 in the sidecar.** The worker CLI ships the `docker compose` plugin; ensure the toolset matches what target repos expect (compose v2 subcommand, not the legacy `docker-compose` binary) — pin and test in M1.
- **Open: default for `docker push`?** Recommendation: **deny by default, per-repo allowlist**, consistent with #18's opt-in posture — resolve in M3.
- **Open: k8s device plugin choice** (`smarter-device-manager` vs. a generic fuse device plugin) and whether dev-cluster nodes support unprivileged userns + fuse — a prerequisite check for M4, gated behind #58 anyway.

## Decision Log

- **2026-07-18 (created).** Prompted by "we need docker to test docker stuff" (uzi's own e2e/smoke need `docker compose up`, unrunnable on a worker). Investigated coder's four documented approaches (Sysbox RuntimeClass / Envbox / rootless Podman / privileged sidecar) against uzi's trust model, and multica's model (host daemon + host CLI, docker "free" via zero isolation — an anti-pattern here). Chosen mechanism: **rootless-DinD sidecar as an opt-in worker capability**, primary on both compose and k8s; privileged and host-socket paths disqualified (untrusted code + the recorded `no docker.sock` invariant); sysbox/envbox documented as k8s-only alternatives, not the default (node-level infra + a fading dependency). Phased into two delivery tracks (not chart versions): the **compose track** (self-contained, unblocks dogfooding) ships first; the **k8s track** follows — extends #58's shipped controller (released v0.3.0, live on dev-cluster), held only until the compose track ships — and is resolved as a **separate `baseline` namespace tier**, not a relaxation of #58's default `restricted` posture.
