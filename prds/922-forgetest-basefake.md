# PRD #922: shared forgetest.BaseFake for the six forge.Forge test fakes

**GitHub Issue**: [#922](https://github.com/vtmocanu/uzi/issues/922)
**Status**: Draft (created 2026-09-01)
**Priority**: Medium
**Parent**: epic #915 (Batch 1, P6)
**Related**:
- `api/internal/forge/forge.go` — the 25-method `Forge` interface.
- The six full implementers (3352 lines of stubs total): `handler/forge_test.go:19` (only UserExists meaningful, ~90 lines of no-op stubs), `seed/seed_test.go:17`, `poller/autopilot_test.go`, `poller/ci_autofix_test.go` (`cfForge`), `privcheck/checker_test.go:27`, `forgesvc/sync_test.go:21`. Consumers use tiny subsets (privcheck 4/25 methods, autopilot 3/25).
- NOT in scope: `workersvc/ci_fix_snapshot_test.go` — it **embeds the interface itself** (`forge.Forge`), so it inherits new methods already and is not one of the six (ARCHITECTURE.md documents this distinction).
- Line refs at `0fdec3791dad53d28f44193290f04a139e8a0719`; the interface-tax mechanism fact-checked at `f8e3116`.

## Problem

Adding one method to `forge.Forge` costs 9 edits today: 3 drivers + 6 hand-written fakes. The epic's architect judged the interface *width* fine (a read/write split would not reduce driver or fake count); the tax is the six independent fakes, five of which meaningfully implement almost nothing.

## Solution

A new test-support package `api/internal/forge/forgetest` exporting `BaseFake`: implements all 25 methods with safe defaults, designed for embedding; each test fake embeds it and overrides only the methods it actually exercises. Interface change cost becomes 3 drivers + 1 BaseFake.

Default semantics (the load-bearing design choice, see D2): methods a test did not override must **fail loudly, not succeed silently** — return a sentinel error (`forgetest.ErrNotStubbed` wrapping the method name), except where the codebase has an established "absence" sentinel (pipeline reads return `forge.ErrNoPipeline`-equivalent, matching what consumers branch on).

## Milestones

- [ ] **M1 — forgetest.BaseFake + its own test.** New package with `BaseFake` (all 25 methods; `ErrNotStubbed` defaults; documented absence-sentinel exceptions). A compile-time `var _ forge.Forge = (*BaseFake)(nil)` assertion. One test proving the default behavior (an unstubbed call returns ErrNotStubbed naming the method — the loud-failure property is the contract). `task gate:api` green (deadcode gate: the package is imported only from tests, which `deadcode -test` covers — verify `task deadcode:api` stays green before assuming).
- [ ] **M2 — migrate the six fakes.** One commit per package (handler, seed, poller ×2, privcheck, forgesvc): embed `forgetest.BaseFake`, delete the no-op stubs, keep each fake's meaningful methods verbatim. Existing tests must stay green **unmodified** — if a test starts failing with ErrNotStubbed, that test was silently depending on a no-op stub's zero return; STOP and replicate the old stub's return explicitly in that fake (visible now, which is the improvement), noting it in the MR. `task gate:api` green.
- [ ] **M3 — fix the docs that enumerate the six.** ARCHITECTURE.md's forge-layer section and root CLAUDE.md's Architecture bullet both enumerate the six fakes and their history (the "this said five until 2026-08-19" parenthetical); update them to describe the BaseFake pattern (interface change = drivers + BaseFake) while preserving the correction history per house style. Also re-verify the embedder note (`workersvc/ci_fix_snapshot_test.go`) still holds and say so with the date.

## Success criteria

1. Interface-change tax measurably reduced: adding a dummy method to `forge.Forge` locally (throwaway, not committed) breaks compilation in exactly 4 places (3 drivers + BaseFake), none of the six test files. That experiment is the PRD's acceptance proof; record its output in the MR.
2. Zero behavior change: no existing test modified (except explicit ErrNotStubbed replications, each listed).
3. Stub-line count in the six files drops by roughly the 3000-line order; `task gate:api` green; docs updated (M3).
4. No `.github/workflows/**` in the branch diff.

## Decision Log

- **D1 — shared fake over consumer-narrowed interfaces.** Narrow per-consumer interfaces (privcheck taking a 4-method interface) is more idiomatic Go but is M/L effort across production signatures and changes non-test code; the epic scoped this PRD test-only. Narrowing remains open for the deferred workersvc-decomposition epic.
- **D2 — unstubbed methods fail loudly (ErrNotStubbed), not silently succeed.** A silent zero-value default is exactly how a fake hides a code path change (the epic's evidence-discipline theme: an instrument that cannot produce the disconfirming answer). The absence-sentinel exceptions exist because some consumers legitimately branch on "no pipeline" as a non-error.
- **D3 — package lives under forge/forgetest.** Follows the stdlib convention (httptest under net/http); depguard's forge-sdk-isolation rule is unaffected (forgetest imports forge's types, no raw SDK).
- **D4 — one commit per package in M2.** Keeps each migration reviewable against its own test file's diff and bisectable if a hidden stub-dependency surfaces late.

## Risks & mitigations

- **A test silently depended on a no-op stub's zero return.** By design that now fails loudly with the method name (D2); the fix is an explicit stub, disclosed in the MR. This is the PRD's only real behavioral risk and it manifests as a red test, never a silent pass.
- **Interface drift during flight.** If `forge.Forge` gains a method while this PRD is in a sweep run, BaseFake fails to compile — loud, and the fix is one method. The compile-time assertion in M1 guarantees the loudness.
- **Docs claim drift (M3).** The ARCHITECTURE.md/CLAUDE.md sentences being edited have their own correction-history style; preserve the dated parentheticals rather than flattening them (house rule: past-tense records stay).
