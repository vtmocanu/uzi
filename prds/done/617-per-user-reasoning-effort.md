# PRD #617 — Per-user reasoning effort setting (`default_effort`)

**Issue**: #617
**Status**: Done — implemented 2026-08-24 (M1–M6 landed and reviewed on branch `agent/issue-617`)
**Priority**: Medium

## Problem

uzi never sets the Claude Agent SDK's reasoning-effort knob. The SDK exposes it as
`Options.effort` (`node_modules/@anthropic-ai/claude-agent-sdk/sdk.d.ts:1690`),
typed `EffortLevel` (`sdk.d.ts:555` = `'low' | 'medium' | 'high' | 'xhigh' |
'max'`), plus a per-subagent `AgentDefinition.effort` (`sdk.d.ts:87`). None of the
worker query builders (`agent/src/sdk-executor.ts`, `chat-executor.ts`,
`judge-runner.ts`, `review-runner.ts`) pass any of them, so every uzi run inherits
the SDK default, documented as `high` ("Deep reasoning (default)", `sdk.d.ts:1684`).

Users who want cheaper/faster runs (`low`/`medium`) or deeper reasoning
(`xhigh`/`max`, on models that support it) have no control. The account-level model
picker exists (`default_model`, PRD #17); effort is the missing sibling.

## Solution

Add a nullable per-user `default_effort`, threaded through the **same layers**
`default_model` already touches, and surfaced in the same Settings card, directly
under the worker-model dropdown. The design mirrors `default_model` at every seam
(with `judge_model`/`summary_model` as its exact existing siblings), with two
differences called out below: effort needs its own **closed-enum** validation
(not the open `validateModel` token check), and its worker-side type is a **strict
union**, not a plain `string`.

The worker applies it at exactly one new line beside the existing model resolution
(`sdk-executor.ts:885`, `if (leadModel) baseOptions.model = leadModel;`): set
`baseOptions.effort` **only when a value is present**. That one assignment reaches
both the plan turn and the implement turn, because the implement options are built
by spreading `baseOptions` (`sdk-executor.ts:1380-1381`,
`{ ...baseOptions, agents: … }`). The chat lane has its own apply line
(`chat-executor.ts:271`).

## Decisions (Decision Log)

1. **Storage: a nullable column on `users`, not a new table.** Follows PRD #17
   Decision 3 (`default_model` is `users.default_model text`, migration
   `00031_user_default_model.sql`). `default_effort` is one nullable scalar.

2. **Unset means "do not send the key", so current behavior is byte-identical.**
   NULL `default_effort` ⇒ `ClaimConfig.DefaultEffort` omitted (`omitempty`) ⇒
   `protocol.ts` field absent ⇒ the worker never assigns `baseOptions.effort`, so
   the SDK default (`high`) applies exactly as today. This mirrors how NULL
   `default_model` falls back to the lead template's model, and the mechanism to
   copy is the model-omit guard at `sdk-executor.ts:885` /
   `chat-executor.ts:271` (`if (x) options.x = x` — never emit `undefined`).

3. **Validation is a closed enum, NOT `validateModel`.** `ValidateModel`
   (`api/internal/agenttmpl/model.go:27`) deliberately has no allowlist — any single
   token is accepted, typos fail only at SDK runtime. Effort is different: the valid
   set is a closed union. A new `ValidateEffort` accepts blank → NULL (inherit), or
   one of exactly `low|medium|high|xhigh|max`; anything else is a 400.
   **Named levels only, no integers.** The field the worker actually sets is
   top-level `Options.effort` (`sdk.d.ts:1690`), typed `EffortLevel` (`sdk.d.ts:555`)
   which is named-only — it has **no** `| number`. Only the per-agent
   `AgentDefinition.effort` (`sdk.d.ts:87`) accepts an integer, and this PRD does not
   touch per-agent effort, so an integer accepted by the API could never be applied
   without a TypeScript error — it would be dead input.

4. **No cross-validation against the selected model.** `xhigh`/`max` are only
   honored on some models (`sdk.d.ts:1250` `supportedEffortLevels`), but the SDK
   **silently downgrades** an unsupported level for the chosen model
   (`sdk.d.ts:186`, "after any silent downgrade for the selected model"). We store
   the user's choice verbatim and let the SDK downgrade; no effort×model matrix
   server-side (it would stale, and the API cannot enumerate model capabilities
   without calling Anthropic — the same reason `default_model` has no allowlist).

5. **Scope: one per-user default that governs lead + subagents.** Setting top-level
   `Options.effort` covers the lead, and the implement turn inherits it via the
   `baseOptions` spread (verified, `sdk-executor.ts:1380-1381`). **Cascade to
   subagents is an inference, not a documented guarantee — it must be test-verified
   (see Decision 8).** The PRD #305 subagent-model override
   (`applySubagentModelOverride`, `sdk-executor.ts:684-685` and `:1368-1369`) exists
   only to force a lead model onto subagents carrying per-template model **pins**;
   this PRD adds no per-template effort pins, so that path needs **no** parallel
   effort change. Per-template effort, per-run/per-schedule override, and an
   `--effort` flag are out of scope (see Future work).

6. **CLI parity matches `default_model` exactly: no setter command.** The CLI does
   not expose reading or writing `default_model` (it only decodes the settings blob
   for the TUI meters, `uzicli/client.go:901`). We keep parity: `default_effort` is
   added to `apitypes.UserSettingsDTO` for decode fidelity, and no new `uzi` command
   is introduced. Setting it stays a web action.

7. **No `.github/workflows/**` changes.** This feature touches none, keeping it
   clean for the uzi worker's PAT scope (`.claude/rules/prds.md`); both
   implementation and validation stay out of the workflow tree.

8. **The subagent cascade is an assumption and gets its own test.** The SDK types
   document model inheritance for subagents explicitly (`sdk.d.ts:56`,
   "If omitted or 'inherit', uses the main model") but say **nothing** equivalent for
   `effort`. So "top-level `Options.effort` reaches every subagent without its own
   effort" is inferred by analogy, not proven by the vendored types. M4 must include
   an agent-side test that inspects an assembled subagent roster / the implement-turn
   options and asserts effort is present there when set — not just on the lead turn.
   If that inheritance turns out false, the fallback is to also set each
   `AgentDefinition.effort` (the roster is already rebuilt per turn), and this
   decision flips.

9. **The worker-side protocol field is a strict union, not `string`.** `Options.effort`
   is `EffortLevel`, and `string` is not assignable to it (unlike `Options.model`,
   a plain `string`, which is why `resolveLeadModel` returns `string` cleanly). So
   `protocol.ts`'s `default_effort` must be typed as an `EffortLevel`-shaped union (or
   the assignment cast `as EffortLevel`). `ValidateEffort` already guarantees the
   value server-side, so a narrowing cast at the single apply site is defensible;
   without it, `gate:agent` typecheck fails.

