# PRD #4809: Evals core, compare two run configurations on one open issue

**Status**: Draft  
**Priority**: Normal  
**Umbrella**: #4808  
**Blocked by**: none

## Problem

An owner who wants to know which harness and model works best on their own repository has no way to find out with uzi. Public benchmarks measure other codebases, and comparing two ordinary runs by hand is unfair and noisy: each starts from whatever the default branch is at claim time, a human approves each plan differently, and both runs share the issue's branch (`agent/issue-<n>`, seeded by the issue clone path in `agent/src/git.ts`), its labels, its comments and its merge request. uzi allows one active run per issue (the `uq_runs_one_active_per_issue` index), so the runs cannot even overlap.

## Outcome

An owner starts an **eval** from the CLI or API: one open issue, frozen at creation, one pinned base commit, and two **candidates** (harness, optional custom model provider, model, effort), each run 1 to 3 times. Each **trial** is an `eval_trial` run that never writes to the forge and ends with its diff frozen on the api. The report lists every trial's outcome, failure reason, queue, active and elapsed time, and cost with its provenance, plus per-candidate pass counts. Grading (quality scores) arrives in #4810; until then "pass" means the trial completed and produced a non-empty diff.

Acceptance examples:

1. `uzi eval create --repo <id> --issue 42 --candidate a=claude:<model> --candidate b=codex:<model> --trials 2` returns a draft with its revision and an admission summary (frozen issue digest, base SHA, four planned trials, estimated spend or "no estimate", the capacity notice) and starts nothing. `uzi eval confirm <eval> --revision 1` starts the trials one at a time. Issue 42 gains no label, comment, branch or merge request, and no checkpoint, held, terminal or salvage ref appears on the forge.
2. `uzi eval report <eval>` shows, per trial, `completed` or the failure reason, `active_ms`, `elapsed_ms`, `queue_ms`, `limit_wait_count`, cost with the folded `cost_status` (never shown as zero), and `uzi eval patch <eval> <trial>` downloads the frozen patch. A candidate on a custom provider carries a note that its duration reflects the provider endpoint.
3. An eval created with `--spend-threshold 5` whose first two trials have an estimated spend of 6 USD admits no third trial; the eval ends `completed` with the remaining trials `not_started: spend_threshold`.

## Out of scope

- Quality grading, reviewer panels and preferences: #4810.
- Draft-PR publication and CI evidence: #4811. Hidden tests: #4812. Statistics, trade-offs and composite scores: #4813. The web Evals tab ships last: #4814.
- Closed-issue replay: dropped by the owner.
- In-run plan and code cross-check on trials: frozen off.
- `/api/v1` exposure for external products: not planned; the API stays session-scoped.
- Landing a trial's work: the owner may start a normal issue run afterwards; nothing promotes a trial to a merge request here.

## Modules and seams

