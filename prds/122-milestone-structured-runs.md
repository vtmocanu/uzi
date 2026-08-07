# PRD #122: Milestone-structured runs — approved milestones, live progress, and mid-run checkpoints

**GitLab Issue**: [#122](https://gitlab.example.com/vtmocanu/uzi/-/issues/122)
**Status**: Draft (created 2026-07-24; revised same day after a fable adversarial review that opened every cited reference and verified each load-bearing claim against the code — the review found all citations sound but corrected the loss model in Problem §3, exposed an unscoped server-side change in M2, pinned down Decision 9, and added the server-validation decision; see the Decision Log)
**Priority**: Medium
**Related**: [#218](https://gitlab.example.com/vtmocanu/uzi/-/issues/218) (**park/requeue work-loss — SHIPPED 0.15.0 + branch `218-m6`, validated live 2026-08-04; it built the durability primitive this PRD's Phase 2 proposed, so Phase 2 is superseded in part — see the banner below and Decisions 7 & 9**), [#110](https://gitlab.example.com/vtmocanu/uzi/-/issues/110) (checkpoint agent work — closed will-not-implement; **this PRD reopens the door its analysis closed, by a route that PRD did not consider — see Decision 8**), [#105](https://gitlab.example.com/vtmocanu/uzi/-/issues/105) (session lost on a different-worker requeue), [#41](https://gitlab.example.com/vtmocanu/uzi/-/issues/41) (plan revision — the gate loop this must not break), [#51](https://gitlab.example.com/vtmocanu/uzi/-/issues/51) (worker uid split), [#58](https://gitlab.example.com/vtmocanu/uzi/-/issues/58) (single-uid non-root start — the k8s posture that made #110 unsafe). Durability cluster that landed or was filed AFTER this PRD was drafted and that Phase 2 must be reconciled against: [#216](https://gitlab.example.com/vtmocanu/uzi/-/issues/216) (worker load-balancing / `ClaimRun` rewrite + affinity grace), [#217](https://gitlab.example.com/vtmocanu/uzi/-/issues/217) (which credential a resume spends), [#224](https://gitlab.example.com/vtmocanu/uzi/-/issues/224) (requeue work-loss with no usage limit), [#222](https://gitlab.example.com/vtmocanu/uzi/-/issues/222) (steering-channel staleness across a re-clone).

> **⚠️ 2026-08-07 — PHASE 2 IS SUPERSEDED IN PART BY PRD #218.** #218 (`prds/done/218-park-resume-work-loss.md`, SHIPPED 0.15.0 + branch `218-m6`, validated live on dev-cluster 2026-08-04) built the exact durability primitive Phase 2 here proposed, and this PRD predates it and references it nowhere. **Read `prds/done/218-park-resume-work-loss.md` and `agent/src/git.ts` before starting any Phase-2 work.** What #218 shipped:
> - The `refs/uzi-runner/<branch>` tracking ref and the credential-free `file://` `fetchAgentBranch` fetch-back into the worker's bare on `/data` — this PRD's **Decision 7**, verbatim (`git.ts` `RUNNER_TRACKING_PREFIX`, `fetchAgentBranch`).
> - The reseed preferring the tracking ref under the **strict-descendant** rule `git merge-base --is-ancestor origin <tracking>`, origin winning on divergence — this PRD's **Decision 9's** rule (`git.ts` `seededFrom: "origin" | "tracking" | "default"`).
> - A settlement of Decision 9's one **open** question, the stale-ref lifecycle: a **run-identity anchor** (`uzi-trackowner.<branch>` worker-bare config stamp; the reseed reads the tracking ref only when the stamp equals `claim.run_id`) — stronger than the resume-gating Decision 9 sketched.
> - The fetch-back wired on the **park and graceful-shutdown (SIGTERM)** paths (`fetchBackBestEffort`, `agent/src/runner.ts`).
>
> **Consequences for this PRD (applied below):**
> - **M7 (resume precision) is DELIVERED by #218 M2/M3** (recovered-commit count carried into the resume and stated in the feed). Dropped.
> - **M6 shrinks to a delta over #218**, not a foundation. #218 fetches back only on park + graceful SIGTERM, so a hard **SIGKILL** (OOM, node kill with no grace window) still loses everything since the last fetch-back. M6's residual value is the *proactive milestone-boundary checkpoint* (reap + fetch-back while the run is alive), bounding that loss to "since the last milestone", and it must adopt #218's `uzi-trackowner` anchor + `seededFrom` machinery rather than re-deriving Decision 9.
> - **Phase 2 file:line citations are STALE** — written against a pre-#218 tree that #218 rewrote. Re-derive against HEAD before citing (current symbols: `RUNNER_TRACKING_PREFIX`, `seededFrom`, `fetchBackBestEffort`).
> - **Phase 1 (M1–M5) is UNAFFECTED and still unbuilt** — the progress UI + budget resize remains the real motivation. One Phase-1 citation also drifted: the iteration badge is now `web/src/pages/RunView.tsx:515-517`, not `:270-272`. Re-verify Phase-1 line refs at implementation time too.
> - **The budget hard ceiling is now DECIDED (2026-08-07)**: profile "Generous" — milestone-count cap **12**, total turn ceiling `RUN_MAX_ITERATIONS × min(n, 12)` (≤ 60 turns), absolute wall-clock ceiling **8h**; single-milestone plans stay at today's 5 turns / 2h, and the count cap is enforced server-side (Decision 12). M2 implements it. See the 2026-08-07 Decision Log entry.
>
> **Status (2026-08-07): Phase 1 backend DONE — M1 (MR !194) + M2 (MR !195) merged.** M1 = `submit_plan.milestones` + the `00098` two-column store + DTO + Decision-12 validation; M2 = progress reporting (union/overwrite/membership) + the milestone-scaled budget with Decision 5b's per-run sweeper timeout + `00099`. **M3/M4/M5 (web UI, Slack, CLI) are NOT built and were deliberately not seeded.** Active work is **M8 — per-checkpoint origin publish via a server-side push broker** (see the two 2026-08-07 Decision Log entries at the bottom; Decision 8/14 + M8 rewrite pending approval).

## Problem

Three gaps, in descending order of how much they hurt.

### 1. A run cannot tell you where it is

The only progress datum a run reports is `iteration_count`: the worker sends it
at the top of each implement⇄review turn (`agent/src/runner.ts:330` →
`StateRequest`, `agent/src/protocol.ts:681`), the api stores it
(`api/internal/workersvc/service.go:1470,1519`), the DTO carries it
(`api/internal/apitypes/run.go:33`), and the web renders it as a badge
(`web/src/pages/RunView.tsx:270-272`).

`iteration 3` counts **review rounds**, not work completed. It is the same badge
whether the lead is 10% or 90% through the task, and it goes *up* when things go
badly (more review rounds) — so as a progress signal it is not merely absent, it
is inverted. Watching a long run today, the only way to answer "what is done and
what is left" is to read the message feed and infer.

### 2. The run budgets are sized for single-shot work

`RUN_MAX_ITERATIONS` defaults to **5** (`agent/src/sdk-executor.ts:54`,
`docs/configuration.md:177`) and the wall clock to **2h**
(`agent/src/sdk-executor.ts:52`). Both are per-**run** budgets over a single
`for (;;)` loop (`sdk-executor.ts:515`). A PRD-shaped issue with seven
milestones therefore either crams several milestones into one turn or trips
`REASON_MAX_ITERATIONS` and fails the whole run.

This is a live defect, not a hypothetical: the budgets predate any notion of a
run doing more than one coherent unit of work, and nothing scales them to the
size of the task the plan describes.

### 3. Durability is all-or-nothing

> **2026-08-07 — this section was written pre-#218 and is now half-mitigated.** PRD #218 (shipped) added a best-effort `fetchAgentBranch` fetch-back on the **park and graceful-shutdown (SIGTERM)** paths, plus a reseed that prefers the checkpointed tracking ref, so a same-worker resume after a park or a graceful requeue now recovers committed work. The residual, and the only durability gap Phase 2 still addresses, is a **hard SIGKILL** with no grace window and, more usefully, **loss between the last park/shutdown and now** — which the proactive milestone-boundary checkpoint (M6, re-scoped) bounds to "since the last milestone". The correction to #110's loss model below stands and #218 makes the same correction independently (its Problem section).

A run pushes **exactly once**, after the executor returns: the worker reaps the
agent tree (`agent/src/runner.ts:345`) and only then pushes
(`agent/src/runner.ts:399`). The runner clone that holds the agent's commits is
`fs.rm`'d and re-cloned at every seed (`agent/src/git.ts:280`), and on hosted
k8s it lives on an **emptyDir** when the worker is docker-capable
(`controller/internal/kube/render.go:796`) — so an attempt interrupted
mid-flight leaves nothing behind. The in-code comment says so in as many words
(`agent/src/git.ts:139-145`).

**The loss is wider than PRD #110 recorded, and the difference matters.** #110
states that a same-worker requeue inside `WORKER_AFFINITY_GRACE` "keeps the
in-flight SDK session on disk and resumes it — no loss". That conflates the
*session* with the *work*. The session transcript does survive, but **every**
claim re-seeds the runner clone — `runner.ts:208` → `runnerCloneForBranch` →
`fs.rm` (`git.ts:280`) → a fresh clone based off origin (`git.ts:291-293`) — so
the interrupted attempt's local commits are destroyed on a **same-worker**
requeue exactly as on a different-worker one. The resumed lead then remembers
making commits that no longer exist, which is a worse failure than plain
amnesia. `git.ts:139-145` says this plainly; #110's summary did not.

What genuinely is already mitigated: work from a *completed* prior run is pushed
and reused via `priorCommits` (`git.ts:288-299`, `runner.ts:258`,
`prompt.ts:167-176`). So the residual loss is **any** mid-implement interruption,
not only the different-worker case — which is what M6 addresses, and why the
Risks gate must measure interrupted runs rather than cross-worker hand-offs.

## Solution Overview

Give a run the structure its plan already implies, then let everything else hang
off that structure.

- **The plan carries milestones.** `submit_plan` gains an optional
  `milestones: [{id, title}]`. The human approves the plan **and** the breakdown
  at the existing gate. From approval on, the list is frozen.
- **Progress is reported as a set, not a cursor.** The lead reports which
  milestone ids are complete and which are in progress. The api unions the
  completed set; the UI derives done / in-progress / left from it.
- **The UI shows it.** A checklist on `RunView`, a compact `M3/7` badge where
  the iteration badge lives today, on the Dashboard, the runs list, and in
  `uzi` CLI output.
- **The budgets scale to the plan.** The iteration cap becomes a total turn
  ceiling derived from milestone count; the wall clock scales the same way, with
  a hard ceiling.
- **The milestone boundary becomes a durable checkpoint.** The lead calls
  `checkpoint`, the worker reaps the agent tree and fetches the branch back into
  its own bare clone on the `/data` PVC — a local `file://` fetch that carries
  **no credential at all** (`git.ts:353`). Work stops living only in the
  ephemeral runner clone.
- **A requeued run resumes precisely.** The claim carries the milestone list and
  the completed set, so an amnesiac lead is told "M1–M3 are committed, start at
  M4" instead of today's vague "the branch carries N commits".

Phasing is deliberate: **M1–M5 deliver the progress feature with no new security
surface** (including Slack, M4, and CLI, M5); M6–M7 add durability using no
credential; M8 (origin push at checkpoints) is deferred and gated on evidence.

## Design Decisions

**1. Milestones are a SET, not a cursor.** The obvious model — one
`milestone_index` integer — assumes strictly sequential completion. The lead
delegates to several subagents inside a single turn, so it can legitimately
finish M2 and M4 while M3 is still open; an index of 4 would then claim M3 is
done, and an index of 2 would hide M4. Store `completed` as a set of ids.

**2. Two states — CANDIDATE and FROZEN — not one.** The list the human approves
must be visible *before* they approve it (M3), so a candidate list rides the
`awaiting_approval` report alongside `plan_md`, and is **replaced** on each PRD
#41 revision round. It becomes FROZEN when the gate resolves `approve`, and from
then on it is immutable for the run: if the lead could rewrite it mid-flight, the
progress bar would lie and the gate would stop meaning anything. A milestone that
turns out wrong is a follow-up, not a silent edit.

The two states are not cosmetic — only the frozen list may drive the budget
(Decision 5) or accept progress reports, or a lead could inflate its own budget
by submitting a plan it never gets approved. **Autopilot needs its own path**: an
auto-approved run never reports `awaiting_approval` at all (the short-circuit at
`runner.ts:585` returns before that report), so on that path the frozen list
rides a `running` report instead — exactly as the autopilot agent selection
already does (`runner.ts:601-605`).

**3. `completed` is unioned server-side; `in_progress` is overwritten.** Union
makes the completed set monotone and idempotent, so a dropped or duplicated
report cannot regress the UI. `in_progress` is a snapshot with no such property,
so it is replaced wholesale on each report. Milestones never un-complete.

**4. Additive-optional on the wire, reported fire-and-forget.** A worker sending
an unknown field to an older api 400s under `DisallowUnknownFields` — which is
exactly why the repo-agent roster is reported with `void reportState(...).catch()`
(`runner.ts:275`). Milestone reporting takes the same treatment: an
informational field must never fail a run. No milestones reported ⇒ column NULL
⇒ the UI falls back to today's `iteration N` badge, unchanged.

**5. The budget becomes a total turn ceiling scaled by milestone count, not a
per-milestone counter.** Because milestones can progress in parallel inside one
turn (Decision 1), a turn cannot be cleanly attributed to a milestone, so
per-milestone counters would be guesswork. A total ceiling
(`max_iterations × milestone_count`, capped) is attribution-free and preserves
the existing semantic of `RUN_MAX_ITERATIONS` for a single-milestone plan. The
idle timeout (10m) is unchanged and remains the real stall detector.

**5b. The wall clock cannot be scaled worker-side — it needs a server change,
and that change is part of M2.** `RUN_TIMEOUT` is enforced by the **sweeper**,
not the worker: `SweepRunningTimeout` (`workersvc/service.go:2455`,
`store/queries/runtime.sql:571`) fails any `running` run whose `started_at` is
past the global cutoff, and `docs/configuration.md:175` states outright that the
value shipped in the claim is "for its own reference; the server's own sweeper is
the actual enforcement". A worker that scaled its own budget to 6h is still
failed at 2h. So M2 must **derive an effective per-run timeout at freeze, persist
it on the run row, and have `SweepRunningTimeout` honour it** — and the Run-health
"slow" clamp (`workersvc/health.go:396-406`, which clamps against the global
`RunTimeout`) must clamp per-run too, or every scaled run renders as slow for
most of its life. Missing this would leave M2 unable to meet its own verified
criterion while appearing to be a worker-only change.

**6. A checkpoint is a DURABILITY boundary, not a quality gate.** `checkpoint`
means *the lead claims this milestone is done* — precisely the trust level
`signal_done` already carries today (its description says "call this once the
work is committed locally and the reviewer is satisfied"; nothing verifies it).
The worker can cheaply check two things and must: **the branch tip moved since
the last checkpoint**, and **the working tree is clean**. It cannot check that
review happened or tests passed — worker-side test execution exists only for
`self_improve` runs (`runner.ts:374`, `runSelfImproveChecks`). The UI must be
worded so a green milestone does not imply verification.

**The two checks must not violate B2.** "Tree is clean" is tempting to implement
as a worker-uid `git status` in the runner clone — which reads a **runner-owned
config source**, the exact thing the (b) topology exists to prevent
(`git.ts:166-170`). Either run it through `runGitAsRunner`, or check tip
movement only, which is computable entirely in the worker's own bare.

**7. The checkpoint fetch-back carries NO credential.** *(2026-08-07: SHIPPED by PRD #218 — `fetchAgentBranch` is the current no-credential primitive and `fetchBackBestEffort` already fires it on park + SIGTERM. This decision is now the design record for a mechanism that exists; M6 reuses it rather than building it.)* `fetchAgentBranch`
(`git.ts:353`) is a local `file://` pack fetch from the runner clone into the
worker's bare, already hardened by six B2 invariants, and takes no PAT. It moves
the agent's commits from the ephemeral runner clone onto the `/data` PVC
(`controller/internal/kube/render.go:442`), which survives a pod kill and
re-attaches to the same worker. This is the entire durability mechanism for
M6 — no push, no CI trigger, no branch at origin, no new secret exposure.

**8. The reap is what makes a checkpoint safe, and it is why PRD #110's
conclusion does not apply here.** #110 closed because a mid-run push happens
*while the agent is alive*, and hosted k8s is single-uid (`runAsUser: 10001`, no
`CAP_SETUID`), so the agent could read the PAT from the push child's
`/proc/environ`. This design does not push mid-turn: at the checkpoint the lead
has ended its turn, the executor calls `killAgentTree()` — the same call the
end-of-run push already depends on (`runner.ts:345`) — and only then does any
git run. That restores the **temporal closure** #110 said a checkpoint could not
have. Stated honestly: the residual risk (a `setsid` double-fork escaping the
group kill) is **identical to today's end-of-run push**, not new. If
`killAgentTree` is trusted there, it is trusted here; if it is not, uzi has a
bug today. Note this argument is what unblocks M8 in principle — M6/M7 do not
need it at all, since they use no credential.

One honest qualifier, for M8 only: the risk *class* is identical, the *exposure
count* is not. Today a run has exactly one reap→push window; M8 would give it
one per milestone, with the agent re-spawned between them. The M8 security review
must weigh N windows, not one.

**9. The clone seed must prefer the checkpointed ref — under a rule that is
stated exactly, not by feel.** *(2026-08-07: SHIPPED by PRD #218. The strict-descendant rule below is implemented as `seededFrom` in `git.ts`, and the "open decision" in the third bullet — the tracking-ref lifecycle on terminal failure — was settled by #218's `uzi-trackowner.<branch>` run-identity anchor, which gates the tracking-ref read on the config stamp equalling `claim.run_id`. Treat the bullets below as the rationale for shipped behaviour; M6 must not re-derive them.)* `runnerCloneForBranch` bases a resume off
`refs/remotes/origin/<branch>` (`git.ts:291-293`). The checkpoint writes to
`refs/uzi-runner/<branch>` in the same bare. Without teaching the seed to prefer
that ref, the checkpoint saves work the next attempt then ignores — the
fetch-back would be a no-op feature. This is load-bearing, and "prefer it when it
is ahead" is too loose to implement safely:

- **The rule.** Compute the base as today (origin branch if present, else the
  default branch). Prefer the tracking ref **only when it is a strict descendant
  of that base** (`git merge-base --is-ancestor <base> <tracking>`). In the
  primary M6 case the branch was never pushed, so the comparison is against the
  default branch, not a nonexistent origin ref.
- **On divergence, origin wins, loudly.** If a human pushed to the branch after a
  checkpoint, the two have diverged and "ahead" is undefined. Take origin and
  emit a feed notice saying checkpointed work was set aside — never
  silently merge, never silently discard without saying so.
- **The ref needs a lifecycle.** Nothing in the codebase deletes
  `refs/uzi-runner/*` today (verified: `fetchAgentBranch` is the only writer, via
  a force refspec; `removeRunnerClone` does not touch refs). So a tracking ref
  from a **failed or abandoned** attempt persists in the bare indefinitely,
  invisible in every UI. Left unhandled, a later "fresh" run on the same branch
  would silently seed off abandoned work — and the `self_improve` fixed branch
  makes that a certainty across cycles rather than a corner case. After a
  successful push the tracking ref equals origin and is inert; the open decision
  is **keep or delete on terminal failure**, and it is a product decision (does an
  abandoned attempt's work deserve to be inherited?), not an implementation
  detail. M6 must settle it explicitly.

**10. The boundary is model-cooperative, and the iteration boundary stays the
fallback.** `checkpoint` ends a turn the same way `submit_plan` does today: the
model stops because the tool description tells it to, not because the tool
forces it (`signals.ts:45-57`). A lead that never checkpoints degrades to
exactly today's behavior. To avoid a silent regression in durability, the
executor also checkpoints at the ordinary iteration boundary.

**10b. Only the model-cooperative checkpoint reaps; the fallback does not.**
Today `killAgentTree` runs once, at the end of the run (`runner.ts:345`,
`sdk-executor.ts:543`), so a process the lead backgrounds during iteration 1 — a
dev server it means to test against in iteration 2 — survives across iterations.
Reaping at *every* iteration boundary would kill those, a real behavior change
the lead cannot see coming. It is also unnecessary: the fetch-back carries no
credential (Decision 7), so the reap there buys consistency, not security. So the
fallback checkpoint fetch-backs **without** reaping, and the reap happens only
where the lead declared a milestone complete and therefore expects a boundary.
(M8, which does carry a credential, must reap at every checkpoint including the
fallback — another reason it is a separate decision.)

**11. Milestone state rides the claim.** `ClaimResponse` carries the frozen list
and the completed set, so a requeued run's planning prompt can name what is
already committed. This is a strict improvement on issue #105's `priorWork`
note, which can only say "N commits".

**12. The api validates milestone data; a worker-side cap is not a control.**
Milestone titles are model output over untrusted issue text, reported by a worker,
and then rendered in the approval panel, the run checklist, and the CLI. The
repo's own rule applies verbatim — "the API does not take a worker's word for the
shape of what it persists and then renders in an approval panel"
(`workersvc/service.go:1510-1513`), with the PRD #108 rune caps
(`service.go:1026-1046`) and `stripNUL` as the working precedent. At `SetState`
the api must enforce: a **count cap**, a **per-title rune cap**, an **id-shape
check**, a **NUL strip**, and — critically — that every id in
`completed`/`in_progress` is a member of the **frozen list**. Without that last
one, union-on-write (Decision 3) makes a hostile or garbage id **permanent by
design**. This is the one genuine security surface Phase 1 adds, and it is
server-side or it does not exist.

**13. Only `issue` runs get milestones in this PRD.** The plan phase and
`scanSignals` are shared with `ci_fix` and `self_improve`
(`sdk-executor.ts`), so both would inherit milestone prompting for free — and
with it a self-scaled budget (Decision 5), which is not something a CI-fix run
should be able to grant itself. Neither kind is milestone-shaped: a `ci_fix`
diagnoses one failure, and a `self_improve` cycle picks one improvement. So the
milestone prompt is emitted for `kind === "issue"` only, and a milestone list
reported by any other kind is rejected server-side under Decision 12. Chat and
the judge are out by construction and need no guard: chat runs a separate
executor (and `SweepRunningTimeout` already excludes `kind='chat'`,
`runtime.sql:571`), and the judge denies every tool via a deny-all `PreToolUse`
hook, so it can never reach a signal tool.

## Touchpoints

- `agent/src/signals.ts` — `milestones` on `submit_plan`; new `checkpoint` tool; `scanSignals` extraction (main-thread-only guarantee unchanged)
- `agent/src/prompt.ts` — plan prompt asks for milestones; implement prompt names the current milestone; resume note uses the completed set
- `agent/src/sdk-executor.ts` — loop restructure, budget model, checkpoint handling, reap at boundary
- `agent/src/runner.ts` — checkpoint callback: reap → fetch-back → report; freeze list at gate approval
- `agent/src/git.ts` — seed base prefers `refs/uzi-runner/<branch>` when ahead; a `branchTip` helper for the moved-tip check
- `agent/src/protocol.ts` — `StateRequest` + `ClaimResponse` fields
- `api/internal/store/migrations/` — one migration (number assigned at merge, per CLAUDE.md)
- `api/internal/store/queries/runtime.sql` + sqlc regen — union-on-write for `completed`; **`SweepRunningTimeout` honours a per-run effective timeout** (Decision 5b)
- `api/internal/workersvc/service.go` + `claim.go` — accept/apply/serve the fields, **plus the Decision 12 validation at `SetState`**
- `api/internal/workersvc/health.go` — clamp the "slow" threshold against the per-run timeout, not the global one (Decision 5b)
- `api/internal/apitypes/run.go` — DTO
- `web/src/pages/RunView.tsx`, `Dashboard.tsx`, `RunsList.tsx` + tests; `web/src/mocks/{data,mockApi,engine}.ts`
  - *2026-08-07 coordination note: PRD #237 (live token counts, merged) also touches `RunView.tsx` (usage-panel region, ~`:691`) and `web/src/mocks/data.ts`. No semantic overlap with M3 (its `hasUsage → hasConfirmed` rename is confined to `runUsage.ts`/`RunUsage.tsx`, which M3 never touches; the milestone badge/checklist sits at the iteration-badge region ~`:515`, a different hunk). Write M3 against the post-#237 tree — textual merge only.*
- `api/internal/slacksvc/notifier.go` — root-line counter + thread line on count-advance (M4)
- `api/internal/store/queries/slack.sql` (+ sqlc regen) — extend `GetSlackRunContext` with the milestone columns; new "last-notified count" column on `slack_run_messages` + a generation-guarded setter modelled on `SetSlackRunGateGen` (M4)
- one migration for that `slack_run_messages` column (M4; folds into M1's migration if landed together)
- `api/cmd/uzi/` — CLI parity (repo convention: a run-DTO change must not update only the web)
- `docs/configuration.md` — budget semantics; `specs/ai.md` — append-only decision record

## Milestones

**Phase 1 — the progress feature (no new security surface).**

- [x] **M1 — Milestone list on the plan (agent + wire + store, no UI)**:
      `submit_plan` gains optional `milestones`; `scanSignals` extracts them
      (main-thread-only, as today); candidate rides `awaiting_approval` and is
      frozen at approve, with the autopilot path on a `running` report (Decision
      2); migration + sqlc + `StateRequest`/DTO plumbing; **the Decision 12
      server-side validation lands with the first write, not later**; milestone
      prompting is gated to `kind === "issue"` (Decision 13). Reporting is
      fire-and-forget (Decision 4). **Verified**: a run whose lead submits
      milestones stores exactly the approved list; a run whose lead submits none
      stores NULL and behaves exactly as today; an over-cap, mis-shaped, or
      non-member id is rejected by the api, not merely by the worker.
- [x] **M2 — Progress reporting + budget resize (executor AND server)**: the
      implement loop reports `completed` (unioned) and `in_progress`
      (overwritten) at each boundary; the iteration cap becomes a total turn
      ceiling scaled by milestone count (Decision 5); **the effective wall clock
      is derived at freeze, persisted, and honoured by `SweepRunningTimeout`,
      with the run-health "slow" clamp following it** (Decision 5b — without this
      the milestone is a no-op, since the sweeper is the real enforcement);
      `REASON_MAX_ITERATIONS` copy names the new semantics.
      `docs/configuration.md` updated in the same MR. **Verified**: a
      seven-milestone plan no longer trips the cap at turn 5 **and is not swept
      to failed at the global 2h**, a single-milestone plan's budget is unchanged
      from today, and a scaled run does not render as "slow" for its whole life.
- [ ] **M3 — Web progress UI**: `RunView` renders a milestone checklist (done /
      in progress / left) with copy that does not imply verification (Decision
      6); `Dashboard` shows a compact `M3/7` badge alongside its existing
      iteration badge and `RunsList` gains one (it has none today); the plan gate
      renders the CANDIDATE list (Decision 2) so the
      human approves what they are approving. Mocks + tests updated. NULL
      milestones render today's badge. **Verified**: progress updates live over
      the existing run stream with no new endpoint.
- [ ] **M4 — Slack milestone progress**: a run that is linked to Slack (PRD #25)
      shows its milestone progress there, in the surface the owner actually
      watches. In the notifier (`api/internal/slacksvc/notifier.go`, inside the
      existing run-state `handle`, alongside `handleGate`): the root status line
      gains a compact counter (`▶ running · 3/7`, the same in-place edit
      `statusLabel` already re-renders on every state event), and a threaded line
      is posted when the completed count advances (`✓ 3/7 · working M5`).
      **Scope beyond notifier.go** (this is more than two edits): `GetSlackRunContext`
      (`slack.sql:97`) must be extended to select the milestone columns (it selects
      none today) + sqlc regen; and dedup needs a NEW append-only column on
      `slack_run_messages` (a "last-notified completed count") plus a
      generation-guarded setter — **modelled on** `SetSlackRunGateGen`
      (`slack.sql:146`) but distinct from it: `gate_generation` is the PLAN gate's
      own counter, actively read/written by `handleGate`, and cannot be reused.
      Without the new column a redelivered `running` event re-posts the line. The
      milestone label is rendered through `EscapeMrkdwn` + `ScrubSecrets` like
      every other field (`renderRoot`), so no new untrusted surface reaches Slack.
      Wiring note, corrected: `PublishState`/`notify` already fire on **every**
      applied `SetState` including each `running` report (`service.go:1549-1553`),
      so `handle` runs on every report and reads fresh milestone data — the block
      just needs its own dedup on the completed count, not off status. Best-effort
      and self-degrading: an unlinked or opted-out user gets nothing and the run
      is unaffected (`handle` drops silently on `ErrNoRows`, `notifier.go:285`).
      **Verified**: a linked run reflects each completion in its thread exactly
      once and never re-broadcasts on a repeated `running` report; an unlinked run
      behaves exactly as today.
- [ ] **M5 — CLI parity**: `uzi` run show/list surface the same milestone
      progress as the web, per the repo's "new functionality ⇒ check the CLI"
      convention. **Verified**: `uzi run show <id> --json` carries the milestone
      fields and the human output shows the same state the web does.

**Phase 2 — durability (still no credential). RE-SCOPED 2026-08-07 against PRD #218 — read the banner at the top and `prds/done/218-park-resume-work-loss.md` before starting. #218 shipped the tracking-ref + fetch-back + strict-descendant-reseed foundation this phase was originally built around; only the delta below remains, and it is optional (Phase 1 is the feature).**

- [ ] **M6 — Proactive milestone-boundary checkpoint (delta over #218)**: new
      `checkpoint` signal tool; on it the executor ends the turn, the runner calls
      `killAgentTree()`, the worker rejects a no-op checkpoint (tip unmoved / tree
      dirty, Decision 6) and otherwise runs #218's existing `fetchAgentBranch`
      into the bare on the PVC (Decisions 7, 10, 10b). **This is a NEW trigger for
      an existing mechanism, not the mechanism itself** — do not re-build the
      tracking ref, the `seededFrom` reseed rule, the `uzi-trackowner` run-identity
      anchor, or the park/SIGTERM fetch-back; all shipped in #218. The value M6
      adds over #218: #218 fetches back only on park + graceful SIGTERM, so a hard
      **SIGKILL** (OOM, node kill with no grace window) still loses everything
      since the last fetch-back; a proactive fetch-back *while the run is alive* at
      each milestone bounds that loss to "since the last milestone". **Verified**:
      with #218 already in the tree, `SIGKILL`ing a worker mid-run **between two
      milestones** and letting the same worker re-claim resumes with the
      already-checkpointed milestones present (the pre-M6 baseline loses them,
      since the last fetch-back was the previous park/shutdown, which may never
      have happened). **Honest bound (unchanged from #218's R1)**: this holds when
      the same worker re-claims within `WORKER_AFFINITY_GRACE` (2m); a docker-
      capable pod that must re-pull and reseed nix (~2.6 GiB; PRD #113 wedged one
      for 14 minutes) can lose the affinity and a different worker takes the run
      onto a different PVC where the checkpoint is invisible. Cross-worker recovery
      is M8 (origin push), still deferred.
- [ ] ~~**M7 — Resume precision**~~ — **DELIVERED by PRD #218 (M2/M3), dropped
      2026-08-07.** #218 already carries the recovered-commit count into the resume
      (`priorWork` widened) and states it in the feed either way. The only piece
      this PRD adds on top is naming progress by **milestone** rather than by
      commit count, which is a trivial rider on M6 once the completed set rides the
      claim (Decision 11) — folded into M6, not a milestone of its own.

**Phase 3 — deferred, gated on evidence.**

- [ ] **M8 — Origin push at checkpoints (DEFERRED)**: push the branch to origin
      at each checkpoint so work survives a **different** worker re-claiming the
      run. Blocked on: (a) the `requeue_count` measurement in Risks showing this
      actually happens, (b) an explicit security review of Decision 8, (c) a
      decision on CI-trigger suppression (`-o ci.skip`) so N checkpoints do not
      fire N pipelines. **Not** to be started with M6.

## Success Criteria

- Watching a run answers "what is done, what is in progress, what is left"
  without reading the message feed.
- A seven-milestone plan runs to completion instead of failing at
  `REASON_MAX_ITERATIONS`, and a one-milestone plan's budget behaves exactly as
  it does today.
- A run whose lead submits no milestones is byte-for-byte unchanged in behavior
  and renders today's `iteration N` badge.
- A scaled run is not swept to failed at the global `RUN_TIMEOUT`, and is not
  flagged "slow" for the whole of its extended life.
- The api rejects an over-cap, over-long, mis-shaped, or non-member milestone id
  regardless of what the worker sends — verified by test, not by the worker's
  own cap.
- Milestone progress never regresses in the UI, even when a state report is lost
  or duplicated.
- The web and `uzi` CLI show the same milestone state.
- A run linked to Slack reflects each completion in its thread exactly once and
  never re-broadcasts on a repeated `running` report; when two milestones
  complete in one turn the count advances correctly (a `+2` jump is not lost),
  and an unlinked run behaves exactly as today. Note the dedup is on the completed
  COUNT, so the guarantee is "every advance is shown once, none missed", not "one
  separate line per id" — the count-based rendering (`3/7`) makes that honest.
- After M6: a worker pod killed mid-run, re-claimed by the same worker, resumes
  with the checkpointed commits present rather than re-doing them.
- After M6: **no new credential is exposed** — the checkpoint path passes no PAT
  to any subprocess, and the run still pushes exactly once, at the end.
- The plan gate shows the milestone breakdown, so the human approves the
  decomposition and not just the prose.

## Out of Scope

- **Machine-verified milestone completion** (running the repo's tests per
  milestone before marking it done). That means executing agent-authored code as
  the worker uid under the scrubbed check env — the PRD #46 M9 machinery — for
  every run, not just `self_improve`. Its own design, not a rider on this one.
- **One run per milestone** (decomposing a PRD issue into several runs on the
  same branch). The branch plumbing already supports it, but it changes the
  product model and the gate cadence.
- **Milestone editing after approval** (Decision 2).
- **Parallel milestone execution across worktrees or workers.** "Parallel"
  here means several subagents inside one turn on one working tree; nothing in
  this PRD creates a second checkout, a second branch, or a merge.
- **Any change to the end-of-run push + MR path.**
- **Editing PRD or issue markdown as milestones progress.** These milestones are
  the lead's own implementation-plan breakdown for the forge issue it is working,
  reported over the API wire and stored on the `runs` row — they are NOT the
  `- [ ]` checkboxes in a `prds/*.md` file. uzi never writes a PRD file to track
  its own progress; the run's milestone state lives on the run and is shown in
  the web/CLI/Slack. (If a lead edits a PRD file, it is because that edit is part
  of the issue's actual work — an ordinary commit on the branch, gated by the
  same review and MR as any other change, not a progress-tracking side effect.)

## Risks

- **Breadth, not depth.** Seven touchpoints across three languages must land
  together or the feature half-works. Each individually follows a working
  precedent (`iteration_count` is the exact template for the wire, PRD #41 for
  the gate-loop change), but the coordination is the real cost. Mitigation: M1–M5
  are individually shippable and each is useful alone.
- **The budget change touches a safety mechanism.** `RUN_MAX_ITERATIONS` and the
  wall clock exist to bound cost and stop runaway runs. Scaling them by a
  model-supplied milestone count means a lead that emits 40 milestones buys
  itself a 40× budget. Mitigation: hard ceiling on the derived budget,
  independent of milestone count; the milestone-count cap is enforced
  **server-side** (Decision 12), since a worker-side cap is not a control against
  a compromised worker; only the FROZEN list scales anything (Decision 2); the
  idle timeout is unchanged.
- **A progress UI that overstates certainty.** "M4 ✓" is a self-report
  (Decision 6). Mitigation: the two cheap machine checks, plus copy that says
  "reported complete", plus the existing MR/CI as the real verification.
- **M6's benefit must be measured, but against the right population.** An
  earlier draft of this PRD (following #110) proposed gating durability work on
  how many requeues landed on a *different* worker. That gate is wrong: as
  Problem §3 now records, a **same-worker** requeue destroys the attempt's
  commits too, so the cross-worker filter would exclude precisely the events M6
  fixes. **Measure instead**: `runs.requeue_count > 0` (the column exists,
  migration `00020`; exposed on the DTO at `apitypes/run.go:32`) restricted to
  runs interrupted after the plan gate — i.e. those that had reached the
  implement phase and had commits to lose. If *that* is near zero, Phase 1 (the
  progress feature, M1–M5) is the whole feature and Phase 2–3 should be dropped.
- **Prompt compliance.** The lead must both emit a sensible milestone list and
  call `checkpoint` at the right time. Mitigation: both degrade to today's
  behavior when absent (Decisions 4 and 10) — the feature fails off, never
  broken.

## Dependencies

- None blocking. PRD #41's gate loop must keep working across the freeze point
  (Decision 2); the #105 resume path is improved, not replaced.
- M8 additionally depends on the Decision 8 security review and, if that review
  goes the other way, on the k8s two-container uid split that PRD #110 names as
  its revisit condition.

## Validation

- Agent unit tests (`agent/test/`) for: milestone extraction from `submit_plan`
  including a subagent frame (must NOT latch), freeze-at-approval across a
  revision round, union semantics, the derived budget, checkpoint rejection on
  an unmoved tip, and the fetch-back call ordering (reap strictly before any
  git).
- Go tests for the store's union-on-write and the claim payload; a golden wire
  test for the new DTO fields, matching the existing `wire_test.go` pattern.
- Go tests for the Decision 12 validation as a **ceiling**, not a smoke test: an
  over-cap list, an over-long title, a NUL, a mis-shaped id, and a `completed` id
  that is not a member of the frozen list must each be rejected at `SetState`.
- A sweeper test proving a run with a scaled effective timeout survives past the
  global `RUN_TIMEOUT` and is still failed at its own (Decision 5b), plus a
  run-health test that such a run is not flagged slow for its whole life.
- A seed test per Decision 9: tracking ref a strict descendant ⇒ preferred;
  diverged from origin ⇒ origin wins and the notice is emitted; stale ref from an
  abandoned attempt behaves as the settled lifecycle says.
- Web tests for the three surfaces plus the NULL-milestone fallback.
- Slack notifier tests (matching `notifier_notify_test.go`): a count-advancing
  report posts one thread line; a repeated `running` report with an unchanged
  completed count posts nothing (dedup on the new column); a `+2` advance in one
  turn posts one line and does not lose the jump; an unlinked/opted-out user gets
  nothing and the run is unaffected.
- Live-DB tests where the union semantics are the point, run per the CLAUDE.md
  live-DB rules (`./e2e/run-store-it.sh`, positive control required — a `PASS=0`
  sweep is not evidence).
- e2e (`./e2e/run-e2e.sh`) with the stub executor extended to submit milestones
  and checkpoint, so the whole wire is exercised without a live SDK.
- Manual: on dev-cluster (the primary runtime), run a multi-milestone PRD issue
  and kill the worker pod mid-run to confirm the M6 claim.

## Decision Log

- **2026-07-24 — PRD created.** Grew out of a design discussion that started as
  "can we commit after each PRD milestone as a checkpoint?" and moved through
  three reframings: (a) PRD #110 had already closed exactly that idea, on
  single-uid k8s PAT-exposure grounds; (b) reaping the agent tree at the
  boundary restores the temporal closure #110 relied on, which reopens it
  (Decision 8); (c) once milestones exist as data, the **progress UI turned out
  to be the stronger motivation than the durability it started as** — the
  durability then comes nearly free, and in a form that needs no credential at
  all (Decision 7). The phasing reflects that reordering: value first, durability
  second, credentialed push deferred.
- **2026-07-24 — the cursor model was proposed and rejected within the same
  discussion.** A single `milestone_index` was the first design; the owner asked
  what happens when the lead works several milestones at once, which showed the
  cursor cannot represent out-of-order completion. Set semantics (Decision 1) and
  the attribution-free budget (Decision 5) both follow from that one question.
- **2026-07-24 — PRD #110's record needs TWO corrections, not one.** Its central
  finding (a mid-run push *without* reaping is unsafe on single-uid k8s) remains
  true and is not contradicted here. But (a) its "When to revisit" section names
  the k8s two-container uid split as the only revisit condition, and
  reap-then-push is a second one; and (b) its "Why the loss it would prevent is
  tolerable" section says a same-worker requeue inside the affinity grace means
  "no loss", which conflates the resumed session with the destroyed commits — see
  Problem §3. Both corrections should be made to #110 in the same MR that lands
  M1, with a pointer to this PRD. *(2026-08-07: partly overtaken — PRD #218's R2
  already engaged #110's push-during-run argument at park time and made the same
  temporal-closure point, so the #110 correction is now a reconcile-three-records
  task (#110 ↔ #122 ↔ #218), not a fresh edit. #110 is also still an open
  `prds/*.md` file rather than archived; whoever lands Phase 1 should reconcile
  all three so the durability records stop contradicting each other.)*
- **2026-07-24 — fable adversarial review, verified against `main`.** Every
  cited `file:line` in this PRD was opened and checked; all held. The review's
  substantive findings are folded in above rather than listed here, but four
  changed the document materially: the wall-clock scaling needs a **server**
  change nobody had scoped, because the sweeper — not the worker — is the
  enforcement (Decision 5b, M2 rescoped); the loss model in Problem §3 was
  inherited from #110 and was **wrong**, which makes M6 more valuable and the
  original measurement gate a filter on the wrong population (Risks rewritten);
  Decision 9's "prefer when ahead" was too loose to implement and had no ref
  lifecycle at all; and Phase 1's one real security surface — server-side
  validation of worker-reported milestone data — was unstated (Decision 12).
  Decisions 2, 6, 8, 10b and 13 gained the paragraph each was missing.
- **2026-07-24 — Slack milestone progress added as M4 (non-optional), at the
  owner's direction.** It rides M1–M3's data, lands in Phase 1 (no new security
  surface), and reuses the existing per-run Slack message plus the gate's
  generation-guard dedup pattern. The former CLI milestone shifted M4→M5, and the
  durability/push milestones shifted M5→M6, M6→M7, M7→M8. A scope boundary was
  recorded at the same time (Out of Scope): run-plan milestones are not
  `prds/*.md` checkboxes, and uzi does not edit PRD files to track progress.
- **2026-07-24 — opus re-review of the M4 insertion, verified against code.**
  Renumbering passed clean (every impl ref shifted, every illustrative ref left
  alone, list gapless). Two under-scopings were corrected in M4: it is more than
  two edits to `notifier.go` — `GetSlackRunContext` selects no milestone column
  today and dedup needs a NEW `slack_run_messages` column (the gate's
  `gate_generation` is plan-specific and cannot be reused, only mirrored); and
  "each completion once and only once" was false under parallel completion, since
  a count-based dedup coalesces a `+2` jump — resolved by choosing count-based
  rendering (`3/7`) and softening the criterion. Also corrected: `PublishState`
  fires on every `running` report (`service.go:1549`), so the notifier wiring is
  smaller than first written, not larger.
- **2026-08-07 — Phase 2 re-scoped against PRD #218 (which shipped after this PRD
  was drafted); Phase 1 unchanged.** #218 built the durability primitive Phase 2
  was designed around — the `refs/uzi-runner/<branch>` tracking ref, the
  credential-free `fetchAgentBranch` fetch-back on park + graceful SIGTERM
  (`fetchBackBestEffort` in `agent/src/runner.ts`), the strict-descendant reseed
  rule (`seededFrom` in `git.ts`, exactly Decision 9's rule), AND a run-identity
  anchor (`uzi-trackowner.<branch>`) that settled Decision 9's one open question,
  the stale-ref lifecycle. Verified against HEAD, not from the review summary:
  the symbols are present in `agent/src/git.ts` and `agent/src/runner.ts`.
  Changes made here: banner added at top and #218/#216/#217/#224/#222 added to
  Related; Decisions 7 and 9 marked SHIPPED-BY-#218 (now design records for
  existing code, not build tasks); M7 dropped as delivered by #218 M2/M3; M6
  narrowed to the one residual gap — a *proactive* milestone-boundary fetch-back
  that also covers a hard SIGKILL, reusing #218's mechanism rather than
  re-deriving it; Problem §3 annotated as half-mitigated; the #110-corrections
  action item flagged as partly overtaken by #218's R2. **Phase 2 file:line
  citations are stale** (pre-#218 tree) and one Phase-1 citation drifted (the
  iteration badge is now `RunView.tsx:515-517`); both flagged inline for
  re-derivation at implementation time. The open Phase-1 decision remains the
  budget **hard ceiling** — still unnumbered, a maintainer call before M2. This
  is a documentation re-scope only; no code changed.
- **2026-08-07 — M1 landed (MR !194, merge `3d0720ee`), and the budget hard
  ceiling was decided.** M1 shipped via a seeded uzi run (`bc631d9c`) as
  `feat(milestones): M1 …` (`52598983`): `submit_plan.milestones` (schema-gated to
  issue runs, Decision 13), main-thread-only extraction, the two-column store
  (`00098_run_milestones.sql`: `milestones_candidate` / `milestones_frozen` jsonb,
  `NULL ≠ '[]'`), the DTO field, and the Decision 12 server-side validation. Notes
  for whoever picks up M2, none of which change the design but all of which the M2
  plan should assume rather than rediscover:
  - **The milestone type lives in `apitypes` (`Milestone{ID,Title}`) with a
    `workersvc.Milestone` type ALIAS** (`api/internal/workersvc/milestones.go`),
    mirroring `RepoAgent`/`AgentSelection`, so `apitypes` stays the stdlib-only
    leaf the CLI links (PRD #64 M1). M2 reads the frozen list from there.
  - **The Decision 12 count cap landed at 50** (`maxMilestonesPerRun`), a
    whole-list reject (not a clamp), with a strict ASCII id-shape regex
    `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`, dup-id reject, and a 200-rune title cap
    through `hasUnsafeChar` + `stripNUL`. This is the storage/hygiene cap and is
    **independent of the budget-scaling cap below** — a run may legitimately store
    up to 50 milestones while M2 scales the budget on only the first N of them.
  - **The budget hard ceiling (the open Phase-1 decision above) is now set:
    profile "Generous" — milestone-count cap 12, total turn ceiling
    `RUN_MAX_ITERATIONS × min(n, 12)` (so ≤ 60 turns), absolute wall-clock ceiling
    8h.** A single-milestone plan stays exactly at today's 5 turns / 2h; the 10m
    idle timeout remains the real stall detector. M2 enforces the count cap
    **server-side** (Decision 12's chokepoint), not worker-side, since a
    worker-side cap is not a control. Decision 5 (the scaling formula) and Decision
    5b (the persisted per-run effective timeout that `SweepRunningTimeout` must
    honour, plus the run-health "slow" clamp) are M2's load-bearing halves — the
    server change is what makes the wall-clock scale real, since the sweeper, not
    the worker, is the enforcement.
- **2026-08-07 — M2 landed (MR !195), Phase 1 backend complete.** M2 shipped via a
  seeded uzi run (`1de50700`) with migration `00099_run_milestone_progress.sql`.
  Both halves landed and were verified against the merged diff:
  - **Budget resize, server-authoritative.** `api/internal/workersvc/budget.go`
    pins the Generous ceiling — `milestoneBudgetCap = 12`,
    `budgetWallCeilingSeconds = 8h` — and the freeze SQL derives
    `budget_max_iterations = run_max_iterations × LEAST(frozen_count, 12)` and
    `budget_wall_seconds = LEAST(run_timeout × LEAST(frozen_count, 12), 8h)`,
    written idempotently via `COALESCE` at BOTH the human-approve
    (`CreateApprovePlanInput`) and autopilot (`SetRunRunning`) freeze paths, and
    NULL for a 0/1-milestone run so single-milestone budgets are byte-for-byte
    today's (5 turns / 2h). The cap is applied in the freeze SQL, not worker-side.
  - **Decision 5b done correctly.** `SweepRunningTimeout` now compares
    `started_at` against a PER-RUN cutoff
    (`now - make_interval(secs => COALESCE(budget_wall_seconds, global_timeout))`),
    and `budget_wall_seconds` rides the running-run health read so the "slow" clamp
    follows the effective timeout — a scaled run is not swept at 2h and is not
    flagged slow for its whole life. The 8h ceiling is enforced at the WRITE
    (freeze) path, with a source comment warning future writers to keep it there.
  - **Progress: Decision 3.** `milestones_completed` is UNIONED (dedup via
    `jsonb_agg(DISTINCT …)`), `milestones_in_progress` is OVERWRITTEN, a NULL param
    leaves the column untouched, and every reported id is membership-checked against
    the frozen list server-side before the write (Decision 12's M2 half). Live-DB
    integration test (`run_milestone_progress_integration_test.go`, +337) covers it.
  - **Still no UI** (M3/M4/M5 unbuilt). The remaining Phase-1 milestones were
    deliberately NOT seeded — the maintainer redirected to the M8 work below.
- **2026-08-07 — M8 (per-checkpoint origin publish) reopened and re-designed to a
  SERVER-SIDE PUSH BROKER (design agreed; PRD Decision 8/14 + M8 rewrite pending).**
  At the maintainer's direction ("I want per-checkpoint publish, as secure as
  possible"), a three-agent code-grounded security review (researcher + auditor +
  architect) reached a unanimous recommendation. The auditor **demonstrated** that
  reap-then-push does NOT close the vector on single-uid k8s: `killAgentTree`
  process-group-kills only the SDK CLI pid, so a `setsid`/new-session child escapes
  the reap and can read the push child's `/proc/environ` (agent uid == worker uid
  == 10001). Chosen design: the worker ships its credential-free pack
  (`origin/<b>..refs/uzi-runner/<b>`, already in its bare from #218's
  `fetchAgentBranch`) to a new authorization-scoped
  `POST /api/worker/runs/{id}/publish`, and the api pushes with the PAT it already
  decrypts — no PAT-bearing git child ever runs under the agent-reachable uid
  (spatial closure, strictly stronger than reap-then-push's temporal closure).
  Rejected: Infisical agent-vault (needs a separate-pod/egress isolation single-uid
  k8s can't give, and moves push capability to the agent) and scoped tokens (the
  Developer bot can't mint them). **go-git spike (2026-08-07): `PushOptions.Options
  []string` in the go-git source confirms the client can send `ci.skip`**, so the
  api push client stays pure-Go and the api image stays distroless-static — no git
  binary needed. Fallback if a real-forge push proves fragile: Option A (worker
  reap-then-push hardened with `hidepid=2` + a cgroup reap that catches the setsid
  escape). ADR `adr/0122-checkpoint-push-broker.md` to be authored on approval of
  the Decision 8/14 rewrite.
