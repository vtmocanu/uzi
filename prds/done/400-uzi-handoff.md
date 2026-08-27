# PRD #400: uzi handoff (ephemeral branch-scoped task runs)

**Issue**: [#400](https://github.com/vtmocanu/uzi/issues/400)
**Status**: Complete (2026-08-19) — all milestones landed on `agent/issue-400`.
**Priority**: Medium

## Problem

uzi today has effectively one way in for repo work: a forge issue, worked through the full PRD flow (plan gate, implement/review, an MR, the issue as the durable record). That ceremony is right for product work, but it is the wrong shape for the thing developers do constantly with a local agent: hand off a long-running, throwaway task. "Take this, do it, I will pull the result." No issue to file, no PRD to write, no MR to review, no forge record to clean up afterward.

The result is that uzi cannot be used as a personal async agent runner, which is a large fraction of how its own maintainers already use coding agents (this PRD was produced in a session that hand-orchestrated a coder and a reviewer on a throwaway branch, then pulled the result: that loop is what this feature productizes).

## Concept: a rented remote worktree

The mental model is a **remote worktree you rent for a task**. You push it some work, watch it, pull the result, and throw the branch away. It is the sibling of the existing `prompt` run kind (repo-ful, issue-less) but produces **commits on a branch you pull, not an MR**.

This splits uzi into two coherent modes:

- **PRD flow** (existing): durable, issue-tracked, MR-gated. Product-grade.
- **Handoff flow** (this PRD): ephemeral, CLI-native, MR-less, branch-only. The dev loop.

## Design decisions (Decision Log)

1. **New run kind `task`, the seventh.** `runs.kind` already holds six values (`issue`, `ci_fix`, `chat`, `judge`, `self_improve`, `prompt` — see `api/internal/store/migrations/00104_run_schedule_provenance_prompt_kind.sql`); `task` is the seventh. (The doc comment at `api/internal/apitypes/run.go` still lists only `issue|ci_fix|chat` and is stale; M8 fixes it.)
2. **Model `task` on the `prompt` kind, not `chat`.** `prompt` (PRD #241) is the near-exact template: **repo-ful and issue-less**, carrying its inline instruction in the **existing `issue_description` column**, created by `CreatePromptRun` (`api/internal/workersvc/prompt.go`), which is deliberately NOT `createRun` and is shaped like `CreateSelfImproveRun`. So a task's inline context reuses `issue_description` (no new context column), inheriting its 256 KiB cap (`MaxIssueDescriptionBytes`) and sanitization for free. `chat` is the wrong model (repo-less, branch-less); `plan_md` is a seeded *issue* run, not issue-less.
3. **CLI verb `uzi handoff`** (alias `uzi task`). Chosen over `send`/`dispatch`/`job`/`order` because it matches the "hand it off like an agent" mental model and does not collide (verified: the taken top-level verbs are `login/logout/auth/whoami/run/schedule/tui/review/findings/worker/token/memory/repo/admin/skill/version`; `handoff` and `task` are both free).
4. **The task branch is server-named `uzi/task/<run-id>`, always.** The destination is never caller-controlled in v1, which makes it **safe by construction** (a server-named branch in the `uzi/task/*` namespace can never be a protected/default branch) and keeps the prune/namespace invariant clean. The user's local branch is the **source content** (see Decision 6), not the destination name. Arbitrary/user-named destinations are deferred to Later.
5. **No MR by default; `--mr` opts one in.** The deliverable is commits on `uzi/task/<run-id>`, which the user pulls. MR-open is a discrete step already skipped by `chat`/`judge` runs, so "do not open an MR" is reuse, not new code. `--mr` is the **escalation bridge**: a throwaway task that turns out to be keeper work opens an MR without redoing anything, and an MR-opened branch is **exempt from cleanup** (an MR needs its source branch).
6. **Branch-infer source + explicit create/push ordering.** `uzi handoff -m "..."` run inside a checkout uses the current branch's **HEAD as the source content**. Because `<run-id>` is server-assigned, the ordering is explicit and non-circular: (a) the CLI creates the task run and receives its id and the server-assigned `uzi/task/<run-id>` name; (b) the CLI pushes local HEAD to `uzi/task/<run-id>` using the **user's own credentials**; (c) the worker then claims the run, clones that branch, works, and pushes its commits back to the same branch. `--base <ref>` overrides the source (branch from a named base instead of local HEAD). Note: today `runs.branch` is written only at completion and derived server-side from the issue iid (`api/internal/workersvc/service.go`); a task needs the branch known **at create time**, a deliberate deviation M1 must implement.
7. **Two writers, coordinated by ordering.** The CLI push (step 6b, user creds) is a one-time seed **before** the worker starts; after that the worker is the sole writer (it holds the PAT and does all network git — `agent/src/git.ts`). Continuation is `uzi handoff -m "more"` / `uzi run follow-up <id>`, which feeds more context to the worker (the worker pushes more commits); a user re-pushing local commits to a live task branch mid-run is out of scope for v1 (the worker push is non-forced, so a divergent user push would be rejected non-fast-forward — fail-closed and safe, documented in M8).
8. **The `main`-never-touched guardrail is unchanged, and its attribution is corrected.** The SDK `PreToolUse` deny-hook (`agent/src/guardrails.ts`) denies **every** agent `git push` and is **not in the worker's PAT push path**, so it does not distinguish `uzi/task/*` from `main` and is not what keeps a task off a protected branch. The layers that actually apply here are: (a) Decision 4's server-named branch (structural safety); (b) a cheap create-time assertion that the resolved branch is in `uzi/task/*` and is not the repo's default branch, reusing the existing `guardDefaultBranch` posture check (`api/internal/workersvc/composite.go`) and, if a general check is wanted, `DefaultBranchProtection(ctx, projectID, branch, botUserID)` (`api/internal/forge/forge.go`) which already takes an arbitrary branch; and (c) the forge's own rejection of a push to a protected ref, which the worker already relies on (`agent/src/runner.ts`), backed by the push being non-forced (no history rewrite possible). Force/history-rewrite denial, credential-read denial, and `settingSources: []` are untouched.
9. **Cleanup is client-side in v1.** No `DeleteBranch` primitive exists in the `Forge` interface or any driver, and the sweeper does run-lifecycle work only, so server-side auto-prune is genuinely new capability and is deferred to Later. v1 ships `uzi handoff rm <id>`, which deletes the remote `uzi/task/<id>` branch with the **user's own credentials** (`git push origin --delete`), needing no server change. An `--mr` branch is left alone.
10. **`--review` is a new diff-review run, reusing only the judge's output plumbing.** The judge reviews a run's **trace** and does no clone/git (`agent/src/judge-runner.ts`, `docs/judge.md`), so it cannot produce file-anchored diff findings. `--review` is a fresh review run that clones `uzi/task/<id>`, diffs it against its base, runs a reviewer-style agent, and emits structured findings; only the **persist-as-JSON + fetch-via-CLI** plumbing is reused from the judge. Findings are fetched via the CLI as JSON and are **never committed to the branch**. Schema: file + symbol + line + severity + summary + rationale. (The existing `report_finding`/`IncidentalFindingDTO` surface is deliberately symbol-anchored with no line number because *cross-run dedup* would drift on line numbers; that rationale does not apply to a single-diff review, so including `line` here is intentional, with `symbol` also carried for stability.) `--then-fix` (Decision/M5) chains a follow-on task seeded with these findings, precedented by `api/internal/selfimprove/engine.go` consuming judge recommendations into an auto-approved run.

## Scope: v1 vs later

**v1 (this PRD):** the `task` run kind end to end (`CreateTaskRun`, server-named `uzi/task/<id>`, worker execution, no-MR default + `--mr`); `uzi handoff` (branch-infer source + create/push/dispatch ordering, `-m`/`-f`/stdin, `--base`, `--mr`, continuation via follow-up, watch via the existing `uzi run get/logs --follow` and `uzi tui`); `uzi handoff rm` (client-side delete); `--review` (diff-review returning JSON); `--then-fix` (chained fix).

**Later (out of scope, design leaves room):** user-named/arbitrary destination branches (with the load-bearing protected-branch refusal that implies); server-side auto-prune (needs a new `Forge.DeleteBranch` across 3 drivers + 6 test fakes + sweeper wiring); review of an arbitrary branch not authored by uzi; fan-out and compare (N branches, pull the winner); recurring handoff via the existing `schedule` machinery.

## Technical scope (all resolvable inside the clone; no open web)

Every fact below is readable in the cloned repository; this feature adds no external dependency. Extend these existing seams:

- **Run kind + data model** (`api/internal/store`, `api/internal/apitypes`): add `task` to **both** the `runs_kind_check` and the `runs_kind_shape` CHECK constraints (every kind-add migration drops and re-adds both — see `00058`, `00104`). `task`'s shape is novel: `kind='task' AND repo_id IS NOT NULL AND issue_iid IS NULL AND branch IS NOT NULL`. Reuse `issue_description` for the inline context; add `base_branch` and `open_mr` as the only new columns; ensure `branch` is settable at create. Number assigned at merge time per the repo convention; strict goose, so land above the live head.
- **Run creation** (`api/internal/workersvc`): add a `CreateTaskRun` modeled on `CreatePromptRun` (issue-less, repo-ful, bypasses `createRun`), calling `guardDefaultBranch` and asserting the server-named branch is in `uzi/task/*`. There is **no** single "shared seam" to extend — `run create` mandates an issue and the issue-less creators (`CreatePromptRun`/`CreateChatRun`/`CreateSelfImproveRun`) each have their own method.
- **Worker execution** (`agent/`): on a task run, clone `uzi/task/<id>` (already seeded by the CLI push), run the agent over the inline context, commit, and push back with the existing non-forced `pushBranch` (`agent/src/git.ts`); skip MR-open unless `open_mr`, exactly as `chat`/`judge` already skip it (`agent/src/runner.ts`). No second credential path.
- **CLI** (`api/cmd/uzi/`): a `uzi handoff` cobra command (alias `task`) implementing the Decision-6 ordering, reading context from `-m`/`-f`/stdin, pushing local HEAD via the forge detected from `git remote get-url origin`; plus `uzi handoff rm <id>` (client-side `git push --delete`). Follows the CLI's `--json`/exit-code/NO_COLOR conventions (`commands_test.go` is the pattern). This is a second consumer of the same API the web UI drives (root `CLAUDE.md` "new functionality => check the CLI").
- **Review** (`--review`): a new clone-and-diff review run emitting structured findings, reusing the judge's JSON-persist + CLI-fetch plumbing (`api/internal/handler/judge*.go`, `agent/src/judge-runner.ts`) for storage/retrieval only; the diff-review execution is new. `--then-fix` seeds a follow-on task from the findings (precedent: `api/internal/selfimprove/engine.go`).

## Milestones

- [x] **M1 - Data model + `CreateTaskRun`.** `task` added to both `runs_kind_check` and `runs_kind_shape` (novel shape); `issue_description` reused for inline context (256 KiB cap + sanitization inherited); `base_branch` + `open_mr` added; `branch` settable at create. `CreateTaskRun` (modeled on `CreatePromptRun`) creates a server-named `uzi/task/<id>` run, calls `guardDefaultBranch`, and asserts the branch is namespaced and not the default branch.
- [x] **M2 - Worker task execution.** A task run clones the seeded `uzi/task/<id>`, runs the agent over the inline context, commits, and pushes back (no MR by default), reusing the non-forced `pushBranch`; MR-open is skipped unless `open_mr`.
- [x] **M3 - `uzi handoff` CLI.** Create-then-push-then-dispatch ordering (Decision 6): create the run, push local HEAD to the server-named branch with the user's creds, dispatch. `-m`/`-f`/stdin context, `--base`, `--mr`; `uzi handoff rm <id>` (client-side delete); continuation via `uzi run follow-up`. Watchable via the existing `uzi run get/logs --follow` and `uzi tui`.
- [x] **M4 - Review handoff (`--review`).** A new clone-and-diff review run over `uzi/task/<id>` vs its base, emitting structured findings (file + symbol + line + severity + summary + rationale), persisted and fetched as JSON via the CLI, never committed to the branch. Reuses the judge's JSON plumbing only.
- [x] **M5 - Chained fix (`--then-fix`).** On `--review --then-fix`, a follow-on task run seeded with M4's findings pushes fixes to the same branch (precedent: `selfimprove` consuming judge recs).
- [x] **M6 - Temp-branch lifecycle.** `uzi/task/*` namespace; `uzi handoff rm <id>` deletes the remote branch client-side; an `--mr` branch is exempt. (Server-side auto-prune deferred to Later, stated in docs.)
- [x] **M7 - Tests + guardrail verification.** Unit + live-DB coverage for `CreateTaskRun` and the migration shape, the worker push path, the CLI (including `--json`/exit-code/NO_COLOR render), the review JSON contract, and `rm`. Explicitly verify the branch is always namespaced and a task can never resolve to the default/a protected branch, and that the worker push is non-forced. Run through the repo's `task gate:*` targets. *(Landed: live-DB shape + dispatch-gate + task-review round-trip + then-fix enqueue tests, a behavioural non-forced-push proof, `rm` namespace/MR/non-task guards, and handoff `--json`/exit-code coverage; `gate:api`/`gate:agent` green. Minor gap: NO_COLOR is not separately asserted for the handoff verb specifically — handoff renders through the shared `env.printer`, whose NO_COLOR behaviour is covered generally.)*
- [x] **M8 - Docs.** `docs/cli.md` (the `uzi handoff` section) and the embedded CLI skill source (`api/internal/uzicli/skill/SKILL.md`); a feature doc under `docs/`; an `ARCHITECTURE.md` pointer describing `task` as the **seventh** run kind; a `specs/ai.md` decision record; and fix the stale kind list in `api/internal/apitypes/run.go`.

## Parallelization / phase plan

| Phase | Milestones | Notes |
|---|---|---|
| Phase 1 | **M1** | The data model + `CreateTaskRun` gates everything else. |
| Phase 2 (parallel) | **M2** ‖ **M3** ‖ **M8-docs (start)** | M2 (worker) and M3 (CLI) both build on M1's create API and touch disjoint trees (`agent/` vs `api/cmd/uzi/`); doc drafting can begin against the settled design. |
| Phase 3 | **M4** → **M5** | M4 (review) needs M2+M3; M5 (`--then-fix`) consumes M4's findings, so it follows M4. |
| Phase 4 | **M6**, **M7**, **M8-finalize** | Lifecycle, the full test sweep, and doc finalization once behavior is settled. |

## Success criteria

1. From a local checkout, `uzi handoff -m "<context>"` creates a task, pushes local HEAD to a server-named `uzi/task/<id>` branch, and the worker pushes its commits back to that branch — with no forge issue and no MR created — and the user can `git pull` it.
2. `--mr` opens an MR for the branch and exempts it from `uzi handoff rm`; a completed no-MR task branch is removed by `uzi handoff rm <id>`.
3. `--review` returns structured findings (file/symbol/line/severity) as JSON through the CLI, with nothing committed to the branch; `--review --then-fix` pushes fixes derived from those findings to the same branch.
4. The task branch is always in `uzi/task/*` and can never resolve to the default/a protected branch; the worker push is non-forced; every existing guardrail test still passes.
5. `task gate:api`, `task gate:agent`, and any touched component gate are green, with non-vacuous coverage of the new behavior; a `grep` sweep for the retired "issue|ci_fix|chat" kind comment is clean and `docs/cli.md` + `SKILL.md` both list `uzi handoff`.

## Risks and mitigations

- **Touching core run/worker machinery.** Model `task` strictly on `prompt`/`CreatePromptRun`; keep the single PAT git path; land the migration above the live head; update both kind CHECK constraints together.
- **A task escaping to a protected branch.** In v1 the destination is server-named `uzi/task/<id>`, safe by construction; the create-time namespace/default-branch assertion and the forge's own protected-ref rejection (non-forced push) are defense in depth. Arbitrary destinations, which would make a protected-branch refusal load-bearing, are deferred to Later.
- **Branch/run-id ordering.** Resolved explicitly in Decision 6 (create -> receive id + name -> CLI push -> worker), avoiding the circular "push before the id exists".
- **Two writers to one branch.** The CLI seed push precedes the worker; after that the worker is sole writer; a mid-run user push is out of scope and fail-closed (non-ff rejection). Documented in M8.
- **`--review` schema vs the no-line-number findings convention.** The convention exists for cross-run dedup, which does not apply to a single diff; `line` is included deliberately, with `symbol` for stability. Called out so a reviewer does not read it as an inconsistency.
- **Traceability tradeoff.** A raw handoff has no forge issue/record; the run transcript and inline context are still persisted in uzi. Accepted property of the ephemeral mode, documented in M8.

## Dependencies

- `CreatePromptRun` / `CreateSelfImproveRun` as the data-model + creation precedent; the `runs_kind_check` + `runs_kind_shape` migration pattern.
- The worker's non-forced `pushBranch` and MR-skip behavior; the existing PAT git path.
- `guardDefaultBranch` / `DefaultBranchProtection` for the branch-safety assertion.
- The judge's JSON persist + CLI-fetch plumbing (reused by `--review`); `selfimprove` as the structured-output-seeds-a-run precedent (`--then-fix`).
- The forge CLI on the user's machine for `uzi handoff rm` (`git push --delete`).

## Offline-worker readiness

This PRD is self-contained for a restricted-egress worker: every seam it names lives in the cloned repository, and the two genuinely new capabilities (the diff-review run and the client-side branch delete) are built from in-repo patterns (the `pushBranch` seam, the judge JSON plumbing, `selfimprove` consumption) plus normal forge/Anthropic egress. No milestone requires the open web, an external docs lookup, or a prior-art fetch (this is a uzi-internal extension). Note `uzi handoff` and `uzi handoff rm` run on the **user's** machine with the user's credentials, not on the restricted worker, so their `git` operations have no worker-egress implication.
