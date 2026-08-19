# PRD #377: Fail early when a run touches unpushable workflow files

**Issue**: #377
**Priority**: Medium
**Status**: Draft

## Problem

uzi's GitHub bot token is scoped to exactly `repo` by design: `privcheck` treats
`{repo, workflow}` as a save-blocking over-privilege violation
(`api/internal/privcheck/checker.go:304-317`, set-equality at `:327-342`). A
`repo`-only token cannot create or update any file under `.github/workflows/`, so
GitHub rejects such a push with:

```
! [remote rejected] agent/issue-N -> agent/issue-N (refusing to allow a Personal
Access Token to create or update workflow `.github/workflows/<name>.yml` without
`workflow` scope)
```

Today a run that produces a workflow-file change does **all** its work and then
dies at the very last step, the finalize push in `agent/src/runner.ts:1237`
(`withForgeRetry(() => this.git.pushBranch(...))`, network op at `git.ts:335`).
The thrown error falls through to the generic catch (`runner.ts:1319`), so the run
ends `failed` with `fail_origin = agent_failure` and the raw GitHub stderr copied
verbatim into the free-text `failure_reason` (`runner.ts:1364-1394`,
`MAX_FAILURE_REASON_LEN = 512`). The generic catch does **not** call
`fetchBackBestEffort` (only the parked/shutdown arms at `runner.ts:1345`/`:1357`
do), so the agent's committed work in the runner clone is torn down in the `finally`
(`runner.ts:1472-1478`) and lost. Three costs:

1. **The agent's output is thrown away.** In issue #188 the agent produced a
   correct, reviewed `main-guard.yml`; the run failed and the file was lost. A
   human had to reconstruct it from the run transcript.
2. **The reason is opaque.** A raw `remote rejected ... without workflow scope`
   line reads as an infra error, not a by-design boundary. Nothing points the
   reader at the fix (land the file as a human).
3. **It is structurally guaranteed, not a fluke.** Because privcheck forbids
   `workflow` scope at save time, a GitHub connection using a classic `repo`-only
   token will hit this on **every** run that touches `.github/workflows/**`. It is
   a permanent, known failure mode with no early signal.

This is not a misconfiguration to fix by widening the token: adding `workflow`
scope makes the connection over-privileged and the privilege sweep refuses runs on
it (PRD #66 / ADR-0238). The boundary is intentional (CI-integrity: an agent must
not touch the workflows that guard `main`). What is missing is uzi **handling** the
boundary gracefully instead of face-planting into it.

## Goal

When a run's branch touches a path the bot token cannot push, uzi should:

- **Detect it at finalize, before the doomed push** (and, as a stretch, advise the
  agent at edit time).
- **End the run in a typed, actionable `failed` outcome** that names the offending
  paths and says "land these as a human PR; see `docs/github-bot-setup.md`" instead
  of a raw git error.
- **Preserve the agent's diff** so the human can apply it without re-deriving it
  from the transcript.

Scope is GitHub `.github/workflows/**` (the one structurally-guaranteed case). The
design should not preclude generalising later to "any forge/path the token can't
push", but that generalisation is explicitly out of scope here (see Non-goals).

## Why this is safe to hand to an offline worker

Every fact this PRD relies on is in this repo's own source (cited inline) and was
resolved locally during authoring. No milestone needs the open internet: the
detection predicate, the finalize call sites, the failure model, the `fail_origin`
enum + migration mechanics, and the web surface are all in-tree. The one external
unknown considered during design (whether GitHub blocks a workflow-bearing push to
the `refs/uzi-checkpoints/*` namespace) was **designed around**, not left as a
worker task: the preservation mechanism was chosen to never touch the forge (see
D1). A worker with restricted egress can implement every milestone by reading the
cited files.

## Key facts (resolved during authoring, cite before changing)

