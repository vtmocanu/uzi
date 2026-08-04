# 224 — Worker pods declare no ephemeral-storage request

**Status:** design wave in flight. **Branch:** `224`. **Branch point:** `0111f01c` (== `origin/main`,
verified with `git fetch origin && git rev-parse origin/main` at kickoff, 2026-08-04).

**This file is the SPEC.** Corrections amend this file with a dated `## Amendments` entry; messages
only name the section that moved. The upstream statement of the problem is GitLab issue
**#224** (`env -u GITLAB_TOKEN glab issue view 224`) — read it, it carries the raw eviction evidence.
`.claude/agent-team-tasks/` is gitignored here (`.gitignore:52`), so the brief lives in `prds/`,
alongside its sibling `prds/218-park-resume-work-loss.md`.

---

## 1. The defect, in one paragraph

A worker pod evicted for **node ephemeral-storage pressure** destroys every in-flight run's work.
kubelet evicts, the ReplicaSet reschedules elsewhere, the new pod registers, `Service.Register`
calls `RequeueWorkerRuns`, and on re-claim `createOrAttachRunnerClone` → `runnerCloneForBranch`'s
unconditional `fs.rm` reseeds from origin. Nothing was ever pushed. The run's feed shows a normal
re-claim, so the loss is silent. kubelet's own message names the cause:
`Container worker was using 100060Ki, request is 0, has larger consumption of ephemeral-storage.`

**🔴 WHAT THIS ISSUE'S FIX DOES AND DOES NOT DO — read this before §5, or the verification bar
reads as closing the issue.** Nothing in §3 touches steps 5-7 of that chain. A declared request
changes kubelet's eviction **ranking**; it does not make a pod eviction-proof, and on a 17.55 GiB
node running a 5.77 GB agent image that is not a remote scenario. So #224 as scoped **lowers the
frequency** of a silent, total work loss. It does not make the loss non-silent or non-total. The
remedy for the loss itself is #218's shutdown-path fetch-back, deliberately out of scope here
(§3). The reviewer (N7) and the auditor reached this independently, from different directions; it
is stated at the top because a reader who takes §1 as the defect and §5 as the bar will conclude
more than either supports.

**🔴 AND THAT REMEDY SENTENCE IS ITSELF TOO STRONG — corrected by A5.1 and A7.4.** #218's
fetch-back **cannot fire on a kubelet eviction on this cluster as configured**: every threshold here
is hard (`evictionSoft: null`), so the eviction manager overrides the grace period to 0 and the
clamp is 2 seconds. It **does** save every *voluntary* shutdown path — rollout, scale-down,
spec-hash roll, node drain — all of which honour the full 30s, and that includes the fleet roll this
very fix triggers. Making it fire on eviction would need `evictionSoft` +
`evictionMaxPodGracePeriod` in the kubelet config: a cluster-config change, not a repo change.

**Request is 0 because nothing declares one.** Verified on this tree at `0111f01c`:

```
rg -n 'EphemeralStorage|ephemeral-storage' controller/    -> no matches
```

The four places a container's resources are set in `controller/internal/kube/render.go` all set
CPU and memory only:

| Container | Source | Line |
|---|---|---|
| `worker` | `spec.Size.{CPU,Memory}{Request,Limit}` | `render.go:846-855` |
| `dind` sidecar | `cfg.dindResources()` | `render.go:236-253`, used at `:973` |
| `dind-init` | `dindInitResources` | `render.go:256-266`, used at `:1076` |
| `seed-nix` init | `seedResources` | `render.go:291-300`, used at `:699` |

`preset.Size` (`controller/internal/preset/preset.go:47-55`) carries `CPURequest`, `CPULimit`,
`MemoryRequest`, `MemoryLimit`, `DataSize` — no ephemeral field.

---

## 2. 🔴 The premise that must be settled FIRST

**The chart already ships a ResourceQuota key for exactly this resource, and if it is live in the
cluster the pods should not be admissible at all.**

`deploy/chart/values.yaml:518` sets, under `workers.docker.quota` (`enabled: true` at `:512`):

```yaml
      # emptyDir dind data roots land on node ephemeral storage; bound it so a runaway
      # image pull cannot fill a node.
      ephemeralStorage: 200Gi
```

which `deploy/chart/templates/worker-docker-namespace.yaml:88-90` renders as
`requests.ephemeral-storage` in the docker tier's ResourceQuota. That same template's own header
comment (`:68-71`) states the mechanism:

> Same load-bearing side effect: a quota on requests.\* makes admission REJECT any pod
> with a container declaring no requests — initContainers and native sidecars included.
> The controller renders explicit requests on the seed, dind-init and dind containers for
> exactly this reason.

And the docker LimitRange's `defaultRequest` (`values.yaml:530-532`) is **cpu + memory only**, so
nothing defaults the missing value in.

**So there is a contradiction on the table, and resolving it changes the fix:**

- If the quota key is live on dev-cluster, docker worker pods declaring no ephemeral-storage
  request should be rejected at admission — yet issue #224 shows them running and being evicted.
  One of (a) the chart default is overridden to empty in the ArgoCD values, (b) the quota-forces-
  declaration rule does not apply the way the chart comment claims, or (c) something else supplies
  the value, must be true.
- ~~If (b) is what is true, the same comment is load-bearing for `seedResources` / `dindResources`
  / `dindInitResources` and is wrong there too, and CLAUDE.md's fix-the-doc rule applies.~~
  **WITHDRAWN — see Amendment A3/§1. Do not act on this bullet.**

**Deliverable for the design wave: settle this with a citation, not an argument.** The live values
are in the ArgoCD repo (`~/repos/myorg/myorg/k8s/argo-apps/`, per the user's global
CLAUDE.md) and the live cluster is `dev-cluster`. A rendered `helm template` with the shipped
defaults plus a `kubectl get resourcequota -n uzi-workers-docker -o yaml` settles it outright.

Note the restricted default tier (`workers.quota`, `values.yaml:387-393`) has **no** ephemeral key
at all, so whatever is true of the docker tier is not automatically true there.

---

## 3. Scope

**In scope (issue §Suggested fix):**

1. Declare `requests.ephemeral-storage` on the worker container and the dind sidecar in
   `render.go`, and decide on limits (see §4 Q2).
2. Size the values against observed usage, not a guess. The `100060Ki` in the eviction message is
   the value at the moment of eviction and is **not** a steady state.
3. Extend the same treatment to the init containers (`seedResources`, `dindInitResources`), per the
   argument those constants already carry in their own comments.
4. Wire whatever is not a flat constant through the chart the way `dindResources` already is
   (`values.yaml:546-553` → `UZI_WORKER_DIND_{REQUEST,LIMIT}_{CPU,MEMORY}`), so a cluster can tune
   it without a controller release.

**Cleanup — settled, see Amendment A1.** Manual one-off deletion by exact name, plus a recorded
note on why k8s does not reap them. No controller-side reaper in this task.

**Out of scope:** the #218 fetch-back on the shutdown path (explicitly "filed as an amendment there
rather than duplicated here" by the issue itself, and already recorded at `0111f01c`'s tip commit);
#222's stale steer input.

---

## 4. Design questions the wave must answer

Every one of these is a mechanism claim about code or about k8s that can be settled by reading a
file or running one command. **Name the file and quote the line; do not reason from memory.**

**Q1 — Is the quota premise in §2 true?** Blocking. See §2 for what settles it.

**Q2 — Request only, or request AND limit?** This is the trade-off that can make the bug worse.
A container exceeding its ephemeral-storage **limit** is evicted by kubelet — which is the exact
failure this issue is about, arriving from a different direction and now deterministically rather
than only under node pressure. A request alone buys scheduler placement plus eviction-ranking
protection, and cannot itself trigger an eviction. State which, and why, with the eviction-ranking
semantics cited rather than assumed.

**Q3 — Per-size preset field, or flat constant?** `DataSize` varies by size; `nixSize` is
deliberately flat and says why in its own comment (`preset.go:128-146`). A disk-heavy `l` worker
plausibly wants more than an `s`. Note what a preset field does NOT cost: the api's goldens
(`api/internal/hostedsvc/testdata/hosted_sizes.json`) carry **names only**, not quantities, so a new
`Size` field does not move that contract. Confirm that against
`controller/internal/preset/preset_contract_test.go` and `size_display_golden_test.go` rather than
taking this line's word for it.

**Q4 — Does the emptyDir accounting change the sizing?** A docker worker's daemon data root is an
`emptyDir` (`render.go` volumes, and `values.yaml:509` sizes the tier's quota for "~5-20 GiB" of
it). emptyDir consumption counts toward the pod's ephemeral-storage usage. If that is right, the
worker container's own ~100 MiB is not the number that matters and the sidecar's is.
`emptyDir.sizeLimit` is a third instrument neither the issue nor this brief has considered — say
whether it is the better tool for the daemon's data root.

**Q5 — What is the steady-state number?** Nobody has measured it. **Settled in ORDER but not in
VALUE — see Amendment A2:** ship a conservative, fully chart-tunable default now, measure after.
So the question you still owe an answer to is not "what is the number" but **"what measurement
would give it"** (a `kubectl exec du` sweep across live workers, `kubelet_volume_stats`, a Grafana
query) and how conservative the interim default has to be to be safe without the measurement. A
request guessed too low re-creates the bug; too high strands node disk across the fleet.

**Q6 — Do the presets' `max` ceilings need to move?** The docker LimitRange's `max`
(`values.yaml:534-536`) is cpu/memory only today. If a limit is declared per Q2, a `max` for it may
be needed, and the chart comment's invariant ("`max` must stay >= the largest worker preset's
limits") acquires a third dimension.

---

## 5. Verification bar

k8s is the primary runtime here (CLAUDE.md, Conventions, first bullet), so a controller unit test is
necessary and not sufficient. The change must be shown to render correctly and to admit:

- `task gate:controller` green (fmt-check + vet + build + lint + deadcode + test).
- `helm template` of the chart, parsed (`scripts/assert-chart-render.sh` is in the `helm_chart` CI
  job and asserts one `kind:` per document — see CLAUDE.md's `*/ -}}` entry for why a grep is not
  evidence here).
- The rendered worker pod actually carrying the requests, read out of a parse rather than a grep.
- A statement of what the KinD smoke (`e2e:kind-smoke`, protected refs only) does and does not
  cover for this change.

**Gates:** recipes live in root `Taskfile.yml`; **read `.claude/agent-team.md` §Quality gates
(lines 1307-1725) before running any of them** — it carries the golangci-lint host-global lock and
cross-worktree cache traps that will otherwise be misread as findings on this branch.

---

## Roster

`units: 1` — one unit (`controller/` plus `deploy/chart/` plus docs). The chart and the controller
are the two halves of one contract (a value the chart injects, a default the controller owns), and
splitting them would put the seam between two coders instead of inside one commit.

