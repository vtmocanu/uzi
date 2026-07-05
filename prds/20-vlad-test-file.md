# PRD #20: vlad-test.txt smoke-test file

**GitLab Issue**: [vtmocanu/uzi#20](https://gitlab.example.com/vtmocanu/uzi/-/issues/20)
**Status**: Draft
**Priority**: Low
**Created**: 2026-07-05
**Depends on**: nothing (standalone smoke test of the run pipeline)

## Problem

Issue #20 is a bot-created (`uzi-bot-vmocanu`) `PRD`-labeled test issue used to exercise the uzi factory end to end: "create a test file called vlad-test.txt and write dark factory in it". There is no product problem; the value is verifying the plan → approval → implement → MR pipeline on a minimal, unambiguous change.

## Solution Overview

Add a single file `vlad-test.txt` at the repository root containing the text `dark factory`, delivered via the standard guardrailed flow: work branch + MR, never touching `main` directly.

## Scope

- **In**: one new file `vlad-test.txt` (content: `dark factory`, trailing newline), branch + MR referencing issue #20.
- **Out**: everything else — no code, docs, tests, or config changes. The file is a throwaway marker and may be deleted in a later cleanup once the pipeline test is confirmed.

## Milestones

- [ ] **M1: File created on a work branch** — `vlad-test.txt` exists at repo root with content `dark factory`; branch pushed; MR opened against `main` referencing issue #20.
- [ ] **M2: MR merged and issue closed** — MR reviewed/merged via the normal flow; issue #20 closed; PRD moved to `prds/done/`.

No parallelization: two sequential milestones, one file.

## Success Criteria

- `git show origin/main:vlad-test.txt` prints `dark factory` after merge.
- `main` was only ever modified through the MR merge (guardrail layers untouched).

## Decision Log

- 2026-07-05: File placed at repo root (issue gives no path; root is the simplest verifiable location).
- 2026-07-05: Content is exactly `dark factory` plus trailing newline.
