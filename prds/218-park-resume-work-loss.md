# PRD #218 — A usage-limit park loses the agent's work

**Issue**: [#218](https://gitlab.example.com/vtmocanu/uzi/-/issues/218) · **Label**: PRD · **Priority**: High
**Area**: `agent/src/runner.ts` (the park path and the cleanup carve-out) · `agent/src/git.ts` (`runnerCloneForBranch`, `fetchAgentBranch`) · `specs/ai.md` + `prds/done/35-run-limit-retry.md` (M5) · `adr/0035-run-limit-retry.md` (**M6 only** — see M5 for why it is not an M5 site).
**Line references** are against `136d976a`.
**Status**: not started.
**Evidence**: measured 2026-08-03 against the live stack, run `a146df98` on example-app issue #78, from the run feed and the code.

## Problem

PRD #35 Decision 6a preserves exactly three things when a run parks on an
Anthropic usage limit, and the carve-out that does it
(`agent/src/runner.ts:766-832`) is emphatic about why all three are needed:
*"Preserving only some of them would resume into a session missing its plugins or
its worktree."*

**Two of the three work. The third is undone by the very next claim.**

`createOrAttachRunnerClone` → `runnerCloneForBranch` opens with an
**unconditional** removal (`agent/src/git.ts:329-332`):

```js
return this.withLock(barePath, async () => {
  const repoDir = path.basename(barePath).replace(/\.git$/, "");
  const clonePath = path.join(this.runnerRoot, repoDir, key);
  await fs.rm(clonePath, { recursive: true, force: true });   // <- the preserved clone
```

and then re-seeds the branch from **origin**, never from disk
(`git.ts:343-346`): `refs/remotes/origin/<branch>` if that ref exists, else the
repository's default branch.

**A parked run has published nothing of its own.** (Narrowly: `origin/<branch>`
may exist from a *prior completed* run on the same issue, and then the reseed's
first leg fires and that work survives — see "A pushed branch would have
survived" below. What is never at origin is anything *this attempt* committed,
and which of the two holds is exactly what decides which M2 leg fires.) The push
is the last thing a run does,
after the agent signals done — `runner.ts:632-645`, *"work complete; pushing
branch and opening merge request"*. So does the fetch-back into the worker's own
bare (`runner.ts:556`, `git.fetchAgentBranch`). Neither runs on the park path,
which is a `catch` (`runner.ts:700`). The agent's commits exist in exactly one
place, the runner clone, and the resume deletes it.

### Measured, on a real run

Run `a146df98`, example-app issue #78, 2026-08-03:

| time (UTC) | event |
|---|---|
| 12:30:52 | first clone: *"detected **8** agent(s) in the repo's `.claude/agents/`"* (feed seq 2) |
| 14:38:06 | `five_hour` limit after **61 turns**, `duration_ms: 7324373` (2h02m), `total_cost_usd: 110.74` |
| 14:41:14 | re-clone: *"detected **10** agent(s)"* (seq 2580) — a different, newer `main` |
| 14:41:xx | the lead, seq 2591: *"The worktree was reset to a fresh clone — the previous session's commits are gone (`src/f5/devices.ts` and `src/net/egress.ts` no longer exist, `src/net/example-proxy.ts` is back). Rebuilding…"* |
| 15:31:08 | **it parked again** (seq 3269, `resets_at 17:40Z`); `runs.limit_wait_count = 2`, so the measured loss is not a single event |

Two artefacts are stronger evidence than any of the above and both sit in feed
seq 2589/2590: the new clone's reflog holds exactly two entries (`clone:` then
`checkout:`), and `git cat-file -t ac2360d` on the prior session's commit returns
`fatal: Not a valid object name`. The objects are **gone from the store**, not
merely unreferenced.

`glab api projects/vtmocanu%2Fexample-app/repository/branches?search=agent/issue-78`
returns nothing, and the run row carries `branch: null`, `mr_iid: null`. So the
reseed took the default-branch leg, which is also why the roster moved from 8 to
10: the run resumed onto two hours of newer `main` it had never seen.

