# PRD #1551: Per-harness worker model defaults

**Issue:** [#1551](https://github.com/vtmocanu/uzi/issues/1551)

**Status:** Reviewed by two independent lenses and committed for later implementation. Not queued or dispatched to uzi.

**Priority:** Medium

**Related:** [PRD #1106](1106-codex-harness-phase1.md), completed public activation [PRD #1429](done/1429-codex-public-activation.md), judge-model follow-up [#1454](https://github.com/vtmocanu/uzi/issues/1454).

**Reviewed UI direction:** [interactive grouped-settings mock](mockups/1551-per-harness-model-defaults.html).

Use current `main` and a short-lived worktree. Never switch the `main/` worktree away from `main`. This PRD changes no workflow file.

## Problem

Uzi now lets a user choose Claude or Codex for a run, but still stores one shared `users.default_model`. The Run Defaults page therefore presents the model vocabulary for only the currently effective harness. Changing the default harness clears an incompatible saved model instead of retaining one preference for each provider.

That creates three user-facing defects:

1. A user who alternates between Claude and Codex must reselect the model after changing the default harness.
2. The separate **Default harness** and **Worker model** cards present one coupled decision as two unrelated settings.
3. Codex exposes only the two curated model IDs. Unlike Claude, it has no **Other (custom model ID)** escape hatch, so a newly available or account-specific model cannot be selected until uzi ships a picker update.

The current behavior was deliberate in #1106/#1429: Codex used a closed two-model vocabulary, and shared model storage made an incompatible value fall back visibly. This PRD intentionally supersedes that part of D9/D6. It does not weaken provider, endpoint, credential or harness boundaries.

## Solution

Group the settings into one **Harness and worker models** card:

1. Show **Default harness** at the top when both harnesses are usable, preserving today's `Use my default | Claude | Codex` behavior.
2. Show one retained model lane per usable harness below it. When both are usable, Anthropic and Codex appear side by side; a one-harness user keeps a simple one-lane form with no redundant harness selector.
3. Save the default harness plus both model defaults in one request and one database statement. Changing the harness never clears either model.
4. Keep the curated choices for fast selection, and add **Other (custom model ID)** to the Codex default lane using the same syntax/safety validation as Claude.
5. Resolve the harness first when a run is created, then read only that harness's user-model lane. An explicit schedule model still wins for that run. An unset lane still inherits the existing harness fallback.

The implementation adds nullable `default_claude_model` and `default_codex_model` columns. The legacy `default_model` column stays intact as a compatibility projection for one release, so an upgrade and immediate rollback keep the pre-upgrade value and stale web assets retain their old contract.

## User journey

### Both harnesses usable

The user opens **Settings → Run defaults** and sees one card. They select a default harness, choose an Anthropic model, choose a Codex model, and save once. Selecting another default harness updates which lane carries the **Default harness** badge but leaves both selections untouched. A future run without an explicit harness uses the chosen harness and its saved model.

### One harness usable

The redundant harness selector stays hidden, as today. Only the usable harness's model lane is shown. A saved preference for a temporarily unavailable harness remains stored and reappears if that harness becomes usable again.

### No harness usable

The card remains available, preserving today's ability to choose a model before connecting a credential. It shows the Claude lane and no harness selector. The Codex lane remains stored but hidden until Codex becomes usable. Settings GET and PUT never fail merely because no credential is usable; the legacy `default_model` compatibility field deterministically projects to and writes the Claude lane in this state.

### Custom Codex model

The user chooses **Other**, enters a model ID, and saves it. Uzi validates the string for length and unsafe characters but does not make a provider call at save time. The fixed OpenAI provider receives that ID on the next Codex run. An unavailable or unsupported ID fails on that run, exactly as an unavailable custom Claude ID does today. Uzi never treats the string as a provider URL or configuration fragment.

## Resolved facts

Verified locally against `main` at `698a3109` on 2026-09-22. Recheck anchors before implementation.

- `api/internal/store/migrations/00031_user_default_model.sql` added the one nullable `users.default_model` column. Migration 00226 later added nullable `users.default_harness`; there is no per-harness model storage.
- `api/internal/store/queries/users.sql` reads and writes `default_model` independently of `default_harness`. `api/internal/handler/user_settings.go` already decodes and validates every present field before any write, but the individual setting updates are separate statements.
- `web/src/pages/RunDefaults.tsx` has separate adjacent cards. Saving a harness calls `modelCompatibleWithHarness` and clears the shared model when incompatible. The model picker follows the effective harness, including the Codex-only case fixed during PR #1449 review.
- `web/src/components/ModelSelect.tsx` owns the curated lists. Claude allows **Other**; Codex is deliberately closed to `gpt-6-astra` and `gpt-5.6-sol`.
- `api/internal/workersvc/harness_create.go` repeats that closed Codex allowlist. `claim_assembly.go` loads the single user default, lets a frozen schedule model override it, then drops a cross-harness value with a visible status note.
- `agent/src/codex/render.ts` repeats the two-model contract and falls back to `gpt-6-astra` for an unknown model. `agent/src/codex/codex-pricing.ts` already has an `unreported` path for models without a price row, so a custom model does not need guessed pricing.
- The claim wire already carries one effective `default_model`. Separate storage needs no worker-protocol field: the API resolves the run harness and sends only that lane's effective model.
- The chart runs the API as one replica with `strategy: Recreate`. Schema migration and API replacement therefore do not overlap old and new API pods, but stale browser assets can still call the new API. The legacy settings field remains for that bounded client-skew case.
- Hosted worker images roll independently of the API/web release and can remain busy on an older image. `codex_harness_v1` proves general Codex support only; it does not prove custom root-model passthrough. An explicit new capability is required before a custom model can be claimed.
- `api/cmd/uzi/` has no user-default settings command. PRD #1429 deliberately added per-run and per-schedule `--harness` without a user-default setter. This PRD does not add one.
- `docs/worker-model.md` documents one Worker model and the Claude custom-ID flow. Its screenshot and precedence text must change when this ships. `specs/human.md` likewise describes one per-user default model.

## Decisions

### D1: one task-oriented card, one save

Default harness and per-harness models are one user decision and share one card. The web sends all three values together. The API persists them atomically in one SQL statement, so a database error cannot leave the harness changed while one model lane remains stale.

### D2: expand to two explicit lanes; keep the legacy column through the compatibility window

Add nullable `users.default_claude_model` and `users.default_codex_model`. Renaming or dropping `users.default_model` would violate the repository's additive rolling-compatibility discipline and would make an immediate rollback lose the migrated value. Public JSON gains the two explicit names; `default_model` stays as a deprecated compatibility projection for one release and is not removed by this PRD.

The additive migration copies `gpt-6-astra` and `gpt-5.6-sol` from the legacy slot into `default_codex_model`; every other non-null legacy value copies into `default_claude_model`. It does not clear or reinterpret `default_model`. This is conservative: #1429 allowed only those two Codex values to reach a run, so an unknown legacy value was never an effective Codex default and must not become newly executable merely because the migration ran.

The Down migration first projects the currently selected lane back into `default_model`, then drops only the two new columns. Two independent values cannot collapse losslessly into one on rollback, so the inactive lane may be lost after a post-upgrade edit; the active preference is retained and the limitation is stated in the migration comment and release notes. The Up path itself discards no value.

### D3: legacy API writes are bridged, not guessed from model spelling

New clients use the two explicit fields. For the bounded stale-client window, a request carrying only legacy `default_model` keeps the old effective-harness meaning: the handler routes it through the same server-resolved default harness used for run creation, using a simultaneously supplied non-null `default_harness` value when present. A response carries the explicit lanes plus a legacy projection for that effective harness. No new code infers provider ownership from arbitrary prefixes such as `gpt-` or `claude-`.

Credential availability never makes settings fail. If D11 cannot resolve a usable harness, or a request explicitly clears `default_harness`, the legacy projection/write target is Claude. If both explicit fields and the legacy field are present, the handler first determines that target and requires the legacy value to equal the corresponding explicit lane; a conflict returns 400 and writes nothing. The compatibility behavior, null/default cases and deprecation are contract-tested. Removal, if ever wanted, is a later contract migration.

### D4: lane provenance replaces root-model string classification

After the run harness is frozen, claim/chat assembly reads the matching lane. The resulting wire field remains `default_model`. A user Codex default can therefore be a validated custom ID without teaching the API to recognize every future OpenAI model name.

Precedence remains:

1. a frozen per-run schedule model, when present and compatible with the run's harness;
2. the run owner's model default for the frozen harness;
3. the existing harness fallback, including `gpt-6-astra` for Codex and the lead-template/default behavior for Claude.

The visible `harness_model_fallback` note remains for ambiguous legacy or schedule pins. A lane-scoped user default does not need that fallback classification.

### D5: custom Codex is narrow and fail-loud

The custom escape hatch applies to the per-user Codex worker default in this PRD. It does not silently widen schedule model pins, agent-template role pins, judge/summary models, custom providers, base URLs or local models.

The API uses the existing model-field validator. The Codex renderer treats a server-selected worker root model as an opaque validated ID and passes it to the fixed OpenAI provider. The effort contract remains the independent closed `low|medium|high|xhigh|max` set for curated and custom models alike; the provider accepts or rejects the pair. Uzi never silently substitutes Astra. API-key usage for an unknown price row remains token-complete with `cost_status=unreported`; subscription usage remains token-complete with `cost_status=subscription` regardless of a price row.

The shared renderer must receive trusted model provenance rather than relax one global function. Ordinary run roots and task-review advice that consume the worker default may accept the custom lane. Judge and summary advice remain on their own curated model contracts, and per-role Codex pins remain curated and fall back to the resolved run root when incompatible. Tests prove each source separately. This prevents a customized Claude template, judge model or summary model from becoming an arbitrary Codex model merely because worker-root custom IDs are allowed.

### D6: custom-model support is capability-gated during worker rollout

The agent advertises a new `codex_custom_model_v1` protocol capability only after its renderer can pass a custom worker-root model without substitution. A Codex run or task review whose effective worker model is outside the curated pair requires that capability in every non-bypassable placement seam: claim selection, eligible-worker/peer-spread counts and the queued reason. An old `codex_harness_v1` worker cannot claim it.

The capability predicate follows D4 precedence at claim time: a frozen run/schedule model first, otherwise the owner's current Codex lane. This preserves today's behavior in which a queued ordinary run observes the user default when it is claimed. Curated-model runs still require only `codex_harness_v1` and are not held behind the fleet rollout.

### D7: reasoning effort remains shared

This PRD separates model defaults only. The existing per-user reasoning-effort value remains shared across harnesses, with the current per-model downgrade behavior. A separate effort per harness would be a different product decision.

## Out of scope

- Separate per-harness reasoning-effort defaults.
- Per-harness judge or summary models; #1454 owns the judge-model design gap.
- Custom Codex IDs for schedule pins or per-role agent-template pins.
- Provider discovery, model-catalog synchronization or save-time provider validation.
- OpenAI-compatible custom endpoints, local models or user-controlled provider configuration.
- A CLI command for user-level defaults. Existing run/schedule `--harness` behavior is unchanged.
- Changes to credential defaults, explicit run/schedule harness overrides, pricing tables or the default Codex fallback.
- Any `.github/workflows/**` change.

## Milestones

### Phase 1: parallel foundations

#### M1: additive persistence and atomic settings contract

- Add nullable `users.default_claude_model` and `users.default_codex_model` in a draft-numbered migration. Backfill only the two previously effective Codex IDs into Codex, copy every other non-null legacy value into Claude, and leave `default_model` untouched. Down projects the active lane into the legacy slot before removing the new columns and documents its unavoidable inactive-lane loss.
- Extend sqlc settings reads and the public DTO/contract fixtures with explicit Claude and Codex fields while retaining the deprecated legacy field.
- Add one PATCH-like SQL update for `default_harness`, both explicit lanes and the legacy projection, with present/null/value semantics for each. When any grouped field is supplied, one statement applies the entire group and returns the saved values.
- Implement D3's legacy request/response bridge without string-prefix guessing and without making credential availability a prerequisite for settings. Validate the complete request before the atomic write.
- Add handler, query and live-DB tests for set, clear, omitted-field retention, zero-credential fallback, explicit `default_harness:null`, conflicting legacy/explicit input, migration classification, immediate rollback projection and an injected write failure that leaves all stored values unchanged.
- Regenerate sqlc. Gates: `task gate:api`, `task gate:repo`, `./e2e/run-store-it.sh`.

#### M2: custom Codex root-model transport

- Split the agent's curated picker from its root-model transport rule. A validated server-selected worker root ID passes through unchanged instead of falling back to Astra; effort remains the independent closed uzi set.
- Add trusted provenance to advice rendering: task review may consume the custom worker default, while judge and summary advice stay curated. Keep per-role model pins curated and preserve their existing fallback to the resolved run root.
- Advertise `codex_custom_model_v1` only from a worker with the new passthrough behavior.
- Preserve the fixed provider endpoint and TOML escaping. Model text never selects a provider, URL, environment variable or config key.
- Preserve honest accounting: known API-key price rows meter normally; unknown API-key models are unreported; subscription remains subscription; all retain token totals.
- Add discriminating agent tests for a custom root ID reaching start/resume/task review, custom judge/summary and unknown role pins staying closed, malformed input never reaching config, provider rejection remaining visible, both cost-status modes, and capability advertisement.
- Gate: `task gate:agent` and the existing Codex-focused test targets reached by it.

#### M3: grouped Run Defaults experience

- Replace the adjacent Default harness and Worker model cards with the approved [grouped mock](mockups/1551-per-harness-model-defaults.html): harness first, one lane per usable harness, active-lane badge, retention explanation and one **Save defaults** action.
- Keep the harness selector hidden for a one-harness user. Show only the usable lane while retaining any unavailable lane server-side. With zero usable harnesses, preserve today's pre-credential setup by showing the Claude lane and no harness selector.
- Maintain independent dirty/saved state for both lanes. Changing the harness never clears a model. A successful save updates all three snapshots; failure leaves the form dirty and reports one actionable error.
- Extend `ModelSelect` so the Codex default lane allows **Other** while schedule/template Codex callers remain closed. Preserve Claude custom behavior and give each custom input a provider-specific placeholder and accessible label.
- Add component tests for both-harness, Claude-only, Codex-only and zero-credential states; harness switching without model loss; custom IDs and provider-specific copy in each lane; one atomic payload; save failure; keyboard labels/focus; and the dynamic status copy. Avoid vacuous retired-copy assertions.
- Gate: `task gate:web` and `cd web && npm run build`.

### Phase 2: sequential integration and acceptance

#### M4: harness-first runtime selection and compatibility acceptance

- Update every dynamic user-default consumer to read the lane matching the already-frozen run harness. Chat remains the literal-Claude exception and always reads the Claude lane; this PRD does not enable Codex Chat. Do not alter the claim wire.
- Preserve schedule-model precedence and the fallback note for incompatible frozen schedule or legacy values. Preserve byte-compatible Claude behavior when the Claude lane is unchanged.
- Require `codex_custom_model_v1` for a custom effective Codex root in claim, eligible-worker, peer-spread and queued-reason paths. Prove an old `codex_harness_v1` worker cannot claim and the run does not silently start on Astra; curated runs remain eligible there.
- Add API/live-DB tests proving: both lane values coexist; a Claude run and literal-Claude Chat receive only Claude; a Codex run receives only Codex; changing the default harness changes the selected lane but mutates neither value; explicit schedule models still win; clearing one lane does not affect the other; a custom root requires the new capability; and a capable worker receives it unchanged.
- Add a stale-client contract test for D3 and a regression that the old shared-slot/reset behavior fails against the new implementation.
- Run the joined gates once on the final tree: `task gate:api`, `task gate:agent`, `task gate:web`, `task gate:repo`, and `./e2e/run-store-it.sh`.

#### M5: documentation, specification and decision closure

- Update `docs/worker-model.md` for the grouped card, per-harness precedence, custom Codex behavior, first-run failure semantics, the narrow root-only scope, and honest API-key/subscription cost status. Replace `docs/img/worker-model-settings.png` with the implemented UI.
- Update `docs/worker-effort.md` to describe the still-shared effort contract for Claude and Codex and the provider-rejection behavior for an unsupported custom model/effort pair.
- Update `specs/human.md` tersely: separate retained model defaults, grouped save, and custom IDs for both harnesses, tagged `(AI-synced 2026-09-22)`.
- Mark #1106 D9 and #1429 D6 as superseded by #1551 for per-user defaults only; their closed schedule/role-pin decisions remain in force. Add the same narrow supersession note to ADR-1106's model decision rather than creating a competing ADR.
- Run `task docs:sync`, commit the embedded mirror, then run `task check-docs:web` and `task gate:api`.
- Record implementation decisions below and move this PRD to `prds/done/` only after all acceptance criteria pass.

### Phase 3: cross-repo and hosted-runtime acceptance

#### M6: hosted-k8s rollout and live acceptance (maintainer)

- Publish/release the worker image containing `codex_custom_model_v1` and roll the hosted dev fleet through its normal operator deployment. No GitOps file change is prescribed by this PRD.
- From the implemented grouped card, save distinct Claude and Codex defaults and verify a reload preserves both.
- Run one Claude control and one Codex run on hosted k8s and confirm each receives its own lane. Then set a deliberately unsupported but syntactically valid custom Codex ID and prove the failure names provider/model availability; it must not run Astra and must not be claimed by any still-old worker.
- Restore a supported Codex model after the negative control and prove the next hosted run reaches its normal gate. Record nonsecret run IDs, worker versions/capabilities and outcomes in this PRD's progress log.
- Remove only test-owned issue/run artifacts. Do not change another repository merely to satisfy this milestone.

The feature implementation is single-repo. Phase 3 is operational validation against the primary hosted runtime; it requires the normal deployment to consume the published uzi image but no separately specified cross-repo implementation.

## Dependency graph and parallelism

| Phase | Milestone | Depends on | Primary files | Repo |
|---|---|---|---|---|
| 1, parallel | M1: persistence/API contract | PRD decisions | `api/internal/store/**`, `api/internal/handler/user_settings*`, `api/internal/apitypes/**`, `fixtures/api-contract/**` | uzi |
| 1, parallel | M2: custom Codex root transport | PRD decisions; wire stays unchanged | `agent/src/codex/**`, `agent/test/codex-*` | uzi |
| 1, parallel | M3: grouped web UX | PRD field contract; can use mocks before M1 lands | `web/src/pages/RunDefaults*`, `web/src/components/ModelSelect*`, `web/src/lib/apiTypes.ts` | uzi |
| 2, sequential | M4: joined runtime/capability/compatibility acceptance | M1, M2, M3 | `api/internal/workersvc/**`, `api/internal/store/queries/runtime.sql`, `agent/**`, `web/**`, focused live-DB tests | uzi |
| 2, sequential | M5: docs/spec closure | M4 accepted UI and behavior | `docs/**`, `api/internal/uzidocs/embed/**`, `specs/human.md`, `prds/**`, `adr/1106-codex-harness.md` | uzi |
| 3, cross-repo/runtime | M6: hosted-k8s acceptance | M4-M5 merged, worker image published and fleet rolled | deployment evidence only; no prescribed repo edit | operator deployment |

M1 to M3 touch separate primary files and can run concurrently. M3 may code against the frozen DTO names in this PRD, but its final gate waits for M1. M4 is the join point and owns cross-surface corrections. M5 follows the accepted implementation so screenshots and user documentation cannot drift from the shipped form. M6 is maintainer-owned because a uzi worker cannot publish and roll its own runtime image or validate the hosted cluster.

## Success criteria

1. A user with both harnesses can save a default harness, a Claude model and a Codex model from one card and one request; reloading preserves all three.
2. Switching the default harness changes neither saved model. Future implicit runs use the selected harness's lane; explicit run/schedule harness selection still wins.
3. Claude-only and Codex-only users see one relevant model lane and no redundant harness selector. A temporarily unavailable lane remains stored.
4. A zero-credential user can still preconfigure the Claude lane, and ordinary settings GET/PUT never fails because D11 has no usable harness.
5. Both lanes offer curated choices and **Other**. A syntactically valid custom Codex worker-root ID reaches the fixed OpenAI provider unchanged on a capable worker; provider rejection is visible and no fallback model is silently used.
6. An old worker without `codex_custom_model_v1` cannot claim a custom-model run. It remains queued with a specific capability reason rather than silently running Astra.
7. Unknown API-key model pricing reports unreported; subscription reports subscription; both retain token totals and neither fabricates dollars.
8. Schedule-model precedence, per-role pins, judge/summary models, shared reasoning effort, literal-Claude Chat and the claim wire retain their pre-#1551 behavior.
9. Existing data migrates conservatively into explicit lanes while the legacy value stays intact. Immediate rollback restores the active lane; its documented inactive-lane limitation is accepted.
10. The grouped update is atomic, all new API fields are contract-tested, focused live-DB tests execute with zero skips, all named gates pass, and M6 passes on hosted k8s.

## Risks

| Risk | Mitigation |
|---|---|
| A migration assigns an ambiguous legacy custom ID to the wrong provider | Copy only the two IDs that were previously effective on Codex; copy every unknown into Claude and leave the legacy slot intact |
| An immediate rollback loses the active preference | Down projects the active lane into the legacy slot before dropping new columns; inactive-lane loss after a post-upgrade edit is explicit and unavoidable |
| Opening custom Codex accidentally opens role pins or schedules | D5 scopes the escape hatch to the per-user root default; separate tests pin closed callers |
| An unknown model gets silently replaced by Astra | The root renderer passes validated custom IDs unchanged and treats provider rejection as the run error |
| An old worker silently replaces a custom model with Astra | New capability gates every claim/eligibility seam and the hosted rollout has a negative control |
| An unknown model produces misleading cost | API-key usage is unreported, subscription stays subscription, token totals remain, and no dollar estimate is invented |
| Grouped UI looks atomic while backend writes partially | One SQL statement updates harness and both lanes; a failure regression proves no partial state |
| Stale web assets write the legacy field during deploy | Keep and contract-test the bounded D3 bridge for one release; do not drop the field here |
| The refactor changes schedules, judge models or Claude role pins | Those surfaces are explicitly out of scope and get preservation tests at the M4 join |

## Decision Log

| Date | Decision | Rationale |
|---|---|---|
| 2026-09-22 | Group default harness and both worker-model defaults in one card | They form one run-default decision and should save coherently |
| 2026-09-22 | Retain independent Claude and Codex values when the default harness changes | Harness switching is selection, not destructive preference replacement |
| 2026-09-22 | Add **Other** to the Codex user-default lane | Curated lists age quickly; provider availability should fail visibly on first use rather than block storage |
| 2026-09-22 | Add two explicit lane columns and retain the legacy column for one compatibility release | It is additive, keeps upgrade/rollback data, and follows the established appearance-settings expansion pattern |
| 2026-09-22 | Keep custom Codex narrow to the root user default | Schedule and role-pin values lack equivalent provider provenance and need a separate design |
| 2026-09-22 | Require `codex_custom_model_v1` for custom worker-root execution | A mixed hosted fleet must never silently substitute Astra on an old renderer |

## Review record

- 2026-09-22: Two independent reviews covered architecture/data/API/runtime compatibility and product/UX/testability. Their findings were folded in before handoff: rollback-safe explicit lane storage, the zero-credential state, stale-client routing, custom-model worker capability gating, advice-source provenance, accurate subscription/API-key cost semantics, literal-Claude Chat preservation, provider-specific custom-field copy, `docs/worker-effort.md`, and hosted-k8s acceptance.
