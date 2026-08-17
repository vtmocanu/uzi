# PRD #306: Demo — uzi self-test doc

**Issue**: [#306](https://github.com/vtmocanu/uzi/-/issues/306)
**Priority**: Low
**Status**: Draft

> This is a **throwaway demo** whose only purpose is to exercise the uzi factory end to end (clone → plan → implement ⇄ review → branch + MR). The resulting MR is meant to be **closed, not merged**, and this issue closed afterward. Keep every change trivial, additive, and gate-safe.

## Problem

We want a fast, low-risk way to confirm uzi can take a `PRD`-labeled issue all the way to a merge request, without risking any real functionality or tripping the repo's quality gates.

## Solution

Add a single new root-level file, `DEMO.md`, built up over three tiny milestones. It is plain prose Markdown at the repo root, so it is outside every gated path: not under `docs/` (no `check-docs.mjs` frontmatter rules), not a `.sh`/`.yml`/Homebrew-formula file (no `gate:repo` checks), and carries no secret-shaped content (no `scan:secrets` hit). No Go, no web, no agent code changes.

## Success Criteria

1. `DEMO.md` exists at the repo root on the MR branch and reads as a coherent short document.
2. The change is purely additive — no existing file is modified.
3. `task gate:repo` stays green (the only gate a root Markdown file can touch).
4. uzi opens an MR against a non-`main` branch, demonstrating the full run lifecycle.

## Milestones

Each milestone is ~2 minutes of work and additive to the same new file, so they run in sequence within a single uzi worker run.

- [ ] **M1 — Create `DEMO.md` with title + intro.** Add a new root-level `DEMO.md` containing an H1 title (`# uzi self-test`) and a one-paragraph intro stating this file is a smoke-test artifact produced by a uzi demo run and is safe to delete.
- [ ] **M2 — Add a "What this exercises" section.** Append an H2 `## What this exercises` with a short bullet list naming the run lifecycle steps this demo proves: clone the repo, plan from the PRD, implement the milestones, open an MR (never touching `main`).
- [ ] **M3 — Add a "Verification" section.** Append an H2 `## Verification` with 2–3 bullets describing how to confirm the demo worked: `DEMO.md` is present on the MR branch, the MR was opened against a non-`main` branch, and the repo gate stays green.

## Technical Scope

- **Files touched**: `DEMO.md` (new, repo root) only.
- **No** changes to Go modules, `web/`, `agent/`, `controller/`, `docs/`, `Taskfile.yml`, CI, or any config.
- **No** new dependencies, migrations, or API/CLI surface changes.

## Risks & Mitigations

- **Risk**: an agent over-implements and touches gated code. **Mitigation**: the milestones and Technical Scope explicitly bound the change to one new root Markdown file.
- **Risk**: accidental placement under `docs/` triggers `check-docs.mjs` frontmatter validation. **Mitigation**: the file lives at the repo root by design; the PRD says so in three places.

## Out of Scope

Anything beyond `DEMO.md`. This PRD intentionally does not add tests, docs pages, or product behavior — it is a factory self-test, not a feature.
