# PRD #85: Version-stamp the builtin roles and make library drift detectable

**GitLab Issue**: [#85](https://gitlab.example.com/vtmocanu/uzi/-/issues/85)
**Status**: Draft (created 2026-07-21)
**Priority**: Medium
**Related**:
- `github.com/vtmocanu/skills` `agent-team/roles.yaml` — the upstream role library (11 roles, per-role `version:` since v0.15.0; local HEAD `a6ae17e` = PR #10).
- MR !17 (`a1f9615`) decoupled `builtins/` from `.claude/agents/`; MR !87 + MR !93 (`d69fe53e`) are the two hand-run syncs this PRD exists to make verifiable.
- Issue #87 / `prds/done/87-prebake-browser-web-ux.md` — prebakes a browser into the worker image. `web-ux` ships here **before** that lands; see Decision 7.
- Issue #63 — dev-team ↔ product role-parity nudge. Adjacent, not this: that compares `.claude/agents/` to `builtins/`; this compares `builtins/` to the upstream library.

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
2. **Stamp the nine library-derived builtins** with the library version their
   body was last ported from (`coder` 3, `reviewer` 3, `auditor` 3, `tester` 4,
   `documenter` 2, `architect` 3, `researcher` 2, `spec-keeper` 2,
   `fact-checker` 2 — each **verified against the library at port time**, not
   copied from this table). `lead` carries no stamp: it has no library
   counterpart.
3. **Vendor a distilled library manifest** (`name → version`, plus the upstream
   commit SHA and sync date) and add a Go test that fails when a builtin's stamp
   is behind it, when a manifest role has no builtin, or when a builtin claims a
   version the manifest does not have. It rides `go test ./...`, the gate CI
   already runs.
4. **Mirror `release` and `web-ux`** into `builtins/`, taking the roster to 12.
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

7. **Ship `web-ux` now, before its browser exists (user decision,
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
   still wrong for us: the worker SDK genuinely never provides those four names,
   and the resolver is case-sensitive and fail-closed, so an unmatched entry
   grants nothing (`agent/src/repoagents.ts:20-29`). But all nine existing
   library-derived builtins carry them verbatim (`builtins/auditor.md:4`,
   `builtins/reviewer.md:4`, …), and this PRD's entire thesis is that a builtin
   and its library counterpart should be diffable (Decision 3). Pruning two of
   twelve files would make those two the only ones whose `tools` line does not
   match `roles.yaml`, to remove names that are already inert. So: **keep them
   verbatim.** If the inert names are worth removing, that is a separate,
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
| Builtins | `api/internal/agenttmpl/builtins/*.md` | 9 files gain `version:`; `release.md` + `web-ux.md` added |
| Manifest | `api/internal/agenttmpl/library/` (new) | vendored `name → version` + upstream SHA/date, `go:embed`-ed |
| Drift check | `api/internal/agenttmpl/library_test.go` (new) | stamp-vs-manifest comparison |
| Tests | `api/internal/agenttmpl/render_test.go` | `builtinNames` 10 → 12; round-trip; version render/omit cases; phrase pins for the two new roles |
| Runbook | `docs/` or `prds/` (M7 decides) | the port procedure |
| Docs | `docs/agent-templates.md:30`, CLAUDE.md's `Builtin agent templates` bullet, `ARCHITECTURE.md:128` | "ten"/"nine subagents" → twelve/eleven |
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

Dependency shape: **M1 unblocks everything** (no stamp can be written until the
parser accepts one). M2 and M3 are independent of each other and can run in
parallel once M1's field order is frozen. **M4 needs both**: the manifest
distills all 11 library roles, and its missing-builtin check goes red on
`release`/`web-ux` until M3 lands — so shipping M4 first would leave
`go test ./...` red on `main`. M5–M7 follow.

- [ ] **M1 — `Definition.Version` lands.** Parser accepts `version:` (positive
  integer; malformed or `<= 0` is a parse error), `Render` emits it after
  `name`, omitted when zero. `TestRenderFieldOrderAndOmission` extended with a
  stamped and an unstamped case. `cd api && go test ./internal/agenttmpl/`
  green. **Freezes the contract** the rest depends on.
- [ ] **M2 — The nine library-derived builtins are stamped.** Each version read
  from `roles.yaml` at `a6ae17e` and **confirmed against that role's body**, not
  copied from this PRD's table. `lead` deliberately unstamped. Round-trip
  byte-match green for all 10.
- [ ] **M3 — `release` and `web-ux` mirrored in.** Ported from `roles.yaml` v2
  (stamped `version: 2`, `tools` kept verbatim per Decision 9) with
  tail-referencing prose adapted (Decision 1) and `web-ux`'s
  browser-unavailable / no-ad-hoc-install branch added (Decision 7). Roster is
  12.
- [ ] **M4 — Vendored manifest + drift check.** All 11 library roles in the
  manifest; test fails on behind-stamp / missing-builtin / unknown-version.
  Verified by temporarily bumping one manifest entry and watching it go red (a
  check that has never been seen red is not a check) — possible only once M2
  and M3 have made the other two conditions green, which is why this is last of
  the four.
- [ ] **M5 — Tests cover the roster and the new roles.** `builtinNames` updated
  **and `TestBuiltinsSetIsExactlyTen` renamed to `…Twelve`** (its name and its
  header comment both state the count — `render_test.go:8-20`; a rename, not a
  count bump). Phrase pins for the load-bearing behaviors of each new role
  (`web-ux`'s mutation-safety rules, browser-unavailable branch, and
  no-ad-hoc-install rule; `release`'s never-modify-code, stop-on-failure, and
  confirm-before-irreversible). Full `go test ./...` green with
  `UZI_TEST_DATABASE_URL` **unset**.
- [ ] **M6 — Docs tell the truth, and #87 is re-scoped.**
  `docs/agent-templates.md:30`, CLAUDE.md's `Builtin agent templates` bullet,
  `ARCHITECTURE.md:127-128`
  counts corrected (ten → twelve, nine subagents → eleven) and the two new roles
  described; `cd web && npm run build` (check-docs) green. Comment on issue #87
  re-scoping its §6/M6 per Decision 9.
- [ ] **M7 — The sync runbook exists.** Written procedure: refresh the manifest
  → the check goes red → port each red role → adapt tail-referencing prose →
  bump the stamp in the same commit as the body change → record the upstream
  SHA. Explicitly states that a stamp bump without a body port is the one thing
  no tooling here can catch (Decision 2).

## Out of Scope

- **Generic-body / `## For this repo` tail split for builtins** — dropped, see
  Decision 1.
- **Persisting `version` to `agent_templates`, the API DTO, the admin UI, or the
  CLI** — Decision 8; follow-up issue to file.
- **Byte-comparing builtin bodies against library bodies** — Decision 2/5;
  it would permanently redden four deliberately-adapted roles.
- **Automating the port itself** (a generator that writes builtins from
  `roles.yaml`). The four adapted bodies mean a generator would have to
  round-trip local edits; the manual port plus a red test is the honest version
  until the adaptation set shrinks.
- **Pruning the four inert Claude Code team tools** (`SendMessage`,
  `TaskUpdate`, `TaskList`, `TaskGet`) from builtin `tools` lines — Decision 9.
  If worth doing, it is one roster-wide change, not a two-file exception here.
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
- Round-trip byte-match green for all 12 builtins, including the unstamped
  `lead` and the two new roles.
- Boot the stack and confirm the two new builtins seed: `docker compose up` on
  an existing database, then check the `reconciled builtin agent templates`
  log line reports `inserted=2` and both appear in the admin template list with
  a default allocation. (Use an isolated project per CLAUDE.md; never a bare
  `docker compose up`.) **`inserted=2` holds only on a database with no custom
  template already named `release` or `web-ux`** — such a row shadows the
  builtin and is warn-only (`agent_templates_builtins.go:59-77`). On a
  shadowed database the expected result is the warning, not the insert.
- `cd web && npm run build` — check-docs passes after the doc edits.
