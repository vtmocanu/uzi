# PRD #2151: Cross-checker model and effort pins per stage and model family

**Status**: Draft. Child 5 of 6 under umbrella #2148 (Cross-check). Blocked by PRD #2149. Naming follows PRD #2149 (D13, D14).

Resolved facts below were read at `main` `8a5f138e`; the per-stage grid was decided at `main` `8c35d39b`.

## Problem

PRD #2149 cross-checks an auto-approved run's plan on the other model family, using whatever model and effort the cross-checker's claim resolves from the owner's worker lanes. A user who wants a different cross-checker than their worker default (for example `gpt-6-astra` at `xhigh` for Codex cross-checks while their Codex workers stay on their default) cannot say so without changing every run they start on that family. The two stages also cost differently: a plan cross-check is short and high-leverage, a code cross-check (PRD #2170) reads a whole diff, so one setting for both would force a bad trade-off.

## Outcome

On Settings → Run defaults, in the Cross-check section, the user sees a grid: one row per stage, one column per family ("Claude cross-checker", "Codex cross-checker"), each cell a model and a reasoning effort. This PRD ships the **Plan cross-check** row; PRD #2170 adds the **Code cross-check** row. Each field shows `Default · <value> (<source>)` and follows the user's worker default for that family until the user picks a value, which pins it. A pin is hard: a cross-check either runs on exactly the pinned model and effort or fails visibly. The values a cross-check actually ran with are recorded on it and shown with its findings. On main only the Codex cross-checker exists (a Claude lead checked on Codex); a Claude cross-checker arrives with PRD #2460 (Codex-lead plan cross-check). Until then the Claude column is stored and editable but marked inactive ("Used once Codex-lead runs are cross-checked"), and nothing delivers it.

Acceptance examples:

