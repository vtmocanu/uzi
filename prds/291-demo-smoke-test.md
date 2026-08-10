# PRD #291: uzi pipeline smoke-test demo (3 tiny milestones)

**GitLab Issue**: [#291](https://gitlab.example.com/vtmocanu/uzi/-/issues/291)
**Status**: Draft (created 2026-08-10)
**Priority**: Low

## Problem

There is no fast, throwaway way to exercise uzi end to end (plan -> approval gate -> implement <-> review -> branch + MR, never touching `main`) on demand. Real PRDs are large; we want a deliberately trivial one whose only job is to prove the factory works and to shake out any pipeline regressions cheaply.

## Solution

Three deliberately trivial, **independent** milestones, each ~2 minutes to implement and each landing in a **different toolchain** so a single run exercises all three of uzi's gates (`web`, `agent`, `api`). Each change is self-contained, in its own directory, with its own passing test/check. Nothing here is intended to survive long: it is a smoke test, easy to revert.

The demo is intentionally low-risk. It adds no product behaviour, no routes, no config, and touches no shared files, so the three milestones can be worked in parallel and cannot conflict.

## Milestones

- [ ] **M1 — Demo user doc (web docs gate).** Add `docs/demo.md` with valid leading-fence frontmatter (`title`, a unique `order`, `audience: user`) and a short paragraph explaining this is a uzi smoke-test page. Because `audience: user`, it renders in-app at `/docs/demo`. Validated by `web/scripts/check-docs.mjs` (runs inside `npm run build`): frontmatter valid, `order` not duplicated, no broken relative links. No code. See `docs/README.md`.

- [ ] **M2 — Agent demo helper + unit test (agent gate).** Add a single pure function to the agent worker (e.g. `agent/src/demo.ts` exporting `demoBanner(): string` returning a fixed literal) plus a `node --test` test asserting its output. Keep the export **consumed** so knip's `error`-tier "unused files" check stays green (the test file importing it as an entry is sufficient; if knip still flags it, re-export from an existing barrel). Green via `task gate:agent` (follow `.claude/rules/agent.md`, incl. `--test-timeout=120000`).

- [ ] **M3 — API demo helper + table test (api gate).** Add one exported pure Go function (e.g. `Greeting(name string) string` in a small package under `api/internal/`) plus a table test that calls it. The test-as-root keeps `deadcode -test ./...` green (a function reached only from its test is reachable), and an **exported** symbol is not reported by golangci `unused`. Green via `task gate:api` (runs `-race`, `-count=1`, the lint ratchet, and deadcode — see repo `CLAUDE.md` and `.claude/rules/go.md`).

## Success criteria

1. `task gate` is green on the branch (all of `gate:repo`, `gate:web`, `gate:agent`, `gate:api`).
2. Each milestone is an independent, self-contained change in its own directory; no shared files touched, no product behaviour changed.
3. uzi drives the issue to an open MR against a feature branch, `main` never touched.
4. The whole thing is trivially revertable (delete the three added files + any barrel re-export).

## Risks / notes

- **Gate landmines are the point of the test, not accidents.** The two code milestones deliberately sit next to uzi's documented gate traps (deadcode zero-baseline, knip unused-files `error` tier, golangci lint ratchet). A competent worker wires the helpers so their tests act as reachability roots; if a gate reddens, that is a real signal about the pipeline, which is what a smoke test is for.
- **Parallelizable.** M1/M2/M3 touch disjoint files (`docs/`, `agent/src/`, `api/internal/`), so they have no ordering dependency.
- **Disposable.** Not meant to ship as a feature; revert after the smoke test if desired.

## Execution plan (parallelism)

| Phase | Milestone | Depends on | Files | Toolchain/gate |
|---|---|---|---|---|
| 1 (parallel) | M1 | none | `docs/demo.md` | web / `check-docs.mjs` |
| 1 (parallel) | M2 | none | `agent/src/demo.ts` (+ test) | agent / `task gate:agent` |
| 1 (parallel) | M3 | none | `api/internal/<pkg>/*.go` (+ test) | api / `task gate:api` |

All three milestones are in Phase 1 (fully parallel); there is no Phase 2.