1. **Eval store** (`api/internal/store`, one migration): tables `evals`, `eval_candidates`, `eval_trials` per the umbrella contract. `evals.issue_iid` is a reference only and is never copied into `runs.issue_iid`, so the run lifecycle's null-issue skips apply to trials (`notifyOnce` and the terminal-comment path in `api/internal/runlifecycle/lifecycle.go`). Deleting an eval cascades to its candidates, trials and patches; its runs stay as history.
2. **Run kind `eval_trial`**: added to `api/internal/runkind` (DB `runs_kind_check` order, `runkind_migration_test.go`), `fixtures/run-kinds/registry.json`, the agent `RUN_KIND_PROFILES` (`agent/src/run-kind.ts`) and the web run-kind mirror (no UI). Every predicate is stated, because several are negative lists that would silently return true for a new kind: `Delivery` false and `Supporting` true; `Listed` false (surfaced through its eval); `PlanningCapable` true (trials plan, auto-approved); `WallTimed` false, because the wall sweeper parks wall-timed runs and a parked trial would hold the eval's only trial slot (like a job, a trial fails at its own wall instead); `JudgeEligible`, `CodeCrossCheckable`, `PlanCrossCheckable` false; excluded from CI-fix, mr_rework and self-improve by their existing kind allowlists. Modeled on `task` (`api/internal/store/migrations/00134_run_task_kind.sql`): repo-ful, issue-less, its context in `issue_title`/`issue_description`; an issue body over the 256 KiB `issue_description` cap is refused at create (`issue_too_large`), never truncated.
3. **Worker capability `eval_trial_v1`**: required at claim, placement, health reason ("no worker that supports eval trials is online"), hibernation wake and ephemeral provisioning, following the `job_runner_v1` and `cross_check_v1` patterns. Without it an older worker would take the default delivery path, which pushes and opens a merge request (`openMr` in `agent/src/runner.ts` is true for every kind except a no-MR `task`). Roll the api first.
4. **Eval service** (`api/internal/evalsvc`, new). Flow: `create` takes a client idempotency key and returns a `draft` with a `revision` and the canonical admission summary; `confirm --revision <n>` binds consent to that revision, and any later change to the draft (including another PRD's attachment, such as a hidden-test bundle) bumps the revision and voids earlier consent. Create validates the repo, issue (open, owned access) and candidates, freezes the issue text and digest, resolves `base_sha` (default branch head at create, or `--base <sha>` verified to exist on the forge), and freezes candidates through the existing harness and provider resolvers, including the provider's current revision (`model_provider_revisions`), so a later key or endpoint change cannot alter a candidate. Trials admit one at a time per eval through one gate: the next `eval_trial` run is created only after the previous trial's run is terminal and its final usage fold is recorded, so the sequential rule (I3) and the spend threshold are checked at one point. Errors: unknown or foreign repo, provider or issue is a 404; `issue_not_open`; more than 2 candidates or more than 3 trials each is a 400.
5. **Forge silence, structural** (invariant I1):
   - **No write credential reaches the delivery code.** An `eval_trial` claim carries an empty `secrets.forge_pat` and a separate fetch credential read only by the clone and fetch seam. The push, merge-request, description and reply paths read `forge_pat` and fail closed with no credential. The fetch credential is held like `forge_pat` today: by the worker process only, used through the worker's env-scoped git configuration for its own fetches, and never written to the clone's git config, the agent's environment or the trial workspace (a test asserts all three after a trial's clone). Where the worker/agent uid split is active (`docs/proc-hardening.md`), agent processes cannot read worker memory or files; on a single-uid worker they can, exactly as for today's runs. The token itself can read and write (no read-only PAT exists); the ADR records both as the residual risk.
   - **Own delivery branch.** The `eval_trial` profile derives a local branch from the run id (`cloneBranch` / `deriveCloneKey`, never naming the candidate), seeded at `base_sha`. At done it takes an eval-artifact delivery branch that pushes nothing and uploads the patch. `planned_base_commit` / `require_base_match` are not used: they guard divergence from the default-branch head, they do not pin.
   - **Api refusals by route.** A `refuseEvalTrialRuns` middleware, sibling of `refuseJobRuns` (`api/internal/handler/handler.go`), refuses for `eval_trial` with `eval_forge_write_refused`: `/runs/{id}/publish` (including the mid-turn checkpoint tick), `/held-publication`, `/publication-context`, `/terminal-preservation`, `/forge/mr-threads/*`, `/pr-description/*`, and the forge reads that would expose the live issue (`/forge/issues*`). The worker profile never enters those paths, so a refusal cannot leave a custody hold that blocks worker rolls. Archive capture (`/archives/*`) goes only to the api and stays allowed. A route-table test lists every worker route with its expected eval behaviour.
   - **No cross-trial information.** Cross-run memory and the decisions memo (`/memory` and every `/memory/*` subroute, `/decisions-memo`) are refused and not injected at claim, so trial 1 cannot inform trial 2.
