# PRD #191: Slack as a conversational surface — chat, run control, and status from a DM

**GitLab Issue**: [vtmocanu/uzi#191](https://github.com/vtmocanu/uzi/-/issues/191)
**Status**: Complete (2026-08-09) — all milestones M1–M7 implemented and reviewed; retired to `prds/done/`. History below is preserved as the drafting/review record.
**Status (drafting)**: Draft — reviewed 2026-07-29 by two agents, all findings applied.
Architect: 2 BLOCKER + 6 MAJOR + 6 MINOR (marked ↳review) — changed the milestone
graph, not the concept: the inbound half survived intact, the outbound and lifecycle
half was rebuilt around a mechanism the first draft did not know about (B2 below).
Fact-check: every load-bearing claim CONFIRMED, 1 wrong line range + 10
true-but-imprecise claims (marked ↳fact-check), of which 4 targeted text the architect
pass had already deleted. Two findings outlived their own review: the "outbound-only"
heading must NOT be rewritten, and a pre-existing chat-injection hole over HTTP is
recorded in Decision 5 as **out of scope** rather than absorbed.
**Refreshed 2026-08-09** (tree at `32e91f99`, v0.22.0). Every file:line citation below
was **re-derived to `32e91f99`** in this pass; the drafting-era `f25eff39` offsets
survive only in the historical block. Every load-bearing premise was re-verified and
all still hold — the mechanisms survived nine releases, only the line numbers drifted.
Material shifts folded in: #192 (the Decision 5 chat-injection hole) landed and is now
CLOSED; a sixth run kind `prompt` (PRD #241) corrected the kind enumerations; PRD #122
(v0.21.0) added a THIRD read-on-DB content path to Slack — milestone titles — which
bears on Problem, Decision 4, M2b, M3 and M7 (noted inline). Re-derive before acting all
the same: a number without a SHA is not a citation.
**Priority**: Medium
**Created**: 2026-07-29
**Depends on**: PRD #25 (Slack integration, done), PRD #39 (chat agent, done), PRD #41 (plan revision gate, done — the plan-in-thread precedent), PRD #88 (clarification questions, done — the threaded-answer precedent)
**Related**: `docs/slack.md`, `docs/chat.md`, `ARCHITECTURE.md` §"Chat with uzi (the fifth surface)" and §"Slack integration (outbound-only)"

> Every file:line citation was derived at `01236897` (2026-07-29), re-derived once by
> review, and re-derived again against **`f25eff39`** (`chore(release): 0.13.0`), which
> landed on `main` while this was being written. That last pass was not ceremonial:
> the release inserted 5 lines at `workersvc/service.go:269`, shifting **six** of this
> document's citations into that file — `SubmitInput` 3111→3116, `GetRun` 2975→2980,
> `PublishMessage` 478→483, `SettingsReader` 543→548, and two more. Those offsets were
> at `f25eff39`; the body below was **re-derived to `32e91f99`** on 2026-08-09, so its
> numbers are at that SHA, not this one. Re-derive before acting on one: a line number
> without a SHA is not a citation, and this document proved it on itself inside one hour
> — and again across the nine releases to `32e91f99`.

## Problem

Slack today is a **notification bridge with in-thread verbs on an existing run**. It
cannot hold a conversation, and the gap is structural rather than a missing flag:

- **A top-level DM to the bot is silently dropped.** `routeMessage`
  (`api/internal/slacksvc/socket.go:230-232`) returns early when `thread_ts` is
  empty, so a user who types "what's running?" into the uzi DM gets **no reply at
  all** — not an error, not a hint. The manifest already subscribes `message.im`
  (`docs/slack.md:36-49`), so the event arrives and is discarded.
- **A thread reply is routed by the anchored run's status** (`replier.go:162-245`, in
  `HandleMessage` `:111`), and by the anchor's gate state where the two
  `awaiting_approval` arms are compound (`:162`, `:200`). Six outcomes: revise feedback,
  reject reason, question answer, `follow_up` steer, an open-gate nudge, or "that run
  has already finished" (`:222-223`). There is no free-text path.
- **Message content structurally cannot reach Slack.** `Notifier.PublishMessage` is
  a deliberate no-op — *"run message CONTENT never goes to Slack (content
  minimization — only status/title/links do)"* (`notifier.go:177-180`). The
  exceptions are read from the database on a state transition, not streamed: plan
  bodies (`notifier.go:478` reads `rc.PlanMd`) and question text (`notifier.go:377`
  reads `GetLatestRunQuestion`; `gate.go:266` and `question.go` only render what those
  reads returned) — and, ↳update 2026-08-09 via PRD #122 (v0.21.0), **milestone
  titles** (`handleMilestone`, `notifier.go:705`, posts `✓ N/M · working <title>`). So
  it is **three** read-on-DB exceptions at HEAD, not the two the first draft saw; M7
  and Decision 4 inherit this.

