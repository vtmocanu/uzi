# PRD #84: Capability-aware run scheduling & plan-gate — match runs to workers that can run them

**GitLab Issue**: [#84](https://gitlab.example.com/vtmocanu/uzi/-/issues/84)
**Status**: Draft (created 2026-07-18)
**Priority**: Medium
**Depends on**: PRD #4 (run state machine + claim protocol — `queued → claimed → running → awaiting_approval → running`, and the `FOR UPDATE SKIP LOCKED` claim this PRD adds a predicate to); PRD #18 (worker templates + the declared/reported self-report this PRD promotes from observability to a scheduling input, and the devbox tool tiers whose "provisionable" set the gate must subtract); PRD #41 (plan-revision gate — the approval surface this PRD extends).
**Related**: PRD #83 (docker-capable worker) is this PRD's **first consumer** and depends on M1–M4 — docker is the first capability that cannot be runtime-provisioned, which is what forces this mechanism to exist. PRD #58 (hosted k8s workers) provides the "provision a docker worker" remediation the gate offers. PRD #42 (concurrency) — the sweeper must not time out a run that is *waiting for an eligible worker* as if it were a stale claim.

## Problem

A run is claimed by whatever worker grabs it first (`FOR UPDATE SKIP LOCKED`, with an affinity grace for resumes). A worker's template is **observability-only** today — PRD #18 self-reports it and badges declared-vs-reported drift, but nothing routes on it. So if a run needs a toolchain or capability the claiming worker lacks, it fails **mid-run** with `command not found`, after burning a claim and part of a token budget, with a confusing failure the user cannot pre-empt.

This has been survivable for one specific reason: missing **CLI tools** are auto-provisioned per run (devbox tier-1/2, PRD #18), so "tool not found" is largely self-healing. That masks the absence of any real matching.

**PRD #83 breaks the mask.** Docker is the first capability that **cannot** be runtime-provisioned — it needs a sidecar/daemon, not a nix package (PRD #83 Decision 1). A docker-needing run that lands on a `base` worker is **dead on arrival**: nothing can install a daemon into it at run time. So the system must know, *before committing the run to a worker*, that it needs a docker-capable one — and must do something better than crash if none exists. (`jvm` has had the same latent gap since PRD #18; it is simply rarely hit. This PRD fixes the general case and retrofits `jvm`.)

How does the user — or the planner — know a run needs a docker worker, and what happens when the available worker can't satisfy it?

## Inspiration check

| Aspect | Kubernetes scheduler | CI runners (GitHub/GitLab) | uzi today | uzi (this PRD) |
|---|---|---|---|---|
| Match work to capable executor | `nodeSelector` / taints+tolerations / `runtimeClass` | `runs-on:` labels / runner tags | None — any worker claims any run; template is a badge | Capability set per worker + `required ⊆ caps` predicate on the claim |
| "No capable executor" | Pod stays `Pending`, surfaced with a reason | Job queues on a label with no runner; surfaced as pending | Silent — run is claimed and fails mid-flight | Pre-claim "no eligible worker" state + remediation, or blocked at the plan gate |
| Who declares the need | Author sets selectors/tolerations on the pod | Author sets `runs-on` | Nobody | Static per-repo hint **and** plan-time inference by the lead (both) |

The pattern is settled prior art: **capability-as-selector scheduling with a pending/surfaced state when nothing matches.** uzi already has the two hard parts — workers self-report (PRD #18) and runs already **stop for human approval** (`awaiting_approval`). This PRD wires those together rather than inventing a scheduler.

## Solution Overview

Three layers — **Declare → Match → Gate.**

1. **Declare (both sources — user decision 2026-07-18):**
   - **Static per-repo hint** (`required_capabilities: [docker, …]`) in repo settings, next to PRD #18's tool profile / tier-2 toggle; optionally an issue label (`uzi:needs-docker`). Known **before claim**, so the scheduler can route without waiting for a plan to run.
   - **Plan-time inference** by the lead: the planning step scans the clone (`docker-compose*.yml`, `Dockerfile`, testcontainers deps, test scripts invoking `docker`) and emits a structured `required_capabilities` in the plan, alongside the agents it already proposes. **Authoritative and escalation-only** — it can add a requirement the hint missed, never remove a hinted one.
   - Effective requirement = hint ∪ plan-inferred.

2. **Match (capability-based claim):** every worker advertises a **capability set** derived from its reported template (via a server-side template→capabilities map) plus provisioned-tool tags. The claim query gains a predicate: a run's **non-provisionable** required capabilities must be a subset of the claiming worker's capabilities. A `base` worker will not claim a `docker` run. Affinity/resume grace is unchanged.

3. **Gate (stop only when genuinely unsatisfiable):** `unmet = required − (worker_caps ∪ provisionable)`. A missing CLI tool that tier-1/2 can install is **not** unmet (it is provisioned, not gated). If `unmet` is non-empty:
   - **At the plan-approval gate** (when discovered by inference): the approval UI shows required capabilities + whether the assigned/available workers satisfy them, and **blocks approval** with a reason ("this plan needs a `docker` worker; you have `base`") and one-click remediation — **provision a docker worker** (hosted, via PRD #58) or the compose bring-up, or re-plan.
   - **Pre-claim** (when the static hint already makes it unsatisfiable and no eligible worker is online): the run enters a surfaced **"no eligible worker"** state instead of hanging silently or crashing mid-run — same remediation CTA. It does not consume a claim.

## Design Decisions

1. **Capability matching is *scheduling*, and is distinct from *provisioning*.** The load-bearing split: **provisionable** needs (nix CLI tools, PRD #18 tiers) are satisfied by installing them per-run and must never gate; **non-provisionable** needs (docker, and today `jvm` unless in the template, later gpu) can only be satisfied by landing on the right worker, so they are matched at claim time and gated when unmet. The requirement's *type* picks the path. Getting this split right is what makes docker usable without regressing #18's provisioning UX.

2. **Two declaration sources: the hint pre-routes, the plan escalates** (user, 2026-07-18: "both"). The static repo hint is known before a plan exists, so the scheduler can route a statically-hinted run to the right worker with zero plan latency; plan-time inference is authoritative and catches needs the hint missed. They compose as a union, and inference is **escalation-only** so a heuristic can never silently *drop* a requirement the user explicitly declared.

3. **Reuse the existing plan-approval gate; add exactly one new pre-claim state — do not invent a scheduler.** uzi already halts at `awaiting_approval`; capability satisfaction is surfaced there for the inference path. The only genuinely new state is a pre-claim **"no eligible worker"** for the case where the hint makes a run unsatisfiable *before* any plan runs — otherwise such a run would either hang in `queued` forever (no worker will ever claim it) or, worse, be claimed and crash. That state is surfaced with remediation, not a silent queue.

4. **Capabilities are a bounded, declared vocabulary — not free-form strings.** A small server-owned enum (`docker`, `jvm`, `gpu`, …), each mapped to (a) the templates that provide it and (b) optionally a provisionable-tool tag. Templates declare what they provide; the map is code-reviewed, so matching is deterministic and the UI/CLI can render a fixed set. No repo-supplied arbitrary capability names (they would be unmatchable and a spoofing surface).

5. **Never over-gate.** A requirement that tier-1/2 provisioning can satisfy must not block approval or claiming — that would regress the PRD #18 experience where "install my repo's tools" just works. The gate subtracts the provisionable set first; only the residue stops a run. (This is Decision 1 restated as a hard rule for the gate implementation and its tests.)

6. **The hint is a contract; inference is a heuristic.** The static hint is the user's explicit, always-honored declaration. Plan-time inference (scan for compose/Dockerfile/testcontainers/`docker` in scripts) has false negatives by nature; it may only **add** requirements. A repo the scan misses still has the hint as the reliable path, and a missed inference degrades to today's clear mid-run failure — never to a wrong *removal* of a requirement.

7. **Matching is best-effort scheduling + a clear failure, not a security boundary.** A worker self-reports its capabilities; the join token stays the sole trust anchor (PRD #18 Decision 5). A worker that falsely claims `docker` and lacks it simply fails the run — identical to today's drift, not an escalation. So this PRD adds no trust surface; it improves routing and UX. (Guardrails and the uid split remain the security layers.)

## Technical Design

### 1. Capability vocabulary + worker advertisement
- A server-owned capability enum + a `template → capabilities` map (e.g. `base → {}`, `jvm → {jvm}`, `docker → {docker}`), plus provisioned-tool tags where relevant. Code-reviewed, tested.
- Register payload (extends PRD #18's `template` self-report): the worker advertises its capability set (derived from template + any baked/provisioned tools). Stored per worker; surfaced on the workers/admin pages next to the drift badge.

### 2. Run requirement model + static hint
- `runs.required_capabilities` (array/JSONB, or a small join table). Populated at enqueue from the repo hint (and issue label if present); updated by plan inference (§4), union-merged.
- Repo settings: `repo.required_capabilities` static hint, living beside the #18 tool profile + tier-2 toggle. Optional issue-label parsing (`uzi:needs-<cap>`).

### 3. Capability-aware claim
- Extend the claim query (`workersvc` / the `FOR UPDATE SKIP LOCKED` selection): a run is claimable by a worker only if `required_capabilities − provisionable ⊆ worker_capabilities`. `provisionable` is computed from the same allowlist/tier config #18 uses. Affinity/resume grace and all other claim semantics unchanged.
- The sweeper (PRD #42) must distinguish a run **waiting for an eligible worker** from a **stale claim** — the former is not a timeout/requeue candidate on the claim-heartbeat path.

### 4. Plan-time inference + the gate
- The lead's plan step (`agent/src` planning path) gains a bounded capability-inference pass over the clone and emits `required_capabilities` in the plan payload/schema, next to the proposed agents.
- The plan-approval gate (PRD #41 surface) computes `unmet` and, when non-empty, **blocks approval** with the reason + remediation actions (provision a docker worker via #58 / compose instructions / re-plan). The plan schema and the approval DTO carry the capability list + satisfaction status.
- New pre-claim **"no eligible worker"** run state for statically-known unsatisfiable runs; surfaced in web + CLI with the same remediation. Documented in the run state machine (`specs/ai.md` §36/§lifecycle).

### 5. Surfacing (web + CLI)
- Plan approval shows required capabilities and per-worker satisfaction; the block state renders the remediation CTA.
- Workers page shows each worker's capabilities. A run blocked on capability shows *why* + how to fix.
- `api/cmd/uzi` (PRD #64 CLI): the same requirement/satisfaction/block info exposed (list/inspect a run's required caps; the blocked state + remediation hint) — the CLI is a second consumer of the same API (CLAUDE.md CLI-parity rule).

### 6. Docs + specs
- `specs/ai.md`: capability model, the provisionable-vs-not split, the claim predicate, the new run state, plan `required_capabilities`. `ARCHITECTURE.md`: scheduling paragraph (workers advertise capabilities; runs carry requirements; the gate).
- Docs: repo-settings capability hint; what the plan gate's "needs a docker worker" block means and how to resolve it.
- `specs/human.md` is user-stated — not edited without approval.

## Milestones

| Phase | Milestone | Depends on | Touches | Parallel? |
|---|---|---|---|---|
| 1 | **M1** capability vocabulary + worker advertisement | #18 | `api` (enum, template→caps map, register), workers UI | first (foundational) |
| 2 | **M2** run requirement model + static repo hint + claim predicate | M1 | `api` (schema, `workersvc` claim), repo settings web | after M1 |
| 2 | **M3** pre-claim "no eligible worker" state + surfacing + remediation CTA | M1, M2 | `api` (state machine, sweeper), web, `api/cmd/uzi`, #58 CTA | after M2 |
| 3 | **M4** plan-time inference + approval-gate capability check | M1–M3 | `agent/src` (inference), `api` (plan schema, gate), web | after M3 |
| 3 | **M5** retrofit `jvm` onto the mechanism + docs + specs | M1–M4 | template→caps map, `docs/`, `specs/ai.md`, ARCHITECTURE.md | after M4 |

- [ ] **M1 — Capability vocabulary + worker advertisement.** Server capability enum + `template → capabilities` map; workers advertise their capability set at register; workers/admin UI shows it.
- [ ] **M2 — Requirement model + static hint + claim predicate.** `runs.required_capabilities`; repo-settings static hint (+ optional issue label); claim query filters `required − provisionable ⊆ worker_caps`. Statically-hinted runs now route correctly.
- [ ] **M3 — "No eligible worker" state + remediation.** Pre-claim surfaced state (not a silent hang / mid-run crash); sweeper distinguishes it from a stale claim; web + CLI show why + a "provision a docker worker" (#58) CTA.
- [ ] **M4 — Plan-time inference + gate.** Lead emits `required_capabilities` from a clone scan; the plan-approval gate blocks on non-empty `unmet` with remediation; plan schema + approval DTO carry caps + satisfaction.
- [ ] **M5 — Retrofit `jvm` + docs/specs.** `jvm` becomes a matched capability; `specs/ai.md`/ARCHITECTURE/docs updated; CLI parity verified.

**Consumer note:** PRD #83 (docker worker) registers `docker` in M1's vocabulary and relies on M2–M4 for routing + the gate. #83's compose track can ship a docker worker image before this lands, but a docker run is only *reliably reachable and gated* once M2–M4 are in.

## Risks & Open Questions

- **Inference false negatives.** A repo that needs docker in a way the scan misses won't be gated. Mitigated by the static-hint contract (Decision 6) and the fact that a missed inference degrades to today's clear mid-run failure, not a wrong result. Tune the scan heuristics; document them.
- **Provisionable-vs-not classification drift.** The gate's correctness depends on an accurate "provisionable" set; it must track PRD #18's tiers/allowlist as they evolve. A capability wrongly marked provisionable would let a run be claimed by a worker that then fails; wrongly marked non-provisionable would over-gate. Single source of truth + tests.
- **New run state × sweeper/timeouts.** "No eligible worker" must not be timed out like a stale claim (PRD #42), but also must not wedge forever — decide a policy (indefinite with a surfaced age, or a long soft cap with notification). **Open.**
- **Where inference runs.** Plan-time inference needs the plan step to execute on *some* worker; if the run is already unsatisfiable statically, the pre-claim state (M3) handles it without ever planning. If it's satisfiable statically but the plan escalates to a capability the worker lacks, the gate (M4) catches it after planning. Confirm both paths are covered by tests.
- **Open: issue-label declaration** — ship in M2 or defer? Recommendation: repo-settings hint in M2, label parsing deferred unless cheap.

## Decision Log

- **2026-07-18 (created).** Split out of PRD #83 at the user's direction: capability matching is a general mechanism (any capability), and docker is merely its first *non-provisionable* consumer, so it earns its own PRD rather than being folded into the docker feature. User decisions: **own PRD** (not folded into #83); **both** declaration sources (static repo hint pre-routes, plan-time inference escalates). Core shape: Declare → Match → Gate, reusing the existing plan-approval gate + worker self-report, with the provisionable-vs-non-provisionable split (Decision 1) as the idea that makes docker work without regressing #18's tool-provisioning UX. `jvm` retrofitted (M5) since it shares the latent gap.
