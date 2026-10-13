# M3 validation — attention, usage and summary

Source SHA: `f2083e525eb1caddddcd0aceee92652ededf4b2f`. Unit base: `df805bdac1eeafc920639040bf4d80a9dc472b47`. Cumulative M3-owned review base: `2bf9a00e6eac5e3989097bdb7167cd3143a092df`. M4 remains pending.

## Assertion migration inventory

**Zero old -> new migrations in M3.** The two M3 test suites are additions; every pre-existing test file is unchanged against the unit base. The older count/copy/selector migrations remain exhaustively recorded in [M1](2789-m1-validation.md) and [M2](2789-m2-validation.md), with their original scoped failure/pass evidence. This does not reclassify their assertions as new M3 coverage.

54 new cases cover confirmed/live/side/tail costs and links, cache rounding, saved true/false/unset choices through gate exit, malformed/expired/failed storage, parsed safe excerpts, attention/history/progress order, and owner-gated approval/holds. Existing summary and count assertions are retained. No prices, API fields, schemas, workflows, or dependency files changed.

## Isolated deliberate breaks

Every case used a fresh archive of the exact source SHA under scratch; the archive's web lockfile was compared byte-for-byte with the provisioned checkout before linking that checkout's own node_modules. No dependency copy or install, nested worktree, or primary-tree mutation occurred. Each old source anchor was asserted present and the mutation asserted applied. All thirteen valid breaks typechecked with exit 0, then failed focused Vitest with exit 1 and a named intended assertion failure. The unmodified export passed 72 cases and typecheck both before and **after** the breaks. These are focused checks, not full-suite mutation reruns.

Commands from each archive's web directory: `npm run typecheck`; `npm exec vitest -- run src/components/RunProgressCard.m3.test.tsx src/pages/RunView.m3.test.tsx src/lib/prefs.test.ts -t <selection>`. The unmodified selection was `.`. Logs are named below for runner-local audit; all essential source mutations and failing-test excerpts are included here so this evidence survives scratch removal.

### confirmed-status

Selection: `confirmed status`. Typecheck **0**, test **1**.

`web/src/components/RunProgressCard.tsx`:

```text
usage.hasConfirmed ? run.usage?.cost_status ?? "" : sideUsage(usage).status
->
usage.hasConfirmed ? "metered" : sideUsage(usage).status
```

Named intended failures:

```text
   × confirmed status subscription uses run status and the same total as the panel 29ms
   × confirmed status unreported uses run status and the same total as the panel 17ms
   × confirmed status  uses run status and the same total as the panel 18ms
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > confirmed status subscription uses run status and the same total as the panel
AssertionError: expected '$2.00API-equivalent1.0k in (99% cache…' to contain 'No estimate'
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > confirmed status unreported uses run status and the same total as the panel
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > confirmed status  uses run status and the same total as the panel
AssertionError: expected '$2.00API-equivalent1.0k in (99% cache…' to contain 'Unavailable'
```

Logs: `m3-proof-f2083e52-confirmed-status-typecheck.log`, `m3-proof-f2083e52-confirmed-status-test.log`.

### side-status

Selection: `side statuses`. Typecheck **0**, test **1**.

`web/src/components/RunProgressCard.tsx`:

```text
sideUsage(usage).status
->
(sideUsage(usage), run.usage?.cost_status ?? "")
```

Named intended failures:

```text
   × side statuses subscription/subscription own the display with no confirmed result 35ms
   × side statuses unreported/unreported own the display with no confirmed result 18ms
   × side statuses / own the display with no confirmed result 13ms
   × side statuses metered/unreported own the display with no confirmed result 14ms
   × side statuses metered/ own the display with no confirmed result 13ms
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > side statuses subscription/subscription own the display with no confirmed result
AssertionError: expected 'Summary usage$0.00API-equivalent200 i…' to contain 'No estimate'
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > side statuses unreported/unreported own the display with no confirmed result
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > side statuses / own the display with no confirmed result
AssertionError: expected 'Summary usage$0.00API-equivalent200 i…' to contain 'Unavailable'
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > side statuses metered/unreported own the display with no confirmed result
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > side statuses metered/ own the display with no confirmed result
AssertionError: expected 'Summary usage$0.25API-equivalent200 i…' to contain 'Unavailable'
```

Logs: `m3-proof-f2083e52-side-status-typecheck.log`, `m3-proof-f2083e52-side-status-test.log`.

### live-billing