Meanwhile the Chat page (PRD #39) does exactly what users want from Slack, and is
**web-only**. The CLI hit the same wall and chose to refuse rather than half-build
it: `api/cmd/uzi/tui_steer.go:85-90` marks chat runs watch-only. (↳update 2026-08-09:
the CLI is still watch-only at `:88`, but that file's comment claiming a raw follow-up
"would inject" is stale post-#192 — `SubmitInput` now *rejects* it. The point holds;
the comment is a separate code fix, not this PRD's.)

The practical failure is not "a feature is missing" but **"uzi ignored me"**.

### ↳review — and Slack cannot say anything about a chat even if asked to

The first draft missed this and built a milestone on top of the gap. **No chat state
event can reach Slack today**, by three independent constructions:

1. `GetSlackRunContext` — the notifier's one context query — **INNER JOINs `repos`**
   (`api/internal/store/queries/slack.sql:145`, `JOIN repos rp ON rp.id = r.repo_id`;
   PRD #65 D2 added a `JOIN forge_connections` on top and PRD #122 added three
   `milestones_*` selects — neither reachable by a repo-less chat run). A chat run has
   `repo_id NULL` (`00053_chat_runs.sql:32`, `runs_kind_shape`), so the query returns no
   rows and `Notifier.handle` (`notifier.go:279`) returns silently.
2. `CreateChatRun` (`workersvc/chat.go:244`) publishes nothing; run creation never
   calls `PublishState` anywhere.
3. Chat is excluded from the health lane by design — `ListActiveRunsForHealth` ends
   `AND kind <> 'chat'` (`queries/runtime.sql:1841`, guard @`:1860`), one of **eight**
   `kind <> 'chat'` guards across `runtime.sql`.

(1) is the load-bearing one: terminal transitions — idle-complete, turn-cap complete,
failed — *do* publish state, and every one is dropped there. So "this conversation
ended", "you hit the turn cap" and "your chat failed" are all invisible in Slack. Any
milestone that promises a chat status message needs this fixed **first**, which is
why M2b exists.

**↳fact-check — this is a DECISION in the code, not an oversight, and the PRD should
inherit its reasoning rather than overturn it blindly.** `notifier.go:282-289` names
the exact mechanism and rules on it: *"No row for a chat run (PRD #39):
GetSlackRunContext INNER-JOINs repos … Chat transitions have no repo-scoped DM to send
— skip silently."* That was correct while chat had no Slack presence at all. M2b's
claim is narrower than "fix a bug": a Slack-anchored chat **does** have a DM to send
to, so the premise expires for chat runs that have an anchor row — and for those only.
A chat created from the web, with no anchor, must keep skipping exactly as today.

### What already exists, and why this is not a rewrite

1. **The identity join is done and already trusted for writes.**
   `GetConfirmedUserBySlackID` filters `slack_link_confirmed_at IS NOT NULL AND
   is_active = true` and is already the authz basis at `replier.go:124` and
   `gatekeeper.go:109`. Deactivation falls out for free.
2. **Chat verbs are service-level.** `CreateChatRun`, `ListChatRuns`,
   `SubmitChatMessage`, `EndChat`, `ContinueChat` are at
   `workersvc/chat.go:244,261,288,319,332`; `handler/chat.go` is a thin HTTP shell.
3. **The untrusted-text rendering pipeline is built and documented.**
   `planThreadBlocks` (`gate.go:266-277`) is ScrubSecrets → whole-blob `EscapeMrkdwn`
   → `truncateForSlackSection` (2900 runes) → deep link in a **separate** block, with
   a doc comment arguing why whole-blob escaping is correct for model-authored text.
4. **Riding `kind='chat'` genuinely works** (↳review, verified): `ClaimChat` /
   `assembleChatClaim` (`workersvc/chat.go:147`/`:173`) join no repo and no forge
   connection, `ChatClaimPayload` has no PAT field by construction, and
   `resume_of_run_id`, the turn cap, the idle backstop and `WORKER_CHAT_SESSIONS` are
   all origin-agnostic. Nothing kind-scoped breaks on a Slack-originated conversation.

### What is genuinely missing

1. An **outbound message path** for chat runs (the no-op above).
2. An **outbound state path** for chat runs (the repo JOIN above).
3. An **inbound entry point** that is not thread-anchored.
4. **Three composite operations that live in HANDLERS, not services**, and therefore
   cannot be called from Slack: proposal confirm (`handler/chat.go:194-255`), run
   creation (`handler/workers.go:725`), and the per-user chat **spend limiter**, which
   is route-mounted (`handler.go:1127,1129,1134`, the three
   `chatLimiter.PerUserMiddleware` mounts) so any non-HTTP caller silently bypasses it.

That fourth group is the real work. uzi's service layer is clean everywhere the web
was the only consumer of a *simple* verb, and leaks into handlers exactly where the
verb is *composite*. Slack is the second consumer that forces the issue; the CLI is
the next one.

## Solution Overview

**A top-level DM to the uzi bot opens a conversation**, backed by the existing
`runs.kind='chat'` machinery, streamed back into that DM's thread. The agent gains a
`start_run` tool (human-confirmed), and issue proposals render as Block Kit cards
with real Create / Dismiss buttons.

- **No new run kind, no new service, no new claim lane.**
- **No manifest change and no workspace re-install** — `message.im` is subscribed and
  `chat:write` / `im:write` / `reactions:write` are granted (`docs/slack.md:36-49`).
  Every workspace that has uzi's Slack app gets this on upgrade with no admin action.
- **Content minimization is relaxed for `kind='chat'` ONLY.** ↳fact-check: there are
  **six** run kinds, not two — `issue`, `ci_fix`, `chat`, `judge`, `self_improve` and
  `prompt` (`workersvc/ci_fix.go:18-19`, `chat.go:21`, `judge.go:23,27,31`). The other
  five keep today's posture byte-for-byte; `judge` and `self_improve` are additionally
  already suppressed from the run-state DM path (`notifier.go:296`), while `prompt` is
  repo-ful and NOT suppressed, so it rides the same run-state DM lane as
  `issue`/`ci_fix`. (↳update 2026-08-09: was "five … the other four" before `prompt`,
  PRD #241, landed as a run kind.)
- **The chat agent still holds no credential.** `start_run` and `propose_issue` are
  *requests to the api*, which performs the forge call with the user's own connection.

## Design Decisions

1. **A top-level DM opens a chat run; the bot replies in a thread on that message.**
   Slack has no notion of a conversation beyond a thread, and uzi's inbound model is
   thread-anchored, so a Slack chat becomes structurally identical to a Slack run DM.

2. **↳review — the anchor stores TWO timestamps, because `root_ts` cannot serve both
   roles.** An inbound reply's `thread_ts` is the **user's** top-level message, so
   `GetSlackRunMessageByRoot(channel_id, root_ts)` resolves only if `root_ts` is that
   user message. But every existing consumer treats `root_ts` as the **bot's
   editable** root: `00044_slack.sql:26-28` (*"status edits target it"*), and both
   `notifier.go:344` and `notifier.go:594` call
   `poster.Update(ctx, existing.ChannelID, existing.RootTs, root)`. A bot cannot edit
   another user's message, and every failure on that path is best-effort-logged, so
   this would have been silent.
   Resolution: `root_ts` keeps its inbound meaning (the user's message, which is what
   Slack hands us), and the migration adds **`status_ts`** for the bot's own editable
   status message — exactly the shape `gate_ts` and `question_ts` already have. The
   chat state path edits `status_ts`; nothing edits a chat row's `root_ts`. Recorded
   here rather than left to the implementer because the first draft's Decision 3
   ("the gate/question columns simply stay NULL") did not cover it, and the failure
   mode is invisible.

3. **New vs. continue is decided by the anchor, not by parsing intent.** A top-level
   DM starts a new chat; continuing happens by replying **in the thread**, the
   affordance Slack already teaches. Rejected: "continue the most recent live chat if
   within the idle window" — it makes a message's meaning depend on a clock the user
   cannot see, the same arrival-time-as-identity mistake
   `00093_slack_question_anchor.sql:29-42` documents at length.
   **↳review addendum**: with `WORKER_CHAT_SESSIONS`=1 and `CHAT_IDLE_TIMEOUT`=70m,
   three top-level DMs mint three runs and two queue behind a conversation that will
   not idle out for over an hour. So a second top-level DM while the user already has
   a live chat is **refused**, with a threaded pointer to the live one. A refusal is
   not a clock-based continue, and needs no clock.

4. **Extend `slack_run_messages`; do not add a table.** It is already keyed `run_id
   uuid PRIMARY KEY` with `(channel_id, root_ts)` (`00044_slack.sql:29-36`), which is
   the shape a chat needs. ↳review verified all seven queries touching that table key
   on `run_id` or `(channel_id, root_ts)` and none joins `runs`, so a chat row with
   NULL gate/question columns breaks none of them; and a forged chat run id on a gate
   button hits `!anchor.GateTs.Valid` at `gatekeeper.go:147-151` and gets the
   "superseded" ephemeral. Inert. Every column added since `00044` is nullable
   (`00074:12`, `00093:24,43`, and — ↳update 2026-08-09, PRD #122 — `00101:15`
   `milestones_notified_completed int`), so the NULL-column claim holds against the
   current shape and not merely the original one.
   **↳fact-check — one stated premise expires, and M2 must re-establish it.** There is
   **no UNIQUE index** on `(channel_id, root_ts)`; `replier.go:136-138` argues the
   `:one` lookup is safe because the pair is *effectively* unique — *"each run posts
   its OWN root message, so its ts is distinct within the DM channel."* A Slack chat
   anchors on the **user's** message, so that sentence no longer describes every row.
   The conclusion still holds (a given user message ts is unique in the channel and is
   claimed by exactly one chat), but it now holds for a **different reason**, and an
   argument that survives by accident is one nobody can maintain. M2 either adds the
   UNIQUE index or updates that comment to state both reasons.

5. **The replier branches on `run.Kind == "chat"` BEFORE its status switch.** A chat
   run's status is `queued`/`claimed`/`running`, which lands in the `default:` arm and
   would submit a raw `follow_up` — the injection the CLI refuses at
   `tui_steer.go:85-90`. ↳review verified the premise: `workersvc.GetRunByIDForUser`
   (`service.go:3399`; was `GetRun` `:2980` at `f25eff39`) is kind-agnostic and returns
   `store.Run` with `.Kind`. Turns route through `SubmitChatMessage` instead, which
   enforces the turn cap and the terminal 409. This requires extending `PlanGateSubmitter`
   (`gatekeeper.go:48-69`) and its `gateSubmitter` impl in `api/cmd/server/main.go` —
   both now in Touchpoints.
   **↳update 2026-08-09 — the SAFETY half of this premise expired: `SubmitInput` now
   DOES gate chat.** PRD #258/#192 landed `if run.Kind == RunKindChat && kind ==
   "follow_up"` at the service boundary (`service.go:3549`), returning
   `ErrChatInputNotAllowed` before any write. So the replier branch no longer prevents
   an *injection* — a raw `follow_up` is refused by the service, not accepted. It is
   still required, but for CORRECTNESS not safety: without it a Slack thread reply lands
   in the `default:` arm, submits `follow_up`, and now *errors* instead of continuing
   the chat. Route through `SubmitChatMessage` so the reply becomes a turn, not a 409.
   **↳fact-check (2026-07-29) — the same hole was OPEN over HTTP, and it was not this
   PRD's to fix.** As verified at `f25eff39`: `SubmitInput` checked ownership and
   terminal status and **never** `run.Kind`, and `CreateRunInput` (`handler/workers.go`)
   validated only the *input* kind against `runInputKinds`. So `POST
   /api/runs/{id}/inputs {"kind":"follow_up"}` on a chat run the caller owned was
   **accepted**, bypassing `SubmitChatMessage`'s turn cap — the CLI refused to *offer*
   the affordance (`tui_steer.go:85-90`) but the API accepted the call. A pre-existing
   spend-cap-bypass defect, filed separately rather than smuggled into a Slack
   milestone: **[vtmocanu/uzi#192](https://github.com/vtmocanu/uzi/-/issues/192)**.
   **↳update 2026-08-09 — #192 is now CLOSED** (`fix(258): reject follow_up on chat
   runs at the SubmitInput boundary`, `32e91f99`): the guard sits at the service
   boundary and covers HTTP, CLI and future Slack. Decision 5 no longer *keeps Slack out
   of* an open hole — the hole is shut for every caller; the replier branch is now the
   correctness half described in the update above.

6. **↳review — post one Slack message per assistant TURN, and enumerate every way a
   turn can end.** Turn boundaries *are* observable at the `PublishMessage` seam:
   turn start is `kind:"user_message"` (`agent/src/chat-runner.ts:260`), happy-path
   end is the SDK result frame arriving as `kind:"status"` with
   `payload.event=="result"` (`chat-executor.ts:462-471`), and re-deliveries are
   excluded because only `inserted`/new messages are broadcast (`service.go:2100,2161`),
   so no double-post. But **three turn-end paths emit no result frame**, all in
   `chat-executor.ts`: a cancelled turn (`:476`, dup `:485`, emits nothing), a wall-clock
   timeout (`:477-479`, dup `:486-488`, emits a prose `status`), and the catch-all error
   (`:481`, emits `kind:"error"`). A consumer waiting only on `event=="result"` strands
   the "uzi is thinking…" placeholder forever on exactly the turns a user most needs
   explained. The consumer therefore treats **all four** as turn-end.
   Two further rulings: **`thinking` frames are never posted** (`sdk-messages.ts:120-122`
   emits them; streaming model reasoning to Slack is a wider widening than Decision 10
   describes and nobody asked for it) — only `text` frames compose the turn body. And
   the per-turn buffer is in-memory, so an api restart mid-turn orphans a placeholder;
   the placeholder carries the deep link so an orphan is still useful, and M2b's status
   line corrects it on the next transition.
   Rejected: per-frame posting — `chat.postMessage` is **special-tier**, roughly one
   message per second per channel (verified against docs.slack.dev), which rate-limits
   it into uselessness. And edit-per-frame streaming, **on UX grounds alone**: a
   visibly rewriting message reads worse than a short wait. ↳fact-check: the first
   draft also claimed `chat.update` was rate-limited "too", implying parity. It is
   **Tier 3, 50+ per minute** — two orders of magnitude looser, and it does not carry
   the rejection. One good reason beats one good reason plus a wrong one.

7. **Rendering reuses the plan-blob pipeline verbatim** (`gate.go:254-277`). Chat
   answers are model-authored text quoting uzi source and forge content, precisely the
   threat that pipeline was written for. A truncated answer keeps its "Open in uzi"
   context block, which lives outside the truncated region by construction.

8. **↳review — lift the two composite operations into `workersvc`, and note the two
   dependencies it does not yet have.** `ConfirmProposalForUser` and
   `StartRunForUser` move the claim/forge/settle and the `GetIssue`/PRDLESS logic out
   of the handlers. Two corrections to the first draft's "this is a move, not a
   rewrite":
   - `workersvc` has **no forge dependency at all** today, and it cannot import
     `forgesvc` — `forgesvc` already imports `workersvc`
     (`api/internal/forgesvc/judge_issue_close.go:10`), so that is a cycle. The lift
     needs a narrow builder interface on `workersvc`
     (`ForgeForConnection(string, string, []byte) (forge.Forge, error)`), the shape
     `selfimprove`, `privcheck` and `runlifecycle` already use, plus wiring in
     `main.go`.
   - `StartRunForUser` needs `PrdlessEnabled`/`PrdlessLabel`
     (`handler/workers.go:788-790`); `workersvc.SettingsReader`
     (`service.go:608-621`) carries only `JudgeEnabled`, `JudgeModel`, `PRDLabel`.
   Also: `handler/workers.go:839-877` is the sentinel→HTTP-status switch and **stays
   in the handler**. The lift ends at the service boundary, not at the response.

9. **↳review — do NOT move the spend limiter off the routes; export an `Allow` seam
   instead.** The first draft said move it, which is wrong twice.
   `mw.Limiter.PerUserMiddleware` (`middleware/ratelimit.go:101-120`) keys on
   `RoutePattern() + "|" + userID` and calls the unexported `l.allow`, and
   `workersvc` importing `internal/middleware` is the wrong dependency direction.
   Worse, `handler/route_limiter_mounts_test.go` is an exhaustive 159-row
   route→limiter table (↳update 2026-08-09: was 142) that exists *because* deleting all
   28 `.With(…PerUserMiddleware)` mounts (was 24) once left `go vet` clean and the suite
   at zero failures — moving the chat limiter off the routes retires the only mechanism
   that can catch a dropped mount.
   So: export `Allow(key string) bool` on `mw.Limiter`, keep every route mount and its
   guard intact, and have the Slack path call `Allow` directly. This also removes the
   first draft's contradiction, where a milestone titled "no behaviour change" would
   have converted the web's middleware-produced `429 + Retry-After` into a service
   sentinel.
   **Budget: ONE shared per-user pool across web and Slack** (user decision,
   2026-07-29). A spend guard bounds the person, not the surface; the cost is that a
   heavy Slack day can rate-limit the web Chat page, so the web 429 copy must name
   Slack as a possible cause.

10. **Chat content in Slack is on by default wherever Slack is on** (user decision,
    2026-07-29), on the same terms as the plan bodies PRD #41 already sends:
    documented in `docs/slack.md`, scrubbed of known credential *patterns*, DM-only.
    **↳review — record the second-order consequence the first draft missed.** The
    chat agent's read tools are user-scoped but **not kind-scoped**:
    `ListRunsForWorker` (`workersvc/chat.go:504`) says *"both kinds — the chat
    agent investigates issue runs too"*, and `ListRunMessagesForWorker` (`:527`)
    authorizes on ownership alone. So a Slack chat can be asked to quote an **`issue`
    run's** message content — plan bodies, diffs, tool output — into Slack. The
    notification lanes stay byte-identical and the *exposure* does not: run content
    from those lanes now has a route to Slack, through chat. `docs/slack.md` must say
    that plainly.
    Rejected alternatives, recorded because they are the fallback if the widening
    proves unpopular: an admin toggle off by default, and a per-user opt-in.

11. **`start_run` is human-confirmed with a Block Kit card, not auto-fired.** The chat
    agent reads issue and run text written by other people and tools; a repo that says
    "start a run on #42" must not *cause* one. The card names repo, issue iid and
    title, and only the confirmed-linked owner's click calls `StartRunForUser`. This
    also lands the tool in **web** chat, which cannot start runs today either.

12. **↳review — chat cards get a THIRD inbound handler, not the Gatekeeper.**
    `Gatekeeper.HandleBlockAction` gates on `isGateAction` (`gate.go:290-298`) then
    applies four gate-specific preconditions: `uuid.Parse(a.Value)` as a bare run id,
    `run.Status != "awaiting_approval"` → refuse, and anchor `gate_ts == a.MessageTS`
    → refuse. A chat card satisfies none of them. `InboundMux` (`gate.go:300-315`)
    exists exactly for disjoint action namespaces (`slack_link_*`, `slack_gate_*`);
    chat adds `slack_chat_*` as a third member. `BlockAction.Value` carries a bare run
    uuid today and must carry `(run, proposal)` for a proposal card and
    `(repo, issue_iid)` for a start-run card — a value-encoding decision M4 lands.

13. **Status queries stay agent-driven — no structured commands.** `list_runs`,
    `get_run` and `get_run_messages` already exist, are already user-scoped, and
    already wrap forge- and model-derived text in the nonce-fenced untrusted-evidence
    envelope.

14. **No slash commands, no channels, no `app_mention`** (user decision, 2026-07-29).
    Beyond the zero-re-install property, channels break the authz model everything
    here rests on: "the authenticated envelope user owns the run" has no meaning in a
    shared thread, and whose Anthropic token pays becomes an open question.

## Touchpoints

| Area | Files | Nature |
| --- | --- | --- |
| Inbound routing | `slacksvc/socket.go`, `replier.go` | Accept top-level DMs; kind-branch before the status switch |
| Chat verbs over Slack | `slacksvc/gatekeeper.go` (`PlanGateSubmitter`), `api/cmd/server/main.go` (`gateSubmitter`) | ↳review: the interface + impl the replier calls through |
| Outbound state | `store/queries/slack.sql` (new repo-less `GetSlackChatContext`), `slacksvc/notifier.go` | ↳review B2: chat runs never reach `handle` today |
| Outbound messages | `slacksvc/notifier.go` (+ new `chatpost.go`) | Chat-scoped `PublishMessage` consumer, turn coalescing |
| Anchor | new migration, `store/queries/slack.sql` | ↳review: `status_ts` column (Decision 2) |
| Rendering | `slacksvc/gate.go` helpers, `redact.go` | Reuse `EscapeMrkdwn`/`truncateForSlackSection`; new card blocks |
| Block actions | new `slacksvc/chatactions.go`, `gate.go` (`InboundMux`) | ↳review: `slack_chat_*` namespace, not the Gatekeeper |
| Service lifting | `workersvc/chat.go`, `service.go`, `handler/chat.go`, `handler/workers.go`, `main.go` | + forge-builder interface, + `SettingsReader` prdless |
| Spend guard | `middleware/ratelimit.go` | ↳review: export `Allow(key)`; routes keep their mounts |
| Agent tools | `agent/src/uzi-tools.ts` | `start_run` tool + card emission |
| Docs | `docs/slack.md`, `docs/chat.md`, `ARCHITECTURE.md` | New capability + the widened posture (Decision 10) |

## Milestones

**↳review — Phase 1 is SEQUENTIAL, not parallel.** The first draft claimed M1 ‖ M2 on
disjoint files; both touch `main.go`, and semantically M2 creates chat runs from a
non-HTTP path that is spend-unguarded until M1 lands — precisely the state Decision 9
calls "worse than no guard, because the surface *looks* protected".

**Phase 1 — foundations (sequential).**

- [x] **M1 — Service lifting + the `Allow` seam.** `ConfirmProposalForUser` and
      `StartRunForUser` move into `workersvc` behind a forge-builder interface
      (Decision 8); `SettingsReader` gains the prdless accessors; `mw.Limiter` exports
      `Allow(key)` (Decision 9) with routes untouched. **↳review — verification is
      tests to WRITE, not tests to inherit.** There are no handler tests for either
      operation today. ↳update 2026-08-09: at `f25eff39`,
      `rg -l "ConfirmProposal|CreateRun\("` across `api/internal/handler/*_test.go`
      returned nothing; at `32e91f99` it returns `seeded_plan_livedb_test.go` (a service
      `CreateRun(...)` setup call, not a handler composite test) and `handler/chat_test.go`
      now exists with 7 tests — but still **none** is a ConfirmProposal composition/revert
      test, so the "no net to inherit" spirit survives while the literal output changed.
      `workersvc/chat_test.go:192-237` tests the service primitives the lift does not move.
      Inheriting that net would have been a control that cannot discriminate.
      **↳update 2026-08-09 — the claim-first path the lift wants to mutation-test already
      SHIPS in the handler**: `handler/chat.go` `ConfirmProposal` (`:194-255`) already does
      `ClaimProposalForConfirm` + revert-on-failure at **three** post-claim points
      (`:212,:224,:239`, not the four the draft listed), calling
      `forgesvc.ForgeForConnection` (`forgesvc/service.go:168`). The lift is still unbuilt
      (`ConfirmProposalForUser`/`StartRunForUser` absent from `workersvc`), but M1's tests
      describe behaviour that already exists.
      **Verified**: a composition test per post-claim failure point
      (`handler/chat.go:212,224,239`) asserts the proposal returns to `pending`;
      a concurrent-confirm test creates exactly **one** forge issue, and a mutation
      that removes claim-first ordering reddens it; the route→limiter table
      (`route_limiter_mounts_test.go`) is unchanged, all 159 rows.

- [x] **M2 — Inbound: a top-level DM opens a chat.** `routeMessage` accepts
      `thread_ts == ""` in a DM; the path creates a chat run via `CreateChatRun`
      (through `Allow`), writes the anchor with the user's ts as `root_ts`, posts the
      bot's threaded root and stores its ts as `status_ts` (Decision 2). The replier
      gains the `run.Kind == "chat"` branch ahead of its status switch, routing turns
      through `SubmitChatMessage`. A second top-level DM during a live chat is refused
      with a pointer (Decision 3). **Verified**: a top-level DM from a confirmed user
      creates exactly one `kind='chat'` run owned by that user and exactly one anchor
      row whose `status_ts` is a **bot** message; a thread reply submits a chat turn
      and **not** a raw `follow_up` (assert on the persisted input kind); a DM from an
      unlinked or deactivated Slack user creates nothing; a second top-level DM
      creates no second run and posts the pointer.

**Phase 2 — outbound (sequential; M3 needs M2b's context query and M2's anchor).**

- [x] **M2b — ↳review NEW: chat runs can reach the notifier at all.** A repo-less
      `GetSlackChatContext` (no `repos`/`forge_connections` join) plus a chat branch in
      `Notifier.handle`, so a chat's terminal transitions stop being dropped at
      `queries/slack.sql:145`. Status edits target `status_ts`, never `root_ts`.
      Nothing is added to the health lane — chat's exclusion there
      (`queries/runtime.sql:1841`) is correct and stays. **Verified**: a chat run
      reaching `completed`/`failed` posts exactly one Slack status update against
      `status_ts`; the same event on an `issue` run renders byte-identically to today;
      a chat run's transition performs **zero** `Update` calls against `root_ts`.

- [x] **M3 — Outbound: chat turns stream into the thread.** A chat-scoped
      `PublishMessage` consumer posts one placeholder per turn and edits it with the
      assembled `text`-frame body, treating **all four** turn-end signals as terminal
      (Decision 6), rendered through the scrub/escape/truncate pipeline. Every other
      kind keeps the no-op path. Two ↳fact-check constraints on the implementation:
      **(a) `PublishMessage` carries no run kind** — its `kind` parameter
      (`service.go:543`) is the *message* kind (`text`/`tool_use`/`proposal`), so the
      consumer must resolve `runs.kind` per message via a lookup or a cache, on the hot
      path for every frame; decide which in M3, not at review time. **(b) This is a
      PARALLEL path, not a branch in the existing renderer** — everything downstream of
      `GetSlackRunContext` (deep link, repo path, issue identity) is unavailable to a
      repo-less chat run, which is why `chatpost.go` is a new file rather than an
      `if kind == "chat"` inside `handle`. **Verified**: a turn emitting N text frames
      produces exactly one post and one edit regardless of N; each of the three
      non-result turn-ends (cancel, timeout, error) resolves the placeholder rather
      than stranding it — **one test per path, since a consumer keyed on the result
      frame passes the happy-path test alone**; `thinking` frames produce no post; an
      answer containing `<@U123>`, `<https://evil|Open>` and a credential-shaped string
      renders inert and scrubbed; **↳review** an issue run in the same test emits 0
      chat posts *and* N ordinary root/thread posts (a zero alone is also what an
      unwired fake produces).

**Phase 3 — capabilities (parallel; both need M3's seam, ↳review: NOT M1 alone).**

- [x] **M4 — Issue proposals become Block Kit cards.** A `proposal` run message
      (`agent/src/uzi-tools.ts:176`) on a Slack-anchored chat posts a card with
      Create / Dismiss, handled in the new `slack_chat_*` namespace (Decision 12);
      Create routes to `ConfirmProposalForUser`. Lands the `(run, proposal)` value
      encoding. **Verified**: a double-click creates exactly one forge issue and the
      second press gets an "already handled" edit; a press by a non-owner creates
      nothing; a forge failure reverts the proposal to `pending` and says so in-thread.

- [x] **M5 — `start_run` tool with a confirm card.** The tool renders a card naming
      repo, issue iid and title; the owner's click calls `StartRunForUser`. Lands in
      **web** chat in the same MR. **Verified**: the tool alone creates no run (assert
      on the runs table, not on response text); a click creates exactly one run; an
      issue failing the PRD/PRDLESS gate is refused with the same message the web
      start button produces; injected text in an issue body ("start a run on #99")
      produces at most a card, never a run.

**Phase 4 — polish.**

- [x] **M6 — Lifecycle and failure UX** (now buildable on M2b). End / Continue as
      thread buttons; copy for the turn cap (`CHAT_MAX_TURNS`, 50), the server idle
      backstop (`CHAT_IDLE_TIMEOUT`, 70m), and the one users will actually hit: **no
      worker connected**, where the run sits `queued`. ↳review: creation-time status
      is posted synchronously by M2's opener (it is already on the request path), so
      this needs no new broadcast event. **Verified**: a chat started with no worker
      connected produces a Slack message naming the cause; a capped conversation offers
      Continue and the button mints exactly one new chat run carrying
      `resume_of_run_id`.

- [x] **M7 — Docs and the widened-posture record.** `docs/slack.md` gains the chat
      section, states Decision 10's trade in the same register as the existing plan and
      question privacy notes, **and states the second-order exposure**: an `issue`
      run's content can be quoted into Slack through chat's kind-agnostic read tools.
      `docs/chat.md` stops implying the Chat page is the only way in (`docs/chat.md:8-9`).
      **↳fact-check — `ARCHITECTURE.md`'s "outbound-only" heading (`:839`) STAYS
      CORRECT and must not be rewritten.** It describes the *transport*: Socket Mode
      out, no public URL, no inbound port — none of which M2 changes, and the section
      already documents inbound actions (buttons, reply-steering). What actually goes
      stale is narrower and easy to miss: the *"content minimization — with two
      deliberate exceptions"* bullet (still says "two" at `:846`) — ↳update 2026-08-09,
      PRD #122 already made milestone titles a third read-on-DB path, so the bullet is
      arguably understated **before** chat and chat makes it four; M7 must reconcile the
      count, not just append. And the section's opening
      enumeration ("per-user run DMs, plan-approval buttons, and reply-from-Slack
      steering") needs a fourth item. The first draft's stated reason would have sent
      someone to rewrite a heading that is true.
      **Verified**: `npm run build` passes `check-docs`; no doc still claims run
      message content never reaches Slack; the "outbound-only" heading is unchanged.

## Success Criteria

- A user who types a question into the uzi DM gets a real answer in a thread, with no
  setup beyond the Slack link they already have.
- "What's running?" and "why did #57 fail?" are answerable from Slack, from the user's
  own runs only.
- A run can be started from Slack in two steps (ask, confirm) and never in one.
- An issue can be drafted and filed from Slack, the forge write still gated on a click.
- Existing workspaces need **no** app re-install and **no** admin action.
- The **notification lanes for all five non-chat kinds** (`issue`, `ci_fix`, `judge`,
  `self_improve`, `prompt`) are byte-identical to today. Stated as the lane, not the
  system — Decision 10 records that run content gains a route to Slack through chat's
  read tools. (↳update 2026-08-09: `prompt`, PRD #241, is the fifth; it was not a run
  kind when this was written.)

## Out of Scope (deliberate)

- Slash commands, `app_mention`, channel or group conversations (Decision 14).
- Multi-user or shared conversations.
- Chat runs in the health lane (`queries/runtime.sql:1841` excludes them correctly).
- Voice, file upload, image input.
- Making the CLI a chat consumer — the obvious next beneficiary of M1's seams, and a
  separate PRD.

## Risks

- **Rate limits under a chatty agent.** Mitigated by Decision 6. Residual: a burst of
  very short turns can approach the per-channel limit; degrade by coalescing, never by
  dropping silently.
- **Content widening lands on existing installs at upgrade.** Accepted (Decision 10),
  mitigated by documenting it in `docs/slack.md` in the same MR as M3 rather than
  waiting for M7 — the doc must not lag the behaviour.
- **Prompt injection reaching a run start.** Mitigated by the confirm card and the
  nonce-fenced evidence envelope. Residual and named: a user who reflexively clicks
  Confirm is the last line, as with `propose_issue` today.
- **The service lift touches the forge write path** with no existing regression net
  (↳review B1). Mitigated by M1 landing first, alone, with the composition tests
  written **as part of M1** and claim-first atomicity treated as a mutation-tested
  invariant rather than a reviewed one.
- **Spend.** A Slack chat is as expensive as a web chat and much easier to start by
  accident. Decision 9 is the mitigation; without it the risk is unbounded.
- **Shared budget surprises the web surface** (Decision 9, accepted): a heavy Slack
  day can 429 the Chat page. Mitigated by copy, not by mechanism.

## Dependencies

- A connected Slack workspace with the current manifest (no changes needed).
- The user's own worker running with a default Anthropic token — inherited from
  PRD #39.
- `WORKER_CHAT_SESSIONS` (default 1) bounds concurrent chats per worker: a Slack chat
  and a web chat compete for one slot. Decision 3's refusal and M6's copy are what
  keep that legible instead of looking like a hang.

## Validation

- Go: `cd api && go build ./... && go vet ./... && go test -count=1 ./...` with
  `UZI_TEST_DATABASE_URL` **unset**; the live-DB sweep via `./e2e/run-store-it.sh`
  separately. `-count=1` is mandatory, not stylistic.
- Controller module is untouched.
- Agent: `cd agent && npm run typecheck && npm test`.
- Web (M5 lands a web change): `cd web && npm run typecheck && npm test && npm run build`.
- Slack paths have no CI coverage by construction (no live workspace in CI), so every
  behaviour above must be unit-testable through the existing `Poster` /
  `MessageHandler` seams that `replier_test.go` and `gatekeeper_test.go` already use.
  **A milestone whose verification requires a live workspace is mis-specified.**

## Related Work

- **PRD #25** listed *"Events API / public-URL mode, slash commands, Slack as a full
  chat surface"* as deliberately out of scope. This PRD reverses the
  third item only, on a narrower footprint: DM-only, no slash commands, no channel
  binding.
- **A comparable Slack integration was reviewed** (mrkdwn conversion, a typing
  indicator, history handling, channel binding) — the prior art PRD #25's comparison
  table cites. Per the inspiration-first convention, M3 and M6 should weigh its
  documented trade-offs before implementing.
  **↳fact-check — the first draft misread the "typing indicator" finding.** It is
  not a Slack typing API and involves nothing about Socket Mode: it posts a 👀
  **reaction**, on the reasoning that Slack has no animated "typing" reaction like
  some other chat platforms, so a universal 👀 stands in. uzi already grants
  `reactions:write` (`docs/slack.md:43`) and already calls `AddReaction`
  (`poster.go:117`, `replier.go:398`), so "if Slack supports it here" was a question
  with an answer already in the tree. It is also not a substitute for a placeholder — a
  reaction cannot carry answer text. The real trade M3 should weigh is
  **react-then-post-once** (1 post, 0 edits) against **placeholder-then-edit** (1 post,
  1 edit).
- **PRD #41** established that model-authored text may leave the box for a Slack
  thread, and built the escaping pipeline this reuses.
- **PRD #88** established threaded-free-text-as-input and the
  ordering-not-arrival-time identity rule Decision 3 follows.

## Decision Log

- **2026-07-29 — Scope set by the user**: all four capabilities; DM-only entry;
  content widening accepted as default-on with Slack; **one shared per-user chat
  budget across web and Slack**. Rejected alternatives are kept in Decisions 9, 10 and
  14 rather than discarded, because two of them are the natural fallback if the
  widening proves unpopular.
- **2026-07-29 — No new run kind.** Chat rides `runs.kind='chat'`. A `slack_chat` kind
  would fork the claim lane, the executor and the tool surface for no behavioural gain.
  Review verified nothing kind-scoped breaks.
- **2026-07-29 — The composite-operations-in-handlers finding is the PRD's spine.**
  Found while investigating whether Slack *could* reach chat; it is why M1 is its own
  milestone rather than incidental refactoring inside M4/M5, and it will be needed
  again by the CLI.
- **2026-07-29 (↳review) — the first draft's outbound half was built on an assumption
  nobody had checked**: that a chat run's state events reach the notifier. They do not
  (`queries/slack.sql:145`). M2b exists because of it, M6 was unbuildable without it,
  and the general lesson is the one CLAUDE.md keeps making: the draft cited the
  notifier's *message* no-op as the only outbound gap and never asked what happened to
  *state*. Two adjacent negatives, one checked.
- **2026-07-29 (↳fact-check) — the corrections that matter are not the wrong facts.**
  Every load-bearing claim held; what needed fixing was a class the architect pass
  could not see, because it reads for design and this reads for accuracy. Three
  instances worth keeping as a pattern: a **right action with a wrong reason** (M7 —
  rewrite the content-minimization bullet, do NOT touch the "outbound-only" heading,
  which stays true because it describes the transport); a **true claim carrying a
  false implication** (`chat.update` "is rate-limited too" — Tier 3 at 50+/min against
  postMessage's ~1/sec, so the parity it implies does not exist and the rejection it
  supported never needed it); and a **citation one layer off the fact** (`gate.go:266`
  renders the plan, `notifier.go:478` reads it). None would have failed a build; each
  would have sent an implementer somewhere real and wrong.
- **2026-07-29 — a defect found by review is filed, not absorbed.** The HTTP
  chat-injection hole (Decision 5) was real, verified at `f25eff39`, and predated this
  work. Folding it into a Slack milestone would have hidden a spend-cap bypass inside a
  feature MR and left it unfixed for as long as this PRD takes. Filed as
  [#192](https://github.com/vtmocanu/uzi/-/issues/192). **↳update 2026-08-09 —
  #192 landed independently** (`fix(258)`, `32e91f99`), which is the outcome
  filing-not-absorbing exists to enable: the fix shipped on its own timeline instead of
  waiting on this PRD. Decision 5 updated to match.
- **2026-07-29 — the tree moved under this document while it was being written**, and
  the citation block records it. `chore(release): 0.13.0` shifted six `service.go`
  citations by +5 in the ~90 minutes between drafting and committing. Nothing here was
  wrong when written, which is the point: the SHA is what makes a stale number
  recoverable instead of merely wrong.
