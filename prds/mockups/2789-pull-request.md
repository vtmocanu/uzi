Opening a run currently means reconciling duplicate milestone lists, conflicting count/phase wording and controls below a long feed. #2789 puts identity and lifecycle controls in the header, owner attention first, and progress, live work, Now and cost in one card.

The Now prompt also describes intended work from titles and role labels while preserving its one-sentence output and usage accounting.

## Behavior

- One progress card renders each frozen milestone once. The head reads ≈N% · k of M reported complete; never-reported completion reads – of M reported while retaining server percent. Counts use milestoneBadge/frozen membership; percent and active id retain server meanings.
- Phase, iteration and labelled run-wide stage move beside active work; accessible track information survives removal of repeated labels. Now attaches by active_milestone_id or remains unattached if no row matches. Effective attribution, repeated-role suppression, declared-owner/live-lane dedup and quiet Assigned owner strips remain. Untrusted titles, ids, lanes, activity and Now stay sanitized plain text.
- Header-only Pause/Stop share the extracted production predicate and owner/busy/terminal gates. Jobs say Stop this job. The composer/queue remains below with a header pointer and existing receipts/follow-up behavior. Run settings exposes token switching, every recorded epoch including one pin/time, and applicable toggles. Effective nondefault states remain neutral header chips; parked waits remain attention panels. Keyboard/focus and responsive nested pickers are covered; reduced-motion Spinner starts no interval.
- Existing approval/question/decision/hold/terminal panels occupy the attention slot before progress; input history follows the slot. Plan/hero content and handlers remain. Optimistic steering does not relax the separate confirmed-owner CodeCrossCheck gate.
- The footer shares the usage derivation and cost helpers: confirmed/live/side-pass/estimated-tail semantics match the full panel, cache percentages stay honest, an unpriced tail reads + unpriced tail, and Full usage appears only with the actual panel.
- Saved collapse=true/false wins; absent/expired/corrupt choices apply the gate-state default, including gate exit without remount. Collapse retains a parsed, sanitized plain intent excerpt; Expand restores the full body.
- Both Now harnesses use the same work-focused prompt: title/role/label describe intent, leading [mN] tags are ignored, success and next steps are not inferred, and tool names are a fallback. Captured-prompt tests cover both controller paths and matching inert JSON fences. Runtime redaction, the 120-character result cap, command exclusion, triggers and accounting are preserved. These tests prove instructions; live wording remains a maintainer spot-check.

## Scope and review boundaries

Design/decisions: [PRD](../done/2789-run-page-unified-progress.md), [interactive mock](2789-run-page-unified-progress-mock.html). The approved implementation plan governs scope, the revised PRD governs behavior, and the mock governs unspecified presentation.

Target-default cumulative review base: 2bf9a00e6eac5e3989097bdb7167cd3143a092df. Published presentation prototype floor: 3bef77073807e55d01e097f7f60a43d583d6ee21, followed by lead/integration edits. The default branch was merged, preserving published history. The published prototype was treated as unreviewed starting code, not completed milestones. Source reviews cover the whole branch from the target merge base, including kept prototype code, within each owner's scope.

M1 proof/source review: bffcbb77e92b4a58937cd0cc114585a39f574b83. M2 proof/source review: 630523ce8775dd2e7131b8cc1623b916c4b6e0f2; later CSS-only nested-picker repair e5d39e35107c025e4b1d687511d72a7412db507b received clean cumulative reviewer/tester/auditor reports and shipping browser coverage. The cancelled first M2 tester has no verdict; the fresh tester does. M3 source: f2083e525eb1caddddcd0aceee92652ededf4b2f, unit base df805bdac1eeafc920639040bf4d80a9dc472b47; cumulative M3-owned review starts at 2bf9a00e, not the prototype. Evidence review covers f2083e52..df1b5d28; named-pass evidence is committed at cefffcabe77385341f214275d682a0ffa0950914.

M4 source is reviewed at 9255f834199f4f7a9d44cef96529da210a2ec53f. Later committed evidence, stale demo-documentation correction and PRD lifecycle receive their own immutable-range review. Final shipping head is recorded by the run at submission; cumulative coverage includes the entire prototype and lead work within each reviewed module.

This change introduces no API/DTO/schema/migration, server progress rule, pricing, workflow or dependency change. CLI/TUI layout stays out of scope; M1 changes their comments only and their milestone k of M wording deliberately remains. Board/runs-list/dashboard badge helpers remain. M4 is prompt-only: preserve the model-pass seam, input trimming/fencing/redaction, triggers, gating and accounting. Inherited limitations remain: Claude accumulates model text with no local accumulation cap; Codex retains its existing 8 MiB bound and does not forward maxOutputTokens. The retained 120-character result cap does not imply equal harness transport/output-token bounds. No harness hardening is added here.

## Mock deviations

The [M3 permanent report](2789-m3-validation.md) records necessary differences:

- Existing full approval/hero content, controls and semantic app styling remain rather than the mock's shortened panels. Sample identity/model/token/time/ages/usage differ; scenario messages supply usage, and the approval fixture has no usage footer to invent.
- Terminal progress is null: frozen reported rows remain without fabricated terminal progress state or active segment. The plan segment retains its plan colour and completed rows retain strike-through.
- The mock's simplified summary becomes production Expand/Collapse plus the safe plain excerpt. Run-wide stage and effective toggle chips preserve D14 information; owners read Assigned owner rather than an unsupported idle claim.
- Reference captures apply a capture-only [hidden] rule because the mock's author display styles otherwise reveal multiple example states; reference source is unchanged.

Dropped prototype behaviors: duplicate milestone/Active role surfaces, active-ordinal count wording, implied phase sequence, redundant header count/iteration/stage when shown by the card, whole-card disappearance despite frozen rows, generic side-only “in flight” cost copy and ambiguous unpriced-tail wording. Saved-choice/excerpt migrations and real header-only control tests replace prototype shortcuts while retaining the old pinned facts described below.

Permanent four-state sheets compare running, awaiting approval, failed and completed with the mock: [light](2789-run-page-parity-light.png), [dark](2789-run-page-parity-dark.png). Eight real Chromium pages had zero page errors; terminal fixtures had no Now/live lanes. Corrected full-approval captures supersede the earlier crop. Narrow-view behavior is proved separately by shipping M2 browser tests, not inferred from desktop sheets: [M2 light](2789-m2-header-parity-light.png), [M2 dark](2789-m2-header-parity-dark.png). Earlier card comparisons: [M1 light](2789-m1-progress-parity-light.png), [M1 dark](2789-m1-progress-parity-dark.png).

## Recorded verification

These are recorded lead executions at their tested revisions; later documentation does not imply the full suites were rerun at that later head.

