# PRD #1335 — TUI forge view: `←` goes back in drill-ins, and the CI-run detail header shows the run's identity

**Issue**: #1335
**Priority**: Medium
**Status**: Draft
**Base at authoring**: `e19f93d0b8661e9333b00c111c647da09d4cf963` (line anchors below are as of this commit; re-derive symbol-first, lines drift)

Two independent, small, related defects in the PRD #1255 TUI forge view. Both are internal (no open-web dependency): safe for an offline uzi worker.

## Problem

### P1 — `←` does nothing in the forge drill-ins

The forge view has two list screens that drill into a detail screen:

- Pulls list → PR view: `enter`/`→` opens the selected PR (`tui_pulls.go:399`, `case keyEnter, keyRight`) → `viewPR`.
- CI list → CI-run view: `enter`/`→` opens the selected run's jobs (`tui_ci.go:810`, `case keyEnter, keyRight`) → `viewCIRun`.

In both drill-ins the only way back is `esc`:

- `viewPR`: `prKey` handles `keyEsc` → `m.view = m.prReturn` (`tui_pr.go:275`). `keyLeft` is unhandled.
- `viewCIRun`: `ciRunKey` handles `keyEsc` → `m.view = viewCI` (`tui_cirun.go:207`). `keyLeft` is unhandled.