| Role | Disposition |
|---|---|
| architect | **reported** 2026-08-04 at `9cccfa99` — design in A4, incl. the blocking spec-hash fleet roll (A4.1) |
| reviewer | **reported** 2026-08-04 at `9cccfa99` — citations all confirmed bar one attribution; A3 |
| auditor | **reported** 2026-08-04 at `9cccfa99` (detached worktree) + live dev-cluster — A3 |
| fact-checker | **reported** 2026-08-04 at `9cccfa99`/`8778add2` — A5, incl. the refuted 30s grace |
| coder | **BLOCKED on the user's A4.1 sequencing decision.** Design otherwise frozen at A4.3. |
| tester | pending — spawns after the coder's FIRST commit, not at kickoff. Owes Q5's distribution (A3.5's instrument) and A4.8's checks. |
| documenter | pending — CHANGELOG line, plus the **eight** doc corrections in A3.8, mandatory in the same commit as the work. **The CHANGELOG must not say "fixes worker eviction"** (§1's box, A4). |
| spec-keeper | pending — `specs/` exists; dispatch after blocking findings clear |
| web-ux | **closed, REASON CORRECTED.** The original reason was false: the picker *does* render quantities (`"M — up to 2 CPU / 4Gi RAM / 10Gi disk"`, `workerSizes.ts:52-56`) off `hosted_size_specs.json`, not off `hosted_sizes.json`. It stays closed because A4.2 puts ephemeral in `render.go` and leaves `preset.Size` unchanged, so neither golden moves. **Reopen if that reverses.** |
| researcher | closed — no research surface; every question in §4 was answerable from this tree, the chart, or the live cluster, and all four were. |
| release | closed — no release cut in this task's scope. **But A4.1 means the ROLLOUT is not routine**; whoever cuts it reads A4.1 first. |

## Amendments

### A1 — 2026-08-04, USER DECISION: evicted-pod cleanup is manual, not a reaper

The issue's `## Cleanup` section is in scope as a **one-off manual deletion by exact name** plus a
recorded explanation of why they linger (k8s reaps terminated pods only above the control plane's
`--terminated-pod-gc-threshold`, which is not something this repo sets). **No controller-side
reaper.** That would be a new reconcile responsibility, a new RBAC delete verb and its own tests,
which is a different issue from the one-line-cause defect this is.

Deletion obeys CLAUDE.md's namespace rule: **exact names, never a `uzi-` glob.** The three pods are
in `uzi-workers-docker` on dev-cluster and two are named in issue #224's Evidence section.

### A2 — 2026-08-04, USER DECISION: ship tunable now, measure after

Sizing order is settled even though the value is not. **Land a deliberately conservative default
that is fully overridable from chart values** — the precedent to copy exactly is `dindResources`
(`values.yaml:546-553` → `UZI_WORKER_DIND_{REQUEST,LIMIT}_{CPU,MEMORY}`, with the controller's
built-in constants as the fallback for any field left empty). dev-cluster can then tune without a
controller release. **Then open a follow-up issue** to replace the default with a measured one once
the fleet has run on it.

This constrains the design rather than merely scheduling it: any answer to Q3 that makes the value
a compiled-in preset field with **no** chart override does not satisfy A2. A per-size field and a
chart override are not mutually exclusive — say how they compose if you want both.

### A3 — 2026-08-04, DESIGN WAVE: reviewer + auditor. The design is now mostly frozen.

> **If you are a teammate with an in-flight verification assignment on Q1-Q5, file your own verdict
> before reading past this line.** Two instruments have already agreed here and a third that reads
> this first is agreement, not corroboration.

Both reports were reached independently: the reviewer from this tree plus upstream source, the
auditor from a detached worktree plus a live `dev-cluster` read plus upstream source. They agree on
every point below. Where a number appears it is **measured**, not recalled.

**A3.1 — Q1 is SETTLED, the answer is (b), and §2's contingency bullet is WITHDRAWN.**
`pkg/quota/v1/evaluator/core/pods.go`'s `validationSet` (release-1.29, the cluster's version) lists
cpu and memory only, and `Constraints` intersects the quota's tracked resources against it. So a
quota on `requests.ephemeral-storage` is **tracked but never forced**: it cannot reject a pod that
declares none. Upstream's own comment calls the cpu/memory enforcement a frozen mistake and says in
so many words not to extend it. Live positive control on dev-cluster: the docker tier's quota
reports `used.requests.cpu: "3"` and `used.requests.memory: 12Gi`, which reproduce exactly under
`(worker + restartable sidecar) x 2 pods`, while `used.requests.ephemeral-storage` is `"0"` — the
quota controller is demonstrably live and correctly accounting these same pods, and reports zero
for the one resource it cannot enforce.

**The withdrawn bullet said that if (b) held, the same comment would be "wrong there too" for
`seedResources` / `dindResources` / `dindInitResources`. That inference does not follow and acting
on it would break the fleet.** Those three declare **cpu and memory**, which *are* enforced, so the
rule that justifies them holds exactly. (b) invalidates the comment's *scope*, not the constants.
Do not delete or relax them.

**A3.2 — Q2 is SETTLED: REQUEST ONLY. A limit is strictly worse than nothing.** Three cited
mechanisms, any one of which is sufficient:

1. `podEphemeralStorageLimitEviction` fires as soon as **any** container declares a limit, and
   compares the **sum of declared limits** against pod usage that **includes both emptyDirs**. A
   limit on the worker container alone therefore creates a pod-level ceiling the dind cache counts
   against. That is a new **deterministic** eviction path that fires with no node pressure at all —
   the failure this issue is about, arriving on schedule instead of by luck.
2. A limit on the `dind` sidecar is inert at container level: `containerEphemeralStorageLimitEviction`
   builds its map from `pod.Spec.Containers` only, and `dind` is a restartable initContainer. It
   also measures rootfs+logs only, so emptyDir is excluded from container-level enforcement entirely.
3. A request adds **no** eviction path: all three arms of `localStorageEviction` are limit- or
   sizeLimit-gated. It buys placement plus `exceedDiskRequests` ranking, over a `podDiskUsage` that
   **does** include emptyDir.

`preset.go:44-46` already makes precisely this argument for memory ("the requests deliberately sit
ABOVE the measured peak … kubelet evicts pods whose usage exceeds their REQUESTS first"). Reuse it;
do not re-derive it.

**A3.3 — NEW DESIGN CONSTRAINT, from the auditor and in neither the issue nor this brief: put the
request on the `worker` CONTAINER, not on the sidecar.** Quota and the eviction ranker use
different formulas for the same pod. `PodRequests` (quota + scheduler) sums containers **plus
restartable initContainers**; `GetResourceRequestQuantity`, which `exceedDiskRequests` uses, is
`max(sum(Containers), max(InitContainers))` with **no sidecar special case**. So budget placed on
`dind` is charged in full by the quota and credited only partially by the ranker — worked example,
worker 1Gi + dind 8Gi charges 9Gi and credits 8Gi. The worker container counts in both.

**A3.4 — Q4 is SETTLED and it inverts the issue's own framing. `100060Ki` is the WRONG number.**
`evictionMessage` computes container **rootfs + logs only** and iterates `Spec.Containers` only, so
it structurally cannot see an emptyDir and can never name the sidecar. 97.7 MiB is the smallest of
the consumers. There are **TWO** unbounded emptyDirs, and the brief named one:

- `dind-data` (`render.go:793`) — the daemon's image and build cache. `values.yaml:508-509` sizes
  it at "~5-20 GiB".
- `run-workdir` (`render.go:796`, `/data/runner`) — **the run's entire working tree**, per
  `specs/ai.md:8728-8729`. Checkout plus `node_modules` plus build output. Unmentioned anywhere in
  the issue, and it is the volume whose growth is proportional to the work being protected.

Neither declares a `sizeLimit`. **`emptyDir.sizeLimit` is REJECTED as an instrument**: kubelet
enforces it by evicting the pod, so it is a fourth eviction trigger with A3.2's hazard, and a
sizeLimit on `run-workdir` would evict the pod for the run's own checkout growing.

**A3.5 — Q5: measured, and there is a CAPACITY CEILING nobody had stated.** Instrument, read-only
and repeatable: `kubectl get --raw /api/v1/nodes/<node>/proxy/stats/summary`, per-pod
`ephemeral-storage.usedBytes` with per-volume breakdown. No `exec` needed, no fix needed.

```
node ephemeral-storage allocatable  17.55 GiB   (identical on all 4 worker nodes)
whole worker pool                   70.21 GiB
nodes already ~half full before uzi: 7.8-10.2 GiB used, 8.3-10.7 GiB free
working worker  pod eph = 509.68 MiB  <- dind-data 506.16 MiB (99.3%), run-workdir 3.24 MiB,
                                         worker container rootfs 0.04 MiB
idle worker     pod eph =   0.99 MiB
```

Three consequences, all binding on the coder:

- **20 GiB — the top of the chart's own documented dind range — is 1.14x a whole node's
  allocatable.** It cannot be declared as a request at all; the pod would never schedule. Even
  5 GiB fits at most one docker worker per node from today's free space.
- **The 200Gi quota is inert and stays inert after the fix.** `deployments: 10` x node allocatable
  caps the placeable total at 175.53 GiB < 200 GiB, so no per-pod value the scheduler can satisfy
  makes it bind. Decorative across its whole reachable input space, not merely mis-set.
- **Any per-worker request above ~2.3 GiB makes a full fleet (10 docker + 20 restricted)
  unschedulable on this node pool**, and `render.go:846-855` feeds `spec.Size` to **both** tiers, so
  a preset-level field reserves node disk for restricted workers too. The failure presents as
  CLAUDE.md's known shape: a worker that provisions and never appears. The docker-tier quota cannot
  warn you, because it cannot bind.

**So state it plainly in the shipped comment: a request in the 1-4 GiB band is a RANKING mitigation,
not a CAPACITY guarantee.** The capacity answer is bigger node disks or moving the dind data root to
a PVC — which `render.go:790-792` already names as the alternative ("a PVC is the durable/size
alternative, arch §Q4"). That is a separate issue; say so rather than implying this one covers it.

**A3.6 — Q6: do NOT add a LimitRange `max`.** `maxConstraint` in the limitranger admission plugin
rejects any pod that declares no **limit** for a resource the `max` names — every pod in the
namespace, not just workers. With Q2 landing request-only, adding an ephemeral `max` breaks
everything in that namespace. If a `max` is ever wanted it needs a `default` in the same values
block, and then A3.2's trap applies to every pod there by default. The existing comments'
invariant ("`max` must stay >= the largest preset's limits") does **not** acquire a third dimension
under a request-only design; say so in the brief rather than leaving Q6 open.

**A3.7 — Q3: the goldens claim is CONFIRMED, with one silent-staleness trap.**
`hosted_sizes.json` is `{"sizes":["s","m","l"]}` and carries no quantities, so a new `Size` field
moves neither `preset_contract_test.go` nor `size_display_golden_test.go`. **That green is the
problem.** `size_display_golden_test.go:55-58` claims its golden "describes the pod spec
**completely** rather than the half that happens to be displayed today", while
`buildSizeDisplayGolden` copies **six named fields explicitly**. Add a field to `Size` and not to
`sizeSpecEntry` and that sentence becomes false with nothing red. Either extend `sizeSpecEntry`
(and regenerate, and follow the chain into `web/src/lib/workerSizes.ts`) or amend the comment.
Same shape, unpinned by a new pair: `TestEveryPresetIsBurstable` and
`TestSizeDisplayGoldenCarriesParseableQuantities` both iterate hand-written cpu/memory field lists.

**A3.8 — DOC CORRECTIONS, mandatory in the same commit as the work** (CLAUDE.md fix-the-doc). The
false sentence — "a quota on `requests.*` makes admission reject any pod with a container declaring
none" — is at **six** sites. The `initContainers included` half is TRUE; the `requests.*` half is
false and is cpu+memory only, forever, by upstream's explicit design:

| Site |
|---|
| `deploy/chart/templates/worker-docker-namespace.yaml:68` |
| `deploy/chart/templates/worker-namespace.yaml:67` |
| `deploy/chart/values.yaml:384` |
| `controller/internal/kube/render.go:216` |
| `controller/internal/kube/render.go:283` |
| `controller/internal/kube/render_test.go:165` |

Two more, unrelated to that sentence and both verified by the lead on this tree:

- `values.yaml:516-517`, repeated at `worker-docker-namespace.yaml:88-89`: *"emptyDir dind data
  roots land on node ephemeral storage; bound it so a runaway image pull cannot fill a node."*
  **False in both clauses** — quota never observes emptyDir usage, and with nothing declared it
  observes nothing at all. A runaway image pull is exactly what happened.
- `render.go:289` names **`presetRequestsDominateTheSeed`**, which does not exist anywhere in the
  tree. The property is pinned by `TestSeedContainerRequestsStayUnderEveryPreset`
  (`render_test.go:259`). A present-tense claim naming a symbol that is not there is a wrong doc,
  not a typo — and §3 item 3 edits exactly that constant, so a coder reads this sentence as the
  safety argument before touching it.

**A3.9 — §5's verification bar is insufficient and gains three lines.** A rendered-pod parse proves
the requests are *present*; it cannot prove the pod is *admissible* or *schedulable*, and every trap
above lives past that point.

1. **A unit test asserting `sum(container ephemeral requests) <= node allocatable`** against a
   configured node-size value, so an unschedulable preset reddens at `task gate:controller` rather
   than as a worker that provisions and never appears.
2. **`kubectl apply --dry-run=server`** of a rendered worker Deployment into a KinD namespace
   carrying the real quota + LimitRange. This is the only check that discriminates admissibility.
3. **Extend the four tests that would not notice a missing ephemeral request**:
   `TestRenderedResourcesComeFromThePreset` (`render_test.go:277`),
   `TestSeedContainerRequestsStayUnderEveryPreset` (`:259`),
   `TestDindResourceRequestsAndLimitsDefaultAndOverride` (`:964`),
   `TestEveryPresetIsBurstable` (`preset_contract_test.go:204`).

**A3.10 — coupling to record, not to fix here.** All three `localStorageEviction` arms call
`evictPod` with `gracePeriodOverride: 0`. So if a limit is ever declared (against A3.2), #218's
SIGTERM fetch-back gets **zero** grace — the fix would disarm the mitigation the issue's `Related`
section leans on. Not a defect in current code; a coupling nothing else records.

**A3.11 — CLI: checked and negative.** `git grep` over `api/cmd/uzi/` finds no hosted-worker
size/spec display surface, so CLAUDE.md's "does the CLI need a matching change" convention comes
back negative. Recorded so nobody re-derives it.

**A3.12 — LEAD ERROR, recorded because it cost verification effort.** The dispatch told all four
agents "no writer is live in this worktree", and then the lead committed A1/A2 into that same
worktree, moving HEAD from `9cccfa99` to `8778add2` mid-run. The auditor caught it and had already
protected itself with `git worktree add --detach`; nothing it cited was touched. The claim was
false when made — a lead editing the brief IS a writer. Under the standing rule the lead may edit a
shared file without taking the writer token **only if it pre-warns in the same breath**, and no
warning was sent. Every future dispatch on this branch states the live-writer status truthfully.

### A4 — 2026-08-04, DESIGN WAVE: architect. The design, and a BLOCKING sequencing problem.

**A4.1 — 🔴 BLOCKING, and nobody else found it: SHIPPING THIS FIX TRIGGERS THE EXACT DATA LOSS
#224 IS ABOUT, FLEET-WIDE AND SIMULTANEOUSLY.**

`render.go:1099-1117` computes `AnnotationSpecHash` as a sha256 over the **whole rendered
`PodTemplateSpec`**; `materializer.go:536-543` patches the Deployment whenever that hash moves;
`render.go:594` sets `Strategy: Recreate`. Adding a resource key to the worker container therefore
changes the hash for **every worker, plain and docker**. The moment the new controller image rolls
out, every worker pod is killed and replaced, which is precisely §1's chain: `Service.Register` →
`RequeueWorkerRuns` → re-claim → `createOrAttachRunnerClone` → `runnerCloneForBranch`'s
unconditional `fs.rm`. **Every in-flight run loses its tree at once.**

The team has engineered around this property before and left the evidence at `render.go:690-692`:
*"Built as slices so the NON-docker render stays byte-identical to #58 — same spec hash, so enabling
docker in the product never rolls an existing plain worker."* **There is no engineering around it
here — the change IS a pod-spec change.** So it must be sequenced, and this reverses the tidy split
in §3: the brief puts #218's fetch-back out of scope while shipping the thing that fires the loss.

Two ways out, and it is a user call (see the decision recorded in A5):

1. **Land #218 M1's shutdown-path fetch-back first**, so the SIGTERM handler
   (`agent/src/main.ts:202`, 30s grace) pushes the tracking ref before the pod dies. `0111f01c` —
   this branch's own branch point — is the commit that amended that requirement into #218.
2. **Gate the rollout on zero in-flight runs** and say so in the release note.

Doing neither is not an option the PRD may leave open.

**A4.2 — Q3 is ANSWERED, and the answer is NEITHER option the brief offered.** The axis is
`docker` vs non-docker, not `s`/`m`/`l`. `preset.Size` is **unchanged**: no new field, so neither
golden moves and A3.7's silent-staleness trap is not armed. Three arguments, in weight order:

1. **The footprint tracks the TIER, not the preset.** 99.3% of it is the dind emptyDirs, which
   exist only when `w.Docker` and whose size is a function of the user's repo images, not of the
   worker's CPU/RAM. Two live docker workers the same day measured **1.0 MiB and 509.7 MiB — 500x
   apart at the same size.** An `s`/`m`/`l` split cannot express that; a docker/plain split can.
2. **`nixSize`'s own argument applies verbatim** (`preset.go:128-146`, pinned by
   `TestNixSizeIsFlatAcrossEveryPresetAndTemplate`: *"a per-size nix value creeping back in is a
   table with one repeated number"*).
3. **The right value is a property of the CLUSTER'S NODES, not of the product** — which is exactly
   the argument `render.go:220-225` already makes for making `dindResources` chart-overridable.

**A4.3 — THE DESIGN THE CODER BUILDS.** One unit: `controller/` + `deploy/chart/`. No `preset`
change, no api change, no web change.

Two flat defaults in `render.go` beside `dindDefault*` (`:226-231`), each with its reason beside it:

| constant | value | scope |
|---|---|---|
| `workerDefaultEphemeralRequest` | `1Gi` | every worker |
| `workerDefaultDockerEphemeralRequest` | `6Gi` | docker workers — **replaces**, does not add to, the above |

Two `RenderConfig` fields and two env vars mirroring the `DinD*` precedent exactly:
`WorkerEphemeralRequest` / `WorkerDockerEphemeralRequest` ← `UZI_WORKER_EPHEMERAL_REQUEST` /
`UZI_WORKER_DOCKER_EPHEMERAL_REQUEST`, validated as `resource.ParseQuantity` at boot through
`config.go:317-335`'s existing `quantities` slice, empty falling back to the constant via the same
`pick` idiom as `dindResources` (`:237-242`). Chart surface, `{{- with }}`-guarded like the dind
block: `workers.ephemeralRequest: 1Gi` and `workers.docker.ephemeralRequest: 6Gi`. **This is what
satisfies A2.**

| container | `requests.ephemeral-storage` | `limits.ephemeral-storage` |
|---|---|---|
| `worker` (`.spec.containers[0]`) | **YES — the whole pod budget** | **NO** |
| `dind` (native sidecar) | no | **NO** |
| `dind-init` (init) | no | **NO** |
| `seed-nix` (init) | no | **NO** |

**The whole budget goes on `worker` alone, and the comment must say why** — the architect and the
auditor derived this independently (A3.3). `.spec.containers` has exactly one entry, so putting the
number there makes the ranker's view, the scheduler's view and the quota's view all equal to that
one number with no double count. **Without the comment the next reader "fixes" it by splitting the
number across containers, which silently halves the ranking threshold.** Likewise write the
no-limit prohibition and its reason beside the constants: "add a limit for symmetry with cpu/memory"
is what the next person will do, and A3.2 is why they must not.

**What does NOT change:** `workers.quota.*` and `workers.docker.quota.*` (the 200Gi key stays);
both `limitRange.max` and `limitRange.default` (A3.6); `preset.Size`; both goldens; the api; the web.

**A4.4 — sizing, stated as constraints so Q5 can move the value without redoing the design.**

- **Hard ceiling ~16 GiB.** Above node allocatable (17.55 GiB) minus the 15% threshold, the pod is
  **permanently unschedulable, silently** — the worker-that-never-appears shape.
- **Practical ceiling ~6-8 GiB.** Live headroom to threshold measured 7.8 GiB and 5.3 GiB on the two
  nodes. Two 6Gi docker workers will not co-schedule on one node, which is arguably correct.
- **Floor: above the observed steady state** — 506 MiB at 10h of light work, 1 MiB idle.
- **`values.yaml:509`'s own "~5-20 GiB" dind budget has an upper half that does not fit on a
  dev-cluster node at all.** That is a fact about the cluster the chart does not know.
- Ship **6Gi**, revise on Q5. **If Q5's p95 exceeds ~8 GiB the answer is not a bigger request** — it
  is the PVC alternative in A4.6.

**A4.5 — `emptyDir.sizeLimit` on `dind-data`: RIGHT INSTRUMENT, DEFERRED, NOT DROPPED.** It is
better than a container limit on three counts (attributable — the message names the volume rather
than the wrong container; targeted; and it bounds one tenant's runaway from evicting others). But it
is still an eviction, on a number nobody has measured, inside a change whose purpose is to stop
evictions. **`run-workdir` must never get one** — that volume holds the checkout we are trying not
to lose. Land it as a second commit once Q5 has a distribution.

**A4.6 — what we would regret in six months, filed rather than built.** Every instrument at this
layer — request, container limit, pod limit, `sizeLimit`, quota — is either a ranking hint or an
eviction trigger. **None produces backpressure.** `render.go:790` already names the alternative:
*"emptyDir in v1 (a PVC is the durable/size alternative, arch §Q4)"*. Moving the daemon data root to
a PVC removes it from ephemeral accounting **entirely** and changes the failure from *pod eviction*
(destroys the tree) to *ENOSPC in the daemon* (the docker step fails, the tree survives, the user
sees an error) — strictly the better failure for this issue's actual complaint. Quota headroom
exists (4/20 PVCs, 64Gi/140Gi). Not in this change; **file it, and do not foreclose it.**

**A4.7 — the KinD smoke covers NONE of this, and §5 must say so.** `e2e:kind-smoke` runs
`scripts/smoke.sh` against a `helm install`ed chart with `workers.enabled: false` by default. It
never renders a worker pod, never exercises the renderer, and cannot observe an eviction. It proves
the chart still installs. A green pipeline must not be read as coverage here.

**A4.8 — verification, replacing §5's list.** (1) `task gate:controller` green. (2) `render_test.go`
gains: plain worker → `1Gi` on `worker`; docker worker → `6Gi`; and **the invariant test — no
container in `.spec.containers` OR `.spec.initContainers` declares `limits.ephemeral-storage`**,
which is the one that must exist because it is what a future "for symmetry" edit breaks. (3) A
config test: a bad quantity fails at boot naming the var. (4) `helm template` +
`scripts/assert-chart-render.sh`, then **parse** the rendered controller Deployment for the two env
vars. (5) A3.9's admissibility dry-run and node-allocatable assertion still stand.

**A4.9 — ADR: NO, with one defensible counter.** The durable facts here are k8s semantics, not a uzi
seam, and the uzi-side decision belongs beside `nixSize`'s and `dindResources`' existing arguments
in `render.go`. The five-ADR set is deliberately small. **The counter, recorded because it is
genuinely arguable:** *"never declare an `ephemeral-storage` limit, and never add an ephemeral key
to the LimitRange `max`"* is exactly an invariant a future change would silently break, and it spans
two components. If that rule should outlive the PRD, `adr/0224-*` is justified **on that rule
alone**, not on the sizing. Lead's call: comment + decision log, and A4.8's invariant test is what
actually enforces it.

**A4.10 — adjacent, not ours, but somebody should file it.** The live `l` worker's `/nix` PVC is at
**2.97 GiB of 3.86 GiB (77%)** on a **4Gi** volume. That worker predates PRD #87's bump of `nixSize`
to 20Gi and **there is no resize path**. Separate issue.

**A4.11 — where the architect says it could be wrong**, recorded rather than smoothed over: the
`imageContainerSplitFs` branch selection (`helpers.go:1159` vs `:1171`) was inferred from the node
reporting distinct `fs` and `runtime.imageFs` capacities, not from reading this kubelet's config. It
does not change the recommendation — both branches include `fsStatsLocalVolumeSource`, so emptyDirs
dominate either way — but it changes whether the writable layer is in the ranking.
**Resolved by the fact-checker — see A5.2. The architect's inference was right for the node it
sampled and wrong as a generalisation.**

### A5 — 2026-08-04, DESIGN WAVE: fact-checker. One refuted premise that the other three shared.

The fact-checker read at `9cccfa99`/`8778add2` and had **not** seen A3 or A4 when it filed, so
everything below is independent rather than agreement. It confirms A3.1 (Q1 = (b)), A3.2/A4.3 (the
request goes on `worker`; the ranking formula discards sidecars), and A3.4 (the emptyDirs are the
footprint, measured at **99.3%** on the same live pod). Those are now **three** independent
derivations each, from three instruments.

**A5.1 — 🔴 REFUTED, and it was load-bearing for A4.1, for issue #224's `## Related`, and for
#218 M1: THERE IS NO 30-SECOND GRACE ON THE EVICTION PATH. IT IS TWO SECONDS.**

Every eviction on this cluster: `eviction_manager.go:408-411` sets `gracePeriodOverride := int64(0)`
and raises it to `MaxPodGracePeriodSeconds` **only for a soft threshold**. dev-cluster configures
`evictionHard` only — `evictionSoft: null`, read from `/api/v1/nodes/<n>/proxy/configz`. `killPodNow`
takes the override verbatim and `killContainer` clamps it to `minimumGracePeriodInSeconds`, **which
is 2** (`kuberuntime_manager.go:78`). The three `localStorageEviction` arms pass `0` as well.

So the pod spec's `terminationGracePeriodSeconds: 30` **is not what a worker gets when it is
evicted**, and the fact-checker's conclusion is blunt: *"a `git fetch` in a SIGTERM handler does not
fit in 2s."* Issue #224's `## Related` paragraph and #218's M1 both reason from the 30 seconds.

**LEAD INFERENCE, flagged as inference and NOT yet verified by anyone — it decides A4.1 and it must
be checked before the sequencing decision is acted on.** The 2-second clamp is documented above for
the *eviction* path. A controller-driven **rollout** (the spec-hash change of A4.1, `Strategy:
Recreate`) is an ordinary pod deletion, which should honour the pod's own
`terminationGracePeriodSeconds: 30`. If that holds, then:

- A4.1's option 1 (land #218's fetch-back first) **does** protect against the fleet roll that
  #224's own fix causes — the case with 30 seconds;
- and it does **not** protect against the original node-pressure eviction — the case with 2.

That would make #218's fetch-back a necessary precondition for shipping #224 safely while still
leaving #224's headline failure only mitigated, not fixed. **Do not treat this paragraph as
settled.** It is the lead reasoning about a path nobody measured, in a document whose whole point is
that such reasoning is how wrong premises enter the record.

**A5.2 — the four worker nodes are NOT homogeneous, and the 2026-08-03 eviction happened on the odd
one.** Resolves A4.11's self-flagged uncertainty:

```
node    nodefs cap       imagefs cap      dedicated imagefs?
5shb4   20,941,697,024   20,941,697,024   NO    <- the 2026-08-03 eviction happened here
7fvcq   20,941,697,024   52,520,497,152   yes   <- the 2026-07-27 eviction happened here
kxpgk   20,941,697,024   52,520,497,152   yes
m6chc   20,941,697,024   52,520,497,152   yes
```

Positive control on the reading: the issue's own `Threshold quantity: 3141254678` is **exactly 15%
of 20,941,697,024**, matching `evictionHard.imagefs.available: 15%` on a node where imagefs ==
nodefs. Consequence: a **container-level** ephemeral limit would enforce against **logs alone** on
three nodes and logs+rootfs on the fourth, with no signal to the operator — one more reason A3.2's
no-limit rule is right. The **pod-level** path is the fs-layout-independent one.

**A5.3 — the eviction message has two blind spots, so `100060Ki` is "a real number about the wrong
thing".** `evictionMessage` iterates `pod.Spec.Containers` **only**, and `dind` is a restartable
init container — so **`dind` is structurally incapable of appearing in that message**. "Container
worker was using 100060Ki" is not evidence that `worker` was the largest consumer; it is evidence
that `worker` is the only container eligible to be named. And the per-container figure is
`Rootfs + Logs`, which is not what the ranking used and on three of four nodes lives on a different
filesystem from the one that ran out.

`request is 0` is **strictly ambiguous** — it renders identically for "no request declared" and for
an explicit `0`. The live pod spec disambiguates (`worker.resources.requests` = cpu + memory, no
ephemeral key), so the brief's reading is correct **for this instance, by spec inspection rather
than by the string**.

**A5.4 — `emptyDir.sizeLimit` EVICTS; it does NOT block writes.** For a default-medium emptyDir,
enforcement is a periodic comparison that evicts the pod at grace 0. **The fact-checker flags that
an LLM summary told it the opposite** — that sizeLimit prevents writes and returns "no space left
on device" — which is true only for `medium: Memory` and false here. If that claim appears in any
design message on this branch, it is wrong. This strengthens A4.5's deferral rather than changing
it. Note it contributes nothing to scheduling, ranking or quota, so **it is not a substitute for the
request**: #224's failure is a ranking failure and `sizeLimit` does not touch ranking.

**A5.5 — image layers: the issue's sentence is TRUE and the tempting stronger reading is FALSE.**
Image layers do occupy node ephemeral storage and can cause the pressure, but they are **never
attributed to a pod** — `fsStatsImages` is not consumed by `containerUsage` or `podDiskUsage`. So
images cause the eviction and can never rank you for it.

**A5.6 — A1 spot-checks, both material to the cleanup.** The `--terminated-pod-gc-threshold` default
is **12500** and dev-cluster's kube-controller-manager does not set it, which is why the pods
linger — A1's stated reason is CONFIRMED. But **the count is wrong**: issue #224 and A1 both say
three lingering `Failed`/`Evicted` pods; as of 2026-08-04 there are **two**
(`…-7b45c774f9-rmqvx`, 2026-07-27 and `…-ccb67bb74-zzb7q`, 2026-08-03). **Whoever does the cleanup
re-enumerates rather than deleting from the issue's list** — which is also the CLAUDE.md
exact-names rule arriving from a second direction.

**A5.7 — what the fact-checker could NOT see, recorded rather than smoothed over.** It ran no
`helm template`, did not read the ArgoCD values repo, and read no Grafana history, so its live
quota/LimitRange reads are what the cluster holds now, not proof of what the chart renders (the
auditor covers that half). It did not check whether non-uzi workloads on those nodes declare
ephemeral requests, so A3.5's headroom is a node ceiling rather than a free remainder. **And every
source citation is upstream `release-1.29` while this is a vendor/platform FIPS build
(`v1.29.4+vendor.3-fips.1`) whose patch set it did not diff** — a vendor deviation in the eviction
manager would have been invisible to it. The live message rendering byte-matches the upstream format
string, which is a positive control on that one path and not on the others.

### A6 — 2026-08-04, USER DECISIONS: ship-and-accept, and all four follow-ups land HERE

**A6.1 — sequencing: SHIP IT AND ACCEPT ONE LOSS EVENT.** A4.1's fleet roll is accepted rather than
mitigated. #218 M1 is **not** a precondition, and the rollout is **not** gated on a drained fleet.
The user was shown all four options with the loss stated plainly and chose this one.

Two obligations follow, and they are not optional:

1. **The release note and the CHANGELOG must say that rolling this kills every in-flight run once.**
   Silence here converts an accepted, understood cost into an incident.
2. **A5.1's open question is now MOOT for the sequencing decision.** Whether the rollout path keeps
   its 30s grace only mattered for choosing between options; nothing now depends on it. The
   fact-checker's answer is still worth having as a durable fact about this cluster — it corrects
   issue #224's `## Related` and #218 M1 either way — but it no longer blocks anything.

**A6.2 — all four follow-ups are IN SCOPE for this session, not filed.** The user's words: *"cant we
fix them all now, in this session?"* This makes #224 substantially larger than its `effort::small`
label, deliberately and with the cost stated. **One argument actively favours it: every item here is
a pod-spec change, so doing them together costs ONE fleet roll instead of three** — which under A6.1
is exactly the cost being accepted.

**A6.3 — 🔴 (a) AND (c) WERE CONTRADICTORY AND THE LEAD RESOLVED IT IN FAVOUR OF (a).** They are
alternatives, not complements. Moving the daemon data root to a PVC removes it from ephemeral
accounting **entirely**, so there is no `emptyDir` left for an `emptyDir.sizeLimit` to bound.
Selecting both is not buildable.

- **(a) SHIPS.** It was the architect's own preference (A4.6), it is the only instrument in play that
  produces **backpressure** rather than an eviction, and it changes the failure mode from *pod
  eviction destroys the tree* to *ENOSPC in the daemon, the docker step fails, the tree survives*.
- **(c) is MOOT BY CONSTRUCTION, not done.** Recorded as such so nobody later reads it as delivered.
  `run-workdir` becomes the only remaining emptyDir and **must never** get a `sizeLimit` — it holds
  the checkout this whole issue exists to protect (A4.5).

**A6.4 — (a) RE-OPENS THE SIZING THE WAVE JUST CLOSED, in the easy direction.** A4.4's 6Gi and its
~2.3 GiB fleet ceiling were both derived from a footprint that is **99.3% `dind-data`**. Move that
to a PVC and the residual ephemeral footprint is the measured **~4 MiB** (run-workdir 3.2 MiB +
both containers' rootfs+logs ~0.3 MiB). So:

- the docker/plain split in A4.2 may collapse — if the residual is a few MiB either way, one flat
  constant may serve both tiers;
- **A3.5's fleet-unschedulability ceiling dissolves**, because a sub-GiB request cannot approach
  17.55 GiB even at 30 workers;
- **the 200Gi quota stops being decorative for a different reason than A3.5 gave** — the bytes move
  to `requests.storage`, which that quota already tracks at 140Gi with 64Gi used. **The PVC quota
  and `persistentvolumeclaims: "20"` count are now the binding constraints and must be re-derived**:
  a third PVC per docker worker changes both.

**The architect must re-answer A4.3's numbers under this design before the coder starts.** Do not
carry 6Gi forward by inertia; it was correct for a design that is no longer the one being built.

**A6.5 — (b) is genuinely independent and is the one item with no interaction.** The `/nix` PVC at
2.97 GiB of 3.86 GiB (A4.10) is a different volume, a different failure and a different fix.
**Feasibility is unestablished and must be checked before it is committed to**: PVC expansion needs
`allowVolumeExpansion: true` on the StorageClass, and the controller would have to reconcile
`spec.resources.requests.storage` on an existing PVC. If the StorageClass does not allow expansion
there is no in-place path at all and the honest answer is delete-and-reprovision, which is what
`nixSize`'s own comment already says. **Check the StorageClass before designing the fix.**

**A6.6 — (d) is bounded by what a session can measure, and the honest answer is partial.** A p95 over
a fleet-week is not obtainable today. What is obtainable now, read-only: a broader sweep across every
live worker and both node classes, at more than the two time points the wave took. Under A6.4 the
number matters far less anyway. **The follow-up to replace a default with a fleet-measured one
survives this session** and should be filed rather than claimed as done.

**A6.7 — decomposition. `units: 1`, three sequential milestones, ONE writer.** M-a (dind data root →
PVC), M-b (ephemeral requests + the eight doc corrections, numbers re-derived per A6.4), M-c (nix
resize path, conditional on A6.5). This is **not** three parallel units: M-a and M-b both edit the
worker container's resources and the volume list in `render.go`, and M-c edits `renderPVC` and
`preset` alongside them. The file-disjointness test fails, so the parallel-worktree shape does not
apply — one coder, sequential commits, per the validated single-writer-token pipeline.

### A7 — 2026-08-04, architect (crossing A6): the eviction signal was misidentified from the start

Written at `8778add2`, so it had read A1-A3 and **not** A4/A5/A6. It answers A2 and crossed the A6
re-derivation dispatch. Everything here is independent of A6 and survives it.

**A7.1 — 🔴 BOTH EVICTIONS WERE `imagefs.available`, NOT `nodefs.available`, AND THE NODE POOL IS
HETEROGENEOUS. Nobody caught it because the tell is a number everyone read past.** The two evicted
pods report *different* threshold quantities:

```
zzb7q  node 5shb4  "Threshold quantity: 3141254678, available: 2632184Ki"
rmqvx  node 7fvcq  "Threshold quantity: 7878074885, available: 7691084Ki"
```

The kubelets carry exactly one 15% threshold (`imagefs.available`; `nodefs.available` is 10%), and
`float32(0.15)` = 0.15000000596046448 reproduces **both** to the byte against their own node's
imagefs capacity: `20941697024 × f32(0.15) = 3141254678.4`,
`52520497152 × f32(0.15) = 7878074885.8`. **`nodefs.available` never fired.**

```
5shb4   nodefs 20941697024   imagefs 20941697024   MERGED   <- 2026-08-03 eviction
7fvcq   nodefs 20941697024   imagefs 52520497152   split    <- 2026-07-27 eviction
kxpgk   nodefs 20941697024   imagefs 52520497152   split
m6chc   nodefs 20941697024   imagefs 52520497152   split
```

**A5.2 found the same heterogeneity independently; A7 adds that both evictions were the imagefs
signal, and computes both thresholds exactly.** A3.5's "allocatable identical on all 4" stays true
and does not extend to topology — allocatable derives from *nodefs* capacity, while topology selects
the threshold **and the ranking formula**. The message cannot discriminate: `helpers.go:95` and
`:99` map both signals to `v1.ResourceEphemeralStorage`, so the string is identical either way and
the threshold quantity is the only tell.

**A7.2 — what that changes, and the news is good.** On a split node an imagefs eviction ranks on
`{fsStatsRoot, fsStatsImages}` (`helpers.go:1164`), and `containerUsage` has **no** `fsStatsImages`
branch — so images contribute nothing per-pod and emptyDirs are not in the set at all:

| | ranking input for our pod | what the request must beat |
|---|---|---|
| **split** (3 of 4), imagefs eviction | container writable layer only — **268 KiB** measured | anything over ~1 MiB wins outright |
| **merged** (1 of 4) | images + rootfs + logs + emptyDirs (`helpers.go:1185`) | the emptyDir footprint; the value matters |

On three nodes of four the **value barely matters** — a cheaper win than the sizing debate assumed,
and another argument for erring low.

**A7.3 — 🔴 ON SPLIT NODES THE REAL DRIVER IS IMAGE SIZE, WHICH NO EPHEMERAL REQUEST CAN TOUCH.**
`7fvcq` had 7.33 GiB free on a 48.9 GiB imagefs against a **5.77 GB** agent image; one more tag
fills it. `imageGCHighThresholdPercent: 85` reclaims only *unused* images. **A separate defect, not
addressed by #224, and the architect's judgement is that it is the more likely repeat cause on 3 of
4 nodes.** Strong follow-up candidate.

**A7.4 — the architect WITHDRAWS its own `emptyDir.sizeLimit` deferral**, reaching A6.3's
conclusion independently and by a different route: all three `localStorageEviction` arms evict at
grace 0, and with `evictionSoft: null` every threshold here is hard, so node-pressure eviction is
zero-grace too. That is the source of the §1 correction above.

**A7.5 — Q3 FINAL, unchanged by A2, with A2's composition question answered.** Flat constants gated
on `w.Docker`, chart-overridable — it *is* the `dindResources` precedent A2 names. Of three ways a
per-size field could compose with an override: (a) flat-override-wins is rejected because the first
tuning silently flattens the table; (b) per-size chart values is the only shape that genuinely
delivers both, and is rejected because it makes the chart a **fourth** place preset names appear and
**the only one with no golden gating it** — the ungated-skew class `preset.go:11-17` exists to
prevent; (c) a multiplier, rejected on sight. **(d) is the real answer: they are SEQUENCEABLE.**
Flat → per-size later is a pure controller-internal refactor with no api or wire change, so shipping
flat **forecloses nothing** and is strictly dominant under A2's own ordering.

**A7.6 — "CONSERVATIVE" HERE MEANS ERR *LOW*, and this is the sentence the shipped comment most
needs.** The failure modes are asymmetric: too low = today's behaviour, probabilistic, recoverable,
visible in a message we now know how to read. Too high = **unschedulable**, total, deterministic,
**silent**, presenting as `preset.go:14-17`'s worker-that-never-appears — and A3.5 proves the docker
quota cannot warn you, because it cannot bind. "When unsure, request more" is what everyone writes
otherwise.

**A7.7 — the PLACEMENT half of the issue's own claim is FALSE; only the RANKING half lands.**
`kubectl describe node` reports **`ephemeral-storage 0 (0%)`** requested on nodes physically
**51-52% full**. Nothing in this cluster declares the resource, so the scheduler's ephemeral view is
empty on a half-full node and the ~10 GiB of undeclared usage stays invisible no matter what we
declare. This is the mechanism behind A3.5's "ranking mitigation, not capacity guarantee" and
belongs beside it in the shipped comment.

**A7.8 — CORRECTION to §4 Q5's instrument list: `kubelet_volume_stats` CANNOT answer this.**
`volume_stats.go:116-120` skips every volume whose `PVCRef == nil`, so there is **no**
`kubelet_volume_stats_*` series for `dind-data` or `run-workdir`, ever. A Grafana panel built on it
would show `/data` and `/nix` and silently omit the two volumes that matter. The summary API is the
only confirmed instrument; the architect says "not found" rather than "does not exist", having not
searched exhaustively.

**A7.9 — pre-A6 values, kept for the audit trail and SUPERSEDED by the A6 re-derivation.** Under the
emptyDir design the architect revised 6Gi down to **4Gi docker / 256Mi restricted**: the tiers are
asymmetric, so A3.5's uniform ~2.3 GiB squeeze (98.3% of pool) becomes 64.1% with 25 GiB of margin.
Under A6 the data root leaves ephemeral accounting entirely and the numbers are being re-derived.
**The asymmetry argument survives; the numbers do not.**

**A7.10 — the A2 follow-up needs a DISTRIBUTION, and the statistic is named.** Two spot reads 515x
apart is exactly why a spot read cannot set this number. It needs per-pod
`ephemeral-storage.usedBytes` **with the per-volume breakdown**, sampled on a schedule across all
live workers, retaining the max per worker-lifetime, **tagged with the node's imagefs topology**
(per A7.1, two runs otherwise disagree and nobody finds the reason). The docker value is set by the
p95 of `dind-data` at end-of-run; the restricted value is set by container-log growth, bounded by
rotation, and needs no measurement at all.

**A7.11 — A5.1's follow-up: CONFIRMED by the fact-checker. Rollout = 30s, eviction = 2s.** Six cited
links: the controller patches the Deployment and never deletes a pod; `rolloutRecreate` waits for
old pods to be gone (so the worker's window is not raced by its successor — and this is also why
`Recreate` dodges the Multi-Attach deadlock `render.go:592-593` cites); the ReplicaSet controller
deletes with empty `DeleteOptions`; the apiserver fills `period` from
`Spec.TerminationGracePeriodSeconds`; the kubelet treats that as "bedrock truth" and the 2s path
arrives *only* through `PodTerminationGracePeriodSecondsOverride`, which a plain deletion leaves nil;
and `setTerminationGracePeriod`'s first case returns the 30 unmodified (the `minimumGracePeriod`
clamp is a **floor**). No preStop hooks exist, so nothing subtracts from it. **Three riders:**

- **It is a derivation, not a measurement, and one field collapses links 3-6.** During the first
  post-fix roll, while the old pod is `Terminating`:
  `kubectl -n uzi-workers-docker get pod <old> -o jsonpath='{.metadata.deletionGracePeriodSeconds}'`.
  **Add this to the verification bar** rather than trusting the derivation.
- **🔴 A THIRD path exists and is worse than either: `shutdownGracePeriod: "0s"` — graceful node
  shutdown is DISABLED on these kubelets.** On an ungraceful node power-off or machine replacement,
  pods get **no SIGTERM window at all**. `kubectl drain` is unaffected (it goes through the same
  `CheckGracefulDelete`, so 30s). **If the fleet is ever rolled by replacing NODES rather than by
  patching Deployments, none of the 30s reasoning applies** — and this fleet sits on a platform node
  pool, so that is not hypothetical.
- **30s being AVAILABLE is not the fetch-back FITTING in it.** A `git fetch` against
  `gitlab.example.com` from a worker holding an arbitrary repo is unmeasured. Note the asymmetry
  that makes this worth saying: at 2s the answer is obviously no, which is what made A5.1 a clean
  finding; at 30s it is a real, repo-size-dependent question. Relevant to #218, not to A6.1.
- Minor, and it touches A1 rather than A5.1: `CheckGracefulDelete` sets `period = 0` for pods already
  `Failed`/`Succeeded`, so **the lingering Evicted pods delete immediately** — no grace to wait out.

### A8 — 2026-08-04, architect (re-derived under A6) + auditor (storage facts). Design complete bar two user calls.

**A8.1 — 🔴 THE LEAD WAS WRONG IN A6.4, AND THE CORRECTION STRENGTHENS THE DESIGN.** A6.4 said the
post-M-a residual is "the measured ~4 MiB" and speculated the docker/plain split might collapse.
**That 3.2 MiB is a spot read of `run-workdir` on one pod at one instant, and `run-workdir` is not a
residual — it is the run's entire working tree**, which the brief's own A3.4 already says. Bounds,
none of them spot reads: `dindWorkdirDir = /data/runner` (`render.go:203`) is `git.ts`'s
`runnerRoot`, one clone per run; `$TMPDIR` for docker workers points at the **same** volume
(`render.go:681`); and `WORKER_MAX_CONCURRENT_RUNS` **multiplies** it. This repo's checkout alone is
**26 MiB** (1495 tracked files) and its bare-clone cache measured **139.7 MiB** live, so a runner
clone is ~**170 MiB before a single `npm install`**. Realistic range is **0.5-2 GiB per concurrent
run**. The 3.2 MiB was a tree that had barely started.

**The split does not collapse — it survives for a BETTER reason than the one it had.**
`render.go:787` mounts `run-workdir` **only** `if w.Docker`. For a plain worker there is no such
emptyDir, so `/data/runner` falls through to the **`data` PVC** and the working tree is never on
ephemeral storage at all. So the rule is now structural rather than measured:

> **docker = the working tree is on an emptyDir. plain = the working tree is on a PVC.**

Everything else in A6.4 holds: the fleet-unschedulability ceiling dissolves, and the binding
constraint moves to the PVC quotas.

**A8.2 — M-a: dind data root → third per-docker-worker PVC, flat 20Gi, chart-overridable.**
`dindDataVolume`'s source changes from `EmptyDir` (`render.go:793`) to `PersistentVolumeClaim`;
`RenderPVCs` (`:442-448`) gains a third entry **only when `w.Docker`**; `dindDataPVCName(id)` joins
`dataPVCName`/`nixPVCName`; `materializer.go` gains `HasDinDDataPVC` in `observeNamespace`, a
create-gate case and a teardown entry. 20Gi is the top of the chart's own documented band and is now
a **ceiling producing ENOSPC** rather than a reservation producing eviction. `X <= 20Gi` is a hard
admission ceiling, not a budget choice — the LimitRange PVC `max` is 20Gi.

**Rejected alternative, named because it looks cheaper:** the daemon root on the existing `data` PVC
via `subPath: dind`. `subPath` does confine the container, so Decision 3 survives — but a runaway
pull would then ENOSPC the **clone cache** too, which is exactly the coupling the PVC is being paid
to remove, and absorbing the cache means raising `Size.DataSize`, which moves the display golden and
`workerSizes.ts` (A3.7's chain) and wastes the increase on every restricted worker with no daemon.

**A8.3 — M-b: values re-derived. 6Gi is RETIRED and must not be carried forward.**

| constant | value | basis |
|---|---|---|
| `workerDefaultEphemeralRequest` | **512Mi** | plain: working tree is on the PVC, so ephemeral is logs + writable layer. Measured 45 KiB; kubelet default log-rotation ceiling 10Mi x 5 per container. ~10x that ceiling. |
| `workerDefaultDockerEphemeralRequest` | **4Gi** | docker: `run-workdir` = working tree x `WORKER_MAX_CONCURRENT_RUNS`, ~170 MiB per clone before deps, plus `node_modules`, plus `$TMPDIR`. Covers ~2 concurrent heavy runs. |

Fleet check: `10 x 4Gi + 20 x 512Mi = 50 GiB` of 70.21 GiB = **71%**, 4 docker workers per node
against a tier cap of 10. **No unschedulability at any fleet size the quotas permit.** A7.6's "err
low" still applies but no longer binds, which is why plain is 512Mi rather than 1Gi. Everything in
A4.3 stands: request on the `worker` container only, **no `limits.ephemeral-storage` anywhere**,
`preset.Size` untouched.

**A8.4 — 🔴 THE DOCKER TIER'S QUOTA TRIPLE HAS NEVER BEEN SELF-CONSISTENT, AND M-a FORCES THE
CONVERSATION.** Independently derived by both agents. Live, `2026-08-04T05:25:46Z`:

```
hard: {deployments 10, persistentvolumeclaims 20, requests.storage 140Gi}
used: {deployments  2, persistentvolumeclaims  4, requests.storage  64Gi}   <- 2 x `l`
```

`requests.storage` **binds today**, at ~3 `l` workers, while `deployments` advertises 10. The tier
has room for **exactly one more worker right now**. After M-a, at `X >= 10Gi` with `l`, that
headroom is gone and **the tier is full at its current occupancy** — the next worker is refused with
a quota error, not a scheduling one, and **no test in this repo can see it.**

Auditor's table, D docker workers at three PVCs each (`requests.storage` binds in every cell; the
PVC-count cap is D<=6 and deployments D<=10, never reached):

| X (Gi) | `s` | `m` | `l` |
|---|---|---|---|
| 0 *(today)* | 5 | 4 | **3** |
| 10 | 4 | 3 | **2** |
| 20 | 3 | 2 | **2** |

Restricted tier, untouched by M-a but carrying the same latent inconsistency: `requests.storage`
caps it at **7-11** against a tier designed for 20.

**A8.5 — MEDIUM, pre-existing, and it is the CAUSE of A8.4: `values.yaml:380-382`'s sizing comment
is FALSE.** It reads *"20 x (10Gi data + 4Gi nix) = 280Gi"*. That arithmetic is correct for
**`nix = 4Gi`, the PRE-#87 value**. `nixSize` is now **20Gi** (`preset.go:146`). True requirement for
the comment's own stated fleet is 20 x (10+20) = **600Gi**; at 280Gi the tier holds **9**. The quota
is **2.14x too small for the fleet its own comment says it was sized for.** PRD #87 bumped `nixSize`
and did not follow the value into the quota comment. Fix-the-doc owed; M-b's doc sweep is the home.

**A8.6 — M-c: THE TWO AGENTS DISAGREE, AND THE AUDITOR'S EVIDENCE UNDERCUTS THE PREMISE.**

*Auditor, measured across the entire fleet (n=2, which is every worker that exists):*

| worker | nix PVC | used | fill |
|---|---|---|---|
| `0b83f044` | **19.52 GiB** (current 20Gi default) | 2.77 GiB | **14.2%** |
| `8e1fef71` | **3.86 GiB** (legacy 4Gi) | 2.77 GiB | **71.7%** |

**Both store exactly the same 2.77 GiB.** Only the denominator differs. So A4.10's 77% is **not**
evidence the current default is too small — it is evidence that **one worker predates PRD #87's
bump**, and newly-provisioned workers sit at 14.2% with 15.7 GiB free. **n=1 affected worker**, and
`nixSize`'s own comment already prescribes the remedy: *"v1's remedy is delete + reprovision"*.

*And the expansion path has ZERO headroom:* `allowVolumeExpansion: true` on `storage-class` (resizer runs
`--handle-volume-inuse-error=false`, so online-capable), **but the LimitRange PVC `max` is 20Gi and
`nixSize` is already 20Gi.** The limitranger validates PVC **updates** (only *Pod* updates are
exempt, `admission.go:427-429`), so a patch beyond 20Gi is rejected at admission. M-c can take the
legacy 4Gi to 20Gi and no further without also raising `maxPVCStorage`.

*Architect, if M-c is built:* make it **generic, not nix-specific** — after the create-gate, for each
rendered PVC, if `desired > observed`, patch `spec.resources.requests.storage`; **never** when
`desired <= observed` (k8s forbids shrinking, so a preset going down must be a no-op, not an error).
Never block reconcile on completion. **Two things it must carry:** it overturns a stated decision
(`materializer.go:489-491`, *"never patched: PVC specs are near-immutable, so a size change is
delete + reprovision"*), which must change in the same commit; and it sits beside issue #114 BUG 3 —
an unconditional PVC create charged `used.requests.storage` at admission and k8s does **not**
decrement on `AlreadyExists` (upstream #119593), pinning the quota at its limit. **A patch charges
the delta the same way**, so the gate must be on an *observed size*, which means `observeNamespace`
must record sizes and not just today's booleans. Under either online or offline CSI the design is
identical — patch, don't wait; offline resizes land on the roll M-a and M-b already pay for.

**Lead's read: the auditor's premise-level evidence is the stronger claim and it arrived after the
architect's design.** Going to the user (A9).

**A8.7 — two costs M-a adds, both agreed by both agents, neither a redesign.**

1. **A third RWO PVC is a third Multi-Attach surface, on the exact code path #224 is about.** Issue
   #224's own Cleanup paragraph records the 19:04 replacement hitting `Multi-Attach error` on
   **both** existing PVCs; this makes it three. `Strategy: Recreate` means no surge pod, so the
   window is the old node's `VolumeAttachment` releasing — unchanged in kind, wider by one volume.
   **A6.3 did not weigh this when it chose the PVC over the sizeLimit.** It does not reverse the
   choice (backpressure still beats eviction), but it belongs beside the benefit. Release note.
2. **The image cache becomes PERSISTENT and has no GC.** Today the emptyDir dies with the pod, which
   is a crude but real bound; on a PVC it only grows. This is the identical residual `nixSize`'s
   comment documents for `/nix` (`preset.go:143-145`) and the identical failure A4.10 found live.
   **Write it beside the constant, in `nixSize`'s own words**, and note `docker system prune` as the
   remedy an agent could run. It is the honest cost of trading eviction for backpressure.

**A8.8 — `fsGroup` is the most likely thing to break M-a in practice and NO unit test reaches it.**
The rootless daemon runs as uid 1000 (`render.go:206-213`); the pod's `fsGroup` is 10001. An emptyDir
mount root is created writable by fsGroup, and a freshly-provisioned PVC should get the same
treatment (`fsGroupPolicy: ReadWriteOnceWithFSType`, confirmed live on this CSI driver) — **but it is
invisible to `helm template` and to every unit test, and only a real provision shows it.** First
thing to check on the dev-cluster run.

**A8.9 — the `data` PVC is the largest quota term and is measurably near-empty.** `0b83f044` 0.0%,
`8e1fef71` 0.7%, both on 19.52 GiB. `l`'s `DataSize` of 20Gi is the single biggest term in both
tiers' arithmetic. If A8.4's headroom is what M-a runs into, the cheapest release valve may be
re-examining `l`'s `/data` rather than raising `requestsStorage`. **Not proposed as a change** — two
near-idle workers at one instant is not a basis for resizing a preset. Filed as the measurement A6.6's
follow-up should take, since the same sweep answers both.

**A8.10 — sequencing confirmed, with a stronger reason than file-disjointness, and one condition.**
M-a **must** precede M-b because M-b's values are *derived from* M-a having moved `dind-data` off
ephemeral: shipping M-b first ships 6Gi, and shipping both in one commit makes the 4Gi
unjustifiable from the diff alone. M-c last, so its resizes ride the roll the first two already pay
for. **"One fleet roll instead of three" holds ONLY if all three land in one release** — three
controller rollouts would be three rolls and three loss events. That was the argument that justified
the enlarged scope, so it belongs in the release note.

**A8.11 — A5.4 does NOT touch M-a, and this is on the record so nobody mis-applies it.** A5.4
refutes "`emptyDir.sizeLimit` blocks writes with ENOSPC" — correct, it evicts. **A full PVC is a
different mechanism**: the filesystem is genuinely out of space, the writer gets `ENOSPC`, no
eviction occurs. M-a's backpressure claim rests on the PVC and never on `sizeLimit`.

**A8.12 — verification additions beyond A4.8.** M-a: a render test that a **plain** worker gets no
`dind-data` PVC and no third volume (the `render.go:690-692` byte-identity property for non-docker
renders must survive, or a docker-only change rolls every plain worker); a test that the docker
render's `dind-data` volume is a `PersistentVolumeClaim` and not an `EmptyDir`; a materializer test
for the third PVC's create-gate; and the A8.8 ownership check on a real cluster. M-c, if built: a
materializer test that `desired < observed` issues **no** patch — the case that would otherwise
produce a rejected update every tick.

**A8.13 — issue #225 FILED** (`https://gitlab.example.com/vtmocanu/uzi/-/issues/225`) for A7.3's
imagefs/image-accumulation defect, per the user's decision to file rather than fix.

### A9 — 2026-08-04, USER DECISIONS. **THE DESIGN IS NOW FROZEN. The coder builds exactly this.**

**A9.1 — M-c is DROPPED.** No PVC resize path. The auditor's premise-level evidence carried it: the
77% reading is a **legacy 4Gi volume**, both live workers store an identical **2.77 GiB**, and the
current 20Gi default sits at **14.2%** with 15.7 GiB free. **n=1 affected worker.** The remedy is the
one `nixSize`'s own comment already prescribes — *"v1's remedy is delete + reprovision"* — and under
A6.1 a disrupting roll is already accepted, so the marginal cost is zero.

**What this avoids, which is the actual argument:** overturning `materializer.go:489-491`'s stated
*"never patched"* decision, a new `patch` verb on PVCs in the controller Role, observed-**size**
tracking in `observeNamespace` (it records booleans today), and re-arming issue #114 BUG 3 — where
k8s does not decrement `used.requests.storage` on a redundant admission charge (upstream #119593) and
pinned the quota at its hard limit. A patch charges the delta the same way.

> **OPERATIONAL, NOT CODE — do not put this in a commit.** Worker `8e1fef71`'s legacy 4Gi `/nix` PVC
> is fixed by deleting and reprovisioning **that worker**, which mints a new worker ID and therefore
> new PVCs. A pod roll does **not** do this: a Deployment rollout replaces pods and leaves PVCs
> alone. The disruption is already priced in under A6.1; the action is not automatic. Whoever runs
> the rollout does this by hand, by exact name.

**A9.2 — the quota triple is made HONEST at `deployments: 10`.** In `deploy/chart/values.yaml`:

| key | from | to | tier |
|---|---|---|---|
| `workers.docker.quota.requestsStorage` | `140Gi` | **`600Gi`** | docker |
| `workers.docker.quota.persistentVolumeClaims` | `20` | **`30`** | docker |
| `workers.quota.requestsStorage` | `280Gi` | **`600Gi`** | restricted |

**Raising a quota PROVISIONS NOTHING** — it is a ceiling on claims, not a reservation — so this costs
zero storage until workers actually exist. The docker figure is `10 x (20 data + 20 nix + 20 dind)`;
the restricted figure is `20 x (10 data + 20 nix)`, which is exactly the number A8.5 shows
`values.yaml:380-382`'s own comment already asks for and has been getting wrong since PRD #87.
**Both comments' arithmetic is rewritten with the current `nixSize`, in the same commit** (A8.5).

**A9.3 — final scope, in build order. One release, or the "one roll instead of three" argument fails.**

1. **M-a** — `dind-data` emptyDir → third per-docker-worker PVC. Flat **20Gi**,
   `workers.docker.dindDataSize` → `UZI_WORKER_DIND_DATA_SIZE`, `pick`/`ParseQuantity` like every
   other override. Rendered **only** when `w.Docker`. Plus the A9.2 quota raise, which M-a requires.
2. **M-b** — `requests.ephemeral-storage` **512Mi** plain / **4Gi** docker, on the **`worker`
   container only**, **no `limits.ephemeral-storage` anywhere in the pod spec**. Plus the doc sweep:
   A3.8's eight sites and A8.5's two quota comments.
3. **Not code:** A1's manual pod cleanup (**two** pods, not three — A5.6 — re-enumerate, exact names)
   and A9.1's worker reprovision.

**M-a must precede M-b in the history**, and not for file reasons: M-b's 4Gi is *derived from* M-a
having moved `dind-data` off ephemeral accounting. One combined commit makes 4Gi unjustifiable from
the diff alone, and M-b first would ship 6Gi.

**A9.4 — what ships in the release note**, none of it optional: rolling this **kills every in-flight
run once** (A6.1); the third RWO PVC widens the Multi-Attach window on reschedule (A8.7.1); the image
cache is now **persistent with no GC** (A8.7.2, `docker system prune` is the remedy); and #224
**lowers the frequency** of a silent total work loss rather than fixing it (§1).

### A10 — 2026-08-04, M-a LANDED at `35ef2996`, and a spec defect the LEAD introduced

**A10.1 — 🔴 A9.2 AND A9.3 CONTRADICTED EACH OTHER AND THE CODER WAS RIGHT.** A9.2 said A8.5's two
quota comments are rewritten *"in the same commit"* as the raise, which is **M-a**; A9.3 item 2
listed the same correction under **M-b**'s doc sweep. Both were written by the lead, in one sitting.
**Resolved in favour of A9.2 — the correction belongs in M-a**, because M-a is the commit that moves
`280Gi → 600Gi`, and a comment still reading *"= 280Gi"* beside `requestsStorage: 600Gi` is a wrong
doc **created by that commit**. A9.3 item 2 is amended accordingly; do not read it as still owing.

**A10.2 — the A3.8 sweep is now SEVEN sites, not eight.** The coder also took
`values.yaml`'s `ephemeralStorage` comment and its verbatim copy in `worker-docker-namespace.yaml`
into M-a, and the reasoning is sound: **M-a falsifies that sentence a second time** — the data root
is no longer on node ephemeral storage at all — and splitting one sentence's correction across two
commits is worse than doing it once. Both clauses were corrected (the emptyDir claim and the
"bounds a runaway pull" claim). **M-b's remaining sweep: the six `requests.*` enforcement sentences
plus `render.go:289`'s `presetRequestsDominateTheSeed`.**

**A10.3 — a pre-existing staticcheck nil-deref was pulled in by the ratchet, exactly as documented.**
`render_test.go:815-824` (`bb187d97`, not this branch's) blocked the gate the moment the coder
touched that file — `whole-files: true` behaving as CLAUDE.md says it does, **not a red gate to
report**. Fixed in M-a: the nil-capabilities check is now `Fatal`, which is the correct semantics
independently, since reading `caps.Add` past a non-fatal `Error` panics instead of reporting the
security regression the test exists to catch. Lint was re-run with
`--max-same-issues=0 --max-issues-per-linter=0` **before** counting, per CLAUDE.md's cap-reading
trap: **0 issues**, so the printed list was not a cap reading.

**A10.4 — lead's own verification of M-a, against the artifact rather than the report.** 12 files,
matching the claim exactly. `render.go:859` renders `dind-data` as a `PersistentVolumeClaim`;
`:864` and `:869` keep `run-workdir` and `dind-sock` as `EmptyDir` (correct — A4.5/A8.11:
`run-workdir` must never be bounded). No `ResourceEphemeralStorage` anywhere in `render.go` yet,
which is right, that is M-b. Quotas live at docker `600Gi`/`30` and restricted `600Gi`/`40` — the
restricted PVC count correctly **unchanged**, since that tier gains no third volume. Tree clean.

**A10.5 — mutation evidence, recorded because it is the part that makes the tests mean anything.**
All four load-bearing sites were folded and each reddened a named test; restore was by **cp backup**
(not `git checkout --`, which would revert to HEAD and is banned here while work is uncommitted),
the pattern-present assert was checked, and the restore was verified by a green full suite rather
than by a grep:

| fold | reddened |
|---|---|
| `dind-data` volume back to `EmptyDir` | `TestDinDDataVolumeIsAPVCAndRunWorkdirIsStillAnEmptyDir` |
| drop `if w.Docker` in `RenderPVCs` | 4 tests incl. `TestPVCsSizeFromThePresetAndNixIsFlat` |
| drop the `HasDinDDataPVC` create-gate arm | `TestDinDDataPVCIsCreatedForDockerWorkersOnlyAndGatedOnObservation` |
| drop `dindDataPVCName` from teardown | `TestTeardownRemovesTheDinDDataPVC` |

### A11 — 2026-08-04, M-b LANDED at `2a63ddb3`. A9.3's CODE scope is complete.

`512Mi` plain / `4Gi` docker on the `worker` container, chart-overridable per tier
(`workers.ephemeralRequest`, `workers.docker.ephemeralRequest` → `UZI_WORKER_EPHEMERAL_REQUEST`,
`UZI_WORKER_DOCKER_EPHEMERAL_REQUEST`, both `ParseQuantity`-validated at boot; docker **replaces**
plain). `preset.Size` untouched. `task gate:controller` rc=0, lint "0 issues.", deadcode "clean".

**A11.1 — lead's verification against the artifact.** `render.go:1044` sets
`ResourceEphemeralStorage` in **Requests only**; the `Limits` list immediately below is cpu and
memory, with a comment naming the enforcing test. `TestNoContainerDeclaresAnEphemeralStorageLimit`
exists at `render_test.go:400` and covers `.spec.containers` **and** `.spec.initContainers`, with a
`seen < 2` guard so it cannot pass vacuously if the render ever stops producing containers. **That
guard is the part worth noticing** — it is the *a control that produces no output is not a control*
rule applied to an invariant test, without being asked for.

**A11.2 — the mutation that matters most passed.** Folding in `limits.ephemeral-storage` "for
symmetry" reddened `TestNoContainerDeclaresAnEphemeralStorageLimit` in **all three postures**. That
is the exact future edit A4.3 predicted and A3.2 forbids, and it is now mechanically blocked rather
than only documented. The other three folds (remove the request = the pre-#224 state; put a budget
on the `dind` sidecar; raise docker to 20Gi) each reddened their intended test.

**A11.3 — the chart was parsed in BOTH postures, which caught a real ordering requirement.**
docker ON → all three vars present; docker OFF → `EPHEMERAL_REQUEST` present and both docker vars
**absent**. That is why `UZI_WORKER_EPHEMERAL_REQUEST` is validated **outside** `validateDockerTier`
— a plain-tier var validated inside the docker gate would go unchecked on every non-docker
deployment. `assert-chart-render.sh`: 38 / 24 documents, one kind each.

**A11.4 — doc sweep complete, seven sites, and the corrections carry a guard against their own
misreading.** Each corrected sentence now names cpu and memory *and* states that the rule holds
**exactly** for the constants it justifies and does not generalise — so nobody reads A3.1 as licence
to relax `seedResources`/`dindResources`/`dindInitResources`, which is the precise error the
withdrawn §2 bullet would have caused. Seventh site: `presetRequestsDominateTheSeed` →
`TestSeedContainerRequestsStayUnderEveryPreset`, with a line recording that no such symbol ever
existed. `git grep -F` sweep clean; every surviving hit is correction text naming the retired
wording or this PRD's past-tense record. Nothing in `specs/`, `docs/`, `adr/`, `ARCHITECTURE.md` or
`.claude/` carried either claim.

**A11.5 — TWO CHECKS THE CODER DECLINED, BOTH CORRECTLY, BOTH STILL OWED.** Neither was skipped
silently, and neither is a code change:

1. **A3.9(2)'s `kubectl apply --dry-run=server`** of a rendered worker Deployment into a namespace
   carrying the real quota + LimitRange. It needs a live cluster. **It is the only check that
   discriminates ADMISSIBILITY**, which is the failure that presents as a worker that provisions and
   never appears. `e2e:kind-smoke` does **not** cover it (A4.7: `workers.enabled: false`, never
   renders a worker pod). → tester, or the rollout.
2. **A8.8's `fsGroup` check on a real provision.** Rootless daemon uid 1000 against pod `fsGroup`
   10001, on a freshly-provisioned PVC. Invisible to `helm template` and to every unit test; A8.8
   calls it the most likely thing to break M-a in practice. → **first thing to check on the
   dev-cluster run.**

**A11.6 — still outstanding, none of it code.** A9.3 item 3: A1's manual pod cleanup (**two** pods,
re-enumerate, exact names) and A9.1's reprovision of worker `8e1fef71`. Plus A9.4's four
release-note lines, which are the documenter's. Tip carries no CI-skip marker, so a push produces a
real pipeline.

### A12 — 2026-08-04, AUDIT of M-a at `35ef2996`. Verdict sound; one Medium to fix before merge.

Read from a detached worktree. **The cluster went unreachable partway through** (`dial tcp
192.0.2.25:6443: i/o timeout`, 4 attempts over ~10 min), so the live quota `used` after the raise
was **not** re-verified and the auditor says so rather than inferring it. See A12.6.

**A12.1 — 🔴 MEDIUM: `dindDataSize` is DOCUMENTED IN THREE PLACES AND GUARDED IN NONE, and the
failure is the exact shape this issue is about.** `dindDataDefaultSize = "20Gi"` (`render.go:257`),
`workers.docker.dindDataSize: 20Gi`, and `workers.docker.limitRange.maxPVCStorage: 20Gi` — **equal**,
so the default sits exactly on the ceiling and cannot be raised without raising the max too.
`validateDockerTier` puts the new var in the same `quantities` loop as the DinD overrides, which
checks only that the string **parses**; there is no comparison against `maxPVCStorage`, and the
controller cannot know that value.

Traced at this SHA, with `dindDataSize: 40Gi`: `helm template` renders **clean**; the controller
boots **clean**; `RenderPVCs` emits the 40Gi claim; limitranger rejects it (*"maximum storage usage
per PersistentVolumeClaim is 20Gi, but limit is 40Gi"*); `materializer.go:531-534` returns **before
the Deployment is created**; `reconcile.go:221` logs *"reconcile cycle failed; retrying next tick"*.
**The worker never appears, forever, retrying every tick, with the reason only in the controller's
log** — nothing is reported back to the api (`:221`'s log line is the only consumer). That is
`preset.go:14-17`'s worker-that-provisions-and-never-appears, arriving through the knob this change
just added.

**The fix is already an idiom here.** `deploy/chart/templates/worker-invariants.yaml` carries **six**
`fail` guards including cross-value ones of exactly this shape, and its header says the guard *"stays
the single definition of the rule."* A seventh converts an infinite silent retry into a `helm
install` error naming both values. **`nixSize` (20Gi) sits on `maxPVCStorage` (20Gi) with no guard
either, on BOTH tiers** — pre-existing, not M-a's, and a guard written against the *max* rather than
against one claimant covers both pairs at once.

**A12.2 — LOW: the worst-case principle was applied to one tier and not the other, in the commit
that states that principle.** Docker is now self-consistent for the first time — at `l` (60Gi/worker)
all three caps meet at exactly **10**, the advertised number, and the auditor's earlier finding is
resolved. Restricted at `600Gi` caps at **15** `l` workers against `deployments: "20"`. The comment
is **not false** — it says "20 x `m`" and delivers 20 `m` workers — but `l` is available on that tier
(`preset.SizeNames()` is global, `render.go` takes `spec.Size.DataSize` for both tiers with no tier
scoping). **20 x `l` needs 800Gi.** Same defect class M-a exists to fix, one tier over, one line.

**A12.3 — RELEASE NOTE, second-order and easy to misread as a bug: raising `dindDataSize` on a live
cluster is a SILENT NO-OP for every existing docker worker.** `specHash` hashes the pod template,
which names `ClaimName` but never the claim's size, and `RenderPVCs` is *"never patched"*. So no
roll, no error, no effect. The commit message states this and the reasoning is right; **A9.4's list
does not**, and an operator who raises the value and sees nothing happen will file a bug.

**A12.4 — Lens 3, Multi-Attach: NO FINDING.** `renderPVC` is unchanged in access mode (`RWO`, same
function, one added call site), the strategy is still `Recreate`, and the dind claim mounts into the
`dind` container in the same pod — same node, same attach lifecycle. The change is **+1 in the
count and nothing in kind**. A8.7.1's planned release-note sentence is accurate as written.

**A12.5 — Lens 4, fsGroup: CLEAN by inheritance, with one genuinely new combination.** Pod-level
`FSGroup: 10001` with `FSGroupChangeOnRootMismatch` is unchanged, and fsGroup is a **supplementary
group on every container**, so `dind` at uid 1000 carries gid 10001 regardless of its `RunAsUser`.
`volume_linux.go` **ORs** the mask and never reduces, so the delta from the emptyDir is exactly one
bit:

| | base | after fsGroup | owner |
|---|---|---|---|
| emptyDir (before) | 0777 | **2777** | `root:10001` |
| fresh ext4 PVC (after) | 0755 | **2775** | `root:10001` |

**Only `o+w` is lost, and `o+w` was never the access path** — uid 1000 reaches it through
supplementary group 10001. Positive control that fsGroup is live on this cluster at all: `data` and
`nix` are the same class of object and are written by uid 10001, so if fsGroup were not applied they
would already be broken.

**The caveat: `dind-data` is the FIRST PVC in this pod mounted by a container whose uid is NOT the
fsGroup.** New combination, not a new mechanism. **On dev-cluster it is moot** —
`deploy/values/dev-cluster.yaml:175` sets `rootless: false`, so `dind` runs `RunAsUser: 0` and real
root ignores DAC. **But the CHART DEFAULT is `rootless: true`**, so the uid-1000 combination is what
every other cluster gets and no deployment exercises it today. Sharpening it: the volume mounts
**directly at the data root** (`render.go:1032`), so the daemon's data root *is* the volume root,
owned `root:10001`, and the daemon does not own it. Whether rootless dockerd tolerates that is not
decidable read-only and the auditor declined to assert it. Check commands are in its report; the
real signal is **`docker info` succeeding**, not the daemon merely answering, since dockerd creates
subdirectories lazily.

**A12.6 — OPERATIONAL, before anyone provisions a docker worker post-merge:**
`kubectl get resourcequota -n uzi-workers-docker`. If ArgoCD has not synced `600Gi`/`30`, M-a's PVC
creates are rejected on `requests.storage` at the old 140Gi — **the same infinite-retry path as
A12.1, from a different cause.** The auditor could not check this; the apiserver was down.

**A12.7 — security scan: CLEAN, with an armed instrument.** `gitleaks` scoped to the commit: `no
leaks found`, rc=0. `task scan:secrets`: `EXIT=0`, `0 findings in tracked files (1495 in the index)`,
and **`canaries DETECTED (gitlab-pat, gitleaks v8.30.1)`** naming both planted files. **The canary
line is the positive observation** — a bare `no leaks found` is exactly what a disarmed scanner
prints. Decision 3 intact: `dindContainer` still mounts only its own data root, the shared workdir
and (rootless) the socket dir, never the token, `/data` or `/nix`.

**A12.8 — noted, not graded: the observe/teardown asymmetry is defended only by prose plus one
test.** `RenderPVCs` gates on `w.Docker`; `observeNamespace` and teardown deliberately do **not**,
because teardown runs against a worker the api has already dropped and no `Docker` flag exists to
branch on. Both comments say why. **That is exactly the kind of asymmetry a later refactor "tidies"
into a bug**, and `TestTeardownRemovesTheDinDDataPVC` is the only mechanical defence.

### A13 — 2026-08-04, REVIEW of M-a at `35ef2996`. No Blocking. Two LEAD errors corrected.

**A13.1 — the byte-identity property is CONFIRMED EMPIRICALLY, not read for.** A harness using only
exported API (`RenderDeployment`/`RenderPVCs`/`RenderSecret`/`SpecHashOf`), compiled **unchanged at
both SHAs**, dumping canonical JSON for a **plain** worker across all 6 `(template, size)` pairs with
a maximally-populated `RenderConfig`:

```
19dab430 (parent)  47239 bytes
35ef2996 (M-a)     47239 bytes      IDENTICAL — Deployment + PVCs + Secret + spec hash, all 6
```

**With a negative control, because an empty diff is also what a broken harness produces**: flipping
one field to `Docker: true` and re-running the identical harness gives rc=1, **234 lines**, six
changed spec hashes and the emptyDir→PVC swap. So the harness discriminates and the plain render
genuinely did not move. **This is the check that matters most for A6.1's cost** — if it had moved,
a docker-only change would roll every plain worker and add a fleet-wide loss event.

Two tests pin what is pinnable *within one tree*; neither pins the plain hash to a **literal**, so a
future change altering the plain render some other way is uncaught. **Not a gap the coder created**
— no golden of the plain render exists in the module, and a cross-commit comparison is exactly what
a single-tree test cannot do.

**A13.2 — the create-gate, teardown and #114 BUG 3 are correct, including the part that is easy to
get wrong.** The gate is on **observation**, matching `HasDataPVC`/`HasNixPVC`; the steady-state test
asserts **zero** creates and reads create **names** rather than a count, so it can say *which* claim
was skipped. And `observeNamespace` derives `id` from the **label** (`IsOurs(p.Labels)`), not by
parsing the object name — checked specifically because `-dind-data` contains `-data`, so a
name-parsing observer would read `uzi-hw-abc-dind-data` as id `abc-dind` and set the wrong flag.
Teardown is unconditional and NotFound-tolerant; a docker-gated delete would have leaked a 20Gi claim
per worker forever against a quota that counts PVCs.

**A13.3 — the direction that would have been FATAL was checked: limitranger's `maxConstraint` is
`>`, not `>=`.** A 20Gi request under `maxPVCStorage: 20Gi` is **admitted**. Had it been `>=`, every
docker worker's dind PVC would have been refused and the whole tier would provision pods that never
appear. Nobody else checked the inclusivity of that comparison.

**A13.4 — 🔴 LEAD ERROR 1: the `render.go:690-692` citation does not resolve, and I PROPAGATED IT.**
At `35ef2996` those lines are the `NODE_EXTRA_CA_CERTS` comment. The byte-identity comment is at
**`750-753` at `35ef2996`** and at `689-692` at the parent — so the citation is off-by-one against
the parent and points at unrelated code in the tree the commit creates. The commit message is
immutable here (no force-push), so **the actionable part is that my dispatch to the reviewer
repeated it verbatim**, and A8.12 carries it too. **Use `render.go:750-753 at 35ef2996`.** The
comment the commit *adds* at `render.go:494` cites no line number and is the right pattern.

**A13.5 — 🔴 LEAD ERROR 2: A10.3's `(bb187d97, not this branch's)` implies authorship and is not
quite right.** `bb187d97` **relocated** the code — its diff removes the identical
`caps == nil ||` / `range caps.Add` pair unindented and re-adds it inside a `t.Run`. The pattern
**originates at `5c676f7c`** (`git log -S 'for _, c := range caps.Add'`). The commit message itself
says only "pre-existing", which is correct; only my parenthetical implied more. This is the
stale-identifier class the same commit swept seven sites of, committed inside the record of that
sweep.

**A13.6 — N2 and N3: two conditional Blockings, DISCHARGED BY M-b HAVING LANDED, and the condition
is now load-bearing.** At `35ef2996` alone, `worker-docker-namespace.yaml` states **P and ¬P about
the same mechanism 23 lines apart** (`:70-72` the old false `requests.*` sentence, `:93-95` the new
refutation), with nothing marking one as superseding the other; and `values.yaml:551` describes M-b
in the **present tense** while M-a alone renders no `requests.ephemeral-storage` at all. Both are
true the moment M-b lands and false in any release carrying M-a alone. **M-b (`2a63ddb3`) has
landed and A8.10/A9.3 already require both in ONE release** — so this is discharged. **But it
converts "one release" from an efficiency argument into a correctness one**: splitting them ships a
file that contradicts itself.

**A13.7 — N4, for the follow-up commit: the suite's own fixture demonstrates an INADMISSIBLE
value.** `TestDinDDataSizeDefaultsOverridesAndDoesNotRollThePod` uses `DinDDataSize = "40Gi"` and
`TestLoadDockerDinDDataSize` asserts `UZI_WORKER_DIND_DATA_SIZE=40Gi` loads. Both pass, and 40Gi is
**double** `maxPVCStorage` — it boots cleanly and then has its PVC rejected, which is A12.1's exact
failure. The boot validation catches an unparseable string (the lesser problem) and **structurally
cannot** catch an oversized one, because the controller does not know the LimitRange. Fix: use
`10Gi` as the fixture, or one sentence saying 40Gi is deliberately above the ceiling and why the
render-level assertion is still valid.

**A13.8 — N5 agrees with A12.2 from the other direction.** The two tiers now use **different sizing
conventions** and coincide at 600Gi — docker worst-case, restricted nominal — which invites reading
them as one derivation. A12.2's raise to 800Gi resolves it; the restricted comment should also say
it is `m`-sized on purpose and what an `l`-heavy fleet costs.

**A13.9 — N6: the 4Gi→20Gi nix drift has TWO MORE LIVE SITES, in `specs/ai.md`.** `:6884` reads
*"`/nix` is a flat 4Gi outside the table"*, sitting directly beneath a preset table whose other
figures are all current — so it reads as present-tense and is false. `:8394` carries *"`preset.nixSize`
(a flat 4 GiB)"*. The `prds/done/58` and `prds/done/87` occurrences are past-tense records and
correctly stay. **→ spec-keeper (task #7).** This commit's doc sweep was specifically about this
drift and stopped at the chart.

**A13.10 — N7: `docs/worker-setup.md:122` now under-informs a k8s operator.** It calls the dind
cache *"a compose `dinddata` volume, or its k8s equivalent"* and gives a compose-only reclaim recipe.
After M-a the k8s equivalent is a persistent PVC with no GC. One sentence naming `docker system
prune` and delete+reprovision. **→ documenter (task #6).**

**A13.11 — N8, UNRESOLVED, cluster down: PV reclaim policy is now a 1.5x'd exposure.** Teardown
deletes the PVC; whether the backing PV and datastore disk are freed depends on `storage-class`'s
`reclaimPolicy`. **Under `Retain` the storage leaks per torn-down worker and the quota cannot see
it**, because a quota counts PVCs, not orphaned PVs. Pre-existing for `/data` and `/nix`; three
claims per docker worker now. One command answers it:
`kubectl get sc storage-class -o jsonpath='{.reclaimPolicy}'`. **Reported unresolved rather than
guessed** — the apiserver went unreachable for the reviewer too, independently of the auditor.

**A13.12 — the fsGroup scoping nit AGREES WITH A12.5, reached independently.** Both flag that
dev-cluster sets `rootless: false`, so `dind` runs as real root and ownership cannot block it, while
the chart default `rootless: true` is the unexercised posture. **A8.8's "first thing to check"
should be scoped to the rootless posture**, not stated flatly.

**A13.13 — release-note addition: the daemon's BUILD CACHE now survives a pod roll.** A hosted worker
is per-user so there is no new cross-user surface, but build-arg and layer residue from run A now
outlives a restart into run B **for the same user**, where the emptyDir erased it. Beside A9.4's
no-GC residual line.

**A13.14 — what the reviewer did NOT run, recorded so the report is not read as broader than it
is:** no `fmt-check`, no `lint` (it set no `GOLANGCI_LINT_CACHE` because it ran no lint at all, and
did not `cache clean`); `go vet` on `./internal/kube/` only; `go build` covered implicitly by the
test compile. Everything else it re-ran end to end, including `go test -count=1 -race ./...` at
`RUN=182 PASS=182 FAIL=0 SKIP=0`.

### A14 — 2026-08-04, AUDIT of M-b at `2a63ddb3`. Clean. Coverage of both commits confirmed.

**A14.1 — the `validateDockerTier` split is CORRECT and load-bearing, proved two independent ways.**
`UZI_WORKER_EPHEMERAL_REQUEST` is validated in `loadWorkerSettings` (`config.go:284-289`), outside
the gate; the docker vars stay inside it. The reasoning is sound rather than merely plausible:
`validateDockerTier` opens with `if ns == "" && img == "" { return nil }`, a genuine early return, so
a plain-tier var placed there is **silently ignored on every non-docker deployment**.

*Proof 1, Go-side mutation* — moved the plain var into the docker list, i.e. built exactly the
mistake the comment says it avoids. `go vet` **rc=0** (behavioural, not a build error), and the
plain subtest reddened with a self-explaining message while the docker subtest stayed green.
*Proof 2, chart-side render* — the Go split only matters if the chart injects the same way:

```
=== DOCKER ON (34 docs) ===              === DOCKER OFF (26 docs) ===
UZI_WORKER_EPHEMERAL_REQUEST     512Mi   UZI_WORKER_EPHEMERAL_REQUEST     512Mi
UZI_WORKER_DOCKER_EPHEMERAL_REQ  4Gi     UZI_WORKER_DOCKER_EPHEMERAL_REQ  <ABSENT>
UZI_WORKER_DIND_DATA_SIZE        20Gi    UZI_WORKER_DIND_DATA_SIZE        <ABSENT>
```

**With the tier off the plain var is injected ALONE** — which is the deployment the placement
argument names, now an observation rather than an argument.

**A14.2 — 🔴 "MERGED TO `main`" IS NOT "DEPLOYED" FOR THIS CHANGE, AND THE DELIVERY STEP IS IN A
DIFFERENT REPOSITORY.** This closes the item A12.6 had to leave open; the cluster came back at
`06:11:43Z` and **still carries the OLD quotas** (docker `140Gi`/`20`, restricted `280Gi`). That is
expected, and the reason is the operative part:

The quota raise lives in **`deploy/chart/values.yaml`** — inside the chart, which ArgoCD takes from
Harbor OCI at `targetRevision: 0.14.0` (source 1) — **not** in `deploy/values/dev-cluster.yaml`
(source 2, tracking `main`). **So merging to `main` delivers neither the quota raise nor the new
controller.** Both arrive only with a `v*` release tag **plus a manual `targetRevision` bump in
`argo-apps/apps/uzi/app.uzi.yaml`**, which is a different repo.

**No skew window exists, and that was checked rather than assumed:** `controller-deployment.yaml:58`
renders the image tag as `.Values.workers.controller.image.tag | default .Chart.AppVersion`, and
`workers.controller.image` is **unset** per-cluster (verified by parsing the file, not by grep) —
live confirms `controller:0.14.0` against `targetRevision: 0.14.0`. **Image and quotas cannot arrive
separately.** Even a hypothetical skew is bounded: under the old 140Gi, the two existing docker
workers gaining a 20Gi dind PVC each takes `used` 64Gi → **104Gi** (PVCs 4→6 of 20), which fits; only
a *third* worker would be refused, at 164Gi. **→ A9.4 gains a line.**

**A14.3 — M-b's substance encodes the mechanisms correctly.** `ephemeralRequest`'s header states,
with reasoning beside each: the three-formula asymmetry (`PodRequests` sums restartable sidecars,
`GetResourceRequestQuantity` does not, so 1Gi+8Gi is charged 9 and credited 8), the three
independent reasons never to declare a limit, the `max`-needs-a-`default` trap, and that
**"conservative" means err LOW** because too high is unschedulable, silent and total. The auditor's
note: that last is the one it would have written itself.

**A14.4 — the invariant test is real, confirmed by a third party.** It walks
`.spec.initContainers` **and** `.spec.containers` across plain / docker-rootless /
docker-non-rootless and carries `if seen < 2 { t.Fatalf }` with the reason stated. Mutation on
M-b's load-bearing site (removing the request) reddened both intended tests; `go vet` rc=0 first, so
it was behavioural rather than a build break; restored by `cp` backup, `git status` clean, full suite
green.

**A14.5 — the doc sweep verified against the auditor's OWN original enumeration**, not against the
coder's claim — it found the six sites in the design wave, so it is the right checker.
`git grep -F 'quota on requests.*'` and `'ResourceQuota on requests.*'` both **rc=1, no matches**.
The three surviving `presetRequestsDominateTheSeed` hits are all correct: `render.go:440` is a
deliberate past-tense record (*"It named `presetRequestsDominateTheSeed` until issue #224; no such
symbol has ever existed in this tree"*) and two are this PRD's own record. **That is CLAUDE.md's
past-tense-is-a-typo / present-tense-is-a-wrong-doc distinction applied the right way round.**

**A14.6 — both M-a findings STILL STAND at `2a63ddb3`** (already dispatched to the coder):
`worker-invariants.yaml` still carries six guards and none mentions `dindDataSize`, `maxPVCStorage`
or `ephemeralRequest`; the restricted tier still holds 15 `l` workers against the 20 it advertises.
**M-b adds no new instance of the Medium, correctly** — neither tier declares an ephemeral `max`, so
`ephemeralRequest` has no admission ceiling to collide with, and adding one would need a `default`
that hands the deterministic-eviction trap to every pod in the namespace.

**A14.7 — the 200Gi `ephemeralStorage` quota key finally does something.** M-b is the first change
that makes any pod declare the resource, so `used.requests.ephemeral-storage` stops being
permanently `0`. It cannot wedge anything: 10 workers x 4Gi = 40Gi against 200Gi hard; the key binds
at 50 workers against a 10-deployment cap; even at a 20Gi override it is reached exactly as the
deployment cap is. **Worth knowing because a reader who remembers "that key reads 0" will now see
40Gi and wonder what changed.**

**A14.8 — two below the bar, both deliberate asymmetries worth one sentence each if anyone edits
nearby.** A garbage `UZI_WORKER_DOCKER_EPHEMERAL_REQUEST` with the tier OFF is **silently ignored**
until the tier is switched on — the pre-existing behaviour of all five docker knobs, and the correct
trade-off (refusing to boot over an inert knob is worse), but it is the mirror image of the bug the
plain-var placement avoids. And `ephemeralRequest(w)` returns the built-in `4Gi` for a docker worker
whose docker override is empty, **never falling through to the plain value** — which is what
"REPLACES" means and is implemented correctly, but the two values.yaml comments say "replaces"
without saying what an empty docker override does, and a reader could guess either way.

**A14.9 — scanners, armed.** `gitleaks` over `35ef2996~1..2a63ddb3`: exit 0, 3 commits, 64.65 KB, no
leaks. `task scan:secrets`: EXIT=0, `0 findings in tracked files (1495 in the index)`, **`canaries
DETECTED (gitlab-pat, gitleaks v8.30.1)`**. Exit codes read off files on the following line, not
through `${PIPESTATUS[0]}` — the auditor walked into that zsh trap in its M-a report and did not
repeat it.

### A15 — 2026-08-04, PRE-GUARD CONTROL and three traps in A12.1's own fix

Captured by the auditor at `2a63ddb3` **before** the guard lands, because it gets harder afterwards:
a post-fix green proves the tree is green, not that the guard *changed* anything.

```
CONTROL A  shipped defaults (dindDataSize 20Gi)    rc=0  34 docs  UZI_WORKER_DIND_DATA_SIZE=20Gi
CONTROL B  --set dindDataSize=40Gi vs max 20Gi     rc=0  34 docs  UZI_WORKER_DIND_DATA_SIZE=40Gi
                                                   stderr: 0 bytes
```

**B is the disconfirming observation**: today the oversized value renders *completely clean* — zero
stderr, a full manifest, a controller env happily carrying `40Gi` — and then ships a controller that
retries a rejected PVC forever. That is the state the guard must flip, now anchored at a known SHA.

**A15.1 — 🔴 THE GUARD MUST BE STRICT `gt`, OR IT BREAKS EVERY INSTALL.** `dindDataSize` (20Gi) and
`maxPVCStorage` (20Gi) are **equal in the shipped defaults** — by design, since 20Gi is
simultaneously the default and the ceiling. `{{- if ge ... }}` therefore **rejects the shipped
defaults**. A coder who tests only the 40Gi case sees a working guard and ships one that fails closed
on the default path. **Control A is the arm that catches this**, which is why it was flagged before
the commit rather than after.

**A15.2 — 🔴 HELM'S `ge`/`gt` ARE STRING COMPARISONS: `"4Gi"` > `"20Gi"` IS TRUE.** `"4"` sorts above
`"2"`, so **4Gi reads as larger than 20Gi**. Sprig has no `resource.Quantity`, so the guard needs
explicit normalisation — parse the suffix, or restrict to a documented unit and compare `int64`.
**A guard that silently mis-orders is worse than none**, because it fails in the reassuring direction
on exactly the values an operator is most likely to try. Same family as every instrument trap in this
file: clean, repeatable, confident, wrong.

**A15.3 — write the guard against `maxPVCStorage` AS THE CEILING, not against `dindDataSize` as one
claimant.** `nixSize` is 20Gi in `preset.go`, hard-equal to the same max, with **no chart knob at
all**. The broad form covers both pairs; the narrow form covers one. Recorded so the narrow choice,
if taken, is deliberate.

**A15.4 — the four arms that demonstrate the guard.** Two are not enough: the guard is a *constraint
between two values*, so it must be shown both to fire and **not** to fire.

| arm | pre-guard at `2a63ddb3` | post-guard required |
|---|---|---|
| A — shipped defaults | rc=0, 34 docs *(measured)* | **rc=0, 34 docs** — unchanged, or the guard broke the default path |
| B — `dindDataSize=40Gi` | **rc=0, 0 bytes stderr** *(measured)* | **rc≠0**, message naming both values |
| C — `maxPVCStorage=40Gi` **and** `dindDataSize=40Gi` | not captured; expect rc=0 | rc=0 — raising them together is the documented escape |
| D — A12.2's `requestsStorage: 800Gi` | 600Gi | 800Gi, and `800/40 = 20` `l` workers == the 20 advertised |

**Arm C is the one that proves the guard is a constraint rather than a hard-coded rejection of
40Gi.** The auditor's own framing, which generalises past this guard: *"a guard nobody has seen fire
is not a guard — and a guard nobody has seen NOT fire is not one either."*

### A16 — 2026-08-04, REVIEW of M-b at `2a63ddb3` (delta pass). No Blocking. One record correction, one real finding.

**A16.1 — the two load-bearing kubelet claims CONFIRMED against upstream, independently, for the
third time.** The reviewer read `pkg/api/v1/resource/helpers.go` rather than trusting the comment:
`GetResourceRequestQuantity` (which `exceedDiskRequests` calls) sums `.spec.containers` then
max-folds each `.spec.initContainers` entry **with no `RestartPolicy` check anywhere**, while
`PodRequests` **does** special-case `ContainerRestartPolicyAlways` and adds it cumulatively. So the
shipped comment's worked example is arithmetically exact: worker 1Gi + dind 8Gi is **charged 9 and
credited 8**. *"The single most falsifiable claim in the commit, and it holds."* All three
never-a-limit sub-claims likewise confirmed, including that all three `localStorageEviction` arms
pass `0` as `gracePeriodOverride` — **which is what structurally couples this to #218**: a limit
would delete the pod with no grace, so any SIGTERM-time fetch-back #218 grows could never run.

**A16.2 — 🔴 REAL FINDING (N2): the fleet-fit test COPIES two chart values into Go with nothing
gating the copy, and the copy defeats the test's own purpose.**
`TestShippedEphemeralDefaultsFitAWholeFleetOnRealNodes` hardcodes
`const dockerWorkers, plainWorkers = 10, 20`, which are `values.yaml`'s `count/deployments.apps` for
the two tiers. **Raise the docker tier to 20 there and the real fleet needs `20 x 4Gi + 20 x 512Mi =
90 GiB` against a 70.2 GiB pool — precisely the condition this test exists to catch — and it stays
green**, because its premise is a hardcoded 10.

`nodeAllocatable = 17.55` is a *different* kind of constant and its comment justifies it honestly (a
fact about one cluster; a smaller cluster lowers the values through the chart). **The two quota
counts are not: they have a source in this repo and were copied instead of referenced.** Cheapest
fix, and the one to take: a line on both `values.yaml` keys saying a controller test mirrors them.
**→ the follow-up commit.**

**A16.3 — 🔴 CORRECTION TO A11.3, AND IT IS THE UNCONTROLLED-COMPARISON SHAPE THIS TEAM FLAGS
ELSEWHERE.** A11.3 recorded "38 / 24 documents" as a docker ON/OFF pair. **Those halves come from
two different value sets:**

```
-f deploy/values/dev-cluster.yaml                                       38
-f deploy/values/dev-cluster.yaml --set workers.docker.enabled=false    30
-f deploy/values/dev-cluster.yaml --set workers.enabled=false           19
chart DEFAULTS --set workers.enabled=true --set api.tls.enabled=true     24   <- what 24 actually is
```

So **38 is the dev-cluster render and 24 is the chart-defaults render**, reported as one
before/after. The reviewer re-took it as a controlled pair — same file, one `--set` flipped — and
**the conclusion is unaffected**: ON injects both vars, OFF injects the plain one alone. Only the
numbers in the record were wrong. A11.3 is amended: the controlled pair is **38 / 30**.

**A16.4 — AND THE TWO VALIDATORS' ABSOLUTE COUNTS DISAGREE, WHILE THEIR DELTA AGREES. Recorded, not
adjudicated.** A14.1 (auditor) reports **34 ON / 26 OFF**; A16.3 (reviewer) reports **38 ON / 30
OFF**. Both describe dev-cluster with one toggle flipped, both at `2a63ddb3`. **The delta is 8 in
both**, so whatever differs is a constant 4 documents present in one invocation and not the other —
plausibly a release-name, an extra `--set`, or empty-document handling, none of which either agent
recorded. **Neither is being treated as the winner.** The claim both support is the one the design
rests on and it is not a tally: *with the docker tier off, the plain env var is injected alone.*
This is CLAUDE.md's own rule arriving on our own artifacts — **cite the shape, not the count** — and
it is the second time in this PRD that two careful agents produced different numbers for what each
described as the same measurement.

**A16.5 — the invariant test survives both questions a test has to answer.** *What production edit
fails it?* Adding the key to any container's `Limits` in `render.go` — a production edit, so it is
not decoration. *Do the assertions execute in the failing case?* Yes: no `Fatalf` inside either
loop, and the `seen < 2` guard is after them. **And it is the negative half of a PAIR** —
`TestWorkerDeclaresTheWholeEphemeralBudgetAndNoOtherContainerDoes` positively asserts the same typed
key in `Requests`. That is CLAUDE.md's *deliberate* paired shape rather than the unpaired vacuous
one, and the key is a typed constant, so no copy change can silently retire it.

**A16.6 — mutations reproduced independently, and the discrimination is sharper than reported.** The
sidecar-request fold reddens the budget test on **docker postures only** with `plain` correctly
unaffected — **and leaves the limit test green**, because a request is not a limit and the two tests
separate cleanly. The 20Gi fold reddens both arms of the fleet-fit test with self-explaining
messages naming the node ceiling and the pool total.

**A16.7 — N3, a conditionally-true clause in an otherwise exact paragraph.** `render.go`'s trap (2)
says `containerEphemeralStorageLimitEviction` "measures rootfs+logs". Upstream measures `Logs`
unconditionally and `Rootfs` **only when `!*m.dedicatedImageFs`** — so on a dedicated-imagefs node,
which **A7.1 established 3 of 4 of this pool's nodes are**, container-level enforcement measures
**logs alone**. The conclusion drawn ("emptyDirs are excluded from container-level enforcement
entirely") is correct on **both** branches, so nothing downstream moves. Flagged because the
split-imagefs fact is load-bearing elsewhere in this PRD and someone will try to reconcile the two.

**A16.8 — N4, zero margin where it matters most.** The `seen < 2` guard passes at the boundary on a
plain render, which has exactly two containers (`seed-nix` + `worker`); docker postures have 3 and 4.
It achieves its stated purpose and should not change — **but it would not notice a plain render that
lost its init container**, which is the neighbouring regression. One word in the message if anyone
touches it.

**A16.9 — `--dry-run=server` is worth MORE now than it was at M-a.** M-b is the commit that first
moves the docker tier's `used.requests.ephemeral-storage` off `0` — to `10 x 4Gi = 40Gi` against the
200Gi ceiling. Comfortable, but a **live** number rather than a dead one for the first time since
that key was written. The restricted tier tracks no ephemeral key at all, so plain workers' 512Mi
stays unbudgeted — **correct** (a quota only rejects on resources it tracks) and stated here so
nobody "fixes" it.

**A16.10 — `deadcode` without `-test` is byte-identical across M-a, M-b and the pre-#224 parent** (4
findings, only `SpecHashOf`'s line number moving). **Neither commit orphaned anything.** Gates
re-run: `go test -count=1 -race ./...` EXIT=0, `RUN=195 PASS=195 FAIL=0 SKIP=0`. Doc sweep re-done
against **five alternative phrasings** with the hits read rather than post-filtered: zero survivors
anywhere in `specs/`, `docs/`, `adr/`, `ARCHITECTURE.md`.

### A17 — 2026-08-04, FOLLOW-UP `6bf44a86` closes A12.1 / A12.2 / A13.7. **A15.3 was WRONG.**

Verified on the branch: `git merge-base --is-ancestor 6bf44a86 HEAD` → yes, sitting between A14 and
A15 (the lead's docs commits interleave with the code commits, which is expected). `gt` confirmed at
`worker-invariants.yaml:140`, `uzi.quantityBytes` at `_helpers.tpl:214`, `requestsStorage: 800Gi` at
`values.yaml:446`, `TestPresetPVCSizesFitTheChartsLimitRangeMax` at `preset_contract_test.go:231`.
Gates: `task gate:controller` **rc=0**, `task gate:repo` **rc=0** with **both canaries DETECTED**.

**A17.1 — 🔴 LEAD ERROR, THE THIRD THIS PRD: A15.3's "the broad form covers both pairs" IS FALSE,
AND A HELM GUARD CANNOT BE MADE TO COVER THEM.** I told the coder to write the guard against
`maxPVCStorage` as the ceiling "so it covers `nixSize` too". **Helm cannot read a Go constant.**
Verified on this tree: `grep -rn 'nixSize' deploy/chart/` returns **comments only, never a value** —
`nixSize` is `preset.go:146` and nothing else. So no form of a chart guard, broad or narrow, can see
it. The broad form covers **one** pair and generalises cheaply to future *chart-supplied* claimants;
that is its real value, and it is not what I claimed.

**Three claimants sit exactly on the 20Gi ceiling** — `dindDataSize` (chart), `nixSize` (Go), `l`'s
`DataSize` (Go) — **and they are guarded by TWO mechanisms, neither of which covers all three:**

| claimant | guarded by |
|---|---|
| `dindDataSize` | the chart guard (`worker-invariants.yaml`), broad form over a claimant dict |
| `nixSize`, `l.DataSize` | `TestPresetPVCSizesFitTheChartsLimitRangeMax`, mutated twice (`nixSize`→40Gi, `l.DataSize`→30Gi) |

Putting the Go quantities into `values.yaml` so the chart guard could reach them **was considered and
rejected**, correctly: it would make the chart a **fourth place preset quantities appear and the only
one with no golden gating it** — A7.5's own rejected class, cited back at me. The commit message says
all of this; it is repeated here because A15.3 implies the chart half is sufficient and **it is not**.

**A17.2 — THE HELPER HAD MY OWN FAILURE SHAPE IN IT, AND A THIRD MODE I DID NOT NAME.** I warned
about `ge`-vs-`gt` (A15.1) and string comparison (A15.2). The obvious implementation has a **third**
defect that **fails OPEN**: regex the digits, look the suffix up in a dict, multiply — **a missing key
multiplies to zero**, so `20GB` rendered as **`0`** and passed silently. *"A guard built on that
passes precisely the misconfigurations it exists to catch, while looking like it works."* Found **by
probing, not by reading**. `uzi.quantityBytes` now validates digits and suffix explicitly and `fail`s
on either.

**A17.3 — the guard was mutated SEVEN ways, each checked for the RIGHT failure rather than merely for
failing.** Both directions of the string-compare trap are now covered — my A15.2 example
(`4Gi` under max `20Gi`) **renders**, where a string compare would have rejected it; the converse
(`20Gi` vs `20000M`) **fails**, which is the case a string compare gets backwards.

| case | result |
|---|---|
| 20Gi vs 20Gi (shipped) | renders — `gt` not `ge`, equal is correct (A15.1's arm A) |
| 40Gi vs 20Gi | fails, naming both keys and both values |
| 40Gi vs 40Gi (raise the max too) | renders — so it is the **pair** that is guarded (A15.4's arm C) |
| 20Gi vs 20000M | fails — the case a string compare gets backwards |
| 20000M vs 20Gi | renders — correct direction |
| `40GB` / `20gi` | fail **closed** on the suffix, not silently compared as 0 |
| docker tier OFF | does not fire |

**A17.4 — the Go-side guard duplicates `maxPVCStorage`, acknowledged rather than overlooked, and it
fails safe in the direction that matters.** Lowering the chart's max without lowering the test's
leaves the test green while the cluster rejects claims — the mild direction. **Both `maxPVCStorage`
keys now name the test and the test names both keys**, which is the mitigation A16.2 asked for on the
fleet-fit constants, applied here without being asked.

**A17.5 — A12.2 landed and both tiers are now EXACTLY self-consistent at their advertised fleet**,
verified from a yaml parse rather than mental arithmetic:

```
restricted  20 x (20Gi data + 20Gi nix)      = 800Gi   quota 800Gi   pvcs 40 vs 40
docker      10 x (20 + 20 + 20)              = 600Gi   quota 600Gi   pvcs 30 vs 30
```

The coder applied the 800Gi decision as directed **and disagrees with the alternative on record**:
scoping `l` off the restricted tier would be a real behaviour change with an api-visible surface,
since nothing in `preset.SizeNames()` or `render.go` is tier-aware today — against a quota raise that
provisions nothing. Recorded because it is the reasoning, not just the outcome.

**A17.6 — A13.7 closed**: both 40Gi fixtures → 10Gi, and the config one keeps a sentence saying the
boot validation **structurally cannot** catch an oversized value. A12.3 is documented at the
`dindDataSize` values key, where an operator reads before raising it.

**A17.7 — the coder corrected its own earlier slip, unprompted.** It reported `task lint:repo` as
"does not exist"; that was its slip, **not a repo defect** — `lint:repo` is a CI *job* that invokes
`task gate:repo`, the same name-collision shape CLAUDE.md documents for `build:web`. Nothing to fix.

**A17.8 — the document-count instability now has a THIRD reading, and it settles A16.4's approach as
the right one.** The coder measures **32** (shipped defaults + docker on) and **38** (dev-cluster);
the auditor reported 34, the reviewer 38/30. **The coder declines to call the auditor wrong** and
says only that *a count is not a stable identity across invocations* — which is exactly right, and is
CLAUDE.md's cite-the-shape-not-the-tally rule reached independently by a third agent. **The control
is invocation-independent and is the thing to re-run:** at `2a63ddb3` the 40Gi case renders clean; at
`6bf44a86` it fails naming both keys and both values. **That flip is the evidence, not any count.**