| Evidence | Ref | Recorded result |
| --- | --- | --- |
| M1 focused falsification | bffcbb77 | 37 valid breaks: typecheck 0, intended focused failure 1; unmodified 185 tests, exit 0; 65 replacements covered |
| M1 full web | b05cc1b7, source identical to proof SHA | exit 0; 6,659 unit + 14 browser tests |
| M1 repo/build | 60e5ccc1 | both exit 0; secret/Semgrep canaries detected; existing large-chunk warning |
| M2 focused falsification | 630523ce | 18 valid breaks: typecheck 0, intended focused failure 1; unmodified 89 tests, exit 0; 12 replacements covered |
| M2 full web/build/repo | e5d39e35 | exit 0; 6,694 unit + 19 browser tests; responsive controls/pickers at 320/390/768/1024/1280px |
| M3 full web | f2083e52 | exit 0; 6,748 unit in 332 files + 19 browser tests in five files |
| M3 production build | f2083e52 | exit 0; existing large-bundle warning, no splitting claim |
| M3 docs sync/mirror | f2083e52 source checkpoint | docs:sync 0; uncached TestEmbeddedDocsMatchSource with -count=1, exit 0 |
| M3 focused falsification | f2083e52 | 13 valid breaks: typecheck 0, intended failure 1; unmodified typecheck/test 0 before and after, 72 cases |
| M3 final repo gate | df1b5d2874db1d77ad311ef5959b3b83dd05a241 | exit 0; secret/Semgrep canaries detected, zero tree findings/errors |
| Final M3 docs hygiene | cefffcab | docs check, token-literal guard and canary secrets scan exit 0 (bW88xa); named-pass source still f2083e52 |
| M4 focused and falsification | 9255f834 | 79 passed; 23 archived breaks typecheck 0 / named failure 1; fresh unmodified 79 passed afterward, zero skipped |
| M4 full agent | 9255f834 | gate exit 0; 12,986 unit passed / 14 conditional skipped / zero failed; 183 Codex conformance passed and required clause matrix satisfied |

Repo gates explicitly skipped workflow lint because actionlint was unavailable; triage rendering was Bash-only because zsh was unavailable. No workflow file changed. Four M1 CLI/TUI comment files had empty gofmt -l output, exit 0.

[Permanent M1 report](2789-m1-validation.md), [M2 report](2789-m2-validation.md), [M3 report](2789-m3-validation.md) preserve source mutations and actual failures. Source verdicts are cumulative within owned scope and original SHAs, not live API/model-output proof. M3 source reviewer/tester/auditor ran 115/680/197 focused tests respectively with other checks itemized in the report. M3 evidence reviewers found no mandatory evidence/security issue. Tester suggestions at df1b5d28 were named passing output, coverage thresholds and a pre-commit gate: the instrumentation gap is closed at ceff by matching all 29 named failure occurrences to explicit passes among 72 tests (zero skipped). The two coverage/pre-commit infrastructure suggestions remain deferred. Earlier M1/M2 formatter/coverage suggestions likewise remain deferred under the approved existing-gates/no-workflow-change scope; no new threshold or workflow is claimed.

M3 has **zero old→new migrations and 54 new cases** in two added suites; pre-existing tests are unchanged against its unit base. The 72-case proof selection includes existing prefs tests and is not 72 new cases. M4 has zero migrations and nine new tests; every original test is unchanged.

## Exhaustive old→new assertion inventory

This body contains the complete inventory: M1 35 groups, 47 old statements → 65 replacements; M2 11 groups, 11 old statements → 12 replacement assertions/selectors. Repeated statements are distinct sites/branches and intentionally retained.

Locations use extraction baselines and proof heads, not the current moving tree:

- M1 OLD = merged-prototype 41504df5579fa17589721eff9c440b3101ba3677; NEW = bffcbb77e92b4a58937cd0cc114585a39f574b83.
- M2 OLD = M1 checkpoint f95ad5def64e1fb5e03e5cc2dee73a5678104f69; NEW = 630523ce8775dd2e7131b8cc1623b916c4b6e0f2.
- Each NEW site lists replacement-specific break IDs from coverage, with M2 runtime-branch matching. Each listed break has typecheck 0 / intended focused failure 1 / unmodified 0 (185 M1 / 89 M2). These are focused runs, not full-suite reruns per break.

Both proof-results uncovered arrays are empty. Inventory uncovered:true is stale pre-proof extraction metadata, not actual missing coverage. Fresh immutable archives used matching provisioned dependencies and checked source anchors. The corrected assertion collector records immediate expect.soft failures so later unchanged predicates execute with normal cleanup. Earlier throwing-afterEach evidence was discarded and rerun. Missing/ambiguous M2 getByRole initializers still throw with exact unique existence semantics; no fabricated UI or weak query result is substituted. M2 resolves per-statement targetFile before group targetFile before original file, and testFilter distinguishes terminal/non-owner/original queued-job branches. Line-only matching would misattribute parameter failures.

### M1 migration sites

#### M1-A01

Pinned fact: Collapse hides and persists the full body across a gate-state remount; the safe intent excerpt remains and Expand restores the body.

Test OLD: collapses on toggle and remembers the choice per run across a remount

Test NEW: collapses on toggle and remembers the choice per run across a remount

OLD `web/src/pages/RunView.test.tsx:827`:
```tsx
expect(screen.queryByText("Some intent.")).toBeNull();
```

OLD `web/src/pages/RunView.test.tsx:831`:
```tsx
expect(screen.queryByText("Some intent.")).toBeNull();
```

NEW `web/src/pages/RunView.test.tsx:834` — breaks: summary-expanded-body; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("What this run will implement")).toBeTruthy();
```

NEW `web/src/pages/RunView.test.tsx:835` — breaks: summary-expanded-body; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("Some intent.")).toBeTruthy();
```

NEW `web/src/pages/RunView.test.tsx:836` — breaks: summary-expanded-body; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("Body plan.")).toBeTruthy();
```

NEW `web/src/pages/RunView.test.tsx:837` — breaks: summary-expanded-body; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("Body delta.")).toBeTruthy();
```

NEW `web/src/pages/RunView.test.tsx:839` — breaks: summary-collapse-body, summary-collapse-excerpt, summary-expanded-body; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("Some intent.")).toBeTruthy();
```

NEW `web/src/pages/RunView.test.tsx:840` — breaks: summary-collapse-body, summary-expanded-body; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.queryByText("What this run will implement")).toBeNull();
```

NEW `web/src/pages/RunView.test.tsx:841` — breaks: summary-collapse-body, summary-expanded-body; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.queryByText("Body plan.")).toBeNull();
```

NEW `web/src/pages/RunView.test.tsx:842` — breaks: summary-collapse-body, summary-expanded-body; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.queryByText("Body delta.")).toBeNull();
```