Selection: `live-only`. Typecheck **0**, test **1**.

`web/src/components/RunProgressCard.tsx`:

```text
const hasRecorded = usage && (usage.hasConfirmed || hasSideUsage(usage));
->
const hasRecorded = usage && (usage.hasConfirmed || usage.hasLiveTokens || hasSideUsage(usage));
```

Named intended failures:

```text
   × live-only provisional input, tail false, never prices live or shows output 110ms
   × live-only provisional input, tail true, never prices live or shows output 29ms
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > live-only provisional input, tail false, never prices live or shows output
AssertionError: expected 'Summary usageUnavailabletokens only ·…' not to contain ' out'
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > live-only provisional input, tail true, never prices live or shows output
AssertionError: expected 'Summary usageUnavailabletokens only ·…' not to contain ' out'
```

Logs: `m3-proof-f2083e52-live-billing-typecheck.log`, `m3-proof-f2083e52-live-billing-test.log`.

### total-once

Selection: `confirmed plus sides`. Typecheck **0**, test **1**.

`web/src/components/RunProgressCard.tsx`:

```text
sideUsage(usage).status, usage.total.costUsd)
->
sideUsage(usage).status, usage.total.costUsd * 2)
```

Named intended failures:

```text
   × confirmed plus sides counts each once and suppresses live input, live false 101ms
   × confirmed plus sides counts each once and suppresses live input, live true 26ms
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > confirmed plus sides counts each once and suppresses live input, live false
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > confirmed plus sides counts each once and suppresses live input, live true
AssertionError: expected '$5.00API-equivalent1.2k in (97% cache…' to contain '$2.50'
```

Logs: `m3-proof-f2083e52-total-once-typecheck.log`, `m3-proof-f2083e52-total-once-test.log`.

### cache-honesty

Selection: `confirmed status`. Typecheck **0**, test **1**.

`web/src/components/RunProgressCard.tsx` (all matching occurrences):

```text
cacheDisplayPct(usage.total)
->
Math.round(usage.total.cached / (usage.total.fresh + usage.total.cached) * 100)
```

Named intended failures:

```text
   × confirmed status metered uses run status and the same total as the panel 120ms
   × confirmed status subscription uses run status and the same total as the panel 19ms
   × confirmed status unreported uses run status and the same total as the panel 17ms
   × confirmed status  uses run status and the same total as the panel 19ms
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > confirmed status metered uses run status and the same total as the panel
AssertionError: expected '$2.00API-equivalent1.0k in (100% cach…' to contain '1.0k in (99% cached)'
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > confirmed status subscription uses run status and the same total as the panel
AssertionError: expected 'No estimateno cost estimate recorded1…' to contain '1.0k in (99% cached)'
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > confirmed status unreported uses run status and the same total as the panel
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > confirmed status  uses run status and the same total as the panel
AssertionError: expected 'Unavailabletokens only · cost unavail…' to contain '1.0k in (99% cached)'
```

Logs: `m3-proof-f2083e52-cache-honesty-typecheck.log`, `m3-proof-f2083e52-cache-honesty-test.log`.

### unpriced-tail

Selection: `tail-only`. Typecheck **0**, test **1**.

`web/src/components/RunProgressCard.tsx`:

```text
"+ unpriced tail"
->
"+~$0 unrecorded"
```

Named intended failures:

```text
   × tail-only unpriced/null has no panel or Full usage link 93ms
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > tail-only unpriced/null has no panel or Full usage link
AssertionError: expected '+~$0 unrecorded' to be '+ unpriced tail' // Object.is equality
```

Logs: `m3-proof-f2083e52-unpriced-tail-typecheck.log`, `m3-proof-f2083e52-unpriced-tail-test.log`.

### tail-link

Selection: `tail-only`. Typecheck **0**, test **1**.

`web/src/components/RunProgressCard.tsx`:

```text
{hasUsage && <a href="#run-full-usage"
->
{(hasUsage || tailVisible(run.usage_estimated_tail)) && <a href="#run-full-usage"
```

Named intended failures:

```text
   × tail-only unpriced/null has no panel or Full usage link 74ms
   × tail-only estimated/0.5 has no panel or Full usage link 11ms
   × tail-only estimated/0.001 has no panel or Full usage link 11ms
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > tail-only unpriced/null has no panel or Full usage link
AssertionError: expected '+ unpriced tailFull usage' to be '+ unpriced tail' // Object.is equality
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > tail-only estimated/0.5 has no panel or Full usage link
AssertionError: expected '+ ~$0.50 estimated unrecordedFull usa…' to be '+ ~$0.50 estimated unrecorded' // Object.is equality
 FAIL  |jsdom| src/components/RunProgressCard.m3.test.tsx > tail-only estimated/0.001 has no panel or Full usage link
AssertionError: expected '+ <$0.01 estimated unrecordedFull usa…' to be '+ <$0.01 estimated unrecorded' // Object.is equality
```

Logs: `m3-proof-f2083e52-tail-link-typecheck.log`, `m3-proof-f2083e52-tail-link-test.log`.

### gate-default

Selection: `stored preference`. Typecheck **0**, test **1**.

`web/src/pages/RunView.tsx` (all matching occurrences):

```text
summaryCollapse.getCollapsed(run.id) ?? run.status !== "awaiting_approval"
->
summaryCollapse.getCollapsed(run.id) ?? run.status === "awaiting_approval"
```

Named intended failures:

```text
   × stored preference undefined wins at mount and gate exit without remount 367ms
 FAIL  |jsdom| src/pages/RunView.m3.test.tsx > stored preference undefined wins at mount and gate exit without remount
```

Logs: `m3-proof-f2083e52-gate-default-typecheck.log`, `m3-proof-f2083e52-gate-default-test.log`.

### explicit-false

Selection: `stored preference`. Typecheck **0**, test **1**.

`web/src/pages/RunView.tsx` (all matching occurrences):

```text
summaryCollapse.getCollapsed(run.id) ?? run.status !== "awaiting_approval"
->
summaryCollapse.getCollapsed(run.id) || run.status !== "awaiting_approval"
```

Named intended failures:

```text
   × stored preference false wins at mount and gate exit without remount 59ms
 FAIL  |jsdom| src/pages/RunView.m3.test.tsx > stored preference false wins at mount and gate exit without remount
```

Logs: `m3-proof-f2083e52-explicit-false-typecheck.log`, `m3-proof-f2083e52-explicit-false-test.log`.

### explicit-true

Selection: `stored preference`. Typecheck **0**, test **1**.

`web/src/lib/prefs.ts`:

```text
return pruned[runId]?.collapsed;
->
return pruned[runId]?.collapsed === true ? undefined : pruned[runId]?.collapsed;
```

Named intended failures:

```text
   × stored preference true wins at mount and gate exit without remount 77ms
 FAIL  |jsdom| src/pages/RunView.m3.test.tsx > stored preference true wins at mount and gate exit without remount
```

Logs: `m3-proof-f2083e52-explicit-true-typecheck.log`, `m3-proof-f2083e52-explicit-true-test.log`.

### excerpt-scrub

Selection: `collapsed excerpt parses`. Typecheck **0**, test **1**.

`web/src/components/PlainMarkdownExcerpt.tsx`:

```text
stripUnsafeChars(parts.join(""))
->
parts.join("")
```

Named intended failures:

```text
   × collapsed excerpt parses lists, references, nested destinations, entities, images and code into safe plain text 116ms
 FAIL  |jsdom| src/pages/RunView.m3.test.tsx > collapsed excerpt parses lists, references, nested destinations, entities, images and code into safe plain text
AssertionError: expected 'Intent Build feature and nested diagr…' to be 'Intent Build feature and nested diagr…' // Object.is equality
```

Logs: `m3-proof-f2083e52-excerpt-scrub-typecheck.log`, `m3-proof-f2083e52-excerpt-scrub-test.log`.

### excerpt-format

Selection: `collapsed excerpt parses`. Typecheck **0**, test **1**.

`web/src/components/PlainMarkdownExcerpt.tsx`:

```text
tree.children = [{ type:
->
if (false) tree.children = [{ type:
```

Named intended failures:

```text
   × collapsed excerpt parses lists, references, nested destinations, entities, images and code into safe plain text 130ms
 FAIL  |jsdom| src/pages/RunView.m3.test.tsx > collapsed excerpt parses lists, references, nested destinations, entities, images and code into safe plain text
AssertionError: expected 'Intent\n\nBuild feature and nested\n …' to be 'Intent Build feature and nested diagr…' // Object.is equality
```

Logs: `m3-proof-f2083e52-excerpt-format-typecheck.log`, `m3-proof-f2083e52-excerpt-format-test.log`.

### history-order

Selection: `attention comes before history`. Typecheck **0**, test **1**.