- **Diff is already computed at finalize.** `git.ts:777 changedFiles(barePath,
  trackingRef)` runs `git diff --name-only <defaultBranch>...<trackingRef>` and is
  already called four times in the finalize block (`runner.ts:1071`, `:1116`,
  `:1182`, `:1203`). The agent branch is fetched into the bare at `runner.ts:1059`
  (`trackingRef`) before the push, so the diff exists pre-push. **It returns `null`
  on diff-computation failure** (fail-open sentinel; the `ci_fix` guard treats null
  as fail-closed — this PRD chooses the opposite, see D6).
- **A glob matcher already exists.** `agent/src/ci-config-guard.ts` ships
  `ciConfigPathToRegex` (`:31-46`, `**`/`*`, dotfile-safe) and
  `flagCIConfigPaths(changedFiles, paths)` (`:50-61`), and `.github/workflows/**`
  is already a member of `DEFAULT_CI_CONFIG_PATHS` (`:19-24`). It is currently
  wired only to the `ci_fix` auto-approval guard (`runner.ts:1192-1218`), which
  `throw`s on a match; it is NOT applied to normal `issue`/`self_improve`/`prompt`
  runs.
- **The predicate needs no API call.** `forge_type` is on the claim
  (`ClaimRepo.ForgeType`, `workersvc/claim.go:224`; read at `runner.ts:1259-1264`).
  Every connected GitHub token is a classic `repo`-only PAT (fine-grained tokens are
  refused at connect, `github.go:169`; see D4), so `forge_type === "github"` alone is
  sufficient for "a `.github/workflows/**` change in this branch will be rejected".
- **`fail_origin` is a closed enum; `failure_reason` is free text.** Enum members
  in `api/internal/workersvc/failorigin.go:35-45`, CHECK in
  `api/internal/store/migrations/00126_run_fail_origin.sql`. **`00126` is already
  landed and applied** (live migration head today is `00130`; strict goose,
  `store/migrate.go`, never re-runs an applied migration). Two lockstep tests
  guard the vocabulary: `TestFailOriginVocabularyMatchesCheck`
  (`failorigin_test.go:66`, which **hard-codes the path
  `../store/migrations/00126_run_fail_origin.sql`**) and `TestCoerceFailOrigin`
  (`failorigin_test.go:33,46`, which asserts `worker-reportable + server-only ==
  vocabulary`). Worker-reportable subset today is only `{provisioning_failed,
  credential_unavailable, rate_limited, agent_failure}` (`failorigin.go:79-84`).
  The in-tree precedent for widening a CHECK-list enum is
  `00127_recommendation_cost_efficiency_category.sql`, which does a `DROP
  CONSTRAINT ... ADD CONSTRAINT ... CHECK (... IN (widened))` in a **new**
  migration (with a narrowing Down that deletes offending rows).
- **`fail_origin` is stamped only on the failed path; the failed and completed
  render paths are disjoint.** `SetRunFailed` stamps `fail_origin`
  (`service.go:2975-2989`); `SetRunCompleted`/the `report_only` path has no
  `FailOrigin` field (`service.go:2957-2965`). In the web, `failure_reason` renders
  only on a non-completed terminal run (`RunView.tsx:811` gates the card on
  `terminal && run.status !== "completed"`, body at `:823`/`:1393`), while
  `report_md` renders only on `status === "completed" && report_only && report_md`
  (`RunView.tsx:797-808`). So a `completed`+`report_only` run can carry **neither**
  `fail_origin` nor `failure_reason` — which is why this PRD's outcome is a `failed`
  run, not a report-only completion (see D1).
- **`report_only` today means "committed nothing" and is issue-only.** It fails a
  completion if a checkpoint ref was published (`runner.ts:1010-1033`),
  `report_md` is stored only when `report_only` is accepted
  (`workersvc/report_only.go:41-57`), and it is gated to `run.kind == "issue"`
  (`:20-29`). This PRD does **not** reuse the `report_only` path (it committed a
  file, and the outcome must serve non-issue kinds too), so none of those
  constraints apply.
