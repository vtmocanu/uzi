# PRD #85: Version-stamp the builtin roles and make library drift detectable

**GitLab Issue**: [#85](https://github.com/vtmocanu/uzi/-/issues/85)
**Status**: COMPLETE — the drift-check core (M1, M2, M4, M7) landed on branch `agent/issue-85`; M3 dropped, M5/M6 and Phase 2 were already done/superseded. See the banner below for the scope.
**Priority**: Medium
**Related**:
- `github.com/vtmocanu/skills` `agent-team/roles.yaml` — the upstream role library (11 roles, per-role `version:` since v0.15.0; local HEAD `a6ae17e` = PR #10).
- MR !17 (`a1f9615`) decoupled `builtins/` from `.claude/agents/`; MR !87 + MR !93 (`d69fe53e`) are the two hand-run syncs this PRD exists to make verifiable.
- Issue #87 / `prds/done/87-prebake-browser-web-ux.md` — prebakes a browser into the worker image. `web-ux` ships here **before** that lands; see Decision 7.
- Issue #63 — dev-team ↔ product role-parity nudge. Adjacent, not this: that compares `.claude/agents/` to `builtins/`; this compares `builtins/` to the upstream library.

## 🔄 Refreshed 2026-08-22 — scope narrowed to the drift-check core

Most of the original 2026-07-21 plan has been overtaken by events. This PRD is now
**only** the still-open, self-contained core: version-stamp the builtins and make
library drift detectable by a Go test. Verified against `main` on 2026-08-22.

**In scope (still open):** M1 (`Definition.Version`), M2 (stamp the **11**
library-derived builtins), M4 (vendored manifest + Go drift-test), M7 (runbook).
None of these touch `.github/workflows`, so this PRD is safe to sweep to uzi.

**Done or dropped — do NOT re-do:**

- **M3 (mirror `release`) — DROPPED.** `release` is now a deliberate dev-team-only
  role (`scripts/role-parity-accepted.tsv`: "release runbook is a dev-machine-only
  role"). The roster is already **12** (`ux-designer` + `web-ux`, not `release`), so
  the original "roster → 12" target is already met without it. Re-promoting `release`
  would contradict a shipped decision — do not.
- **M5 (roster test) — DONE.** The test is already `TestBuiltinsSetIsExactlyTwelve`
  (`api/internal/agenttmpl/render_test.go:20`) with `builtinNames` holding 12.
- **M6 (doc counts) — DONE.** `docs/agent-templates.md:30` already reads "twelve
  builtin templates" and `ARCHITECTURE.md:141` "Twelve builtin roles … plus eleven
  subagents". Only the `#87` re-scope comment (Decision 9) may remain.
- **Phase 2 (M8–M11) — DONE / superseded.** M10 shipped (#201 M4a, MR !171). M11's
  "auto-update pristine rows, never touch an admin-edited one" shipped as **PRD #275**
  (`RefreshPristineBuiltin`, guarded by the `customized` flag —
  `api/internal/store/agent_templates_builtins.go`). M9's per-row hash is superseded
  by that `customized` discriminator; M8 (persist `version`) survives only as an
  optional admin-facing label and is deferred out of this PRD.

**M2 correction:** uzi stamps the **11** library-derived builtins — every builtin
except `lead`: `architect`, `auditor`, `coder`, `documenter`, `fact-checker`,
`researcher`, `reviewer`, `spec-keeper`, `tester`, `ux-designer`, `web-ux` (both
`ux-designer` and `web-ux` are library roles; verified 2026-08-22). `lead` is uzi-only
and stays unstamped.

**🔴 M4 scope — the manifest is EXACTLY those 11 role names, not "all library roles".**
Upstream `roles.yaml` actually defines **14** roles: the 11 above plus **`release`,
`tui-ux`, `skill-reviewer`**, which uzi deliberately does **not** ship as builtins
(`release` is dev-team-only per `role-parity-accepted.tsv`; `tui-ux`/`skill-reviewer`
were never product roles). So the drift check must cover **shipped builtins only** —
it fails on a builtin whose stamp is *behind* its manifest entry or claims a version
the manifest lacks, and it must **NOT** assert roster completeness against the library
(no "a library role has no builtin → red" condition — that would redden on the 3
omitted roles and push a worker to invent builtins nobody scoped). This resolves the
apparent Decision 4 ⇄ M4 tension in Decision 4's favor: no test asserts the roster's
shape against the library.

**Reading versions at implementation time:** clone `github.com/vtmocanu/skills`, file
`skills/agent-kit/agent-team/roles.yaml` (the local dev checkout is at
`~/stuff/gitrepos/gh/vtmocanu/skills/…`; the repo is public). Pin **one** upstream ref,
stamp all 11 builtins and the manifest from that single snapshot, and record its commit
SHA in the manifest. Do not copy any version numbers from this document — they move
within hours. "Stamp each builtin with the current upstream version of the same-named
role" is the whole instruction; do **not** try to byte-confirm the body against the
library (bodies are deliberately adapted — provenance, not identity, per Decision 2).
Note the clone is an external-egress step: `github.com` is reachable on the docker
worker tier (the nightly sweep runs there), not the restricted/standard tiers.

**Sibling artifacts (filed 2026-08-22):**

- **#601 (label `local`, NOT for uzi):** after this lands, a maintainer
  verifies the drift gate actually reddens in real CI, and optionally wires the
  scheduled upstream-refresh PR bot. Both touch `.github/workflows`, which the worker
  PAT cannot push — so this stays a local maintainer task.
- **#602 (design):** admin-configurable git repo that uzi syncs agent roles
  from at runtime. The bigger bet; this PRD keeps the *shipped defaults* honest and is
  its stepping stone (the vendored manifest generalizes to the synced snapshot).

The sections below are the original 2026-07-21 plan with its dated corrections, kept
as the design-rationale record. Where a milestone below and this banner disagree, the
banner wins.

## Problem

`api/internal/agenttmpl/builtins/*.md` are ported by hand from the upstream
`roles.yaml`, and **nothing in the repo can say whether a port happened**.

The parser rejects unknown frontmatter keys
(`api/internal/agenttmpl/builtins.go:112` — `default: return ... "unknown
frontmatter key"`), so the library's `version:` stamp cannot even be written
into a builtin file. The consequence, stated in the issue's own follow-up
comment: the 2026-07-21 sync (MR !93, commit `d69fe53e`) **cannot be verified by
the next person**. They must re-run an ad-hoc similarity script, exactly as this
issue's author did, because there is no number to read.

Similarity is a poor substitute, and this is measured rather than assumed. Nine
builtins have a library counterpart; against the pre-sync library bodies they
scored:

| builtin | similarity | why |
|---|---|---|
| `documenter`, `architect`, `researcher`, `spec-keeper`, `fact-checker` | 1.000 | verbatim mirror |
| `coder` | 0.787 | deliberate adaptation |
| `reviewer` | 0.769 | deliberate adaptation |
| `auditor` | 0.687 | deliberate adaptation |
| `tester` | 0.108 | effectively uzi's own document |

**Deliberate adaptation and genuine staleness look identical to a similarity
score.** The sub-1.000 rows are not behind: MR !87 ported the library's
quality-gate change into all four. They differ because builtins carry local
style and, more importantly, because library bodies reference a `## For this
repo` tail that builtins deliberately do not have, so that prose was rewritten
as self-discovery. A number that cannot distinguish "we chose this" from "we
forgot this" cannot gate anything.

Two secondary gaps:

- **Roster.** The library has 11 roles; `builtins/` has 10, of which 9 are
  library roles plus the uzi-only `lead`. `release` and `web-ux` are missing.
  *(Correction, 2026-08-02, found while fixing citations for PRD #103: `web-ux`
  is no longer missing. `api/internal/agenttmpl/builtins/web-ux.md` landed in
  `30b06b94`, "feat(prd-87): M6 ship the web-ux builtin (roster ten->eleven)"
  — a different PRD's milestone, not this one's. `builtins/` now holds 11 of
  the 12 files this roster bullet counts against; only `release` is still
  missing. This PRD's milestone plan (M3/M5/M6 below, which still budget work
  for porting `web-ux`) needs re-deriving against that fact before this PRD
  resumes — not done here, since rewriting another PRD's plan is out of scope
  for a citation fix.)*
- **Stale docs.** `docs/agent-templates.md:30`, CLAUDE.md's `**Builtin agent
  templates**` bullet, and
  `ARCHITECTURE.md:128` all hard-code "ten" / "nine subagents". *(Line cite
  replaced with a string cite 2026-08-02, PRD #103: `CLAUDE.md`'s content had
  moved past `:117` before this correction and keeps moving as later PRDs edit
  it — cite the bullet, not a line.)* *(Second correction, same date and PRD:
  this bullet's own claim is now stale too, not just its citations —
  `docs/agent-templates.md:30` currently reads "uzi seeds eleven builtin
  templates" and `ARCHITECTURE.md` nearby reads "Eleven builtin roles", neither
  the "ten" / "nine" this bullet says they hard-code. Same rule as the Roster
  correction above: noted, not fixed here — #85's owner re-derives the actual
  gap against current text before resuming this PRD.)*

## Solution Overview

1. **`Definition.Version`** — `parse()` accepts a `version:` frontmatter key
   (positive integer), `Render()` emits it immediately after `name`, matching
   the byte layout the agent-team skill generates. Round-trip stays byte-stable.
2. **Stamp the eleven library-derived builtins** with the library version their
   body was last ported from — each **verified against `roles.yaml` at
   implementation time**, not copied from any table here (roles move within hours).
   The set is all builtins except `lead`: `architect`, `auditor`, `coder`,
   `documenter`, `fact-checker`, `researcher`, `reviewer`, `spec-keeper`, `tester`,
   `ux-designer`, `web-ux`. `lead` carries no stamp: it has no library counterpart.
   *(Refreshed 2026-08-22: was "nine"; `ux-designer` and `web-ux` are library roles
   too. The pinned version numbers that used to sit here were removed — read them live.)*
3. **Vendor a distilled library manifest** — exactly the **11 stamped role names**
   (name → version, plus the one upstream commit SHA and sync date) — and add a Go
   test that fails when a builtin's stamp is **behind** its manifest entry, or when a
   builtin claims a version the manifest does not have. It **does not** assert roster
   completeness against the library (no "a library role has no builtin → red"): the
   library has 14 roles and uzi deliberately omits 3 (`release`, `tui-ux`,
   `skill-reviewer`), so a completeness check would redden on roles nobody ships. It
   rides `go test ./...`, the gate CI already runs.
4. ~~**Mirror `release` and `web-ux`** into `builtins/`, taking the roster to 12.~~
   *(Refreshed 2026-08-22: DROPPED. `web-ux` already shipped; `release` is a
   deliberate dev-team-only role and the roster is already 12. See banner.)*
5. **A written sync runbook** so the next port is a procedure, not an
   archaeology session.

Explicitly **not** in this PRD: splitting builtins into generic body + `## For
this repo` tail (Decision 1), and persisting `version` to the database or any
UI (Decision 8).

## Design Decisions

1. **Drop the issue's step 2 — builtins stay tail-free.** The original plan was
   to split each builtin into a verbatim generic body plus a `## For this repo`
   tail. Builtins ship into *strangers'* repos via `go:embed`; there is no "this
   repo" for them to describe, and the tail's whole purpose is repo-local tuning
   the shipping binary cannot know. Tails belong in `.claude/agents/` (all 11
   files there carry one, under the `## For this repo (uzi)` heading the skill
   splits on). Step 2 is restated as: **keep builtins tail-free, and adapt
   tail-referencing library prose into self-discovery on port** — which is what
   MR !87 already did by hand, now written down as the rule.

2. **`version:` is provenance, not identity.** The stamp means "this body was
   last ported from library v*N*", not "this body is byte-identical to library
   v*N*". Four of nine bodies are deliberately adapted (0.108–0.787 similarity
   above), so an identity claim would be false for them on the day it was
   written. Provenance is the property that is both true and useful: it answers
   "does this need a port?" — the only question the check needs to answer.
   Consequence, stated plainly: **a stamp can lie if someone bumps it without
   porting.** No mechanism here prevents that; the runbook (M7) makes the
   bump-with-the-port the documented single step, and the review gate is the
   backstop. A byte-comparison could not close this either, since it would
   redden the four adapted bodies permanently.

3. **`version` renders immediately after `name`.** The skill's generated agent
   files order frontmatter `name, version, description, model`
   (`.claude/agents/coder.md`'s frontmatter block: `name, version, description,
   model`). Matching that order means a builtin and a
   skill-generated file are diffable with `diff`, which is the whole point of
   the exercise. Field order in `Render()` becomes: `name, version, description,
   tools, model`. `parse()` is order-independent already, so only the renderer
   and its byte-stability test move.

4. **`lead` carries no version line.** It is uzi-only by design and must never
   be flagged (CLAUDE.md's `**Builtin agent templates**` bullet and issue #63
   both say so). The drift check keys
   off the vendored **manifest**, not off the roster: a builtin with no manifest
   entry is simply not checked, and no test asserts the roster's shape against
   the library. Omission also exercises the omit-when-empty path the `tools` and
   `model` lines already use.

5. **Vendor a distilled manifest, not the full `roles.yaml`.** Options weighed:
   fetch from GitHub at check time (adds a network dependency to a gate that is
   currently hermetic, and flaps when the branch moves), a submodule (pulls a
   whole repo for one file, and a SHA bump is indistinguishable from a
   deliberate sync in review), or a vendored copy. Vendored wins on
   determinism. **Distilled rather than the full file** because vendoring the
   900-line `roles.yaml` invites the exact trap Decision 2 rejects: someone will
   write the body-byte-compare, and it will redden the four adapted roles
   forever. The manifest holds only what the check can act on — role name,
   version, upstream SHA, sync date.

6. **The drift check is a Go test, not a new CI stage.** It is pure data
   comparison against an embedded file, so it belongs in `go test ./...`
   alongside the existing parse/validity tests and inherits the `test:api` job
   for free. It **fails**, not warns: the only way it can go red is a
   deliberate manifest refresh with no port, and a warning in that situation is
   a warning nobody reads. (Contrast issue #63's nudge, which must never gate —
   different relationship, different rule. That one compares our team's shape to
   the product's; this one compares the product to its own upstream.)

7. *(Refreshed 2026-08-22: **`web-ux` already shipped** as a builtin. Under this
   refresh M2 only ADDS `web-ux`'s `version:` line — it does NOT modify the body, so
   the degraded-branch design below is history, not live work. Do not rewrite
   `web-ux.md`'s prompt body.)* **Ship `web-ux` now, before its browser exists (user decision,
   2026-07-21).** Its defining duty is driving a real browser via the
   `agent-browser` CLI, and that binary is **not in the worker image today** —
   `rg 'agent-browser' agent/ docs/` returns nothing; PRD #87 is what puts it
   there. So `web-ux` ships **degraded**: it can review code, DOM-adjacent
   files, and accessibility by reading, but cannot do the one thing that makes
   it worth having. **The mitigation has to forbid the improvisation, not just
   prescribe honest reporting.** PRD #87's M0 is a live worker crash in which
   the engaged `web-ux` did *not* notice the absence and stop: it installed
   `agent-browser` ad hoc, pulled a nix Chromium, and that Chromium aborted on
   the SUID sandbox under the PRD #51 hardening
   (`prds/done/87-prebake-browser-web-ux.md:28-45`). Nothing in the worker blocks
   that today — #87's crash-close is unimplemented — and a *builtin* web-ux is
   seeded as a global default allocation, so the exposure widens from
   repo-detected agents in dogfooding runs to every run whose lead picks the
   role. So the ported body's not-available branch must, in order: (a) probe for
   `agent-browser` before planning any browser work; (b) if absent, **never
   install `agent-browser`, a browser, or any nix/devbox package to obtain one**
   — that is the exact M0 improvisation, and it also violates the PRD #18
   no-runtime-download rule; (c) report which findings are therefore
   unvalidated, rather than silently falling back to code reading as if it were
   equivalent. This is a knowingly accepted cost, taken to get 12/12 roster
   parity in one move; the alternative was `release` now and `web-ux` on #87.

8. **Source-only: no database column, no DTO, no UI.** `version` stays in the
   embedded files and the drift check. `agent_templates` gains no column,
   `ReconcileBuiltinTemplates` and `builtinColumns` are untouched, and the
   handler/web/CLI surfaces do not change — so the CLAUDE.md "check whether
   `api/cmd/uzi/` needs a matching CLI change" rule resolves to *no change*, by
   inspection rather than by omission. **Follow-up worth filing:** the seeder is
   edit-preserving (`ON CONFLICT DO NOTHING`), so an admin-edited builtin
   silently never receives improvements. A persisted version would let the admin
   UI say "shipped is v4, yours is based on v3" and offer reset-to-shipped. Real
   value, different PRD.

9. **This PRD supersedes PRD #87 §6/M6, and keeps the library's `tools` line
   verbatim.** PRD #87 §6 ships the `web-ux` builtin itself, and prescribes
   three things this PRD makes wrong: drop `version:` (because
   `builtins.go:112` rejects unknown keys — M1 removes that constraint), prune
   the four Claude Code team tools `SendMessage, TaskUpdate, TaskList, TaskGet`
   from the frontmatter, and rename `TestBuiltinsSetIsExactlyTen` → `…Eleven`
   with docs going ten → eleven (this PRD lands twelve). **#87's §6/M6 must be
   re-scoped to "web-ux already shipped in #85; verify it now actually works"**
   — an issue-#87 comment, filed as part of M6 here, so an implementer running
   #87 afterwards does not execute obsolete instructions.

   On the tools line specifically, #87 is factually right and its conclusion is
   still wrong for us: ~~the worker SDK genuinely never provides those four names,~~
   the resolver is case-sensitive and fail-closed, so an unmatched entry
   grants nothing (`agent/src/repoagents.ts:20-29`). *(**Corrected 2026-08-03,
   issue #210: the struck clause is false for at least two of the four.** The
   worker SDK provides `SendMessage` — 26 `tool_use` entries across runs
   `71d83432` / `84b6a933` / `c13cff61`, 18 successful, plus six `ToolSearch`
   resolutions of `select:SendMessage` — and `TaskList` (3 calls). `TaskUpdate`
   and `TaskGet` are UNOBSERVED in those traces, which is not the same as observed
   absent, so this correction deliberately does not claim all four exist. **The
   decision's conclusion survives and its REASON INVERTS**: keep them verbatim not
   because they are inert, but because at least two are live and useful — a
   subagent's only channel to the lead's conversation **while it is still
   running** runs through `SendMessage`, which is precisely what issue #210 fixes
   the recipient of. The qualifier is load-bearing and was missing: the Agent
   tool's return value is a second channel, and issue #210's own traces show three
   subagents falling back to it (6468 / 6251 / 4400 chars, run `84b6a933` seq 337
   / 994 / 2454). What `SendMessage` uniquely provides is reaching the lead
   BEFORE the subagent finishes. Recorded here rather
   than silently edited so the next reader does not conclude Decision 9 was wrong
   rather than under-argued.)* But all nine existing
   library-derived builtins carry them verbatim (`builtins/auditor.md:4`,
   `builtins/reviewer.md:4`, …), and this PRD's entire thesis is that a builtin
   and its library counterpart should be diffable (Decision 3). Pruning two of
   twelve files would make those two the only ones whose `tools` line does not
   match `roles.yaml`, ~~to remove names that are already inert~~ *(struck
   2026-08-03, issue #210: at least `SendMessage` and `TaskList` are live — see
   the correction above. The whole purpose clause goes, not the adjective alone:
   striking only "inert" leaves "to remove names that are already", which is why
   this strike is wider than the one 165 lines below. The reason for pruning was
   never strong and this removes what was left of it.)*. So: **keep them
   verbatim.** If ~~the inert names~~ *(same correction — read: those four names)*
   are worth removing, that is a separate,
   roster-wide change (all twelve at once, with the diffability cost taken
   deliberately), not a two-file exception.

10. **The worker-side parser needs no change.** `agent/src/repoagents.ts` parses
   a *user's* cloned `.claude/agents/*.md` and **ignores unknown frontmatter
   keys** (`repoagents.ts:339-392` — only `name`/`description`/`model`/`tools`
   are kept). Skill-generated repos already carry `version:` and already parse
   fine. The strictness is asymmetric: only the Go parser rejects unknown keys,
   which is why only `agenttmpl` changes. Verified 2026-07-21.

## Touchpoints

| Area | File(s) | Change |
|---|---|---|
| Model | `api/internal/agenttmpl/render.go` | `Definition.Version int`; `Render` emits `version:` after `name` |
| Parser | `api/internal/agenttmpl/builtins.go` | `case "version":` with positive-int validation |
| Builtins | `api/internal/agenttmpl/builtins/*.md` | **11 files** (all except `lead`) gain a `version:` line; **no builtin files are added or removed** *(refreshed 2026-08-22 — `release.md`/`web-ux.md` are NOT added; web-ux already ships)* |
| Manifest | `api/internal/agenttmpl/library/` (new) | vendored `name → version` + upstream SHA/date, `go:embed`-ed |
| Drift check | `api/internal/agenttmpl/library_test.go` (new) | stamp-vs-manifest comparison |
| Tests | `api/internal/agenttmpl/render_test.go` | round-trip byte-match (11 stamped + unstamped `lead`); `version:` render/omit cases. *(Refreshed: `builtinNames` is already 12; no "two new roles" phrase pins — none are added.)* |
| Runbook | `docs/` or `prds/` (M7 decides) | the port procedure |
| Docs | — | **DONE (M6).** Counts already read twelve/eleven (`docs/agent-templates.md:30`, `ARCHITECTURE.md:141` — the older `:128` cite is stale). No doc change under this refresh. |
| Issue #87 | GitLab comment | re-scope its §6/M6 per Decision 9 |

Not touched:

- `api/internal/store/agent_templates_builtins.go` — new builtins are picked up
  automatically: `ReconcileBuiltinTemplates` inserts missing rows at every boot
  and seeds each one's default allocation **once, on insert** (`n > 0`), so a
  default an admin later removes stays removed
  (`agent_templates_builtins.go:51-58`).
- `web/src/mocks/data.ts` — it enumerates only 7 builtins and already omits
  `lead`, `architect`, and `researcher` (`mocks/data.ts:1214-1240`). It
  deliberately does not track the roster, so by that precedent it needs no
  change. Recorded here so M6's implementer does not invent a completeness
  requirement.
- The migrations, the handlers, `web/` beyond the note above, and
  `api/cmd/uzi/` (no builtin or roster references; it is data-driven off the
  API).

## Milestones

> **Refreshed dependency shape (2026-08-22):** the live scope is **M1 → M2 → M4 →
> M7**. M3/M5/M6 and Phase 2 are done or dropped per the banner. M1 unblocks
> everything (no stamp until the parser accepts one); M2 stamps the 11
> library-derived builtins; M4's manifest covers all library roles and its
> missing-builtin check is already green because the roster is complete (12,
> `release` excluded by design), so M4 no longer waits on any mirror milestone.

Original 2026-07-21 dependency note (kept for the record): **M1 unblocks
everything** (no stamp can be written until the parser accepts one). M2 and M3 are
independent of each other and can run in parallel once M1's field order is frozen.
**M4 needs both**: the manifest distills all 11 library roles, and its
missing-builtin check goes red on `release`/`web-ux` until M3 lands — so shipping M4
first would leave `go test ./...` red on `main`. M5–M7 follow.

- [x] **M1 — `Definition.Version` lands.** *(DONE.)* Parser accepts `version:` (positive
  integer; malformed or `<= 0` is a parse error), `Render` emits it after
  `name`, omitted when zero. `TestRenderFieldOrderAndOmission` extended with a
  stamped case; `TestParseRejectsBadVersion` added. `cd api && go test ./internal/agenttmpl/`
  green. **Freezes the contract** the rest depends on.
- [x] **M2 — The eleven library-derived builtins are stamped.** *(DONE — stamped
  from upstream `roles.yaml` at pinned SHA `7f4850a66a612c615d0db4ca6f6cabd96c6823f8`:
  architect 4, auditor 9, coder 8, documenter 4, fact-checker 7, researcher 3,
  reviewer 9, spec-keeper 3, tester 10, ux-designer 3, web-ux 6.)* *(Refreshed
  2026-08-22: **eleven**, not ten — `ux-designer` and `web-ux` are both library
  roles. Only `lead` is uzi-only and stays unstamped.)* Stamp each builtin with the
  **current upstream `version:` of the same-named role**, read from `roles.yaml` **at
  implementation time** from a single pinned snapshot (record that SHA for M4) — not
  copied from any table here, which moves within hours. Do **not** try to byte-confirm
  the body against the library: bodies are deliberately adapted (provenance, not
  identity — Decision 2), so "matches the version" means "was derived from it", not
  "is byte-identical". `lead` deliberately unstamped. Round-trip byte-match green for
  all 12 (11 stamped + unstamped `lead`).
- [x] **M3 — DROPPED (2026-08-22).** `release` is a deliberate dev-team-only role
  (`scripts/role-parity-accepted.tsv`: "release runbook is a dev-machine-only role").
  The roster is already 12 (`ux-designer` + `web-ux`), so its target is met without
  `release`. Do not mirror `release` in — it contradicts a shipped decision. Original
  text kept below for the record.

  *(Updated 2026-08-03: M2 said "nine … all 10" and M3 said "`release` and
  `web-ux`". **`web-ux` landed independently**, along with `architect` and
  `researcher`, after this PRD was written on 2026-07-21 — `builtins/` is now
  **11** (`architect auditor coder documenter fact-checker lead researcher
  reviewer spec-keeper tester web-ux`) and `release` is the only library role
  still missing. So ten are library-derived, the round-trip covers eleven, and
  M3's target roster of 12 is unchanged. Decision 7's `web-ux` browser-degraded
  branch should be checked against the shipped file rather than re-applied.)*
- [x] **M4 — Vendored manifest + drift check.** *(DONE — `api/internal/agenttmpl/library/manifest.json`
  + `TestBuiltinLibraryDrift` in `api/internal/agenttmpl/library_test.go`, proven red
  once by bumping a manifest entry, then restored.)* The manifest holds **exactly the 11
  stamped role names** (+ the one upstream SHA/date); the test fails on
  **behind-stamp** or **unknown-version** only — **no missing-builtin / roster-
  completeness condition** (see the 🔴 M4 scope note in the banner: the library has 14
  roles, uzi ships 11 by design). Verified by temporarily bumping one manifest entry
  and watching it go red (a check that has never been seen red is not a check). Depends
  only on M2 (stamps), not on any mirror milestone — M3 is dropped. This is last of
  the four.
- [x] **M5 — DONE.** The roster test is already `TestBuiltinsSetIsExactlyTwelve`
  (`render_test.go:20`), `builtinNames` at 12. Original text kept below. *(M2 still
  adds a stamped/unstamped round-trip case; that lives in M1/M2, not here.)* `builtinNames` updated
  **and `TestBuiltinsSetIsExactlyTen` renamed to `…Twelve`** (its name and its
  header comment both state the count — `render_test.go:8-20`; a rename, not a
  count bump). Phrase pins for the load-bearing behaviors of each new role
  (`web-ux`'s mutation-safety rules, browser-unavailable branch, and
  no-ad-hoc-install rule; `release`'s never-modify-code, stop-on-failure, and
  confirm-before-irreversible). Full `go test ./...` green with
  `UZI_TEST_DATABASE_URL` **unset**.
- [x] **M6 — DONE (doc counts).** `docs/agent-templates.md:30` reads "twelve builtin
  templates" and `ARCHITECTURE.md:141` "Twelve builtin roles … plus eleven subagents"
  already. Only the optional #87 §6/M6 re-scope comment (Decision 9) may remain — file
  it if #87 is still open. Original text kept below.
- [x] **M7 — The sync runbook exists.** *(DONE — `api/internal/agenttmpl/library/README.md`,
  co-located with the manifest, out of `check-docs`'s walk.)* Written procedure: refresh the manifest
  → the check goes red → port each red role → adapt tail-referencing prose →
  bump the stamp in the same commit as the body change → record the upstream
  SHA. Explicitly states that a stamp bump without a body port is the one thing
  no tooling here can catch (Decision 2).

### Phase 2 — reaching a running install (folded in from #201, 2026-08-03)

> **Refreshed 2026-08-22 — Phase 2 is closed, do not implement it here.** M10 shipped
> (#201 M4a, MR !171). M11's core — auto-update pristine rows, never overwrite an
> admin-edited one — shipped as **PRD #275** (`RefreshPristineBuiltin`, guarded by the
> `customized` flag). That `customized` discriminator supersedes M9's per-row hash. M8
> (persist `version`) survives only as an optional admin-facing "shipped v4 / yours v3"
> label and is deferred out of this PRD (file separately if wanted). The prose below is
> the 2026-08-03 record; none of M8/M9/M11 is live work under this refresh.

M1-M7 above get the **right role bodies into the repo**. They do not get them into
anybody's uzi. `agent_templates.sql:74` is `ON CONFLICT (name) WHERE scope <> 'user'
DO NOTHING` and `agent_templates_builtins.go` says it plainly — *an existing row
(builtin or admin-edited) is never overwritten* — so a shipped prompt change is
**inert on every install that has booted once**, including dev-cluster. The recovery
today is admin-only per-template **Reset to default**, verbatim, no merge.

That was filed as **#201** and is folded here because the two halves are one pipeline
(library → repo → running install) and neither delivers the actual goal alone. Folding
also puts the whole thing under one Decision Log, which is what would have prevented
the duplicate #202 was.

**These are gated behind M1 and ship as independent MRs, the way PRD #103's milestones
did** — folding does not make this one large change.

> ### 🔴 AMENDED 2026-08-03 by #201 M4a. READ THIS BEFORE M8-M11.
>
> **M10 SHIPPED, on its own, and NOT in the order below.** It is #201's M4a, merged as
> MR !171: a computed `differs_from_builtin`, a "differs from shipped" badge, a
> shipped-vs-stored diff, and `GET /agent-templates/{id}/builtin`. Spec of record is
> `prds/201-builtin-drift-signal.md`; design decisions are `specs/ai.md` §476-§478.
>
> **1. The ORDER BELOW IS INVERTED, and following it costs a migration you do not need.**
> M8 (`version`) is listed before M9 (hash) and both are gated behind M1's parser change.
> **The hash needs neither.** The question delivery must answer is *"has this row been
> edited since we seeded it"*, which is a **hash** question, not a version question — as
> M9's own text already says: *"an admin editing a stored prompt bumps no version."*
> Verified against the tree: `Definition` has no `Version` field, the parser rejects the
> key, and nothing in the reconcile path reads a version. **So the drift work ships
> without M1-M3 and without M8**, which is the difference between a milestone and a PRD.
>
> **2. M8 SURVIVES, for a narrower reason than it claims.** What `version` still buys is
> M10's admin-facing *label* ("shipped is v4, yours is based on v3"). A hash yields a
> boolean. That argues for keeping M8 as a later UX improvement, not for doing it first.
>
> **3. NEVER HASH `Render(def)`. Hash a canonical projection of the persisted columns.**
> Two changes scheduled in THIS PRD each silently break a `Render`-based hash with nothing
> reddening: **Decision 3** reorders the frontmatter, and **M2** adds a `version:` line
> that a hash-only world can never reproduce from a row. Every stored hash would mismatch
> and every row would reclassify as edited. A third reason found during M4a: `Render`
> omits `tools` and `model` when empty, so a stored `tools: []` and a stored `tools: NULL`
> render identically, which HIDES a difference the UI displays as "none" vs "all".
>
> **4. M11's stated policy is A NO-OP AS WRITTEN — corrected inline below.** "Auto-update
> rows still byte-identical to **the shipped default**" describes rows that have nothing
> to update, since a row identical to what is *currently* shipped is already current. The
> intent is **the default it was SEEDED with**. That distinction is the whole mechanism:
> it is what separates a pristine row that is merely behind from a row an admin edited.
>
> **5. M11's real hard part is the NULL-hash backfill, which is not mentioned below.**
> Every row on an already-seeded install has no seed hash, and that is the population this
> exists to serve. See #201's note_22449 for the embedded historical-hash design, plus D4
> (do NOT convert `InsertBuiltinAgentTemplate` to `DO UPDATE` — it is `:execrows` and the
> `n > 0` branch seeds a global default allocation), D5 (never fatal at boot; it runs
> pre-listen in a hard singleton, so a returned error is CrashLoopBackOff) and D7 (a
> misclassifying auto-update has no in-product undo, because Reset restores the shipped
> body, which is exactly what the bad update just wrote).
>
> **6. Issue #223 is a PREREQUISITE for M11**, not optional cleanup: `Handler.q` is a
> concrete `*store.Queries`, so every DB-touching handler in `agent_templates.go` is at
> 0.0% coverage, and M4a left three implementations of the drift predicate pinned by
> nothing. M11's classifier is the fourth consumer, and it is the one that writes.

- [x] **M8 — CLOSED per refresh (2026-08-22). Do not implement — no migration.** Survives only as an optional admin-facing "shipped v4 / yours v3" label; file separately if wanted. Original text kept for the record: **`version` persisted.** Migration adding the column to
  `agent_templates` (Decision 8 previously deferred exactly this), plus the DTO and
  the reconcile path that writes it on insert. **`lead` stays unstamped per M2** and is
  covered by M9's hash instead.
- [x] **M9 — CLOSED per refresh (2026-08-22). Do not implement.** Superseded by PRD #275's `customized` flag, which is the "has this row been touched" discriminator M9 wanted. Original text kept for the record: **A content hash beside the version, because a version is not sufficient.**
  A **version** answers *which library revision is this body derived from*; a **hash**
  answers *has this row been touched*. **An admin editing a stored prompt bumps no
  version**, so drift detection cannot work on versions alone — and the hash is also
  what covers `lead`, which has no upstream number to carry. Establish both at reconcile
  time.
- [x] **M10 — The drift signal.** ~~Admin-visible: this builtin has an upstream change
  pending. Today the only way to know is to have read a changelog, which is why #197's
  fix is sitting unapplied.~~ **DONE 2026-08-03 as #201 M4a (MR !171), ahead of M8/M9 and
  without either.** No version, no hash, no migration: the badge compares the stored row
  against the embedded definition at request time. See the amendment above.
- [x] **M11 — CLOSED per refresh (2026-08-22). Do not implement.** Shipped as PRD #275 (`RefreshPristineBuiltin`): auto-update pristine rows, never overwrite an admin-edited one. Original text kept for the record: **The update policy, and this is the hard part, not the schema.** What
  happens to a row an admin has edited. The cheapest defensible shape, using M9's hash:
  **auto-update rows still byte-identical to the default THEY WERE SEEDED WITH** —
  provably uncustomized, which is most installs — and **flag the rest for a manual diff**,
  never overwriting anyone's work. *(Corrected 2026-08-03: this read "byte-identical to
  the shipped default", which is a no-op — a row identical to what is currently shipped
  has nothing to update. Seeded-with vs shipped-with is the entire mechanism.)* Decide it in the Decision Log before writing the migration;
  the edit-preserving behaviour in `ON CONFLICT … DO NOTHING` is **correct** and must not
  simply be reversed. A third option between *never update* and *discard everything* is
  the entire product of this phase.

## Out of Scope

- **Generic-body / `## For this repo` tail split for builtins** — dropped, see
  Decision 1.
- ~~**Persisting `version` to `agent_templates`, the API DTO, the admin UI, or the
  CLI**~~ — **no longer out of scope. Folded in as Phase 2 (M8-M11) on 2026-08-03**,
  along with issue **#201**, which was the "follow-up issue to file" this entry
  called for. Decision 8 deferred it when nobody had costed it; the cost turned out
  to be four milestones that ship independently behind M1, and splitting the pipeline
  across two artifacts is what produced the duplicate #202. See Phase 2 above for the
  version-versus-hash distinction and the update-policy question, which is the real
  work there.

  Duplicate issue **#202** separately proposed giving `lead` a uzi-owned
  `version: 1`; that was filed without reading this PRD and is superseded by M2's
  deliberately-unstamped `lead`, with M9's hash covering it instead.
- **Byte-comparing builtin bodies against library bodies** — Decision 2/5;
  it would permanently redden four deliberately-adapted roles.
- **Automating the port itself** (a generator that writes builtins from
  `roles.yaml`). The four adapted bodies mean a generator would have to
  round-trip local edits; the manual port plus a red test is the honest version
  until the adaptation set shrinks.
- **Pruning the four ~~inert~~ Claude Code team tools** (`SendMessage`,
  `TaskUpdate`, `TaskList`, `TaskGet`) from builtin `tools` lines — Decision 9.
  If worth doing, it is one roster-wide change, not a two-file exception here.
  *(Corrected 2026-08-03, issue #210: "inert" is wrong for at least `SendMessage`
  and `TaskList`, both observed executing in real runs — see the correction under
  Decision 9. This stays out of scope, but the reason is now stronger rather than
  weaker: pruning a LIVE tool would remove a subagent's only channel to the lead
  **while it is still running** — the Agent tool's return value still delivers a
  finished report, so the qualifier is what makes the claim true.)*
- **The dev-team ↔ product parity nudge** — issue #63, a different comparison.
- **Prebaking a browser into the worker image** — issue #87 / PRD #87. This PRD
  ships `web-ux` degraded and says so; it does not make it functional.
- **Upstream-freshness alerting** (a scheduled job that notices `roles.yaml`
  moved on GitHub before anyone refreshes the manifest). Reconsider once the
  manual refresh proves too easy to forget.

## Validation

- `cd api && go test ./...` with `UZI_TEST_DATABASE_URL` **unset** — the
  ordinary gate. Positive control required (CLAUDE.md): the new drift test must
  appear as `--- PASS` with `RUN > 0`, not as a silent skip.
- Drift check proven red at least once by a deliberate manifest bump (M4), then
  restored.
- Round-trip byte-match green for all 12 builtins — the 11 newly stamped plus the
  unstamped `lead`.
- *(Refreshed 2026-08-22: the "boot the stack / confirm `inserted=2` / two new
  builtins seed" bullet is **removed** — this refresh adds **no** builtin files, so
  `inserted=0` is the correct and expected outcome. The store/seed path is untouched
  per Decision 8; there is nothing new to seed.)*
- No doc edits under this refresh (M6 is already DONE), so no `npm run build`
  check-docs step is required for this PRD.
