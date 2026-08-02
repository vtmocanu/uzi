# PRD #196 — Configurable board membership and run-eligible labels

**Issue**: [#196](https://gitlab.example.com/vtmocanu/uzi/-/issues/196) · **Label**: PRD · **Priority**: Medium
**Area**: `web/src/lib/boardCards.ts` + `web/src/pages/Board.tsx` + `web/src/pages/IssueView.tsx` (render filter and card affordances) · `api/internal/settings` (three new keys) · a new per-user board-preference table · `api/internal/workersvc/service.go` (the run-eligibility gate) · `web/src/pages/AdminSettings.tsx`.
**Mockup**: [`prds/mockups/196-board-membership-labels-mock.html`](mockups/196-board-membership-labels-mock.html) — seven sections, reviewed and approved 2026-08-02.
**Line references** are against `a87fd521`.
**Status**: open. **M4 (eligibility) is the milestone that touches a guardrail-adjacent gate and must be reviewed on its own**, with two named guard tests. Everything before it is a render filter plus settings rows and cannot break a run.

**Reviewed 2026-08-02** by a verification subagent that opened every code citation. Ten defects and eight scope gaps were found and are folded in below; the [Review findings](#review-findings) section records what changed and why, because two of them were design errors rather than typos.

## Problem

A board renders only issues carrying the configured PRD label. `visibleCards`
(`web/src/lib/boardCards.ts:63-70`) filters to `labels.includes(prdLabel)` unless a
per-browser boolean is on, and that boolean defaults to off
(`web/src/pages/Board.tsx:119`). The only alternative it offers is *everything*.

Measured on this repo's own GitLab project (`vtmocanu/uzi`, project 393) on 2026-08-02,
across all 81 open issues:

| Population | Count | On the board today |
|---|---|---|
| carries `PRD` | 23 | yes |
| carries `bug` | 18 | **no** |
| carries both | 0 | — |
| carries neither | 40 | no |

The 18 `bug` issues — the largest coherent group of real work in the repo after the
PRDs, and disjoint from them — are invisible. The escape hatch shows all 81, which
buries the 23 that matter in 58 that mostly do not. There is no way to say "PRD and
bug, nothing else", which is the state actually wanted.

> **These are OPEN-issue counts and they are not the board's card count.** The board
> also renders a Closed column (`web/src/pages/Board.tsx:57,502`); `visibleCards` has
> no closed filter and `ListIssuesByRepo`
> (`api/internal/store/queries/forge.sql:215`) has no state predicate. The label-filtered
> sync fetch leaves `State` at its zero value, which is `StateAll`
> (`api/internal/forge/forge.go:316-318`), so every closed `PRD` issue stays cached —
> 85 of them on this project. A board here renders roughly **108** cards today, not 23.
> Stated because an earlier draft of this PRD wrote "the board shows 22 of 80" and set
> a success criterion of "40 cards", both of which are wrong about the product.

### And a visible bug still cannot be worked

Turning the toggle on does not help much, because visibility and eligibility are
separate gates and the second one is unmoved. Getting an agent onto a bug issue takes
three forge writes, in a fixed order:

1. **Promote** — adds the PRD label (`api/internal/handler/board.go:946`). Note this
   is *not* what its name suggests: uzi cannot generate a PRD document, and it never
   rewrites an issue description, only redirects a `prds/*.md` link the description
   already carried (`docs/prdless.md:63`).
2. **Mark PRDLESS** — the issue still has no `prds/*.md` link, so the PRD-link gate
   (`api/internal/workersvc/service.go:2879`) refuses the run without it.
3. **Start run**.

They cannot be reordered: the PRDLESS toggle is itself gated on `isPRD`
(`web/src/pages/Board.tsx:1491`, PRD #102 Decision 16), so the card must be promoted
before the toggle even appears.

### Nothing needs fetching

The payload already carries every open issue regardless of label — the additive fetch
landed in PRD #102 M6 and is documented at `api/internal/handler/board.go:205` and
`:449`. The 18 `bug` rows are in the cache and in the response on every board load;
`visibleCards` drops them at render. This is a filter change, not a sync change, and
[the sync fetch must stay exactly as it is](#the-sync-fetch-is-untouched-and-that-is-load-bearing).

## Solution

Three configurable label lists, with **different owners**, because they answer
different questions and carry different risk.

| List | Question it answers | Owner | Default |
|---|---|---|---|
| **Primary label** — today's `prd_label`, unchanged | "which label does uzi *write*, and *fetch*?" | Admin | `PRD` |
| **Run-eligible labels** | "may uzi work this?" | **Admin only** | `["PRD", "bug"]` |
| **Board extra labels** | "which cards do I want to look at?" | Admin default + **per-user, per-repo** override | `["bug"]` |

The membership set a board renders is **`primary ∪ extras`**.

### Eligibility does not imply visibility, and that is deliberate

The obvious rule — membership is `run-eligible ∪ extras` — is wrong, and an earlier
draft shipped it. With `bug` in the eligible set by default, a union makes `bug`
un-hideable: this PRD's own headline user journey ("untick `bug` on a repo where they
are noise") becomes impossible, because unticking an extra cannot remove a label the
other operand already contributes.

So the two lists stay orthogonal. Only the **primary** is pinned in the picker. An
admin who adds `security` to the eligible set but not to the extras default gets
issues that are runnable but not shown by default — which is coherent (the label still
appears in the picker with its count, and the issue view can still start it), and is
the admin's choice rather than an accident.

Visibility is a personal view preference and costs nothing to get wrong. Eligibility
decides what an agent can be pointed at, so it is instance policy — a per-user setting
must never widen it on a shared instance. **The visibility list is never made
runnable.** That separation is the structural rule of this PRD.

A user's saved extras are absolute once written; the admin default applies while they
have none, and *Reset to default* re-adopts it.

### Why loosening the run-eligibility gate is defensible

The gate at `api/internal/workersvc/service.go:2872` has a narrow stated purpose. From
PRD #102 Decision 14 (`prds/done/102-board-v2.md:501`), it exists so that

> a non-PRD issue that happens to contain a `prds/*.md` link would [not] become
> runnable **by accident**

A human clicking Start on a card carrying a label an admin deliberately configured as
run-eligible is not that accident. The anti-accident property survives intact: an issue
carrying neither `PRD` nor `bug` is exactly as unrunnable as it is today, PRD link or
no PRD link.

### The sync fetch is untouched, and that is load-bearing

`FullSync` and `IncrementalSync` (`api/internal/forgesvc/service.go:406`, `:461`) pass
the primary label to `ListIssues`, and `ListIssuesOptions.Labels` is **ANDed** —
stated in the contract at `api/internal/forge/forge.go:307-310` (*"Labels each issue
must carry (AND semantics)"*) and honoured by both drivers
(`gitlab.go:260-262`, `forgejo.go:320-322`).

Handing that call the eligible set would fetch issues carrying `PRD` **and** `bug`,
which by the measurement above is **zero issues** — and `FullSync` then evicts
everything not in `keep` (`:436`, `DeleteIssuesNotIn`), wiping the cached backlog
including all 85 closed PRD issues. No compile error, no red test without a live sync.

The same applies to `withoutLabel(openIssues, prdLabel)` (`:419`, `:481`), whose own
doc (`:487-498`) requires it to match *"like the filter the forge itself applied to
the first fetch"* — the two must move in lockstep or issues are written twice or
evicted.

**All four sites keep using the primary label.** This is an explicit non-change.

### What deliberately does not change: autopilot

`ListAutopilotCandidateIssues` (`api/internal/store/queries/autopilot.sql:26-27`)
requires an issue to carry **both** the autopilot label **and** the PRD label. That
predicate keeps matching the **primary** only, so this PRD changes what a human may
point uzi at and changes nothing about what uzi picks up by itself. Verified against
the query and its callers: it is the only entry to `detect`
(`api/internal/poller/autopilot.go:94-98`), so widening `isPRDIssue` downstream cannot
widen candidacy.

Every other run-creation path was enumerated and none is newly widened: `handler/workers.go:724`
(human, web and CLI through the same endpoint), `handler/ci_fix.go:115` (human, ref-based,
no issue-label gate), `handler/chat.go:43` (human, no issue), `selfimprove/engine.go:232`
(autonomous but keyed on `TrackingLabel = "uzi-self-improve"`, no `prd_label` in the
package), `slacksvc` (no run creation at all), and the sweeper
(`workersvc/service.go:3453`, requeues only). **The only widened path requires a human
click.**

### The PRD-link waiver, and why it is load-bearing rather than a nicety

A `bug` issue has no `prds/*.md` link. Making it run-eligible therefore gets it past
the label gate and straight into the *link* gate at `service.go:2879`, and the user is
back to two clicks with a differently-worded button. So:

> **An issue eligible by a NON-PRIMARY label does not require a PRD link.**

Instance-wide setting, **defaulting on**. An admin who declares `bug` runnable has
already said bugs do not need a PRD file — the same judgement PRDLESS expresses, applied
to a class instead of an instance.

**The "non-primary" qualifier is a safety property, not phrasing.** Autopilot
candidates always carry the primary label, so the waiver never applies to them and an
autopilot-labelled `PRD` issue with no link is still refused, exactly as today. Drop
the qualifier and that issue starts **unattended** — a reversal of current behaviour
that M4's autopilot-candidate test could not detect, because that query is unchanged
in the failing scenario. M4 therefore carries a second, targeted test (see M4).

## User journey

**Today.** Open a board on `vtmocanu/uzi`: 23 open PRD cards (plus the Closed column). A
colleague files a bug. It does not appear. Tick "Show other issues": 81 open cards, and
the bug is somewhere among 58 others. Find it, Promote, Mark PRDLESS, Start run.

**After.** Open the board: the 23 PRDs and the 18 bugs, and nothing else. The bug card
carries a highlighted `bug` chip saying why it is there, and a **Start run** button.
Click it.

**Tuning it.** The "Issues" control in the board toolbar lists the labels actually
present on the payload with counts of how many cards each adds. Tick `documentation` to
bring in two more; untick `bug` for a repo where they are noise (possible precisely
because membership is `primary ∪ extras`). Choices are per repo and follow the account,
not the browser. "Show all other issues" survives as the last row for "I do not know
what label it has", and *Reset to default* re-adopts the admin's set.

## Open questions

### 1. Does the admin default ship empty or opinionated? **Opinionated — `bug`, approved 2026-08-02.**

`bug` ships in **both** default lists: visible on every board and run-eligible. A board
is more useful with bugs on it, and a bug you can see but cannot start is the friction
this PRD set out to remove.

**The cost was raised and accepted.** Compiled defaults apply to any instance that never
set the key (`api/internal/settings/settings.go:176`), so on upgrade every existing
instance's boards gain their `bug` cards *and* those cards gain a Start button, with no
admin action. The visibility half is cosmetic and per-user reversible. The eligibility
half is a run-gate change and must be called out in `CHANGELOG.md` and the release
notes, not discovered (M5).

### 2. Per-repo per-user, or one set per user? **Per-repo.**

Matches today's `uzi.board.<repoId>.showNonPRD` key shape, and a monorepo and a docs
repo do not want the same labels.

### 3. Does the CLI need a change? **No — and not for the reason first given.**

There is **no issues board in the CLI**. `api/cmd/uzi/` has `tui_board.go` /
`tui_lanes.go`, and that board is a **runs** board (`docs/cli.md:232`, *"Your own runs,
refreshed on a poll"*). A search found zero `prd_label` / `PRDLabel` references anywhere
under `api/cmd/uzi/` or `api/internal/uzicli/`. `uzi run start` goes through the same
endpoint as the web client (`uzicli/client.go:881`, `cmd/uzi/run.go:253`) and is
therefore covered server-side. Recorded per CLAUDE.md's check-the-CLI rule.

### 4. What happens to an existing `showNonPRD: true`? **Migrate, do not lapse.**

Seed that user's stored set with *Show all other issues* checked on first load. Letting
it lapse silently narrows the boards of the only users who had deliberately widened them.

### 5. Is the PRD-link waiver per-instance or per-label? **Per-instance for now.**

One bool, defaulting on. Per-label is a strictly larger config surface for a distinction
nobody has asked for yet.

### 6. Does a `bug` run still get asked to update a `prds/*.md` file? **Already handled — verified.**

The lead's instruction opens on "if the issue description links a `prds/*.md` file"
(`agent/src/prompt.ts:103`), and `:112-113` carries the explicit no-op arm for an issue
with no link. No prompt change.

### 7. Does PRDLESS still earn its keep? **Leave it alone for now — decided 2026-08-02.**

Once a whole label can waive the PRD link, PRDLESS is arguably the one-off version of
the same idea. **This PRD does not make that case and does not touch PRDLESS.** It stays
exactly as it is: same setting, same label, same toggle, same semantics.

The two mechanisms are not the same shape, which is the substantive reason to defer: the
waiver is a standing policy about a *class* of issue, PRDLESS is a per-issue human
judgement, and collapsing them removes the ability to bypass the link gate on one issue
without declaring its whole label runnable.

It is also in use, though less than a first count suggested. Four open issues carry
`PRDLESS` (iids 62, 63, 78, 141), but only **#78** also carries `PRD` — on the other
three the label currently grants nothing, because `Board.tsx:1491` hides the toggle
behind `isPRD` and `createRun` refuses at `:2872` before the link gate is reached. So
the real evidence of use is one issue, not four. Any consolidation would still be a
migration rather than a deletion.

Revisit after this ships, with usage data. Filed as a follow-up, not scope here.

### 8. Does a label with zero matching cards appear in the picker? **Yes, when it is a configured default.**

Options are otherwise derived from payload labels, so ordinary rows always have a
non-zero count. A configured default (`bug` on a repo that has none) is the exception
and must render with a `0`, greyed — hiding it would leave a user unable to see why
their default is inert. This resolves a contradiction between two risk rows in an
earlier draft.

## Technical scope

### Settings — three new keys

`api/internal/settings/settings.go`. `KeyPRDLabel` (`prd_label`) stays as the **primary**
and is not renamed; three keys join it:

- `run_eligible_labels` — default `PRD,bug`. Must contain the primary.
- `board_extra_labels` — default `bug`. The per-user default.
- `eligible_label_waives_prd_link` — bool, default `true`.

**Comma-separated, not JSON**, following the in-repo precedent for a list-valued setting
(`KeyDockerRepoAllowlist` / `validateRepoAllowlist`, `settings.go:954-965`). This is safe
because `ValidateLabel` (`:972-982`) already rejects any label containing a comma, so the
separator cannot collide with a legal label name. A deliberate choice, not a default.

**Both validators need work, and `Validate` is the one that bites.** `Validate`
(`:798-828`) dispatches per key and **falls through to `ValidateLabel` for anything
unrecognised** (`:824-827`). Without new `case` arms:

- the two list keys are rejected outright — `ValidateLabel` refuses commas. Fails loudly.
- `eligible_label_waives_prd_link` is accepted as any 1-64-char comma-free string, so
  `"yes"`, `"maybe"` and `"PRD"` all validate. **Fails open**, and must join the
  `validateBool` arm at `:802`.

`ValidateMerged` (`:1016-1027`) gains the cross-key checks alongside the three that already
reject a `prdless_label` colliding with `prd_label` or `autopilot_label`: no duplicates
within a list, nothing equal to `autopilot_label` or `prdless_label`, a length cap, and the
primary is not removable from the eligible set.

`LabelChanged` (`:993-1003`, called from `handler/settings.go:225`) decides whether a
settings PUT forces a full repo resync, and is a hard-coded whitelist of `prd_label` and
`autopilot_label`. **The two new list keys must NOT join it** — the sync fetch does not
read them (see above), so a resync would be pointless, and adding them is how someone
re-opens the AND-semantics defect from the other end. Stated here so the omission is
visibly deliberate.

### The read/write split — the defect this design invites

Consumers of `settings.PRDLabel` divide into groups the type system cannot tell apart,
because every one of them is a bare `string`:

**Readers/gates — become set checks:**
- `api/internal/workersvc/service.go:2872` (`isPRDIssue`) — the run gate.
- `api/internal/handler/workers.go:738` — the "promote it before starting a run" hint text.

**Transport — keeps shipping the primary AND gains the sets:**
- `api/internal/handler/auth.go:345` (`sessionPayload`) — this is how `prd_label` reaches
  the SPA (`web/src/auth/AuthContext.tsx:66,87,178,196`), and it feeds **both** `Board.tsx`
  and `IssueView.tsx`. It is not a gate. The primary must keep flowing (Promote copy,
  chip exclusion) and the eligible set must join it — see [IssueView](#issueview-is-the-second-consumer-of-the-predicate-being-changed).

**Writers — must keep writing the PRIMARY, unchanged:**
- `api/internal/handler/board.go:981` — Promote.
- `api/internal/handler/review_issue_draft.go:102`, `review_issue_file.go:131` — the judge
  filing an issue.
- `api/internal/handler/issues.go:62` — issue creation from the board.

**Untouched — the primary, deliberately:**
- `api/internal/poller/autopilot.go:97,306` — autopilot candidacy.
- `api/internal/forgesvc/service.go:406`, `:461` — the ANDed sync fetch.
- `api/internal/forgesvc/service.go:419`, `:481` — `withoutLabel`, which must mirror it.

A writer handed the set would start labelling new issues `bug`; a fetch handed the set
would evict the backlog. Nothing fails to compile. M4 carries a test per writer.

### IssueView is the second consumer of the predicate being changed

`web/src/pages/IssueView.tsx` imports the same predicate (`:9`) and uses it at `:121`
(`isPRDCard`), `:122` (`canPromote`), and `:210,246,248,285,288` for chip exclusion and
Promote / "not PRD" copy. `boardCards.ts:9-12` records why the sharing exists:

> so the issue view uses the SAME answer the board does. Two implementations of "is this
> a PRD card" is exactly how the card's affordances and the detail page's come to disagree.

So the set-taking predicate must be wired into **both**, and the delivery mechanism
matters: IssueView has no board payload, it reads `prdLabel` from `useAuth()`. The
**eligible set therefore rides the session payload** (`api/internal/handler/auth.go:344`),
not only the board payload.

### Storage — one new table

A per-user, per-repo board preference row: `user_id`, `repo_id`, `extra_labels`, `show_all`.
Goose number assigned at merge time per CLAUDE.md — the live head is
`00094_run_revise_count.sql`, so this drafts as `00095` and is renamed on the landing rebase.

### API

- `GET`/`PUT` on the per-user board preference, mounted beside the existing board routes in
  `api/internal/handler/handler.go`.
- The board payload gains the resolved membership set; the **session** payload gains the
  eligible set and the waiver bool. `cardDTO` gains nothing.
- `web/src/lib/api.ts:434,515` carry `prd_label` on two DTOs and need the new fields.

### Web

- `web/src/lib/boardCards.ts` — `visibleCards(cards, prdLabel, showNonPRD)` becomes
  `visibleCards(cards, membershipLabels, showAll)`; `isPRDCard` gains a set-taking sibling
  for the Start/Promote decision. The `uzi-self-improve` exclusion is unchanged.
- `web/src/lib/labelChips.ts` — the exclusion set takes the **primary** only. `bug` is real
  content and must keep rendering as a chip. **`MAX_CARD_CHIPS = 4`** (`:52`) can push the
  "this is why the card is here" chip into the `+N` overflow on a card with five or more
  labels; hoist matched labels to the front of the chip list.
- `web/src/pages/Board.tsx` — the checkbox becomes an "Issues" popover fed from payload
  labels, the same derivation the Columns suggester uses (`:1591-1600`, specifically the
  `seen` accumulation at `:1592-1593`). The card's Start/Promote branch (`:1454-1469`) keys
  off the eligible set. `nonPRDCount` (`:701`) and `boardDescription` (`:708-711`) both need
  rewording.
- `web/src/pages/IssueView.tsx` — the same predicate change, per above.
- `web/src/pages/AdminSettings.tsx` — two tag inputs and a checkbox in the Labels section.
- `web/src/mocks/mockApi.ts` — more than a scenario: it re-implements the server-side
  settings validation (`:1076-1090`), pins the `AppSettings` shape (`:159`), and writes the
  PRD label on promote and create-issue (`:1855`, `:1887`, `:1917`).

### Tests that pin the current single-string behaviour

None of these were listed in the first draft; all need updating:
`web/src/lib/boardCards.test.ts` (19 cases across `isPRDCard` / `visibleCards` / `canPromote`),
`web/src/lib/labelChips.test.ts`, `web/src/pages/IssueView.test.tsx`, `web/src/mocks/data.test.ts`,
`api/internal/workersvc/prd_label_gate_test.go`, `api/internal/poller/autopilot_prd_label_test.go`,
`api/internal/store/autopilot_candidates_integration_test.go`,
`api/internal/forgesvc/additive_sync_test.go` (including a direct `withoutLabel` case at `:422-426`),
`api/internal/handler/card_labels_test.go`.

### Product copy that is already wrong

Three rendered strings, invisible to any sweep of `docs/` or code comments. The first two
went stale when PRD #102 M6 landed and are owed regardless of this PRD:

- `web/src/pages/AdminSettings.tsx:278` — "Marks an issue as factory work. **The board only
  shows issues carrying this label.**" False since the toggle shipped.
- `web/src/pages/Board.tsx:708-711` — accurate today, wrong the moment the control is
  renamed. The source comment at `:704` records that this exact sentence was already missed
  once.
- `web/src/pages/Board.tsx:1548` (`CreateIssueForm`) — "Opened on GitLab with the
  **{prdLabel}** label." Still true (creation writes the primary), but it needs review
  alongside the other two rather than being found later.

### Closed-issue asymmetry — a decision, not an oversight

Closed `PRD` issues keep appearing in the Closed column, because the label-filtered fetch is
`StateAll`. Closed `bug` issues **never** will, because the additive fetch is `State:
StateOpened` (`forgesvc/service.go:415`, `:466`). So a bug card vanishes when it closes while
a PRD card moves to Closed. Accepted as-is: fetching closed issues for every extra label
would grow the cache without bound. Recorded so it is not mistaken for a bug.

### Docs

`docs/board.md` (the control and the lists), `docs/admin-settings.md` (the new keys),
`docs/prdless.md` (a pointer to the waiver, stating PRDLESS itself is unchanged — open
question 7), `CHANGELOG.md` (the upgrade behaviour change, both halves).

## Milestones

- [ ] **M1 — The filter, client-side only.** `visibleCards` takes a label set; `labelChips`
      excludes the primary only, with matched labels hoisted ahead of the chip cap; the
      toolbar checkbox becomes the "Issues" popover reading payload labels; `IssueView` moves
      to the same predicate. Persistence still `localStorage`. Unit tests on the pure
      functions, and the existing `boardCards.test.ts` / `labelChips.test.ts` / `IssueView.test.tsx`
      cases updated. The board can show PRD + bug and nothing else.
- [ ] **M2 — Admin defaults and delivery.** The three settings keys with `Validate` case arms
      (including the `validateBool` arm — it fails open otherwise) and `ValidateMerged`
      cross-key checks; `LabelChanged` deliberately not extended; the AdminSettings tag inputs;
      the board payload carries the resolved membership set and the session payload carries the
      eligible set; `api.ts` DTOs and `mockApi.ts` updated. Visibility only — run eligibility
      does not move yet.
- [ ] **M3 — Per-user persistence.** The migration, the endpoint pair, the board reading and
      writing it, *Reset to default*, and the `showNonPRD` migration from open question 4. A
      user's set follows the account across browsers.
- [ ] **M4 — Run eligibility.** `isPRDIssue` becomes a set check; the PRD-link waiver **scoped
      to non-primary eligibility**; the Start/Promote branch on the card and in IssueView; the
      PRDLESS toggle's `isPRD` gate widened to "is eligible". **Reviewed on its own.** Ships
      with three guard tests:
      1. one per writer call site (Promote, judge draft, judge file, issue create), asserting the
         primary is still what gets written;
      2. `ListAutopilotCandidateIssues` still matches the primary only, with the reason in the
         test name;
      3. **`CreateAutopilotRun` on a `PRD` + autopilot issue with `has_prd_link=false` and no
         `PRDLESS` still returns `ErrNoPRDLink` with the waiver ON.** Test 2 cannot fail in that
         scenario, so it is not a substitute — a waiver implemented without the non-primary
         qualifier starts that run unattended.
- [ ] **M5 — Docs, copy, changelog.** The three strings above, `docs/board.md`,
      `docs/admin-settings.md`, `docs/prdless.md`, and a changelog entry stating the upgrade
      behaviour change in both halves. No CLI change (open question 3).
- [ ] **M6 — Mock parity check.** The shipped board matches
      `prds/mockups/196-board-membership-labels-mock.html` §3/§4/§7, verified in a browser
      against a mock-mode build (`VITE_UZI_MOCK=1` — never a non-mock `vite dev`/`preview`,
      which proxies to the live stack per CLAUDE.md).

### Parallelisation

M1 and M2 are **not** file-disjoint: M2's payload work lands in `api/internal/handler/board.go`,
`web/src/lib/api.ts` and `web/src/mocks/mockApi.ts`, all of which M1's new `visibleCards`
signature and popover consume. Run M1 first, or agree the `api.ts` type shape up front and
accept one merge. M3 depends on M2 (it needs a default to fall back to). M4 depends on M2 and
is otherwise independent of M3. M5 depends on M4 for final copy. M6 is last.

## Risks & mitigations

| Risk | Mitigation |
|---|---|
| A writer call site is handed the eligible set and starts labelling new issues `bug` | M4 test 1, one per writer. The compiler cannot help: every site is `string` today. |
| **The sync fetch is handed the eligible set and evicts the backlog** | Documented as an explicit non-change with the AND-semantics contract quoted (`forge.go:307-310`). `LabelChanged` is deliberately not extended. Worth a test asserting `FullSync` calls `ListIssues` with exactly one label. |
| The waiver is implemented without the non-primary qualifier and autopilot starts a link-less run unattended | M4 test 3, which is the only one of the three that can fail in that scenario. |
| A later cleanup threads the set into the autopilot query | M4 test 2, reason in the test name. |
| The upgrade surprises an existing instance | Changelog + release notes call out both halves (M5). Visibility is per-user reversible; eligibility is one admin edit. Autopilot is unaffected, so nothing starts unattended. |
| `bug` does not exist on some repo | The filter matches nothing and the board is unchanged; the picker shows a greyed `0` row so the inert default is visible (open question 8). |
| Board gets noisy on a repo with hundreds of bugs | Per-user, per-repo override, and the count beside each row says how much bigger the board gets before ticking it. |

## Success criteria

- On `vtmocanu/uzi` with shipped defaults, a board's **open** cards go from 23 to 41 (the 18
  `bug` issues), with no change to the Closed column. Stated as a delta because the absolute
  card count includes ~85 closed PRD issues.
- A `bug` card offers **Start run**, and starting one succeeds with no Promote and no PRDLESS
  step.
- An issue carrying neither an eligible nor an extra label is still invisible, and still
  refused by the run gate even when its description contains a `prds/*.md` link.
- Unticking `bug` in the picker removes those cards (the check that membership is
  `primary ∪ extras`, not `eligible ∪ extras`).
- `ListAutopilotCandidateIssues` returns the same rows before and after, and an autopilot
  `PRD` issue with no PRD link is still refused.
- `FullSync` still fetches with exactly one label, and the cached issue count does not drop.
- Promote, the judge's filed issues, and board issue creation all still write `PRD`.
- The board and the issue view agree on every card's affordances.
- A user's label choice survives a different browser.

## Decision log

1. **Three lists, different owners.** Visibility is a per-user view preference; eligibility is
   admin-only instance policy; the primary is what uzi writes and fetches. The visibility list
   is never made runnable.
2. **Membership is `primary ∪ extras`, not `eligible ∪ extras`.** Eligibility does not imply
   visibility, so an eligible-by-default label stays hideable. The union rule was in the first
   draft and made this PRD's headline user journey impossible.
3. **`prd_label` is not renamed or replaced.** It is the primary: the label uzi writes, the
   label it fetches with, and the only label autopilot matches.
4. **`bug` ships in both default lists** (open question 1), with the upgrade consequence stated.
5. **The sync fetch and `withoutLabel` keep the primary.** `ListIssuesOptions.Labels` is ANDed;
   a set there fetches nothing and evicts everything.
6. **Autopilot does not widen.** An explicit non-change, guarded by two tests, one of which
   exists because the other cannot see the failure.
7. **The PRD-link waiver defaults on and is scoped to non-primary eligibility.** Without the
   default the one-click Start is two clicks; without the scope, autopilot widens.
8. **Settings lists are comma-separated**, following `KeyDockerRepoAllowlist`; safe because
   `ValidateLabel` already rejects commas in a label.
9. **User extras are absolute, not a delta**, with *Reset to default* as the way back.
10. **PRDLESS is untouched** (open question 7). Consolidation is a follow-up and would be a
    migration.
11. **Chips exclude the primary only**, and matched labels are hoisted ahead of the four-chip
    cap. Unlike `PRD`, `bug` is content; the chip is what tells a user why the card is there.
12. **Filtering stays at render, never at fetch.** Inherited from PRD #102 Decision 12, and it
    is what makes the control instant rather than a poll-cycle away.

## Review findings

Reviewed 2026-08-02 by a verification subagent instructed to open every code citation and
assume some were wrong. Ten confirmed defects, five likely problems, eight scope gaps. Two were
design errors rather than citation drift, and they are the reason this section exists rather
than a silent edit:

- **`forgesvc/service.go:406`/`:461` were classified as readers that "become set checks".**
  They are ANDed forge fetches. Following the instruction as written would have fetched zero
  issues and evicted the cached backlog. Independently re-verified against
  `forge.go:307-310` before folding in. Now classified untouched, with `withoutLabel`
  (`:419`, `:481`) added alongside.
- **Membership as `run-eligible ∪ extras` contradicted the "untick `bug`" journey.** With `bug`
  eligible by default, a union makes it un-hideable. Changed to `primary ∪ extras`
  (Decision 2).

Also folded in: the `Validate` fall-through (both the loud and the fail-open half), the
`LabelChanged` non-change, `IssueView` and the session-payload delivery, the third stale copy
string, the chip-cap interaction, the closed-issue asymmetry, the test list, the CLI answer
(there is no issues board in the CLI), the zero-count picker row, and the corrected counts.

Corrected measurements: 81 open issues, 23 `PRD`, 18 `bug`, 0 overlap (an earlier draft said
80/22, measured before this PRD's own issue existed). The "board shows 22 of 80 / would show
40" framing was wrong in kind, not just in arithmetic — it ignored ~85 closed `PRD` cards in
the Closed column, so the success criterion is now stated as a delta.

The PRDLESS usage argument was also weakened on evidence: four open issues carry the label but
only #78 also carries `PRD`, so the real evidence of use is one issue.
