# #2789 M1 validation and assertion migrations

Historical M1 checkpoint record; later milestone status is tracked in the PRD.

Status: M1 source review, focused checks, web/repository gates and production build passed; corrected evidence review is tracked separately below. M2, M3 and M4 remain pending.

Code reviewed and tested: `bffcbb77e92b4a58937cd0cc114585a39f574b83`. Cumulative M1 review base: `2bf9a00e6eac5e3989097bdb7167cd3143a092df`; published prototype floor: `3bef77073807e55d01e097f7f60a43d583d6ee21`. The default branch was integrated by merge, preserving published history.

Reviewer, tester and auditor scoped source reviews returned clean at this head after fixing paused DTO lanes, completed-row fallback precedence, unmatched Now placement and stage dictionary fallback. Source claims are separate from tool execution evidence; no final branch readiness is claimed.

## Deliberate-break method

Every break used a fresh git archive of the exact code SHA under the worker scratch directory, with the identical provisioned web package lock/dependencies. The canonical worktree and its tests were never mutated. Each counted break passed npm run typecheck (exit 0), then a focused Vitest selection failed (exit 1) with named assertions. The unmodified selection passed (exit 0).

A temporary AST-based collector in the exports wrapped existing assertion statements in try/catch solely to collect all failing predicates; each caught predicate failure recorded an expect.soft failure immediately. Normal cleanup hooks were left unchanged; no throwing afterEach collector was used. The assertion conditions stayed unchanged. Canonical fail-fast suites were also run separately. This avoids counting a test's first failure as evidence for later assertions that never executed.

The earlier throwing-afterEach evidence was invalidated by the tester because it prevented cleanup; all coverage maps were discarded and all 37 breaks rerun using the corrected method. The failed experiment that made the fallback branch statically false did not typecheck and was excluded; its corrected dynamic wrong-destination break is counted. A bad excerpt anchor stopped one pass, and four budget-page assertions required broadening the selection; neither incomplete pass was claimed complete. Final proof: 37 valid breaks; 65 replacement assertions covered, no uncovered inventory entries.

Selected commands: npm run typecheck; npm exec -- vitest run <the affected count, row, attribution, preference, control, summary and stage files> -t <the named M1/legacy suites>. Tests used TZ=UTC, Node 24 and Vitest 4. The selection explicitly includes RunView's progress-card page cases; it does not run the full web suite per mutant.

| Deliberate break | Typecheck | Focused test |
| --- | --- | --- |
| count-offset | 0 | 1 (expected failure) |
| unreported-positive | 0 | 1 (expected failure) |
| unreported-negative | 0 | 1 (expected failure) |
| count-foreign-members | 0 | 1 (expected failure) |
| mark-completed | 0 | 1 (expected failure) |
| mark-in-progress | 0 | 1 (expected failure) |
| mark-unattached-negative | 0 | 1 (expected failure) |
| fallback-sibling-negative | 0 | 1 (expected failure) |
| fallback-first-positive | 0 | 1 (expected failure) |
| rows-missing | 0 | 1 (expected failure) |
| rows-duplicate | 0 | 1 (expected failure) |
| track-accessibility | 0 | 1 (expected failure) |
| track-titles | 0 | 1 (expected failure) |
| activity-label | 0 | 1 (expected failure) |
| phase-text | 0 | 1 (expected failure) |
| unknown-phase-negative | 0 | 1 (expected failure) |
| row-tooltip | 0 | 1 (expected failure) |
| row-id | 0 | 1 (expected failure) |
| track-min-width | 0 | 1 (expected failure) |
| missing-percent-negative | 0 | 1 (expected failure) |
| hint-tooltip | 0 | 1 (expected failure) |
| sanitizer-inert-reversal | 0 | 1 (expected failure) |
| segment-visible-copy | 0 | 1 (expected failure) |
| active-track-title | 0 | 1 (expected failure) |
| activity-clean-label | 0 | 1 (expected failure) |
| preference-absence | 0 | 1 (expected failure) |
| summary-collapse-body | 0 | 1 (expected failure) |
| summary-collapse-excerpt | 0 | 1 (expected failure) |
| summary-collapse-persistence | 0 | 1 (expected failure) |
| summary-expanded-body | 0 | 1 (expected failure) |
| tooltip-neutral | 0 | 1 (expected failure) |
| tooltip-zero | 0 | 1 (expected failure) |
| owner-dedup | 0 | 1 (expected failure) |
| repeated-role | 0 | 1 (expected failure) |
| control-owner-terminal | 0 | 1 (expected failure) |
| control-busy | 0 | 1 (expected failure) |
| stage-prototype | 0 | 1 (expected failure) |

## Exact deliberate breaks and observed predicate failures

### count-offset

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunProgressCard.tsx",
    "badge.reported ? badge.done : \"–\"",
    "(badge.reported ? badge.done + 1 : \"–\")"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.test.tsx:74`; TestingLibraryElementError: Unable to find an element with the text: 2 of 3 reported complete. This could be because the text is broken up by multiple elements. In this case, you can provide a function for your text matcher to make your matcher more flexible.. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### unreported-positive

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunProgressCard.tsx",
    "badge.reported ? badge.done : \"–\"",
    "badge.reported ? badge.done : \"?\""
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.m1.test.tsx:33`; AssertionError: expected '≈11%·? of 4 reported' to be '≈11%·– of 4 reported' // Object.is equality. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### unreported-negative

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunProgressCard.tsx",
    "{badge && <span data-progress-what",
    "{badge && <><span data-progress-what"
  ],
  [
    "src/components/RunProgressCard.tsx",
    "{badge.reported ? \"reported complete\" : \"reported\"}</span>}",
    "{badge.reported ? \"reported complete\" : \"reported\"}</span>{!badge.reported && <span>0 of {badge.total} reported complete</span>}</> }"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.m1.test.tsx:33`; AssertionError: expected '≈11%·– of 4 reported0 of 4 reported c…' to be '≈11%·– of 4 reported' // Object.is equality. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### count-foreign-members

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/lib/runBadge.ts",
    "const done = frozen.reduce((n, m) => (completed.has(m.id) ? n + 1 : n), 0);",
    "const done = completed.size;"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.m1.test.tsx:43`; TestingLibraryElementError: Unable to find an element with the text: 1 of 4 reported complete. This could be because the text is broken up by multiple elements. In this case, you can provide a function for your text matcher to make your matcher more flexible.. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### mark-completed

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunMilestoneRows.tsx",
    "aria-label=\"reported complete\"",
    "aria-label=\"not started\""
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.m1.test.tsx:45`; AssertionError: expected  to have a length of 1 but got +0. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### mark-in-progress

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunMilestoneRows.tsx",
    "const inProgress = new Set(run.milestones_in_progress ?? []);",
    "const inProgress = new Set<string>();"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.m1.test.tsx:78`; TestingLibraryElementError: Unable to find an element with the text: reviewer. This could be because the text is broken up by multiple elements. In this case, you can provide a function for your text matcher to make your matcher more flexible.. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### mark-unattached-negative

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunMilestoneRows.tsx",
    "inProgress.has(m.id) ? \"in_progress\"",
    "true ? \"in_progress\""
  ]
]
```

Observed original predicate failure: `web/src/pages/RunView.nowline.test.tsx:132`; TestingLibraryElementError: Found multiple elements with the text of: in progress. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### fallback-sibling-negative

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunMilestoneRows.tsx",
    "m.id === firstInProgress && (",
    "(m.id === firstInProgress || m.id === \"m3\") && ("
  ]
]
```

