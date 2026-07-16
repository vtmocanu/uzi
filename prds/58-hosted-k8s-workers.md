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
   reaches. Verbs pinned minimal: Deployments create/get/patch/delete; Secrets
   **create/delete only** (it writes them, never needs to read them back);
   PVCs create/delete; Pods get/list (drift detection only).
2. **Controller ⇄ api protocol: outbound-only poll, like a worker.** The
   controller authenticates to the api with its own bearer credential
   (hash-stored, chart-provisioned) and polls a controller-facing endpoint for
   desired state (hosted-worker rows: id, template, size, generation). No
   inbound port on the controller; the api never dials it. The controller is
   stateless — desired state lives in the DB, observed state in the cluster; it
   reconciles the two on every poll, so api or controller restarts lose nothing.
3. **Join token: delivered once, never at rest in plaintext server-side.** At
   provision the api generates the token, stores the sha256 (as today), and
   keeps the plaintext secretbox-sealed only until the controller's next poll
   picks it up (delivered-once: the api then deletes the sealed copy). The
   controller writes it into the worker's k8s Secret, mounted as a **file**
   consumed via `UZI_WORKER_TOKEN_FILE` — never a `secretKeyRef` env var, which
   would reopen the `/proc/<pid>/environ` leak that compose hardening (M6,
   `docs/proc-hardening.md`) closed. Residual, documented in the threat model:
   the token is plaintext in etcd for the worker's lifetime; anyone who reads
   it can impersonate that worker and claim its owner's runs (receiving the
   decrypted Anthropic token when the vault is unlocked). Bounded by the empty
   worker namespace + minimal RBAC; per-worker rotation is v2.
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
   was still uncommitted WIP. Posture disclosed: a hosted worker carries PRD
   #51's same-uid residual exactly as today's compose worker does — hosting
   neither widens it (the residual is intra-user, `ARCHITECTURE.md:427-428`)
   nor closes it.
7. **Worker type = template, size = built-in preset.** Type selects the
   published per-template agent image (`agent-base`, `agent-jvm`; tag = release,
   Model B like api/web); the deployed image's baked `UZI_WORKER_TEMPLATE` must
   equal the declared type, or the server would provision a worker that badges
   its own template drift. Sizes are **code constants** (S/M/L: cpu, memory,
   PVC size) — no CRUD. All presets pin `WORKER_MAX_CONCURRENT_RUNS=1` until
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
   busy workers roll after their runs finish. Missing objects are recreated;
   unrecognized `uzi-hw-*` objects are flagged as orphans, never adopted.
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

- [ ] **M1 — Controller skeleton + protocol**: new `controller/` component
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
- **PRD #51 M2's entrypoint must tolerate a non-root start** (Decision 6). If it
  lands unconditional, M3's pod CrashLoopBackOffs at `setpriv --reuid`; fallback
  is pinning a pre-#51-M2 agent image tag until the conditional lands.
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