`web/src/pages/RunView.tsx`:

```text
      <IssueInputHistory run={run} />

      <RunProgressCard run={run} activity={currentActivity} usage={usage} stage={stage} />
->
      <RunProgressCard run={run} activity={currentActivity} usage={usage} stage={stage} />

      <IssueInputHistory run={run} />
```

Named intended failures:

```text
   × awaiting_approval attention comes before history directly before progress 488ms
   × completed attention comes before history directly before progress 82ms
   × failed attention comes before history directly before progress 47ms
   × cancelled attention comes before history directly before progress 45ms
 FAIL  |jsdom| src/pages/RunView.m3.test.tsx > awaiting_approval attention comes before history directly before progress
 FAIL  |jsdom| src/pages/RunView.m3.test.tsx > completed attention comes before history directly before progress
 FAIL  |jsdom| src/pages/RunView.m3.test.tsx > failed attention comes before history directly before progress
 FAIL  |jsdom| src/pages/RunView.m3.test.tsx > cancelled attention comes before history directly before progress
AssertionError: expected +0 to be truthy
```

Logs: `m3-proof-f2083e52-history-order-typecheck.log`, `m3-proof-f2083e52-history-order-test.log`.

The first side-status fixture removed the helper import's only use and failed typecheck (TS6133); it is **excluded** from proof. The valid replacement still invokes the helper while deliberately choosing run status. That corrected mutation typechecked and produced the named side-status assertion failures above.

Unmodified post-break logs: `m3-proof-f2083e52-unmodified-after-typecheck.log` and `m3-proof-f2083e52-unmodified-after-test.log`: typecheck 0, 72 passed, test 0. Primary checkout stayed at the source SHA and clean throughout.

## Frozen gates and reviews

- `task gate:web`: exit 0 at source SHA; 6,748 unit tests in 332 files and 19 browser tests in five files. Log `gate-log.skDqj7`. Browser cases include shipping RunView header/disclosure at 320, 390, 768, 1024 and 1280px.
- `npm run build` in web: exit 0 at source SHA; log `gate-log.Z04Scc`. The existing large-bundle warning remains; no bundle-splitting claim.
- `task docs:sync`: exit 0; source run-activity and embedded mirror updated together.
- `go test -count=1 ./internal/uzidocs -run '^TestEmbeddedDocsMatchSource$'` in api: exit 0, log `gate-log.KYILhX`. `gofmt -l` for the four M1 CLI/TUI comment files produced no paths, exit 0.
- `task gate:repo`: exit 0 at evidence SHA `df1b5d2874db1d77ad311ef5959b3b83dd05a241`, log `gate-log.luND2f`; secrets canaries DETECTED with zero tracked findings, Semgrep canary detected with zero tree findings/errors. Workflow lint was explicitly SKIPPED because actionlint is absent; triage skill rendering was Bash-only because zsh is absent. No workflow files changed.
- Reviewer, tester and auditor verified the immutable evidence range `f2083e52..df1b5d28`, including exact raw failures/typecheck results, post-break passing summaries and both PNGs. No mandatory evidence/security findings or optional code/security notes. Tester independently passed the docs check and suggested named passing output, coverage thresholds and a pre-commit gate. Named passing output was added below; broader coverage/pre-commit infrastructure suggestions are deferred under the approved existing-gates/no-workflow-change scope.

Reviewer, tester and auditor found no demonstrated mandatory source defects and no optional code/security notes at the source SHA. They checked the unit and cumulative M3-owned ranges, including kept prototype behavior, attention contents/handlers, optimistic controls and the separate confirmed-owner CodeCrossCheck gate. Reviewer ran 115 focused tests, deadcode and mirror checks; tester ran 680 focused tests, focused lint and scope/mirror checks; auditor ran 197 focused tests, secrets (canaries DETECTED), Semgrep canary/tree scan and npm audit. These reports are scoped review evidence, not live API or model-output proof. They explicitly left new mutation/browser evidence to the lead and its later immutable review.

## Browser comparison

Backend-free named `run-progress` scenario, real Chromium, 1280px desktop, reduced motion; running, awaiting approval, failed and completed in Hall light and Ember dark. All eight pages had zero page errors. Approval and terminal heroes precede progress; running selectors match real Now and lane strips, while both terminal fixtures have neither. The corrected capture includes the full approval panel and progress, without app navigation/update-banner chrome. Log `gate-log.z8FwYo`. First capture `gate-log.RvAW4n` passed runtime assertions but cut off approval below 1200px; that visual crop is superseded and not the permanent evidence.