6. **Every trial settles unattended.** Any path that would wait on a human ends the trial instead, with a reason: `awaiting_input` questions (`needs_human: question`), `auto_approve_blocked_reasons` (`needs_human: approval_blocked`). The per-trial wall (eval cap, default 2 hours, counting usage-limit waits) ends a trial `failed: wall_exceeded`; a usage-limit wait is allowed only inside that wall. A stale worker is never requeued: the trial ends `infra_failure: worker_lost`. No trial path parks or waits for an owner Resume.
7. **Frozen artifact** (I2, I3): `POST /api/worker/runs/{id}/eval-artifact`, generation-fenced, body capped with `MaxBytesReader` at 4 MiB (patch at most 2 MiB plus its pack at most 2 MiB), sha256 computed on the server. Before `head_sha` is fixed, the worker commits the trial's whole workspace state (tracked modifications and new untracked files, excluding worker scratch paths and ignored files) into one final local commit, so nothing the candidate produced is left out; `head_sha` is that commit and `tree_sha` is its tree. A workspace the worker cannot capture ends the trial `failed: artifact_capture`. It carries `git diff --binary base_sha..head` (no commit messages, authors or trailers), `base_sha`, `head_sha` and `tree_sha`; the worker checks that the patch reconstructs `tree_sha` from `base_sha` before upload, and every consumer (#4810, #4811, #4812) re-checks it before use. The same upload also carries a thin git pack holding one **neutral commit** (parent `base_sha`, tree `tree_sha`, fixed author, committer, date and message, no trailers), capped at 2 MiB; the api checks its parent and tree and stores it beside the patch. #4811 publishes that commit through the api's push broker, so no eval code path ever needs a write credential on the worker. Over the cap the worker sends size only and the trial is `diff_too_large`. **The first accepted upload freezes the trial for good**: it settles in the same transaction that moves the trial terminal, every later upload with a different digest is refused whatever its generation, and the run is driven terminal. A retry from the same generation with the same digest returns the original receipt, so a lost acknowledgement is safe. Trials are never resumed: a worker lost before upload ends the trial `infra_failure: worker_lost`, reported apart from candidate failures and outside pass denominators.
8. **Bounds**: at most 6 trials per eval and 2 non-terminal evals per owner (24 MiB of patches in flight). Patch bytes are deleted 30 days after the eval ends, and an owner keeps at most 200 MiB of retained patch and neutral-pack bytes together, oldest dropped first; summary fields stay. #4811's draft PRs are the durable copy when published.
9. **Timing and cost**: `queue_ms` from creation to `first_started_at`; `elapsed_ms` from `first_started_at` to terminal; `active_ms` = `elapsed_ms` minus `budget_paused_seconds` (time excluded from the run budget: human gates, cross-check, policy and credential holds, resume waits). Usage-limit waits are not banked in seconds, so the report shows `limit_wait_count` (`00091_run_limit_wait.sql`) and flags `active_ms` as including limit waits when it is non-zero. Cost is the folded run usage with its `cost_status` (`docs/run-cost.md`).
10. **Spend and capacity**: the admission summary shows the maximum trial count and an estimate from the owner's recent comparable runs, or "no estimate". The spend threshold (set at create) is checked before admitting each trial, against the previous trials' usage as folded at their terminal reports (the next trial is admitted only after that fold); a trial whose cost is `unreported` adds nothing it can count, which the summary says; estimated cost cannot enforce a hard ceiling, which the summary says. Trials use ordinary run slots at normal priority.
11. **CLI** (`api/cmd/uzi/`): `uzi eval create|confirm|list|get|cancel|delete|report|patch`, `--json` on reads. `docs/evals.md` (`audience: user`, documenting the CLI and API) plus `task docs:sync`.

## Testing decisions

- Store: LiveDB tests for the migration, cascade delete, freeze-on-first-upload (a same-generation, same-digest retry returns the original receipt; a conflicting digest or a stale generation is refused), retention and the per-owner caps.
- Run kind: `runkind_migration_test.go` drift test, the Delivery/Supporting partition test, a table test asserting every predicate's value for `eval_trial`, the agent profile exhaustiveness check, the web mirror test.
- Capability: claim tests that a worker without `eval_trial_v1` never claims a trial and that health names the reason.
- Forge silence: a route-table test of every worker route (refused or allowed for `eval_trial`); an agent test that a trial reaching delivery with an empty `forge_pat` throws before any network call, including the park and shutdown paths, using the existing fake-forge and fake-git fixtures.
- Sequential admission, spend threshold and no-human endings: evalsvc unit tests with a fake clock and fake usage.
- E2E: `e2e/forge-fake/` gains no route; a phase asserting zero forge writes for an eval is listed as owed in the PR, not run in-run.

