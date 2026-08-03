# Issue #201 — M4a: the builtin drift signal

**Status:** design-critique wave, not yet implemented.
**Branch:** `fix/201-builtin-drift`, worktree `/home/user/repos/myorg/vtmocanu/uzi/prd-201`, based on origin/main `25ebcd39`.
**Spec of record:** this file. Issue #201's body states the problem; its newest comment
(note_22449, 2026-08-03) is the settled design for the whole of #201. **This file scopes
M4a only.** Corrections amend this file with a dated entry under `## Amendments`; messages
only name the section that moved.

> **PRD #85 Phase 2 (M8-M11) contradicts the issue's brief on sequencing and must not be
> followed as written.** It orders `version` (M8) before `hash` (M9) and gates M8 behind
> #85 M1's parser change. The brief inverts that deliberately. Amending #85's Decision Log
> is part of the overall #201 work, and is **out of scope for M4a**.

---

## Why M4a ships alone, and first

`ResetAgentTemplate` is all-or-nothing. An admin pressing **Reset to default** on a row they
have edited loses that edit with no diff shown anywhere. So the drift signal is a **strict
prerequisite** for M4b's auto-update, not a nicety: the diff view is what makes Reset safe
to press, and it is the surface an unclassifiable row needs in M4b. Without it, M4b delivers
auto-update for the provably-pristine half and nothing at all for the hard half.

Concrete stakes: issue #210 (in flight on !168) fixes ten builtin templates that named an
unreachable recipient, measured at 8 of 26 SendMessage calls failing across three real runs.
That fix does not reach dev-cluster. Its CHANGELOG entry tells an admin to open ten
templates and click Reset. Today they must do that blind.

## Scope

**In:** a computed, never-stored `differs_from_builtin` on the template DTO; a badge on
`web/src/pages/Agents.tsx` and `web/src/pages/AgentDetail.tsx`; a shipped-vs-stored diff in
`web/src/components/AgentTemplateEditor.tsx`.

**Out, and deliberately:** no migration, no schema change, no boot-path change, no hash, no
historical-hash set, no auto-update, no kill switch. All of those are M4b. If M4a finds
itself needing a column, that is a signal the design drifted, not a reason to add one.

## Mechanisms this brief asserts — VERIFY EACH BEFORE IT IS BUILT

Every claim below is a claim about code that already exists at `25ebcd39` and can be read
now. The design wave's required deliverable is a **citation per numbered claim**: the file
that implements it and the line. Refute freely; several of these were read quickly.

1. **The shipped definition is available in-process by name.**
   `api/internal/agenttmpl/builtins.go:58` exposes `BuiltinByName(name string) (Definition, bool)`.
   `Definition` is `{Name, Description, Model string, Tools []string, PromptBody string}`
   (`api/internal/agenttmpl/render.go:13-26`). So the comparison needs no new embedding and
   no I/O.

2. **The DTO is assembled in one place.** `agentTemplateDTO` and `templateDTO(store.AgentTemplate)`
   at `api/internal/handler/agent_templates.go:53-75`. A computed field added there reaches
   every response that goes through it — **confirm that is actually every list and detail
   response, and name any handler that builds a template response some other way.**

3. **Comparison is over the five persisted columns**, per the issue brief's D2: name,
   description, model, tools, prompt_body. **Never `Render(def)`** — #85 Decision 3 reorders
   the frontmatter and #85 M2 adds a `version:` line, either of which silently reclassifies
   every row while nothing reddens.

4. **`model` and `tools` need normalization before comparison, and this is the likeliest
   defect in M4a.** `Definition.Model` is `string` with empty meaning inherit, while the DTO
   carries `*string`. `Definition.Tools` is `[]string` with empty meaning inherit-all, and
   the stored column is jsonb written via `json.Marshal` (`builtinColumns`, in
   `api/internal/store/agent_templates_builtins.go`) which Postgres re-serializes with
   different spacing. **A naive comparison here reports drift on every row, and the failure
   is loud rather than silent — but a comparison that over-normalizes (trimming, sorting
   tools, coercing nil to empty) can hide a real edit.** Say which direction each
   normalization errs in.

5. **Only `scope='builtin'` rows can drift.** For any other row there is no shipped
   counterpart, so the field is `false`. **An admin row that SHADOWS a builtin name**
   (`is_builtin=false`, same name — the case `ReconcileBuiltinTemplates` already warns about
   at `api/internal/store/agent_templates_builtins.go`, the `n == 0` branch) must NOT report
   drift: it is not a builtin and Reset does not apply to it. Confirm the DTO can tell these
   apart, and that `scope` rather than `is_builtin` is the right discriminator.

6. **A builtin row with no shipped counterpart is possible** — a builtin removed from the
   repo while its seeded row survives. `BuiltinByName` returns `false`. That is not drift;
   decide and state what the field reports, and make sure it is not `true`.

## Acceptance criteria

1. `differs_from_builtin` is computed per request and stored nowhere. No migration in the diff.
2. A pristine builtin row reports `false`; a row whose description, model, tools or
   prompt_body was edited reports `true`. Each of the four columns is covered — an
   implementation comparing only `prompt_body` passes a one-column test.
3. A `user`/`global`-scope row, and an admin row shadowing a builtin name, both report `false`.
4. A builtin row with no shipped counterpart reports `false`, not `true`.
5. The badge appears on both pages, and the editor shows a shipped-vs-stored diff.
6. `task gate:api` and `task gate:web` green, run SERIALLY. `task` exits 201 on any failure,
   so test for non-zero, never a number.
7. No change to `ReconcileBuiltinTemplates` or anything `api/cmd/server/main.go` calls before
   the HTTP listener starts.

**Not required for M4a:** any live-DB test. The jsonb-normalization hazard that makes a
live-DB test mandatory in M4b applies to a *stored* hash. M4a stores nothing. If the
implementation turns out to need one, that is a finding worth reporting, not a silent add.

## Roster

- `architect`: dispatched — design wave, `25ebcd39`.
- `reviewer`: dispatched — design wave, `25ebcd39`.
- `auditor`: dispatched — design wave, `25ebcd39`.
- `coder`: pending — spawns only after the design wave settles and this brief is rewritten.
- `tester`: pending — spawns after the coder's FIRST commit, never at kickoff.
- `web-ux`: pending — M4a lands a user-facing badge and a diff view, so it is dispatched on
  the wave where those land. **Blocker to name first: the mock fixtures must contain a
  template that actually differs from its builtin, or the pass validates rendering and is
  structurally blind to the badge.**
- `documenter`: pending — `docs/agent-templates.md:122-125` states the current no-auto-update
  behaviour and CHANGELOG needs an entry.
- `fact-checker`: pending — this brief and the eventual comments carry mechanism claims.
- `spec-keeper`: pending — `specs/` exists; sync after blocking findings clear.
- `researcher`: closed — no external research needed; every mechanism is in-tree.
- `release`: closed — M4a is not a release.

## Amendments

_None yet._
