# PRD #116 — Distinguish handled guardrail denials from errors in the activity feed

**Issue**: [#116](https://gitlab.example.com/vtmocanu/uzi/-/issues/116) · **Label**: PRD · **Priority**: Low
**Status**: **DONE** — M1–M3 complete on branch `agent/issue-116`. Closed 2026-07-27. See
"As built" below for the five departures from this document.

## Problem

When a PreToolUse guardrail denies a call, the run activity feed renders it with
the same red **"✗ error"** treatment as a genuine tool crash. Observed live on the
#115 run: the lead tried to spawn the SDK built-in `Explore` subagent, the
subagent guard denied it (`REASON_UNKNOWN_SUBAGENT`), the lead recovered by using
`researcher`, and the run continued fine — yet the feed showed a red "error"
badge. The guardrail firing is the system working **as designed** (defense-in-depth
+ cost control, `agent/src/guardrails.ts` M4 audit item 7 / issue #34), and the
agent recovered, so painting it as an error is misleading: it makes a healthy run
look broken and prompts "is this a bug?" from anyone watching.

This is not specific to the subagent guard. All **15** guardrail denials share the
stable, deliberate prefix `"denied by guardrail: "` (`guardrails.ts:92-111`):
git push / remote / force / config-read / config-write, env / ps / /proc /
secret-file reads, docker-no-daemon / docker-redirect, command-nesting-depth,
file-outside-worktree, .git-access, and the unassembled-subagent case. Every one
currently renders as a red error. (Two hooks also have a colon-less
`?? "denied by guardrail"` fallback at `guardrails.ts:539,786` — practically dead,
since every `deny()` carries a reason, but the match must tolerate it.)

## Solution

Introduce a third `tool_result` presentation state — **"blocked"** — for results
whose text carries the `"denied by guardrail: "` prefix, rendered with a calm,
non-alarming tone/label instead of the red danger treatment. The classification
is purely presentational; it does not change whether the result is an error for
any executor/health/judge purpose (see Decision 2).

Current states → new:

| result | today | after |
|--------|-------|-------|
| genuine failure (`is_error`, non-guardrail) | red "✗ error", auto-expands | unchanged |
| guardrail denial (`"denied by guardrail: …"`) | red "✗ error", auto-expands | neutral "⊘ blocked" chip (warn-tinted glyph), collapsed by default |
| success | "✓ ok" / "✓ N lines" | unchanged |

## Recommended / open decisions

1. **Web-only detection (recommended primary approach).** Do the whole change in
   the shared `RunEvent` renderer: when `is_error` and the flattened result text
   **contains** `"denied by guardrail"` (substring, colon-optional), render the
   "blocked" state. Use `.includes(...)`, not `.startsWith(...)`, so it tolerates
   both the colon-less fallback and a possible future `<tool_use_error>…</…>`
   wrapper (see Risks); gated on `is_error`, over-match would require a genuinely
   *failing* result whose text contains the exact phrase — negligible.
   - *Pros*: one component (+ tests), zero backend, and it **works for historical
     runs** automatically (no persisted marker required).
   - *Cons*: couples the web to the guardrail reason phrase — but that phrase is
     already a stable, user-facing contract (15 constants, all sharing it), and a
     `guardrails.test.ts` assertion (M2) pins the contract from the agent side.
   - **Open alternative** (heavier, for the reviewer to weigh): tag the message at
     the agent translation layer (`agent/src/sdk-messages.ts` `mapUser`, where the
     `tool_result` block is mapped, ~line 153-160) with an **additive** structured
     field, e.g. `denied: true`, carried through the `jsonb` `run_messages.payload`
     (no migration — payload is opaque JSON). Web renders off the field, falling
     back to the phrase match for pre-existing runs. More robust against an SDK
     bump, but touches agent + protocol + web for a cosmetic change.
     **Recommendation: ship web-only now; add the structured marker only if the
     SDK-wrapper risk below actually materializes.**
2. **`is_error` stays true; "blocked" is a render-time decision only.** Do NOT flip
   a guardrail denial to non-error at the source. Not because the executors would
   break — they would not: `isErrorResult` (`sdk-messages.ts:259-263`) returns
   false for anything but the terminal `type:"result"` frame, and all three
   consumers gate it behind `isResult(msg)` (`sdk-executor.ts:654`,
   `chat-executor.ts:455`, `judge-runner.ts:275`), so a `tool_result` block's
   `is_error` is read **only** by `RunEvent.tsx:557` and nothing else. We keep
   `is_error` true because the persisted `run_messages` record should stay honest
   to exactly what the SDK emitted (and historical frames cannot be rewritten
   anyway). The "blocked" state is layered on top at render time; no run-health,
   error-count, or judge path is touched (verified: a guardrail deny affects none
   of them today).
3. **Blocked results do not auto-expand.** Genuine errors auto-expand with a danger
   tint (`RunEvent.tsx:562`, `useState(isError)`) because they need attention. A
   handled, recovered-from denial does not — collapse it by default (expandable on
   click like a normal result) so it reads as expected, not urgent.
4. **Tone: neutral chip, warn-tinted `⊘` glyph.** Pin one treatment. The chip
   frame reuses the neutral success family (`border-edge bg-raised/50 text-muted`,
   `RunEvent.tsx:604`) so it is visibly *not* a danger error; the leading `⊘`
   glyph carries `text-warn` (the `--warn` token exists in both themes,
   `index.css:40/116`) so it reads as "held/blocked", not "ok". We deliberately do
   NOT tint the whole chip warn — `--warn` already marks the "plan awaiting
   approval" chip (`RunEvent.tsx:765`) and the slow-duration tint (`531`), so a
   full warn chip would collide semantically. Glyph-only warn avoids that.

## User journey

- A run where a guardrail fires now shows a neutral **"⊘ blocked"** chip, collapsed
  by default; expanding it shows the reason text verbatim ("denied by guardrail:
  only the run's assembled subagents may be invoked" — the body is unchanged, only
  the chip label/tone differs). The next line ("I'll use the researcher subagent
  instead") reads as a normal recovery, and the run no longer looks broken.
- A real tool failure is unchanged: red "✗ error", auto-expanded, danger tint.

## Technical scope

- **`web/src/components/RunEvent.tsx`** (`ToolResultBody`, ~lines 555-627): add a
  `blocked` classification (`isError && text.includes("denied by guardrail")`).
  When blocked: label "blocked", leading `⊘` glyph in `text-warn` (not
  `text-danger`), neutral chip frame (reuse the `border-edge bg-raised/50
  text-muted` success family, not `border-danger`), and initial `open=false`.
  Update the a11y labels ("Show blocked output" / "Tool blocked output"); the
  `resultToText`/pairing/body behavior stays as-is (raw reason text preserved).
- **`web/src/components/ChatMessages.tsx`** (`:147`): renders tool results through
  the **same** `RunEventRow`/`ToolResultBody`, so the chat surface inherits the
  blocked chip for free — no separate change, but M2 must pin it (guardrail denies
  do happen in chat: the Bash + path guards are wired in `chat-executor.ts:259`).
- **No backend change** in the recommended approach. (The structured-marker
  alternative would touch `agent/src/sdk-messages.ts` and the protocol type; no DB
  migration either way — `run_messages.payload` is `jsonb`, `00020_workers_runs.sql:76`.)
- **CLI**: no change. `uzi run logs` prints message text verbatim; the "blocked vs
  error" distinction is a visual affordance of the web feed. (Convention check per
  CLAUDE.md; noting it so the reviewer confirms we are not leaving the CLI stale —
  we are not, because there is nothing colored to keep in sync.)

## Milestones

- [x] **M1 — Blocked render state.** `RunEvent.tsx` classifies `"denied by
  guardrail: "` results as "blocked": calm tone/label/glyph, collapsed by default,
  non-danger border. A genuine `is_error` result is visually unchanged.
- [x] **M2 — Tests.**
  - `web/src/components/RunEvent.test.tsx`: a guardrail-deny `tool_result` renders
    "blocked" (not "error"), is collapsed initially (`aria-expanded=false`, body
    `hidden`), uses a non-danger tone, and preserves the raw reason in the body; a
    real `is_error` result still renders "✗ error" and still auto-expands; a
    success result is untouched. The existing error pins (≈`:225-238` auto-expand
    ✗, `:418-425` "Hide Read error output") use non-guardrail text and must stay
    green as-is — do not weaken them.
  - `web/src/components/ChatMessages.test.tsx`: assert the blocked chip renders in
    the chat surface too (same `RunEventRow`).
  - `agent/src/guardrails.test.ts`: assert **all 15** reason constants start with
    `"denied by guardrail"` — pins the phrase contract the web match depends on
    (non-optional; this is the belt that makes the string coupling safe).
  - Mock sample: add a `tool_result` carrying a `"denied by guardrail: …"` reason
    via the `fm()` helper in `web/src/mocks/data.ts` (≈`:1762`, mirror the existing
    `is_error` sample ≈`:1776`; live mock runs set it through `engine.ts:80`'s
    `opts.error → is_error`) so the blocked state is demonstrable without a live run.
  - `npm test` + `npm run typecheck` green.
- [x] **M3 — Spec note.** Append a short decision to `specs/ai.md` (append-only at
  the tail) recording the "blocked" presentation state, the prefix contract it
  keys off, and that `is_error` is deliberately left true (presentation-only).
  (Landed as `specs/ai.md` §418.)

## As built — where the implementation departs from the plan above

Recorded because five things in this PRD did not survive contact with the code.
Each was verified before changing course.

1. **The match is anchored to the START of a LINE, not `.includes` anywhere in the
   text** (Decision 1 and the Risks section both specify `.includes`). The anchor
   test strips leading whitespace and an optional `<tool_use_error>` open tag first,
   so every real shape still matches: the plain reason, the colon-less fallback, a
   wrapped reason, and a denial that is one of several content blocks joined with
   `\n` by `resultToText`. What it no longer matches is a MID-line mention — and the
   over-match this PRD dismissed as "negligible" turned out to be reachable from
   this very change: a failing `cd agent && npm test` prints `node --test` titles
   like ``ok 14 - the ".git access" deny reason starts with "denied by guardrail"``,
   so a red gate run would have rendered calm and collapsed. That is the "Under-
   alarming a real problem" risk in this document, arriving for real. Pinned by two
   must-stay-red tests (the TAP log, and a failing grep of `guardrails.ts`).
2. **Decision 2's "read ONLY by `RunEvent.tsx:557` and nothing else" is false, and the
   conclusion survives for a different reason.** `api/internal/workersvc/judge.go`'s
   `toolResultOutcome` parses a `tool_result` block's `is_error`, feeding
   `observedGreenTools` — the PRD #121 missing-tool prescan. (The `isErrorResult`
   half of the survey holds.) Nothing here changes, because the frame is untouched —
   but the guarantee comes from the frame being UNCHANGED, not from nobody reading
   it. The distinction is load-bearing: flipping a denial to non-error at the source,
   which that sentence would license, would make a denied command count as green
   evidence that it ran.
3. **The contract test lives at `agent/test/guardrails.test.ts`** (this PRD says
   `agent/src/`), and the 15 `REASON_*` constants are module-PRIVATE, so "assert all
   15 constants start with the phrase" is not directly expressible. It is pinned two
   ways instead, with `agent/src/guardrails.ts` left untouched: behaviourally (all 15
   deny paths driven through the public API, asserting 15 *distinct* reasons) and
   structurally (a source scan asserting every `REASON_*` DECLARATION is one the scan
   can read and carries the phrase — so a future 16th reason fails even if written in
   a form the literal regex cannot parse, which a mutation test confirmed was
   otherwise a silent hole).
4. **`web/src/components/ChatMessages.test.tsx` did not exist** — it was created
   rather than extended. (The mock line references in M2 are also transposed:
   `mockDoneMessages` is the fixture to extend, not the `fm()` helper — that one
   belongs to `mockFailedMessages`.)
5. **The `⊘` glyph is not `font-bold`** (Decision 4 pins the tone, not the weight). A
   browser pass measured that at 11px with weight 700 the glyph's counter closes and
   it rasterises as an amber blob — confusable with the feed's existing status dots.
   It renders non-bold at 13px; `✓`/`✗` keep their weight.

**Deliberately not done**, and worth a follow-up rather than a silent omission: the
chip label is the terse `"blocked"` this PRD pins, but a browser pass argued for
`"blocked by guardrail"` — collapsed is the state it ships in, and "blocked" alone
does not say who blocked it (a first read of "did someone cancel this?" is plausible).
Declined here because Decision 4 pins the treatment and every other chip label in this
row is terse ("ok", "error", "3 lines"); it is a copy call for a human, not a defect.

## Risks & mitigations

- **SDK-bump wrapper fragility (the real risk).** The reason text arrives in the
  `tool_result` content *verbatim* today, but that is a property of the pinned
  claude-agent-sdk: the same binary already wraps *adjacent* deny paths
  (no-such-tool, MCP/web-isolation, thrown tool errors) in
  `<tool_use_error>…</tool_use_error>`. An SDK bump that moved the PreToolUse-deny
  path onto that wrapper would silently revert every blocked chip to red with no
  test failing. Mitigations, both cheap: (a) match with `.includes("denied by
  guardrail")` (chosen — survives a leading/trailing wrapper); (b) the
  `guardrails.test.ts` phrase-contract test (M2) guards the agent side. If it ever
  does regress, the structured-marker alternative (Decision 1) is the durable fix.
  **(a) is superseded — see "As built" #1: the match anchors the phrase to the start
  of a line, because the over-match dismissed below turned out to be reachable. And
  the wrapper is not the nearest SDK risk: the same binary builds a
  `<hook> hook error: <reason>` form of the denial and yields it just before the raw
  one, so that preamble is stripped too.)**
- **Phrase coupling.** If a reason constant ever dropped the phrase, that denial
  reverts to "error". Held by the non-optional 15-constant `guardrails.test.ts`
  assertion in M2; the phrase is itself a stable user-facing contract.
- **Over-broad match.** Only `is_error` results are considered, and the phrase is
  specific, so a normal successful output that happens to mention it is not
  reclassified (it is not an error). Low risk.
- **Under-alarming a real problem.** If a guardrail denial ever indicates a genuine
  misconfiguration (e.g. docker-no-daemon on a run that needs docker), "blocked"
  might under-signal it. Accepted: the agent's own follow-up text and the run
  outcome carry that signal; the feed chip is not the place to escalate config
  errors.

## Success criteria

- A `tool_result` with text starting `"denied by guardrail: "` renders a
  non-danger "blocked" chip, collapsed by default.
- A genuine `is_error` result is byte-for-byte unchanged (red "✗ error",
  auto-expanded).
- No change to `isErrorResult` or any executor/health/judge path.
- Web tests + typecheck green; the blocked state is visible in mock mode;
  `specs/ai.md` records the decision.
