# PRD #921: workersvc/service.go file split + settings boolSetting helper

**GitHub Issue**: [#921](https://github.com/vtmocanu/uzi/issues/921)
**Status**: Draft (created 2026-09-01)
**Priority**: Medium
**Parent**: epic #915 (Batch 1, P5)
**Related**:
- `api/internal/workersvc/service.go` — 6485 lines; `*Service` has 178 methods package-wide (86 in service.go). The package is already decomposed into 40+ sibling files, so same-package moves are the established pattern.
- `api/internal/settings/settings.go` — 11 bool accessors sharing one 3-state switch body; `intSetting` exists at :1279, `boolSetting` does not.
- Maintainer decision (epic #915, 2026-09-01): **file split only this epic; the package split (judgesvc/chatsvc) is deferred** to a later epic after the 178-method god-object is decomposed.
- Line refs at `0fdec3791dad53d28f44193290f04a139e8a0719`, fact-checked at `f8e3116`.

## Problem

`service.go` is the largest hand-written file in the repo and holds ~12 distinguishable responsibilities in one file; navigation, review, and merge conflicts all pay for it. The epic's review identified five clean cohesion seams whose sibling files already establish the target shape. Separately, `settings.go` re-implements an identical 3-state bool switch 11 times while the int equivalent already has a helper — the asymmetry is the tell.

## Solution

Pure same-package code motion along the five seams, plus one tiny helper in settings. **No exported-API change, no package split, no behavior change.**

Seams (approximate service.go ranges at the pinned SHA):
1. pgtype param helpers (~:6310-6482, no receiver) → `pgparams.go` — easiest, do first.
2. Sweeper: `Sweep`/`resumePoolWaitRuns`/`publishSwept`/`hasLivePoller` (~:5969-6299) → `sweep.go`.
3. Input submission/approval: `SubmitInput`…`submitScopeCeiling` (~:5359-5969) → `submit.go`.
4. Message ingest + usage fold: `AppendMessages`/`appendMessages`/`foldRunUsage` (~:2760-3390) → `usage_fold.go`.
5. Credential/secret selection (~:1512-1953) → `secretchoice.go`; claim assembly (`assembleClaim` cluster, ~:1953-2760) → into the existing `claim.go` or a new `claim_assembly.go`.

## Milestones

- [ ] **M1 — seams 1+2 (pgtype helpers, sweeper).** Move-only commits: function bodies byte-identical (verify with `git diff --color-moved=dimmed-zebra` showing pure moves; any non-move edit in a moved function is a red flag). The approve-freeze log line `workersvc: approve-time milestone freeze` and its callers move untouched — external docs grep for that exact string (`.claude/rules/go.md`'s approve-freeze entry), so its text is load-bearing. Full suite proof: `task gate:api` green AND the live-DB sweep `./e2e/run-store-it.sh` with its positive control (named tests `--- PASS`, `RUN>0`, `SKIP=0`) since workersvc is store-heavy.
- [ ] **M2 — seams 3+4+5.** Same discipline, three commits (one per seam) so review stays per-theme. `task gate:api` + live-DB sweep green again.
- [ ] **M3 — settings.boolSetting.** Add `boolSetting(ctx, key) (bool, error)` using `Defaults[key] == "true"`, propagating the read error (all 11 accessors share the same switch body including the error path — fact-checked; `MrReworkEnabled`'s deliberate error propagation is the same shape, not an exception). Migrate the 11: SlackEnabled, JudgeEnabled, GithubProjectSyncEnabled, EphemeralWorkersEnabled, MrReworkEnabled, JudgeEnforceAll, AgentSourceEnabled, ReleaseCheckEnabled, ReleaseCheckBannerEnabled, HealthEnabled, CapabilityAwareScheduling. Do NOT touch Branding's inner closure or AgentSourceCredentialConfigured (different shapes, correctly excluded). Existing settings tests green unmodified. `task gate:api` green.

## Success criteria

1. `service.go` under ~2500 lines, every moved function byte-identical, zero exported-API diff (`go doc ./internal/workersvc` output unchanged modulo source locations).
2. Full `task gate:api` AND `./e2e/run-store-it.sh` green with positive controls.
3. `git grep -c 'case "true"' -- api/internal/settings/settings.go` drops to the helper (plus Branding's closure).
4. No `.github/workflows/**` in the branch diff.

## Decision Log

- **D1 — file split only, package split deferred** (maintainer decision on the epic). A package split would need the `*Service` god-object decomposed first and touches import paths across the module; the file split delivers most of the navigability win at near-zero risk.
- **D2 — move-only commits, one seam per commit.** The reviewability of "pure code motion" is the entire safety argument; mixing a rename or cleanup into a move destroys it. Any improvement noticed during the move gets filed, not folded in.
- **D3 — boolSetting rides in this PRD** rather than its own: same module, S-effort, file-disjoint from every other Batch 1 PRD, and it mirrors the intSetting precedent exactly.

## Risks & mitigations

- **Ratchet surfacing (`whole-files: true`) — the BIG one here.** service.go is one of the two files the epic baseline names as carrying the real pre-existing G115 int-overflow gosec findings, and the moved code carries them into new files where `new-from-merge-base` + `whole-files` will gate them. Expect `task lint:api` to redden on the moved code. Handle deliberately: fix the G115s that are trivially fixable (checked conversions) in a SEPARATE commit clearly labeled as behavior-checked fixes, and `//nolint:gosec // <why>` the rest with justifications; never mix those edits into the move commits (D2).
- **Cross-module fixture cache trap.** workersvc reads repo-root fixtures (`fixtures/judge-fidelity/`, `fixtures/run-usage/`); `-count=1` is already on the gate and the live-DB sweep — do not "optimize" it away while validating.
- **A moved function's doc comment cites a line number.** Sweep moved code for intra-file line references and fix them as part of the move commit (a stale self-reference is a wrong doc).