1. Plan cross-check × Codex is pinned to `gpt-6-astra` / `xhigh`; the Codex worker default is `gpt-6-sol` / `high`. A Claude-lead autopilot run's plan is cross-checked by a Codex child on `gpt-6-astra` at `xhigh`; the cross-check record and run page show both. A Codex-lead run's own implementation still runs on `gpt-6-sol` / `high`.
2. Plan cross-check × Claude is pinned to `sonnet`. The pin is saved and shown, marked inactive; no run is affected, because no Claude cross-checker exists until PRD #2460. That PRD's acceptance covers live Claude-checker delivery (a pin wins; Default follows the Claude worker default).
3. Plan cross-check × Codex is pinned to a custom model and no online worker advertises `codex_custom_model_v1`. The child stays queued (the placement gate keeps other workers from claiming it) until the deadline, and the run parks with `plan cross-check: timed out`. Separately, if claim assembly cannot honour a pin it is handed (a custom model the user's Codex account can no longer use, or a stored effort the harness no longer accepts), the child fails with `plan cross-check: checker unavailable` and the run parks. Neither path ever runs the default model or a clamped effort.

## Out of scope

- The Code cross-check row: PRD #2170 widens the stage domain and adds its row, delivery and tests.
- The epic #1703 Settings → Models grid. When that tab ships (child I) these rows move into it as "Plan cross-check" and "Code cross-check" rows; epic #1703 is updated to list them.
- A per-run override (epic #1703 locked "no per-run model or effort override").
- The `uzi handoff --review` model (#1570): same shape, separate setting.
- Admin instance defaults for the cross-checker.
- Inheritance between stages: each cell falls back to the worker default for its family, never to another stage's cell (D6).

## Modules and seams

- **Storage**: a table `user_cross_check_pins` (`user_id`, `stage`, `harness`, `model` nullable, `effort` nullable, primary key `(user_id, stage, harness)`, `CHECK (stage IN ('plan'))` here, `harness` in the two families). A missing row or a null field is Default. A table rather than four `users` columns, because PRD #2170 adds a second stage without new columns.
- **DTO**: `cross_check_pins` on `/api/me/settings` (`api/internal/handler/user_settings.go`), a list of cells keyed by `(stage, harness)`: GET readable with a CLI token, PUT cookie-only, as today. Update semantics follow the existing PATCH-like rule in `PutMySettings` (`api/internal/handler/user_settings.go`): an omitted cell, or an omitted field inside a submitted cell, is unchanged; an explicit `null` resets that field to Default; a duplicate `(stage, harness)` cell, an unknown stage or an unknown harness is a 400. The handler validates every submitted cell first and then writes them in one transaction, so a bad cell writes nothing. GET returns, per cell, the stored pin (`model` / `effort`, null when Default) separately from the resolved value and its source (`resolved_model`, `resolved_effort`, `model_source`, `effort_source` in `pin | worker default`), so a client never mistakes a resolved default for a pin.
- **Write validation**: model ids pass `agenttmpl.ValidateModel` (`api/internal/agenttmpl/model.go`, rejects control and format characters) and are valid for their family: Claude and curated Codex ids by `harnessModelCompatible` (`api/internal/workersvc/harness_create.go`); custom Codex ids by the same custom-lane validation the Codex worker-model lane uses (PRD #1551), since `harnessModelCompatible` rejects custom Codex ids. An effort must be in that harness's effort domain. Invalid input is a 400 naming the stage, harness and field.
- **Delivery, a new branch in claim assembly for `kind = 'cross_check'`**: the pin for the child's stage and harness is read from the owner's row at claim time and delivered on the existing `default_model` and effort claim fields. The frozen `runs.model` path is not used: a frozen custom Codex model is never forwarded (`harness_create.go`) and claim assembly falls back to the lane with a note (`claim_assembly.go`), and effort has no per-run freeze (`claim_effort.go`). The branch has no fallback: a pin the claim cannot honour fails the claim assembly for that child with a typed reason that ends the child as `failed`, reason `checker unavailable`, and PRD #2149 parks the lead. Default takes PRD #2149's lane resolution unchanged.
- **Post-claim re-check.** ClaimRun commits before claim assembly runs, so a pin can change between the placement gate and assembly (main already handles this race for ordinary Codex runs: the PRD #1551 post-claim custom-Codex re-check in `api/internal/workersvc/claim_assembly.go`). Assembly re-checks the *resolved* pin against the claiming worker's advertised capabilities before delivering any credential, and distinguishes two cases: a worker-capability mismatch (for example the pin became a custom Codex id and this worker lacks `codex_custom_model_v1`) returns the requeue error so an eligible worker claims the child; an intrinsically unusable pin (no longer valid for the user, effort out of domain) fails the child with `checker unavailable`.
- **Claude column**: stored, validated and rendered like the Codex column; GET reports it with `active: false` until a worker path can deliver it. The claim-assembly branch in this PRD handles only the Codex checker (the only `cross_check` harness `runtime.sql` creates today); PRD #2460 adds the Claude branch, reading the same row.
- **Placement**: a `cross_check` child with a custom Codex pin is claimable only by a worker advertising `codex_custom_model_v1`, mirrored in ClaimRun, `CountOnlineWorkersClaimableForRun`, the ephemeral provisioning queries and the cross-check lane claim (PRD #2169, if shipped), following the custom-root clause in `runtime.sql`.
- **Record**: the `cross_checks` row stores the model and effort the claim delivered (PRD #2149) and, here, their source (`pin` or `worker default`).
- **Web**: the Cross-check section on `web/src/pages/RunDefaults.tsx` gains the grid with the Plan cross-check row: model and effort selects per family with epic #1703's `Default · value (source)` wording and the helper text "Checks leads running on the other model family"; mock-mode fixture.
- **CLI**: the account settings view shows each cell and its sources. No CLI model or effort setting has a write path yet; epic #1703 requires CLI get, set and reset for every model and effort setting through its child O, so these fields are not an exception: child O covers them like the other model fields. Check `api/cmd/uzi/` and `docs/cli.md`.

## Testing decisions

- Resolution: a pin wins; Default follows a later change of the worker default; the delivered values equal the recorded ones; a pin on one stage never affects another stage's resolution (asserted once PRD #2170 adds the second stage; here, a test pins that resolution is keyed by stage).
- Hard pins: an unsupported custom Codex model, a model incompatible with the harness, and an out-of-domain effort each fail the child with `checker unavailable` and never run the default or a clamped value. Regression tests that fail if the frozen-model fallback or effort clamping is reused.
- Dormant Claude column: a stored Claude pin is returned with `active: false`, rendered as inactive, and never changes any claim's delivered model or effort.
- Placement: a custom-pinned child is never claimed by a worker without `codex_custom_model_v1`; mirrors agree.
- Post-claim race: a pin changed from a curated to a custom Codex id between claim and assembly requeues the child on a worker without `codex_custom_model_v1` and delivers nothing; an intrinsically unusable pin fails it. A mutation that removes the re-check reddens the case.
- Validation: each field's 400 cases; unknown stage or harness refused; duplicate cells refused; omitted cell or field unchanged, explicit null resets to Default; a submission with one bad cell writes nothing; GET separates stored pins from resolved values; curated and custom ids accepted per family; control characters refused.
- Web and CLI: rendering of `Default · value (source)`, atomic save, CLI view.

## Milestones

- [ ] **M1: A user pins the plan cross-checker's model and effort per family, and plan cross-checks run on exactly those or fail visibly.** Migration, DTO and validation, the `cross_check` claim-assembly branch keyed by stage, placement clause and mirrors, recording with source, the Run defaults grid with its Plan cross-check row, CLI view, `docs/cross-check.md` and `docs/cli.md` then `task docs:sync`, `specs/human.md`, CHANGELOG. Blocked by: PRD #2149. Gates: `task gate:api`, `task gate:web`, `task gate:agent`, LiveDB via `./e2e/run-store-it.sh`, `task gate:repo`.

No `.github/workflows/**` change in implementation or validation.

## Decision Log

- **D1. A Claude/Codex pair per stage; Default follows the worker default.** Epic #1703's rule for every model setting, and the user's request: keep the worker default or override it.
- **D2. Pins are hard, for model and effort alike.** Epic #1703's judge decision: an explicit choice that cannot be honoured is a visible failure, never a substitution.
- **D3. Delivered through a dedicated claim-assembly branch, not the frozen `runs.model` path.** That path drops custom Codex ids with a fallback and cannot carry effort.
- **D4. No per-run override.** Locked by epic #1703; these are mostly unattended runs.
- **D5. Separate PRD.** A cross-check is useful on worker defaults; user decision 2026-10-03.
- **D6. Per-stage grid, no inheritance between stages.** User decision 2026-10-03: the plan and code stages differ in length and leverage, so each gets its own model and effort. A cell falls back only to its family's worker default; chaining code → plan → worker default would make "where did this model come from?" hard to answer. Rows per stage and columns per family match epic #1703's grid shape.
- **D7. Claude-checker pins are stored but inactive until PRD #2460.** Buddy review 2026-10-07 at dispatch: main cannot create, assemble or execute a Claude cross-checker (`cross_check.go` requires a Claude lead, `runtime.sql` creates the child as `codex`, `cross_check_claim.go` requires Codex credentials). Storing both columns keeps this PRD independent; live Claude delivery and its acceptance move to PRD #2460.
