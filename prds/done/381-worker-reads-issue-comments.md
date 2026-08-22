# PRD #381: Feed issue comments to the worker, not just the title + description

**GitHub Issue**: [#381](https://github.com/vtmocanu/uzi/issues/381)
**Status**: Draft (created 2026-08-19; revised the same day after an architect + fact-checker review — see Decision Log)
**Priority**: Medium
**Related**:
- [#323](https://github.com/vtmocanu/uzi/issues/323) — the run-health `slow` fix whose adversarial-review **comment** carried two load-bearing implementer refinements a worker could not see. That is the incident that motivated this PRD: the guidance had to be hand-injected with `uzi run follow-up`. This feature removes that manual step.
- [#246](https://github.com/vtmocanu/uzi/issues/246) — trusted-repo instructions (`adr/0246-trusted-repo-instructions.md`). Precedent for the trust framing: repo/issue text is untrusted input, wrapped and never executed. Comments join that same class.
- [#47](https://github.com/vtmocanu/uzi/issues/47) — the run-health nudge posts uzi's **own** issue comments (`CreateIssueNote`). This is why D1 (exclude bot-authored comments) is load-bearing: reading them back would feed uzi its own status chatter.

## Problem

A worker never sees an issue's **comments**. The run instruction is assembled from the issue **title + description**, and (for issue runs) an appended note telling the agent to read the linked `prds/*.md` from the checked-out worktree. Context a human or reviewer adds in a comment after the description was written is invisible to the agent.

Verified in code:

- The `Forge` interface (`api/internal/forge/forge.go:361-469`, 21 methods) has `GetIssue` (`:381`) returning an `Issue` whose fields are `IID/Title/State/Labels/Description/Author/WebURL/UpdatedAt` (`forge.go:216-225`). There is **no** comment field and **no** `ListIssueComments` read method. `CreateIssueNote` (`:411`) is write-only, returning `IssueNote{ID, Body}` (`:240-243`).
- The run snapshots the issue description at creation: `CreateRun(ctx, userID, repo.ID, issueIID, issue.Description, …)` (`api/internal/workersvc/composite.go:218`), and the claim payload carries only `IssueDescription` (`api/internal/workersvc/claim.go:31`).
- The agent prompt wraps `<issue_title>` and `<issue_description>` under an "UNTRUSTED INPUT" frame (`agent/src/prompt.ts:23-28`, tags assembled at `:627-635`). A separate lifecycle note (`prompt.ts:111-123`, issue runs only) tells the agent to treat a linked `prds/*.md` as spec; the agent reads that file from the worktree via file tools — the prompt does not embed its text. Comments are part of neither path.
- The on-demand `get_issue` forge tool (`agent/src/forge-tools.ts:95`; server `WorkerForgeGetIssue` at `api/internal/handler/worker_forge.go:120-146`) returns the same `Issue` shape (`GetIssue` + `truncateForgeBody`), so an agent that explicitly reads an issue mid-run also sees no comments.

**Concrete cost, observed on #323 (2026-08-19):** the adversarial-review comment on that issue carried two implementer refinements (guard the budget-scaling on `BudgetWallSeconds.Valid`; **revise** the existing `TestHealthSlowFlagsWithRecentActivity` rather than only appending a test). The worker planning #323 could not see either; we hand-steered them in with a `run follow-up`. That manual step is exactly what this feature removes.

## Solution

Add issue comments as a first-class, bounded, **nonce-fenced**, untrusted input the worker sees, in two places:

1. **The initial instruction** (primary): snapshot the issue's human comments when the run is created, alongside the description snapshot, store them **structured**, and render them in the agent prompt under a per-prompt nonce fence (see D5).
2. **The live `get_issue` tool** (secondary): include comments in the on-demand forge read so an agent can pull the latest thread mid-run.

Both need one new forge read method, implemented across the three drivers, plus the plumbing to carry and render the result.

### Resolved facts (SDK read methods — offline-verified from the module cache, no open web needed)

The worker has no open-web egress, but the forge SDKs are in the Go module cache (a package cache the worker can reach), so these are stated as resolved facts. Both reviewers verified them against the cache:

- **GitLab** (`gitlab.com/gitlab-org/api/client-go/v2@v2.44.0`): `NotesService.ListIssueNotes(pid, issue, opt, …) ([]*Note, *Response, error)` (`notes.go:162`). `Note` carries `Body string` (`:67`), `Author NoteAuthor` (`:71`), `CreatedAt *time.Time` (`:73`), **and `System bool` (`:72`)** — GitLab returns *system* notes in the same list, so the driver **must filter `!System`**. **Default sort is `created_at` DESC (newest first)** — the driver normalizes to oldest-first (D8).
- **Forgejo/Gitea** (`code.gitea.io/sdk/gitea v0.25.1`): `Client.ListIssueComments(owner, repo, index, opt) ([]*Comment, *Response, error)` (`issue_comment.go:53`). `Comment` carries `Poster *User` (`:24`), `Body string` (`:27`), `Created time.Time` (`:28`); it has **no System/Type field**, so no in-SDK system filter exists. Gitea's comment list is human comments only (system/timeline events are a separate endpoint — documented Gitea REST behavior, not derivable from the cache). Default sort ASC — already oldest-first.
- **GitHub** (`github.com/google/go-github/v90`): `IssuesService.ListComments(ctx, owner, repo, number, opts) ([]*IssueComment, *Response, error)` (`issues_comments.go:63`). `IssueComment` carries `User *User` (`:19`), `Body *string` (`:18`), `CreatedAt *Timestamp` (`:21`). Issue comments are human comments only (events/timeline are separate endpoints — documented GitHub REST behavior). Default sort ASC — already oldest-first.

The self-comment filter uses the connected bot's **stable forge user id**, already stored at connect: `forge_connections.bot_forge_user_id` (`api/internal/store/queries/forge.sql:8`; also `BotIdentity.ForgeUserID` from `VerifyToken`, `forge.go:96`). No live `VerifyToken` call is needed — read the stored id.

## Decisions

**D1 — Exclude uzi's own bot-authored comments.** uzi posts issue comments via `CreateIssueNote` from autopilot (`poller/autopilot.go:273`), `ci_autofix` start/halt (`poller/ci_autofix.go:268`/`:224`), and run lifecycle (`runlifecycle/lifecycle.go:417`). Feeding those back would add noise and risk a loop where the agent reacts to uzi's own status notes. Drop any comment whose author forge-user-id equals the connection's stored `bot_forge_user_id`. This is the load-bearing decision that makes the feature safe, and it is *more* load-bearing on the autopilot/scheduled path (D6), which posts bot comments itself.

**D2 — Exclude forge system notes.** GitLab's `ListIssueNotes` includes system notes (`Note.System`); the GitLab driver filters them so the neutral type carries only human comments. Gitea and GitHub do not include system notes in their comment lists, so the neutral `IssueComment` never carries one regardless of driver. No `System` field on the neutral type — the boundary is enforced in each driver.

**D3 — Snapshot at run creation, not at claim.** The description is already snapshotted at run creation; comments follow the same model. A run sees the comments that existed when it was queued (which is when the human guidance was added). Deterministic, needs no forge call at claim time, matches the existing snapshot contract. Freshness after queueing is served by the live `get_issue` tool (M4), not by re-fetching the initial instruction.

**D4 — Bound volume in ASSEMBLY, not in the driver.** The driver returns the **complete** comment set (bounded only by the existing forge sanity ceilings that error-on-exceed — `maxForgeItems`/`maxForgePages` in `pagination.go`, the same pattern `ListIssueLabelEvents` uses). The size bound is an assembly concern: take the most-recent comments up to a byte cap in the spirit of `MaxForgeBodyBytes` (32768, `worker_forge.go:42`), keep chronological (oldest-first) order in the output, and set an explicit `truncated` flag (mirroring `DescriptionTruncated`) so the agent knows the thread was clipped.

**D5 — Nonce-fenced untrusted rendering (security-critical).** Comments are attacker-influenceable free text, the same trust class the description already is (see #246) — but they are the *worst* case: multi-author, each independently attacker-authored, and a body can embed a literal `</issue_comments>` plus a forged `author: admin (approved)` line to break out of a static tag or spoof uzi-generated labels. Every other attacker-authored block in `prompt.ts` (cross-run memory, in-flight targets, recommendations, deps dirs) is already wrapped in a per-prompt CSPRNG **nonce fence** (`fenceNonce()`, e.g. `depsProvisionImplementNote`) precisely to defeat that breakout class. Comments are rendered the same way: a nonce-fenced block whose layout and per-entry `author`/`timestamp` labels are uzi's, and whose bodies are DATA. A static `<issue_comments>` tag would be below the file's own current bar and is explicitly rejected.

**D6 — Issue-backed runs only; that includes autopilot and scheduled.** The inclusion set is every real issue-backed run: manual (`StartRunForUser`), autopilot (`poller`), and scheduled (`schedsvc`). Excluded: `chat` (reuses `IssueDescription` for the chat message, `chat.go:254`), `ci_fix` (synthetic description, `ci_fix.go:187`/`:120`), `self_improve`, and `judge` — none has a human issue thread to read.

**D7 — Structured JSONB storage (required by D5).** A per-prompt nonce cannot be baked into a server-stored *text* blob (the nonce is minted in `prompt.ts` after the data arrives), so the snapshot is stored **structured**: a new nullable `runs.issue_comments` JSONB column (goose migration numbered at merge time; sqlc regen) holding `[{author_username, author_forge_user_id, created_at, body}]` plus a `truncated` flag, carried structured on the claim next to `IssueDescription`. Per-entry rendering + the nonce fence happen in `prompt.ts` (M3). Nullable so every existing run and every non-issue kind reads NULL.

**D8 — Normalize ordering to oldest-first in each driver.** GitLab defaults newest-first, Gitea/GitHub oldest-first (see Resolved facts); each driver normalizes to oldest-first, matching the `ListIssueLabelEvents` convention, so assembly and the agent see one order regardless of forge.

**D9 — Fail safe on an unknown bot id.** A legacy connection with a missing/zero `bot_forge_user_id` cannot be filtered by D1, so the snapshot **omits comments entirely for that connection** (skip the feature) rather than risk leaking uzi's own comments into the prompt. Better to miss the feature than create the feedback loop D1 exists to prevent.

**D10 — No CLI change.** Per the repo convention ("New uzi functionality ⇒ check whether `api/cmd/uzi/` needs a matching CLI change"): comments are worker-facing run *context*, not run *status* surfaced by `uzi run get`/`logs`, so no `api/cmd/uzi/` change is warranted. Recorded so the convention is satisfied explicitly.

## Milestones

Dependency graph (parallel-milestone convention): **M1 → { M2a → M2b → M3 , M4 } → M5**. M4 depends only on M1 and can run in parallel with the M2/M3 chain.

- [x] **M1 — Forge read method + three drivers + six fakes.** Add `ListIssueComments(ctx, projectID, issueIID int64) ([]IssueComment, error)` to the `Forge` interface and a neutral `IssueComment{AuthorForgeUserID int64, AuthorUsername string, Body string, CreatedAt time.Time}`. Implement in `gitlab.go` (`Notes.ListIssueNotes`, filter `!System`, normalize oldest-first — D8), `forgejo.go` (`ListIssueComments`), `github.go` (`Issues.ListComments`). The driver returns the **complete** set bounded only by the existing forge sanity ceilings (D4) — no `Limit`, no per-call cap. Update the **six** interface fakes so both Go modules build: `handler/forge_test.go` (`fakeUserForge`), `seed/seed_test.go` (`fakeForge`), `poller/autopilot_test.go` (`apForge`), `poller/ci_autofix_test.go` (`cfForge`), `privcheck/checker_test.go` (`fakeForge`), `forgesvc/sync_test.go` (`fakeForge`). (`workersvc/ci_fix_snapshot_test.go`'s `fixSnapForge` embeds `forge.Forge`, so it inherits the new method and needs no change.) Per-driver unit tests assert the GitLab system-note filter, the oldest-first normalization, and the neutral shape. **Success:** `task gate:api` green; a GitLab driver test proves a system note is dropped and a human note kept, in oldest-first order.
- [x] **M2a — Schema + structured claim wire + fetch plumbing.** Add the nullable `runs.issue_comments` JSONB column (goose + sqlc) and carry it structured on the claim (`claim.go`). Centralize the comment fetch **inside `workersvc.createRun`** (build a driver from the run's repo connection and fetch there — one site, one extra round-trip, no change to the `StartRun`/autopilot/scheduled seam interfaces or their fakes), so the fetch reaches every issue-backed origin (D6) without rippling through the `Create*Run` family. Read the connection's `bot_forge_user_id` for the filter, with the D9 fail-safe. **Success:** a live-DB test creates a run on an issue with a human comment and a bot comment and asserts the stored JSONB contains the human one and not the bot one; an issue with no (non-bot) comments stores NULL; a connection with a zero bot id stores NULL (D9).
- [x] **M2b — Filter + cap + ordering assembly.** The assembly logic feeding M2a's store: drop bot-authored (D1) and (already driver-filtered) system notes, keep oldest-first (D8), and apply the D4 byte cap + `truncated` flag over the most-recent tail. **Success:** unit tests for the bot filter, the byte cap + truncation flag, and ordering; an over-cap thread keeps the newest content and sets `truncated`.
- [x] **M3 — Nonce-fenced prompt assembly (agent).** Render the structured comments in `agent/src/prompt.ts` as a per-prompt nonce-fenced block (the `fenceNonce()` pattern, D5), placed after `<issue_description>`: one entry per comment with uzi-owned `author` + `timestamp` labels and the body as data, plus a truncation marker when D4 clipped the thread. Empty/NULL renders nothing (byte-for-byte unchanged for today's comment-less runs). **Success:** a prompt-assembly test asserts a commented run renders the nonce-fenced block and that a body containing a literal fence-close string does not break out; a comment-less run renders exactly as today.
- [x] **M4 — Live `get_issue` tool includes comments (parallel to M2/M3).** Extend `WorkerForgeGetIssue` (`handler/worker_forge.go`) and the `get_issue` forge tool (`agent/src/forge-tools.ts`) to include comments (same D1/D2/D8 filtering), with the single-issue route applying its **own** count + byte cap (it currently caps only the description). Confirm the `ForgeIssueDTO` change (`apitypes/forge.go`) is additive and enumerate its consumers. Depends only on M1. **Success:** the tool's response carries filtered, bounded comments under the tool's untrusted-evidence framing; existing `get_issue` callers still compile.
- [x] **M5 — Tests, docs, security note, and a CLAUDE.md correction.** Cross-cutting coverage beyond the per-milestone tests; update `ARCHITECTURE.md` "Forge integration" and the relevant `docs/` page to state that human issue comments are now part of the worker's untrusted input, bot/system-filtered, bounded, and nonce-fenced; add a short security-review note recording the widened injection surface (D5) and why it is not a new trust boundary. **Also correct the root `CLAUDE.md`** forge-interface note, which this PRD's review found wrong: it says "five test fakes" (there are **six** — add `poller/ci_autofix_test.go`) and mislabels the embedder as `handler/ci_fix_snapshot_test.go` embedding `fakeUserForge` (it is `workersvc/ci_fix_snapshot_test.go` embedding `forge.Forge`). **Success:** `task gate:api` + `task gate:agent` + the docs check (`web/scripts/check-docs.mjs`) green; ARCHITECTURE.md, the security note, and the CLAUDE.md fix land in this PRD's MRs.

## Success Criteria

1. A worker planning an issue that has human comments sees them (author + timestamp + body), bounded and inside a per-prompt nonce fence, ordered oldest-first after the description.
2. uzi's **own** bot-posted comments and forge **system** notes never appear in the worker prompt or the `get_issue` tool output (proven per-driver and end-to-end), and a connection with an unknown bot id omits comments entirely (D9).
3. A comment body containing a literal fence/close string cannot break out of its block or spoof the uzi-owned author/timestamp labels (D5, proven by test).
4. Comment volume is bounded; an over-cap thread keeps the newest content and sets `truncated`, never silently dropped.
5. A comment-less issue produces byte-for-byte the same prompt as today (no regression for the common case).
6. The three drivers return the same neutral `IssueComment` shape; the **six** interface fakes compile; both Go modules and the agent module pass their gates.

## Out of scope / non-goals

- **Reading MR/PR comments or CI-comment threads** — this PRD is issue comments feeding an issue-backed run.
- **Reacting to comments added after a run is claimed** on the *initial instruction* — that is D3's deliberate boundary; the live `get_issue` tool (M4) is the freshness path.
- **Writing comments** — unchanged; `CreateIssueNote` already exists.
- **Any new trust boundary** — comments are the same untrusted class as the description (D5), rendered with the same nonce-fence discipline already used for every other attacker-authored block.

## Risks & mitigations

- **Feedback loop from uzi's own comments** (the main risk) — mitigated by D1 (filter on the stored stable `bot_forge_user_id`) + D9 (fail-safe when that id is unknown), tested per-driver and in the M2a live-DB snapshot test.
- **Prompt-injection breakout via comment text** — mitigated by D5 (per-prompt nonce fence, matching the file's existing standard), proven by an M3 breakout test.
- **Prompt bloat / cost** on a long thread — mitigated by D4's byte cap + most-recent selection.
- **Interface-change spread** (three drivers + six fakes) — a known, mechanical shape; M1 does it in one milestone with per-driver tests. The fetch is centralized in `createRun` (M2a) so the change does NOT ripple through the `Create*Run`/poller/schedsvc seam interfaces.

## Decision Log

- **2026-08-19 — created**, then **revised the same day** after an architect (design/scope) + fact-checker (citations) review. Changes from the first draft: comment rendering moved from a static `<issue_comments>` tag to a **nonce fence** (D5) with **structured JSONB storage** (D7) to support it; M2 split into M2a/M2b with the fetch **centralized in `createRun`** to avoid a 3-package seam ripple (B3); the fake count corrected from **five to six** (`poller/ci_autofix_test.go`'s `cfForge` is a full implementer passed as `forge.Forge`); driver fetch changed from "paginate to a cap" to "return the complete set, cap in assembly" (D4); ordering normalization (D8), bot-id-fail-safe (D9), and no-CLI-change (D10) added as explicit decisions; the autopilot/scheduled inclusion set stated (D6). All three SDK "resolved facts" and the plumbing citations were verified against the module cache and HEAD.

## Notes for an offline (uzi) worker

Everything this PRD needs is in-repo or in the Go module cache: the three forge SDKs (versions and exact method signatures stated above as resolved facts), the `Forge` interface, the claim/prompt plumbing, the `fenceNonce()` pattern, and the existing snapshot/truncation code. No milestone depends on the open web. The goose migration number is assigned at merge time (repo convention); do not hardcode one.