NEW `web/src/pages/RunView.test.tsx:846` — breaks: summary-collapse-body, summary-collapse-excerpt, summary-expanded-body; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("Some intent.")).toBeTruthy();
```

NEW `web/src/pages/RunView.test.tsx:847` — breaks: summary-collapse-body, summary-collapse-persistence, summary-expanded-body; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.queryByText("What this run will implement")).toBeNull();
```

NEW `web/src/pages/RunView.test.tsx:848` — breaks: summary-collapse-body, summary-collapse-persistence, summary-expanded-body; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.queryByText("Body plan.")).toBeNull();
```

NEW `web/src/pages/RunView.test.tsx:849` — breaks: summary-collapse-body, summary-collapse-persistence, summary-expanded-body; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.queryByText("Body delta.")).toBeNull();
```

#### M1-A02

Pinned fact: Paused runs retain frozen rows without a live Now strip; presence selects the actual list.

Test OLD: does NOT render the live 'now' strip on a PAUSED run

Test NEW: does NOT render the live 'now' strip on a PAUSED run

OLD `web/src/pages/RunView.test.tsx:1744`:
```tsx
expect(screen.getByText(/reported complete/i)).toBeTruthy();
```

NEW `web/src/pages/RunView.test.tsx:1767` — breaks: rows-missing; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(container.querySelector("[data-run-progress-card] ul")).not.toBeNull();
```

#### M1-A03

Pinned fact: Frozen membership reports 3 complete of 7, rather than active ordinal.

Test OLD: renders M{done}/{total} for a milestone-structured run

Test NEW: renders reported counts for a milestone-structured run

OLD `web/src/pages/RunView.test.tsx:5647`:
```tsx
expect(screen.getByText("M3/7")).toBeTruthy();
```

NEW `web/src/pages/RunView.test.tsx:5673` — breaks: count-offset; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("3 of 7 reported complete")).toBeTruthy();
```

#### M1-A04

Pinned fact: Never-reported completion stays neutral rather than claiming zero.

Test OLD: renders M–/N (not a 0/N) when the tracker was never reported (PRD #265 M2)

Test NEW: renders – of N reported (not a reported zero) when the tracker was never reported (PRD #265 M2)

OLD `web/src/pages/RunView.test.tsx:5657`:
```tsx
expect(screen.getByText("M–/4")).toBeTruthy();
```

OLD `web/src/pages/RunView.test.tsx:5658`:
```tsx
expect(screen.queryByText("M0/4")).toBeNull();
```

NEW `web/src/pages/RunView.test.tsx:5683` — breaks: unreported-positive; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("– of 4 reported")).toBeTruthy();
```

NEW `web/src/pages/RunView.test.tsx:5684` — breaks: unreported-negative; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.queryByText("0 of 4 reported complete")).toBeNull();
```

#### M1-A05

Pinned fact: An explicitly reported empty set remains genuine zero.

Test OLD: still renders M0/N when an empty completion set WAS reported (genuine zero)

Test NEW: still renders 0 of N reported complete when an empty completion set WAS reported (genuine zero)

OLD `web/src/pages/RunView.test.tsx:5663`:
```tsx
expect(screen.getByText("M0/4")).toBeTruthy();
```

NEW `web/src/pages/RunView.test.tsx:5689` — breaks: count-offset; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("0 of 4 reported complete")).toBeTruthy();
```

#### M1-A06

Pinned fact: The completed row mark stays accessible and honestly labelled reported complete.

Test OLD: renders every title with a done / in-progress / left indicator

Test NEW: renders every title with a done / in-progress / left indicator

OLD `web/src/pages/RunView.test.tsx:5694`:
```tsx
expect(screen.getByLabelText("done")).toBeTruthy();
```

NEW `web/src/pages/RunView.test.tsx:5720` — breaks: mark-completed, rows-missing; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByLabelText("reported complete")).toBeTruthy();
```

#### M1-A07

Pinned fact: The known 1-of-3 completion remains explicitly reported, not verified.

Test OLD: says 'reported complete' and NEVER implies verification

Test NEW: says 'reported complete' and NEVER implies verification

OLD `web/src/pages/RunView.test.tsx:5703`:
```tsx
expect(screen.getByText(/reported complete/i)).toBeTruthy();
```

NEW `web/src/pages/RunView.test.tsx:5729` — breaks: count-offset; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("1 of 3 reported complete")).toBeTruthy();
```

#### M1-A08

Pinned fact: The count stays clamped to frozen membership; foreign ids cannot inflate 2 of 3.

Test OLD: shows a done/total count that is clamped to frozen membership

Test NEW: shows a done/total count that is clamped to frozen membership

OLD `web/src/pages/RunView.test.tsx:5710`:
```tsx
expect(screen.getByText("2/3")).toBeTruthy();
```

NEW `web/src/pages/RunView.test.tsx:5736` — breaks: count-offset, count-foreign-members; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("2 of 3 reported complete")).toBeTruthy();
```

#### M1-A09

Pinned fact: An unreported three-row list stays neutral and excludes reported zero.

Test OLD: shows '–/N' (not '0/N') in the header when completion was never reported (PRD #265 M2)

Test NEW: shows '–/N' (not '0/N') in the header when completion was never reported (PRD #265 M2)

OLD `web/src/pages/RunView.test.tsx:5720`:
```tsx
expect(screen.getByText("–/3")).toBeTruthy();
```

OLD `web/src/pages/RunView.test.tsx:5721`:
```tsx
expect(screen.queryByText("0/3")).toBeNull();
```

NEW `web/src/pages/RunView.test.tsx:5746` — breaks: unreported-positive; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("– of 3 reported")).toBeTruthy();
```

NEW `web/src/pages/RunView.test.tsx:5747` — breaks: unreported-negative; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.queryByText("0 of 3 reported complete")).toBeNull();
```

#### M1-A10

Pinned fact: A reported empty three-row list shows genuine zero.

Test OLD: shows a genuine '0/3' when an empty completion set WAS reported

Test NEW: shows a genuine '0/3' when an empty completion set WAS reported

OLD `web/src/pages/RunView.test.tsx:5728`:
```tsx
expect(screen.getByText("0/3")).toBeTruthy();
```

NEW `web/src/pages/RunView.test.tsx:5754` — breaks: count-offset; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("0 of 3 reported complete")).toBeTruthy();
```

#### M1-A11

Pinned fact: m2 retains its in-progress mark; fallback coder activity attaches to m2 alone, not m3.

Test OLD: shows the `◐ <firstInProgress>` header marker for m2 (unchanged, non-goal for M5)

Test NEW: marks m2 in progress and attaches fallback activity only to the first row

OLD `web/src/pages/RunView.baseline.test.tsx:169`:
```tsx
expect(screen.getByText(/◐\s*m2/)).toBeTruthy();
```

OLD `web/src/pages/RunView.baseline.test.tsx:171`:
```tsx
expect(screen.queryByText(/◐\s*m3/)).toBeNull();
```

NEW `web/src/pages/RunView.baseline.test.tsx:169` — breaks: mark-in-progress, rows-missing, row-id; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(within(screen.getByText("m2").closest("li") as HTMLElement).getByLabelText("in progress")).toBeTruthy();
```

