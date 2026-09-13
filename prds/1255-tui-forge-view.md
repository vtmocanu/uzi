# PRD 1255: TUI forge view — open PRs, live PR checks, CI runs (bot-PAT backed)

- **Issue**: #1255
- **Status**: Draft, sent to uzi (Auto mode)
- **Priority**: Medium
- **Surfaces**: the forge layer (`api/internal/forge`, three additive read methods), three owner-scoped read routes + DTOs (`api/internal/handler`, `api/internal/apitypes`), the CLI client + two new commands (`api/internal/uzicli`, `api/cmd/uzi`), and the TUI (`api/cmd/uzi/tui_*.go`). **Everything is in the `api` Go module**, so `task gate:api` is the gate for every milestone; `task gate:web` runs only where `web/src/lib/apiTypes.ts` changes (the DTO three-file edit). No migration, no new table, no web page.
- **Trust boundary**: read-only forge reads through the connection PAT the api already holds; the new routes are owner-scoped like `GET /api/repos/{id}/board`. No new credential, no new secret, no forge writes.
- **Guardrail**: neither implementation nor validation touches `.github/workflows/**` (worker PAT lacks `workflow` scope; `.claude/rules/prds.md`). Invariant at finalize: `git diff --name-only <base>..HEAD` shows zero entries under `.github/workflows/`. CI-related behaviour is validated with canned JSON fixtures and `forgetest.BaseFake` overrides, never a real workflow file.

## Problem

`uzi tui` shows the factory floor and one run. It shows nothing about what happens *after* a run pushes: whether the PR's checks are green, whether CodeRabbit asked for changes, whether the branch conflicts with `main`, whether the release tag's workflow ran. Today that means leaving the TUI for the GitHub web UI, or a locally installed and authenticated `gh`. The api already holds the forge PAT and already reads pipelines for the board badges, so every byte the TUI would need is one forge call away with credentials uzi already has. The user's stated goal: **not have to check the GitHub web UI for these statuses.**

## Solution

Two new top-level TUI screens beside the floor, plus two drill-ins, all fed by the api through the stored connection PAT. The user configures nothing new: no `gh`, no personal token.

| screen | what it is | drill-in |
|---|---|---|
| `pulls` | every open PR/MR on the selected repo, banded like the board (NEEDS YOU / IN FLIGHT / READY) with a checks cell, a review cell, the branch, the title, and a `↳ <run>` link when a uzi run opened it | **PR view**: the `gh pr checks` shape (every check with description and elapsed, pending first), REVIEWS, MERGE, re-polled live every 5s |
| `ci` | the repo's workflow-run list (the `/actions` shape): every event (push to `main`, `pull_request`, `schedule`, a `v*` tag push), banded RUNNING / FAILED / RECENT | **CI run view**: jobs and steps with status and duration; a failed job's log tail (M7, optional) |

The two drill-ins are peers of the run view, cross-linked through `runs.mr_iid`: the PR view has `↳ run`, the run view gains `↳ pr`.

### Mock (rendered with the shipped palette; the frames below are the spec for layout and vocabulary)

The frames are 100 columns; the real views adapt to the terminal width the way the board does (drop columns right-to-left before the title, `boardRowPrefixWidth` pattern). Column widths and glyphs are the shipped board's: `▸` cursor, `▌` andon spine, one state glyph, `✓ ✗ ● · ✎ ⚠`, `▰▱` micro-bar, the three-band layout with a faint CAPS eyebrow (amber for the needs-you band).

`pulls`:

```
 ▚▚ uzi · floor  pulls  ci   vtmocanu/uzi                             ✎ 3 · ● 2 · ✓ 2 · 7 open · 1–7
 ▏github · uzi-bot · synced 4s ago

 NEEDS YOU · 3
▸▌✎ #1254  ✓ 6/6   ✎ changes   2h   agent/issue-1246     Completion interlock M4–M6: ro…  ↳ f8064ef5
   coderabbitai · changes requested · 14m ago · 3 actionable, 2 nitpicks · no conflicts with main
 ▌✗ #1256  ✗ 1/6   · review    41m  agent/issue-1251     TUI andon surfaces: update pro…  ↳ 0c947b6c
 ▌⚠ #1258  ✓ 5/5   ✓ approved  1d   fix/1234-mktemp-gnu  e2e: portable mktemp — conflic…

 IN FLIGHT · 2
 ▌● #1257  ● 3/6   · review    12m  agent/issue-1253     Run merged-signal coverage + l…  ↳ 9c672af9
 ▌● #1255  ● 5/6   · review    26m  uzi/self-improve/42  self-improve: tighten claim af…  ↳ d8648b31

 READY · 2
 ▌✓ #1249  ✓ 6/6   ✓ approved  3h   renovate/golangci-…  chore(deps): update golangci-l…
 ▌✓ #1244  ✓ 4/4   ✓ approved  2d   docs/oidc-degraded   docs: describe the oidc-degrad…

 enter/→ open · ↳ run · w rework · f fix ci · / filter · R repo · tab ci · r refresh · ? keys · q quit
```

PR view, live (checks still landing):

```
 ‹ pulls  PR #1254 · agent/issue-1246 → main                      ● 5 pending · ✓ 5/10   ● live · 5s
 Completion interlock M4–M6: rollout switch + permit fin… · uzi-bot · run f8064ef5 · 2h · +842 −131

 CHECKS  ● 5 in progress · 5 passed · 0 failing · re-polled 1s ago
▸▌● CodeRabbit                                    Waiting for status — Review in progress    3m00s
   ↗ https://github.com/vtmocanu/uzi/actions/runs/34677104577/job/103508797991
 ▌● CodeQL                                        waiting on 1 analysis                      —
 ▌● CodeQL / Analyze (javascript-typescript)      in progress                                1m08s
 ▌● CI / test-api                                 in progress                                2m10s
 ▌● CI / test-web                                 in progress                                1m36s
 ▌✓ CodeQL / Analyze (actions)                                                               45s
 ▌✓ CodeQL / Analyze (go)                                                                    1m52s
 ▌✓ CodeQL / Analyze (python)                                                                55s
 ▌✓ CI / lint-api                                                                            2m31s
 ▌✓ KinD Smoke                                                                               16s

 REVIEWS
 · coderabbitai  reviewing           started 3m00s ago
 · vtmocanu      review requested    2h ago

 MERGE
 ✓ no conflicts with main   ● waiting on 5 checks · 6 required · branch protection

 ↑↓ move · l job log · f fix ci · ↳ run · esc back · ? keys
```