Observed original predicate failure: `web/src/pages/RunView.nowline.test.tsx:118`; TestingLibraryElementError: Found multiple elements with the text: coder. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### fallback-first-positive

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunMilestoneRows.tsx",
    "m.id === firstInProgress && (",
    "m.id === firstInProgress && m.id === \"no-fallback-destination\" && ("
  ]
]
```

Observed original predicate failure: `web/src/pages/RunView.nowline.test.tsx:118`; TestingLibraryElementError: Unable to find an element with the text: coder. This could be because the text is broken up by multiple elements. In this case, you can provide a function for your text matcher to make your matcher more flexible.. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### rows-missing

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunMilestoneRows.tsx",
    "if (milestones.length === 0) return null;",
    "if (milestones.length >= 0) return null;"
  ]
]
```

Observed original predicate failure: `web/src/pages/RunView.nowline.test.tsx:117`; TestingLibraryElementError: Unable to find an element with the text: m2. This could be because the text is broken up by multiple elements. In this case, you can provide a function for your text matcher to make your matcher more flexible.. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### rows-duplicate

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunProgressCard.tsx",
    "<MilestoneRows run={run} activity={live} activeContent={<>{phaseLine}{nowLine}</>} />",
    "<><MilestoneRows run={run} activity={live} activeContent={<>{phaseLine}{nowLine}</>} /><MilestoneRows run={run} activity={live} /></>"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.m1.test.tsx:31`; AssertionError: expected …(2) to have a length of 1 but got 2. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### track-accessibility

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunProgressCard.tsx",
    "aria-label={trackLabel}",
    "aria-label={trackLabel + \" broken\"}"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.test.tsx:76`; AssertionError: expected 'Plan approved; 2 of 3 milestones repo…' to be 'Plan approved; 2 of 3 milestones repo…' // Object.is equality. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### track-titles

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunProgressCard.tsx",
    "data-seg={state} title={title}",
    "data-seg={state} title={title + \" broken\"}"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.test.tsx:80`; AssertionError: expected [ …(4) ] to deeply equal [ [ 'done', 'Plan approved' ], …(3) ]. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### activity-label

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunMilestoneRows.tsx",
    "const safeLabel = stripUnsafeChars(label);",
    "const safeLabel = stripUnsafeChars(label).replace(/./g, \"x\");"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.test.tsx:87`; AssertionError: expected 'leadxxxxxxxxxxxxxxBash1m ago' to contain 'running a gate'. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### phase-text

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunProgressCard.tsx",
    "phase && `phase: ${stripUnsafeChars(phase)}`",
    "phase && `current phase: ${stripUnsafeChars(phase)}`"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.test.tsx:103`; AssertionError: expected 'current phase: review' to be 'phase: review' // Object.is equality. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### unknown-phase-negative

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunProgressCard.tsx",
    "? p.phase : \"\";",
    "? p.phase : \"implement\";"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.test.tsx:112`; AssertionError: expected <div …(4)></div> to be null. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### row-tooltip

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunMilestoneRows.tsx",
    "title={stripUnsafeChars(m.title)}",
    "title={stripUnsafeChars(m.title) + \" broken\"}"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.test.tsx:165`; AssertionError: expected 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx…' to be 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx…' // Object.is equality. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### row-id

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunMilestoneRows.tsx",
    "{stripUnsafeChars(m.id)}</span>",
    "{stripUnsafeChars(m.id) + \"broken\"}</span>"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.test.tsx:171`; AssertionError: expected 'recovery-docs-validationbrokenxxxxxxx…' to be 'recovery-docs-validationxxxxxxxxxxxxx…' // Object.is equality. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### track-min-width

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunProgressCard.tsx",
    "className=\"grid min-w-0 gap-1.5\"",
    "className=\"grid gap-1.5\""
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.test.tsx:173`; AssertionError: expected 'grid gap-1.5' to contain 'min-w-0'. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### missing-percent-negative

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunProgressCard.tsx",
    "? Math.max(0, Math.min(100, Math.round(p.pct))) : null;",
    "? Math.max(0, Math.min(100, Math.round(p.pct))) : 11;"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.test.tsx:217`; TestingLibraryElementError: Unable to find an element with the text: limit wait · resumes 09:00 PM. This could be because the text is broken up by multiple elements. In this case, you can provide a function for your text matcher to make your matcher more flexible.. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### hint-tooltip

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunProgressCard.tsx",
    "title=\"A question mentions this run; this is a hint, not a recorded dependency.\"",
    "title=\"Not a hint\""
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.test.tsx:284`; AssertionError: expected 'Not a hint' to be 'A question mentions this run; this is…' // Object.is equality. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### sanitizer-inert-reversal

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/lib/safeText.ts",
    "return s.replace(UNSAFE_CHAR, \"\");",
    "return s.replace(UNSAFE_CHAR, \"$&\");"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.test.tsx:315`; TestingLibraryElementError: Unable to find an element with the text: eviltxt.exe-step. This could be because the text is broken up by multiple elements. In this case, you can provide a function for your text matcher to make your matcher more flexible.. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### segment-visible-copy

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunProgressCard.tsx",
    "      />\n\n    </div>",
    "      >{title}</div>\n\n    </div>"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.m1.test.tsx:37`; AssertionError: expected false to be true // Object.is equality. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### active-track-title

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunProgressCard.tsx",
    "active milestone: ${stripUnsafeChars(milestones.find(m => m.id === active)?.title ?? active)}",
    "active milestone: broken${stripUnsafeChars(milestones.find(m => m.id === active)?.title ?? active)}"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.test.tsx:76`; AssertionError: expected 'Plan approved; 2 of 3 milestones repo…' to be 'Plan approved; 2 of 3 milestones repo…' // Object.is equality. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### activity-clean-label

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunMilestoneRows.tsx",
    "const safeLabel = stripUnsafeChars(label);",
    "const safeLabel = stripUnsafeChars(label).replace(\"fix it\", \"different task\");"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.test.tsx:325`; AssertionError: expected 'coderdifferent taskBash1m ago' to contain 'fix it'. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### preference-absence

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/lib/prefs.ts",
    "return pruned[runId]?.collapsed;",
    "return pruned[runId]?.collapsed ?? false;"
  ]
]
```

