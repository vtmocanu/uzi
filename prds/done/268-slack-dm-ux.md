# PRD #268: Slack DM UX — deliver chat answers to the thread, and give every DM real Slack formatting

**GitLab Issue**: [vtmocanu/uzi#268](https://github.com/vtmocanu/uzi/-/issues/268)
**Status**: Done (2026-08-09)
**Priority**: High
**Created**: 2026-08-09
**Depends on**: PRD #191 (Slack as a conversational surface — done; this fixes and polishes what it shipped)
**Related**: `docs/slack.md`, `api/internal/slacksvc/`, `ARCHITECTURE.md` §"Slack integration (outbound-only)"

> Every file:line citation below was derived at **`c1dc9e39`** (v0.23.0). A line number
> without a SHA is not a citation — re-derive before acting.

## Problem

Two defects in the Slack surface PRD #191 shipped (v0.23.0), both observed live in the
product owner's own 1:1 DM with the `uzi` bot.

### Problem 1 (bug): chat answers never reach the Slack thread

Opening a chat from Slack (a top-level DM) and every follow-up turn resolves in the
thread to:

```
_(this turn produced no text reply — open it in uzi for the details)_
https://uzi.example.com/chat/<id> - Open in uzi
```

…while the **real answer is present on the web chat page**. The agent answered; its
answer text never made it into Slack. Reproduced twice in one session ("hey", then
"what runs do we have now?"), both turns empty in Slack, both answered on web.

**Root cause** (traced at `c1dc9e39`). A chat turn is coalesced by
`applyChatFrame` in `api/internal/slacksvc/chatpost.go`. The turn state machine is:

- `user_message` frame → `startChatTurn` posts the `💬 _uzi is thinking…_` placeholder and sets `convo.active = true` (chatpost.go:164-168, 247-262).
- `text` frames → buffered **only while `convo.active`** (chatpost.go:169-176).
- a terminal frame resolves the turn: `status` with `event=="result"`, an `error`, or a **prose `status`** (timeout / conversation-end line) — `flushChatTurn` edits the placeholder to the assembled body (chatpost.go:177-190).

The bug is in the `status` case (chatpost.go:177-190):

```go
case "status":
    if p.Event == "result" {
        n.flushChatTurn(ctx, convo, "") // happy-path turn end
        return
    }
    // A prose status … resolves an open turn …
    if convo.active {
        n.flushChatTurn(ctx, convo, chatDynamic(p.Text))
    }
```

The real Claude Agent SDK emits a **`system:init`** frame at the start of every query.
`mapSdkMessage` maps it to a run message `{ kind: "status", payload: { event: "init", model } }`
(`agent/src/sdk-messages.ts:246-258`) — a **status frame with no `text` and `event != "result"`**.
It is persisted and broadcast like any other run message, and `chatRelevantKind`
lets `status` through (chatpost.go:59-66). So the frame ordering for turn 1 is:

1. `user_message` → placeholder posted, `active = true`, buffer empty.
2. **`status` `event:"init"`** → `p.Event != "result"`, `convo.active == true` → `flushChatTurn(convo, chatDynamic(""))` → body is the empty buffer → resolves to `chatNoAnswerText`, `active = false`.
3. the assistant's `text` frames arrive → `active == false` → **dropped**.
4. `status` `event:"result"` → `active == false` → no-op.

The turn is flushed empty by the init heartbeat before a single answer token is
buffered. The web feed is unaffected because it renders persisted `run_messages`
directly and never goes through this coalescer.

**Why tests did not catch it.** The e2e/unit path uses `ChatExecutorStub`
(`agent/src/chat-executor-stub.ts`), which emits only `text` then a `status`
`event:"result"` — it **never emits a `system:init` frame**, so the premature-flush
path is unreachable in tests. There is no notifier test exercising a text-less,
non-result `status` during an active turn (`grep init` in the chat notifier tests is
empty).

### Problem 2 (formatting): every DM is a plain-text dump

The bot's status and judge DMs are plain-text, single-line dumps with no visual
hierarchy. Corrected baseline (from the Slack UX review — some of the first-pass
symptoms were half-right):

- **Redundant `[uzi]` prefix** on exactly three sites: `renderRoot` (`notifier.go:745`), `renderNotification` (`notifier.go:302`), `SendTestDM` (`linker.go:221`). In a 1:1 DM with the `uzi` bot the sender is already `uzi` — the prefix is pure noise. Nowhere else has it.
- **Inconsistent status glyphs — this is the "raw/ugly emoji" complaint.** `statusLabel` / `limitWaitLabel` / `healthNudgeHead` use **text-presentation** symbols with no emoji variation selector (`▶` U+25B6, `⏸` U+23F8, `⚠` U+26A0, `✓` U+2713 for milestones) — thin monochrome glyphs — right beside full-color emoji (`✅ ❌ 🚫 💬`). Mixed presentation reads as broken. (The MCP history renders these as `:arrow_forward:` / `:white_check_mark:` shortcodes, which is what surfaced them.)
- **The deep link is already `<url|Open in uzi>` mrkdwn** (`runLink`/`notifyLink`, `notifier.go:315`/`1044`) — *not* a bare URL — but it sits as a naked trailing text line with no visual role. The fix is to promote it to a `context` block / button, not to "add a link".
- **No Block Kit structure**: everything is one plain-`Post` line. The judge "Run review ready" DM dumps the whole verdict + summary as one unbroken paragraph, no hierarchy, no blockquote lane, no truncation discipline beyond a 280-rune preview.

What the owner sees in the DM (raw message text; the link is real mrkdwn):

```
[uzi] run on vtmocanu/uzi#246 «Trusted-repo context…» — ▶ running · 0/4
<https://uzi.example.com/runs/…|Open in uzi>

[uzi] Run review ready — verdict: ok — 1 recommendation: <giant unbroken paragraph>
<https://uzi.example.com/judge?run=…|Open in uzi>
```

The transport already supports Block Kit — `poster.go` has `PostBlocks` / `UpdateBlocks`
(chat status, gate, question, cards already use them) — so the status DMs (A) and the
generic notification DMs (D, incl. judge) are the plain-`Post` holdouts, not a capability
gap. **Two transport migrations (`renderRoot` + `renderNotification`) do ~80% of the work.**

## Solution

Two independent tracks under one PRD; the bug fix (M1) ships first and stands alone.

### Track A — fix the dropped chat turn (M1)

In `applyChatFrame`, a text-less status frame must **not** resolve an open turn. The
intended contract (its own comment) is "a **prose** status … resolves an open turn";
`event:"init"` carries no prose. Guard the flush on the presence of resolving content:

```go
case "status":
    if p.Event == "result" {
        n.flushChatTurn(ctx, convo, "")
        return
    }
    // Only a status that carries TEXT (a timeout line, the conversation-end line)
    // resolves an open turn. A text-less heartbeat (event:"init", and any future
    // eventless status) must not flush the turn before its answer is buffered.
    if convo.active && strings.TrimSpace(p.Text) != "" {
        n.flushChatTurn(ctx, convo, chatDynamic(p.Text))
    }
```

Guarding on "has text" rather than special-casing `event=="init"` is deliberate: it is
robust to any future text-less status heartbeat, and it matches the comment's own words.
(An explicit `event=="init"` skip is the narrower alternative — see Decision 1.)

The regression test must emit the **real** frame order including a `system:init` status
between `user_message` and the answer `text`, and assert the thread shows the answer —
not the stub's abbreviated stream. This closes the "stub never emits init" test gap.

### Track B — Block Kit formatting overhaul (M2–M4)

A consistent house style across every bot message, built from a Slack UX proposal (see
the *Formatting house style* section, populated from the `web-ux` agent's proposal):

- **Drop the `[uzi]` prefix** everywhere (it is a 1:1 DM with the bot).
- **Lead with a rendered status emoji** (▶️ running, ✅ completed, ⏸️ awaiting approval, ❓ needs answer, ❌ failed, 🚫 cancelled, ⚠️ health flag) instead of raw shortcodes.
- **Block Kit structure**: `section` for the primary line with mrkdwn, `context` for secondary metadata (repo#issue, milestone counter, timestamps), `actions` for deep links / buttons, `divider` where it aids scanning.
- **Links as `<url|label>`** or an "Open in uzi" button, never a bare URL line.
- **Bound long bodies**: Block Kit `section.text` caps at 3000 chars — judge/plan bodies get a headline + a truncated excerpt + an "Open in uzi" affordance for the full text.
- **Keep the safety pipeline**: all model-authored text still goes through `ScrubSecrets` + `EscapeMrkdwn` even inside blocks; every `PostBlocks` carries a good `fallbackText` (that is what shows in the OS notification).

Message families to convert (grouped by the Slack UX inventory, letters A–L in the
*Formatting house style* section):
- **M2 — status root (A) + glyph canonicalization + `[uzi]` deletions.** Migrate `renderRoot` (A) from plain `Post`→`PostBlocks`/`UpdateBlocks`; move the milestone/health suffixes off the label string into a `context` block; canonicalize `statusLabel`/`limitWaitLabel`/`healthNudgeHead`/milestone to the one emoji-presentation set; delete `[uzi]` from the three sites; restyle the test DM (K2). This slice owns the shared `statusLabel`, so it lands the glyph set once for everyone.
- **M3 — generic notification (D) + terminal thread events (B) + health (E).** Migrate `renderNotification` (D) to `PostBlocks`. To carry `Emoji` + `Facts []string` to the renderer, **widen the cross-package delivery seam**: `Slacker.PublishNotification(userID, title, body, link string)` (`notifysvc/service.go:46`, called :134, implemented `slacksvc/notifier.go:186`) takes positional args, so prefer passing a struct (the existing `notifysvc.SlackRender` at service.go:76, or a neutral DM struct) through `PublishNotification` — a mechanical but real interface change that also touches two test fakes (`notifysvc/service_test.go:56`, `notifier_notify_test.go:115`) and the `notifyEvent`→`renderNotification` chain; builders that fill it (judge_worker.go:259, selfimprove/engine.go:266/293, schedsvc/scheduler.go:468) are additive. Give the judge DM a headline + verdict glyph + blockquote excerpt + Open-in-uzi (bump the summary preview from 280→~600 runes); restyle terminal thread events (`renderThread`: completed/failed/cancelled/limit_wait — failure reason stays a full `section`, never a `context`) and the health nudge (E).
- **M4 — chat surface (F, G polish).** Per-turn answer as a `section` (model markdown renders) + `context` deep link **instead of** appending `\n\n<link>` into the body (chatpost.go:284); placeholder as a `context` block; the empty-turn degrade (post-M1, rare) as a `context` block; proposal/run-request/chat-status copy + emoji polish. Depends on M1 so the answer is real.
- Gate (H), question (I), cards (J), link confirm (K) are already Block Kit and structurally sound — **copy/emoji polish only, folded into M2–M4, no rework.**

### Non-goals

- No change to the run lifecycle, the notification delivery rules, the scrub/escape safety model, or which content leaves the box.
- No new Slack scopes (Block Kit sections/buttons/actions all work under the existing manifest).
- No web-UI or CLI changes.

## Milestones

- [x] **M1 — Chat turn no longer dropped by the init heartbeat.** `applyChatFrame` guards the status flush on resolving content; a **slacksvc notifier regression test** (Go, not the agent stub) emits the real `user_message → status:init → text → status:result` order and asserts the thread carries the answer. `task gate:api` green.
- [x] **M2 — Status root (A), glyph canonicalization, `[uzi]` deletions.** `renderRoot`→`PostBlocks`/`UpdateBlocks`; progress/health moved to a `context` block; one emoji-presentation glyph set across `statusLabel`/`limitWaitLabel`/`healthNudgeHead`/milestone; `[uzi]` removed from `notifier.go:302`, `notifier.go:745`, `linker.go:221`; test DM (K2) restyled. Every new `PostBlocks` carries a fixed/escaped `fallbackText`. Tests updated.
- [x] **M3 — Generic notification (D) + terminal thread events (B) + health (E).** `renderNotification`→`PostBlocks`; widen `Slacker.PublishNotification` to carry a struct (Emoji/Facts) + update its two test fakes; judge DM = headline + verdict glyph + blockquote excerpt (preview 280→~600) + Open-in-uzi; self-improve/scheduled restyled; `renderThread` completed/failed/cancelled/limit_wait restyled (failure reason full `section`); health nudge restyled. Tests updated.
- [x] **M4 — Chat surface (F).** Per-turn answer as `section` + `context` deep link (link out of the body); placeholder + empty-turn degrade as `context`; chat-status/proposal/run-request copy + emoji polish. Depends on M1. Tests updated.
- [x] **M5 — Docs.** `docs/slack.md` updated to describe the new message shapes and glyph set; examples refreshed. `web/scripts/check-docs.mjs` green.

## Success criteria

1. A Slack chat turn ("hey", "what runs do we have now?") shows the agent's **actual answer** in the thread, not `chatNoAnswerText`, with the web and Slack answers matching.
2. No bot DM contains the literal `[uzi]` prefix or a raw `:shortcode:` in body text.
3. Every run-status, judge, health, chat, and link DM is a Block Kit message with a rendered status glyph, a link affordance (not a bare URL), and a populated `fallbackText`.
4. Long judge/plan bodies never exceed Block Kit limits and always offer the full text via an Open-in-uzi affordance.
5. All model-authored text inside blocks still passes through `ScrubSecrets` + `EscapeMrkdwn`. `task gate:api` and `task gate:agent` green.

## Decision Log

- **Decision 1 — Guard the status flush on "has text", not on `event=="init"`.** The narrower `event!="init"` skip fixes today's symptom; the "has text" guard also covers any future text-less status heartbeat and matches the code's own "a *prose* status resolves an open turn" contract. Chosen the broader guard. (If review prefers the explicit init skip for legibility, that is an acceptable substitute — both fix the reproduced bug.)
- **Decision 2 — One PRD, bug first.** The bug (M1) is a two-line fix + test and is independently shippable; the formatting overhaul (M2–M4) is larger and cosmetic. Bundled because both are "the Slack DM feels wrong" and share the same files, but M1 must not wait on the redesign.
- **Decision 3 — Reuse the existing Block Kit transport and safety pipeline.** `PostBlocks`/`UpdateBlocks` already exist and are used by the gate/question/proposal cards; the redesign extends that pattern to the plain-`Post` holdouts rather than introducing anything new. No new Slack scopes.
- **Decision 4 — Formatting details come from the `web-ux` (Slack UX) agent proposal**, folded into the *Formatting house style* section below and reconciled during review.

## Formatting house style

From the `web-ux` Slack UX agent's proposal (read against `notifier.go`, `judge_worker.go`,
`chatactions.go`, `docs/slack.md` at `c1dc9e39`). Implementation-ready.

### Message inventory + verdict

| # | Message | Builder (file:line) | Transport today | Action |
|---|---|---|---|---|
| A | Run status root (`[uzi] run on repo#iid «title» — status`) | `renderRoot` notifier.go:744 | `Post`/`Update` plain | **→ PostBlocks/UpdateBlocks** (biggest win) |
| B | Terminal thread event (completed/failed/cancelled/limit_wait) | `renderThread`/`limitWaitLabel` notifier.go:916/954 | `Post` plain | **→ PostBlocks** (failed & limit_wait especially) |
| C | Milestone progress (`✓ N/M · title`) | `handleMilestone` notifier.go:852 | `Post` plain | → one `context` block (low priority) |
| D | Generic notification — judge "Run review ready", self-improve, scheduled-run paused | `renderNotification` notifier.go:301; body `reviewNotificationBody` judge_worker.go:270 | `Post` plain | **→ PostBlocks** (2nd-biggest win) |
| E | Run-health nudge | `healthNudgeText` health.go:171 | `Post` plain | **→ PostBlocks** |
| F | Chat per-turn answer + `💬 _thinking…_` placeholder | `flushChatTurn`/`startChatTurn` chatpost.go:267/247 | `Update`/`Post` plain | **→ section + context** |
| G | Chat status root (End/Continue) | chatactions.go:483/500 | PostBlocks | copy/emoji polish |
| H | Plan gate + plan body | `gateBlocks`/`planThreadBlocks` gate.go:111/266 | PostBlocks | add header + plan-body context header |
| I | Clarification question | `questionThreadBlocks` question.go:137 | PostBlocks | leading `❓` |
| J | Proposal / start-run card | chatactions.go:396/541 | PostBlocks | minor |
| K | Account-link Confirm/Not-me | `linkConfirmBlocks` linker.go:285 | PostBlocks | fine |
| K2 | Test DM (`[uzi] test message…`) | `SendTestDM` linker.go:221 | `Post` plain | drop `[uzi]`, add ✅ |
| L | Resolved-gate/card edits, ephemerals | gate/gatekeeper/chatactions | Blocks + ephemeral | fine as-is |

### House-style rules (every message)

1. **No `[uzi]` prefix** — remove from the three sites above (nowhere else has it).
2. **Lead with a rendered status emoji, always paired with a text word** (never color-only — a11y). Canonical, all emoji-presentation:

| State | Emoji | Label |
|---|---|---|
| queued | ⏳ | Queued |
| claimed/running | ▶️ | Running |
| awaiting_approval | ⏸️ | Needs your approval |
| awaiting_input | ❓ | Needs your answer |
| limit_wait | ⏸️ | Paused · usage limit |
| completed | ✅ | Completed |
| failed | ❌ | Failed |
| cancelled | 🚫 | Cancelled |
| MR/PR link | 🔀 | View MR/PR |
| milestone | 🧩 | N/M milestones (replaces monochrome `✓`) |
| health stalled/looping/slow/waiting/approval-idle | 💤 / 🔁 / 🐢 / ⏳ / ⏸️ | |
| chat | 💬 | |
| issue proposal | 📝 | |
| self-improvement | 🔧 | |
| judge review | 🔎 | verdict ✅ ok / ⚠️ needs-changes / ❌ bad |

3. **Repo#iid as inline `code`; issue/plan title bold** (both already `EscapeMrkdwn`'d).
4. **Deep-link placement:** passive status/notification → link in a `context` block (`🔗 <url|Open in uzi>`), compact. Actionable (gate, cards) → keep the URL **button** in the actions row.
5. **Move progress + health OUT of the status-label string into a `context` block** (today `milestoneSuffix` notifier.go:760 and `healthSuffix` health.go:113 glue `· 3/7`, `· ⚠ slow` onto the label).

### Before / after — the two worst offenders

**A. Run status root** (edited in place via `UpdateBlocks`, same root_ts anchor):

```
section : ▶️ *Running*
          `vtmocanu/uzi#246`  ·  *Trusted-repo context sync*
context : 🧩 0/4 milestones      🔗 <runURL|Open in uzi>
```
completed → `✅ *Completed*` + context `🧩 4/4 milestones   🔀 <mrURL|MR !312>   🔗 <runURL|Open in uzi>`;
health-flagged adds `⚠️ slow` as a context element (not a label suffix).
fallbackText: `Running · vtmocanu/uzi#246 — Trusted-repo context sync`.

**D. Judge "Run review ready"**:

```
section : 🔎 *Run review ready*
context : Verdict ✅ *ok*  ·  1 recommendation  ·  `correctness`
section : > {scrubbed + escaped summary, markdown preserved, rune-capped}
context : 🔗 <judgeURL|Open the review in uzi>
```
fallbackText: `Run review ready — verdict ok, 1 recommendation`. Recommendation
categories (`recommendationCategories`, a closed enum) render as `code` chips.
Implementation: give the renderer `Emoji string` + `Facts []string` so it renders
per-kind without title string-matching. This is a **cross-package interface change**,
not just a struct edit — `Slacker.PublishNotification(userID, title, body, link string)`
(notifysvc/service.go:46) is positional, so widen it to pass a struct (reuse
`SlackRender`, service.go:76) and update the two test fakes (notifysvc/service_test.go:56,
notifier_notify_test.go:115); `buildReviewNotification` fills `Emoji:"🔎"`, self-improve `Emoji:"🔧"`.

### Terminal thread events (B) — these DO notify (root edits don't)

- completed → `✅ *Completed*` + actions `[View MR ↗] [Open in uzi ↗]`.
- failed → `❌ *Failed*` + a **full `section`** with the escaped, `boundReason`-truncated reason (never a `context` block) + context deep link.
- cancelled → `🚫 *Cancelled*` (borderline; may stay plain).
- limit_wait → `⏸️ *Paused · usage limit*` + context `{rate type} · resumes <!date^…> · pause N` (preserve the existing local-timezone `<!date^…>` token from limitWaitLabel notifier.go:969).

### Truncation / overflow (section text hard cap = 3000)

- Keep `truncateForSlackSection` (gate.go:246, 2900-rune, rune-safe) for every model-authored body (plan, question, chat answer, card, judge summary); on overflow it appends `…` and the full content lives behind the deep link.
- **Never truncate a link** — keep every `<url|label>` in its own block outside the truncated region (already the discipline in planThreadBlocks/questionThreadBlocks).
- Bump the judge summary preview 280→~600 runes (judge_worker.go:285); do not dump the full summary.
- Multi-field cards already bound the **whole assembled section** (chatactions.go:411-415) — Slack rejects the entire payload on overflow, so the card would silently never post. Keep that rule for any new multi-field section.

### mrkdwn safety — do NOT regress when moving to blocks

- Keep `ScrubSecrets` on every outbound string / dynamic field (blocks are not exempt).
- Keep **per-field** `EscapeMrkdwn` for forge/model fields beside trusted `<url|label>` markup (repo path, title, failure reason, agent names, labels) — redact.go:20 explains why per-field.
- **Whole-blob** `EscapeMrkdwn` only for a body carrying no trusted markup of its own (plan, question, chat answer) — the documented exception (gate.go:259). Never escape the assembled message.
- 🔴 **`fallbackText` is parsed for mrkdwn AND mentions even when the blocks are inert** (chatactions.go:338 already learned this). Every NEW `PostBlocks` must build `fallbackText` from fixed strings + escaped/scrubbed fields, never a raw model/forge title. **The single easiest thing to get wrong in the migration.**
- `header` blocks are `plain_text` (inert) but capped at 150 chars — FIXED labels only; untrusted model text stays in sections.

### Accessibility / fallback

- Every message that becomes `PostBlocks` gains a hard requirement: a good `fallbackText` (OS/mobile notification + screen-reader text). Format `{Status} · {repo}#{iid} — {title}`, no `[uzi]`. A and D use plain `Post` today with no separate fallback — the migration must add one.
- Never convey status by emoji/color alone — always the text word beside it.
- Prefer emoji-presentation glyphs (the table); the current `▶`/`⏸`/`⚠`/`✓` text-symbols read inconsistently across platforms and screen readers.
- Failure reasons and plan bodies stay in full `section` blocks, never `context` (context is de-emphasized).
