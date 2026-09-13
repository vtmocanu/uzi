# PRD #1317: Freeze `specs/ai.md`; AI design decisions live in PRD Decision Logs and ADRs

**Status:** Done (2026-09-13). **Issue:** #1317. **Library:** vtmocanu/skills #60 and #61 (spec-keeper v4 to v6).

## Problem

`specs/ai.md` was an append-numbered log of AI design decisions: 637 sections, 24,267 lines. Two parallel PRs claimed the same next number on every close landing (PR #1310 vs #1184 both took 635), and the uniqueness gate only proved the collision after the fact.

## Findings (measured on main adfdd15b)

- Every one of the 127 PRDs anchored in an `ai.md` heading has its PRD file in `prds/` or `prds/done/`: the rationale existed twice.
- Append-only in practice: 89 commits since the public import, none deleting more than 20 lines. Nothing was superseded in place.
- Incomplete anyway: 70 of 263 done PRDs are not mentioned in it.
- Nothing reads it at run time. The lead builtin greps `specs/human.md`; the spec-keeper subagent writes it and reads it only to append.
- Dropping the sequential number (issue option A) does not remove the git conflict: two number-free tail appends still conflict (measured). Only removing the tail does.

## Decision

Freeze the file. `specs/ai.md` stays at its path so every `§N` reference keeps resolving, gains a `FROZEN` banner, and is never appended, renumbered or edited again. The rebuild-from-specs contract is `specs/human.md` (requirements) plus `adr/` (decisions that outlive the work) plus the PRD Decision Logs (`prds/done/`).

## Milestones

- [x] **M1 Library.** spec-keeper v6 in vtmocanu/skills: never creates a `specs/ai.md`; records decisions in ADRs and PRD Decision Logs; honors a `FROZEN` marker on an existing file (legacy repos keep theirs until frozen). Regenerated `product-agents/`.
- [x] **M2 uzi agents.** Builtin and dev-roster spec-keeper at v6, vendored manifest `spec-keeper: 6` at the library merge SHA, `architect` and `release` tails, `.claude/agent-team.md`.
- [x] **M3 Freeze gate.** `scripts/check-spec-numbering.sh` asserts unique numbers, none above 637, count 637; the canary plants a duplicate and a 638 so both arms prove live.
- [x] **M4 Docs.** `AGENTS.md` specs contract, README, the uzi-watcher and uzi-release skills (collision recipes retired), the `ai.md` banner, superseded notes on ten unfinished PRD milestones and two plan bullets that reserved a section number, CHANGELOG.

## Decision Log

- **D1 Freeze, not rewrite.** Options A (drop numbers) and C (split files) keep a fourth copy of the rationale growing; B (terse rewrite) is a big-bang risk with no trigger. Freezing costs no migration and keeps 946 in-file and about 800 external `§N` references valid.
- **D2 The freeze signal is in the file, not in `AGENTS.md`.** A repo's `CLAUDE.md` reaches only the lead, opt-in and advisory (`agent/src/executor.ts`, PRD #246); the spec-keeper subagent never sees it. Its workflow reads `ai.md` on every dispatch, so a `FROZEN` marker at the head is the channel that reaches a tail-free product subagent.
- **D3 Library default changes for every consumer.** The measurements are properties of the two-file design, not of uzi, so v6 stops creating `ai.md` for fresh repos. Legacy repos keep an unfrozen `ai.md` as their record until they freeze it; nothing is edited in other repos.
- **D4 Gate shape.** Uniqueness plus max plus count, no stored manifest of section numbers: today the numbers are exactly 1 to 637, so count equals head, and each arm catches one edit class (renumber, append, delete).
- **D5 No retroactive Decision Log gate.** Requiring a `## Decision Log` section in `prds/done/` would fail most existing PRDs and is a contract change; deferred, nudge at most.
- **D6 Superseded sections stay unmarked.** A per-section pointer would edit the frozen file. The banner supersedes the append-at-tail convention block explicitly.
- **D7 In-flight runs.** A run claimed on the v4 template may still append a section; the freeze gate reds its PR and the section moves into that PRD's Decision Log during rework. The dev-cluster follows the library at a pinned tag, so cut a skills release and re-pin the agent source after this lands.
