# library — syncing uzi's builtins from the upstream skills library

The 11 builtin roles in `api/internal/agenttmpl/builtins/` other than `lead.md`
(`architect`, `auditor`, `coder`, `documenter`, `fact-checker`, `researcher`,
`reviewer`, `spec-keeper`, `tester`, `ux-designer`, `web-ux`) are **byte-for-byte
copies** of the upstream role library's published files, `product-agents/<role>.md`
in `github.com/vtmocanu/skills`, at the commit `manifest.json` pins
(`upstream_sha`). `lead.md` is uzi-only and has no upstream counterpart. See
PRD #1849 and ADR-1849.

Nothing uzi-specific goes in those files:

- **uzi runtime rules** (the `.uzi/scratch/` directory, the gate-log and
  review-snapshot recipe, the safety rules) live in the worker's prompt append,
  `agent/src/prompt.ts`, which reaches every subagent, builtin or repo-authored.
- **uzi-only tools** (the fact-checker's `mcp__forge__*` read tools) live in
  `productToolDelta` (`../tool_delta.go`). They are applied to the embedded
  builtins at init and to an agent-source sync that overrides a builtin row.
- **Generic fixes** go upstream first (`roles.yaml` in the skills repo), then
  arrive here by the sync below. Upstream bodies must stay runtime-neutral;
  `../scratch_guidance_test.go` lists the phrases a builtin must not contain.

## What checks it

- `TestBuiltinLibraryDrift` (`../library_test.go`, part of `task gate:api`)
  compares each builtin's `version:` stamp with `manifest.json`: a stamp behind,
  ahead of, or missing from the manifest fails.
- `task nudge:builtins` (`scripts/builtin-parity.sh`, `api/cmd/builtinparity`)
  fetches `product-agents/` at `upstream_sha` and byte-compares every builtin.
  It never gates and needs network access.
- The weekly `roles-manifest-refresh` workflow does the sync below for you
  (#1851): it checks out the latest stable `vX.Y.Z` skills release and runs
  `scripts/refresh-role-manifest.sh`, which copies the builtin bodies, moves the
  versions and `upstream_sha`, syncs `.claude/agents/` with the library's
  `sync.py`, and byte-checks parity. It opens one PR (a draft when parity fails)
  with the builtins and the roster in separate commits, listing every body line
  the roster sync dropped. A role missing upstream or a version moving backward
  writes nothing and opens a tracking issue instead. `TestBuiltinLibraryDrift`
  compares numbers only, so the parity line in that PR (or `task
  nudge:builtins`) is what shows the bodies match.

## Sync procedure (what the bot does; by hand when needed)

1. Pick the upstream commit (normally a release tag's commit) and note its SHA.
2. Copy `product-agents/<role>.md` for the 11 roles into `builtins/`, unchanged.
3. Set `manifest.json`'s `upstream_sha`, `synced` and `roles` versions to match.
4. Run `task nudge:builtins` (expect "builtins match upstream") and
   `cd api && go test ./internal/agenttmpl/ ./internal/agentsource/`.
5. If a copied body now contradicts the worker (a new uzi-specific need), fix it
   upstream, not here, and add the uzi half to `agent/src/prompt.ts`.

The dev-team roster (`.claude/agents/`) follows the same release through the
library's `sync.py apply`, which keeps each file's `## For this repo` tail and
its `model:`. `scripts/role-sync-allowlist.tsv` lists the local differences the
bot expects and does not report (today: `tester` runs on `sonnet`); it matches
the exact `sync.py check` detail, so any other difference is still reported.

Upstream ships more roles than uzi builds in (`release`, `tui-ux`,
`skill-reviewer`). They are not builtins; an agent-source sync can still add them
as global templates. To ship a new upstream role, add its file here and its name
to `manifest.json`'s `roles`.
