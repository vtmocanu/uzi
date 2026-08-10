# PRD #292: Slack styling — render Markdown as Slack mrkdwn across bot DMs

**GitLab Issue**: [vtmocanu/uzi#292](https://gitlab.example.com/vtmocanu/uzi/-/issues/292)
**Status**: Complete (delivered 2026-08-10, commits `b4f6f3c1`..`445d376a`; architect-reviewed, all BLOCKER/MAJOR findings folded in)
**Priority**: Medium (high-visibility: it affects every model-authored DM)
**Created**: 2026-08-10
**Depends on**: PRD #268 (Slack DM UX — done; migrated the DMs to Block Kit and is the PRD whose "model markdown renders" assumption this corrects), PRD #191 (Slack conversational surface — done)
**Related**: `docs/slack.md`, `ARCHITECTURE.md` §"Slack integration (outbound-only)" (`ARCHITECTURE.md:875`), `api/internal/slacksvc/`, `api/internal/handler/judge_worker.go`, `api/internal/notifysvc/service.go`, `web/src/lib/notifications.ts`

> Every file:line citation below was derived at **`b94f5244`** (v0.26.0). A line number
> without a SHA is not a citation — re-derive before acting.

## Problem

Every model-authored body the `uzi` bot posts to Slack renders its **CommonMark source
text literally**, because the DM path escapes but never *translates* markdown into Slack's
mrkdwn dialect. Slack mrkdwn is not CommonMark: bold is `*x*` not `**x**`, italic is `_x_`
not `*x*`, links are `<url|label>` not `[label](url)`, there is no `#` heading syntax, and
`-`/`*` bullets do not auto-render. So the owner's own 1:1 DM is full of raw markdown noise.

### Live evidence (product owner's DM with `uzi`, 2026-08-10)

Chat answer to "status of runs?" (verbatim message text):

```
Here's the current picture of your runs (newest first): **Active right now (3 running)**
- **#280** — Seeded `--plan-file` runs bypass the approval gate — *running* (started 11:14)
- **#279** — Evidence/verification runs have no report-only path … — *running* (started 11:14)
```

In Slack this shows literal `**Active right now (3 running)**`, literal `**#280**`, literal
`*running*`, and the `- ` bullets as literal dashes. Only the `` `--plan-file` `` code span
renders (Slack supports code spans). The answer is a wall of asterisks.

Judge "Run review ready" DM (verbatim, 2026-08-10 11:20):

```
Run review ready — Verdict ✅ ok, 1 recommendation, improveuzi — **Re-seeded run on
already-completed, already-merged work.** Run `413d9e7b` was seeded on branch …
```

The `**Re-seeded run on already-completed, already-merged work.**` renders with literal
asterisks.

### Root cause (traced at `b94f5244`)

Model bodies are passed to Block Kit `section`/`context` mrkdwn objects after
`EscapeMrkdwn(ScrubSecrets(...))`. `EscapeMrkdwn` is `slackutilsx.EscapeMessage`, which is
only:

```go
strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")   // via api/internal/slacksvc/redact.go:20
```

It touches `& < >` and nothing else — by design, to neutralize injected `<@mention>` /
`<https://evil|Open>` markup in untrusted text (`redact.go:11-19`). It does **not** convert
`**` → `*`, `*italic*` → `_italic_`, `#` → bold, `[l](u)` → `<u|l>`, `- ` → `• `, or `~~` →
`~`. So CommonMark passes through untranslated and Slack renders the source characters.

The UNTRUSTED model-body render sites, all at `b94f5244` (this table lists three; a **fourth**
peer — the clarification-question body `questionThreadBlocks` `question.go:146` — was found
during implementation and is also in scope, see Decision 4 correction):

| Path | Site | Body |
|---|---|---|
| Chat per-turn answer (+ chat cards) | `renderChatBody` `chatpost.go:352-357` (`truncateForSlackSection(EscapeMrkdwn(ScrubSecrets(s)))`) — **shared** by chatpost.go:294 and the chat cards at chatactions.go:405 (proposal) / :547 (run-request) | agent chat answer / card description |
| Judge "Run review ready" (+ 3 other producers) | `notificationBlocks` blockquote `notifier.go:345-352` (`escapedBody := truncateForSlackSection(EscapeMrkdwn(ScrubSecrets(body)))`) | judge summary preview; **also** selfimprove started/skipped and schedule-paused bodies |
| Plan / gate body | `planThreadBlocks` `gate.go:266-267` (`truncateForSlackSection(EscapeMrkdwn(ScrubSecrets(planMD)))`) | approval-gate plan |

`renderChatBody` being shared is why the chat cards need no separate site — fixing the one
helper covers all three chat surfaces (correcting this PRD's first draft, which listed the
card lines as separate M4 targets).

### Second, smaller defect: the judge summary flattens newlines

`reviewSummaryPreview` (`api/internal/handler/judge_worker.go:364-372`) collapses **all**
whitespace including newlines to single spaces (`strings.Join(strings.Fields(summaryMd), " ")`)
before the 600-rune cap. So even after markdown is translated, the judge blockquote is a
run-on line: paragraphs and bullet lists become `- x - y - z` on one line. Chat and plan
preserve their newlines; the judge summary uniquely does not, and `notificationBlocks`
already renders a multi-line blockquote correctly (`notifier.go:347-350` prefixes every
line with `> `), so the collapse is pure loss for the Slack body.

### Why PRD #268 did not fix this

PRD #268 M4's own text says "Per-turn answer as a `section` (**model markdown renders**)".
That premise is false — it conflated CommonMark with Slack mrkdwn. #268 correctly migrated
the DMs to Block Kit and fixed the dropped-answer bug (M1), but left the body text
untranslated. This PRD closes that gap.

## Solution

Introduce **one injection-safe CommonMark → Slack-mrkdwn renderer** and route every
UNTRUSTED model-authored body through it, replacing the whole-blob `EscapeMrkdwn` **for
those bodies only** (per-field `EscapeMrkdwn` on trusted fields — repo path, title, agent
names, card fields — is unchanged; see Decision 2).

Conversions the renderer must perform:

| CommonMark | Slack mrkdwn |
|---|---|
| `**bold**`, `__bold__` | `*bold*` |
| `*italic*`, `_italic_` | `_italic_` |
| `# H` … `###### H` | `*H*` (bold line; no Slack heading syntax) |
| `- item`, `* item`, `+ item` | `• item` |
| `1. item` | keep `1. item` (Slack shows it acceptably) |
| `[label](url)` | `<url|label>` — **https only** (Decision 5), else drop the link and keep the escaped label text |
| `~~strike~~` | `~strike~` (needs goldmark GFM extension — Decision 1) |
| `` `code` ``, ```` ```fence``` ````, `> quote` | pass through, `&<>`-escaped content (Slack already renders these) |
| tables, images, raw HTML | degrade to escaped plain text (no Slack equivalent) |

### The safety contract (load-bearing — this is the point of the PRD, not the prettifying)

The renderer runs on UNTRUSTED model/forge text and must be at least as safe as today's
whole-blob escape. Implemented as a custom goldmark `NodeRenderer` (Decision 1), the
contract is expressed per AST node:

1. **Text, RawHTML, HTMLBlock, and code (span/fence) nodes are `&<>`-escaped**, never
   passed through. This is what makes an injected `<@U123>` mention or `<https://evil|Open>`
   link inert: goldmark parses `<@U123>` as raw-HTML-inline/text, **not** as a Link node, so
   escaping those node kinds neutralizes it. (Pin goldmark's classification of `<@U…>` with
   a test — M1.)
2. **`<url|label>` is emitted ONLY from a `Link` node** whose destination scheme is `https`
   (Decision 5). Any other scheme (`http`, `mailto`, `javascript`, `data`, bare autolink)
   degrades to the escaped label text with no link.
3. **The emitted `<url|label>` is itself sanitized**, not just scheme-checked. A CommonMark
   destination or label may legally contain `<`, `>`, `|`, or whitespace (e.g.
   `[l](https://e.com/?a=1|<@U0>)` parses to that literal dest), and Slack's grammar is
   `<url|label>` where `|` splits and `>` terminates — so emitting it raw re-opens the exact
   injection this PRD closes. Therefore: reject or percent-encode `< > |` and whitespace in
   the URL, and `&<>`-escape the label and neutralize any `|` in it. (This was the review's
   BLOCKER B1.)
4. **Reuse the existing primitives, do not re-implement them.** Text-node escaping calls the
   same `slackutilsx.EscapeMessage` that `EscapeMrkdwn` wraps (so the two escaping notions
   cannot drift); scheme validation aligns with `isHTTPSURL` (`notifier.go:1242`).
   **Correction (as-built):** `isHTTPSURL` is CASE-SENSITIVE at HEAD (`strings.HasPrefix(u,
   "https://")`) and guards TRUSTED forge URLs, so the renderer does NOT modify or call it;
   it does its own case-insensitive scheme check (`hasHTTPSScheme`, `EqualFold`) on the
   untrusted destination, keeping the two paths independent while enforcing the same
   https-only rule.
5. **Robust on malformed / pre-truncated input.** The judge caps `summaryMd` to 600 runes
   *before* the renderer sees it (`reviewSummaryPreview` → `notificationBlocks`), so the
   renderer must degrade an unterminated `[label](htt` or an unclosed `**` to literal
   escaped text rather than emitting a half-open link or re-opening formatting. A parser
   degrades unclosed constructs to text for free — another reason for Decision 1. The
   renderer must also never leave a dangling unbalanced marker or half-written `<url|` after
   the section-level `truncateForSlackSection` cap (render-then-truncate where feasible;
   otherwise the renderer's own output must be balanced).
6. **`ScrubSecrets` still runs on every outbound body**, and `fallbackText` is still built
   from fixed/escaped fields only (`notificationFallback` `notifier.go:366`, `flushChatTurn`
   `boundReason`) — never a raw model body that could carry a live token. The fallback stays
   *escaped, not rendered*, so literal `**` may appear in the OS-notification preview by
   design; Success Criterion 1 is scoped to block **bodies**, not fallback text.

### Non-goals

- No change to which content leaves the box, to `ScrubSecrets`, or to the per-field escape
  discipline for trusted fields. Only the *untrusted model-body* render changes.
- No new Slack scopes; no CLI changes.
- **No web-UI behavior change.** `reviewSummaryPreview`'s output also feeds the web inbox
  (`notifysvc.Notification.Payload["summary"]` and `["body"]`, read by
  `web/src/lib/notifications.ts` → `Notifications.tsx`). The newline change must not alter
  what the web inbox shows (Decision 7 keeps the inbox one-liner collapsed).
- No block-shape changes; the renderer emits into the same `section`/`context` objects #268
  built.
- Not a general Markdown library — scope is exactly the constructs uzi's agents emit.

## Milestones

- [x] **M1 — Injection-safe CommonMark→Slack-mrkdwn renderer (goldmark).** A single
  exported function in `api/internal/slacksvc` (e.g. `SlackMrkdwn(s string) string`) built
  as a custom goldmark `NodeRenderer` with the GFM (strikethrough/table) extension enabled,
  performing every conversion above and subsuming `EscapeMrkdwn`'s escaping for untrusted
  bodies. Promote goldmark from indirect→direct in `api/go.mod` (`go mod tidy`). Adversarial
  unit suite is a gate on this milestone, covering: injected `<@U123>`; injected
  `<https://evil|Open>`; `[a](https://x/?q=|<@U0>)` and a label containing `>`/`|` (the B1
  cases); a non-https `[x](http://…)` / `[x](javascript:alert(1))` (degrade to text);
  unbalanced `**` and unterminated `[l](htt` (pre-truncated input); `**` inside a code span
  (stays literal); nested/adjacent emphasis; a cap landing mid-construct. `task gate:api`
  green.
  <br>**Delivered** (`api/internal/slacksvc/mrkdwn.go`, commit `b4f6f3c1`): implemented as an
  `ast.Walk` over the goldmark AST (equivalent to a NodeRenderer for our single-pass needs),
  with **only** the Strikethrough + Table extensions enabled — deliberately NOT the full
  `extension.GFM`, which also bundles Linkify (would autolink bare URLs into live markup) and
  TaskList. Code content has its backticks zero-width-space-neutralised so a `` ``` `` run in
  code cannot break out of the code context. Also made `truncateForSlackSection` (gate.go)
  link-safe — it now drops a trailing unbalanced `<` so a cut inside an emitted `<url|label>`
  cannot re-open injection.
- [x] **M2 — Chat surface uses the renderer.** `renderChatBody` (`chatpost.go:352`) routes
  through `SlackMrkdwn`; this single change covers the chat per-turn answer **and** the two
  chat cards (proposal chatactions.go:405, run-request :547) that share the helper. Chat
  notifier/card tests updated; add cases asserting `**x**` → `*x*`, bullets/links render,
  and an injected mention stays inert. Tests updated.
  <br>**Delivered** (commit `171fb0a4`). Note: the chat OS-notification **fallback** is kept
  ESCAPED, not rendered — a sibling `renderChatFallbackText` feeds `flushChatTurn`'s fallback
  in parallel with the rendered body, so a model https link becomes a live `<url|label>` only
  in the block, never in the notification preview (safety contract §6). Test:
  `TestChatFallbackStaysEscapedNotRendered`.
- [x] **M3 — Plan/gate body uses the renderer.** `planThreadBlocks` (`gate.go:266`) routes
  through `SlackMrkdwn`; the plan renders real headings/bullets/bold. The trusted per-field
  and template escapes in `chatactions.go` (`cardField` :438, `chatResolvedBlocks` :468) and
  the assembled-chrome truncation sites (:415/:551) are **left on `EscapeMrkdwn`** — they
  carry intentional chrome markup and are not untrusted blobs (Decision 2/4). Tests updated.
  <br>**Delivered** (commit `171fb0a4`).
- [x] **M4 — Judge notification + shared `notificationBlocks` + newline fix (the tricky
  one).** `notificationBlocks` body (`notifier.go:346`) routes through `SlackMrkdwn`; this
  block is **shared by four producers** (judge, selfimprove started, selfimprove skipped,
  schedule-paused) — the other three post server-authored prose today, so the renderer is a
  safe superset, but add tests asserting all three still render unchanged. `reviewSummaryPreview`
  (`judge_worker.go:364`) stops collapsing newlines for the Slack body (keep scrub + rune
  cap on the multi-line text), while the web inbox one-liner (`reviewNotificationBody`)
  collapses newlines itself so the inbox is unchanged (Decision 7); add a web-side test that
  the inbox row is still a one-liner. Resolve the blockquote/code-fence collision:
  `notificationBlocks` prefixes every line with `> `, which — once newlines are preserved and
  fences pass through — would inject `> ` into a fenced block's inner lines and break code
  rendering (Decision 6). `fallbackText` discipline unchanged. Tests updated.
  <br>**Delivered** (commit `445d376a`). Decision 6 is resolved via an **AST signal**, not a
  substring scan: the renderer exposes `SlackMrkdwnBlock(s) (string, bool)` reporting whether
  a real fenced/indented code block was emitted, and `notificationBlocks` skips the `> `
  blockquote only when that is true — so a literal `` ``` `` appearing in *prose* (a Text
  node, not a fence) is still correctly blockquoted. The OS-notification fallback is flattened
  to one line and stays escaped.
- [x] **M5 (added) — Clarification-question body uses the renderer.** `questionThreadBlocks`
  (`question.go:146`) was found to be a **fourth** untrusted model-body site on the identical
  `truncateForSlackSection(EscapeMrkdwn(ScrubSecrets(...)))` pipeline (the root-cause table
  above enumerated three; this peer was missed). It now routes `questionBody(p)` through
  `SlackMrkdwn` so clarification questions render like the other surfaces. Delivered with
  M2/M3 (commit `171fb0a4`); `question_test.go` updated.
- [x] **M6 — Docs + acceptance smoke.** `docs/slack.md` describes what markdown the bot
  translates and the safety contract; `ARCHITECTURE.md` §"Slack integration" notes the
  renderer as the untrusted-model-body path. `web/scripts/check-docs.mjs` green. Acceptance
  is **fixture-based**: a live Socket-Mode DM is not reachable from the run environment, so
  each surface is validated by its render + injection unit tests (chat, judge/notification,
  plan, and question) asserting `**x**`→`*x*`, bullets/https-links render, and injected
  mentions/links stay inert — `task gate:api` and `task gate:web` green.

## Success criteria

1. In a chat, judge, and plan DM (live or fixture-simulated), `**bold**`, `*italic*`,
   `# heading`, `- bullet`, `[label](https://…)`, and `~~strike~~` render as Slack
   formatting — **no literal markdown source characters** (`**`, `##`, `[..](..)`) appear in
   any bot DM **block body** (fallback text is out of scope by design — Decision, safety
   contract §6).
2. Injection safety is preserved and tested: an attacker-controlled `<@U123>` mention, a
   `<https://evil|Open>` link, a `[l](https://x/?q=|<@U0>)` link whose URL/label carries
   `< > |`, a non-https `[x](scheme:…)` link, and raw `< > &` in a model body cannot produce
   a live mention, a live/mis-targeted link, or unescaped markup. Regression tests assert
   each.
3. The judge Slack body preserves paragraph/list structure (newlines no longer collapsed)
   and still respects the rune cap and Slack's 3000-char section limit; a fenced code block
   in a judge/plan body renders as code, not as `> `-prefixed lines.
4. The web inbox is unchanged: the notification row still shows a collapsed one-liner
   (verified by a web test).
5. The three non-judge `notificationBlocks` producers (selfimprove ×2, schedule-paused)
   render unchanged.
6. Every model-body block still passes through `ScrubSecrets`; `fallbackText` is still built
   from fixed/escaped fields only. `task gate:api` (and `task gate:web` for the inbox test) green.

## Decision Log

- **Decision 1 — Parser-based renderer using goldmark (RESOLVED; the delegated architect
  call).** The killer reason is not "better nesting": in CommonMark a single `*` is
  *emphasis* (→ Slack `_italic_`) while in Slack a single `*` is *bold*, so a purely lexical
  rewrite cannot map `*` without first knowing from structure whether it was emphasis or
  strong. An AST makes the transform unambiguous and reduces injection-safety to a clean
  node-typed walk (safety contract §1–3), which is near-untestable as regex over
  nested/adjacent/code-span input. **goldmark is already in `api/go.mod`** (v1.7.17, indirect
  via `charm.land/glamour/v2`, which `api/cmd/uzi` uses — glamour is itself a goldmark
  renderer), so Decision 1 adds no new supply-chain review; it promotes an existing pinned
  module to direct and newly links it into the server binary. GFM extension must be enabled
  for `~~strike~~` and table handling (§N4 of review). Hand-rolled is rejected explicitly,
  with the `*`-ambiguity as the decisive reason.
- **Decision 2 — The renderer replaces `EscapeMrkdwn` only on UNTRUSTED model blobs.** The
  three sites in the root-cause table (`renderChatBody`, `planThreadBlocks`,
  `notificationBlocks` body) are whole-blob untrusted text; `SlackMrkdwn` owns their escaping
  (double-escaping would corrupt the emitted `<url|label>`). The per-field trusted paths
  (`cardField` chatactions.go:438, `chatResolvedBlocks` :468, the assembled-chrome
  truncation at :415/:551, and every repo/title/agent-name field) stay on `EscapeMrkdwn` —
  they carry intentional chrome markup and are not free-text blobs. The two helpers are
  applied to disjoint inputs.
- **Decision 3 — Renderer must be safe on malformed/pre-truncated input.** The judge caps
  `summaryMd` *before* rendering, so "produce balanced output then truncate" is insufficient
  — the renderer must degrade unclosed/half constructs to literal escaped text (a parser
  does this for free). Section-level `truncateForSlackSection` must additionally not leave a
  dangling unsafe token; render-then-truncate where feasible (safety contract §5).
- **Decision 4 — Scope is the untrusted model-body paths, not "all DMs".** Status
  roots, facts, milestone counters, deep links, and gate/question/card *chrome* are built
  from fixed strings and closed enums that intentionally carry mrkdwn and are already correct
  (#268). Touching them risks regressing intentional markup for no gain.
  **Correction (as-built):** the untrusted model-body paths turned out to be **four**, not
  three — the clarification-question body (`questionThreadBlocks`, `question.go:146`) is a
  peer of chat/plan/notification and is now in scope (added M5). The *chrome* exclusions above
  are unchanged.
- **Decision 5 — https-only links (product-owner decision, 2026-08-10).** Only `https`
  destinations become `<url|label>`; every other scheme degrades to escaped text. Matches
  the repo's untrusted-URL precedent (`isHTTPSURL` notifier.go:1242; `FORGE_ALLOWED_BASE_URLS`
  https-only SSRF guard). **Correction (as-built):** the renderer does NOT reuse the shared
  `isHTTPSURL` (case-sensitive, guards trusted forge URLs); it applies its own
  case-insensitive `hasHTTPSScheme` to the untrusted destination and percent-encodes any
  `< > |`/whitespace in the accepted URL.
- **Decision 6 — Blockquote/fence collision in `notificationBlocks`.** Once M4 preserves
  newlines and fences pass through, the `> `-per-line prefix (`notifier.go:347-350`) would
  corrupt a fenced block. Resolve by not blockquoting a body that contains a code fence (or
  degrading fences inside the quoted body); pin with a test. Today's newline-collapse masks
  this, so it is a new-exposure of M4, not a pre-existing bug.
- **Decision 7 — Keep the web inbox one-liner collapsed.** `reviewSummaryPreview` feeds both
  the Slack body (wants multi-line) and the web inbox payload (wants a one-liner). Preserve
  newlines for the Slack body, and have the inbox one-liner (`reviewNotificationBody`)
  collapse them itself, so the "no web-UI change" non-goal holds; add a web test.

## Risks & mitigations

- **Injection regression** (highest risk): a converter that emits raw `<…>` from model text,
  or an unsanitized `<url|label>`, would be strictly worse than today's blunt escape.
  Mitigation: M1's adversarial suite (safety contract §1–5, incl. the B1 URL/label cases) is
  a gate on the milestone, not an afterthought; escaping reuses `slackutilsx.EscapeMessage`
  and scheme-check reuses `isHTTPSURL` so the notions can't drift.
- **Truncation splitting markup**: capping mid-`**` or mid-`<url|`, incl. the judge's
  pre-render 600-cap. Mitigation: Decision 3 + tests for a cap landing inside each construct
  and for pre-truncated input.
- **Shared-render blast radius**: `renderChatBody` (3 chat surfaces) and `notificationBlocks`
  (4 producers) each fan out. Mitigation: M2/M4 explicitly test the co-tenants render
  unchanged.
- **Over-conversion of code content**: `**`/`[x](y)` inside a code span/fence must stay
  literal. Mitigation: renderer treats code nodes as opaque (escape `&<>` only), with a test.
- **New server-binary link of goldmark**: cheaper than a new dependency (already pinned +
  sumdb-verified, already in the module graph), but the server binary newly links it.
  Mitigation: recorded here; `go mod tidy` + gate covers it.