Permanent comparisons: [light](2789-run-page-parity-light.png), [dark](2789-run-page-parity-dark.png). Both sheets were inspected by the lead for order, single list, row glyphs, semantic colours, active context, Now/lane attachment, footer and summary defaults. Responsive controls remain covered by shipping browser tests and the [M2 captures](2789-m2-validation.md); these desktop sheets do not themselves prove narrow-view layout.

Necessary mock differences under the approved PRD: existing approval/hero content, controls and semantic styling are retained rather than replaced by the mock's shortened panels; actual usage/ages come from scenario messages, so the amounts differ from illustrative mock totals; the approval fixture has no usage and therefore no invented footer. Terminal progress is null, so frozen reported rows remain without a fabricated terminal progress state or active segment. The plan segment uses the existing plan colour and completion rows retain their existing strike-through. The mock's simplified summary is a production Expand/Collapse control plus a plain excerpt. These follow preserved renderer/panel semantics and M3's no-redesign constraint. The prototype's generic side-only 'in flight' line and ambiguous unpriced-tail wording were intentionally dropped.

## Named post-break passing evidence

A fresh unmodified archive of the source SHA passed typecheck and all 72 selected tests using Vitest JSON output after the mutation sequence. All 29 named failure occurrences recorded across the 13 breaks match an explicit passing result by source-file suffix and exact trimmed test name. This closes the tester's optional passing-name instrumentation gap; the earlier pre-break count summary remains a count-only observation.

Commands: `npm run typecheck`; `npm exec vitest -- run src/components/RunProgressCard.m3.test.tsx src/pages/RunView.m3.test.tsx src/lib/prefs.test.ts --reporter=json --outputFile=<scratch result>`. Matching lockfile and own provisioned dependencies, no primary mutation or install. Logs: `gate-log.OSodG5`, `m3-named-pass-typecheck.log`, `m3-named-pass-tests.log`; raw JSON `m3-named-pass-results.json` and checked mapping `m3-named-pass-match.json`. Typecheck 0, tests 0, 72 passed, zero skipped.

| Break | Failing test now explicitly passing |
| --- | --- |
| confirmed-status | `confirmed status subscription uses run status and the same total as the panel` |
| confirmed-status | `confirmed status unreported uses run status and the same total as the panel` |
| confirmed-status | `confirmed status  uses run status and the same total as the panel` |
| side-status | `side statuses subscription/subscription own the display with no confirmed result` |
| side-status | `side statuses unreported/unreported own the display with no confirmed result` |
| side-status | `side statuses / own the display with no confirmed result` |
| side-status | `side statuses metered/unreported own the display with no confirmed result` |
| side-status | `side statuses metered/ own the display with no confirmed result` |
| live-billing | `live-only provisional input, tail false, never prices live or shows output` |
| live-billing | `live-only provisional input, tail true, never prices live or shows output` |
| total-once | `confirmed plus sides counts each once and suppresses live input, live false` |
| total-once | `confirmed plus sides counts each once and suppresses live input, live true` |
| cache-honesty | `confirmed status metered uses run status and the same total as the panel` |
| cache-honesty | `confirmed status subscription uses run status and the same total as the panel` |
| cache-honesty | `confirmed status unreported uses run status and the same total as the panel` |
| cache-honesty | `confirmed status  uses run status and the same total as the panel` |
| unpriced-tail | `tail-only unpriced/null has no panel or Full usage link` |
| tail-link | `tail-only unpriced/null has no panel or Full usage link` |
| tail-link | `tail-only estimated/0.5 has no panel or Full usage link` |
| tail-link | `tail-only estimated/0.001 has no panel or Full usage link` |
| gate-default | `stored preference undefined wins at mount and gate exit without remount` |
| explicit-false | `stored preference false wins at mount and gate exit without remount` |
| explicit-true | `stored preference true wins at mount and gate exit without remount` |
| excerpt-scrub | `collapsed excerpt parses lists, references, nested destinations, entities, images and code into safe plain text` |
| excerpt-format | `collapsed excerpt parses lists, references, nested destinations, entities, images and code into safe plain text` |
| history-order | `awaiting_approval attention comes before history directly before progress` |
| history-order | `completed attention comes before history directly before progress` |
| history-order | `failed attention comes before history directly before progress` |
| history-order | `cancelled attention comes before history directly before progress` |