PR view, settled (the user's own `gh pr checks 1254` state: all green, changes requested):

```
 ‹ pulls  PR #1254 · agent/issue-1246 → main             ✓ 10/10 · ✎ changes requested   ● live · 5s
 Completion interlock M4–M6: rollout switch + permit fin… · uzi-bot · run f8064ef5 · 2h · +842 −131

 CHECKS  ✓ all checks passed · 10 successful · 0 failing · 0 pending · 0 skipped
▸▌✓ CodeRabbit                                    Review completed                           4m12s
   ↗ https://github.com/vtmocanu/uzi/actions/runs/34677104577/job/103508797991
 ▌✓ CodeQL                                                                                   3s
 …
 REVIEWS
 ✎ coderabbitai  changes requested   just now · 3 actionable, 2 nitpicks
 · vtmocanu      review requested    2h ago

 MERGE
 ✓ no conflicts with main   ✓ required checks passed   ✎ blocked: changes requested · admin override

 ↑↓ move · l job log · w rework (fix CR findings) · ↳ run · esc back · ? keys
```

`ci`:

```
 ▚▚ uzi · floor  pulls  ci   vtmocanu/uzi                               ● 3 · ✗ 2 · ✓ 41 today · 1–9
 ▏github actions · uzi-bot · synced 4s ago · main ✓ green

 RUNNING · 3
▸▌● CI #1039        pull_request  agent/issue-1246     2m14s   PRD #1226 continuation: …   ▰▰▱▱▱ 2/5
   ↳ PR #1254 · ✓ lint-repo ✓ validate-api ● test-api 1m50s ● test-web 1m12s · gate-agent queued
 ▌● CodeQL #1195    pull_request  refs/pull/1254/head  1m08s   PR #1254                    ▰▰▰▱▱ 3/5
 ▌● CI #1040        push          uzi/self-improve/42  6m40s   self-improve: tighten cl…   ▰▰▰▰▱ 4/5

 FAILED · 2
 ▌✗ CI #1037        pull_request  agent/issue-1251     5m02s   TUI andon surfaces: upda…  ✗ lint-api
 ▌✗ nightly #88     schedule      main                 38m     gitlab lane: mr-rework p…       ✗ e2e

 RECENT · 4
 ▌✓ KinD #1062      pull_request  agent/issue-1246     16s     PRD #1226 continuation: …          1m
 ▌✓ CI #1038        push          main                 7m41s   docs(prd-1253): create P…          3h
 ▌✓ CodeQL #1194    schedule      main                 4m03s   scheduled                          3h
 ▌✓ release #61     push          v0.77.0              11m     chore(release): 0.77.0             2d

 enter/→ jobs · l log · f fix ci · / filter · R repo · tab pulls · r refresh · ? keys · q quit
```

(The `o browser` key in the earlier mock is dropped, D9: links are OSC-8 hyperlinks on the id/name, as the issue link already is, and the URL is drawn faint on the selected row so it can be copied. No shell-out.)

## Resolved facts (current code; an offline worker can verify each by reading the cited file)

### Credentials: nothing new to configure

- The api holds the forge PAT sealed in `forge_connections.token_ciphertext` (`api/internal/store/migrations/00002_forge.sql:6-18`) and opens it in exactly one place, `ForgeForConnection` (`api/internal/forgesvc/service.go:258-264`). The TUI talks only to the api with its `uzc_` token. **There is no per-repo bot identity**: `repos.connection_id` points at the connecting user's connection, so the view sees whatever that bot account sees.
- **No PAT scope change on any forge.** GitHub: the classic `repo` scope uzi requires is documented as covering "issues, pull requests, and read of Actions runs/jobs/logs" (`docs/github-bot-setup.md:41-44`); check-runs and commit statuses on a private repo are also under `repo`. GitLab: `api` (`docs/gitlab-bot-setup.md:19`). Forgejo: `write:repository, write:issue, read:user`, and uzi already refuses Forgejo < v16.0.0 because the Actions job-log endpoint shipped there (`docs/forgejo-bot-setup.md:36-58`). `privcheck` needs no change.

### What the forge layer already has, and what it lacks

- `Forge` interface, `api/internal/forge/forge.go:448-594`. Already there and reused as-is: `GetMergeRequest` (`:516`, returns only `IID, State, WebURL`), `ListMergeRequestComments` (`:527`, per-comment `ReviewState`), `LatestPipeline` (`:563`), `LatestMRPipeline` (`:572`), `ListPipelineJobs` (`:577`, `Job{ID,Name,Stage,Status,WebURL}`), `JobLogTail` (`:587`, full download then tail, 16 MiB fail-closed ceiling, documented as fix-trigger-time only), `ErrNoPipeline` (`:28`), `ErrForgeVersionUnsupported` (`:44`).
- **Missing** (grepped): no MR/PR *list*, no head SHA / draft / conflicts / review decision on `MergeRequest`, no per-check listing (check-runs are folded into one status string, never returned as jobs, `api/internal/forge/github_pipelines.go:185-221`), no commit-status read at all (`Repositories.ListStatuses`/`GetCombinedStatus` are not called anywhere in the package), no workflow-run list. `Job` carries no timing and no steps.
- GitHub driver today: `LatestPipeline` = `Actions.ListRepositoryWorkflowRuns{Branch}` (`github_pipelines.go:28-37`) with a check-runs fallback (`:51-53`, `listCheckRunsForRef :228`, `listCheckSuitesForRef :259`); `LatestMRPipeline` resolves the head SHA via `PullRequests.Get` (`:62-84`); `ListPipelineJobs` = `Actions.ListWorkflowJobs` (`:462`); `JobLogTail` = `Actions.GetWorkflowJobLogs` + second-hop fetch with SSRF guards (`:500-627`). Status mappers `githubActionsStatus :648`, `toGitHubPipeline :657`, `toGitHubJob :671`.
- The status classifier is `api/internal/pipelinestatus` (`IsFailed :51`, `IsSuccess :72`, the SQL twin `FailedStatuses :61`); the web's five-tone badge taxonomy is `web/src/lib/pipelineBadge.ts` (passed / failed / running / attention / neutral). The TUI mirrors those five tones onto the palette: passed→`sage ✓`, failed→`alarm ✗`, running→`wait ●`, attention→`amber ✎/⚠`, neutral→`faint ·`.
- **Why the new views cannot ride the `pipeline_statuses` cache**: it is latest-per-`(repo, ref)` for the default branch plus at most `CI_WATCH_MAX_REFS` (default 20) recent agent-run branches (`api/internal/forgesvc/pipeline_sync.go:50-155`, `docs/configuration.md:165`). It has no PR list, no per-check detail, no workflow-run history, and a 14-day run window. The new routes read the forge on demand (D4) and use the cache only as the source of the `f fix ci` precondition (the existing endpoint reads it, `api/internal/handler/ci_fix.go:47`).

### Existing api pieces the feature reuses verbatim

- The repo routes are split in two sub-groups in `api/internal/handler/routes_repos.go`: **`RequireUser`** (`:25-26`, Bearer `uzc_` or cookie; `GET /` = `uzi repo list` at `:27`, `GET /{id}/board` at `:111`) and **`RequireAuth`** (`:76-77`, cookie-only; the comment at `:17-23` explains a Bearer token 401s there). The new read routes go in the `RequireUser` sub-group.
- `forgeLimiter` is a fixed-window counter, `FORGE_RATE_LIMIT_MAX` = 30 per `FORGE_RATE_LIMIT_WINDOW` = 1m, keyed on **chi route pattern + user id** (`api/internal/middleware/ratelimit.go:38,109`; config `api/internal/config/config.go:750-751`). It must be `.With`-ed **after** `RequireUser` or it degrades to a shared IP bucket (`routes_repos.go:28-30`), and `api/internal/handler/route_limiter_mounts_test.go:144` names every route↔limiter pair, so each new route is added to that table. A 5 s drill-in poll is 12 requests/min per route, a 10 s list poll 6/min: under the 30/min window even with two terminals open.
- No generic short-TTL memo exists in the api today (the only singleflight is private to OIDC discovery, `oidc/provider.go:86`; `settings.Cache` is a settings-table cache). D4's memo is new code.
- Actions the TUI keys map onto: `w rework` → `POST /api/runs/{id}/rework` (`handler/mrrework_run.go:90`, mounted under `RequireUser` + the limiter at `handler.go:798`, already CLI-reachable via `uzi run rework`; preconditions: kill-switch `settings.MrReworkEnabled` fails closed → 409, owner-scoped run, then the typed 409 sentinels `ErrReworkRunNotCompleted`, `ErrReworkNoMR`, `ErrReworkMRNotOpen`, `ErrReworkNothingNew`, … at `:166-173`). `f fix ci` → `POST /api/repos/{id}/ci-fix-runs {"ref"}` (`handler/ci_fix.go:25`; 409 unless `GetPipelineStatusByRef` has the ref **and** `pipelinestatus.IsFailed` on it, `:46-60`). **That route is mounted in the cookie-only `RequireAuth` sub-group (`routes_repos.go:146`), so the TUI cannot call it today**; D12 moves it to the `RequireUser` sub-group.
- `pipelinestatus` (`api/internal/pipelinestatus/pipelinestatus.go`) exports exactly `IsFailed :51`, `FailedStatuses :60`, `IsSuccess :72` and deliberately has **no running/pending classifier**; the web's five tones live only in `web/src/lib/pipelineBadge.ts:14,90`, and `TestMirrorsWebPipelineBadge` pins just the failed/passed sets. M1 adds a Go `Tone(status)` classifier for all five tones and widens that drift test to all five, so the TUI and the web can never colour one status two ways.
- Run ↔ PR linkage: `runs.mr_iid` (`migrations/00020_workers_runs.sql:43`), `runs.mr_web_url` (`00069_forgejo_and_mr_web_url.sql:58`), `runs.mr_state` (`00029_run_mr_state.sql:18`); on the wire `RunDTO.MrIID/MrWebURL/MrState` (`api/internal/apitypes/run.go:222-246`) and `RunDTO.RepoID/ForgeType` (`run.go:91-96`). The `(repo_id, mr_iid)` query shape to copy is `GetActiveMRReworkRunForMR` (`api/internal/store/queries/runtime.sql:2127`) minus its `kind`/`status` filters, as a `:many` ordered `created_at DESC` (template: `RecentSelfImproveMRRunsForRepo`, `selfimprove.sql:99`); a `:one` is not possible once the filters go. `RepoDTO` carries `ID, PathWithNamespace, WebURL, DefaultBranch, Enabled` (`apitypes/repo.go:6-12`) and the CLI client already has `ListRepos` (`api/internal/uzicli/client.go:130`), which the TUI does not call yet.
- PR titles, branch names, check names, workflow names, commit titles, logins and descriptions are **forge-authored untrusted text** (D7): drawn through `m.renderer.Plain` (= `capCell(cellText(s))`, `api/cmd/uzi/tui_render.go:103`), added to `d7UntrustedFields` (`tui_d7_guard_test.go`), covered by the hostile-value render test (`tui_model_test.go`). Forge-supplied URLs are trusted into an OSC-8 link only when `https` (the web's `isHttpsUrl` rule, `specs/ai.md` §130).

### TUI seams

- Exactly two top-level screens today: `viewBoard`, `viewDetail` (`api/cmd/uzi/tui.go:58-63`); `View()` dispatch `tui.go:1012-1027` (quitting → help → detail → board); `handleKey` `tui.go:960-996` (ctrl+c modal, `q`, `?`, then `switch m.view`). Board → detail at `tui_board.go:274-287`; detail → board `tui_detail.go:465` and `:504-512`. `tab` cycles panes in the detail view only (`tui_detail.go:500-503`). `boardKey` (`tui_board.go:204`) binds `j k ↑ ↓ pgup pgdn / r a h enter →`; **`tab`, `1`, `2`, `3`, `p`, `c` are unbound on the board**, but every one-rune key is swallowed as filter text while `m.board.filtering` (`:220-226`), so new bindings sit after that block. `m` is unbound in the detail view (`detailKey`).
- Poll chain to reuse for the new lists: `boardRunsMsg` `tui.go:67`, `boardTickMsg{gen}` `:82`, `tickAfter` `:339`, backoff `boardTickInterval` `:348`, the tick handler (generation guard + in-flight guard) `:634-669`, the reply handler `:683-699`, request ids `startBoardReq` `:441`, the guard fields and invariant `tui_board.go:30-42`, `boardState.apply` `:72`; `boardPollInterval = 2s`, timeout `10s`, backoff cap `30s` (`tui.go:33-43`). One fetch example: `fetchRunsCmd` `tui.go:417-434`. The new screens get their own `pullsState` / `ciState` with the same three fields (`reqSeq`, `waitID`, `tickGen`) rather than sharing the board's.
- Chrome: wordmark `renderBoard` `tui_board.go:292-335`; eyebrow `boardEyebrow :447`; row painter `boardRow` `tui_board_rows.go:202` (`paintSeg`, `padSeg`, `padCell`, `clampVisual` in `tui_text.go`); footer `boardFooter :456` / `keyHint` `tui_detail.go:864`; palette `newPalette` `tui_render.go:161-196`; state token `stateToken :323`; `▰▱` micro-bar `milestoneMarker` `tui_board_rows.go:438`; OSC-8 `issueLink` / `linksEnabled` `tui_osc.go:41-56`; colorprofile from `tea.ColorProfileMsg` `tui.go:627-629`.
- The uxlab generator (`api/cmd/uzi/uxlab_gen_test.go:80`, `scenes := map[string]func(dark bool) string`, frame box 100×34, gated by `UZI_UXLAB_GEN=1`) renders the shipped model to light/dark PNGs offline; a new scene is one map entry + a builder (`.claude/rules/tui.md`). Do not use the `--sketch` harness for this unattended worker (PRD #1251 D8 reasoning applies: finalize invariant, only `template` registered).
- `d7UntrustedFields` (`api/cmd/uzi/tui_d7_guard_test.go:86`) is a flat `[]string` of bare field **names** mixing apitypes names and the internal struct fields the TUI copies them into; add **both** spellings for every new untrusted field, or the AST guard misses the draw (rule at `:26-31`; indirection through a local variable is a known gap, so draw fields directly).
- Detail keys in use: `←→ h l tab`, `↑↓ j k`, `g`, `c`, `v`, `f`, `y n`, `x`, `esc`, `?` (`tui_keys.go:11-33`, `helpLines :63-86`). Board keys in use: `enter →`, `/`, `a`, `h`, `r`, `?`, `q`, `j k ↑↓`.

### CLI seams

- Commands are cobra subcommands registered by one `root.AddCommand(...)` call in `api/cmd/uzi/root.go:230` (`newRepoCmd` at `:245` is the shape: `func newRepoCmd(env Env, gf *globalFlags) *cobra.Command`, `repo.go:15`); one file per noun, a separate `_render.go` only when it grows. The `--json` / table idiom is `repo.go:35-42` (`p := env.printer(gf)`; `p.JSON(v)` or `p.Table(headers, rows)`, `api/internal/uzicli/output.go:142,165`). `uzi run get` renders `MR` via the forge-aware noun `mrAbbrev` (`api/cmd/uzi/render.go:62-67`, "MR" on GitLab, "PR" on Forgejo/GitHub); the new commands use it too.
- A client method is a **four-site** edit: the `uzicli.Client` interface (`api/internal/uzicli/client.go:25`, 71 methods today), `HTTPClient` in the matching `client_*.go`, a canned-reply field on `FakeClient` (`fake.go:33`) and its method in the matching `fake_*.go` (71 fake methods, one per interface method; `--demo` and the TUI tests inject the fake).
- A DTO is three registrations (`.claude/rules/go.md`): the Go struct + a `newContractCase[XDTO]("x")` row in `contractCases()` (`api/internal/apitypes/contract_test.go:69`); `fixtures/api-contract/x.{zero,full}.json` recorded from the failing test's output, never hand-authored; and on the TS side an import + type-assert block and a `{ stem: "x", nullable }` row in `dtos` (`web/src/lib/apiContract.test.ts:593`) plus the type in `web/src/lib/apiTypes.ts`. `gate:api` / `gate:web` name whichever is missing; fixtures live above the Go module, so `-count=1` (already on the gate).

### Forge SDK capability (verified offline with `go doc` against the module cache)

Module versions from `api/go.mod`: `github.com/google/go-github/v90` (v90.0.0), `gitlab.com/gitlab-org/api/client-go/v2` (v2.60.0), `code.gitea.io/sdk/gitea` (v0.25.1). `golang.org/x/sync` (singleflight) is already a dependency (`api/go.mod:35`, used by `api/internal/oidc/provider.go:23`).

| need | GitHub (go-github v90) | GitLab (client-go v2) | Forgejo (gitea SDK v0.25.1) |
|---|---|---|---|
| list open PRs | `PullRequestsService.List(ctx, owner, repo, *PullRequestListOptions{State, Sort, Direction, ListOptions}) ([]*PullRequest, …)`. **`Mergeable`, `MergeableState`, `Commits`, `Additions`, `Deletions` are NOT populated by List** (documented on the struct); they need `PullRequests.Get` per PR | `MergeRequestsService.ListProjectMergeRequests(pid, *ListProjectMergeRequestsOptions{State, OrderBy, Sort, Draft, …}) ([]*BasicMergeRequest, …)`; `BasicMergeRequest` carries `SHA, Draft, HasConflicts, DetailedMergeStatus, BlockingDiscussionsResolved, Author.Username, SourceBranch, TargetBranch, WebURL, Reviewers` as value fields | `Client.ListRepoPullRequests(owner, repo, ListPullRequestsOptions) ([]*PullRequest, …)`; `PullRequest{Draft, Mergeable bool, Head *PRBranchInfo, Updated, Additions/Deletions *int}` |
| head SHA / draft / conflicts | `PullRequest.Head.SHA`, `Draft *bool`; conflicts only via `Get` → `Mergeable *bool` + `MergeableState *string` (`dirty` = conflicts) | on the list row: `SHA`, `Draft`, `HasConflicts`, `DetailedMergeStatus` | on the list row: `Head.Sha`, `Draft`, `Mergeable` |
| review decision (D6) | `PullRequestsService.ListReviews(ctx, owner, repo, number, *ListOptions) ([]*PullRequestReview{State, User.Login, SubmittedAt, CommitID}, …)`; `PullRequest.RequestedReviewers []*User` | no review-decision concept; `MergeRequestApprovalsService.GetConfiguration(pid, mr) (*MergeRequestApprovals{Approved bool, ApprovedBy, …})` exists in the SDK with **no tier annotation** — approvals are a Premium feature, a CE instance answers 403/404 at runtime, which the driver maps to `none`; `BasicMergeRequest.BlockingDiscussionsResolved` is free-tier | the SDK has **no** `ListPullRequestReviews`; use the driver's existing raw helper `rawGetLimited` (`api/internal/forge/forgejo.go:230`) on `GET /repos/{o}/{r}/pulls/{index}/reviews` and fold as GitHub |
| checks for a sha (D5) | `ChecksService.ListCheckRunsForRef` (already used, `github_pipelines.go:233`) → `CheckRun{Name, Status, Conclusion, HTMLURL, StartedAt, CompletedAt, App.Slug, Output.Title/Summary}` **plus** `RepositoriesService.GetCombinedStatus(ctx, owner, repo, ref, *ListOptions) (*CombinedStatus{State, Statuses []*RepoStatus{State, Context, Description, TargetURL, UpdatedAt}})` | `JobsService.ListPipelineJobs` (already used, `gitlab_pipelines.go:73`) on the MR's latest pipeline (`ListMergeRequestPipelines`, `:47`) → `Job{Name, Stage, Status, WebURL, Duration float64 (seconds), StartedAt, FinishedAt}`, plus `CommitsService.GetCommitStatuses(pid, sha, *GetCommitStatusesOptions) ([]*CommitStatus{Name, Status, Description, TargetURL, …})` for external statuses | `Client.ListStatuses(owner, repo, ref, ListStatusesOption) ([]*Status, …)` plus the Actions jobs of the runs matching `HeadSHA` (`ListRepoActionRuns{HeadSHA}` then `ListRepoActionRunJobs`, both already used in `forgejo_pipelines.go:76-117`) |
| workflow runs (D5) | `ActionsService.ListRepositoryWorkflowRuns(ctx, owner, repo, *ListWorkflowRunsOptions{Branch, Event, Status, Actor, HeadSHA, ExcludePullRequests, ListOptions})` (already used, `github_pipelines.go:37`) → `WorkflowRun{ID, Name, RunNumber, RunAttempt, Event, Status, Conclusion, HeadBranch, HeadSHA, HTMLURL, Actor.Login, DisplayTitle, CreatedAt, UpdatedAt, RunStartedAt}`; **no sort option** (the endpoint returns newest first) | `PipelinesService.ListProjectPipelines(pid, *ListProjectPipelinesOptions{Ref, Status *BuildStateValue, Source, OrderBy, Sort, ListOptions}) ([]*PipelineInfo{ID, IID, Status, Source, Ref, SHA, Name, WebURL, CreatedAt, UpdatedAt})` (already used, `gitlab_pipelines.go:25`); duration needs `GetPipeline` (`Pipeline.Duration int64` seconds) | `Client.ListRepoActionRuns(owner, repo, ListRepoActionRunsOptions{Branch, Event, Status, Actor, HeadSHA, ListOptions})` (already used for `LatestPipeline`, `forgejo_pipelines.go:43`; the v16.0.0 floor is enforced at connect, `forgejo.go:24,158`) |
| jobs + steps | `ActionsService.ListWorkflowJobs` (already used, `:471`) → `WorkflowJob.Steps []*TaskStep{Name, Status, Conclusion, Number *int64, StartedAt, CompletedAt}` | jobs only, no steps (`Steps` empty) | `ListRepoActionRunJobs` (already used, `:117`), no steps |
| conditional requests | **go-github has no ETag API** (`doc.go:153-168`: "does not handle conditional requests directly … designed to work with a caching http.Transport"); a `304` surfaces as `*github.ErrorResponse` from `CheckResponse` (`github.go:1796-1802`); the ETag is readable from `Response.Header.Get("ETag")` (`github.Response` embeds `*http.Response`). So the conditional layer is an `http.RoundTripper` on the driver's client (D4), not per-call code | n/a (not needed for the reference instance's rate budget; GitLab's limits are per-instance) | n/a |
| rate limit | `*github.RateLimitError{Rate, Response, Message}` and `*github.AbuseRateLimitError{RetryAfter *time.Duration}` (both already handled in `api/internal/forge/github.go:122-123`); every `github.Response` carries `Rate{Limit, Remaining, Used, Reset}` | `*gitlab.ErrorResponse` with 429 | `*gitea.…` 429 |

Driver tests use `httptest.NewServer` + `http.ServeMux` with canned JSON against the real driver: `newMockGitHub` (`api/internal/forge/github_test.go:31`, routes auto-prefixed `/api/v3`), `newMockGitLab` (`gitlab_test.go:24`), `newMockForgejo` (`forgejo_test.go:30`). `forgetest.BaseFake` (`api/internal/forge/forgetest/basefake.go:47`) stubs 26 methods, 24 as `ErrNotStubbed` and 2 pipeline reads as `ErrNoPipeline`; `basefake_test.go:107` asserts the 24/2 split, so three new methods move that count to 27 (M1 updates the assertion). Six embedders exist (listed in `CLAUDE.md`).

## Design

### D1 — Navigation: two list screens + two drill-ins, a tab strip in the wordmark

- `tuiView` grows `viewPulls`, `viewPR`, `viewCI`, `viewCIRun`. The wordmark becomes a tab strip, `▚▚ uzi · floor  pulls  ci`, active tab in `title` (tungsten bold), the others `faint`, followed by the selected repo's `path_with_namespace`.
- `tab` cycles floor → pulls → ci → floor on the three list screens (unused on the board today; it keeps toggling panes inside the run view). `1`/`2`/`3` jump directly. `esc` leaves a drill-in for its list; `esc` on a list returns to the floor (the floor's `esc` is unchanged).
- `enter`/`→` on a `pulls` row opens the PR view; on a `ci` row the CI run view. `↳ run` (key `↳` in the legend is drawn, the binding is `u`) opens the run view of the linked run from a PR row / PR view; the run view gains `m` → PR view when `MrIID` is set (`m` is unused in the detail view). Both jumps push the view they came from so `esc` returns there.
- The `?` overlay and `docs/cli.md`'s keybinding table list every new key.

### D2 — Repo scope: one repo at a time, `R` cycles, default = the repo of the newest run

- The floor is cross-repo; the forge views are per repo (a PR number or a workflow-run list is meaningless across repos, and the two enabled repos on the reference instance sit on different forges). `R` cycles the user's enabled repos (`ListRepos`, `Enabled`), the header names the current one, and the choice persists across tab switches for the session. Default: the `RepoID` of the newest non-chat run on the board **if it is in that enabled set** (the admin board lists other users' runs, whose repos are not); otherwise the first enabled repo. One enabled repo → `R` is hidden.
- Both lists carry a `/` filter over title, branch, author, workflow name (same `board.filter` mechanics).

### D3 — Bands and vocabulary

- `pulls` bands: **NEEDS YOU** (amber eyebrow) = review decision `changes_requested`, or any failing check, or `conflicts`; **IN FLIGHT** = checks running, or review pending, or draft (draft rows faint); **READY** = checks green and review `approved` (or no review required). A closed/merged PR never appears (`state=opened` only). Sort inside a band: newest activity first.
- Row cells: spine+glyph (`✎` changes requested, `✗` failing, `⚠` conflicts, `●` running, `✓` ready, `·` draft), `#iid`, checks `✓ 6/6 | ✗ 1/6 | ● 3/6 | · —` (passed/total, coloured by the worst state), review `✎ changes | ✓ approved | · review | · draft`, age (`relAge`), branch, title, `↳ <run>` (faint, tungsten on the selected row) when `run_id` is set. The selected row's second line: the latest review's author + state + age, actionable/nitpick counts when the review body carries them, `no conflicts with main` / `conflicts with main`.
- `ci` bands: **RUNNING** (`queued`/`in_progress`/`waiting`), **FAILED** (amber eyebrow: `failure`/`timed_out`/`action_required`), **RECENT** (everything else, newest first, capped). Row cells: spine+glyph, `<workflow> #<run_number>`, event, branch (for a PR-triggered run the `refs/pull/N/head` ref is shown as GitHub shows it), elapsed (`shortDuration`), commit/display title, and a right cell: `▰▰▱▱▱ 2/5` jobs done/total for a running run (the milestone micro-bar glyphs, tungsten), `✗ <failed job>` for a failed one, age for the rest. The selected row's second line: `↳ PR #N ·` then each job with its glyph and elapsed.
- The PR view: header line 1 `‹ pulls  PR #iid · head → base` + right-aligned rollup (`● N pending · ✓ P/T`, or `✓ T/T · ✎ changes requested`, or `✗ F failing`) + `● live · 5s`; line 2 title (untrusted, clipped) `· author · run <id> · age · +adds −dels`. CHECKS sorted failing → pending → passed → skipped, each `▌glyph name  description  elapsed`, the selected check's URL faint beneath it (`↗ https://…`). REVIEWS: one line per reviewer, latest state each. MERGE: conflicts, required checks, blocking reason (`✎ blocked: changes requested`, `✗ blocked: <check> failing (required)`, `● waiting on N checks`), with `admin override` faint when the viewer could admin-merge (informational only; uzi never merges).
- The CI run view: header `‹ ci  <workflow> #<run_number> · event · branch · sha7` + rollup + `● live · 5s`; JOBS with steps indented under the selected job (`✓ step · 12s`, the failing step in `alarm`), `l` on a failed job opens the log tail (M7).

### D4 — Data path: on-demand forge reads behind the api, short server-side memo, conditional requests, one limiter

- Three read routes (M2), owner-scoped, in the `RequireUser` repo group, each behind `forgeLimiter.PerUserMiddleware`:
  - `GET /api/repos/{id}/pulls` → `[]PullDTO`
  - `GET /api/repos/{id}/pulls/{iid}` → `PullDetailDTO` (the PR + `checks[]`, `reviews[]`, `merge{}`)
  - `GET /api/repos/{id}/ci/runs?limit=` → `[]CIRunDTO`; `GET /api/repos/{id}/ci/runs/{run_id}` → `CIRunDetailDTO` (jobs + steps)
  - M7 adds `GET /api/repos/{id}/ci/jobs/{job_id}/log?tail=` (bounded, failed jobs only).
- **Server-side memo with a 5 s TTL and singleflight** (new small package, e.g. `api/internal/forgememo`, built on `x/sync/singleflight`, importing no forge SDK: depguard's `forge-sdk-isolation` rule), so N terminals polling one PR cost one forge round-trip per 5 s. **Every key is tenant-scoped**: the prefix is `(connection_id, sha256(token_ciphertext), repo_id)`, then `route name` and the normalised query (the list route: sorted query params; a drill-in: the iid / run id). An `iid` is a per-repo number (PR #5 exists on every repo), so no memo key may start below the repo. The ciphertext hash in the prefix means a PAT rotated in place misses naturally; a disabled repo is refused by the route (below) before the memo is consulted; a deleted connection cascades its repos, so its routes 404. **Error responses are never memoised** (singleflight still collapses concurrent callers), so a forge blip lasts one request, not one TTL. Process-local, bounded (LRU of 256 entries, entries larger than 512 KiB are not cached, with an eviction test), no table: forge is the source of truth, uzi never persists this. **This memo is the primary load-bearing layer**; the ETag layer below is second-order (it only helps across TTL boundaries) and its tests are scoped to that.
- **The new routes serve enabled repos only** (`repoForRequest` does not gate on `enabled`; these handlers do, answering 404 for a disabled repo the way a disabled repo has no board).
- **Per-PR enrichment is memoised on a change key, not a clock, under the same tenant prefix.** GitHub's PR list omits mergeability, additions/deletions and commits (SDK table), and no forge puts the review decision on a list row, so the list route fills those from per-PR calls memoised as: reviews per `(prefix, iid, updated_at)` (a review moves `updated_at`, so an unchanged PR costs zero calls), mergeability per `(prefix, iid, head_sha)` with a 60 s TTL (it also changes when the base moves), capped at the first 30 open PRs (the rest read `Conflicts = null`, drawn without `⚠`). The fan-out runs through a pool of 4; an `AbuseRateLimitError` aborts the remaining fan-out and the request answers `429`. GitLab and Forgejo carry conflicts on the list row and need only the approvals/reviews call.
- **An outbound budget protects the poller.** `forgeLimiter` caps inbound requests per user, not forge spend, and the poller's board sync rides the same PAT, so an interactive reader must never be able to starve it. Two shedding rules, both answering `429` + `Retry-After` *before* any forge call: (1) on GitHub, every response's `Rate.Remaining`/`Reset` is recorded per connection (process-local) and interactive reads are shed while `Remaining < max(500, 10 % of Limit)`, reserving that headroom for the poller and the worker lanes; (2) on every forge, a per-connection token bucket for interactive reads, `FORGE_INTERACTIVE_RATE_MAX` (default 120 calls/min, env, documented in `docs/configuration.md`), counted at the forge-call site so enrichment fan-out is charged too. The TUI renders both as `~ rate-limited · retry in Ns`; `uzi … --watch` backs off per `Retry-After`, keeps watching, and prints one stderr line.
- **Conditional requests on GitHub are an `http.RoundTripper`, not per-call code**: go-github has no ETag API and surfaces a `304` as an error (SDK table). The GitHub driver's client is wrapped with a process-wide ETag cache that sends `If-None-Match` on GET and replays the cached body as a `200` on `304`. **Key = `(sha256(token), URL)`**, derived inside `forge.New`, which already receives the plaintext token; neither `forge.New(type, baseURL, token, timeout)` nor `ForgeForConnection(forgeType, baseURL, tokenCiphertext)` carries a connection id, and threading one through their ~40 call sites is out of scope. Bounded: LRU of 256 entries, bodies over 512 KiB are not cached, only `GET`, only for the new read paths (an allowlist of URL prefixes: `/pulls`, `/commits/*/check-runs`, `/commits/*/status`, `/actions/runs`, `/actions/runs/*/jobs`); every other request passes through untouched so the poller and worker lanes see no behaviour change. GitHub's documented rule ("Best practices for using the REST API", conditional requests): a `304` does not count against the primary rate limit. The driver is rebuilt per request by `ForgeForConnection`, which is why the cache is process-wide, not held on the driver.
- The TUI polls a list every 10 s and a drill-in every 5 s, on the board's tick-chain pattern with its backoff, generation and in-flight guards. A `RateLimitError` / `AbuseRateLimitError` from the forge is mapped by the handler to `429` with `Retry-After` (from `Rate.Reset` / `RetryAfter`); the TUI backs off to that and draws `~ rate-limited · retry in Ns` in the header instead of an error line. The poller keeps using the same PAT, so this is a shared budget: a classic PAT has 5 000 requests/h. **Bounds, stated so the worker can test them**: a cold `pulls` list on GitHub costs at most `1 + 2 × min(open PRs, 30)` = 61 calls (list + reviews + mergeability per PR, fanned out through a bounded pool of 4 so a burst never trips GitHub's secondary limit); a warm one costs 1 (+ enrichment only for PRs whose change key moved); a drill-in costs at most 4 per TTL window; a `ci` list costs `1 + running rows`. Steady state for one terminal on one PR is therefore ~48/h list + ~720/h drill-in worst case, most of them `304`s. Tests pin: two concurrent detail requests inside one TTL window issue one forge call; an unchanged PR issues no reviews call; a cold list on 40 open PRs issues 61 calls, not 81.
- Each PR row carries `run_id` when a run on that repo has `mr_iid = <iid>` (the `(repo_id, mr_iid)` `:many` query, newest run wins), so `↳ run` and `w rework` need no second lookup.

### D5 — Additive `Forge` read methods and one field, three drivers + `BaseFake`

Every interface addition costs four sites (`gitlab.go`, `forgejo.go`, `github.go`, `forgetest.BaseFake`), so the surface is kept minimal.

**As implemented (run 1):** the merge-request read is a cheap list + a per-iid detail rather than one bundled call — `ListMergeRequestRefs` (one list call → `{IID, HeadSHA, UpdatedAt}`, no per-PR enrichment) plus `GetMergeRequestSummary(iid)` (the full `MergeRequestSummary`, now incl. `State`; returns the `ErrMergeRequestNotFound` sentinel on a 404). This split lets D4's memo change-key each PR's enrichment on its `updated_at` and lets the PR drill-in fetch one PR by iid without scanning the list (fixing a top-30-window 404). A further read, `ListMergeRequestReviews(iid)` → `[]Review`, feeds the PR view's per-reviewer list (GitLab returns empty — no free-tier review stream). With `ListChecks`, `ListWorkflowRuns` and the `Job` field additions the interface gained **five new read methods + one field** (`ListMergeRequestRefs`, `GetMergeRequestSummary`, `ListMergeRequestReviews`, `ListChecks`, `ListWorkflowRuns`), not the three the draft anticipated.

| method | GitHub | GitLab | Forgejo |
|---|---|---|---|
| `ListMergeRequestRefs(ctx, projectID, opts{State: opened, Limit})` → `[]MergeRequestRef{IID, HeadSHA, UpdatedAt}` **and** `GetMergeRequestSummary(ctx, projectID, iid)` → `MergeRequestSummary{IID, Title, Author, SourceBranch, TargetBranch, HeadSHA, Draft, Conflicts *bool, ReviewDecision, State, WebURL, Additions, Deletions, Commits, CreatedAt, UpdatedAt}`; plus `ListMergeRequestReviews(ctx, projectID, iid)` → `[]Review` | `PullRequests.List` (refs) + per-iid `PullRequests.Get` (summary: `Conflicts`/`Additions`/`Deletions`/`Commits`, memoised per `(iid, updated_at)`, D4) + `ListReviews` (decision + reviews, D6) | `ListProjectMergeRequests` (refs) + `GetMergeRequest` (summary; `HasConflicts`, `DetailedMergeStatus`, `Draft`, `SHA`, `BlockingDiscussionsResolved`) + `MergeRequestApprovals.GetConfiguration` (`approved`; 403/404 → `none`); no per-reviewer stream (reviews empty) | `ListRepoPullRequests` (refs) + `GetPullRequest` (summary; `Mergeable`, `Draft`, `Head.Sha`, `Additions`, `Deletions`) + `rawGetLimited` on `/pulls/{index}/reviews` (decision + reviews) |
| `ListChecks(ctx, projectID, sha)` → `[]Check{Name, Status (queued/in_progress/completed), Conclusion (success/failure/neutral/cancelled/skipped/timed_out/action_required/""), Description, WebURL, StartedAt, CompletedAt, Source (app slug or "status")}` | `ListCheckRunsForRef` **plus** `GetCombinedStatus` (both, deduped by name; CodeRabbit and other apps report through either) | jobs of the MR's latest pipeline (`ListMergeRequestPipelines` max-by-id + `ListPipelineJobs`) plus `GetCommitStatuses` for external statuses | `ListStatuses` for the sha plus the Actions jobs of the runs with that `HeadSHA` |
| `ListWorkflowRuns(ctx, projectID, opts{Limit, Branch, Event, Status})` → `[]WorkflowRun{ID, Name, Number, Event, Branch, SHA, Status, Conclusion, Title, Actor, WebURL, CreatedAt, UpdatedAt, StartedAt, JobsDone, JobsTotal}` | `ListRepositoryWorkflowRuns` (newest first, one page, `PerPage` ≤ 100) | `ListProjectPipelines` (`OrderBy: id, Sort: desc`; a pipeline is a "run": `Name` = pipeline `Name` or the ref, `Event` = `Source`, `Number` = `IID`) | `ListRepoActionRuns` (already used; the v16 floor is enforced at connect) |
| `Job` gains `Steps []Step{Name, Status, Conclusion, Number, StartedAt, CompletedAt}` and `StartedAt/FinishedAt` | `WorkflowJob.Steps` | jobs only (`Steps` empty); `Duration float64` seconds → `FinishedAt - StartedAt` | jobs only (`Steps` empty) |

Every forge answers `ListWorkflowRuns` on the reference instance's supported versions, so the "not available on this forge version" empty state is reached only through `ErrForgeVersionUnsupported`, kept as the honest degrade path rather than a guess. `JobsDone/JobsTotal` on a list row is best-effort: no forge's runs list includes jobs, so the list route fills it only for RUNNING rows (one jobs call each, memoised on `(run id, updated_at)`), never for the whole page.

### D6 — Review decision is derived, not trusted from one field

GitHub's REST has no `reviewDecision`; derive it the way GitHub's GraphQL `reviewDecision` is defined: take the latest non-`COMMENTED`, non-`DISMISSED` review per reviewer (excluding the PR author); any `CHANGES_REQUESTED` ⇒ `changes_requested`; else any `APPROVED` ⇒ `approved`; else if `RequestedReviewers` or `RequestedTeams` is non-empty (a team-only review request counts too, and GitHub's GET pull request populates `requested_teams`) ⇒ `review_required`; else `none`. GitLab: `changes_requested` := `BlockingDiscussionsResolved == false` (free-tier, on the list row; this deliberately conflates "an unresolved blocking thread" with "a reviewer asked for changes", the closest free-tier signal, and the enum's doc comment says so, so an MR with one open thread banding as NEEDS YOU is by design, not a bug); `approved` := `GetConfiguration().Approved` when the instance answers (approvals are Premium; 403/404 → skip); else `review_required` when `Reviewers` is non-empty; else `none`. Forgejo: the same latest-per-reviewer fold over `GET /pulls/{index}/reviews` (states `APPROVED` / `REQUEST_CHANGES` / `COMMENT`). The decision is a closed enum (`changes_requested | approved | review_required | none`) implemented as one pure function with table tests; review bodies are never interpreted, only counted (the "3 actionable, 2 nitpicks" second line comes from counting the latest review's inline comments by the reviewer, not from parsing prose).

### D7 — Untrusted text and links

Every forge-authored string in the new DTOs (titles, branch names, logins, check names and descriptions, workflow and job and step names, commit titles, merge reasons, **and every URL field**) is drawn through `m.renderer.Plain`, registered in `d7UntrustedFields` under both its apitypes name and any internal copy, and exercised **individually** by the hostile-value render test (the AST guard misses indirection, so the render test is the real check; check descriptions are additionally clipped to one cell line). **The link sanitizer is the existing `oscLink` (`api/cmd/uzi/tui_osc.go`), which strips control / Cf / ESC / BEL runes from the OSC-8 target**: a URL that parses as `https` can still carry a BEL or newline into the `\x1b]8;;…` envelope, so the scheme check is a filter on top of `oscLink`, never a substitute. Rule: a forge URL is emitted only through `oscLink`, only when it parses as `https`, and only when `linksEnabled()`; the faint `↗ URL` line under the selected row goes through `renderer.Plain`. Log tails (M7) go through the ci_fix snapshot scrubber (`api/internal/workersvc/ci_fix_snapshot.go`, token shapes + auth header lines) and then `sanitizeTTY` before rendering.

### D8 — Colour is never the only carrier

Under `colorprofile.Ascii` / `NO_COLOR` the spine fill and tints vanish; every state is also its glyph and, in the header rollups, a word. The `▰▱` jobs bar keeps its `done/total` text. The Ascii-profile test asserts the glyph, the word and the `done/total` text are **present** in the frame, not merely that SGR escapes are absent (an "SGR-absent" check passes a colour-only-then-blank regression).

### D13 — Filter mode and legends on the new screens

`m.filtering()` (`tui.go:1000`) is `view == viewBoard && board.filtering`; it widens to the new screens' own filter state, or typing `q` / `?` into a `pulls` filter quits / opens help. A seam test types `q` into the `pulls` filter. Legend keys appear in the milestone that binds them: `w rework` / `f fix ci` on both the list row (acting on the row's linked run / ref) and the PR view land in M5; `l log` lands in M7, so a scoped-off M7 leaves no `l` in any legend. Both list screens share one legend shape (`enter/→ · ↳ run · / filter · R repo · tab · r refresh · ? keys · q quit`), clamped to width like the board's footer.

### D9 — No shell-out, no browser key

The earlier mock's `o browser` is dropped: the TUI never spawns `open`/`xdg-open`. The id and name cells are OSC-8 hyperlinks (terminal-handled, as the issue link is today), and the selected row shows its URL faint so a user on a terminal without hyperlink support can copy it.

### D10 — CLI twins ship with the routes

`uzi pr list [--repo <id>]`, `uzi pr checks <iid> [--repo <id>] [--watch]`, `uzi ci list [--repo <id>] [--limit n]`, `uzi ci jobs <run-id> [--repo <id>]`, all with `--json` (top-level arrays for lists, one object for a detail; `--watch` re-prints on the same 5 s cadence and exits 0 when no check is pending). The noun in the human table follows `mrAbbrev` (`MR` on GitLab). `--repo` defaults to the single enabled repo, and errors with exit 2 naming the choices when there are several.

### D11 — Two runs, split after M3

M1–M3 are backend + CLI and M4–M8 are the TUI + docs; M4a depends only on M2a. Ten milestones over four seams is too much for one gated run, so the plan is **two runs on this issue**: run 1 implements **M1, M2a, M2b, M3** (independently shippable, CLI-verifiable) and is scoped there at the gate (`uzi run scope --through 4`, counting the frozen milestones) if the lead plans further; after its MR merges the maintainer ticks those four in this file and starts run 2 for **M4a–M8**. M7 is optional and last by design.

### D12 — `POST /api/repos/{id}/ci-fix-runs` moves to the Bearer-reachable group

The TUI's `f fix ci` needs the existing ci-fix trigger, which is mounted cookie-only (`routes_repos.go:146`, `RequireAuth`). It moves to the `RequireUser` sub-group with `forgeLimiter.PerUserMiddleware` applied after `RequireUser` (the ordering rule at `:28-30`), and the pair is renamed in `route_limiter_mounts_test.go`. This is the same posture as `POST /api/runs/{id}/rework` and `POST /api/repos/{id}/runs`, both already Bearer-reachable: all three queue a run that spends the caller's own Anthropic token on the caller's own repo/run, mint nothing and reveal nothing, and are owner-scoped; the later branch/MR push uses the worker PAT, so the `uzc_` token performs no credential write (the no-token-write-from-CLI rule holds). Precondition, verified: `CreateCIFixRun` / `BuildFailureSnapshot` have no `IsAdmin`-dependent branch (the reason `PatchRepo` / `SetRepoEnabled` stay cookie-only does not apply). Bearer routes are not CSRF-attackable, so no CSRF regression. A router-level auth test covers Bearer accepted / cookie accepted / anonymous 401 / foreign repo 404. `uzi ci fix <ref>` is the CLI twin (M3).

## Milestones

Each milestone's acceptance is its own behaviour plus a green `task gate:api` (run once to a log, then read it: Run economy in `CLAUDE.md`); M2a additionally `task gate:web` for the `apiTypes.ts` half of the DTO contract, M7 additionally `task scan:secrets`. Deterministic tests only; a screenshot never gates. Ten milestones, split across two runs per D11.

- [x] **M1 — Forge layer: `ListMergeRequests`, `ListChecks`, `ListWorkflowRuns`, `Job.Steps`, `pipelinestatus.Tone` (D5, D6).** Neutral types + the three methods on the interface, implemented in `github.go`, `gitlab.go`, `forgejo.go`, stubbed loud in `forgetest.BaseFake` (the 24/2 split assertion in `basefake_test.go:107` becomes 27/2); the review-decision fold as one pure function with table tests covering all three forges' inputs; the five-tone `Tone(status)` classifier in `pipelinestatus` with `TestMirrorsWebPipelineBadge` widened to all five tones by **reading `web/src/lib/pipelineBadge.ts` from disk** (as it does today for the two sets; a copied literal would drift silently); per-driver tests on canned JSON via `newMockGitHub` / `newMockGitLab` / `newMockForgejo`, including: a PR whose CodeRabbit result arrives as a commit status and another where it is a check-run (each appears once), a GitHub `RateLimitError` and `AbuseRateLimitError` mapped to a typed rate-limit error carrying the reset, GitLab `HasConflicts` / `DetailedMergeStatus` / `BlockingDiscussionsResolved` mapping and a 403 on approvals folding to `none`, Forgejo reviews via the raw helper, and `ErrForgeVersionUnsupported` passthrough. Errors pass the PAT-scrubbing redactor like every other forge call.
- [x] **M2a — DTOs, routes, auth, ci-fix route move, store query (D4, D12).** `PullDTO`, `PullDetailDTO`, `CheckDTO`, `ReviewDTO`, `MergeStateDTO`, `CIRunDTO`, `CIRunDetailDTO`, `CIJobDTO`, `CIStepDTO` with the three contract registrations each (Go table, recorded fixtures, TS `dtos` row + `apiTypes.ts`); the three read routes in the `RequireUser` repo sub-group with `forgeLimiter.PerUserMiddleware` after `RequireUser`, enabled-repo gate, added to `route_limiter_mounts_test.go`'s table, with a router-level auth test (Bearer `uzc_` accepted, cookie accepted, anonymous 401, another user's repo 404, disabled repo 404); `POST /{id}/ci-fix-runs` moved to that sub-group (D12) with the same auth test; the `(repo_id, mr_iid)` store query with a live-DB test (package `store` or `handler`, both already in the sweep lists). Handlers call the forge directly in this milestone (no memo yet); tests through `BaseFake` overrides. Gate: `task gate:api` + `task gate:web`.
- [x] **M2b — `forgememo`, enrichment memos, ETag transport, outbound budget, 429 (D4).** The `forgememo` package (tenant-prefixed keys, 5 s TTL, singleflight, LRU 256 / 512 KiB with an **eviction test**, errors never memoised) wired under the three routes; the change-key enrichment memos with the pool of 4; the ETag `RoundTripper` **inside `api/internal/forge`** (`github_etag.go`, net/http only, keyed `(sha256(token), URL)`, URL-prefix allowlist) with tests that the httptest mock **asserts the inbound `If-None-Match` equals the ETag it served** before answering `304`, that the `304` replays the cached body, and that two concurrent requests in one TTL window issue one forge call; the per-connection outbound budget (GitHub `Rate.Remaining` reserve + `FORGE_INTERACTIVE_RATE_MAX` bucket) shedding with `429` + `Retry-After` **before** the forge call, with a test that a shed request makes zero forge calls; forge `RateLimitError` / `AbuseRateLimitError` → `429` + `Retry-After`; the call-count bounds from D4 pinned (cold list on 40 PRs = 61 calls, unchanged PR = 0 enrichment calls).
- [x] **M3 — CLI (D10, D12).** `uzicli.Client` gains `ListPulls`, `GetPull`, `ListCIRuns`, `GetCIRun`, `CreateCIFixRun` (interface + `HTTPClient` + `FakeClient` field + fake method, four sites each); `uzi pr list|checks`, `uzi ci list|jobs|fix`, tables + `--json`; `--watch` with an **injectable cadence** (a package var like `blinkInterval`, shrunk in tests) and a deterministic termination test (fake feeds pending → settled, exit 0) plus a 429-mid-watch test (backs off per `Retry-After`, continues, one stderr line); exit codes per the skill's table (429 → exit 6 with the retry hint). `docs/cli.md` command block and sections, and the uzi-cli skill source (`api/internal/uzicli/skill/SKILL.md`), list the new verbs; **`task docs:sync` is run in this milestone and the mirror committed**, or `TestEmbeddedDocsMatchSource` reddens `gate:api`.

> **Run 1 shipped (M1–M3), notes where reality diverged from the draft above** — the merge-request read landed as `ListMergeRequestRefs` + per-iid `GetMergeRequestSummary` (+ `ListMergeRequestReviews` for the PR view), not one bundled `ListMergeRequests`, so the per-PR enrichment can be change-key memoised and the drill-in fetches one PR by iid (see the updated D5); `forgetest.BaseFake` ends at 29 action methods. `TestMirrorsWebPipelineBadge` was rewritten to **read `pipelineBadge.ts` from disk** (it was a hand-maintained literal before, not "as it does today"). The review DTO is `PullReviewDTO` (the `ReviewDTO` name was already taken by the run-judge wire type). `FORGE_INTERACTIVE_RATE_MAX` is documented in `docs/configuration.md` already (moved up from M8). D4's outbound budget shipped in full: the per-connection token bucket **and** the GitHub `Rate.Remaining` reserve (interactive reads shed with 429 + `Retry-After` before the forge call; the poller lane is never shed). M4a–M8 (the TUI, plus the `specs/ai.md`/`ARCHITECTURE.md` finalize) remain for run 2; this file stays in `prds/`.

- [x] **M4a — TUI `pulls` list screen (D1, D2, D3, D7, D8, D13).** `viewPulls`, the wordmark tab strip, `tab`/`1`/`2`/`3`, `R` repo cycle with the default-repo rule, bands, rows, the selected-row second line, `/` filter with `m.filtering()` widened, the 10 s poll on a `pullsState` copy of the board's tick chain, rate-limit header state, empty states ("no open PRs", "not synced yet"). Seam tests: bands and glyph/word per state; poll-guard invariants via `FakeClient` call counts (the `ListRunsCalls` precedent, `fake.go:58`: a tick while a request is in flight issues no second call; a stale reply is dropped; backoff grows on error); the Ascii-profile test asserting glyph + word presence; the per-field hostile-value render test for every new untrusted field; typing `q` into the filter does not quit. Two uxlab scenes (populated + empty).
- [x] **M4b — TUI `ci` list screen (same decisions).** `viewCI` on the same skeleton: bands, rows with the jobs micro-bar and the failed-job cell, the selected-row second line, filter, poll, empty states ("CI not available on this forge version", "no runs yet"). Same test set as M4a (call counts, Ascii presence incl. `done/total`, hostile fields), two uxlab scenes.
- [x] **M5 — TUI PR view + list-row actions (D3, D4, D9, D13).** Drill-in with CHECKS / REVIEWS / MERGE, 5 s live re-poll with the `● live · 5s` header state and `re-polled Ns ago`, `↑↓` over checks with the selected check's `↗ URL` line (through `renderer.Plain`; links only via `oscLink`), `u` (`↳ run`) → run view when `run_id` is set, `w rework` (existing rework endpoint; shown only when the PR has a completed linked run and an open MR; the server's 409 reason drawn inline), `f fix ci` (existing ci-fix endpoint; the 4xx reason drawn inline), the same `u`/`w`/`f` bound on the `pulls` list row and added to its legend. Run view gains `m` → PR view when `MrIID` is set. Seam tests for every header rollup state (pending / all green / failing / changes requested), the key gating, call-count poll guards, Ascii presence; uxlab scenes: live, settled-changes-requested, failing.
- [x] **M6 — TUI CI run view (D3).** Jobs + steps drill-in, `↑↓` over jobs expands the selected job's steps, failing step in `alarm`, 5 s re-poll, `f fix ci` where the run's ref is a watched failed ref. Seam tests (rollup states, call-count guards, Ascii presence, hostile step/job names) + one uxlab scene.
- [ ] **M7 — Failed-job log tail (optional, last; D7).** `GET /api/repos/{id}/ci/jobs/{job_id}/log?tail=`: `?tail=` is **clamped server-side** to `CI_FIX_LOG_TAIL_BYTES` (a client-declared length is not a cap; `JobLogTail` downloads the whole trace up to 16 MiB before tailing), refused for a non-failed job (409) so it never becomes a live-log poller, and the tail is memoised per `(prefix, job id)` for 60 s (a finished job's log is immutable) so the amplification is bounded; reuses `JobLogTail` + the snapshot scrubber; the TUI `l` key opens a scrollable pane (transcript-pane mechanics) rendered through `sanitizeTTY`; `l` joins the legends only here. `uzi ci log <job-id>` twin. Tests: scrubber applied (a runtime-assembled token shape never reaches the frame, per `.claude/rules/prds.md`), non-failed job refused, clamp honoured, memo hit issues no second download. Gate line adds `task scan:secrets`. If budget is short, `run scope --through 6`.
- [x] **M8 — Docs, parity, finalize.** `docs/cli.md` "Watching runs live" gains the two screens, both drill-ins and the keybinding rows; `docs/board.md` cross-links; `docs/configuration.md` documents `FORGE_INTERACTIVE_RATE_MAX`; `task docs:sync` run and the mirror committed (`TestEmbeddedDocsMatchSource`); `specs/ai.md` section for this PRD; `ARCHITECTURE.md` one line under Forge integration ("read-through forge views: pulls / checks / CI runs, memoised, never persisted"). Finalize invariants: zero `.github/workflows/**` entries in `git diff --name-only <base>..HEAD`; sketch registry unchanged.

> **Run 2 shipped (M4a, M4b, M5, M6, M8); M7 deferred.** The two list screens, both drill-ins, and the docs/`specs/ai.md`/`ARCHITECTURE.md` finalize landed and are reviewed; zero `.github/workflows/**` in the branch diff. **M7 (the failed-job log tail — the `l` key, a log pane, `uzi ci log`, and the `GET .../ci/jobs/{id}/log` route) was scoped off as a follow-up** (D11: M7 is optional and last, and the feature is complete and coherent without it): its server-side non-failed-job guard (409) needs a job-status read the `Forge` interface does not expose from a bare job id, so it wants either a new `GetJob`-by-id method (four sites) or a route-shape change to carry `run_id` — a design decision better made in its own issue. This file therefore **stays in `prds/`** until M7 lands. Where Run 2 diverged from the drafts above: the `pulls` list shows a neutral `·  —` checks placeholder (the list route carries no per-check array — checks are the PR drill-in's job) and the `ci` list shows the run age, not a `✗ <failed job>` cell or a per-job second line (the list DTO carries neither — both live in the CI run drill-in), each an honest reframing forced by Run 1's list-DTO shape; each drill-in carries a monotonic per-open **session guard** (`prGen` / `ciRunGen`, mirroring the run view's `detailGen`) so a stale reply from a previously-open PR/run can never apply to — or misfire an action against — a newly-open one; and the shared repo scope **self-heals** a transient `ListRepos` failure on the poll tick rather than leaving the forge screens dead for the session.

## Risks

- **R1 — Rate limit shared with the poller.** Mitigated by D4 (5 s memo + singleflight as the primary layer, change-key enrichment, the bounded fan-out pool, ETag/304 as the second-order layer, per-user limiter, 429 → backoff and an honest header state). The reference instance's poller already runs on the same PAT; the cold-list and steady-state bounds are stated in D4 with the tests that pin them.
- **R2 — Forge parity gaps.** GitLab has no `changes_requested`, approvals are Premium, and pipelines are not workflows; Forgejo's Actions runs list may not exist on every version. Each gap degrades to an explicit neutral value (`none`, `·`, an empty-state sentence), never a guess; drivers return `ErrForgeVersionUnsupported` where the endpoint is absent, and the handler maps it to a typed `unsupported` field the TUI renders as a sentence.
- **R3 — Untrusted text with a new width.** Check names and descriptions are long and adversarial (a CI job name is repo-authored). D7 + clipping + the hostile-value test cover every new field; a new field drawn raw passes every clean fixture, so the guard list is part of the milestone, not an afterthought.
- **R4 — GitHub's runs list is paged and large (2 500+ on the reference repo).** The route reads one page (default 30, cap 100) newest-first; the TUI never asks for more than it can show; filters are server-side query params, not a client scan.
- **R5 — Budget.** Eight milestones across four seams. D11 names the split point; the TUI half can be scoped off and shipped as a follow-up run without leaving anything broken (routes + CLI are independently useful).
- **R6 — `f fix ci` on an unwatched ref, or with the watch off.** The existing endpoint requires the cache to show `failed` for the ref, and the cache covers only watched refs, so `f` on an old failed run 4xxs; on an instance with `CI_WATCH_MAX_REFS=0` the cache is empty and every `f` 4xxs while the on-demand `pulls` / `ci` views keep working. The TUI draws the server's reason inline; widening the cache is out of scope.
- **R7 — D12 widens an existing route's reachability.** Moving `ci-fix-runs` from cookie-only to Bearer-reachable is a deliberate, reviewed change with the `rework` precedent; it stays owner-scoped and rate-limited. If the reviewer at the plan gate disagrees, `f fix ci` degrades to a hint ("Fix CI from the web") and M3 drops `uzi ci fix`; nothing else in the PRD depends on it.

## Validation strategy

- **Deterministic (CI)**: per-driver httptest tests on canned JSON (M1); handler tests through `BaseFake` (M2) including the router-level auth test, memo/singleflight/ETag tests, and a live-DB test for the `(repo_id, mr_iid)` query (`./e2e/run-store-it.sh` lists `handler` and `store`); `FakeClient`-driven CLI tests (M3); TUI seam tests with SGR + substring assertions and Ascii fallback (M4–M7); `fixtures/api-contract` byte checks on both sides. Gate line: `task gate:api` per milestone, `task gate:web` on M2/M3; M7 adds `task scan:secrets` to its gate line.
- **Visual (review, not a gate)**: uxlab scenes rendered light + dark and reviewed by the `tui-ux` agent.
- **Live (maintainer, post-merge, dev-cluster)**: `uzi tui` against the reference instance with the GitHub repo (`vtmocanu/uzi`) and the GitLab repo (`ai-team/dobby`): watch a PR's checks land, confirm the CodeRabbit line and the review decision match `gh pr checks` / the web UI, confirm a release-tag workflow run appears under `ci`.

## Out of scope

- A web page for pulls / CI (the routes are web-usable; a follow-up PRD can add the page).
- Any forge write from these views beyond the two existing run actions: no approve, no merge, no re-run, no comment.
- Live log streaming of a running job (no REST API for it on GitHub); M7 is a bounded tail of a finished, failed job.
- Widening the `pipeline_statuses` watch set or the PAT scopes; extending the worker's in-run forge read tools (`docs/forge-read-tools.md`) with the new methods (natural follow-up).
- `.github/workflows/**` edits (guardrail).

## Decision log

- **D1** Two list screens + two drill-ins as peers of the run view; `tab`/`1-3` on lists, `esc` back; cross-links `u`/`m` via `runs.mr_iid`. (User-confirmed 2026-09-12: "another view".)
- **D2** Per-repo scope with `R` cycle; default = newest run's repo.
- **D3** Board vocabulary reused: three bands, spine + glyph, `▰▱` micro-bar, faint CAPS eyebrows, amber only for needs-you.
- **D4** Read-through forge views: on-demand behind the api, 5 s memo + singleflight + ETag, one per-user limiter, 429 → backoff; nothing persisted. The `pipeline_statuses` cache is not widened.
- **D5** Three additive methods + one field; four sites each; unsupported → `ErrForgeVersionUnsupported`, rendered honestly.
- **D6** Review decision derived by the latest-per-reviewer fold; closed enum.
- **D7** Forge text through `renderer.Plain` + `d7UntrustedFields` + hostile test; links only `https` + `linksEnabled()`; log tails scrubbed then `sanitizeTTY`.
- **D8** Glyph + word under every colour.
- **D9** No shell-out; OSC-8 links + faint URL on the selected row.
- **D10** CLI twins ship with the routes (repo convention: the CLI is a second consumer of the same API).
- **D11** Two runs on this issue, split after M3; M7 optional; the CI tab shows all events incl. `push` to `main` and `v*` tag pushes (user-confirmed 2026-09-12); logs are a bounded failed-job tail, not a live stream (user asked "or better not yet?", answered: phased).
- **D12** `POST /api/repos/{id}/ci-fix-runs` moves to the `RequireUser` sub-group so the TUI and `uzi ci fix` can call it; same posture as the already Bearer-reachable `rework` route. Reversible per R7.
