# PRD #3248: Per-harness summary model (Codex lane for the intent and plan summaries), with a visible credential-failure note

**Issue:** #3248
**Sibling issues (children of #1703):** A #3255, B #3247, B2 #3251, C #3245, D #3248, E #3257, F #3254, G #3249, H #3244, I #3263, J1 #3256, J2 #3260, K #3261, L #3262, M #3259, N #3258, O #3264, P #3250, Q-spike #3246, Q-claude #3252, Q-codex #3253.  
**Status:** Draft r3 2026-10-21 (child D of epic #1703). Source anchors checked against origin/main `522aab1ba`. Since r2's `587b66d74` no anchor below changed; #3234 added `testOwnedPassHomeRoot()` (`agent/test/teardown-fixtures.ts`), which new agent tests use. Consumes child C's final r2 names.
**Priority:** Normal.
**Parent:** #1703. **Closes:** #1454. Child #3245 lands first (#3248 is blocked by #3245), and its PR says `Part of #1454`; this PR says `Closes #1454`. #3245 answers #1454's Judge question; this PRD answers its Summary question (option 2, one model per harness).
**Blocked by:** child #3245. This PRD consumes #3245's shared seams by their exact names and defines no copies (see "Consumed from child #3245").
**Related:**
- #2833 shipped the Codex intent/plan summaries on a private pin.
- #2721 and #2686: summary usage attribution. #2942: eligible kinds. #3014: the plan summary never blocks the verdict wait. #3010. #2603: the Now summary.
- PRD #362 (`prds/done/362-run-summaries.md`) and PRD #1798 (`prds/1798-plain-english-pr-descriptions.md`, D13).
- Children A, C, E, F, G, I, M, O.

**Delivery:** one PRD, one gated implementation run, one PR; default harness, mr_rework=false. Three serial milestones. One additive migration, draft number 00349 (M1); main ends at 00346 at the pinned SHA, so assign the next free number at landing with `task migration:renumber`. The credential note needs no migration (module 6). The run needs Docker for the LiveDB store test. No implementation or validation writes under `.github/workflows/`.

**Scope gate:** passes. The outcome is one capability. A user or admin chooses the model the Codex intent and plan summaries run on and sees the value in effect with its source. The run feed says when one of those summaries was skipped because the Codex credential could not be used. M3 (the note) is small and rides an existing path, so it stays in this PR.

**Open questions for the user:** none. Both product forks are decided (Decision Log, "User decision 2026-10-21").

## Problem

A user whose runs execute on Codex cannot choose the model their intent and plan summaries are written on. Since #2833, Codex runs do get both summaries, but on a private agent constant (`gpt-6-luna`, effort `low`).

The only Summary model setting (per user on Run defaults, per instance on Admin → Instance) uses the Claude vocabulary:
- Its picker offers Claude aliases only.
- Codex runs ignore it, and only one sentence of help text says so.
- An admin has no instance default for Codex summaries.
- An admin cannot reset the Claude instance value to the built-in, because blank is rejected.

When a Codex summary fails because the run's credential would need a refresh, nothing tells the user: the card silently shows the run title.

This is #1454's Summary question. It also means the epic's rule is unmet for summaries: every model setting is a Claude/Codex pair whose value in effect and source are visible.

## Verified baseline (origin/main 522aab1ba)

### Summary triggers today (both executors)

| Executor | Intent summary trigger (anchor) | Plan summary trigger (anchor) |
|---|---|---|
| Claude, `agent/src/sdk-executor.ts` | `summariesOn = isRunSummaryKind(ctx.kind) && !!oauthToken && !!this.client`. If `summariesOn && !ctx.summaryIntentPresent`, an `IntentFlight` starts asynchronously at run start, before planning, on both the planning and the seeded/pre-approved paths. It is not awaited, and is aborted with a bounded settle at teardown. It posts via `client.postIntentSummary` only if not abandoned, fenced or cancelled. | Runs from the `onPlanPersisted` callback passed to `ctx.gatePlan`. The runner (`agent/src/runner.ts`, `gatePlan`) invokes it only after the server accepted the plan: at a human gate when `ack.applied && ack.status === "awaiting_approval"`, or after autopilot persistence (`autopilotState.plan_md`). It is invoked again for each revised plan and is never awaited by the verdict wait (#3014). Posts via `client.postPlanSummary` with the NUL-stripped `plan_md`. |
| Codex, `agent/src/codex/codex-executor.ts` `CodexExecutor.run` | `summariesOn = isRunSummaryKind(ctx.kind)` (no token gate). If `summariesOn && !ctx.summaryIntentPresent && summaryAllowed()`, an intent flight starts before the plan-gate block, so a pre-approved resume gets one too. `summaryAllowed()` requires an open summary lifecycle and no abort, cancel or claim fence. Usage is emitted at most once (`flight.delivered`). | `beginPlanSummary()` is passed to `ctx.gatePlan` on the initial gate and on every revised gate. Each call bumps `planGeneration` (`supersedePlanSummary` aborts the earlier flight and clears `latestPlanSummary`). The flight runs only while `generation === planGeneration` and it is the current flight. The result is latched only after the API accepted it. The runner conditions are the same as for Claude. |
| Both | `isRunSummaryKind` (`agent/src/run-kind.ts`) admits `issue`, `self_improve` and `prompt` (a missing kind means issue). | There is no plan-summary pass without accepted plan persistence. Seeded and pre-approved runs, and pre-approved resumes that skip the gate, get none. A run can therefore reach the intent trigger but not the plan trigger. |

### Seams this PRD changes or relies on

| Seam | Anchor (symbol + file) | Current behavior |
|---|---|---|
| Codex intent/plan model | `CODEX_INTENT_PLAN_MODEL`, `CODEX_INTENT_PLAN_EFFORT`, `SummaryRunner.runModel` in `agent/src/summary-runner.ts` | Codex claim (valid `secrets.codex`): advice harness on `gpt-6-luna`, effort `low`, `refresh: "deny"`; `input.model` is ignored. Non-Codex claim: Claude on `input.token` + `input.model`. `git grep -n -F 'CODEX_INTENT_PLAN' -- agent web api` returns only those four `summary-runner.ts` lines. |
| Codex call sites | `summaryInputs` in `CodexExecutor.run` | Already passes `model: ctx.summaryModel ?? ""` (the claim's `summary_model`) to both passes, so the executor needs no model plumbing. |
| PR description (delivery) | `SummaryRunner.deliveryHarness`; `CODEX_PR_DESCRIPTION_MODEL` in `agent/src/codex/pr-description-model.ts` | Codex: fixed `gpt-6-sol`, no effort. Claude: the claim's `summary_model` if `isValidModel`, else `DEFAULT_DELIVERY_SUMMARY_MODEL` (`haiku`). So the Claude Summary model already governs Claude PR descriptions, which `docs/run-summaries.md` does not say. |
| Now line | `NOW_MODEL`, `NOW_MODEL_CODEX` in `agent/src/now-summary.ts` | Fixed `haiku` / `gpt-6-luna`; follows no setting on either harness (PRD #2603 D3). |
| Every agent reader of `summary_model` | `git grep -n -F 'summary_model' -- agent/src` | `protocol.ts` (field); `runner.ts` (`summaryModel: claim.summary_model`); `summary-runner.ts` (`DeliverySummaryClaimView`, the `deliveryHarness` Claude branch); `diagram-diagnostic.ts` `diagramIdentity` (Claude branch only, chosen by `selectCodexBinding(claim.secrets)`); `pr-description-model.ts` (a comment). No reader on the Codex branch. |
| Advice model contract | `CONTRACT_MODELS`, `resolveModel`, `renderCodexAdvice` in `agent/src/codex/render.ts`; `CodexAdviceHarness.run` in `agent/src/codex/codex-advice-harness.ts` | Advice accepts only `gpt-6-astra`, `gpt-5.6-sol`, `gpt-6-sol`, `gpt-6.1-sol` and `gpt-6-luna`. Any other value is dropped with `unknown_model`, and the harness then runs `rendered.model ?? this.provider.model` (`gpt-6.1-sol`). That is a **silent substitution** to a model about 20x pricier than luna (`codex-pricing.json`). |
| Credential at summary time | `makeCodexAdviceHarness` / `buildAdviceAuthConfig` (`codex/codex-executor.ts`); `CodexAppServerAuthSession` (`codex/appserver-auth.ts`); `runCodexAdviceModelPass` policy (`agent/src/model-pass.ts`) | The factory releases the run's frozen credential first; a release throw builds nothing. With `refresh: "deny"`, a server refresh request gets `REFRESH_FAILED_CODE` ("codex advice refresh is denied for this call"), so the run's credential generation never advances. Any error terminal becomes the generic `summary model call returned an error result`, which the runner swallows to a warn. **No note is shown anywhere**: the card falls back to the title, as `docs/run-summaries.md` documents ("an expired credential makes the summary fail"). The terminal does carry failure categories (`HarnessError.category`, incl. `authentication` and `authorization`, `agent/src/harness.ts`; `unauthorized` maps to `authentication` in `codex/terminal-normalize.ts`), but summary callers never see them. |
| Existing visible run-feed note path | Worker `ctx.emit({ kind: "status", agent: "worker", payload: { text } })`, e.g. `factsStatus` in `CodexExecutor.run`. Web: `RunEvent.tsx` `case "status"` → `MetaLine` via `describeStatus` (renders `payload.text`), `ActivityFeed.tsx` `case "status"`. CLI: `renderMessage` in `api/cmd/uzi/run_render.go` (`uzi run logs`), filtered only by `isAccountingMessage` (`accounting_presentation.go`); TUI `presentationFrames`. | A worker-authored status note is persisted through the claim-fenced batcher and rendered as inert text on the run page activity feed, in `uzi run logs` and in the TUI. `uzi run get` (detail rows) does not render messages. This path exists, so the note needs no new route or columns (module 6). |
| Claim resolution | `assembleClaim` in `api/internal/workersvc/claim_assembly.go` (the block after "Run-summary model resolution is user-value-wins") | `users.summary_model` (non-blank) wins, else `settings.Cache.SummaryModel` (non-blank; error ⇒ nil), else nil. No harness check. Sent on every `assembleClaim` claim as `ClaimPayload.SummaryModel` (omitempty, `claim.go`). Reads via `GetUserSummaryModel` (workersvc `Store`, `service.go`) and `SettingsReader.SummaryModel`. |
| Per-harness precedent on one field | `defaultModel` from `GetUserHarnessModelDefaults`, same function | `default_model` already carries the Codex lane on a Codex claim and the Claude lane on a Claude claim. |
| Instance key | `KeySummaryModel`, `DefaultSummaryModel = "haiku"` (`api/internal/settings/keys.go`); `Validate` case `KeyJudgeModel, KeySummaryModel` → `validateModelAlias` | Blank and whitespace are rejected, pinned by `TestSummaryModelAccessor` (`""` and `"   "` must error, `settings_test.go`). `UpdateSettings` upserts the raw value. |
| User settings API | `userSettingsDTO`, `PutMySettings`, `userSettingsResponse` (`api/internal/handler/user_settings.go`); mirror `apitypes.UserSettingsDTO` | `summary_model` is nullable (null = inherit). `validateModel` trims; blank ⇒ NULL; any family accepted. GET accepts `uzc_`; PUT is cookie-only (`routes_me.go`). |
| Contract fixtures | `fixtures/api-contract/settings_user_settings.{full,zero}.json` (Go `contract_test.go`); `user_settings.{full,zero}.json` (`web/src/lib/apiContract.test.ts`) | Shape-pinned DTO. |
| Web | `RunDefaults.tsx` "Run summaries" card | `<ModelSelect id="summary-model">`, always shown, copy says "your own Anthropic token". |
| Web lane rule | `RunDefaults.tsx` | `showClaudeLane = claudeUsable \|\| !codexUsable`, `showCodexLane = codexUsable`. |
| Web admin card | `SummarySettingsCard.tsx` | Free-text `Input`. Client guard "The summary model must not be empty.", pinned by `AdminSettings.test.tsx` `/summary model must not be empty/i`. |
| Demo mock (second implementation) | `web/src/mocks/mockApi/settings.ts` | Summary half of the admin branch (C splits the shared `judge_model \|\| summary_model` branch), the user `summary_model` patch branch, and the persisted-blob validators. |
| CLI | `newSettingsCmd` (`api/cmd/uzi/settings.go`) | Prints C's Model settings table once C lands. No setters (child O). |
| Usage | `usageReporter`, `SummaryPassName` (`summary-runner.ts`); `normalizeSummaryUsagePayload` / `foldSummaryUsage` (`workersvc/summary_usage.go`) | Usage is keyed by the requested model under `summary_pass:<model>`. |

## Outcome

Each user has a Claude and a Codex Summary model, and each instance has a Claude and a Codex default.
- The Codex value replaces the private agent pin for the Codex intent and plan summaries. Unset everywhere, it resolves to the built-in `gpt-6-luna`, so nothing changes for anyone who does not choose.
- Both pickers show the value in effect and its source, and `uzi settings get` prints the same.
- An admin can reset either instance value to its built-in.
- A Codex intent or plan summary skipped because the run's Codex credential could not be used leaves one fixed, bounded note in the run feed. It is advisory: no retry, and the run's outcome is unchanged.

Acceptance examples:

1. **User picks a Codex summary model.** A user with a usable Codex credential opens Settings → Run defaults → Run summaries. They see **Codex summary model: Default · gpt-6-luna (built-in)**, pick `gpt-6-astra` and save.
   - Their next Codex issue run's intent and plan advice requests carry model `gpt-6-astra`, effort `low`, refresh denied.
   - The run's usage shows the summary passes under `gpt-6-astra`.
   - Their Claude runs still summarize on `haiku`, their Codex PR descriptions still run on `gpt-6-sol`, and their Codex Now line still runs on `gpt-6-luna`.
2. **Admin sets and resets the instance default.** An admin sets the instance Codex summary model to `gpt-6.1-sol`.
   - A user with no own value sees **Default · gpt-6.1-sol (instance default)**. `uzi settings get` prints `summary  codex  Default  gpt-6.1-sol  gpt-6.1-sol  instance default` in C's Model settings table.
   - The admin picks **Default · gpt-6-luna (built-in)** and saves `""`. The next claim carries `gpt-6-luna`, and the source reads `built-in`.
   - Saving `"   "` for the Claude instance field also resets it: the stored value is `""`, the effective value `haiku`, the source `default`.
3. **Validation.**
   - `PUT /api/me/settings {"codex_summary_model":"opus"}` → 400.
   - `{"codex_summary_model":"gpt-7-custom"}` → 400 (no custom IDs).
   - `{"summary_model":"gpt-6-luna"}` → 400 (a Codex ID in the Claude lane).
   - `{"codex_summary_model":null}` and `{"codex_summary_model":"  "}` → 200, inherit.
4. **Credential failure.** A Codex run's subscription credential needs a refresh before the plan summary.
   - No plan summary is written. The run feed shows one worker status line, "Plan summary skipped: the run's Codex credential needed a refresh, which summaries may not do. The run continues." (reason `refresh_denied`). It appears on the run page activity feed, in `uzi run logs` and in the TUI.
   - The run's outcome is unchanged, no Anthropic call is made, the credential generation does not advance, and no second attempt is made in that plan generation.
   - A network error while releasing the credential for the same pass writes no note.

## Out of scope

- The Now line's model on either harness, and the Codex PR-description model (user decision, D2). Summary reasoning effort (D6).
- "Runs on" for summaries (child E). The Models tab (child I). The admin model-defaults grid (child M).
- The general resolver endpoint (child F). It goes last and calls this PRD's resolver.
- CLI setters (child O). `PUT /me/settings` and `PUT /admin/settings` stay cookie-only.
- Custom Codex model IDs for summaries (D4).
- Credential-failure notes for Claude summaries, the PR-description pass and the Now line (D7).
- A summary-card or `uzi run get` rendering of the note (D7).
- The judge model and its instance reset (child C).

## Modules and seams

### Consumed from child C (exact names from C's r2; D defines no copies)

| C seam | Exact name | How D uses it |
|---|---|---|
| Codex model fixture | `fixtures/codex-models/models.json` (`curated`, `role`) | D's Codex summary vocabulary **is** `role`. D adds nothing to this file (it holds named sets only). |
| Go set | `agenttmpl.CodexRoleModels` | Codex accept rule and validator. |
| Claude-lane guard | `agenttmpl.ValidateClaudeLaneModel` | `summary_model`, at user and instance level. |
| Generic resolver | `agenttmpl.ResolveTieredModel` (result `ModelResolution`) | Wrapped by D's `agenttmpl.ResolveSummaryModel`. |
| Instance accessor | `Cache.InstanceModel(ctx, key)` | Both summary keys. Error posture as C defines it: log, then treat the tier as unset. |
| Trim-at-storage | `settings.NormalizeForStorage` | D adds `summary_model` and `codex_summary_model` to its key set. |
| Settings cell | `apitypes.ModelSettingCellDTO` | `{harness, model, default_model, default_source, resolved_model, source}`, built by C's handler cell helper. D's field is `summary_models`. |
| Agent role export | `isCodexRoleModel` (`agent/src/codex/render.ts`) | Codex summary model check. |
| Web role export | `CODEX_ROLE_MODELS` (`web/src/lib/codexModels.ts`) | Codex summary picker list. |
| Label table | `fixtures/model-settings/contract.json` `source_labels`, rendered by C's `modelSettingSourceLabel` (`web/src/lib/modelSettings.ts`) and Go `modelSettingSource` | Every summary source label. |
| CLI table | C's `uzi settings get` "Model settings" table (`SETTING HARNESS STORED DEFAULT RESOLVED SOURCE`) | D adds the two `summary` rows through C's function. No second table. |
| Admin defaults | C's `settingsResponse.defaults` map | Every web "Default · X (built-in)" summary label. |

D's own fixture facts go in a sibling file in C's directory, `fixtures/model-settings/summary.json`: `{"builtins": {"claude": "haiku", "codex": "gpt-6-luna"}, "cases": [...]}`. The reasons:
- `models.json` holds named model sets only ("later landers may add named sets"), and a built-in is not a set.
- `contract.json`'s readers are C's, with case-count assertions keyed to `judge_cases`. A sibling leaves C's readers untouched.

### 1. `agenttmpl.ResolveSummaryModel` (wraps C's generic resolver)

- **Interface:** `ResolveSummaryModel(harness, user, instance string) ModelResolution`, beside C's `ResolveJudgeModel` (same package, same signature, same result type).
  - Built-ins: new constants `agenttmpl.DefaultClaudeSummaryModel = "haiku"` and `agenttmpl.DefaultCodexSummaryModel = "gpt-6-luna"`. `settings.Defaults` references them, and `settings.DefaultSummaryModel` becomes an alias of the Claude one (C's import-cycle pattern).
  - Claude `accept`: not in `CodexRoleModels` (C's judge Claude rule).
  - Codex `accept`: in `CodexRoleModels`.
- **Output:** `ModelResolution{Model, Source, DefaultModel, DefaultSource, Dropped}`. Summaries record no run note for `Dropped`: they are advisory, and the settings cell shows the rung actually used.
- **Invariants:** `Model` is never empty. The Codex `Model` is always a `role` member, so the advice renderer never substitutes.
- **Callers (only these):** `assembleClaim` and `userSettingsResponse` (via C's cell helper). F later calls it.
- **Mirror:** the demo mock is a deliberate second implementation, pinned by `fixtures/model-settings/summary.json` `cases`.

### 2. Settings (`api/internal/settings`)

- **New key:** `KeyCodexSummaryModel = "codex_summary_model"`, with a `Defaults` entry (→ `agenttmpl.DefaultCodexSummaryModel`). It is `Known` and non-secret, and its own `Validate` case accepts `""` or a `CodexRoleModels` member after trimming. Distinct messages for a Claude alias and for "not a Codex summary model".
- **Claude key:** `KeySummaryModel` gets its own `Validate` case, `ValidateClaudeLaneModel`, so `""` and whitespace-only are accepted as the reset.
- **Trim-at-storage:** both keys join `NormalizeForStorage`'s set, so `"   "` is stored as `""`. Without this, `Cache.effective` would keep `"   "`, the Claude claim would drop it, and Claude summaries would silently move to the SDK default model (review-D R1).
- **Removals:** `Cache.SummaryModel` and `SettingsReader.SummaryModel` go, with their fakes, once `assembleClaim` reads through `InstanceModel`. C's r2 anticipates this. The deadcode gate does not catch test-only survivors, so the removal is explicit.

### 3. User column and API (`users.codex_summary_model`, `PUT/GET /api/me/settings`)

- **Migration (draft 00349):** `ALTER TABLE users ADD COLUMN codex_summary_model text;` and a goose Down that drops it.
  - Nullable, no default, no backfill: NULL means inherit, which resolves to today's behavior. A Codex run never read the Claude lane, so there is nothing to copy.
  - No CHECK constraint, as for `summary_model`.
- **Queries:**
  - Add the column to `GetUserSettings`.
  - A setter mirroring `SetUserSummaryModel`.
  - Widen `GetUserSummaryModel` (or add a sibling) to read both summary columns in one row for the claim. The workersvc `Store` interface and its in-memory fake follow.
  - `sqlc generate`.
- **PUT fields:**
  - `codex_summary_model` is nullable: absent = untouched; null or blank after trim = inherit; otherwise it must be a `CodexRoleModels` member.
  - `summary_model` switches to `ValidateClaudeLaneModel` (user decision).
  - Errors are 400 with the field name.
- **GET and PUT response:** adds `codex_summary_model` (stored) and read-only `summary_models`, two `ModelSettingCellDTO` cells in the order claude, codex, from C's cell helper. It is response-only: the PUT rejects it as an unknown field. It carries no credential material. `apitypes.UserSettingsDTO` mirrors both fields.

### 4. Claim assembly (`assembleClaim`)

`ClaimPayload.SummaryModel` now means "the summary model for this claim's harness" (D3):
- A Claude claim gets `ResolveSummaryModel("claude", ...)`, which equals today's value except for the two deltas in D5 and D12.
- A Codex claim gets `ResolveSummaryModel("codex", ...)`.

Same field, same omitempty, no protocol change. With settings unwired, both stay nil, as today. The value is resolved per claim (`fixed_at = claim`): a resume or a requeue re-resolves it.

### 5. Agent: Codex intent/plan model (`agent/src/summary-runner.ts` and a leaf module)

- **Leaf module:** new, no imports, following the pattern of `codex/pr-description-model.ts`, e.g. `agent/src/codex/summary-model.ts`. It holds `DEFAULT_CODEX_SUMMARY_MODEL = "gpt-6-luna"` (the old-server fallback, pinned to `summary.json` `builtins.codex`) and `CODEX_SUMMARY_EFFORT = "low"`. These replace the two private constants.
- **`runModel` Codex branch:** use `input.model` when `isCodexRoleModel(input.model)`, else `DEFAULT_CODEX_SUMMARY_MODEL`. An older server sends the Claude lane value here, so log at info, not warn. Effort stays `low` and refresh stays `deny`.
- **Unchanged:** the Claude branch, `deliveryHarness`, `now-summary.ts`. `diagramIdentity` is also unchanged; its accepted misreport is noted in Risks.
- **Invariant:** a Codex summary advice request always carries a contract model.

### 6. Credential-failure note via the existing run-feed status path (M3)

The existing path is enough, so this adds no route, no columns and no migration. A worker `status` message with `payload.text` is persisted through the claim-fenced batcher and rendered inert on the run page (`RunEvent.tsx` `case "status"` → `describeStatus`), in `uzi run logs` (`renderMessage`) and in the TUI (`presentationFrames`).

**Classification (`model-pass.ts` and `summary-runner.ts`).** Only typed credential-state refusals classify, each to one closed `SummarySkipReason`. Message text never decides.

| Reason | Fires exactly when (typed evidence) | Fixed sentence (after "Intent summary skipped:" / "Plan summary skipped:") |
|---|---|---|
| `refresh_denied` | The `refresh: "deny"` bridge built by `buildAdviceAuthConfig` refused a refresh the app-server requested during this call. The deny bridge records a typed flag that the pass reads, proven by construction. | "the run's Codex credential needed a refresh, which summaries may not do." |
| `vault_locked` | The advice factory's credential release rejected and `codexDeferralReason(err) === "vault_locked"` (the existing typed 409 classifier in `agent/src/client.ts`). | "the run owner's vault is locked." |
| `account_unavailable` | The release rejected and `codexDeferralReason(err) === "account_unavailable"`. | "the run's Codex account is unavailable." |
| `credential_rejected` | The error terminal's `HarnessError.category` is `authentication` (the provider's typed `unauthorized`, `codex/terminal-normalize.ts`), read through the terminal's existing failure materialization. The provider gives no further cause. | Generic: "the run's Codex credential could not be used." |

Every sentence ends with "The run continues." Text is per closed reason; the one reason whose cause is not proven beyond "the provider rejected it" gets the generic sentence.

**Never fires** (stays silent, as today):
- A release rejection that is not a typed deferral: transport or network errors, 5xx, abort, capability or ownership loss, a 409 with an unknown reason.
- The `authorization` category (inside the Codex executor it also carries vault-locked and lifecycle-cancelled reconcile errors, so it is ambiguous).
- Timeout, refusal, model or effort error, empty or unparseable output, a missing factory, a malformed codex block, an abort.

The classification is typed. One way: a `ModelPassCredentialError extends Error` thrown by the Codex path, carrying the reason and the existing generic message, so judge, review and Now callers keep their behavior.

**Agent seam.** `IntentSummaryInput` gains an optional `onSkip?(reason: SummarySkipReason)`, called at most once and on the Codex branch only. The generators still return `null`.

**Emission (`CodexExecutor.run`).** Each flight records the reason. Under the same guards that gate today's summary post, it emits one note:
- intent: the flight is current, not abandoned or aborted, and `summaryAllowed()` holds;
- plan: `allowed()` holds, i.e. the generation is current and so is the flight.

The note is `ctx.emit({ kind: "status", agent: "worker", payload: { text, event: "summary_skipped", pass, reason } })`:
- `text` is the fixed per-(pass, reason) sentence from the table above (eight constants).
- `pass` ∈ {`intent`, `plan`}; `reason` ∈ {`refresh_denied`, `vault_locked`, `account_unavailable`, `credential_rejected`}.
- `event`, `pass` and `reason` are machine-readable for `--json` and later surfaces. `event: "summary_skipped"` collides with no event any usage reader keys on (`init`, `result`, `codex_response_usage`), so usage folding ignores it.

**Bounds and retries.**
- No provider or model text; closed enums only.
- At most one note per intent flight and per plan generation.
- No retry: one attempt per flight, as today. An existing re-claim still regenerates the intent summary when none is stored (unchanged).
- Old workers never emit it. An old API stores it as an ordinary status line, so there is no rollout dependency.

### 7. Web interim controls

- **`ModelSelect` `models` prop:** an optional closed `models` list override, curated options only, custom entry only with `allowCustom`. Omitted ⇒ today's harness lists, so every existing caller is unchanged. D adds it unless G landed it first, in which case D reuses G's.
- **Run defaults "Run summaries" card:**
  - The Claude picker shows under `showClaudeLane`, the Codex picker under `showCodexLane`: the page's lane rules, which C's judge pair also uses. Behavior change: a Codex-only user no longer sees the ineffective Claude picker.
  - The Codex picker passes `models={CODEX_ROLE_MODELS}`.
  - Each label comes from its cell: `Default · <default_model> (<modelSettingSourceLabel(default_source)>)`. One Save per lane.
  - The card copy is harness-neutral. Per-cell help states the scope: the Claude cell covers intent, plan and PR description; the Codex cell covers intent and plan (Codex PR descriptions use `gpt-6-sol`); neither drives the Now line.
- **Admin `SummarySettingsCard`:**
  - The free-text field becomes a Claude | Codex pair of `ModelSelect`s, like C's admin Judge pair. Each pair's only empty option is the reset, labelled `Default · <defaults[key]> (built-in)` and selected iff `sources[key] === "default"`.
  - Save sends only changed keys (`""` for the reset). The client's "must not be empty" guard goes.
  - Coordination with child A: if A has already converted this field to a `ModelSelect` with no empty option, D adds the reset option to A's select. Otherwise D converts it, and A skips this field.
- **Demo mock:**
  - Mirror both user fields, the instance key, trimming, `defaults`/`sources` for the summary keys, the Claude-lane guard and `summary_models`.
  - `summary_models` comes from C's mock resolver function, extended with the summary rules and pinned by `summary.json` `cases`.
  - Touch only the summary half of the branch C split. A persisted pre-change blob stays valid.
- **Mock screens:** there is no mock screen for these interim controls. The final surfaces are child I (`AfterModels.dc.html`) and child M (`AfterAdmin.dc.html`). Their Codex summary value (`gpt-6-astra`) is stale: they should render resolver output.

### 8. CLI read

`uzi settings get` adds the two `summary` rows to C's Model settings table through C's function. `--json` carries `codex_summary_model` and `summary_models`. The credential note reaches the CLI through `uzi run logs`; `--json` carries the closed fields. There is no setter (child O).

## Testing decisions

- **Resolver** (Go, through `ResolveSummaryModel` only): authored cases in `fixtures/model-settings/summary.json` `cases`, with a count assertion (count equals the length of `cases`). The cases:
  - harness × {user set, user NULL, user whitespace, user out of vocabulary};
  - harness × {instance set, instance `""`, instance out of vocabulary};
  - Claude: a legacy stored Codex ID (curated, and `gpt-6-luna`) is dropped to the next tier (D5);
  - the Codex `Model` is always a `role` member;
  - the default fields equal the walk without the user tier.

  Follow C's `judge_cases` table test. The web mock resolver runs the same file. Go asserts `builtins` equals the two `agenttmpl` constants.
- **Settings:**
  - Both summary keys accept `""`, and `"   "` stores `""` via `NormalizeForStorage`.
  - `TestSummaryModelAccessor` is repointed: its `""`/`"   "` error rows invert, and its accessor half goes with `Cache.SummaryModel`. Watch it red first.
  - The Claude key rejects each `role` member. The Codex key rejects `opus`, `gpt-7-x` and `two words`.
  - No assertion about `judge_model` (C owns it).
- **Admin PUT** (handler): `summary_model: "   "` → 200, stored `""`, GET effective `haiku`, source `default`. The same for `codex_summary_model`, which yields `gpt-6-luna`.
- **User PUT/GET** (handler):
  - Round trip, clear, and whitespace → inherit.
  - Each 400 in example 3.
  - An absent field leaves the other lane untouched.
  - Cells for every source.

  Follow `TestPutMySettingsSummaryModelRoundTrip` and its siblings (`user_settings_test.go`). Update the contract fixtures.
- **Store:** LiveDB round trip of the column, following `TestUserSummaryModelLiveDB`.
- **Claim:**
  - `TestClaimCarriesSummaryModelFromInstanceDefault`, `TestClaimSummaryModelUserOverrideWins` and `TestClaimOmitsSummaryModelWhenNoSettings` (`service_test.go`) stay green, repointed to `InstanceModel`.
  - New Codex siblings. A Codex claim carries the user's `gpt-6-astra`, **watched red** on current code (it carries the Claude value).
  - A Claude claim with a legacy stored `gpt-6-sol` carries the instance value.
- **Agent model selection** (`agent/test/summary-runner-codex.test.ts`): parametrize the Codex selection test over the claim `model` ∈ {absent, `gpt-6-astra`, a Claude alias, an unknown ID}.
  - Expect `gpt-6-astra` for the member and `gpt-6-luna` otherwise, effort `low`, refresh `deny`, zero Claude calls. The `gpt-6-astra` case is **watched red** today.
  - Usage: `summary-runner-usage.test.ts` asserts the snapshot key is the configured model.
  - Executor: in `codex-executor.test.ts`, beside the `requests.map(r => [r.model, r.effort])` luna/low check, run a configured model through a revised gate. The superseded flight reports no usage, and the replacement uses the configured model.
  - The agent reads `summary.json` `builtins.codex` and asserts it equals `DEFAULT_CODEX_SUMMARY_MODEL` and that `isCodexRoleModel` is true for it.
- **Credential note** (the regression for the user decision), **watched red** (no note today):
  - **Summary-runner seam:** `onSkip` fires exactly once with the matching reason for: a denied refresh driven through the existing advice-harness fake transport (`refresh_denied`); a release rejected with a typed 409 `vault_locked` and `codex_account_unavailable` body (`vault_locked`, `account_unavailable`); an `authentication` error terminal (`credential_rejected`).
  - **Never fires** (each a named case): a release rejected with a transport `TypeError`, a 5xx `RequestError`, a 409 with an unknown reason, an abort; an `authorization` error terminal; a timeout, refusal, model error, empty output, missing factory, malformed binding. In every case the result is null, there are zero Claude calls and no second factory build.
  - New agent tests take their `homeRoot` from `testOwnedPassHomeRoot()` (`agent/test/teardown-fixtures.ts`, #3234).
  - **Executor:** exactly one status message with the closed fields and the reason's fixed text per intent flight and per plan generation. None after a fence, cancel or supersede. No `postIntentSummary`/`postPlanSummary`. The run's terminal outcome is unchanged.
  - **Rendering:** web `RunEvent` renders the fixed text through `describeStatus` (no new branch needed; assert it). CLI `renderMessage` prints it, and `isAccountingMessage` is false for it.
- **Web:**
  - `RunDefaults.test.tsx` ("per-user summary model" describe): a Codex-only user sees the Codex picker with exactly `CODEX_ROLE_MODELS`, no Claude alias and no custom entry, and does not see the Claude picker. This is the #1454 vocabulary regression, **watched red** (there is no Codex picker today).
  - Labels come from `modelSettingSourceLabel`, and Save sends `codex_summary_model`.
  - `ModelSelect.test.tsx`: the `models` override, and an omitted prop leaves the lists unchanged.
  - `AdminSettings.test.tsx`: `/summary model must not be empty/i` is repointed. The Default option sends `""`, and a `default` source selects it.
  - Run the retire-a-string sweep (`.claude/rules/web.md`) for "your own Anthropic token" on the summary card.
- **CLI:** the Model settings table with summary rows (stored, default, unknown-source and two-cell-cap cases), using C's golden-style test.
- **E2E:** `git grep -n -F -e summary_model -e gpt-6-luna -e summary_skipped -- e2e` is empty at the pinned SHA. Re-run it before the PR and record the result.
- **Gates:** `task gate:api`, `task gate:agent`, `task gate:web`, `task gate:repo` (migration numbering and additivity, changelog fragment), `task check-docs:web` and `task docs:sync`.

## Milestones

### M1: Codex summary model at user level, end to end (with the instance key backend)

Blocked by: child #3245 (PRD-level; all consumed seams must be on the base).

- **Server:**
  - `ResolveSummaryModel` and the built-in constants; `fixtures/model-settings/summary.json`.
  - The settings key and validators, the `NormalizeForStorage` set, and `Cache.SummaryModel` / `SettingsReader.SummaryModel` removal.
  - Migration 00349 (draft), the queries, and `sqlc generate`.
  - PUT/GET fields and `summary_models` cells; claim assembly per harness.
- **Agent:** the leaf module and `runModel`.
- **Web:** the `ModelSelect` `models` prop; the Run defaults pair with lane rules, labels and copy; the demo mock (user side); the contract fixtures.
- **CLI:** the summary rows in the Model settings table.
- **Docs, specs, changelog:**
  - `docs/run-summaries.md` "Which model it runs on": both cells and their scope, including that the Claude cell drives Claude PR descriptions.
  - `docs/cli.md` `uzi settings get`, then `task docs:sync`.
  - A terse `specs/human.md` entry `(AI-synced YYYY-MM-DD)`.
  - A changelog fragment `changelog.d/<uuid>.md` citing #3248.
- **Acceptance:**
  - Examples 1 and 3 pass as tests.
  - The three watched-red tests (claim, agent, web vocabulary) are recorded red, then green.
  - The Claude agent tests are green.
  - With nothing configured, every Codex summary request is `gpt-6-luna`/`low`.
  - All gates are green.
- **Safe alone:** the instance Codex key is settable only through the admin API, and it defaults to the built-in.

### M2: Instance defaults for both summary lanes on Admin → Instance, with reset

Blocked by: M1.

- The `SummarySettingsCard` pair with reset options from `defaults`, the child A coordination clause, the mock admin side, and `AdminSettings.test.tsx` repointed.
- `docs/admin-settings.md` "Run summaries", then `task docs:sync`; update the changelog fragment.
- **Acceptance:** example 2 works end to end (the admin sets a value, the user label and CLI source follow, the admin resets including with whitespace, and the next claim follows). Gates green.

### M3: Visible credential-failure note for Codex intent and plan summaries

Blocked by: M1 (the same agent seams; serial in one PR).

- The classification in `model-pass.ts`, `onSkip` in the summary runner, and the executor status emission with the two fixed texts.
- `docs/run-summaries.md` "It's advisory" and "Which model it runs on": replace "an expired credential makes the summary fail" with the note, and say where it shows. Then `task docs:sync`, a specs entry, and an update to the changelog fragment.
- **Acceptance:** example 4 passes as tests, and the credential-note regression is watched red, then green. Gates green.

## Risks

- **Rollout: new API, old workers.** This is the common window, because the worker fleet is pinned separately.
  - A Codex claim's `summary_model` carries the Codex lane. An old worker ignores it (it keeps the private luna pin) and emits no note.
  - Accepted race: a user's Codex choice is not honoured, and no note appears, until the fleet runs this PR's agent. The settings page shows the requested value, not an observation.
  - Boundary: until the worker pin includes this PR. Reason: an advisory pass on the cheapest model, with no safety or correctness effect. Usage stays attributed to the model actually used.
- **Rollout: old API, new workers.**
  - A Codex claim carries the Claude lane value (e.g. `haiku`). It is not a `role` member, so the worker uses `gpt-6-luna`, which is today's behavior.
  - The note is an ordinary status line that any API version stores.
  - Residual: a legacy Claude value that is itself a `role` member would be honoured. That is reachable only for a user who stored a Codex ID in the Claude field before the guard existed. Accepted.
- **Delayed writes.** A setting changed mid-run applies at the next claim, like every live-read setting. A late note from a superseded plan flight is suppressed by the generation guard, and a fenced claim's batcher drops it.
- **Silent substitution (riskiest assumption):** that the advice renderer never sees a non-contract summary model. It is validated early in M1 by the parametrized agent test (an unknown ID maps to `gpt-6-luna`, never to undefined) and by `isCodexRoleModel` over the built-in.
- **Classification precision (second riskiest):** that every firing case is backed by typed evidence.
  - M3 starts with the denied-refresh fake-transport test, which proves the deny-bridge flag is observed by the pass.
  - Release failures classify only through `codexDeferralReason`; transport, 5xx, abort and unknown-reason 409 stay silent.
  - `authorization` is excluded because it also carries vault-locked and lifecycle-cancelled reconcile errors.
  - The note never names a cause it cannot prove: `credential_rejected` uses the generic sentence.
- **Note visibility limits.** The note shows in the run feed, `uzi run logs` and the TUI, not on the summary card or in `uzi run get` detail rows. Accepted (D7): it is the simplest visible path, it is append-only and claim-fenced by construction, and child I can add a card hint later from the closed `event`/`pass` fields.
- **Field meaning change.** `summary_model` on a Codex claim changes from "Claude lane, unused" to "Codex lane".
  - Every Codex-branch reader ignores it except the one this PRD changes (baseline grep).
  - This relies on Codex claims carrying no Anthropic token (PRD #1798 D13, `harness_claim_repair_livedb_test.go`).
  - Accepted, diagnostic-only inaccuracy (R12): a Codex-harness claim with an unusable codex block takes `diagramIdentity`'s Claude branch. It would log `editor_model` as the Codex lane ID with `harness: "claude"`. No pass runs, because there is no Anthropic token.
- **New state: readers and writers.**
  - Writers: `PUT /me/settings` (cookie), `PUT /admin/settings` (cookie, admin), the demo mock, and the worker status note (existing batcher).
  - Readers: `GET /me/settings` (session or `uzc_`); `assembleClaim` (every kind goes through it, but only summary kinds consume the value); `GET /admin/settings`; `uzi settings get`; Run defaults; the admin card; the run feed surfaces for the note.
  - Untouched: judge claims, `/api/v1` and `api/openapi/v1.yaml`.
- **Behavior changes to existing surfaces:**
  - A user PUT of `summary_model` with a `role` ID returns 400 (user decision).
  - The instance `summary_model` accepts `""` and whitespace as a reset, where it returned 400 before.
  - The Claude summary picker is hidden for a Codex-only user (the page lane rule).
  - A Claude claim drops a legacy stored Codex ID to the next tier (D5).
  - A settings-cache read error yields `haiku` for Claude instead of no model (D12).
  - No stored values are rewritten.
- **Sibling overlap:**
  - Child A: the admin Summary field conversion (module 7 clause). Whichever lands second rebases.
  - Child G: the `ModelSelect` `models` prop. The first lander adds it.
  - Children I and M carry both cells and render resolver output.
  - Child E can reuse the note pattern for its visible "Runs on" skip, and D3 leaves it room.
  - Child F (last) calls `ResolveSummaryModel` and drops its `not_configurable` Codex summary row.
- **Migration:** additive and nullable. An older API image ignores the column, and Down drops it. The number is a draft.

## Decision Log

- **D1. The Codex value is model-only and lives beside the existing Claude columns.**
  - New: `users.codex_summary_model` and the instance key `codex_summary_model`. `summary_model` stays the Claude lane.
  - Reason: additive, and it matches `default_codex_effort` (00285).
  - Rejected: a keyed per-harness JSON setting.
- **D2. User decision 2026-10-21: the Codex Summary model does not drive Codex PR descriptions.**
  - It governs the Codex intent and plan summaries only. Codex PR descriptions stay on `gpt-6-sol` (PRD #1798 D13), and the Now line stays fixed on both harnesses (PRD #2603 D3).
  - Reasons: it replaces exactly the #2833 pin, with zero change when unset. A cheap summary choice must not silently degrade every PR body. Excluding the Now line is lane parity, since the Claude Now line already ignores the Summary model.
  - Cost: the pair is asymmetric on PR descriptions; the copy says so per cell.
- **D3. Reuse `summary_model` on the claim as "the summary model for this claim's harness".**
  - Reason: `default_model` already works this way. Old workers ignore the field on Codex, so it needs no protocol version or capability.
  - Rejected: a new claim field.
- **D4. The Codex vocabulary is C's `role` set, with no custom IDs.**
  - Reason: the advice renderer drops any other ID, and the harness then silently runs `gpt-6.1-sol`. `gpt-6-luna` is the built-in. C records why its judge uses `curated` instead.
  - Rejected: `curated` (it lacks the built-in); custom IDs with capability gating.
- **D5. User decision 2026-10-21: reject a Codex ID in the Claude summary lane.**
  - New writes go through `ValidateClaudeLaneModel`.
  - On read, the Claude `accept` rule is C's judge rule: a legacy stored Codex ID is dropped to the next tier, with no note.
  - This is a deliberate change from r1's "honour on read". Today such a value makes every Claude summary fail at the SDK, so dropping it turns a guaranteed failure into a working summary. It also keeps judge and summary on one rule.
  - Rejected: honouring it on read (r1; keeps the failure); rewriting stored data.
- **D6. Summary effort stays fixed: `low` on Codex, unsent on Claude.**
  - Reason: no demonstrated need.
- **D7. User decision 2026-10-21: restore the visible credential-failure note.** It is bounded, sanitized and visible; advisory; no refresh and no retry; the run outcome is unchanged; regression-tested.
  - **Typed classification only, four closed reasons, fixed text per reason** (r3): `refresh_denied` (deny-bridge flag), `vault_locked` and `account_unavailable` (`codexDeferralReason`), `credential_rejected` (`authentication` category, generic text). Transport errors and other untyped failures never fire; `authorization` is excluded as ambiguous.
  - **Rejected (r2):** classifying any release rejection or the `authorization` category, which would label a network blip or a cancelled lifecycle as a credential refresh.
  - **Delivered on the existing worker `status` run-feed path:** a fixed text plus closed `event`/`pass`/`reason` fields. The baseline anchor proves the path exists (`ctx.emit` `status` → `RunEvent.tsx` `describeStatus`, `renderMessage`, `presentationFrames`). So there is no new route, no `runs` columns and no second migration.
  - **Scope:** Codex intent and plan only, the lane this PRD changes and the epic's stated case.
  - **Rejected:** r2-draft's `POST /summary/skip` route with two `runs` columns. It makes the note show on the card and in `uzi run get`, at the cost of a route, a migration and plan-persistence clears; not needed, because the existing path is visible.
  - **Rejected:** silence (r1; overruled by the user). Provider error text (unbounded).
- **D8. Reset = store `""`, with instance values stored trimmed (shared design v4.1 §5, C's `NormalizeForStorage`).**
  - No new rejection, no delete route.
- **D9. The summary resolver wraps C's `ResolveTieredModel` in `agenttmpl`.**
  - Reason: one package, one signature and one result type for judge and summary (C D3, cross-review X4). The value shown is the value claimed.
  - Rejected: r1's own resolver, and r2-draft's `workersvc` placement.
- **D10. Built-ins are `agenttmpl` constants, and the agent keeps a mirrored fallback, pinned through a sibling fixture in C's directory.**
  - `fixtures/model-settings/summary.json` holds `builtins` and `cases`, because `models.json` holds named sets only and C's `contract.json` readers are C's.
  - The web reads built-ins from `defaults` and the cells, never from a constant.
  - Rejected: D's own vocabulary fixture (r1, cross-review X5); comment-only mirroring.
- **D11. The interim web follows the page's lane rules and C's admin pair.**
  - Rejected: always showing both pickers; keeping the free-text admin input.
- **D12. Adopt C's accessor error posture for both lanes.**
  - On a cache read error, Claude now yields `haiku` instead of no model. Before, the Claude passes then ran on the SDK default.
  - Reason: one posture on a shared seam.
- **D13. No ADR.**
  - The invariants are pinned by tests and stated in `docs/run-summaries.md`: a Codex summary model is always a `role` member, and a credential note is only ever the closed reason.