Observed original predicate failure: `web/src/lib/prefs.test.ts:78`; AssertionError: expected false to be undefined. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### summary-collapse-body

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/pages/RunView.tsx",
    "{!collapsed && (",
    "{true && ("
  ]
]
```

Observed original predicate failure: `web/src/pages/runView/DeliveredCard.test.tsx:322`; AssertionError: expected <section …(2)>…(7)</section> to be null. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### summary-collapse-excerpt

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/pages/RunView.tsx",
    "{collapsed && <p",
    "{false && collapsed && <p"
  ]
]
```

Observed original predicate failure: `web/src/pages/RunView.test.tsx:832`; TestingLibraryElementError: Unable to find an element with the text: Some intent.. This could be because the text is broken up by multiple elements. In this case, you can provide a function for your text matcher to make your matcher more flexible.. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### summary-collapse-persistence

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/lib/prefs.ts",
    "return pruned[runId]?.collapsed;",
    "return pruned[runId] ? undefined : undefined;"
  ]
]
```

Observed original predicate failure: `web/src/pages/RunView.test.tsx:847`; AssertionError: expected <h3 …(1)></h3> to be null. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### summary-expanded-body

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/pages/RunView.tsx",
    "{!collapsed && (",
    "{collapsed && ("
  ]
]
```

Observed original predicate failure: `web/src/pages/RunView.test.tsx:740`; TestingLibraryElementError: Unable to find an element with the text: What this run will implement. This could be because the text is broken up by multiple elements. In this case, you can provide a function for your text matcher to make your matcher more flexible.. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### tooltip-neutral

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunProgressCard.tsx",
    "\"No milestone completion reported for this run\"",
    "\"Wrong neutral tooltip\""
  ]
]
```

Observed original predicate failure: `web/src/pages/RunView.test.tsx:5699`; AssertionError: expected 'Wrong neutral tooltip' to be 'No milestone completion reported for …' // Object.is equality. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### tooltip-zero

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunProgressCard.tsx",
    "\"Milestones reported complete of the approved plan\"",
    "\"Wrong zero tooltip\""
  ]
]
```

Observed original predicate failure: `web/src/pages/RunView.test.tsx:5702`; AssertionError: expected 'Wrong zero tooltip' to be 'Milestones reported complete of the a…' // Object.is equality. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### owner-dedup

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/RunMilestoneRows.tsx",
    "const ownerIsLive = attribution ? laneList.some((l) => l.agent === attribution.agent) : false;",
    "const ownerIsLive = attribution ? laneList.some((l) => l.agent === attribution.agent) && false : false;"
  ]
]
```

Observed original predicate failure: `web/src/components/RunProgressCard.m1.test.tsx:77`; AssertionError: expected [ <span></span>, <span></span> ] to have a length of 1 but got 2. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### repeated-role

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/lib/runBadge.ts",
    "if (match !== null) return null; // two-or-more → ambiguous, suppress on all",
    "if (match !== null) return match; // deliberately wrong repeated-role join"
  ]
]
```

Observed original predicate failure: `web/src/pages/RunView.attribution.test.tsx:219`; AssertionError: expected <span …(1)></span> to be null. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### control-owner-terminal

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/SteerQueueCard.tsx",
    "if (terminal || !canSteer) return null;",
    "if (terminal && !canSteer) return null;"
  ]
]
```

Observed original predicate failure: `web/src/components/SteerQueueCard.test.tsx:281`; AssertionError: expected <button …(2)></button> to be null. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### control-busy

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/components/SteerQueueCard.tsx",
    "disabled={busy}",
    "disabled={false && busy}",
    "all"
  ]
]
```

Observed original predicate failure: `web/src/components/SteerQueueCard.test.tsx:654`; AssertionError: expected false to be true // Object.is equality. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).

### stage-prototype

Each edit below is file, original text, replacement text, optional all-occurrences flag. Each original anchor was checked before replacement.

```json
[
  [
    "src/lib/runStage.ts",
    "typeof name === \"string\" && Object.prototype.hasOwnProperty.call(TOOL_STAGE, name) ? TOOL_STAGE[name] : \"Working\"",
    "typeof name === \"string\" ? TOOL_STAGE[name] ?? \"Working\" : \"Working\""
  ]
]
```

Observed original predicate failure: `web/src/lib/runStage.test.ts:9`; AssertionError: expected [Function Object] to be 'Working' // Object.is equality. Typecheck exit 0; focused mutation test exit 1; unmodified selected run exit 0 (185 tests).


## Exhaustive changed-assertion inventory

This inventory compares the merged prototype with the code SHA above. All changed assertion statements in the owned test files are listed; import/wrapper and setup-only migrations are listed separately below. Each replacement line names at least one deliberate break that failed that actual assertion. The PR body must carry this old -> new inventory.

### M1-A01 — web/src/pages/RunView.test.tsx

Test: collapses on toggle and remembers the choice per run across a remount -> collapses on toggle and remembers the choice per run across a remount

Old:

```typescript
expect(screen.queryByText("Some intent.")).toBeNull();
expect(screen.queryByText("Some intent.")).toBeNull();
```

New:

```typescript
expect(screen.getByText("What this run will implement")).toBeTruthy();
expect(screen.getByText("Some intent.")).toBeTruthy();
expect(screen.getByText("Body plan.")).toBeTruthy();
expect(screen.getByText("Body delta.")).toBeTruthy();
expect(screen.getByText("Some intent.")).toBeTruthy();
expect(screen.queryByText("What this run will implement")).toBeNull();
expect(screen.queryByText("Body plan.")).toBeNull();
expect(screen.queryByText("Body delta.")).toBeNull();
expect(screen.getByText("Some intent.")).toBeTruthy();
expect(screen.queryByText("What this run will implement")).toBeNull();
expect(screen.queryByText("Body plan.")).toBeNull();
expect(screen.queryByText("Body delta.")).toBeNull();
```

- web/src/pages/RunView.test.tsx:834 failed under summary-expanded-body; unmodified selection passed.
- web/src/pages/RunView.test.tsx:835 failed under summary-expanded-body; unmodified selection passed.
- web/src/pages/RunView.test.tsx:836 failed under summary-expanded-body; unmodified selection passed.
- web/src/pages/RunView.test.tsx:837 failed under summary-expanded-body; unmodified selection passed.
- web/src/pages/RunView.test.tsx:839 failed under summary-collapse-body, summary-collapse-excerpt, summary-expanded-body; unmodified selection passed.
- web/src/pages/RunView.test.tsx:840 failed under summary-collapse-body, summary-expanded-body; unmodified selection passed.
- web/src/pages/RunView.test.tsx:841 failed under summary-collapse-body, summary-expanded-body; unmodified selection passed.
- web/src/pages/RunView.test.tsx:842 failed under summary-collapse-body, summary-expanded-body; unmodified selection passed.
- web/src/pages/RunView.test.tsx:846 failed under summary-collapse-body, summary-collapse-excerpt, summary-expanded-body; unmodified selection passed.
- web/src/pages/RunView.test.tsx:847 failed under summary-collapse-body, summary-collapse-persistence, summary-expanded-body; unmodified selection passed.
- web/src/pages/RunView.test.tsx:848 failed under summary-collapse-body, summary-collapse-persistence, summary-expanded-body; unmodified selection passed.
- web/src/pages/RunView.test.tsx:849 failed under summary-collapse-body, summary-collapse-persistence, summary-expanded-body; unmodified selection passed.

