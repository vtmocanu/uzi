# PRD #4574: Codex providers on judge and cross-check

**Issue**: [#4574](https://github.com/vtmocanu/uzi/issues/4574)
**Priority**: High
**Umbrella**: [#4319](https://github.com/vtmocanu/uzi/issues/4319)
**Blocked by**: [#4323](https://github.com/vtmocanu/uzi/issues/4323) (landed in #4638), and merged [#4322](https://github.com/vtmocanu/uzi/issues/4322) M1a before #4574 M1. The remaining #4322 slices are not prerequisites.
**Disposition**: Prepared follow-up. Dispatch waits for the applicable slice prerequisites and final architecture/spec approval. One slice per gated run; no on-deck sweep enrollment. Splitting and filing this PRD do not dispatch it or extend the #4323 dispatch grant; dispatch requires an applicable user grant.

## Problem and outcome

After #4323, a Codex provider source serves the issue/task lane, but a Codex judge for that owner skips with `provider_lane_unsupported`, and any plan or code cross-check where the lead or checker is Codex-provider-bound refuses (plan: human gate; code: `checker_unavailable`). Deliver both lanes on the Codex provider source, mirroring #4322's per-lane slices for Claude, while preserving each lane's independent credential authority and lifecycle.

Examples:

1. An owner with `codex_provider_id` set and a Codex judge gets a provider-frozen judge at creation; a bad selected source skips explicitly, never spending a vendor credential.
2. A Claude lead's Codex plan or code checker freezes the owner's Codex source and uses only it. A Codex provider lead's Claude checker uses its own Claude authority (#4322).

## Out of scope

Jobs, chat and the isolated lane on Codex; admin-shared providers (#4324); new public gate reasons; any change to vendor/vendor pairs. No implementation or validation edits under `.github/workflows/`.

## Shared contract

Same invariants as #4323 and #4322: exclusive frozen binding on the five `runs.model_provider_*` columns, `enablement_rev` fence, provider-only claim (no `secrets.codex` block or vendor credential-operation capability), no vendor fallback, #4505 key-forms redaction on every lane output and the fail-closed upload scan, accepted redirect risk (owner 2026-11-19) unchanged. Lane settlement follows #4322's matrix (typed terminal failure for judge/checker child revocation; no generic provider parking of repo-less evaluators).

Per-lane capabilities, alongside `codex_provider_v1`: `codex_provider_judge_v1` (M1) and `codex_provider_checker_v1` (M2, both stages). Each is advertised only once its consumer is complete, and is required in ClaimRun, the `claim_assembly.go` backstop, `fn_placement_admitting_workers` (additive replacement migration, draft number), health, demand and reap predicates. Old workers receive no lane claim; an unused capability is harmless. Each slice's placement and claim predicate change is serial with #4322's: Up extends the live definition at landing and preserves every merged Claude lane predicate, and Down restores that exact immediate predecessor.

## Slices

### M1: Codex provider judge

Reuse merged #4322 M1a judge authority, settlement and capability seams.

At both judge creation sites (`judge_enqueue.go`, `judge_read.go`), when `judgeHarnessFor` selects Codex and the owner's Codex source is set, freeze that provider/key/revision at creation; null source keeps the existing Codex vendor default/account-mode resolver. Do not invent a Codex judge pin/auto setting or apply `judge_anthropic_bind_mode`. Later source edits affect new judge rows only. Validate the actual judge model against the frozen Codex map. Wire `judge-runner.ts` through the #4323 provider arm. Test provider-only owner, unset source, source change while queued, revoked final delivery and gateway limit (`provider_gateway_limit`, terminal).

### M2: Codex provider plan and code checkers, with the same-model guard

Both `CreatePlanCrossCheckChild` and `SubmitCodeCrossCheck` / `CreateCodeCrossCheckChild` resolve the checker engine's own source and freeze it, not the lead's binding; remove the #4323 interim refusals only when the consumer and capability are ready. Widen `singleFamilyCheckerCustody` on the Codex side only: a Codex checker holds exactly one Codex credential (vendor or provider) and no Claude credential. The Claude side is #4322's. Wire `cross-check-runner.ts`. Stage outcomes stay: plan failure takes the existing human gate; code failure calls `RecordCodeCrossCheckFailure`.

Both plan and code cross-checks retain the opposite-engine requirement and the provider-involved actual-model guard specified by #4322. Before model execution or credential delivery at actual claim, compare the resolved lead/checker identities after frozen model mapping. Evaluate only provider-involved pairs; vendor/vendor behavior remains unchanged. Two provider sides with the same normalized base-URL host and resolved model ID are a known match; with one vendor side, matching resolved model IDs are a conservative known match. An unresolved identity in a provider-involved pair is unavailable, and different names/hosts do not prove independence. Use existing `checker_unavailable` with server-side `same_model` detail for a known match: the plan stage follows its existing unavailable human-gate path, while the code stage calls `RecordCodeCrossCheckFailure` with no verdict. Reuse the shared guard if #4322 has landed; otherwise implement the smallest shared guard for the newly enabled Codex paths without enabling unfinished Claude lanes. Do not make unfinished #4322 an implicit dependency or invent a new public reason. Test both directions, both stages, mapped aliases, unresolved identities and unchanged vendor/vendor pairs.

The checker capability is advertised only with the guard complete; M2 is one PR.

## Verification

Each slice is one PR with watched red/green regressions. In-run: `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent` plus touched tests and `task test:codex-m4`; `task check:api` plus affected Go packages with `-race -count=1`; migration and sqlc checks; `docs:sync` + `check-docs:web`; `check:changelog-entry`; `task scan:secrets` with canaries. Update ADR-4320 D9 lane status, ADR-2149 checker custody, `docs/providers.md` (remove the #4323 interim note) and an AI-synced `specs/human.md` entry. Sweep changed reasons across `e2e/` and run `task test:e2e-lib` and `task test:forge-fake`.

Landing obligations, outside milestones: **full agent suite: exact-head PR CI** and the applicable API/repo CI checks. Hosted real-gateway acceptance is a linked maintainer issue, not a merge blocker.

## Decision Log

- **2026-11-21: Split from #4323.** Reason: one outcome per run, and #4322's per-lane slices, capabilities and same-model guard are the pattern to mirror. Rejected: bundling judge and checkers into the Codex issue-lane PRD.
- **2026-11-21: Same-model guard reused, not duplicated.** Reason: opposite engines no longer imply different models once a gateway maps names; #4322 owns the contract. Rejected: a Codex-specific guard or a new public reason.
