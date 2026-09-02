# PRD #963: forgesvc/projectsync.go file split — provision/seed, forward+reverse sync, visibility/share

**GitHub Issue**: [#963](https://github.com/vtmocanu/uzi/issues/963)
**Status**: Done (created 2026-09-01)
**Priority**: Medium
**Parent**: epic #915 (Batch 2, P13; finding A8). Decided 2026-09-01 evening. Same recipe as #921 (`prds/done/921-workersvc-file-split.md`, PR #955): same-package, move-only, one seam per commit.
**Line refs**: at `01be854` (current main). Implementer re-derives at their base; anchors are identifiers, not offsets.
**In-flight overlap**: #954 (running) adds a test pinning `forgesvc.ensureProjectScope` — a `_test.go` in this package, which this PRD never touches, and `ensureProjectScope` stays in `projectsync.go`; #956 (tonight) leaves `forgesvc` untouched by design (its D5); #960 is `web/` only.

## Problem

`api/internal/forgesvc/projectsync.go` is 2098 lines and 37 functions/methods (51 top-level declarations with the types, consts and vars), the last four-digit Go file the epic named; every other file in the package is under 700 lines (`service.go` 656, `pipeline_sync.go` 266). It holds four things that share only the `*ProjectSyncService` receiver and the two entry helpers `projectSyncResolve` (`:266`) / `projectSyncPreamble` (`:301`):

1. **Provision / adopt / seed / resync** — `Adopt` `:220`, `Provision` `:332`, `provisionColors`+`provisionColor` `:375-389`, `doneColumnName`/`doneColumnColor` `:390-397`, `appendDoneOption` `:405`, `provisionPrepare` `:420`, `seedParams` `:527`, `seed` `:547`, `launchSeed` `:567`, `prepareSeedLink` `:621`, `adoptPrepare` `:708`, `Resync` `:747`, `resyncPrepare` `:794`, `AutoCreateColumns` `:849`, `seedItems` `:963`, `Disable` `:1070-1122`, plus `unmatchedNote` `:2076` (called only from `adoptPrepare` `:736` and `resyncPrepare` `:807`). ≈ 850 lines.
2. **Visibility / sharing** — `GetVisibility` `:1123`, `RepoOwnerType` `:1145`, `SetVisibility` `:1165`, `ShareWithUser` `:1188`, `Unshare` `:1211-1242`. ≈ 120 lines.
3. **Forward + reverse board sync** — `ForwardMove` `:1243`, `addForwardItem` `:1382`, `ReverseSync` `:1423`, `reconcileItems` `:1584`, `backfillItem` `:1734`, `plannedMove` `:1790`, `reverseDiff` `:1813-1963`, plus the three helpers only this seam calls: `stampLinkError` `:2026` (callers `:1312,:1320,:1364` in `ForwardMove`; the `:2010` mention is its mirror's doc comment, not a call), `stampLinkErrorReverse` `:2012` (callers `:1535,:1718,:1722,:1919,:1931`), `markerValue` `:2038` (callers `:1349,:1626,:1664,:1838`). ≈ 760 lines.
4. **The type and its plumbing** — consts/vars `:37-94`, the four interfaces `:95-155`, `ProjectSyncService` `:156`, `NewProjectSync` `:187`, `SetMover` `:208`, resolve/preamble `:266-331`, `ProjectSyncStatus` type+method `:1964-2011`, `ensureProjectScope` `:2049`, and the two helpers more than one seam calls: `optionMarker` `:2068` (provision `:1046`, sync `:1328-:1951`) and `truncateErr` `:2087` (provision `:234,:356,:594,:766`, and both stamp helpers). ≈ 370 lines. (Caller map measured 2026-09-01 with `git grep -n -w <name> -- 'api/internal/forgesvc/*.go'`, comment mentions excluded.)

The ratchet exposure that dominated #921 is **nil here, measured**: with the ratchet cleared and both caps lifted (`--new-from-merge-base= --max-same-issues=0 --max-issues-per-linter=0`, golangci-lint v2.12.2 under `GOTOOLCHAIN=go1.26.6`), `projectsync.go` carries **0** findings; the package's single finding is a G115 in `pipeline_sync.go`, which this PRD does not touch.

## Solution

Pure same-package code motion along the three seams into three sibling files, leaving (4) in `projectsync.go`. **No exported-API change, no package split, no behavior change, no test edit.** Each seam is one move-only commit; declaration bodies and doc comments move byte-identical; the only new text is each file's package clause, imports, and a two-line header comment naming what the file holds. **That header goes BELOW the `package forgesvc` line, after a blank line, exactly as `projectsync.go:1-3` does it** — the package doc comment lives above the clause in `service.go`, and a comment placed immediately above `package forgesvc` in a new file becomes a second package doc comment that `go doc -all` concatenates, which would break success criterion 1. Helpers follow their callers: the four helpers that only one seam calls are listed under that seam above and move with it; the two that serve more than one seam (`optionMarker`, `truncateErr`) and the two entry helpers stay in `projectsync.go`. Any further deviation for cohesion is allowed if the commit message names it (the #921 precedent).

Target files:
- `projectsync.go` — (4) above, ≈ 370 lines.
- `projectsync_provision.go` — (1), ≈ 850 lines.
- `projectsync_sync.go` — (3), ≈ 760 lines (forward + reverse share the stamp helpers and the marker conventions, so they stay together; `plannedMove`/`reconcileItems` are reverse-only).
- `projectsync_share.go` — (2), ≈ 120 lines.

## Milestones

- [x] **M1 — baseline + the share seam.** Before moving anything, capture the exported-API baseline: `cd api && go doc -all ./internal/forgesvc > /tmp/forgesvc-doc-before.txt` (`go doc` orders by name, not file, so it is a file-layout-independent invariant). Then one commit moving (2) to `projectsync_share.go`. Verify: `gofmt -l` prints nothing; `git diff --color-moved=dimmed-zebra origin/main..HEAD -- api/internal/forgesvc/` shows only moved blocks plus the header/imports; `task gate:api` green.
- [x] **M2 — the sync seam.** One commit moving (3) to `projectsync_sync.go`, same discipline, including the three sync-only helpers (`stampLinkError`, `stampLinkErrorReverse`, `markerValue`) listed under (3). Re-derive the caller map before moving (`git grep -n -w <name> -- 'api/internal/forgesvc/*.go'`, ignoring comment lines); if a helper has gained a caller in another seam since `01be854`, leave it in `projectsync.go` and say so in the commit.
- [x] **M3 — the provision seam, doc-sync, and the full proof.** One commit moving (1) to `projectsync_provision.go` (including `unmatchedNote`). Then: `go doc -all` again and `diff` against the M1 baseline (must be empty); `task gate:api` green with `task lint:api` at `0 issues` (if the `whole-files` ratchet surfaces anything in a new file it was latent, fix or `//nolint` it in a **separate, labeled** commit, never inside a move commit — #921's G101 precedent). **The live-DB sweep is NOT the instrument for this PRD, measured:** `./e2e/run-store-it.sh` sweeps only `./internal/store/...` and `./internal/handler/...` (`e2e/run-store-it.sh:105-106`), and the package's two `*LiveDB` tests (`label_livedb_test.go`, `mr_rework_cancel_livedb_test.go`) reference `ProjectSyncService` zero times. The proof for the moved code is the three fake-store suites `task gate:api` already runs with `-race -count=1`: `projectsync_test.go` (2294 lines), `projectsync_reverse_test.go` (1434), `projectsync_convergence_test.go` (532) — confirm in the gate output that all three ran (`-v` names them; a package time under a second is the tell that nothing ran, `.claude/rules/go.md`). Doc-sync, fix-the-doc for present-tense claims: `specs/ai.md:22198` ("`forgesvc/projectsync.go`'s `ShareWithUser`/`Unshare`") and `:22204` ("Four service methods (`forgesvc/projectsync.go`): `GetVisibility`, `SetVisibility`, `ShareWithUser`, `Unshare`") → `projectsync_share.go`; `:22164` (`Adopt`/`Provision` route through `projectSyncPreamble` in `forgesvc/projectsync.go`) stays true because the preamble stays — verify, do not edit; `:22184` names `api/internal/forge/projectsync.go`, the driver, not this file. Sweep the moved code for intra-file references in comments ("above", "below", `:NNN`) and fix any that no longer hold. Tick this PRD's milestones and criteria on evidence, `git mv` it to `prds/done/`.

## Success criteria

1. `projectsync.go` ≈ 370 lines and the three new files carry exactly the members listed above (plus any cohesion deviation the commit names). Every moved declaration byte-identical: `git diff --color-moved=dimmed-zebra` shows no non-moved residue except package clauses, imports, and the file headers; `go doc -all ./internal/forgesvc` byte-identical before and after.
2. `task gate:api` green after each milestone; `task lint:api` reports `0 issues` at the end; the PR records that the three `projectsync*_test.go` suites ran (named in `-v` output, non-trivial package time).
3. `git diff --name-only origin/main..HEAD -- '*_test.go'` is empty; no test was edited, deleted, or added.
4. `specs/ai.md:22198` and `:22204` repointed; `:22164` verified still true; no other `projectsync.go` mention in `CLAUDE.md`, `ARCHITECTURE.md`, `docs/**`, `.claude/**`, `specs/**`, `adr/**` describes current code wrongly (measured 2026-09-01: those four `specs/ai.md` lines are the only `forgesvc/projectsync.go` hits in those directories; a bare `git grep projectsync.go` also returns hits under `api/internal/forge/`, which name the driver file `forge/projectsync.go`, not this one — skip them).
5. No `.github/workflows/**` in the branch diff.

## Decision Log

- **D1 — file split only.** The epic's standing rule for `workersvc` (file split this epic, package decomposition deferred) applies here for the same reason: a package split moves import paths across the module for a navigability win the file split already delivers at near-zero risk.
- **D2 — one seam per commit, move-only.** Reviewability of pure motion is the entire safety argument; a rename, a cleanup, or a "while I'm here" folded into a move destroys it. Anything noticed during the move is filed as an issue, not fixed.
- **D3 — helpers follow their callers; only genuinely shared ones stay.** Measured caller map at `01be854`: `projectSyncResolve` (Adopt, preamble, Disable, GetVisibility) and `projectSyncPreamble` (every seam) are entry helpers and stay; `optionMarker` (provision `seedItems` + the sync seam) and `truncateErr` (provision + both stamp helpers) are shared and stay; `stampLinkError`, `stampLinkErrorReverse`, `markerValue` are called only from the sync seam and `unmatchedNote` only from the provision seam, so they move with their callers. A first draft of this PRD called the whole cluster "shared" from its position at the bottom of the file; the grep said otherwise, which is why the map is written down.
- **D4 — forward and reverse sync share one file.** The epic listed them as one seam; `plannedMove` and `reconcileItems` are reverse-only, but forward and reverse share the two stamp helpers and the `markerValue`/`optionMarker` marker conventions, and 760 lines is under the epic's 1000-line threshold. Splitting them would put a type and its consumers on different sides of a file boundary for no gain.
- **D5 — smallest seam first.** M1's share seam is 120 lines: it proves the tooling (`color-moved`, `go doc` diff, the gate) on a diff a reviewer can read in full before the two large moves.

## Risks & mitigations

- **Ratchet surfacing (`whole-files: true`).** Nil by measurement (0 findings in `projectsync.go` today). If the linter nonetheless reports on a new file, it is latent code the ratchet had grandfathered; handle it in a separate labeled commit, never inside the move.
- **Comments that cite positions.** A moved doc comment saying "above"/"below" or citing a line is a wrong doc after the move — the M3 sweep exists for this (the #921 risk list carried the same item).
- **Import pruning.** `gofmt` does not remove unused imports; `go build` fails on them, so the per-file import list is settled by the compiler, not by hand.
- **Live-DB coverage.** None of the moved code is exercised by a `*LiveDB` test (measured above), so there is no positive-control sweep to run and none is claimed; the fake-store suites are the coverage, and `-count=1` on the gate keeps them from being served from cache.
- **#954's `ensureProjectScope` test.** If #954 merges before this runs, the base carries a new `_test.go` in the package. Same package, unchanged function, unchanged file for that function — nothing to reconcile.
- **Offline worker.** Every fact above is in-repo; nothing needs the open web.

## Outcome (implemented on `agent/issue-963`, base `297a4c2c`)

All three seams landed as pure move-only commits, each verified byte-identical and gate-green:

- **M1** `529de9a5` — share seam → `projectsync_share.go` (5 methods; 121 moved lines diff-clean).
- **M2** `6bfff1f3` — forward+reverse sync seam → `projectsync_sync.go` (10 decls incl. the three sync-only helpers; 764 moved lines diff-clean). Helper caller map re-derived: still sync-only.
- **M3** `ca75f34f` — provision seam → `projectsync_provision.go` (18 decls + `unmatchedNote`; 850 moved lines diff-clean). `unmatchedNote` still called only from `adoptPrepare`/`resyncPrepare`.

`projectsync.go` reduced from 2098 to 357 lines (the type, constructor, resolve/preamble, `ProjectSyncStatus`, `ensureProjectScope`, and the two shared helpers `optionMarker`/`truncateErr`). Proofs: `go doc -all ./internal/forgesvc` byte-identical before/after (success criterion 1); `task gate:api` green with `task lint:api` at `0 issues` (criterion 2); `git diff --name-only 297a4c2c..HEAD -- '*_test.go'` empty (criterion 3); `specs/ai.md:22198`/`:22204` repointed to `projectsync_share.go`, `:22164` verified still true (criterion 4); no `.github/workflows/**` in the diff (criterion 5). Doc-comment positional-reference sweep of the moved code: no stale `above`/`below`/`:NNN` refs.
