# PRD #66: Refuse runs when the bot can push or merge to the default branch

**GitLab Issue**: [#66](https://gitlab.example.com/vtmocanu/uzi/-/issues/66)
**Status**: **Done** (created 2026-07-17; split out of [PRD #65](65-forgejo-support.md) mid-session, on the architect's escalation, once it was clear this is a GitLab behaviour change with no Forgejo content in it; extended 2026-08-12 at the user's direction with the admin per-repo override — D8, M8–M9; all nine milestones landed and reviewed 2026-08-13)

**As-built close-out (2026-08-13).** All milestones shipped. One operational item is
NOT a code deliverable and remains for the operator at rollout: M3's *impact count* —
run `uzi admin guardrail-impact` (or `GET /api/admin/guardrail-impact`) against the live
instance BEFORE upgrading to see how many enabled repos would be refused and which. The
count is instance-specific and was not captured here (this run has no live instance); the
mechanism (the live, non-persisting scan) shipped in M3 and the release note (CHANGELOG,
M7) instructs the operator to run it. The one design refinement from build: an empty
scan under `UZI_PRIVILEGE_CHECK_INTERVAL=0` is *unknown*, not *zero affected* (R1) —
surfaced in the admin blocked-repos list and the CLI output.
**Priority**: High
**Depends on**: **PRD #65** — it lands `WriteRoleCanMerge`/`BotCanMerge`, the `Role` enum, and the shared `evaluateRepo` whose fields this PRD consumes. #65 reports; #66 refuses.
**Touches the contracts of**: PRD #5 (privilege checks — **this PRD changes its warn-don't-block policy**, the first time uzi refuses a run for any reason), PRD #19 (autopilot creates runs), PRD #6 (CI-fix creates runs), PRD #46 (self-improve creates runs), PRD #42 (claim path).

**This PRD changes existing GitLab behaviour on purpose.** It is the only reason it
is a separate PRD.

## Problem

**uzi has never refused a run.** Per-repo privilege findings — unprotected default
branch, Developers may push to it, the bot has a direct push grant — are computed by
`privcheck`, written to `forge_connections.privilege_report` (jsonb), and rendered as
a badge. Nothing reads them again:

- `handler/runs.go`, `handler/board.go`, and `workersvc/` contain **zero** references
  to `privilege_status` (verified by grep across `api/`).
- The only blocking gate in the product is the save-time **token** check
  (`handler/forge.go:191`), whose own comment says per-repo checks *"warn later"*.
- Those findings already land in `rr.Violations` — the `violations` tier — **and block
  nothing.** The tier name is decorative.

So `ARCHITECTURE.md` claims four independent guardrail layers, and layer 1 (the forge
role + protected branch) is checked, reported, and then **ignored**. A repo whose
`main` the bot can push to runs exactly like one whose `main` it cannot. uzi is
running on three layers while documenting four.

PRD #65 surfaced this by asking a question GitLab's defaults had made moot: **on a
default-configured Forgejo, a `write` bot can merge its own PR into protected `main`**
(`models/git/protected_branch.go:155-157` — `IsUserMergeWhitelisted` falls back to
write permission when the merge whitelist is off, and `EnableMergeWhitelist` defaults
to false at `:43`). GitLab's "Fully protected" default happens to forbid it, so the
gap never showed. It was always there.

A warning nobody reads is not a guardrail.

## Solution Overview

**If the bot can push or merge to the default branch, uzi refuses** — at repo-enable,
at the PAT-bearing run inserts, and at claim. The check is **live** (not the stored
report) and **fails closed** (cannot-evaluate is cannot-run).

Three enforcement points, because the capability is granted at three different
moments and a run can sit queued between them.

**Admin escape hatch (D8).** An instance admin may explicitly allow a specific repo
whose `main` the bot can reach — a per-repo, reasoned, audited exception, never a
global off-switch (R6). The refusal is the default; the override is the deliberate,
recorded departure from it. It waives only the "bot is too strong" findings and never
the fail-closed `protection_unreadable` case, so a forge blip still refuses even an
allowed repo. Added 2026-08-12 at the user's direction.

## Out of Scope

- **Not a new check.** The findings come from PRD #65's `evaluateRepo`. This PRD
  consumes them.
- **No gating of judge or chat runs.** They carry no PAT by construction (`judge.go:173`
  "ForgePAT left empty by design"; `chat.go:111` "deliberately no ForgePAT") — no PAT,
  no push, nothing to guard.
- **No auto-disabling of repos.** Refuse the action, keep the config (D4).
- **No blocking on `bot_not_member` / `bot_role_below_write`.** Those mean the bot is
  too *weak*; D6a's mandate is that it is too *strong*. Such a run fails anyway, so
  blocking might be better UX — flagged as a follow-up on the same mechanism, not
  smuggled in here.
- **No blocking on principals other than the bot's PAT.** A write **deploy key** can
  push to protected `main` and this check cannot see it (`WhitelistDeployKeys`,
  `models/git/protected_branch.go:45`). uzi provisions none. Docs must say "the bot's
  PAT cannot", never "nothing can".

## Design Decisions

### D1 — Three enforcement points, not one

"Run creation" is **not one place**, and this is the finding that shapes the PRD.

`handler/runs.go` only has `ListRuns`/`AdminListRuns`. Run creation is
`handler/workers.go:456` → `workersvc.CreateRun` (`service.go:1252`) — and there are
**six run-creating queries, three of which carry the PAT**:

| Insert | PAT? | Reached via |
|---|---|---|
| `CreateRun` | **yes** | `handler/workers.go:456` **and autopilot** |
| `CreateCIFixRun` (`ci_fix.go:149`) | **yes** | CI-fix trigger |
| `CreateSelfImproveRun` (`self_improve.go:37`) | **yes** | its own query comment: *"a dedicated insert, NOT createRun"* |
| judge (`judge.go:173`) | no — by design | — |
| chat (`chat.go:111`) | no — by design | — |

**A gate in the handler covers 1 of 3. A gate in `workersvc.CreateRun` covers 2 of 3.**
Same bypass class as the `config.go:820` seed path that keeps the version gate out of
the connect handler in #65 (R1).

The three layers:

1. **Repo-enable** (`handler/forge.go:552`) — best UX. The user is present and can fix
   it. Mirrors PRD #5's own philosophy for the token gate: block at the moment the
   user can act.
2. **One shared helper at the service layer**, called by all three PAT-bearing
   inserts. Not the handler — that misses autopilot, CI-fix, and self-improve.
3. **The claim backstop** (`service.go:682`) — the single place `ForgePAT` is
   attached to a claim.

**Layer 3 is not gold-plating.** A queued run can sit for a long time (worker
offline), so protection can be removed *between creation and claim*. Layers 1-2 check
when a run is *requested*; layer 3 checks when the capability is actually *handed
over*. **If this ever needs trimming, drop layer 2, not layer 3** — 3 subsumes 2 for
security; 2 exists to make the failure legible (a 422 on click, rather than a run that
queues and quietly dies).

**Failure shape.** 422 at both HTTP gates, reusing the existing `handler/forge.go:191`
body shape (`error` + findings array) so the UI pattern already exists. At claim: the
run goes `failed` with a reason — never a 500, since the worker is not at fault.

### D2 — Live check, not the stored report

The stored `privilege_report` loses, and the deciding argument is not the obvious one.

**The objection to live — "it couples run-creation to forge availability" — is already
true.** `handler/workers.go:480` **already** calls `f.GetIssue` and **502s** when the
forge is down. Live adds ≤2 calls of latency, **not a new failure mode**. That was the
load-bearing objection against it and it does not survive contact with the code.

Why the blob loses, in order:

1. **`UZI_PRIVILEGE_CHECK_INTERVAL=0` kills it.** The knob legally disables the
   sweeper *and* `Boot()`'s initial stamp (`main.go:381,395`; `config.go:459`), so the
   report is never written. A blob-based gate then either **refuses everything** (a
   perf knob bricks the product) or **fails open** (a perf knob **silently disables a
   security control**). **Live dissolves it**: `INTERVAL=0` then disables *reporting*,
   not *enforcement* — which is what that knob always meant.
2. **Staleness makes the rule incoherent.** D3 says "if the bot **can** push" — present
   tense. A 24h-old answer errs in both directions.
3. **It strands users who fix the problem.** Under the blob, a user who protects their
   branch stays blocked for up to 24h. An infuriating bug, and it would be *ours*.

A middle option ("trust the blob, but treat `privilege_checked_at` older than N as
itself blocking") was rejected: it inherits (1) — there is no `checked_at` at all when
`INTERVAL=0` — and N must exceed the 24h sweep or it blocks spuriously, so its best
case is trusting a 24h-old answer. It buys staleness and keeps the bypass.

### D3 — The rule, and fail-closed evaluation

**`user_can_push == true` OR `user_can_merge == true` on the default branch → refuse.**
Rationale: `main` is never touched. If layer 1 does not stop the bot, uzi does not run.

**Cannot-evaluate is cannot-run.** The existing checker **fails open**:
`privcheck/checker.go:135-139` turns a `DefaultBranchProtection` read error into a
*warning* and returns early; same at `:116-117` for `ProjectRole`. Under a naive
implementation:

> a hostile forge does not need to lie `user_can_push:false` — **it just needs to
> error.** A 403, a 5xx, or a timeout would pass the very check this PRD makes
> blocking.

So the gate's evaluator **diverges from the reporting checker**: an unevaluable read
refuses. That divergence is deliberate and must be explicit — hence *one shared
evaluator*, not two implementations that drift. A forge blip refusing runs is the
correct direction for a guardrail.

**The inversion trap (inherited from #65's R12, and this is where it would bite).**
`checker.go:141` early-returns on `!bp.Protected`, so on an **unprotected** branch
`WriteRoleCanPush`/`BotCanPush`/`BotCanMerge` are `false` **because they were never
evaluated** — indistinguishable from "evaluated and safe". The obvious
`if canPush || canMerge { refuse }` reads `false, false` and **lets the worst case
through**. `Protected` must be checked **first**. #65 lands the shared `evaluateRepo`
that makes this structurally impossible; this PRD must not reintroduce it.

### D4 — Refuse the action; never auto-disable the repo

Silently dropping a repo off the board is a worse surprise than a clear refusal, and
it destroys the user's config for what is often a one-click forge fix. Keep the repo
enabled, refuse the action, and make the reason actionable.

**M4 gap: the Repos page has no blocking badge today.** A user would discover the
block only by clicking Run. **D6a without that badge is a wall with no sign on it** —
the badge is part of the feature, not polish.

### D5 — Coded findings, severity from a table

`Violations []string` is **free text**, so the blocking set is not enumerable at all
today — which is precisely how a reviewer asked "which findings block?" and the answer
had to be read out of prose.

Replace the parallel string slices with `Finding{Code, Severity, Message}`, and take
**severity from a `findingSeverity` map — never hand-set at the call site.** Otherwise
"exhaustively enumerable" is lost again to code-reading, one `append` at a time.

Rejected: a parallel `Blocking []Code` — duplicated state that drifts from the
findings it describes.

**Migration**: the reshape changes the jsonb blob's shape. #65's D7 already NULLs
`privilege_report` (old rows hold `"role": 30` as an int against a `Role` string
field), so **if #66 lands close behind #65 the reshape can ride that same NULL**;
otherwise it needs its own one-line NULL. The report is derived cache that rebuilds in
seconds — never a jsonb rewrite.

### D6 — The exhaustive blocking table

The point of D5 is that this table is the spec, not prose.

| Code | GitLab | Forgejo | Tier |
|---|---|---|---|
| `default_branch_unprotected` | 404 from protected-branches | `protected == false` | **BLOCK** |
| `write_role_can_push` | push access levels admit Developer | `user_can_push == true` | **BLOCK** |
| `bot_can_push` | per-user allow-to-push grant | subsumed by `user_can_push` | **BLOCK** |
| `write_role_can_merge` | `merge_access_levels` admits Developer | `user_can_merge == true` | **BLOCK** |
| `bot_can_merge` | per-user merge grant | subsumed by `user_can_merge` | **BLOCK** |
| `unprotected_file_patterns` | n/a | pattern list non-empty | **BLOCK** (D7) |
| `protection_unreadable` | read error | read error | **BLOCK** (D3, fail closed) |
| `bot_not_member` | 404 from members/all | derived from permission payload | warn |
| `bot_role_below_write` | role < 30 | `permission` < write | warn |
| `bot_role_above_write` | role > 30 | `permission` ∈ {admin, owner} | warn¹ |
| `group_push_grant_undetected` | documented gap | team-whitelist gap | warn |

¹ `bot_role_above_write` stays a warning **only** because an admin/owner bot is
already caught by the token gate at save (PRD #5 blocks an instance-admin PAT). If
that ever changes, this should block.

### D7 — `unprotected_file_patterns` blocks

By D3's own words. A non-empty pattern list means the bot **can push to `main`** for
commits touching only those paths (`hook_pre_receive.go:392-407`: the allow-return at
`:403-406` sits in the can't-push path, before the final deny at `:409-414`).

The directive is "`main` is never touched", not "`main` is never touched except
`*.md`". **An agent pushing `README.md` to `main` is touching `main`.** A carve-out
justified by "it's only markdown" is exactly what the four-layer doctrine refuses.

Same reasoning for GitLab's `bot_can_push` and `write_role_can_push`: **both block.**

### D8 — Admin per-repo override (the escape hatch)

The guardrail defaults to refusing; **an admin may allow one named repo** through it,
with a reason, on the record. This is not the R6 bypass — that is a flag that *disables
enforcement*; this is a per-repo, admin-only, audited accept-risk decision, the same
shape as PRD #89's `docker_repo_allowlist` (a per-repo trust grant, fail-closed by
default). "`main` is never touched" stays the default; the override is the deliberate,
logged exception to it, on the repo an admin explicitly named.

**Scope — per repo, admin only, multi-user.** Repos are per-user (every repo query is
scoped to the owning forge connection). So:

- The **owner** (a member) sees the block on their Repos page and a pointer to ask an
  admin. **They cannot self-allow** — the owner is exactly who R6 says would route
  around a block they consider wrong.
- An **admin** allows/revokes: inline on the Repos page for a repo they own, and from a
  new **admin cross-user "blocked repos" list** (beside Agents-status) for anyone
  else's. The write is **admin-only, with no member path at all** — gated on
  `user.IsAdmin` and using an unscoped by-id query, the shape of `PatchRepo`'s *admin*
  branch (`SetRepoTrustFlags`), **not** its member `...ForUser` branch. Note `PatchRepo`
  *does* let a member patch their own repo's trust flags (`forge.go:646` else-branch); the
  override deliberately does not, because a member self-allowing is exactly the R6
  route-around. So the override is best modelled as a **dedicated admin-only route**
  (`POST /api/admin/repos/:id/guardrail-override`, required `reason`), not a fourth
  disjoint branch inside `PatchRepo` (which is bool-pointers-only and rejects combining
  paths at `forge.go:627`). Only the admin *read* surface (the blocked-repos list) and
  this admin write are new.

**What it waives, and what it never does (fail-closed preserved).** The override
downgrades the known "bot is too strong" blocking codes — `default_branch_unprotected`,
`write_role_can_push`, `bot_can_push`, `write_role_can_merge`, `bot_can_merge`,
`unprotected_file_patterns` — to a non-blocking "overridden" state on that repo. It
**never** waives `protection_unreadable` (D3): a 403/5xx/timeout still refuses, even on
an allowed repo, because you cannot acknowledge a risk uzi could not read and a hostile
forge must not pass by erroring. Mechanically this is a **post-evaluation severity
downgrade**: `evaluateRepo` runs unchanged (Protected-first, R3), produces its
`Finding{Code, Severity}` set, and only *then* are the six waivable codes downgraded on
an allowed repo — **never** an early `if override { skip }`, which would reintroduce the
Protected-first inversion (an unprotected `main` reads `false,false` as safe) and would
waive `protection_unreadable` (R8). The override enters as a **new parameter** to the
gate evaluator and a **new field** on `privcheck.Repo` for the badge; `evaluateRepo`
itself (today pure, with no override state) stays pure. So an allowed repo is allowed
identically at enable, insert, and claim — there is no second code path to drift.

**Storage — on the `repos` row (Q2).** Three columns: `guardrail_override_reason` (text,
**NULL = no override** — this is the active discriminator, so Revoke NULLs all three),
`guardrail_override_by` (user FK — the admin) and `guardrail_override_at` (timestamptz).
Chosen over an `app_settings` repo allow-list (the lighter `docker_repo_allowlist` shape)
because multi-user needs a per-owner audit trail and a list cannot hold a reason. The
actor FK must **not** be a naive `ON DELETE SET NULL` (the agent-template `updated_by`
trap CLAUDE.md flags): nulling the actor while the override stays live is an audit gap —
use `ON DELETE RESTRICT`, or treat null-actor-with-non-null-reason as needing
re-attestation.

**Lifecycle — persist until revoked, never silently re-arm (Q3).** An override does
**not** auto-expire. Auto-expiry would re-block a member's runs with nobody present to
fix it — the exact "block nobody is watching" failure the guardrail exists to avoid.
Instead the admin list shows the override's age and flags it stale past ~30 days
(visibility, not automatic revocation). Fixing protection on the forge clears the
finding on the next sweep and the override simply sits harmless; **Revoke** re-arms the
block immediately.

**Discovery — pull now, nudge later (Q4).** The admin blocked-repos list is the pull
surface shipped here. A Slack/notification nudge on the first guardrail block per repo
is the right follow-up on the existing `slacksvc`/health-nudge machinery, filed rather
than built on this milestone's critical path. The member already learns via the 422 at
the UI gate, or a `failed: guardrail` run for the autopilot / CI-fix / self-improve
paths.

The one-line rule across storage, lifecycle, and discovery: **fail closed, but never
silently** — the exception persists and stays visible, and nothing re-blocks without a
human seeing it.

## Milestones

- [x] **M1 — Coded findings + severity table** (D5, D6): `Finding{Code, Severity, Message}`
      replaces the string slices; severity from the map; the blocking set becomes
      enumerable. Reporting only — nothing refuses yet.
- [x] **M2 — The shared fail-closed evaluator** (D3): one `evaluate` used by both the
      gate and the reporting checker, `Protected` checked first, unevaluable → block.
      Tests: unprotected → blocked; read error → blocked; the `false,false` inversion
      cannot be read as safe.
- [x] **M3 — Pre-flight impact count** (R1): **run before M1's migration NULLs the
      evidence.** Re-sweep with the new checks (a jsonb query over stored reports is
      not sufficient — see R1) and report how many live repos would be refused.
- [x] **M4 — Repo-enable gate + Repos-page blocking badge** (D1 layer 1, D4): 422 with
      the `forge.go:191` body shape; the badge that stops this being a wall with no sign.
- [x] **M5 — Service-layer gate** (D1 layer 2): one helper called by `CreateRun`,
      `CreateCIFixRun`, `CreateSelfImproveRun`. **Not the handler** — that misses
      autopilot.
- [x] **M6 — Claim backstop** (D1 layer 3): at `service.go:682`, the single place
      `ForgePAT` is attached. Run → `failed` with a reason, never a 500.
- [x] **M7 — Release note + docs**: name the affected repos from M3, say how to fix
      each forge, and flip `docs/forgejo-bot-setup.md`'s "uzi will not do it for you"
      to "uzi will refuse to run until you do"; and document the admin override (D8) —
      who can set it, that it is per-repo and audited, and that it never waives the
      unreadable case.
- [x] **M8 — Admin per-repo override, backend** (D8): a goose migration adding the three
      `repos` columns (draft number, renamed to the next free number at merge per repo
      convention); **admin-only, unscoped** set/clear-override queries (no `...ForUser`
      member variant); the new columns added to **every projection that must see them** —
      the gate's repo fetch, `GetRepoForUser` (`forge.sql:80`, explicit column list — a
      new column is invisible until added there), and the badge read; a dedicated
      admin-only route `POST /api/admin/repos/:id/guardrail-override` (required `reason`);
      the shared evaluator downgrades the six waivable codes to "overridden"
      **post-evaluation**, never `protection_unreadable`; the three gates honor it. Tests:
      an allowed repo runs at all three gates; a member cannot self-allow **any** repo
      (owned included); an allowed repo whose protection read *errors* is still refused;
      the Protected-first inversion is not reintroduced.
- [x] **M9 — Override UI** (D8): **extend** M4's Repos-page badge (same file, later
      phase — do not rebuild it) with "ask an admin" (member) / inline Allow-anyway +
      Revoke (admin-owner); a new admin cross-user **blocked repos** list (allow/revoke
      any owner's repo) beside Agents-status, backed by a **new admin-only query + route**
      (precedent `AdminListRuns`) reading the **stored** `privilege_report` /
      `privilege_status` — cheap and display-appropriate, but it inherits R1's
      `INTERVAL=0` → empty-is-*unknown* caveat, which the list must state rather than
      render as "none blocked". The Allow-anyway modal names the exact findings being
      accepted and requires a reason. Per repo convention, check `api/cmd/uzi/` for a
      matching CLI surface (the CLI is a second API consumer) and note it even if deferred.

### Execution plan

| Phase | Agents | Rationale |
|---|---|---|
| **1** | **M3** alone | **Must precede M1** — the migration destroys the evidence. |
| **2** | **M1** → **M2** | M2 builds on the coded findings. |
| **3** | **M4** ∥ **M5** ∥ **M6** | Three disjoint gates: handler+web / service / claim. |
| **4** | **M8** → **M9** | Override backend before its UI; both need the gates (M4–M6) landed and the coded findings (M1). |
| **5** | **M7** alone | Docs once the as-built, the impact count, and the override are known. |

**Nothing here lands dark.** Unlike #65, **M4/M5/M6 refuse GitLab repos the moment
they land** — no `forgejo` row needed. This PRD's landing is when the behaviour change
becomes user-visible, and it should be released on that basis.

## Risks

| # | Risk | Severity | Mitigation |
|---|---|---|---|
| **R1** | **Unmeasured blast radius on existing GitLab connections.** Repos with unprotected or bot-mergeable `main` go from *warning + working run* to *refused*. | **High** | **M3 counts before M1 ships.** A jsonb query over stored reports is **not sufficient**: existing reports predate `WriteRoleCanMerge`, so a protected-but-bot-mergeable GitLab repo is **invisible** to it. Must be a re-sweep with the new checks. **And with `INTERVAL=0` the query returns nothing — that is "unknown", not "zero affected".** Do not read empty as safe. |
| **R2** | **The migration destroys the evidence M3 needs.** M1's reshape NULLs `privilege_report`; M3's count reads it. Two independently-designed things that collide. | **High** | Phase ordering: **M3 strictly before M1.** Written here because the collision is invisible from either milestone alone. |
| **R3** | **The inversion**: `false,false` on an unprotected branch means *unevaluated*, not *safe* (#65 R12). A naive gate would wave through the worst case. | **High** | `Protected` checked first in the shared evaluator (M2), inherited from #65. Test asserts unprotected → blocked. |
| **R4** | **Fail-closed turns a forge blip into refused runs.** | Medium | Accepted and correct — the alternative is a forge that passes the guardrail by erroring. Note run-creation already 502s on forge unavailability (`workers.go:480`), so this is not a new coupling. |
| **R5** | **A gate in the wrong layer misses autopilot / CI-fix / self-improve** — three separate PAT-bearing inserts, not one. | Medium | D1's service-layer helper + claim backstop. The handler alone covers 1 of 3. |
| **R6** | **Users route around a block they consider wrong** — e.g. by disabling privcheck. | Medium | Verify no bypass flag exists before shipping; if one does, it must not disable *enforcement*. `INTERVAL=0` is explicitly reporting-only under D2 and must be tested as such. **The admin override (D8) is the sanctioned route: per-repo, admin-only, audited — not a global switch.** |
| **R7** | **The override becomes a de-facto always-on bypass** — set once and forgotten, silently defeating the guardrail on that repo. | Medium | Per-repo + admin-only + required reason + actor/timestamp + a visible, staleness-flagged admin list (D8). Scoped and logged, not a global switch. No auto-expiry precisely so re-blocking is never silent — but the list surfaces age and flags stale overrides. |
| **R8** | **An override waives the fail-closed unreadable case**, letting a hostile or erroring forge pass on an allowed repo. | High | The evaluator only downgrades the known "too strong" codes; `protection_unreadable` is **never** overridable (D8, D3). Test: an overridden repo whose protection read errors is still refused. |

## Success Criteria

- A repo whose `main` the bot can push to **cannot be enabled** — 422, with a message
  naming the fix (D1 layer 1).
- The same repo, if already enabled, **cannot start a run** — via the UI, **autopilot,
  CI-fix, or self-improve** (D1 layer 2). Each path tested; the handler-only gate is
  the failure mode this criterion exists to catch.
- A run queued while `main` was protected, and claimed after protection was removed,
  **fails at claim** rather than pushing (D1 layer 3).
- A repo whose branch-protection read **errors** is refused, not passed (D3).
- An **unprotected** branch is refused — not read as `false,false` safe (R3).
- A judge run and a chat run are **unaffected** (no PAT, out of scope).
- Fixing protection on the forge **unblocks the next attempt immediately** — no
  24h wait (D2).
- `UZI_PRIVILEGE_CHECK_INTERVAL=0` disables *reporting* and **not** *enforcement* (D2).
- M3's count is in the release note, and every affected repo is named before the flip
  (R1).
- An **admin** can allow a blocked repo — one they own (inline on Repos) and one owned
  by another user (from the admin blocked-repos list) — and it then runs at all three
  gates (D8).
- A **member cannot self-allow**; they see the block and a pointer to ask an admin (D8).
- An **overridden** repo whose branch-protection read **errors** is still refused — the
  override never waives the unreadable case (D8/D3, R8).
- An override **persists until revoked** and never silently re-arms; the admin list
  surfaces its age (D8, Q3), and **Revoke** re-blocks immediately.
- The override is **recorded** with reason, actor, and timestamp on the `repos` row
  (D8, Q2).

## Decision Log

| # | Decision | Date | Rationale |
|---|---|---|---|
| D1 | Three enforcement points | 2026-07-17 | Architect. Run creation is 3 PAT-bearing inserts; handler-only covers 1 of 3. Claim backstop because a queued run outlives its check. |
| D2 | Live check, not the stored report | 2026-07-17 | Architect. The "couples to forge availability" objection is already true (`workers.go:480` 502s today). Blob-based makes `INTERVAL=0` a knob that silently disables a guardrail, and strands users who fix the problem for 24h. |
| D3 | Fail closed; `Protected` first | 2026-07-17 | Security audit. The existing checker warns on read error — a hostile forge would pass by erroring. The early-return inversion would wave through the worst case. |
| D4 | Refuse the action, never auto-disable | 2026-07-17 | Architect. Silently dropping a repo is a worse surprise and destroys config for a one-click fix. Badge is part of the feature. |
| D5 | Coded findings, severity from a table | 2026-07-17 | Architect + Fable review. Free-text violations make the blocking set unenumerable; hand-set severity loses it again. |
| D7 | `unprotected_file_patterns` blocks | 2026-07-17 | Architect. "`main` is never touched", not "except `*.md`". |
| D8 | Admin per-repo override (escape hatch): per-repo, admin-only, audited; owner cannot self-allow; never waives `protection_unreadable`; stored on the `repos` row with reason/actor/timestamp; persists until revoked; multi-user needs a new admin cross-user blocked-repos surface | 2026-08-12 | User. A scoped, logged accept-risk exception (the `docker_repo_allowlist` shape), not the R6 global bypass. Q2 storage = `repos` row; Q3 = persist-until-revoked with staleness surfaced; Q4 = pull list now, Slack nudge as follow-up. **Refined same day per architect review**: admin-only write with NO member path (`PatchRepo` has a member branch — reuse only its admin shape, via a dedicated `POST /api/admin/repos/:id/guardrail-override`); override applied as a post-evaluation severity downgrade (never an early skip, R3/R8); migration + new admin queries + column projections (incl. `GetRepoForUser`) named in M8; actor FK not naive `ON DELETE SET NULL`. |
| — | **Split from #65** | 2026-07-17 | User, on the architect's escalation. A GitLab behaviour change with no Forgejo content should not ship as a footnote to a driver PRD, and it needs its own impact count, release note, and rollout. Splitting also restored #65's dark-landing property. |
