# #2789 M2 validation and assertion migrations

Historical M2 checkpoint record; current milestone status is tracked in the PRD. Source repairs, focused checks and final integration gates passed; this evidence/lifecycle commit receives its own immutable-range review before checkpoint.

Guard and assertion proof SHA: `630523ce8775dd2e7131b8cc1623b916c4b6e0f2`. CSS-only nested-picker repair: `e5d39e35107c025e4b1d687511d72a7412db507b`; its three sizing classes and browser assertions do not change the guards or the migrated test predicates proved at the earlier SHA. Cumulative M2 source review covers target-default `2bf9a00e` through this repair in its owned modules; M1 has earlier cumulative review. M3 and M4 remain pending.

## Falsification method

Each of 18 deliberate breaks used a fresh immutable-SHA archive in worker scratch, exact matching provisioned dependencies, checked source anchors, typecheck exit 0, and a named focused test failure (exit 1). The unmodified selection passed 89 tests (exit 0). No primary worktree code or test was mutated.

Original expect statements were wrapped only to log their actual predicate failure and record an immediate `expect.soft` failure, allowing later assertions to execute. Throw-on-missing/ambiguous selector initializers logged the original error and rethrew it; their exact/unique selection semantics were preserved. No fabricated test UI, replacement query result, or throwing cleanup hook was used. Normal cleanup remained unchanged. The records include runtime test names, so terminal and non-owner parameter cases can be distinguished.

The first 16 typecheck-valid breaks left two migration predicates uncovered. Two additional breaks detected the original issue-textbox positive and job Stop-run negative; maps were rebuilt from failures at the same SHA. The queue-only migration is mapped to the scoped BODY negative, not the unrelated page ownership gate; controls-relocated fails that actual predicate. No uncovered inventory entry remains.

Selected command (from archive web): `npm exec -- vitest run src/pages/RunView.job.test.tsx src/components/SteerQueueCard.test.tsx src/components/Spinner.test.tsx -t .`; typecheck: `npm run typecheck`. Node 24.20.0, Vitest 4.1.11, TZ=UTC. These are focused mutation runs, not full suites.

## Exhaustive old -> new assertion inventory

Compared M1 checkpoint `f95ad5de` with the guard proof SHA: 11 changed/moved old assertions, 12 equivalent production-page/component predicates. The real page replaces the synthetic header wrappers. Exact lookup initializers remain assertions of unique accessible existence because Testing Library throws when missing or ambiguous. New body attachment/count/callback assertions strengthen the existing facts.

### M2-A01 — Terminal runs retain the queue but offer no Stop; queue receipt/composer assertions remain in the original suite.

Old location: `web/src/components/SteerQueueCard.test.tsx`, test renders the queue read-only (no composer, no Stop) on a terminal run with a non-empty queue.

```typescript
expect(screen.queryByText("Stop run")).toBeNull();
```

New test: gates real page controls: owner %s status %s.

`web/src/pages/RunView.job.test.tsx:176`:

```typescript
expect(screen.queryByRole("button", { name: "Stop run" })).toBeNull();
```

Failed under controls-blocked in M2 header and settings > gates real page controls: owner true status completed: AssertionError: expected <button …(2)></button> to be null; unmodified candidate passed.

### M2-A02 — An owned live run retains a reachable Stop and composer; the body points to the real header control.

Old location: `web/src/components/SteerQueueCard.test.tsx`, test a live run shows the composer + Stop alongside the queue.

```typescript
expect(screen.getByText("Stop run")).toBeTruthy();
```

New test: a live run shows the composer and header pointer alongside the queue.

`web/src/components/SteerQueueCard.test.tsx:280`:

```typescript
expect(screen.getByText("Pause or stop this run from the header.")).toBeTruthy();
```

Failed under queue-pointer in SteerQueueCard survives the terminal transition (B1) > a live run shows the composer and header pointer alongside the queue: TestingLibraryElementError: Unable to find an element with the text: Pause or stop this run from the header.. This could be because the text is broken up by multiple elements. In this case, you can provide a function for your text matcher to make your matcher more flexible.; unmodified candidate passed.

`web/src/pages/RunView.job.test.tsx:146`:

```typescript
const stop = screen.getByRole("button", { name: "Stop run" });
```

