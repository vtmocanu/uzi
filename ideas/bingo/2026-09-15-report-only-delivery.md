# Report delivery — a report-only run's answer reaches the issue that asked for it

Line references are against `ab1d7431`.

## Rationale

uzi has a success terminal that opens no merge request. A report-only issue run —
one whose deliverable is a report, a validation result, or a fix-in-place with
nothing to open an MR for — completes with its findings in `report_md` and never
tells the issue that asked. The findings exist, are already hardened, and stay
locked inside uzi.

**Ingest already exists and is already safe.** The worker's `report_md` is narrowed
at completion by `clampWireReportMd`, whose doc comment states the invariant —
"report_md is the deliverable of a report-only completion, so it is stored ONLY when
`reportOnly` is true" (`api/internal/workersvc/report_only.go:35-36`) — and whose one
transform is `s := scrubThenBound(*p, ReviewSummaryMaxBytes)`
(`api/internal/workersvc/report_only.go:53`). `scrubThenBound` sanitizes, then
`secretscrub.Scrub`es, then bounds (`api/internal/workersvc/proposal_filing.go:96-99`),
and the bound is `ReviewSummaryMaxBytes = 8 * 1024` — 8 KiB
(`api/internal/workersvc/judge_review.go:36`). So the stored field is control-char
stripped, secret-scrubbed, and length-capped before anything downstream sees it.

**Every consumer is read-only, inside uzi.** The DTO mapper copies it
(`api/internal/handler/runs_dto.go:193`, `ReportMd: textPtrValue(...)`); the CLI prints
it under a `findings:` heading (`api/cmd/uzi/run_render.go:253`,
`if r.ReportOnly && r.ReportMd != nil {`); the web shows it in a Findings card
(`web/src/pages/RunView.tsx:2142-2154`, the `<h2 …>Findings</h2>` at `:2147`) and names
it in the hero line (`web/src/pages/RunView.tsx:2446`, the `"; findings below"` clause).
It never leaves uzi: `git grep -n 'ReportMd\b' -- api/ | grep -v '_test\|sql.go\|/store/'`
returns only those consumers plus the ingest/DTO sites, and the forge-write method
`CreateIssueNote` has exactly five non-test callers — `poller/autopilot.go:280`,
`poller/ci_autofix.go:247` and `:293`, `poller/mr_review_watch.go:260`, and
`runlifecycle/lifecycle.go:470` — none of which carries `report_md`. No `notifysvc`
or `slacksvc` path references it either.

**The hook that could carry it already runs, and deliberately declines.** The single
terminal-comment hook posts on autopilot runs only —
`if !mc.AutoApprove {` / `return // only autopilot runs comment; a manual run's outcome
is the user's own` (`api/internal/runlifecycle/lifecycle.go:447-448`) — and on a
completion with no MR it posts a bare stub:
`"Autopilot completed without opening a merge request."`
(`api/internal/runlifecycle/lifecycle.go:489-490`). `docs/autopilot.md` documents
exactly two outcomes — the MR-link success (`docs/autopilot.md:66-68`) and the
failure comment (`docs/autopilot.md:81-84`) — and a report-only completion is neither.
There is no completed-run notification kind: the notification `Kind` literals are
`run_failed` (`api/internal/notifysvc/run_failure_notifier.go:149`), `ci_autofix_landed`
(`api/internal/forgesvc/pipeline_sync.go:259`), `mr_rework_halted`
(`api/internal/poller/mr_review_watch.go:311`), `early_limit_reset`
(`api/internal/notifysvc/service.go:187`), and `incidental_finding`
(`api/internal/notifysvc/service.go:181`) — none is a run completion.

**The maintainer already pays for the gap by hand.** The release skill has to sweep
these out manually: "Also surface report-only runs whose issue could now be closed —
not every delivery is a PR." (`.agents/skills/uzi-release/SKILL.md:42`), reading each
one's answer with `uzi run get <id> --field report_md` and warning "report-only ≠
resolved" (`.agents/skills/uzi-release/SKILL.md:52`).