NEW `web/src/pages/RunView.baseline.test.tsx:171` — breaks: fallback-sibling-negative, rows-missing, row-id; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(within(screen.getByText("m3").closest("li") as HTMLElement).queryByText("coder")).toBeNull();
```

NEW `web/src/pages/RunView.baseline.test.tsx:172` — breaks: fallback-first-positive, rows-missing, row-id; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(within(screen.getByText("m2").closest("li") as HTMLElement).getByText("coder")).toBeTruthy();
```

#### M1-A12

Pinned fact: The active strip remains under the marked m2 row; redundant header id copy retires.

Test OLD: renders the active strip under the in-progress row and names the id in the header

Test NEW: renders the active strip under the marked in-progress row

OLD `web/src/pages/RunView.nowline.test.tsx:117`:
```tsx
expect(screen.getByText(/◐\s*m2/)).toBeTruthy();
```

NEW `web/src/pages/RunView.nowline.test.tsx:117` — breaks: mark-in-progress, rows-missing, row-id; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(within(screen.getByText("m2").closest("li") as HTMLElement).getByLabelText("in progress")).toBeTruthy();
```

#### M1-A13

Pinned fact: No declared in-progress row means no in-progress mark; existing unattached-strip predicates remain.

Test OLD: renders an UNATTACHED strip under the header when nothing is declared in progress

Test NEW: renders an UNATTACHED strip under the header when nothing is declared in progress

OLD `web/src/pages/RunView.nowline.test.tsx:132`:
```tsx
expect(screen.queryByText(/◐/)).toBeNull();
```

NEW `web/src/pages/RunView.nowline.test.tsx:132` — breaks: mark-unattached-negative; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.queryByLabelText("in progress")).toBeNull();
```

#### M1-A14

Pinned fact: The m2 mark survives with no activity; existing no-strip predicates remain.

Test OLD: renders NO strip when there is no activity (a milestone run with nothing acting)

Test NEW: renders NO strip when there is no activity (a milestone run with nothing acting)

OLD `web/src/pages/RunView.nowline.test.tsx:145`:
```tsx
expect(screen.getByText(/◐\s*m2/)).toBeTruthy();
```

NEW `web/src/pages/RunView.nowline.test.tsx:145` — breaks: mark-in-progress, rows-missing, row-id; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(within(screen.getByText("m2").closest("li") as HTMLElement).getByLabelText("in progress")).toBeTruthy();
```

#### M1-A15

Pinned fact: Frozen-order first m2 receives fallback activity, not sibling m3; marks and strip attribution retain separate meanings.

Test OLD: D4: with several ids in progress, only the FIRST by frozen order is named and carries the strip

Test NEW: D4: with several ids in progress, only the FIRST by frozen order is named and carries the strip

OLD `web/src/pages/RunView.nowline.test.tsx:183`:
```tsx
expect(screen.getByText(/◐\s*m2/)).toBeTruthy();
```

OLD `web/src/pages/RunView.nowline.test.tsx:184`:
```tsx
expect(screen.queryByText(/◐\s*m3/)).toBeNull();
```

NEW `web/src/pages/RunView.nowline.test.tsx:183` — breaks: mark-in-progress, rows-missing, row-id; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(within(screen.getByText("m2").closest("li") as HTMLElement).getByLabelText("in progress")).toBeTruthy();
```

NEW `web/src/pages/RunView.nowline.test.tsx:184` — breaks: fallback-sibling-negative, rows-missing, row-id; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(within(screen.getByText("m3").closest("li") as HTMLElement).queryByText("coder")).toBeNull();
```

NEW `web/src/pages/RunView.nowline.test.tsx:185` — breaks: fallback-first-positive, rows-missing, row-id; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(within(screen.getByText("m2").closest("li") as HTMLElement).getByText("coder")).toBeTruthy();
```

#### M1-A16

Pinned fact: DTO progress contains the sole frozen list and Second title; a separate following checklist retires.

Test OLD: renders the card from run.progress, above the milestone checklist

Test NEW: renders the card from run.progress with the sole frozen list

OLD `web/src/pages/RunView.budget.test.tsx:333`:
```tsx
expect(screen.getByText("milestone 2 of 2 · Second")).toBeTruthy();
```

OLD `web/src/pages/RunView.budget.test.tsx:336`:
```tsx
expect(cardEl.compareDocumentPosition(checklist) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
```

NEW `web/src/pages/RunView.budget.test.tsx:333` — breaks: count-offset; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("1 of 2 reported complete")).toBeTruthy();
```

NEW `web/src/pages/RunView.budget.test.tsx:336` — breaks: rows-missing; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(rows).not.toBeNull();
```

NEW `web/src/pages/RunView.budget.test.tsx:337` — breaks: rows-missing, rows-duplicate; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(container.querySelectorAll("ul")).toHaveLength(1);
```

NEW `web/src/pages/RunView.budget.test.tsx:338` — breaks: rows-missing; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(rows?.textContent).toContain("Second");
```

#### M1-A17

Pinned fact: Server percent, frozen count, accessible track states/titles and lead activity survive in one list; duplicate Active role retires.

Test OLD: percent: ≈70%, milestone k of M with the active title, the segmented track and the active role

Test NEW: percent: ≈70%, reported count, active-title track and fallback activity

OLD `web/src/components/RunProgressCard.test.tsx:74`:
```tsx
expect(screen.getByText("milestone 3 of 3 · recovery-docs-validation")).toBeTruthy();
```

OLD `web/src/components/RunProgressCard.test.tsx:76`:
```tsx
expect(track.getAttribute("aria-label")).toBe(
      "Plan progress: planning done, 2 of 3 milestones done, milestone 3 (recovery-docs-validation) in progress",
    );
```

OLD `web/src/components/RunProgressCard.test.tsx:80`:
```tsx
expect(segs).toEqual([
      ["done", "plan"],
      ["done", "api-completion-receipt"],
      ["done", "worker-receipt-ordering"],
      ["now", "recovery-docs-validation"],
    ]);
```

OLD `web/src/components/RunProgressCard.test.tsx:86`:
```tsx
expect(screen.getByText("Active role")).toBeTruthy();
```