### What makes this worse than "some work was redone"

**The resume looked healthy.** The session resumed with full context — the feed
carries no *"this run was picked up again, but its earlier session could not be
found on this worker"* (`runner.ts:361-372`), so `sessionTranscriptResolvable`
passed and the SDK transcript under the preserved HOME was used. The plan was
preserved too (seq 2582). Every visible signal said resume; only the disk was
empty.

**Nothing told the user.** The lead's *"Rebuilding…"* is not a worker status, it
is the model noticing on its own by reading the tree. The one mechanism that
would have warned it, `priorWork` (`runner.ts:380-383`), requires
`resumeDropped && runnerClone.priorCommits > 0` — and here `resumeDropped` was
false (the session was fine) and `priorCommits` was 0 (nothing at origin to
count). Both guards are keyed on the *session* being lost; this is the case where
the session survives and the *tree* is lost, which no existing signal covers.

**A pushed branch would have survived**, since the reseed bases off
`origin/<branch>`. So the exposure is precisely the window between a run's first
commit and its final push — which is nearly the whole run.

## Solution

Make the park durable **inside the worker**, using the primitive the done path
already uses, and teach the reseed to read it.

**M1 — fetch back on park.** `git.fetchAgentBranch(barePath, clonePath, branch)`
(`git.ts:484-494`) fetches `+refs/heads/<branch>:refs/uzi-runner/<branch>` from
the runner clone into the worker's bare over the hardened file/pack transport.
It is the one point the worker reads a runner-controlled store and carries six
stated B2 invariants for exactly that reason. Calling it on the park path adds no
new trust-boundary crossing — it is the same call at a different time — and it
moves the agent's commits somewhere the next `fs.rm` cannot reach.

**M2 — the reseed prefers that record.** `runnerCloneForBranch` resolves its base
from `refs/remotes/origin/<branch>` or the default branch; add
`refs/uzi-runner/<branch>` as a candidate. **The rule is per-leg, and that is the
whole of it:**