Failed under controls-owner-positive in M2 header and settings > places the only lifecycle controls above the real composer, including pending-read optimism: TestingLibraryElementError: Unable to find an accessible element with the role "button" and name "Stop run"; unmodified candidate passed.

### M2-A03 — A confirmed non-owner has no Stop; composer and empty-body negatives remain in the original suite.

Old location: `web/src/components/SteerQueueCard.test.tsx`, test a non-owner viewer (canSteer=false, empty queue) renders NOTHING — no heading, no broken Send.

```typescript
expect(screen.queryByText("Stop run")).toBeNull();
```

New test: gates real page controls: owner %s status %s.

`web/src/pages/RunView.job.test.tsx:176`:

```typescript
expect(screen.queryByRole("button", { name: "Stop run" })).toBeNull();
```

Failed under controls-blocked, controls-owner-positive in M2 header and settings > gates real page controls: owner false status running: AssertionError: expected <button …(2)></button> to be null; unmodified candidate passed.

### M2-A04 — The queue/composer body has no Stop, paired with a real header Stop positive in the same render.

Old location: `web/src/components/SteerQueueCard.test.tsx`, test the queue card alone has no Pause or Stop and points to the header.

```typescript
expect(screen.queryByRole("button", { name: "Stop run" })).toBeNull();
```

New test: places the only lifecycle controls above the real composer, including pending-read optimism.

`web/src/pages/RunView.job.test.tsx:154`:

```typescript
expect(within(pointer.parentElement!).queryByRole("button", { name: "Stop run" })).toBeNull();
```

Failed under controls-relocated in M2 header and settings > places the only lifecycle controls above the real composer, including pending-read optimism: AssertionError: expected <button …(2)></button> to be null; unmodified candidate passed.

### M2-J1 — An issue run retains its actual follow-up textbox.

Old location: `web/src/pages/runView/JobResultPanel.test.tsx`, test the header offers Stop job while the steer card has no follow-up composer.

```typescript
expect(screen.getByRole("textbox")).toBeTruthy();
```

New test: places the only lifecycle controls above the real composer, including pending-read optimism.

`web/src/pages/RunView.job.test.tsx:153`:

```typescript
expect(pointer.parentElement!.contains(screen.getByRole("textbox"))).toBe(true);
```

Failed under issue-composer-missing in M2 header and settings > places the only lifecycle controls above the real composer, including pending-read optimism: TestingLibraryElementError: Unable to find an accessible element with the role "textbox"; unmodified candidate passed.

### M2-J2 — An issue run retains its accessible Stop run control.

Old location: `web/src/pages/runView/JobResultPanel.test.tsx`, test the header offers Stop job while the steer card has no follow-up composer.

```typescript
expect(screen.getByRole("button", { name: "Stop run" })).toBeTruthy();
```

New test: places the only lifecycle controls above the real composer, including pending-read optimism.

`web/src/pages/RunView.job.test.tsx:146`:

```typescript
const stop = screen.getByRole("button", { name: "Stop run" });
```

Failed under controls-owner-positive in M2 header and settings > places the only lifecycle controls above the real composer, including pending-read optimism: TestingLibraryElementError: Unable to find an accessible element with the role "button" and name "Stop run"; unmodified candidate passed.

### M2-J3 — A job has no follow-up textbox.

Old location: `web/src/pages/runView/JobResultPanel.test.tsx`, test the header offers Stop job while the steer card has no follow-up composer.

```typescript
expect(screen.queryByRole("textbox")).toBeNull();
```

New test: names the real $status job Stop exactly and retains its pointer without a composer.

`web/src/pages/RunView.job.test.tsx:166`:

```typescript
expect(screen.queryByRole("textbox")).toBeNull();
```

Failed under job-composer in M2 header and settings > names the real 'queued' job Stop exactly and retains its pointer without a composer: AssertionError: expected <textarea …(3)></textarea> to be null; unmodified candidate passed.

### M2-J4 — A job retains its exact no-follow-ups explanation.

Old location: `web/src/pages/runView/JobResultPanel.test.tsx`, test the header offers Stop job while the steer card has no follow-up composer.

```typescript
expect(screen.getByText("A job takes no follow-ups. Stopping it cancels the job.")).toBeTruthy();
```

New test: names the real $status job Stop exactly and retains its pointer without a composer.

`web/src/pages/RunView.job.test.tsx:168`:

```typescript
expect(screen.getByText("A job takes no follow-ups. Stopping it cancels the job.")).toBeTruthy();
```

Failed under job-composer in M2 header and settings > names the real 'queued' job Stop exactly and retains its pointer without a composer: TestingLibraryElementError: Unable to find an element with the text: A job takes no follow-ups. Stopping it cancels the job.. This could be because the text is broken up by multiple elements. In this case, you can provide a function for your text matcher to make your matcher more flexible.; unmodified candidate passed.

### M2-J5 — A job retains the pointer to its header Stop.

Old location: `web/src/pages/runView/JobResultPanel.test.tsx`, test the header offers Stop job while the steer card has no follow-up composer.

```typescript
expect(screen.getByText("Stop this job from the header.")).toBeTruthy();
```

New test: names the real $status job Stop exactly and retains its pointer without a composer.

`web/src/pages/RunView.job.test.tsx:169`:

```typescript
expect(screen.getByText("Stop this job from the header.")).toBeTruthy();
```

Failed under job-composer in M2 header and settings > names the real 'queued' job Stop exactly and retains its pointer without a composer: TestingLibraryElementError: Unable to find an element with the text: Stop this job from the header.. This could be because the text is broken up by multiple elements. In this case, you can provide a function for your text matcher to make your matcher more flexible.; unmodified candidate passed.

### M2-J6 — A job has a uniquely accessible Stop, renamed exactly Stop this job; queued and running fixtures both execute.

Old location: `web/src/pages/runView/JobResultPanel.test.tsx`, test the header offers Stop job while the steer card has no follow-up composer.

```typescript
expect(screen.getByRole("button", { name: "Stop job" })).toBeTruthy();
```

New test: names the real $status job Stop exactly and retains its pointer without a composer.

`web/src/pages/RunView.job.test.tsx:163`:

```typescript
const stop = screen.getByRole("button", { name: "Stop this job" });
```

Failed under controls-owner-positive, job-accessible-copy in M2 header and settings > names the real 'queued' job Stop exactly and retains its pointer without a composer: TestingLibraryElementError: Unable to find an accessible element with the role "button" and name "Stop this job"; unmodified candidate passed.

### M2-J7 — A job never labels its Stop as Stop run; the issue positive remains observable.

Old location: `web/src/pages/runView/JobResultPanel.test.tsx`, test the header offers Stop job while the steer card has no follow-up composer.

```typescript
expect(screen.queryByRole("button", { name: "Stop run" })).toBeNull();
```

New test: names the real $status job Stop exactly and retains its pointer without a composer.

`web/src/pages/RunView.job.test.tsx:167`:

```typescript
expect(screen.queryByRole("button", { name: "Stop run" })).toBeNull();
```

Failed under job-wrong-extra-stop in M2 header and settings > names the real 'queued' job Stop exactly and retains its pointer without a composer: AssertionError: expected <button …(1)></button> to be null; unmodified candidate passed.

## Exact deliberate breaks

### controls-blocked

Edits are file, checked original text, replacement text, optional all-occurrences flag.

