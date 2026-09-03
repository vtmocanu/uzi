# ADR-1036: A checkpoint ref may carry a worker-built `.github/workflows` overlay commit; the broker stays byte-untouched

**Status**: Accepted (PRD #1062 M2 / issue #1036 — implemented on `agent/src/git.ts` + `agent/src/runner.ts`; agent-side only)
**Date**: 2026-09-03
**Deciders**: architect (M2 design pass, D7 → Option B); PRD #1062 Decision Log (D7/D8); Vlad (maintainer)
**PRD**: [prds/done/1062-checkpoint-durability-completion.md](../prds/done/1062-checkpoint-durability-completion.md) (GitHub issue [vtmocanu/uzi#1036](https://github.com/vtmocanu/uzi/issues/1036)) — the PRD carries the milestone, the fixture set, and the full Decision Log; this ADR carries the transport-wrapper contract and the two decisions (the base-first parent order, and the post-reap PAT classification) most likely to look wrong to a future reader.
**Related**: closes the mid-run half of [ADR-456](0456-rebase-before-finalize-push.md)'s deferred D5 — it is the checkpoint sibling of that ADR's #627 `workflow-subtree` finalize overlay. Builds on [ADR-122](0122-checkpoint-push-broker.md) (the api-brokered checkpoint machinery this contract feeds without changing) and [ADR-628](0628-cross-worker-resume-durability.md) (whose #1042/#1059 owner-anchor `runs.checkpoint_tip` this overlay must keep matching on resume).

## Decision (summary)

A GitHub worker whose branch is behind `main` under `.github/workflows/` had every forge checkpoint rejected and mapped to `skipped:"workflow_scope"` (the broker PAT lacks `workflow` scope; ADR-456's premise), so a long-running behind-on-workflows branch that died between finalize attempts lost its work with no mid-run safety net. This is closed **entirely worker-side**, with the `pushbroker` and `workersvc.Service.Publish` **byte-untouched** (Option B). The contract:

1. **The checkpoint ref may carry a synthetic `ckpt(overlay):` commit.** When the branch is behind on workflows (and did not itself modify a workflow file), the worker builds `O_ov` — a wrapper commit whose tree equals the real tip's tree except that `.github/workflows` is replaced by the current default's — and declares `O_ov` (not the real tip) as the checkpoint tip. Its workflow tree byte-matches the default's, so GitHub's tip-vs-default scope check passes and the **unchanged** broker pushes it as an ordinary fast-forward.
2. **The wrapper is transport metadata, never branch content.** On adoption the worker **peels** `O_ov` back to its real tip, discarding the swapped tree, so the agent lands exactly where it parked and the branch never contains the overlay (which preserves #377's "the agent branch never carries a workflow-swapped tree" property at finalize).
3. **The overlay is a post-reap, PAT-bearing operation.** Building it fetches the default tip with the worker PAT, so it is threaded ONLY on paths where the agent tree is already reaped. This narrows `checkpointPack`'s security classification (see the second decision) but keeps REAP-BEFORE-GIT intact.

Entry points: `buildWorkflowOverlay` / `checkpointPack` and the overlay peel + `isOverlayMarker` in `runnerCloneForBranch` (`agent/src/git.ts`); the reaped-paths-only overlay wiring and `flight.lastCheckpointRefTip` in `agent/src/runner.ts`.

## Context

ADR-456 established that GitHub rejects a `repo`-only PAT push whenever the pushed tip's `.github/workflows/` tree differs from the current default — even when the branch's own commits never touched a workflow file. Its #627 amendment already ships a **finalize** fix: `alignBranchWithDefault(…, "workflow-subtree")` overlays the default's workflow subtree onto the agent tip before the finalize push. ADR-456's D5 explicitly deferred the **mid-run checkpoint** sibling: the broker is fast-forward-only, shallow-fetched, and worktree-less, so it could only detect-and-skip the rejection, leaving a behind-on-workflows run with no mid-run coverage. This ADR closes that gap by moving the overlay to the worker, where the same subtree swap is available, without touching the secrets-holding broker.

## The decisions

### The overlay is built via a temp index — no worktree, no filter drivers

`buildWorkflowOverlay` synthesizes `O_ov` as pure object-DB work in the worker bare, using a throwaway index (`GIT_INDEX_FILE`): `read-tree <realTip>` → `rm --cached -r --ignore-unmatch .github/workflows` → (only when the default has a workflows tree) `read-tree --prefix=.github/workflows <defaultTip>:.github/workflows` → `write-tree` → `commit-tree`. `--ignore-unmatch` is load-bearing: against a temp index with no worktree, `git rm --cached` exits non-zero when nothing matches, so without it the no-`.github`/already-deleted edges would error instead of no-op. The one `read-tree --prefix` step needs a `GIT_WORK_TREE` set (it refuses in a bare repo), so a throwaway empty work-tree is passed for that op **without `-u`** — nothing is checked out, so no `.gitattributes` filter/smudge driver fires. That is a deliberate security property, not an accident: it is why the throwaway-*worktree* form of this design was rejected (see Alternatives). The result carries a `ckpt(overlay):`-prefixed subject and a **deterministic** identity (`AGENT_GIT_IDENTITY`, author+committer date = the real tip's own committer date, `commit.gpgsign=false`), so a no-new-work rebuild yields the same OID.

### 🔴 Parent order: base FIRST, real tip LAST (this is the load-bearing correction)

`O_ov = commit-tree newRoot -p <prevCheckpointTip> -p <realTip>` — the prior checkpoint tip is parent[0], the real tip is the **last** parent. With no prior checkpoint there is a single parent `-p <realTip>`. The PRD's original draft had the reverse order (`-p realTip [-p prevOverlay]`); that order is **rejected by the broker** and would make every sequential overlay skip `not_descendant`.

Why: the broker's strict-descendant check (`strictDescends`/`descendsOrEqual` in `api/internal/pushbroker/pushbroker.go`, `descendsOrEqual` runs go-git `baseCommit.IsAncestor(declared)`) is a **parent[0]-first preorder DFS** from `declared` toward its parents, over the broker's **depth-1** object store. (The "parent[0]-first" order is go-git's `NewCommitPreorderIter` behaviour — verified against go-git v5.19.2, the pinned version; the in-repo `descendsOrEqual` comment says only "preorder", so this base-first decision is coupled to that iterator's ordering and would need re-checking if go-git ever changed it.) The broker's own comment at that call documents the walk: a genuine descendant reaches the base and stops before running below it; a non-descendant runs off the end of the depth-1 pack into the branch-fork's excluded parent (`D_old`), surfaces `plumbing.ErrObjectNotFound`, and is mapped to a benign `not_descendant` skip.

- With **realTip as parent[0]**, the walk descends the real tip's whole chain to the branch fork and hits the depth-1-EXCLUDED `D_old` **before** it ever reaches parent[1] = the base — so a legitimately-sequential overlay is falsely rejected `not_descendant`.
- With the **base as parent[0]**, `IsAncestor` finds the base immediately and stops → the overlay is accepted as a fast-forward. The broker stays byte-unchanged.

The determinism above also matters here: because the OID is stable across a no-new-work rebuild, the broker's "already up to date" early-return still fires on an idempotent resume.

### Adoption peels to the LAST parent, before the wip-park soft-reset

In `runnerCloneForBranch`, when the adopted base is a `ckpt(overlay):` commit (`isOverlayMarker` probes the subject), the base is re-pointed to the overlay's **last** parent — which is the real tip, by construction (base-first/realTip-last), NOT `^1` and NOT "the first non-overlay parent". This DISCARDS the swapped tree. The peel runs **before** the existing `wip(park):` `reset --soft` recovery, so a wip-park marker stacked under an overlay is still recovered. The two markers have **opposite** tree semantics and that ordering is the whole point: overlay = discard-by-reparent (outer), wip-park = keep-by-soft-reset (inner). Because `runs.checkpoint_tip` stores the declared `O_ov` (the broker persists the declared tip unchanged), ADR-628's #1042/#1059 owner-anchor matches the mirrored ref trivially on resume, and the terminal CAS-delete lines up.

### The overlay changes `checkpointPack`'s security classification on reaped paths

Pre-M2, `checkpointPack` was a credential-free local object read. Building the overlay adds a PAT-bearing `fetchDefaultTip`, so the overlay context is threaded ONLY where the agent tree is already reaped: park, graceful shutdown, and the mid-run `reap:true` milestone (where `opts.reap ⇒ killAgentTree` is hoisted strictly before the fetch). The mid-run `reap:false` time-gate runs with the agent ALIVE and passes NO overlay — a PAT git op there would violate REAP-BEFORE-GIT, so that path is byte-behaviourally unchanged. GitHub-only; other forges impose no workflow-scope rule and take no overlay.

### The broker and `service.go` are deliberately NOT involved

`O_ov` is a genuine fast-forward descendant of the checkpoint ref, so every `pushbroker` invariant (never-forced, strict-descendant, depth-1 fetch) holds unchanged; `Publish` persists the declared tip via `SetRunCheckpointTip` exactly as before (no `Result.Tip`, no synthesis in the secrets holder). Keeping the overlay out of the broker is the core of the decision, not an incidental convenience — see Alternatives.

## Alternatives considered

- **Option A — broker-side go-git synthesis of the overlay.** Rejected (PRD D7). It would add object synthesis AND a strict-descendant/never-forced rework to `pushbroker`, the exact secrets-holding surface issue #1009 hardened, plus a `realTip`-vs-`O` split in `runs.checkpoint_tip` (the D2 seam) that reproduces #1009's total-loss risk if any consumer forgot to unwrap. Option B leaves the broker byte-untouched and reuses #627's already-shipped subtree overlay shape.
- **The throwaway-*worktree* form of Option B** (reuse `alignBranchWithDefault`'s `checkout`/`reset --hard`/`clean`/`checkout <ref> -- …`). Rejected. Those are working-tree ops; worker-side they fire attacker-chosen `.gitattributes` filter/smudge drivers **as the PAT holder** — the code-exec vector the bare-only invariant closes — and would `reset --hard` the live runner clone's in-flight work. The temp-index form fires no filter driver and never populates a work tree.
- **The PRD's original parent order (realTip first).** Rejected: the broker's parent[0]-first depth-1 DFS runs off the depth-1 pack into the excluded old default and falsely skips a sequential overlay `not_descendant` (see the base-first decision). Base-first is the fix.
- **Merge or rebase the branch onto the default mid-run.** Rejected (ADR-456 D2 lineage). A rebase rewrites SHAs and diverges the checkpoint from `refs/uzi-runner/<branch>` and the live session, failing the owner-anchor; a merge FF-composes but adds a needless commit to a transport wrapper. Only the conflict-free subtree overlay is used.

## Consequences and residuals

- A behind-on-workflows GitHub branch now gets a real mid-run/park/shutdown checkpoint the unchanged broker accepts and adoption peels cleanly — closing ADR-456 D5's deferred gap with zero broker/schema/migration change.
- Documented residuals (recorded, not solved):
  1. A between-milestone `reap:false` hard-kill on a behind-on-workflows branch is still uncovered (no PAT git op with the agent alive); park, graceful shutdown, and the `reap:true` milestone cover the dominant hosted case.
  2. A plain checkpoint shipped after an overlay (the branch is no longer behind while a prior overlay is on the ref) skips `not_descendant` until the next overlay chains and lands — self-healing.
  3. A cold-resume no-work rebuild grows the overlay chain by one commit (the deterministic early-return does not fire when `prevCheckpointTip` is a prior overlay).
  4. The raised best-effort persist window: push-success then persist-fail then death leaves the ref at `O_ov` while the DB tip is stale, so the checkpoint is not adopted on the next claim.
- GitHub-only; GitLab/Forgejo skip the whole path.

## Linked from ARCHITECTURE.md

Linked from ARCHITECTURE.md's Run-lifecycle checkpoint-durability section (the PRD #1062 bullet, alongside ADR-456 and ADR-628), per the repo convention.