| state | base |
|---|---|
| no tracking ref (today's only case) | unchanged: `origin/<branch>` if it exists, else the default branch |
| `origin/<branch>` exists **and** a tracking ref | the tracking ref only when `git merge-base --is-ancestor origin/<branch> <trackingRef>` succeeds; on divergence, origin. Another worker may have pushed, and silently preferring local work would drop a published commit. |
| no origin branch, a tracking ref (**first park**) | the tracking ref, with **no ancestry test** — there is no competing published work to drop and the current default tip is not a meaningful reference |

**🔴 THE LAST ROW NEEDS ONE MORE CONDITION, AND WITHOUT IT THE FIX INTRODUCES
ISSUE #105's OWN FAILURE.** Nothing in `agent/src/` ever deletes
`refs/uzi-runner/<branch>`: the only writer is `git.ts:485` (a force refspec), the
only reader `git.ts:281`, and `removeRunnerClone` touches the clone dir alone. The
bare is a persistent per-worker PVC. So a run that parks (M1 writes the ref) and
then dies permanently — `RUN_LIMIT_MAX_WAITS` exhausted, cancelled while parked, a
failed push — leaves that ref behind with no origin branch to shadow it. A later,
**fresh** run on the same issue landing on the same worker would then hit row 3,
seed off a dead run's commits, and start with a session that has no memory of
them: exactly the *"continuing WITHOUT its earlier context, so some work may be
repeated"* shape (`runner.ts:361-372`) that M3 exists to prevent, arriving through
the fix. **So row 3 is additionally gated on the claim being a RESUME** — the
claim's `session_id` matching, or `limit_wait_count > 0` — or the ref is scoped by
run id. Choose one in M2 and say which; do not leave it implied.

**🔴 A single uniform ancestor test against "the origin-or-default candidate" is
WRONG, and it is wrong on exactly the case this PRD measured.** A first draft
wrote it that way; the review refuted it on run 78's own numbers. `ensureClone`
re-fetches the bare on every claim (`git.ts:246-257`, called at `runner.ts:304`)
and `fetch()` runs `remote set-head origin --auto` (`git.ts:547-552`), so
`defaultBranchRef` resolves a **fresh** `origin/HEAD` and the default-branch
candidate at resume is a tip the parked work never saw.

Run 78's resume checked out base `65496d0e`, committed
**2026-08-03T13:09:02 UTC** — 38 minutes *after* the 12:30:52 first clone. The
DAG settles it without relying on clocks at all: example-app `main` ran
`75542653` (11:49:03Z) → `66594623` (11:55:11Z) → `a38f7421` (11:58:22Z) →
`65496d0e` (13:09:02Z), so the tip at first clone was `a38f7421`, and
`65496d0e`'s `parent_ids` is exactly `[a38f7421]` — its **direct child**. Work
forked at `a38f7421` therefore cannot have `65496d0e` as an ancestor,
`--is-ancestor` returns non-zero, and the uniform rule discards the very work M1
just saved. It would have worked only where a branch was already pushed, i.e.
where prior work survives today anyway, and failed where the loss is total.

**That whole chain is checkable from GitLab alone, with no database**, which
matters because every other figure in this section needs a `runs`/`run_messages`
query nobody will be able to run in a month: `.claude/agents/` holds exactly **8**
files at `a38f7421` and exactly **10** at `65496d0e` (the additions are
`release.md` and `researcher.md`, named in `65496d0e`'s own commit message), which
corroborates the feed's 8→10 roster and "a different, newer `main`" from a primary
source. `glab api projects/vtmocanu%2Fexample-app/repository/commits/65496d0e` and
`…/branches?search=agent/issue-78` (which returns `[]`) are the two calls.

**M3 — say it either way.** A resume that recovers prior work should say how much;
one that cannot should say so, in a worker status rather than by the lead noticing.

### Why not reuse the preserved clone in place

It is the obvious fix and it is the wrong one. The `fs.rm` at `git.ts:332` is
deliberate on two counts: staleness (`git.ts:317-318` — a stale dir is *"simply
removed and recloned"*) and, load-bearing here, **ownership** — the seed is a
trusted-source local clone (`git.ts:320-327`) whose tree is runner-owned
(`:357-361`), so attaching to one the worker did not just create means trusting a
directory an untrusted uid could have replaced.
Fetching the commits out and re-cloning keeps the existing ownership model
untouched, which is why M1/M2 are two small additions rather than a rework of the
clone lifecycle.

### 🔴 The fix makes Decision 6a's clone leg redundant, and that is a finding, not a side effect

Once the commits live in the bare, preserving the clone directory buys nothing:
the reseed recreates it at the **same path** (`runnerRoot/<repo>/issue-<iid>`), so
the SDK session — keyed by HOME *and* cwd (`runner.ts:320-322`) — still resolves,
and `skillsPluginDir` is a *sibling* of the clone
(`agent/src/skills-plugin.ts:49-51`, `path.dirname(worktreePath)/.uzi-skills-…`)
so the `fs.rm` never touched it in the first place. The clone leg of the carve-out
has therefore been a no-op since it was written, and a parked run holds a full
working tree for up to the 8-day `RUN_LIMIT_MAX_PARK`
(`api/internal/config/config.go:677`) — not the seven days of Anthropic's own
window, which `runner.ts:758` is about and which is a different quantity. **M6
removes it — after M1/M2 are validated, never before**, since removing it first
would delete the clone the fetch-back has to read.

Both halves of this were verified rather than argued: the clone path carries *"no
runId, no attempt counter, no nonce, so a re-claimed run reuses the same clone
dir"* (`agent/src/sdk-session.ts:62-65`), and run 78 is the live control — its
clone was destroyed, recreated at the same path, and the session still resolved
(feed seq 2582, and it ran on to seq 2586+ rather than dying on turn 1 the way
`runner.ts:322-324` says an unresolvable id does).

## Milestones

- [ ] **M1 — fetch the agent branch back on the park path.** Between the limit
      being caught (`runner.ts:700`) and the cleanup finally, run the same
      fetch-back the done path runs at `runner.ts:556`. Best-effort: a failed
      fetch-back must park the run anyway, since a park that fails is worse than a
      park that loses work.

      **The reap ordering is already free, and a first draft made it a task.**
      `LimitReachedError` is thrown in `driveTurn` (`sdk-executor.ts:1489`) and
      propagates through `run()` (`:340`), whose `finally` calls `killAgentTree`
      at **`sdk-executor.ts:1095`** — explicitly *"Covers the failure/cancel/no-plan
      paths too, not just the runner's explicit pre-push call."* So the tree is
      dead before `runner.ts:695`'s catch is entered, and the safety requirement
      is satisfied anywhere in the catch or the finally. There is no done-path
      ordering to copy; `runner.ts:527`'s explicit call is a second, belt-and-braces
      one at the security boundary.

      **Two mechanical constraints the implementer will hit immediately.**
      `runnerClone` is declared **inside** the `try` at `runner.ts:309` while
      `barePath` (`:289`) and `worktreePath` (`:290`) are hoisted, so the branch
      name needs a hoisted `let` — the source of truth is `runnerClone.branch`,
      there is no `result` on this path. And `handleLimitReached` closes the
      batcher at `runner.ts:942` (`:927` on the opt-out leg) **before** `parked` is
      known, so the fetch may live there but nothing M3 wants to *say* about it can
      be emitted after `runner.ts:700`.
- [ ] **M2 — the reseed considers the tracking ref**, per the table above, and
      **decides and states how a stale tracking ref from a permanently-dead run is
      excluded** (resume-gated vs run-id-scoped — the red-flagged paragraph above
      is the argument, and leaving it implied reintroduces issue #105's failure
      through the fix). Feed the chosen base into `priorCommits`/`defaultBranchCommit` so the
      existing seeding log line (`git.ts:355`) stays honest about which leg fired.
      **This inverts `priorCommits`' documented contract**, which `git.ts:134-144`
      states outright: *"NOT 'the commits the interrupted attempt made'… What lands
      here is prior PUSHED work."* Either correct that comment in the same commit
      or introduce a separate field — load-bearing, because M3 turns this number
      into lead-facing prompt text.

      **M2 also changes which agent roster a resumed run sees.** The 8→10 move is
      read above as a *symptom* of the default-branch leg firing; it is also a
      product of the fix. `parseRepoAgents` re-derives the roster from whatever
      tree the reseed chose, so after M2 run 78's first-park resume would seed off
      the tracking ref and keep the roster its parked tree carried — `a38f7421`'s
      8 files, which the run never edited — rather than acquiring `65496d0e`'s 10
      (shas and counts are in the "checkable from GitLab alone" passage above).
      Strictly it is the parked tree's roster, not the base commit's, and the two
      coincide only because nothing in the run wrote under the clone's
      `.claude/` (the SDK's own transcript lives under `/data/agent-home/<run-id>/`
      and is a different tree). That is
      correct — a resume that keeps its tree should keep
      the roster that tree was planned against — and it closes the reverse hazard,
      where a resume adopts a half-edited or role-dropping `.claude/agents/` that
      landed on the default branch mid-run. What moves is the SELECTED roster on
      an `--agent-source repo` run; `parseRepoAgents` runs unconditionally either
      way, so the detection status at `runner.ts:1011` still tracks the base even
      on an `own`-source run.
- [ ] **M3 — the feed states what the resume recovered.** A worker status naming
      the recovered commit count when M2's tracking-ref leg fires, and one
      admitting the loss when a resume finds neither an origin branch nor a
      tracking ref. Widen `priorWork` (`runner.ts:380-383`) so the lead is told
      about prior commits whenever they exist, not only when `resumeDropped`.
- [ ] **M4 — tests.** Unit: the park path fetches back and the done path is
      unchanged; **both** legs of M2's rule, including the two that a uniform
      ancestor test gets wrong — first park with a MOVED default branch (tracking
      ref must win) and a pushed branch that diverged (origin must win); a failed
      fetch-back still parks. Regression: park with local commits, resume, assert
      the commits are present — the test that would have caught this. The home for
      all of it is `agent/test/runner-usage-limit-park.test.ts`, which already
      holds the carve-out tests. Mind `agent/`'s per-FILE `--test-timeout`
      behaviour recorded in CLAUDE.md.
- [ ] **M5 — docs.** `specs/ai.md:13840-13844` states Decision 6a's three-way
      preservation as though it works (*"THREE removals, not two, and preserving
      only some of them resumes into a session missing its worktree or its
      plugins"*); correct it with the evidence and the date. So does the carve-out
      comment at `runner.ts:766-777`, which is where the claim is enforced, and
      `prds/done/35-run-limit-retry.md:68-71`, where it originates — the last is a
      `prds/done/` file and so may fall under the repo's past-tense-record
      convention, which is a call to make explicitly rather than by omission.
      **`adr/0035-run-limit-retry.md` needs NO edit here**, contrary to a first
      draft: it is deliberately narrow (`:279-280`) and says nothing about the
      three-way preservation. Its one adjacent sentence is a pointer at
      `:284-287` about the *acknowledgement condition*, which is true today and
      stays true after this PRD — but its word "three" becomes "two" at M6, so the
      edit belongs there.
- [ ] **M6 — drop the clone leg of the carve-out.** Only after M1/M2 are validated
      on a real park. Removes **`&& !parked`** from the `removeRunnerClone` guard
      at `runner.ts:791-797` — not the whole condition, since `worktreePath` is the
      undefined guard and dropping it calls `removeRunnerClone(undefined)` —
      leaving the plugin-dir and HOME legs untouched; updates the
      *"preserving its clone, plugin dir and
      HOME"* log line at `:831`; and updates `adr/0035:284-287`'s "three
      filesystem cleanups" to two. Frees a full working tree per parked run, for
      up to the 8-day `RUN_LIMIT_MAX_PARK` (`api/internal/config/config.go:677`).
- [ ] **M7 — validate on dev-cluster.** Per the k8s-first convention, and this one
      genuinely needs it: the worker bare lives on `/data`, a per-worker PVC in
      k8s (`git.ts:252`), and R1 below is about exactly that boundary. Run 78 is a
      better subject than its evidence table suggests — it parked **twice**
      (`limit_wait_count = 2`, the second at 15:31:08 with `resets_at 17:40Z`).

## Success criteria

1. A run that parks with local commits resumes with those commits present. The
   control is the one this bug failed: a file the agent created before the park
   still exists after it.
2. The resume no longer silently rebases onto a default branch that moved,
   whenever a tracking ref is available.
3. When prior work genuinely cannot be recovered, the **feed** says so before the
   lead does.
4. A run whose branch WAS pushed never loses a published commit: origin wins on
   divergence, and the tracking ref is used only when it strictly descends from
   origin. **Not "behaves exactly as today"** — in the descends case the base
   legitimately moves from `origin/<branch>` to the tracking-ref tip, so a test
   asserting `base == origin/<branch>` would pin the bug rather than the fix.
5. The done path and the requeue path are unchanged in behaviour. **The e2e stub
   flow is NOT untouched, and saying so would be false**: `StubExecutor` has its
   own `LimitReachedError` throw (`agent/src/executor.ts:719`, the
   `STUB_LIMIT_SENTINEL` path) and `e2e/run-e2e.sh:1442,:1518,:1548` drive three
   scenarios through it, so M1's fetch-back runs there too and a later stub resume
   takes the tracking-ref leg. What must hold is that the stub's **non-limit**
   scenarios are unchanged and the limit ones still park. (R5 is vacuous there:
   `StubExecutor` implements no `killAgentTree` — optional at `executor.ts:223` —
   because it has no tree to reap.)

## Risks

- **R1 — a cross-worker resume still loses the work, and this fix cannot change
  that.** `refs/uzi-runner/<branch>` lives in the claiming worker's own bare on
  its `/data` PVC (`git.ts:252`, `:386`). A resume that lands elsewhere finds
  neither the tracking ref nor the SDK transcript. That case already degrades
  honestly for the session (`runner.ts:361-372`); M3 makes it honest for the tree
  too. **The exposure is wider than "a resume that lands elsewhere" suggests**:
  same-worker resume is a **2-minute grace**, not a guarantee —
  `WORKER_AFFINITY_GRACE` defaults to `2*time.Minute`
  (`api/internal/config/config.go:670`, consumed at `service.go:827`) and
  `runtime.sql:436-437` says that after it lapses *"any of the user's workers may
  claim it"*. A park that lasts hours is far past it; run 78 returned to its own
  worker by a route PRD #216 documents, not by a guarantee. Making work
  recoverable across workers means pushing to the forge, which is R2.
- **R2 — pushing on park is NOT in scope, and it has a closed decision record that
  must be engaged rather than re-opened.** PRD #110
  (`prds/110-checkpoint-agent-work.md`, *"Closed — will not implement"*) rejected
  push-during-run on a PAT-disclosure argument. **That objection may not survive
  at park time**, for a reason this PRD already establishes: #110's case rests on
  a checkpoint being mid-run so the agent tree cannot be reaped, and at park the
  tree is *already* dead (`sdk-executor.ts:1095`), which restores exactly the
  temporal closure #110 says a checkpoint cannot have. So a park-time push is
  probably stronger than "worth its own PRD later" — it survives a cross-worker
  resume and a worker's death, which the tracking ref does not. It still
  publishes half-finished work, creates branches for runs that may never produce
  an MR, and needs the push and MR paths decoupled. It stays out of scope so
  M1/M2 can be validated alone, and whoever picks it up starts from #110's
  argument. (#110 cites `runner.ts:325-331` and `:385` in the present tense;
  both have since moved to `:527` and `:639`.)
- **R3 — UNCOMMITTED work is still lost, and the fix does not pretend otherwise.**
  `fetchAgentBranch` fetches `refs/heads/<branch>`, so a limit landing mid-edit
  loses whatever is not committed. On run 78 the lead commits at wave boundaries,
  so the exposure is real but bounded. An auto-commit on park is deliberately not
  proposed: a snapshot of a half-applied edit that later gets built on is a worse
  failure than a clean loss, and it would need a marker the agent understands.
- **R4 — the park path is a `catch`, so a new failure there is a new way to lose a
  park.** Hence M1's best-effort requirement. The measured consequence of a lost
  park is worse than the measured consequence of a lost tree: the run fails
  outright rather than resuming diminished.
- **R5 — reading a store an untrusted uid is mutating.** The hazard is real: if
  the fetch-back ran while the runner still held the clone, the worker would read
  a store under concurrent mutation. **It is already closed on this path**, by
  `sdk-executor.ts:1095`'s `finally` reaping the tree before the catch is entered
  (see M1) — so this is a constraint to preserve, not work to do. A first draft
  said M1 "must copy the ordering" from the done path, which describes an ordering
  that does not exist there to copy.

## Out of scope

- Pushing to the forge on park (R2), and anything that changes when an MR opens.
- Reusing the preserved clone directory in place, for the ownership reason above.
- Auto-committing uncommitted work at park time (R3).
- Any change to PRD #217's credential selection. The two PRDs touch the same park
  and are independent: #217 is about *which token* the resume spends, this is
  about *what tree* it resumes onto. Neither blocks the other.
- **Steering-channel staleness — real, measured, and NOT a risk of this fix.**
  Tracked as [#222](https://gitlab.example.com/vtmocanu/uzi/-/issues/222); this
  bullet stays the canonical write-up and that issue points back at it. A
  follow-up queued while a run is parked is drained by `pullFollowUp`
  (`sdk-executor.ts:1048`) inside the implement loop, after the reseed's `fs.rm`,
  so a correction written against the parked tree reaches a session that no longer
  matches it. Measured 2026-08-03: we sent a push-your-branch follow-up to both
  parked runs — `a146df98` (example-app #78) and `edbc3884` (**uzi #209**) — and the one
  to `edbc3884` cites *"your fix wave (committed 15:28)"*, commits today's
  behaviour destroys before the message is read. The durable anchors are the
  input rows themselves — **id 43** (`created_at 2026-08-03T16:41:03.144954Z`)
  and **id 44** (`16:44:44.761582Z`) — not their `consumed_at: null` at the time
  of writing, which the worker's consume-on-read `GET /inputs`
  (`workersvc/service.go:2467`) stamps the instant either run resumes. The instruction
  stays correct; its premise does not, and the agent has no way to tell. It
  belongs in its own issue rather than here on three counts: it is anchored to the
  bug, not to anything M1/M2 introduce; it fires on any re-clone, including a
  requeue or a dropped session, so it is not park-specific; and every post-fix
  residual we can enumerate is already covered by R1, R3, or D2's force-update
  note (an exhaustiveness claim we cannot prove complete, so read it as "no
  exposure we could find" rather than a closed set). Note also that M3 cannot
  close it as drafted:
  the worker status is feed-facing, and `priorWork` renders only through the three
  plan-prompt builders (`prompt.ts:528`, `:749`, `:872`), which the `preApproved`
  resume skips entirely (`sdk-executor.ts:702-707`) — so on exactly the measured
  path, an agent-facing emission would be new work, not a reordering. Whoever
  picks it up has a natural home: `buildImplementPrompt` already carries
  first-turn-only facts (`prompt.ts:608-650`), and the ordering there is free,
  since `pullFollowUp` runs at the END of an iteration and turn 1 therefore
  cannot carry a QUEUED follow-up. (Only queued: the `ask_user` path also writes
  `followUp`, at `sdk-executor.ts:1036`/`:1042`, each with `iteration--; continue`,
  so turn 1 can carry the lead's own answered question — a different channel.)

## Related

- **PRD #217** — the same park, the other axis (which credential the resume
  spends). Independent.
- **PRD #110** — the closed decision record on pushing during a run. R2's
  starting point, not a settled objection at park time.
- **PRD #216** — dissects the *same* run and the *same* 14:41:12 re-claim
  (`prds/216-worker-load-balancing.md:29-32`), and independently records that it
  returned to its prior worker, so run 78's loss was **not** a cross-worker
  effect. It also rewrites `ClaimRun`, which is where R1's affinity grace lives:
  a live dependency in both directions.

## Decision log

- **D1 — fetch back into the worker bare rather than push to origin.** Cheapest
  durable store that survives the `fs.rm`, uses an existing hardened primitive,
  and publishes nothing. Accepts R1's cross-worker limitation deliberately.
- **D2 — the ancestor test applies to the `origin/<branch>` leg ONLY.** There,
  divergence means someone else pushed and preferring local work would drop a real
  commit to save a recoverable one. On the first-park leg there is no published
  work to protect and the current default tip is not a meaningful reference — the
  measured case proves a uniform test discards the work outright. Note
  "recoverable" is true only at the instant of the decision: the tracking ref is
  force-updated (`+refs/heads/<branch>`, `git.ts:490`), so once origin wins and the
  agent commits again, the next fetch-back overwrites the parked work for good.
- **D3 — do not reuse the clone in place.** The `fs.rm` guards staleness
  (`git.ts:317-318`) and, more importantly, ownership: a trusted-source local
  clone (`:320-327`) with a runner-owned tree (`:357-361`).
- **D4 — best-effort fetch-back.** A park that fails is worse than a park that
  loses work (R4).
- **D5 — the clone leg of Decision 6a is removed, but LAST.** It has been a no-op
  since it was written, and M1 temporarily depends on it. Ordering is the whole
  decision.
- **D6 — no auto-commit on park.** A half-applied edit that survives is worse than
  one that does not (R3).