So `→` opens but `←` is dead — asymmetric and, to a user, reads like a bug. `keyLeft` is currently unbound in all four forge views (`viewPulls`/`viewCI`/`viewPR`/`viewCIRun`); the only forge-view motion keys are the vertical `↑↓`/`jk` cursor (`motionDelta`, `tui_keys.go:61`). Grep confirms `keyLeft` is handled only in `sketch.go` and `tui_detail.go:545` (`case "h", keyLeft` — the run detail's crew-rail focus).

### P2 — the CI-run detail header renders empty

Opening a run from the CI list shows a header that reads `‹ ci  #0 · · ·` on line 1 and `· · -` on line 2 while the JOBS list below is fully populated. Reported on the live dev instance 2026-09-13 on a real GitHub Actions run (12 jobs passed, `validate-api`, etc.) whose header showed no name, `#0`, no event/branch/sha, no title, no actor, and `-` for age.

What the glyphs are (not decorative — empty fields):

- Line 1, `ciRunHeaderLine1` (`tui_cirun.go:450`): `‹ ci  <name> #<number> · <event> · <branch> · <sha7>`. With every scalar zero this renders `‹ ci  #0 · · ·` — `#0` is `Number == 0`, the three `·` are the separators around empty event/branch/sha.
- Line 2, `ciRunHeaderLine2` (`tui_cirun.go:473`): `<title> · <actor> · <age>`. Empty title/actor plus a zero `CreatedAt` (`relAge` → `-`) renders `· · -`.

Root cause, two seams:

1. **Server** — the CI-run detail endpoint builds the DTO with only the ID and jobs and never fills the run-level scalars:
   ```go
   // api/internal/handler/forgeview.go:457
   detail := apitypes.CIRunDetailDTO{CIRunDTO: apitypes.CIRunDTO{ID: runID}, Jobs: []apitypes.CIJobDTO{}}
   ```
   It then fetches jobs (`f.ListPipelineJobs`, `:463`) and sets `detail.Jobs` (`:478`). `Number`, `Event`, `Branch`, `SHA`, `Name`, `Title`, `Actor`, `CreatedAt`, `Status`, `Conclusion`, `WebURL` are left at their zero values. This contradicts the DTO's own contract: `CIRunDetailDTO` is documented as "the CIRunDTO scalars plus the run's jobs" (`api/internal/apitypes/forgeview.go:139`).
2. **Client** — the TUI seeds the header from the selected list row at open (`m.cirun.detail.CIRunDTO = run`, `tui_ci.go:831`, with a comment expecting "the first reply replaces this with the full jobs/steps"), but `ciRunState.apply` wholesale-replaces `detail` on the first reply (`c.detail = msg.detail`, `tui_cirun.go:144`). Because the reply's header is empty, the seeded identity is clobbered the instant the first `GetCIRun` fetch lands.

The chosen fix is server-side (see D1): make the endpoint return a self-consistent DTO so the header is right for **any** consumer, not only when the TUI happens to seed it — and the client `apply` (`c.detail = msg.detail`) then becomes correct as written, no client change needed. On an error `apply` returns early without touching `c.detail` (`tui_cirun.go:131-139`), so the TUI's seeded header survives a failed refresh regardless.

## Solution

### S1 — bind `←` = back in the two forge drill-ins

Add `case keyLeft` alongside the existing `case keyEsc` in `prKey` (`tui_pr.go:275`) and `ciRunKey` (`tui_cirun.go:207`), sharing the same body (pop back). Scope is deliberate: **do not** add `keyLeft` to `viewDetail` (where `←`/`h` focuses the crew rail, `tui_detail.go:545`) and **do not** make it a global "back". The forge list screens (`viewPulls`/`viewCI`) are out of scope for this PRD — `esc`-to-board there is a broader change; this PRD only makes the drill-in `→`/`←` pair symmetric.

Update the `?` help legends so `←` is advertised: `viewPR` and `viewCIRun` currently list only `esc back / dismiss` (via `common`, `tui_keys.go:83`). Add an explicit `← back` line to each (`helpLines`, `tui_keys.go:116` and `:124`).

### S2 — server fills the CI-run header (best practice)

Add a single-run getter to the `Forge` interface and have the detail endpoint call it, so the returned `CIRunDetailDTO` carries the run's identity scalars for every consumer.

**New interface method** (`api/internal/forge/forge.go`, beside `ListWorkflowRuns` at `:834` and `ListPipelineJobs` at `:791`):

```go
// GetWorkflowRun returns one CI run's header (the neutral WorkflowRun) by its
// forge-native run/pipeline id, for the `ci` drill-in. ErrForgeVersionUnsupported
// on a forge whose Actions/pipelines API is absent (mirrors ListWorkflowRuns).
GetWorkflowRun(ctx context.Context, projectID, runID int64) (WorkflowRun, error)
```

Per the root `CLAUDE.md` forge-layer note, an interface change costs four sites: three drivers plus `forgetest.BaseFake`. All three drivers already have a mapper from their native run type to `WorkflowRun`; each single getter reuses or mirrors it. Confirmed offline against the pinned SDKs (`api/go.mod`):

- **GitHub** (`google/go-github/v91`, `github_forgeview.go`): `g.client.Actions.GetWorkflowRunByID(ctx, slug.owner, slug.repo, runID)` → `*gh.WorkflowRun`, then the existing `toGitHubWorkflowRun` (`:404`). Guard with `shedIfReserved` + `repoSlugFor` exactly as `ListWorkflowRuns` (`:365-371`). (`GetWorkflowRunByID` confirmed at `actions_workflow_runs.go:213`.)
- **GitLab** (`gitlab-org/api/client-go/v2`, `gitlab_forgeview.go`): `g.client.Pipelines.GetPipeline(projectID, runID, gitlab.WithContext(ctx))` → `*gitlab.Pipeline`. The list mapper `toGitLabWorkflowRun` takes `*gitlab.PipelineInfo` (`:373`); the single get returns the richer `*gitlab.Pipeline`, so add a sibling mapper `toGitLabWorkflowRunFromPipeline(*gitlab.Pipeline)`. `Pipeline` fields (confirmed at `pipelines.go`): `ID`, `IID` (→ `Number`), `Ref` (→ `Branch`, and `Name` fallback when `Name == ""`, matching the list), `SHA`, `Status`, `Source` — typed `PipelineSource`, so `Event: string(p.Source)` — `WebURL`, `*CreatedAt`/`*UpdatedAt`/`*StartedAt`, and `User *BasicUser` (so `Actor` can be filled from `p.User.Username` when non-nil, an improvement the list row lacks). (`GetPipeline` confirmed at `pipelines.go:298`.)
- **Forgejo/Gitea** (`code.gitea.io/sdk/gitea@v0.25.1`, `forgejo_forgeview.go`): `c.GetRepoActionRun(slug.owner, slug.repo, runID)` → `*gitea.ActionWorkflowRun`, then the existing `toForgejoWorkflowRun` (`:521`). Map a 404 to `ErrForgeVersionUnsupported` exactly as the list does (`:497-499`). (`GetRepoActionRun` confirmed at the SDK's `action_run.go:172`.)

**BaseFake** (`api/internal/forge/forgetest/basefake.go`, beside `ListWorkflowRuns` at `:197`): add the loud default `GetWorkflowRun` returning `notStubbed("GetWorkflowRun")`, and the matching case in `basefake_test.go` (beside `:114`). The six fakes embedding `BaseFake` inherit the loud default automatically, so nothing else breaks compilation; only a test that exercises the CI-run detail route overrides it.

**Handler** (`api/internal/handler/forgeview.go`, `GetCIRun` around `:437`): before (or alongside) the jobs fetch, fetch the header via the new method, memoized like jobs (`h.memo().Do`, key `forgeMemoPrefix(repo)+"cirun|"+strconv.FormatInt(runID, 10)`, charge budget inside as the jobs closure does at `:460`), and build the response through a **new pure mapper** `ciRunDetailFromRun(run forge.WorkflowRun, jobs []forge.Job) apitypes.CIRunDetailDTO` (sibling of `ciRunDTO` at `forgeview.go:592`) so the population logic is unit-testable offline (see M4 — the handler itself is only reachable via a live-DB test). Keep `ID = runID`. Handle errors symmetrically with the jobs fetch: `ErrForgeVersionUnsupported` → `detail.Unsupported = forgeViewUnsupportedMsg` and return (`:470-473`); any other error → `h.writeForgeError(w, "ci run", err)`. Use the same `loadCtx` (`:456`). No response-shape change: `CIRunDetailDTO` embeds `CIRunDTO`, so the client and `uzicli` deserialize the now-populated scalars for free.

**Rewrite the stale `GetCIRun` docstring** (`forgeview.go:433-436`): it currently says "the run-level scalars the drill-in header shows come from the list row the client drilled in from, so this route fills only id (from the path) and jobs" — false after S2. Restate it as "fills the run header (via GetWorkflowRun) and jobs" (fix-the-doc; it is in the exact file this milestone edits).

**Update the existing live-DB router-auth test** (`forgeview_auth_livedb_test.go`): `fakeForgeView` (`:30`) overrides `ListPipelineJobs`/`ListWorkflowRuns` but not `GetWorkflowRun`; after S2 the `ci_run_detail` subtest (`:103`, asserts 200) would 502 (BaseFake `notStubbed` → `writeForgeError` → `StatusBadGateway`). Add a `GetWorkflowRun` override returning a canned OK `forge.WorkflowRun{ID: 100, Name: "CI", ...}`. This test lives only in the store-it sweep, so `task gate:api`/`task gate` do **not** catch the break — it must be fixed in this PRD or it reddens CI's `test:api-store-it` (see M4/M5).

No client change is required: `uzicli.GetCIRun` already decodes the full `CIRunDetailDTO`, and the TUI `apply` (`c.detail = msg.detail`) is correct once the reply carries the header. The open-time seed (`tui_ci.go:831`) stays; it covers the pre-first-reply window and a failed refresh.

## Decision log

- **D1 — Server-side fix for P2 (best practice), not a client merge.** A client-only fix (in `apply`, keep the seeded header when the reply's header is empty) would work today because the drill-in is only ever entered from a seeded row (`tui_ci.go:810` is the sole entry). It was rejected: it leaves the endpoint returning a half-populated DTO that is correct only by the client's luck — a latent trap for any future consumer (web, a deep link, a contract test) and exactly the kind of "works because of an implicit invariant elsewhere" the repo avoids. The server fix makes the DTO honor its documented contract (`apitypes/forgeview.go:139`) and is self-verifying via a handler test.
- **D2 — A dedicated `GetWorkflowRun`, not reuse of `ListWorkflowRuns`.** Filtering the run out of a `ListWorkflowRuns` page is fragile: the run may be outside the newest-N window, and it spends a list call to answer a point lookup. All three forges expose a cheap single-run GET (verified against the pinned SDKs above), so the interface gets a first-class method.
- **D3 — Header fetch errors are handled symmetrically with the jobs fetch (fatal to the route, except version-unsupported which degrades).** This keeps the endpoint honest (if the forge cannot serve the run, the route says so) and costs the TUI little: `ciRunState.apply` returns early on `msg.err` without touching `c.detail` (`tui_cirun.go:131-139`), so the seeded header stays on screen and `ciRunHeaderNote` renders "could not refresh" (`tui_cirun.go:537`). Two honest tradeoffs, both acceptable: (a) the route now makes **two** forge calls (header + jobs), so it has strictly more failure surface than before and spends **2** interactive-read budget tokens per uncached open instead of 1 — consistent with the per-round-trip budget model (`chargeBudget`; `ListPulls` already spends 1+N); (b) a transient header hiccup on first open now also blanks the JOBS list (previously jobs rendered whenever the endpoint was up), because a symmetric-fatal header error fails the whole route. The seeded header still shows the run's identity through it, and a retry (`r`, or the 5s tick) recovers. Both are preferred over returning a silently half-populated DTO (D1).
- **D4 — `←` scoped to the two drill-ins only.** `viewDetail` already binds `←`/`h` to crew-rail focus (`tui_detail.go:545`); a global back-on-`←` would collide. The forge *list* screens keep `esc`-to-board unchanged — adding `←` there (back to the board) is a defensible follow-up but is a broader navigation change than "make `→`/`←` symmetric in the drill-in", so it is out of scope.
- **D5 — GitLab `Actor` enrichment is a bonus, not a requirement.** The list row leaves `Actor` empty for GitLab; the single `Pipeline` carries `User`, so the drill-in header can show the actor. Fill it when `p.User != nil`; do not fail if absent.
- **D6 — No new DTO fields, so no `fixtures/api-contract/` change.** P2 populates fields that already exist on `CIRunDTO`; the wire shape is unchanged. The three-file DTO rule (`.claude/rules/go.md`) does not apply. The handler test, not a contract fixture, proves population.

## Scope / non-goals

- No change to `viewDetail` navigation, and no global `←`-back binding (D4).
- No `esc`-to-board change on the forge list screens; no new binding there (D4).
- No new DTO fields; no `apiTypes.ts` / `fixtures/api-contract/` change (D6).
- No migration, no `sqlc` change. (The handler comment at `forgeview.go:455` "no per-request DB read" refers to the *forge-load closure* not doing enrichment reads like `ListPulls`'s `newestRunIDForMR`; the route still resolves the repo via a real DB read in `h.forgeViewRepo`→`repoForRequest`→`GetRepoForUser`, which is why the route-level handler test is live-DB — see M4.)
- No `docs/*.md` or `specs/human.md` change owed: the forge view's user-facing behavior is unchanged except that a broken header now renders correctly and `←` now works; grep the base commit before asserting otherwise, and if a `docs/cli.md` or forge-view doc names the CI-run header or the drill-in keys, sync it terse in-branch (then `task docs:sync`).
- No `api/cmd/uzi/` CLI change beyond the TUI (the CLI-parity check in the root `CLAUDE.md`): the forge view is TUI-only; there is no non-TUI CLI surface for it.
- No `.github/workflows/**` change in implementation or validation (`.claude/rules/prds.md`; the worker PAT lacks `workflow` scope).

## Milestones

- [ ] **M1 — Nav regression tests, written first and shown failing on the unfixed key handling.** In `api/cmd/uzi/tui_pr_test.go` and `tui_cirun_test.go`, beside the existing `esc`-back tests, drive the model with a `keyLeft` press:
  - `viewPR` + `keyLeft` → `m.view == m.prReturn` (default `viewPulls`), mirroring the `esc` assertion at `tui_pr.go:275`. Must **fail** on the base commit (`←` is unhandled, view stays `viewPR`).
  - `viewCIRun` + `keyLeft` → `m.view == viewCI`, mirroring `tui_cirun.go:207`. Must **fail** on the base commit.
  - A guard test that `viewDetail` + `keyLeft` still focuses the crew rail (unchanged), pinning D4 so the fix cannot leak a global back-binding. This passes on the base commit and must keep passing.
  - Evidence in the run: the `go test` output for these files red on the unfixed tree, green after M2's nav edit.

- [ ] **M2 — `←` back in the two drill-ins + help legends.** Add `case keyLeft` to `prKey` (`tui_pr.go:275`) and `ciRunKey` (`tui_cirun.go:207`) sharing the `keyEsc` body; add a `← back` line to `helpLines` for `viewPR` (`tui_keys.go:116`) and `viewCIRun` (`:124`). M1 goes green. Re-render the TUI screenshots per `.claude/rules/tui.md` (`cd api/cmd/uzi/uxlab && devbox run build`) only if a legend scene changed; the nav change itself is not visually assertable, so a `tui_render`/`tui_model` seam test (M1) is the gate, not a screenshot.

- [ ] **M3 — `Forge.GetWorkflowRun` on all three drivers + BaseFake.** Add the interface method (`forge.go`), implement it in `github_forgeview.go`, `gitlab_forgeview.go` (with `toGitLabWorkflowRunFromPipeline`), `forgejo_forgeview.go` (404 → `ErrForgeVersionUnsupported`), and add the `BaseFake` loud default + its `basefake_test.go` case. Per-driver unit tests beside the existing `*_forgeview_test.go` `ListWorkflowRuns` tests: assert the returned `WorkflowRun` carries the identity scalars (number/event/branch/sha/title/actor/created-at) from a fake native run, and that Forgejo's 404 maps to `ErrForgeVersionUnsupported`. `sqlc` untouched. Gate: `task gate:api` (carries `-race`).

- [ ] **M4 — Handler fills the CI-run header + regression tests (offline unit test + live-DB route test).** Wire the new method into `GetCIRun` (memoized, budget-charged, errors symmetric with jobs per D3), building the response via the pure mapper `ciRunDetailFromRun` (S2), and rewrite the stale `GetCIRun` docstring (`:433-436`). Two test layers, because the route resolves the repo through a real DB read (`h.forgeViewRepo`), so a route-level handler test is necessarily `*LiveDB`:
  - **Offline unit test (the primary regression gate, runs under `task gate:api`)**: table-test `ciRunDetailFromRun` in `api/internal/handler` (a pure-helper test like `forgeview_test.go`, no DB, no forge) — given a populated `forge.WorkflowRun` + jobs it returns a `CIRunDetailDTO` whose `Number`/`Branch`/`SHA`/`Event`/`Title`/`Actor`/`CreatedAt` are the run's values (not zero) and `ID == runID`. This is the fails-before/passes-after evidence the offline worker can produce; the helper does not exist at the base commit, so frame it as "written with the fix" and prove it exercises the population.
  - **Live-DB route test (store-it sweep)**: add the missing `GetWorkflowRun` override to `fakeForgeView` in `forgeview_auth_livedb_test.go` (blocking, S2) so `TestForgeViewRoutesAuthLiveDB`'s `ci_run_detail` subtest stays 200; and, beside it, assert the `ci_run_detail` response body carries the run header scalars from the fake, plus a version-unsupported case (`GetWorkflowRun` returns `ErrForgeVersionUnsupported`) asserting `Unsupported` set and 200. Run via `./e2e/run-store-it.sh` (needs docker + `postgres:17`); honor the `*LiveDB` list rules (`.claude/rules/go.md`) — this route's test already lives in the `handler` package the sweep enumerates, so no two-list edit is owed.

- [ ] **M5 — Full gate + store-it + docs sync.** `task gate` green (repo + all four components; run once to a log and read the log, per the root `CLAUDE.md` run-economy note). **Also run `./e2e/run-store-it.sh`** — the live-DB route test in M4 (and the `fakeForgeView` fix) is invisible to `task gate`, so a green gate alone does **not** prove P2's route wiring; the store-it sweep is required to prove `ci_run_detail` stays 200 and returns the header (read the tally per the `PASS=0` family rules, `.claude/rules/go.md`). If store-it cannot run in the worker (no docker/`postgres:17`), say so explicitly in the report rather than claiming P2 verified — the offline `ciRunDetailFromRun` unit test still gates the population logic. If any `docs/*.md` was touched, `task docs:sync` and commit the mirror. Confirm no `.github/workflows/**` in the branch diff.

## Validation

- **P1**: the M1 tests are the deterministic gate (`Update→msg` seam). Manual confirmation via `cd api && go run ./cmd/uzi tui --demo`: open a PR and a CI run, press `←`, land back on the list; press `?` in each and see `← back`.
- **P2**: the M4 offline `ciRunDetailFromRun` unit test gates the population logic under `task gate:api`; the M4 live-DB route test (store-it sweep) proves the wired route returns the header and stays 200. Manual confirmation on the live/demo TUI: open a real CI run and see the header populated (name, `#<number>`, event, branch, sha7, title, actor, age) instead of `#0 · · ·` / `· · -`.
- **Risk / mitigation**: the only cross-cutting change is the forge interface (four sites + fakes); the loud `BaseFake` default and `go build` across both modules catch any missed site. `task gate:controller` is unaffected (the controller does not import the forge drivers), but run the full `task gate` anyway (M5).

## Success criteria

- `←` returns from the PR view and the CI-run view to where each was opened; the `?` legends say so.
- The CI-run drill-in header shows the run's identity for every forge, populated by the server, with a regression test that fails on the pre-fix handler.
- `main` untouched; branch + MR only; full gate green.
