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