OLD `web/src/components/RunProgressCard.test.tsx:87`:
```tsx
expect(screen.getByText("lead").closest("dd")?.textContent).toBe("lead · running a gate");
```

NEW `web/src/components/RunProgressCard.test.tsx:74` — breaks: count-offset; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("2 of 3 reported complete")).toBeTruthy();
```

NEW `web/src/components/RunProgressCard.test.tsx:76` — breaks: track-accessibility, active-track-title; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(track.getAttribute("aria-label")).toBe(
      "Plan approved; 2 of 3 milestones reported complete; active milestone: recovery-docs-validation",
    );
```

NEW `web/src/components/RunProgressCard.test.tsx:80` — breaks: track-titles; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(segs).toEqual([
      ["done", "Plan approved"],
      ["done", "api-completion-receipt · reported complete"],
      ["done", "worker-receipt-ordering · reported complete"],
      ["now", "recovery-docs-validation · in progress"],
    ]);
```

NEW `web/src/components/RunProgressCard.test.tsx:86` — breaks: rows-missing, rows-duplicate; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(container.querySelectorAll("ul")).toHaveLength(1);
```

NEW `web/src/components/RunProgressCard.test.tsx:87` — breaks: rows-missing, rows-duplicate, activity-label; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("lead").parentElement?.parentElement?.textContent).toContain("running a gate");
```

#### M1-A18

Pinned fact: Without an active milestone, the reported count stays 1 of 3; existing no-now-segment predicates remain.

Test OLD: names `N of M milestones done` when no milestone is active, with no `now` segment

Test NEW: names `N of M reported complete` when no milestone is active, with no `now` segment

OLD `web/src/components/RunProgressCard.test.tsx:95`:
```tsx
expect(screen.getByText("1 of 3 milestones done")).toBeTruthy();
```

NEW `web/src/components/RunProgressCard.test.tsx:95` — breaks: count-offset; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("1 of 3 reported complete")).toBeTruthy();
```

#### M1-A19

Pinned fact: The selected phase alone is current; explicit wording replaces an implied sequence arrow.

Test OLD: marks the current phase %s with ▸ and only that one

Test NEW: marks the current phase %s and only that one

OLD `web/src/components/RunProgressCard.test.tsx:103`:
```tsx
expect(cur.textContent).toBe(`▸ ${phase}`);
```

NEW `web/src/components/RunProgressCard.test.tsx:103` — breaks: rows-missing, phase-text; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(cur.textContent).toBe(`phase: ${phase}`);
```

#### M1-A20

Pinned fact: The actual current-step element contains that same current phase; redundant stepper caption retires.

Test OLD: marks the current phase %s with ▸ and only that one

Test NEW: marks the current phase %s and only that one

OLD `web/src/components/RunProgressCard.test.tsx:105`:
```tsx
expect(screen.getByText("current phase only, from the active role")).toBeTruthy();
```

NEW `web/src/components/RunProgressCard.test.tsx:105` — breaks: rows-missing, phase-text; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(within(cur).queryByText(/phase: (review|validate|implement)/)?.textContent).toBe(`phase: ${phase}`);
```

#### M1-A21

Pinned fact: Empty/unknown phase has no current-step element or implement text, against real known-phase positives.

Test OLD: hides the phase row when the phase is empty or unknown (PRD D6)

Test NEW: hides the phase row when the phase is empty or unknown (PRD D6)

OLD `web/src/components/RunProgressCard.test.tsx:113`:
```tsx
expect(container.textContent).not.toContain("▸");
```

OLD `web/src/components/RunProgressCard.test.tsx:114`:
```tsx
expect(screen.queryByText("implement")).toBeNull();
```

NEW `web/src/components/RunProgressCard.test.tsx:113` — breaks: unknown-phase-negative; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(container.querySelector('[aria-current="step"]')).toBeNull();
```

NEW `web/src/components/RunProgressCard.test.tsx:114` — breaks: unknown-phase-negative; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.queryByText(/phase: implement/)).toBeNull();
```

#### M1-A22

Pinned fact: The original state/activity parameter matrix still controls actual fallback visibility.

Test OLD: active role line in the %s state (%s): %s

Test NEW: row activity fallback in the %s state (%s): %s

OLD `web/src/components/RunProgressCard.test.tsx:150`:
```tsx
expect(screen.queryByText("Active role") !== null).toBe(shown);
```

NEW `web/src/components/RunProgressCard.test.tsx:149` — breaks: rows-missing, rows-duplicate, activity-label; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.queryByText("running a gate") !== null).toBe(shown);
```

#### M1-A23

Pinned fact: The long title remains available by its title attribute while the head displays honest count.

Test OLD: a long unbroken milestone title cannot widen the card

Test NEW: a long unbroken milestone title cannot widen the card

OLD `web/src/components/RunProgressCard.test.tsx:165`:
```tsx
expect(what.textContent).toBe(`milestone 3 of 3 · ${long}`);
```

NEW `web/src/components/RunProgressCard.test.tsx:164` — breaks: count-offset; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(what.textContent).toBe("2 of 3 reported complete");
```

NEW `web/src/components/RunProgressCard.test.tsx:165` — breaks: rows-missing, rows-duplicate, row-tooltip; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText(long).getAttribute("title")).toBe(long);
```

#### M1-A24

Pinned fact: The active track retains frozen title plus supplied long label; existing truncation/width constraints remain.

Test OLD: a long unbroken milestone title cannot widen the card

Test NEW: a long unbroken milestone title cannot widen the card

OLD `web/src/components/RunProgressCard.test.tsx:171`:
```tsx
expect(label.textContent).toBe(long);
```

NEW `web/src/components/RunProgressCard.test.tsx:171` — breaks: row-id; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(label.textContent).toBe(`recovery-docs-validation${long}`);
```

#### M1-A25

Pinned fact: The actual active segment retains min-w-0 containment after label structure moves.

Test OLD: a long unbroken milestone title cannot widen the card

Test NEW: a long unbroken milestone title cannot widen the card

OLD `web/src/components/RunProgressCard.test.tsx:173`:
```tsx
expect((label.parentElement as HTMLElement).className).toContain("min-w-0");
```

NEW `web/src/components/RunProgressCard.test.tsx:173` — breaks: track-min-width; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(track.querySelector('[data-seg="now"]')?.className).toContain("min-w-0");
```

#### M1-A26

Pinned fact: Frozen rows/count survive absent/null/none/unknown/missing-pct progress without fabricated percent; replaces the approved prototype whole-card-empty shortcut.

Test OLD: renders nothing for null/absent progress, `none`, an unknown state, or a percent without pct

Test NEW: keeps frozen rows for null/absent progress, none, unknown or percent without pct

OLD `web/src/components/RunProgressCard.test.tsx:262`:
```tsx
expect(container.innerHTML).toBe("");
```

NEW `web/src/components/RunProgressCard.test.tsx:259` — breaks: rows-missing, rows-duplicate; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(container.querySelectorAll("li")).toHaveLength(3);
```

