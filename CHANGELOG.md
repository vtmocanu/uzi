# Changelog

Notable changes to uzi, loosely following [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions are release git tags (`deploy/chart/Chart.yaml`'s `version`/`appVersion`, Model B) — this
file is not bumped per-commit; `[Unreleased]` collects everything since the last tag.

Format each bullet as a bold title on its own physical line, then the description directly on the
next physical line (NO blank line between them), the description on ONE physical line (no
mid-description newlines) indented two spaces so it stays inside the list item: a release's notes are
this file's `## [X.Y.Z]` section verbatim (`scripts/changelog-section.sh body`), and GitHub renders
single newlines in a release body as hard `<br>` breaks, so the title and its description render on
consecutive lines with nothing between them, while a hard-wrapped description would show as short,
ragged lines and a blank line after the title would open a gap. Keeping the description on one
physical line lets GitHub reflow it to the reader's width. Blank lines between bullets and `###`
subsection headers are unaffected. (Title-line-then-description established 2026-08-22, applied
back across earlier sections too; one-physical-line-per-bullet was the interim rule from 2026-08-21
through `[0.52.0]`.)

## [Unreleased]

### Added

- **Slack DM when a usage-limit-paused run resumes ([#1116](https://github.com/vtmocanu/uzi/issues/1116)).**
  A run parked on an Anthropic usage limit already posts a ⏸️ Paused reply into its Slack DM thread; now the first time it is running again it posts a single ▶️ Resumed reply into the same thread, carrying how long it waited and (from the second pause) the pause count, deduped through the per-run Slack anchor so a redelivered running report never re-posts.

## [0.78.0] - 2026-09-03

### Added

- **Pause every schedule at once, with an optional auto-resume ([#1093](https://github.com/vtmocanu/uzi/issues/1093)).**
  A new user-level kill switch pauses every schedule you own, catalog defaults and your own alike, on every repo, with an optional `until` that auto-resumes on its own with no background job; a recurring schedule due while paused records a benign `schedules_paused` skip and keeps advancing its cadence so nothing replays on resume, a one-time schedule instead waits and fires once the pause lifts, `Run now` still bypasses the switch, and runs already in flight are untouched. It's reachable from the Schedules page (a "Pause all" control, an inline picker, and a paused banner), the CLI (`uzi schedule pause-all --until <when>` / `resume-all` / `pause-status`), and a paused fire's Last fire record.

### Changed

- **Agent roles re-synced to skills ff87616: fact-checker v9, architect v8 ([#1080](https://github.com/vtmocanu/uzi/issues/1080), [#1095](https://github.com/vtmocanu/uzi/pull/1095)).**
  Both `.claude/agents/` and the shipped builtins pick up vtmocanu/skills#41: the fact-checker's defect fold runs in a detached throwaway worktree and its description says so, the architect's write surface is the design-doc directory its tail names and never `specs/`; the vendored role manifest moves to ff87616. `CLAUDE.md`'s run-economy rework rule now names every high-risk class (trust-boundary, data-integrity, auth, untrusted-input), matching the lead builtin.

- **Builtin agent-team role templates re-synced to the upstream skills library ([#1080](https://github.com/vtmocanu/uzi/issues/1080)).**
  The eleven library-derived builtins under `api/internal/agenttmpl/builtins/` were ported to the terser upstream bodies with new version stamps and the library's model choices (`researcher` moves to `sonnet`), keeping uzi's `main`-recipient wording and `fact-checker`'s forge tools; `lead` gains the run-economy rules and the vendored role manifest is refreshed to `b5288fd`.

## [0.77.0] - 2026-09-03

### Added

- **The board, run view and crew rail now show what your crew is doing right now, not just which milestone is "done" ([#1064](https://github.com/vtmocanu/uzi/issues/1064)).**
  A milestone reported in progress gets a live "now" line naming the active lane's role, its task label, its last tool and an age, sourced from the run's newest tool-use frame (never a raw `Bash` command). The web run view, dashboard/runs-list cards, the TUI's crew rail and board second line, and `uzi run get`'s new `NOW` row all read the same server-derived `current_activity` field on the run, so the four surfaces cannot disagree; the web milestone badge also gains a `◐` suffix when a milestone is in progress.
- **The TUI's in-progress milestone cell blinks instead of sitting static ([#1064](https://github.com/vtmocanu/uzi/issues/1064)).**
  The board's micro-bar and the crew rail's milestone row alternate `▰`/`▱` in the wait colour on a half-second tick. A piped or offline render, or `UZI_TUI_NO_BLINK=1` (a reduced-motion opt-out), pins the static `▱` frame instead of blinking.
- **A milestone shows as in progress the moment the worker reports it, instead of only at the next turn boundary ([#1064](https://github.com/vtmocanu/uzi/issues/1064)).**
  The worker now pushes a `running` state report the instant it observes a `report_progress` signal, rather than waiting for the next iteration or checkpoint report to carry it, and emits a feed line per transition ("milestone m2 started — <title>" / "milestone m1 reported complete — <title>"). Wording always says "reported complete", never "done" or "verified" — uzi shows what the worker reported and has not itself checked the work.
- **The Workers page is split into "Your workers" and "Add a worker" tabs, hosted-first ([#1063](https://github.com/vtmocanu/uzi/issues/1063)).**
  The page leads with your running fleet and moves the join/enrol flow onto its own tab, so managing existing workers and adding a new one are no longer crammed into one screen.
- **Scheduled runs can now fix their own MRs and CI ([#1069](https://github.com/vtmocanu/uzi/pull/1069)).**
  Autofix (MR rework and CI autofix) now covers the scheduled `prompt` and `self_improve` run kinds, not just interactive issue runs, so a scheduled run that opens an MR can rework it on review feedback and repair its own red pipeline.
- **A throwaway TUI sketch harness for previewing a new TUI feature before building it ([#1061](https://github.com/vtmocanu/uzi/issues/1061)).**
  `uzi tui --sketch` renders a preview surface on the uxlab harness so a TUI change can be seen and iterated on before it is implemented; it is a development aid and never lands on `main` as product behaviour.

### Changed

- **Forge checkpoints now cover the non-issue run kinds ([#1037](https://github.com/vtmocanu/uzi/pull/1037)).**
  `self_improve`, `chat`, `prompt`, `mr_rework` and `ci_fix` runs now publish forge checkpoints like issue runs do, so a parked or interrupted run of any kind can resume from its last checkpoint instead of losing work.
- **The worker runs a pre-push secret scan at finalize and reports a typed failure origin for GitHub push protection ([#1076](https://github.com/vtmocanu/uzi/pull/1076)).**
  Before pushing a finished branch the worker scans the whole push range for secret-shaped strings, and a GitHub push-protection rejection (GH013) now surfaces as a typed `fail_origin` with the finished work preserved, instead of an opaque push failure.
- **Dependency: bump `gitlab.com/gitlab-org/api/client-go` to v2 ([#1067](https://github.com/vtmocanu/uzi/pull/1067)).**

### Fixed

- **Hosted worker: `agent-browser` starts out of the box on the musl image ([#1082](https://github.com/vtmocanu/uzi/issues/1082)).**
  The shim now picks the native binary by the dynamic loader on disk instead of letting the npm launcher sniff `ldd --version` off the PATH, where a provisioned nix glibc (uzi's own `ruby@4.0.6`) shadowed musl's `ldd` and made it spawn the glibc build; the image build guard repeats the check behind a fake glibc `ldd` and opens `about:blank` through the shim, so a regression reddens the build rather than costing a subagent ~15 tool calls mid-run.
- **Run cost was under-counted on every multi-iteration run, by as much as 3.8x ([#1079](https://github.com/vtmocanu/uzi/issues/1079)).**
  `run_usage` assumed each SDK result frame reported a cumulative session total and kept only the largest one (a 3-iteration run read $77.19 against a true $153.58, a 7-iteration run read $26.70 against a true $100.63); it now keys each row by the SDK `query()` leg that produced it and sums the legs, and every pre-existing run's totals were automatically re-folded from history on first boot after the fix.
- **A branch behind `main` only on `.github/workflows` no longer loses work at checkpoint time ([#1036](https://github.com/vtmocanu/uzi/pull/1036)).**
  The broker PAT lacks the `workflow` scope, so a checkpoint of a branch that is behind on workflow files was skipped and a run that then died lost its work; the worker now synthesizes a wrapper commit whose workflow subtree matches the current default so the unchanged broker can push it, and adoption peels the wrapper back off.
- **Checkpoint adoption unified on the owner anchor ([#1059](https://github.com/vtmocanu/uzi/pull/1059)).**
  A resumed run now adopts its checkpoint tip through a single owner-anchored path, closing the residual work-loss gaps from the earlier checkpoint machinery so a resumed run keeps its lineage instead of breaking it.
- **`self_improve` owner-key reader missed pre-#774 flattened tracking-owner stamps ([#887](https://github.com/vtmocanu/uzi/issues/887)).**
  Across the #774 rollout window the owner-key reader could miss the older flattened tracking-owner stamp form, so a `self_improve` run could fail to recognise its own prior work; it now reads both stamp shapes.

## [0.76.0] - 2026-09-03

### Added

- **Close or reopen an issue straight from the board ([#1051](https://github.com/vtmocanu/uzi/pull/1051)).**
  The **Closed** column is now a drop target on every forge (GitHub, GitLab, Forgejo): drag an open card into it to close the issue, drag a closed card back onto any open lane to reopen it and move it in the same drag. Closing leaves the card's column labels untouched, so it is the forge open/closed state that puts it there, not a label; a reopened card lands at the bottom of its destination lane. The write is forge-first, so a failed close or reopen snaps the card back. Closed issues also sync to a linked project board's Done status where one is available. No new lane is added: the board still reads Backlog, Planned, In Progress, Human Review, Later, Closed.
- **Slack DM alert when your Anthropic 7-day rate limit resets early ([#1020](https://github.com/vtmocanu/uzi/pull/1020)).**
  A new opt-out setting (on by default) fires a loud Slack DM, plus a durable inbox notification, when uzi's usage poller sees your weekly window reopen more than 8 hours before its previously expected reset; it only notifies and does not resume parked runs early.

### Changed

- **Worker resume durability.** A run now carries an owner-anchored checkpoint tip (`runs.checkpoint_tip`) with a CAS-delete and adoption guard ([#1053](https://github.com/vtmocanu/uzi/pull/1053)), and holds worker affinity through a fleet roll with reliable forge checkpoints, so a resumed run keeps its lineage instead of breaking it ([#1041](https://github.com/vtmocanu/uzi/pull/1041)).
- **Dependency and security bumps.** api `golang.org/x/crypto` CVE fix ([#1050](https://github.com/vtmocanu/uzi/pull/1050)) and the Kubernetes `client-go` monorepo for the controller ([#1039](https://github.com/vtmocanu/uzi/pull/1039)).
- **Internal refactor sweep (epic #915): large source files split into per-seam files, with no behavior change.** forge drivers ([#1040](https://github.com/vtmocanu/uzi/pull/1040)), the slacksvc notifier ([#1038](https://github.com/vtmocanu/uzi/pull/1038)), the handler route table ([#1023](https://github.com/vtmocanu/uzi/pull/1023)) and the schedules and workers handlers ([#1056](https://github.com/vtmocanu/uzi/pull/1056)), the settings domain ([#1029](https://github.com/vtmocanu/uzi/pull/1029)), the uzicli client ([#1027](https://github.com/vtmocanu/uzi/pull/1027)), the `api/cmd/uzi` CLI ([#1028](https://github.com/vtmocanu/uzi/pull/1028)), the controller `render.go` DinD renderer ([#1054](https://github.com/vtmocanu/uzi/pull/1054)), and web page component extraction from RunView and Board ([#1018](https://github.com/vtmocanu/uzi/pull/1018)) plus Judge.tsx into `pages/judge/` ([#1055](https://github.com/vtmocanu/uzi/pull/1055)).

### Fixed

- GitHub integer conversion in the forge driver ([#1052](https://github.com/vtmocanu/uzi/pull/1052)).
- Default an empty label color on GitLab `EnsureLabels` ([#1035](https://github.com/vtmocanu/uzi/pull/1035)).
- Bound the notification page offset so an out-of-range value returns 400 instead of a bad query (CodeQL alert 28, #1019).

## [0.75.1] - 2026-09-02

### Fixed

- **GitHub Actions pipeline now detected on agent branches; `pipeline_ref` no longer stays null ([#1010](https://github.com/vtmocanu/uzi/pull/1010)).**
  The ci_fix and mr_rework flows could not detect a GitHub Actions pipeline on an agent branch, so `pipeline_ref` stayed null and CI-fix and MR-rework never triggered; the GitHub forge driver now resolves the branch pipeline.

### Changed

- **run-kind registry: one Go source of truth for `runs.kind` ([#997](https://github.com/vtmocanu/uzi/pull/997)).**
  The eight run kinds are now sourced from a single Go registry with parity-pinned agent and web mirrors, replacing the previously duplicated kind tables; no runtime behavior change.
- **API contract fixtures pin the hot DTOs against the web types ([#1000](https://github.com/vtmocanu/uzi/pull/1000)).**
  Differential wire-shape tests compare the Go apitypes/handler DTOs to web's apiTypes.ts so the two cannot silently drift; internal test infrastructure, no product behavior change.
- **mockApi split into per-domain modules ([#1011](https://github.com/vtmocanu/uzi/pull/1011)).**
  The web mock API under `web/src/mocks/mockApi/` is now composed from per-domain modules into one typeof-realApi object; internal refactor of dev/test infrastructure, no UI behavior change.
- **Web generation-counter copy deduped into `useAsyncData` ([#1012](https://github.com/vtmocanu/uzi/pull/1012)).**
  Judge.tsx and the `deferred()` test fixture now reuse useAsyncData's generation-counter instead of a duplicated copy; internal refactor, no behavior change.
- **Routine dependency bumps: `@anthropic-ai/claude-agent-sdk` to 0.3.246 ([#987](https://github.com/vtmocanu/uzi/pull/987)), `gitlab.com/gitlab-org/api/client-go/v2` to v2.59.1 ([#988](https://github.com/vtmocanu/uzi/pull/988)), and `actions/upload-artifact` to v7.0.1 ([#989](https://github.com/vtmocanu/uzi/pull/989)).**
  Routine updates; the agent-SDK bump is additive (user_message_uuid, costBasis, modelPricing, perTaskStopAffordance) with no uzi code change required, and the upload-artifact major only raises the runner floor that GitHub-hosted runners already meet.

## [0.75.0] - 2026-09-02

### Added

- **Propose-only `refactor-scout` refactoring default added to the schedule catalog ([#962](https://github.com/vtmocanu/uzi/pull/962)).**
  A new opt-in catalog default that periodically surveys a repository for refactoring opportunities and reports them, without opening code changes on its own; off by default and enabled per repository like the other catalog defaults.
- **Configurable MR review quiet period, plus expanded end-to-end harness coverage ([#976](https://github.com/vtmocanu/uzi/pull/976)).**
  Adds a `MR_REVIEW_QUIET_PERIOD` setting controlling how long the MR review watcher waits for a review to settle before reworking, and rebuilds the e2e test harness around a phase registry with fail-soft reporting and run-kind/schedule coverage.

### Changed

- **httpx.PathUUID helper for the repeated uuid.Parse(chi.URLParam) handler pattern ([#952](https://github.com/vtmocanu/uzi/pull/952)).**
  Internal refactor consolidating the handler sites that parse a UUID path parameter onto one helper, with no change to request handling.
- **workersvc/service.go split, with a shared boolSetting helper ([#955](https://github.com/vtmocanu/uzi/pull/955)).**
  Internal refactor breaking up the worker service file and extracting a boolean-setting helper, with no runtime behaviour change.
- **Agent run god-methods split into phase steps ([#957](https://github.com/vtmocanu/uzi/pull/957)).**
  Internal refactor extracting phases out of the agent's RunRunner.execute() and SdkExecutor.run() methods, with no change to how a run executes.
- **Web load/loading/error fetch cycle consolidated into a useAsyncData hook ([#958](https://github.com/vtmocanu/uzi/pull/958)).**
  Internal refactor replacing the hand-rolled load/loading/error/reload pattern repeated across the web pages with one shared hook.
- **forgesvc/projectsync.go split by concern ([#975](https://github.com/vtmocanu/uzi/pull/975)).**
  Internal refactor separating provision/seed, forward and reverse sync, and visibility/share into their own files, with no change to sync behaviour.
- **pgconv package consolidating the duplicated pgtype parameter helpers ([#977](https://github.com/vtmocanu/uzi/pull/977)).**
  Internal refactor unifying the repeated pgtype conversion helpers with explicit Text vs TextOrNull semantics, with no change to stored values.
- **Web file splits: api.ts DTO types, AdminSettings cards, and mocks/data.ts domains ([#978](https://github.com/vtmocanu/uzi/pull/978)).**
  Internal refactor breaking large web modules into focused files, with no UI behaviour change.

### Fixed

- **Notifications page recovers after a failed Load more, and stale errors clear on refetch ([#973](https://github.com/vtmocanu/uzi/pull/973)).**
  Post-migration follow-up to the useAsyncData hook: the Notifications list is no longer stuck hidden after one failed Load more, promote/save/schedule mutation errors clear when their view refetches, unguarded side-effect fetches are guarded against stale responses, and a disabled hook no longer reloads.
- **Evicted zombie pod no longer permanently badges a healthy hosted worker "Upgrade failed" ([#953](https://github.com/vtmocanu/uzi/pull/953)).**
  The controller stopped treating a leftover evicted pod as an upgrade failure, so a healthy hosted worker keeps its correct status.
- **Secret scrubbing hardened, with fail-safe token handling and a request size limit ([#968](https://github.com/vtmocanu/uzi/pull/968)).**
  Widened secret-prefix detection (including the Slack xoxe- family), a fail-safe default in TokenInfo, and a 413 on oversized JSON request bodies, pinned by characterization tests.
- **Release publish jobs tolerate a transient cosign download failure ([#946](https://github.com/vtmocanu/uzi/pull/946)).**
  The image and chart signing jobs now install a pinned, checksum-verified cosign through a local action that retries the download, so a one-off TLS hiccup on a runner no longer fails a publish job and stalls the GitHub Release (issue [#945](https://github.com/vtmocanu/uzi/issues/945)).

## [0.74.0] - 2026-09-01

### Added

- **Per-user setting to disable AI attribution in worker commits and MR descriptions ([#942](https://github.com/vtmocanu/uzi/pull/942)).**
  A new per-user toggle in Settings lets each user opt out of the AI attribution that uzi's worker adds to its git commits and merge-request descriptions; the current attribution stays on by default, with a backfill migration for existing users.

### Changed

- **Forge driver internals consolidated: shared wrapErr, bounded rawGet, and a pagination helper ([#937](https://github.com/vtmocanu/uzi/pull/937)).**
  Internal refactor across the GitLab, Forgejo, and GitHub drivers extracting shared error wrapping, a size-bounded raw GET, and a common pagination helper, with no change to forge behaviour.
- **Shared forgetest.BaseFake behind the forge.Forge test fakes ([#939](https://github.com/vtmocanu/uzi/pull/939)).**
  Internal refactor giving the forge test fakes one shared base implementation so a new interface method no longer has to be hand-stubbed in each, with no runtime behaviour change.
- **Agent read-only model passes consolidated into runReadOnlyModelPass ([#931](https://github.com/vtmocanu/uzi/pull/931)).**
  Internal refactor unifying the agent's read-only model passes (judge, review, chat) onto a single helper, with no change to what those passes do.
- **Web mechanical dedup: shared errorMessage() and useNow() helpers ([#935](https://github.com/vtmocanu/uzi/pull/935)).**
  Internal refactor extracting repeated error-message formatting and current-time logic into shared helpers across the web components, with no UI behaviour change.
- **Dependency bumps: @anthropic-ai/claude-agent-sdk to 0.3.245 ([#923](https://github.com/vtmocanu/uzi/pull/923)), agent-browser to 0.35.0 ([#924](https://github.com/vtmocanu/uzi/pull/924)), and gitlab.com/gitlab-org/api/client-go/v2 to v2.59.0 ([#925](https://github.com/vtmocanu/uzi/pull/925)).**
  Routine dependency updates; the agent-SDK bump is patch-level parity with Claude Code (0.3.242 through 0.3.245) with no uzi code change required.

### Fixed

- **Selected floor-row title stays legible on light terminals in the CLI TUI ([#940](https://github.com/vtmocanu/uzi/pull/940)).**
  The selected floor row's title in the TUI board was drawn in a colour that vanished against light terminal backgrounds; it now keeps contrast in both light and dark themes.

## [0.73.0] - 2026-09-01

### Added

- **Slack DM when a locked vault is blocking your work ([#890](https://github.com/vtmocanu/uzi/issues/890)).**
  A boot reconciler now DMs each Slack-linked user whose locked vault is blocking a queued run or a due schedule, once per lock-episode, so they unlock before the work silently stalls; the copy is cause-neutral (it never asserts a restart as the cause) and reuses the existing Slack notification gate.
- **`scripts/init-env.sh` generates the local secrets so a fresh `docker compose up` just works ([#894](https://github.com/vtmocanu/uzi/issues/894)).**
  A new `./scripts/init-env.sh` (also `task init`) writes `.env` with freshly generated `JWT_SECRET`, `UZI_SECRET_KEY` and `POSTGRES_PASSWORD`, starting from `.env.example` so every other option stays documented; it is generate-once (writes only when `.env` is absent and never regenerates, keeping the encryption key and Postgres password stable), the `${VAR:?}` compose guards are unchanged so a non-local misconfig still fails loudly, and the Kubernetes/Helm deploy still uses explicit secrets.
- **Clickable MR/PR chip in the Runs view ([#803](https://github.com/vtmocanu/uzi/issues/803), PR [#901](https://github.com/vtmocanu/uzi/pull/901)).**
  The merge-request chip on a run row is now a real deep-link to the forge request, preferring the persisted `mr_web_url` and otherwise reconstructing it from the run's own issue URL; it opens in a new tab while the rest of the card still follows the stretched link to the run detail.
- **Ephemeral workers default to auto-select credential mode ([#804](https://github.com/vtmocanu/uzi/issues/804), PR [#905](https://github.com/vtmocanu/uzi/pull/905)).**
  An auto-provisioned throwaway worker now defaults to auto-select (using the owner's eligible token pool) instead of "use my default token", conditioned on a non-empty pool to avoid a pool-wait stall; a user's first or sole Anthropic token is born auto-eligible (new writes plus a backfill migration for existing sole-token users), so a single-token user's run spends their own token. Pinned and persistent workers are unchanged.

### Changed

- **Docs accuracy pass across the in-app and CLI docs ([#664](https://github.com/vtmocanu/uzi/issues/664), PR [#903](https://github.com/vtmocanu/uzi/pull/903)).**
  Folded CodeRabbit findings from PR [#656](https://github.com/vtmocanu/uzi/pull/656) into the shipped docs (Homebrew `brew trust` per-formula wording, run-summary model description, CI-autofix token wording, 2FA and OIDC clarification, and others), keeping the `docs/` source and the embedded `api/internal/uzidocs/embed` mirror in sync.

### Fixed

- **self_improve run no longer fails on `fetchAgentBranch` ([#887](https://github.com/vtmocanu/uzi/issues/887), PR [#902](https://github.com/vtmocanu/uzi/pull/902)).**
  The legacy flat `refs/uzi-runner/uzi/self-improve` tracking ref D/F-conflicted with the new per-cycle `refs/uzi-runner/uzi/self-improve/<runId>` ref; the conflicting ancestor is now archived and cleared before the fetch, so per-cycle self-improve runs succeed.
- **Sidebar forecast rate-limit tooltip keeps the "resets in" countdown ([#859](https://github.com/vtmocanu/uzi/issues/859), PR [#904](https://github.com/vtmocanu/uzi/pull/904)).**
  Forecast rows now carry the reset countdown in their tooltip, matching the safe rows.
- **Plan-submission serialization nudge to cut retry loops ([#858](https://github.com/vtmocanu/uzi/issues/858), PR [#900](https://github.com/vtmocanu/uzi/pull/900)).**
  The plan prompt now nudges the lead to keep `plan_md` valid JSON (escaped paths, no unescaped control characters, one complete piece), reducing SDK-side rejections that forced whole-plan re-emits. A mitigation, not a full fix.
- **judge-runner tolerates prose before the JSON inside a fenced block (PR [#907](https://github.com/vtmocanu/uzi/pull/907)).**
  `extractJsonObject` now runs the balanced-object scan on the fence inner content, so a model assessment with a prose preamble inside the fenced block still parses.

## [0.72.1] - 2026-08-31

### Changed

- **Demo mode is toggled only from Settings; the sidebar quick toggle was removed ([#893](https://github.com/vtmocanu/uzi/pull/893)).**
  The "Demo mode: On/Off" toggle in the sidebar user cluster was removed, leaving the Settings toggle as the single control; the sidebar still reflects demo mode by masking its own values, so masking behavior is unchanged.

## [0.72.0] - 2026-08-31

### Added

- **Demo mode masks identifying values in the web UI for screenshots ([#889](https://github.com/vtmocanu/uzi/pull/889)).**
  A device-local Demo mode toggle in Settings and the sidebar user area masks names, emails, repository paths, hosts, usernames, IP addresses and domains at their display sites across the app, so a screenshot can be shared without leaking identifying data; masking is display-only (underlying data, API responses, links, forms and navigation are unchanged), persists in local storage, and stays in sync across browser tabs.

## [0.71.2] - 2026-08-31

### Added

- **Cordon/drain state now shows on the admin fleet lists ([#879](https://github.com/vtmocanu/uzi/pull/879)).**
  A worker that is draining before a roll shows a cordon badge on the Dashboard and admin all-workers strip, instead of a bare online badge that hid the drain.

### Changed

- **SAST is now enforced in the quality gate ([#872](https://github.com/vtmocanu/uzi/pull/872), [#874](https://github.com/vtmocanu/uzi/pull/874)).**
  gosec runs inside golangci-lint and semgrep runs in lint-repo, checking uzi's own guardrail invariants, so a security regression reddens the gate rather than shipping.
- **GitHub Projects sync reads skip the extra scope preflight ([#877](https://github.com/vtmocanu/uzi/pull/877)).**
  The board-access panel's visibility read no longer makes an extra token-introspection round-trip on every open, while writes still enforce the project scope, and a read or toggle failure now renders the same friendly copy instead of a raw server message.

### Fixed

- **Schedule edit modal shows a default job's actual repo ([#875](https://github.com/vtmocanu/uzi/pull/875)).**
  A default schedule now renders its real repo read-only, and a non-default edit selects the job's actual repo even when the repo list omits it, instead of silently falling back to the first repo in the list.
- **CLI table truncation is rune-safe ([#879](https://github.com/vtmocanu/uzi/pull/879)).**
  compactText caps on runes rather than bytes, so a multibyte character straddling the cap is no longer split into invalid UTF-8, and the read stays bounded for large payloads.
- **Report-only completion no longer fails on a leftover park marker ([#878](https://github.com/vtmocanu/uzi/pull/878)).**
  A run that parked with a wip(park) marker and then legitimately finished report-only is no longer blocked by that abandoned marker, while a checkpoint holding real committed work still blocks to avoid orphaning it.
- **Base-align diff preservation is tightened ([#876](https://github.com/vtmocanu/uzi/pull/876)).**
  A double-fault (aligned push rejected, then a fallback conflict) preserves exactly the agent's work rather than a superset that carried the aligning strategy's own changes, a non-fast-forward merge-push failure preserves the diff and fails typed, and the workflow-scope diff is computed once and reused.
- **Lead context-window meter is more robust ([#880](https://github.com/vtmocanu/uzi/pull/880)).**
  The worker's context read is keyed to the lead lane and moved off the hot message loop so a slow read no longer delays a turn's frames, and the web reader accepts the reading from either carrier so historical and resumed runs keep a live meter.

## [0.71.1] - 2026-08-30

### Fixed

- **Sidebar build-info popover is centered and the version badge aligned ([#869](https://github.com/vtmocanu/uzi/pull/869), [#864](https://github.com/vtmocanu/uzi/pull/864)).**
  The sidebar footer's version badge is aligned and its build-info popover is centered, instead of the badge sitting off-alignment and the popover drawing off-center.
- **Sidebar tagline spacing tightened to the powered-by line ([#868](https://github.com/vtmocanu/uzi/pull/868), [#866](https://github.com/vtmocanu/uzi/pull/866)).**
  The sidebar tagline and its powered-by line now sit at the intended spacing rather than drifting apart.
- **Run-page option toggles now sit side by side ([#870](https://github.com/vtmocanu/uzi/pull/870), [#867](https://github.com/vtmocanu/uzi/pull/867)).**
  The run page's option toggles are laid out side by side on wide screens and stack vertically on mobile, instead of always stacking.

## [0.71.0] - 2026-08-30

### Added

- **semgrep is baked into the worker toolchain ([#863](https://github.com/vtmocanu/uzi/pull/863)).**
  uzi workers now carry semgrep (nixpkgs v1.164.0, pinned) in the baked devbox toolchain, so the SAST engine the security gate needs is present on every worker, the same "every worker should have it" class as yamllint and shellcheck. Delivered BYO-on-PATH via the toolchain (semgrep ships no static binary), with a fail-closed `toolchain-guard.tsv` row and the go:embed'd toolseed copy kept byte-identical. Prerequisite for the semgrep security-gate milestones (PRD #862).

## [0.70.0] - 2026-08-30

### Added

- **Runs now record a `trigger_source`: what, how, and who started each run ([#860](https://github.com/vtmocanu/uzi/pull/860)).**
  A first-class `trigger_source` column (NOT NULL, a 13-value enum covering manual, autopilot, schedule, self_improve, ci_fix, mr_rework, chat, task, task_review, then_fix, judge, judge_rerun and resume) is stamped on every run-insert path and backfilled for existing rows, a structured "run created" log line records it at creation, and it is surfaced in `uzi run get --json`/`--field`, `uzi admin runs`, and the web run DTO. New migration 00180.
- **The UI surfaces when a newer uzi release is available ([#848](https://github.com/vtmocanu/uzi/pull/848)).**
  A new server-side release-check service polls GitHub's latest release for this repo through a guarded HTTP client, persists the facts to app settings, and derives update-available, far-behind and security-update signals at read time behind a semver guard, which the web UI shows as an unobtrusive update cue. On by default; an admin master gate can turn it off so the api never calls github.com.
- **Layered enable/disable for MR review rework ([#847](https://github.com/vtmocanu/uzi/pull/847)).**
  MR review rework can now be turned on or off at four layers (per-run, per-schedule, CLI, and the admin/user default) with live inheritance: nullable `mr_rework_enabled` on runs and schedules resolves via COALESCE so an unset level inherits the one above it, exposed through `PUT /api/runs/{id}/mr-rework`, the run and schedule create paths, and `uzi schedule`. New migration 00179.
- **`stop_reason` is surfaced in the run read path ([#525](https://github.com/vtmocanu/uzi/pull/525)).**
  A run's `stop_reason` is now carried through the RunDTO, `uzi run get`, and the web run detail view, so why a run stopped is visible without digging into logs.

### Changed

- **The MIT © credit is now gated behind a build-time flag, hidden by default ([#835](https://github.com/vtmocanu/uzi/pull/835)).**
  The license credit shown in the app chrome is gated by a `SHOW_LICENSE_CREDIT` build-time constant (`web/src/lib/flags.ts`), defaulting to off/hidden; flip the constant and rebuild to show it. This reverses the previously always-shown behavior.
- **`uzi handoff` v1.1 hardening ([#403](https://github.com/vtmocanu/uzi/pull/403)).**
  Seven follow-up findings from the handoff review are addressed: the review base now defaults to the seed commit rather than the repo default, `--repo` must match the origin remote it seeds, and rm-safety gains a branch-wide MR exemption plus a terminal and active-child guard, alongside related cleanups across the agent and worker paths.
- **TUI context-meter refinements ([#571](https://github.com/vtmocanu/uzi/pull/571)).**
  Web-parity and performance and cleanup follow-ups for the TUI context meter, tightening the `uzi` TUI context and detail views.

### Fixed

- **A completed issue run that owns an open MR now blocks a fresh re-run of the same issue ([#861](https://github.com/vtmocanu/uzi/pull/861)).**
  A completed issue run is terminal, so the active-run gate could not see that it still owned an open merge request, and a fresh manual, board, Slack or sweep start would re-plan and re-run the whole review wave onto the already-open MR (silent wasted Anthropic spend; main is never touched). A create-time open-MR guard now hard-refuses such a start (HTTP 409 `issue_has_open_mr`, new scheduler skip reason `open_mr_exists`), releasing only on a terminal MR state, with a `uzi run create --force` override that bypasses only this guard.
- **In-flight MR rework is now aborted when its MR is merged or closed mid-run ([#854](https://github.com/vtmocanu/uzi/pull/854)).**
  When an MR merges or closes while an `mr_rework` run against it is still going, the MR-close watcher now cancels that run through the operator-cancel path (a live worker gets a stop verdict so it stops spending; a run with no live poller is flipped server-side), instead of letting it run on against a resolved MR.
- **The awaiting-followup watermark no longer strands a genuinely idle run ([#846](https://github.com/vtmocanu/uzi/pull/846), [#817](https://github.com/vtmocanu/uzi/pull/817)).**
  The `open_followup_id` watermark could be lowered to 0 by a worker re-claiming a requeued run, which then let the wake guard admit an already-consumed follow-up and spuriously un-park an idle run (a #559 regression); the watermark now has a current-value monotone floor.
- **Concurrent admin settings PUTs are serialized to preserve the cross-key label invariant ([#844](https://github.com/vtmocanu/uzi/pull/844), [#831](https://github.com/vtmocanu/uzi/pull/831)).**
  Two concurrent admin settings writes could both commit and leave `uzi_label` and `autopilot_label` equal (the FOR UPDATE cross-key check locked only pre-existing rows, and a fresh DB has no `uzi_label` row after the #764 rename); a `pg_advisory_xact_lock` now serializes all settings writes, fixing an intermittently red nightly compose E2E. New migration 00178.
- **Worker lint-ratchet base clamp is now durable across a mid-run `git fetch` ([#843](https://github.com/vtmocanu/uzi/pull/843), [#363](https://github.com/vtmocanu/uzi/pull/363)).**
  An agent-initiated `git fetch origin main` re-applied the clone's fetch refspec and forced the default-branch tracking ref back to the frozen mirror head, undoing the ratchet-base clamp and false-reddening the golangci-lint backlog; the runner clone's `remote.origin.fetch` refspec is now removed right after the clamp so a fetch touches no tracking ref.
- **GitHub review-thread and comment fetch no longer truncates at 100 ([#761](https://github.com/vtmocanu/uzi/pull/761)).**
  The GitHub forge driver now paginates review threads and comments via a GraphQL cursor instead of stopping at the first 100 nodes, so large MRs are read in full.
- **Cross-kind branch guard in `mr_rework` create is hardened against a TOCTOU race ([#760](https://github.com/vtmocanu/uzi/pull/760)).**
  The guard that prevents a second run of a different kind on the same branch is now an atomic count-plus-insert rather than a check-then-write, closing a time-of-check to time-of-use window.
- **Dynamic navigation encodes server-derived path segments at every call site ([#644](https://github.com/vtmocanu/uzi/pull/644)).**
  Every dynamic `navigate()` sink that builds a path from server-derived values now encodes the segment, hardening the web app against path injection per call site.
- **Version build-info popover no longer spills past the expanded sidebar ([#852](https://github.com/vtmocanu/uzi/pull/852)).**
  The build-info popover is now bounded to the sidebar width so it no longer overflows the sidebar's right edge when expanded.
- **`PageHeader` description cap widened so subtitles fill the row ([#842](https://github.com/vtmocanu/uzi/pull/842)).**
  The `PageHeader` description max-width is widened so page subtitles use the available row width instead of wrapping early.
- **Sidebar dividers are bounded to their cluster ([#830](https://github.com/vtmocanu/uzi/pull/830), [#828](https://github.com/vtmocanu/uzi/pull/828)).**
  Sidebar section dividers are now scoped to their own cluster rather than a sub-element, fixing a misdrawn divider.
- **Schedules page no longer crashes to a black screen on a labelless sweep default ([#829](https://github.com/vtmocanu/uzi/pull/829)).**
  A labelless `assigned-sweep` catalog entry hit `null.map` in the Default jobs view and crashed the Schedules page; the render now guards the null.

## [0.69.0] - 2026-08-29

### Added

- **Default schedules gain model-parity with custom schedules ([#691](https://github.com/vtmocanu/uzi/issues/691)).**
  An owner can now set "Apply model also to agents" (`override_subagent_model`) on a default (catalog) schedule in place — via the web modal, the API, and `uzi schedule edit --apply-model-to-agents` — instead of having to clone it first; Reset restores it to the catalog baseline (off) alongside the other editable fields. `uzi schedule edit` also gains a `--model <alias|id>` flag (empty clears back to the Worker-model default) for both custom and default schedules, closing the gap where the CLI could not change a schedule's model that the web UI and API already could.
- **Instance branding gains named logo presets ([#780](https://github.com/vtmocanu/uzi/issues/780), [#797](https://github.com/vtmocanu/uzi/issues/797)).**
  The admin App-logo picker replaces the default/custom dropdown with a tile radiogroup (uzi, Metaminds, or a custom upload), backed by a new `app_logo_preset` setting and a web-side preset catalog that is the single source of truth, so an unknown preset slug degrades gracefully to the stock uzi mark; the app-mark fallback element is now tagged for a non-vacuous test.
- **Branded favicon and browser-tab title ([#688](https://github.com/vtmocanu/uzi/issues/688)).**
  When a branded app logo is set, the browser-tab favicon becomes that logo (the run-status overlay dot still drawing on top) and the tab title follows the white-label, applied for signed-out visitors too; an unbranded instance keeps the ember factory mark.
- **Enabling a default schedule adopts your detected timezone ([#660](https://github.com/vtmocanu/uzi/issues/660)).**
  Enabling a catalog default job now sends the browser-detected IANA timezone so it fires in your local zone from the first run instead of the catalog's baked UTC; an idempotent re-enable never clobbers the stored zone.
- **Owner-guidance overlay now works on sweep default schedules too ([#675](https://github.com/vtmocanu/uzi/issues/675)).**
  The small owner-guidance overlay shipped for prompt defaults ([#662](https://github.com/vtmocanu/uzi/issues/662)) now extends to sweep-target defaults: the catalog guidance stays baked and read-only while your overlay is appended at fire time (8 KiB cap), editable from the web modal, the API, and `uzi schedule edit --guidance`.

### Changed

- **Kube-native worker egress is now single-sourced and self-checking ([#808](https://github.com/vtmocanu/uzi/issues/808)).**
  The restricted-tier FQDN allow-list's forge entry is now derived from `FORGE_ALLOWED_BASE_URLS` instead of a second hand-kept copy, so the api's SSRF allowlist and the worker egress can no longer diverge, and `search.devbox.sh` (the devbox package resolver) is added to the shipped default allow-list; a build-time completeness guard fails the `helm-chart` CI job, naming the host, if a canonical worker destination is ever missing from the rendered list.
- **Kube-native workers can provision devbox tools on a fresh deployment ([#818](https://github.com/vtmocanu/uzi/issues/818)).**
  Completes #808's egress model by adding `api.github.com` to the shipped default FQDN allow-list and the completeness guard: `devbox install` resolves the floating `nixpkgs-unstable` ref for its generated dev-env flake via `api.github.com`, which #808 omitted, so a fresh kube-native deployment could not provision tools until now; the host is forge-independent (nixpkgs lives on GitHub) and a deliberate widening backstopped by the server-side tier-1 admin allowlist.
- **Assigning an issue to the uzi-bot is now a run-eligibility signal too ([#767](https://github.com/vtmocanu/uzi/issues/767)).**
  An issue carrying the `uzi` label OR assigned to the uzi-bot account is runnable through the single eligibility gate, so the manual Start button, autopilot's poller, and every sweep's per-issue gate recognize either signal. Existing label sweeps still pick their candidates by label (`bug`/`Planned`, unchanged) — a candidate that is bot-assigned rather than `uzi`-labelled now fires where it was skipped before — while a new opt-in `assigned-sweep` catalog default picks bot-assigned issues that carry no selector label (`auto_approve` on, matching the existing sweeps). The board marks a bot-assigned card runnable with no label needed. Assignment alone never starts a run: unattended execution still needs the `autopilot` label or an enabled sweep, exactly like a `uzi`-labelled issue.
- **Non-interactive handoff task runs get a dedicated 4h budget ([#785](https://github.com/vtmocanu/uzi/issues/785)).**
  A `uzi handoff` task run is issue-less and not milestone-structured, so it never received the milestone-scaled budget and fell back to the ~2h global `RUN_TIMEOUT`, often too short; non-interactive handoffs now persist a dedicated budget from new `HANDOFF_RUN_TIMEOUT` (default 4h) and `HANDOFF_RUN_MAX_ITERATIONS` (default 10) server settings, capped at the 8h wall ceiling, while interactive handoffs keep today's idle-bounded behavior.
- **Task and handoff runs now inherit the owner's wait-on-limit default ([#815](https://github.com/vtmocanu/uzi/issues/815)).**
  `uzi handoff` (kind `task`) runs were the only run-creation path that ignored the owner's `wait_on_limit` default, so a user who had it on got task runs that failed on an Anthropic usage limit instead of pausing; they now resolve the owner default like every other run kind.
- **Docker-allowlist "waiting for worker" queued reason is documented, plus an internal refactor ([#509](https://github.com/vtmocanu/uzi/issues/509)).**
  The docker-worker allowlist queued reason is now described under the run-health docs so the cross-link from admin settings resolves to real content, and the duplicated docker allow/blocked-set computation behind the project and repo listings is consolidated into one shared helper with no behavior change.
- **Default jobs row copy fixes ([#676](https://github.com/vtmocanu/uzi/issues/676)).**
  The Default jobs tab no longer shows the enabled-repo count twice (the disclosure toggle is now a bare chevron; the count still reads from the green Next-run pill), and the pencil action's tooltip is relabeled from "Edit cadence" to "Edit settings" since the modal edits timezone, model, auto-approve, wait-on-limit, sweep max-issues and guidance, not only cadence.
- **Dependency updates.**
  The Claude Agent SDK to 0.3.240 ([#802](https://github.com/vtmocanu/uzi/pull/802)).

### Fixed

- **`run_usage` no longer undercounts a broken-resume run's cost ([#632](https://github.com/vtmocanu/uzi/issues/632)).**
  When a worker restart failed to resume and started a fresh SDK session, that leg accumulated token/cost from zero under a new session row and the `run_usage_totals` rollup's MAX-per-model silently masked the smaller leg, so a run's recorded total could undercount by up to a full leg (measured live: one run recorded $64.74 against an actual $321.66). The server now stamps a defaulted `lineage_epoch` marker, bumped once when the worker signals a dropped resume (`resume_lineage_break`), and the totals view MAXes within each `(run, model, lineage_epoch)` then SUMs across epochs — recovering the masked leg. Advisory telemetry only (nothing is billed from it), and existing rows default to epoch 0 so no historical number is restated; the web run-page's own client-side usage strip stays epoch-unaware for now (tracked separately).
- **Board auto-move now ensures the target column's label before writing it ([#800](https://github.com/vtmocanu/uzi/issues/800)).**
  `AutoMove` was the only board column-write path that wrote forge labels without first ensuring the target column's label exists, so a missing or drifted column label (for example after the Upcoming to Planned rename) could produce a wrong color or a failed move; it now ensures the label, with the correct pinned color for a default column and the standard grey for a custom one.
- **Milestone budget no longer counts time spent waiting at the approval gate ([#783](https://github.com/vtmocanu/uzi/issues/783)).**
  A gated run's wall-clock budget was measured purely from `started_at`, so time parked at `awaiting_approval` or `awaiting_input` consumed the implementation budget and a slowly-approved plan (overnight, busy approver) could false-fail with `RUN_TIMEOUT` and discard its remaining milestones; each park's duration is now banked (new `budget_paused_seconds`) and added back to the timeout deadline, leaving run-duration display and health baselines unchanged.
- **Interactive wake-guard watermark no longer strands a run on a peek/stamp race ([#559](https://github.com/vtmocanu/uzi/issues/559)).**
  The awaiting-followup watermark was server-derived, which raced a follow-up consumed during the park report's DB round-trip and could strand a run at `awaiting_followup` until the ~30-minute sweeper; the watermark is now worker-provided and clamped server-side (floored at 0, ceilinged at the max-consumed id, with a fallback for older workers), closing the race and restoring the park-skip ownership ACK.

## [0.68.0] - 2026-08-29

### Changed

- **Run-eligibility collapses to a single `uzi` label ([#764](https://github.com/vtmocanu/uzi/issues/764)).**
  Labeling an issue `uzi` (configurable, `uzi_label`) is now the only thing that makes it runnable; the `PRD`/`PRDLESS`/waiver either-or, the `run_eligible_labels`/`board_extra_labels` admin sets, and the `prd_label` special-casing are removed end to end, along with the `no_prd_link` schedule skip-reason. `Planned` and `bug` remain pure sweep selectors that only fire a candidate once it also carries `uzi`. A linked `prds/*.md` file is now optional everywhere: still auto-detected and implemented when present, and shown as a neutral "PRD" presence badge on the board card and the runs view, but never required to start a run. The board still shows every open issue; its filter toggle switches from PRD-based to `uzi`-only / all. A committed, idempotent one-shot, `scripts/backfill-uzi-labels.sh`, is provided to add `uzi` to currently-runnable open issues; run it once at cutover.
- **Scheduled self-improve now targets the enabling repo, not just uzi ([#686](https://github.com/vtmocanu/uzi/issues/686)).**
  The `self-improve` default job reviews and improves whichever repo it's enabled on; folding your accumulated "improve uzi" recommendations and running uzi's own trusted directive is now an explicit, owner-set per-repo capability that existing self-improve schedules were switched on for automatically, so nothing changes for uzi's own instance. Each cycle also now opens a fresh merge request off current main instead of extending one long-lived branch, capped at 2 concurrently open self-improve merge requests per repo so further cycles skip (with a notification) until you merge or close one.
- **Dependency updates.**
  The GitLab API client (`gitlab.com/gitlab-org/api/client-go/v2`) to v2.58.2 ([#786](https://github.com/vtmocanu/uzi/pull/786)).

### Fixed

- **Automated MR-rework runs now actually run ([#778](https://github.com/vtmocanu/uzi/issues/778)).**
  The mr_rework lane never worked: the worker had no branch-derivation case for it and the claim wire never carried the merge-request branch, so a rework run could not push. Rework changes now push to the existing MR branch even without an issue reference, and the run's title and description identify it as an automated rework.
- **Worker no longer strands a run seeded off a stale, disjoint remote ref ([#781](https://github.com/vtmocanu/uzi/issues/781)).**
  A worker could seed a run's clone off a stale remote-tracking ref whose history shares no commit with the default branch, leaving the branch impossible to base-align or push. The worker now prunes the fetch and rejects a base with no common history, so valid work is no longer lost to a disjoint seed.
- **Worker run survives a transient network blip during the claim-time clone ([#775](https://github.com/vtmocanu/uzi/issues/775)).**
  The claim-time bare clone had no retry on the clone/fetch path and the forge classifier closed a connect timeout as permanent, so a momentary network hiccup failed the run. Transient connection failures on clone and checkout now retry, cleaning up incomplete data before each attempt, and a connect timeout is treated as transient.
- **web tests no longer fail under Node >=26 ([#340](https://github.com/vtmocanu/uzi/issues/340)).**
  Node 26's built-in localStorage shadowed jsdom's, breaking the web test setup; a fallback for missing or incomplete browser storage keeps local and session storage working in the test environment.

## [0.67.0] - 2026-08-28

### Added

- **Instance branding (Admin → Branding, [#685](https://github.com/vtmocanu/uzi/issues/685)).**
  Replace the app mark with a custom logo or full white-label, add an optional "POWERED BY" brand in the sidebar, and surface a fixed MIT/author credit that no branding setting can remove, all runtime admin settings, no redeploy; fresh installs stay unbranded.
- **Default jobs tab shows each repo's last-run outcome ([#752](https://github.com/vtmocanu/uzi/pull/752)).**
  The Default jobs tab now surfaces the per-repo last-run status the My schedules tab already showed, so an enabled default's most recent result is visible at a glance.

### Changed

- **MR review comments are now auto-reworked in place, on by default, for every opted-in user ([#700](https://github.com/vtmocanu/uzi/issues/700)).**
  When a completed run's merge request gets new review comments on a green pipeline, uzi starts an `mr_rework` run that treats each finding as untrusted data, implements the ones still valid, and replies to (and, where the forge supports it, resolves) each thread on the existing branch and MR, capped at 5 rework cycles per MR by default. This is an announced behavior change: after upgrading, opted-in users' MRs (including unattended nightly-sweep MRs) get reworked automatically on their own Anthropic token, unless they opt out from Settings ("Auto-rework MR review comments on my runs") or an admin turns off the instance-wide `mr_rework_enabled` kill-switch.
- **Auto lane never spends a non-pooled token ([#754](https://github.com/vtmocanu/uzi/pull/754)).**
  Auto worker selection now floors to the shared pool and holds when the pool is empty, instead of falling back to a non-pooled token, so an auto-lane run cannot consume a user's personal token by accident.
- **Ephemeral workers burst on saturation, not only on capability gaps ([#757](https://github.com/vtmocanu/uzi/pull/757)).**
  A saturated fleet now spins up ephemeral workers to absorb the backlog, matching the burst behavior previously reserved for capability gaps.
- **self_improve schedules are editable, plus PRD #590 follow-ups ([#753](https://github.com/vtmocanu/uzi/pull/753)).**
  self_improve schedules can now be edited, the auto_approve no-op is corrected, and vault-skip handling was polished.
- **Release and CI image builds tolerate transient network blips ([#739](https://github.com/vtmocanu/uzi/pull/739)).**
  The image build steps now retry through transient network failures, so a registry or network hiccup no longer fails a release or CI run.
- **Dependency updates.**
  Bubble Tea v2 ([#734](https://github.com/vtmocanu/uzi/pull/734)), the Kubernetes client-go monorepo ([#733](https://github.com/vtmocanu/uzi/pull/733)), chi to v5.3.2 ([#741](https://github.com/vtmocanu/uzi/pull/741)), and the Claude Agent SDK to 0.3.239 ([#769](https://github.com/vtmocanu/uzi/pull/769)).

### Fixed

- **A gated run no longer loses in-progress work across an Anthropic usage-limit park ([#759](https://github.com/vtmocanu/uzi/issues/759)).**
  On park, any uncommitted edits are auto-committed to a clearly-marked throwaway `wip(park):` commit before the tree is wiped, so the existing checkpoint machinery carries them off the worker; on resume the reseed restores that snapshot to uncommitted (never entering the branch history or the merge request), exactly for a same-worker resume and best-effort for a cross-worker one. `WORKER_AFFINITY_CEILING` is raised from 30 minutes to 2 hours so a long park stays pinned to its original, still-alive worker, where the SDK session and the recovery are both exact; a resumed run whose plan was provably human-reviewed and whose work recovered cleanly keeps implementing instead of re-planning from scratch, while a recovery failure still re-gates for human review; and the run feed now says whether it recovered an uncommitted snapshot or a committed milestone instead of leaving the loss to PVC forensics.
- **A run stays in the "needs approval" grouping while it re-plans after a revise ([#758](https://github.com/vtmocanu/uzi/pull/758)).**
  A run actively re-planning after a revise no longer drops out of the needs-approval grouping mid-replan.
- **Hosted-worker upgrade classifier no longer flags a reuse-retagged worker as "outdated" ([#755](https://github.com/vtmocanu/uzi/pull/755)).**
  When `workers.image.tag` points at a reuse-retagged agent image, the upgrade classifier now reads the worker as current instead of perpetually "outdated".
- **Judge page label counts reconcile with the To-triage count ([#749](https://github.com/vtmocanu/uzi/pull/749)).**
  The Judge page's per-label group counts now match the To-triage recommendations count.
- **Default jobs Enable button shows only when it can act ([#746](https://github.com/vtmocanu/uzi/pull/746)).**
  The per-row Enable button now appears only when enabling is possible, replacing the dimmed, unactionable wall.
- **Schedule edit modal Delete button no longer overlaps the footer at narrow width ([#743](https://github.com/vtmocanu/uzi/pull/743)).**
  The Delete button and the footer summary no longer collide when the schedule edit modal is narrow.
- **uzi TUI no longer panics on exit ([#751](https://github.com/vtmocanu/uzi/pull/751)).**
  A stale detail load is dropped, so exiting the TUI board can no longer hit a nil-map panic.

## [0.66.3] - 2026-08-27

### Changed

- **Maintenance release: republished the container images and Helm chart.**
  Refreshes the published packages; no application, chart, or worker behavior change.

## [0.66.2] - 2026-08-27

### Changed

- **Published images and the Helm chart declare their source repository.**
  Each image and the chart now carry an org.opencontainers.image.source label/annotation so the registry links the packages to the repository.
- **Hosted worker fleet pinned to the 0.66.2 agent image.**
  Advances the hosted worker image tag to 0.66.2 so the fleet runs a freshly built agent image at the current version.

## [0.66.1] - 2026-08-27

### Changed

- **Hosted worker fleet pinned to the 0.66.1 agent image.**
  Advances the hosted worker image tag to 0.66.1 so the fleet rolls onto the current agent image; no application behavior change.

## [0.66.0] - 2026-08-27

### Changed

- **Maintenance release; no functional changes since 0.65.1.**
  Republished the container images and Helm chart from a fresh repository baseline; application behavior is unchanged from 0.65.1.

## [0.65.1] - 2026-08-25

### Changed

- **The `coder` builtin no longer carries the worker-runtime dependency note in its body ([#702](https://github.com/vtmocanu/uzi/issues/702), [#708](https://github.com/vtmocanu/uzi/pull/708)).**
  That guidance now rides the subagent harness (shipped in 0.65.0), so PRD #702 M6 removes it from the `coder` builtin body; the stripped body reaches pristine installs on the next api boot via the existing content-guarded refresh.
- **Agent-source roster file cap raised from 16 to 32 for roster headroom ([#710](https://github.com/vtmocanu/uzi/pull/710)).**
  A larger published roster (for example the canonical `product-agents/` set) now syncs without hitting the previous 16-file read cap.
- **Worker-runtime context lifted out of the web-ux, architect, and fact-checker role bodies so the synced generic versions are safe ([#709](https://github.com/vtmocanu/uzi/issues/709)).**
  The worker now guarantees Chromium's `--no-sandbox` flag from the agent environment itself (not only the browser-launcher shim), so it holds even when the shim is bypassed; with that guarantee in place three worker-runtime notes are removed from the corresponding builtin bodies (web-ux's `--no-sandbox` note, architect's worker-tool-gating note that missing write tools means the worker is enforcing write-after-approval rather than a broken environment, and fact-checker's file-access-guardrail note), leaving only generic guidance that is non-destructive to overwrite when an agent-source sync publishes the upstream generic roles.

## [0.65.0] - 2026-08-25

### Added

- **Follow an agent-roster source at a configurable folder, with a one-click preset and an "update available" badge ([#702](https://github.com/vtmocanu/uzi/issues/702), [#707](https://github.com/vtmocanu/uzi/pull/707)).**
  The admin agent-source card gains a source-folder field (default `.claude/agents`, so existing installs are unchanged), a Preset button that fills the canonical `github.com/vtmocanu/skills` source, its `product-agents/` folder, and the latest tag resolved at click time, and a badge that surfaces when the configured source has published a newer resolvable ref (with a "bump pin" button); applying still flows through the existing sync, approve, and apply gate.

### Changed

- **Worker-runtime dependency guidance now rides the subagent harness instead of individual role bodies ([#702](https://github.com/vtmocanu/uzi/issues/702), [#707](https://github.com/vtmocanu/uzi/pull/707)).**
  Every subagent's system prompt carries a generic, repo-neutral note that the worker pre-installs the repo's JS dependencies in the background, so a role no longer depends on its body text to know not to run its own `npm ci` or `npm install`; the lead channel already carried an equivalent note.
- **Routine dependency bump: gitlab client-go to v2.58.1 ([#701](https://github.com/vtmocanu/uzi/pull/701)).**
  Updated `gitlab.com/gitlab-org/api/client-go/v2` in the api module; mechanical, no behavior change.

## [0.64.0] - 2026-08-25

### Added

- **Per-user reasoning effort setting ([#617](https://github.com/vtmocanu/uzi/issues/617)).**
  Each user can pick a default reasoning-effort level (low, medium, high, xhigh, max) for their runs, threaded through the same layers as the existing per-user model picker and surfaced in the Settings card beneath the worker-model dropdown; leaving it unset inherits the SDK default.
- **Board footer shows a live CLI-vs-server version readout ([#681](https://github.com/vtmocanu/uzi/issues/681), [#687](https://github.com/vtmocanu/uzi/issues/687)).**
  The `uzi tui` board footer now carries a compact version readout that collapses when the CLI and server match and turns red only when the CLI is semver-behind, auto-refreshing on a few-minute ticker so a server rolling forward while the TUI is open lights up within one interval without a restart.

### Changed

- **Routine dependency bump: sqlc to v1.31.1 ([#679](https://github.com/vtmocanu/uzi/pull/679)).**
  Regenerated the sqlc-generated store package (`api/internal/store/*.sql.go`) against the new sqlc release; mechanical, no behavior change.

### Fixed

- **Judge pre-scan no longer trusts a bare `--version` as tool presence ([#282](https://github.com/vtmocanu/uzi/issues/282)).**
  The retrospective command-not-found pre-scan stopped counting a bare `<tool> --version`/`-V`/`version` probe as evidence a tool ran, so a Claude Code shell-function shim that answers `--version` in-session no longer masks a genuine `command not found` for the same name in a shim-blind context; a tool that does real work in the same command still suppresses as before.
- **Judge pre-scan no longer matches tool names inside file-read content ([#326](https://github.com/vtmocanu/uzi/issues/326)).**
  The retrospective command-not-found pre-scan stopped matching tool names that appear only inside file-read output rather than as an executed command, eliminating false `install_worker_tool` recommendations.

## [0.63.0] - 2026-08-24

### Changed

- **Catalog base prompts fold in generic test, bug, and docs discipline ([#683](https://github.com/vtmocanu/uzi/pull/683)).**
  Every user's default jobs now auto-track guidance that previously lived only in per-repo owner overlays: bug-hunt restores the test escape-hatch (skip a reproducing test only for a non-code or genuinely contrived fix, with a stated reason), docs-hygiene refuses to guess a repoint when a broken link has more than one plausible target, and test-improvement adds fold-to-an-existing-value mutation quality, the untestable-seam out-of-scope rule, and incidental-bug reporting.

## [0.62.0] - 2026-08-24

### Added

- **Default prompt schedules take editable owner guidance ([#671](https://github.com/vtmocanu/uzi/pull/671)).**
  A catalog prompt default can be enabled and then layered with repo-specific steering: guidance is capped at 8 KiB, clearing it restores the catalog default, partial edits preserve existing guidance, and the original run title is kept.

### Changed

- **Agent worker images cache their expensive layers and skip rebuilds when the runtime surface is unchanged ([#670](https://github.com/vtmocanu/uzi/pull/670)).**
  A release that does not touch the agent runtime re-tags the previous image instead of rebuilding the ~2.6 GiB nix/Chromium layers, and the builds that do run reuse a registry layer cache.
- **Catalog prompt jobs open merge requests instead of only reporting ([#667](https://github.com/vtmocanu/uzi/pull/667)).**
  Bug-hunt, docs-hygiene, test-improvement, and feature-bingo jobs now commit and open an MR for a confirmed change, fall back to a report when nothing is worth doing, and feature-bingo dedupes against existing ideas.
- **Catalog base prompts enriched with generic discipline ([#674](https://github.com/vtmocanu/uzi/pull/674)).**
  The bug-fix, docs-audit, feature-proposal, and test-improvement base prompts gain stale-reference and frontmatter checks plus stronger requirements for deterministic regression tests and meaningful assertions, enabling the enable-a-default-plus-small-overlay workflow.

### Fixed

- **`uzi schedule edit` no longer fails on default-origin schedules ([#669](https://github.com/vtmocanu/uzi/pull/669)).**
  The CLI sends only editable fields for a default-origin schedule (preserving server-managed models and sweep limits) and routes catalog-owned changes through the clone-then-edit flow instead of returning a 400.
- **TUI run-detail header always renders on one line ([#668](https://github.com/vtmocanu/uzi/pull/668)).**
  The header stays a single row at any terminal width by truncating a long run title first, keeping status, elapsed time, and transport visible.

## [0.61.0] - 2026-08-24

### Added

- **Web UI for ephemeral worker auto-provisioning ([#651](https://github.com/vtmocanu/uzi/pull/651)).**
  An admin instance kill-switch and a per-user "Auto-provision on demand" toggle land on the Workers page, alongside an `ephemeral` badge in the fleet list.
- **Per-run cost and token spend in the TUI ([#652](https://github.com/vtmocanu/uzi/pull/652)).**
  A COST column and floor total on the board (whole dollars) and a SPEND block in the run view (cents), each formatted for its width budget.
- **Embedded application docs in the CLI binary via a new `uzi docs` command ([#656](https://github.com/vtmocanu/uzi/pull/656)).**
  Agents can now answer onboarding questions offline, without a live web UI to consult.

### Changed

- **Workers settings card reflows badges and controls ([#653](https://github.com/vtmocanu/uzi/pull/653)).**
  Read-only badges move to the top-right and interactive controls to their own row.
- **Factory-floor board columns reorder to account, cost, issue (e034b5e22).**
  Width math in `boardRowPrefixWidth` is order-independent, so only the render blocks moved.
- **The Claude SDK session survives a same-worker restart ([#654](https://github.com/vtmocanu/uzi/pull/654)).**
  A mid-run requeue now resumes the existing session instead of re-planning from scratch.
- **Bug-triage default prompt gains false-positive and deterministic-test guidance (98ae8b4cb).**
  The built-in bug-triage schedule now takes an explicit report-only path on a false-positive or already-fixed issue and requires deterministic tests.
- **Dependency: github.com/slack-go/slack bumped to v0.29.0 ([#619](https://github.com/vtmocanu/uzi/pull/619)).**

### Fixed

- **A lead's terminal refusal now stops the run instead of re-prompting ([#655](https://github.com/vtmocanu/uzi/pull/655)).**
  Previously it re-prompted through the entire iteration budget over an unchanged worktree.
- **`SetRunRunning` no longer re-stamps `status_since` on every heartbeat ([#657](https://github.com/vtmocanu/uzi/pull/657)).**
  That silently reset the approval-gate clock.

## [0.60.0] - 2026-08-23

### Added

- **Custom schedules can span several sibling repos in one call, grouped in the UI ([#637](https://github.com/vtmocanu/uzi/issues/637)).**
  Creating or enabling a custom schedule now accepts multiple repos at once, producing one independently tunable schedule per repo shown as a sibling group on the Schedules page, and the Default jobs tab gained matching polish.

### Changed

- **Builtin agent-template drift detection is pinned end to end, closing [#223](https://github.com/vtmocanu/uzi/issues/223) ([#646](https://github.com/vtmocanu/uzi/pull/646), [#648](https://github.com/vtmocanu/uzi/pull/648)).**
  A narrow faked-DB write-store seam makes the builtin-template get and reset handlers testable at their entry point, and a shared cross-language fixture pins the three "has this row drifted from the shipped definition?" predicates (the Go content check, the web mock, and the drifted-columns selector) so they can no longer silently diverge; both add mutation-pinned tests with no change to runtime behavior.

### Fixed

- **Mid-run STOP and milestone-scope reductions are now honored, not just acknowledged ([#639](https://github.com/vtmocanu/uzi/issues/639), [#645](https://github.com/vtmocanu/uzi/issues/645)).**
  A follow-up sent mid-run to stop the run or shrink its remaining milestone scope was consumed but never acted on; the worker now applies both, including the STOP-at-zero edge where a NULL `milestones_completed` let a run slip past the scope-ceiling gate.
- **The schedule issue-target guard can no longer be bypassed by adding a repo ([#641](https://github.com/vtmocanu/uzi/issues/641), [#643](https://github.com/vtmocanu/uzi/issues/643)).**
  Adding a repo to an existing schedule skipped the issue-target guard that new schedules enforce; the guard now covers the add-repo path, alongside several schedule-UI fixes.

## [0.59.0] - 2026-08-23

### Added

- **Weekly upstream role-manifest refresh bot keeps the builtin agent roles from silently drifting ([#601](https://github.com/vtmocanu/uzi/issues/601)).**
  A scheduled workflow fetches the upstream skills `roles.yaml` and, when a shipped role's version has moved, bumps the vendored manifest and opens a PR (falling back to a tracking issue where it cannot open one), which reddens the builtin-drift test until a human ports the changed bodies, closing the PRD #85 maintenance loop without the remember-to-check-upstream toil.
- **Admin-configurable agent-source repo sync ([#602](https://github.com/vtmocanu/uzi/issues/602)).**
  Admins can point uzi at an external repo whose `.claude/agents/` roster is synced in as an additive source layer (off by default, empty URL), with scope-aware origin provenance so a synced or admin-edited role is never clobbered by the boot-time builtin refresh; it is gated by approve-before-apply as the primary control, a separate `AGENT_SOURCE_ALLOWED_BASE_URLS` SSRF allowlist, a secretbox-sealed credential, and ref pinning.

### Changed

- **Self-improvement is now a `self_improve` default scheduled job; the bespoke engine is retired ([#590](https://github.com/vtmocanu/uzi/issues/590)).**
  Self-improvement moves off the standalone `api/internal/selfimprove` engine onto PRD #589's catalog machinery as a promptless seventh default job, generalized from admin-only and instance-wide to per-user on any owned repo; its orchestration relocates into `schedsvc`, the active-run dedup becomes per-repo instead of instance-wide (so distinct owners no longer serialize), and an enabled legacy install is boot-migrated to the new schedule form. Closes [#301](https://github.com/vtmocanu/uzi/issues/301), [#296](https://github.com/vtmocanu/uzi/issues/296), and [#524](https://github.com/vtmocanu/uzi/issues/524).
- **Builtin agent templates refreshed from the upstream role library (process lessons from PRD #72 / issue [#147](https://github.com/vtmocanu/uzi/pull/147), plus judge recommendations).**
  The `coder`, `reviewer`, and `web-ux` builtins gained hard-won guidance: the reviewer scopes dead-code/stale-reference sweeps with `git grep` to the touched packages (no more multi-MB `node_modules` hits) and calibrates a search against a known-present string before trusting an empty result; the coder never emits raw control bytes into generated source (documented escapes only); the web-ux agent uses `agent-browser set viewport` rather than guessing the verb.
- **Base-align now overlays only the `.github/workflows/` subtree when a GitHub branch is behind main on workflows but never touched them (PRD #456, [#627](https://github.com/vtmocanu/uzi/issues/627)).**
  This runs before the existing merge/rebase chain, cannot conflict, preserves original commit SHAs as a fast-forward, and avoids the whole-tree merges that used to abort unrelated behind-only branches as `finalize_base_align_conflict`; the merge→rebase→preserve-and-fail fallback is unchanged for every other case.
- **L worker preset memory limit raised 8Gi to 12Gi to stop the runtime OOMKill tail ([#131](https://github.com/vtmocanu/uzi/issues/131)).**
  Runs whose peak wanted 8-12 GiB were being OOMKilled on the L preset; its `MemoryLimit` moves to 12Gi (request stays 4Gi, so packing density is unchanged and the s/m sizes are untouched) across the preset table, both worker-namespace LimitRange ceilings in the chart, and the web display mirror, with a new test asserting every preset's limit fits the chart's LimitRange max so a future mismatch fails in CI rather than silently at cluster admission.
- **The run-detail TUI now shows the run's Anthropic account in the crew rail and drops the header credential tag ([#623](https://github.com/vtmocanu/uzi/issues/623)).**
  The account a run is spending is always shown as the first ACCOUNTS entry (force-shown even when it is deselected in settings), and the credential tag is removed from the run-detail header's first line so the title flows into the reclaimed width.

### Fixed

- **Mock mode now reaches the RunSummary card ([#616](https://github.com/vtmocanu/uzi/issues/616)).**
  Mock fixtures did not seed the structured run-summary fields (`summary_intent`/`summary_plan`/`summary_deltas`), leaving the RunSummary card unreachable under `VITE_UZI_MOCK=1`; the mock runs now seed them (an awaiting-approval run gets intent + plan + a deltas array covering the added/changed/dropped kinds, a running run gets intent only), so the card renders in mock mode.
- **Cross-worker resume durability: a `limit_wait` park re-claimed by a different worker no longer re-implements committed milestones ([#628](https://github.com/vtmocanu/uzi/issues/628)).**
  A run that parked on an Anthropic usage limit and was later resumed on a different worker used to redo milestones it had already committed; the park now checkpoints its progress, resume affinity is drain-aware, cross-worker recovery is verified, and `milestones_completed` is reset only on a genuine default reseed rather than on every resume (PRD #628, M0-M4).

## [0.58.0] - 2026-08-23

### Added

- **Ephemeral hosted workers: auto-provision one run-bound worker when no online worker has the capability ([#529](https://github.com/vtmocanu/uzi/issues/529)).**
  When a queued run needs a capability nothing in the online fleet can satisfy, the api now provisions a single hosted worker bound to that one run, lets it do the work, and tears it down when the run finishes (dropping the row; the controller reaps the pod). It is off by default and gated twice, by an admin instance switch and a per-user opt-in, with its own concurrency cap separate from the standing hosted-worker quota. Because each ephemeral worker gets fresh per-worker PVCs, it also closes the cross-run executable-persistence residual tracked in ADR-91 for ephemeral usage.
- **Plan approval gate: surfaces the files the planning turn wrote to the worktree ([#212](https://github.com/vtmocanu/uzi/issues/212)).**
  A plan turn can write to the worktree before implementation even starts, and those writes used to be invisible at the gate and get swept into the first implement commit unseen; the gate now shows a "Files changed during planning" list in both the web approval panel and `uzi run get` so you approve with the full picture. It's surface-only, it doesn't block the write or the approval.
- **A catalog of 6 built-in default scheduled jobs, enable per repo with one click (PRD #589).**
  uzi now ships a `go:embed`'d catalog of generic default jobs (weekly test improvement, docs hygiene, a deep bug-hunt audit, feature-bingo brainstorming, and daily bug/Planned label sweeps) that you enable rather than write from scratch; a default's baked prompt (or sweep label) is resolved from the catalog at fire time rather than copied onto your schedule, so a prompt fix uzi ships later reaches every repo that already enabled the job with nothing to re-seed, while its cadence, model, and run options stay yours to edit with a Reset-to-default action.
- **Enable a default (or create a schedule) on several repos at once, and clone any schedule into an editable copy (PRD #589).**
  The Schedules page's new Default jobs tab and the CLI's `schedule catalog enable`/`schedule create` both take multiple `--repo`s in one call, creating one independent, separately tunable schedule per repo; `schedule clone` (web and CLI) copies any schedule, default or custom, into a fresh editable one, and cloning a default lifts its prompt lock so the baked text becomes ordinary editable content.
- **Enabling a sweep default warns before it can silently match nothing (PRD #589).**
  Enabling a sweep default, or creating or editing any sweep schedule's label selector, now checks whether the selector label exists on the target repo and offers to create it (`--create-missing-labels` on the CLI, a confirm prompt in the web); the check is advisory and never blocks the schedule, including when the check itself can't reach the forge.
- **`uzi run wait --min-plan-seq`, plus driver-hazard fixes in the uzi-cli Send-to-uzi playbook ([#606](https://github.com/vtmocanu/uzi/issues/606)).**
  `uzi run wait` gained a `--min-plan-seq` gate so a scripted driver can wait until the plan has actually advanced past a known point rather than racing the plan turn.
- **Capability hints on non-issue run kinds and an allowlist-aware capability count ([#605](https://github.com/vtmocanu/uzi/issues/605)).**
  A follow-up to the PRD #84 capability surface: the plan-time capability hint now shows on non-issue run kinds too, the online-worker count is allowlist-aware, and the capability vocabulary is sourced from one place.

### Changed

- **Builtin agent templates now track a versioned upstream role library ([#85](https://github.com/vtmocanu/uzi/issues/85)).**
  The eleven library builtin roles carry an upstream version stamp and a drift test that fails when a shipped builtin falls behind the versioned role library, so the builtins can no longer silently drift from upstream. `lead` stays product-only and unstamped.
- **Dependency: `github.com/slack-go/slack` updated to v0.28.0 ([#598](https://github.com/vtmocanu/uzi/pull/598)).**

### Fixed

- **A scheduled prompt run that commits nothing no longer opens an empty merge request ([#341](https://github.com/vtmocanu/uzi/issues/341)).**
  A `prompt`-kind scheduled run that makes no commits now completes report-only, the same as an `issue` run that finds nothing to change, instead of pushing an empty branch and opening a merge request with no diff, which is what lets the report-only default jobs (test improvement, docs hygiene, bug hunt) fire nightly without leaving empty MRs behind on a quiet week.
- **A seeded `--agent-source repo` run against a clone with no agent roster now falls back to your own templates instead of running with zero subagents ([#231](https://github.com/vtmocanu/uzi/issues/231)).**
  A well-formed `source: "repo"` selection against a clone that turns out to have no `.claude/agents/` is unsatisfiable; the worker now falls back to your own agent templates and records the fallback in the run feed, rather than handing implement an empty subagent map.

### Security

- **Reject Unicode control and format characters in agent-template and skill descriptions ([#166](https://github.com/vtmocanu/uzi/issues/166)).**
  Template and skill descriptions render into another user's tooltip (`title=` attribute), so they cross a principal boundary; the write API now rejects descriptions carrying Unicode control (Cc) or format (Cf) characters such as zero-width joiners and bidi overrides, closing a gap where the old gate caught only control characters. Descriptions rendered exclusively to their own author are unaffected.

## [0.57.0] - 2026-08-22
<!-- release-title: approve-freeze instrumentation + rate-limit countdown -->

### Added

- **TUI: rate-limit reset countdown on the ACCOUNTS panel and board strip ([#588](https://github.com/vtmocanu/uzi/pull/588), [#592](https://github.com/vtmocanu/uzi/pull/592)).**
  The ACCOUNTS panel and board status strip now show a live countdown to each Anthropic token's rate-limit reset, so a limited account's free-up time is visible at a glance instead of a bare timestamp.

### Fixed

- **Board: a persistent nudge to set Column-by to "uzi Status" ([#595](https://github.com/vtmocanu/uzi/pull/595), PRD #593).**
  After uzi provisions or auto-creates its own "uzi Status" field the GitHub board keeps grouping by the built-in Status and hides the values uzi writes, so the sync panel now shows a persistent hint telling you to set Column by to "uzi Status" and save the view.

### Internal

- **Instrumented the approve-time milestone freeze to pin the #259 visibility gap ([#604](https://github.com/vtmocanu/uzi/pull/604), issue [#260](https://github.com/vtmocanu/uzi/pull/260)).**
  submitApproval now logs the live milestones_frozen / milestones_candidate / updated_at on both sides of the approve-time freeze so a future human-gated milestone run reveals why the primary freeze once read the candidate as NULL, with the pathological case (a candidate present yet frozen left NULL) raised to a warn carrying a stable signature; the freeze itself is unchanged.
- **knip: promoted the web and agent unused-export tiers to error ([#599](https://github.com/vtmocanu/uzi/pull/599), [#600](https://github.com/vtmocanu/uzi/pull/600)).**
  Burned down the remaining unused exports in web and agent and promoted the unused-export tier from warn to error in both, so a newly unused export now fails the gate.
- **TUI: deduped the int64 pointer helper and covered the countdown-widened strip clamp ([#594](https://github.com/vtmocanu/uzi/pull/594)).**
  Test and helper cleanup following the rate-limit countdown work.
- **Added a local issue-triage skill (ee903ef3).**
  A backlog triage workflow (auto-pick, plain-English explain, freshness check, sweep-label) that takes one un-triaged issue to a queued decision.
- **Docs: PRD and documentation updates with no shipping code (3594f5ee, 0524cb6e, 8da3fddf, 201453b3, 8cb8954b, 23023ba4).**
  Agent-source repo-sync PRD #602, drift-check refresh for PRD #85, scheduled-jobs and self-improve PRDs #589/#590, PRD #593 Part B design-note park, PRD #603, and a check-docs forward-reference fix that unblocked a red main.

## [0.56.0] - 2026-08-22
<!-- release-title: closed-issue Done status + Resync field-id fix -->

### Added

- **GitHub Projects sync: closed issues land in a Done status, and reopening restores the card (issue [#584](https://github.com/vtmocanu/uzi/issues/584), PRD #584).**
  Closing an issue now sets its linked board card's Status to a dedicated `Done` option and keeps the card tracked instead of leaving it stranded at its last-known Status; reopening the issue restores the card to its current column (or clears it to No Status). A uzi-created field (Provision or auto-create) and an adopted built-in GitHub Status field both get the `Done` option automatically; a `uzi Status` or custom field without one gets an advisory in the sync panel and `uzi project-sync status` instead, since uzi never adds an option to an existing field. Reverse sync is unchanged: a Done card never reopens or closes the issue.

### Fixed

- **GitHub Projects sync: Resync no longer re-points a uzi-Status board back to the built-in Status ([#585](https://github.com/vtmocanu/uzi/pull/585), PRD #582).**
  Resync now re-reads the exact field the link already points at (by its stored id) instead of re-resolving by name, so a board synced through uzi's own `uzi Status` field keeps using it across a Resync instead of silently switching to GitHub's built-in Status and leaving most columns unmatched.
- **check-docs: repointed a moved artifact reference in specs/ai.md (3aeffd8b).**
  `specs/ai.md` cited `prds/158-forge-read-tools.md` after it moved to `prds/done/`, which failed the docs check on every pull request that merged with main.

### Internal

- **Docs: clarified that run-log message content lives under `payload` ([#144](https://github.com/vtmocanu/uzi/pull/144)).**

## [0.55.1] - 2026-08-22
<!-- release-title: forge read tools reach the fact-checker -->

### Fixed

- **Forge read tools (`mcp__forge__*`) now reach the fact-checker subagent ([#583](https://github.com/vtmocanu/uzi/pull/583), issue [#581](https://github.com/vtmocanu/uzi/pull/581)).**
  The run-lane forge read tools shipped in PRD #158 only worked from the lead session; the fact-checker, the one agent granted them, got "No such tool available" because the SDK exposes the top-level in-process MCP server map to the lead only. Each allowlisted subagent that names a forge or findings tool now receives the in-process server by reference through its AgentDefinition.mcpServers, so the fact-checker can verify a claim against live forge issue, MR and CI state instead of the repo's own restatement of it.

## [0.55.0] - 2026-08-22
<!-- release-title: GitHub Projects sync adopt-first UX + safe column auto-create -->

### Added

- **GitHub Projects sync: Adopt-first UX, health-aware badge, and safe column auto-create ([#580](https://github.com/vtmocanu/uzi/pull/580), PRD #576).**
  The repo Projects-sync panel now defaults to Adopt with a terse explainer (Provision stays available for org-owned repos, the only place a bot can own the linked board), the repo-list "Sync" pill reflects real health (green when linked and error-free, amber or red on a sync error) from a new per-repo sync-health field, adopting a board now surfaces the columns it had to skip and offers a one-click Resync, adopt seeds asynchronously so it no longer returns a cosmetic 502, and a new auto-create action adds the missing columns via a fresh uzi-owned Status field with an atomic marker reset so it can never strip labels off real issues.
- **Boards page: remember the selected forge and grow the board list naturally ([#579](https://github.com/vtmocanu/uzi/pull/579), issue [#578](https://github.com/vtmocanu/uzi/pull/578)).**
  The Boards page remembers your selected forge connection for 7 days (validated against the live list on load, falling back to the first connection if it was since removed), and the board list grows down the page like /schedules instead of sitting inside a padded scroll container.
- **TUI: full-width account meters and the lead context-window meter on the run-detail rail ([#577](https://github.com/vtmocanu/uzi/pull/577), issue [#574](https://github.com/vtmocanu/uzi/pull/574)).**
  The `uzi tui` run-detail rail now shows full-width per-account usage meters alongside the lead's context-window meter.

### Fixed

- **Reverse Projects-sync can no longer bulk-strip labels off real issues ([#580](https://github.com/vtmocanu/uzi/pull/580), PRD #576).**
  The reverse Status-to-label sync now counts a tick's destructive label moves before executing any and aborts the whole tick when they exceed a relative cap, so a Status field edited or cleared out from under uzi (or a botched option reconcile) cannot cascade into mass label-stripping on the real forge issues.
- **In-app changelog renders each bullet's bold title on its own line ([#575](https://github.com/vtmocanu/uzi/pull/575), issue [#573](https://github.com/vtmocanu/uzi/pull/573)).**
  The in-app changelog view renders each bullet's bold title on its own physical line with the description directly below, matching the GitHub release-notes layout.

## [0.54.0] - 2026-08-22

### Added

- **GitHub Projects sync: board visibility and collaborator sharing from the UI ([#568](https://github.com/vtmocanu/uzi/pull/568), PRD #534 follow-up).**
  The repo's Board-access panel can now read and flip the linked GitHub Project board's public/private visibility and grant or revoke Reader access by GitHub username, via four owner-or-admin routes on the github-only ProjectBoardSyncer; sharing is write-only since GitHub exposes no readable collaborator list.
- **TUI: lead context-window meter on the run view's lead row ([#570](https://github.com/vtmocanu/uzi/pull/570), issue [#565](https://github.com/vtmocanu/uzi/pull/565)).**
  The lead's crew-rail row in the `uzi tui` run view now shows an inline context-window meter (a bar plus percentage) with cool, molten, and near-full states, mirroring the web meter shipped in PRD #516.

### Fixed

- **Board card titles no longer overflow on a long unbreakable token ([#566](https://github.com/vtmocanu/uzi/pull/566), issue [#562](https://github.com/vtmocanu/uzi/pull/562)).**
  A long unbreakable token in a board card's title now wraps instead of overflowing the card.
- **Revert the no-op SDK todo-tools flag (issue [#561](https://github.com/vtmocanu/uzi/issues/561), reverting [#550](https://github.com/vtmocanu/uzi/pull/550)).**
  `CLAUDE_CODE_ENABLE_TODO_TOOLS=1`, added in 0.53.0 (whose notes billed it as "Restore SDK todo/task tools"), is a no-op on claude-agent-sdk 0.3.233: TodoWrite was removed from the SDK and no flag surfaces it, so the flag and its comment are removed. The SDK 0.3.233 pin ([#531](https://github.com/vtmocanu/uzi/pull/531)) is unchanged, and `CLAUDE_CODE_ENABLE_TASKS` is deliberately not added since the Task tools already work as deferred tools.

## [0.53.1] - 2026-08-22
<!-- release-title: automatic worker-image roll + interactive-task hardening -->

### Changed

- **Hosted-worker image roll is now automatic at release time.**
  `scripts/worker-tag-autobump.sh <version>`, run while cutting a release, bumps `deploy/chart/values.yaml` `workers.image.tag` (and the decouple assert's `PINNED_TAG`) to the release version only when the agent image's runtime surface (`agent/src`, agent deps, `agent/templates`, `agent/devbox-global`, `agent/bin`, `agent/tsconfig.json`) changed since the currently-pinned tag, so an app-only release still rolls zero workers (PRD #422) while an agent change rolls the fleet, draining in-flight runs, with no manual tag edit. Applied this cycle, the worker pin is bumped to 0.53.1, so the hosted fleet rolls to the current agent image.

### Fixed

- **Interactive task-run post-ship hardening ([#558](https://github.com/vtmocanu/uzi/pull/558), PRD #517).**
  Follow-up fixes to the interactive/long-lived task runs shipped in 0.52.0: a wake guard against spurious follow-up wakeups, a stop-on-crash wind-down path so a crashed interactive run finalizes instead of parking forever, `wallScaled` budget handling, and an `awaiting_followup` mock for the web tests.
<!-- coverage: merge 8b2d60a30 = the #558 hardening described in this bullet (its merge-commit message carries no issue number, so the coverage oracle needs the SHA cited here) -->

## [0.53.0] - 2026-08-22
<!-- release-title: GitHub Projects sync kill-switch + per-repo UI, merged-MR state, SDK todo tools -->

### Added

- **GitHub Projects sync: admin kill-switch and per-repo web UI ([#555](https://github.com/vtmocanu/uzi/pull/555), PRD #364).**
  The per-repo Projects-sync routes (status, adopt, provision, disable) move off the `/admin` group to the per-repo `/repos/{id}/github-project-sync*` group behind an owner-or-admin guard, a new admin Instance-settings toggle (`github_project_sync_enabled`, default off) gates the feature instance-wide, and the Repos page gains the per-repo sync controls.

### Fixed

- **Record "merged" MR state for cleanly-merged PRs ([#551](https://github.com/vtmocanu/uzi/pull/551), issue [#527](https://github.com/vtmocanu/uzi/pull/527)).**
  A run's "merged" badge almost never appeared because `runs.mr_state` was only written while the issue was still open, yet merging a PR closes the issue before the state is recorded; MR-watch now keeps observing a completed run's MR after its issue closes (a new Lane B) so `merged`/`closed` is recorded, and backfills historical merged PRs.
- **Restore SDK todo/task tools dropped by claude-agent-sdk 0.3.233 ([#550](https://github.com/vtmocanu/uzi/pull/550), issue [#549](https://github.com/vtmocanu/uzi/pull/549)).**
  claude-agent-sdk 0.3.233 removes the todo/task-tracking tools (TodoWrite, TaskCreate/Get/Update/List) from the default surface on newer models, silently stripping them from the lead and its inherit-all subagents; `buildSdkEnv` now sets `CLAUDE_CODE_ENABLE_TODO_TOOLS=1` to restore the pre-0.3.233 surface everywhere, inert where a strict allowlist or deny hook already constrains the surface.

### Internal

- **Dependency bump ([#531](https://github.com/vtmocanu/uzi/pull/531)).**
  `@anthropic-ai/claude-agent-sdk` to v0.3.233.

## [0.52.0] - 2026-08-22
<!-- release-title: interactive task runs, context + rate-limit meters, board sort, forge-connect UX -->

### Added

- **Interactive, long-lived task runs ([#540](https://github.com/vtmocanu/uzi/pull/540), PRD #517).**
  `uzi handoff --interactive` keeps a task run alive past a clean `signal_done` instead of finalizing it: the run checkpoint-pushes its branch and parks in a new non-terminal `awaiting_followup` status, holding the same agent session, clone and branch open. `uzi run follow-up <id>` wakes it for another turn with full context (no history replay); the new `uzi run stop <id>` winds it down gracefully (finalize, optional MR) as a distinct alternative to the hard-abort `run cancel`; a forgotten park is finalized by a 30-minute worker-side idle timeout (`WORKER_TASK_IDLE_TIMEOUT`), with the existing stale-worker requeue as the dead-worker backstop.
- **Lead context-window meter ([#538](https://github.com/vtmocanu/uzi/pull/538), PRD #516).**
  The run Activity panel shows the lead session's live context-window fill (read from the SDK once per lead turn) as a meter on the lead lane plus a micro-meter on the lead crew chip, predicting autocompaction. Subagent lanes get no meter (the SDK exposes only the main-loop window). This is window fill, distinct from the token spend the run page already tallies.
- **TUI run-detail account meters ([#536](https://github.com/vtmocanu/uzi/pull/536), PRD #530).**
  The `uzi tui` run-detail view renders per-account 5h/7d rate-limit meters under the milestone block, mirroring the board strip and web sidebar selection.
- **TUI rate-limit auto-refresh ([#539](https://github.com/vtmocanu/uzi/pull/539), PRD #533).**
  The TUI rate-limit strip now polls its meters on its own 60s ticker instead of freezing at launch, matching the web sidebar cadence.
- **Board sort direction toggle ([#544](https://github.com/vtmocanu/uzi/pull/544), PRD #412).**
  The board's Sort control gains an ascending/descending toggle (disabled for Manual); each mode keeps its previous direction as default. The chosen sort applies to every lane including Closed, which was previously always pinned to issue-number order.
- **Forge connect base-URL and type sync ([#542](https://github.com/vtmocanu/uzi/pull/542), PRD #337).**
  The connect form two-way-syncs the base URL and forge type (selecting a type fills the default URL; a recognized URL infers the type), and the PAT field gains a reveal/hide toggle. The reveal is client-only over what the user is typing; no stored secret is ever surfaced.
- **Worker cordon/drain signal ([#546](https://github.com/vtmocanu/uzi/pull/546), PRD #496).**
  The Workers list and `uzi worker list` now show when a hosted worker is draining/cordoned (finishing its current runs but not claiming new ones), so an online worker with a free slot that is not picking up a queued run reads as an in-progress roll rather than a bug.

### Changed

- **TUI floor-strip account separator ([#537](https://github.com/vtmocanu/uzi/pull/537), issue [#532](https://github.com/vtmocanu/uzi/pull/532)).**
  The factory-floor rate-limit strip replaces the faint " · " token separator with a per-account left accent bar, tinted alarm-red when the account's peak 5h/7d window is at or above the danger threshold and faint otherwise.

### Fixed

- **Bound server-controlled strings at the terminal ([#541](https://github.com/vtmocanu/uzi/pull/541), issue [#220](https://github.com/vtmocanu/uzi/pull/220)).**
  Three server-controlled strings that reached the terminal unbounded (an error body and the device-login user-code and email) now pass through the truncating `cellText` (200-byte cap), so a hostile server response cannot flood the CLI output.
- **Warn the lead when a resume re-clone destroyed the tree ([#543](https://github.com/vtmocanu/uzi/pull/543), issue [#222](https://github.com/vtmocanu/uzi/pull/222)).**
  On a resumed run whose re-clone rebuilt the working tree, the first implement turn now warns the lead that a queued follow-up written against the old tree may not reflect surviving work, so a stale steer input is not acted on as if that work were present.

### Internal

- **Web suite flake fix ([#545](https://github.com/vtmocanu/uzi/pull/545), issue [#227](https://github.com/vtmocanu/uzi/pull/227)).**
  The JudgePanel poll-cap tests no longer drive a 149-turn timer chain (the cap is now an injectable prop), removing the per-test timeout bumps and the CI-contention flake without loosening coverage.

## [0.51.0] - 2026-08-21
<!-- release-title: capability-aware scheduling, wait-on-limit on by default, live runs list -->

### Added

- **Capability-aware run scheduling and plan gate ([#510](https://github.com/vtmocanu/uzi/pull/510), [#523](https://github.com/vtmocanu/uzi/pull/523), PRD #84).**
  Runs now carry a required-capability set ({docker, jvm}) inferred at plan time, and repos carry a static hint; both the worker-claim predicate and an authoritative approval-gate check route a run only to a worker whose effective capabilities satisfy it. A queued run with no eligible worker surfaces a capability-specific health reason instead of the generic "waiting for a worker", the plan-approval gate shows met/unmet capability chips with a "run without <caps>" override, and `uzi run get` prints the requirement rows. Gated by a `capability_aware_scheduling` admin kill-switch (default on).
- **Contextual in-app documentation links ([#497](https://github.com/vtmocanu/uzi/pull/497), PRD #57).**
  Settings, management, and admin surfaces now link directly to their in-app guide, via a shared DocLink component and a slug registry.
- **Per-repo Setup indicator ([#508](https://github.com/vtmocanu/uzi/pull/508), PRD #361).**
  The Repos page shows a Setup chip and popover for a repo that is not yet Docker-allowlisted, and a run queued behind that restriction now surfaces the specific reason.
- **Live-updating runs list ([#522](https://github.com/vtmocanu/uzi/pull/522), PRD #518).**
  The /runs list refreshes on its own while the tab is visible instead of needing a manual browser reload.
- **`uzi review file`: file judge recommendations from the CLI ([#504](https://github.com/vtmocanu/uzi/pull/504), [#507](https://github.com/vtmocanu/uzi/pull/507), PRD #365).**
  A new CLI command files a judge recommendation as a forge issue or a run finding; the underlying file/dismiss endpoints move to Bearer-capable `RequireUser` auth.
- **TUI: clickable issue link and rate-limit meters ([#528](https://github.com/vtmocanu/uzi/pull/528)).**
  The factory-floor board and the run-detail header render the forge issue id as an OSC-8 hyperlink (plain text under NO_COLOR/Ascii terminals), and the board shows the viewer's own Anthropic 5h/7d rate-limit meters, mirroring the web sidebar.

### Changed

- **wait-on-limit now defaults ON ([#526](https://github.com/vtmocanu/uzi/pull/526), [#520](https://github.com/vtmocanu/uzi/pull/520)).**
  New and existing users default to parking-and-resuming a run that hits an Anthropic usage limit instead of failing it; judge runs still never park. The change is a one-way backfill via migration.
- **Lead agent run-context guidance ([#511](https://github.com/vtmocanu/uzi/pull/511), PRD #501).**
  The builtin `lead` template and the autopilot plan prompts now tell the lead three things it previously learned too late: (A) the Bash working directory persists across tool calls and the run starts at the worktree root, so relative-path greps and `cd`s should use absolute paths or re-`cd` from root (generalized from the integration-gate-only note); (B) on an autopilot run (no human in the loop) it is told up front, at planning time, to resolve open decisions on best judgment and record the assumption rather than spending an `ask_user` round-trip; and (C) any commit landing after a clean review, including the lead's own edits, re-opens a read-only validator wave over the new range before the run is signalled done.
- **Cancel/reject steering reason is now captured ([#521](https://github.com/vtmocanu/uzi/pull/521), PRD #503).**
  `uzi run reject` now requires a reason (pass `-m`, or pipe it on stdin) instead of accepting an empty one, and the reason is persisted as the run's `failure_reason` on every reject path (it was previously dropped and replaced by a hardcoded "plan rejected" on the server-side path). `uzi run cancel` gains an optional `-m/--message`, stored on the run in a new nullable `runs.stop_reason` column on both cancel paths.
- **`submit_plan` names the `plan_md` key ([#515](https://github.com/vtmocanu/uzi/pull/515), [#502](https://github.com/vtmocanu/uzi/pull/502)).**
  The plan-submission tool schema and description now name the markdown key explicitly.
- **Dependency majors:**
  `@anthropic-ai/claude-agent-sdk` ([#493](https://github.com/vtmocanu/uzi/pull/493)), `golang.org/x/mod` ([#494](https://github.com/vtmocanu/uzi/pull/494)), and the Babel monorepo ([#495](https://github.com/vtmocanu/uzi/pull/495)).

### Fixed

- **Operator cancellations are no longer classified as agent failures or judged ([#521](https://github.com/vtmocanu/uzi/pull/521), PRD #503).**
  A run cancelled or plan-rejected while a live worker held it used to end `failed` with `fail_origin='agent_failure'`, so an operator cancellation was judged and blamed on the agent (polluting per-agent reliability signal). A live cancel now ends `cancelled` (and is never judged), and a live plan-rejection carries `fail_origin='plan_rejected'`, both matching the existing server-side paths.
- **Card issue-ref anchors ([#491](https://github.com/vtmocanu/uzi/pull/491), PRD #411).**
  Issue-ref links on run cards and the needs-attention strip are now valid sibling anchors, with restored badge tooltips and distinct link names under the stretched-link overlay.
- **GitHub Projects sync cleanup ([#489](https://github.com/vtmocanu/uzi/pull/489), PRD #364).**
  Dropped an orphan `ListGithubProjectSyncedRepos` query and a redundant clear on the forward-move item-missing branch.
- **Judge pre-scan false positives on repo shell functions ([#513](https://github.com/vtmocanu/uzi/pull/513)).**
  The command-not-found pre-scan no longer flags a repo's own shell functions (anchored to column-0 or a `;&|` boundary, excluding indented methods and IIFEs).

### Internal

- **Run-liveness sweep interval is now configurable ([#506](https://github.com/vtmocanu/uzi/pull/506), PRD #97).**
  A `SWEEP_INTERVAL` knob for the run-liveness sweeper, plus e2e-suite speedups.
- **Two `gate:repo` structural checks ([#514](https://github.com/vtmocanu/uzi/pull/514), PRD #500):**
  migration-number collision and binary/control-byte text-file detection.
- **Dev-team/product role-parity nudge ([#490](https://github.com/vtmocanu/uzi/pull/490)).**
  A non-gating nudge reporting any role present on one roster and absent from the other.

## [0.50.0] - 2026-08-21
<!-- release-title: board + Projects v2 sync, worker fleet roll, dependency majors -->

### Added

- **Bidirectional board to GitHub Projects v2 Status sync ([#478](https://github.com/vtmocanu/uzi/pull/478), PRD #364).** A uzi
  board column-label and its mapped GitHub Projects v2 Status field now stay in sync in
  both directions: moving a card on the uzi board updates the Projects v2 Status, and a
  Status change made in GitHub Projects flows back to the uzi board column.
- **Clickable issue links on runs ([#477](https://github.com/vtmocanu/uzi/pull/477), issue [#411](https://github.com/vtmocanu/uzi/pull/411)).** A run's originating forge issue
  number now links to the issue on the forge, from the runs list, run detail, dashboard,
  the board's needs-attention strip, and a schedule's Last fire panel, opening in a new
  tab like the existing Open MR button. Runs with no issue (task, CI-fix, prompt) show a
  muted kind chip instead of a dead link.
- **Repo-enable guardrail violations surfaced in the UI ([#483](https://github.com/vtmocanu/uzi/pull/483), issue [#345](https://github.com/vtmocanu/uzi/pull/345)).** The Repos
  page now renders the guardrail violations that block a repo from being enabled, with
  actionable copy and an accessible violations card instead of a generic failure.

### Changed

- **Hosted-worker fleet rolled to 0.50.0.** This release deliberately advances
  `workers.image.tag` to `0.50.0` (picking up the agent-side base-align fixes below and
  the TypeScript 7 toolchain upgrade). Under the PRD #422 surge model the controller
  cordons each busy worker and lets its in-flight run finish before rolling it, bounded
  by `workers.drainDeadline` (default 24h), so running work is drained rather than
  killed. `scripts/assert-worker-tag-decoupled.sh`'s pin is bumped to match.
- **TypeScript 7 (native compiler) in web and agent ([#486](https://github.com/vtmocanu/uzi/pull/486)).** Upgraded from the 5.x
  series to the native Go port; the app's own types needed no source changes, only the
  tooling that drives the compiler API programmatically.
- **Tailwind CSS v4 in web ([#487](https://github.com/vtmocanu/uzi/pull/487)).** Migrated the frontend from Tailwind v3 to v4 through
  the `@tailwindcss/postcss` plugin, keeping the existing tokenized JS config via
  `@config`.
- **Frontend framework majors: React 19 and React Router 7 ([#467](https://github.com/vtmocanu/uzi/pull/467)).**
- **Build and CI dependency majors: Docker 29 ([#464](https://github.com/vtmocanu/uzi/pull/464)), plus jsdom 30, Helm 4, Ruby 4, and
  cosign 3 / cosign-installer 4.**

### Fixed

- **Controller no longer leaks orphaned worker pods ([#480](https://github.com/vtmocanu/uzi/pull/480), issue [#360](https://github.com/vtmocanu/uzi/pull/360)).** The worker
  Deployment now sets a bounded `revisionHistoryLimit`, so wedged pods from superseded
  ReplicaSets are garbage-collected instead of accumulating.
- **base-align: conflict-reason truncation and resumed-branch edge ([#475](https://github.com/vtmocanu/uzi/pull/475), issue [#471](https://github.com/vtmocanu/uzi/pull/471)).**
  A follow-up to #470 that base-aligns the conflict-reason truncation, fixes the
  resumed-branch edge case, and corrects test naming.
- **Version popover: the Changelog button is reachable by mouse again ([#474](https://github.com/vtmocanu/uzi/pull/474)), and the
  stray hover underline on the PRDs link is removed ([#476](https://github.com/vtmocanu/uzi/pull/476)).** An 8px hover gap used to
  close the popover mid-transit before the pointer reached the Changelog button.
- **Mock result-frame fixtures now exhibit the divergence class they validate ([#481](https://github.com/vtmocanu/uzi/pull/481),
  issue [#199](https://github.com/vtmocanu/uzi/pull/199)).** The `num_turns` result-frame mocks are strictly decreasing, so they can
  actually reproduce the divergence the tests assert against.

### Security

- **Strip terminal-control and bidi override runes from derived chat titles ([#484](https://github.com/vtmocanu/uzi/pull/484), issue
  [#213](https://github.com/vtmocanu/uzi/pull/213)).** `deriveChatTitle` no longer lets ESC and Unicode bidi override characters into
  `runs.title`, which renders in the cross-tenant admin runs table.

## [0.49.0] - 2026-08-20
<!-- release-title: CLI contexts, in-app changelog, workflow-scope run recovery -->

### Added

- **In-app changelog / release notes panel ([#440](https://github.com/vtmocanu/uzi/pull/440),
  [#415](https://github.com/vtmocanu/uzi/issues/415)).** The web UI now renders the project
  CHANGELOG in an in-app panel, from a build-time bundle whose per-version body mirrors
  `scripts/changelog-section.sh` so what users read in-app matches the release notes exactly.
- **uzi CLI named contexts (multi-token profiles) ([#436](https://github.com/vtmocanu/uzi/pull/436),
  PRD [#427](https://github.com/vtmocanu/uzi/issues/427)).** A `--context`/`-c` persistent flag
  and `$UZI_CONTEXT` let the CLI hold several named API endpoints/tokens and switch between them,
  with precedence flag > `$UZI_CONTEXT` > configured current > default.
- **Explicit per-repo remove action ([#438](https://github.com/vtmocanu/uzi/pull/438),
  [#357](https://github.com/vtmocanu/uzi/issues/357)).** An owner-scoped `DELETE /api/repos/{id}`
  removes a disabled repo and cascades its derived data (runs, cached issues, board columns) via
  existing foreign keys, guarded 404 for missing/foreign and 409 for a still-enabled repo.
- **Anthropic credential shown in the TUI, plus board to run navigation
  ([#435](https://github.com/vtmocanu/uzi/pull/435)).** The board and run-detail views surface the
  active token label (gated the same way the web UI gates it), and the board now opens a run while
  run detail backs out, with the header collapsing to one row when width allows.
- **Sweep backfill: refill a skipped slot from the next eligible candidate
  ([#426](https://github.com/vtmocanu/uzi/pull/426), [#416](https://github.com/vtmocanu/uzi/issues/416)).**
  A sweep's `max_issues` cap now counts runs actually started rather than candidates matched, so a
  skipped candidate is backfilled from the next eligible one instead of wasting the slot.

### Changed

- **Surge upgrade: release the stack without killing in-flight runs
  ([#422](https://github.com/vtmocanu/uzi/issues/422)).** The hosted-worker image tag
  is now pinned to a concrete version (`workers.image.tag`) independent of
  `Chart.appVersion`, reversing the old Model-B lockstep — an app-only release
  (api/web/db/controller) renders an unchanged worker spec-hash and rolls **zero**
  worker pods, so in-flight runs keep running on the old worker (which talks to the new
  API unchanged). A deliberate worker-image roll now **cordons** a busy worker (new
  orthogonal `workers.draining_since` column + a `POST /api/controller/workers/{id}/drain`
  control-write) and lets its runs finish before rolling it, bounded by a configurable
  drain deadline (`workers.drainDeadline`, default 24h) with an operator force-roll
  override (`workers.forceRoll`); past the deadline the run takes the existing
  requeue-resume path. The upgrade badge compares hosted workers against the pinned
  worker version so an intentionally-pinned-behind worker is not flagged `outdated`. An
  additive-migration guard and an old-worker↔new-API skew test enforce the N-1
  compatibility this relies on. See `adr/0422-decouple-worker-version.md`. **Operator
  note:** an out-of-tree per-cluster values file that left `workers.image.tag` empty
  must now set a concrete pinned tag (the chart `required`-wraps it).
- **Incidental findings now fire on normal runs ([#459](https://github.com/vtmocanu/uzi/pull/459),
  PRD [#457](https://github.com/vtmocanu/uzi/issues/457)).** The incidental-findings capability and
  a short discovery nudge are injected at the agent-assembly seam, so builtin and shared-library
  repo agents both get them without editing agent files. A follow-up
  ([#461](https://github.com/vtmocanu/uzi/pull/461)) makes the nudge string derive the tool name
  rather than hardcode it, with no change to the emitted text.
- **Mid-run milestone progress reporting is enforced
  ([#437](https://github.com/vtmocanu/uzi/pull/437), PRD [#390](https://github.com/vtmocanu/uzi/issues/390)).**
  An all-empty or defaulted `report_progress` call now counts as no signal, so it can no longer
  overwrite real milestone progress, and the lead must actually mark milestones for the board to move.
- **Run-summary intent and plan render through hardened Markdown
  ([#424](https://github.com/vtmocanu/uzi/pull/424)).** The run-summary intent and plan cards now
  go through the same hardened `<Markdown>` sink the judge summary already uses (no raw HTML, unsafe
  characters stripped before parse), so model-emitted code spans, bold and lists render instead of
  showing as literal source.
- **TUI board milestone bar improvements
  ([#460](https://github.com/vtmocanu/uzi/pull/460), [#379](https://github.com/vtmocanu/uzi/issues/379)).**
  A milestone-structured run that has reported nothing yet draws a graphical empty bar (0/N) instead
  of `-/N` text, and the micro-bar now renders for up to 9 milestones (only 10+ fall back to text),
  with the column widened so a 9-cell bar fits.
- **Go dependency bumps.** `golang.org/x/mod` ([#433](https://github.com/vtmocanu/uzi/pull/433)),
  `golang.org/x/crypto` ([#432](https://github.com/vtmocanu/uzi/pull/432)),
  `gitlab.com/gitlab-org/api/client-go` v2 ([#431](https://github.com/vtmocanu/uzi/pull/431)),
  `github.com/yuin/goldmark` ([#430](https://github.com/vtmocanu/uzi/pull/430)), and
  `github.com/coreos/go-oidc/v3` ([#429](https://github.com/vtmocanu/uzi/pull/429)).
- **CI: KinD smoke unblocked under Helm 4 and gated on chart PRs
  ([#447](https://github.com/vtmocanu/uzi/pull/447)).**
- **Test-coverage hardening for viewer-identity, GetSkill and the AST checker
  ([#439](https://github.com/vtmocanu/uzi/pull/439), PRD [#97](https://github.com/vtmocanu/uzi/issues/97)).**
  Pins six `*ForViewer` handler call sites, adds `GetSkill` property tests, and tightens the AST
  inert-code checker so an admin-bypass mutation no longer passes silently.

### Fixed

- **A hosted worker cordoned mid-rollout no longer stays cordoned forever when the
  drift is reverted ([#458](https://github.com/vtmocanu/uzi/issues/458)).** If an
  operator reverted `workers.image.tag` after a busy hosted worker had been cordoned
  (`draining_since` set) but before it rolled, the worker kept heartbeating ("online")
  yet claimed no runs forever — `draining_since` was cleared only by a worker
  re-registering on an actual roll, and a reverted drift never rolls, so neither the
  drain deadline nor an operator force-roll could recover it. The controller now
  clears the cordon on its no-drift reconcile path (a new
  `DELETE /api/controller/workers/{id}/drain` uncordon control-write mirroring the
  cordon `POST`), so the worker resumes claiming within one reconcile tick instead of
  needing a manual pod restart. Follow-up to [#422](https://github.com/vtmocanu/uzi/issues/422); see
  `adr/0422-decouple-worker-version.md`.
- **Runs no longer lose all their work when a branch is behind `main` on `.github/workflows`
  ([#470](https://github.com/vtmocanu/uzi/pull/470), PRD [#456](https://github.com/vtmocanu/uzi/issues/456)).**
  A run whose branch was behind `main` had its finalize push rejected for a workflow-scope reason
  even though it touched no workflow file, losing the branch. The push broker now skips the
  workflow-scope block on that path and a typed `finalize_base_align_conflict` fail origin carries a
  clear reason. Relatedly, a run that genuinely does touch an unpushable `.github/workflows` file now
  fails early with a typed `workflow_scope_missing` reason and preserves the agent's diff
  ([#454](https://github.com/vtmocanu/uzi/pull/454), PRD [#377](https://github.com/vtmocanu/uzi/issues/377)),
  instead of failing opaquely at push time.
- **uzi handoff no longer fails auth for every CLI token
  ([#434](https://github.com/vtmocanu/uzi/pull/434)).** `POST /api/repos/{id}/task-runs` was in the
  cookie-only auth group, so the first forge-writing call `uzi handoff` makes exited with
  "authentication required" for a Bearer token; it now requires a user via the token-aware path like
  its dispatch sibling.
- **Conversation and run-archive lists sort by instant, not string
  ([#468](https://github.com/vtmocanu/uzi/pull/468)).** Same-second timestamps of differing
  fractional precision (`…:00Z` vs `…:00.5Z`) ordered wrong under a string compare; both lists now
  compare as instants.

## [0.48.0] - 2026-08-20
<!-- release-title: signed container images and chart -->

### Added

- **Container + chart signing (cosign keyless) and an optional signature-enforcing
  admission policy ([#414](https://github.com/vtmocanu/uzi/pull/414)).** `release.yml` now signs every published
  OCI artifact by digest with Sigstore keyless signing (the api, web, controller and
  agent images plus the Helm chart), using each release job's GitHub OIDC identity, so
  no signing key is stored. Signatures are separate `.sig` artifacts and do not change
  the image manifests. The chart ships an optional Kyverno `ClusterPolicy`
  (`imageVerification.enabled`, off by default, Audit-first) that admits a uzi image
  only if it carries a valid signature from the release workflow; see
  `docs/container-signing.md` for manual `cosign verify` and enforcement setup.

## [0.47.0] - 2026-08-20
<!-- release-title: automated GitHub Release notes -->

### Added

- **GitHub Releases with human-readable notes, generated from the CHANGELOG ([#413](https://github.com/vtmocanu/uzi/pull/413)).**
  Tagging `vX.Y.Z` now creates a GitHub Release whose body is that version's
  CHANGELOG section and whose title is `vX.Y.Z` plus an optional one-line summary
  (an HTML `release-title` marker placed under the section heading).
  `scripts/changelog-links.sh` keeps the Keep-a-Changelog compare-link footers
  current and turns uzi PR/issue citations into links, leaving `PRD #N` and
  cross-repo references (e.g. `k8s #119593`) plain; `release.yml` gates that those
  links are current and its new `publish-release` job creates the Release after
  every image and the chart have published.

## [0.46.1] - 2026-08-20

### Added

- **check-docs now gates stale backticked artifact paths ([#410](https://github.com/vtmocanu/uzi/pull/410), issue [#189](https://github.com/vtmocanu/uzi/pull/189)).**
  `web/scripts/check-docs.mjs` validates backticked `prds/…` and `adr/…` paths, not
  just Markdown links, so archiving a PRD into `prds/done/` can no longer silently
  rot its inbound backtick references. A `check-docs:ignore-path` marker opts out the
  didactic examples and the not-yet-created forward references. Landed with a one-off
  sweep repointing the existing stale references across the docs and specs to
  `prds/done/`.

### Changed

- **Dependency and CI maintenance.** Bumps `charm.land/lipgloss/v2` (v2.0.5 to
  v2.0.6) and `github.com/charmbracelet/x/ansi` (v0.11.7 to v0.11.8) ([#407](https://github.com/vtmocanu/uzi/pull/407)),
  `@anthropic-ai/claude-agent-sdk` (0.3.226 to 0.3.228) ([#406](https://github.com/vtmocanu/uzi/pull/406)), `tsx` (to 4.23.12)
  ([#405](https://github.com/vtmocanu/uzi/pull/405)), and `knip` (to 6.32.2) ([#409](https://github.com/vtmocanu/uzi/pull/409)); bumps the Docker and paths-filter GitHub
  Actions to their Node 24 majors ([#404](https://github.com/vtmocanu/uzi/pull/404)).

## [0.46.0] - 2026-08-19

### Added

- **Aggregated "all agents" lane and readable run transcript ([#402](https://github.com/vtmocanu/uzi/pull/402)).** The TUI
  run view gains a synthetic "all agents" lane, prepended and default-selected
  once a run has two or more actors, that interleaves every actor's frames in seq
  order with per-line speaker attribution. The transcript itself is rewritten for
  a person rather than a log reader: speaker and tool markers, a compact arg
  preview per tool call, and one-line result summaries. Follow-up fixes on the
  same PR pair each tool result with its own call by id (so parallel calls no
  longer render under the wrong tool), surface a failed call with a ✗ marker (a
  glyph, so it survives NO_COLOR), mark `thinking` frames so the model's internal
  reasoning is not read as its output, and preserve the selected lane across a
  rebuild so a run growing past one actor does not swap the view out from under
  you.
- **uzi handoff: ephemeral, branch-scoped task runs (PRD #400, #401).** A new
  `task` run kind and a `uzi handoff` CLI for ephemeral, branch-scoped runs that
  never open an MR, for driving a scoped change on an existing branch.
- **Memory-save nudges for structural counts and environment capability ([#395](https://github.com/vtmocanu/uzi/pull/395)).**
  `save_memory`'s volatile-snapshot nudge now also catches structural counts (row
  and column totals, field counts) and adds a nudge for claims that a tool or
  binary is present or absent in the worker environment. Both stay append-only
  advisory prose on an already-successful save (never an error), matching the
  existing nudges.

### Changed

- **ANDON terminal-UI redesign ([#399](https://github.com/vtmocanu/uzi/pull/399)).** The TUI is refreshed into a quiet, warm
  board where the only lit surface is whatever needs a human: tungsten chrome and
  andon amber attention replace the cyan brand and the solid status chips. The
  board gains NEEDS YOU / ON THE FLOOR / DONE triage bands, a per-row andon strip,
  colored status words, a milestone micro-bar, and a full-row selection highlight;
  the run-detail view gains a two-line priority header (so the live tag no longer
  clips), a one-row amber plan-gate band with inline approve/reject, and a
  hairline divider. NO_COLOR twins, light and dark, the D7 injection guards, and
  the transcript viewport invariant are all preserved.

### Fixed

- **Recurring schedules no longer replay the missed window on resume ([#396](https://github.com/vtmocanu/uzi/pull/396),
  [#397](https://github.com/vtmocanu/uzi/pull/397)).** Pausing a recurring schedule and resuming it later immediately fired
  the window missed while paused, because pause/resume flipped only `enabled` and
  left `next_fire_at` frozen in the past. Resume now recomputes `next_fire_at` to
  the next future cron occurrence in the same write that flips `enabled`. A parked
  (`status='error'`) schedule stays parked on resume, and `once` resume is
  unchanged.

## [0.45.0] - 2026-08-19

### Added

- **Plain-English run summaries (PRD #362, #387).** The worker now persists a
  short intent/plan/deltas summary per run, produced on a dedicated summary model:
  a new admin `summary_model` setting (default haiku) with a per-user override,
  wired through the issue-run claim end to end. Two review followups landed on top
  ([#392](https://github.com/vtmocanu/uzi/pull/392)): `summary_model` folds into the single `GetUserSettings` read instead of a
  second query, and the summary PRD-link resolver now prefers the `prds/*.md` core
  whose number matches the run's issue id (falling back to first-valid) so a body
  mentioning another PRD first summarizes against the right one.
- **Workers now see issue comments ([#381](https://github.com/vtmocanu/uzi/pull/381)).** A bounded, bot/system-filtered,
  nonce-fenced comment feed is added to the initial run instruction and to the live
  `get_issue` tool, so review guidance that lands in comments is no longer invisible
  to the agent. Backed by a new `Forge.ListIssueComments` across all three drivers
  and a `runs.issue_comments` snapshot captured at run creation (bot-self-filtered,
  fail-safe to omit when the bot user is unknown).
- **TUI milestone progress and full-height run viewer ([#380](https://github.com/vtmocanu/uzi/pull/380)).** A compact
  `M{done}/{total}` badge in a new MILE column on the board (the web `MilestoneBadge`
  twin), a per-milestone checklist in the run-detail crew rail, and a run-detail body
  that now fills the terminal height.
- **TUI live milestone refresh and collapsible crew ([#383](https://github.com/vtmocanu/uzi/pull/383)).** Milestone counts
  refresh live on the 2s board tick, the crew rail is collapsible (`c`) so a tall
  roster cannot hide the milestone block, the header shows run duration, and the
  steady-state live indicator folds into the header.
- **TUI board: hide finished runs and windowed scrolling ([#389](https://github.com/vtmocanu/uzi/pull/389)).** `[h]` hides
  finished runs (client-side, no refetch), the run list is windowed to terminal
  height with an N-M of T readout and cursor-following scroll that survives
  auto-refresh, judge markers lay out in fixed sub-columns, and `q` quits instantly.
- **Scheduled main-guard workflow ([#376](https://github.com/vtmocanu/uzi/pull/376)).** A scheduled and dispatchable
  re-validation of `main` that `[skip ci]` markers cannot suppress, plus a docs note
  that workflow-scope push rejections are expected by design and workflow files must
  be landed by a human.

### Changed

- **Run-health slow threshold scales to a run's frozen budget ([#323](https://github.com/vtmocanu/uzi/pull/323)).** A
  milestone-scaled run (frozen `budget_wall_seconds`, PRD #122) lives far longer than
  the global `RUN_TIMEOUT`, so the flat 45m `health_slow_seconds` flagged it slow for
  most of its life while it was actively working. The raw threshold now scales by the
  run's budget ratio before the per-run clamp (raised only when a scaled budget is
  frozen), so an 8h run flags at ~3h instead of 45m and unscaled runs are unchanged.
  Fixes the Slack nudge, web badge, and CLI surfaces at once.
- **00092 revise backfill gains live-DB coverage ([#187](https://github.com/vtmocanu/uzi/pull/187)).** The one divergence path
  no existing test could structurally see is now covered by a live-DB test, with a
  small refactor to `migrate.go` to make the backfill exercisable.

### Fixed

- **GitLab driver: guard nil elements in branch-protection loops ([#378](https://github.com/vtmocanu/uzi/pull/378)).**
  `DefaultBranchProtection` dereferenced each element of the push/merge access-level
  slices, so a forge returning a null array element (e.g.
  `{"push_access_levels":[null]}`) decoded to a nil pointer and panicked on the
  security-sensitive privcheck path against an allowlisted-but-untrusted forge. Both
  loops now skip nil elements, completing the nil-element hardening for this driver.
- **Board: restore horizontal column scroll, drop pinned headers ([#375](https://github.com/vtmocanu/uzi/pull/375)).** #367
  (shipped in v0.44.0) regressed the board: flex-wrap columns wrapped onto new rows
  and horizontal card scroll was gone. The board row is back to one horizontally
  scrolling row with fixed-width, non-shrinking lanes, and the ResizeObserver
  toolbar-measuring machinery is removed.
- **`e2e/run-store-it.sh` reports an unavailable throwaway-Postgres image as a
  framed infrastructure fault, not an opaque `docker run` error.** The script now
  checks for the image (`docker image inspect`) and, only if it is absent, pulls it
  with a visible status line before starting the container. A failed pull prints the
  same loud `INFRASTRUCTURE FAILURE … NO TESTS RAN` banner (on stderr, non-zero exit)
  that a readiness timeout already gets (issue [#171](https://github.com/vtmocanu/uzi/pull/171)), so an offline host or an
  unreachable registry can no longer masquerade as a raw Docker error one step
  earlier in the pipeline. A host with the image already cached is unaffected
  (`docker image inspect` is a local no-op, no registry call); this does not change
  the readiness-wait timeout behavior.

## [0.44.0] - 2026-08-18

### Changed

- **Board scrolls as one page with pinned column headers ([#370](https://github.com/vtmocanu/uzi/pull/370), issue [#367](https://github.com/vtmocanu/uzi/pull/367)).** The
  per-column bounded scroll box (`max-h-[70vh] overflow-y-auto`) is gone: columns now grow
  to fit their cards, the whole page scrolls vertically, and each column header pins under
  the sticky toolbar as you scroll past its cards. Columns fit the width and wrap to full
  width on narrow screens instead of opening a horizontal scroller. This supersedes PRD
  #304 Decision 3's bounded-scroll clause; the paged reveal (Show more / Collapse, page of
  50) is unchanged.

## [0.43.0] - 2026-08-18

### Added

- **Schedule repo repoint, and an enabled/disabled control on create.** `PATCH
  /api/schedules/{id}` can now change a schedule's `repo_id` (`uzi schedule edit --repo`,
  plus a repo selector in the web edit form), and both `uzi schedule create --enabled` and
  the web create form can start a schedule already paused. A repoint validates owner-scoped
  exactly like create; an issue-target schedule refuses a repoint with 422, because forge
  issue IIDs are repo-relative and a repoint would very likely resolve a different,
  unrelated issue at the same IID (issue [#344](https://github.com/vtmocanu/uzi/pull/344)).
- **specs/ai.md section-number uniqueness gate.** `task gate:repo` now runs
  `scripts/check-spec-numbering.sh`, a whole-file duplicate-section-number check with a
  liveness canary, so a colliding section number is caught in CI instead of slipping past a
  tail-only read. Uniqueness only, never order or gaps: the file is append-numbered by
  design (issue [#181](https://github.com/vtmocanu/uzi/pull/181)).

### Fixed

- **Board card and run view now agree on the planning phase for all whitespace.** The
  board's `has_plan_md` SQL predicate widened its `btrim` set from ASCII-only whitespace to
  Go's full `unicode.IsSpace` set, so a plan made entirely of Unicode whitespace (NBSP, em
  space, ideographic space) reads as absent on both the board card and the run DTO, matching
  the Go-side `strings.TrimSpace` (issue [#342](https://github.com/vtmocanu/uzi/pull/342)).

## [0.42.2] - 2026-08-17

### Added

- **Homebrew install for the `uzi` CLI.** The `uzi-cli` formula now publishes to the
  public tap `vtmocanu/homebrew-tap`, so `brew install vtmocanu/tap/uzi-cli` builds the
  CLI from the tag's public source tarball with no product-repo access (PRD #64).
  Reworked the formula to the shared public-tap shape the sibling formulae use and added
  the tap-publish workflow (bfffb8d6).

### Changed

- Final pre-public scrub of internal literals in net-new CI/chart/brew files (940082fa).

## [0.42.1] - 2026-08-17

### Changed

- **Public-GitHub migration groundwork: internal-data scrub + CI/registry rehome
  to GitHub Actions and GHCR.** Generalized internal hosts, cluster names, org
  identifiers, CIDRs, and PII across the app code, tests, fixtures, specs, docs,
  PRDs, and the Helm chart, and flipped the chart's default image references to
  `ghcr.io/vtmocanu/uzi/*` with public-friendly defaults (plain-Secret mode, no
  required pull secret, bundled simple-postgres). Ported the remaining CI from
  the retired GitLab pipeline to GitHub Actions: PR image-validation builds, a
  KinD chart-install smoke, and the `v*`-tag release that publishes the images and
  the OCI Helm chart to GHCR. Pinned the go1.26.6 toolchain so the vulncheck gate
  tracks patched stdlib. No behavior change to the shipping app. Commits:
  5e856d71, ad04c450, 779cd0b8, 53021eee, e8a054e6, 3eaab75f, f8258b9e, bf9e72bd,
  39eb9419, b180ec8b.

## [0.42.0] - 2026-08-17

### Added

- **Run queue priority: interactive runs claim ahead of background
  retrospection, with a manual expedite (PRD #320).** On a saturated worker pool,
  interactive runs (issue, ci_fix) are now claimed before background runs (judge,
  self_improve), and any queued run can be bumped to the front with a new
  owner-only **Expedite** action (`uzi run expedite <id>`, or the button on the
  Runs list and run page). An age-based fail-open (`RUN_BACKGROUND_GRACE`, default
  15m) guarantees a demoted run can never starve: once it has waited past the
  grace it is restored to normal priority. This is ordering only, never
  eligibility, and the Kanban board is unchanged. Adds migration
  `00130_run_priority.sql`. (PRD #320)

- **A run's planning phase is now visually distinct from its running phase
  ([#321](https://github.com/vtmocanu/uzi/pull/321)).** Before a plan is approved a run showed the same "running" badge it
  shows while implementing; it now reads "planning" (a new indigo tone) until the
  implement loop begins. The distinction is derived server-side from existing
  columns (no new status value, no migration) and renders consistently on the
  board, the Runs list, the run view, and the CLI. ([#321](https://github.com/vtmocanu/uzi/pull/321))

- **Cancel and steer a run from Chat and Slack, human-gated ([#322](https://github.com/vtmocanu/uzi/pull/322)).** Both chat
  surfaces can now stop a live run or send it a follow-up instruction without
  leaving the conversation. The chat agent gained two tools, `cancel_run` and
  `steer_run`, each of which only proposes a card — a danger **Cancel run**
  card (web) or button-plus-confirm (Slack), and a **steer** card with an
  editable follow-up (web) or a **Steer** button that asks you to reply in the
  thread with your instruction (Slack, no modal). The human's click or reply
  is what performs the write, and it funnels to the same owner-only
  `SubmitInput` endpoint the web run view and CLI already use (`cancel` /
  `follow_up` kinds), so a forged or foreign run is refused with 404 and an
  already-terminal one with 409, and no new endpoint or migration was added.
  Steering only applies to issue runs — pointed at a chat, it's refused with a
  message explaining that a chat's follow-ups go through the conversation
  itself.

### Changed

- **Routine dependency and CI-image bumps.** The `alpine` (to 3.24) and
  `alpine/helm` (to 3.21.3) CI base images, and the `tsx`, `postcss`, and
  `autoprefixer` dev dependencies, were bumped to their current releases.
- **Doc-link hygiene in archived PRDs.** Repaired broken relative links (mockup
  and ADR references) across 16 `prds/done/*.md` files, left dangling when those
  PRDs were moved a directory deeper.

### Fixed

- **Intermittent `test:web` failure `No "ApiError" export is defined on the
  "../lib/api" mock` ([#165](https://github.com/vtmocanu/uzi/pull/165)).** Four error-path tests would fail in CI and pass
  on retry at the same SHA. Root cause was a runtime import cycle: `lib/api.ts`
  imports `mockApi` at module top (for the MOCK_MODE `api` swap) and
  `mocks/mockApi.ts` imported the runtime values `ApiError` / `isTerminalRun`
  back from the `../lib/api` barrel, so under parallel first-load vitest's
  `importActual("../lib/api")` could observe the barrel before its deferred
  `ApiError` binding populated. `ApiError` moved to `lib/apiError.ts` and
  `TERMINAL_RUN_STATUSES` / `isTerminalRun` to `lib/runStatus.ts` (leaf modules,
  re-exported from the barrel so the public import surface is unchanged); the
  mock client imports the two runtime values from the leaves, making the graph
  acyclic. A deterministic guard test (`mocks/api-acyclic.test.ts`) fails if any
  mock-graph file reintroduces a runtime edge to the barrel.

- **Flaky agent wall-clock tests under CI load ([#162](https://github.com/vtmocanu/uzi/pull/162)).** Two agent tests
  (batcher-poison backoff, steering epoch) depended on real elapsed time and
  failed under runner CPU contention. They now drive a deterministic timer pump
  and event-driven waits instead of wall-clock deadlines, exercising the same
  batcher-backoff and steering-epoch behavior without the timing sensitivity.
  ([#162](https://github.com/vtmocanu/uzi/pull/162))

## [0.41.0] - 2026-08-16

### Changed

- **Findings now sits directly under Runs in the sidebar (`ad98e205`).** The
  Findings nav item moved up in the Work group to sit immediately below Runs,
  the run lane whose off-task findings it collects, instead of at the bottom of
  the group after Chat. Presentation-only; the route and badge are unchanged.

## [0.40.0] - 2026-08-16

### Added

- **`check-styles` build gate for unresolved Tailwind classes ([#170](https://github.com/vtmocanu/uzi/pull/170)).** A
  Tailwind utility whose stem is not in `tailwind.config.js` fails completely
  silently — no error, no build warning; the element just inherits (a shipped
  `text-warning`, where the token is `warn`, rendered grey instead of amber).
  `web/scripts/check-styles.mjs` now extracts class tokens from the TypeScript
  AST (`className` attributes and `cx()` args only, so comments and prose can
  never be mistaken for classes) and asks the project's own postcss+tailwind
  engine whether each color-family utility resolves, failing the build on any
  that do not. Runs in `npm run build` and in `task gate:web` / CI
  `validate:web`, alongside `check-docs`. Fixed the six latent offenders it
  surfaced (`bg-bg` → `bg-ink`, `border-line` → `border-edge`). ([#170](https://github.com/vtmocanu/uzi/pull/170))

- **`yamllint` is now baked into the default worker toolchain ([#330](https://github.com/vtmocanu/uzi/pull/330)).** Every
  worker image ships `yamllint` through the pinned devbox-global toolchain
  (1.37.1), restoring local fidelity of uzi's own `lint:yaml` gate on the
  worker: `task lint:yaml` previously failed open and printed SKIPPED because
  `yamllint` was absent, so a worker could not exercise that gate the way CI
  does. Same "every worker should have it" class as the already-baked
  `shellcheck`. ([#330](https://github.com/vtmocanu/uzi/pull/330))

- **`ux-designer` builtin agent template ([#314](https://github.com/vtmocanu/uzi/pull/314)).** uzi now ships a twelfth
  builtin role: a build-capable UX/UI design lead that sets opinionated visual
  and information-architecture direction, implements the frontend/UI (including
  mock/demo state), and validates it in a real browser — distinct from the
  read-only `web-ux` validator. It runs on `opus` and inherits the full
  toolset. Boot-seeded via `ReconcileBuiltinTemplates`; existing installs pick
  it up on the next boot. ([#314](https://github.com/vtmocanu/uzi/pull/314))
- **A stable `resume_lineage_break` tag now marks the one path that breaks
  `run_usage` resume lineage ([#334](https://github.com/vtmocanu/uzi/pull/334)).** When a resume is dropped and the
  runner starts a fresh SDK session because the claimed session's transcript
  is not resolvable on this worker (`agent/src/runner.ts`), both the run-feed
  status message and the worker's structured warning log now carry
  `event: "resume_lineage_break"` — a resolved resume records nothing. This
  is a measure-before-you-build instrument for #332: a maintainer can now run
  `select count(*) from run_messages where payload->>'event' =
  'resume_lineage_break'` to size how often the undercount actually happens
  before deciding whether #332's deferred Option B (a `lineage_epoch`
  schema+protocol change) is worth building. No change to `run_usage`, its
  fold, the merge, or the totals view. ([#334](https://github.com/vtmocanu/uzi/pull/334), [#332](https://github.com/vtmocanu/uzi/pull/332))
- **Role-aware in-app docs: admins now see the operator setup guides ([#75](https://github.com/vtmocanu/uzi/pull/75)).**
  The in-app `/docs` section gains an "Admin / operator" area alongside the
  existing user howtos, surfacing installation, configuration, OIDC/Keycloak,
  and vault threat-model pages — routable, indexed, and searchable — to any
  admin (`me.is_admin`). It's presentation-only: every `docs/*.md` file was
  already bundled to every browser, so this gates the index/routing/search on
  admin status rather than introducing a new access boundary, and operator
  pages carry no secrets.

- **Incidental findings: a worker can flag an off-task bug without stopping
  its run, and you file it later on your own schedule ([#333](https://github.com/vtmocanu/uzi/pull/333)).** A new
  `report_incidental_issue` tool on the run lane (issue/ci_fix/prompt/
  self_improve) lets an agent record a bug it noticed outside its current
  task and keep working — no blocking, no forge write. It surfaces as a blue
  card in the run's stream and a coalesced inbox + Slack notification (one
  DM per run, however many findings it flags), and collects into a per-repo
  **Findings** backlog (`/findings`) deduped on `(repo, location)` across
  every run, so a bug five runs independently trip over is one row reading
  "seen in 5 runs." Filing opens a real issue on your own forge connection
  with a server-assigned marker label; dismissing a finding keeps it gone —
  a later run re-reporting the identical bug does not re-notify you or
  reappear in the backlog, only a materially different finding at the same
  spot does. The worker never holds a forge credential at any point; you
  gate every filing, same as every other forge write in uzi. New CLI verbs:
  `uzi findings list/file/dismiss`. See [docs/findings.md](docs/findings.md).
  ([#333](https://github.com/vtmocanu/uzi/pull/333))

- **`uzi run revise` CLI verb steers a plan at the approval gate ([#335](https://github.com/vtmocanu/uzi/pull/335)).** The
  API already supported revising a queued plan at the approval gate; the CLI now
  exposes it as `uzi run revise`, so a plan can be steered from the command line
  without the web UI. ([#335](https://github.com/vtmocanu/uzi/pull/335))

### Changed

- **The judge deterministically downgrades high-confidence recommendations its
  signals cannot confirm ([#336](https://github.com/vtmocanu/uzi/pull/336)).** Implements `#81` proposal #4: a
  recommendation the judge cannot back with concrete trace signals is demoted
  from high confidence rather than surfaced as-is, reducing false-confident
  advice. ([#336](https://github.com/vtmocanu/uzi/pull/336), [#81](https://github.com/vtmocanu/uzi/pull/81))

- **The judge auto-dismisses recommendations targeting denylisted CLIs ([#167](https://github.com/vtmocanu/uzi/pull/167)).**
  A deterministic net behind the existing prompt-side fix: a recommendation that
  would steer a run toward a denylisted CLI is now dismissed automatically
  rather than relying on the prompt alone. ([#167](https://github.com/vtmocanu/uzi/pull/167))

- **Housekeeping and public-migration prep.** The Go module path was renamed to
  `github.com/vtmocanu/uzi` (`c7bbd9ac`); a batch of internal-only data was
  scrubbed ahead of the public release (`1df03bd7`); the deploy chart's
  per-cluster values were relocated and cleaned up (`5dbdb0be`); and CI's
  chart-render check was decoupled from the per-cluster values file
  (`ca489c62`, #16).

### Fixed

- **Resetting a builtin agent template to default and saving no longer
  re-marks it customized ([#339](https://github.com/vtmocanu/uzi/pull/339)).** Pressing **Reset to default** and then
  **Save changes** on a builtin agent template re-marked the row
  `customized=true` even when the saved content was byte-for-byte the shipped
  builtin, silently opting it out of the boot-time shipped-body auto-refresh
  (`RefreshPristineBuiltin`), with no drift badge to reveal it (the badge
  reflects content, not the flag). `UpdateAgentTemplate` now content-derives
  the `customized` flag — a builtin whose submitted content matches the
  shipped definition (per `agenttmpl.SameContent`) is stored
  `customized=false`, making "save the shipped body" idempotent with Reset so
  the row keeps tracking future shipped changes. ([#339](https://github.com/vtmocanu/uzi/pull/339))
- **The worker's message batcher no longer re-enters the PRD #108 no-backoff
  retry storm when bisection abandons its first probe.** When the api
  permanently rejected a batch (4xx poison) and the very first bisection probe
  then hit a transient (5xx/timeout) or a 413, `bisect` handed the whole batch
  back with nothing persisted, but `handleFailure`'s permanent arm still reset
  `consecutiveFailures`/`failingSince` and told `doFlush` to keep going — so it
  re-posted immediately with no backoff, and the `TRANSIENT_TRIP_MS` breaker
  never accrued because `failingSince` was wiped every pass (and `close()` could
  hang under a synchronously-resolving client). `bisect` now reports whether it
  made progress (a sub-batch was persisted, or the poison was isolated and
  tombstoned); a no-progress abandonment is treated as the transient failure it
  is — backing off and keeping the breaker clock running — while only genuine
  progress clears the streak.
- **An idle backgrounded tab no longer keeps its session alive forever via the
  favicon poll ([#331](https://github.com/vtmocanu/uzi/pull/331)).** The tab-icon poll (`useFavicon`) fetches `listRuns`
  every ~20s even while hidden, and `RequireAuth`'s rolling refresh re-minted the
  session on every authed request past half its TTL — so a backgrounded idle tab
  slid its own expiry forward indefinitely and never reached `AUTH_TOKEN_TTL`
  idle expiry. The favicon poll now marks its request passive (`X-Uzi-Passive:
  1`), and the middleware skips ONLY the rolling-refresh side-effect for passive
  requests; auth validation and CSRF are unchanged. Suppressing refresh can only
  make a session expire sooner, never later, so the client-sent marker is safe.
  ([#331](https://github.com/vtmocanu/uzi/pull/331))
- **`uzi run logs` no longer dies mid-body on a large run and prints an empty
  result that looks like "no messages" ([#160](https://github.com/vtmocanu/uzi/pull/160)).** The viewer messages endpoint
  (`GET /api/runs/{id}/messages`) now gzips its response and gained an opt-in
  `?limit=` (clamped to 1000; omitting it is unchanged and unbounded, so the
  web SPA sees no behavior change). `uzi run logs` fetches a run's history in
  bounded `?after=&limit=` pages internally and reassembles them, and it is
  now all-or-nothing: it prints the complete history or nothing at all, exiting
  non-zero on any page failure — a failed fetch can no longer be mistaken for
  a run with an empty log. Paging is entirely transparent; callers still pass
  only `--after`/`--follow`, not a page size. ([#160](https://github.com/vtmocanu/uzi/pull/160))
- **`e2e/run-store-it.sh` no longer masquerades a Postgres-readiness timeout as
  a passing/skipped test run ([#171](https://github.com/vtmocanu/uzi/pull/171)).** The throwaway-Postgres wait was a
  hard-coded 30s; on a daemon busy with mutation containers it timed out and the
  run ended with no package times and a `RUN=0 PASS=0 FAIL=0` log —
  indistinguishable from the false-green `.claude/rules/go.md` documents. The
  wait is now 120s and env-overridable (`UZI_STORE_IT_PG_WAIT_SECS`), and a
  readiness timeout prints a loud `INFRASTRUCTURE FAILURE … NO TESTS RAN` banner
  on stderr (non-zero exit) that cannot be read as a test result. Documented as
  a distinct cause of the double-zero signature in `.claude/rules/go.md`. ([#171](https://github.com/vtmocanu/uzi/pull/171))
- **The two `react-hooks/exhaustive-deps` suppressions PRD #103 M3 deferred are
  resolved as fixes, not baselined ([#200](https://github.com/vtmocanu/uzi/pull/200)).** Dashboard's first-load effect now
  lists `user?.is_admin` and WorkersSettings' `rebind` callback now lists
  `announce`; both `// eslint-disable-next-line` directives are removed. Each
  added dependency is inert — `announce` is a stable `useCallback([])` wrapper,
  and `user?.is_admin` is a stable boolean because `ProtectedRoute` renders the
  page only after auth resolves — so neither changes runtime behaviour, which is
  why the fix (rather than a permanent suppression) was the honest outcome. ([#200](https://github.com/vtmocanu/uzi/pull/200))

- **A completed run with an opened MR is no longer recorded as a total loss
  ([#329](https://github.com/vtmocanu/uzi/pull/329)).** The run-timeout sweeper and the worker's completion report could
  race: when the sweeper's `RUN_TIMEOUT` write landed first, it clobbered the
  worker's later completion. A genuine completion now supersedes a
  run-timeout failure, and the merge-request link is recorded independently
  of the final run status, so a run that opened an MR never displays
  "MR: none". ([#329](https://github.com/vtmocanu/uzi/pull/329))
- **A run's declared PRD-completion move is now auditable after the fact
  ([#150](https://github.com/vtmocanu/uzi/pull/150)).** The run DTO and `uzi run get` (human view, `--json`, and `--field`)
  now expose `prd_done_path` (the repo-relative path a run declared it moved a
  completed PRD to) and `prd_patch_settled_at` (the RFC3339 timestamp when the
  PRD-link patch lifecycle settled, null while still pending); the web run
  footer surfaces `prd_done_path` alone. All read-only and emitted only when
  set, so a run predating the feature is unchanged. ([#150](https://github.com/vtmocanu/uzi/pull/150))
- **Archiving a PRD to `prds/done/` no longer leaves broken inbound doc links
  ([#257](https://github.com/vtmocanu/uzi/pull/257)).** The `prd-lifecycle` skill now tells a run that performs
  `git mv prds/<file>.md prds/done/` to sweep the tree (`git grep -lF`) for
  relative links to the old path and repoint them to the new location in the
  same commit — so the archiving run fixes the links in files it never
  otherwise touches, instead of failing the `check-docs` gate at merge time.
  (`web/scripts/check-docs.mjs` already reports every broken link in one pass,
  so the surviving gap was the repoint, not the reporting.) ([#257](https://github.com/vtmocanu/uzi/pull/257))

- **Status-favicon review follow-ups from PRD #70 ([#73](https://github.com/vtmocanu/uzi/pull/73)).** Addresses the review
  follow-ups filed against the PRD #70 status-favicon work (MR !66), in
  `web/src/lib/favicon.ts`. ([#73](https://github.com/vtmocanu/uzi/pull/73), [#70](https://github.com/vtmocanu/uzi/pull/70))

### Security

- **Both hostile-forge DoS vectors closed across all three forge drivers
  ([#74](https://github.com/vtmocanu/uzi/pull/74)).** A semi-trusted (compromised) connected forge could crash the shared,
  multi-tenant api two ways, both symmetric across the gitlab, forgejo and
  github drivers. First, a job-log fetch buffered the entire response body in
  memory (inside the SDK) before the 16 MiB ceiling was evaluated, so a forge
  streaming a multi-GB log body could OOM the process; the gitlab and forgejo
  drivers now issue a raw request read through an `io.LimitReader` so the
  transfer itself is byte-bounded (github was already bounded), and the gitlab
  trace request additionally refuses redirects so a hostile 302 cannot replay
  the bot PAT cross-host. Second, a forge returning a one-element pipeline/run
  list whose single entry was JSON `null` dereferenced a nil pointer and
  panicked the poller's pipeline-sync tick; all three drivers now guard the nil
  element, and the poller's per-repo goroutine recovers any panic so one repo's
  hostile response degrades to skipping that repo's sync rather than crashing
  the api. ([#74](https://github.com/vtmocanu/uzi/pull/74))

- **Forge-driver pagination is now backstopped against an unbounded-loop DoS
  ([#338](https://github.com/vtmocanu/uzi/pull/338)).** A semi-trusted (compromised or buggy) connected forge could return a
  perpetually non-zero next page and drive any driver's paginating list call
  (projects, labels, issues, label events, pipeline jobs) forever — either
  growing the accumulator until the shared api OOMs, or spinning on empty pages
  that never grow it. Every accumulating loop in the gitlab, forgejo and github
  drivers now enforces two backstops: an item cap (bounds the memory-growth
  vector) and a page cap (bounds the empty-page spin the item cap would miss).
  On exceed the driver returns an ERROR, never a truncated slice — a partial
  fetch that looked complete would let `forgesvc.FullSync` treat it as
  authoritative and evict cached issues. Both caps are sized far above any real
  forge list, so only a misbehaving forge hits them. Mirrors the CLI's existing
  `RunLogs` / `maxLogsMessages` backstop. ([#338](https://github.com/vtmocanu/uzi/pull/338))

- **Terminal and label sanitization converged onto a single unsafe-char
  predicate ([#161](https://github.com/vtmocanu/uzi/pull/161)).** The remaining hand-rolled unsafe-character predicates now
  route through `termsafe.Unsafe`, and the Go and web test corpora are pinned
  together so the two stay in lockstep, closing the drift that let one surface
  sanitize differently from another. ([#161](https://github.com/vtmocanu/uzi/pull/161))

- **The worker-name delete announcement is now sanitized ([#173](https://github.com/vtmocanu/uzi/pull/173)).** The delete
  screen-reader announcement was the one `announce()` call whose "its visible
  counterpart is already sanitized" justification did not hold, so it could emit
  unsanitized worker-supplied text; it now sanitizes like the rest. ([#173](https://github.com/vtmocanu/uzi/pull/173))

- **WorkersSettings' two worker-name sanitizers converged ([#172](https://github.com/vtmocanu/uzi/pull/172)).** Three sites
  used `stripUnsafeChars` while one used `sanitizeLabel`; all four now use the
  same predicate, removing the inconsistency that could let one field accept
  what another rejected. ([#172](https://github.com/vtmocanu/uzi/pull/172))

## [0.39.0] - 2026-08-16

### Added

- **Board columns reorder by a drag-and-drop grip handle ([#318](https://github.com/vtmocanu/uzi/pull/318)).** The board
  Settings > COLUMNS editor now reorders columns by dragging a 6-dot grip
  handle, replacing the per-row up/down arrow buttons. It reuses the board
  cards' existing hand-rolled drag idiom (no new dependency) and still persists
  only on Save columns. The arrows were dropped entirely at the owner's
  direction, so column reordering is now pointer-only: a recorded,
  owner-accepted WCAG 2.1.1 keyboard/touch residual scoped to this editor (the
  board cards keep their keyboard fallback). ([#318](https://github.com/vtmocanu/uzi/pull/318))
- **Slack workspace state on the self-service notifications card ([#56](https://github.com/vtmocanu/uzi/pull/56)).** A
  non-admin user can now see why Slack DMs cannot send. The `/me/slack` link
  response carries a new server-derived `workspace` field that collapses the
  five manager connection states to four public values (unconfigured,
  connecting, connected, error) without leaking the error class. The Settings
  notifications card renders an alert above the link-state helpers and disables
  its controls when Slack is unconfigured. No new endpoint, no migration, and no
  change to the notification path. ([#56](https://github.com/vtmocanu/uzi/pull/56))

### Fixed

- **rollhealth names the flapping container on the flapping path ([#159](https://github.com/vtmocanu/uzi/pull/159)).** On a
  Ready-but-flapping rollout the stuck verdict and its named subject were
  computed from two different containers, so the reason, restart count, and exit
  code could describe a container other than the one that caused the verdict.
  Subject selection now prefers the flapping container (preserving init-first
  ordering), so every operator-facing field describes the same container. No
  behavior change on the not-Ready or blocking-container paths. ([#159](https://github.com/vtmocanu/uzi/pull/159))

### Changed

- **Mock/demo mode is guarded against drift ([#311](https://github.com/vtmocanu/uzi/pull/311)).** A CI mock-image build, a
  mock-mode route smoke test, and a realism guard over the demo usage fixtures
  keep the demo build in step with the real product, so a stale mock is caught
  in CI rather than in a live demo. Web and CI only, no product behavior change.
  One consequence: the Slack controls are disabled in demo mode, since the mock
  reports an unconfigured workspace. ([#311](https://github.com/vtmocanu/uzi/pull/311))

### Dependencies

- Bump `github.com/pressly/goose/v3` to 3.27.3, refreshing the api module's
  `golang.org/x/{crypto,sync,net,sys,text}` and `sethvargo/go-retry` in lockstep
  (c02c9910).
- Bump `github.com/go-chi/chi/v5` to 5.3.1 (6f197399).
- Bump the Kubernetes client libraries (`k8s.io/api`, `apimachinery`,
  `client-go`) to 0.36.3 in the controller (c5f9488f).
- Bump `@anthropic-ai/claude-agent-sdk` to 0.3.226 in the agent worker, with the
  lockfile regenerated to match (24a1a9b8).
- Re-pin the `golang:1.26` build-image digest (844a3855) and the
  `gcr.io/distroless/static-debian12:nonroot` runtime-image digest (6cc344cb) to
  current.

## [0.38.1] - 2026-08-15

### Fixed

- **TUI: the crew/transcript `│` separator no longer zigzags ([#327](https://github.com/vtmocanu/uzi/pull/327)).** In the
  `uzi tui` run-detail view, a crew-rail line longer than the fixed rail width
  (a long lane label such as "Sweep terraform occurrences + seed mapping", or a
  wide-rune role name) was left unpadded, so the column divider was pushed right
  and landed at a different position on each row. `joinColumns` now clamps every
  left cell to `laneRailWidth` visual columns (ANSI- and wide-rune-aware, via
  `ansi.Truncate` with an ellipsis) before padding, so the `│` sits at one fixed
  column on every row regardless of label or name length. Display-only fix; the
  `laneLabelCap` rune sanitation cap is unchanged. ([#327](https://github.com/vtmocanu/uzi/pull/327))

## [0.38.0] - 2026-08-15

### Added

- **Owner heads-up when an approved run drops a guard role (PRD #319 M3).**
  Approving a plan whose agent selection explicitly excludes a guard role
  (`spec-keeper` today) now sends the run owner one notification — an in-app
  inbox row plus a best-effort Slack DM — naming the dropped role. It fires only
  on an active exclusion (never on a role merely absent from a roster), only
  after the selection validates, and never blocks the approve. No new wire
  field, no migration, and no web/CLI change. ([#319](https://github.com/vtmocanu/uzi/pull/319))
- **Judge mode: off, optional, or enforced for everyone (PRD #69 M1).** A new
  admin **Enforce the judge on every run** setting combines with the
  existing kill-switch into three effective modes: off (the kill-switch
  always wins), optional (today's per-user opt-in), and enforced (every
  user who holds an Anthropic token is judged regardless of their own
  opt-in, still spent on their own token — an admin can force that judging
  *happens*, never redirect *who pays*). Enforcement also makes a per-user
  admin force-disable on the Users page inert while it's on, since one flag
  can't distinguish "an admin disabled you" from "you opted out." ([#69](https://github.com/vtmocanu/uzi/pull/69))
- **Per-user judge model override (PRD #69 M2).** A new **Settings → Run
  judge → Judge model** picker lets a user pin the model their own judge
  runs on, independent of the instance default; left on Inherit it falls
  back to the instance setting. Resolution happens at judge-claim time and
  always spends the run owner's own token — an admin still cannot redirect
  judge spend to another user's account. ([#69](https://github.com/vtmocanu/uzi/pull/69))
- **Per-user judge spend guards: cooldown and daily budget (PRD #69 M5).**
  Two admin-tuned, count-based settings — a per-user cooldown (default 60s,
  `0` disables it) and a per-user daily budget (default `0` = unlimited) —
  are checked before every judge is enqueued, in every mode. Both are
  best-effort and fail **open** on a settings-read error; a tripped guard
  skips the judge silently, with no notification and no queued run. Ships
  alongside the opus default below so a heavier judge can't recreate a
  runaway-cost loop on its own. ([#69](https://github.com/vtmocanu/uzi/pull/69))
- **Judge run cost and time are now visible (PRD #69 M6).** A judge run's
  own tokens, duration, and cost are captured the same way a work run's
  are, so they now fold into the owner's usage totals and render as a
  compact strip (Tokens in/out, Duration, Cost) on the reviewed run's judge
  panel. A judge run predating this change shows no strip rather than a
  fabricated zero. ([#69](https://github.com/vtmocanu/uzi/pull/69))
- **Judge accuracy: a trusted failure-class signal, and a pre-start skip
  (PRD #69 M7a).** The API now computes a closed-enum failure class for a
  failed run from server-owned axes (status, iteration count, and the
  terminal transition's own origin) — never by parsing the free-text
  failure reason — and hands it to the judge, which no longer recommends
  retry/backoff for a policy- or config-denied failure (a provisioning,
  credential, or guardrail block). A run that failed before its agent ever
  started (0 iterations, a pre-start infra origin) skips the judge entirely
  and gets a deterministic failure notification instead of an opus
  retrospective. ([#69](https://github.com/vtmocanu/uzi/pull/69))
- **Judge `cost_efficiency` recommendation category (PRD #325).** The
  retrospective judge can now surface quality-first cost-efficiency
  recommendations: ways a run could have reached the same outcome for fewer
  tokens, turns, or agents, without reducing correctness, verification depth, or
  code quality. It is triage-only (it appears in the backlog for
  resolve/dismiss/file-issue) and does not feed the self-improvement loop. Wired
  across the agent prompt, server validation, the DB CHECK, the CLI `--category`
  filter, and the web filter chip. ([#325](https://github.com/vtmocanu/uzi/pull/325))
- **Per-run tool provisioning without GitHub egress (PRD #123).** A tier-1 seed
  gate restricts a worker's tool allowlist to the baked toolchain and aliases
  baked binary-name mismatches, with the tier-2 denylist decision documented, so
  the provisioning path no longer depends on GitHub egress. ([#123](https://github.com/vtmocanu/uzi/pull/123))

### Changed

- **`uzi tui` redesigned as a factory shift board (PRD #325).** The run board
  now colours each row by status, glyphs it NO_COLOR-safe, and leads with a
  summary bar (`N runs · N needs you · N stalled`) and a HEALTH column that
  calls out stalled runs. Run detail gains two **focusable panes** — the crew
  rail and the transcript, moved between with `←`/`→` (or `tab`), `↑`/`↓`
  acting within the focused one — and a **follow-live** transcript (`● FOLLOWING`
  / `⏸ PAUSED ↓N new`, re-attach with `g`). Plan gates and clarifying questions
  each get their own attention banner, the judge review overlay shows a verdict
  severity chip, and the keybinding footer now fits one line. New: a hidden
  `uzi tui --demo` boots the real TUI over seeded fixtures with no server, and
  an offline screenshot harness (`api/cmd/uzi/uxlab/`) renders every state to
  PNG in light and dark. ([#325](https://github.com/vtmocanu/uzi/pull/325))
- **Diagnostic env reads `printenv PATH` / `printenv TMPDIR` are now allowed
  (PRD #319 M2).** The Bash guardrail permits a targeted read of the two
  non-secret diagnostic variables (allow iff the call has ≥1 argument and every
  argument is in `{PATH, TMPDIR}`), while enumeration (bare `env`/`printenv`)
  and any non-allowlisted or secret-bearing variable stay denied. The real
  containment remains the SDK's replacement subprocess env, which carries no
  secret in those vars. See `adr/0319`. ([#319](https://github.com/vtmocanu/uzi/pull/319))
- **Two lead-orchestrator prompt nudges (PRD #319 M4).** The builtin `lead`
  template now reserves independent-verifier fan-out for genuinely uncertain or
  post-implementation claims — no re-dispatching verifiers to re-read
  coordinates the lead already confirmed itself — and carries a PRD's exact
  literal tokens (e.g. NBSP vs ASCII space) verbatim into the plan. Re-applies
  to pristine builtin rows on the next boot. ([#319](https://github.com/vtmocanu/uzi/pull/319))
- **Default judge model is now `opus`, not `haiku` (PRD #69 M3, supersedes
  PRD #59).** The judge's recommendation half feeds self-improvement, so
  the strongest model is now the instance default; the per-user override
  and the admin instance setting are both still there as the cost levers.
  **Upgrade note:** an existing instance with the judge enabled and no
  `judge_model` pinned starts spending opus (roughly 5–15× haiku per run)
  right after upgrading — pin **Judge model** to `haiku` or `sonnet` first
  if you want to keep the previous cost. On a subscription-plan Anthropic
  token, an opus judge also spends plan/rate-limit quota rather than
  dollars, which can eat into what your real runs need — this matters most
  under the new enforced mode above. ([#69](https://github.com/vtmocanu/uzi/pull/69))
- **Web main content column widened to 1088px (`max-w-[68rem]`)**, alongside
  agent-browser session isolation for the web-ux and ux-designer dev agents.
  (ec65d87d)

### Fixed

- **Schedules: dropped the stray play glyph from the last-fire run chip.**
  (14411306)
- **The agent `ask_user` per-run deadline test is now deterministic**, removing a
  flaky-test source in the run harness. (71f6f6ca)

### Security

- **Untrusted Markdown strips Unicode bidi/format characters before render
  (PRD #319 M1).** The `<Markdown>` component (plan bodies at the approval gate,
  agent prose, chat, questions, feedback) now strips Cc/Cf control and
  bidirectional-override characters, closing a Trojan-Source / bidi-spoofing
  vector on untrusted LLM/forge text (cf. issue [#124](https://github.com/vtmocanu/uzi/pull/124)). Centralized in the
  component so every current and future untrusted sink is covered by
  construction; trusted docs rendering is unaffected. ([#319](https://github.com/vtmocanu/uzi/pull/319))

## [0.37.0] - 2026-08-14

### Changed

- **Runs page IA + UX tweaks (PRD #316).** `/runs` becomes a live console (your
  Active runs plus, for admins, the Factory card), with past runs moved to a
  searchable, date-grouped `/runs/history` tab that reveals 50 at a time like the
  board. The admin Factory card now lists other users' active runs only (your own
  already appear in Active). The sidebar collapse toggle folds into the footer
  cluster instead of consuming a full row, and the Schedules "Last fire"
  disclosure renders a proper SVG chevron rather than a broken glyph. Web-only; no
  new service and no new trust boundary. ([#316](https://github.com/vtmocanu/uzi/pull/316))

## [0.36.0] - 2026-08-14

### Changed

- **Web UX information-architecture restructure (PRD #315).** Workers becomes a
  first-class `/workers` page (out of Settings; `/settings/workers` redirects, and
  the `excludeSubpath` active-state hack is deleted). Admin consolidates into one
  5-tab shell reached from a single sidebar entry, and Settings splits into
  Account & tokens / Run defaults. The sidebar rate-limit meters become a per-user
  explicit choice: a "Show in sidebar" checkbox per token, the default token
  always pinned, persisted as a new additive `users.sidebar_token_ids` column
  (migration 00123, nullable, so an older server reads as default-only). Tab
  strips no longer jump on a tab switch (constant per-shell header), and the full
  admin nav fits a 1440x900 laptop with boards still expanded. No new service and
  no new trust boundary. ([#315](https://github.com/vtmocanu/uzi/pull/315))

### Fixed

- **The hosted worker's golangci-lint ratchet base is clamped to the true branch
  base.** On resume legs the base could resolve through the frozen
  `refs/heads/main` mirror, a stale ancestor of the branch's real base; #262 then
  advanced the clone's `origin/main` to that stale commit, so the
  new-from-merge-base ratchet regressed below the fork point and false-reddened
  pre-existing backlog in untouched files. When the default-branch commit is an
  ancestor-or-equal of the base SHA, `origin/<default>` is now pointed at the base
  SHA the lead computes, so the ratchet compares against the true fork point.
  `.golangci.yml` is untouched. ([#313](https://github.com/vtmocanu/uzi/pull/313))

## [0.35.0] - 2026-08-14

### Fixed

- **A healthy worker roll no longer badges "outdated".** The upgrade forecast's
  `upgrading_since` anchor was effectively per-release rather than per-incident:
  any transient not-Ready blip stamped it (set-if-NULL on a `rolling`/`stuck`
  report), and it only cleared when the registered version actually moved, so the
  blip's own restart re-registered at the same version and the anchor never
  cleared. The stale anchor then rode into the next upgrade and ceiling-gated the
  R2 check, making a perfectly healthy roll show "outdated" with attention set. The
  roll-health arm now also requires the reported tag to differ from the worker's
  own registered version, and it fails closed on an unparseable tag (the audited
  suppression hole where a `+_x`-style invalid build-metadata tag would strip-equal
  the version and never arm). Keyed on the worker's authenticated `version`, not
  the forgeable target tag. ([#155](https://github.com/vtmocanu/uzi/pull/155))

- **The worker's `RunKind` TypeScript union now includes `chat`.** The DB CHECK on
  `runs.kind` allows six kinds (`issue`, `ci_fix`, `chat`, `judge`,
  `self_improve`, `prompt`) but `agent/src/protocol.ts` omitted `chat`, so a
  `switch` on `RunKind` was non-exhaustive at runtime while typechecking as
  exhaustive: a real `chat` row would walk straight into a `default:
  assertNever(kind)` the compiler had certified. `RunKind` is now derived from a
  `RUN_KINDS` tuple, and a new parity test asserts the union matches the live DB
  constraint set both ways so the two cannot silently drift again. ([#142](https://github.com/vtmocanu/uzi/pull/142))

### Security

- **CI Go image bumped to go1.26.6 and nanoid to 3.3.18.** The absolute vulncheck
  gates went red on freshly-published advisories rather than on any change:
  govulncheck flagged six (api) and four (controller) called Go standard-library
  vulnerabilities, all fixed in go1.26.6, and npm audit flagged nanoid 3.3.17
  (high, GHSA-2v37-7h3g-55p8). The `golang:1.26` CI image is re-pinned to its
  go1.26.6 digest and nanoid is bumped in the web lockfile (transitive via
  postcss); react-router stays as a below-threshold moderate. (f2deab30)

## [0.34.0] - 2026-08-13

### Changed

- **The rate-limit burn-rate forecast meter was reworked after a live design
  review.** The projection "ghost" used to be a translucent bar painted over the
  fill, which left a dark notch between the two when their tones matched and an
  orange blend when they differed (a gold current-usage fill under a coral
  projection). It now draws the ghost from zero to the projected point as the
  bottom layer with the opaque fill on top, so current usage flows into the
  projection with no seam at any bar size. The overflow marker changed from a `»`
  text glyph (which needed a platform-specific vertical nudge) to a viewBox-centered
  inline SVG chevron, and its placement now reads the trajectory: an "over"
  projection sits just outside the bar's right edge (overshooting past the cap),
  while an "on pace" one sits flush at the end of the current fill, pointing toward
  the projection. The shared meter atom and the worker CPU/memory gauges are
  unchanged. Web-only, and it supersedes the 0.33.0 marker fix. ([#309](https://github.com/vtmocanu/uzi/pull/309))

## [0.33.0] - 2026-08-13

### Fixed

- **The `»` rate-limit forecast overflow marker is now vertically centered on its
  bar.** It rendered about 1.24px below the bar midline, most visible on the thin
  sidebar micro-meter where that offset is a quarter of the bar height: the
  `-translate-y-1/2` positioning centers the glyph's box, but the `»` ink is
  bottom-heavy in its line box under `leading-none`, so centering the box left the
  ink hanging low. The nudge to `-translate-y-[62%]` centers the ink instead. The
  past-the-cap horizontal placement introduced with the forecast (PRD #309) is
  unchanged, and the rendered glyph is still `»`. Web-only. (!286, 23a0b693)

## [0.32.0] - 2026-08-13

### Changed

- **The Claude rate-limit forecast is now always-on and computed from a single
  reading.** The burn-rate forecast shipped in 0.31.0 needed an in-session sample
  series and a live, climbing burn rate, so its ghost and `»` marker stayed silent on
  idle windows and on the 7-day window, exactly where a "you are about to run out"
  warning matters. It now uses an anchored single-reading projection
  (`projected = used% * window / elapsed`), backed by a live measurement that the
  unified 5h/7d windows reset at a fixed boundary, so the forecast renders immediately
  on page load whenever a window is heading past its cap, idle and 7-day windows
  included. Each token now also shows its 7-day reset as a "resets <Day HH:MM>" label
  under its name, and the admin table column is retitled "Utilization & Forecast".
  Web-only. ([#310](https://github.com/vtmocanu/uzi/pull/310))

## [0.31.0] - 2026-08-13

### Security

- **uzi now refuses to run against a repo whose bot could push or merge straight to
  the default branch (PRD #66).** Enforcement lands at three points: enabling the
  repo (422, before the toggle flips), creating a run — the web UI, autopilot,
  the CI-fix loop, self-improve, and a scheduled prompt all go through the same
  gate — and at claim, where a run queued while the branch was still protected
  fails outright if protection was removed in the meantime, rather than pushing.
  The check is **live**, not the cached privilege report, and it **fails closed**:
  a forge that errors, times out, or cannot confirm branch protection also
  refuses, where it previously only warned. This is the first time uzi refuses a
  run for any reason, and it changes existing GitLab/Forgejo/GitHub behavior on
  purpose. **Before upgrading**, run `uzi admin guardrail-impact` (or
  `GET /api/admin/guardrail-impact`) against your instance — a live, read-only
  scan that reports how many currently-enabled repos would now be refused, and
  which ones, so the blast radius is known before the rollout rather than
  discovered repo by repo afterward. To fix an affected repo: protect its
  default branch and keep the bot off it — see [GitLab bot
  setup](docs/gitlab-bot-setup.md#least-privilege-what-uzi-verifies), [Forgejo
  bot setup](docs/forgejo-bot-setup.md#least-privilege-what-uzi-verifies), and
  [GitHub bot setup](docs/github-bot-setup.md#least-privilege-what-uzi-verifies).
  This guards only the bot's own PAT: a write **deploy key**, which uzi never
  provisions, can still push to a protected branch and this check cannot see it.

- **Judge free text is stripped of Unicode format and bidi-control characters
  before it renders (issue [#124](https://github.com/vtmocanu/uzi/pull/124)).** The last untreated sink, a run's agent label
  in the activity feed, now passes through the same `\p{Cc}\p{Cf}` strip that the
  other judge-text surfaces already use, closing a bidi-spoofing vector in the
  review UI. Newlines and tabs are preserved; combining marks and emoji are left
  untouched.

### Added

- **An instance admin can allow one named repo through the new guardrail (PRD
  #66).** The override is per-repo, admin-only — a member cannot self-allow,
  not even for a repo they own — and requires a written reason; who set it and
  when is recorded on the repo, persists until explicitly revoked, and is
  flagged stale on the admin list after ~30 days rather than silently
  re-blocking. It shows inline on the Repos page (a blocked repo carries a
  badge and, for an admin, an "Allow anyway" control; a member instead sees a
  pointer to ask an admin) and on a new admin **Blocked repos** page listing
  every user's blocked or overridden repos, with Revoke re-arming the block
  immediately. The override never waives the fail-closed case: a repo whose
  branch protection uzi cannot read stays refused even when allowed. New `uzi
  admin guardrail-impact` and `uzi admin blocked-repos` CLI commands surface
  the same data from the terminal — see [docs/cli.md](docs/cli.md).

- **Schedules now record and surface why a fire started nothing (PRD #308).** Each
  schedule keeps a `last_fire` outcome, the matched / started / skipped breakdown
  with a typed skip reason per candidate (`no_prd_link`, `not_eligible`,
  `already_running`, `description_too_large`, `fetch_failed`), shown as an outcome
  badge and a detail panel on the Schedules page, in `uzi schedule get`, and
  returned by `run-now`. A capped-sweep hint calls out when an issue cap, rather
  than eligibility, is why a sweep started nothing. A manual `run-now` reports its
  full outcome but never overwrites the recorded last scheduled fire.

- **The Claude rate-limit meters now show a burn-rate forecast (PRD #309).** Each
  meter grows a translucent "ghost" toward its projected landing point, plus a `»`
  marker when the projection overflows, computed from a trailing burn rate sampled
  during the session. It is display-only and deliberately hedged ("if this pace
  holds"), never an input to model auto-selection; the fast 5-hour window carries
  the useful signal while the slow 7-day window stays mostly quiet. Web-only: no
  API, database, or migration change.

### Changed

- **Dev and test Node bumped from 22 to 24 (Active LTS), matching CI (316a742b).**

### Fixed

- **The e2e poller no longer spuriously times out its forge sync (issue [#139](https://github.com/vtmocanu/uzi/pull/139)).**
  The per-tick context deadline was pinned to the poll interval, so a 2-second
  e2e interval cancelled a 15-second forge call mid-flight. The tick deadline is
  now floored at twice the forge HTTP timeout, decoupling it from the poll cadence.
  At the default one-minute interval behavior is unchanged, and a genuinely wedged
  forge still surfaces through the forge client's own 15-second timeout.

## [0.30.0] - 2026-08-12

### Added

- **A schedule's model now applies to every subagent, not just the lead.** A new
  opt-in ("Apply model also to agents") makes a scheduled run's resolved model
  override every subagent's model, pinned or not, whether the run uses the owner's
  own agent roster or the cloned repo's. Default off preserves the prior behavior,
  where a subagent's own `model:` pin wins. This fixes the case where a `fable`
  schedule that delegates to a subagent still ran that subagent on `opus`. ([#305](https://github.com/vtmocanu/uzi/pull/305))

### Security

- **Guardrails now deny an inline `git -c key=value` / `--config-env=key=env` write
  to a protected config namespace** (remote, core, http, url, credential, include,
  includeIf, alias, filter), mirroring the existing `git config <ns>.<x>` write
  deny. The inline form sets the same config without ever reaching `git config`, so
  an `alias.<x>=!<shell>` body or a credential/remote repoint could otherwise slip
  past the subcommand scan. (a4326e64)

### Fixed

- **Runs no longer get stuck at `awaiting_input` after a plan-phase clarification
  is answered.** The status now flips back to `running` when the answer is
  consumed, so `uzi run wait` and the web run page no longer report a false state
  (the page previously advised cancelling a healthy run), and Slack no longer
  accepts a stale answer as a fresh one. ([#307](https://github.com/vtmocanu/uzi/pull/307))
- **The judge no longer flags a tool invoked via `go run <module>@<version>` as a
  missing worker tool.** A repo that runs a linter or checker through a pinned module
  ref (for example `go run .../golangci-lint@v2.12.2`) needs no bare executable on
  PATH, so the deterministic command-not-found pre-scan now recognises that shape and
  suppresses the false "install this tool" recommendation. Genuinely absent tools are
  still reported. (58f53100)
- **Worker tool provisioning no longer trips a devbox "legacy format" warning.** The
  synthesized per-run `devbox.json` now pins every unversioned package to `@latest` (for
  example `go` becomes `go@latest`), which is what actually clears the warning (devbox
  keys it on unversioned packages), so `devbox install` runs cleanly. Already
  version-pinned packages are left unchanged. (58f53100)
- **De-flaked the docker-wiring readiness-timeout test** with an injectable clock,
  removing a load-dependent CI flake. (b70e5dc5)

## [0.29.0] - 2026-08-11

### Added

- **`make` is baked into every worker image.** Agents can now run
  Makefile-driven builds and test targets out of the box, with no per-repo
  devbox provisioning. It ships through the pinned devbox-global toolchain (GNU
  make 4.4.1) alongside go/gcc/jq, so it is version-pinned and covered by the
  image build guard. (2c92dac2, 347df966)

### Fixed

- **CLI install docs now include the tap step.** The README quick-start and
  `docs/cli.md` were missing the one-time `brew tap vtmocanu/tap
  git@github.com:vtmocanu/homebrew-tap.git` and `brew trust --tap
  vtmocanu/tap` commands, so a fresh install could not resolve the GitLab-hosted
  tap from the `vtmocanu/tap` shorthand (which Homebrew otherwise reads as a
  GitHub tap).

## [0.28.0] - 2026-08-11

### Added

- **Scheduled runs can pin the model they use (PRD #300).** A schedule now
  carries an optional model that is frozen onto every run it fires, overriding
  your per-user Worker model at claim time; leaving it unset inherits the Worker
  model exactly as before. Set it in the schedule modal (all target kinds) or
  from the CLI with `uzi schedule create --model`, and the frozen model shows on
  the run detail as a badge (visible on failed runs too). A subagent template's
  own `model:` pin still wins, and no worker image change is needed.
- **New `uzi schedule edit` command (PRD #302).** Change a schedule's cron,
  timezone, prompt, labels, guidance, or max-issues in place without churning
  its id or losing its run history. It reads the current schedule and overlays
  only the flags you pass, so editing one field leaves the rest untouched;
  `--clear-guidance` and `--clear-max-issues` remove a value explicitly.
  Previously the only way to change these was to delete and recreate the
  schedule.
- **Board search and per-lane "Show more" paging (PRD #304).** The board has a
  search field that filters every column live by title, issue number, or label
  and highlights the match. Each column now shows a set number of cards
  (default 10, tunable with the "Per lane" control) and reveals the rest a page
  at a time with "Show N more", so a busy backlog stays scannable instead of
  rendering hundreds of cards at once. The toolbar stays pinned while you
  scroll a long column. These are view-only changes; drag-and-drop ordering is
  unaffected.

## [0.27.0] - 2026-08-10

### Added

- **A failed pipeline on your own agent MR branch can now fix itself, opt-in
  and off by default.** With the new **Automatic CI fixes** toggle
  (Settings, or forced per-user from Admin → Users), a failing pipeline on a
  branch one of your issue runs pushed to auto-queues the existing `ci_fix`
  run, auto-approved — `main`, the default branch, and non-MR refs are never
  touched. A loop guard caps it at `CI_AUTOFIX_MAX_ATTEMPTS` (2) automatic
  attempts per branch and halts early if a retry's failure signature doesn't
  change, posting one issue comment and an in-app notification either way
  and resetting only once the branch goes green; the manual **Fix CI**
  button remains the escape hatch. A code/test fix pushes automatically, but
  a fix that edits the CI config itself (`.gitlab-ci.yml`, `.gitlab/**`, or
  the project's configured `ci_config_path`) parks for human approval, with
  a fail-closed worker-side backstop that refuses to push an auto-approved
  fix touching those paths. Validated on GitLab; the CI-config-path lookup
  is a GitLab-only stub on Forgejo/GitHub for now. (PRD #71)
- **A run can now finish report-only, instead of being forced to open an empty
  merge request.** When an issue run's deliverable is a report, command output,
  or a verification result with no code change to land, the lead calls
  `signal_done` with `report_only: true`; the worker records the findings
  summary and transcript and opens no merge request. An issue run that reaches
  `signal_done` with nothing committed and no `report_only` declared now fails
  with an actionable message instead of opening an empty MR. The run view and
  `uzi run get` show a neutral "report only" marker in place of the MR chip and
  render the findings summary as escaped plain text — it is untrusted
  worker-authored text, server-scrubbed on the way in and never passed through
  a markdown renderer. ([#279](https://github.com/vtmocanu/uzi/pull/279)) Symmetrically, a `report_only` completion that had
  already published committed work to a checkpoint ref
  (`refs/uzi-checkpoints/<branch>`) on origin now fails with an actionable
  message rather than completing and orphaning that ref. ([#299](https://github.com/vtmocanu/uzi/pull/299))
- **A seeded plan naming a bright-line infrastructure-reconnaissance target is
  now refused before the run is created.** A seeded run (`uzi run create
  --plan-file`) skips both the planning turn and the human approval gate, so a
  new deterministic screen (`api/internal/planpolicy`) checks the scrubbed plan
  text for cloud instance metadata endpoints, the default kube-apiserver
  ClusterIP, and the in-pod service-account token mount, and rejects a match
  with a 422 that redirects the caller to the ordinary, gated run flow. Plain
  issue-planned runs are unaffected. See
  [ADR-280](adr/0280-seeded-plan-safety-screen.md). ([#280](https://github.com/vtmocanu/uzi/pull/280))
- **A green-looking issue-run MR no longer implies gates that never ran.** When
  a component's JS dependencies fail to install, the run now carries the dirs
  whose `js_deps` check came back `ok:false` (excluding a `package.json` with
  no lockfile, which uzi deliberately never installs rather than guesses a
  package manager for) and renders a "⚠️ Quality gates unverified" note on the
  MR body naming them — an ANNOTATE-only signal that never blocks a merge. If
  dependency discovery itself hit its scan cap, the note also warns that
  components beyond the cap were never checked, so a capped run does not read as
  fully verified. ([#293](https://github.com/vtmocanu/uzi/pull/293))
- **The self-improvement picker now sees what uzi is already working on, and is
  told to avoid picking the same fix twice.** At claim time, a `self_improve`
  run is handed a list of every other active run on the connected repo — keyed
  on run status, not on a branch, so a run that hasn't pushed a branch or
  opened an MR yet still counts — and the worker prompt renders it in its own
  untrusted, nonce-fenced block with a directive to skip a recommendation that
  overlaps and record the skip in the run feed. It's advisory (an LLM picker
  choosing over a rendered list, not a hard block) and computed fresh per
  claim, so it reaches every worker immediately even though the prompt code
  that renders it only takes effect for newly provisioned workers. No
  migration. ([#297](https://github.com/vtmocanu/uzi/pull/297))
- **The Runs list now shows which Anthropic token each run used.** A per-run
  credential badge on the Runs list and run views names the token a run drew
  from, so it is clear at a glance whether a run used your own token or a shared
  admin one. ([#295](https://github.com/vtmocanu/uzi/pull/295))
- **uzi's Slack bot now renders Markdown as Slack mrkdwn.** Bot DMs and
  notifications (judge summaries, chat replies) convert Markdown to Slack's
  `mrkdwn` formatting instead of posting raw Markdown, so bold, links, lists and
  code render natively in Slack. ([#292](https://github.com/vtmocanu/uzi/pull/292))
- **The judge's review now renders as formatted Markdown in the web run-review
  panel.** The run view renders the judge's verdict and recommendations as
  Markdown instead of escaped plain text. ([#294](https://github.com/vtmocanu/uzi/pull/294))

### Changed

- **Report and review text sanitization now share one implementation.** The
  duplicated `sanitizeReportText` / `sanitizeReviewText` sanitizers were
  consolidated into `api/internal/termsafe`, removing the silent-drift risk
  between the two paths. ([#298](https://github.com/vtmocanu/uzi/pull/298))
- The bundled `uzi` CLI skill doc gained schedule-sweep guidance: the PRDLESS
  gate, `--max-issues` / `--guidance` flags, and a no-local-edit notice.
  (145c87b2)
- **`task deadcode` no longer reds on a component whose toolchain the change
  never touched.** `deadcode:web`/`deadcode:agent` now delegate to
  `scripts/deadcode-knip.sh`, which loud-SKIPs (exit 0) when the component's
  knip binary is absent instead of failing closed, so the umbrella
  `task deadcode`/`task gate` stays green for a contributor who only installed
  one component's deps. CI arms `UZI_DEADCODE_WEB_REQUIRED=1` /
  `UZI_DEADCODE_AGENT_REQUIRED=1` on `validate:web`/`validate:agent` (which
  always `npm ci` knip), turning the same missing-knip case into exit 2 there
  — a skipped and a passing gate must never look alike, the same shape as
  `lint:formula`/`lint:shell`/`lint:yaml`. ([#293](https://github.com/vtmocanu/uzi/pull/293))

## [0.26.0] - 2026-08-10

### Added

- **A run can now fact-check a claim against its forge instead of the repo's own
  restatement of it.** A run-lane subagent (the `fact-checker`) gets six
  read-only forge lookups as in-process MCP tools — `get_issue`, `list_issues`,
  `list_issue_label_events`, `get_merge_request`, `get_pipeline_jobs`, and
  `latest_pipeline` — so verifying "what does issue [#128](https://github.com/vtmocanu/uzi/pull/128) say?" or "did that MR
  merge / did CI pass?" checks live issue/MR/CI/label state rather than the repo's
  copy. The reads are worker-mediated and run-scoped: the agent never holds the
  forge credential, the project is derived server-side from the run, payloads and
  errors are coordinate-free, and forge prose reaches the model inside an
  untrusted-evidence fence. Works for GitLab and Forgejo. ([#158](https://github.com/vtmocanu/uzi/pull/158))
- **"Send to uzi" Auto-mode orchestration in the CLI skill.** The bundled
  `uzi-cli` skill now documents an interactive orchestration recipe: on "send to
  uzi" / "ship to uzi" it asks once how much to automate (Auto, Supervised, Seed &
  ship, Custom), then in Auto drives a PRD issue through a gated run, a
  skill-reviewed plan approval, MR review and merge via the forge's own
  `glab`/`gh`/`tea`, and local post-merge CI fixing. The merge and CI fixing are
  done by the local session, never by uzi, so `main` stays untouched. Vocabulary
  split so "seed to uzi" now names only the `--plan-file` path.
- **`fd` and `yq` are now baked into the default worker toolchain.** Agents in
  any repo can reach for `fd` (fast file finder) and `yq` (YAML processor,
  mikefarah/yq) without hitting a `command not found` exit 127 — the same
  general-purpose CLI class as the already-baked `jq`/`file`/`openssl`/
  `coreutils`. (from judge recommendations)

### Changed

- Read-only validator agents keep the run worktree clean. The builtin `web-ux`
  and `fact-checker` prompts now direct transient scratch artifacts (screenshots,
  pulled-down bundles) so a clean delivery never hinges on a final `rm`, and
  `fact-checker` no longer tells the worker to write outside the worktree, which
  the file-access guardrail rejects. (from judge recommendations)
- Worker agents stop stumbling on `cd api && …` gate commands. The builtin
  `coder` and `lead` prompts now note that the worker shell's working directory
  is not reliable across separate Bash calls, and to use absolute paths (or `cd`
  from the worktree root fresh each command). (from judge recommendations)
- The lead stops wasting messages trying to reach its own subagents by role
  name. The builtin `lead` prompt now says a subagent (still running or already
  returned) cannot be reached by role name and needs no reply — it reports to
  `main` itself. (from judge recommendations)

### Fixed

- **Worker provisioning now retries a transient devbox toolchain install instead
  of failing the worker.** A network-phrase classifier (`classifyDevboxError`)
  tells a transient install failure (a momentary network blip while installing
  the toolchain) from a real one and retries it with backoff, so a worker no
  longer fails to come up over a blip it could have ridden out. ([#290](https://github.com/vtmocanu/uzi/pull/290))

## [0.25.0] - 2026-08-10

### Added

- **A run that hits a transient forge failure now retries instead of dying, and
  a run that fails for good raises a failure inbox notification.** Pushes and
  merge-request creates that fail for a transient reason (network blips, forge
  5xx) are retried on a bounded backoff (five attempts, 1/2/4/8/16s) rather than
  failing the run on the first error; a run that still cannot publish its work
  surfaces as a failure notification (Slack inbox badge) instead of quietly
  ending. The retry and publish paths stay credential-free — the worker holds
  the PAT, the agent never sees it. ([#284](https://github.com/vtmocanu/uzi/pull/284))

- **Queued runs now spread across idle workers instead of piling onto one.**
  Fleet-aware claiming: while a queued run is still fresh (younger than
  `WORKER_SPREAD_GRACE`, default 3× the worker poll interval), a worker already
  running something defers it to a live, strictly-less-loaded peer that has a
  free slot, rather than taking a second run while that peer sits idle. Resume
  affinity is checked first and always wins, and past the grace window the run
  is claimable by any eligible worker, so a run is never stranded waiting for a
  peer that isn't there. The run-health signal now also distinguishes a
  saturated fleet from an idle queue. ([#216](https://github.com/vtmocanu/uzi/pull/216))

### Changed

- **The built-in `lead` agent template now runs a per-unit, commit-anchored
  review lane, overlaps the quality gate with review, and splits work along
  seams.** Internal template tuning refreshed from accumulated judge
  recommendations; run guardrails are unchanged. ([#215](https://github.com/vtmocanu/uzi/pull/215))

### Security

- **Hosted-worker deploy hardening: the CloudNativePG subchart is fetched from
  the ghcr.io OCI registry, and github.com is dropped from the restricted worker
  egress allowlist.** With the CNPG chart pulled via ghcr.io, the
  standard/restricted worker's FQDN allowlist no longer needs github.com,
  tightening the default-deny egress floor. ([#285](https://github.com/vtmocanu/uzi/pull/285))

## [0.24.0] - 2026-08-09

### Added

- **Scheduled runs default to parking on a usage limit instead of failing,
  a label sweep now caps how many issues it starts per fire, and a schedule
  can carry optional owner guidance.** Three additive changes to the
  schedules feature (PRD #241): (1) a new schedule's `wait_on_limit` now
  defaults **on**, and — unlike before — that setting now actually takes
  effect on the common auto-approve path, so an unattended, typically
  off-hours fire parks on the Anthropic usage window rather than dying
  silently; (2) a new label-sweep schedule defaults a `max_issues` cap of
  **10**, applied oldest issue first, so one fire can't fan out across an
  entire label's backlog at once (clear it in the web modal for unlimited);
  (3) a pinned-issue or sweep schedule can carry optional `guidance` text
  (capped at 8 KiB, truncated rather than dropped if it would push a large
  issue over the run's size limit), injected into the run instruction as a
  section clearly separate from the issue body to steer *how* a run
  approaches its task without editing every issue. All three are
  create-time defaults, per-schedule overridable, and leave existing
  schedules untouched. CLI: `uzi schedule create` gains `--max-issues` and
  `--guidance`, and `--wait-on-limit` now defaults on. ([#274](https://github.com/vtmocanu/uzi/pull/274))

- **A trusted repo's own conventions can now reach the agent.** The **Trusted
  repo** panel on the Repos page gains a **Repo instructions** toggle, a
  sibling of the existing **Repo skills** toggle, both independently
  revocable per repo. When on, the lead reads that repo's root `CLAUDE.md`
  as advisory context on every run against it — never as instructions it
  must follow, and never seen by subagents. The block is nonce-fenced and
  labelled UNTRUSTED/ADVISORY (the same framing PRD #90's cross-run memory
  uses) so a crafted file cannot forge trusted delimiters; the file is
  structurally sanitized before injection (root-only, symlinks never
  followed, `@`-import lines stripped, 64 KiB cap), never content-filtered.
  `settingSources` is untouched — the read goes through uzi's own channel,
  not the SDK's project loader — and the deny-hook, protected-branch
  guardrail, worker-held PAT, and human MR review are all unchanged: this
  opt-in grants context, not permissions. ([#246](https://github.com/vtmocanu/uzi/pull/246))

- **Runs now show how long they have been going, on the Runs page, the board
  cards, and the CLI.** Each run carries a duration token (elapsed for an active
  or parked run, total for a finished one), so you can tell that a run has been
  working for 90 minutes or waiting on your approval for half an hour without
  opening it. Display only; no API, DTO, or schema change. ([#256](https://github.com/vtmocanu/uzi/pull/256))

- **A run now publishes its committed work to origin on a time interval, not only
  at milestone boundaries.** A new `CHECKPOINT_INTERVAL` (a Go-style duration)
  periodically pushes the per-iteration checkpoint to origin, bounding how much
  work a worker's disk loss (a pod eviction or crash mid-milestone) can discard.
  The publish path is credential-free (auditor-confirmed), reusing the
  brokered-origin mechanism milestone checkpoints already use. ([#267](https://github.com/vtmocanu/uzi/pull/267))

- **Pristine builtin agent templates now refresh automatically on boot.** When
  uzi ships an improved builtin role template, instances pick it up on the next
  boot for any builtin the user has not customized; customized templates are left
  untouched, so shipped improvements to the built-in roster propagate without a
  manual reseed and without clobbering local edits. ([#275](https://github.com/vtmocanu/uzi/pull/275))

### Changed

- **The Judge's filter-chip counts now scope to the selected triage tab** instead
  of always counting the whole backlog, so each chip's number matches what the
  current tab is showing. ([#270](https://github.com/vtmocanu/uzi/pull/270))

- **Builtin and dev-team agent templates refreshed from accumulated judge
  recommendations** (the `coder` and `lead` builtins, plus the repo's
  `coder`/`web-ux` roster). Internal template tuning; run guardrails are
  unchanged. (`737984b0`)

### Fixed

- **Returning to an already-finished multi-agent run now shows its agent lanes
  collapsed — the same as a live run — instead of auto-expanding every lane.**
  Opening or reloading a done multi-agent run previously sprang every lane open;
  it now opens collapsed, with "Expand all" as the one-click way to reveal them.
  This generalizes the earlier watched-while-finishing fix: a done run and a live
  run now share one code path, and expansion no longer branches on run state at
  all (single-actor runs still auto-expand). ([#277](https://github.com/vtmocanu/uzi/pull/277))

- **An `auto` worker no longer immediately re-picks the Anthropic token that just
  hit its usage limit.** After a usage-limit park, token selection excludes the
  just-exhausted credential until its window resets, so a parked run resumes on an
  account with real headroom instead of bouncing straight back onto the exhausted
  one. ([#217](https://github.com/vtmocanu/uzi/pull/217))

- **Slack chat answers are delivered to the thread you asked in, and every DM now
  renders with real Slack formatting.** Fixes two defects the Slack surface
  shipped in v0.23.0 (PRD #191): replies landing on the channel root instead of
  the originating thread, and direct messages rendered as raw text rather than
  Block Kit. ([#268](https://github.com/vtmocanu/uzi/pull/268))

- **Costs of $1000 or more drop the cents in the web UI.** `formatCost` renders a
  whole-dollar amount (for example `$1119`) at or above $1000, where the cents are
  noise, and keeps the two-decimal form below that. ([#269](https://github.com/vtmocanu/uzi/pull/269))

- **The label filter's "Clear" control no longer shifts the layout, and now reads
  as a button rather than a link.** Reserving its height stops the filter row from
  jumping as Clear appears or disappears, and the restyle brings it in line with
  the other actions. ([#276](https://github.com/vtmocanu/uzi/pull/276))

- **A repo whose `devbox.json` is JSONC (carries comments) no longer silently
  provisions no tools.** Tier-2 worker devbox provisioning is now best-effort: a
  parse failure warns and skips rather than aborting the run, and tier-1 versus
  tier-2 provisioning failures are reported distinctly. ([#278](https://github.com/vtmocanu/uzi/pull/278))

## [0.23.0] - 2026-08-09

### Added

- **Board membership and who can run what are now configurable label sets,
  not one hardcoded PRD label.** Two new admin-only instance settings replace
  what used to be a single gate: **run-eligible labels** (default `PRD,bug`)
  control which issues a human may click Start run on, and **board-extra
  labels** (admin default `bug`, overridable per user, per repo) control
  which non-primary issues a board shows by default. The board toolbar's
  **Issues** popover replaces the old "Show other issues" checkbox: a pinned
  always-shown primary row, one row per label with a count of how many cards
  it adds, a greyed `0` row for a configured default the repo has none of,
  the old "Show all other issues" escape hatch, and **Reset to default**. A
  third setting, `eligible_label_waives_prd_link` (default **on**), lets an
  issue eligible via a *non-primary* label (e.g. `bug`) start a run with no
  `prds/*.md` link — scoped to a human's own interactive Start click; it
  never applies to autopilot or to a scheduled/timer-fired run. The primary
  label (`prd_label`) itself is unchanged: still the label uzi writes to mark
  its own work, the only one boards fetch with, and the only one autopilot
  matches. ([#196](https://github.com/vtmocanu/uzi/pull/196))

  **Operators — this changes default behavior on upgrade, in two halves.**
  *Visibility:* every board gains `bug` cards it didn't show before —
  cosmetic, and reversible per user from the Issues popover, or instance-wide
  by editing `board_extra_labels`. *Eligibility:* those `bug` cards also gain
  a working **Start run** button with no admin action, because `bug` ships in
  the eligible default (`PRD,bug`) and the PRD-link waiver defaults on — a
  `bug` issue with no `prds/*.md` link is startable where it wasn't before.
  **Autopilot is unaffected** — it still matches only the primary label and
  never receives the waiver — and neither is a scheduled or swept run: the
  waiver is scoped to an interactive, human-initiated Start click only, so
  nothing starts unattended that couldn't before. To keep the prior
  behavior, set `run_eligible_labels` to just the primary label, or turn
  `eligible_label_waives_prd_link` off, from Admin → Instance settings. See
  [docs/board.md](docs/board.md#which-issues-show-up) and
  [docs/admin-settings.md](docs/admin-settings.md#run-eligibility).

- **Per-label counts on the Judge filter chips ([#244](https://github.com/vtmocanu/uzi/pull/244)).** Each chip now shows how
  many recommendation groups are in that category across your whole backlog, every
  bucket and triage state — sourced from a new server aggregate, never tallied off
  the on-screen list, so a chip reads correctly even when the truncation banner is
  showing. A zero-count chip stays visible, just dimmed.

- **Slack is now a two-way conversational surface: chat, run control, and status,
  from a DM.** A top-level DM to the uzi bot opens a real conversation, backed by
  the existing `runs.kind='chat'` machinery and streamed back into that DM's
  thread; a threaded reply continues it. The agent can propose filing an issue or
  starting a run, and both render as Block Kit cards with real Create/Dismiss or
  Confirm buttons — nothing fires from text alone, and only the confirmed-linked
  owner's click calls through. Run message content, previously withheld from
  Slack entirely ("content minimization"), is now streamed for `chat` runs only;
  every other run kind's notification lane is byte-for-byte unchanged. **This is a
  real widening of what reaches Slack, not just a new feature**: a chat's read
  tools are user-scoped but not kind-scoped, so a Slack DM can be asked to quote
  an `issue` run's saved content (plans, diffs, tool output) into the thread —
  `docs/slack.md` documents this plainly. No app re-install and no admin action
  needed on any workspace that already has uzi's Slack app connected; a spend
  limiter is shared with the web Chat page, so a heavy Slack day can rate-limit
  it too. ([#191](https://github.com/vtmocanu/uzi/pull/191))

- **Runs can now be scheduled — one-time or recurring — instead of only starting
  on demand or via autopilot.** A new Schedules surface (web `/schedules` page +
  create/edit modal + an issue-view "Schedule…" action) and a `uzi schedule` CLI
  group manage a persisted, owner-scoped schedule against one of three targets: a
  pinned issue, a label sweep (every open issue on a repo matching a label
  selector, still gated by the normal run-creation rules), or an ad-hoc prompt — a
  new issue-less `prompt` run kind that opens an MR directly against a repo with
  no issue involved. A background scheduler goroutine (the same wake-ticker shape
  `selfimprove` already uses) claims due schedules and fires them as their owner,
  on the owner's own Anthropic token and forge PAT. Issue and sweep targets fire
  through the same run-creation seam autopilot uses, inheriting every existing
  gate (PRDLESS bypass, active-run dedup, the usage-limit park); the prompt target
  is the deliberate exception and bypasses the PRD-issue sanction gate by design.
  Auto-approve defaults on per schedule. See [docs/scheduling.md](docs/scheduling.md).
  ([#241](https://github.com/vtmocanu/uzi/pull/241))

- **`uzi run wait` replaces the hand-rolled poll loop for driving a gated run
  headless.** `uzi run wait <id>` blocks until a run reaches an actionable or
  terminal state — a plan gate, a clarification park, or done — polling
  client-side and printing each transition to stderr; `--until` narrows the
  target set (after approving, wait again for only `completed,failed,cancelled`,
  since a run lingers briefly at `awaiting_approval` right after a successful
  approve), `--timeout` gives up with a new exit code 7, and a single transient
  server blip is retried rather than fatal. `uzi run get --field <name>`
  complements it: a single top-level scalar field printed raw and unquoted, one
  per line, sidestepping the footgun of piping `--json` through a shell that
  re-interprets escapes (notably zsh's `echo`, which mangles the CLI's
  `\uXXXX`-escaped control bytes and breaks `jq`). ([#264](https://github.com/vtmocanu/uzi/pull/264))

### Changed

- **The admin rate-limits table no longer needs a horizontal scrollbar to see the
  status.** The two ~280px 5-hour/7-day window columns are now one stacked
  **Utilization** column (a mono `5h`/`7d` chip, meter, percent and reset
  countdown per row), and the "Updated" timestamp folds under the Status pill —
  six columns down to four, no data removed, fitting a normal laptop content
  width with no scrollbar and no clipped pill. ([#240](https://github.com/vtmocanu/uzi/pull/240))

- **Six built-in agent roles picked up sharper review and gate-reading guidance,
  synced from the role library.** `auditor` gained lenses for uncapped
  amplification/resource-exhaustion reads, injection into a non-shell sink
  (terminal, log, a shared admin surface), and verifying that a security-gating
  check fails closed; `coder` gained a note that a directive or skip marker fires
  on any line carrying its literal text, including a comment warning against it;
  `researcher` gained the symlinked-directory blind spot (a recursive `grep`/`rg`
  sweep silently skips a symlinked corpus and reports a clean negative);
  `reviewer` gained "a fix at one call site is a claim about the whole sibling
  set" plus three checks specific to a status/health/authorization predicate;
  `tester` gained three gate-instrument traps (a shell pipeline misreporting its
  own exit code, a severity-staged tool's warn tier passing green with findings
  uncounted, and a stale result cache or an unstaged file silently excluded from
  a gate's scope); and `web-ux` gained a check that a mutating control is gated
  client-side on the same scope predicate the server enforces, not just hidden
  from an unauthorized viewer. **Operators:** on an already-seeded install these
  six templates badge as "differs from shipped" (issue [#201](https://github.com/vtmocanu/uzi/pull/201)'s mechanism) until
  you open each and click Reset to default. (`9b930988`)

### Fixed

- **A completed run no longer shows `0/N` milestones when it actually finished
  them.** The tracker used to reconcile only from mid-run `report_progress`
  reports, so a run that jumped straight to its final turn with no progress
  report in between showed a misleading `0/N` at completion. The lead now
  declares which frozen milestones it finished on its `signal_done` call, the
  server unions that declaration into `milestones_completed` the same way it
  already unions mid-run reports, and the UI distinguishes "not reported"
  (neutral) from a genuine `0/N` (which now only means "reported and truly
  none"). ([#265](https://github.com/vtmocanu/uzi/pull/265))

- **`save_memory` no longer lets an unverified claim become a self-reinforcing
  "fact."** A run had asserted, without ever testing it, that a builtin
  subagent lacked write tools, saved that to durable cross-run memory, and two
  later runs cited it as "the repo convention I noted" and over-serialized work
  onto the lead's own thread instead of delegating. `save_memory` now requires
  the writer to declare whether a claim was **observed** (a tool result,
  command output, or a `file:line`) or merely **inferred**, and a retrieved
  *inferred* entry is individually marked "re-verify before acting" rather than
  relying on the single blanket "memory is advisory" notice that had already
  failed once. Separately, the lead's per-turn roster now states each
  subagent's actual write capability instead of leaving it to guess — the
  specific trigger for the incident above. `save_memory` also nudges (never
  rejects) against saving a claim about the run's own runtime configuration, a
  class that goes stale and should be read live instead. See
  [docs/memory.md](docs/memory.md). ([#266](https://github.com/vtmocanu/uzi/pull/266))

- **The judge's command-not-found pre-scan no longer flags generic output
  words as missing worker tools.** A low-confidence `X: not found` match (the
  dash/busybox form) — which a plain word like `key` or `foo` in unrelated
  output can trigger — is now corroborated against the commands the run
  actually invoked, and dropped unless the run really ran that command; the
  three high-confidence "command not found" forms are unaffected. ([#263](https://github.com/vtmocanu/uzi/pull/263))

- **A worker's own gate run no longer false-flags a large pre-existing lint
  backlog in files it never touched.** The golangci-lint ratchet
  (`new-from-merge-base: origin/main`) computes its merge base against the
  runner clone's `origin/main`, but that ref was copied from the bare mirror's
  frozen snapshot at first clone and never advanced — so on an older mirror the
  merge base landed far enough back that the whole existing backlog read as
  branch-introduced. The clone now advances `origin/main` to the fresh default
  branch head before the gate runs, so the ratchet gates only what the branch
  actually introduced. ([#262](https://github.com/vtmocanu/uzi/pull/262))

## [0.22.0] - 2026-08-08

### Added

- **GitHub forge support ([#238](https://github.com/vtmocanu/uzi/pull/238)).** A third forge driver (github.com, classic PAT)
  behind the forge-generic interface, at full parity with GitLab and Forgejo: board
  sync, runs, pull-request creation and watching, privilege guardrails, and the
  GitHub Actions CI-fix loop. Connect a GitHub bot PAT and your PRD-labeled issues
  populate the board; cards read "Pull Request"/"PR"/"#N". Ships dark behind the
  connect-form forge picker.

### Fixed

- **Milestone progress UI stayed blank on human-gated runs ([#259](https://github.com/vtmocanu/uzi/pull/259)).** Milestones are
  now frozen on the first running report (`milestones_frozen` is set), so the PRD #122
  progress UI populates on runs that pass through a plan gate.
- **Bug bundle: controller vulnerabilities, web papercuts, and a chat-cap bypass
  ([#258](https://github.com/vtmocanu/uzi/pull/258)).** A batch fix that landed alongside #238, also closing #221, #152, #163,
  #183, #185, #204, and #192.

## [0.21.0] - 2026-08-08

### Added

- **Milestone progress across the product ([#122](https://github.com/vtmocanu/uzi/pull/122)).** A run whose lead breaks its plan
  into milestones now shows what is done, in progress, and left everywhere the run
  appears: a checklist plus an M/N badge on the run page, and the candidate breakdown
  at the plan gate (M3); a milestone counter on the Slack root line and a threaded
  line as each milestone completes (M4); and the same state in `uzi run get` (M5). A
  run with no milestones is unchanged and keeps its iteration badge.
- **Per-worker uptime on the fleet UI and CLI ([#251](https://github.com/vtmocanu/uzi/pull/251)).** Each worker shows how long it
  has been online.

### Fixed

- **A working run no longer reads as stalled or idle ([#193](https://github.com/vtmocanu/uzi/pull/193)).** The wall-clock "slow"
  health flag was painting the actively-running lane amber as if it had stalled, and
  lane headers showed when a lane opened instead of its last activity. Both are fixed
  in the run activity view; the server health detector was already correct.
- **Version popover ordering (`91584071`).** Reordered so PRDs follow Commit and Uptime
  is last.

### Changed

- **uzi CLI skill documentation ([#255](https://github.com/vtmocanu/uzi/pull/255)).** Documented the seeded-plan budget tradeoff,
  the per-verb JSON envelope shapes, the full run-status enum (including `limit_wait`),
  and that `uzi run logs --follow` returns only on a terminal status; added a
  post-session "improve this skill" note.

## [0.20.2] - 2026-08-08

### Fixed

- **The checkpoint push broker no longer OOM-kills the api on a real repository.**
  M8's broker fetched the run branch's full history into an in-memory git store
  before pushing; on a real repo that unpacked the entire pack into RAM (~740 MB
  RSS for uzi's 130 MiB pack) and OOM-killed the 512Mi api pod at every milestone
  checkpoint. The base fetch is now **shallow (depth 1)** — it pulls only the tip
  snapshot the delta pack actually references (~190 MB peak, measured against the
  real forge) — and the api memory limit is raised 512Mi → 1Gi for concurrency
  headroom. This surfaced on the first real deploy of 0.20.1; the broker's unit
  tests use a one-commit local fixture where fetch depth is a no-op. ([#122](https://github.com/vtmocanu/uzi/pull/122) M8)

## [0.20.1] - 2026-08-08

First **published** build of the 0.20.0 changeset. The `v0.20.0` tag was created
but its publish pipeline was skipped by a stray `[skip ci]` marker on the tagged
commit (the marker suppresses the tag's own publish pipeline too), so 0.20.0
produced no images or chart and was never deployed. 0.20.1 is identical in
shipping code — see the [0.20.0] section below for the actual changes.

## [0.20.0] - 2026-08-08

### Added

- **Runs now checkpoint committed work at milestone boundaries and recover it
  after a worker crash.** When the lead completes a milestone, the worker durably
  saves the run's branch to its data volume (surviving a pod kill and a
  same-worker restart), and — via a new server-side push broker — publishes it to
  a `refs/uzi-checkpoints/<branch>` ref on the forge, so a *different* worker that
  re-claims the run recovers the completed milestones instead of redoing them. The
  forge token is never exposed to a worker git process (the API holds it and
  performs the push), the push is never forced, and no CI pipeline fires on the
  checkpoint ref. The plan carries an optional milestone list (`submit_plan`), the
  run budget scales with it, and progress is reported over the existing run
  stream; the user-visible progress UI (web, Slack, CLI) is not part of this
  release. ([#122](https://github.com/vtmocanu/uzi/pull/122) M1, M2, M6, M8)

- **The run page shows live in-flight token counts.** Usage on the run page
  updates from the first model call rather than only after the run records
  usage, so a running agent's token spend is visible as it happens. ([#237](https://github.com/vtmocanu/uzi/pull/237))

- **The Runs menu item carries an in-progress count badge.** The nav badge shows
  how many runs are currently in progress at a glance. ([#239](https://github.com/vtmocanu/uzi/pull/239))

- **The version popover and `uzi version` now show PRD roadmap progress.** A
  `PRDs  N done · M open` row (sidebar popover) and matching `prds  N done, M
  open` line (`uzi version`) count completed PRDs (`prds/done/*.md`) and active
  ones (`prds/*.md`) in the source tree the running image was built from, so a
  published instance's roadmap progress is visible without cloning the repo.
  Both counts are build stamps computed in CI and injected via ldflags, the
  same way the existing commit count is — the API's Docker build context has
  neither `.git` nor `prds/` to count from at runtime — so like that field
  they're simply absent (never shown as zero) on an unstamped dev build. ([#245](https://github.com/vtmocanu/uzi/pull/245))

- **`save_memory` now steers agents away from saving numbers that will go stale.**
  The lead's prompt and the tool's own description ask agents to record the durable
  fact rather than today's count (test-pass tallies, version numbers, and the
  like), and a non-fatal warning fires when a saved memory's body looks like an
  obvious snapshot shape. (fcaecf57)

### Fixed

- **A subagent addressing "lead", "orchestrator", or "team-lead" no longer fails
  to send its message.** Those names now get transparently rewritten to `main` by
  a guardrail hook, defense in depth for repo-sourced and user-authored agent
  templates (builtins already said `main`). A repo that registers a real subagent
  literally named `lead` is unaffected: the rewrite only fires when no such
  subagent is registered. (61795aac)

- **The judge triage meter's "to do" segment is now amber.** It previously read
  as grey/"dismissed", making outstanding recommendations look already handled;
  amber distinguishes to-do from dismissed. ([#243](https://github.com/vtmocanu/uzi/pull/243))

## [0.19.1] - 2026-08-07

First published cut of the 0.19.0 content: the `v0.19.0` tag was blocked by the release
changelog-coverage gate — a builtin-role refresh merged during the release cut and was not
yet cited — and `v*` tags are immutable here, so the identical code ships as 0.19.1.

### Added

- **Filter the Judge backlog by recommendation label.** A row of chips above the bucket
  tabs — one per label (enable a tool, install a worker tool, adjust a template, improve
  an agent, add an agent, improve uzi) — narrows the cross-run worklist to whichever you
  tick. It's multi-select (OR: tick two and see either), lives in the URL as a shareable
  `?category=`, and `uzi review backlog --category` does the same from the terminal. Like
  the existing `--run` anchor, the filter runs before the server's row cap, so narrowing
  by label makes the "backlog was truncated" banner less likely to bite, not more. ([#235](https://github.com/vtmocanu/uzi/pull/235))

### Changed

- **Built-in agent roles refreshed from the role library.** The shipped agent roster
  (architect, coder, auditor, documenter, fact-checker, researcher, lead, web-ux) picked
  up judge-recommendation improvements and was re-synced from the role library, so new
  runs get sharper role instructions. (8199f8e8)

## [0.18.0] - 2026-08-07

### Added

- **Worker toolchain now ships `task` and `jq`.** The baked worker image includes
  the `task` runner, so agents can drive a repo's own `Taskfile` gate recipes
  instead of reconstructing them by hand, plus `jq`. Closes two
  `install_worker_tool` judge recommendations. ([#233](https://github.com/vtmocanu/uzi/pull/233))

### Fixed

- **Judge recommendation backlog no longer fragments one recurring finding into
  separate rows.** Recommendations are deduped on a canonicalized
  `(category, target)` key, so a finding that recurs across runs collapses to a
  single row carrying its true "seen in N runs" frequency instead of splitting N
  ways. ([#232](https://github.com/vtmocanu/uzi/pull/232))
- **Worker runner clones now carry a git author identity.** The clone the agent
  works in is pre-configured with `user.name`/`user.email`, so the agent's first
  `git commit` no longer fails with "Author identity unknown" (exit 128) and
  self-heals, which was burning an iteration on every commit-producing run. ([#234](https://github.com/vtmocanu/uzi/pull/234))
- **Activity feed no longer springs every lane open when a run finishes while you
  watch it.** Auto-expand still applies when you open an already-finished run, but a
  live `running → completed` transition now preserves the collapsed view you were
  reading instead of discarding it.

## [0.17.0] - 2026-08-04

### Added

- **Seed a plan onto a run: `uzi run create --plan-file <plan.md>`.** A run can now
  be created with an externally-authored plan (written locally in Claude Code),
  skipping uzi's Phase-1 planning turn and the approval gate: the worker implements
  the supplied plan directly and opens an MR, so the human checkpoint moves from the
  plan gate to MR review. Optional `--agent-source` / `--exclude-agents` pick the
  subagent roster, and `--planned-commit` / `--require-base` guard against a stale
  base commit. The seeded plan is size-capped and secret-scrubbed; a run created
  with no `--plan-file` is unchanged. Web run pages render a seeded run's plan. See
  `docs/seeded-plans.md`. PRD #209.

### Changed

- **Contributor tooling: MR pipelines no longer wait for the full gate before
  they start building images.** Each `build:*` job now `needs:` only the
  checks for its own component (`build:api` waits on `validate:api`,
  `test:api` and `lint:api`, not the whole gate set) instead of every check
  across all four components, so it starts as soon as its own component is
  clean rather than after the slowest of all four. Publish jobs' `needs:`
  are untouched. Part of PRD #230. Developer-facing only: no change to how
  uzi behaves.

- **Contributor tooling: `test:api-store-it` no longer sits on the image-build
  critical path, and still gates every release exactly as before.** It's
  dropped from the validation-gate `needs:` list build jobs used to depend
  on, but stays in the publish-gate list, so it still runs on every ref and a
  tag pipeline still can't publish without it passing. Part of PRD #230.
  Developer-facing only: no change to how uzi behaves.

- **Contributor tooling: `e2e:kind-smoke` no longer rebuilds the api and web
  images from scratch on `main`.** `build:api`/`build:web` now emit a
  `--tarPath` image archive as an artifact on protected non-tag refs, and the
  e2e job loads that instead of running its own cold `docker build` inside
  DinD; a tag pipeline still self-builds its images, unchanged. `scripts/smoke.sh`
  is untouched. Part of PRD #230. Developer-facing only: no change to how uzi
  behaves.

- **Contributor tooling: an MR that doesn't touch a component now skips that
  component's image build.** `build:api`, `build:web` and `build:controller`
  gained a `changes:` filter scoped to each component's real Dockerfile
  inputs (`build:web`'s includes `docs/**`, which its Dockerfile copies in).
  `build:agent` is deliberately excluded and always builds, and no
  protected-ref pipeline skips a build, so `main` and tag pipelines are
  unaffected. Part of PRD #230. Developer-facing only: no change to how uzi
  behaves.

- **Contributor tooling: `golangci-lint` no longer compiles from source on a
  cold lint cache.** `task lint:api`/`lint:controller` now fetch a pinned,
  sha256-verified release binary (`scripts/golangci-lint.sh`) instead of
  `go run`-ing the module from source, with the cached binary re-verified
  against its own pin on every run rather than trusted on a cache hit alone.
  The pin is checked against both Go modules' `go` directive at gate time and
  fails loudly if a future linter release's minimum Go version outgrows them.
  Findings are unchanged: verified identical to the previous `go run` build,
  by diff, on both modules. Part of PRD #230. Developer-facing only: no
  change to how uzi behaves.

- **Contributor tooling: `test:api-store-it`'s throwaway Postgres now runs
  with durability off.** `fsync`, `full_page_writes` and `synchronous_commit`
  are disabled on the job's disposable `postgres:17` service, since its data
  is destroyed with the job either way; a mid-job crash could corrupt it,
  which is an accepted trade for data that was never going to outlive the
  job. Part of PRD #230. Developer-facing only: no change to how uzi behaves.

## [0.16.0] - 2026-08-04

### Changed

- **A worker no longer keeps a parked run's on-disk clone around.** When a run
  parks on a usage limit (or the worker shuts down), the worker used to preserve
  the run's clone directory on the theory a resume needed it. It never did: a
  resume re-clones unconditionally and the committed work is now fetched back into
  the worker's own repo (a tracking ref) and recovered from there instead,
  validated live on dev-cluster by a real worker eviction that recovered the
  committed work byte for byte. So the clone is now cleaned up on a park like on
  any other terminal path, freeing a full working tree per parked run for up to
  the 8-day park window; the plugin dir and the per-run session HOME are still
  preserved so the session resumes cleanly (issue [#218](https://github.com/vtmocanu/uzi/pull/218)).

## [0.15.0] - 2026-08-04

### Added

- **Agent templates now flag when a builtin has drifted from what this uzi
  version ships.** A **differs from shipped** badge appears on the Agents
  list and on a builtin's detail page whenever its stored description,
  model, tools, or prompt body no longer matches the shipped definition,
  whether the drift is your own edit or a shipped update you haven't picked
  up yet. Opening the template now shows the actual diff before you press
  **Reset to default**, which still replaces the whole body verbatim and
  still isn't automatic (issue [#201](https://github.com/vtmocanu/uzi/pull/201)). **Operators:** issue [#210](https://github.com/vtmocanu/uzi/pull/210) rewrote ten
  of the eleven builtin templates' bodies to fix an unreachable report
  recipient (see Fixed, below); on any already-seeded install, those ten
  will badge as differing the moment you deploy this build. That's the new
  signal working as intended, not a defect: open each and click Reset to
  pick it up. See
  [docs/agent-templates.md](docs/agent-templates.md#resetting-a-builtin-template).
- **`uzi` now warns when the CLI you're running is older than the server it's
  talking to.** A CLI three minors behind was silently dropping fields it
  didn't know about — `run get --json` printed `null` for token attribution
  that a `curl` against the same endpoint returned fine — with nothing telling
  you why. The warning prints to stderr (never stdout, so `--json` output stays
  parseable) and costs at most one version probe an hour. Suppress it with
  `--quiet` or `UZI_VERSION_CHECK=0` (issue [#144](https://github.com/vtmocanu/uzi/pull/144)).

### Changed

- **Subagents no longer have the ordinary route to write files during a run's
  plan turn**, the phase before you approve a plan. Every subagent's `Edit`,
  `Write`, `MultiEdit` and `NotebookEdit` tools are removed for that turn
  only; the implement turn is unaffected, so a role that needs to write is
  exactly as able to once the plan is approved. This is a hygiene control,
  not a guarantee: the turn still runs under `Bash`, and nothing at the
  guardrail layer denies a shell-based write (`echo >`, `sed -i`, `tee`), so
  what changes is the tool a model reaches for by default and its awareness
  that one exists, on top of the prompt instruction that was previously the
  only thing saying not to. Actually catching a worktree change no matter how
  it was made is tracked separately as issue [#212](https://github.com/vtmocanu/uzi/pull/212) (issue [#203](https://github.com/vtmocanu/uzi/pull/203)).

- **Worker names and CLI-token names are now validated on write, and reject
  terminal-unsafe characters that were accepted before.** `POST
  /api/workers`, the hosted-worker provisioning path, and both the
  CLI-token mint and device-start flows now return 400 for control
  characters, Unicode format characters (bidi overrides, zero-widths, the
  BOM, the soft hyphen), and invalid UTF-8; names are still trimmed of
  leading and trailing whitespace before that check, unchanged from before.
  These names are read back in cross-tenant admin listings beside a
  different user's account, so an unvalidated one was terminal control
  injection into another user's session, and an embedded newline could
  forge a whole table row in a listing an admin reads to make decisions.
  Existing stored names are untouched by this change and stay covered by
  the render-side fix instead (issue [#180](https://github.com/vtmocanu/uzi/pull/180)), which strips the same
  characters on the way out (issue [#169](https://github.com/vtmocanu/uzi/pull/169)).

- **Contributor tooling: dependency vulnerabilities are now scanned on every
  MR**, for the first time in this repo — `govulncheck` for both Go modules and
  `npm audit --audit-level=high` for both npm packages. They hang off the
  existing per-toolchain jobs (`lint:api`, `lint:controller`, `validate:web`,
  `validate:agent`) rather than adding new ones. Locally they are
  `task vulncheck` and are **deliberately not part of `task gate`**: their
  verdict is a function of a remote mutable database, so they can answer
  differently on two runs of one commit with nobody's diff in between, and a
  contributor's gate must be deterministic against the tree. All four host jobs
  are in `*publish_needs`, so this is release-blocking by inheritance — a CVE
  published on a Tuesday can redden a `v*` tag publish that contains nobody's
  diff. That is accepted, not overlooked. The npm half gates at `high` rather
  than zero because two moderate `react-router` advisories have no patched 6.x
  to move to; clearing them is a React Router 6 → 7 major through **runtime,
  shipped SPA routing code**, filed as issue [#226](https://github.com/vtmocanu/uzi/pull/226). Part of PRD #103.
  Developer-facing only: no change to how uzi behaves.
  See [docs/dev-conventions.md](docs/dev-conventions.md).

- **Contributor tooling: the gate now refuses environment variables that would
  silently narrow what it looks at.** Three tools turned out to have one — a
  substituted `GITLEAKS_CONFIG`, an `NPM_CONFIG_OMIT=dev` that drops the dev
  tree and prints "found 0 vulnerabilities" at exit 0, and a `GOPACKAGESDRIVER`
  stub or `GOFLAGS=-tags` that build-tags the vulnerable call out of the graph.
  Each produces output shaped exactly like a clean run. The scripts now refuse
  them, narrowly where a legitimate use exists (`GOFLAGS=-buildvcs=false` is a
  documented workflow here, so only `-tags` is rejected). Part of PRD #103.
  Developer-facing only: no change to how uzi behaves.
  See [docs/dev-conventions.md](docs/dev-conventions.md).

- **Contributor tooling: `task gate:web` and `task gate:agent` now check that
  your `node_modules` matches the branch before running anything else.** A stale
  install does not make lint, knip, `tsc` and vitest *wrong* — it makes them
  answers about a different tree, all green. The check is two parts because
  `npm ls` compares against declared ranges only, so a transitive bump is
  invisible to it; the second part joins against the lockfile. Measured:
  `gate:agent` was green on a tree holding all five of the vulnerable versions
  this release bumps. Part of PRD #103. Developer-facing only: no change to how
  uzi behaves. See [docs/dev-conventions.md](docs/dev-conventions.md).

- **Contributor tooling: `web` moved to vitest 4.1.10, pinned exactly.** A major,
  taken because the 2.x line is where the remaining high-severity `web`
  advisories lived. The pin is exact rather than a range, and so is
  `@vitest/coverage-v8`, which is an exact-version optional peer — the two move
  together or the install breaks. `environmentMatchGlobs` and `test.workspace`
  are both gone in 4.x. The upgrade's control was a count written down before the
  run and reproduced after (118 files / 1660 tests), because a green with a lower
  collected count is a silently narrowed suite that no exit code reveals. Part of
  PRD #103. Developer-facing only: no change to how uzi behaves.
  **Existing checkouts need `npm install --ignore-scripts` in `web/`.**

- **Contributor tooling: test coverage is now measured for `api`, `controller`
  and `web`, and shown on every MR.** `task test:api` and `task test:controller`
  write a profile and print the statement total; `task test:web` runs vitest with
  the v8 provider. GitLab reads the number off each job and the Cobertura reports
  annotate the MR diff. **There is deliberately no failing threshold** — a number
  picked before the current one was known is either vacuous or blocks unrelated
  work, so this milestone measures and a later one chooses. Two things worth
  knowing before reading the figures: the totals exclude packages with no tests of
  their own, and the single percentage GitLab puts on an MR is an unweighted mean
  across the three jobs rather than a repo-wide figure — read the per-job numbers.
  Part of PRD #103. Developer-facing only: no change to how uzi behaves.

- **Contributor tooling: `web` tests now get their environment from
  `vite.config.ts` instead of relying on a per-file docblock.** A test under
  `src/components` or `src/pages` that forgets `// @vitest-environment jsdom` used
  to run under node, which is usually a loud error and is not reliably one. Those
  directories now default to jsdom via `test.projects`, while `src/lib` and
  `src/mocks` — where every node-side test in the suite lives — stay on node. The
  docblock still wins over the config where a file carries one, measured on
  vitest 4 rather than assumed, so no existing file changed environment: the
  per-file census is identical before and after. Part of PRD #103.

- **Also in this release, smaller changes that merged alongside the above.** The
  builtin agent templates gained additional role guidance for several roles
  (auditor, coder, fact-checker, reviewer, tester, web-ux) (`e963f672`); on an
  already-seeded install these badge as "differs from shipped" until reset, like
  the #201 note above. The root `CLAUDE.md` was reorganised into path-scoped
  contributor rule files (`8fd735ba`, NEW-6). And a rejection-message wording in
  the secrets API handler was aligned with the project's lint rule (`10f7b6a5`).

### Fixed

- **A run that parks on an Anthropic usage limit, or whose worker is shut down
  or evicted, no longer loses the work the agent already committed.** Previously
  the committed branch lived only in the per-run clone; the next claim deleted
  that clone and re-seeded from the default branch, so a resume came back to an
  empty working tree while every other signal said it had resumed cleanly. The
  worker now fetches the agent's committed branch back into its own repository
  before cleanup, on both the park and the shutdown or eviction paths, and a
  resume rebuilds from that recovered work instead of silently rebasing onto a
  default branch that moved in the meantime. When work genuinely cannot be
  recovered, the run says so in its activity feed rather than re-treading it
  silently. The runner clone, plugin dir and per-run HOME are still preserved on
  a park exactly as before; removing the now-redundant clone-preservation step is
  a deferred follow-up (issues [#218](https://github.com/vtmocanu/uzi/pull/218), [#224](https://github.com/vtmocanu/uzi/pull/224)).

- **Subagents in ten of the eleven builtin agent templates now reach the team
  lead when they report back.** Each template's "report via SendMessage"
  instruction named the recipient "the team lead," but the only address
  reachable from a builtin body is `main`: the lead's own template runs as the
  main thread, not as an invokable subagent, so a call addressed to "the team
  lead" (or a bare "lead") failed with "No agent named 'lead' is reachable."
  Measured across three real runs, 8 of 26 SendMessage reports failed this
  way. Five subagents recovered by resending to `main`, each costing a
  regenerated report of several kilobytes plus a few tool round-trips; the
  other three gave up on SendMessage entirely and returned their findings
  through a different channel, each prefixed with an apology about uzi's own
  plumbing that a human then read in the report. The thirteen recipient
  instructions across the ten templates now name `main` explicitly; the role
  is still called "the team lead" in surrounding prose, since a blanket
  rename there would misread as proposing action to the tool address rather
  than describing the role. **Operators:** the fix does not reach an install
  that has already booted, since builtin seeding never overwrites an existing
  template row. An admin must open each of the ten affected templates and
  click **Reset to default** to pick it up (issue [#210](https://github.com/vtmocanu/uzi/pull/210)).

- **Release tooling: the CHANGELOG coverage gate could block a good release,
  about once every dozen runs, and a retry made it go away.** It reported a
  merge as uncited while the section cited it on the line the check had just
  matched. Cause: `printf '%s' "$SECTION" | grep -q <pattern>` under
  `set -o pipefail`. `grep -q` exits the instant it matches and closes the pipe;
  bash's `printf` builtin writes line-buffered, so an 8.5 KB section leaves the
  shell as 72 separate writes and is still writing when grep goes; the next
  write takes SIGPIPE and `pipefail` reports 141 for a pipeline whose grep
  succeeded. Every `grep` in the script now reads a file, which has no writer to
  kill. Present since the gate was first added, on the issue-number lookup and
  on both escape hatches, not only on the short-SHA citation added the same day
  it was found (`19ad63c3` was the merge it flagged; job 134884 failed, retry
  134892 passed on the identical commit). Developer-facing only: no change to
  how uzi behaves.

- **A hostile server can no longer flood your terminal through `uzi version`.**
  The build-info strings uzi prints are now capped as well as stripped of
  terminal control characters: the stripping arrived with the shared render
  boundary (issue [#180](https://github.com/vtmocanu/uzi/pull/180)), but that boundary deliberately does not truncate, so a
  server returning a megabyte-long version string still printed all of it. The
  version line is bounded now (issue [#144](https://github.com/vtmocanu/uzi/pull/144)).

- **Hosted worker pods now declare an ephemeral-storage request (512Mi plain,
  4Gi docker-tier) on the worker container, and the Docker sidecar's data root
  moved off the pod's ephemeral storage onto its own PVC.** Every container in
  a worker pod previously requested zero ephemeral storage, so the scheduler
  placed pods with no account of their real disk footprint and kubelet ranked
  them first for eviction the moment a node ran low; an evicted pod's runs
  re-queue onto a stale local clone and lose every uncommitted local commit
  (one measured loss ran to 82 minutes of work, issue [#209](https://github.com/vtmocanu/uzi/pull/209)). **This lowers how
  often that happens; it does not close it.** A declared request changes
  kubelet's eviction ranking, it does not make a pod eviction-proof, and the
  fix for the underlying loss, fetching work back before an evicted worker's
  tree is discarded, is issue [#218](https://github.com/vtmocanu/uzi/pull/218)'s, not this change's. **Operators:**
  rolling this out replaces every worker pod once, so every run in flight at
  deploy time loses its tree, the same accepted cost as any other
  worker-pod-spec change. It ships inside the chart, so merging to `main`
  alone does not deploy it: delivery needs a `v*` release tag published to
  Harbor plus a manual `targetRevision` bump in the `argo-apps`
  repo's `apps/uzi/app.uzi.yaml`. The new per-docker-worker dind-data PVC
  (20Gi, chart-overridable) is a third RWO volume per worker, widening the
  existing Multi-Attach window on reschedule by one; it is never
  garbage-collected, so `docker system prune` on the worker is the reclaim
  path, and the daemon's build cache now survives a pod restart instead of
  being wiped with the old emptyDir. Raising `workers.docker.dindDataSize` on
  a live cluster is a silent no-op for workers that already exist: the size
  isn't part of the pod spec hash and the PVC is never resized after
  creation, so only a newly-provisioned worker picks up a raised default
  (issue [#224](https://github.com/vtmocanu/uzi/pull/224)).

- **The worker controller now refuses to boot when a rendered worker PVC
  would exceed its tier's LimitRange storage ceiling, instead of retrying
  forever on a worker that provisions and never appears.** A preset's
  `/data`, `/nix`, or the dind data PVC could be sized above what the
  cluster's LimitRange allows and still pass `helm template` and the
  controller's own boot cleanly; the PVC create was then rejected on every
  reconcile tick with the reason visible only in the controller's own log,
  nothing reported back to the api, and no worker ever appeared. The boot
  check now names every offending claimant, its size, and its ceiling, and
  which value to lower, as a `CrashLoopBackOff` an operator will actually
  see. Existing worker pods and runs in flight are unaffected, since the
  api's run lanes never go through the controller. **This is a genuine new
  cost, not a pure improvement: provisioning, drift reconciliation, and
  teardown all run in the same reconcile loop, so a prolonged crash-loop
  also stops teardown, leaking PVCs and storage quota for workers the api
  has already dropped.** Before this change the same misconfiguration gave
  a silently stalled fleet with teardown still working; now it gives a loud
  failure with teardown stopped. **Operators:** recovery needs a chart
  values change plus an ArgoCD sync, not `kubectl set env` on the
  Deployment: `selfHeal: true` reverts that the moment it notices (issue
  [#224](https://github.com/vtmocanu/uzi/pull/224)).

### Security

- **The `uzi` CLI no longer renders server-supplied text on your terminal
  raw.** Every human-readable render path (tables, printed lines, the error
  line, login and version output) passed the server's strings straight
  through, so a hostile or compromised server the user had pointed `--url`
  at could clear the screen, rewrite the window title, or use a bidi
  override to make a name read as something other than what it says.
  Terminal control characters and Unicode format characters (the bidi
  overrides, zero-widths, the BOM) are now stripped at a shared render
  boundary before anything reaches stdout; `--json` output is deliberately
  untouched and stays the lossless forensic channel, because its bytes go to a
  parser rather than to a terminal. `encoding/json` escapes C0 and
  U+2028/U+2029 only, so DEL, the C1 range and the Cf characters above (bidi
  overrides, zero-widths, the BOM, the soft hyphen) all survive in `--json`:
  piping it straight to a TTY is outside the guarantee. **The accepted cost:** a zero-width joiner is itself one
  of the stripped characters, so a multi-part emoji built from one (a family
  emoji) now renders as its separate members instead of the joined glyph; a
  single-codepoint emoji is unaffected (issue [#180](https://github.com/vtmocanu/uzi/pull/180)).

- Bumped `golang.org/x/text` v0.38.0 → v0.39.0, closing `GO-2026-5970` (infinite
  loop on invalid input), and `github.com/yuin/goldmark` v1.7.8 → v1.7.17,
  closing `GO-2026-5320` (XSS). Both were pre-existing on `main`, caught in an
  unrelated `govulncheck` scan and folded in here rather than filed separately.

## [0.14.0] - 2026-08-03

### Added

- **The run page now says when a judge is already on its way**, instead of
  offering a **Run judge** button whose only possible outcome was an error
  toast. A finished run enqueues a judge automatically, but the review panel
  used to show the same "not judged yet" state whether one was in flight or
  not, so the obvious click hit the one-active-judge-per-target index and came
  back a 409. The panel now reads **Judge scheduled** or **Judge running…**
  with the button disabled, keeps any existing verdict on screen while a
  re-judge runs, and swaps in the new one on its own. It is server truth, so it
  survives a reload and shows to every viewer of the run, not just the tab that
  started it. `uzi review` and its `--json` output carry the same distinction
  via a new `pending_judge` key (issue [#119](https://github.com/vtmocanu/uzi/pull/119)).

### Changed

- **Contributor tooling: the `lead` template's phrase pins are now scoped to the
  region of the prompt they belong to**, so moving a rule between the plan-turn
  paragraph and the post-implementation bullet fails the test instead of
  satisfying it from the wrong section (issue [#205](https://github.com/vtmocanu/uzi/pull/205)). Test-only: no change to how
  uzi behaves.
- **The plan you approve has now been read against the code first.** The `lead`
  must back its plan with citations — for every mechanism the plan asserts, the
  file that implements it and the line — and it collects them by sending the
  allocated read-only validators over the plan itself before submitting it for
  approval. That wave reports only; nothing in the worktree changes before you
  approve. Validators still fan out again over the diff after each
  implementation unit lands, which used to be the only time they ran, so a
  wrong plan was discovered only once it had been built (issue [#197](https://github.com/vtmocanu/uzi/pull/197)).
  **Operators: a shipped change to a builtin prompt does not reach an existing
  install** — an already-seeded template row is never overwritten. An **admin**
  must open the `lead` template and click **Reset to default** to pick this up
  (editing a builtin is admin-only; everyone else gets a 403). Reset re-applies
  the shipped body verbatim, so re-apply any local customization on top. See
  [docs/agent-templates.md](docs/agent-templates.md#resetting-a-builtin-template).
- **Contributor tooling: the `agent` suite's per-test timeout is 120000, and its
  largest test file was split into seven.** The cap was binding in CI and nowhere
  else — node isolates test files in a child process per file on the CI image but
  shares one process locally, so the same flag caps a whole *file* there and each
  *suite* here, and a file running 96s locally passed a 30s cap while failing it
  in CI. Splitting the file is what removed the knife edge; the raised cap only
  bought margin. Cut the whole agent suite from 112s to 46s locally
  (`37eefea6`, `32ff4bcf`). Developer-facing only: no change to how uzi behaves.

- **Contributor tooling: the prior-art reference projects are no longer vendored
  as git submodules.** They were moved to ordinary clones kept outside the repo,
  so a fresh clone no longer drags the corpus along (`19ad63c3`). Developer-facing
  only: no change to how uzi behaves, and a worker container never had access to
  the corpus either way.

- **Contributor tooling: both Go modules are now `gofmt`-clean and a format check
  runs in the gate.** `task fmt-check` fails on any formatting drift and names the
  files; it also runs first inside `task gate:api` / `task gate:controller` and
  first in CI's `validate:api` / `validate:controller`, so drift is caught on every
  MR instead of accumulating. Part of the developer-loop quality-gate work in
  PRD #103, which also moved every gate recipe into the root `Taskfile.yml`.
  Developer-facing only: no change to how uzi behaves.
  See [docs/dev-conventions.md](docs/dev-conventions.md).

- **Contributor tooling: linting now runs in the gate and on every MR**, for the
  first time in this repo. `task lint` covers all four components and each
  `task gate:<component>` runs its own. Go gets golangci-lint (`errcheck`,
  `staticcheck`, `ineffassign`, `unused`, `unparam`, and `nolintlint`, which
  lints the suppressions themselves so a bare or vacuous `//nolint` is a finding
  rather than a silent blanket exemption), ratcheted against
  `origin/main` so only findings a branch introduces block; `web` and `agent` get
  oxlint, configured so `react-hooks/rules-of-hooks` actually runs, which the
  default severity tier does not reach. The 16 pre-existing TypeScript findings
  were fixed rather than baselined. Part of PRD #103. Developer-facing only: no
  change to how uzi behaves. **Existing checkouts need
  `npm install --ignore-scripts` in *both* `web/` and `agent/` before
  `task gate:web` / `task gate:agent` will run** — oxlint is a new devDependency
  and the lint step fails closed with `oxlint: command not found` until it is
  installed. `--ignore-scripts` matters in `agent/`: without it, `agent-browser`'s
  postinstall rewrites the host's `agent-browser` binary. See
  [docs/dev-conventions.md](docs/dev-conventions.md).

- **Contributor tooling: dead code is now detected in the gate and on every MR.**
  `task deadcode` covers all four components and each `task gate:<component>`
  runs its own. The Go modules use `golang.org/x/tools/cmd/deadcode` against a
  committed baseline that ships **empty**, so both are held at zero — the one
  unreachable function that existed was deleted rather than baselined. `web` and
  `agent` use knip, which gates unused files and dependencies at zero while
  reporting unused *exports* without failing the build; burning that tier down
  is tracked as issue [#206](https://github.com/vtmocanu/uzi/pull/206). Neither tool sees a dead *branch*, which stays a review
  question. Part of PRD #103; no new CI jobs. Developer-facing only: no change to
  how uzi behaves. **Existing checkouts need `npm install --ignore-scripts` in
  *both* `web/` and `agent/`** before `task gate:web` / `task gate:agent` will
  run — knip is a new devDependency and the step fails closed with
  `knip: command not found` until it is installed, the same way oxlint did.
  See [docs/dev-conventions.md](docs/dev-conventions.md).

- **Contributor tooling: shell scripts, YAML and the Homebrew formula are now
  checked in the gate and on every MR.** `task gate:repo` runs first inside
  `task gate` and covers the checks that belong to no single component:
  shellcheck over every tracked shell script (the git index unioned with a
  shebang scan, so `agent/templates/entrypoint.sh` and `agent/bin/agent-browser`
  — the worker container entrypoint and the `agent-browser` shim inside every
  worker pod, neither reachable by extension alone — are finally in scope),
  yamllint over every tracked YAML except the Helm templates (which are Go
  templates, not YAML), and a syntax check of `Formula/uzi-cli.rb`, which release
  CI copies verbatim into the shared tap on every tag. All three gate at zero: the
  four shellcheck warnings and three yamllint findings that existed were fixed
  rather than suppressed. One new CI job, `lint:repo`. `web/scripts/check-docs.mjs`
  also now validates relative links in `prds/*.md` and `adr/*.md`, which nothing
  checked before, and the three that had rotted are repaired. Part of PRD #103.
  Developer-facing only: no change to how uzi behaves. **None of the three tools
  is required locally** — a missing shellcheck, yamllint or Ruby ≥3.1 prints a
  loud skip and passes rather than blocking you (the formula check falls back to
  Homebrew's own bundled Ruby first, so it only skips outright without Homebrew
  either). **A shellcheck that IS present must be exactly 0.11.0**: the version
  is asserted and a mismatch exits 2 rather than quietly grading differently,
  because 0.10.0 does not report one of the diagnostics this repo relies on. CI
  always runs all three. **Adding an `ignore` key to `.yamllint` — top-level,
  `ignore-from-file`, or an indented per-rule `ignore` inside a `rules:`
  block — is refused at exit 2, before yamllint even runs**, because it would
  silently narrow what gets read while the gate's success line still reports
  the tracked-file count from its own separate query, so the two would quietly
  disagree. Disabling a *rule* is still a legitimate, deliberate suppression —
  it leaves that count truthful, only an ignore key breaks the agreement
  between what was counted and what was checked. The clean-run line now says
  files were **handed to yamllint** rather than **checked**, which is the
  claim the script can actually make good on. See
  [docs/dev-conventions.md](docs/dev-conventions.md).

- **Contributor tooling: the repo is now scanned for secrets on every gate run
  and every MR**, which nothing did before. `task scan:secrets` runs gitleaks
  (pinned to v8.30.1) over every tracked file and joins `task gate:repo`, so it
  is part of `task gate` and of CI's `lint:repo` job. Eight fake-credential test
  fixtures that the first scan reported are now annotated individually with a
  written justification, rather than by excluding test files as a class. The
  seven `gitleaks:allow` annotations this repo already carried — written before
  anyone had run gitleaks — were each checked by removing it and re-scanning:
  five suppress a real finding and keep their line, two suppressed nothing and
  are gone. **The scanner's own configuration lives in the tree it scans**, so a
  four-line `.gitleaks.toml` could switch it off in the same merge request that
  adds a secret, and the job would report green. A committed canary closes that:
  `scripts/gitleaks-canary.txt` holds a fake token the scan must report on every
  run, and any allowlist broad enough to hide a real secret hides the canary too,
  so the check exits 2 (instrument broken) instead of 0. A run that finds nothing
  now prints the canary it did find. Part of PRD #103. Developer-facing only: no
  change to how uzi behaves. **gitleaks needs no separate install** — it arrives
  through `go run`, like the linters — so unlike the three checks above this one
  has no skip and always runs. See
  [docs/dev-conventions.md](docs/dev-conventions.md).

### Fixed

- **The run page's token counts were low, by 2.5x on output and up to 229x on
  input, and now match the board.** The run page read one field of each result
  frame while every rollup surface (the board, `uzi run list`, `GET /api/usage`,
  the admin totals) read another, and on the current Anthropic SDK those two
  fields no longer agree. Cost was low too, by whatever a model that dropped out
  of the run's last frame had spent. Both surfaces now fold the same field, per
  model, and a recorded fixture from a real run pins them to each other from both
  sides so they cannot drift apart again (issue [#195](https://github.com/vtmocanu/uzi/pull/195)). This also unblocks the
  live cost estimate in PRD #194.

- **Git repositories the worker creates no longer leave a background daemon
  watching their directory.** Any git command in a repo where `core.fsmonitor`
  is on spawns `git fsmonitor--daemon --detach`, which reparents to init and
  holds directory handles for as long as it lives, so the run's cleanup deleted
  every file and then could not remove the directory. Issue [#127](https://github.com/vtmocanu/uzi/pull/127) removed a
  different detached child (`git maintenance run --auto`) and could not cover
  this one: a retry absorbs a lock held for milliseconds, not a watcher that
  never lets go. The worker's bare clone, the runner clone and the seed
  destination now set `core.fsmonitor=false`. **In a worker container this is a
  no-op** — the daemon dies with the container either way — so the effect is on
  local development, where one such daemon was found still alive 21 days after
  its repo was created (issue [#127](https://github.com/vtmocanu/uzi/pull/127)).

### Security

- **The web image's SPA build stage now runs `npm ci --ignore-scripts`.** No
  dependency in the current lockfile tree would execute a lifecycle script
  while producing the bundle served to users; the flag keeps that true across
  the next lockfile refresh, which is when nobody is looking. Hardening, not a
  fix: the image built both ways is byte-identical today. Deliberately not
  applied to the worker's base image, whose postinstall is what bakes in the
  `agent-browser` CLI.

## [0.13.0] - 2026-07-29

### Added

- **An agent can now stop mid-run and ask you a question**, instead of guessing or
  stalling on something it shouldn't decide alone. The run parks with a **needs
  your answer** badge, the question lands in the run feed with any suggested
  options, and you answer from the run view's composer, a reply in the run's
  Slack thread, or `uzi run answer <id>` — all three read the same open question
  off the feed, so no surface invents a question the others don't have. Left
  unanswered, the run fails closed ("clarification timed out") rather than
  hanging forever. Autopilot runs never park on a question — they proceed on
  their own best judgment and record the assumption in the feed instead
  (PRD #88). See [docs/run-activity.md](docs/run-activity.md).

- **The footer version badge is now a build-info popover.** Hover, focus or tap
  it to see the release version, the project's founding date, age, and process
  uptime, plus, on a release image built in CI, the source commit, build
  timestamp and commit count. `uzi version` now reports the server's build info
  alongside its own (PRD #175). See [docs/cli.md](docs/cli.md).

### Fixed

- **A run whose owner had already answered no longer keeps nudging them to
  approve a plan.** The health check flagged `approval_idle` purely on how long
  `runs.updated_at` had sat still, and a revise response deliberately never
  touches that column — so a user who requested changes was told for the rest
  of the wait that they were the one being waited on. Slack, the board, and the
  run view now report that the run is waiting on the worker instead (issue [#182](https://github.com/vtmocanu/uzi/pull/182)).

- **The plan-revision cap could be exceeded by two concurrent submissions.**
  Two revise requests arriving at the cap's last slot could both read the same
  pre-update count and both land, letting a run exceed `PLAN_MAX_REVISIONS`.
  The cap is now enforced atomically inside a single row update instead of a
  read-then-insert, closing the race for both the web and Slack paths
  (issue [#106](https://github.com/vtmocanu/uzi/pull/106)).

## [0.12.0] - 2026-07-28

### Added

- **An agent can now stop mid-run (or before planning) and ask you a
  question**, instead of guessing or stalling on something it shouldn't
  decide alone. The run parks with a **needs your answer** badge, the
  question lands in the run feed with any suggested options, and you answer
  from the run view's composer, a reply in the run's Slack thread, or `uzi
  run answer <id>` — all three read the same open question off the feed, so
  no surface invents a question the others don't have. Left unanswered, the run fails
  closed ("clarification timed out") rather than hanging forever; both the
  answer deadline and the per-run question cap are held in worker memory and
  reset on a worker-death requeue, so the honest worst case over a run's
  life is 48h and 10 questions on the defaults, not 24h and 5. Autopilot
  runs never park on a question — they proceed on their own best judgment
  and record the assumption in the feed instead (PRD #88).

- **The footer version badge is now a build-info popover.** Hover, focus or tap it to see the
  release version, the project's founding date, age (computed client-side, so it stays correct
  between releases), and process uptime, plus, on a release image built in CI, the source commit,
  build timestamp and commit count. `GET /api/version` keeps its `version` key unchanged, both
  name and value, and adds the rest of those fields alongside it; unstamped fields are omitted
  rather than zero-filled, so a local `docker compose` build reports only version, founding date
  and uptime, and the commit count is available only on a tagged release build. `uzi version`
  now reports the server's build info alongside its own: the CLI's own version stays top-level
  and the server's nests under a `server` key, so existing `--json` parsers are unaffected
  (PRD #175).

- **Manual drag-to-reorder for board cards, a sort switcher, and a keyboard
  equivalent.** Drag a card within its column to set the board's order by
  hand, or use the new per-card up/down buttons for the same reorder without
  a pointer: native HTML5 drag-and-drop has no keyboard path in any browser.
  A five-mode switcher (Manual, Issue number, Recent run activity, Last
  updated, Title) lets you view the board differently without discarding
  the manual order underneath it; Manual is the default and reproduces
  plain issue-number order on a board nobody has touched. A reorder is
  durable per owner and follows you across devices (PRD #102 M5).

- **Cards show their other GitLab labels as chips**, so `bug` or `security`
  is visible without opening the issue. The PRD label, the PRDLESS escape
  hatch, the autopilot label, and the card's own column label are excluded,
  since none of those tell you anything the card doesn't already show some
  other way. A card with many labels shows the first few plus a `+N` count
  with the rest on hover (PRD #102 M4).

- **A per-browser "Show other issues" toggle reveals a repo's open issues
  that don't carry the PRD label**, off by default so a board nobody
  touches looks exactly as it did before. Those cards render with a dashed
  border and offer **Promote to PRD** in place of Start run, since they
  can't start a run until they carry the label (PRD #102 M6).

- **Workers can be set to pick their Anthropic token automatically.** Settings → Workers gains
  an **auto (from pool)** option beside the existing per-token choices, and `uzi worker
  set-token <id> --auto` does the same from the CLI. An auto worker chooses per claim from the
  tokens you opted into the pool, so it needs no restart and no new join token to change what
  it spends. Existing workers are untouched: one with a pinned token stays pinned, one without
  stays on your default. A worker whose pinned token you later delete now says plainly that it
  uses your default, rather than showing a pin to a token that no longer exists. If you set a
  worker to auto while your pool is **empty**, the row says so rather than claiming it
  auto-selects: every claim would spend your default token, and finding that out from a
  finished run is too late (PRD #111 M3).

- **An opt-in pool for auto-selecting Anthropic tokens.** Settings → Anthropic tokens gains a
  per-token "Auto-select from this token" toggle, and `uzi token pool <label> --on|--off` does
  the same from the CLI. The pool is empty by default and opting a token in is deliberate: a
  pool that helped itself to every credential would spend the one you reserved for something
  else. Beside the toggle, each pooled token shows whether auto-selection could actually pick
  it right now — `in pool`, or `never polled` / `stale reading` / `no usage data` / `low
  headroom` when it could not. That is the point of the chip rather than decoration: a token
  uzi has never managed to read a usage figure for can never be picked, and without the chip it
  would sit there looking active. The status is computed server-side from the same rule the
  selector uses, so the page cannot promise something the selector will not do (PRD #111 M2).
  New operator knobs `UZI_AUTOSELECT_MIN_HEADROOM`,
  `UZI_AUTOSELECT_HEADROOM_TIE_PCT`, `UZI_AUTOSELECT_MAX_STALENESS` and
  `UZI_AUTOSELECT_INFLIGHT_PENALTY` — see
  [docs/configuration.md](docs/configuration.md).

- **uzi now picks the Anthropic token, per claim, by rate-limit headroom.** A worker set to
  auto spends whichever of your pooled tokens has the most room left — headroom being whichever
  of the 5-hour and 7-day windows is fuller, since both have to allow the run. Two tokens within
  a few points of each other are separated by which one **replenishes soonest**, and it is the
  reset of the window actually holding it back that counts: a 5-hour reset does not relieve a
  7-day cap. A small bias against tokens with runs already in flight keeps several claims
  arriving together from piling onto the same credential. Operators can tune all of it
  (`UZI_AUTOSELECT_*`); nobody has to.

  **It never fails a run**, and the ways it can decline to pick are worth knowing apart: nothing
  opted in, nothing with a current reading — which is also what a switched-off usage poller
  looks like — or a chosen token whose stored value would not decrypt. Each falls back to your
  default token and says which happened, in the run view and in `uzi run get`.

  🔴 **"Auto" does not mean "only my pool".** The fallback spends your **default** token, and
  that path does not consult the opt-in — so a token you deliberately kept *out* of the pool can
  still pay for a run if it happens to be your default. There is no third option, because
  failing the run would be worse; if you want a credential never spent by ordinary runs, it must
  not be your default either (PRD #111 M4).

- **Every run now names the Anthropic credential it spent, and why that one.** The credential
  is **recorded on all three lanes** (issue/ci_fix, judge and chat) and **shown in the run
  view** as a `token <label> — <mode>` chip and by `uzi run get` as an `ANTHROPIC_TOKEN` row.
  A chat conversation has its own view and does not show the chip yet, though its spend is
  recorded like any other run's. Until now a run recorded what it cost but not which account
  paid, which was a distinction without a difference while a user could hold one token and
  stopped being one the moment PRD #104 let them hold several. The label is a snapshot taken
  when the run was claimed, so it survives that token being renamed or deleted: a finished run
  keeps naming the account it billed even after the credential is gone, and says `(deleted)`
  so it is not mistaken for one you can still go and look at. Runs claimed before this landed
  show nothing rather than a guess.

  **The mode is there because the label alone cannot answer the question.** An automatic pick
  and a fallback to your default can name the *same* token, so the chip says which of them
  happened: `console-key — auto, 62% headroom`, `console-key — pinned`, `default — judge
  binding`. When an `auto` worker did *not* get an automatic pick, it says so and says why —
  `default (auto: no fresh usage readings)` — and links to Settings → Anthropic tokens, which
  is where that particular problem gets fixed. Those runs are the only ones that carry a
  warning colour: nothing failed, but your pool did no work, which is usually a setting you
  can change. A run that spent the least-consumed token of a pool where **every** token was
  nearly exhausted is called out separately, in blue rather than amber — it worked, but your
  pool is nearly out — and links to the same page. `uzi worker list` also gains a **TOKEN**
  column, so the three-way choice the CLI could already *set* is finally one it can *show*
  (PRD #111 M1 + M5).

- **A run that hits your Anthropic usage limit can now pause instead of failing.** Opt in
  per run from the run view, or set the default for every new run in Settings — off until
  you turn it on, and the Settings default is the **only** way to reach an autopilot,
  CI-fix, or self-improvement run, since none of those has a start button of its own. A
  paused run keeps its branch, its history, and an already-approved plan: once your 5-hour
  or 7-day window reopens it resumes on the same worker in the same session, without asking
  you to re-approve a plan it already had. It can pause more than once if the limit keeps
  recurring, backing off between attempts, up to a retry budget an operator can tune
  alongside how long any one pause may last (`RUN_LIMIT_MAX_WAITS`, `RUN_LIMIT_MAX_PARK` —
  see [docs/configuration.md](docs/configuration.md)). A run that stays opted out still
  fails the moment it hits the limit, exactly as before, but now says which window and when
  it resets instead of a bare `agent run failed: error_during_execution` (PRD #35).

### Changed

- **The board's implicit first column is now named Backlog, not Open.**
  Display only: it's still the lane for issues carrying none of the
  configured column labels (PRD #102 M1).

- **Newly connected repos now seed Planned, In Progress, Human Review,
  Later, with Planned first, replacing Upcoming.** Existing boards keep
  their Upcoming column as-is; see
  [docs/configuration.md](docs/configuration.md) for the manual rename
  procedure (PRD #102 M2).

- **Only PRD-labelled issues can start a run.** Start-run previously
  accepted any issue with a PRD link or the PRDLESS escape hatch, so a
  stranger's open issue that merely mentioned a `prds/*.md` path could
  satisfy both and become runnable, by hand or from autopilot. The label
  gate now runs first and applies uniformly to the manual handler, the
  poller, and the CLI (PRD #102 M6).

- **Anthropic token labels can no longer contain invisible formatting characters.** Zero-width
  spaces and joiners, bidirectional overrides, the byte-order mark and the soft hyphen are now
  rejected when a token is created or renamed, alongside the control characters that were
  already rejected. The reason is what a label is for: it is the string you read to answer
  "which account did this run bill?", and an invisible codepoint breaks exactly that — a
  right-to-left override makes a label *read* as a different account, and a zero-width space
  makes two genuinely different tokens draw identically in a browser while remaining distinct
  rows. **The accepted cost, stated because it is a real one:** the zero-width joiner is itself
  one of these characters, so multi-part emoji built from it (`👨‍👩‍👧`) can no longer be used in a
  label; a single-codepoint emoji (`🔑`) is unaffected. Rejecting only the bidirectional
  controls would have left the look-alike problem in place while appearing to fix it. Labels
  saved before this landed are untouched (PRD #111 M2).

- **A guardrail denial no longer reads as a failure in the run activity feed.** When a
  PreToolUse guardrail denies a call — a `git push`, an env or `/proc` read, an unassembled
  subagent — the agent recovers and the run carries on, but the feed painted the result with
  the same red "✗ error" badge as a genuine tool crash, so a healthy run looked broken to
  anyone watching. Such results now render a third, calm state: a neutral "⊘ blocked" chip
  (only the glyph is warn-tinted, because a full warn chip would collide with the
  "plan awaiting approval" and slow-duration tints) that stays collapsed rather than
  auto-expanding, since a handled and recovered-from denial does not want attention. A real
  failure is untouched — still red, still auto-expanded. The chat surface inherits it, as it
  renders through the same row. Detection is render-time, keyed off the `"denied by guardrail"`
  phrase all 15 deny reasons carry, so historical runs get the calm chip too with no persisted
  marker and no migration; `is_error` is deliberately left true on the stored frame, which keeps
  the record honest to what the SDK emitted and is what leaves every downstream reader — the
  judge's missing-tool prescan among them — seeing exactly what it saw before. The phrase must
  START a line of the result, not merely appear in it: matching anywhere would have meant a
  genuinely failing command whose output quotes the phrase rendering calm and collapsed, and this
  change created that case itself, since a red `npm test` in the agent prints test titles carrying
  the phrase. The coupling to the phrase is real and is pinned from the agent side, since the two
  are separate npm packages: `agent/test/guardrails.test.ts` now drives all 15 deny paths through
  the public API and also scans the reason declarations in source, so a future 16th reason added
  without the phrase — or written in a form the scan cannot read — fails there rather than
  silently turning its chip red again (issue [#116](https://github.com/vtmocanu/uzi/pull/116)).

### Fixed

- **A crash-looping hosted worker's diagnostics no longer vanish the moment its pod
  reports Ready.** A `settled` roll report overwrote `blocking_container`,
  `blocking_reason`, `restart_count` and `last_exit_code` with empties whether or not it
  had looked at them, so a worker with five restarts and exit 1 could read as pristine
  in the database at exactly the moment someone was reading the row to debug it. The
  four columns now move together and are only replaced by a report whose phase
  (`rolling`/`stuck`) means the controller actually measured them; the worker's own
  authenticated version move still clears them (issue [#145](https://github.com/vtmocanu/uzi/pull/145)).

- **The Workers page badge no longer flickers `upgrade failed` → nothing →
  `upgrade failed` while a container crash-loops.** The worker container has no
  readiness probe, so `Ready == True` means only that the process started, not that the
  agent works — a Ready-but-flapping container was being reported `settled`. A Ready pod
  is now withheld from `settled` while any container has 3+ restarts and its current
  instance has been up less than 10 minutes, and self-clears once the container stays
  up. A negative container uptime (clock skew between kubelet and controller) no longer
  reads as flapping either (issue [#145](https://github.com/vtmocanu/uzi/pull/145)).
  See [docs/worker-upgrades.md](docs/worker-upgrades.md).

- **The run-view usage tables' left-aligned cells (the Agent, Phase and Model columns)
  are legible again instead of uniformly dimmed.** Two Tailwind classes of equal
  specificity were both emitted on the same cell, and stylesheet order picked the
  muted one every time (issue [#152](https://github.com/vtmocanu/uzi/pull/152)).

- **The run-view usage tables are usable with a screen reader.** Column headers now
  carry `scope="col"`, each table has its own accessible name, and the decorative
  disclosure triangles next to each `<details>` no longer get announced as a second,
  contradictory reading of the same expand/collapse state.

### Security

- **Unicode "format" characters — bidi overrides like U+202E, zero-width
  spaces/joiners, the BOM — in untrusted text can no longer make it render in an
  order its bytes don't have (Trojan Source, CVE-2021-42574).** Stripped at
  render — in visible text and in attributes like tooltips and accessible names —
  everywhere judge output, run titles, forge- or agent-supplied issue, proposal and
  memory text, the worker fleet page, and a filed-issue draft seeded from judge text
  reach the UI; stripped at review ingest so a hostile character can no longer be
  stored in the first place; and stripped from the CLI's worker table. A worker's own
  name is covered too — the one case where the reader isn't the field's own owner: it
  could otherwise render, crafted, in an admin's fleet list next to a different user's
  email. Coverage is per-surface, not blanket: `agent_label` is a separate, still-open
  gap tracked as issue [#164](https://github.com/vtmocanu/uzi/pull/164) (issue [#124](https://github.com/vtmocanu/uzi/pull/124)).

- **The judge no longer recommends installing a tool that policy permanently forbids.**
  A credential-bearing CLI such as `glab`, `gh`, `aws` or `az` is barred outright, even
  against an explicit admin allowlist, because a logged-in one reachable from the agent's
  shell would defeat the rule that the worker holds the forge credential and the agent does
  not. But nothing told either half of the judge that: the deterministic missing-tool scan
  keys on a `command not found` string rather than on whether the tool could ever be
  installed, and the judge model was never told the class exists. Both could produce an
  install-worker-tool recommendation nobody could action, and two such recommendations had
  reached the backlog. Seeing one of these absent is the policy working, so the scan now
  drops them and the judge prompt names the barred class, redirecting the model to report
  the wasted effort as a prompt or agent defect, which is actionable. Ordinary missing tools
  are unaffected and still reported. The
  suppression matches on the EXECUTABLE rather than the package name, which is the part
  that is easy to get wrong: the barred package is `awscli` while the command a shell
  reports is `aws`, so a name comparison would have covered `glab` and quietly missed the
  whole cloud-CLI half of the list.

- **Two password-manager CLIs an admin could allowlist are now barred, as was always
  intended.** The list of credential-bearing tools that may never be installed carried
  entries for `op` and `bw`, which are not real package names and therefore matched
  nothing — the 1Password and Bitwarden CLIs have been installable the whole time. The
  real names are now on the list. Nothing in the live allowlist or any repo's tool
  profile named either, so no existing configuration is affected. What changes is that an
  administrator can no longer add either one to the allowlist; a repository asking for one
  was already refused, simply for not being allowlisted rather than for being barred.

## [0.11.12] - 2026-07-27

### Fixed

- **`file` did not work in 0.11.11, despite that release announcing it.** The package was
  declared as bare `file`, and devbox resolved that to the package's `dev` output -- headers
  and libmagic, containing no `file` program at all. `devbox global install` reported success,
  so the image shipped with `file` simply absent from the toolchain, exactly the silent
  `command not found` the release was meant to remove. It is now declared `file.out`, the
  output that carries the program, and verified from the realized profile rather than from a
  shell that had the raw store path on its search path -- which is what made the original
  check look green. This is the same trap `openssl.bin` documents; the earlier note reasoned
  from a nixpkgs attribute that turns out not to govern what devbox installs, and that
  reasoning is now corrected in place. Found by the new guard below, on its first real run.

- **The worker image's toolchain guard had stopped covering the toolchain, and now cannot
  drift again.** The guard exists so a broken toolchain fails the image BUILD instead of
  shipping a silent `command not found` to every subagent at run time. It was a hardcoded
  line naming five binaries, and it never grew as the manifest did: `chromium`,
  `fontconfig` and `dejavu_fonts` arrived, then `file`, `perl`, `coreutils`,
  `kubernetes-helm` and `kubeconform` in 0.11.11, none of them guarded. By that release it
  was verifying 5 of 13 packages while reporting success, so a green `publish:agent` said
  nothing about whether `helm` resolved. Worse than an uncovered package is a green light
  nobody re-derives. The guard now checks its own coverage: it reads what `devbox global
  install` actually installed and fails the build if any package lacks a row in
  `agent/devbox-global/toolchain-guard.tsv`, so adding a package without guarding it is no
  longer possible. Each row also runs its tool rather than only resolving it, because a nix
  package can land its libraries and manual pages while its CLI never appears -- the
  `openssl.bin` bug this guard missed the first time. Packages that legitimately ship no
  binary are declared as such rather than guessed: `fontconfig` sounds like it provides
  `fc-cache` and does not.

## [0.11.11] - 2026-07-27

### Added

- **Five more tools every run can reach: `file`, `perl`, `fmt`, `helm` and `kubeconform`.**
  The first three close judge recommendations raised by runs that lost work to a missing
  executable — a fact-checker hex-dumped a PNG header to read its magic bytes because it had
  no `file`, a reviewer's mutation sweep exited 127 on `perl -0pi -e` and had to be rewritten
  as a Python heredoc, and a docs run hand-rewrapped the same paragraph twice for want of
  `fmt`. `helm` and `kubeconform` are new capability rather than repair: an agent editing
  `deploy/chart/` can now lint, render and schema-validate it in-worker instead of committing
  blind and learning from CI's `helm_chart` job minutes later. All five ride the same pinned
  nix/devbox path as the rest of the toolchain and are baked into the image at build time, so
  they cost no per-run provisioning. Two caveats are worth knowing rather than discovering:
  `coreutils`, which is where `fmt` comes from, places GNU versions of 82 busybox applets
  (`ls`, `cp`, `date`, `sort`, `stat`, …) ahead of the image's own on `PATH`; and the pinned
  nixpkgs revision carries helm 4 while CI gates on helm 3, so a local render is a smoke
  check and never the authority over a red `helm_chart` job.

### Changed

- **Hosted workers may now reach the CloudNativePG chart repository, at the cost of two
  general-purpose GitHub hosts.** Rendering uzi's own chart needs `helm dependency build` to
  fetch the `cluster` subchart, and that fetch redirects twice before it lands: from
  `cloudnative-pg.github.io` to `cloudnative-pg.io` for the index, then from `github.com` to
  `release-assets.githubusercontent.com` for the 63 KB tarball. All four names are now in
  `workers.fqdnEgress.allowFQDNs`, in both the chart default and the dev-cluster values —
  Helm replaces list values rather than merging them, so an entry added to only one of the
  two reaches no cluster. The last two names are by a distance the widest entries in that
  list (any repository, any release asset) on a fence whose stated purpose is the
  repository-exfiltration residual, so this is a deliberate trade and not an oversight. The
  same chart is also published as an OCI artifact on `ghcr.io`, which would narrow this to a
  registry carrying no repository or code access if the `Chart.yaml` change proves worth it.

### Fixed

- **The agent no longer reinstalls dependencies the worker has already provisioned.**
  0.11.9 added JS dependency provisioning before the agent's first turn, and it works: the
  first real run after it shipped installed both workspaces before the agent's first tool
  call, and hit zero `command not found` for a gate tool where earlier runs hit them
  routinely. But nothing told the agent any of that, so it planned an `npm ci` at plan time
  (when the background install genuinely has not finished yet) and then ran it. Since
  `npm ci` deletes `node_modules` before installing, that destroyed the provisioned tree and
  rebuilt it, costing exactly the time the feature exists to save. The agent is now told:
  the plan prompts state the mechanism without promising it will succeed, since the install
  can fail; the implement prompt carries the actual per directory results, reporting a
  failed directory as failed so the agent can react rather than trusting a claim that is
  false for that run. It also now says when discovery hit its directory bound, so a bounded
  scan cannot read as full coverage (issue [#157](https://github.com/vtmocanu/uzi/pull/157)).

- **Repo supplied directory names reaching the agent's prompt are contained.** Those names
  come from reading an untrusted cloned repo, and they land outside the fences that mark
  quoted content as data, which is the one position where instruction shaped text is what
  those fences exist to stop. A repo could commit a directory whose name reads as an
  instruction. Names are now filtered to a conservative character set, length bounded, and
  placed inside a nonce fence whose tag a repo cannot forge, since the tag is drawn at
  random during the run while the name was committed to git before the run existed. A name
  the filter had to alter is flagged as not being a usable path, with a pointer to locate
  the real directory, so honest names like `my project` or `café` do not silently become
  paths that cannot be found. This reduces the surface rather than closing it: the fence
  relabels the text as data, it does not remove it from the model's context (issue [#157](https://github.com/vtmocanu/uzi/pull/157)).

## [0.11.10] - 2026-07-27

### Fixed

- **The crossed-off steps in the dashboard's "Get the factory running" checklist are
  legible again.** The strikethrough was drawn in a border token (`--edge-strong`) sitting
  at roughly 1.9:1 against the card, while the struck text beside it is at roughly 6.9:1 —
  so the line marking a step done was all but invisible, in both the ember and mission
  themes. The decoration now inherits the muted text colour, which keeps it legible in
  every theme without adding a token (issue [#60](https://github.com/vtmocanu/uzi/pull/60)).

- **Two objects the chart declares were being deleted from the rendered manifest, so
  restricted-tier hosted workers could not be provisioned.** A Helm template comment
  ending `*/ -}}` immediately before a `---` trims the newline as well, gluing the
  document separator onto the previous value. The two objects on either side then merge
  into a single YAML document with duplicate keys, and a parser keeps only the last — so
  the `uzi-workers` ServiceAccount and the `InfisicalSecret` that materializes its Harbor
  pull secret silently vanished, taking the pull secret with them. The docker tier was
  unaffected only because its object happened to come last in each merged pair. Nothing
  cheap caught it: `helm lint` passed, `helm template` exited 0, the text still contained
  `kind: ServiceAccount`, and ArgoCD reported `Synced/Healthy` truthfully, being in sync
  with what the manifest declared once parsed. `scripts/assert-chart-render.sh` now
  asserts one `kind:` per document in CI (issue [#149](https://github.com/vtmocanu/uzi/pull/149)).

## [0.11.9] - 2026-07-27

### Added

- **JS dependency provisioning before the agent's first turn.** A run touching a JS repo (npm, pnpm, yarn, or bun, including monorepo workspaces) now gets its cloned repo's dependencies installed automatically: the worker detects the lockfile and kicks off a frozen, `--ignore-scripts` install as the run starts, overlapping it with the plan turn (and the approval wait, on human-gated runs) so it's ready before the agent's first implement turn, instead of the agent discovering a missing `node_modules` mid-task. Runs under the same runner-uid + scrubbed-env sandbox that already runs the repo's tests; an unrecognized layout or a failed install is skipped honestly, same as before (PRD #121).

- **A Model column in the run view's per-agent usage table.** The usage panel named the run's
  model only once, in the top strip — the main thread's. A run is multi-model (a subagent can
  pin its own model, PRD #37; the owner's default governs the lead, PRD #69), so the per-agent
  breakdown now shows which model each agent actually ran on: the model string normally,
  `<primary> +K` for an agent that spanned several, `—` for a run whose frames predate the
  feature. Tokens only — per-agent cost remains unavailable, and no cost surface changed
  (PRD #93).

### Fixed

- **A hosted worker that can never get a pod is now alerted on, and the alert no longer
  expires.** Two defects found by live cluster validation of 0.11.8, neither catchable
  before deploying. First, `deriveRollHealth` returned `rolling` unconditionally when a
  worker had zero pods, and every stuck-detection arm needs a pod, so a worker whose
  Deployment could not create one (a missing ServiceAccount, in the measured case) was
  reported as a healthy in-progress roll forever. It now reports `stuck` plus the
  Kubernetes reason when the Deployment carries `ReplicaFailure=True` (issue [#148](https://github.com/vtmocanu/uzi/pull/148)).
  Second, that alert then expired: the anti-suppression ceiling gated the `upgrade_failed`
  row as well as the `upgrading` one, and its clock starts when the api first believes a
  roll is in progress, which for a hosted worker is while it is still being provisioned.
  On the measured worker that left a 70-second window out of 45 minutes before the badge
  went quiet again, permanently. The ceiling now gates only the suppressing direction, so
  a worker the cluster keeps reporting as stuck keeps its badge until the pod recovers
  (issue [#151](https://github.com/vtmocanu/uzi/pull/151)). See [docs/worker-upgrades.md](docs/worker-upgrades.md).

- **A self-improve check could outlive its 15-minute cap and orphan a process.** The
  wall-clock cap was enforced by `execFile`'s own `timeout`, which kills from the worker
  uid, and under the runner-uid split that is `EPERM` against a process running as
  `runner`. Measured: a 2-second cap called back at 2008ms carrying `EPERM` while the
  runner's `sleep 120` was still alive six seconds later. Checks now run as a detached
  process group that is killed as a group (issue [#153](https://github.com/vtmocanu/uzi/pull/153)).

- **A check no longer runs against dependencies its own install failed to build.** The
  `node_modules` pre-flight treated a surviving directory as "deps ready", but a failed
  `npm ci` leaves the previous tree intact with the new dependency absent, so the install
  failed, the directory remained, the pre-flight passed, and the check reported a
  real-looking failure that was really a stale tree. The signal is now the gap between
  what the manifest declares and what the tree contains (issue [#154](https://github.com/vtmocanu/uzi/pull/154)).

- **The `/nix` guidance stopped flickering out of the upgrade detail strip.** A fast-failing
  `seed-nix` init container alternates between `CrashLoopBackOff` and `Error` as kubelet
  cycles it (a measured 71/29 split), and the explanation was gated on the first reason
  alone, so on roughly three polls in ten it vanished while the badge correctly stayed
  failed. The operator watched the explanation for an unchanged failure appear and
  disappear (issue [#146](https://github.com/vtmocanu/uzi/pull/146)).

- **The judge's missing-tool scan no longer flags a tool the run actually ran.** `scanCommandNotFound` used to text-match any `command not found` hit and report that tool as missing even when the same run later ran it successfully through an npm script (`node_modules/.bin/tsc`, `vitest`, `eslint`); it now checks up to a bounded window of the run's tool-invocation trace and drops a hit once that tool is seen running clean later in the run (PRD #121).

## [0.11.8] - 2026-07-26

### Added

- **Worker upgrade health.** Settings → Workers shows a per-worker upgrade badge, a Fleet
  upgrade summary, and a detail strip on a worker whose upgrade failed; the Workers nav item
  carries an alert-toned count of workers needing attention. `uzi worker list` gains an
  `UPGRADE` column. See [docs/worker-upgrades.md](docs/worker-upgrades.md) (PRD #113).

- **Builtin skills now ship allocated.** Each builtin carries a default shared allocation, seeded the first time uzi inserts the skill, so a fresh instance no longer needs an admin to click allocate before a builtin can reach a run. That gap was total rather than partial: allocations are what build the run's skill union, so an unallocated builtin reached nobody, not the subagents it was meant for and not the lead either. Seeding happens only on that first insert, so an allocation an admin removes afterwards stays removed; `ci-cd-norms` (whose row predates the mechanism everywhere, so its insert can never fire again) is backfilled onto existing instances by a migration, allocated to `coder` and `reviewer` (PRD #72 M2). See [docs/skills.md](docs/skills.md).

- **A `prd-lifecycle` builtin skill**, allocated to `lead` and `reviewer` by default: the playbook for updating the issue's linked `prds/*.md` file at the end of an issue run, ticking items only on direct evidence, and moving the file to `prds/done/` when (and only when) every item is complete. The lead's own instructions carry a short version of the same step so it does not depend on the skill being allocated, conditional in its wording on the issue actually linking a PRD, and the plan prompt now asks the submitted plan to name that step, so a human approving a plan sees that the repo's spec file changes too. Both layers are instructions to a model rather than enforcement: the merge request stays the check on whether a PRD update is honest (PRD #72 M3). See [docs/skills.md](docs/skills.md).

- **A run started with repo agents now carries the run's delivered skills.** Allocations key on your agent templates and a repo's `.claude/agents/` roster has none, so repo subagents previously received no delivered skill at all: a run started with agents from git silently lost every skill its owner had allocated. Each repo subagent now receives exactly the run's materialized skill union, which is the same set the lead already receives and no superset of it. Per-template scoping is unchanged on template runs. The trade, recorded rather than discovered later: a repo-authored subagent can now read every delivered skill body and could write it into the branch, accepted because skill bodies are user data and never secrets by product policy (PRD #72 M1). See [docs/repo-agents.md](docs/repo-agents.md).

- **The issue's own PRD link is corrected after the merge.** When an issue run reports that it moved its PRD to `prds/done/`, uzi rewrites that link in the issue description once the run's merge request has merged, so the link a human clicks still resolves against the default branch. It is edge-triggered (once, then never again, so it does not fight a later human edit) and only ever repoints a `prds/*.md` link the description already carried, matched on the moved file's name, so a link to a different PRD is untouched. The bound is that a run cannot introduce a link, not that it cannot pick among the ones already there: an issue linking several PRDs can have the wrong one repointed if the run declares a matching filename, which costs description integrity on that one issue and nothing wider. `ci_fix` and `self_improve` runs are excluded at both the write and the read side. Adds `UpdateIssueDescription` to the forge interface and both drivers (PRD #72 M5). See [docs/autopilot.md](docs/autopilot.md).

### Changed

- **`UZI_AGENT_VERSION` now carries a real build stamp instead of a frozen placeholder.** CI
  stamps the release into the agent image, and an unstamped image reports no version rather
  than the retired `0.1.0-m4` literal — so a hand-set value is no longer the only thing that
  variable could mean (PRD #113).

## [0.11.7] - 2026-07-26

### Added

- `uzi tui`: a full-screen terminal UI — a live board of your runs, a run detail view with a per-agent lane rail and live transcript, and in-place steering (follow-up, approve/reject, cancel) and judge-review triage, all without leaving the keyboard (PRD #112).

### Changed

- `/api/ws` now accepts a Bearer CLI token (`uzc_`/`uza_`) as well as a browser session cookie, so a headless client can subscribe to a run's live event stream; per-run authorization and the socket's origin check are unchanged (PRD #112 M1).

- **The `uzi` CLI strips more from untrusted text before printing it, which changes the output of existing commands** — not only the new `uzi tui`. `uzi run logs`, `uzi run get`, `uzi review show`, `uzi review backlog` and the disposition tables now also remove DEL (`0x7f`) and every Unicode format character (category `Cf`: the bidi overrides `U+202A`–`U+202E`, the isolates `U+2066`–`U+2069`, `U+200F`, zero-width spaces and joiners, the BOM, and the soft hyphen). Previously only C0 (except tab and newline) and C1 were removed, so a bidi override could visually reorder a judge's `target` or an agent's label into something it is not, and zero-width runes could silently consume a table column's width budget while drawing nothing. Printable text is unaffected, and `--json` output is byte-exact as before. If you script against the human tables, this removes characters that were previously passed through (PRD #112 M3).

- **Consequence of the above, stated because it is visible:** `U+200D` (zero-width joiner) is itself a format character, so **emoji ZWJ sequences decompose** in CLI output — a family emoji renders as its component people, a profession emoji as its parts. This affects all of the commands listed above, not only `uzi tui`. It is the accepted cost of rejecting the whole `Cf` category rather than an allowlist: an allowlist of "safe" format characters is a list somebody has to keep correct, and getting it wrong reopens the bidi-override spoof (PRD #112 M3).

### Fixed

- **Browser launches on hosted k8s workers get `--no-sandbox` again.** The worker's `CMD` is `npm run start`, and npm's run-script prepends `/app/node_modules/.bin` to `PATH` — so on the non-root (single-uid) start the real `agent-browser` CLI shadowed the PRD #87 shim, and every launch silently lost the flags the shim injects. Chromium then aborted on the setuid sandbox that the worker hardening makes impossible. The entrypoint now pins the runner PATH on both start modes, so the shim resolves first on k8s as it always did on compose. Runner children also stop resolving the worker's own `node_modules/.bin` (`tsx`, `tsc`, `esbuild`, …), which is the intended boundary (PRD #120, issue [#120](https://github.com/vtmocanu/uzi/pull/120)).

## [0.11.6] - 2026-07-25

### Added

- A run whose updates cannot be saved is now flagged **looping** with a reason that names the cause ("the agent's updates can't be saved, so it keeps resending them") instead of falling through to the 45-minute "taking longer than usual" wall clock. The signal is the API's own count of failed writes for that run, so unlike the existing loop detector it is not blind to a broken message stream (PRD #108).
- A confirmed per-run write loop is **stopped automatically**, in about a minute, under a conjunction of guards that must all hold — including "other runs on this instance are successfully saving messages", so an API-wide outage stops nothing. Such a run ends `failed` with the new stop kind `auto_stopped`, which `uzi run get` shows as a `STOP_KIND` row and the web styles as breakage rather than as a stop you asked for. Operators can turn the stop off with `UZI_AUTOSTOP_ENABLED=false`, which leaves the flag working (PRD #108). See [docs/run-auto-stopped.md](docs/run-auto-stopped.md).
- `GET /api/admin/cli-tokens` / `uzi admin cli-tokens` lists every CLI token in the factory with its owner — closing a gap `workers` hasn't had since PRD #42, where a token was visible to its own holder and nobody else. Read-only (no admin revoke) and never returns the token value or its hash, at four independent layers (query projection, generated row type, DTO, handler). The row does carry `last_used_ip`, so an `admin_ro` holder can see every user's source IPs — deliberate, since `admin_ro` is factory-wide read, but worth knowing before minting one (PRD #98). See [docs/cli.md](docs/cli.md#commands).
- The Slack health nudge now forks on cause: a persistence loop reads "⚠ This run's updates aren't being saved, so it keeps re-sending them", distinct from the pre-existing "⚠ This run looks like it's repeating the same step" (PRD #108).

### Changed

- `uzi worker list` gains a **VERSION** column. The operator page's first remedy for an auto-stopped run is "check the worker's version", and the web has rendered it since PRD #42 — so the page shipped a remedy one of its two audiences could not follow (PRD #108).

### Fixed

- The **docker worker tier** now gets the same values-gated egress allow to uzi's own in-cluster `web` Service that the restricted tier already had. PRD #87 M5 landed the rule in `worker-networkpolicy.yaml` only, so `web-ux` on a docker-capable worker had no target to drive — found by running PRD #87's M7 gate for real, where the browser launched correctly but every request to the UI timed out. Off by default; `dev-cluster` opts in (PRD #87 M5, MR !105).

### Known limitations

- Auto-stop needs at least one *other* run saving messages at the same time to act. On an instance running a single run at a time it will flag and notify but never stop — the correct behaviour on insufficient evidence, and the reason the flag, not the stop, is the value on that deployment shape (PRD #108).
- A run failing because the **worker itself** is sending malformed requests is flagged but never auto-stopped, because a worker defect affects every run that image touches and hiding them one at a time would hide the pattern. The remedy is to roll the worker image; the log line carries `failure_class=invalid` (PRD #108).

## [0.11.5] - 2026-07-25 [NOT RELEASED]

Version burned; nothing was published under it. The tag was pushed against the wrong commit (a bare-clone root's stale detached `HEAD`, carrying `Chart.yaml` 0.9.0). The tag pipeline's `publish:assert-version` job compares chart `version`/`appVersion` against the tag and blocks **all** pushes on a mismatch, so no image and no chart reached Harbor — the guard did exactly what it exists for. The tag is protected and could be neither deleted nor moved, so 0.11.5 is skipped rather than reused. Its intended payload shipped as 0.11.6.

## [0.11.4] - 2026-07-24

### Changed

- The agent worker's Claude Agent SDK moves 0.3.201 → 0.3.219, so `opus` resolves to **Opus 5** rather than the previous generation. The builtin agent templates pin `model: opus` (ten of the eleven roles; `documenter` is sonnet) and the alias is resolved by the `claude` binary the SDK bundles — so runs on a role that pins `opus` change model with this release, without any template edit.
- `openssl` is baked into the default worker toolchain instead of being a per-repo tier-2 package. It is a broadly useful base crypto/TLS CLI rather than a repo-specific extra, so every worker gets it without a repo opting in. (Specifically `openssl.bin` — the bare `openssl` attribute installs no CLI, which failed the image build guard with exit 127.)

## [0.11.3] - 2026-07-22

### Fixed

- The `Select` UI primitive no longer discards a caller's `className`, so the per-worker Anthropic token picker on the Workers page is styled like every other field instead of rendering as an unstyled native `<select>`. `Input` and `Textarea` already merged correctly; only `Select` was broken (issue [#118](https://github.com/vtmocanu/uzi/pull/118)).
- The your-usage card's "see per-run detail →" link no longer orphans its arrow onto a line of its own at narrow widths (issue [#117](https://github.com/vtmocanu/uzi/pull/117)).

## [0.11.2] - 2026-07-22

### Changed

- Meter colour thresholds move to **warn ≥40%**, **danger ≥85%** (from 80/95), so a rate-limit budget reads amber well before it is nearly gone. The status pill is decoupled from the bar: only a ≥95% window escalates it to "nearly out", and an 85–94% row keeps a green "Live" pill while the bar carries the red. A dedicated ≥95% screen-reader announcement keeps that escalation audible now that the danger tone steps at 85 (PRD #115). Busy workers now sit amber or red as their steady state — an accepted trade for the earlier warning.

## [0.11.1] - 2026-07-22

Re-ships the PRD #87 browser prebake + `web-ux` builtin (v0.11.0, rolled back to v0.10.1 after live testing on dev-cluster caught three cluster-only bugs). Fixes all three (issue [#114](https://github.com/vtmocanu/uzi/pull/114)).

### Fixed

- Docker-tier workers no longer CrashLoop at `seed-nix`: the browser build guard, running as root, created a `root:root 0700` directory in the prebaked Chromium nix closure that the non-root (uid 10001) seed tar could not read; `/nix` store permissions are now normalized after the guard in both worker Dockerfiles (BUG 1).
- The prebaked browser now launches under the hardened worker: the `agent-browser` shim's `XDG_CONFIG_HOME` is uid-scoped so the Crashpad database directory (previously baked `root:root` by the root build guard) is writable by uid 10001 at runtime. This resolves the `Chrome exited early without writing DevToolsActivePort` / Crashpad `recvmsg` reset failure, which is not caused by seccomp: a non-writable XDG is the sole determinant, confirmed on-cluster under both RuntimeDefault and Unconfined (BUG 2a and 2b, one root cause).
- The `uzi-hosted-workers-docker` ResourceQuota no longer over-counts storage: the controller now skips re-creating PVCs it already observes as present, ending the per-tick admitted-then-`AlreadyExists` creates that inflated `used.requests.storage` without decrement (`k8s #119593`) and pinned the quota at its limit, blocking new workers (BUG 3).

## [0.10.1] - 2026-07-22

### Fixed

- Worker message batches carrying a NUL byte, an unpaired UTF-16 surrogate, or invalid UTF-8 are sanitized server-side instead of wedging the run in a silent retry loop (PRD #108).
- A permanently-unstorable message batch now returns 400 (never retried) instead of 500 (retried forever); an oversize batch returns 413 and is split, never treated as poison (PRD #108).
- The worker's message batcher bounds batch size, backs off exponentially, and bisects a rejected batch down to the single poisoned message instead of growing the retry body past the API's 1 MiB cap (PRD #108).
- A poisoned message is tombstoned in place (visible in the run's history) rather than dropped, so the live run view no longer freezes at a permanent gap (PRD #108).
- `/api/worker/runs/:id/state` text fields (`failure_reason`, `session_id`, `plan_md`, `branch`, `mr_web_url`) are NUL-stripped, closing the same poison-pill class on the sibling route (PRD #108).
- `run_usage`'s `session_id`/`model` are length-capped before the upsert, closing a second poison-pill route (an oversized composite-key index entry, Postgres 54000) (PRD #108).
- Run HOME cleanup now restores write permission before removing a tree, so a Go module cache's read-only directories no longer strand disk on every Go-touching run; a one-off startup sweep (`UZI_HOME_RECLAIM`) reclaims HOMEs already stranded on existing volumes (PRD #108).

### Security

- Worker-side redaction now covers the `agent` and `kind` message fields, not just the payload and `agent_instance`/`agent_label`, closing a gap where a secret placed in either field reached the API, the WebSocket frame, the browser, and `uzi run logs` unscrubbed (PRD #108).

[Unreleased]: https://github.com/vtmocanu/uzi/compare/v0.78.0...HEAD
[0.78.0]: https://github.com/vtmocanu/uzi/compare/v0.77.0...v0.78.0
[0.77.0]: https://github.com/vtmocanu/uzi/compare/v0.76.0...v0.77.0
[0.76.0]: https://github.com/vtmocanu/uzi/compare/v0.75.1...v0.76.0
[0.75.1]: https://github.com/vtmocanu/uzi/compare/v0.75.0...v0.75.1
[0.75.0]: https://github.com/vtmocanu/uzi/compare/v0.74.0...v0.75.0
[0.74.0]: https://github.com/vtmocanu/uzi/compare/v0.73.0...v0.74.0
[0.73.0]: https://github.com/vtmocanu/uzi/compare/v0.72.1...v0.73.0
[0.72.1]: https://github.com/vtmocanu/uzi/compare/v0.72.0...v0.72.1
[0.72.0]: https://github.com/vtmocanu/uzi/compare/v0.71.2...v0.72.0
[0.71.2]: https://github.com/vtmocanu/uzi/compare/v0.71.1...v0.71.2
[0.71.1]: https://github.com/vtmocanu/uzi/compare/v0.71.0...v0.71.1
[0.71.0]: https://github.com/vtmocanu/uzi/compare/v0.70.0...v0.71.0
[0.70.0]: https://github.com/vtmocanu/uzi/compare/v0.69.0...v0.70.0
[0.69.0]: https://github.com/vtmocanu/uzi/compare/v0.68.0...v0.69.0
[0.68.0]: https://github.com/vtmocanu/uzi/compare/v0.67.0...v0.68.0
[0.67.0]: https://github.com/vtmocanu/uzi/compare/v0.66.3...v0.67.0
[0.66.3]: https://github.com/vtmocanu/uzi/compare/v0.66.2...v0.66.3
[0.66.2]: https://github.com/vtmocanu/uzi/compare/v0.66.1...v0.66.2
[0.66.1]: https://github.com/vtmocanu/uzi/compare/v0.66.0...v0.66.1
[0.66.0]: https://github.com/vtmocanu/uzi/compare/v0.65.1...v0.66.0
[0.65.1]: https://github.com/vtmocanu/uzi/compare/v0.65.0...v0.65.1
[0.65.0]: https://github.com/vtmocanu/uzi/compare/v0.64.0...v0.65.0
[0.64.0]: https://github.com/vtmocanu/uzi/compare/v0.63.0...v0.64.0
[0.63.0]: https://github.com/vtmocanu/uzi/compare/v0.62.0...v0.63.0
[0.62.0]: https://github.com/vtmocanu/uzi/compare/v0.61.0...v0.62.0
[0.61.0]: https://github.com/vtmocanu/uzi/compare/v0.60.0...v0.61.0
[0.60.0]: https://github.com/vtmocanu/uzi/compare/v0.59.0...v0.60.0
[0.59.0]: https://github.com/vtmocanu/uzi/compare/v0.58.0...v0.59.0
[0.58.0]: https://github.com/vtmocanu/uzi/compare/v0.57.0...v0.58.0
[0.57.0]: https://github.com/vtmocanu/uzi/compare/v0.56.0...v0.57.0
[0.56.0]: https://github.com/vtmocanu/uzi/compare/v0.55.1...v0.56.0
[0.55.1]: https://github.com/vtmocanu/uzi/compare/v0.55.0...v0.55.1
[0.55.0]: https://github.com/vtmocanu/uzi/compare/v0.54.0...v0.55.0
[0.54.0]: https://github.com/vtmocanu/uzi/compare/v0.53.1...v0.54.0
[0.53.1]: https://github.com/vtmocanu/uzi/compare/v0.53.0...v0.53.1
[0.53.0]: https://github.com/vtmocanu/uzi/compare/v0.52.0...v0.53.0
[0.52.0]: https://github.com/vtmocanu/uzi/compare/v0.51.0...v0.52.0
[0.51.0]: https://github.com/vtmocanu/uzi/compare/v0.50.0...v0.51.0
[0.50.0]: https://github.com/vtmocanu/uzi/compare/v0.49.0...v0.50.0
[0.49.0]: https://github.com/vtmocanu/uzi/compare/v0.48.0...v0.49.0
[0.48.0]: https://github.com/vtmocanu/uzi/compare/v0.47.0...v0.48.0
[0.47.0]: https://github.com/vtmocanu/uzi/compare/v0.46.1...v0.47.0
[0.46.1]: https://github.com/vtmocanu/uzi/compare/v0.46.0...v0.46.1
[0.46.0]: https://github.com/vtmocanu/uzi/compare/v0.45.0...v0.46.0
[0.45.0]: https://github.com/vtmocanu/uzi/compare/v0.44.0...v0.45.0
[0.44.0]: https://github.com/vtmocanu/uzi/compare/v0.43.0...v0.44.0
[0.43.0]: https://github.com/vtmocanu/uzi/compare/v0.42.2...v0.43.0
[0.42.2]: https://github.com/vtmocanu/uzi/compare/v0.42.1...v0.42.2
[0.42.1]: https://github.com/vtmocanu/uzi/compare/v0.42.0...v0.42.1
[0.42.0]: https://github.com/vtmocanu/uzi/compare/v0.41.0...v0.42.0
[0.41.0]: https://github.com/vtmocanu/uzi/compare/v0.40.0...v0.41.0
[0.40.0]: https://github.com/vtmocanu/uzi/compare/v0.39.0...v0.40.0
[0.39.0]: https://github.com/vtmocanu/uzi/compare/v0.38.1...v0.39.0
[0.38.1]: https://github.com/vtmocanu/uzi/compare/v0.38.0...v0.38.1
[0.38.0]: https://github.com/vtmocanu/uzi/compare/v0.37.0...v0.38.0
[0.37.0]: https://github.com/vtmocanu/uzi/compare/v0.36.0...v0.37.0
[0.36.0]: https://github.com/vtmocanu/uzi/compare/v0.35.0...v0.36.0
[0.35.0]: https://github.com/vtmocanu/uzi/compare/v0.34.0...v0.35.0
[0.34.0]: https://github.com/vtmocanu/uzi/compare/v0.33.0...v0.34.0
[0.33.0]: https://github.com/vtmocanu/uzi/compare/v0.32.0...v0.33.0
[0.32.0]: https://github.com/vtmocanu/uzi/compare/v0.31.0...v0.32.0
[0.31.0]: https://github.com/vtmocanu/uzi/compare/v0.30.0...v0.31.0
[0.30.0]: https://github.com/vtmocanu/uzi/compare/v0.29.0...v0.30.0
[0.29.0]: https://github.com/vtmocanu/uzi/compare/v0.28.0...v0.29.0
[0.28.0]: https://github.com/vtmocanu/uzi/compare/v0.27.0...v0.28.0
[0.27.0]: https://github.com/vtmocanu/uzi/compare/v0.26.0...v0.27.0
[0.26.0]: https://github.com/vtmocanu/uzi/compare/v0.25.0...v0.26.0
[0.25.0]: https://github.com/vtmocanu/uzi/compare/v0.24.0...v0.25.0
[0.24.0]: https://github.com/vtmocanu/uzi/compare/v0.23.0...v0.24.0
[0.23.0]: https://github.com/vtmocanu/uzi/compare/v0.22.0...v0.23.0
[0.22.0]: https://github.com/vtmocanu/uzi/compare/v0.21.0...v0.22.0
[0.21.0]: https://github.com/vtmocanu/uzi/compare/v0.20.2...v0.21.0
[0.20.2]: https://github.com/vtmocanu/uzi/compare/v0.20.1...v0.20.2
[0.20.1]: https://github.com/vtmocanu/uzi/compare/v0.20.0...v0.20.1
[0.20.0]: https://github.com/vtmocanu/uzi/compare/v0.19.1...v0.20.0
[0.19.1]: https://github.com/vtmocanu/uzi/compare/v0.18.0...v0.19.1
[0.18.0]: https://github.com/vtmocanu/uzi/compare/v0.17.0...v0.18.0
[0.17.0]: https://github.com/vtmocanu/uzi/compare/v0.16.0...v0.17.0
[0.16.0]: https://github.com/vtmocanu/uzi/compare/v0.15.0...v0.16.0
[0.15.0]: https://github.com/vtmocanu/uzi/compare/v0.14.0...v0.15.0
[0.14.0]: https://github.com/vtmocanu/uzi/compare/v0.13.0...v0.14.0
[0.13.0]: https://github.com/vtmocanu/uzi/compare/v0.12.0...v0.13.0
[0.12.0]: https://github.com/vtmocanu/uzi/compare/v0.11.12...v0.12.0
[0.11.12]: https://github.com/vtmocanu/uzi/compare/v0.11.11...v0.11.12
[0.11.11]: https://github.com/vtmocanu/uzi/compare/v0.11.10...v0.11.11
[0.11.10]: https://github.com/vtmocanu/uzi/compare/v0.11.9...v0.11.10
[0.11.9]: https://github.com/vtmocanu/uzi/compare/v0.11.8...v0.11.9
[0.11.8]: https://github.com/vtmocanu/uzi/compare/v0.11.7...v0.11.8
[0.11.7]: https://github.com/vtmocanu/uzi/compare/v0.11.6...v0.11.7
[0.11.6]: https://github.com/vtmocanu/uzi/compare/v0.11.5...v0.11.6
[0.11.5]: https://github.com/vtmocanu/uzi/compare/v0.11.4...v0.11.5
[0.11.4]: https://github.com/vtmocanu/uzi/compare/v0.11.3...v0.11.4
[0.11.3]: https://github.com/vtmocanu/uzi/compare/v0.11.2...v0.11.3
[0.11.2]: https://github.com/vtmocanu/uzi/compare/v0.11.1...v0.11.2
[0.11.1]: https://github.com/vtmocanu/uzi/compare/v0.10.1...v0.11.1
[0.10.1]: https://github.com/vtmocanu/uzi/releases/tag/v0.10.1
