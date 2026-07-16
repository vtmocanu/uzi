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
   silently break this feature) gains one rule: worker-namespace pods, matched
   by namespace + pod label selector, to the TLS port only. (b) The worker
   namespace gets default-deny ingress (nothing dials a worker) and
   default-deny egress with explicit allows: DNS, api (TLS port), forge, nix
   substituters, Anthropic API — explicitly excluding the kube-apiserver
   ClusterIP and the cloud metadata IP (169.254.169.254). FQDN egress rules
   need CNI support (Cilium/Antrea) — M3 verifies what dev-cluster's CNI can
   express; fallback is a maintained CIDR allowlist with the gap documented as
   an accepted residual, not silently dropped.
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
     bare** (cheap, LAN-local, a pure cache — see Decision 6). All presets pin `WORKER_MAX_CONCURRENT_RUNS=1` until
   PRD #51 lands: cap>1 would server-provision the documented intra-user
   concurrency residuals (`docs/worker-setup.md` §Concurrent runs). Type and
   size are validated server-side against the built-in lists; user input never
   reaches an image reference or pod spec as free text.
8. **Self-service + one quota knob.** Any user may provision up to the
   admin-set per-user quota (single admin setting, default 2, 0 disables
   self-service). Enforcement is atomic (guarded insert under the same
   transaction, no TOCTOU). Quota counts hosted workers only. Defense-in-depth
   at the k8s layer: the worker namespace carries a ResourceQuota + LimitRange
   so an app-level bug cannot noisy-neighbor the shared cluster. Provision and
   delete endpoints are rate-limited (existing limiter pattern, PRD #53).
9. **Upgrade/drift semantics.** The chart injects the release's agent image
   tag into the api config; a hosted worker whose Deployment differs from
   desired (image tag, spec hash) is *drifted*. The controller rolls a drifted
   worker only when it holds no non-terminal run (same predicate as delete);
   busy workers roll after their runs finish. Missing objects are recreated
   (**except Secrets — see below**); unrecognized `uzi-hw-*` objects are flagged
   as orphans, never adopted.
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

## Milestones

- [x] **M1 — Controller skeleton + protocol** (landed 2026-07-16): new `controller/` component
  (Go, reuses api's module or its own — decide in M1), controller bearer auth,
  desired-state poll endpoint, delivered-once token handoff (Decision 3),
  migration extending `workers` with hosted metadata (kind, template, size,
  generation), feature flag. api gains no k8s access. Success: protocol unit
  tests against a fake store; compose boots with the flag off, zero behavior
  change.
- [ ] **M2 — Hosted worker API + quota**: provision (type + size, validated)
  and delete endpoints; atomic quota enforcement + the admin quota setting;
  rate limits. Success: API tests cover quota races, disabled flag,
  delete-while-busy refusal; provisioning is not user-reachable until M3
  (endpoints ship behind the flag).
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
- [ ] **M3 — Materialization + policies**: controller renders Secret
  (file-mounted token) / Deployment (Decision 6) / PVC; worker namespace with
  PodSecurity `restricted`, ResourceQuota, LimitRange, both NetworkPolicies
  (incl. the api-ingress amendment); CNI FQDN-egress capability verified on
  dev-cluster; drift/orphan reconciliation per Decision 9. Success: fake-client
  tests assert rendered objects; a hosted worker on a kind cluster registers,
  goes online, and completes a stub run end to end.
- [ ] **M4 — TLS worker hop**: api TLS listener + cert wiring (cert-manager in
  the chart), controller and hosted workers on `https://`, docs for the cert
  values. Success: claim traffic on dev-cluster is TLS; plain-HTTP worker port
  unreachable from the worker namespace.
- [ ] **M5 — Web UI**: Settings → Workers hosted section — provision dialog
  (type + size), hosted-marked rows, delete; hidden when hosting is disabled.
  Success: vitest coverage; web-ux review.
- [ ] **M6 — CI + chart + rollout**: publish per-template agent images
  (`agent-base`, `agent-jvm`; templates listed in a CI variable to bound build
  cost) and the controller image on `v*` tags; chart adds controller
  Deployment, worker namespace + RBAC + quotas + policies, hosting values;
  ArgoCD rollout to dev-cluster. Success: a tagged release provisions a real
  hosted worker on dev-cluster that completes a run.
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
