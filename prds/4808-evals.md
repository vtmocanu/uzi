# Evals: shared contract for the six child PRDs (umbrella #4808)

Shared names, invariants and decisions for the child PRDs indexed by umbrella #4808. Each child PRD carries its own milestones and Decision Log; this file holds what several children must agree on.

## Children, order, blocking edges

| # | PRD | Outcome | Blocked by |
|---|---|---|---|
| A (#4809) | prds/4809-evals-core.md | Owner runs an eval from the CLI/API: one frozen open issue, one base SHA, 2 candidates x 1-3 trials, forge-silent `eval_trial` runs, frozen verifiable diff per trial, report of time/cost/failure | none |
| B (#4810) | prds/4810-evals-grading.md | Each frozen trial is graded by a lead reviewer + optional opposite-family cross reviewer, blind, same rubric; pairwise order-swapped preference | A |
| D (#4811) | prds/4811-evals-draft-prs.md | Default-on (per-repo opt-out) draft-PR publication of frozen trials + pinned CI check set as grading evidence (CLI/API) | A |
| E (#4812) | prds/4812-evals-hidden-tests.md | Owner-curated hidden test bundle per eval, verified red on base, run after freeze in a credential-free sandbox; results join the pass rule (CLI/API) | A, B |
| F (#4813) | prds/4813-evals-statistics.md | Uncertainty and trade-off computation: intervals, insufficient-data rule, Pareto set, owner-weighted composite (CLI/API) | A, B |
| C (#4814) | prds/4814-evals-tab.md | LAST: web Evals tab rendering everything (create with consent, watch, trials, both reviewer scores, PR/CI evidence, holdout results, statistics) | A, B, D, E, F |

Maintainer decisions (2026-11-28):

- Open issues only; closed-issue replay is out.
- The Evals tab (C) ships last. D, E and F are CLI/API only and C renders all of them. D may start after A; E and F after B; D, E and F can run in parallel. No feature switch: docs (`docs/evals.md`, `audience: user`), CLI and API ship with each PRD.
- Draft PRs are the default once D ships, on verified forges (GitHub first), with a per-repo opt-out and a per-eval override; the admission summary names the PR count and that CI and installed apps run. A whole eval's drafts may await CI at once (at most 6 per repo). CI evidence is recorded on the live target. uzi closes drafts itself after a re-check; the branch delete is guarded by `ExpectedOldTip`.
- Grading uses a lead reviewer plus an optional opposite-family cross reviewer, reusing the cross-check checker transport.
- Hidden tests are owner-curated and run on a controller-provisioned, run-bound ephemeral k8s worker with its private rootless DinD sidecar, role assigned server-side; compose is unsupported at first; the live isolation proof (#4815) gates enabling them.
- Custom model providers are allowed for candidates and reviewers.

## Names (use these exact identifiers across all PRDs)

- Run kinds: `eval_trial` (implementation trial, report-only towards the forge), `eval_grade` (report-only grader), `eval_holdout` (E: one hidden-test check on one frozen artifact, run only on a controller-provisioned ephemeral hosted worker bound to that run). All three are added to `api/internal/runkind`, `fixtures/run-kinds/registry.json`, the DB `runs_kind_check`, agent and web mirrors. None is judge-eligible, CI-fix, mr_rework or self-improve eligible.
- Tables (A unless noted): `evals`, `eval_candidates`, `eval_trials`; B: `eval_gradings`, `eval_preferences`; D: `eval_publications`, `eval_trial_checks`; E: `eval_holdouts`, `eval_holdout_results`.
- `evals`: owner user, repo, `issue_iid` (reference only, never copied to runs.issue_iid), frozen `issue_title`/`issue_body` + digest, `base_sha`, status (`draft|running|grading|completed|cancelled|failed`), caps (max trials, per-trial wall, spend threshold), `publish_mode` (`local` until D ships; from D, `draft_pr` is the default on supported forges unless the repo opted out, overridable per eval at create), grading config (B), created/finished times.
- `eval_candidates`: opaque `label` (`a`,`b`), harness, optional `provider_id`, model, effort, `trials` (1-3). In-run plan/code cross-check is frozen OFF for all trials. Frozen at create.
- `eval_trials`: candidate, ordinal, `run_id`, status, failure reason, frozen artifact (`base_sha`, `head_sha`, output `tree_sha`, binary-safe patch bytes + sha256, size; the patch must reconstruct `tree_sha` from `base_sha`; cap 2 MiB, over cap = `diff_too_large`, ungradable), `queue_ms`, `active_ms`, `elapsed_ms`, `limit_wait_count`, usage + `cost_status` provenance (`metered|subscription|unreported`).
- Flow: `create` (idempotency key) returns a `draft` with `revision` + canonical admission summary; `confirm --revision n` binds consent to that revision; any draft change (incl. E's bundle attach) bumps the revision.
- API: owner-scoped, cookie session + CSRF, under `/api/evals` (NOT `/api/v1`). CLI: `uzi eval create|list|get|cancel|report` (A), `uzi eval grade` options (B), `--publish draft-pr` (D), `uzi eval holdout set|verify` (E).

## Invariants (ADR to be written in A, number = A's issue)

- I1 Forge silence: an eval run writes nothing to the forge (no push, checkpoint ref, label, comment, MR), across retries, parks, shutdown and recovery; enforced at worker AND api. Only exception, D's authorized writes and nothing else: create the `uzi/eval/<eval-id>/<opaque>` branch if absent with one neutral commit reconstructed from the frozen artifact, create the draft PR, add the `uzi-eval` label, and custody-checked cleanup (uzi closes the PR after re-reading that it is still an unchanged, unmerged draft, a residual race D records; then an exact-old-SHA branch delete). Only after every trial of the eval is terminal. Remote checkpoints stay disabled in both publish modes.
- I2 No feedback: grading and hidden tests run only after the trial is frozen and never alter, resume or gate a trial.
- I3 Fairness: all trials of an eval share frozen issue text, base SHA, prompt, tools, budgets and plan auto-approval; candidates differ only in their frozen candidate fields. Artifact settles atomically with trial terminal state, bound to the claim generation; patch bytes retained 30 days after eval end. Trials run sequentially (one active trial per eval).
- I4 Blinding: graders get the frozen task, rubric and anonymous diff only (no model, harness, provider, branch, commit message, trailer, PR URL, cost). No Co-Authored-By attribution in eval commits.
- I5 Holdout secrecy (E): hidden tests never reach a trial workspace, trace, prompt, PR or grader input; graders may see only pass/fail counts if E chooses.
- I6 Owner only: an eval, its candidates' credentials/providers and its reviewers are the owner's own; owner consents to the max runs, max reviewer runs and sending diffs to reviewer endpoints before start. Spend threshold stops admission (soft: estimated cost cannot enforce a hard ceiling).
- I7 Results are per harness+model configuration, never "model vs model"; missing cost is never ranked as zero; local/provider durations note that they measure the endpoint's hardware.

## Reuse anchors (cite symbols, not line numbers; re-verify when drafting)

- Issue-run coupling to avoid: `uq_runs_one_active_per_issue` index; issue branch seeding in `agent/src/git.ts` (`agent/issue-<n>`); lifecycle null-issue skips in `api/internal/runlifecycle/lifecycle.go` (`notifyOnce`, terminal comment).
- Delivery: `openMr` in `agent/src/runner.ts` is true for every kind except a no-MR task; final push and MR go worker to forge on `secrets.forge_pat`, so forge silence withholds `forge_pat` from eval claims.
- Route refusals: `refuseJobRuns` in `api/internal/handler/handler.go` is the precedent for `refuseEvalTrialRuns`.
- Exact-commit read-only checkout: `runnerCloneAtCommit` (`agent/src/git.ts`), for graders and holdouts; trials use a run-id branch via `cloneBranch`/`deriveCloneKey`.
- Checker adapters: `agent/src/claude-cross-check.ts`, `agent/src/codex/cross-check.ts`, `agent/src/cross-check-runner.ts` (transport reused; prompt, schema, validator new).
- Tool-free model pass: `runReadOnlyModelPass` (`agent/src/model-pass.ts`).
- Cost provenance: `docs/run-cost.md` "Metered, subscription, and unreported cost"; providers: `docs/configuration.md` "Custom model providers".

## Authoring rules (all PRDs)

- Sections: Problem, Outcome (2-3 acceptance examples), Out of scope, Modules and seams (resource bounds for untrusted input), Testing decisions, Milestones (vertical slices, `Blocked by`, acceptance), Decision Log. Header: `# PRD #<n>: <title>`, `**Status**: Draft`, `**Priority**: Normal`, `**Umbrella**: #4808`.
- uzi-bound: no `.github/workflows/**` in implementation or validation; no `task gate` / full `gate:<component>` as acceptance; agent changes list `full agent suite: exact-head PR CI` under "Landing obligations". Offline-resolvable facts only.
- Public: no internal hostnames, real users, product names; no names of non-permissive prior-art projects.
- Changelog fragment per shipping PR; docs/*.md edits need `task docs:sync`; CLI parity (`api/cmd/uzi/`) in the same slice as the API.
- No em dashes in PRD prose (public-facing).
- Keep each PRD to one implementation run / one PR. Target 120-220 lines.
