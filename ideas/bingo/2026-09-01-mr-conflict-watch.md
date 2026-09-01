# MR merge-conflict watch: stop letting a finished run's MR rot into an unmergeable branch in silence

Line references are against `736df08d`.

## Rationale

uzi automates two of the three things that can block a completed run's MR from
landing, and is completely blind to the third.

**What it does know.** A red pipeline on an agent branch becomes a `ci_fix` run
(`api/internal/poller/poller.go:428`, `e.ciAutoFix.detect(ctx, r, f)`). New review
comments become an `mr_rework` run (`poller.go:440`, `e.mrReviewWatch.detect(ctx, r, f)`).
PRD #6's own rationale names the problem shape exactly: *"an agent's MR with failing CI
looks \"done\" in uzi while being unmergeable"* (`prds/done/6-ci-status-integration.md:11`).

**What it does not.** The forge seam carries no mergeability at all:

```go
// api/internal/forge/forge.go:284-288
type MergeRequest struct {
	IID    int64
	State  string
	WebURL string
}
```

That gap was flagged as out of scope by the previous bingo idea and explicitly
parked: *"MR mergeability/conflict state (still absent from `MergeRequest` — a
separate idea)"* (`ideas/bingo/2026-08-25-mr-review-rework-runs.md:134`). This is that
idea, and the intervening tree confirms it is still open — `ListMergeRequestComments`
shipped (`forge.go:521`, migrations `00166`/`00167`/`00168`), `MergeRequest` did not
move. To head off a false "this exists already": the worker *does* handle merge
conflicts today, but only **worker-side, at finalize, on the local clone** —
`alignBranchWithDefault` (`agent/src/git.ts:1347`) returns `"aligned" | "conflict"`,
and a conflict fails the run via `fail_origin: "finalize_base_align_conflict"`
(ADR 0456). The **server-side, per-poll-tick, forge's-view-of-the-MR** observation —
the whole window after the MR opens, while a human review is pending and `main` is
moving — is what is absent.

**Why this bites in practice, three ways:**

1. **The human finds out last.** A completed run parks its card in Human Review
   (`docs/board.md:194-195`: *"Closing a completed run's merge request without merging
   it is treated as \"rework needed\""*). While it sits there, `main` moves. Nothing in
   uzi observes that the branch stopped merging; the board still shows a green-CI,
   review-clean, ready-looking card. This repo's own maintainer skill is the evidence —
   its release survey opens by hand-reading mergeability per PR
   (`.claude/skills/uzi-release/SKILL.md:26-27`, `gh pr view <n> --json
   number,title,headRefName,mergeable,mergeStateStatus,files`) and has to explain the
   vocabulary because nothing upstream of it does: *"`CONFLICTING` means a real
   conflict"* (`SKILL.md:30`). The whole manual resolution runbook then follows at
   `SKILL.md:90-96` (sibling worktree → `git merge origin/main` → renumber the
   colliding migration → regenerate sqlc → gate → push).

2. **`mr_rework` actively makes it worse.** Its candidate query gates on
   `AND r.mr_state = 'opened'` (`api/internal/store/queries/mr_rework.sql:44`) and its
   first detector gate is CI, not mergeability — `// GATE 1 — GREEN HEAD PIPELINE`
   (`api/internal/poller/mr_review_watch.go:168`). Neither file gates on MR
   mergeability anywhere (the SQL file's `ON CONFLICT` lines are Postgres upserts, and
   its "branch conflict" comment is about the cross-kind branch-claim guard, not the
   MR). So a conflicted-but-green MR keeps drawing auto-rework runs that pile commits
   onto a branch that cannot land, and each one burns a cycle of the per-MR cap ledger
   (`api/internal/store/migrations/00168_mr_rework_ledger.sql`, default 5 —
   `docs/mr-review-watcher.md:126-127`). Token spend on a branch a human must rebase
   by hand anyway.

3. **The branch-align machinery exists but only fires once, for a different reason.**
   `alignBranchWithDefault` is invoked only at finalize, GitHub-only, and only when
   `.github/workflows/` differs (`adr/0456-rebase-before-finalize-push.md`, Decision
   summary steps 1-3). ADR-456 even names keeping a branch aligned over its life as
   deferred: *"a periodic/early-align follow-up that keeps a run's branch aligned with
   main throughout its life … is deferred in the PRD as optional"*
   (`adr/0456-rebase-before-finalize-push.md:55`). Post-MR alignment — the window
   where a human review is actually happening and `main` is actually moving — is
   covered by neither.