- **Checkpoints are brokered to the forge, so they are not a forge-independent
  channel.** The worker sends checkpoint packs to the api with the join token, no
  PAT (`runner.ts:884-885`), but the api then pushes them to the forge via
  `pushbroker` as `refs/uzi-checkpoints/<branch>` using the bot credential
  (`pushbroker.go:137`, `:507`). Whether GitHub blocks a workflow-bearing push to
  that non-`refs/heads/` namespace is unverified external behaviour. The
  preservation mechanism (D1) therefore avoids the forge entirely.

## Approach (recommended)

Detect at **finalize/pre-push**, end the run in a **`failed`** outcome tagged with a
new `fail_origin`, and **preserve the diff via a forge-independent api-stored
field** (never a forge push). Keep an edit-time guardrail advisory as a stretch.
Concretely:

1. At finalize, **after** the existing `ci_fix` guard and immediately before the
   push (`runner.ts` ~`:1220`), if `forge_type === "github"` and
   `flagCIConfigPaths(changedFiles(...), [".github/workflows/**"])` is non-empty,
   branch off the normal push/MR path. A `null` diff fails **open** to the normal
   push (D6).
2. Skip the push. Serialize the agent's branch diff (`git diff
   <base>...<trackingRef>`), secret-scrub and size-cap it, and send it to the api on
   the terminal `failed` report in a stored field that the run view renders.
3. Report the run `failed` with `fail_origin = workflow_scope_missing` and a
   composed `failure_reason` that names the offending path(s) (truncating the path
   list, never the doc link, under the 512-char cap) and points at
   `docs/github-bot-setup.md`.
4. (Stretch, M3) Emit an edit-time advisory into the run feed when the agent writes
   a `.github/workflows/**` file, so it can wrap up as a report rather than doing
   more work it cannot land. Non-blocking: it must not prevent the write (the diff
   must survive).

## Milestones

M1 is a cross-module MVP that addresses all three costs on its own. M2-M4 refine
and validate it.

- [ ] **M1 — Detect, fail with a typed reason, and preserve the diff (the MVP).**
  - *api (Go):* a **new** migration (number assigned at merge, above the live head)
    that `DROP CONSTRAINT ... ADD CONSTRAINT ... CHECK` widens the `fail_origin`
    set to include `workflow_scope_missing` (mirroring
    `00127_recommendation_cost_efficiency_category.sql`, with a narrowing Down); add
    the member to `failOrigins` and `workerReportableFailOrigins`
    (`failorigin.go`); **repoint `TestFailOriginVocabularyMatchesCheck`** at the new
    migration and **rebalance `TestCoerceFailOrigin`**'s partitions; add a
    nullable stored field carried on the **failed** report for the preserved patch
    (a dedicated column keeps `report_only` semantics untouched — see D3).
  - *agent (TS):* finalize detection (GitHub + `.github/workflows/**`) placed after
    the `ci_fix` guard, before the push; null-diff fail-open; on a hit, skip the
    push, secret-scrub + size-cap the diff, and report `failed` with
    `workflow_scope_missing` + the composed `failure_reason` + the patch.
  - Serves every forge-pushing kind (`issue`, human-approved `ci_fix`,
    `self_improve`, `prompt`), because the failed path is not issue-gated.
- [ ] **M2 — Web surface.** Render the preserved patch and the typed reason on the
  **failed** card in `RunView.tsx` (the failure card already renders
  `failure_reason`; the patch needs a rendered block gated on the new field).
  Resolve D2 (keep the human message in `failure_reason` text vs. add `fail_origin`
  to `RunDTO` for a structured badge). It must read as "the agent did valid work
  uzi can't auto-land; here it is", not as a crash.
- [ ] **M3 — Edit-time advisory (stretch).** When the agent writes a
  `.github/workflows/**` file, surface an advisory (guardrail path classifier or a
  run-feed message) so the agent knows the change cannot be auto-landed. Must not
  block the write (the diff must survive for M1's preservation).
- [ ] **M4 — Cross-cutting validation and docs.** Deterministic tests over the
  matrix that unit tests in M1 can't cover in one place: GitHub + workflow path ⇒
  blocked-and-preserved; GitLab/Forgejo or non-workflow path ⇒ normal push;
  null-diff ⇒ normal push; each forge-pushing kind; `fail_origin` round-trip; both
  lockstep tests green. Use a **synthetic fixture** (`changedFiles = [".github/
  workflows/…"]` via the existing test seams), not a fetch of issue #188. Extend the
  `docs/github-bot-setup.md` troubleshooting note (added in #188) to say uzi now
  detects this, fails cleanly, and surfaces the diff — keeping frontmatter valid and
  adding no broken relative link or duplicate `order` (`web/scripts/check-docs.mjs`).

## Success criteria

- A GitHub run whose branch touches `.github/workflows/**` never reaches the
  doomed push; it ends `failed` with `fail_origin = workflow_scope_missing` and a
  human-readable `failure_reason`, not the raw `remote rejected` string.
- The failed run carries the agent's diff (a scrubbed, capped patch) rendered in
  the run view, verified against a synthetic reproduction of the #188 change.
- A non-workflow change, a null diff, and any GitLab/Forgejo run are unaffected
  (normal push + MR).
- Every forge-pushing run kind (`issue`, human-approved `ci_fix`, `self_improve`,
  `prompt`) gets the typed outcome, not just `issue`.
- Both `fail_origin` lockstep tests stay green (`TestFailOriginVocabularyMatchesCheck`
  repointed at the new migration; `TestCoerceFailOrigin` partitions balanced).
- Tests are deterministic (no timing/sleep/wall-clock assertions).

## Non-goals

- Widening the bot token or weakening the exactly-`repo` boundary. The boundary is
  the point; this PRD handles it, it does not remove it.
- Generalising to "any forge/path the token cannot push". GitHub
  `.github/workflows/**` is the one structurally-guaranteed case; per-forge scope
  knowledge on the claim is out of scope here.
- Auto-landing workflow files by any side channel (a second token, a bot with
  `workflow` scope). Landing workflow files stays a human action by design.
- Surfacing the diff via a forge push (checkpoint ref or a partial branch). Rejected
  because the forge push is exactly the operation the bot token can't do for these
  files; preservation is api-stored only.

## Risks and mitigations

- **`fail_origin` migration must be NEW, not an edit of `00126`.** `00126` is
  applied; editing/renumbering it is a no-op on migrated instances, so a worker
  reporting `workflow_scope_missing` would hit `runs_fail_origin_check` (SQLSTATE
  23514) — a second failure. Mitigation: a new migration DROP/ADD-CONSTRAINTs the
  widened set (mirror `00127`), renumbered to the live head at merge (CLAUDE.md).
- **Both lockstep tests must move together.** `TestFailOriginVocabularyMatchesCheck`
  hard-codes the `00126` path and must be repointed at the migration that now
  declares the live CHECK; `TestCoerceFailOrigin` asserts the worker-reportable and
  server-only partitions sum to the vocabulary, so the new member must be added to
  `workerReportableFailOrigins`. Missing either reddens the Go gate.
- **Patch is sanitized and capped, not byte-exact.** The stored patch passes
  `secretscrub` and a size cap, so control-char stripping/truncation can mutate it;
  a very large diff may not `git apply` cleanly. Acceptable for workflow YAML (the
  #188 file was 112 lines); document that `git apply` is best-effort and the run
  view is the source of truth. Cap the `failure_reason` path list (512-char field),
  never the doc link.
- **Detection false-negative via Bash writes (M3 only).** The edit-time advisory
  can't see a Bash `tee`/`sed -i` into a workflow file. Mitigation: the
  authoritative detection is the finalize diff (M1), which sees the committed result
  regardless of how it was written; M3 is only an early nicety.
- **Divergent treatment of auto-`ci_fix`.** An auto-approved `ci_fix` touching
  `.github/workflows/**` is already blocked earlier (`runner.ts:1192-1218`,
  `agent_failure`, no preservation), while a human-approved `ci_fix` reaches M1's
  new typed outcome. Mitigation: acknowledge the split; optionally fold the auto path
  into the same typed outcome as a follow-up (out of scope for M1).

## Decision Log

- **D1 (decided) — terminal shape is `failed`, preservation is a forge-independent
  api-stored patch.** A `completed`+`report_only` run can carry neither
  `fail_origin` nor `failure_reason` (disjoint render paths; see Key facts), reads
  as success to the judge/board, and is issue-gated. A `failed` run carries the
  typed reason and origin natively and serves all kinds. The diff is preserved in a
  stored field sent to the api on the failed report — never a forge push, so it is
  immune to the workflow-scope block that a checkpoint-ref mirror might hit.
- **D2 (open, resolve in M4/M2) — typed reason on the wire.** Option A: keep the
  user-facing message in `failure_reason` (already rendered on the failed card),
  use `fail_origin` only for internal classification. Option B: add `fail_origin`
  to `RunDTO` for a structured badge. **Recommendation: A** for the MVP (no DTO
  change to ship); revisit B only if the UI wants a distinct badge.
- **D3 (decided) — store the patch in a dedicated field, not `report_md`.** Reusing
  `report_md` would entangle the report-only semantics ("committed nothing",
  issue-gated) with a failed run that did commit. A dedicated nullable column keeps
  both clean; it rides the same failed report and render path.
- **D4 (decided) — predicate is `forge_type === "github"`, and it is exact.** Every
  connected GitHub token is a classic `repo`-only PAT, so there is no connected-token
  shape that is both GitHub and able to push `.github/workflows/**`. Fine-grained
  tokens (`github_pat_`) cannot be connected at all: `VerifyToken` rejects them up
  front (`api/internal/forge/github.go:169`) and the connect handler aborts the save
  on that error before the scope gate runs (`api/internal/handler/forge.go:219-224`);
  privcheck then forbids `{repo, workflow}` for the classic tokens that do connect.
  (An earlier draft carried a "fine-grained tokens might be accepted, so the predicate
  over-blocks harmlessly" caveat — that was wrong; they are refused at connect, so no
  caveat is needed and `docs/github-bot-setup.md` is correct as written.)
- **D5 (decided) — detection point is finalize, not edit-time.** The finalize diff
  is the only point that both sees the committed result (regardless of Bash vs.
  file-tool writes) and can preserve it. Edit-time guardrail detection is an
  advisory (M3), never the authoritative outcome.
- **D6 (decided) — a null diff fails open to the normal push.** `changedFiles`
  returns `null` on a diff-computation failure; blocking on it would fail a
  possibly-legitimate non-workflow run. The whole point is graceful handling, so on
  null the run proceeds to the normal push (and faces the pre-existing behaviour).
  Covered by a test.

## Parallelization

- **M1** is the foundation (detection + `fail_origin` + preservation). Its api half
  (migration + enum + tests + patch column) and agent half (detection + report) are
  sequentially coupled: the enum member must exist before the worker reports it, so
  the api half lands first within M1.
- **M2** (web, `RunView.tsx`) depends on M1's wire shape (the patch field + D2).
- **M3** (edit-time advisory, `agent/src/guardrails.ts`) is independent of M2 and
  can run parallel to it once M1 lands.
- **M4**'s test matrix depends on M1-M3; its docs half (`docs/github-bot-setup.md`)
  is independent of the code and can start immediately.

Phase 1: M1 (api half → agent half). Phase 2 (parallel): M2, M3. Phase 3: M4
(docs half may start in Phase 1).