NEW `web/src/components/RunProgressCard.test.tsx:260` — breaks: count-offset; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText("2 of 3 reported complete")).toBeTruthy();
```

NEW `web/src/components/RunProgressCard.test.tsx:261` — breaks: missing-percent-negative; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(container.querySelector("[data-progress-pct]")).toBeNull();
```

#### M1-A27

Pinned fact: The possible-dependency link remains an exact-tooltip hint, not recorded dependency.

Test OLD: blocked-by: an info link to /runs/<id> showing the first 8 characters, beside the waiting flag

Test NEW: blocked-by: an info link to /runs/<id> showing the first 8 characters, beside the waiting flag

OLD `web/src/components/RunProgressCard.test.tsx:278`:
```tsx
expect(screen.getByText("open question mentions an issue that run is working")).toBeTruthy();
```

NEW `web/src/components/RunProgressCard.test.tsx:284` — breaks: hint-tooltip; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(link.getAttribute("title")).toBe("A question mentions this run; this is a hint, not a recorded dependency.");
```

#### M1-A28

Pinned fact: Hostile title remains sanitized visible plain text and title attribute; the track drops duplicate visible copy.

Test OLD: strips control and bidi characters from a hostile milestone title and activity, in text and attributes

Test NEW: strips control and bidi characters from a hostile milestone title and activity, in text and attributes

OLD `web/src/components/RunProgressCard.test.tsx:309`:
```tsx
expect(screen.getByText(`milestone 2 of 2 · ${clean}`)).toBeTruthy();
```

OLD `web/src/components/RunProgressCard.test.tsx:311`:
```tsx
expect(now.textContent).toBe(clean);
```

NEW `web/src/components/RunProgressCard.test.tsx:315` — breaks: rows-missing, rows-duplicate, sanitizer-inert-reversal; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText(clean)).toBeTruthy();
```

NEW `web/src/components/RunProgressCard.test.tsx:317` — breaks: rows-missing, rows-duplicate, row-tooltip, sanitizer-inert-reversal; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByText(clean).getAttribute("title")).toBe(clean);
```

NEW `web/src/components/RunProgressCard.test.tsx:318` — breaks: segment-visible-copy; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(now.textContent).toBe("");
```

#### M1-A29

Pinned fact: The accessible track retains sanitized active title.

Test OLD: strips control and bidi characters from a hostile milestone title and activity, in text and attributes

Test NEW: strips control and bidi characters from a hostile milestone title and activity, in text and attributes

OLD `web/src/components/RunProgressCard.test.tsx:313`:
```tsx
expect(screen.getByRole("img").getAttribute("aria-label")).toContain(`(${clean})`);
```

NEW `web/src/components/RunProgressCard.test.tsx:320` — breaks: sanitizer-inert-reversal, active-track-title; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByRole("img").getAttribute("aria-label")).toContain(`active milestone: ${clean}`);
```

#### M1-A30

Pinned fact: Sanitized activity label remains in the real fallback strip; unchanged hostile-text negatives remain.

Test OLD: strips control and bidi characters from a hostile milestone title and activity, in text and attributes

Test NEW: strips control and bidi characters from a hostile milestone title and activity, in text and attributes

OLD `web/src/components/RunProgressCard.test.tsx:318`:
```tsx
expect(dd.textContent).toBe("coder · fix it");
```

NEW `web/src/components/RunProgressCard.test.tsx:325` — breaks: activity-label, activity-clean-label; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(strip.textContent).toContain("fix it");
```

#### M1-A31

Pinned fact: An unmatched active id remains sanitized in the accessible track rather than redundant header text.

Test OLD: falls back to the sanitised id when the active milestone is not in the frozen list

Test NEW: falls back to the sanitised id when the active milestone is not in the frozen list

OLD `web/src/components/RunProgressCard.test.tsx:336`:
```tsx
expect(screen.getByText("active milestone · ghost")).toBeTruthy();
```

NEW `web/src/components/RunProgressCard.test.tsx:343` — breaks: sanitizer-inert-reversal, active-track-title; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(screen.getByRole("img").getAttribute("aria-label")).toContain("active milestone: ghost");
```

#### M1-A32

Pinned fact: Absent preference is distinguishable from saved false so the caller supplies the gate-state default.

Test OLD: defaults to expanded when the run has no stored choice

Test NEW: returns undefined when the run has no stored choice

OLD `web/src/lib/prefs.test.ts:78`:
```tsx
expect(summaryCollapse.getCollapsed("run-1")).toBe(false);
```

NEW `web/src/lib/prefs.test.ts:78` — breaks: preference-absence; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(summaryCollapse.getCollapsed("run-1")).toBeUndefined();
```

#### M1-A33

Pinned fact: Saving run-1 does not manufacture a run-2 choice; saved run-1 true remains pinned.

Test OLD: persists a collapse choice and reads it back after a reload (fresh read, no state)

Test NEW: persists a collapse choice and reads it back after a reload (fresh read, no state)

OLD `web/src/lib/prefs.test.ts:85`:
```tsx
expect(summaryCollapse.getCollapsed("run-2")).toBe(false);
```

NEW `web/src/lib/prefs.test.ts:85` — breaks: preference-absence; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(summaryCollapse.getCollapsed("run-2")).toBeUndefined();
```

#### M1-A34

Pinned fact: Expired entries become absent; fresh sibling, expiration boundary and GC predicates remain.

Test OLD: drops an entry older than 7 days on read (GC) while keeping a fresh sibling

Test NEW: drops an entry older than 7 days on read (GC) while keeping a fresh sibling

OLD `web/src/lib/prefs.test.ts:98`:
```tsx
expect(summaryCollapse.getCollapsed("stale", t0 + 8 * DAY)).toBe(false);
```

NEW `web/src/lib/prefs.test.ts:98` — breaks: preference-absence; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(summaryCollapse.getCollapsed("stale", t0 + 8 * DAY)).toBeUndefined();
```

#### M1-A35

Pinned fact: Corrupt maps do not throw and yield absence, preserving explicit-choice precedence.

Test OLD: returns the default when the stored map is corrupt, without throwing

Test NEW: returns undefined when the stored map is corrupt, without throwing

OLD `web/src/lib/prefs.test.ts:119`:
```tsx
expect(summaryCollapse.getCollapsed("run-1")).toBe(false);
```

NEW `web/src/lib/prefs.test.ts:119` — breaks: preference-absence; typecheck 0 / failure 1 / unmodified 0 (185 tests).
```tsx
expect(summaryCollapse.getCollapsed("run-1")).toBeUndefined();
```

