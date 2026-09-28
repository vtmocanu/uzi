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
- The weekly `roles-manifest-refresh` workflow runs
  `refresh-role-manifest.sh` and opens a manifest-bump PR when upstream versions
  move. That reddens `TestBuiltinLibraryDrift` until the builtins' `version:`
  stamps match. The test compares numbers only: bumping the stamps without
  copying the bodies turns it green, so run `task nudge:builtins` to confirm
  the bodies match.

## Sync procedure

1. Pick the upstream commit (normally a release tag's commit) and note its SHA.
2. Copy `product-agents/<role>.md` for the 11 roles into `builtins/`, unchanged.
3. Set `manifest.json`'s `upstream_sha`, `synced` and `roles` versions to match.
4. Run `task nudge:builtins` (expect "builtins match upstream") and
   `cd api && go test ./internal/agenttmpl/ ./internal/agentsource/`.
5. If a copied body now contradicts the worker (a new uzi-specific need), fix it
   upstream, not here, and add the uzi half to `agent/src/prompt.ts`.

Upstream ships more roles than uzi builds in (`release`, `tui-ux`,
`skill-reviewer`). They are not builtins; an agent-source sync can still add them
as global templates. To ship a new upstream role, add its file here and its name
to `manifest.json`'s `roles`.
