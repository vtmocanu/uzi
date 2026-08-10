# PRD #297: Self-improvement picker skips work already in progress or done

**Status**: Draft (2026-08-10; two code-verified reviews folded in — the precedent, the
threading model, and the M3 file path below are the corrected versions)

**Priority**: Medium

**Effort**: small–medium (4 milestones)

**Independent of PRD #293.** The two share only the motivating anecdote; they touch
disjoint files (#293 is `Taskfile.yml` + `agent/src/sdk-*.ts`; #297 is
`api/internal/workersvc` + `agent/src/prompt.ts` + `store/queries`). Do not serialize
them.

## Problem

The self-improvement engine (`api/internal/selfimprove`) folds the accumulated
`improve_uzi` backlog into one auto-approved `self_improve` run per cycle, and the
run's lead agent **picks one** improvement to implement. That pick is made blind to
what uzi is *already doing*: there is no reconciliation against runs currently in
flight or work that just landed.

On 2026-08-10 this produced a live near-miss. The self_improve run
(`2fb52bcc`, issue #296) picked **PRD #293 M3** — the `deadcode:web`/`deadcode:agent`
graceful-skip — while issue **#293's own dedicated run** (`ad991651`) was at
iteration 2 actively implementing *all five* milestones of #293, M3 included
(its frozen milestone `m3` is titled, verbatim, "Graceful-skip for
deadcode:web/deadcode:agent when knip absent"). Two runs were about to open
competing MRs on `Taskfile.yml`. It was caught only because a human noticed and
cancelled the self_improve run before it opened its MR.

### Why the existing dedup did not fire

The overlap was **invisible to every signal we check today**:

- The self_improve cycle gate (`CountActiveSelfImproveRuns`, `engine.go:185`;
  `selfimprove.sql:43`) counts only *other self_improve runs*, not issue runs. An
  active issue run on #293 does not block the cycle, and correctly so — that is not
  what that gate is for.
- Branch/MR-based dedup could not see #293 either: the #293 run had produced **no
  branch and no MR yet** (`branch=null`), because it was still mid-implementation.
  A frozen, actively-worked run is exactly the state a branch/MR check misses. So the
  reconciliation must be **status-driven, not branch-driven**.
- The backlog handed to the picker (`composeRunDescription`, `engine.go:351`,
  `344-350`) is the untrusted recommendation text alone. It carries no signal about
  what is in flight or what recently landed.

This is the general form of recommendation **#27** already in the #296 backlog
("reconcile branch/MR state before scheduling a run"), one level up: reconcile the
*pick* against in-flight work, keyed on run status rather than on a branch that does
not exist yet.

## Solution

Give the self_improve worker a **DB-derived list of work already in flight**, and
instruct the picker to skip any recommendation whose fix is already being worked.

**The seam, corrected against the code.** The anchor precedent is
**`known_improve_uzi_targets`** (`agent/src/protocol.ts:508`, issue #232) — NOT a
"trusted" field (an earlier draft of this PRD called it `known_l_targets` and
"trusted"; both were wrong). Its real, verified shape is exactly what we want to
mirror:

- **Computed at claim time**, best-effort, in the claim-assembly path
  (`assembleJudgeClaim` → `ListKnownImproveUziTargetsForUser`,
  `api/internal/workersvc/judge.go:809-833`), carried on the claim response
  (`claim.go:138-144`). It is **not** snapshotted at run-create and needs **no
  column on `runs`**.
- **Rendered nonce-fenced as UNTRUSTED data**, never as instructions
  (`agent/src/prompt.ts` `recommendationsFrame`, ~924-940). Our in-flight list is
  assembled by uzi's own code but its *content* is issue titles and agent-authored
  milestone titles — attacker-influenceable (anyone who can file an issue on the
  connected repo). Trusted *provenance* is not trusted *content*: the list rides the
  same untrusted nonce-fence, kept as its own fenced block distinct from the
  recommendation backlog.
- **Additive, optional claim-response field** (like `known_improve_uzi_targets?`,
  `milestones?`): an older worker ignores it, a newer worker treats absent as empty.
  So no coordinated api/agent deploy is required (this property is *because* we compute
  at claim time, not at create — see D5).

**Advisory, not a hard block (D4).** A free-text `improve_uzi` target ("gate signal
done when …") cannot be reliably string-matched to a milestone title, and a
self_improve pick has no issue coordinate to collide against the one-active-run index
that dedupes real issue runs. So the picker is an LLM applying judgment over a list,
exactly as `known_improve_uzi_targets` informs rather than enforces. The
reconciliation makes the overlap *visible to the picker*; the picker decides.

## Root-cause map

| Signal we have | Why it missed #293 |
|---|---|
| `CountActiveSelfImproveRuns` (engine.go:185) | counts self_improve runs only, not issue runs (by design) |
| branch / MR existence | the #293 run had no branch/MR yet (mid-implementation) |
| `composeRunDescription` backlog (engine.go:351) | untrusted rec text only; no in-flight context |

## Milestones

- [ ] **M1 — Assemble the in-flight avoid-set at CLAIM TIME (api).** In the claim path
      (`assembleClaim`, gated on `kind=='self_improve'`, mirroring the judge path's
      `assembleJudgeClaim`), build a list of coordinates for active runs and attach it
      as a new additive-optional claim-response field (`inflight_targets`, name TBD).
      Reuse `ListActiveRunsAll` (`store/queries/runtime.sql:428`) — its embedded run
      row already carries `issue_iid`, `issue_title`, and `milestones_frozen`, so **no
      new column and no new query are needed** for this half; filter to the
      self-improve repo in Go (that query is all-repos/all-users, `LIMIT 500`). Include
      statuses `queued`/`claimed`/`running`/`awaiting_approval`/`awaiting_input`. Cap
      the list. No migration.

- [ ] **M2 — Worker directive: render the list nonce-fenced, instruct the picker to
      skip in-flight work (agent).** The self_improve picker directive lives in
      `agent/src/prompt.ts` — `SelfImprovePlanPromptInput` and
      `buildSelfImprovePlanPrompt` ("pick exactly ONE top improvement, keep the
      guardrails intact"), consumed from `sdk-executor.ts` (~869-873); `self-improve.ts`
      is branch/check-env/MR-section logic, NOT the picker prompt. Add the field to
      `SelfImprovePlanPromptInput`, render it as its own **untrusted nonce-fenced**
      block (the "trusted directive outside the fence" seam already exists there), and
      instruct: do not pick an improvement whose fix is already in flight; if the top
      candidate overlaps, pick the next and record the skip + reason in the run feed.

- [ ] **M3 — (additive, gated on D2) "recently landed" work.** Extend the avoid-set to
      work that just merged. **This half has no clean timestamp**: there is no
      `merged_at` column on `runs` (only `mr_state`, edge-triggered, `00029`;
      `finished_at`, which is MR-*open* time not merge time; and `updated_at`, written
      by many). Resolve D2 first — accept `mr_state='merged'` ordered by `updated_at`
      as an approximate proxy (and state the skew), or add a merge-time stamp (a
      migration). Only build M3 once D2 is decided; the in-flight M1/M2 alone already
      fix the motivating bug (which was issue-level, `branch=null`).

- [ ] **M4 — Tests + docs.** Logic test with an in-package fake store (given an active
      run on issue N, the assembled avoid-set contains N's coordinates; given a rec that
      overlaps, the field is populated) — this needs no live DB. **Any new SQL M3 adds
      gets a `*LiveDB` test** run via `./e2e/run-store-it.sh` (a green `sqlc generate`
      is not evidence the query runs — sqlc's type deduction is not Postgres's). Worker
      render test in `agent/` (`node --import tsx --test`, not vitest; carry
      `--test-timeout`, read the exit code not the tally) asserting the list renders in
      its own untrusted fence, separate from the recommendation fence. Update
      `docs/self-improvement.md` (including the residual advisory-not-enforced risk and
      the worker-rollout caveat), CHANGELOG, and tick this PRD.

## Decisions to make in the plan

- **D1 — "in flight" scope.** Confirm the status set
  (`queued`/`claimed`/`running`/`awaiting_approval`/`awaiting_input`) and which run
  kinds count (issue, self_improve, prompt; probably exclude chat/judge — as
  `ListActiveRunsAll` already does). Status-driven, not branch-driven.
- **D2 — "recently landed" key (blocks M3 only).** No `merged_at` exists. Accept
  `mr_state='merged'` + `updated_at` as an approximate window key (state the skew), or
  add a merge-time column. Also verify `runs.mr_state` is actually written to
  `'merged'` for in-scope runs before relying on it.
- **D3 — coordinate shape the picker sees.** Issue iid + title + frozen milestone
  titles (all already on the `ListActiveRunsAll` row, so milestone-level overlap — the
  #293 case — is reachable without a new fetch). Decide whether to also extract a PRD
  number from the title.
- **D4 — advisory vs guard.** MVP is advisory. A deterministic loud-flag has no
  reliable coordinate to test (a self_improve pick is free text, no issue id), so
  scope any backstop to "only when the picker emits a structured issue/PRD reference,"
  or defer.
- **D5 — claim-time vs create-time (resolved: claim-time).** Compute in `assembleClaim`,
  not in `runCycle`/`CreateSelfImproveRun`. Rationale: matches the
  `known_improve_uzi_targets` precedent, needs no `runs` column or migration, is
  fresher (snapshot is seconds old, dissolving most of the staleness risk), and keeps
  the field additive-optional so no coordinated deploy is needed. Record this so it is
  not re-litigated back to a create-time column.

## Open questions

1. Does the self_improve claim path have a natural slot for a best-effort list
   alongside the existing claim assembly, mirroring `assembleJudgeClaim`? (Expected
   yes.)
2. Should a cycle that finds its *entire* top backlog already in flight fall back to
   the open-ended code-review path (the empty-backlog branch already exists in
   `composeRunDescription`), and surface that as a skip notification?

## Parallelization plan

| Phase | Milestones | Depends on | Files (distinct) |
|---|---|---|---|
| 1 | **M1** (claim-time in-flight set) | — | `api/internal/workersvc/{service,judge}.go`, `agent/src/protocol.ts` (field decl) |
| 2 | **M2** (worker directive) | M1's field | `agent/src/prompt.ts`, `sdk-executor.ts` |
| 3 | **M3** (recently-landed) | D2 | `store/queries/*.sql` (+ maybe a migration) |
| 4 | **M4** (tests + docs) | M1–M3 | `*_test.go`, `agent/**/*.test.ts`, `docs/`, CHANGELOG |

M1 and the protocol field decl can land together; M2 depends on the wire field; M3 is
independent and additive but gated on D2.

## Risks and mitigations

- **The avoid-list is treated as trusted and injected as instructions (audit C1).**
  This was the mistake in this PRD's own first draft. Mitigation: render it
  nonce-fenced as untrusted data (like `known_improve_uzi_targets`), in its own block
  distinct from the recommendation backlog; a worker test asserts the fencing.
- **Advisory context does not guarantee the picker obeys.** True by design; this
  reduces the failure rate, it does not prove impossibility. Documented as a residual
  risk; D4's optional flag is the only stronger signal available and is itself limited.
- **Worker-rollout skew (#201-class).** M2 changes agent prompt code, which reaches
  only newly provisioned workers unless the fleet re-provisions (the caveat #293 also
  carries). The M1 api change takes effect immediately at claim; the M2 render does not
  until workers roll. Mitigation: state the rollout in M4/docs. (The additive-optional
  field means an un-rolled worker simply ignores the list — no breakage, just no
  benefit yet.)
- **Prompt-budget growth.** Two capped lists (backlog + avoid-set). Cap the avoid-set;
  prefer titles over bodies; `ORDER BY recency LIMIT n` in SQL, not load-all-then-cap.
- **Staleness.** Largely moot at claim-time (snapshot is seconds old). A run that
  starts moments later is not in it — acceptable; the window that bit us was tens of
  minutes.

## Success criteria

1. Given an active issue run on issue N (no branch/MR yet), a self_improve **claim**
   carries N's coordinates in the in-flight field — verified by a logic test against a
   fake store returning that active run, not against LLM behavior.
2. Any new SQL added for M3 executes against a real Postgres — verified by a `*LiveDB`
   test via `./e2e/run-store-it.sh`, not by `sqlc generate` passing.
3. The worker renders the in-flight list in its own untrusted nonce-fence, separate
   from the recommendation backlog fence — verified by an `agent/` `node --test` unit
   test (read the exit code and named test, not a tally).
4. `docs/self-improvement.md` documents that the picker is shown in-flight work and
   directed to avoid overlap, including the residual (advisory, not enforced) risk and
   the worker-rollout caveat.
5. The 2026-08-10 scenario (a rec overlapping an actively-worked issue run) is covered
   by a regression fixture (state whether it is an in-code fake table — no `-count=1`
   concern — or a cross-module golden file, which would need `-count=1`).

> **Note — no success criterion asserts the picker *decides* to skip.** That is
> deliberate: the advisory behavior is unfalsifiable against LLM output, so the
> criteria assert only on data assembled and rendered. Do not add a "picker avoids
> overlap" criterion.