```json
[
  [
    "src/components/SteerQueueCard.tsx",
    "if (terminal || !canSteer) return null;",
    "if (terminal && !canSteer) return null;"
  ]
]
```

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/components/SteerQueueCard.test.tsx:637`, RunControls — Pause ▾ menu (PRD #1190) > hides Pause and Stop for terminal and non-owner viewers — AssertionError: expected <button …(3)></button> to be null.

### controls-owner-positive

Edits are file, checked original text, replacement text, optional all-occurrences flag.

```json
[
  [
    "src/components/SteerQueueCard.tsx",
    "if (terminal || !canSteer) return null;",
    "if (terminal || canSteer) return null;"
  ]
]
```

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/components/SteerQueueCard.test.tsx:446`, RunControls — Pause ▾ menu (PRD #1190) > is ABSENT on a chat run (unpausable kind — chat parks between turns) — TestingLibraryElementError: Unable to find an element with the text: Stop run. This could be because the text is broken up by multiple elements. In this case, you can provide a function for your text matcher to make your matcher more flexible..

### controls-busy

Edits are file, checked original text, replacement text, optional all-occurrences flag.

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

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/components/SteerQueueCard.test.tsx:615`, RunControls — Pause ▾ menu (PRD #1190) > disables both controls while busy and never calls their callbacks — AssertionError: expected false to be true // Object.is equality.

### job-visible-copy

Edits are file, checked original text, replacement text, optional all-occurrences flag.

```json
[
  [
    "src/components/SteerQueueCard.tsx",
    "      {run.kind === \"job\" ? \"Stop this job\" : \"Stop run\"}\n",
    "      {run.kind === \"job\" ? \"Stop job\" : \"Stop run\"}\n"
  ]
]
```

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/pages/RunView.job.test.tsx:164`, M2 header and settings > names the real 'queued' job Stop exactly and retains its pointer without a composer — AssertionError: expected 'Stop job' to be 'Stop this job' // Object.is equality.

### job-accessible-copy

Edits are file, checked original text, replacement text, optional all-occurrences flag.

```json
[
  [
    "src/components/SteerQueueCard.tsx",
    "aria-label={run.kind === \"job\" ? \"Stop this job\" : \"Stop run\"}",
    "aria-label={run.kind === \"job\" ? \"Stop job\" : \"Stop run\"}"
  ]
]
```

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/components/SteerQueueCard.test.tsx:660`, honors the pause kind predicate for job — TestingLibraryElementError: Unable to find an accessible element with the role "button" and name "Stop this job".

### queue-pointer

Edits are file, checked original text, replacement text, optional all-occurrences flag.

```json
[
  [
    "src/components/SteerQueueCard.tsx",
    "\"Pause or stop this run from the header.\"",
    "\"Wrong pointer.\""
  ]
]
```

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/components/SteerQueueCard.test.tsx:280`, SteerQueueCard survives the terminal transition (B1) > a live run shows the composer and header pointer alongside the queue — TestingLibraryElementError: Unable to find an element with the text: Pause or stop this run from the header.. This could be because the text is broken up by multiple elements. In this case, you can provide a function for your text matcher to make your matcher more flexible..

### pause-pending

Edits are file, checked original text, replacement text, optional all-occurrences flag.

```json
[
  [
    "src/components/SteerQueueCard.tsx",
    "&& !run.pause_requested_at &&",
    "&& run.status === \"running\" &&"
  ]
]
```

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/components/SteerQueueCard.test.tsx:482`, RunControls — Pause ▾ menu (PRD #1190) > is ABSENT once a pause is already pending (the header chip owns it then) — AssertionError: expected <button …(3)></button> to be null.

### pause-kind

Edits are file, checked original text, replacement text, optional all-occurrences flag.

```json
[
  [
    "src/components/SteerQueueCard.tsx",
    "return PAUSABLE_KINDS.has(run.kind);",
    "return PAUSABLE_KINDS.has(run.kind) || !run.interactive;"
  ]
]
```

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/components/SteerQueueCard.test.tsx:444`, RunControls — Pause ▾ menu (PRD #1190) > is ABSENT on a chat run (unpausable kind — chat parks between turns) — AssertionError: expected <button …(3)></button> to be null.

### pause-final

Edits are file, checked original text, replacement text, optional all-occurrences flag.

```json
[
  [
    "src/components/SteerQueueCard.tsx",
    "const showMilestoneItem = !onFinalMilestone;",
    "const showMilestoneItem = !onFinalMilestone || run.status === \"running\";"
  ]
]
```

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/components/SteerQueueCard.test.tsx:548`, RunControls — Pause ▾ menu (PRD #1190) > HIDES the milestone item on the final milestone (only Pause now remains) — AssertionError: expected <span …(1)><span …(1)></span></span> to be null.

### switch-choice

Edits are file, checked original text, replacement text, optional all-occurrences flag.

```json
[
  [
    "src/pages/RunView.tsx",
    "<RunCredentialOverride run={run} />",
    "<RunCredentialOverride run={{ credential_switch: run.credential_switch }} />"
  ]
]
```

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/pages/RunView.job.test.tsx:136`, M2 header and settings > keeps one actual token and only a valid switch chip: requested — TestingLibraryElementError: Unable to find an element with the text: token override: pinned “Chosen account”. This could be because the text is broken up by multiple elements. In this case, you can provide a function for your text matcher to make your matcher more flexible..

### switch-unknown

Edits are file, checked original text, replacement text, optional all-occurrences flag.

```json
[
  [
    "src/pages/RunView.tsx",
    "(run.credential_switch === \"requested\" || run.credential_switch === \"released\")",
    "Boolean(run.credential_switch)"
  ]
]
```

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/pages/RunView.job.test.tsx:139`, M2 header and settings > keeps one actual token and only a valid switch chip: unknown — AssertionError: expected <span …(2)></span> to be null.

### settings-focus

Edits are file, checked original text, replacement text, optional all-occurrences flag.

```json
[
  [
    "src/pages/RunView.tsx",
    "trigger.current?.focus();",
    "trigger.current?.blur();"
  ]
]
```

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/pages/RunView.job.test.tsx:211`, M2 header and settings > opens with Enter/Space and returns focus on Escape/trigger close; includes epoch one pin and time — AssertionError: expected <input …(2)></input> to be <button type="button" …(3)></button> // Object.is equality.

### spinner-reduced

Edits are file, checked original text, replacement text, optional all-occurrences flag.

```json
[
  [
    "src/components/ui.tsx",
    "if (!preference?.matches) {",
    "if (preference?.matches || !preference?.matches) {"
  ]
]
```

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/components/Spinner.test.tsx:22`, stays static without a timer under reduced motion and follows preference changes — AssertionError: expected 1 to be +0 // Object.is equality.

### job-composer

Edits are file, checked original text, replacement text, optional all-occurrences flag.

```json
[
  [
    "src/components/SteerQueueCard.tsx",
    "const takesFollowUps = run?.kind !== \"job\";",
    "const takesFollowUps = run?.kind !== \"prompt\";"
  ]
]
```

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/pages/RunView.job.test.tsx:166`, M2 header and settings > names the real 'queued' job Stop exactly and retains its pointer without a composer — AssertionError: expected <textarea …(3)></textarea> to be null.

### controls-relocated

Edits are file, checked original text, replacement text, optional all-occurrences flag.

```json
[
  [
    "src/pages/RunView.tsx",
    "          <RunControls run={run} busy={busy} canSteer={canSteer} terminal={terminal}\n            onStop={() => cancelRun()} onPause={(mode) => act(async () => { await api.pauseRun(run.id, mode); await refreshRun(); })} />",
    "          {run.id.length < 0 && <RunControls run={run} busy={busy} canSteer={canSteer} terminal={terminal}\n            onStop={() => cancelRun()} onPause={(mode) => act(async () => { await api.pauseRun(run.id, mode); await refreshRun(); })} />}"
  ],
  [
    "src/components/SteerQueueCard.tsx",
    "          <p className=\"text-xs text-faint\">{takesFollowUps ? \"Pause or stop this run from the header.\" : \"Stop this job from the header.\"}</p>",
    "          <p className=\"text-xs text-faint\">{takesFollowUps ? \"Pause or stop this run from the header.\" : \"Stop this job from the header.\"}</p>\n          {run && <RunControls run={run} busy={busy} terminal={terminal} canSteer={canSteer} onPause={() => {}} onStop={() => {}} />}"
  ]
]
```

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/pages/RunView.job.test.tsx:150`, M2 header and settings > places the only lifecycle controls above the real composer, including pending-read optimism — AssertionError: expected false to be true // Object.is equality.

### settings-visible-closed

Edits are file, checked original text, replacement text, optional all-occurrences flag.

```json
[
  [
    "src/pages/RunView.tsx",
    "hidden={!open}",
    "hidden={false}"
  ],
  [
    "src/pages/RunView.tsx",
    "open ? \"grid\" : \"hidden\"",
    "open ? \"grid\" : \"grid\""
  ]
]
```

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/pages/RunView.job.test.tsx:197`, M2 header and settings > opens with Enter/Space and returns focus on Escape/trigger close; includes epoch one pin and time — AssertionError: expected false to be true // Object.is equality.

### issue-composer-missing

Edits are file, checked original text, replacement text, optional all-occurrences flag.

```json
[
  [
    "src/components/SteerQueueCard.tsx",
    "{takesFollowUps ? (",
    "{takesFollowUps && run?.id.length === -1 ? ("
  ]
]
```

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/components/SteerQueueCard.test.tsx:279`, SteerQueueCard survives the terminal transition (B1) > a live run shows the composer and header pointer alongside the queue — TestingLibraryElementError: Unable to find an element with the text: Send follow-up. This could be because the text is broken up by multiple elements. In this case, you can provide a function for your text matcher to make your matcher more flexible..

### job-wrong-extra-stop

Edits are file, checked original text, replacement text, optional all-occurrences flag.

```json
[
  [
    "src/components/SteerQueueCard.tsx",
    "    </Button>\n  </div>;",
    "    </Button>\n    {run.kind === \"job\" && <Button variant=\"danger\" disabled={busy} onClick={onStop}>Stop run</Button>}\n  </div>;"
  ]
]
```

Typecheck 0; focused test 1; unmodified run 0. First named failure: `web/src/pages/RunView.job.test.tsx:167`, M2 header and settings > names the real 'queued' job Stop exactly and retains its pointer without a composer — AssertionError: expected <button …(1)></button> to be null.

## Setup-only migrations

Queue calls remove unused onStop/onPause props; unchanged queue receipts, terminal survival, read-only composer, owner, parked-message and delivery assertions remain. Pause cases now mount production RunControls directly with the same run/busy/terminal/callback values and explicit canSteer. The job Chrome wrapper is replaced by actual RunView cases, including the exact original queued-job fixture and its running counterpart. The existing Rework-now 409 case opens Run settings before clicking its real action; its inline-error and empty-page-alert assertions remain unchanged. No behavioral assertion was weakened or deleted; the inventory above lists every relocated predicate.

## Browser regressions and visual evidence

Initial settings right anchoring failed the actual mock page at 390px (left -189.5); mobile-left/desktop-right anchoring passed. The relocated Pause menu then failed the actual mock page at 1280px (1078–1366), independently matching reviewer reproduction. The shipping production-RunView browser test failed at three widths on an immutable 30ec archive, then passed after anchoring against the full action group at 630523ce. It uses shipping CSS and a proper /runs/:id route.

The nested token picker failed at 320px: right 321 versus outer-panel right 288. Capping the root/picker widths and wrapping its action row fixed that defect. Shipping browser coverage opens BOTH outer and nested disclosures and checks every nested button/select/input within the picker, plus Escape/focus return. Five widths pass: 320, 390, 768, 1024, 1280. Credential and disabled-panel behavior tests: 36 passed.

[Light header/mobile settings comparison](2789-m2-header-parity-light.png) · [Dark comparison](2789-m2-header-parity-dark.png). Actual mock-mode Chromium checks Enter/Space/Escape and focus return in both themes, settings bounds at 390/768/1024/1280px and Pause bounds at 320/390/768/1024/1280px; no page errors. The shipping production RunView browser test separately checks settings and its nested picker at all five widths. The mock capture-only [hidden] rule remains as documented for M1. Native palette/typeface/components, sample identity/token/model/time values differ; effective nondefault toggle chips are required by D14. Final four-state attention/footer matrix remains for M3.

## Integration and review

Initial full task gate:web passed at 30ec15a9: 6693 unit tests and 14 browser tests, exit 0. That green did not close the independently demonstrated layout defect. The costly gate is repeated only after repair source review is clean. Source reviewer/tester/auditor cumulative reports are clean through 630523ce except the demonstrated nested-picker note, which is treated as mandatory and repaired in e5. The first source-wave tester was cancelled with no verdict; fresh tester coverage was completed on 630523ce. No canceled run is claimed as approval.

Final source reviewer/tester/auditor reports are clean through e5d39e35, closing both responsive findings. The nested-picker note was labelled non-blocking but its demonstrated layout defect was repaired rather than deferred.

Lead checks at e5d39e35:

- `task gate:web`: exit 0, 6694 unit tests across 330 files and 19 browser tests across five files (including the new five-width production page regression).
- `npm run build` from web: exit 0; existing large-chunk warning remains.
- `task gate:repo`: exit 0, canary-checked secrets and Semgrep scans clean. Actionlint was unavailable, so workflow lint was explicitly skipped; zsh triage rendering coverage was unavailable, with bash checks run. No workflow files changed.
- `task docs:sync` executed for the two documentation updates; uncached `go test -count=1 ./internal/uzidocs -run '^TestEmbeddedDocsMatchSource$'` passed, exit 0. Those documentation/mirror bytes are unchanged by later CSS repairs.

These are lead executions, separate from validator checks. Guard mutation evidence remains at its named immutable SHA; no claim of live API persistence or M3/M4 completion is made. Optional tester suggestions for adding formatter/coverage gates are deferred under the approved existing-gate/no-workflow-change scope. This is informational coverage, not a new threshold gate.
