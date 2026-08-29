# PRD #767 — Assign-to-uzi-bot as a run-eligibility signal (companion to the `uzi` label)

**Issue**: #767
**Priority**: Medium
**Status**: Complete
**Depends on**: #764 (the single eligibility gate this widens). **M1 is independent of #764 and may
land first** (it only adds assignee data; nothing consumes it until M2). M2-M6 extend #764's seam
and must be implemented against its **merged** form — see the rebase-seam call-outs below.

## Problem

After #764, `uzi` is the single label that marks an issue as uzi's to run. But **assigning an
issue to a teammate is the most natural "this is yours to do" gesture on any forge** — it is how
you delegate to a person — and uzi cannot act on assignment at all today. A user who assigns an
issue to the uzi-bot account reasonably expects uzi to treat it as its work; nothing happens.

## Target model

An issue is **uzi's to run** if it **carries the `uzi` label OR is assigned to the uzi-bot
account**. Both are the same single concept (eligibility) expressed two natural ways; they plug
into the one gate #764 builds, so the board, the sweeps, and the autopilot poller all treat them
identically.

**Assignment grants eligibility only — it never implies auto-run** (Decision D1). An issue merely
assigned to the bot sits until a human starts it, exactly like a `uzi`-labelled issue. Every
unattended path still needs one explicit extra opt-in on top of assignment:

- `+ autopilot` label → auto-starts now, unattended, skipping the plan gate (the poller).
- `+ an enabled sweep` → auto-starts on that sweep's schedule.

So the safety model is unchanged: assignment is a lighter, more discoverable way to say "this is
uzi's," and the same deliberate opt-ins gate unattended execution.

## Verified facts (checked against HEAD by two independent reviewers; re-derive line numbers at implementation time)