**Why PRD #19's refusal to echo does not bind this field.** The terminal-comment hook
refuses to interpolate free text on purpose — "Bodies are FIXED templates … the reason
is not guaranteed content-free, so the failure comment points at the run for detail
instead of echoing it" (`api/internal/runlifecycle/lifecycle.go:435-440`). That
reasoning is exact for `failure_reason`, whose only sanitiser is `sanitizeFailureReason`
= `stripNUL` + `truncateRunes` with no secret scrub
(`api/internal/workersvc/pgparams.go:38-45`). It is false for `report_md`, which is
already secret-scrubbed at ingest — and the write-boundary primitives that make
untrusted text inert in a filed forge body now exist in a stdlib-only package:
`FenceBlock` ("a backtick run STRICTLY LONGER than the longest backtick run in s",
`api/internal/issuedraft/issuedraft.go:330-331`, func at `:332`), `SanitizeFiledBody`
(`api/internal/issuedraft/issuedraft.go:311`), `StripUnfencedSlashLines`
(`api/internal/issuedraft/issuedraft.go:389`), and `ScrubSecretShapes`
(`api/internal/issuedraft/issuedraft.go:506`). The `issuedraft` package imports only
`regexp`, `strconv`, `strings`, `time`, so a `runlifecycle → issuedraft` dependency is
cycle-free. PRD #929 set the precedent that the api may auto-post a terminal report to
the forge with the agent gaining no write tool — "the api does the forge write — the
agent never gains a forge-write tool, D1"
(`api/internal/workersvc/proposal_filing.go:101-104`); "The api does the writing …"
(`docs/scheduling.md:131`).

The user's own spec falls between the two documented clauses: "Outcome returns as one
GitLab issue comment: MR link on success; on failure, one comment with a run link."
(`specs/human.md:197`). A report-only success is neither an MR link nor a failure.

## Sketch

### M1 — deliver the report on the hook that already exists (no migration)