**The observation adds zero forge API calls**, which is what makes this scopeable.
`SyncMRStates` already performs exactly one single-MR GET per watched candidate per
poll tick:

```go
// api/internal/forgesvc/mr_watch.go:60
mr, err := f.GetMergeRequest(ctx, forgeProjectID, c.MrIid.Int64)
```

and all three drivers already map that one response object
(`api/internal/forge/gitlab.go:517` → `toMergeRequest`, `github.go:591` →
`toGitHubMergeRequest`, `forgejo.go:796` → `toForgejoMergeRequest`). Every forge
returns mergeability **on that same object** — verified against upstream SDK source at
the pinned versions (`api/go.mod`): GitLab client-go v2.58.2's `BasicMergeRequest`
carries `HasConflicts bool` + `DetailedMergeStatus string`; go-github v90's
`PullRequest` carries `Mergeable *bool` + `MergeableState *string`; gitea sdk
v0.25.1's `PullRequest` carries `Mergeable bool`. Two caveats so "zero extra calls"
is not misread as "a free bool":

- **GitHub's value is computed asynchronously** — a single GET can return
  `mergeable: null` ("GitHub has started a background job to compute the
  mergeability… resubmit the request", per its REST docs), so the design must be a
  tri-state with re-observe-next-tick, never a bool.
- **The rework detector does not read the forge** — `ListMRReworkCandidates` reads
  `runs.mr_state` from the DB (`mr_rework.sql:15-17` says so explicitly), so
  suppressing conflicted rework requires *persisting* the observation, not just
  reading a new field on the tick.

The mr_watch candidate set is already precisely "MRs a human is sitting on":
`(i.state = 'opened' AND (jsonb_exists(i.labels, 'Human Review') OR l.mr_state =
'closed'))` (`api/internal/store/queries/forge.sql:545`).

## Sketch

### M1 — observe, record, surface, notify (no token spend)

- **Forge seam.** Widen `MergeRequest` (`forge.go:284`) with `Mergeable string` over a
  neutral closed enum `mergeable | conflicted | unknown`, plus the
  `IsKnownMRState`-style validator idiom already beside it. Map it in each driver's
  existing `to*MergeRequest` from the SDK fields named above. Anything a driver cannot
  answer (GitHub's null, an unrecognized `DetailedMergeStatus`) maps to `unknown`,
  never to `mergeable`: fail-safe in the direction of saying nothing.

- **Persist, mirroring `mr_state` exactly.** A new migration (number assigned at
  merge; live head at write time is `00181_vault_lock_notice.sql`) adding two nullable
  columns in the shape `00029_run_mr_state.sql:18` established (`ALTER TABLE runs ADD
  COLUMN mr_state text;`): `runs.mr_mergeable text` plus `runs.mr_mergeable_since
  timestamptz` (the `status_since` / `health_since` idiom). Written by a
  `recordMRMergeability` sibling of `recordMRState`
  (`api/internal/forgesvc/mr_watch.go:232-239`), on the same observation, under the
  same log-and-skip contract. Recording is unconditional and independent of the
  state-edge machine, so a forge blip cannot poison the PRD #24 baseline.

- **The debounce is load-bearing, and the repo already documented why.** The release
  skill records GitHub's async lag first-hand: *"expect a **transient stale** `Pull
  Request has merge conflicts` right after you push a resolution (GitHub's async
  mergeability lag) — re-check … after a few seconds"*
  (`.claude/skills/uzi-release/SKILL.md:89`). So: **nothing acts on a first
  observation.** `unknown` never fires and never clears; only `conflicted` held
  continuously for `MR_CONFLICT_QUIET_PERIOD` (default ~10m) is actionable — the same
  debounce shape as `MRReviewWatch`'s `quietPeriod`
  (`api/internal/poller/mr_review_watch.go:72`). The poller re-GETs every tick anyway,
  so no bespoke re-poll loop is needed.

- **Suppress the harmful rework.** Add a gate to `detectOne` beside GATE 1
  (`mr_review_watch.go:168`): a candidate whose recorded mergeability is `conflicted`
  is a silent no-op — no ledger write, no cap burn. Costs one column on
  `ListMRReworkCandidates` (`mr_rework.sql:47-53`) and a three-line gate. **This is
  the cheapest, highest-value single change in the whole idea and should land first.**

- **Notify.** One `notifysvc.Notification{Kind: "mr_conflicted"}` per MR per conflict
  episode (latched on `mr_mergeable_since`, so a flapping forge cannot spam), fired
  from the new detector exactly as `notifyHalt` does (`mr_review_watch.go:295-307`).
  Payload: a small `MRConflictPayload` declared beside `CIAutofixPayload`
  (`api/internal/notifysvc/service.go:111`), for the reason stated there — one place
  all producers already import. Give it the soft `{title, body}` convention so the
  inbox needs **no** kind switch (`web/src/lib/notifications.ts:25-34`), and a `RunID`
  so the existing kind-conditional deep-link lands on `/runs/…`
  (`notifications.ts:46-55`). Slack render optional; recommended, since Human Review
  is the surface humans watch least.

- **Web.** A conflict marker is **orthogonal to `MrChipState`, not a fourth value of
  it** — an MR is `opened` *and* `conflicted` — so leave `mrChipState` alone
  (`web/src/lib/runBadge.ts:41`, `"open" | "merged" | "closed"`, derivation at
  `:49-53`) and add a separate warn-toned badge rendered beside `MrChip`
  (`web/src/components/MrChip.tsx`) on the board card and run view. Add
  `mr_mergeable` to the run DTOs next to `mr_state` (`web/src/lib/api.ts:553` and
  `:1802`), carrying forward that field's own staleness caveat verbatim ("a
  superseded run's value can be stale").

- **Mock parity is a CONTRACT here, not a convenience.** `web/src/mocks/mockApi.ts`
  becomes a second implementation of "when does the conflict badge show". Do **not**
  snapshot a golden fixture from the demo data — it would agree with itself. Author it
  to discriminate, one case per reimplemented branch, plus an assertion that the
  fixture actually contains each: (a) `opened` + `conflicted` → badge; (b) `opened` +
  `mergeable` → no badge; (c) `opened` + `unknown`/null → no badge **and no
  "mergeable" claim** (the two must render differently from each other in the test,
  or the test cannot tell "we don't know" from "it's fine"); (d) `merged`/`closed` +
  `conflicted` → badge suppressed (terminal state wins). PRD #311 already enforces
  mock currency; this is the fixture's discrimination requirement on top of it.

- **CLI + docs.** `mr_mergeable` on `uzi run get`, one marker in `render.go`; a "when
  your MR stops merging" section in `docs/board.md` beside the rework paragraph at
  `:194-205`, and a note in `docs/mr-review-watcher.md` beside the trigger list
  (`:22-29`) saying a conflicted MR is not reworked.

### M2 — opt-in auto-realign run (cuttable; see Scoping notes)

A third detector, `MRConflictWatch`, in `api/internal/poller/`, wired after
`SyncMRStates` and **before** `mrReviewWatch` (`poller.go:367` → new → `:440`),
structurally a clone of `MRReviewWatch`: injected store/runs/notifier/settings
interfaces, admin kill-switch read once per repo and **failing closed** on error
(`mr_review_watch.go:101-113`, the `KeyMrReworkEnabled` idiom at
`api/internal/settings/settings.go:251`), per-candidate log-and-skip, a candidate
query cloned from `ListMRReworkCandidates` (`mr_rework.sql:35-67`) including its
`COALESCE(per_branch.mr_rework_enabled, u.mr_rework_enabled) IS NOT FALSE` opt-out
chain and its Anthropic-token EXISTS gate.

**Reuse the `mr_rework` run kind rather than minting a ninth.** The
`runs_kind_shape` row already fits an align run exactly — `(kind = 'mr_rework' AND
repo_id IS NOT NULL AND pipeline_ref IS NOT NULL AND mr_iid IS NOT NULL AND
target_run_id IS NOT NULL)` (`api/internal/store/migrations/00167_run_mr_rework_kind.sql:30`)
— and the run's behaviour is identical in every respect that matters: reworks the
branch **in place**, pushes a fix commit, **never merges**. Distinguish with a
nullable `runs.mr_rework_cause text CHECK (cause IN ('review','conflict'))`, which
selects the prompt context block worker-side. This avoids widening
`runs_kind_check`, the claim wire, the judge allowlist
(`api/internal/workersvc/judge_enqueue.go:22`), and every kind label in web/CLI.
The trade-off, stated so nobody meets it by surprise: the two loops would share one
cap ledger row unless it gains a second counter — so add `align_attempt_count` to
`mr_rework_ledger` (`00168`) rather than sharing `attempt_count`, since a conflict
cycle and a review cycle are different loops.

The worker instruction is the align machinery it already has (`agent/src/git.ts:1347`,
merge-first with rebase fallback and the commit-count preservation assertion at
`:1432-1456`), escalating to the agent only for the conflicts git cannot
auto-resolve. Cross-kind collision is already handled: `CreateAutoMRReworkRun`
carries the create-time branch guard and the one-active-per-MR index
(`api/internal/workersvc/mr_rework.go:60`, `:114`), returning `ErrBranchInUse` /
`ErrActiveMRReworkExists` for the detector to swallow.

## Where it lives / what it touches

- `api/internal/forge/forge.go` — `MergeRequest` + neutral mergeability enum + validator
- `api/internal/forge/{gitlab,github,forgejo}.go` — mapping in the existing
  `to*MergeRequest` (no new API calls); plus the forge fakes across the suite
- `api/internal/forgesvc/mr_watch.go` — record the observation beside `recordMRState`
- `api/internal/store/migrations/` — one new migration: `runs.mr_mergeable`,
  `runs.mr_mergeable_since` (M1); `runs.mr_rework_cause`,
  `mr_rework_ledger.align_attempt_count` (M2)
- `api/internal/store/queries/{forge,mr_rework}.sql` — `SetRunMRMergeability`; the
  conflict gate column on `ListMRReworkCandidates`; M2's `ListMRConflictCandidates`
- `api/internal/poller/mr_review_watch.go` — the conflicted-MR suppression gate;
  `api/internal/poller/mr_conflict_watch.go` + `poller.go` wiring (M2)
- `api/internal/notifysvc/service.go` — `MRConflictPayload`;
  `api/internal/settings/settings.go` — M2 kill-switch key
- `api/internal/apitypes/` + `api/cmd/uzi/run.go` + `render.go` — DTO field and CLI marker
- `web/src/lib/api.ts`, `web/src/components/MrChip.tsx` (+ a sibling conflict badge),
  `web/src/pages/{Board,RunView,IssueView}.tsx`, `web/src/mocks/mockApi.ts` and its
  discriminating fixture
- `agent/src/` — M2 only: the `conflict` prompt-context branch over the existing
  `alignBranchWithDefault`
- `docs/board.md`, `docs/mr-review-watcher.md`, `docs/cli.md`, `CHANGELOG.md`

## Scoping notes

- **Size.** M1 is small-to-medium: one migration, one enum + three one-object
  mappings, one record call, one suppression gate, one notification kind, one badge,
  one fixture. The interface-change spread across the three drivers and the forge
  fakes is the usual mechanical tax (PRD #381 counted six fakes) — a milestone, not a
  footnote. M2 is a medium PRD on top, and is a near-clone of `MRReviewWatch` by
  construction.
- **Where the sizing hides a step.** "Zero extra API calls" is a claim about the
  *mr_watch tick*; the *rework suppression* consumes the persisted column, because
  `ListMRReworkCandidates` never talks to the forge. The persistence step (migration +
  `SetRunMRMergeability` + inventory row if the query file is covered by
  `query_inventory_test.go`) is therefore on M1's critical path, not optional polish.
- **Riskiest assumption, and how to validate it in an hour.** That all three forges
  return *usable* mergeability on the object `GetMergeRequest` already fetches. The
  SDK fields exist at the pins (verified upstream), but semantics need a live probe:
  hit one real MR per forge, confirm GitLab's `DetailedMergeStatus` vocabulary and
  GitHub's null-then-value behaviour. If any forge proves unusable, that driver's
  mapping degrades to `unknown` and the feature ships forge-partial (GitLab/Forgejo
  first) rather than blocking — say so in the PRD rather than discovering it at
  implementation.
- **Second risk: false "conflicted".** A wrong positive suppresses a legitimate
  `mr_rework` and nags the owner. Mitigations are already in the design: `unknown` is
  inert, only `conflicted` held past the quiet period acts, and the notification is
  latched per episode. The suppression gate is the one place a false positive costs
  something real — cap the blast radius by making the suppression *and* the M2
  detector honour the same admin kill-switch.
- **What to cut for a v1.** Cut M2 entirely — the auto-realign run is where all the
  token spend, all the conflict-resolution judgement, and all the cap-ledger
  complexity live. M1 alone converts a silent rot into a visible, notified state and
  stops uzi spending on a branch that cannot land, which is most of the value. Within
  M1, the truly minimal slice is: enum + three mappings + one column + the
  `mr_rework` suppression gate, with the badge and notification following.
- **Explicitly out of scope.** Auto-merge (the four guardrail layers hold; `main` is
  never touched). Reporting *which files* conflict — no forge gives it on the MR
  object, and computing it server-side needs a worktree the api does not have (the
  constraint recorded in `adr/0456-rebase-before-finalize-push.md:55`).
  Migration-number collision detection between sibling PRs
  (`.claude/skills/uzi-release/SKILL.md:90-92`) — same family of pain, but it is a
  *cross-PR* property, not a property of one MR, and needs its own idea.
