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
calls `RequeueWorkerRuns`, and on re-claim `createOrAttachRunnerClone`'s unconditional `fs.rm`
reseeds from origin. Nothing was ever pushed. The run's feed shows a normal re-claim, so the loss
is silent. kubelet's own message names the cause: `Container worker was using 100060Ki, request is 0,
has larger consumption of ephemeral-storage.`

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
- If (b) is what is true, the same comment is load-bearing for `seedResources` / `dindResources`
  / `dindInitResources` and is wrong there too, and CLAUDE.md's fix-the-doc rule applies.

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

**Open scope question, going to the user (§4 Q4):** the issue's `## Cleanup` section — three
lingering `Failed`/`Evicted` pods in `uzi-workers-docker`, one over 7 days old, not GC'd
automatically. Manual one-off cleanup, or a controller-side reaper?

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

**Q5 — What is the steady-state number?** Nobody has measured it. Say what measurement would give
it (a `kubectl exec du` sweep across live workers, `kubelet_volume_stats`, a Grafana query) and
whether it can be taken before the fix lands. A guessed request that is too low re-creates the bug;
too high strands node disk across the fleet.

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
| architect | dispatched 2026-08-04, design wave, at `0111f01c` |
| reviewer | dispatched 2026-08-04, design wave, at `0111f01c` |
| auditor | dispatched 2026-08-04, design wave, at `0111f01c` |
| fact-checker | dispatched 2026-08-04, design wave, at `0111f01c` |
| coder | pending — spawns only once the design freezes (§4 answered) |
| tester | pending — spawns after the coder's FIRST commit, not at kickoff |
| documenter | pending — CHANGELOG entry at minimum; CLAUDE.md/chart comment fixes if §2 lands as a doc correction |
| spec-keeper | pending — `specs/` exists; dispatch after blocking findings clear |
| web-ux | closed — no web surface. Sizes are displayed by name only (`hosted_sizes.json` carries names, not quantities), so no rendered quantity changes. Reopen if Q3 lands a user-visible per-size disk figure. |
| researcher | closed — no research surface; every question in §4 is answerable from this tree, the chart, or the live cluster. |
| release | closed — no release cut in this task's scope. |

## Amendments

_(none yet — findings from the design wave land here, dated, before the coder spawns)_