### M2 migration sites

#### M2-A01

Pinned fact: Terminal runs retain the queue but offer no Stop; original receipt/composer assertions remain.

Test OLD: renders the queue read-only (no composer, no Stop) on a terminal run with a non-empty queue

Test NEW: gates real page controls: owner %s status %s

Proof branch filter: `owner true status completed`.

OLD `web/src/components/SteerQueueCard.test.tsx:281`:
```tsx
expect(screen.queryByText("Stop run")).toBeNull();
```

NEW `web/src/pages/RunView.job.test.tsx:176` — breaks: controls-blocked; typecheck 0 / failure 1 / unmodified 0 (89 tests).
```tsx
expect(screen.queryByRole("button", { name: "Stop run" })).toBeNull();
```

Observed branch: `M2 header and settings > gates real page controls: owner true status completed`.

#### M2-A02

Pinned fact: Owned live runs retain the actual composer and reachable header Stop; the body points to the header.

Test OLD: a live run shows the composer + Stop alongside the queue

Test NEW: a live run shows the composer and header pointer alongside the queue

OLD `web/src/components/SteerQueueCard.test.tsx:296`:
```tsx
expect(screen.getByText("Stop run")).toBeTruthy();
```

NEW `web/src/components/SteerQueueCard.test.tsx:280` — breaks: queue-pointer; typecheck 0 / failure 1 / unmodified 0 (89 tests).
```tsx
expect(screen.getByText("Pause or stop this run from the header.")).toBeTruthy();
```

Observed branch: `SteerQueueCard survives the terminal transition (B1) > a live run shows the composer and header pointer alongside the queue`.

NEW `web/src/pages/RunView.job.test.tsx:146` — breaks: controls-owner-positive; typecheck 0 / failure 1 / unmodified 0 (89 tests).
```tsx
const stop = screen.getByRole("button", { name: "Stop run" });
```

Observed branch: `M2 header and settings > places the only lifecycle controls above the real composer, including pending-read optimism`.

#### M2-A03

Pinned fact: Confirmed non-owners have no Stop; original empty-body/composer negatives remain.

Test OLD: a non-owner viewer (canSteer=false, empty queue) renders NOTHING — no heading, no broken Send

Test NEW: gates real page controls: owner %s status %s

Proof branch filter: `owner false status running`.

OLD `web/src/components/SteerQueueCard.test.tsx:330`:
```tsx
expect(screen.queryByText("Stop run")).toBeNull();
```

NEW `web/src/pages/RunView.job.test.tsx:176` — breaks: controls-blocked, controls-owner-positive; typecheck 0 / failure 1 / unmodified 0 (89 tests).
```tsx
expect(screen.queryByRole("button", { name: "Stop run" })).toBeNull();
```

Observed branch: `M2 header and settings > gates real page controls: owner false status running`.

#### M2-A04

Pinned fact: The composer/pointer body has no Stop, paired with real header Stop presence in the same render.

Test OLD: the queue card alone has no Pause or Stop and points to the header

Test NEW: places the only lifecycle controls above the real composer, including pending-read optimism

OLD `web/src/components/SteerQueueCard.test.tsx:492`:
```tsx
expect(screen.queryByRole("button", { name: "Stop run" })).toBeNull();
```

NEW `web/src/pages/RunView.job.test.tsx:154` — breaks: controls-relocated; typecheck 0 / failure 1 / unmodified 0 (89 tests).
```tsx
expect(within(pointer.parentElement!).queryByRole("button", { name: "Stop run" })).toBeNull();
```

Observed branch: `M2 header and settings > places the only lifecycle controls above the real composer, including pending-read optimism`.

#### M2-J1

Pinned fact: An issue retains its actual follow-up textbox within the real composer body.

Test OLD: the header offers Stop job while the steer card has no follow-up composer

Test NEW: places the only lifecycle controls above the real composer, including pending-read optimism

OLD `web/src/pages/runView/JobResultPanel.test.tsx:167`:
```tsx
expect(screen.getByRole("textbox")).toBeTruthy();
```

NEW `web/src/pages/RunView.job.test.tsx:153` — breaks: issue-composer-missing; typecheck 0 / failure 1 / unmodified 0 (89 tests).
```tsx
expect(pointer.parentElement!.contains(screen.getByRole("textbox"))).toBe(true);
```

Observed branch: `M2 header and settings > places the only lifecycle controls above the real composer, including pending-read optimism`.

#### M2-J2

Pinned fact: An issue retains its uniquely accessible Stop run control in the actual header.

Test OLD: the header offers Stop job while the steer card has no follow-up composer

Test NEW: places the only lifecycle controls above the real composer, including pending-read optimism

OLD `web/src/pages/runView/JobResultPanel.test.tsx:168`:
```tsx
expect(screen.getByRole("button", { name: "Stop run" })).toBeTruthy();
```

NEW `web/src/pages/RunView.job.test.tsx:146` — breaks: controls-owner-positive; typecheck 0 / failure 1 / unmodified 0 (89 tests).
```tsx
const stop = screen.getByRole("button", { name: "Stop run" });
```

Observed branch: `M2 header and settings > places the only lifecycle controls above the real composer, including pending-read optimism`.

#### M2-J3

Pinned fact: The original queued job has no follow-up textbox; its running counterpart also executes.

Test OLD: the header offers Stop job while the steer card has no follow-up composer

Test NEW: names the real $status job Stop exactly and retains its pointer without a composer

Proof branch filter: `'queued' job Stop`.

OLD `web/src/pages/runView/JobResultPanel.test.tsx:170`:
```tsx
expect(screen.queryByRole("textbox")).toBeNull();
```

NEW `web/src/pages/RunView.job.test.tsx:166` — breaks: job-composer; typecheck 0 / failure 1 / unmodified 0 (89 tests).
```tsx
expect(screen.queryByRole("textbox")).toBeNull();
```

Observed branch: `M2 header and settings > names the real 'queued' job Stop exactly and retains its pointer without a composer`.

#### M2-J4

Pinned fact: The job retains its exact no-follow-ups explanation.

Test OLD: the header offers Stop job while the steer card has no follow-up composer

Test NEW: names the real $status job Stop exactly and retains its pointer without a composer

Proof branch filter: `'queued' job Stop`.

OLD `web/src/pages/runView/JobResultPanel.test.tsx:171`:
```tsx
expect(screen.getByText("A job takes no follow-ups. Stopping it cancels the job.")).toBeTruthy();
```

NEW `web/src/pages/RunView.job.test.tsx:168` — breaks: job-composer; typecheck 0 / failure 1 / unmodified 0 (89 tests).
```tsx
expect(screen.getByText("A job takes no follow-ups. Stopping it cancels the job.")).toBeTruthy();
```