## Technical scope (anchors to mirror)

The layers, each with the `default_model` site the new field sits beside. Anchors
verified against the tree at authoring time; the worker must re-derive any that
drifted (see the migration note).

- **DB**: new migration `NNNNN_user_default_effort.sql`, `ALTER TABLE users ADD
  COLUMN default_effort text;` mirroring `00031_user_default_model.sql`. **Assign the
  number at finalize, not now:** the live head is `00156_ephemeral_workers_optin.sql`
  and the tree gains migrations daily, so an offline branch can go stale — pick the
  next free number above the live head at merge. A duplicate prefix is caught by the
  `check:migration-numbering` gate (`Taskfile.yml`, PRD #500), not by a silent goose
  panic, so a collision reddens `gate:repo` rather than breaking boot.
- **Store**: `api/internal/store/queries/users.sql` — add `GetUserDefaultEffort` /
  `SetUserDefaultEffort` (mirror lines 109-117) and fold `default_effort` into
  `GetUserSettings` (lines 148-156; its SELECT list goes from 5 columns to 6).
  **Regenerate with the pinned `sqlc@v1.30.0`** (`go install
  github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0`); CI `validate:api` asserts the regen is
  a **no-op**, so a different sqlc version produces diff noise that fails the gate.
  Regen updates `models.go` (`User.DefaultEffort pgtype.Text`) and every `SELECT
  */RETURNING *` on `users` — sqlc expands `*` at generate time, so CreateUser /
  GetUserBy* / ListUsers pick up the column with no manual query edit.
- **Validation**: new `ValidateEffort` in `api/internal/agenttmpl` (beside
  `model.go`), plus an HTTP wrapper mirroring `validateModel`
  (`handler/agent_templates.go:753`, doc at `:748`).
- **API**: `handler/user_settings.go` — add `DefaultEffort *string` to
  `userSettingsDTO` (fields at `:31-33`), a read map in `userSettingsResponse`
  (**the field-read map is `:53-56`**, not the `:41` signature line), and a
  `PutMySettings` arm mirroring the default_model arm (`:103-122`, `json.RawMessage`
  field at `:92`) using `ValidateEffort`. Add `DefaultEffort` to
  `apitypes/user_settings.go`.
  - **Test double, non-obvious:** `handler/user_settings_test.go`'s
    `fakeSettingsRow.Scan` switches on `len(dest)` with a hardcoded **`case 5`**
    (`:87-115`) for `GetUserSettings`. After regen it scans **6** destinations, so a
    `case 6` (plus a new `effort` field on the fake and an `UPDATE users SET
    default_effort` arm in its `QueryRow`) is **mandatory** — without it Scan falls
    through, populates nothing, and reddens unrelated existing settings tests with
    zero-value fields.
- **Claim → worker (Go)**: `workersvc/service.go` (issue claim: read ~`:1936`, write
  ~`:2161`) and `workersvc/chat.go` (chat claim: read `:206`, write `:235`) read
  `GetUserDefaultEffort` and write `ClaimConfig.DefaultEffort *string
  `json:"default_effort,omitempty"`` (`claim.go:360` sibling; `chat.go:136`).
  **Two hand-maintained sites the model field also lives on, or M3 will not compile:**
  the local `Querier` interface (`service.go:611`, needs a `GetUserDefaultEffort`
  method beside `GetUserDefaultModel`) and its test double `fakeStore`
  (`service_test.go:609`, needs the same method).
- **Protocol (TS)**: `default_effort?` in `agent/src/protocol.ts` run config (`:427`)
  and chat config (`:852`), typed as the `EffortLevel` union per Decision 9.
- **Worker apply (run)**: `agent/src/sdk-executor.ts` — beside the model read
  (`:676`, `:885`), read `ctx.config?.default_effort` and set `baseOptions.effort`
  only when present (cast/narrow per Decision 9). A plain read, **not** a
  `resolveLeadModel`-shaped two-source resolver (`:2386`) — effort has one source.
- **Worker apply (chat)**: the load-bearing sites are in `chat-executor.ts`, **not**
  `chat-runner.ts:247` (which only threads a `ChatContext` field): the `ChatContext`
  type field (`:158`), the `assembleChatOptions` input type (`:231`), the ctx→input
  thread (`:326`), and **`:271`** (`if (input.model) options.model = input.model;`) —
  the omit-when-absent line the chat SC rests on. Mirror `:271`; do not set
  `effort: claim.config.default_effort` unconditionally into `ChatContext`.
- **Web UI**: `web/src/pages/RunDefaults.tsx` — an "effort" card under the
  "Worker model" card (`:465-494`), using a new **`EffortSelect`** dropdown
  (Inherit + the five levels). This is a **bare closed dropdown**, NOT a
  `ModelSelect` clone: it must **not** carry ModelSelect's `"custom"` free-text mode
  or `modelWarning` surface (`ModelSelect.tsx:10-15,79-86`), which exist only because
  the model set is open and would let an out-of-enum string reach the API. Keep its
  level list an **unexported** const — `web/knip.jsonc` sets `exports: error`, so an
  exported-but-not-cross-module-reused const reddens `deadcode:web`. DTOs in
  `web/src/lib/api.ts` (`UserSettings.default_effort` at `:81` sibling,
  `UserSettingsPatch.default_effort` at `:101` sibling), mock in
  `web/src/mocks/mockApi.ts` (type-guard `:218-220`, PUT arm `:2143-2145`).
- **Docs**: new user page `docs/worker-effort.md` mirroring `docs/worker-model.md`
  (`audience: user`; precedence, how-to, the "unset = SDK default `high`" note, and
  the per-model silent-downgrade caveat), cross-linked from `worker-model.md`;
  a one-line mention beside model in ARCHITECTURE.md's per-user-settings note.

## Milestones

- [x] **M1 — Schema, validation, store layer.** Migration adds `users.default_effort`
  (number at finalize, above the live head); `ValidateEffort` enforces the closed
  `low|medium|high|xhigh|max` enum (blank → NULL); sqlc queries added and regenerated
  with **v1.30.0** as a `validate:api` no-op. Unit tests: each level accepted, blank →
  inherit, an unknown value and an interior-whitespace value rejected.

- [x] **M2 — Settings API endpoint (+ CLI decode fidelity).** `GET /api/me/settings`
  returns `default_effort`; `PUT` accepts it with **absent = unchanged, `null` =
  clear, value = set**, rejecting an invalid enum with 400. `apitypes.UserSettingsDTO`
  carries the field. Handler tests cover set / clear-via-blank / **clear-via-literal-
  `null`** (the default_model suite tests only blank-string clear, so this case is
  new) / invalid / unchanged-no-clobber, and bump `fakeSettingsRow` to `case 6` with
  the new fake field + UPDATE arm.

- [x] **M3 — Go claim assembly.** Issue and chat claims read `GetUserDefaultEffort`
  and write `ClaimConfig.DefaultEffort` (`omitempty`), including the `Querier`
  interface (`service.go:611`) and `fakeStore` (`service_test.go:609`) methods so the
  module compiles. Tests assert the claim omits the field when the user has no effort
  set and carries it when set, on both issue and chat claims.

- [x] **M4 — Agent-side apply (run + chat).** `protocol.ts` carries the union-typed
  `default_effort?`; the run path sets `baseOptions.effort` only when present
  (`sdk-executor.ts:885`), the chat path mirrors `chat-executor.ts:271`. Tests, on
  **both** paths, prove the key is **omitted** — `assert.ok(!("effort" in options))`,
  the discriminating form (a bare `=== undefined` is vacuous; precedent
  `agent/test/sdk-executor.test.ts:1338-1347`, `chat-executor.test.ts:273-275`) — when
  unset, and **passed through** when set. Plus the Decision 8 subagent-cascade test:
  effort reaches the implement-turn / subagent options, not only the lead turn.

- [x] **M5 — Web Settings UI.** A closed `EffortSelect` dropdown (Inherit + five
  levels, no custom/warning machinery, unexported level list) renders under the
  worker-model card in `RunDefaults.tsx`, loads from and saves to `/api/me/settings`
  with the same dirty/disabled/save UX as the model card. Mock API supports it.
  Component tests cover load, change, save, and Inherit. `gate:web` (incl. knip
  `exports:error`) green.

- [x] **M6 — Docs.** `docs/worker-effort.md` (`audience: user`) documents the setting,
  its precedence, the unset-equals-SDK-default-`high` behavior, and the per-model
  silent-downgrade caveat; `docs/worker-model.md` cross-links it; ARCHITECTURE.md's
  per-user-settings note mentions effort beside model.

## Success criteria

1. A user with no `default_effort` set produces runs whose assembled SDK options carry
   **no** `effort` key — proven by an agent-side test asserting
   `assert.ok(!("effort" in options))` (not `effort === undefined`, which is vacuous),
   on both the run and chat paths — so behavior is unchanged from before this PRD.
2. Setting `default_effort` to any of the five levels in Settings causes that level to
   reach `baseOptions.effort` for the lead **and** the implement-turn / subagent
   options (Decision 8 test).
3. An invalid effort value is rejected by the API with 400 and never stored.
4. Clearing the setting (present-`null`) returns the user to the unset/inherit state,
   and a test sends literal JSON `null` (not just a blank string).

## Risks & mitigations

- **Vacuous omit-test (this repo's recurring failure mode).** A `strictEqual(o.effort,
  undefined)` omit assertion passes against a branch that wrote zero effort code.
  Mitigation: SC1/M4 mandate the `!("effort" in options)` form, which the model-omit
  precedent already uses (`sdk-executor.test.ts:1346`).
- **`fakeSettingsRow` scan-arity silently breaks unrelated tests.** Folding a 6th column
  into `GetUserSettings` with the fake still hardcoded to `case 5` reddens existing
  settings tests with zero-value fields. Mitigation: the M2 milestone names the
  `case 6` bump explicitly.
- **Cascade-to-subagents is unverified.** SDK types document inheritance only for
  `model` (`sdk.d.ts:56`). Mitigation: Decision 8's dedicated test; fallback is setting
  each `AgentDefinition.effort`.
- **TS union vs `string`.** `Options.effort` is `EffortLevel`; a `string` protocol field
  will not typecheck. Mitigation: Decision 9 (union-typed field or a validated cast).
- **Web/api enum drift with no shared source and no gate.** Unlike `default_model`
  (open on both sides), effort is a closed enum duplicated as a Go list and a TS union
  with nothing asserting they agree; if the SDK adds a level and only one side updates,
  web offers a value the API 400-rejects (or vice versa) silently. Mitigation: keep each
  side's list in one place, comment the cross-reference, and add a small test asserting
  the web level list equals the documented set. (A truly shared source would need
  codegen; out of scope, but flag the drift.)
- **Silent SDK downgrade confuses users** ("I picked `max` but the model ignored it").
  Mitigation: the docs page states the per-model downgrade explicitly.
- **Migration staleness on an offline branch.** Mitigation: assign the number at
  finalize; the `check:migration-numbering` gate catches a collision.
- **deadcode:api zero/empty baseline.** Generated store methods have no test-root until
  their caller lands; the whole PRD merges as one PR so the final state is clean, but do
  not gate a store-only slice in isolation.

## Dependencies

None beyond the installed Agent SDK (`0.3.233`), which already exposes `Options.effort`.
**No open-web dependency:** every SDK fact above is verified from the vendored
`sdk.d.ts` in this repo and is re-checkable offline by the worker.

## Out of scope / future work

- Per-agent-template effort (`AgentDefinition.effort`), the effort analogue of a
  template's model pin.
- Per-run / per-schedule effort override and an `uzi schedule --effort` flag (the
  analogues of `run.Model`, PRD #300, and schedule `--model`).
- An `--apply-effort-to-agents` toggle (the analogue of `--apply-model-to-agents`).
- Effort on the judge/summary/review lanes (this PRD covers the run + chat lanes, where
  the user-facing worker effort lives).
- A shared/codegen'd effort enum across web and api (this PRD keeps mirrored lists).
