# PRD #4574: Codex providers on judge and cross-check

**Issue**: [#4574](https://github.com/vtmocanu/uzi/issues/4574)
**Priority**: High
**Umbrella**: [#4319](https://github.com/vtmocanu/uzi/issues/4319)
**Blocked by**: [#4323](https://github.com/vtmocanu/uzi/issues/4323) (landed in #4638), and merged [#4322](https://github.com/vtmocanu/uzi/issues/4322) M1a before #4574 M1. The remaining #4322 slices are not prerequisites.
**Status**: M1 landed in #4711; M2 (provider plan and code checkers) is the remaining work; no on-deck enrollment.
**Disposition**: M1 had the owner's go (2026-11-25). M2 has the owner's dispatch go after its pre-dispatch fold (2026-11-27). One slice per gated run; no on-deck sweep enrollment.

## Problem and outcome

After #4323, a Codex provider source serves the issue/task lane. Before M1 a Codex judge for that owner skipped with `provider_lane_unsupported`; M1 (#4711) delivered the provider judge. Any plan or code cross-check where the lead or checker is Codex-provider-bound still refuses (plan: human gate; code: `checker_unavailable`); M2 lifts the rows assigned to #4574 in its refusal table. Deliver both lanes on the Codex provider source, mirroring #4322's per-lane slices for Claude, while preserving each lane's independent credential authority and lifecycle.

Examples:

1. An owner with `codex_provider_id` set and a Codex judge gets a provider-frozen judge at creation; a bad selected source skips explicitly, never spending a vendor credential.
2. A Claude lead's Codex plan or code checker freezes the owner's Codex source and uses only it. A Codex provider lead's Claude checker uses its own Claude authority (#4322).

## Out of scope

Jobs, chat and the isolated lane on Codex; admin-shared providers (#4324); new public gate reasons; any change to vendor/vendor pairs. No implementation or validation edits under `.github/workflows/`.

## Shared contract

Same invariants as #4323 and #4322: exclusive frozen binding on the five `runs.model_provider_*` columns, `enablement_rev` fence, provider-only claim (no `secrets.codex` block or vendor credential-operation capability), no vendor fallback, #4505 key-forms redaction on every lane output and the fail-closed upload scan, accepted redirect risk (owner 2026-11-19, extended to judge, job, cross-check and isolated lanes on 2026-11-20 by #4322 D5, which covers both engines; see D4) unchanged. Lane settlement follows #4322's matrix (typed terminal failure for judge child revocation; a checker's provider refusal decides the check `checker_unavailable` lead-first, see M2; no generic provider parking of repo-less evaluators).

Per-lane capabilities, alongside `codex_provider_v1`: `codex_provider_judge_v1` (M1) and `codex_provider_checker_v1` (M2, both stages). Each is advertised only once its consumer is complete, and is required in ClaimRun, the `claim_assembly.go` backstop, `fn_placement_admitting_workers` (additive replacement migration, draft number), health, demand and reap predicates. Old workers receive no lane claim; an unused capability is harmless. Each slice's placement and claim predicate change is serial with #4322's: Up extends the live definition at landing and preserves every merged Claude lane predicate, and Down restores that exact immediate predecessor.

## Slices

### M1: Codex provider judge

Reuse merged #4322 M1a judge authority, settlement and capability seams, made engine-aware. M1 is serial with #4322 M1b: both replace `fn_placement_admitting_workers` and the ClaimRun predicates, so whichever lands second rebases its Up on the other's body.

**Lane decision (creation).** `judge_enqueue.go` and `judge_read.go` already stamp `bindingOf(resolved)` through `createRunResolved` with `JudgeLane`; they need only comment edits. The change is in `api/internal/workersvc/harness_resolver.go`: the Codex arm of `decideJudgeLane`, the `providerJudge` branch and its `resolvedHarness` literal (today hardcoded to `HarnessClaude` / `ClaudeSourceSet`), `freezeProviderJudge` (hardcoded `HarnessClaude`) and `judgePolicyDrift` (for a Codex judge, compare `CodexProviderID` only, so an Anthropic pin change never refuses a Codex provider judge). The decision and freeze stay under the shared secret-mutation lock the resolver already takes; never read the source outside it. When `judgeHarnessFor` selects Codex and the owner's Codex source is set, freeze that provider/key/revision at creation; a null source keeps the existing Codex vendor default/account-mode resolver. Do not invent a Codex judge pin/auto setting or apply `judge_anthropic_bind_mode`. Later source edits affect new judge rows only.

**Judge model rule (D3).** A Codex provider judge resolves its model like the shipped Codex advice resolver (`resolveCodexProviderModelSlot` with explicit=false): the model's own map key when present, otherwise the map's required `default`, including when the judge model comes from user or instance settings. Explicit run and schedule pin rules in `docs/providers.md` are unchanged. `resolveCodexProviderModelSlot` stays the only implementation: the claim carries the Codex model id and the worker maps it. On the server, the Codex arm of the judge-model check (`checkProviderJudgeModel` in `judge_model.go`, and the claim-time resolution in `api/internal/workersvc/judge.go`) is the strict model read plus the existing map validation at freeze (`EvaluateProviderSource`) and at claim (`assembleProviderBinding`); with a valid map, key-or-`default` always resolves. Keep the Claude `modelMapped` arm for Claude. An invalid map or a missing `default` fails closed; a Codex provider judge never takes the `provider_model_unmapped` skip.

**Claim.** In `api/internal/workersvc/judge.go`, lift the Codex refusal, apply the D3 Codex model check, and skip `codexClaimSecrets` for a provider judge (otherwise the server mints a vendor Codex capability and `assertClaimSecretsExclusive` fails every claim). Update `claim_assembly.go`'s backstop and `workerSupportsProvider` (`provider_claim.go`).

**Capability census.** Discover every site with `git grep -n -E 'claude_provider_judge_v1|ClaudeProviderJudgeV1|CLAUDE_PROVIDER_JUDGE_CAPABILITY' -- api agent ':!*_test.go' ':!*.test.ts' ':!*.sql.go' ':!api/internal/store/migrations' ':!api/internal/uzidocs/embed'`; each hit gets a Codex counterpart. Acceptance names, at minimum: ClaimRun, the ClaimRun fleet-spread peer mirror, `CountOnlineWorkersClaimableForRun`, `fn_placement_admitting_workers` (additive replacement migration; its predecessor is `00534_placement_provider_judge_capability.sql`, number is a draft), stale-wake detection (`hibernation.sql` and the Go wake checks), health (the hardcoded reason string and its Claude-only branch in `health.go`), demand, reap, and on the agent `requireProviderCapability` in `client.ts` and the advertisement in `worker.ts`.

**Worker routing and settlement.** Today `judge-runner.ts` picks the vendor path with `selectCodexBinding`, so a claim carrying only `secrets.provider` reaches `judgeProvider`, which refuses any non-Claude binding (`provider_lane_unsupported`). Dispatch through `selectCodexExecutionAuthority` (`agent/src/codex/select.ts`) and add a Codex provider judge path that passes the authority wrapper as `codex.binding` and that wrapper's frozen `providerBinding` as `provider` (`model-pass.ts` checks `provider === codex.binding.providerBinding`). Reuse the key registration and trace scrubbing `execute()` already does for either engine; keep `judgeProvider`'s Claude guard. `runCodexAdviceModelPass` and the advice harness turn every error into a plain `Error`, and `judgeCodex` turns any failure into a fallback review, so add a judge-only Codex seam, mirroring the Claude judge-only `onResult` seam in `model-pass.ts`, that carries the typed terminal classification (closed classification, category and optional transport `httpStatus` from `codex/terminal-normalize.ts`; no parsing of rendered diagnostics) into `execute()`'s existing `ProviderJudgeTerminalError` / `LimitReachedError` arms. Codex terminals carry a classification, not a status: a number exists only on the tagged transport variants. Settlement by classification: a gateway limit, decided by the run lane's predicate in `codex-harness.ts` (`rateLimitExceeded`, `serverOverloaded`, or a transport variant with 429/529) extracted into one shared pure function (keeping its failed-outcome and provider-auth guards), never copied, gives `LimitReachedError(provider_gateway_limit)` with no review and no vendor cooldown; any other classified error terminal (for example `unauthorized`, `internalServerError`, `badRequest`, a policy refusal, a transport variant with a non-limit status), a prompt-build failure or unparseable output keep the scrubbed deterministic fallback review; an error terminal classified `other`/unknown or with no classification, a transport variant with no `httpStatus` (for example an unreachable gateway; parity with the Claude provider judge, where a status-less error is terminal), no terminal at all, and every thrown or timed-out pass give `ProviderJudgeTerminalError` (failed, no review, never `completed`). Shared advice behavior for other callers does not change. Also fix the Claude-only `HarnessClaude` in `recordHarnessModelFallbackNote` and the "Claude judge" comment in `api/internal/workersvc/judge.go`.

**Copy and docs.** After M1, Codex `provider_lane_unsupported` judge skips are historical. Render by the skip row's harness as M1a did (Codex copy now past tense) in `JudgePanel.tsx`, the web mocks, `review_render.go` and its CLI test, and sweep `docs/judge.md`, `docs/configuration.md` (the capability and health-reason text), `ARCHITECTURE.md` (both the provider judge capability text and the unsupported note), `docs/providers.md`, `specs/human.md`, ADR-4320 and the code comments that call the Codex judge unsupported (`git grep -n -F '#4574'` plus the "not supported yet" copy). State D3 for users: `docs/judge.md`'s "The model must be mapped" bullet gets its Codex rule, and the Codex "a model uzi picks" list in `docs/providers.md` adds the judge model. The Codex rows in `judge_rerun_policy_test.go` flip from refused to allowed. `docs/judge.md` documents where judged traces go for a Codex provider judge (D4).

**Tests** (watched red/green):

- Provider-only owner, unset source, source change while queued, revoked final delivery.
- Model rule, end to end: the fake gateway receives the expected gateway model for a `default`-only map, a key present whose target differs from the key, and a key absent. Invalid map or missing `default` fails closed.
- Settlement, one row per outcome. Classification rows use injected terminals: each limit classification gives `provider_gateway_limit`; `unauthorized`, `internalServerError`, `badRequest`, a policy refusal and a non-limit transport status keep the scrubbed fallback; unknown/absent classification, a transport variant without `httpStatus`, no terminal and a thrown or timed-out pass give terminal failure with no review; a prompt-build failure and unparseable output keep the fallback. Write first, as the riskiest assumption: 429 and 529 through the real app-server against the existing fake provider (`e2e/codex-m4/fake-provider.ts`, given an error-status mode; the custom-provider config sets zero retries), asserting the classification it actually produces and terminal `provider_gateway_limit` with no review, no fallback and no vendor cooldown. Controls: summary, task-review and plan-contract-review advice passes on a Codex provider behave as before on a 429.
- Claim carries only `secrets.provider`: no `secrets.codex` block, no capability mint, no `codex_secret_id` / `codex_auth_mode` on the row.
- A worker with only `codex_provider_v1` cannot claim, place, wake or reap-keep a provider judge (reap with busy and custody negative controls).
- A fake gateway echoing the key leaks it into no review frame, usage frame, `failure_reason` or log.
- An unusable source gives the `provider_source_unusable` skip (automatic) or 409 (manual).
- Engine-aware drift: an Anthropic pin change does not refuse a Codex provider judge.
- Unchanged: the Claude judge (vendor and provider) and the vendor Codex judge for an owner without a Codex source.
- LiveDB tests run only via `e2e/run-store-it.sh`; otherwise report them pending `test-api-store-it`.

### M2: Codex provider plan and code checkers, with the same-model guard

**Delivery.** One PR, non_closing for #4574: it uses `Refs #4574` and closes no issue until the owner acceptance below passes, unless the owner says otherwise. Hosted acceptance for M1 and M2 is tracked in #4761 (filed 2026-11-27; the Claude-engine counterpart is #4473).

**Lane decision (creation, recheck, attestation).** Replace the checkers' `refuseProviderLane` use with one `checkerLaneDecision` in `harness_resolver.go`. It returns Refused, Vendor or Provider from the lead's provider-boundness, the checker engine and one `GetUserProviderLaneSources` read.

- **Codex checker:**
  - A selected Codex source gives Provider, which freezes that source and fails closed (`ErrProviderSourceUnusable`, giving the stage's unavailable sink) when the source is unusable. The source resolves through the same source resolver the Codex judge uses, so an admin-shared Codex definition with the owner's own key (#4633) is covered.
  - With no Codex source, a provider-bound lead or a selected Claude source gives Refused (today's refusal).
  - Otherwise the result is Vendor (today's path).
- **Claude checker:** today's rule unchanged. A selected Claude source or a provider-bound lead gives Refused, otherwise Vendor (#4322 M2 widens it).

The one decision feeds:

- the pre-closure checks in `SubmitPlanCrossCheck` and `SubmitCodeCrossCheck` (in the create closures, today's unconditional `providerBoundRun(lead)` refusal becomes the `LeadProviderBound` equality check on the locked lead, described below);
- the transactional recheck in `createRunAtomic`'s origin arm, and both `createRunResolved` arms;
- the `checker_unavailable` attestation in `cross_check_gate_reason.go`.

The attestation accepts Refused, and accepts Provider when the re-read source is unusable (no child exists at attestation). It keeps today's vendor-resolution check only for Vendor.

**Consent toggle (owner decision Q3, 2026-11-27).** `BothHarnessesUsable` (T) follows the lane decision on each half:

- **Codex half**, three cases:
  - selected, usable Codex source: usable (the Provider path);
  - selected, unusable Codex source: not usable (409), even with a usable vendor Codex credential, since a Provider decision never resolves to vendor;
  - no selected Codex source: today's rule (a selected Claude source refuses; otherwise the vendor Codex check).
- **Claude half**, three cases:
  - selected, usable Claude source: usable, so CP/XP owners without a vendor Anthropic token can consent;
  - selected, unusable Claude source: not usable (409), even with a vendor Anthropic token;
  - no selected Claude source: today's vendor Anthropic check.
- Every other combination keeps today's 409 (CP/XV and CV/XV with a Claude source stay refused until the follow-up slice).
- Update the web mock mirror (`setPlanCrossCheckEnabled` / `setCodeCrossCheckEnabled`) and `RunDefaults.test.tsx` to match. No CLI impact (`PUT /me/cross-check` is cookie-only; no CLI text names the rule).
- Accepted: the Claude half follows how Claude runs resolve, not the M2 Claude-checker decision. An owner with both sources who enables cross-check gets Codex-lead checks (XP/CP, XV/CP) refused `checker_unavailable` until #4322 M2; document this in `docs/cross-check.md`.
- Docs: update the two "requires usable Claude and Codex credentials" lines in `docs/cross-check.md`.
- Known limit, not in M2: the toggle's post-enable worker warning does not check `codex_provider_checker_v1`.

**CheckerLane origin.** `RefuseProviderOwner` (`harness_overflow.go`) has only these two checker callers. Replace it with a `CheckerLane` origin:

- It carries `LeadProviderBound` (from the locked lead in the caller), because `createRunAtomic` has no lead row; the create closure re-asserts it against the locked lead, and a mismatch is Refused.
- It takes `LockSecretMutationShared` before the source read, as `JobLane` does. This lock is taken on every checker creation, including the Vendor and held paths: outcomes are unchanged, but a creation can now wait behind an exclusive secret writer. Accepted.
- Provider resolves with `vendorOnly=false`; Vendor and Refused keep `vendorOnly` (Refused returns before resolution).
- A Provider decision never resolves to vendor: after resolution, Provider with `res.ProviderSource == nil` returns `ProviderSourceUnusableError`, mirroring `jobProvider`. `verifyRunBinding` cannot catch this, because a vendor binding is all NULL and equals NULL stamps.
- The no-transaction arms (`createRunAtomic` with a nil `txBeginner`, `createRunResolved`'s fake arm) refuse Provider with `errProviderCreateRequiresTx`.
- `verifyRunBinding` gains a `runkind.CrossCheck` arm that validates only the map (`ValidateModelMapForEngine`), with no lead or template model checks.
- Update the `harness_overflow.go` exclusivity comments and every `_test.go` reference to `RefuseProviderOwner`.

Both child queries stamp the five binding columns from `sqlc.narg` parameters. Update their REFUSED-LANE comments.

**Code-stage capability fast-fail.** `fn_cross_check_child_eligible`'s code arm (latest definition 00383) requires `lead.worker_id = w.id AND r.worker_id = w.id`, so the code checker runs on the lead's worker. For a Provider decision:

- `SubmitCodeCrossCheck`'s closure extends its existing capability check with `workerSupportsProvider(worker, codex, cross_check)` and returns `ErrCrossCheckUnavailable` (creation-time sink, `RecordCodeCrossCheckFailure(checker_unavailable)`);
- `CreateCodeCrossCheckChild`'s guarded write, which already joins `workers w ON w.id = lead.worker_id`, requires the Codex provider capabilities plus `codex_provider_checker_v1` when the stamp is provider-bound.

The plan stage gets no such check: after the affinity cutoff any of the owner's workers may claim, so the lead's worker is not the checker's eligibility there.

**Claim (Codex arm only).**

- Narrow C to `run.Kind == runkind.CrossCheck && run.Harness != harnessCodex`.
- In `workerSupportsProvider`, the Codex arm adds `kind != runkind.CrossCheck || CodexProviderCheckerV1`. This runs against the actual checker worker at claim.
- In `singleFamilyCheckerCustody`, the Codex arm requires no Anthropic token and exactly one of `Secrets.Codex` or a `Secrets.Provider` with `Engine` codex. The Claude arm is unchanged.
- **Lead-first provider-checker settlement** (`claim_recovery.go`). Add `isProviderCheckerChild(run)` (`run.Kind == runkind.CrossCheck && providerBoundRun(run)`):
  - it joins `finishRunClaimTx`'s lead-first entry condition beside `isClaudeCheckerChild`, so every provider checker claim, a successful one included, takes the lead lock before the child lock;
  - the assembly-time mapping (`claudeCheckerClaimError`, generalized to both predicates) maps a provider claim refusal, including the `errProviderLaneClaimRefused` capability backstop, to `errCheckerPinUnavailable`;
  - a `lockedProviderRefusal` hit on a provider checker child (key revoked or rotated, revision changed, destination no longer allowed, between assembly and finish) sets payload nil and `errCheckerPinUnavailable` and clears `providerRefusal` (the park arm keys on it), before the Judge/Job diversion and the generic park arm, mirroring the `credDisabled && isClaudeCheckerChild` arm.
  A provider checker child therefore never takes `ParkCredentialDisabledRun`. Vault-locked and seal-stale stay transient, as for the Claude checker.
- There is no creation-time refusal keyed on the lead's worker at the plan stage; the code stage has the fast-fail above.

**Same-model guard (engine-generic).** It runs in `assemblePlanCrossCheckInput` (`cross_check_claim.go`), under the lead verdict lock. The order is: after `payload.Config` and the provider binding are assembled; before `RecordPlanCrossCheckClaim` / `RecordCodeCrossCheckClaim` and before the claim response.

- **Short-circuit first.** Provider involvement is decided from the lead's frozen binding columns and the child's binding. A vendor/vendor pair returns NotEvaluated before any `run_lead_configs` or provider revision read, so today's lanes gain no new read or DB-error path.
- **Lead evidence.** One new query in `lead_configs.sql` reads the lead's row for the cross-check's `lead_claim_generation` and phase (`plan` for the plan stage, `implement` for the code stage), highest `seq`. `ListLatestRunLeadConfig` is not used: it selects the highest generation with rows, not the check's generation. No row is Unresolved.
- **Lead identity, per side.**
  - **Vendor lead** (CV in M2): the observed id when `observed_model` is present, else `requested_model`. For an alias the pinned CLI reports the canonical id (`agent/src/claude-canonical-models.ts`); the server does not mirror that table, which drifts with each CLI pin. No requested-versus-observed disagreement rule applies, because alias versus canonical id is expected. Unresolved only when neither value is present.
  - **Provider lead** (CP in M2): `ResolveModelID(claude, frozen map, requested_model)`, the same resolution the worker's `resolveProviderModel` applies (blank or `inherit` goes through `sonnet`). An unmapped value is Unresolved. `observed_model` is not part of the identity; the first test below proves the pinned CLI sends the frozen-map target.
  - An observed value is never fed through Codex's key-or-`default` resolver.
  - In M2 the lead side is always Claude and the checker side Codex; a Codex-lead identity is first needed by #4322 M2.
- **Checker identity.** The model comes from the claim-time checker resolution: `payload.Config.DefaultModel` and its `ModelSource`, resolved through `payload.Secrets.Provider`'s frozen map with `explicit` from the model source.
- **Pure functions** (new, in `api/internal/providers`):
  - `ResolveModelID(engine, map, requested, explicit)` returns the target model id. Claude: an alias maps to its target, a value equal to a target maps to itself, blank or `inherit` goes through `sonnet`, anything else is Unresolved. Codex: a key (including a requested literal `default`) maps to its target; otherwise the `default` target, or Unresolved when explicit.
  - `NormalizedHost(baseURL)`.
  - A comparison of RESOLVED IDs with exact string equality. Vendor/vendor gives NotEvaluated. When both sides are provider-bound, the same host plus the same ID gives SameModel. With one vendor side, the same ID gives SameModel. Any Unresolved side gives Unresolved; anything else is Independent.
- **Outcome.** SameModel and Unresolved wrap `errCheckerPinUnavailable` with server-side detail (`same_model`, `model_unresolved`), as `errCheckerRunEnvUnavailable` does (`run_env_claim.go`). `finishRunClaimTx` then settles them through the actual-claim sink above, and no payload is delivered.
- **Re-anchor.** At actual claim, the code stage does not call `RecordCodeCrossCheckFailure`, which is creation-time only.
- **Explicit pin.** A checker model is explicit when its resolved `ModelSource` is the pin source (`agenttmpl.CrossCheckPin`, `cross_check_pins.go`). An effort-only pin leaves the model source at its default and still allows key-or-`default`.
- **Reporter provenance.** The lead-config reporter (`agent/src/lead-config-report.ts`) becomes an authority input for this guard: a missing row (never sent, dropped at `LEAD_CONFIG_QUEUE_MAX`, or late) yields Unresolved. A request-only row still resolves: a provider lead through its frozen map (an unmapped value is Unresolved), a vendor lead through the requested value. So a vendor lead whose observation row is dropped or late keeps an alias-only identity (for example `opus`), which can read Independent against a checker target equal to that alias's canonical id. That residual is the Q6 alias limit, documented with it. Its header comment changes from "must not change what the run does" to say that a missing row makes a provider-involved cross-check unavailable. The rows are worker-reported, from the owner's own worker, at the same trust as the claim: accepted. Accepted limits: a resumed generation with no plan-phase row is unavailable at the plan stage; the code stage sees only the submitting generation's implement rows.

Generic versus Codex-only:

- **Generic:** `checkerLaneDecision`, `CheckerLane`, both SQL stamps, `isProviderCheckerChild` and the lead-first settlement, the `providers` identity and comparison functions, the per-generation lead-config query, and the guard call.
- **Codex-only:** everything else in this section.
- **Claude checker lane:** its decision is today's rule, so no Claude-provider checker lane opens.

**Second implementation (contract).** `ResolveModelID` mirrors the agent's `resolveProviderModel` and `resolveCodexProviderModelSlot` plus the map lookup. Pin them with a new shared fixture under `fixtures/` (for example a `provider-model-identity` cases file), read by a Go test and an agent test, with one case per behaviour, and each test asserts every case was exercised:

- Claude: alias present, alias absent, target value, a non-alias value matching no target (Unresolved; the agent throws `ProviderModelUnmappedError`), blank, `inherit`;
- Codex: a key with a differing target, a key absent and not explicit (gives `default`), a key absent and explicit (Unresolved), blank (gives `default`), a requested literal `default` with explicit (the `default` key);
`NormalizedHost` has no agent twin, so its cases stay in a Go-only test. The guard's Go test (not the shared fixture, since no agent code resolves observed values) covers an observed target that is not a key, never resolved through `default`. `NormalizeBaseURL` already lowercases hosts and refuses a trailing dot, so stored base URLs carry neither.

Amend M1 D3 to read: "`resolveCodexProviderModelSlot` is the only worker implementation; the server's identity twin is fixture-pinned and runs before delivery".

**Capability and placement.** `codex_provider_checker_v1` goes in:

- `api/internal/capability/capability.go`: constant, vocabulary and `protocolOrder`;
- `agent/src/provider-binding.ts`: constant;
- `worker.ts`: advertised under `codexHarness.advertise`, with `crossCheckSlots > 0` and a cross-check runner present;
- `requireProviderCapability` (`client.ts`): required for kind `cross_check`.

Census command (the M1 one with the judge capability): `git grep -n -E 'codex_provider_judge_v1|CodexProviderJudgeV1|CODEX_PROVIDER_JUDGE_CAPABILITY' -- api agent ':!*_test.go' ':!*.test.ts' ':!*.sql.go' ':!api/internal/store/migrations' ':!api/internal/uzidocs/embed'`. At `a2b7749e` it lists:

- `runtime.sql`: the ClaimRun primary and its fleet-spread mirror, `CountOnlineWorkersClaimableForRun`, `CountOnlineWorkersSatisfyingProvider` (`@kind`) and both arms of `ListSaturationQueuedRunsForEphemeral` (demand);
- `hibernation.sql`;
- reap arm (g) in `hosted_workers.sql` (both statements, kept byte-identical);
- `health.go` and `provider_claim.go`.

Each site gets `(r.kind <> 'cross_check' OR 'codex_provider_checker_v1' = ANY(...))` in the Codex provider arm only, or a recorded reason why it never sees a checker child.

Health: no new string. A queued provider checker with no capable worker reads an existing free-text health reason (`reasonNoProviderCapableWorker` through `CountOnlineWorkersSatisfyingProvider`, or `reasonNoCrossCheckWorker` if the cross-check rungs answer first); a test pins which one. Neither is a gate reason.

Migration `00546_placement_codex_provider_checker_capability.sql` (the number is a draft):

- Up: the live `fn_placement_admitting_workers` at landing (00545 at this SHA) plus that term in the Codex provider arm.
- Down: the 00545 Up verbatim, or whatever the immediate predecessor is at landing.

**Worker consumer.**

- `cross-check-runner.ts` keeps the provider throw for a Claude-engine binding and selects the family with `selectCodexExecutionAuthority` (`codex/select.ts`).
- `CodexCrossCheck.run` takes the provider authority. It registers `providerKeyForms` for log and verdict scrubbing, and opens no subscription operation scope. It builds the custom-provider app-server config with `buildCodexProviderConfigToml` (`agent/src/codex/config.ts`), as `codex-executor.ts` and `codex-advice-harness.ts` already do. The model comes from `resolveCodexProviderModelSlot`, with explicit set from the model source as above.
- Model failures keep `model_error` / `model_timeout`. A gateway limit is a non-pass with no vendor cooldown. No new reason.

**Tests** (watched red/green):

- **First, the real-CLI identity check** (riskiest assumption). Capture the pinned Claude CLI's init frame offline against a fake Anthropic gateway, following `test/phase-model-switch.integration.test.ts` and `test/provider-context-window.integration.test.ts`:
  - provider lead: with `ANTHROPIC_DEFAULT_*_MODEL` set from a frozen map, both the observed model and the `model` field the fake gateway receives (recorded as `provider-context-window.integration.test.ts` does) equal the frozen-map target `ResolveModelID` returns, for an alias and for a blank request;
  - vendor lead: an alias request is observed as its canonical id (already pinned for the tables by `phase-model-switch`; extend it to the `observed_model` the reporter sends).
  If the provider half fails, stop and escalate; there is no fallback identity.
- **Lead evidence, fail-closed:** a missing row, a dropped row, a row from another generation, and a request-only row for a provider lead with an unmapped value each give `checker_unavailable`; a request-only vendor row uses the requested value.
- **Vendor-lead rows:** CV lead requested `opus`, observed `claude-opus-5-5`, against an XP checker target `claude-opus-5-5` (SameModel) and a different target (runs); a CV request-only row; a CV builtin row (requested NULL) with an observed id.
- **Lane rows:** one case per table row and stage, including the held and unchanged rows, an unusable Codex source, and an admin-shared Codex definition with the owner's own key.
- **Provider never vendor:** a Provider decision whose resolution returns no provider source fails `ProviderSourceUnusableError`, never a vendor child.
- **Code-stage fast-fail:** a lead worker without `codex_provider_checker_v1` gives an immediate code-stage `checker_unavailable`, with no child created, in the Go check and in the guarded SQL write.
- **Guard:** both directions, mapped aliases, key absent giving `default`, explicit pin absent from the map, effort-only pin, different hosts with the same ID, and unresolved lead and checker.
- **Vendor/vendor:** with both models unset, the check is created and runs unchanged, and the guard performs no lead-config or revision read (a store fake that fails those reads proves it).
- **Late refusal, both stages:** revoke or rotate the key in `claimHooks.afterAssembly`; the check is decided `checker_unavailable` lead-first, the child is never `paused`, and no payload is delivered. Also the capability backstop (`errProviderLaneClaimRefused`) maps lead-first.
- **Attestation:** accepted for each refused or unusable decision.
- **Custody:** rejects a claim holding both credential blocks, and one holding neither.
- **Capability:** a checker worker without it cannot claim or place a provider checker; the health reason test above.
- **Held rows under the new lock:** a LiveDB test that CV/XV and CP/XV outcomes are unchanged with a concurrent source write.
- **Settlement and scrubbing:** key echo appears in no verdict, frame or log.
- **Fixture:** the differential in both languages.
- **Consent toggle:** a handler test per combination, each isolating one half: in every 409 case the other half is usable, and in every 200 case the half that should count its source has no vendor credential, so a wrong fallback cannot pass:
  - Codex source usable, no vendor Codex credential, vendor Anthropic token (200);
  - Claude source usable, no vendor Anthropic token, Codex source usable (200);
  - Codex source unusable with a usable vendor Codex credential, Claude half usable (409);
  - Claude source unusable with a vendor Anthropic token present, Codex source usable (409);
  - Claude source usable with no Codex source, vendor Codex credential usable (409, today's rule);
  - vendor/vendor unchanged.
  The existing `TestProviderLaneSettingsSelectionBeforeCredentials` flips for the source-usable cases; update it. The web mock mirror and `RunDefaults.test.tsx` carry the same cases, and the mock's code toggle returns 409 (today 400) to match the handler.
- LiveDB tests run via `e2e/run-store-it.sh`; otherwise report them pending.

Docs: ADR-4320 D9 amendment; the ADR-2149 "Single-family custody" amendment and its "Claim finalization lock order" paragraph changed to "every Claude or provider-bound checker claim"; the lead-config reporter header; `docs/providers.md` (narrow the interim note to the held rows); the cross-check docs (pin the vendor side for auto-approve; the alias limit); `ARCHITECTURE.md` lane status and an AI-synced `specs/human.md` entry.

**Landing obligations, outside milestones.**

- **full agent suite: exact-head PR CI.**
- LiveDB: exact-head `test-api-store-it` with the named new LiveDB tests executed, not skipped (or `e2e/run-store-it.sh` evidence from an owned Docker-capable DB).
- `test-agent-worker-uid`, which runs for this `agent/**` change.
- The applicable API and repo CI checks.

**Owner acceptance** (in #4761; not a merge blocker; the PR stays non_closing until it passes):

- a real Codex gateway checker for CV/XP and for CP/XP, at both the plan and the code stage, ending in a real verdict;
- the guard with a gateway whose target equals the lead's model: plan takes the human gate, code records `checker_unavailable`;
- the key revoked or disabled while a checker is queued: `checker_unavailable`, never a paused child;
- an old worker without the capability: the plan stage waits to its deadline, the code stage fails fast; then the fleet roll advertises the capability and the next check runs;
- an admin-shared Codex definition with the owner's own key, and a missing-key control;
- no key form in the verdict, findings or worker logs;
- the consent toggle: an owner with a usable Codex source (and a usable Claude source or vendor token) can enable plan and code cross-check; an owner with an unusable source still gets 409.

#### Refusal assignment (pre-dispatch fold, 2026-11-27)

Bindings: CV = Claude vendor, CP = Claude provider, XV = Codex vendor, XP = Codex provider. The checker is always the opposite engine (guarded SQL write in both child queries). The checker binding is decided by the owner's source for the checker's engine at creation (#4322 D2), never by the lead.

Rule (decided): assign by checker engine. A Codex checker (Claude lead) is #4574's; a Claude checker (Codex lead) is #4322's. A Codex provider lead with a Claude vendor checker (XP/CV) waits for #4322 M2.

Shared refusal sites, cited by letter:

- **T** Consent toggle: `SetCrossCheck` (`api/internal/handler/cross_check_toggle.go`) returns 409 "both Claude and Codex credentials must be usable" when `BothHarnessesUsable` (`harness_resolver.go`) is false. `BothHarnessesUsable` calls `refuseProviderLane(HarnessCodex)`, so either selected source refuses the enable, then requires a vendor Anthropic token and a usable vendor Codex credential. Web mock mirror: `setPlanCrossCheckEnabled` / `setCodeCrossCheckEnabled` (`web/src/mocks/mockApi/settings.ts`), pinned by `web/src/pages/RunDefaults.test.tsx`. T gates enabling only; an owner who enabled before selecting a source reaches the rows below.
- **A** `refuseProviderLane` (`api/internal/workersvc/harness_resolver.go`): a Claude source refuses both checker engines; a Codex source refuses the Codex checker only. It is called from:
  - `SubmitPlanCrossCheck` (`cross_check.go`) and `SubmitCodeCrossCheck` (`code_cross_check.go`);
  - the `RefuseProviderOwner` arms of `createRunAtomic` (`harness_resolver.go`, both the transactional arm and the nil-`txBeginner` arm) and `createRunResolved` (`harness_create.go`);
  - the `checker_unavailable` attestation (`validatePlanCrossCheckGateReason`, `cross_check_gate_reason.go`);
  - T, through `BothHarnessesUsable`.
- **L** Lead-side `providerBoundRun(lead)` refusal (any engine), attested by `cross_check_gate_reason.go`. It appears twice in `SubmitPlanCrossCheck` (before commit and in the create closure) and twice in `SubmitCodeCrossCheck` (before the create and in the closure).
- **S** Both child INSERTs write `model_provider_key_mode = NULL::text` and none of the other four binding columns: `CreatePlanCrossCheckChild` (`api/internal/store/queries/runtime.sql`) and `CreateCodeCrossCheckChild` (`code_cross_check.sql`).
- **C** `assembleClaim`'s pre-vault provider refusal predicate (`claim_assembly.go`): `run.Kind == runkind.CrossCheck` gives `errProviderLaneClaimRefused`. `workerSupportsProvider` (`provider_claim.go`) has no checker arm.
- **K** `singleFamilyCheckerCustody` (`cross_check_claim.go`): the Codex arm requires `Secrets.Codex != nil`; the Claude arm requires `Secrets.Provider == nil`.
- **W** Worker: `cross-check-runner.ts` throws `PROVIDER_LANE_UNSUPPORTED` on any `secrets.provider`. `CodexCrossCheck.run` (`agent/src/codex/cross-check.ts`) uses the vendor-only `selectCodexBinding`.
- **P** Placement and ClaimRun SQL: the provider arms of `fn_placement_admitting_workers` (00545) and ClaimRun carry no checker clause. A provider child cannot exist today, so this is latent rather than a live refusal; M2 adds the clause before it lifts S.

Sinks:

- **Creation time:**
  - plan: `ErrCrossCheckUnavailable` leads to the existing human gate;
  - code: `RecordCodeCrossCheckFailure(checker_unavailable)`, which runs only when no child exists.
- **Actual claim:** `finishRunClaimTx` (`claim_recovery.go`) takes the lead lock (`LockPlanCrossCheckLeadForVerdict` / `LockCodeCrossCheckLeadForVerdict`) when its lead-first entry condition holds. Its `errCheckerPinUnavailable` arm then calls:
  - `DecidePlanCrossCheck(failed, checker_unavailable)` for plan, giving the human gate;
  - `DecideCodeCrossCheck(failed, checker_unavailable, empty findings)` for code.
  The child itself then fails through the generic assembly-failure arm (code text "code cross-check: checker unavailable").

| Lead | Checker | Today, plan and code (same sites; stage sink as above) | After #4574 M2 | Owner | Guard |
|---|---|---|---|---|---|
| CV | XV | Runs. Refused by A when the owner has a Claude source (lineage case) | unchanged in M2 | follow-up slice after M2 (owner Q2) | not evaluated (vendor/vendor) |
| CV | XP | A (Codex source), then S, C, K (Codex arm), W | runs | #4574 M2 | evaluated: model ID |
| CP | XV | L, plus A when the Claude source is still selected | unchanged in M2 | follow-up slice after M2 (owner Q2) | n/a while refused |
| CP | XP | L, A, then S, C, K, W | runs | #4574 M2 | evaluated: host + model ID |
| XV | CV | Runs (a Codex source does not trigger A for a Claude checker) | unchanged | none | not evaluated |
| XV | CP | A (Claude source), then S, C, K (Claude arm), W (`claude-cross-check.ts` path) | refused | #4322 M2, reuses #4574's guard | evaluated: model ID |
| XP | CV | L only | refused | #4322 M2, reuses #4574's guard; no Claude-provider consumer needed | evaluated: model ID |
| XP | CP | L, A, then S, C, K (Claude arm), W | refused | #4322 M2, reuses #4574's guard | evaluated: host + model ID |

T sits in front of every row with a selected source: today such an owner cannot enable plan or code cross-check at all. M2 changes T per the owner's Q3 answer (*Consent toggle* above), so the lifted rows reach owners who enable after selecting a source.

Every row still refused after M2 keeps its current site and sink, and its `checker_unavailable` attestation stays accepted. The owner decided (Q2) to lift CV/XV and CP/XV in a separate small slice after M2, with docs; M2 keeps today's refusal.

#### M2 decisions and risks


- **Q1 (decided):** assign by checker engine. XP/CV waits for #4322 M2.
- **Q2 (decided by the owner, 2026-11-27):** lift CV/XV (a Claude source no longer blocks a Codex vendor checker) and CP/XV (a Claude-provider lead's snapshot goes to the owner's vendor Codex credential) in a separate small slice after M2, with docs. M2 keeps today's refusal for both. The slice is a one-branch change in `checkerLaneDecision`, the matching toggle row, plus an ADR-4320 D9 amendment; tracked as #4762.
- **Q3 (decided by the owner, 2026-11-27): the consent toggle.** Each half of `BothHarnessesUsable` follows the lane decision: the Codex half is usable exactly when the Codex checker would take the Provider path (a selected, usable Codex source; with no Codex source, today's rule); the Claude half also counts a selected, usable Claude source (sub-question answered yes). On either half a selected but unusable source gives 409 even when that engine's vendor credential is usable; with no selected source of that engine, today's check applies. Visible behaviour: a 409 becomes a 200 for those owners. Rejected: keeping T unchanged (the lifted rows would reach only owners with prior consent).
- **Q4 (riskiest assumption):** the per-side lead identity. The real-CLI test is the plan's first test; if the pinned CLI does not send the frozen-map target for a provider lead, the run stops and escalates. Missing or ambiguous evidence is unavailable; there is no fallback to `resolveLeadSettings`, which cannot see the worker-side template model.
- **Q5 (decided):** a fixture-pinned Go twin before delivery, plus the D3 wording amendment. Rejected: a worker-side guard, which runs after credential delivery.
- **Q6 (decided):** a vendor alias versus a provider-mapped ID is not an exact match, so a vendor lead with only a requested alias against a checker target equal to that alias's canonical id passes as independent. The same applies when a vendor lead's observation row is dropped or late, leaving only the requested alias. This is the PRD's "different names do not prove independence" limit, documented; no alias catalog.
- **Owner option (open):** comparison is exact string equality on resolved IDs. Case folding, or any wider normalization, stays an owner choice for later.
- **Q7 (decided):** an explicit pin is identified by the resolved model source (`CrossCheckPin`); an effort-only pin still defaults the model.
- **R1:** for a vendor Codex checker, the capability can be minted before `assemblePlanCrossCheckInput`. Accepted: `crossCheckAssemblyError` already settles a minted identity on that path (the run-env precedent). Minting is not delivery. While CP/XV is held, no guard-evaluated pair has a vendor Codex side, so R1 is latent in M2. The Claude-checker twin is in PRD #4322's M2 hand-off.
- **R2:** a lead-config row appended after assembly and before final delivery is not re-read. Accepted boundary: the candidate digest froze the plan, and every later round is a new claim that re-evaluates.
- **R3 (rollout):**
  - API first: an older worker lacks the capability. At the code stage the lead's worker is the checker worker, so the check fails fast with `checker_unavailable`. At the plan stage the child waits to its deadline, then times out (human gate).
  - Worker first: the capability goes unused, which is harmless.
  - Rollback: restoring 00545 removes the new placement predicate. The fail-closed backstop is the older API's pre-vault claim refusal C (`runkind.CrossCheck` refused for any provider-bound run), not the restored placement function. Provider children created before the rollback then time out.
- **R4 (serialization):** serial with #4322 M2 and with any other `fn_placement_admitting_workers` replacement (D8). Confirm the head at landing; run `task migration:renumber`.
- **R5 (new lock):** `CheckerLane` takes the shared secret-mutation lock on every checker creation, held and Vendor paths included. Outcomes are unchanged; a creation can wait behind an exclusive secret writer. Accepted, with the LiveDB test above.
- **Scope guard:** no new public reason, gate state, health string, table, column, service, alias catalog or version floor. New: one pure-function package addition, one read query, one guard call at an existing seam.

**M2 sequencing (does not block M1).** M2 overlaps #4322 M2/M2b on the same mechanism, not only the placement predicates: `cross_check.go`, `code_cross_check.go`, `cross_check_gate_reason.go`, `refuseProviderLane` (a Claude source blocks both engines today), `singleFamilyCheckerCustody`, the checker provider-creation origin and the same-model guard. The two M2 slices are serial, never concurrent: whichever lands first builds the engine-generic mechanism, the second adds only its engine's arm. Overlapping #2169 work goes first; #2169's implementation is merged and only its hosted acceptance is pending, and M2 depends on the implementation only. The refusal table above assigns each refusal by checker engine; for example, a Claude provider lead with a Codex provider checker is #4574's to lift; a Codex provider lead with a Claude vendor checker is #4322 M2's. Unfinished Claude-provider checker lanes stay refused.

## Verification

Each slice is one PR with watched red/green regressions. In-run: `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent` plus touched tests and `task test:codex-m4`; `task check:api` plus affected Go packages with `-race -count=1`; migration and sqlc checks; `docs:sync` + `check-docs:web`; `check:changelog-entry`; `task scan:secrets` with canaries. Update ADR-4320 D9 lane status, ADR-2149 checker custody, `docs/providers.md` (remove the #4323 interim note) and an AI-synced `specs/human.md` entry. Sweep changed reasons across `e2e/` and run `task test:e2e-lib` and `task test:forge-fake`.

Landing obligations, outside milestones: **full agent suite: exact-head PR CI** and the applicable API/repo CI checks. Hosted real-gateway acceptance is maintainer issue #4761 (the Claude counterpart is #4473), not a merge blocker. In-run for M1 also: `task check:web`, the touched `JudgePanel` skip tests and `docs:sync`.

## Decision Log

- **2026-11-21: Split from #4323.** Reason: one outcome per run, and #4322's per-lane slices, capabilities and same-model guard are the pattern to mirror. Rejected: bundling judge and checkers into the Codex issue-lane PRD.
- **2026-11-21: Same-model guard reused, not duplicated.** Reason: opposite engines no longer imply different models once a gateway maps names; #4322 owns the contract. Rejected: a Codex-specific guard or a new public reason.
- **2026-11-25: Codex judge model rule is key-or-`default` (D3).** Reason: it matches the shipped Codex advice resolver, which stays the only implementation (no second server resolver); the Claude-style `modelMapped` check wrongly refused `default`-only maps and differing targets for Codex. Rejected: strict explicit-key matching, which would need a worker change and a new skip.
- **2026-11-25: Redirect risk covers Codex gateways (D4, owner).** The owner confirmed that #4322 D5 (2026-11-20) covers both engines: judged traces, including vendor-run traces judged by provider policy, may reach a Codex gateway. Documented in `docs/judge.md`.
- **2026-11-25: Dual architecture review folded (v2).** Engine-aware change sites, capability census command, typed limit seam scoped to the judge, fuller acceptance tests, copy sweep, M1/#4322 M1b and M2/#4322 M2 serialization.
- **2026-11-25: Conditional capability advertisement (M1).** `codex_provider_judge_v1` is advertised only under the Codex harness advertise gate, and the server gates claim, placement, demand, reap and wake on it for Codex provider judges (placement migration 00545, composed on #4594's 00544 run_env_v1 definition; Down restores 00544). Reason: a worker that cannot run the Codex harness must never be handed a Codex provider judge; the capability follows the same gate as `codex_provider_v1`.
- **2026-11-25: Typed settlement seam (M1).** A judge-only `settlement` on the read-only Codex model pass carries the terminal classification, with escaped-rejection provenance and presence flags; first match wins, and anything unmatched is terminal (failed, no review). Reason: parity with the Claude judge seam without changing shared advice behavior.
- **2026-11-25: `FakeProvider` error-status mode (M1).** The fake provider gained an opt-in error-status mode so the P test can drive 429 and 529 through the real app-server. Reason: the riskiest assumption (which classification a gateway limit produces) had to be proved against the real runtime.
- **2026-11-25: Rerun fake-store handler test rows expect the fake store's 500.** Reason: whether Run judge is allowed for a Codex provider is proved in LiveDB, not in the fake store, so the handler rows now assert the fake store's 500.
- **2026-11-25: Routing by engine (M1).** Only a binding whose engine is `codex` goes through `selectCodexExecutionAuthority`; Claude bindings keep the pre-existing discriminator. Reason: avoids changing the shipped Claude provider judge path.
- **2026-11-25: Accepted limit, journaled terminal log re-send.** A worker's later re-send of a journaled terminal log through its logger is not scrubbed again; the body is key-free because the key is redacted before it is journaled. Reason: re-scrubbing would need a worker-side change outside M1.
- **2026-11-27: M2 refusal assignment and sequencing (pre-dispatch fold).**
  - **Assignment:** cross-check refusals are assigned by checker engine. #4574 M2 lifts CV/XP and CP/XP. #4322 M2 lifts XV/CP, XP/CV and XP/CP. M2 keeps today's refusal for CV/XV and CP/XV; the owner decided to lift both in a separate slice after M2.
  - **Codex lane decision:** a selected Codex source takes the provider path and fails closed if unusable; a Provider decision never resolves to vendor. Without one, a provider-bound lead or a Claude source stays refused; otherwise the vendor path. One decision serves both stages, the transactional recheck and the gate attestation. The consent toggle follows the same decision on each half (owner decision): a selected, usable source counts as usable; with no source of that engine, today's rule.
  - **Generic mechanism:** #4574 M2 lands first and builds it:
    - `checkerLaneDecision` and the `CheckerLane` origin;
    - binding stamps in both child queries;
    - lead-first settlement for every provider checker claim: assembly refusals, the capability backstop and late locked refusals decide `checker_unavailable` and never park;
    - the same-model guard: exact comparison of resolved IDs from fixture-pinned pure functions in `api/internal/providers`, short-circuited for vendor/vendor before any read, called in `assemblePlanCrossCheckInput` and settled at actual claim through `finishRunClaimTx`'s `errCheckerPinUnavailable` arm (`DecidePlanCrossCheck` / `DecideCodeCrossCheck`; `RecordCodeCrossCheckFailure` stays creation-time only).
  - **Lead identity, per side:** read from the lead's `run_lead_configs` row for the check's generation and phase (a new per-generation query). A vendor lead uses the observed id, else the requested one; a provider lead uses its frozen-map resolution, proven against the pinned CLI first. Missing or ambiguous evidence is unavailable. The lead-config reporter becomes an authority input: a missing row makes a check unavailable, while a request-only row falls back to the provider lead's frozen-map resolution or the vendor lead's requested value (with the documented alias-only mismatch risk); worker-reported rows are accepted at claim trust.
  - **Codex arm:** `codex_provider_checker_v1`, required on the checker worker in claim and placement, and on the lead's worker at code-stage creation (the code checker runs there); the custody widening; the agent consumer; placement migration 00546 (draft). Down restores the live predecessor; the older API's pre-vault refusal is the rollback backstop.
  - **Delivery:** one non_closing PR; owner acceptance in the #4574 hosted acceptance issue.
  - **Reason:** one engine's checker per slice, no Claude-provider checker lane opened early, no behaviour change without the owner, no new reason or subsystem.
  - **Rejected:** assigning by lead binding (it splits one checker path across two PRDs); a worker-side guard (it runs after delivery); a plan-stage capability refusal keyed on the lead's worker (any of the owner's workers may claim a plan checker after the affinity cutoff, so it is not the checker's eligibility); a strict requested-versus-observed disagreement rule for a vendor lead (alias versus canonical id is expected, so CV/XP would never run); a server mirror of the CLI's canonical-model table (it drifts with every CLI pin).
- **2026-11-27: Owner decisions for M2 (Q2, Q3).** Q2: CV/XV and CP/XV stay refused in M2 and are lifted in a separate small slice after it, with docs. Q3: each half of the consent toggle (`BothHarnessesUsable`) follows the lane decision; a selected, usable source counts, a selected but unusable source gives 409 even with that engine's vendor credential, and with no selected source the existing check applies. Reviews: lead and buddy, opus/xhigh (final pass APPROVE on the folded draft), gpt-6-astra/high (APPROVE, no findings).
