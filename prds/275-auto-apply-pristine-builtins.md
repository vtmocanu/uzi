# PRD #275: Auto-apply pristine builtin templates on boot (M4b)

**GitLab Issue**: [#275](https://gitlab.example.com/vtmocanu/uzi/-/issues/275)
**Status**: Draft (created 2026-08-09; revised after architect review)
**Priority**: Medium
**Related**:
- [#201](https://gitlab.example.com/vtmocanu/uzi/-/issues/201) — the builtin drift **signal** (M4a: badge + diff UI). This PRD is the pristine-only delivery half of #201's explicitly-deferred **M4b**. See `prds/201-builtin-drift-signal.md`, "Out, and deliberately: … no boot-path change … no auto-update … All of those are M4b."
- [#85](https://gitlab.example.com/vtmocanu/uzi/-/issues/85) — consume the versioned role library from the skills repo. Separate: that is library/git sync; this re-applies the `go:embed`'d builtins already inside the binary.
- [#208](https://gitlab.example.com/vtmocanu/uzi/-/issues/208) — worker sandbox friction. A **consumer**: its builtin-prompt fixes (B web-ux `--no-sandbox`, E BusyBox note) are inert on seeded installs until this ships.
- [#210](https://gitlab.example.com/vtmocanu/uzi/-/issues/210) — the recipient fix. A stranded example: ten builtin templates fixed, none reaching dev-cluster.

## Problem

A shipped improvement to a builtin agent-template prompt **never reaches an install that has already booted once**. The patch merges, CI is green, the embedded file is correct, and every running uzi keeps the old prompt indefinitely.

The boot seed is create-only. `InsertBuiltinAgentTemplate` (`api/internal/store/queries/agent_templates.sql:66-74`) is `INSERT … ON CONFLICT (name) WHERE scope <> 'user' DO NOTHING`; `ReconcileBuiltinTemplates` (`api/internal/store/agent_templates_builtins.go`) runs it per builtin at boot and skips an existing row. #201 **M4a** adds only the drift *signal*. The sole way to *apply* a shipped change today is an admin pressing **Reset to default** per template, blind, in the web UI. On dev-cluster, where the DB persists across every pod redeploy, builtin prompt fixes land nowhere.

## Solution

On boot, **auto-apply the embedded builtin body to pristine rows only** — rows an admin has never touched — and leave admin-edited rows exactly as they are. A pristine row has no local edit to preserve, so it needs no diff/merge surface, which is why this is **independent of #201 M4a** (M4a's diff view exists to make touching an *edited* row safe; this PRD never touches edited rows).

### The pristine discriminator (corrected after architect review)

The load-bearing question is how to tell "pristine" from "admin-customized." Three candidates were evaluated:

1. **`updated_by IS NULL`** — **REJECTED.** `updated_by UUID REFERENCES users(id) ON DELETE SET NULL` (`00011_agent_templates.sql:16`). If the admin who edited a builtin is later deleted, the FK nulls `updated_by`, so an **edited** row reads as pristine and gets silently overwritten on the next boot — a direct, silent violation of SC2. There is no user-row deletion path in the product today (`git grep` finds only `DeleteUser{Secret,Allocations,TemplateAllocations,Vault}`, none delete the row), so it is not exploitable via the API this week — but the schema deliberately chose `ON DELETE SET NULL`, an operator can trigger it with one `DELETE FROM users`, and any future user-management feature arms it. This would be the first *safety-gating* use of the column; it must not be.
2. **`updated_at = created_at`** — robust and migration-free (both are `DEFAULT now()` at insert, equal within the INSERT txn; only `UpdateAgentTemplate` bumps `updated_at`; no `moddatetime` trigger exists). Delete-proof. But `updated_at` is **UI-visible** (`web/src/pages/AgentDetail.tsx:216` renders it), and this scheme forbids the refresh from writing `updated_at` (or it self-invalidates), so a pristine builtin whose body changes on boot would show a frozen, wrong "updated" time.
3. **A dedicated `customized BOOLEAN NOT NULL DEFAULT false` column** — **CHOSEN.** Delete-proof (no FK), and it frees `updated_at` to keep tracking content changes honestly. `customized=false` ⟺ pristine. The seed leaves it false; an admin edit sets it true; **Reset to default** sets it false (return to pristine *and* resume tracking). Costs one migration — see Decision D5 and Open Decision 1.

### Mechanism

- **Migration** — add `customized BOOLEAN NOT NULL DEFAULT false` to `agent_templates`; backfill existing builtins `SET customized = (updated_at > created_at) WHERE scope = 'builtin'` (a previously-reset/edited row is conservatively marked customized; a subsequent Reset returns it to tracking).
- **Admin edit marks the row** — `UpdateAgentTemplate` (the admin-edit path) sets `customized = true`.
- **Reset returns to pristine** — `ResetAgentTemplate` (`api/internal/handler/agent_templates.go:516-559`) uses a dedicated query that re-applies the embedded body **and** sets `customized = false` (D2), instead of the current all-or-nothing `UpdateAgentTemplate` (which would mark it customized).
- **Pristine refresh on boot, content-guarded** — a separate statement in the reconcile loop, run after the insert:

  ```sql
  -- name: RefreshPristineBuiltin :execrows
  UPDATE agent_templates
  SET description = @description, model = @model, tools = @tools,
      prompt_body = @prompt_body, updated_at = now()
  WHERE name = @name AND scope = 'builtin' AND customized = false
    AND (description, model, tools, prompt_body)
        IS DISTINCT FROM (@description, @model, @tools, @prompt_body);
  ```

  The content guard makes the write (and the `updated_at` bump) fire only on a real change, so an unchanged builtin is not rewritten on every boot. Keeping this a **separate statement** (not `ON CONFLICT DO UPDATE`) leaves the insert-only default-allocation seed untouched: `ReconcileBuiltinTemplates` seeds a default allocation only when the INSERT created a row (`n > 0`, `agent_templates_builtins.go:48-65`); the refresh's own `execrows` must never feed that gate, so an admin-removed default is never re-added. Confirmed safe by review.

## Milestones

- [ ] **M0 — Migration.** Add `customized BOOLEAN NOT NULL DEFAULT false`; backfill builtins from `updated_at > created_at`. (Goose number assigned at merge, above the live head.)
- [ ] **M1 — Pristine refresh on boot.** Add `RefreshPristineBuiltin` (content-guarded) and call it per builtin in `ReconcileBuiltinTemplates` after the insert. Pristine rows converge to the embedded body; customized rows untouched; the `n > 0` allocation seed still fires only on true insert.
- [ ] **M2 — Edit marks / Reset resumes.** `UpdateAgentTemplate` (admin edit) sets `customized = true`; `ResetAgentTemplate` sets content **and** `customized = false` so a reset template returns to pristine and tracks future shipped changes.
- [ ] **M3 — Tests (LiveDB — the in-memory `reconcilerFake` cannot model FK/`now()`/`execrows`/partial-unique).** In `agent_templates_scope_integration_test.go` style, run via `./e2e/run-store-it.sh`: pristine row refreshes on boot; **customized row preserved**; **customized row whose editing admin was deleted is STILL preserved** (the discriminating case that fails under `updated_by IS NULL`); allocation not re-added on refresh; **reset → change shipped body → boot → reset row now tracks the new body** (pins D2 end-to-end); a **user-scope** same-name template and a **shadow global** row are never touched; content guard makes an unchanged builtin a no-op.
- [ ] **M4 — Docs.** Update `docs/agent-templates.md`, the `**Builtin agent templates**` bullet in `CLAUDE.md`, and the seed-query comment to the new contract: *pristine builtins track upstream on boot; admin-customized builtins are preserved until Reset*.
- [ ] **M5 — Validate on a seeded DB.** On an already-seeded database, a shipped body change to a pristine template lands on the next boot with no wipe and no manual reset; a customized template does not.

## Success criteria

1. A shipped change to a builtin's prompt reaches a **pristine** row on the next boot — no DB wipe, no per-template Reset.
2. An **admin-edited** builtin is never overwritten on boot — **including after the editing admin's user row is deleted** (the delete-proof requirement that rules out `updated_by`).
3. A default allocation an admin removed is **not** re-added on boot.
4. A **user-scope** template whose name matches a builtin, and a shadow **global** row, are never touched.
5. **Reset to default** returns a builtin to pristine and re-enables tracking.
6. The discriminator is delete-proof; the design takes **one** migration (the `customized` column) rather than overloading `updated_at`. (See Open Decision 1 if no-migration is required, which forces the `updated_at = created_at` variant and its UI-timestamp cost.)

## Decision Log

- **D1 — Pristine-only decouples this from M4a.** #201 sequences M4b behind M4a because it pictured auto-updating *edited* rows, where a diff view is the safety surface. Scoping to pristine rows removes that dependency. M4a's diff/badge remains the surface for the *edited*-row case, which this PRD leaves to #201. The two compose cleanly if both ship (pristine refreshes → no badge; edited preserved → badge). Confirmed independent by review.
- **D2 — Reset returns to pristine (`customized = false`), not "pin to today's body."** Without it a reset row would silently stop receiving future improvements. Requires a dedicated reset query (the current path routes through `UpdateAgentTemplate`, which would mark it customized).
- **D3 — Separate refresh query over `ON CONFLICT DO UPDATE`.** Protects the insert-only default-allocation seed with no `xmax`/`RETURNING` insert-vs-update discrimination.
- **D4 — Boot path, not `init()`.** The refresh runs in `ReconcileBuiltinTemplates` (after `Migrate`, under `main`), not `agenttmpl`'s package `init()` (`builtins.go:41-43` panics on a parse failure there → CrashLoopBackOff). A malformed builtin still panics at `init` regardless; unchanged.
- **D5 — Dedicated `customized` flag over `updated_by IS NULL` and over `updated_at = created_at`.** The architect review's core finding: `updated_by` is nulled by `ON DELETE SET NULL` (breaks SC2); `updated_at = created_at` is robust but overloads the UI-visible `updated_at`. A boolean is delete-proof and keeps `updated_at` honest. Cost: one migration (routine here; numbers assigned at merge).
- **D6 — Any admin write opts a row out of auto-tracking, permanently, until Reset.** Inherent to any provenance discriminator: opening a pristine builtin and saving it unchanged sets `customized = true` and stops tracking. Defensible ("they touched it, they own it"); flagged for confirmation (Open Decision 2).

## Risks & mitigations

- **Changes documented behavior** ("shipped changes don't reach you automatically"). Mitigated: scoped to pristine rows (edits stay durable) and M4 rewrites the three doc sites.
- **Backfill mis-marks a pre-existing reset row as customized** (it had `updated_at > created_at`). Low harm — its content already matches a shipped version, and a later Reset returns it to tracking. Noted, not gated.
- **Overlap with #201 M4a/M4b** — complementary, not contending (this touches only pristine rows). Re-derive #201's live state at merge time; never inherit it from a doc.

## Out of scope

- Auto-updating **admin-edited** builtins (needs the diff/merge UX — #201 M4a/M4b).
- Library/git sync from `roles.yaml` (#85).
- The content fixes that ride on this (web-ux `--no-sandbox`, lead/coder improvements, #210 recipient fix) — each is a separate change; this PRD only builds the delivery.

## Open decisions (for the user)

1. **`customized` column (one migration) vs. `updated_at = created_at` (no migration).** The PRD takes the column: delete-proof *and* keeps the UI `updated_at` honest, at the cost of one routine migration. The no-migration variant is robust too but freezes a pristine row's `updated_at` at creation (the UI would show a stale "updated" time after a boot refresh) and needs the refresh to not write `updated_at`. **Recommend the column.** Confirm, or hold a hard no-migration constraint.
2. **No-op-save semantics (D6).** Confirm that opening + saving a pristine builtin unchanged should permanently opt it out of upstream tracking (until Reset).

## Milestone parallelization

Single-component change (`api/internal/store` migration + queries + reconciler, one handler, LiveDB tests, docs). M0→M1→M2 are sequential (same files); M3 gated on M0-M2; M4 independent prose; M5 the closing validation. No cross-repo or multi-toolchain fan-out — run as one unit.

---

*Reviewed by the architect agent 2026-08-09 against code at HEAD; the discriminator correction (D5), the reset fix (M2), the content guard, the delete-preservation test (M3), and the LiveDB scoping are all its findings.*
