# PRD #4040: Overview preview, show the pending question inline in Needs your decision

**Status**: Ready for dispatch (refreshed 2026-11-07 against main `d3b6baf8`). Follow-up of #3878, child of epic #3873. #3881 landed in PR #4049 and #4041 in PR #4067; this is next in #3873's serial order (every child edits the Overview preview page; run them one at a time).

**Design brief**: `prds/3873-overview-redesign.md`. **Design mock**: `prds/mockups/4041-needs-decision-after.html` (the target card after #4041 and this PRD; open it in a browser), with `prds/mockups/3873-overview-redesign-mock.html` as the original, the question row in "Needs your decision" (`#409 … "Should the cap count exports that are still queued?" Asked 25 min ago`). The brief's mock contract applies: implement the mock for this row, small consistency tweaks are fine, larger deviations go under `Mock deviations` in the PR body with a reason, and the PR carries side-by-side screenshots (app left, mock right; desktop and 400 px; Ember and Dawn). This PRD's text wins where it is more specific.

## Problem

In the Overview preview's **Needs your decision** card (`web/src/components/NeedsDecisionCard.tsx`), a run waiting on the viewer's answer shows only its title, an age and an **Answer** button. The viewer must open the run to learn what it asks, even when the question is one line they could answer at a glance. #3878 recorded this as a mock deviation: the runs summary the page loads (`GET /api/runs?view=summary`, `apitypes.RunSummaryOf`, `api/internal/apitypes/run.go`) carries no question text.

## Outcome

For each of the viewer's own runs in `awaiting_input`, the card shows the first pending question as one quoted, plain-text line, truncated with an ellipsis. When the payload holds several questions, the line ends with "(+N more)". Since #4041 the card renders a lone question as a single row and two or more questions and follow-ups as one expandable "N runs need your input" group (three items previewed while collapsed). The quoted line appears (maintainer decision 2026-11-07):
- on a single question row, under its title, as in the mock;
- on each `awaiting_input` item of a group **only once the group is expanded**; the collapsed preview keeps today's issue ref and title.
`awaiting_followup` items never carry an excerpt. The age label stays "waiting <age>" as #4041 ships it for every kind (maintainer decision 2026-11-07). Everything else in the card is unchanged.

Acceptance examples:

- A run parked on a one-question clarification "Should the cap count exports that are still queued?": its row reads that sentence in quotes under the title; **Answer** still opens the run.
- A question of 2,000 characters: the row shows a single line ending in "…", and the summary response carries at most the excerpt bound (below), never the full text.
- A question whose payload is malformed or missing: the row renders exactly as today (title, age, Answer), with no error.
- Another user's awaiting-input run: never appears in the card, and the admin run list (`GET /api/admin/runs`, full and summary views) never carries any `pending_question`, so an admin never receives another user's question text.

## Out of scope

- `awaiting_followup` rows (follow-up prompts) and any other decision kind: unchanged.
- Answering from the Overview: **Answer** still navigates to the run.
- The admin run list (`AdminListRuns`, both views, which the admin TUI board also reads): never decorated.
- The TUI does not render the field.
- Plan detail lines, PR grouping and kind icons in the card: #4041.

## Modules and seams

- **Owner-scoped decoration in `ListRuns` only** (`api/internal/handler/runs.go`). `AdminListRuns` shares `overlayListDecorations` and `writeRunList`, so the read must not live there: it runs inside `ListRuns`, whose rows are already owner-scoped (`ListRunsForUser`, `WHERE r.user_id = @user_id`, `api/internal/store/queries/runtime.sql`). For the page's rows with `status = 'awaiting_input'` it sets `pending_question`; when the page has none, the query is skipped (the skip pattern of `overlayCodexAccountActions`). One batched read: a new sqlc query that selects the newest question row per run **first** and only then bounds it, returning `CASE WHEN octet_length(payload::text) <= 65536 THEN payload ELSE NULL END` (e.g. `SELECT run_id, CASE WHEN octet_length(payload::text) <= 65536 THEN payload ELSE NULL END AS payload FROM (SELECT DISTINCT ON (run_id) run_id, payload FROM run_messages WHERE run_id = ANY(@ids) AND kind = 'question' ORDER BY run_id, seq DESC) newest`: the inner query picks the newest row per run, the outer one bounds only those rows), so an oversized newest row yields no excerpt and never falls back to an older question, served by the existing partial index `idx_run_messages_question_seq` (migration 00400). Best-effort like the other decorations: a read failure omits the field and never fails the list.
- **Wire contract: the field lives on `RunListItemDTO`**, set like `CurrentActivity`, and `apitypes.RunSummaryOf` copies it. That keeps the pinned invariant that summary keys equal list-item keys minus the omitted set (`TestRunSummaryItemDTOKeys`, `api/internal/apitypes/run_summary_test.go`; web `RunSummaryItem = Omit<RunListItem, …>`, `web/src/lib/apiTypes.ts`). `pending_question?: string`, omitted when absent. Update the api-contract fixtures `run_list_item.full.json` and `run_list_item.zero.json`; `check:api-v1-compat` is unaffected (internal API).
- **Excerpt function in a neutral package** (for example `api/internal/runquestion`), used by the handler and by `slacksvc` (move `parseQuestionPayload` there; the handler must not import `slacksvc`). Rules, in order:
  1. bound the raw input first: select the newest question row per run regardless of size, and return its payload only when `octet_length(payload::text) <= 65536` (`payload` is `jsonb`); an oversized newest row yields no excerpt, never an older question and never a parse of the whole blob. Then parse defensively as today (malformed JSON, missing `question_id`, or no question with text gives no excerpt);
  2. take the first question's `question` text;
  3. collapse every whitespace run (including `\n` and `\t`) to one space, before stripping, so words are not glued together;
  4. remove control and Unicode format characters (bidi overrides, zero-width) using the existing `termsafe.Unsafe` predicate;
  5. trim;
  6. cap at 280 runes, the "…" included in the 280;
  7. when the payload holds more than one question with text, append " (+N more)" after the cap (outside the 280).
  Unit-test it in that package.
  - **Untrusted input, resource bounds**: model-authored text; at most one excerpt per awaiting-input row, at most 200 rows per page (`LIMIT 200`), so at most 200 × ~300 runes per response, and no query at all on pages without awaiting-input rows. The summary view is polled by the favicon (`web/src/lib/useFavicon.ts`), Board, IssueView, RunsList and the Overview, which is why the skip matters.
- **CLI**: `uzi run list` reads the full owner view (`api/cmd/uzi/run_get.go` via `uzicli/client_runs.go`), so `--json` carries the field once it is on the list item; no new human-readable column (the CLI already shows questions in `uzi run logs` / `uzi run get`).
- **Web**: `overviewDecisions` (`web/src/lib/overview.ts`) carries the excerpt on `awaiting_input` question items only; `NeedsDecisionCard` renders it as a quoted, single-line, CSS-truncated text node (React text, never HTML), with the excerpt in a `title` tooltip, in the single question row and in each expanded group item, never in the collapsed group preview. The `title` is a render sink: tests assert its value, not only `textContent`.
- **Mock mode**: seed a sample question only on the viewer's own awaiting-input run fixture. The mock's `toSummary` (`web/src/mocks/mockApi/runs.ts`) copies fields and `mockAdminListRuns` lists other owners' runs, so add a mock test that admin rows never carry the field. If the seed changes the documented mock scenarios (`docs/dev-conventions.md`), run `task docs:sync` and `task check-docs:web`.

## Testing decisions

- Go unit: the excerpt function (whitespace before stripping, format characters removed, 280-rune cap with "…", "(+N more)", malformed payloads); a LiveDB case with an older valid question followed by an oversized (over 64 KiB) newest one, which yields no excerpt rather than the older question.
- Go handler: `ListRuns` decorates only awaiting-input rows and skips the query when there are none (a counting fake asserts one query per page); `AdminListRuns` rows never carry the field in either view.
- Go LiveDB: the batched query returns the newest question per run; follow `runs_summary_view_livedb_test.go`.
- `TestRunSummaryItemDTOKeys` and the web api-contract test pass with the field on the list item.
- Web: `overview.test.ts` for the item excerpt (awaiting_input only, never awaiting_followup); `NeedsDecisionCard` tests for the single-row quoted line, its absence in the collapsed group preview and its presence on each expanded `awaiting_input` item, a mixed question/follow-up group (the follow-up never shows an excerpt), truncation, the `title` tooltip value, and the no-excerpt fallback; the mock admin-list test; one browser case at 400 px with an expanded group (no horizontal overflow).
- Red/green: the handler decoration test and the admin-exclusion test are watched red before the change.

## Run boundary

- In-run: `task docs:sync` and `task check-docs:web` if any doc changes, `task check:api` plus `go test -race -count=1` on the touched packages (LiveDB included), `sqlc generate` with no drift, `task check:web` with the touched Vitest files and the 400 px browser case, `cd web && npm run build`, `task check:changelog-entry`, and `task scan:secrets` plus `task sast:semgrep` (model-authored text reaches the browser).
- Landing obligations (exact-head PR CI): full `test:api`, `test:web`, `test:web-browser` and `gate:repo`.
- No migration, and no `.github/workflows/**` writes in implementation or validation.

## Milestones

### M1: the pending question in the decision row

**Blocked by**: none inside this PRD.

The batched decoration, DTO field, web type, card rendering, mock sample, tests and docs (`docs/` only if a user doc describes the card).

Acceptance: the four acceptance examples above pass as tests; mock parity for the question row, with side-by-side screenshots. Expected `Mock deviations` for the PR body: the row keeps "waiting <age>" where the mock says "Asked"; expanded group items carry the excerpt, which the #4041 target mock does not depict (both maintainer decisions, 2026-11-07).

## Decision Log

- **Maintainer decisions after #4041 (2026-11-07):** the quoted line shows on a single question row and on expanded group items only, not in the collapsed preview; the age label stays "waiting <age>" for consistency with #4041. Rejected: an excerpt on every collapsed preview item (a busy group grows tall), and single-row-only (grouped questions would lose the feature).
- **Server-side excerpt, not a client fetch per run** (2026-11-07): one batched read per summary page keeps the Overview at its existing request count and bounds the payload. Rejected: fetching each awaiting-input run's logs from the browser (N extra requests on a 10 s poll).
- **280-rune excerpt, stripped of control and format characters** (2026-11-07): one line is the row's budget; the full question stays on the run page. The text is model-authored, so invisible and direction-changing characters are removed before it reaches any client.
- **On the owner list item, never the admin list** (2026-11-07, architecture review): the field sits on `RunListItemDTO` so the pinned summary-equals-list-minus-omitted invariant holds, and it is set only in owner-scoped `ListRuns`. Rejected: a summary-only field (breaks that invariant) and decorating in the shared `writeRunList` path (would send other users' questions to admins).
- **No new CLI column** (2026-11-07): the CLI already prints the question where a CLI user answers it; `uzi run list --json` carries the field because it reads the full owner list.
- **Shared excerpt function, whitespace collapsed before stripping** (2026-11-07): one parser for Slack and the web avoids two copies drifting; collapsing first keeps words separated because the control-character check also matches newlines and tabs.