- **Widen the read.** `GetRunMoveContext` (`api/internal/store/queries/runtime.sql:3670`;
  its own comment already says "It also carries the facts the M5 terminal-comment hook
  needs from the same read", `:3678-3682`) gains `r.report_only, r.report_md`. Both
  lifecycle observers already load this row — the inline notify path
  (`maybeTerminalComment` reached at `api/internal/runlifecycle/lifecycle.go:288`) and
  the reconcile loop (`api/internal/runlifecycle/lifecycle.go:571`) — so nothing new is
  read per tick. `sqlc generate`; the columns have existed since
  `api/internal/store/migrations/00114_run_report_only.sql` (`ADD COLUMN report_only`,
  `ADD COLUMN report_md`), so M1 needs **no migration**.
- **Assemble the body.** In `terminalCommentBody`'s completed-without-MR branch
  (`api/internal/runlifecycle/lifecycle.go:487-490`), append
  `issuedraft.FenceBlock(mc.ReportMd)` plus one fixed, server-built provenance line
  (model the deterministic footer on `findingProvenance`,
  `api/internal/issuedraft/finding.go:84`, the sibling of the exported `RenderFinding`
  at `:48`), then route the whole assembled body through `issuedraft.SanitizeFiledBody`.
  The FIXED-template rule is preserved: the only non-template content is one
  breakout-proof fenced block, and it is server-scrubbed twice over (ingest + write
  boundary).
- **Empty stays a stub.** `scrubThenBound` can return the empty string
  (`api/internal/workersvc/report_only.go:54-56`), so an empty/absent `report_md`
  yields today's stub verbatim.
- **Once-per-run is already guaranteed** and is not MR-conditional:
  `ClaimAutopilotTerminalComment` (`api/internal/store/queries/runtime.sql:3708`) claims
  the single comment with `UPDATE runs SET autopilot_commented_at = now() WHERE id = @id
  AND auto_approve = true AND autopilot_commented_at IS NULL;` (`:3717-3718`).
- **Settings gate, honestly scoped (a reviewer finding on this plan).** Add one
  three-state key beside `KeyMrReworkEnabled` (`api/internal/settings/keys.go:261`) and
  `KeyCiAutofixEnabled` (`:268`), read three-state and fail-closed the way
  `CiAutofixEnabled` does (`api/internal/settings/settings_ci_autofix.go`). This is not
  free: `runlifecycle` has no settings access today
  (`git grep -i -F settings -- api/internal/runlifecycle/` is empty; the `Lifecycle`
  struct holds only `q Store`, `mover`, `now`, `projector`, `frontendOrigin`,
  `api/internal/runlifecycle/lifecycle.go:90`), so M1 adds a settings collaborator,
  widens the `Store` interface (`api/internal/runlifecycle/lifecycle.go:62`) and the
  `fakeStore` in `api/internal/runlifecycle/lifecycle_test.go:25`, and threads it through
  the construction sites. And because the hook is record-then-comment — claim first
  (`api/internal/runlifecycle/lifecycle.go:453`), body built after (`:469`), a post
  failure is "Recorded but lost (never retried)" (`:471`) — **read the setting BEFORE
  the claim**: a settings-read error degrades to today's stub, never to no comment at
  all.
- **Docs.** `docs/autopilot.md` "What happens next" gains the third outcome plus one
  sentence on why the failure comment deliberately does not change; `CHANGELOG.md`.
  There is no user docs page for report-only completion today —
  `git grep -rn -i 'report-only' -- docs/` returns only `docs/agent-templates.md:86`,
  which is the unrelated "report-only instruction to the wave" sense.
- **Tests.** A hostile-body test modeled on the existing
  `TestRenderFindingRoutesEveryUntrustedFieldThroughItsSanitiser`
  (`api/internal/issuedraft/finding_test.go:15`) and `TestRenderFenceBreakoutProof`
  (`api/internal/issuedraft/issuedraft_test.go:75`): a 5-backtick run, leading `/close`
  and `/label` lines, an `@`-mention, and a raw image URL, asserted fenced and inert.
  Plus: gate-off posts the stub; empty-report posts the stub; a non-report-only
  completion is unchanged; a settings-read error degrades to the stub.

### M2 — an explicit "Deliver report" action for a manual run (cuttable)

- **Migration.** Additive `runs.report_delivered_at timestamptz`, mirroring the
  `autopilot_commented_at` idiom (`ADD COLUMN autopilot_commented_at timestamptz NULL`,
  `api/internal/store/migrations/00039_autopilot_terminal_comment.sql:20`), with a
  guarded-UPDATE claim so two concurrent POSTs cannot double-post.
- **Endpoint.** `POST /api/runs/{id}/report/deliver` in the `RequireUser` `/runs` group
  (the group's `r.Use(mw.RequireUser(...))` at `api/internal/handler/handler.go:832-833`,
  a sibling of `r.Patch("/{id}/priority", h.SetRunPriority)` at `:885`), so it is
  CLI-reachable on a `uzc_` Bearer. Precondition: owner-scoped, `status = completed`,
  `report_only`, non-empty `report_md`, not already delivered.
- **CLI.** `uzi run deliver-report <id>` in `api/cmd/uzi/run.go` +
  `api/internal/uzicli/client.go` + the `FakeClient` (`api/internal/uzicli/fake.go`) +
  `docs/cli.md` + the embedded `api/internal/uzicli/skill/SKILL.md` that the CLI
  skill-drift test pins.
- **Web.** A "Deliver to issue" action on the Findings card
  (`web/src/pages/RunView.tsx:2142-2154`), re-checking the `:2446` hero sentence; a
  discriminating fixture in `web/src/mocks/mockApi/runs.ts` — one case per precondition
  clause (offered / already-delivered, rendered differently / empty-report hidden /
  non-report-only hidden), plus an assertion the fixture actually contains all four.
- **Post with the connection's bot token**
  (`f, err := l.mover.ForgeForConnection(mc.ForgeType, mc.BaseUrl, mc.TokenCiphertext)`,
  `api/internal/runlifecycle/lifecycle.go:462`) so PRD #381's D1 self-filter drops it
  from later runs' `issue_comments` snapshots —
  `buildIssueCommentsSnapshot` (`api/internal/workersvc/issue_comments.go:44`) drops every
  comment whose author equals the connection's bot id — the "D1 self-filter: drop every
  comment uzi's own bot authored, preserving order." loop
  (`api/internal/workersvc/issue_comments.go:51`);
  `prds/done/381-worker-reads-issue-comments.md:45`.
  It must NOT resolve the caller's own connection the way findings filing does
  ("CreateIssue on the caller's own connection",
  `api/internal/handler/findings_file.go:40`, via `ForgeForConnection(repo.…)` at `:151`).
  Pin the token choice with a test.

## Where it lives / what it touches

- **M1** — `api/internal/store/queries/runtime.sql` (widen `GetRunMoveContext`);
  `api/internal/runlifecycle/lifecycle.go` (+ new settings collaborator, widened `Store`
  and `fakeStore`); `api/internal/issuedraft/` (reused; optionally a small
  `RenderRunReport` sibling of `RenderFinding`); `api/internal/settings/keys.go` +
  `settings_*.go` + the `web/src/pages/AdminSettings.tsx` toggle; `docs/autopilot.md`;
  `CHANGELOG.md`.
- **M2** — `api/internal/store/migrations/` (one additive column; live head at write
  time is `00229_recovery_custody_episode.sql`, so any number here is a draft assigned at
  landing); `api/internal/handler/handler.go` + a new `report_deliver.go`;
  `api/cmd/uzi/run.go`; `api/internal/uzicli/…`; `web/src/pages/RunView.tsx`;
  `web/src/lib/api.ts`; `web/src/mocks/mockApi/`; `docs/cli.md`.
- **Not touched** — `agent/src/**` (the worker gains no tool); `main`; the four
  guardrail layers.

## Caveats / scoping notes

- **Size.** M1 is small (no migration) but not a one-liner: the settings collaborator
  pulls a `Store`/`fakeStore` widening and construction threading behind it. M2 is
  small-to-medium. Ship M1 alone and it is the whole loop for unattended runs.
- **Riskiest assumption, and its hour-scale validation.** That a fenced, sanitized
  `report_md` is inert under all three forges' markdown renderers — the ingest scrub
  "does not cover markdown/link injection"
  (`adr/0279-report-only-completion.md:82-83`). Validate by posting one hostile 8 KiB
  body through `FenceBlock` + `SanitizeFiledBody` to a scratch issue on GitLab, GitHub,
  and Forgejo via `CreateIssueNote` (`api/internal/forge/forge.go:730`; drivers
  `api/internal/forge/gitlab.go:496`, `api/internal/forge/github_issues.go:201`,
  `api/internal/forge/forgejo_issues.go:276`) before the PRD decision. Fallback if any
  renderer proves unsafe: post a link + a server-built headline and keep the body in
  uzi.
- **Feedback loop.** A report comment becomes untrusted input to later runs on the same
  issue; it is dropped only because it posts with the connection's bot token (the D1
  self-filter). M2 must not drift to the caller's PAT — that is why the token choice is
  test-pinned.
