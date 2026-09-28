# ADR-1849: builtin agent files are verbatim upstream copies; uzi's own rules live in the worker append

**Status**: Accepted (issue #1849)
**Date**: 2026-09-28
**Deciders**: the maintainer, with a Codex peer review of the PRD and of the upstream change.
**Related**: PRD #1849 (`prds/done/1849-builtin-agents-mirror-upstream.md`, decision log D1-D7); [ADR-1719](1719-run-scratch-dir.md) (the run scratch directory whose rules move to the append); [ADR-0602](0602-agent-source-repo-sync.md) (agent-source sync, unchanged in design).

## Decision (summary)

The eleven builtin subagent files in `api/internal/agenttmpl/builtins/` (every file except `lead.md`) are byte-for-byte copies of the upstream role library's published `product-agents/<role>.md` at the commit `library/manifest.json` pins. uzi keeps three things out of those files, each in one home:

- **uzi runtime rules** (the `.uzi/scratch/` directory and `scratch=.uzi/scratch`, the gate-log and review-snapshot recipe, "no detached checkout here", "never run Git inside an export", the safety rules): `agent/src/prompt.ts`, the append every subagent gets on both harnesses, builtin or repo-authored.
- **uzi-only tools** (the fact-checker's `mcp__forge__*` read tools): `productToolDelta` in `api/internal/agenttmpl/tool_delta.go`, applied by `WithProductTools` to the embedded builtins at init and to an agent-source sync that overrides a builtin row. Keyed on the builtin row, never on a repo agent's name. An inherit-all tools list is left alone.
- **generic fixes**: upstream first (`roles.yaml`), then copied here.

## Context

The builtins started as copies of the upstream library and were then edited in place, so at equal `version:` stamps ten of eleven bodies differed from upstream, uzi's generic fixes never reached the library, and an agent-source sync of the library replaced the builtins with bodies that lacked uzi's runtime rules and stripped the fact-checker's forge tools. The root cause was one file carrying two kinds of content: generic role guidance and facts that hold only on a uzi worker.

## Consequences

- A sync is a copy plus a manifest update; `task nudge:builtins` byte-compares against the pinned commit and never gates. `TestBuiltinLibraryDrift` still gates the version stamps.
- Upstream bodies must stay runtime-neutral. `staleScratchGuidance` in `scratch_guidance_test.go` names the phrases a builtin may not contain; the skills repo's `CLAUDE.md` records the same constraint.
- A change that needs a uzi-specific sentence in a role goes in the append (all roles) or is proposed upstream in runtime-neutral form (one role). Editing a builtin file here reddens nothing but is reported by the nudge and is lost on the next sync.
- `lead.md` stays uzi-only and outside all of this.
