# PRD #58: Hosted k8s workers — self-service worker provisioning

**GitLab Issue**: [#58](https://gitlab.example.com/vtmocanu/uzi/-/issues/58)
**Status**: Draft (created 2026-07-16; revised same day after parallel design review / security audit / fact-check — mechanism changed from api-managed Deployments to a dedicated controller, TLS on the worker hop pulled into scope, v1 admin surface trimmed)
**Priority**: Medium
**Depends on**: PRD #18 (worker templates), PRD #52 (k8s deploy via Helm/ArgoCD), PRD #42 (concurrency semantics reused for sizing).
**Related**: PRD #51 (uid-split; its k8s remote-worker phase and this PRD's pod spec must be one design when both land). PRD #32/#45 (vault — H1 below is why join tokens get delivered-once semantics).

## Problem

Every user must run their own worker by hand: register in Settings → Workers,
copy a join token, then run the container themselves (compose `--profile agent`
or a standalone `docker run`). On the k8s deployment (dev-cluster) this is pure
friction: the cluster is right there and could host workers for users who want
one, while today each user needs a laptop/VM with Docker and the discipline to
keep it running.

## Solution Overview

Opt-in **hosted workers** on k8s. A user clicks "Provision worker" in
Settings → Workers, picks a **worker type** (image template: `base`, `jvm`, …)
and a **size** (S/M/L presets), and a dedicated **worker controller**
(a new, small deployment — never the `api`) materializes one k8s Deployment for
it in a dedicated worker namespace, delivering the join token as a file-mounted
Secret. The worker registers over the normal join-token flow and shows up
online like any other worker. Users can have several hosted workers (different
types), bounded by an admin-set per-user quota, alongside any external workers
they still run by hand.

This implements (a static-provisioning subset of) the deferred design already
recorded in `specs/human.md` ("a dedicated operator — never the api, which must
never hold container-runtime credentials — spawns worker pods") and
`specs/ai.md` §168. The api keeps **zero** kube-apiserver access
(`automountServiceAccountToken: false` stays). v1 provisions persistent
workers on user request; spawn-on-queued-work stays deferred.

No prior art in the inspiration submodules (bottega, multica, dot-agent-deck —
none do in-cluster worker provisioning); the bar is this repo's specs and
existing code style.

Non-goals (v1):

- No docker-compose hosting; laptop users keep the manual flow.
- No new worker trust model: the join token stays the sole worker trust anchor;
  a hosted worker is an ordinary worker whose container the controller runs.
- No autoscaling / scale-to-zero / spawn-on-demand; a hosted worker runs until
  deleted.
- No preset CRUD, no restart endpoint (delete + reprovision), no pod-phase
  status in the UI (heartbeat-only; `kubectl` is the diagnostic) — all v2.

## Design Decisions

1. **Dedicated controller, not the api.** A new `controller` component (own
   image, own Deployment) holds the only kube-API credentials: a ServiceAccount
   with a Role scoped to a **dedicated worker namespace** that contains nothing
   but hosted-worker objects (no CNPG, no Infisical-materialized Secrets, no
   privileged ServiceAccounts). Rationale: a Role that can create pods in a
   namespace can mount any Secret and impersonate any ServiceAccount in it —
   RBAC cannot scope `create` by label — so the api holding that power would
   both violate the recorded spec constraint and turn any api compromise into
   namespace-wide secret exfiltration. The controller model closes this
   structurally; the empty namespace bounds what even a compromised controller
   reaches. Verbs pinned minimal: Deployments create/get/**list**/patch/delete;
   Secrets **create/delete only** (it writes them, never needs to read them
   back); PVCs create/**list**/delete; Pods get/list (drift detection only).
   - **The Secrets line is load-bearing and stays verbatim** (validated
     2026-07-16 by the M1 review wave, where two of three validators
     independently found M1's first protocol violating it). k8s has no
     existence-only verb for Secrets: `get`/`list` returns contents, and RBAC
     cannot scope it to the dynamic set of hosted-worker Secrets via
     `resourceNames`. Granting either would convert a controller compromise from
     "harvest the tokens that happen to flow through the compromise window" (the
     controller is stateless and retains nothing) into "harvest every hosted
     worker's join token in one call, including every pre-existing one" — each
     token being worker impersonation → claim that owner's runs → receive their
     decrypted forge PAT and Anthropic token. Decision 3's proof-of-possession
     ack is what makes never reading Secrets back achievable; the two are one
     decision.
   - **`list` on Deployments and PVCs added 2026-07-16** (architect pass): the
     original list was under-drawn and Decision 9 could not work without it.
     Orphan detection means *enumerating objects whose names you do not expect*,
     which `get`-by-name cannot do, and `Pods get/list` does not cover it (an
     orphan Deployment scaled to zero has no pods; orphan PVCs are invisible
     entirely) — while the Risks section already promises "orphan sweep flags
     leftovers" for PVCs. This widening leaks nothing: a Deployment references
     its Secret by name, it does not embed it. That asymmetry is the whole point
     — listing Deployments and PVCs is boring, listing Secrets is fleet-wide
     token disclosure. Orphan **Secrets** therefore stay undetectable, which is
     the accepted trade, not a gap to close.
2. **Controller ⇄ api protocol: outbound-only poll, like a worker.** The
   controller authenticates to the api with its own bearer credential
   (hash-stored, chart-provisioned) and polls a controller-facing endpoint for
   desired state (hosted-worker rows: id, template, size, generation). No
   inbound port on the controller; the api never dials it. The controller is
   stateless — desired state lives in the DB, observed state in the cluster; it
   reconciles the two on every poll, so api or controller restarts lose nothing.
3. **Join token: delivered once, never at rest in plaintext server-side.** At
   provision the api generates the token, stores the sha256 (as today), and
   keeps the plaintext secretbox-sealed as a **delivery buffer** until delivery
   is *proven*, then destroys the sealed copy. The
   controller writes it into the worker's k8s Secret, mounted as a **file**
   consumed via `UZI_WORKER_TOKEN_FILE` — never a `secretKeyRef` env var, which
   would reopen the `/proc/<pid>/environ` leak that compose hardening (M6,
   `docs/proc-hardening.md`) closed. Residual, documented in the threat model:
   the token is plaintext in etcd for the worker's lifetime; anyone who reads
   it can impersonate that worker and claim its owner's runs (receiving the
   decrypted Anthropic token when the vault is unlocked). Bounded by the empty
   worker namespace + minimal RBAC; per-worker rotation is v2.
   - **Delivery is proven by worker registration, not asserted by the
     controller** (settled 2026-07-16 by the M1 review wave; see the Decision
     Log). The api destroys the sealed copy when a worker **authenticates with
     the token** — `RequireWorker` already resolves `sha256(presented)` against
     `workers.token_hash`, so a successful auth *is* proof the token arrived and
     works. The controller makes no delivery claim: there is no `Materialized`
     field and the poll is a plain GET (with `Cache-Control: no-store`, since
     the response carries plaintext tokens and a GET, unlike the former POST, is
     cacheable by any future mesh or sidecar). Three properties fall out for
     free: it is **unforgeable** (only a holder of the current token can trigger
     it); it is **version-exact** without any wire-level discriminator (after a
     rotation to T2, a pod still holding T1 simply fails auth and therefore
     cannot ack T2 away); and it requires **no Secret read**, which is what lets
     Decision 1's Secrets line stay verbatim. The deeper reason to prefer it:
     any controller-asserted ack keeps the controller in the trust path for
     destroying the api's only plaintext — refining a report from the component
     whose blast radius this PRD exists to bound is strictly weaker than not
     needing the report.
   - **The pending-token TTL means "no pod ever proved it booted"**, not "the
     controller never picked it up". The window grows from ~2 poll intervals
     (20-60s) to minutes — pod scheduling + image pull + container start +
     register — which on a cold node pulling a multi-GB `jvm` image is one to
     two orders of magnitude larger. 1h is kept anyway, because **expiry is
     benign whenever the Secret was written**: expiry destroys only the api's
     *buffer*; the plaintext is already in the cluster and `workers.token_hash`
     is unchanged, so a pod that finally boots (after the operator clears an
     ImagePullBackOff, a ResourceQuota rejection, an unbindable PVC) reads the
     file, authenticates, registers, and the ack fires late and correctly. Expiry
     therefore strands a worker only when the Secret was **never** written —
     exactly what the TTL is for — and a late registration flipping an expired
     row to delivered is a correct **self-heal**, not a laundered signal.
   - **Second residual, distinct from the etcd one above and not previously
     recorded** (auditor, 2026-07-16): while pending, the sealed copy sits in
     Postgres under `UZI_SECRET_KEY` — precisely the key the vault (PRD #32)
     exists to stop relying on. Per `docs/vault-threat-model.md`, an operator who
     reads the api env plus the DB holds every master-sealed value in plaintext.
     The TTL is what bounds this: without it, a controller that never polls (chart
     not deployed, controller down, hosting disabled after provisioning, worker
     abandoned) would leave "delivered-once" quietly degraded to "at rest
     indefinitely". The expiry sweep therefore runs **unconditionally**,
     regardless of `WORKER_HOSTING_ENABLED` — a stack that provisioned workers and
     then disabled hosting is exactly the case that strands ciphertext.
4. **TLS on the worker/controller→api hop, in scope for v1.** Claim responses
   carry the decrypted forge PAT and Anthropic token; dev-cluster is a shared
   cluster running arbitrary dev pods, so that plaintext does not cross the pod
   network. The api gains an optional TLS listener (cert-manager-issued cert,
   `API_TLS_CERT/KEY`); hosted workers and the controller use `https://` to it
   and verify the cluster-issued CA. Workers hit the api directly (no nginx in
   the path), so XFF trust is unaffected: `TRUSTED_PROXIES` still only covers
   web pods, and the api sees the worker's real peer address.
5. **NetworkPolicies, both directions, default-deny.** (a) The existing api
   ingress policy (`api-networkpolicy.yaml`, today web-pods-only — it would
   silently break this feature) gains **TWO** rules: **controller pods** (release
   namespace) and **worker-namespace pods**, each matched by namespace + pod
   label selector, to the TLS port only. (b) The worker
   namespace gets default-deny ingress (nothing dials a worker) and
   default-deny egress with explicit allows: DNS, api (TLS port), forge, nix
   substituters, Anthropic API — explicitly excluding the kube-apiserver
   ClusterIP and the cloud metadata IP (169.254.169.254). FQDN egress rules
   need CNI support (Cilium/Antrea) — M3 verifies what dev-cluster's CNI can
   express; fallback is a maintained CIDR allowlist with the gap documented as
   an accepted residual, not silently dropped.
   - **CORRECTED 2026-07-17 (M3): (a) said ONE rule and named the wrong half. The
     omission was a LIVE BUG, not a doc slip** — the controller runs in the release
     namespace and polls the api, so under the existing default-deny (web pods only)
     **every controller poll was dropped**. Nothing had caught it because the chart
     shipped no controller until M3. Fixed in the same commit as the text. Both rules
     select the port via the `uzi.workerAPIPort` helper rather than hardcoding 8443,
     so the policy is correct before and after M4's TLS rollout.
   - **CNI question ANSWERED (M3, 2026-07-17): dev-cluster CAN express FQDN egress,
     so the CIDR-allowlist fallback is NOT needed.** Antrea v1.13.3, the
     `AntreaPolicy` feature gate true on both agent and controller, the CRDs present,
     and a server-side dry-run of the real policy accepted through the live
     `annpvalidator` webhook. Three consequences that shaped the split, each verified
     rather than assumed: **FQDN cannot be used for in-cluster destinations** (AntreaProxy
     rewrites a ClusterIP before enforcement), so DNS and the api are selector-based in
     the k8s policy; **DNS egress must be allowed** or every FQDN rule silently matches
     nothing (Antrea learns the IPs by snooping DNS responses); and **the explicit Drop
     belt is NOT redundant** with default-deny, because an FQDN allow programs whatever
     IPs the DNS answer carried — so a poisoned answer for an allowed name would open
     the metadata IP *through* the allow rule. Drops go first, in the same policy.
   - **The enforcement gap stays OPEN, and M6 inherits it.** Capability is proven;
     **no packet has crossed**. Behaviour was established by reading the Antrea v1.13
     source, not by running a probe pod on the shared cluster. **kind cannot close it**:
     kindnet implements no NetworkPolicy at all — not the ANNP and not even the
     default-deny floor — so the policies are created and silently never enforced there.
     A green kind run says nothing about this. Do not record it closed until a real
     hosted worker on dev-cluster either reaches `gitlab.example.com` or does not.
6. **Worker pod posture.** Dedicated zero-permission ServiceAccount with
   `automountServiceAccountToken: false`; `runAsNonRoot`, drop ALL caps,
   `allowPrivilegeEscalation: false`, default seccomp; namespace labeled
   PodSecurity `restricted`; resources from the size preset;
   `imagePullSecrets` from chart values (same Harbor robot pattern as
   api/web). `strategy: Recreate` — RollingUpdate would Multi-Attach-deadlock
   against the RWO PVC. **v1 pod is single-container at `runAsUser: 10001`**
   (PRD #51 M2's `worker` uid), **with no uid split.** PRD #51's A1 mechanism
   (root entry + `setpriv` drop, retaining ambient `CAP_SETUID`/`CAP_SETGID`)
   is the *compose* mechanism and cannot run under PodSecurity `restricted`,
   which forbids a root entry and admits no capability beyond
   `NET_BIND_SERVICE`. Per PRD #51's own Decision 8 ("align at the distinct-uid
   abstraction, not the mechanism"), the k8s split is its (C)/two-container
   form and lands with PRD #51's k8s phase, which *adds* the `runner` container
   (uid 10002) to this pod — the v1 spec is additive-compatible with that, not
   a placeholder to be rewritten. **Hard dependency on PRD #51 M2:** its
   `agent/templates/entrypoint.sh` must skip the drop (and the B4 volume chown)
   when started non-root, or this pod CrashLoopBackOffs at `setpriv --reuid`
   (EPERM without `CAP_SETUID`). Requirement relayed 2026-07-16 while that file
   was still uncommitted WIP.
   - **ACCEPTED AND IMPLEMENTED by PRD #51 — verified 2026-07-16 on their branch
     (`fbd916c`), NOT yet on `main`.** Their shared `agent/templates/entrypoint.sh`
     (one file, so `base` and `jvm` both inherit it) reads the uid via an absolute
     `id` and, when non-root, logs "single-uid non-root mode (PRD #58)" and
     `exec`s tini directly — no volume migration, no token chmod, no `setpriv`
     drop. It cites this PRD by name and by our reasoning (fresh PVC + `fsGroup`,
     and the image-layer ownership `/nix=worker:runner` + `/data=worker:worker`
     already lets uid 10001 write). The image's full PATH is kept on that path, so
     nix/devbox and the jvm JDK still resolve. They added two hardenings we did not
     ask for: the uid is **validated as a clean non-empty number before branching**
     so both paths fail closed under `set -eu` (a garbled `id` must never let a
     *root* start slip into the non-root branch, which would run root single-uid
     with the token unhardened and the full `cap_add`), and `UZI_UID_SPLIT` /
     `UZI_RUNNER_PATH` / `UZI_RUNNER_TMPDIR` are unset there, so a stray
     `UZI_UID_SPLIT=1` in a non-root deploy cannot EPERM every runner spawn into a
     DoS.
   - **Consequence for M3's sequencing: this is now a merge-order dependency, not a
     design risk.** M3 must build against an agent image that contains #51's
     entrypoint, i.e. **#51 must land first**, or M3 pins a pre-#51-M2 image tag
     (whose `USER uzi:uzi` is non-root anyway and needs no conditional). Re-verify
     the branch still carries the non-root branch before M3 starts rather than
     trusting this note: #51 is in flight and its M4+ work is still moving.
   Posture disclosed: a hosted worker carries PRD
   #51's same-uid residual exactly as today's compose worker does — hosting
   neither widens it (the residual is intra-user, `ARCHITECTURE.md:427-428`)
   nor closes it. **M3 MUST render `fsGroup: 10001` on the pod rather than
   relying on image-baked ownership** (architect pass, 2026-07-16). Two reasons,
   and it is what makes the additive claim above a property rather than an
   accident: (a) an RWO PVC mounts `root:root 0755`, so uid 10001 cannot write
   it, and the usual initContainer-chown escape needs root, which PodSecurity
   `restricted` forbids — so v1 needs `fsGroup` regardless; (b) `fsGroup`
   applies to **every** container in the pod, so when PRD #51's k8s phase adds
   the `runner` container at uid 10002 it inherits supplemental gid 10001 and
   the setgid/g+rw volume for free, with no pod-spec rework. **Open question
   owned by PRD #51, not by us:** `fsGroup` grants the runner write access to
   the shared workspace volume by construction. If #51's k8s phase expects the
   uid split to fence *volume* access rather than only process/uid access, that
   is a conflict to settle in #51's design. Our reading is that there is none —
   the residual is intra-user and the workspace is meant to be shared — but #51
   should confirm that deliberately rather than inherit it from this PRD.
   **ANSWERED 2026-07-16 by the PRD #51 session, with a constraint that binds
   M3.** No conflict: #51's containment fences process/credential access, not
   general file/volume access, and the shared workspace *is* the runner's clone
   — runner-writable by intent, not a leak. But `fsGroup` performs a recursive
   `chgrp` + `g+rwX` on its volume, so **the worker's bare repo must never live
   on the `fsGroup`-shared volume**: the runner (10002, holding supplemental gid
   10001) could then write `<bare>/config` and plant a `filter.*` / `commondir`
   code-exec key that fires in the worker's later git, reopening the exact
   channel #51's B2 separate-runner-clone design closes by construction. The
   required k8s layout, once #51's runner container exists: shared workspace
   volume = **the runner clone only** (`fsGroup` g+rw, as designed here);
   **worker bare = worker-container-private** (its own volume, not mounted into
   the runner); the worker fetches the agent branch from the shared runner clone
   (`file://` + pack, the CVE-2022-39253-safe path). This is the compose→k8s
   mapping of the same rule: per-file ownership on one volume in #51's compose
   A1 form becomes volume separation in the k8s (C) form. #51 records the layout
   requirement in its own M7 k8s-alignment gate.
   - **v1 renders ONE `/data` PVC. Do NOT pre-split the bare onto its own
     volume** (architect, 2026-07-16, correcting an earlier over-reaction in this
     PRD that told M3 to render the two-volume topology up front). Two independent
     reasons, either sufficient. **(a) The bare repo is a pure cache, so there is
     nothing to migrate**: `ensureClone` (`agent/src/git.ts`) is "clone the repo
     bare if absent, else fetch to refresh", so the later split is a **cache
     miss**, not a migration — the controller rolls the pod (Decision 9 already
     gates rolls on the worker holding no non-terminal run), the worker finds an
     empty bare path, re-clones from `gitlab.example.com` (LAN-local to
     dev-cluster, so cheap), and carries on. No data moves and no user acts.
     **(b) Pre-splitting is not inert — it needs an image contract that does not
     exist**: `git.ts` derives *both* roots from a single `dataDir`
     (`reposRoot = dataDir/repos`, `worktreesRoot = dataDir/worktrees`), so a
     second volume does nothing unless `agent/` gains a bare-dir knob. M3 may not
     touch `agent/` (that is #51's file and the collision Decision 6 exists to
     have settled), so pre-splitting means mounting a volume nothing writes to,
     guessing a path and env name #51 has not chosen. Deferring costs ~8 lines of
     pod-spec rendering **in the very PR that is already editing the pod spec to
     add the runner container** — same edit, same file, same review.
   - **The additive claim survives, with one word sharpened.** "Additive" never
     meant the pod spec never changes — #51's k8s phase edits it by definition;
     that is where the runner container comes from. It means **no rewrite and no
     data migration**: add a container, add a volume, one PR. The volume split
     rides the container addition for free.
   - **When the split lands: `emptyDir` for the bare, not a second PVC.** The
     Multi-Attach worry does not apply (`strategy: Recreate` fully terminates the
     old pod before the new one starts; that holds for N volumes exactly as for
     one). It is a cache with a LAN-local source, so persistence buys a `fetch`
     delta over a `clone` — seconds against seconds. `emptyDir` cannot be
     orphaned, cannot fail to bind (a second PVC is a second way for a pod to sit
     Pending forever), costs nothing, needs no sweep, and — decisively — dies with
     the pod, which is what makes the v1→#51 transition free and leaves no stale
     bare behind. Honest counter, recorded rather than hidden: Decision 9 rolls on
     drift and the chart injects the release's image tag, so **every uzi release
     rolls every hosted worker**, each re-cloning every repo it works; for a large
     monorepo that is a repeated, visible cost. Start with `emptyDir` anyway —
     **framework: it is a cache, so take the free option and let a *measured*
     re-clone cost, not a prediction, buy the PVC.** The swap is itself one more
     cache miss.
   - **The bare volume must NEVER be mounted into the runner container, in any
     mode, and `readOnly: true` is not an acceptable substitute** (it still
     exposes the objects, and #51's threat is write). This looks like a redundant
     belt next to the `fsGroup` pin and is not: **`fsGroup` is pod-level**, so it
     recursively chgrps the private bare volume too and the runner holds
     supplemental gid 10001 over it. The runner cannot touch the bare **solely
     because it never mounts that volume** — the fence is mount-namespace
     isolation, and the group bits offer no protection here whatsoever. If a
     future reader adds that mount "for convenience", #51's B2 containment
     silently evaporates and `fsGroup` hands them write access. Mount asymmetry is
     the whole control: worker container mounts **both** the shared workspace and
     its private bare (it fetches from the runner's clone into its bare, then
     pushes bare→origin with the PAT); runner container mounts **the shared
     workspace only**. Everything else about `file://`+pack is inside the image
     and is #51's business.
   - Minor handoff to #51, not a design requirement: after the split the legacy
     `/data/repos/` sits orphaned on the shared volume — inert (the worker's
     `reposRoot` is an absolute configured path and never resolves there, so a
     planted `filter.*` can never fire), but wasted bytes the runner can write.
     #51's k8s phase should remove a legacy repos dir on first boot under the new
     layout, or consciously eat the bytes.
7. **Worker type = template, size = built-in preset.** Type selects the
   published per-template agent image (`agent-base`, `agent-jvm`; tag = release,
   Model B like api/web); the deployed image's baked `UZI_WORKER_TEMPLATE` must
   equal the declared type, or the server would provision a worker that badges
   its own template drift. Sizes are **code constants** (S/M/L: cpu, memory, and
   the volume sizes named below) — no CRUD.
   - **The preset covers TWO volumes, not one** (architect, 2026-07-16; this
     Decision previously said "PVC size", singular, which would have quietly cost
     an internet fetch per worker per release). The compose worker has two for a
     reason M3 inherits: `agentdata:/data` **and** `agentnix:/nix`. `/nix` is
     separate precisely because the nix store is an expensive **internet** fetch
     from `cache.nixos.org` (a first-run-only cost, PRD #18 M3 — the volume exists
     for provisioning warm-starts), and Decision 5 explicitly allows
     nix-substituter egress from the worker namespace, so hosted workers *do*
     provision tools. If M3 renders only a `/data` PVC and lets `/nix` fall back
     to the image's baked store, then **every roll — i.e. every release, since
     Decision 9 rolls on image-tag drift — re-fetches tier-1 packages from the
     internet, per worker.** That is exactly the cost class `agentnix` was created
     to avoid. So M3's volume answers point opposite ways, for opposite reasons:
     **persist `/nix`** (expensive, off-LAN, first-run-only), **do not persist the
     bare** (cheap, LAN-local, a pure cache — see Decision 6).
   - **CORRECTION — the note above is right that `/nix` must persist and WRONG about
     how, and taken literally it produces a worker that cannot boot** (tester
     measured + architect, 2026-07-16; **blocking for M3**). **A k8s volume does not
     seed itself from image content.** Seeding an empty volume from the image is a
     **Docker named-volume feature**, which is exactly why compose works and why this
     went unnoticed: the note assumed the compose mechanism transfers, and it does
     not. A PVC (or emptyDir) at `/nix` mounts **empty** and **masks** the image's
     baked 209 MB store — *including the `nix` binary itself*
     (`/nix/var/nix/profiles/default/bin`, on PATH). Measured under the worker's real
     posture (`--user uzi`, `no-new-privileges`, `cap-drop ALL`): the binary is gone,
     `/nix` has 0 entries, and devbox then tries to self-heal by **downloading an
     unpinned `nix-installer` and escalating via `sudo`** — failing with ENOENT, but
     only after reintroducing *both* things `agent/templates/base/Dockerfile:28-33`
     records as blockers it deliberately engineered out. **So it does not degrade to
     "re-fetch per roll"; it hard-fails, and it fails toward the posture the image
     exists to prevent.**
     - *What survives, and it is the substance.* The **goal** was right and is now
       measured rather than argued: provisioning the full seeded tier-1 allowlist
       grows `/nix` 209 MB → **1,703 MB / 1,205 store paths in 10m53s of real
       internet fetch**. Decision 9 rolls every worker on every release, so **not**
       persisting `/nix` costs that per worker per release. That measurement is the
       strongest evidence for persisting it this PRD has; only the mechanism was
       wrong. (e2e can never surface this: the harness bind-mounts a fake devbox
       because the isolated stack has no substituter egress.)
     - **M3 must seed the volume explicitly** — an initContainer (from the agent
       image, so it carries the baked store) copying `/nix` into the volume **only
       when the volume is empty**, never overwriting a provisioned store. Constraints,
       none optional: PodSecurity `restricted` (non-root, drop ALL, no privilege
       escalation — so no root chown escape), writable via the pinned `fsGroup:
       10001`, idempotent across rolls. **Verify the copy under the real posture
       rather than assuming it**: a non-root `cp -a` preserving ownership works only
       while the image's `/nix` is owned by the uid it runs as.
     - *Known property, inherited from compose rather than introduced here*: once
       seeded, the store is **never refreshed from a newer image**, so a nix/devbox
       upgrade baked into a release never reaches an existing worker's `/nix`. Compose
       has had this forever; it is sharper here only because Decision 9 rolls workers
       automatically and users will reasonably expect a roll to update things. Remedy
       is v1's answer to everything: delete + reprovision.
     - *Rejected — one shared RWX read-only nix store for the fleet.* Read-only cannot
       work at all: tool-profile provisioning (PRD #18) **writes** to the store.
       Shared-and-writable is worse than it looks — **the nix store is executable
       content**, so one compromised worker poisons every other user's binaries,
       converting a single-worker compromise into fleet-wide cross-tenant code
       execution. It also needs an RWX StorageClass dev-cluster may not have. **The
       right long-term answer is a substituter, not a mount**: an in-cluster binary
       cache (nix-serve, or a Harbor-backed mirror) keeps per-worker store isolation
       while turning that 10m53s internet fetch into a LAN fetch. Out of scope for M3;
       the correct v2 shape for this cost.
     - *Sizing, now measured*: **`/nix` is byte-identical between `base` and `jvm`**
       (209 MB, 74 store paths — the JDK ships via apk to `/usr/lib/jvm`, never
       through nix). So **`/nix` does not vary by template or by size** and belongs
       **outside the preset table** as a flat value. 1,703 MB is the measured worst
       case, so a flat **4Gi** carries ~2.4x headroom; 8Gi is mostly waste, and storage
       is the binding fleet constraint (10 users × quota 2 × M ⇒ 10 cores / 20Gi RAM
       but **560Gi of PVC** at 8Gi nix). *Residual to document, not build*: **nix has
       no auto-GC**, so a long-lived worker's store only grows and a fixed volume
       eventually fills. `nix store gc` lives in `agent/` (not M3's file), so v1
       documents it and delete + reprovision is the remedy.
     - *It does not change the size-picker debt (M6); it simplifies the table.* The
       incentive problem is a function of the **count-based quota** — if every preset
       costs 1 of 2, the biggest is free whatever the preset contains. Removing `/nix`
       leaves cpu/memory/`/data`, and `/data` does still vary by size, so the picker
       does move the binding constraint. Users still have no reason to care.
   All presets pin `WORKER_MAX_CONCURRENT_RUNS=1` until
   PRD #51 lands: cap>1 would server-provision the documented intra-user
   concurrency residuals (`docs/worker-setup.md` §Concurrent runs). Type and
   size are validated server-side against the built-in lists; user input never
   reaches an image reference or pod spec as free text.
8. **Self-service + one quota knob.** Any user may provision up to the
   admin-set per-user quota (single admin setting, default 2, 0 disables
   self-service). Enforcement is atomic: the provision transaction takes
   `pg_advisory_xact_lock` on the user, then counts, then inserts and seals — one
   transaction. **The lock is the mechanism.** The quota check alone is not
   atomic under READ COMMITTED (this repo's default; no `h.pool.Begin` site sets
   `TxOptions`), in either of its shapes: two concurrent provisions evaluate the
   count against their own snapshots, neither sees the other's uncommitted row,
   and both commit at quota+1. **Measured 2026-07-16 against real Postgres**:
   with the lock removed and the quota check otherwise intact, **8 of 8
   concurrent provisions passed a quota of 2**; with the lock restored, exactly 2.
   `FOR UPDATE` over the counted rows cannot fix it either — it locks rows that
   exist and takes no predicate lock, so it does not fence a phantom insert. The
   original text ("guarded insert under the same transaction, no TOCTOU") named a
   mechanism that does not deliver the property it claims; found 2026-07-16 by
   the M2 priming wave (auditor + reviewer independently, corroborated by the
   architect) before any code was written against it, and confirmed by
   measurement during M2. Quota counts hosted workers only. Defense-in-depth
   at the k8s layer: the worker namespace carries a ResourceQuota + LimitRange
   so an app-level bug cannot noisy-neighbor the shared cluster. Provision and
   delete endpoints are rate-limited (existing limiter pattern, PRD #53).
9. **Upgrade/drift semantics.** The chart injects the release's agent image
   tag into the **controller's** config; a hosted worker whose Deployment differs from
   desired (image tag, spec hash) is *drifted*. The controller rolls a drifted
   worker only when it holds no non-terminal run (same predicate as delete);
   busy workers roll after their runs finish. Missing objects are recreated
   (**except Secrets — see below**); unrecognized `uzi-hw-*` objects are flagged
   as orphans, never adopted.
   - **CORRECTED 2026-07-17 (M3): this said the tag goes into the API's config, and
     implementing that verbatim would have violated Decisions 1 and 7.** The text
     predates M1's protocol. It is not implementable as written: the api knows no image
     tag (nothing in `api/internal/config` or the chart carries one) and M1's wire has
     no image field (`DesiredWorker` = id/template/size/generation/join_token). The tag
     belongs in the **controller's** config (`UZI_WORKER_IMAGE_TAG`) — the only reading
     consistent with the rule that makes this boundary checkable: *the api sends the
     NAME; the controller owns every mapping to a concrete pod-spec value.*
   - **A token rotation must be stamped as a pod-template annotation or the pod
     never rolls** (architect pass, 2026-07-16; blocking for M3). Rotation
     changes the Secret's *content*, but the Deployment mounts it by *name*, so
     the pod template is byte-identical and nothing restarts. Kubelet does
     eventually refresh the mounted file (~60-90s), but the worker read it once
     at boot and holds the old token in memory — so it 401s forever while the
     correct token sits on its own filesystem. Fix: write `hosted_generation`
     into `spec.template.metadata.annotations`, which changes the pod-template
     hash and forces the roll (the standard Helm `checksum/config` idiom). It
     uses `patch` on Deployments (granted), and gives the controller its
     observed-generation source for drift via `get` (granted), with no Secret
     read. **This is what the generation is for** — notably *not* the delivery
     ack, which Decision 3 derives from registration instead.
   - **"Missing objects are recreated" is false for Secrets, by design.** After
     delivery no plaintext exists server-side anywhere (Decision 3), so a deleted
     token Secret can neither be recreated **nor detected** (detection would need
     the Secret read Decision 1 refuses). The real detection is the worker going
     offline — Decision 10's heartbeat, already the documented v1 diagnostic —
     and the recovery is delete + reprovision. v2 hook worth recording rather than
     building: a pod wedged in `ContainerCreating`/FailedMount is a precisely
     diagnosable "the Secret is missing" state and is the only handle anyone has
     on this, via `Pods get/list` (already granted). It belongs with the v2
     pod-phase status work, not v1.
10. **Status = heartbeat, unchanged.** Online/offline/busy stay
    heartbeat-driven; hosted rows are visually marked as hosted. Pod-phase
    diagnostics in the UI are v2; v1 docs point admins at
    `kubectl -n <worker-ns> get pods`.
11. **Delete rules unchanged**: refuse while the worker holds a non-terminal
    run, then remove the registration and let the controller tear down
    Deployment, Secret, and PVC.
12. **Feature-gated.** `WORKER_HOSTING_ENABLED` on the api (set by the chart);
    off (the compose default) the API reports hosting disabled and the UI hides
    everything hosted. Compose deployments are zero-diff.
    - **"Hides everything hosted" means the provision AFFORDANCE, not a user's
      existing rows** (M5, 2026-07-16). The literal reading collides with itself: a
      hosted worker still exists whether or not the flag is on, and hiding its row
      strands something the user owns and must be able to delete. So the flag hides
      the provision card; hosted **rows stay visible, stay badged, and stay
      deletable**, and the badge follows `workers.kind` alone — it never consults the
      flag. Note the two readings are **indistinguishable on the deployment this
      Decision exists to protect**: compose never had hosting on, so no row is ever
      `kind='hosted'` and zero-diff holds automatically from `kind` alone. They
      diverge only on an instance that provisioned hosted workers and *then* turned
      hosting off — where badging is the more honest of the two, since an unbadged
      hosted row reads as "a worker I forgot to start" and sends its owner looking
      for a container they never ran. Same principle the quota-0 case already
      establishes: **the affordance is gated; the rows are not.**

## Milestones

- [x] **M1 — Controller skeleton + protocol** (landed 2026-07-16): new `controller/` component
  (Go, reuses api's module or its own — decide in M1), controller bearer auth,
  desired-state poll endpoint, delivered-once token handoff (Decision 3),
  migration extending `workers` with hosted metadata (kind, template, size,
  generation), feature flag. api gains no k8s access. Success: protocol unit
  tests against a fake store; compose boots with the flag off, zero behavior
  change.
- [x] **M2 — Hosted worker API + quota** (landed 2026-07-16): provision (type + size, validated)
  and delete endpoints; atomic quota enforcement + the admin quota setting;
  rate limits. Success: API tests cover quota races, disabled flag,
  delete-while-busy refusal; provisioning is not user-reachable until M3
  (endpoints ship behind the flag).
  - Delete added **no endpoint**: `DELETE /api/workers/{id}` already implements
    Decision 11 for both kinds, so it gained only the per-user limiter — which
    consequently caps external-worker deletes too (10/min, where there was none).
    Accepted by the user: the shared route is the only non-bypassable place to put
    it, and a hosted-only route would duplicate the active-runs guard.
  - The quota's live-DB tests now run in CI (`test:api-store-it`, a Postgres
    service). They previously existed but skipped silently — `go test ./...`
    reports `ok` for a package whose every test skipped, so the milestone's central
    security property had a green pipeline and zero coverage.
  - **Prerequisite of any rotation path, stated here rather than assumed**
    (auditor, 2026-07-16): a rotation MUST set `workers.token_hash` and park the
    new sealed token (`SealJoinToken`) **in one transaction**. M1's ack destroys
    the buffer under `AND token_hash = @proved_token_hash` — the hash the caller
    actually proved — so its correctness rests on "the hash I proved is still
    current" being equivalent to "the parked ciphertext is the token I proved",
    which holds only while the two are co-written. Split them and the predicate's
    premise breaks silently. M1 reshaped `SealJoinToken` into a free function over
    `Store` precisely so it can join M2's provision/rotation tx (the repo's
    handler-binds-`qtx` idiom, six existing sites). **Provision must also gate on
    `WORKER_HOSTING_ENABLED`** — nothing else stops a flag-off seal.
- [x] **M3 — Materialization + policies** (landed 2026-07-17): controller renders Secret
  (file-mounted token) / Deployment (Decision 6) / PVC; worker namespace with
  PodSecurity `restricted`, ResourceQuota, LimitRange, both NetworkPolicies
  (incl. the api-ingress amendment); CNI FQDN-egress capability verified on
  dev-cluster; drift/orphan reconciliation per Decision 9. Success: fake-client
  tests assert rendered objects; a hosted worker on a kind cluster registers,
  goes online, and completes a stub run end to end.
  - **The "completes a stub run" half is RELOCATED TO M6, not waived** (user decision,
    2026-07-17). M3 proved **register → online** on kind, plus the mechanical claims in
    the live pod (git init+commit on `/data` as uid 10001; node/git/**nix**/devbox all
    resolving off the SEEDED volume — the thing that is gone if the seed is wrong).
    - *Why relocating is stronger than running it on kind, rather than a concession.*
      M6's own success criterion already requires **a real hosted worker on dev-cluster
      completing a run**, which is strictly better evidence on a cluster where `/data` is
      representative. A kind stub run would exercise the agent's run loop — which M3
      changed nothing about and the compose e2e already covers — and its `/data` evidence
      would be **exactly as misleading as the fsGroup assertion proved**: kind's PV is
      hostPath-backed and arrives `0777 root:root`, so a green run there says nothing
      about the volume a real worker gets.
    - *And why it is recorded rather than inferred.* The coder reached this judgement and
      declined to redefine the criterion on its own authority; it needed to be the user's
      call. A milestone's success criterion is not an implementer's to reinterpret,
      however sound the reasoning — the honest move is to flag it and let it be decided.
  - **The Decision 9 / Decision 11 collision is settled by PROVENANCE, not by a name
    prefix — no tombstone, no schema change, no wire change.** The collision existed
    only because "orphan" was defined as `uzi-hw-*`-NAMED. The controller stamps
    `app.kubernetes.io/managed-by: uzi-controller` + `uzi.dev/hosted-worker-id` on
    everything it creates, and the two sets become disjoint: **teardown** =
    `{objects we stamped} \ {desired fleet}` (Decision 11), **orphan** = `uzi-hw-*`-named
    but NOT stamped — flagged, logged, never adopted, never deleted (Decision 9, and
    not vacuous: hand-made objects, an older label scheme, a partial create from a
    crashed reconcile). "Never adopted" gains a precise meaning: *the controller never
    takes ownership of an object it did not stamp.* It also settles the Secret
    asymmetry — the controller cannot enumerate Secrets, but it can DELETE BY NAME
    from the worker id on the observed Deployment/PVC labels, so a deleted worker's
    Secret IS cleaned up and the residual is only "Secrets whose worker id we can no
    longer learn" — exactly the accepted gap, no wider.
    - *Rejected — a bulk-teardown circuit breaker* ("refuse a reconcile tearing down
      more than N% of the fleet"): a user deleting 3 of their 5 workers is routine and
      **indistinguishable** from a bad restore, so it would page an operator during
      normal use, to protect caches. Recorded so it is not re-proposed.
    - *The residual accepted*: a poll that succeeds and LIES (a DB restored from a
      backup predating a provision). A hosted worker's volumes hold no irreplaceable
      data, and a worker whose row is gone has no `workers.token_hash` — its pod cannot
      authenticate and is already dead weight. A poll BLIP cannot trigger it: `Tick`
      returns before `Reconcile` if `Poll` errors, and hosting-disabled 404s. Both fail
      closed.
  - **Sizes chosen (user, 2026-07-17) and they are BURSTABLE — requests < limits.**
    The PRD's own fleet arithmetic ("10 users x quota 2 x M => 10 cores / 20Gi RAM") is
    only coherent as REQUESTS; it is impossible as a limit, since a real run peaks at
    676 MiB and compose grants 4 GiB. So each preset carries **both**, a distinction
    Decision 7 never draws (it names "cpu, memory" as if singular). Guaranteed was
    rejected: it contradicts that math and would strand 4Gi per IDLE worker (measured
    idle: 148 MiB) on a shared cluster. `s`: 250m–1 / 1–2Gi / `/data` 5Gi. `m`: 500m–2 /
    2–4Gi / 10Gi. `l`: 1–4 / 4–8Gi / 20Gi. `/nix` is a **flat 4Gi outside the table**.
    Requests sit deliberately ABOVE the measured peak: kubelet evicts pods exceeding
    their REQUESTS first, so a request below real usage would make workers the first
    thing evicted under node pressure.
  - **THE TWO TRAPS, both now mechanical gates verified BY MUTATION** (this PRD's own
    Decision Log records that a test which cannot fail is not a gate):
    - **Teardown must be `{ours} \ {ALL desired ids}`, INCLUDING ids we could not
      render.** An unknown size or template means skip *rendering*, never leave the
      desired set. Get it backwards and an old controller polling a new api tears down
      **every worker carrying the new size or template** — the exact deployment skew the
      tolerance exists for becomes fleet destruction. **Template is equal to Size here**:
      the PRD only ever discussed Size, but the controller resolves both against its own
      tables, so both skew identically. Introducing the bug is caught for both.
    - **The `hosted_generation` annotation is INERT in v1** (no rotation path exists; it
      is 0 on every provisioned worker). It is stamped and unit-tested, and **nothing
      end-to-end can exercise the roll** — no green e2e may be read as "rotation rolls".
  - **The kind run: what it proved, and what it CANNOT.** Proved, on a real kubelet: the
    tar seed under real PodSecurity `restricted` + non-root + 0555 store dirs; the whole
    object set rendered and admitted; ResourceQuota/LimitRange/PodSecurity admission; the
    `s` preset rendering exactly (250m / 1Gi / 5Gi+4Gi); **register → online**; and
    teardown removing every object by name. **Did NOT prove**, and each was checked
    rather than assumed: **NetworkPolicy at all** (kindnet enforces none — see Decision 5);
    **the fsGroup pin** — asserted as the OBSERVABLE rather than as writability, and it
    duly reported the gap: kind's local-path PV is hostPath-backed and **fsGroup is not
    applied** (`/data` and `/nix` arrive `0777 root:root`, so uid 10001 writes them for a
    reason that does not exist on dev-cluster, where an RWO ext4 volume mounts
    `root:root 0755` and fsGroup is the only thing making it writable). Deferred to M6.
  - **`0440` is confirmed on a real kubelet, not reasoned**: the worker's token mounts
    `440 root:10001` and uid 10001 reads it via group 10001. `0400` would have made a
    worker unable to read its own join token — the file is owned by ROOT, not by
    `runAsUser`.
  - **The nix seed CrashLooped on first contact with a real kubelet, twice, and no fake
    client could have caught either.** (1) `tar -C /nix .` archives a `./` member, so
    extraction ends by chmod/utime-ing the DESTINATION ROOT — the kubelet's mount point,
    owned `root:<fsGroup>`, which uid 10001 does not own. It EPERMs, tar exits non-zero,
    the sentinel is never written, and the pod CrashLoops **having copied the store
    perfectly**. Fixed by archiving the top-level entries so `.` never enters the archive
    (`--no-overwrite-dir` does NOT fix it). (2) An interrupted seed could never recover:
    tar will not extract over the 0555 dirs a previous pass sealed, so the retry failed
    forever behind a manual PVC deletion. Fixed by wiping before extracting, scoped
    **below** the root — busybox `chmod -R` ABORTS on the un-chmodable root and never
    reaches the children, which is why the first attempt at the wipe silently did nothing.
  - **The `lost+found` sentinel trap is closed by a UNIT test, deliberately**, because
    kind cannot catch it: local-path makes a plain directory, so a fresh PVC there really
    IS empty and an emptiness check passes — while dev-cluster's hypervisor CSI formats ext4
    and a fresh PVC carries `lost+found`. It is a property of the check, so it needs no
    kubelet.
  - **The size/template goldens are gates only with `-count=1`, and CI now passes it.**
    Go's test cache hashes only the files a test opens INSIDE its module root
    (`cmd/go/internal/test/test.go:2041`, verified in go1.26.4); every golden lives in
    `api/`, outside `controller/`, so editing one leaves the controller module's cache key
    untouched and `go test ./...` reports "ok (cached)" with the gate never running. It was
    invisible only because `test:controller` had no `cache:` — and that job's own comment
    told M3 to add one, which is exactly what would have armed the trap.
  - **Inherited from M2 — the size golden is INERT until M3 parses it, and M2
    shipped it that way knowingly** (architect, 2026-07-16). M2 landed
    `api/internal/hostedsvc/testdata/hosted_sizes.json` + the producer-side test
    pinning `workersize.Names` against it. The consumer half is M3's:
    `controller/internal/protocol` must read the same file and assert its own
    preset table covers the set. The cross-tree read idiom already exists — the
    wire golden's own test does
    `filepath.Join("..","..","..","api","internal","hostedsvc","testdata", …)`.
    **Until M3 does this, editing one module's size list and not the other is
    silently legal**, which is the entire failure the golden exists to stop: the
    api accepts a size the controller cannot resolve, the worker provisions, no pod
    is ever rendered, and the row sits pending until its token expires — visible
    only as a worker that never comes online (Decision 10's heartbeat), with the
    cause invisible.
  - **M3 must ALSO tolerate an unknown size at runtime — the golden cannot cover
    this.** It catches dev-time drift, not deployment skew: api and controller are
    separately-built images, so even under Model B's version pinning a rollout has a
    window where an old controller polls a new api and sees a size its table lacks.
    Log and skip that worker; never crash the reconcile (one unknown size must not
    stall the whole fleet), never guess or substitute a pod spec.
  - **Decision 9 and Decision 11 collide on a deleted worker's objects, and M3 owns
    settling it — in design, not in the reconcile loop.** Decision 9 says
    unrecognized `uzi-hw-*` objects are "flagged as orphans, never adopted";
    Decision 11 says a deleted worker's objects are torn down. M2 ruled that delete
    is the existing `DELETE /api/workers/{id}`: the row is deleted outright and
    `hosted_worker_tokens` cascades away, so **a deleted worker simply leaves the
    poll's fleet set — and its objects are, by Decision 9's own definition, exactly
    an orphan.** The poll carries no signal distinguishing "deliberately deleted"
    from "never recognized", and the two demand opposite responses. Note the shape
    of the trap before reaching for the obvious fix: a tombstone/`deleted` state on
    the wire means the api retains a row the delete just removed (a schema and
    lifecycle change — who deletes the tombstone, and when?), while "tear down
    anything not in desired state" makes orphan-flagging vacuous and hands a poll
    blip fleet-wide teardown authority. Neither is obviously right; that is why it
    is a design question.
  - **The most likely thing to get backwards**: a `null` `join_token` in the poll
    means "write no Secret for this worker" — **never** "this worker has no token".
    Either a pod already proved it holds one (the cluster Secret is then the only
    copy in existence) or the buffer expired unread. Never invent one, never clear
    the existing Secret on account of it, never log it. M1's `protocol.go` says this
    at length; it is repeated here because that file is not what M3 will be briefed
    from.
  - Smaller M2 facts M3 inherits: **delete is not gated on
    `WORKER_HOSTING_ENABLED`** (provision is — a stack that provisioned workers then
    turned hosting off must still be able to remove them, the same reasoning the
    expiry sweep already uses); and **`hosted_generation` is 0 on every
    M2-provisioned worker** (M2 ships no rotation path — user-confirmed
    2026-07-16, v1 recovery is delete + reprovision), so M3's roll-on-drift
    annotation is the generation's first consumer and the first thing that will ever
    change it.
- [x] **M4 — TLS worker hop** (landed 2026-07-17): api TLS listener + cert wiring (cert-manager in
  the chart), controller and hosted workers on `https://`, docs for the cert
  values. Success: claim traffic on dev-cluster is TLS; plain-HTTP worker port
  unreachable from the worker namespace.
  - **M4's own recorded residual is CLOSED (M3, 2026-07-17), and its reasoning was
    right.** M4 verified the api binary and the chart render separately but never
    together, flagging that `defaultMode: 0400` + `fsGroup: 65532` + uid 65532 might
    CrashLoop the api on `permission denied` reading `tls.key` — "that is reasoning, not
    an observation". Settled opportunistically on M3's kind cluster, where it is
    representative (a tmpfs secret volume, not storage-backed): the api pod comes up
    Running, logs `api listening (tls) addr=:8443`, and serves **HTTP 200 over TLS from
    another pod**. Kubelet ORs `0040` onto read-only secret volumes exactly as M4
    reasoned, so `0400` lands as `0440 root:65532` and uid 65532 reads it via its group.
    No change needed to `api-deployment.yaml`.
  - **The CA relay to workers is M3's and needs no new RBAC verb** (Decision 1 untouched):
    the controller mounts the CA and copies it into the per-worker Secret it already
    creates, and the worker trusts it via `NODE_EXTRA_CA_CERTS` — pure pod spec, since
    Node reads that path before startup and `agent/src/client.ts` uses plain `fetch`.
    Consequence worth knowing: that Secret is written ONCE and never updated (there is no
    Secret read, and delete+recreate would destroy the join token, which after delivery is
    the only copy in existence), so a CA **root** rotation strands existing workers and the
    remedy is delete + reprovision. The leaf rotating every 90 days does not touch `ca.crt`,
    and the chart's `caDuration` is 5 years precisely because re-trusting a new root means
    touching every worker at once.
- [x] **M5 — Web UI** (landed 2026-07-16): Settings → Workers hosted section — provision
  dialog (type + size), hosted-marked rows, delete; hidden when hosting is disabled.
  Success: vitest coverage; web-ux review. **Both met**: 612 → 654 tests (+42, +3 files),
  and web-ux drove the primary journey in a real browser (provision → hosted-marked row →
  delete) plus the quota loop, confirming the at-quota gate **releases** rather than
  dead-ending. Reviewer + web-ux both cleared it with no blocking findings.
  - **Delete gained a hosted-only confirmation** (user decision, 2026-07-16), because M5
    pointed the page's existing one-click Delete at a far more destructive target: an
    external delete revokes a token (the container keeps running; re-register to recover),
    while a hosted delete takes `/data` and `/nix` with it permanently — and with no
    restart endpoint in v1, Delete is the only lifecycle control a user has, so they will
    reach for it to restart a stuck worker. External delete stays **one click**, pinned by
    a test: the confirmation's meaning depends on its rarity.
  - **Deletes announce and take focus, both kinds.** An announcement is feedback *after*
    the act, not friction before it, so it costs no clicks and takes nothing from the
    asymmetry above. Note the conventional "focus the next row's Delete" is **actively
    unsafe here**: the remaining rows are mostly external one-click destructors, so it
    would park a keyboard user on a live one, and double-tapping Enter through a confirm
    would destroy a second worker.
  - **The default is `m`, not `s`** (user decision, 2026-07-16) — see M6. `s` shipped
    first with a rationale that was false about this codebase, and the correction outlived
    the comment fix.
  - **"Dialog" is an inline Card form, not a modal** (architect, 2026-07-16;
    verified): there is **no modal primitive anywhere in `web/`** — no `Modal`, no
    `role="dialog"`, no `<dialog>`. Taken literally the word would have M5 build
    this codebase's first modal (focus trap, escape, `aria-modal`, scroll lock,
    restore-focus) for one two-field form. The sibling affordance is twenty lines
    away: "Register a worker" is an inline `<Card>` with a `<form>`. Read "dialog"
    as loose language for the provision affordance and mirror the sibling.
  - **Sizes ship as NAMES ONLY (S/M/L, no quantities); the display is deferred to
    M6 — an accepted cost, not a design choice** (user decision, 2026-07-16).
    **The architect recommended against it and that objection stands**: a user
    choosing a size with no idea what it buys is choosing blind, and a size picker
    is the one control in this feature whose whole job is to inform that choice.
    It was accepted because the quantities are **unchosen rather than unwritten**
    (see M6), and the alternative — M5 inventing numbers that M3 then discovers —
    is the silent-lie failure the whole question exists to prevent.
- [ ] **M6 — CI + chart + rollout**: publish per-template agent images
  (`agent-base`, `agent-jvm`; templates listed in a CI variable to bound build
  cost) and the controller image on `v*` tags; chart adds controller
  Deployment, worker namespace + RBAC + quotas + policies, hosting values;
  ArgoCD rollout to dev-cluster. Success: a tagged release provisions a real
  hosted worker on dev-cluster that completes a run.
  - **M6 now carries M3's run proof too** (user decision, 2026-07-17): M3's "completes
    a stub run" was relocated here rather than waived, because M6's criterion above
    already subsumes it with strictly better evidence — a REAL run on a cluster where
    `/data` is representative, instead of a stub run on kind where the PV arrives
    `0777 root:root`. **No new work: it is the same sentence already written above.**
    What M6 must not do is let it slip quietly, since it is now the ONLY place a hosted
    worker is proven to execute anything. See M3's bullet for what is already proven
    (register → online, and the toolchain resolving off the seeded volume).
  - **Three gaps land on M6's first real deploy, all deliberately left open by M3**
    rather than papered over: **NetworkPolicy enforcement** (kindnet enforces none, so
    nothing about either policy is proven — including the Antrea FQDN egress, where
    capability is proven but no packet has crossed); **the `fsGroup: 10001` pin** (kind's
    local-path PV is hostPath-backed and ignores fsGroup, so `/data` and `/nix` arrive
    `0777 root:root` there and the pin is untested); and **the `items:` CA projection**,
    which the controller Deployment is the first workload to need — `deploy/README.md`
    documents it as a requirement and M3 could not discharge it, since a hosted worker
    never mounts the api's TLS Secret at all.
  - **MEASURED 2026-07-16 — a real SDK run peaks at 676 MiB, so the memory question
    is largely settled and S is not the OOM risk it looked like.** The live capstone
    (`UZI_E2E_EXECUTOR=sdk`) drove a complete real Claude Agent SDK lifecycle — clone
    → plan → gate → approve → implement → push → MR !1 on `agent/issue-2`, 78
    run_messages gapless across a mid-run restart, $0.80, 424k cache-read tokens.
    Sampled from the worker's own cgroup high-water mark immediately after
    (`memory.peak`, the number no stub run can produce):

    | | measured |
    |---|---|
    | **peak, whole real run** | **708,919,296 B = 676 MiB** (limit 4 GiB) |
    | worker self-report post-run | 154,775,552 B = 148 MiB, cpu 0.49% |
    | `/data` after the run | 1 MB |
    | `/nix` | 209 MB (unchanged — e2e bind-mounts a fake devbox) |

    **This confirms the tester's expectation and refutes the fear behind it: the
    SDK's own Node heap is NOT the driver.** 676 MiB sits at ~45% of a *single*
    1.5 GiB compose slot, so the formula's per-slot figure has ~2.2x headroom over a
    real session. **S (2Gi) fits a real SDK run three times over.**
    - **What this does NOT measure, and it is the whole remaining question: the
      user's build.** The e2e repo is a single-commit fake, so this run compiled
      nothing — `/data` at 1 MB is no signal, exactly as the tester warned. A JVM
      test suite or a large `go build` is what dwarfs the agent, and it is untested.
      So: **the agent is cheap; the workload is unmeasured.** Memory limits are now
      "reasoned about the build" rather than "reasoned about the agent", which is a
      strictly better place to be but not a measurement.
    - Consequence for the default: `m` remains right, but for a *different* reason
      than it was chosen — not because S might OOM on the agent (it will not), but
      because M is compose parity and the formula's floor, and because an
      under-sized default fails invisibly in a version with no pod-phase status.
      **Do not re-litigate `s` on the strength of this number**: it bounds the agent,
      not the build.
  - **The live capstone could not pass as shipped — an e2e harness bug, found by
    running it. FIXED in `0e04bb2`** (2026-07-16). **Four** assertions in the PRD #40
    block hardcoded the stub's synthetic usage (21400 in, 6100 out, a `>= 21400`
    lifetime, a coder message at 12000) and none were gated on `$EXECUTOR`. A real
    session spends what it spends, so all four failed under `sdk`; the first exits,
    so the harness reported failure **after 24 PASS and a fully successful real run**
    (2,229 in / 11,171 out, having cloned, planned, gated, implemented, pushed and
    opened an MR). `e2e/README.md`'s "no milestone assertion depends on this path"
    was true of the *milestones* and false of the *script*.
    - The fix keeps the exact values under the stub — there they also prove the frame
      was **parsed**, not merely non-empty — and asserts the property PRD #40 actually
      owns under `sdk` (usage folded, aggregated, attributed), which holds either way.
    - **Two of the changes are improvements independent of the capstone.** `/api/usage`
      aggregation now asserts against **the run's own usage** rather than a constant:
      strictly stronger, since `>= 21400` would pass an `/api/usage` that ignored this
      run entirely whenever an earlier run had banked the total — a weak assertion
      wearing a specific-looking number. And the per-agent check no longer pins the
      agent name under `sdk`, since live runs take their roster from the cloned repo
      (PRD #37 selects `agent_source=repo` two assertions earlier), so `coder` was
      never guaranteed there.
    - Verified rather than read: each branch mutation-checked against the real
      capstone payload and against absent/zeroed usage, and the full stub e2e is green
      (exit 0, 146 PASS). **Not yet re-run live** — the logic is verified against the
      exact payload that broke it, but a second ~13m/$0.80 capstone would close it.
  - **M6 inherits a debt from M5, and owes TWO things for it: picking the S/M/L
    quantities, and landing the display.** M5 shipped names-only (above). The
    quantities are not merely unwritten — **nobody has ever chosen them**: not this
    PRD (Decision 7 names the *fields* — cpu, memory, and the volume sizes — but no
    values), not `api/internal/workersize` (names only, deliberately), not the
    controller. M3 must pick them to render pods at all, so M6 inherits whatever M3
    lands; if M3 has not, M6 picks. Sizing a user's agent container is a **product
    decision** — take it to the user, do not let it become an implementer's guess.
    Constraints that make the question answerable rather than open-ended:
    - All presets pin `WORKER_MAX_CONCURRENT_RUNS=1` until PRD #51 lands
      (Decision 7), so a size buys headroom for **one** run, never parallelism.
    - `/nix` is a real per-preset volume, not an afterthought (Decision 7): it exists
      so tier-1 tool provisioning is a first-run-only internet fetch. Each preset
      therefore sizes **two** volumes.
    - The worker namespace's ResourceQuota + LimitRange (Decision 8) bounds them:
      the presets must fit inside whatever budget dev-cluster ends up granting.
    - Today's compose worker is what every user runs by hand right now — its real
      footprint is the honest anchor for "M".
    - **`DEFAULT_WORKER_SIZE = "s"` is the one size choice M5 made, and it was argued
      from NAMES, not numbers** — deliberately, since the numbers did not exist. It is
      the pre-selected option in the provision picker, so it is what most users will
      actually get. Revisit it once real figures exist: the smallest preset is the
      right default only if "S" can actually run a worker.
  - **M6 also owes an answer to "why would a user ever pick S?" — and the quantities
    do NOT answer it** (web-ux, 2026-07-16; verified). **The size picker is
    structurally decorative, and landing the numbers will not fix that.**
    - *The finding.* The quota is enforced as a **count of workers** —
      `CountHostedWorkersForUser`, compared `n >= quota` (`handler/hosted_workers.go`).
      So **S, M and L each cost exactly 1 of 2**. From the user's chair: I get 2
      workers, there are three sizes, nobody tells me what they mean, and all three
      cost the same. **The rational move is to pick L every time.** A user who picks S
      is a user who did not think about it.
    - *Why it outlives M6's numbers, which is the whole point.* Presets conserve
      nothing unless users have a reason to pick smaller. Knowing "L = 4 CPU" fixes
      the **informed** half and leaves the **incentive** half untouched — L still
      costs the user nothing over S. So M6 can land every quantity and the picker
      stays exactly as decorative as it is today. **The numbers are necessary and not
      sufficient.**
    - *The honest current state, stated because it is nobody's design.* **The S
      default is load-bearing**: it is the only thing keeping most users off L, and it
      works by **inattention**, not by informing anyone. And the only real "incentive"
      that exists today is a commons failure — if everyone picks L the namespace
      ResourceQuota (Decision 8) fills and *someone else's* worker stops scheduling,
      invisibly, in a version with no pod-phase status. Whoever picks the numbers
      inherits that.
    - *Two levers, both bigger than M5, **user chose neither** (2026-07-16).*
      **(a) A resource-weighted quota** (S=1, M=2, L=3 against a budget). Note for
      whoever costs this: it does **not** reopen Decision 7 — a weight is a *quota
      price*, not a pod-spec value, so `workersize` could carry `Weight(name)` without
      the api learning a single quantity. What it does reopen is M2's landed
      transaction (the count becomes a sum) and, more awkwardly, **the semantics of a
      shipped admin setting**: `hosted_worker_quota: 2` would silently stop meaning
      "2 workers" and start meaning "2 points", so the key name becomes a lie and
      wants renaming. **(b) Offer no choice at all** — one size for everyone. This is
      the lever that **dissolves the quantities debt too** (one size = one set of
      numbers to pick and measure), and it is more reversible than it looks: `00066`
      deliberately has no CHECK on `hosted_size`, so adding presets back later needs
      no migration.
    - *Scope note, applied to this PRD's own design.* Three presets that no user has
      a reason to choose between is speculative generality. Decision 7 committed to
      S/M/L before anyone noticed the quota made them free; that is worth re-deciding
      in M6 rather than inheriting.
    - *Sequencing.* Answer the incentive question **first** — it determines how many
      number sets M6 needs. This does not strand the footprint measurements now in
      flight: those measure **what a worker actually needs**, and presets are derived
      from that, so they inform any of these answers equally.
  - **The display mechanism is DESIGNED AND UNIMPLEMENTED. Implement it; do not
    rediscover it** (architect, 2026-07-16). The quantities must reach the UI
    **without** the api ever learning them — Decision 7 pins that the api sends the
    preset NAME and that shipping resolved values would make it "the authority on a
    pod spec it is not allowed to know anything about". Note that reading them from
    the authority at runtime is **structurally impossible, not merely ugly**:
    Decision 2 makes the controller outbound-only (it polls the api; the api never
    dials it), so any such scheme needs the controller to *push* its table to the
    api — new controller-authenticated write surface whose content the UI would then
    display, with nothing to show before the first push. The specified design
    instead:
    - **`hosted_sizes.json` gains the quantities** — currently names-only, and it
      stays that way until this lands. Shape: raw k8s quantity strings, never
      pre-rendered prose, so both consumers assert structurally — the controller
      against `resource.MustParse` values, the web rendering the strings verbatim.
      Prose is where a lie hides.
      - **CORRECTED 2026-07-17 (M3): the shape here carried a per-size `nix` field,
        which Decision 7's own later correction removes.** `/nix` is measured
        byte-identical across `base` and `jvm` and does not vary by size, so it is a
        flat 4Gi OUTSIDE the preset table (and M3's `controller/internal/preset` ships
        it that way, pinned by a test). Left alone, M6 would ship a per-size field with
        one repeated value. The shape is
        **`{"name":"s","cpu_request":"…","cpu_limit":"…","memory_request":"…","memory_limit":"…","data":"…"}`**
        — note it also needs REQUEST and LIMIT, not one value per resource: M3's presets
        are Burstable, and Decision 7 names "cpu, memory" as if each were singular.
    - **The web keeps a hardcoded constant** (`workerSizes.ts`), exactly as
      `web/src/lib/workerTemplates.ts` mirrors `workertmpl.Names`, and **a vitest
      test reads the golden across the tree** to pin it. Not a new idiom:
      `agent/test/claim-skills-contract.test.ts` already does this against an api
      golden, and its comment blesses the move. **M5 already built this gate for the
      size NAMES** (`web/src/lib/workerSizes.test.ts`) — M6 extends the existing test
      with the quantities rather than creating it.
    - **The cross-tree read is `import … from "…/hosted_sizes.json?raw"`, NOT
      `node:fs`** (M5, 2026-07-16 — measured, not reasoned). The architect's design
      specified `node:fs`, modelled on `agent/test/claim-skills-contract.test.ts`;
      that **does not transplant**, because `agent/` is a Node project and `web/` is a
      pure browser one with no `@types/node` — so `node:fs` fails `tsc --noEmit`
      (TS2307), and adding `@types/node` would widen Node globals across every `src/`
      typecheck for the sake of one test. `?raw` matches `vite/client`'s ambient
      wildcard module declaration, so **tsc never resolves the path** while vitest
      resolves it for real. Recorded because it is invisible until you try it, and
      M6 would otherwise rediscover the TS2307 from scratch.
    - **A shipped web source file may NEVER `import` the golden**: `web/Dockerfile`
      copies `web/` and `docs/` only, so an import would build locally and **break
      the image**. Test-only read. This is doubly why `?raw` is right: `tsc --noEmit`
      runs over the test file *inside* the image build, where `api/` does not exist,
      and the ambient wildcard is what lets it type-check clean there. **Proven, not
      assumed** (M5): the build context was reconstructed (`web/` + `docs/`, no
      `api/`) and the real `npm run build` run green inside it, and tsc was shown to
      pass with a deliberately bogus `?raw` path — so the gate is genuinely invisible
      to the compiler. The one honest cost: a typo'd path fails only when vitest
      runs, never at compile time. CI runs vitest, so it is caught; a reader must
      just not assume tsc has their back.
    - **Decision 7 is not bent by any of this**: nothing on the wire carries
      quantities, `workersize` stays names-only, and the controller remains the
      authority that renders. The golden is a **build-time artifact, not a runtime
      contract** — the drift gate is a test.
    - Honest cost: three copies (web constant, golden, controller table) instead of
      two. The golden buys only that they **cannot drift silently**. It does not
      cover deployment skew (a released bundle showing last release's numbers) —
      the same limit the name golden carries, and strictly less harmful here: a
      stale label misinforms, a stale name strands a worker.
- [ ] **M7 — Docs + specs**: new `docs/hosted-workers.md` (audience: user);
  updates to `worker-setup.md`, `configuration.md`, `admin-settings.md`,
  `vault-threat-model.md` (join-token-in-etcd residual), `ARCHITECTURE.md`
  (controller trust boundary); `specs/ai.md` updated; `specs/human.md` marks
  the deferred operator item as partially delivered (user approval required
  for that edit). Success: `check-docs` green.

## Milestone dependency graph

| Phase | Milestones | Depends on | Files touched | Parallel? |
|---|---|---|---|---|
| 1 | M1 | — | `controller/`, api protocol endpoint, migrations | starts alone |
| 2 | M2, M5 (mock-first), M6-CI (images) | M1 contracts | api handlers/settings; `web/src`; `.gitlab-ci.yml` | M2 ∥ M5 ∥ M6-CI (separate files) |
| 3 | M3, M4 | M1, M2 | controller k8s impl; api TLS listener; chart policies | M3 ∥ M4 (different components) |
| 4 | M6-chart+rollout, M7 | M3, M4, M5 | `deploy/chart`, `docs/`, `specs/` | M6 ∥ M7 |

## Risks

- **New component (controller) to build and operate.** Deliberate cost of
  honoring the "never the api" constraint; kept small (poll + reconcile, no
  CRDs, no webhooks).
- **Join token plaintext in etcd for the worker's lifetime** (Decision 3
  residual) — bounded by the empty namespace + create/delete-only Secret RBAC;
  documented in the threat model; rotation is v2.
- **CNI may not express FQDN egress** — M3 verifies; fallback CIDR allowlist
  with the residual documented.
- ~~**PRD #51 M2's entrypoint must tolerate a non-root start**~~ — **RETIRED
  2026-07-16.** #51 accepted the requirement and implemented it (verified on their
  branch at `fbd916c`; see Decision 6). What remains is not a risk but a
  **merge-order dependency**: M3 must build against an image carrying that
  entrypoint, so #51 lands first or M3 pins a pre-#51-M2 tag. Re-verify at M3 start
  rather than trusting this note — #51 is still in flight.
- **Per-template images multiply CI cost** — build list bounded by a CI
  variable.
- **PVCs accrue cost** — deleted with the worker; orphan sweep flags leftovers.

## Decision Log

- 2026-07-16: Initial choices at PRD creation (user): self-service + quota,
  template + size types, k8s-only scope. Initial mechanism draft was
  api-managed Deployments.
- 2026-07-16: Parallel review (design + security audit + fact-check). Blocker:
  api-managed Deployments contradicted `specs/human.md` ("never the api") and
  opened a namespace-wide secret-mount escalation. User decisions: **dedicated
  controller** (spec-conformant), **TLS on the worker hop in v1**, **trimmed v1
  surface** (preset constants, no restart endpoint, heartbeat-only status).
  Review fixes folded in: api NetworkPolicy ingress amendment, `Recreate`
  strategy vs RWO PVC deadlock, imagePullSecrets, zero-permission worker SA
  with no token automount, file-mounted join token, delivered-once token
  handoff (vault-bypass finding), ResourceQuota/LimitRange backstop, atomic
  quota, upgrade/drift semantics, template-drift invariant, cap=1 presets
  until PRD #51, M5 split across phases in the dependency table.
- 2026-07-16: Cross-PRD collision with #51 settled (user). #51's M1 gate had
  since settled mechanism **(A1)**: a root-entry `setpriv` drop needing ambient
  `CAP_SETUID`/`CAP_SETGID`, with the `USER` line removed from both agent
  Dockerfiles — structurally incompatible with Decision 6's PodSecurity
  `restricted` namespace. Options weighed: **(a)** adopt #51's (C)/two-container
  form now — rejected, it forces #51's deferred `sdk-executor.ts` IPC rebuild
  (pid capture, `killAgentTree`, in-process abort → RPC) into this PRD's M3,
  making a Medium-priority convenience feature block on the hardest part of a
  security PRD, while the only work it saves is ~100 lines of chart YAML;
  **(b)** relax the namespace to `baseline` + `cap_add` — rejected, it puts
  `CAP_SETUID` in a pod on a shared cluster and drops the label that bounds a
  compromised controller, for the sole pod that would need it; **(c)** chart
  overrides `command:`/`runAsUser:` to bypass #51's wrapper from the pod spec —
  rejected, it silently bypasses a security wrapper by duplicating the image's
  CMD in YAML and drifts invisibly whenever #51 touches the entrypoint.
  **Chosen:** v1 single-container at `runAsUser: 10001`, with a conditional
  non-root start required of #51 M2 (relayed to that session the same day, while
  its `entrypoint.sh` was still uncommitted WIP — cheapest possible moment, and
  its file to own). The prior "nothing here may contradict that design" clause
  was already false and is struck. Sequencing note: M1/M2/M4/M5/M6-CI need
  nothing from #51; only M3 and M6-rollout depend on the image posture.
- 2026-07-16: **The delivery ack moved from a controller assertion to a proof of
  possession** (M1 review wave: reviewer + auditor + architect). This and Decision
  1's Secrets line are **the same decision** — the ack is what makes "the controller
  writes Secrets and never reads them back" achievable.
  - *The defect.* M1's first protocol had the controller ack `Materialized: []string`
    (bare worker ids), with the UPDATE guarded on `worker_id` alone. So the ack
    asserted "a Secret exists for W" while the api destroyed "whatever plaintext is
    parked for W". Those diverge on rotation: the api parks T2 → an in-flight poll
    whose observation predates the rotation acks the id → acks apply before desired
    state is computed → **T2 is destroyed, never delivered** → the response reports
    `join_token: null` → the controller leaves the old Secret → the pod holds T1
    while `workers.token_hash` is sha256(T2) → 401 forever, in a row reading
    `(NULL, delivered_at)` = the migration's documented "delivered and destroyed
    (steady state)". Deterministic, not a race; proven against the coder's own fake
    store. All three validators found it independently.
  - *The second, independent defect.* Reading Secret existence needs `get`/`list` on
    Secrets, which Decision 1 denies. M1's contract was silently obligating M3 to
    widen the Role. Found independently by two of three validators.
  - *Rejected — `{id, generation}` on the wire* (the architect's own first proposal,
    which it withdrew): reaches for a new version discriminator when one already
    exists in the schema (`workers.token_hash`, which rotation overwrites by
    definition), and needs a Secret read to obtain the same property worse.
  - *Rejected — observe the Deployment/Pod instead of the Secret* (reviewer's
    option (a)): genuinely stays inside the granted verbs, and its supporting claim
    is true (a pod whose secret volume is absent never reaches Running — kubelet
    blocks at `ContainerCreating`/FailedMount), so Running is strictly stronger
    evidence than "the object exists". It loses on a different axis: its proof rests
    on a **controller-internal ordering invariant** (write Secret(T2) *before*
    patching the generation annotation). Patch first, or die between the two calls,
    and the pod rolls mounting T1, reaches Running carrying annotation N+1, and acks
    `{id, N+1}` — destroying a T2 that was never written anywhere. Silent and
    unrecoverable. The api would be destroying its only plaintext on the strength of
    a `Reconcile()` performing two writes in the right order: code the api cannot
    see, verify, or enforce, inside the component the RBAC exists to distrust.
    Timing did **not** discriminate between the options — option (a)'s ack also
    requires a Running pod, so scheduling and image pull sit inside its window too;
    the delta is container-startup seconds against a 1h TTL.
  - *Accepted — registration-derived.* Delete `PollRequest`/`Materialized`; the poll
    becomes a GET with `Cache-Control: no-store`; the ack hooks into
    `handler.WorkerRegister` (hosted-only, best-effort, non-fatal — a cleanup
    failure must never fail a registration; the TTL is the backstop). `workersvc`
    still never learns about hosted workers. `Observe` **survives** and loses only
    its ack role: Decision 9's drift and orphan passes need a cluster read
    regardless, so it widens to carry per-worker observed state read from Deployments
    and Pods, never Secrets.
  - *Consequence — one verdict inverts, and the clause choice is downstream of the
    option.* Under a controller-asserted ack, promoting an expired `(NULL, NULL)` row
    to `delivered_at` **launders** the strand signal the TTL exists to raise, so the
    strict `AND token_ciphertext IS NOT NULL` would be right. Under proof-of-
    possession the identical transition is **true** and is a correct self-heal, so
    the guard is `AND (token_ciphertext IS NOT NULL OR delivered_at IS NULL)` — kept
    for idempotence on re-registration (a rescheduled pod re-registers with the same
    token and must be a no-op). Taking the strict clause with the accepted option
    would leave a worker that is online and heartbeating permanently marked
    "stranded, recovery must mint a new token" — false, and exactly the state that
    would make M2 rotate a token for a healthy worker. Same SQL, opposite verdict,
    because the trigger's epistemic content changed from a report to a proof.
  - *Process note, recorded because it is the reason this was caught.* The architect
    was dispatched **after** implementation — this branch's base predated the role
    existing on `main`, while `.claude/agent-team.md` prescribes it *before* the coder
    for a new component or contract. The cost was exactly one structural decision
    (whether the ack names a token or a worker), and it was caught only because the
    wave ran three independent lenses over the same diff. Two validators also had
    their own asks retracted or overturned during the wave (the placeholder-guard ask
    was unsatisfiable: a sha256 is uniformly distributed regardless of preimage, so no
    entropy signal survives in it to check).
- 2026-07-16: **Hosted worker sizes ship as names only; the quantities are deferred
  to M6** (user). Raised by the architect at M5 design: the provision picker wants to
  show what S/M/L *buy*, and **the numbers exist nowhere** — Decision 7 names the
  fields (cpu, memory, the two volume sizes) but no values, `workersize` is
  deliberately names-only, and the controller's table is unwritten. So the question
  was never "where does the UI read them from" but "**nobody has chosen them**", and
  choosing them is a product decision, not an implementer's.
  - *Chosen*: M5 ships names only; M6 owes both the numbers and the display.
  - *The architect's objection, preserved because it was overruled rather than
    answered*: a user choosing a size with no idea what it buys is choosing blind,
    and that is the size picker's entire job. This is an **accepted cost**, not a
    design choice — M6 inherits a debt, not a blank.
  - *Rejected — M5 invents the numbers and M3 matches them later*: that is the
    silent-lie failure mode this whole question exists to prevent (the UI advertises
    2 GiB while the controller renders 4, with nothing red anywhere).
  - *The display mechanism is settled and specified in M6's bullet, unimplemented*:
    the quantities ride `hosted_sizes.json` (a build-time golden, not the wire),
    the web mirrors them in a constant, and a vitest test reads the golden across the
    tree to pin it — so the api never learns them and Decision 7 stands unbent.
    Reading them from the authority at runtime is structurally impossible: Decision 2
    makes the controller outbound-only, so the api can never ask it anything.
- 2026-07-16: **Admin-configurable S/M/L quantities proposed (user), and deferred to
  v2 — where this PRD's own non-goals had already put it.** The user asked whether
  S/M/L could be set from the admin webui rather than compiled in. The instinct is
  sound and the need is real (clusters differ in capacity), and it is **already on
  the roadmap**: the v1 non-goals say "no preset CRUD… all v2". Recorded here so it
  is not re-litigated, and because the v2 implementer needs the coupling below.
  - *It does not dissolve the open question it was raised against.* Even a
    configurable preset needs a **default**, so somebody still picks S/M/L's initial
    values (see M6). What it would remove is the display-drift *mechanism* — a
    ~40-line golden + two tests — not the product decision.
  - *Architecturally it inverts the authority*: the api would hold the quantities
    and ship them on the poll, and the controller would render what it is told.
    Decision 7 pins the opposite (the api sends the NAME; the controller resolves),
    and that is the same pattern the **template** already follows — the api says
    `jvm`, the controller resolves it to an image reference. The api sending
    quantities is far less dangerous than the api sending an image, but it breaks
    the one rule that makes the boundary checkable: *the api sends names, the
    controller owns every mapping to a concrete pod-spec value.*
  - *The decisive fork, and it is a real one — both prongs are bad.* An admin-typed
    quantity must be validated somewhere. **(a) Validate in the api**: it cannot.
    `k8s.io/apimachinery` is on `hostedsvc/no_kube_dependency_test.go`'s banned
    module list, which exists to enforce Decision 1 — so validating means either
    deleting a line from a tested guardrail for the sake of a display label, or
    hand-rolling a k8s quantity parser (decimal/binary SI, milli-units, exponents)
    whose subtle wrongness is worse than none. **(b) Skip api validation** and let
    the controller parse defensively: then a fat-fingered `2GGi` is accepted by the
    settings PUT with no feedback and the worker silently never appears — the M2
    auditor's fail-open finding, upgraded from "a bad quota reverts to a default" to
    "a bad quantity strands workers, with no error anywhere". That the api cannot
    cleanly validate a quantity is not a tooling accident; it is the boundary saying
    the api is the wrong owner. The controller validates trivially — it already
    imports apimachinery. Responsibility belongs where the knowledge is.
  - *Why v2 is the right home rather than a compromise.* v1 deliberately has **no
    pod-phase status in the UI** (Decision 10: heartbeat-only, `kubectl` is the
    diagnostic) — the same non-goal line that defers preset CRUD. So v1 is the worst
    possible release in which to let an admin type resource values: the failure mode
    of the feature lands exactly in the blind spot v1 accepted. The two v2 items are
    **coupled, not merely co-scheduled** — pod-phase status is what shows the admin
    why their preset produced no pod. Ship them together; doing either alone is what
    would be wrong.
  - *On the costs this would have removed* (three copies, a build-time golden, no
    protection from deployment skew): those were recorded as honest costs, not as a
    plea for a better mechanism. They are small, and a stale display label is the
    mildest failure in this PRD.
- 2026-07-16: **The design record moved into this PRD, because the working notes
  were never in git** (architect + coder). The M2/M5 designs and the M3 carry-forward
  were written to `.claude/agent-team-tasks/`, which **`.gitignore:27` deliberately
  ignores** (`.claude/agents/` and `agent-team.md` are tracked, so the ignore is
  considered, not an oversight) — they exist only inside the PRD worktree and die
  when it is cleaned up after the merge. The coder declined to `git add -f` over a
  deliberate ignore, correctly calling it a repo-convention decision rather than a
  coder's. This repo already answers it: PRDs are the design rationale record. The
  load-bearing consequence, and the reason this is logged rather than just fixed:
  **M2's `hosted_sizes.json` is inert until M3 parses it**, and the only artifact
  saying so was an ignored scratch file — had it evaporated, M2 would have shipped a
  golden nobody knew M3 must consume, and the drift gate would silently never exist.
  Its content now lives in the M3 bullet above.
- 2026-07-16: **M6 directive, not a risk** (auditor). `randAlphaNum` re-renders on
  every `helm upgrade` and silently rotates a chart-generated credential unless
  anchored with `lookup` or a pre-created Secret — a real and well-known Helm gotcha,
  but the premise does not apply here: `deploy/chart` has no `randAlphaNum` and sources
  secrets through Infisical (`infisical-list.yaml`, the `uzi-secrets` materialization).
  So M6 must **source the controller credential through the existing
  `infisicalList`/existing-Secret pattern and never chart-generate it**, and the hazard
  never arises. Related, for M6/M7: the plaintext (controller file mount) and its hash
  (api env) are two values an operator must keep in sync, and a rotation touching only
  one **fails closed** in both directions (the controller 401s) — the footgun is an
  outage, never a bypass.
- 2026-07-16: **Decision 8's quota mechanism was wrong, and is amended above** (M2
  priming wave: auditor + reviewer independently, corroborated by the architect;
  confirmed by measurement during M2). The requirement ("atomic, no TOCTOU") was
  always right; the parenthetical named a mechanism that does not deliver it. The
  correction is recorded in Decision 8 itself rather than only here, because the
  wrong text was actionable — a coder implementing it verbatim would have shipped an
  unbounded quota that no fake-store test could see.
  - *What decided it was a measurement, not an argument.* With the lock removed and
    the quota check otherwise intact, 8 of 8 concurrent provisions passed a quota of
    2; with it restored, exactly 2. Every reviewer of the original text had to reason
    about whether the check was self-sufficient; none of that reasoning was needed
    once it was run.
  - *The guarded `INSERT … WHERE count < quota` was tried and dropped.* It has the
    same hole as a plain count (each subselect evaluates against its own snapshot)
    while **reading** as though it closed it, and its name asserted in an identifier
    a property it did not have. No honest name exists for "insert if the caller's
    lock is held and the count is under" — that is the signal the property does not
    belong in the statement. The count now happens in Go under the lock, the shape
    `createUserFirstAdmin` already uses for the identical race.
  - *A test that cannot fail is not a gate.* Both lock tests were verified by
    mutation rather than by inspection, and the numbers moved the design: the
    N-goroutine race test misses a missing lock ~1 run in 20 and a lock taken after
    the count ~1 in 6, so neither defect may rest on it. The deterministic test
    (hold the lock, fill the quota underneath the blocked provision, release) decides
    both by construction. The same lens found this PRD's own M2 live-DB tests
    skipping silently in CI — green pipeline, zero coverage — now gated by
    `test:api-store-it`.
- 2026-07-17: **M3's design questions settled, and three of this PRD's own claims
  corrected in the commits that disproved them** (architect design + coder). The four
  open questions are answered in the M3 bullet: the Decision 9/11 collision (by
  provenance), unknown-name tolerance (log, skip rendering, KEEP desired), the
  template golden (in scope, both halves), and chart ownership (M3 = the whole
  security envelope; M6 = the controller Deployment + rollout).
  - *Correction 1 — Decision 5(a) named ONE ingress rule and forgot the controller.
    A LIVE BUG, not a doc slip*: every controller poll would have been dropped by the
    api's default-deny. Invisible because the chart shipped no controller.
  - *Correction 2 — Decision 9's "the chart injects the agent image tag into the api
    config" is not implementable*: the api knows no image tag and M1's wire carries no
    image field. The text predates M1's protocol. The tag is the controller's.
  - *Correction 3 — M6's display golden carried a per-size `nix`* that Decision 7's own
    later correction removes, and it was missing the request/limit split M3's Burstable
    presets require.
  - *What running it changed, and the reason the kind gate was worth its cost.* Both
    of M3's own hard failures were found by a real kubelet and were invisible to every
    fake-client test, because both are about the volume the kubelet mounts, not about
    the objects we render: tar restoring metadata onto a mount root it does not own,
    and an interrupted seed that could never re-run. Each CrashLooped the pod **after
    copying the store perfectly** — the worst shape of failure, since the work is right
    and the exit code is not.
  - *And the reason to keep saying what it did NOT prove.* The same green run says
    nothing about NetworkPolicy (kindnet enforces none) and nothing about the fsGroup
    pin (kind's local-path PV is hostPath-backed and ignores fsGroup — asserted as an
    observable, so it reported the gap instead of passing silently). Both defer to M6.
    The design predicted all three divergences before the cluster existed; the run
    confirmed the predictions rather than discovering them.
- 2026-07-17: **The worker-namespace ingress rule opens an XFF forgery, and M3 closes
  it with TWO layers** (M4 review: reviewer BLOCKING + auditor, converging from
  different lenses; verified and then MEASURED by the M3 coder before implementing).
  - *The defect.* `mw.ClientIP` trusts `X-Forwarded-For` on the **peer IP alone**
    (`ratelimit.go:157`) — it knows nothing about which listener or route a request
    arrived on. dev-cluster's `TRUSTED_PROXIES` is the **whole pod CIDR**
    (`10.244.0.0/16`), because pod IPs are dynamic and no narrower value is
    maintainable, so **hosted worker pods are trusted proxies by construction**. It was
    inert only because the api NetworkPolicy admitted web pods and nothing else — and
    **Decision 5(a)'s rule, which admits the worker namespace to the TLS port, is
    exactly what removes that mitigation**. So it is M3's precondition, not M4's
    cleanup: M3 is the milestone that arms it.
  - *Measured on a live api, not argued* (kind, `TRUSTED_PROXIES` = the pod CIDR,
    attacker pod inside it): 12 `POST /api/auth/login` with a **rotating** XFF →
    **12 × 401, zero 429s: the per-IP auth rate limit bypassed outright**. The same 12
    from the same **unforged** IP → 429 at request 11. So the limiter is real and the
    forgery defeats it.
    - *Correction to this entry's own first draft, and to the brief it came from
      (architect, verified independently here 2026-07-17): the blast radius is the
      **auth rate limiter ONLY**.* "Audit attribution is forgeable the same way" is
      **false of this codebase** and must not be repeated: `ClientIP` has exactly three
      call sites, all in `ratelimit.go`; `X-Forwarded-For`/`RemoteAddr` appear in no
      other non-test Go file; and **no migration defines an IP column at all**
      (`ip_address`, `client_ip`, `inet` — none exist), so uzi never persists a client
      IP and there is nothing to attribute. A bypassed brute-force control on the admin
      login is the whole of the impact, and is reason enough — the case does not need
      an embellishment that cannot happen.
  - *The fix is TWO layers, and both ship* — CLAUDE.md: "four independent layers…
    don't weaken any layer on the theory another covers it."
    - **(a) The TLS listener serves a SUBSET router**: `/api/worker/*` +
      `/api/controller/*` only. `/api/auth/*` and `/api/admin/*` are **not mounted**
      there, rather than mounted-and-defended — a hosted worker runs an agent against a
      user's cloned repo, a semi-hostile position by design. The set was **derived from
      the agent and controller code, not assumed**: the agent declares one base
      (`WORKER_API_PREFIX = "/api/worker"`) and opens **no websocket**, so `/api/ws` —
      which an earlier proposal included — is deliberately absent. Verified live from a
      trusted-proxy pod: every sensitive route 404s on `:8443`; the worker/controller
      surface 401s (exists, needs a credential).
    - **(b) `stripXFF` on that listener.** Narrowing the CIDR was **rejected**: pod IPs
      are dynamic, which is why the whole-CIDR value exists. Stripping makes the
      property true **by construction** rather than by CIDR bookkeeping that a future
      pod-CIDR change would silently invalidate.
    - *The constraint that shaped (a)*: M4 built the router ONCE because building
      `Routes` twice creates two middleware chains and silently doubles every per-IP
      budget. The subset therefore **shares the limiter instances** — the mounts are
      functions both routers call, so there is one registration site per route and the
      two surfaces cannot drift.
  - *`docs/configuration.md` asserted the property with nothing enforcing it* ("no
    `X-Forwarded-For` is trusted from them") and claimed both ports serve the same
    routes. Both corrected to say what **enforces** them, and to name the one condition
    that would change it: if anyone ever fronts the TLS port with a real proxy, the
    strip is the line to revisit.
  - **Explicitly NOT M3's, recorded so it is not lost**: the same weakness **predates
    M4 and exists on compose today** (`docker-compose.yml` defaults `TRUSTED_PROXIES`
    to include the compose network, so the agent container is already a trusted proxy).
    The measurement above was taken on the plain listener for exactly that reason. M4
    did not introduce the weakness — it added a document that denied it. The lead is
    raising the compose half separately.