- **Noise on a public issue.** Bounded (8 KiB), fenced, kill-switched, and once-per-run
  — but it cannot be edited or retracted once posted. Say so.
- **Prior art to reconcile.** ADR-0333 states the findings guardrail: "the human gates
  every filing; the worker never writes to the forge."
  (`adr/0333-incidental-findings.md:11`). The argument for consistency: the api does the
  write, the agent gains no forge tool, the text is fenced + `SanitizeFiledBody`'d, a
  comment on the asking issue is not a *filing* of a new issue, and PRD #929 already set
  the auto-post-on-terminal-report precedent. The maintainer decides whether that holds.
- **Adjacency.** `ideas/bingo/2026-09-01-mr-conflict-watch.md` and
  `ideas/bingo/2026-09-08-repo-wide-ci-failure-gate.md` are the poller/forge-observation
  lane; this is the run-lifecycle write lane — no shared mechanism. One shared FILE:
  both this idea and the CI-gate idea (`ideas/bingo/2026-09-08-repo-wide-ci-failure-gate.md:253`)
  name `api/internal/settings/keys.go` for an additive kill-switch key; the keys are
  distinct, so the edits are non-conflicting. PRD #1253 (`prds/1253-run-merged-signal.md`,
  run-merged signal) is the opposite population — runs WITH an MR. PRD #1255
  (`prds/1255-tui-forge-view.md`, still open) landed a `Conflicts *bool` on its forge
  summary type (`prds/1255-tui-forge-view.md:231`), an unrelated TUI-forge lane.
- **Explicitly out of scope.** Closing the issue — a human already can
  (`prds/done/1034-close-reopen-from-board.md`), and "report-only ≠ resolved"
  (`.agents/skills/uzi-release/SKILL.md:52`). Changing the failure comment. A
  `run_completed` notification kind. Delivering `judge_verdict` or review summaries.
  Prompt-run reports — ingest kind-gates `report_md` to issue runs
  (`api/internal/workersvc/report_only.go:23-26`).