## Milestones

### M1: One trial runs forge-silent and freezes its diff
**Blocked by**: none
Migration and tables; `eval_trial` kind with every predicate stated across api, fixtures, agent and web mirrors; `eval_trial_v1` capability; evalsvc create/confirm/get for one candidate and one trial; claim without `forge_pat`; worker profile (run-id branch at base, eval-artifact delivery); api refusals; unattended settlement (no-human endings, own wall, no requeue); artifact route with freeze and idempotent same-digest retry; CLI `create|confirm|get|patch`; the ADR recording I1 to I7 as they apply to the core (named by this issue number); docs page and changelog fragment.
Acceptance: example 1 with one trial; the route-table and empty-credential tests pass and fail on a build that only skips the worker side.

### M2: Two candidates, repeated trials, report, caps and cancel
**Blocked by**: M1
Two candidates and 1 to 3 trials each; sequential admission; spend threshold; per-owner caps and retention; custom provider candidates; timing and cost provenance; `list|cancel|delete|report`; docs.
Acceptance: examples 2 and 3; cancel stops admission and cancels the active trial; a lost worker yields `infra_failure: worker_lost` outside the pass count.

### Landing obligations
- `full agent suite: exact-head PR CI`.
- Full `test:api`, `test:web`, `gate:repo` pending exact-head PR CI.

## Decision Log

- **New run kind, not an issue-run flag.** A flag would inherit the issue branch, the one-active-run rule, lifecycle writes and every reactor keyed on `issue`. Rejected: `task` with `open_mr=false`, because tasks still push their branch.
- **Forge silence by withholding the write credential, plus api refusals.** The final push and merge request go from the worker straight to the forge, so an api refusal alone cannot stop them; an empty `forge_pat` makes every write path fail closed. Api refusals cover the pushes the api performs and the reads that would leak.
- **A capability gate.** Hosted workers are pinned apart from the app, so mixed fleets are normal; an older worker would deliver a trial as a merge request.
- **Trials are not wall-parked.** Parking would strand the sequential slot; a terminal `wall_exceeded` keeps the eval moving and is reported.
- **Freeze on first upload, never resume.** A resumed trial is a different experiment and could replace an artifact already graded. Worker loss is reported as infrastructure failure, not held against the candidate.
- **The neutral commit rides in A's upload.** #4811 needs a publishable commit after the worker is gone; building it while the trial is still on the worker is cheapest and keeps every forge write on the api. Rejected: a worker push in #4811 (contradicts the empty `forge_pat`).
- **Patch stored on the api, capped at 2 MiB, retained 30 days.** Survives worker loss without a new object store. Rejected: keeping the diff on the worker (lost with an ephemeral worker) and a new artifact store (more infrastructure than the need).
- **Sequential trials.** Fairness (similar load and time of day) and one spend check point outweigh speed.
- **Two candidates.** The owner's question is A versus B; more candidates multiply cost and reviewer work.
- **In-run cross-check off.** Plan cross-check can force a human gate, and the code-check child does not attach to this kind; turning it on per candidate would change what is compared.
- **Limit waits reported by count, not subtracted.** Banking limit-wait seconds would be new accounting on every run kind. Rejected: failing a trial on a usage limit (noise unrelated to the configuration).
- **No feature switch; only the web UI waits.** The owner decided docs, CLI and API ship with each PRD and only the Evals tab waits for #4814. Rejected: an instance-wide off-by-default switch.
- **"Pass" before grading is completion plus a non-empty diff,** stated as provisional; #4810 replaces it.
