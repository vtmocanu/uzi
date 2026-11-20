# PRD #3264: CLI model and effort settings (user defaults and agent templates)

**Status**: Done 2026-11-19 (M1–M3 shipped on branch agent/issue-3264). Drafted r2 2026-11-19 (buddy REVISE 5 and architect REVISE 9 folded) against origin/main `b4ebbd333`.
**Issue**: #3264 (child O of epic #1703).
**Design mock**: none. Use the existing `uzi settings get` table conventions and the source vocabulary in `fixtures/model-settings/contract.json`.

## Problem

A user can read every model and effort setting from the terminal (`uzi settings get` prints the "Model settings" table with stored, default, resolved and source columns), but cannot change any of them. Changing the worker model, reasoning effort, judge or summary model, a cross-check pin, or an agent template's model pin requires the web UI. Agents driving uzi headless (the uzi-cli skill) hit the same wall.

The reason is the auth contract, not missing CLI code:

- `PUT /api/me/settings` is mounted cookie-only (`requireAuth`, `api/internal/handler/routes_me.go`), so a `uzc_` CLI token gets 401. Its read (`GET`) is already `requireUser`.
- The whole `/api/agent-templates` route (reads included) is cookie-only (`api/internal/handler/routes_agents.go`).
- `PutMySettings` validates every field first but then writes each one with its own statement (`SetUserHarnessModels`, `SetUserDefaultEffort`, ... in `api/internal/handler/user_settings.go`), outside a transaction, so a database error mid-way can leave a partial write.

## Outcome

From the CLI, with a `uzc_` token, a user can set and reset each shipped model and effort setting of their own account, and the model pins of their own agent templates. Output shows the stored value and the resolved value with its source. The web UI is unchanged.

Acceptance examples:

1. `uzi settings set worker-model --harness codex gpt-5.5` stores the Codex worker-model lane; `uzi settings get` then shows `STORED gpt-5.5` and `SOURCE your choice` on that row. `uzi settings reset worker-model --harness codex` clears it, and the row shows the resolved default with its non-user source (it follows the upstream value, it is not a copy of it).
2. `uzi settings set effort --harness claude runtime` stores the explicit Runtime default mode, which `settings get` shows as distinct from a reset (cleared) effort.
3. `uzi agent-template set my-reviewer --harness claude sonnet` on the caller's own user-scope template `my-reviewer` stores the pin; the same command on a builtin or global template exits 3 with "builtin and global templates are edited in the web UI by an admin", even with an admin's `uza_` token.
4. A cookie-session `PUT` to the new settings route without the CSRF token is refused, and a `uzc_` Bearer `PUT /api/me/settings` (the existing generic route) still returns 401.

## Out of scope