- **Bot identity is already stored — no new lookup.** `forge_connections`
  (`00002_forge.sql:11-12`) carries `bot_username text NOT NULL` and `bot_forge_user_id bigint NOT
  NULL` — the PAT's own account, resolved at connect (`gitlab.go:86` `Users.CurrentUser` inside
  `VerifyToken`). `00037_autopilot_mapping.sql:6-14` distinguishes `bot_username` (the PAT's bot)
  from `human_username` (the connecting user). **Match on the numeric `bot_forge_user_id`, not the
  name** — immune to a bot rename (D2).
- **Assignees are NOT synced today — this is the bulk of the work.** `forge.Issue`
  (`forge.go:226-235`) carries `IID/Title/State/Labels/Description/Author/WebURL/UpdatedAt`, **no
  `Assignees`**. None of the three mappers reads them: `toIssue` (`gitlab.go:820-840`),
  `toForgejoIssue` (`forgejo.go:724-747`), `toGitHubIssue` (`github.go:1057-1079`). The only
  assignee code in the whole `forge` package is the GitHub **write** path
  (`UpdateIssueRequest{…Assignees}`, `github.go:445`). The `issues` cache (`00002_forge.sql:47-59`)
  has `labels jsonb` + `has_prd_link boolean`, no assignee column.
- **Assignees ride inline on the list/get payloads all three drivers already fetch — no extra API
  call.** GitHub `go-github/v90` `Issue.Assignees []*User`, `User.ID *int64` (nil-guard needed);
  GitLab `client-go/v2` `Issue.Assignees []*IssueAssignee`, `.ID int64` (the `assignees` array is
  returned on all tiers; only *multiple* is Premium; `Assignee` is deprecated); Forgejo/Gitea
  `sdk/gitea` `Issue.Assignees []*User`, `.ID int64`. So `Assignees []int64` normalizes cleanly.
- **`Assignees` is a struct field on `forge.Issue`, not an interface method** — so the six forge
  test fakes (ARCHITECTURE.md) compile unchanged. This de-risks M1 relative to #764's interface
  work.
- **The eligibility gate collapses to one predicate, reached by four wrappers.** `CreateRun`
  (`service.go:4400`), `CreateScheduledRun` (`:4430`), `CreateAutopilotRun` (`:4445`),
  `CreateScheduledAutopilotRun` (`:4470`) all funnel into `createRun` (`:4500`), whose Gate A is
  `isEligibleIssue` (`:4623`, def `:4825`), reading the **cached** issue via `GetIssueByIID`
  (`:4592`). #764 makes this a single `uzi_label` predicate; this PRD widens that one predicate.
- **Every eligibility consumer (complete set):** (1) the create gate above; (2) the sweep candidate
  query `ListSweepCandidateIssues` (`schedules.sql:233`, `labels @> @labels::jsonb`); (3) the
  autopilot poller candidate query `ListAutopilotCandidateIssues` (`autopilot.sql:6-28`); (4) the
  **web** board affordance — the shared predicate `isEligibleCard` (`web/src/lib/boardCards.ts:84`),
  consumed by `Board.tsx:1454` and `IssueView.tsx:125`, and **`canPromote` = `!isEligibleCard`**
  (`boardCards.ts:112-113`, consumed at `Board.tsx:1544`, `IssueView.tsx:126`). The CLI is **not**
  an evaluator (it displays server exit-code mappings); project-sync is **not** a consumer (it seeds
  columns from labels, never the runnable predicate).
- **jsonb numeric-membership trap (correctness).** The label queries match string elements
  (`labels @> …`, `jsonb_exists(labels, @label)`). `assignee_ids` holds **numeric** ids, and
  `jsonb_exists` does **not** match a JSON number — copying the label pattern silently matches
  nothing. The correct form is containment on a number: `assignee_ids @> to_jsonb(@bot_id::bigint)`,
  and it must be proven under live-DB (sqlc's type deduction is not Postgres's).

## Milestones

- [x] **M1 — Sync issue assignees end to end (independent of #764; may land first).** Add
  `Assignees []int64` (forge user ids) to `forge.Issue` and map it in each driver's `ListIssues`
  **and** `GetIssue` from the inline assignee field(s) — a set, normalizing GitHub's `*int64`
  (nil-guarded like the existing author guards) and GitLab/Forgejo's `int64`. Persist it in the
  `issues` cache: a new `assignee_ids jsonb NOT NULL DEFAULT '[]'` column (goose migration, numbered
  at merge time above the live head — `00168` today; `gate:repo`'s `check:migration-numbering`
  guards duplicates), written at sync (`upsertIssues`), inline on create (`handler/issues.go`),
  preserved verbatim on non-sync updates, and **selected by `GetIssueByIID`** so it reaches the gate.
  *Success (live-DB, `./e2e/run-store-it.sh` with positive controls RUN>0 / zero SKIP)*: for each
  driver, an issue assigned to a known account round-trips its id into `issues.assignee_ids` and
  back out through `GetIssueByIID`; an unassigned issue yields `[]`; net-new column + mappings fail
  against pre-change code.

- [x] **M2 — Eligibility: "OR assigned to bot" at the single gate (depends #764).** Widen #764's
  Gate A predicate in `createRun` so an issue is eligible if it carries `uzi_label` **OR**
  `conn.bot_forge_user_id ∈ issue.assignee_ids`, reading the bot id from the repo's
  `forge_connections` row. **Rebase seam**: re-derive against #764's *merged* Gate A shape (today
  `isEligibleIssue(...)`, `:4623-4624`), not the pre-#764 code. Reword the refusal on the messaging
  surfaces #764 enumerates (`handler/workers.go`, `poller/autopilot.go` comment,
  `cmd/server/main.go` Slack copy) to "add the `uzi` label or assign the issue to uzi". *Success*:
  an **assigned-only, unlabelled** issue starts a run on all four create wrappers (pre-change:
  refused not-eligible); a labelled issue still runs; neither → refused with the new message. The
  predicate is unit-tested against a fake store (proves the OR); M1's store-it proves the column
  actually reaches the gate through `GetIssueByIID` (so M2 can't be green while the column is
  silently dropped).

- [x] **M3 — Autopilot poller recognizes assignment (depends #764; shares `autopilot.sql` with it).**
  Widen `ListAutopilotCandidateIssues`' eligibility half to include `assignee_ids @>
  to_jsonb(@bot_id::bigint)`. **Rebase seam + #764 gap**: #764 removes `prd_label` (its D7) but its
  plan never names `autopilot.sql:26` (`jsonb_exists(labels, @prd_label)`); this milestone must
  land against whatever shape #764 leaves — converting that predicate to `@uzi_label` itself if
  #764 did not (raise against #764). **Consent/attribution stays keyed on the `autopilot`-label add
  event**, not on the assignment: `handle`/`eligible` attribute the run to the human who added the
  `autopilot` label and gate on the owner's `human_username` + `autopilot_enabled` + token
  (`autopilot.go:340-358`, `autopilot.sql:30-46`); an assignment has **no adder**, so the consent
  gate must not be bypassed by it. And an assignment produces **no `LabelEvent`**, so
  assignment-eligibility is seen only on each tick's candidate scan, never on a transition — add a
  test for "autopilot label present, bot assigned afterwards" (not swallowed by a stale trigger row,
  not double-fired). *Success*: paired in one fixture — (positive, fails pre-change) an issue
  assigned + `autopilot`, no `uzi` label, is auto-started by the poller; (negative, calibrated by
  mutation — dropping the `autopilot` predicate must redden it) the **same** issue with `autopilot`
  removed is detected-eligible but **held**, no run created. Live-DB (`run-store-it.sh`) for the new
  predicate.

- [x] **M4 — Sweeps recognize assignment + an assigned-queue catalog default (depends #764).**
  "Assigned to the bot" is a **new selector kind, not another label** — the sweep selector is
  modeled today purely as a jsonb label array (`resolveSweepLabels`, `scheduler.go:707-729`, which
  defaults an empty selector to `[PRD]` and enforces non-empty; passed as `Labels: labelsJSON`,
  `scheduler.go:368`). So this milestone adds: (a) a bot-id param + `assignee_ids @>
  to_jsonb(@bot_id::bigint)` path on `ListSweepCandidateIssues`/`CountSweepCandidateIssues`; (b) a
  representation of a non-label ("assigned") selector in the schedule row and in `schedtmpl` catalog
  parsing; (c) bypassing the empty→`[PRD]` default for the assigned kind. Add an **`assigned-sweep`
  catalog default** whose selector is "assigned to the uzi-bot" — self-gating (selector = assignment
  = eligibility, no `PRDLESS`/PRD dance), **opt-in per repo** like every catalog default,
  **`auto_approve` ON** to match the existing `bug-triage`/`planned-sweep` defaults (D3 — enabling
  the sweep is itself the deliberate opt-in; see the decision for why not OFF). *Success (live-DB,
  `run-store-it.sh`, positive controls)*: an enabled assigned-sweep starts runs for bot-assigned
  open issues on its schedule; a non-assigned candidate is a benign skip that advances the schedule;
  the numeric bot id actually matches (guards the jsonb trap).

- [x] **M5 — Web: mark assigned-to-bot as runnable, in the marker, filter, and Promote (depends
  M1).** Widen the **shared** `isEligibleCard` predicate to "carries `uzi` OR bot ∈ assignees" so
  the runnable marker, the `uzi`-only filter (now reads as "uzi's"), **and `canPromote`
  (`!isEligibleCard`)** all inherit — an assigned-but-unlabelled card is marked runnable and stops
  offering Promote. This needs new plumbing the board does not have today: add `assignee_ids` to the
  card DTO (`board.go cardDTO`, `:44-70`) and ship the connection's `bot_forge_user_id` to the SPA
  bootstrap (`auth.go:412` ships `run_eligible_labels` but no bot id). Vitest, with a **positive**
  assertion that an assigned-unlabelled card is runnable and offers no Promote. *Success*: an issue
  assigned to the bot shows as runnable, passes the "uzi's" filter, and hides Promote, with no label.

- [x] **M6 — CLI check, docs, specs, close-out.** Confirm `api/cmd/uzi/` eligibility wording
  reflects "label or assignment" (server-inherited; display only). Docs: `docs/scheduling.md` (the
  assigned-sweep default + the non-label selector kind), `docs/admin-settings.md`, the eligibility
  doc from #764 (assignment as a second signal + the D1 auto-run pin), the default-jobs docs (the
  `assigned-sweep` entry); `ARCHITECTURE.md` (eligibility + forge assignee sync), `specs/ai.md`
  (decision record incl. the trust-equivalence check D6), the CHANGELOG, and the in-repo
  `.claude/skills/issue-triage` + `.claude/skills/uzi-watcher` gating mechanics (assignment is now a
  second way to be eligible). `task gate:api`, `gate:web`, `gate:repo` green (+ store-it for
  M1/M3/M4). Move this PRD to `prds/done/`. *Success*: docs, CLI, and skills describe assignment as
  an eligibility signal and the assigned-sweep default; gates green.

## Out of scope

- **Removing or changing the `uzi` label** — assignment is *additive*; both mean the same thing.
- **uzi auto-assigning the bot** (on run start or MR open) — this PRD only *reads* assignees for
  eligibility (D5). Auto-assign is deferred.
- **Per-catalog-job `auto_approve`** — with D3 shipping the assigned-sweep auto-approve ON (matching
  the others), the package-level `AutoApprove` constant is untouched; making it per-job is a
  separate future nicety, not needed here.
- **`.github/workflows/**` changes** — none needed; the branch stays worker-pushable.
- **Co-assignee semantics beyond membership** — "is the bot one of the assignees" is the only
  question; a human co-assignee alongside the bot does not change eligibility.
- **Enabling the assigned-sweep by default on connect** — it ships opt-in, like every default job.

## Success criteria (whole PRD)

1. Assigning an open issue to the uzi-bot makes it runnable — manually and, if the assigned-sweep is
   enabled, on that schedule — with **no label applied**.
2. Assignment **alone** never auto-runs: an assigned issue with neither `autopilot` nor an enabled
   sweep stays at rest until a human starts it (asserted as the paired negative in M3).
3. A linked PRD is still detected/implemented and badged (inherited from #764), independent of how
   the issue became eligible.
4. `task gate:api`, `gate:web`, `gate:repo` green; the assignee sync, the widened gate, the
   assigned-sweep, and the poller behavior are each covered by tests that **fail against pre-change
   code**; every new SQL predicate (M1 round-trip, M3 poller, M4 sweep) is exercised under
   `./e2e/run-store-it.sh` with positive controls, and each proves a **numeric** bot id matches.

## Risks

- **R1 — Assignee portability across forges.** GitHub multi, GitLab CE single (array on all tiers),
  Forgejo array. *Mitigation*: model `Assignees` as a set of ids; assignees ride inline on the
  payloads already fetched, so no extra API call; GitHub's `*int64` is nil-guarded. M1 tests all
  three.
- **R2 — Assignment silently turning into auto-run.** *Mitigation*: D1 — assignment grants
  eligibility only; auto-run needs `autopilot` or an enabled sweep. M3 asserts an
  assigned-without-`autopilot` issue is held (mutation-calibrated).
- **R3 — jsonb numeric-membership trap.** Copying the string `jsonb_exists`/`@>` label pattern
  matches no numeric assignee id. *Mitigation*: use `assignee_ids @> to_jsonb(@bot_id::bigint)`;
  every milestone with the predicate carries a store-it assertion that a numeric bot id matches.
- **R4 — Assigned sweep is a new selector kind, not an OR.** The selector data-model is label-only.
  *Mitigation*: M4 explicitly adds the non-label selector representation + query param + default
  bypass; scoped as its own milestone.
- **R5 — #764 shared-seam / rebase risk.** M2 (Gate A), M3 (`ListAutopilotCandidateIssues`), and the
  messaging sites are all edited by #764 too, and #764's plan does **not** name `autopilot.sql`'s
  `@prd_label` predicate. *Mitigation*: implement M2-M6 against #764's **merged** form; M3 owns the
  `@prd_label`→`@uzi_label` conversion if #764 left it; raise the gap against #764's MR.
- **R6 — Cache lag, both directions.** Eligibility reads cached `assignee_ids`; assigning on the
  forge then immediately clicking Start hits sync lag (as any label change does), and **unassigning
  to revoke** eligibility also lags — a de-scoped issue stays fireable until the next sync.
  *Mitigation*: documented as the mirror of #764's R5; a forge-first Promote-style path could update
  the cache in the same request if we later want instant assignment from uzi's own UI.

## Decision Log

- **D1 — Assignment grants eligibility only; it never implies auto-run.** An assigned issue is
  runnable but sits until a human starts it. Unattended execution still requires `autopilot` or an
  enabled sweep — the same opt-ins that gate a `uzi`-labelled issue. Safety model identical to the
  label.
- **D2 — Match on `bot_forge_user_id` (numeric), not the bot username.** Stable across a rename,
  already stored per connection, unambiguous across forges.
- **D3 — The assigned-sweep catalog default is `auto_approve` ON, matching the existing sweeps
  (reversed from the draft's OFF).** Reviewers established that per-job auto-approve does not exist:
  `schedtmpl.AutoApprove` is a package-level compile-time constant applied to every default job, the
  parser rejects unknown frontmatter keys (a `auto_approve: false` line panics the binary at boot),
  and five seed/DTO sites plus `defaultEditableDiverges` read the constant — so OFF would require
  net-new plumbing for a single job and would flag the seeded row as diverged-from-catalog. Against
  that cost: **enabling the sweep is itself the deliberate auto-run opt-in** (D1 already keeps bare
  assignment from running), and ON is consistent with `bug-triage`/`planned-sweep`. So ship ON;
  per-job auto-approve is a separate future change if ever wanted. *(Flagged for the user: this
  reverses the OFF default proposed during design — trivial to revisit, but it now costs a plumbing
  milestone.)*
- **D4 — Assignment is additive to the `uzi` label, at the same single gate.** Eligibility is "label
  OR assigned," computed in the one place #764 centralizes, so all consumers inherit it — one concept,
  two expressions, not a second independent path.
- **D5 — Read assignees only; do not auto-assign.** uzi *acts on* assignment; it does not *set* it.
- **D6 — Assign-bot ≈ apply-`uzi`-label is a checked equivalence, not an assumption.** Eligibility by
  assignment is exactly as powerful as by label, so the safety argument rests on the two needing the
  same forge permission tier. M6 verifies per forge that assigning the bot requires the same tier as
  labelling (on GitLab an issue author can sometimes edit assignees at a lower tier than labels —
  confirm and record); if a forge diverges, note it rather than silently assume parity.