## Open questions (for the maintainer)

1. **Default on or off?** Recommend default-off for one release, then flip.
2. **Does `specs/human.md:197` gain a third clause** for the report-only outcome? This
   is the user's call on the spec-keeper's file — flagged, not proposed.
3. **M2 re-deliver semantics:** one-shot (matches `autopilot_commented_at`) or allow a
   re-deliver?

## Dedup checks performed

Commands re-run at `ab1d7431`:

| Check | Command | Result |
|---|---|---|
| Any doc proposes forge-delivering report_md | `git grep -rn -i -F 'report_md' -- prds/ ideas/ adr/ docs/ specs/human.md` | Only `adr/0279-report-only-completion.md` (design of the field), `prds/done/1009-cli-file-splits.md:63`, and `prds/done/377-early-fail-unpushable-workflow.md` (119/125/279/280) — none proposes delivery |
| Phrase sweep | `git grep -rn -i -F` of "deliver the report" / "post the report" / "report to the issue" / "report comment" / "answer on the issue" over `prds/ ideas/ adr/ docs/ specs/` | Empty |
| report_md leaves uzi | `git grep -n 'ReportMd\b' -- api/ \| grep -v '_test\|sql.go\|/store/'` | Only DTO/CLI/web consumers + ingest; no forge write |
| CreateIssueNote callers | `git grep -rn CreateIssueNote -- api/ \| grep -v -i test` | `poller/autopilot.go:280`, `poller/ci_autofix.go:247,293`, `poller/mr_review_watch.go:260`, `runlifecycle/lifecycle.go:470` — none carries report_md |
| Completion notification kind | enumerate `Kind:` literals in `api/internal/notifysvc/` and callers | `run_failed`, `ci_autofix_landed`, `mr_rework_halted`, `early_limit_reset`, `incidental_finding` — none is a completion |
| report-only user docs | `git grep -rn -i 'report-only\|report only' -- docs/` | Only `docs/agent-templates.md:86`, unrelated wave-instruction sense |
| Open PRDs on this surface | grep `runlifecycle`/`report_md` in `prds/1253-run-merged-signal.md`, `prds/1202-on-demand-mr-rework.md`, `prds/1233-structured-blockers-mr-rework.md`, `prds/1293-failed-run-rate-dashboard.md` | Zero matches in all four |

The five `ideas/bingo/` files and their dispositions: run-queue-priority shipped
(`prds/done/320-run-queue-priority.md`); worker-pause-quiesce's adjacent work archived
(`prds/done/496-worker-cordon-pill.md`); mr-review-rework-runs shipped via
`api/internal/poller/mr_review_watch.go`; mr-conflict-watch partly overtaken;
repo-wide-ci-failure-gate open. Nearest prior art is all adjacent-but-distinct: PRD #19
built the hook this idea rides ("Outcomes are surfaced in GitLab through one hook",
`prds/done/19-admin-settings-and-autopilot.md:32`); PRD #929
(`prds/done/929-schedule-output-modes.md`) set the api-writes/agent-gains-no-tool
precedent; ADR-0333 (`adr/0333-incidental-findings.md`); PRD #381's D1 self-filter. The
lead enumerated open forge issues via the forge tool (the 50 most recently updated plus
the `brainstorm`/`enhancement`/`PRD`/`Planned`/`Later`/`uzi` labels) and found no
outbound report-delivery proposal; the nearest were #1328, #261, #186, and #1324/#1088;
older unlabeled issues are not enumerable that way.

**Also considered this cycle (not proposed).** (a) A cross-MR file-overlap and
migration-number-collision warning, grounded in the release skill's "Two PRs adding the
same `NNNNN_*.sql` … will both land as distinct files … and brick strict-goose boot"
(`.agents/skills/uzi-release/SKILL.md:38`) and "Probe cross-PR conflicts first"
(`.agents/skills/uzi-release/SKILL.md:83`) — it lost because it is a cross-PR release
property, not a per-run lifecycle behaviour, so it belongs to the release tooling, not
here. (b) A "no enabled schedule will ever pick this up" coverage gap — it lost because
the eligibility half is already surfaced through
`api/internal/schedsvc/skip_reason.go:19` (`SkipNotEligible = "not_eligible"`), so a new
signal would duplicate an existing one.