- Fields whose owning child has not landed: planning model/effort (#3252, #3253), summaries Runs-on (#4156), per-template reasoning effort (#3258). Each of those children adds its own CLI field in the same change, per #1703's "Every child" rule.
- Admin settings commands (instance defaults, builtin and global template pins). Admin writes stay web-only.
- Per-run overrides (shipped in #3250 as `run create --model/--effort`).
- `default_harness`, the legacy single-lane `default_model`, appearance, sidebar and every consent switch (`/me/autopilot`, `/me/cross-check` enable, `/me/judge` enable, `/me/ci-autofix`, `/me/wait-on-limit`, ...). Consent switches stay cookie-only by design.
- Changing `PUT /api/me/settings` or its write order. The web keeps using it; making its writes transactional is a separate change.
- Any `.github/workflows/**` edit, in implementation or validation.

## Modules and seams

### 1. Model-settings write route (api)

`PUT /api/me/model-settings`, mounted `requireUser` (cookie with CSRF, or `uzc_`/`uza_` Bearer), owner-scoped: identity is the authenticated user, never the body.

- **Allowlist**: `default_claude_model`, `default_codex_model`, `default_effort`, `default_codex_effort`, `judge_model`, `codex_judge_model`, `judge_runs_on`, `summary_model`, `codex_summary_model`, `cross_check_pins`. Any other key is a 400 naming it, and nothing is written. For scalar settings, absent leaves unchanged, null clears, and a value uses the existing field validator. `cross_check_pins` retains `decodeCrossCheckPins` semantics: an array of at most four unique stage/harness patches; omitted cells and omitted model/effort members remain unchanged; null members clear only that member; top-level null is rejected. An empty array is a no-op. Resetting one cell sends both `model: null` and `effort: null` for that keyed cell and never replaces the other cells. Existing error text and accepted values are preserved.
- **Validation is shared, not copied**: extract the per-field decode and validate code from `PutMySettings` (`validateModel`, `rejectCodexIDInClaudeLane`, `rejectClaudeAliasInCodexLane`, `validateDefaultEffort`, the judge-runs-on and cross-check-pin validators) into one function both handlers call, so the two write paths cannot drift. `PutMySettings` behaviour stays byte-identical (its existing tests are the proof).
- **Atomic**: all present fields are written in one transaction; any validation or write error writes nothing. `default_claude_model`/`default_codex_model` are written only through `buildHarnessModels` + `SetUserHarnessModels` (inside the tx), which keeps the PRD #1551 D2 legacy `default_model` mirror; its `resolveSettingsHarness` read happens before the tx, the same accepted race as `PutMySettings`. `saveCrossCheckPins` today opens its own `h.pool.Begin`: refactor it to take a tx-bound querier (same lock order); `PutMySettings` keeps calling it in its own tx. Decode with `httpx.DecodeJSONStrict` (unknown fields and trailing values rejected).
- **Errors**: the shared decoder returns `(field, message)`. `PutMySettings` renders `message` exactly as today (`{"error"}` only, its current inconsistent prefixes included). The new route adds `field`: `{"error": "...", "field": "<name>"}`. The parity test compares `message` only.
- **Write invariants**: at implementation start, check whether Q-claude (#3252) has landed. If it has, CLI effort/model writes use its shared locked settings-write function and combination validator, exactly as the web does. If this issue lands first, Q-claude migrates this route when introducing that invariant; there is no waiting milestone. The new route preserves `SetUserHarnessModels`' grouped lane and legacy-projection behavior, without accepting `default_harness` or legacy `default_model` input. Transaction-dependent reads and writes use the transaction's queries rather than `h.q`; partial saves preserve omitted fields. Cross-check pin mutations join this outer transaction, not a separately committed `saveCrossCheckPins` transaction.
- **Response**: the same `{"settings": {...}}` envelope `GET /api/me/settings` returns, read after commit.
- **Why Bearer-reachable is acceptable**: the fields split into two groups, and only the first is non-additive (as for `/me/usage-reserve`, `/me/codex-account-mode` and `/me/harness-overflow`).
  - **Worker model and effort lanes** (`default_claude_model`, `default_codex_model`, `default_effort`, `default_codex_effort`): non-additive. A token that can already create runs through the `requireUser` `POST /repos/{id}/runs` can already pick a model and effort per run (`run create --model/--effort`), so the stored default grants nothing that route does not. They choose among models and harness credentials the user already holds and grant no new consent to spend unattended. Caveat (as for `/me/rate-limits`, the D23 comment in `routes_me.go`): non-additivity rests on `POST /repos/{id}/runs`, per-run overrides and schedule create staying `requireUser`; a stolen `uzc_` setting `default_effort=max` raises the spend of runs the user already consented to, which those routes already allow. The new route's mount comment says so, and if that authority is ever narrowed this route is revisited.
  - **Judge and summary fields** (`judge_model`, `codex_judge_model`, `judge_runs_on`, `summary_model`, `codex_summary_model`) and `cross_check_pins`: a deliberate new Bearer authority, not covered by a per-run route. They redirect which model or harness already-enabled judge and summary work uses, with no per-run equivalent. They cannot enable that work or add credentials: enabling the judge and cross-check stays cookie-only, and tests prove these writes cannot flip those consent flags. `cross_check_pins` (M2) falls in this group.
- **Resource bounds**: one bounded JSON body (`httpx.DecodeJSON`), one transaction updating one `users` row (plus the caller's cross-check pin rows), no outbound call, no background work.

### 2. Agent-template model-pin routes (api)

- `GET /api/me/agent-templates`, `requireUser`: the templates visible to the caller **as a non-admin** (builtin, global, and the caller's own user templates), even for an admin token, so a Bearer read never lists other users' templates. Fields: `id`, `name`, `scope`, `model`, `codex_model`, `customized`, `editable` (true only for the caller's own `scope='user'` rows). No prompt body.
- `PUT /api/me/agent-templates/{id}/model-pins`, `requireUser`, body `{model?, codex_model?}` with the same tri-state. Authorization: only `scope='user'` rows owned by the caller; a builtin or global row is 403 for everyone on this route (admins edit those in the web through the existing cookie-only route), and another user's row is 404 (existence hiding, matching `authorizeTemplateWrite`). Validation reuses `validateTemplatePin(harness, field, raw)` (`agent_templates_pins.go`) for both lanes, with a tri-state decode for `model` too (today it is `*string`; only `codex_model` is `optionalTemplatePin`). Not `validateTemplateFields`, which rejects an empty description or prompt body. A new sqlc query in `api/internal/store/queries/` updates only `model`, `codex_model`, `updated_by` and `updated_at` (run `sqlc generate`), under the existing `GetAgentTemplateForUpdate` row lock in one transaction, leaving description, tools and prompt untouched. `null` clears a pin to inherit. A `uza_` token reaches handlers with `IsAdmin=true`, so the template handlers never call `authorizeTemplateWrite`, and the list passes `IsAdmin: false` to `ListAgentTemplatesForViewer`.
- The existing `/api/agent-templates` routes are unchanged.

### 3. CLI (`api/cmd/uzi/`)

- For `worker-model`, `effort`, `judge-model` and `summary-model`: `uzi settings set <setting> --harness claude|codex <value>`, and `uzi settings reset <setting> --harness claude|codex`. The ordinary `effort` setting accepts `low|medium|high|xhigh|max|runtime`.
- For `judge-runs-on`: `uzi settings set judge-runs-on same-as-run|claude|codex` and `uzi settings reset judge-runs-on`, with no `--harness`; `same-as-run` and reset send `judge_runs_on: null`, since the server accepts only null, `claude` or `codex`.
- For `cross-check`: `uzi settings set cross-check --stage plan|code --harness claude|codex [--model <id>] [--effort <level>]` requires at least one of model or effort and preserves omitted members; `uzi settings reset cross-check --stage plan|code --harness claude|codex` clears both members of that cell.
- Invalid or inapplicable selectors and missing required arguments exit 2 before a request.
- `uzi agent-template list`, `show <name|id>`, `set <name|id> --harness claude|codex <model>`, `reset <name|id> --harness claude|codex`. A name resolves against the list; an ambiguous name (a user template colliding with a builtin name) is a usage error listing the ids.
- **Output**: for user-default writes, human output combines the returned stored settings with a fresh effective-settings read (`GET /api/me/effective-settings`, the #3254 resolver) and the established source labels, including the judge harness cell; if readback fails after commit, report that the write succeeded and that the effective view is unavailable, without implying rollback. JSON prints the PUT response envelope unchanged. Template PUT returns `{template: <the same restricted metadata DTO as the new list>}`; template list/show/set/reset show the selected template's stored pins and whether each pin inherits. They do not claim a single resolved model/source for an arbitrary template, whose effective value depends on its run role and context. JSON emits the relevant server envelope unchanged.
- **Errors and compatibility**: exit codes follow the CLI table (2 usage, 3 auth or forbidden, 4 not found, 6 server/429). Preserve normal resource errors: unknown or cross-owner template ids return 404/exit 4, and builtin/global writes return 403/exit 3. Diagnose unsupported writes only when the route is known to be absent, not for every resource 404. Every `agent-template` verb first calls `GET /api/me/agent-templates` to resolve the name or id; a 404/405 there means an older server, while a 404 on the later PUT is exit 4 not found. The fixed model-settings PUT route's 404/405 means unsupported. Unsupported routes print "this server does not support CLI model-setting writes; upgrade the server" and exit 1.

### File map (expected)

- `api/internal/handler/routes_me.go` (mounts and their comments), a new `api/internal/handler/model_settings.go` (the route), the decoder extracted from `user_settings.go` into its own file, `cross_check_pins.go` (tx-bound `saveCrossCheckPins`), `agent_templates_pins.go` and a new template-pin handler.
- `api/internal/store/queries/` (the pin-only template update) and the generated `api/internal/store` code.
- `api/internal/uzicli` client methods; `api/cmd/uzi/settings*.go` and a new `api/cmd/uzi/agent_template*.go`.
- `docs/cli.md`, `docs/worker-effort.md`, `docs/judge.md`, `docs/agent-templates.md`, `api/internal/uzicli/skill/SKILL.md`, the `api/internal/uzidocs/embed/` mirror, `specs/human.md`, a `changelog.d/` fragment.

## Testing decisions

- **Router-level auth tests**, not fake-client tests: add the new routes to a live-DB Bearer test beside `TestCLIReachesSettingsOverBearerLiveDB` in `cli_auth_livedb_test.go` (`uzc_` Bearer can PUT both new routes and GET the template list; cookie without CSRF is refused; unauthenticated is 401). Keep `PUT /api/me/settings` and `PUT /api/me/judge` in `TestCLIRejectsBearerOnCookieOnlyRoutesLiveDB`, and add the existing `/api/agent-templates/{id}` PUT there.
- **Allowlist and atomicity**: an unknown key and a bad value next to a good one each write nothing (assert the row is unchanged); a forced write failure in the transaction leaves no field written: live-DB test installs a `BEFORE UPDATE` trigger on `users` that raises when `codex_summary_model` changes (the last write), PUTs `default_effort` + `codex_summary_model`, and asserts 500 with `default_effort` unchanged; watched red against a non-transactional draft. Tests prove rollback across user fields and pin rows, and preserve the untouched worker lane and `default_harness` when setting or resetting either lane.
- **Compatibility**: an existing template collection with a missing template (exit 4) is tested separately from an absent collection route (the upgrade message, exit 1).
- **Validator sharing**: the existing `PutMySettings` tests pass unchanged; a table test drives each allowlisted field through both handlers and expects identical acceptance and error text.
- **Template authorization**: own user template OK; builtin and global 403 even with an admin `uza_` token; another user's template 404; `GET /api/me/agent-templates` with an admin token returns no other user's rows.
- **CLI**: `api/cmd/uzi` tests per command (set, reset, both harnesses, `runtime`, custom ids, invalid values, `--json` fidelity, the older-server message), following `settings_effective_test.go` and `codex_reason_test.go`.
- Bug-shaped changes do not apply here; the new atomic write is pinned by the forced-failure test watched red against a non-transactional draft.

## Milestones

### M1: own model and effort defaults from the CLI

The model-settings route with every allowlisted field except `cross_check_pins`, the shared validator extraction, the single-transaction write, and `uzi settings set|reset` for `worker-model`, `effort`, `judge-model`, `judge-runs-on`, `summary-model`. Docs (`docs/cli.md`, `docs/worker-effort.md`, `docs/judge.md`), the embedded CLI skill source `api/internal/uzicli/skill/SKILL.md`, `task docs:sync`, terse `specs/human.md` entries, a changelog fragment.

- Blocked by: none.
- Acceptance: examples 1, 2 and 4; the router-level auth, allowlist, atomicity and validator-sharing tests; `PutMySettings` tests unchanged; the Q-claude check recorded in the plan; rollback across fields; untouched lane and `default_harness` preserved.

### M2: cross-check pins from the CLI

`cross_check_pins` on the same route (stage x harness, model and effort, same validation and reset semantics as the web) and `uzi settings set|reset cross-check`. Docs and skill updated.

- Blocked by: M1.
- Acceptance: set and reset each of the four stage/harness cells; an invalid stage or effort writes nothing; `settings get` shows the pin and its checker source. Model-only and effort-only saves preserve the other member and the other three cells; top-level null and duplicate cells are rejected without writes; `runtime` is rejected for cross-check effort.

### M3: agent-template model pins from the CLI

The two template routes and `uzi agent-template list|show|set|reset`. Docs (`docs/agent-templates.md`, `docs/cli.md`) and skill updated.

- Blocked by: none; ordered after M1 in the run (they share `docs/cli.md`, `SKILL.md`, `specs/human.md`, the changelog fragment and the CLI command registration).
- Acceptance: example 3; the template authorization tests; a pin write leaves description, tools and prompt unchanged.

## Landing obligations

- Exact-head PR CI green, including `test:api` and the LiveDB sweep (`test-api-store-it`), `validate-web` (check-docs) and `gate:repo`.
- Re-check that the new routes appear in `uzi docs` and the skill copy after `task docs:sync`.

## Decision Log

| # | Decision | Reason | Rejected alternative |
|---|---|---|---|
| D1 | A new allowlisted `PUT /api/me/model-settings` (`requireUser`) | Narrow Bearer write surface with an explicit field list; the established pattern for CLI-reachable self settings (`/me/usage-reserve`, `/me/codex-account-mode`, `/me/harness-overflow`) | Remount `PUT /api/me/settings` as `requireUser`: would expose appearance, sidebar and any future field to Bearer by default |
| D2 | One route for all model fields instead of one route per field | One logical CLI operation (for example cross-check model plus effort) must be atomic; fewer routes to keep in the auth tests | Per-field routes like `/me/codex-account-mode`: cannot write two related fields atomically |
| D3 | Share validation with `PutMySettings` by extraction | The two write paths must accept and reject identically (#3264 "reuse server validation") | Copy the validators: drifts |
| D4 | Template pins only for the caller's own `scope='user'` rows, 403 for builtin/global even for admins | The CLI ships no admin write verbs; builtin/global edits change every user's runs and stay web-only | Honour `authorizeTemplateWrite`'s admin branch: would add the first Bearer admin write |
| D5 | Separate `GET /api/me/agent-templates` with non-admin visibility | A Bearer read must not list other users' templates for an admin token | Remount the existing list as `requireUser`: admin tokens would read every user's templates |
| D6 | Leave `PutMySettings`'s non-transactional writes alone | Out of this PRD's outcome; the web path is unchanged and its tests stay the proof. #3263 (the single atomic Models-tab save) owns making `PutMySettings` transactional, reusing this PRD's shared decoder/writer | Fix it here: widens the diff into the web save path |
| D7 | The shared decoder/writer is the seam later children build on | #3252 (Q-claude) puts its locked settings-write invariant on it; #3263 reuses it for `PutMySettings` | A CLI-only writer: a third write path to keep in sync |
| D8 | Accept the judge and summary model fields, `judge_runs_on` and `cross_check_pins` on the Bearer route as a deliberate new authority, and say so rather than call the whole route non-additive | Non-additivity holds only for the worker model and effort lanes, which per-run overrides on `requireUser` `POST /repos/{id}/runs` already reach. A token that holds the new route can redirect which model or harness already-enabled judge, summary and cross-check work uses; no per-run route offers that. It cannot enable judge, summary or cross-check work: enabling cross-check stays cookie-only, as does every consent switch. A holder of such a token can already create runs, so the added reach is bounded to redirecting enabled work; if that is later judged too broad, the six fields move to a cookie-only route | Claim the whole route is non-additive (inaccurate for the judge and summary fields); drop those fields from the CLI (fails the issue's outcome) |
| D9 | A pin-only template edit leaves `customized` unchanged | `customized` only drives builtin refresh, and builtins are refused on this route; user templates are not refreshed from builtins, so the flag is not meaningful for them | Set `customized` on every pin edit: no effect for user templates and misleading in the list |
| D10 | A malformed template id is a 400 (matching the `PathUUID` handling on `/api/agent-templates`); a PUT 404 is always exit 4, and only the list's error-less 404 or 405 (or an error-less 405 on the PUT) means an older server | Keeps the two routes consistent, and a missing or other-user template must not read as an unsupported server | Treat every 404 as an older server: hides real not-found errors |
| D11 | The web save path's legacy `default_model` mirror can still go stale under concurrent harness changes; accepted as pre-existing | Out of this PRD's outcome (see D6); the new route resolves the harness before its transaction, the same accepted race as `PutMySettings` | Lock the harness read into the transaction: widens the diff into the web save path |