### M1-A02 — web/src/pages/RunView.test.tsx

Test: does NOT render the live 'now' strip on a PAUSED run -> does NOT render the live 'now' strip on a PAUSED run

Old:

```typescript
expect(screen.getByText(/reported complete/i)).toBeTruthy();
```

New:

```typescript
expect(container.querySelector("[data-run-progress-card] ul")).not.toBeNull();
```

- web/src/pages/RunView.test.tsx:1767 failed under rows-missing; unmodified selection passed.

### M1-A03 — web/src/pages/RunView.test.tsx

Test: renders M{done}/{total} for a milestone-structured run -> renders reported counts for a milestone-structured run

Old:

```typescript
expect(screen.getByText("M3/7")).toBeTruthy();
```

New:

```typescript
expect(screen.getByText("3 of 7 reported complete")).toBeTruthy();
```

- web/src/pages/RunView.test.tsx:5673 failed under count-offset; unmodified selection passed.

### M1-A04 — web/src/pages/RunView.test.tsx

Test: renders M–/N (not a 0/N) when the tracker was never reported (PRD #265 M2) -> renders – of N reported (not a reported zero) when the tracker was never reported (PRD #265 M2)

Old:

```typescript
expect(screen.getByText("M–/4")).toBeTruthy();
expect(screen.queryByText("M0/4")).toBeNull();
```

New:

```typescript
expect(screen.getByText("– of 4 reported")).toBeTruthy();
expect(screen.queryByText("0 of 4 reported complete")).toBeNull();
```

- web/src/pages/RunView.test.tsx:5683 failed under unreported-positive; unmodified selection passed.
- web/src/pages/RunView.test.tsx:5684 failed under unreported-negative; unmodified selection passed.

### M1-A05 — web/src/pages/RunView.test.tsx

Test: still renders M0/N when an empty completion set WAS reported (genuine zero) -> still renders 0 of N reported complete when an empty completion set WAS reported (genuine zero)

Old:

```typescript
expect(screen.getByText("M0/4")).toBeTruthy();
```

New:

```typescript
expect(screen.getByText("0 of 4 reported complete")).toBeTruthy();
```

- web/src/pages/RunView.test.tsx:5689 failed under count-offset; unmodified selection passed.

### M1-A06 — web/src/pages/RunView.test.tsx

Test: renders every title with a done / in-progress / left indicator -> renders every title with a done / in-progress / left indicator

Old:

```typescript
expect(screen.getByLabelText("done")).toBeTruthy();
```

New:

```typescript
expect(screen.getByLabelText("reported complete")).toBeTruthy();
```

- web/src/pages/RunView.test.tsx:5720 failed under mark-completed, rows-missing; unmodified selection passed.

### M1-A07 — web/src/pages/RunView.test.tsx

Test: says 'reported complete' and NEVER implies verification -> says 'reported complete' and NEVER implies verification

Old:

```typescript
expect(screen.getByText(/reported complete/i)).toBeTruthy();
```

New:

```typescript
expect(screen.getByText("1 of 3 reported complete")).toBeTruthy();
```

- web/src/pages/RunView.test.tsx:5729 failed under count-offset; unmodified selection passed.

### M1-A08 — web/src/pages/RunView.test.tsx

Test: shows a done/total count that is clamped to frozen membership -> shows a done/total count that is clamped to frozen membership

Old:

```typescript
expect(screen.getByText("2/3")).toBeTruthy();
```

New:

```typescript
expect(screen.getByText("2 of 3 reported complete")).toBeTruthy();
```

- web/src/pages/RunView.test.tsx:5736 failed under count-offset, count-foreign-members; unmodified selection passed.

### M1-A09 — web/src/pages/RunView.test.tsx

Test: shows '–/N' (not '0/N') in the header when completion was never reported (PRD #265 M2) -> shows '–/N' (not '0/N') in the header when completion was never reported (PRD #265 M2)

Old:

```typescript
expect(screen.getByText("–/3")).toBeTruthy();
expect(screen.queryByText("0/3")).toBeNull();
```

New:

```typescript
expect(screen.getByText("– of 3 reported")).toBeTruthy();
expect(screen.queryByText("0 of 3 reported complete")).toBeNull();
```

- web/src/pages/RunView.test.tsx:5746 failed under unreported-positive; unmodified selection passed.
- web/src/pages/RunView.test.tsx:5747 failed under unreported-negative; unmodified selection passed.

### M1-A10 — web/src/pages/RunView.test.tsx

Test: shows a genuine '0/3' when an empty completion set WAS reported -> shows a genuine '0/3' when an empty completion set WAS reported

Old:

```typescript
expect(screen.getByText("0/3")).toBeTruthy();
```

New:

```typescript
expect(screen.getByText("0 of 3 reported complete")).toBeTruthy();
```

- web/src/pages/RunView.test.tsx:5754 failed under count-offset; unmodified selection passed.

### M1-A11 — web/src/pages/RunView.baseline.test.tsx

Test: shows the `◐ <firstInProgress>` header marker for m2 (unchanged, non-goal for M5) -> marks m2 in progress and attaches fallback activity only to the first row

Old:

```typescript
expect(screen.getByText(/◐\s*m2/)).toBeTruthy();
expect(screen.queryByText(/◐\s*m3/)).toBeNull();
```

New:

```typescript
expect(within(screen.getByText("m2").closest("li") as HTMLElement).getByLabelText("in progress")).toBeTruthy();
expect(within(screen.getByText("m3").closest("li") as HTMLElement).queryByText("coder")).toBeNull();
expect(within(screen.getByText("m2").closest("li") as HTMLElement).getByText("coder")).toBeTruthy();
```

- web/src/pages/RunView.baseline.test.tsx:169 failed under mark-in-progress, rows-missing, row-id; unmodified selection passed.
- web/src/pages/RunView.baseline.test.tsx:171 failed under fallback-sibling-negative, rows-missing, row-id; unmodified selection passed.
- web/src/pages/RunView.baseline.test.tsx:172 failed under fallback-first-positive, rows-missing, row-id; unmodified selection passed.

### M1-A12 — web/src/pages/RunView.nowline.test.tsx

Test: renders the active strip under the in-progress row and names the id in the header -> renders the active strip under the marked in-progress row

Old:

```typescript
expect(screen.getByText(/◐\s*m2/)).toBeTruthy();
```

New:

```typescript
expect(within(screen.getByText("m2").closest("li") as HTMLElement).getByLabelText("in progress")).toBeTruthy();
```

- web/src/pages/RunView.nowline.test.tsx:117 failed under mark-in-progress, rows-missing, row-id; unmodified selection passed.

### M1-A13 — web/src/pages/RunView.nowline.test.tsx

Test: renders an UNATTACHED strip under the header when nothing is declared in progress -> renders an UNATTACHED strip under the header when nothing is declared in progress

Old:

```typescript
expect(screen.queryByText(/◐/)).toBeNull();
```

New:

```typescript
expect(screen.queryByLabelText("in progress")).toBeNull();
```

- web/src/pages/RunView.nowline.test.tsx:132 failed under mark-unattached-negative; unmodified selection passed.

### M1-A14 — web/src/pages/RunView.nowline.test.tsx

Test: renders NO strip when there is no activity (a milestone run with nothing acting) -> renders NO strip when there is no activity (a milestone run with nothing acting)

Old:

```typescript
expect(screen.getByText(/◐\s*m2/)).toBeTruthy();
```

New:

```typescript
expect(within(screen.getByText("m2").closest("li") as HTMLElement).getByLabelText("in progress")).toBeTruthy();
```

- web/src/pages/RunView.nowline.test.tsx:145 failed under mark-in-progress, rows-missing, row-id; unmodified selection passed.

### M1-A15 — web/src/pages/RunView.nowline.test.tsx

Test: D4: with several ids in progress, only the FIRST by frozen order is named and carries the strip -> D4: with several ids in progress, only the FIRST by frozen order is named and carries the strip

Old:

```typescript
expect(screen.getByText(/◐\s*m2/)).toBeTruthy();
expect(screen.queryByText(/◐\s*m3/)).toBeNull();
```

New:

```typescript
expect(within(screen.getByText("m2").closest("li") as HTMLElement).getByLabelText("in progress")).toBeTruthy();
expect(within(screen.getByText("m3").closest("li") as HTMLElement).queryByText("coder")).toBeNull();
expect(within(screen.getByText("m2").closest("li") as HTMLElement).getByText("coder")).toBeTruthy();
```

- web/src/pages/RunView.nowline.test.tsx:183 failed under mark-in-progress, rows-missing, row-id; unmodified selection passed.
- web/src/pages/RunView.nowline.test.tsx:184 failed under fallback-sibling-negative, rows-missing, row-id; unmodified selection passed.
- web/src/pages/RunView.nowline.test.tsx:185 failed under fallback-first-positive, rows-missing, row-id; unmodified selection passed.

### M1-A16 — web/src/pages/RunView.budget.test.tsx

Test: renders the card from run.progress, above the milestone checklist -> renders the card from run.progress with the sole frozen list

Old:

```typescript
expect(screen.getByText("milestone 2 of 2 · Second")).toBeTruthy();
expect(cardEl.compareDocumentPosition(checklist) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
```

New:

```typescript
expect(screen.getByText("1 of 2 reported complete")).toBeTruthy();
expect(rows).not.toBeNull();
expect(container.querySelectorAll("ul")).toHaveLength(1);
expect(rows?.textContent).toContain("Second");
```

- web/src/pages/RunView.budget.test.tsx:333 failed under count-offset; unmodified selection passed.
- web/src/pages/RunView.budget.test.tsx:336 failed under rows-missing; unmodified selection passed.
- web/src/pages/RunView.budget.test.tsx:337 failed under rows-missing, rows-duplicate; unmodified selection passed.
- web/src/pages/RunView.budget.test.tsx:338 failed under rows-missing; unmodified selection passed.

### M1-A17 — web/src/components/RunProgressCard.test.tsx

Test: percent: ≈70%, milestone k of M with the active title, the segmented track and the active role -> percent: ≈70%, reported count, active-title track and fallback activity

Old:

```typescript
expect(screen.getByText("milestone 3 of 3 · recovery-docs-validation")).toBeTruthy();
expect(track.getAttribute("aria-label")).toBe(
      "Plan progress: planning done, 2 of 3 milestones done, milestone 3 (recovery-docs-validation) in progress",
    );
expect(segs).toEqual([
      ["done", "plan"],
      ["done", "api-completion-receipt"],
      ["done", "worker-receipt-ordering"],
      ["now", "recovery-docs-validation"],
    ]);
expect(screen.getByText("Active role")).toBeTruthy();
expect(screen.getByText("lead").closest("dd")?.textContent).toBe("lead · running a gate");
```

New:

```typescript
expect(screen.getByText("2 of 3 reported complete")).toBeTruthy();
expect(track.getAttribute("aria-label")).toBe(
      "Plan approved; 2 of 3 milestones reported complete; active milestone: recovery-docs-validation",
    );
expect(segs).toEqual([
      ["done", "Plan approved"],
      ["done", "api-completion-receipt · reported complete"],
      ["done", "worker-receipt-ordering · reported complete"],
      ["now", "recovery-docs-validation · in progress"],
    ]);
expect(container.querySelectorAll("ul")).toHaveLength(1);
expect(screen.getByText("lead").parentElement?.parentElement?.textContent).toContain("running a gate");
```

- web/src/components/RunProgressCard.test.tsx:74 failed under count-offset; unmodified selection passed.
- web/src/components/RunProgressCard.test.tsx:76 failed under track-accessibility, active-track-title; unmodified selection passed.
- web/src/components/RunProgressCard.test.tsx:80 failed under track-titles; unmodified selection passed.
- web/src/components/RunProgressCard.test.tsx:86 failed under rows-missing, rows-duplicate; unmodified selection passed.
- web/src/components/RunProgressCard.test.tsx:87 failed under rows-missing, rows-duplicate, activity-label; unmodified selection passed.

### M1-A18 — web/src/components/RunProgressCard.test.tsx

Test: names `N of M milestones done` when no milestone is active, with no `now` segment -> names `N of M reported complete` when no milestone is active, with no `now` segment

Old:

```typescript
expect(screen.getByText("1 of 3 milestones done")).toBeTruthy();
```

New:

```typescript
expect(screen.getByText("1 of 3 reported complete")).toBeTruthy();
```

- web/src/components/RunProgressCard.test.tsx:95 failed under count-offset; unmodified selection passed.

### M1-A19 — web/src/components/RunProgressCard.test.tsx

Test: marks the current phase %s with ▸ and only that one -> marks the current phase %s and only that one

Old:

```typescript
expect(cur.textContent).toBe(`▸ ${phase}`);
```

New:

```typescript
expect(cur.textContent).toBe(`phase: ${phase}`);
```

- web/src/components/RunProgressCard.test.tsx:103 failed under rows-missing, phase-text; unmodified selection passed.

### M1-A20 — web/src/components/RunProgressCard.test.tsx

Test: marks the current phase %s with ▸ and only that one -> marks the current phase %s and only that one

Old:

```typescript
expect(screen.getByText("current phase only, from the active role")).toBeTruthy();
```

New:

```typescript
expect(within(cur).queryByText(/phase: (review|validate|implement)/)?.textContent).toBe(`phase: ${phase}`);
```

- web/src/components/RunProgressCard.test.tsx:105 failed under rows-missing, phase-text; unmodified selection passed.

### M1-A21 — web/src/components/RunProgressCard.test.tsx

Test: hides the phase row when the phase is empty or unknown (PRD D6) -> hides the phase row when the phase is empty or unknown (PRD D6)

Old:

```typescript
expect(container.textContent).not.toContain("▸");
expect(screen.queryByText("implement")).toBeNull();
```

New:

```typescript
expect(container.querySelector('[aria-current="step"]')).toBeNull();
expect(screen.queryByText(/phase: implement/)).toBeNull();
```

- web/src/components/RunProgressCard.test.tsx:113 failed under unknown-phase-negative; unmodified selection passed.
- web/src/components/RunProgressCard.test.tsx:114 failed under unknown-phase-negative; unmodified selection passed.

### M1-A22 — web/src/components/RunProgressCard.test.tsx

Test: active role line in the %s state (%s): %s -> row activity fallback in the %s state (%s): %s

Old:

```typescript
expect(screen.queryByText("Active role") !== null).toBe(shown);
```

New:

```typescript
expect(screen.queryByText("running a gate") !== null).toBe(shown);
```

- web/src/components/RunProgressCard.test.tsx:149 failed under rows-missing, rows-duplicate, activity-label; unmodified selection passed.

### M1-A23 — web/src/components/RunProgressCard.test.tsx

Test: a long unbroken milestone title cannot widen the card -> a long unbroken milestone title cannot widen the card

Old:

```typescript
expect(what.textContent).toBe(`milestone 3 of 3 · ${long}`);
```

New:

```typescript
expect(what.textContent).toBe("2 of 3 reported complete");
expect(screen.getByText(long).getAttribute("title")).toBe(long);
```

- web/src/components/RunProgressCard.test.tsx:164 failed under count-offset; unmodified selection passed.
- web/src/components/RunProgressCard.test.tsx:165 failed under rows-missing, rows-duplicate, row-tooltip; unmodified selection passed.

### M1-A24 — web/src/components/RunProgressCard.test.tsx

Test: a long unbroken milestone title cannot widen the card -> a long unbroken milestone title cannot widen the card

Old:

```typescript
expect(label.textContent).toBe(long);
```

New:

```typescript
expect(label.textContent).toBe(`recovery-docs-validation${long}`);
```

- web/src/components/RunProgressCard.test.tsx:171 failed under row-id; unmodified selection passed.

### M1-A25 — web/src/components/RunProgressCard.test.tsx

Test: a long unbroken milestone title cannot widen the card -> a long unbroken milestone title cannot widen the card

Old:

```typescript
expect((label.parentElement as HTMLElement).className).toContain("min-w-0");
```

New:

```typescript
expect(track.querySelector('[data-seg="now"]')?.className).toContain("min-w-0");
```

- web/src/components/RunProgressCard.test.tsx:173 failed under track-min-width; unmodified selection passed.

### M1-A26 — web/src/components/RunProgressCard.test.tsx

Test: renders nothing for null/absent progress, `none`, an unknown state, or a percent without pct -> keeps frozen rows for null/absent progress, none, unknown or percent without pct

Old:

```typescript
expect(container.innerHTML).toBe("");
```

New:

```typescript
expect(container.querySelectorAll("li")).toHaveLength(3);
expect(screen.getByText("2 of 3 reported complete")).toBeTruthy();
expect(container.querySelector("[data-progress-pct]")).toBeNull();
```

- web/src/components/RunProgressCard.test.tsx:259 failed under rows-missing, rows-duplicate; unmodified selection passed.
- web/src/components/RunProgressCard.test.tsx:260 failed under count-offset; unmodified selection passed.
- web/src/components/RunProgressCard.test.tsx:261 failed under missing-percent-negative; unmodified selection passed.

### M1-A27 — web/src/components/RunProgressCard.test.tsx

Test: blocked-by: an info link to /runs/<id> showing the first 8 characters, beside the waiting flag -> blocked-by: an info link to /runs/<id> showing the first 8 characters, beside the waiting flag

Old:

```typescript
expect(screen.getByText("open question mentions an issue that run is working")).toBeTruthy();
```

New:

```typescript
expect(link.getAttribute("title")).toBe("A question mentions this run; this is a hint, not a recorded dependency.");
```

- web/src/components/RunProgressCard.test.tsx:284 failed under hint-tooltip; unmodified selection passed.

### M1-A28 — web/src/components/RunProgressCard.test.tsx

Test: strips control and bidi characters from a hostile milestone title and activity, in text and attributes -> strips control and bidi characters from a hostile milestone title and activity, in text and attributes

Old:

```typescript
expect(screen.getByText(`milestone 2 of 2 · ${clean}`)).toBeTruthy();
expect(now.textContent).toBe(clean);
```

New:

```typescript
expect(screen.getByText(clean)).toBeTruthy();
expect(screen.getByText(clean).getAttribute("title")).toBe(clean);
expect(now.textContent).toBe("");
```

- web/src/components/RunProgressCard.test.tsx:315 failed under rows-missing, rows-duplicate, sanitizer-inert-reversal; unmodified selection passed.
- web/src/components/RunProgressCard.test.tsx:317 failed under rows-missing, rows-duplicate, row-tooltip, sanitizer-inert-reversal; unmodified selection passed.
- web/src/components/RunProgressCard.test.tsx:318 failed under segment-visible-copy; unmodified selection passed.

### M1-A29 — web/src/components/RunProgressCard.test.tsx

Test: strips control and bidi characters from a hostile milestone title and activity, in text and attributes -> strips control and bidi characters from a hostile milestone title and activity, in text and attributes

Old:

```typescript
expect(screen.getByRole("img").getAttribute("aria-label")).toContain(`(${clean})`);
```

New:

```typescript
expect(screen.getByRole("img").getAttribute("aria-label")).toContain(`active milestone: ${clean}`);
```

- web/src/components/RunProgressCard.test.tsx:320 failed under sanitizer-inert-reversal, active-track-title; unmodified selection passed.

### M1-A30 — web/src/components/RunProgressCard.test.tsx

Test: strips control and bidi characters from a hostile milestone title and activity, in text and attributes -> strips control and bidi characters from a hostile milestone title and activity, in text and attributes

Old:

```typescript
expect(dd.textContent).toBe("coder · fix it");
```

New:

```typescript
expect(strip.textContent).toContain("fix it");
```

- web/src/components/RunProgressCard.test.tsx:325 failed under activity-label, activity-clean-label; unmodified selection passed.

### M1-A31 — web/src/components/RunProgressCard.test.tsx

Test: falls back to the sanitised id when the active milestone is not in the frozen list -> falls back to the sanitised id when the active milestone is not in the frozen list

Old:

```typescript
expect(screen.getByText("active milestone · ghost")).toBeTruthy();
```

New:

```typescript
expect(screen.getByRole("img").getAttribute("aria-label")).toContain("active milestone: ghost");
```

- web/src/components/RunProgressCard.test.tsx:343 failed under sanitizer-inert-reversal, active-track-title; unmodified selection passed.

### M1-A32 — web/src/lib/prefs.test.ts

Test: defaults to expanded when the run has no stored choice -> returns undefined when the run has no stored choice

Old:

```typescript
expect(summaryCollapse.getCollapsed("run-1")).toBe(false);
```

New:

```typescript
expect(summaryCollapse.getCollapsed("run-1")).toBeUndefined();
```

- web/src/lib/prefs.test.ts:78 failed under preference-absence; unmodified selection passed.

### M1-A33 — web/src/lib/prefs.test.ts

Test: persists a collapse choice and reads it back after a reload (fresh read, no state) -> persists a collapse choice and reads it back after a reload (fresh read, no state)

Old:

```typescript
expect(summaryCollapse.getCollapsed("run-2")).toBe(false);
```

New:

```typescript
expect(summaryCollapse.getCollapsed("run-2")).toBeUndefined();
```

- web/src/lib/prefs.test.ts:85 failed under preference-absence; unmodified selection passed.

### M1-A34 — web/src/lib/prefs.test.ts

Test: drops an entry older than 7 days on read (GC) while keeping a fresh sibling -> drops an entry older than 7 days on read (GC) while keeping a fresh sibling

Old:

```typescript
expect(summaryCollapse.getCollapsed("stale", t0 + 8 * DAY)).toBe(false);
```

New:

```typescript
expect(summaryCollapse.getCollapsed("stale", t0 + 8 * DAY)).toBeUndefined();
```

- web/src/lib/prefs.test.ts:98 failed under preference-absence; unmodified selection passed.

### M1-A35 — web/src/lib/prefs.test.ts

Test: returns the default when the stored map is corrupt, without throwing -> returns undefined when the stored map is corrupt, without throwing

Old:

```typescript
expect(summaryCollapse.getCollapsed("run-1")).toBe(false);
```

New:

```typescript
expect(summaryCollapse.getCollapsed("run-1")).toBeUndefined();
```

- web/src/lib/prefs.test.ts:119 failed under preference-absence; unmodified selection passed.

## Selector declarations and setup-only migrations

- RunView reported-count tooltip case: neutral locator M–/4 -> – of 4 reported; zero locator M0/4 -> 0 of 4 reported complete. The unchanged title predicates separately failed under tooltip-neutral and tooltip-zero, with unmodified success.
- Row-only checklist renders/imports -> production MilestoneRows in RunView attribution, baseline and nowline suites; count and combined-surface cases -> production RunProgressCard. No fabricated checklist header or fake button was introduced. Unchanged attribution/sanitisation predicates were retained; owner-dedup, repeated-role and sanitizer-inert-reversal breaks detected their corresponding failures.

Historical setup-only worker record (its no-mutation wording describes that earlier delegation, not the completed proof above):


Scope: only the five authorized test paths; RunView.test.tsx edits only inside RunSummary (PRD #362 M4). No production edits, commits, installs, full suites, gates, builds, or mutation proof. This maintains seeded behavior; no milestone completion is claimed. M2/M3 product hardening and new scenarios remain pending.

## Every assertion / selector migration

Selectors and expected values not explicitly listed below are unchanged. Existing assertions were retained except the approved absent-choice and collapsed-intent migrations, whose replacement assertions pin the current behavior.

| File / case | Old setup or assertion | New setup or assertion | Retained fact / falsifiability |
| --- | --- | --- | --- |
| prefs: no stored choice | getCollapsed(run-1) === false; title defaults expanded | === undefined; title returns undefined | Absence is distinct from explicit false. Saved true and saved false assertions unchanged. |
| prefs: different run | getCollapsed(run-2) === false after saving run-1 true | === undefined | Per-run isolation; saved run-1 true still required. |
| prefs: expired entry | stale === false after 8 days | === undefined, comment no stored choice | Expiration, fresh sibling true, under-boundary true, and storage GC assertions retained. |
| prefs: corrupt map | corrupt result === false; title returns default | === undefined; title returns undefined | Corrupt storage does not throw; absence is explicit. |
| SteerQueueCard: terminal nonempty queue | Card alone; queue present, Send and Stop absent | SteeringSurface: actual RunControls in header and actual Card below; same assertions | Queue survives terminal while real lifecycle controls disappear; live Stop positive countercase retained. |
| SteerQueueCard: live queue | Card alone; Send and Stop present | SteeringSurface, same Send and Stop selectors | Both composer and actual header Stop remain reachable. |
| SteerQueueCard: stable terminal transition | Card render/rerender; included chip persists and Send disappears | SteeringSurface render/rerender with same queue and terminal/status flip; same assertions | Lifted queue and included receipt survive terminal. |
| Pause menu: chat | Card with chat run/onPause; Pause absent, Stop present | SteeringSurface forwarding identical run/onPause/onStop to real RunControls; same selectors | Unpausable chat still stoppable; issue Pause positive countercase retained. |
| Pause menu: interactive task | Card with interactive task; Pause absent | Same props through SteeringSurface | Interactive kind predicate remains covered; issue positive countercase retained. |
| Pause menu: omitted run/onPause | Card alone, Pause absent; title pre-1190 caller unchanged | Card alone remains; truthful title no Pause or Stop and header pointer; add Stop absent and pointer present | Pins header-only controls; actual header Pause/Stop positives make absence checks falsifiable. |
| Pause menu: pending pause | Card with pause_requested_at; Pause absent | Same props through SteeringSurface | Pending pause suppresses duplicate Pause, against live issue positive case. |
| Pause menu: issue milestone item | Card with 3 milestones / m1 done; open Pause; milestone item present, step absent, Pause now present | Same props and open action through SteeringSurface | Same live menu, milestone terminology and both options preserved. |
| Pause menu: no frozen milestones | Card with prompt/null milestones; open Pause; step present and milestone absent | Same props/action through SteeringSurface | No-frozen fallback remains covered, opposite milestone case supplies positive. |
| Pause menu: final milestone | Card with m1/m2 done of 3; open Pause; milestone and step absent, Pause now present | Same props/action through SteeringSurface | Final-milestone hiding is checked against real menu and both earlier positives. |
| Pause menu: checkpoint age | Card with checkpoint 9 minutes ago; open Pause; age-shaped copy and 9m present | Same props/action through SteeringSurface | Checkpoint age fact unchanged. |
| Pause menu: no checkpoint | Card with null checkpoint; open Pause; exact no-age copy | Same props/action through SteeringSurface | No checkpoint does not invent age. |
| Pause menu: callback/close | Card with onPause spy; open Pause, choose Pause now; called with now, menuitem absent afterward | Same callback/action through SteeringSurface | Actual header callback receives original mode; menu closes, with prior menuitem positive. |
| JobResultPanel: job chrome | Card alone issue -> job rerender; textbox issue positive/job absent, no-follow-up note, Stop job positive/Stop run absent | Chrome helper uses actual RunControls header plus Card; same original issue/job fixtures; same selectors plus issue Stop run positive and job header pointer positive | Job takes no composer, names Stop job, and lacks Stop run; real issue Stop run proves negative is falsifiable. Current copy remains Stop job; exact M2 copy pending. |
| RunSummary: intent card | Fixture default awaiting_approval, assert body heading/intent directly | Explicit running fixture; click actual Expand; same body assertions | Past-gate default collapsed and expanded intent body reachable; gate default still covered separately. |
| RunSummary: Approved plan | Running fixture, assert body directly | Click Expand, same Approved positive/Proposed negative | Proposed-vs-Approved retained; gate Proposed positive/Approved negative unchanged. |
| RunSummary: null deltas | Running fixture, assert no-deviations directly | Click Expand; same no-deviations assertion | Null deltas handled without crash. |
| RunSummary: malformed non-array deltas | Running fixture, assert no-deviations directly | Click Expand; same no-deviations assertion | Tolerate-on-read retained; tagged array cases unchanged. |
| RunSummary: hostile markdown | Fixture default awaiting_approval, inspect body directly | Explicit running fixture; click Expand; unchanged code/strong/script/inert-text/bidi assertions | Actual expanded markdown still formats safely; body positive controls keep safety negatives meaningful. |
| RunSummary: collapse/remount | Intent-only fixture; default expanded intent positive, Collapse; intent absent; remount intent absent and Expand present | Explicit running fixture with intent plus body plan/delta; Expand before body positives; Collapse; excerpt stays positive, body heading/plan/delta absent; remount same id at awaiting_approval, same excerpt/body negatives and Expand positive; Expand and verify all body positives | Body collapse plus per-run persistence retained. Gate remount would default expanded without saved true, so persisted collapse is falsifiable; plan/delta negatives have positives before and after. Intent disappearance migrated to body disappearance plus retained excerpt. |
| DeliveredCard summary host: delivered-only | Assert Delivered region immediately; Collapse -> absent; Expand -> present | Initial Expand added; all original region/collapse assertions unchanged | Delivered lives inside real summary body, collapses and reopens; both negatives have positive region countercase. |
| DeliveredCard summary host: outcome not published | Assert failed-write note and no Delivered region directly | Initial Expand added; unchanged note/region assertions | Failure note remains reachable without falsely claiming publication; published-region positive in adjacent test retained. |

Card-only terminal-empty, non-owner-empty, explicit owner, omitted canSteer, and parked follow-up tests retain original component setup and all assertions. Delivery and scope-state tests are unchanged. RunSummary empty rendering, gate Proposed plan, tagged deltas, and empty-array no-deviations tests are unchanged. DeliveredCard direct tests are unchanged.

Added coverage in the authorized SteerQueueCard file: busy disables actual Pause/Stop and suppresses callbacks/menu; actual Stop calls original callback; actual controls have positive owner/live countercase and vanish for terminal/non-owner. This adds 3 tests, deletes none. Two-case guard loop has exactly two synchronous rerenders; a failing assertion fails the test.

## Recorded checks

All commands use scratch mktemp logs, record EXIT, and finish synchronously. TZ=UTC for tests. No giant RunView execution: -t restricts it to its 10 RunSummary tests (350 skipped).

- `TZ=UTC npx --prefix web vitest run --root web src/lib/prefs.test.ts src/components/SteerQueueCard.test.tsx src/pages/runView/DeliveredCard.test.tsx src/pages/runView/JobResultPanel.test.tsx`
  - gate-log.lvnH8j: EXIT=1; 107 passed, 2 failed due to empty helper header in card-only emptiness cases. Corrected by restoring those two Card-only setups. prefs 18, DeliveredCard 32, JobResultPanel 11 passed and were not edited afterward.
- From web: `TZ=UTC npx vitest run src/pages/RunView.test.tsx -t "RunSummary \\(PRD #362 M4\\)"`
  - gate-log.I9OTGT: EXIT=1; 7 passed, 3 setup failures because fixture default is awaiting_approval (already expanded). Corrected to explicit running in those 3 cases.
  - gate-log.IKm0Sk: EXIT=0; 10 passed, 350 skipped (before strengthened gate-remount persistence setup).
- From web: `TZ=UTC npx vitest run src/components/SteerQueueCard.test.tsx`
  - gate-log.d3UuMr: EXIT=0; 48 passed.
- From web: `TZ=UTC npx vitest run src/components/SteerQueueCard.test.tsx src/pages/RunView.test.tsx -t "SteerQueueCard|RunControls — Pause|RunSummary \\(PRD #362 M4\\)"`
  - gate-log.rhiu4W: EXIT=0; 58 passed, 350 skipped, including final gate-remount persistence setup and restored card-only owner/parked setups. Only subsequent edit was helper's status type annotation.
- From web: `npm run typecheck`
  - gate-log.FEY3Yb: EXIT=1; TS2322 in test helper status assignment. Constrained helper status prop to RunStatus.
  - gate-log.y6HRqI: EXIT=0 on final test source.

Combined affected-scope evidence: 119 passing tests (18 prefs + 48 SteerQueueCard/RunControls + 32 DeliveredCard + 11 JobResultPanel + 10 RunSummary). No remaining focused failures. Full integration gate and mutation proof remain for main; this does not claim M1/M2/M3 complete.

## Gate failure comparison and remaining integration work

The first full web gate on 986e2178 returned exit 201: 19 failed, 6625 passed. Its five failing files concerned the seeded prototype's controls and summary preference/body defaults. A narrowed equivalent selection failed five tests on that candidate and passed six on default-branch base 2bf9a00e (same lockfile, dependencies and TZ). These were task-owned branch regressions, not unrelated failures. Test-only maintenance preserved the behaviors on the already-seeded components. The final full task gate:web passed at b05cc1b7 (source identical to the code SHA above), exit 0, including browser tests (14 tests across four files). task gate:repo at 60e5ccc1 passed, exit 0, including canary-checked secret and Semgrep scans. npm run build from web at 60e5ccc1 passed, exit 0, with the existing large-chunk warning. Full web totals: 6659 unit tests and 14 browser tests passed.

Focused lead checks passed, including 34 stage/row regressions and the docs-mirror drift test with -count=1; gofmt -l returned no paths for the four comment-only Go files. Later full-gate results supersede focused totals rather than implying those totals were full-suite results.

## Browser evidence

[Light running-card comparison](2789-m1-progress-parity-light.png) · [Dark running-card comparison](2789-m1-progress-parity-dark.png). Captured with the backend-free run-progress mock scenario in real Chromium. The implementation has one four-row list, reported count, active phase/iteration/run-wide stage, Now note/age and two live lanes; no page errors occurred.

The reference capture applied a capture-only [hidden] display rule: the mock's author display styles otherwise override HTML hidden attributes and show several state examples simultaneously. The reference file was not changed. App palette, row marks and lane typography use existing components; sample titles, model-written note, ages and usage values differ as example data. The added run-wide stage is required by PRD D14. M2/M3/M4 and the four-state final screenshot matrix remain pending.