Observed branch: `M2 header and settings > names the real 'queued' job Stop exactly and retains its pointer without a composer`.

#### M2-J5

Pinned fact: The job retains its header Stop pointer.

Test OLD: the header offers Stop job while the steer card has no follow-up composer

Test NEW: names the real $status job Stop exactly and retains its pointer without a composer

Proof branch filter: `'queued' job Stop`.

OLD `web/src/pages/runView/JobResultPanel.test.tsx:173`:
```tsx
expect(screen.getByText("Stop this job from the header.")).toBeTruthy();
```

NEW `web/src/pages/RunView.job.test.tsx:169` — breaks: job-composer; typecheck 0 / failure 1 / unmodified 0 (89 tests).
```tsx
expect(screen.getByText("Stop this job from the header.")).toBeTruthy();
```

Observed branch: `M2 header and settings > names the real 'queued' job Stop exactly and retains its pointer without a composer`.

#### M2-J6

Pinned fact: The job retains uniquely accessible Stop, renamed exactly Stop this job; queued and running fixtures execute.

Test OLD: the header offers Stop job while the steer card has no follow-up composer

Test NEW: names the real $status job Stop exactly and retains its pointer without a composer

Proof branch filter: `'queued' job Stop`.

OLD `web/src/pages/runView/JobResultPanel.test.tsx:174`:
```tsx
expect(screen.getByRole("button", { name: "Stop job" })).toBeTruthy();
```

NEW `web/src/pages/RunView.job.test.tsx:163` — breaks: controls-owner-positive, job-accessible-copy; typecheck 0 / failure 1 / unmodified 0 (89 tests).
```tsx
const stop = screen.getByRole("button", { name: "Stop this job" });
```

Observed branch: `M2 header and settings > names the real 'queued' job Stop exactly and retains its pointer without a composer`.

#### M2-J7

Pinned fact: The job does not label Stop as Stop run; the real issue positive makes absence observable.

Test OLD: the header offers Stop job while the steer card has no follow-up composer

Test NEW: names the real $status job Stop exactly and retains its pointer without a composer

Proof branch filter: `'queued' job Stop`.

OLD `web/src/pages/runView/JobResultPanel.test.tsx:175`:
```tsx
expect(screen.queryByRole("button", { name: "Stop run" })).toBeNull();
```

NEW `web/src/pages/RunView.job.test.tsx:167` — breaks: job-wrong-extra-stop; typecheck 0 / failure 1 / unmodified 0 (89 tests).
```tsx
expect(screen.queryByRole("button", { name: "Stop run" })).toBeNull();
```

Observed branch: `M2 header and settings > names the real 'queued' job Stop exactly and retains its pointer without a composer`.

### Setup/import/wrapper migrations and preserved facts

M1 tooltip locator setup migrates M–/4 → – of 4 reported and M0/4 → 0 of 4 reported complete. The title predicates are unchanged and separately fail under tooltip-neutral / tooltip-zero (typecheck 0, focused failure 1, unmodified 0). These are setup/getter migrations additional to the 35 assertion groups above.

MilestoneChecklist imports/row-only renders in attribution, baseline and nowline suites now use production MilestoneRows; count/combined-surface renders use actual RunProgressCard. No fake checklist header is added. Unchanged effective-attribution/unique-role live-tool/age predicates, repeated-role suppression, owner/live dedup, quiet assigned-owner strips and sanitization remain; owner-dedup, repeated-role and sanitizer-inert-reversal prove retained predicates. Existing D17 server/badge parity/count source remains.

M1 summary fixtures expand the real body before intent/approved-plan/null or malformed deltas/hostile Markdown assertions. Collapsed full-body negatives have preceding/following positives and a gate-state remount that would otherwise default expanded. DeliveredCard host cases add initial Expand, preserving publication/failure/body visibility predicates. Saved true/false, per-run isolation, fresh/expiry-boundary/GC and safe corrupt-storage facts remain; only absence becomes undefined so caller state supplies the default.

M1's historical SteeringSurface helper used real controls beside the real queue. M2 removes that synthetic page wrapper and job Chrome helper: queue cases render the actual queue, direct Pause cases mount actual RunControls with the same run/busy/terminal/callback values and explicit canSteer, and relocated page facts mount actual RunView. Removed queue onStop/onPause props do not remove receipt/delivery/terminal-survival/composer/read-only/parked-input assertions. Pending/interactive/final-milestone, checkpoint-age/no-checkpoint and callback/menu-close Pause facts remain. The original queued-job fixture remains plus a running counterpart; visible and accessible Stop this job wording and real cancel dispatch are tested. Unchanged Rework-now 409 inline-error/page-alert predicates only open Run settings before the real action.

Real issue-page Stop/textbox positives make terminal/non-owner/job negatives falsifiable. M2-A04 is the scoped composer-body negative (controls-relocated), not unrelated page ownership. M2-A01/A03 share a line but use different runtime owner/status branches above. M2-J3–J7 prove the original queued branch; running counterparts strengthen coverage. No obsolete-copy absence query substitutes for these facts. The migration preserves every pinned behavioral fact.


## Final documentation validation

The repository gate passed at 5d20c1c1 with positive canaries and zero gating findings. That green did not cover a separate docs-check failure: this PR body initially had 13 invalid relative paths (tester check exit 201). The unmodified archive check skipped PRD validation because archives lack `.git`, so its green was inconclusive. A matched diagnostic enabled that one detection guard in three scratch exports: base 9255f834 passed, broken 5d20c1c1 failed with the same 13 link errors, and repair 2ce4d8a8 passed. These diagnostic runs are distinct from the repaired primary-checkout direct docs check, which passed (75 docs, 48 existing advisory warnings). The lifecycle and repair range receive immutable review before submission. No source/test change or new prompt behavior is introduced by that repair.

## M4 evidence and accepted limits

[Permanent M4 proof](2789-m4-validation.md) records each of 23 exact export-only breaks with successful typecheck, named behavioral failure, and unmodified pass. The full agent gate retained 14 existing conditional profile/platform/live-session skips; no Now test was skipped. UID/root/packaged profile, absent ssh-agent and live Anthropic prerequisites were not added or requested. Reviewer found stale current demo wording; both source and embedded copy were corrected. Admin settings does not quote old tool wording and stays unchanged.

The approved M4 boundary is prompt-only: no response-bound expansion. Claude's inherited accumulation limitation is recorded for off-task triage (84ab3fc3); Codex retains its 8 MiB cap but does not forward requested output tokens. Prompt construction is verified, not live-model compliance or arbitrary conflicting-instruction detection. Optional formatter/coverage/pre-commit infrastructure suggestions remain deferred under the approved existing-gates/no-workflow-change scope.
