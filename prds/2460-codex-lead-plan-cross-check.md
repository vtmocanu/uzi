# PRD #2460: Plan cross-check for Codex-lead runs (Claude checker)

**Status**: Draft. Child 4 of 6 under umbrella #2148 (Cross-check). Split from PRD #2150 on 2026-10-07 (PRD #2150 D5). Blocked by PRD #2150 (both change the plan-gate code; automatic rounds land first). Naming follows PRD #2149 (D13, D14).

Resolved facts below were read at `main` `58cee732`.

## Problem

PRD #2149 cross-checks only a Claude lead's plan, on Codex. An opted-in user whose auto-approved run lands on the Codex harness gets no cross-check: the runner parks it with `plan cross-check: not yet supported for a Codex lead` (`agent/src/runner.ts`, gate reason `codex_lead_unsupported`), so every such run waits for a human. Lifting that park is not enough, because every layer below it assumes a Claude lead and a Codex checker:

- Submit requires a Claude lead (`api/internal/workersvc/cross_check.go`, the `lead.Harness != HarnessClaude` refusals in submit, retry and settlement).
- Child creation hardcodes the child harness as `codex` and filters `lead.harness = 'claude'` (`api/internal/store/queries/runtime.sql`, the `cross_check` INSERT).
- Claim assembly requires a Codex child with Codex credentials and no Anthropic token (`api/internal/workersvc/cross_check_claim.go`, `assemblePlanCrossCheckInput`).
- `CrossCheckRunner` has only a Codex path (`agent/src/cross-check-runner.ts`, `agent/src/codex/cross-check.ts`).
- The Codex executor refuses a checked approval (`agent/src/codex/codex-executor.ts`: `codex cannot consume checked plan approval`).

## Outcome

A Codex lead's plan is cross-checked by a read-only Claude child the same way a Claude lead's is checked on Codex: only an APPROVE of the exact current candidate implements, PRD #2150's automatic rounds apply, and every other outcome keeps PRD #2149's dispositions.

Acceptance examples:

1. Codex lead, auto-approved, user opted in with both families usable. The plan is cross-checked by a Claude child that has only `Read`, `Grep` and `Glob`, confined to its checkout; APPROVE implements on the Codex lead.
2. The Claude checker returns REVISE; the Codex lead revises automatically (PRD #2150) without tripping `codex plan revision budget exhausted`; round 2 passes and implements.
3. The Claude child tries to `Read` a path outside its checkout; the path guard refuses it.
4. The user's Claude credential is unusable at claim time; the child fails with `plan cross-check: checker unavailable` and the lead parks, never running a Codex checker for a Codex lead.
5. Plan cross-check × Claude is pinned to `sonnet` (PRD #2151, if shipped): the Claude checker runs on `sonnet`; with the cell at Default it follows the user's Claude worker default.

## Out of scope

- Code cross-check (PRD #2170).
- Dedicated slots (PRD #2169): the child claims through whatever lane exists when this ships.

## Modules and seams

- **Runner**: lift the `codex_lead_unsupported` park for a lead whose worker advertises the new capability below; keep it for an older worker.
- **Server**: submit, retry and settlement accept a Codex lead; child creation derives the child harness as the opposite family (`claude` for a `codex` lead, `codex` for a `claude` lead) instead of hardcoding `codex`, with a `CHECK` or guarded write that the child's harness differs from its lead's.
- **Claim assembly**: a Claude child receives only the owner's Anthropic credential and no Codex credential (single-family custody, the mirror of today's Codex-only rule); report-only and owner checks unchanged.
- **`CrossCheckRunner` Claude path**: the read-only option shape of `agent/src/chat-executor.ts`: `tools` (not `allowedTools`, which does not restrict under `bypassPermissions`) of `Read`, `Grep`, `Glob`; `disallowedTools` for everything else; a full-replacement `env`; `settingSources: []`; a temporary HOME; and `buildPathGuardHook` rooted at the child's checkout on `Read|Glob|Grep`, which is what makes read-only confinement true. No `Bash`, `WebFetch` or `WebSearch`. Stage-neutral, so PRD #2170 reuses it for the code stage.
- **Codex executor**: consume a checked approval of the exact acknowledged candidate (the handoff the SDK executor already does), and keep its local revise counter for human revises only, so PRD #2150's automatic rounds never reach `codex plan revision budget exhausted`.
- **Capability**: a new protocol capability (`cross_check_codex_lead_v1`) says a worker can run a Codex lead's checked gate and a Claude checker; claimability, health and provisioning mirrors require it for a Claude child, like `cross_check_v1` today.
- **Pins**: when PRD #2151 has shipped, claim assembly delivers the Plan cross-check × Claude cell and marks it active; otherwise the child uses the Claude worker default.
- **Isolation note**: `docs/worker-setup.md` (and PRD #2169's isolation section, if shipped) gains the Claude checker under the path guard.

## Testing decisions

- Opposite-family creation: a Codex lead creates a `claude` child, a Claude lead a `codex` child; a same-family child is refused by the guard.
- Single-family custody: a Claude child's claim carries the Anthropic credential and no Codex credential; a Codex child's the reverse; a mutation that leaks the other family's credential reddens a case.
- Claude checker isolation: asserted on the built SDK options and hook; a `Read` outside the checkout is refused.
- Exact-candidate handoff: the Codex executor implements only the acknowledged candidate's plan; a stale or different candidate is refused.
- Rounds: the Codex executor survives `PLAN_CROSS_CHECK_MAX_REVISIONS` automatic rounds without its budget error; a human revise after automatic rounds still has the full budget.
- Mixed fleet: a worker without `cross_check_codex_lead_v1` never claims a Claude child, and a Codex lead on such a worker keeps the `not yet supported` park.
- Mirrors of PRD #2149's tests for the reverse direction.

## Milestones

- [ ] **M1: A Codex lead's plan is cross-checked on Claude.** Server acceptance of Codex leads and opposite-family child creation, single-family claim assembly, the Claude `CrossCheckRunner` path with path-guard confinement, Codex-executor checked-approval handoff and round handling, the capability and its mirrors, lifting the runner park, pins delivery when PRD #2151 is present, docs (`docs/cross-check.md`, `docs/worker-setup.md`, then `task docs:sync`), `specs/human.md`, CHANGELOG. Blocked by: PRD #2150 M1. Gates: `task gate:agent`, `task gate:api`, LiveDB via `./e2e/run-store-it.sh`, `task gate:repo`.

No `.github/workflows/**` change in implementation or validation.

## Decision Log

- **D1. Claude cross-checker confinement reuses the chat executor's shape.** Moved from PRD #2150 D4. Under `bypassPermissions` an allowlist alone does not confine reads; the path-guard hook does.
- **D2. Split from PRD #2150.** User decision 2026-10-07, on buddy review: independently valuable from automatic rounds, and it needs server, claim and executor changes beyond lifting the runner park.
- **D3. Single-family credential custody.** A child holds only its own family's credential, mirroring PRD #2149's Codex-only rule, so a checker can never act as the lead's family.
- **D4. Implemented on the Claude harness.** User decision 2026-10-07: the work is a Claude SDK checker path.
