# PRD #1049: `web/src/pages/Judge.tsx` component extraction — the page's five render-only components into `pages/judge/`

**GitHub Issue**: [#1049](https://github.com/vtmocanu/uzi/issues/1049)
**Status**: Planned (created 2026-09-02)
**Priority**: Medium
**Parent**: epic #915 (P22; the first of the "web component extractions, one PRD each, the P14 (#1007) recipe" the epic's post-freeze plan lists, pulled forward to fill a free sweep slot). Prerequisite #1007 (P14, PR #1018) is merged: it established `pages/<page>/` directories (`pages/runView/`, `pages/board/`) beside `pages/adminSettings/` (#960). **File-disjoint by construction from everything in flight**: #1022 (`api/internal/handler/**`), #1034 (`web/src/{lib/api.ts,lib/boardOrder.ts,mocks/mockApi/boards.ts,pages/Board.tsx}` + `api/**`), #1042 (`api/internal/workersvc/**`, `agent/src/git.ts`) and #1048 (`controller/**`, `deploy/**`). This PRD touches `web/src/pages/Judge.tsx`, six new files under `web/src/pages/judge/`, and `specs/ai.md`.
**Line refs**: at `d6f39a0` (main, 2026-09-02 22:19 Bucharest). Implementer re-derives at their base; anchors are identifiers, not offsets.

## Problem

`web/src/pages/Judge.tsx` (1295 lines) is the page container `Judge()` (`:90-633`, 544 lines, exported; the file's **only** export) plus 640 lines of declarations the page merely renders, all unexported, all physically after it:

| Declaration | Lines (leading comment's first line .. the blank separator after the declaration, inclusive; a range ending on `}` has no separator) | Used by |
|---|---|---|
| `isBucket` | `:635-655` | `Judge()` only (one call, `:100`) — **stays** |
| `LabelFilter` | `:657-751` | `Judge()` (one render, `:499`) |
| `GroupRow` | `:753-835` | `Judge()` (×1); renders `GroupDisposeControls`, `OccurrenceRow` |
| `GroupDisposeControls` | `:836-919` | `GroupRow` only |
| `OccurrenceRow` | `:921-948` | `GroupRow` only; renders `OccurrenceVerdictBadge`, `OccurrenceBucketChip`, `OccurrenceFileIssue` |
| `OccurrenceVerdictBadge` | `:949-988` | `OccurrenceRow` only |
| `OccurrenceBucketChip` | `:989-1047` | `OccurrenceRow` only; calls `isHttpsUrl` |
| `isHttpsUrl` (local, URL-parsing; not `lib/api`'s prefix check, D5) | `:1049-1055` | `OccurrenceBucketChip` only |
| `MultiSelectBar` | `:1057-1150` | `Judge()` (×1); props are `count`/`onClear`/`onDispose` only, no shared type |
| `UndoToast` | `:1152-1185` | `Judge()` (×1); types on `Toast` |
| `ZeroState` | `:1187-1285` | `Judge()` (×1); renders `VerdictCount` |
| `VerdictCount` | `:1286-1295` | `ZeroState` only (×3) |

Plus two module-level types above the page: `Toast` (`:75-80`), read by `Judge()` (the toast state, `:140`, `:259`) and `UndoToast` (its prop), and `UndoMember` (`:73`, with its doc comment `:61-72`), which only `Toast`'s `undo` member references in code (its other two mentions, `:363` and `:1155`, are comments). `UNDO_CONCURRENCY` (`:82-88`) is `Judge()`-only and stays.

Measured (usage matrix over the file, 2026-09-02): **no moved declaration references anything `Judge()`-local**; the tail reaches only `lib/` and `components/` imports (`api`, `JudgeBacklog`/`JudgeOccurrence`/`JudgeRecommendationGroup`/`RecommendationCategory` types, `verdictTrend`/`rollupLabel`/`rollupTone`/`seenInRunsLabel`, `coordKey`/`JUDGE_CATEGORIES`/`recommendationLabel`, `stripUnsafeChars`, `judgeBadge`, `OccurrenceFileIssue`, the `ui` primitives and four icons, `useState`/`useRef`/`useEffect`, and `Link` from `react-router-dom`, which `Judge()` itself never uses — it is read only by `OccurrenceRow` (`:928,:933`) and `ZeroState` (`:1240,:1245`), so it leaves `Judge.tsx` entirely) and `Toast` (the page-level type; `UndoMember` is referenced only through it). `TriageSummary` (imported from `./RunView`), `useAuth`, `useAsyncData`, `errorMessage`, `useSetJudgeTodo`, `isCategory`, `useSearchParams`, `useCallback`, `useMemo` are `Judge()`-only and stay. Every judge-page change today diffs against a 1300-line file whose second half is five components.

Properties that make this a move rather than a design:

- **Importers and tests need no change.** The file exports only `Judge`; its consumers are `App.tsx:31` (`import { Judge } from "./pages/Judge"`) and three test files that import exactly `{ Judge }` (`pages/Judge.test.tsx:5`, `pages/JudgeNavBadge.test.tsx:6`, `components/OccurrenceFileIssue.test.tsx:19`). No test imports `Judge.tsx?raw` or reads it with `readFileSync` (`git grep -n -E 'readFileSync|\?raw' -- 'web/src/**/*.test.ts*' | grep -i judge` → only `mocks/judgeBacklogFidelity.test.ts`, which reads `fixtures/judge-fidelity/*`, not source). So, as in #1007, **zero `*.test.*` changes**, and unlike #1007 **zero re-exports** are needed.
- **knip stays green by construction.** `web/knip.jsonc` gates unused files, exports and types at `error`, with `ignoreExportsUsedInFile` for `interface`/`type` only. Five components gain `export` and each has a cross-file consumer (`Judge.tsx`); `Toast` gains `export type` and has two cross-file consumers (`Judge.tsx`, `UndoToast.tsx`); `UndoMember` stays unexported inside `shared.ts` (its only code reference is `Toast`'s member type, in the same file); every private helper stays private beside its sole consumer (D2/D4); every new file is imported by `Judge.tsx`.
- **No doc anchor moves.** `git grep -n -F 'Judge.tsx' -- specs docs ARCHITECTURE.md CLAUDE.md .claude web/src` minus tests: `web/src/lib/judge.ts:65` ("the way isBucket (Judge.tsx) guards ?bucket=" — `isBucket` stays, true), `web/src/lib/useAsyncData.ts:10,:137` (the `Judge()` generation-counter pattern — stays, true), `web/src/pages/RunView.tsx:64` ("Judge.tsx's TriageSummary" consumer — `Judge()` imports it, true), `specs/ai.md:19305` ("#235's `LabelFilter` (`Judge.tsx`) shipped chips with no counts" — past tense, a record of what #235 landed; leave). No `docs/*.md`, `ARCHITECTURE.md`, `CLAUDE.md` or `.claude/rules/*` names `Judge.tsx`; no present-tense prose outside `prds/done/**` names `LabelFilter`, `GroupRow`, `MultiSelectBar`, `UndoToast`, `ZeroState` or `OccurrenceRow` by location (`git grep -n -E '\b(LabelFilter|GroupRow|…)\b' -- specs docs ARCHITECTURE.md CLAUDE.md .claude/rules web/src/lib web/src/components` → 0 non-test hits).
- **No lint ratchet on the web side** (oxlint is unratcheted; knip is the only zero-tolerance instrument and it is covered above).

## Solution

Pure motion into `web/src/pages/judge/` (lower-camel, one word, the `board/`-beside-`Board.tsx` shape #1007 M3 established): five component files + one `shared.ts` for the page-level `Toast` type and its member type. Declarations move verbatim with their doc comments; the only new text is imports, the `export` keyword where a moved declaration gains its first cross-file consumer, and `export type` on `Toast`. **Importers stay byte-identical, no test file changes, no re-export from `Judge.tsx`** (nothing outside the page imports a moved symbol today). `Judge()`, `isBucket` and `UNDO_CONCURRENCY` stay where they are.

### Layout after the move

```
web/src/pages/Judge.tsx                  Judge() + isBucket + UNDO_CONCURRENCY (≈ 640 lines)
web/src/pages/judge/shared.ts            Toast (exported) + UndoMember (private, Toast's member type)
web/src/pages/judge/LabelFilter.tsx      LabelFilter (exported)
web/src/pages/judge/GroupRow.tsx         GroupRow (exported) + GroupDisposeControls, OccurrenceRow,
                                         OccurrenceVerdictBadge, OccurrenceBucketChip, isHttpsUrl (private)
web/src/pages/judge/MultiSelectBar.tsx   MultiSelectBar (exported)
web/src/pages/judge/UndoToast.tsx        UndoToast (exported)
web/src/pages/judge/ZeroState.tsx        ZeroState (exported) + VerdictCount (private)
```

No `index.ts` barrel (`adminSettings/`, `runView/`, `board/` have none; the page imports each file by relative path). No file under `judge/` may import from `../Judge` (no edge back to the page; measured unnecessary: the tail references nothing page-local).

**Out of scope**: `Judge()` internals (544 lines; any decomposition of the container is a later characterization-first PRD, the `Board()`/`Repos()` rule); folding the local `isHttpsUrl` into `lib/api`'s (D5); any file under `web/src/mocks/`, `web/src/lib/` or `web/src/components/`; `RunView.tsx` and `runView/JudgePanel.tsx` (the run-scoped judge panel, #1007's file).

## Milestones

- [x] **M1 — `judge/shared.ts`, then the two single-component files: `LabelFilter.tsx`, `MultiSelectBar.tsx`.**
  - Commit 1: `Judge.tsx:61-80` (the `UndoMember` doc comment `:61-72`, `type UndoMember` `:73`, `type Toast` `:75-80`; `:58-60` are the last import lines and a blank and stay) verbatim into `judge/shared.ts`; only `Toast` gains `export` (`UndoMember` has no cross-file code consumer — `Judge.tsx` names it only in comments at `:363`, and importing it would be a TS6133 under `noUnusedLocals`). `Judge.tsx` imports it with `import type { Toast } from "./judge/shared"` (type-only import, so `isolatedModules` is satisfied and nothing runtime is added). `UNDO_CONCURRENCY` (`:81-88`) stays in place.
  - Commit 2: `:657-751` into `judge/LabelFilter.tsx`, `LabelFilter` gaining `export`. Commit 3: `:1057-1150` into `judge/MultiSelectBar.tsx`, `MultiSelectBar` gaining `export`; it needs nothing from `./shared` (its props are `count`/`onClear`/`onDispose`).
  - `Judge.tsx` imports each component from `./judge/<File>`. Imports used only by the moved code leave with it; shared imports are duplicated into the new file. **Do not hand-curate the lists** — let `tsc` + `noUnusedLocals` produce the exact per-file import set (#960 M1, #1007 M1). Relative paths deepen by one level (`../lib/x` → `../../lib/x`, `../components/x` → `../../components/x`).
  - Verification after each commit: `task gate:web` green; `git diff --color-moved=dimmed-zebra origin/main..HEAD -- web/src/pages/Judge.tsx web/src/pages/judge/` shows each block as moved plus the scaffolding named above; `git diff --stat origin/main..HEAD -- 'web/src/**/*.test.ts' 'web/src/**/*.test.tsx' web/src/App.tsx` empty.
- [x] **M2 — The two cluster files: `GroupRow.tsx`, `ZeroState.tsx`; then `UndoToast.tsx`.**
  - Commit 4: `:753-1055` (`GroupRow` through `isHttpsUrl`) verbatim into `judge/GroupRow.tsx`; only `GroupRow` gains `export`. `GroupDisposeControls`, `OccurrenceRow`, `OccurrenceVerdictBadge`, `OccurrenceBucketChip` and `isHttpsUrl` **stay unexported** — their only callers are inside this file, and exporting any of them reddens knip's `exports: error` (`ignoreExportsUsedInFile` covers only `interface`/`type`; #1007 D4).
  - Commit 5: `:1187-1295` (`ZeroState` + `VerdictCount`) into `judge/ZeroState.tsx`; `ZeroState` gains `export`, `VerdictCount` stays private.
  - Commit 6: `:1152-1185` into `judge/UndoToast.tsx`; `UndoToast` gains `export`; it imports `type Toast` from `./shared`.
  - After this, `Judge.tsx` ends with `isBucket` (`:635-655` today) and is ≈ 640 lines. Sweep each moved block for positional comments (`above`, `below`, `this file`, `:NNN`) that the move makes false and fix them in the same commit, listed in the PR (the #963 recipe; `LabelFilter`'s header comment says "The Clear control is always mounted" and describes behaviour, not location — expected zero edits).
  - Verification as M1; additionally `cd web && VITE_UZI_MOCK=1 npm run build` succeeds (bundler resolution of the new directory; `task gate:web` excludes `vite build`, `.claude/rules/web.md`).
- [x] **M3 — Design record + final proofs.**
  - Append the `specs/ai.md` section (`## <N>. PRD #1049 — Judge page component extraction`, `Serves human:` first line, the decisions below one line each). Number = highest existing section + 1, re-derived at landing **across every worktree** (`git worktree list`); §604 is the highest at authoring, and #1022, #1034, #1042 and #1048 land around the same time, so expect to renumber at landing.
  - Final sweeps, read every hit: `git grep -n -E 'pages/Judge\.tsx|Judge\.tsx' -- CLAUDE.md ARCHITECTURE.md 'docs/*.md' .claude specs web/src` (the four sites named in Problem stay true; everything else past-tense or in `prds/done/**`); `git grep -n -F '?raw' -- web/src` unchanged from `origin/main`; `git grep -n -E 'from "\.\./Judge"' -- web/src/pages/judge` → 0.
  - PR description enumerates the non-moved residue per commit (the #955/#960/#1007 precedent): import lines in `Judge.tsx` and in each new file, the `export` keyword on `LabelFilter`/`GroupRow`/`MultiSelectBar`/`UndoToast`/`ZeroState`, `export type` on `Toast`, and any positional-comment fix.

## Success criteria

1. **Importers and tests untouched**: `git diff --stat origin/main..HEAD -- web/src/App.tsx 'web/src/**/*.test.ts' 'web/src/**/*.test.tsx'` is empty.
2. **Pure motion**: `git diff --color-moved=dimmed-zebra origin/main..HEAD -- web/src/pages/` shows each of the six blocks as moved; the non-moved residue is exactly what M3's list names.
3. `task gate:web` green after every commit (typecheck, oxlint, knip at zero, vitest); `cd web && VITE_UZI_MOCK=1 npm run build` succeeds; the `build-web` / `build-web-mock` CI jobs are green on the PR.
4. **Sizes**: `Judge.tsx` ≈ 640 lines; `judge/GroupRow.tsx` ≈ 310 (the largest new file); no other new file over 120.
5. **Exports are exactly the move's needs**: `git grep -n -E '^export (function|type|const) ' -- web/src/pages/judge/` lists exactly `LabelFilter`, `GroupRow`, `MultiSelectBar`, `UndoToast`, `ZeroState`, `Toast` (six; `UndoMember` stays unexported), and `git grep -n -E '^export ' -- web/src/pages/Judge.tsx` still lists exactly one line (`export function Judge()`); `git grep -n -E '^(export )?function (GroupDisposeControls|OccurrenceRow|OccurrenceVerdictBadge|OccurrenceBucketChip|isHttpsUrl|VerdictCount)\b' -- web/src/pages/judge/` shows none of the six exported.
6. **The seam is respected**: `^export function (Judge)\(` and `^function isBucket\(` and `^const UNDO_CONCURRENCY` resolve to `Judge.tsx`; each exported component resolves to its named file.
7. **No back-edge**: `git grep -n -E 'from "\.\./Judge"' -- web/src/pages/judge` → 0 hits; no `index.ts` under `web/src/pages/judge/`.
8. **Doc sweep**: every hit of the M3 greps describes current code truthfully.
9. `specs/ai.md` carries the new section at the tail, numbered above every sibling worktree's highest.
10. No `.github/workflows/**` in the branch diff (implementation or validation).

## Decision Log

- **D1 — no re-exports, because nothing needs one.** #1007 re-exported from the page file to keep `Judge.tsx:45`, `RunView.test.tsx` and `Board.test.tsx` byte-identical; here the only export is `Judge` and every importer imports exactly that, so the page file gains imports and loses declarations and nothing else. A re-export with no external consumer would itself be a knip finding.
- **D2 — one file per page-facing component; private helpers co-located with their sole consumer.** `GroupRow.tsx` carries its four render-only children and `isHttpsUrl`; `ZeroState.tsx` carries `VerdictCount`. The #960 D6 / #1007 D2 rule (no new export beyond what the move needs) wins over a one-declaration-per-file layout, which would have to export six symbols that are private today.
- **D3 — `judge/shared.ts` for `Toast` and its member type**, the `board/shared.ts` precedent (#1007 D3): `Toast` is read by the page (state) and by `UndoToast` (prop); defining it inside `UndoToast.tsx` would make the page import its own state type from one of its components, which reads as a dependency that does not exist. `UndoMember` rides along because `Toast` is defined in terms of it and its doc comment is the page's Undo contract; it stays unexported (no cross-file code consumer; D6-style rule: no export beyond the move's needs). `UNDO_CONCURRENCY` does not join them: it is `Judge()`-only.
- **D4 — `isBucket` stays in `Judge.tsx`** although it sits physically after `Judge()`: it is the page's own `?bucket=` validator, used nowhere else, and `lib/judge.ts:65` cites it "in Judge.tsx". Moving it would repoint a true comment for no reader benefit.
- **D5 — the local `isHttpsUrl` is not folded into `lib/api`'s.** They differ: `lib/api.ts:147` is a `startsWith("https://")` check tolerant of `null`/`undefined`; `Judge.tsx:1049` parses with `new URL` and compares the protocol (so `"https:foo"` differs, and a malformed string returns `false` rather than throwing). Same name, different predicate — exactly the shape `.claude/rules/web.md`'s copy-change rule warns about. It moves verbatim as a private helper; a dedup, if wanted, is a separate behaviour-reviewed change.
- **D6 — the directory is `judge/` beside `Judge.tsx`**, the `board/` beside `Board.tsx` shape. `import { Judge } from "./pages/Judge"` resolves the file (`Judge.tsx`) before any directory index, and there is no index, so a case-insensitive filesystem cannot confuse them (measured working for `Board.tsx` + `board/` since #1007).
- **D7 — no `RunView`/`runView/JudgePanel.tsx` involvement.** The run-scoped judge panel is a different component family (#1007 M2); the page-scoped components here never reference it, and `TriageSummary` (the one symbol the page borrows from `RunView`) stays in `Judge()`.

## Risks & mitigations

- **A `?raw` assertion somewhere targets `Judge.tsx` text.** Measured none: `git grep -n -F '?raw' -- web/src` has about nine sites (`RunView.tsx?raw`/`QuestionPanel.tsx?raw` in `RunView.test.tsx`, `WorkerUpgradeBadge.tsx?raw`, `rateLimits.ts?raw`, the `changelog.ts`/`docs.ts` loaders and three `lib` tests) and none names `Judge.tsx`. If one goes red, page text moved — restore it; never edit the test.
- **knip reports an exported symbol as unused.** Every export above has a measured cross-file consumer; if one is reported, the consumer's import is missing (fix the import), never a `knip.jsonc` ignore.
- **`tsc` import curation.** Duplicating a shared import into a new file is motion; forgetting one is a typecheck error, not a silent change. Let `noUnusedLocals` prune `Judge.tsx`'s now-unused imports (`Link`, `judgeBadge`, `OccurrenceFileIssue`, `verdictTrend`, `rollupLabel`, `rollupTone`, `seenInRunsLabel`, several `ui`/icon names are expected to leave; `useSearchParams` stays).
- **A positional comment inside a moved block goes stale.** Neither `--color-moved` nor the tests see it; sweep each block (M2) and list any fix in the PR.
- **Vite/vitest module resolution for the new directory.** Same mechanism as `board/` and `runView/`; the mock build in M2/SC3 is the proof, as it was for #1007.
- **`specs/ai.md` numbering collision** with #1022, #1034, #1042 and #1048 landing the same night: re-derive at landing (M3).
- **In-flight overlap: none.** Checked 2026-09-02 at `d6f39a0`: #1034 (running) edits `web/src/pages/Board.tsx`, `lib/api.ts`, `lib/boardOrder.ts`, `mocks/mockApi/boards.ts` — none of which this PRD touches (`Judge.tsx`'s `lib/api` imports are type-only reads that survive any `api.ts` change #1034 makes); #1022/#1042/#1048 touch no `web/**` (#1042 also adds a migration and edits `adr/0628` and `specs/ai.md`; still nothing under `web/`). The only shared file is `specs/ai.md`.
- **Offline worker.** Every fact above is in-repo; nothing needs the open internet.
