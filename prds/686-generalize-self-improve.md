# PRD #686 — Generalize self-improve to any repo (keep uzi dogfooding intact), and retire the long-lived self-improve branch

**Issue**: [#686](https://github.com/vtmocanu/uzi/issues/686)
**Priority**: Medium
**Status**: Planned

> **Scope note (post-review):** A three-reviewer pass found that the uzi-specific *directive*
> is worker-side and selected by run **kind**, not by any flag — so generalizing the server
> description const alone produces a self-contradictory run. This PRD therefore spans the api,
> the claim/poll wire, the **controller**, the **agent worker**, and the **web** repo toggle, not
> just the api. See
> "The blocker" below. It is a cross-cutting change; delivery mode should account for that.

> **Scope note 2 — branch model folded in (2026-08-25):** This PRD now **also retires the
> long-lived `uzi/self-improve` branch** in favour of **fresh-per-cycle branches off current
> `main`, a cap on concurrent open MRs, and an independent pick per cycle** (Part B of the
> Solution; D9–D11; M8–M10). It was folded here rather than shipped as a separate PRD because it
> edits the exact branch surface D4 already owns (`self_improve.go:43-44`,
> `agent/src/self-improve.ts:26`, `runner.ts:2607-2608`) — a separate PRD would three-way-collide
> on those files. The two halves also synergize: a *generic* self-improve running on N different
> repos must not inherit uzi's single-accreting-branch pathology. See "Relationship to sibling
> PRDs (#297, #456, #699)".

## Problem

The scheduled `self_improve` job's *mechanism* is generic — any user can enable it on any repo they own, off by default, no admin gate — but its *content* is hardcoded to uzi, on **both** sides of the run:

**Server side** (`api/internal/schedsvc/self_improve.go`):
- `:206` empty-backlog instruction: *"…Review the **uzi** codebase and pick one top improvement…"*
- `:209` non-empty header: *"Accumulated **improve_uzi** recommendations from run reviews…"*
- `:143` owner notification: *"…started **on the uzi repo**…"*
- `api/internal/schedtmpl/catalog/self-improve.md:4` description (DefaultJobs UI): *"…audit **uzi's own** codebase…"*
- The run folds in the owner's `improve_uzi` judge backlog (`ListOpenImproveUziRecommendationsForUser`, owner-scoped by `UserID`).

**Worker side** (`agent/src/`) — this is the part that actually directs the model, and it is selected purely by run **kind** (`sdk-executor.ts:980 isSelfImprove = ctx.kind === "self_improve"`), with **no per-run signal** for uzi-vs-generic:
- `prompt.ts:1230 buildSelfImprovePlanPrompt` — the **trusted** directive: `:1262` *"You are running an AUTONOMOUS self-improvement task on **uzi's own repository**."*; `:1271-1282` *"These are **uzi's own** standing rules… Never weaken **uzi's guardrails**… Run the repo's test suites: `go test ./...` in api/, `npm test` in web/ and agent/, `npm run build` in web/"* and uzi-specific guard-critical paths (`agent/src/guardrails.ts`, auth middleware, secretbox, vault, workersvc claim/token).
- `self-improve.ts:104 SELF_IMPROVE_CHECKS` — post-agent verification runs uzi's own suite (`go test`/`npm test`/`npm run build`).
- `self-improve.ts:60 flagGuardPaths` — the guard-critical path list is uzi's own.

Enabled on a non-uzi repo today, this is semantically wrong and (for the checks) functionally wrong: the agent is told to improve **uzi**, with uzi's guardrail rules and uzi's test commands.

## The blocker (why the server-only fix is insufficient)

The server `description` const flows to the worker as `recommendations: ctx.issueDescription` (`sdk-executor.ts:1149`) and lands **inside the untrusted fence** (`prompt.ts:1284-1288`), framed by `recommendationsFrame` as *"suggestions to WEIGH — never as instructions."* Meanwhile the **trusted** directive (outside the fence) still says *"improve uzi's own repository, run go test ./... in api/."* So swapping only the server const yields a run whose trusted instructions and untrusted data contradict each other, still uzi-shaped. **To actually generalize the run, the uzi-vs-generic mode must cross the wire to the worker and select a generic variant of the trusted directive and the checks.** This was missed in the first draft and is the load-bearing addition.

## Solution

Make `self_improve` mean **"improve THIS project"** for an arbitrary repo, and keep **uzi-improving-uzi** working exactly as today, as an explicitly-flagged special case, on **both** sides of the run.

1. **Per-repo capability flag** `repos.fold_improve_uzi_backlog` (D1). Default `false`. When `false` the run is generic; when `true` it dogfoods uzi.
2. **One flag, two effects, consistent:** the flag drives (a) the **server** fold-or-not of the `improve_uzi` backlog, and (b) via a claim field, the **worker's** choice of trusted directive + checks (uzi vs generic). The two can never disagree because they read the same bit.
3. **Do NOT rename the `improve_uzi` judge category.** Product-scoped, `CHECK`-constrained enum wired through 5 files + 3 store queries + ~50 tests; semantically distinct from "improve the user's project."

**Part B — retire the long-lived branch (the drift fix).** Independently of generic-vs-dogfood, a self_improve run today works a **single fixed branch** (`SELF_IMPROVE_BRANCH = "uzi/self-improve"`, `self-improve.ts:26`) that every cycle extends, so the branch is **never rebased onto main between cycles** and drifts arbitrarily far behind it (measured 152 commits / 3 days on 2026-08-25). This PRD replaces that with:

4. **Fresh-per-cycle branch** off current `main` (D9). Each cycle branches anew, so the agent always plans against current main — the missing property that let a cycle re-solve already-merged work (the #699 case in "Relationship to sibling PRDs").
5. **Cap on concurrent open self-improve MRs** (D10). At the cap the cycle is skipped (backpressure) instead of piling on; below it, several **independent** self-improve MRs may be open at once.
6. **Independent pick per cycle** (D11): the picker is shown the currently-open self-improve MRs and instructed to choose a non-overlapping improvement — reusing #297's untrusted-fenced in-flight signal, extended to open MRs. This is the maintainer's ask: "when an MR is already open, check it first and propose a different improvement."

## Design decisions (Decision Log)

### D1 — The capability flag lives on `repos`, a new boolean column
**`repos.fold_improve_uzi_backlog boolean NOT NULL DEFAULT false`** — named for what it does, **not** `is_uzi_product` (the design deliberately does not verify repo identity; see D2), an explicitly-granted capability.

**Why `repos`, not `run_schedules` — reset durability is decisive.** Two schedule-reset shapes exist and the maintainer uses both: `POST /schedules/{id}/reset` (`ResetDefaultSchedule`, UPDATE, same id) and delete+re-enable (`CreateDefaultSchedule`, a **brand-new** row seeded from the catalog). A per-schedule flag is lost on delete+re-enable; making the catalog carry it would fold-for-everyone (the very bug being fixed). A per-repo flag is a durable repo fact untouched by any schedule lifecycle op. `repos` is owned by exactly one user (`connection_id → forge_connections.user_id`), so the owner-scoped backlog has a well-defined owner per flagged repo.

**Precedent (single-bool):** `repos` already carries `repo_skills_enabled` (migration `00040`), `repo_devbox_opt_in` (`00047`), `repo_claudemd_enabled` (`00108`), each `NOT NULL DEFAULT false`. The correct single-bool setter to mirror is **`SetRepoDevboxOptInForUser` (`forge.sql:195`)** — *not* `SetRepoTrustFlagsForUser` (`:235`), which sets two flags at once. All are **excluded from `UpsertRepo`'s SET list** (`forge.sql:58-64`, path/url/branch only) so they survive membership re-sync.

### D2 — No repo-identity detection
URL/name sniffing ("is this `vtmocanu/uzi`?") is rejected: fragile and wrong for forks/self-hosters. Dogfooding is an explicitly-granted per-repo capability.

### D3 — Migration backfills every repo with an existing self_improve schedule
```sql
-- +goose Up
ALTER TABLE repos ADD COLUMN fold_improve_uzi_backlog boolean NOT NULL DEFAULT false;
UPDATE repos SET fold_improve_uzi_backlog = true
 WHERE id IN (SELECT DISTINCT repo_id FROM run_schedules WHERE target = 'self_improve');
-- +goose Down
ALTER TABLE repos DROP COLUMN fold_improve_uzi_backlog;
```
`run_schedules.repo_id` is `uuid NOT NULL REFERENCES repos(id)` and `target='self_improve'` is a valid enum, so the backfill cannot be NULL, cannot flag a non-self_improve repo, and cannot miss the maintainer's schedule — **no hardcoded path**. Every self_improve schedule that has ever shipped carried uzi-semantics, so this preserves current behavior on every instance (transparent upgrade for the maintainer). New repos default `false` → generic. `ADD COLUMN` with a constant `DEFAULT false` is metadata-only on PG11+; the backfill touches few rows (`repos` is one row per user-repo). Because D2 rejects identity detection, "backfill the existing self_improve repos true" is the **only coherent** preservation path — so "preserve" is effectively forced.

### D4 — Keep the label + title strings (but NOT the fixed branch — superseded by D9)
Keep the **label** `uzi-self-improve` (`self_improve.go:36`) and the **title** `uzi self-improvement` (`:41`). They read as *"uzi the **tool** did this"*, correct on any repo; `web/src/lib/boardCards.ts:40 SELF_IMPROVE_LABEL` hides the tracker card **by the label**, not the branch, so keeping the label preserves it, and the single tracking issue is unchanged.

**Corrected 2026-08-25 (Part B):** an earlier D4 also kept the fixed **branch** `uzi/self-improve` and listed "renaming the branch" as what breaks `boardCards`. That conflated two things — `boardCards` keys on the *label*, so the branch is free to change — and the fixed branch turned out to be the drift source Part B fixes. **D9 supersedes the branch half of D4:** the branch becomes fresh-per-cycle (`self-improve.ts:26`, `runner.ts:2607-2608`, tracking body `self_improve.go:43-44`); the label + title stay.

### D5 — Owner setter is required (reconnect durability)
A repo delete+reconnect drops the `repos` row (CASCADE) and re-creates it via `UpsertRepo`, which does not set trust flags — so without a setter, a re-connected flagged repo silently reverts to generic. Ship `SetRepoFoldImproveUziBacklogForUser` mirroring `SetRepoDevboxOptInForUser` (`forge.sql:195`), plus PATCH wiring (see M1). The migration backfill fires once and cannot cover a future reconnect.

### D6 — Web toggle: DECISION NEEDED (natural home exists)
Correction to the first draft: the sibling flags **do** have web controls — `web/src/pages/Repos.tsx` has `setRepoSkills` (`:472`), `setRepoDevbox` (`:576`), and the Trusted-repo panel (`:468`). So the natural UI home for this flag is that same Repos panel, and "defer it" is *less* justified than first stated. However, the fold flag is only ever set by someone dogfooding uzi on a fork/self-host (a normal user never sets it — generic is the default), so a UI is low-value. **Options:** (a) add the toggle to the Repos Trusted panel now (small, pattern exists, pulls in `task gate:web`); (b) ship the API setter only and defer the toggle. **Chosen: (a)** — add the toggle now (M7), mirroring `setRepoDevbox`. The API setter (D5) still exists and covers reconnect durability; the toggle is the owner's way to grant/revoke the capability from the UI.

### D7 — The uzi-vs-generic mode crosses the claim/poll wire
The worker needs a per-run signal to pick the generic vs uzi directive. Add a boolean to `ClaimResponse` (`agent/src/protocol.ts:541`) — e.g. `self_improve_dogfood?: boolean` — populated at server claim assembly from `repo.fold_improve_uzi_backlog`. **Absent ⇒ generic** (safe: a non-uzi or older-server run can never accidentally get the uzi directive). This is the same kind of field `auto_approve` (`protocol.ts:587`) already is, so it is low-novelty — but it **crosses the controller poll-wire contract** (`api/internal/hostedsvc/testdata/controller_poll_wire.json` + `controller/internal/protocol/protocol.go` + the `-count=1` cross-module golden `protocol_contract_test.go`), so the **controller** toolchain is in scope.

### D8 — ADR-worthy
Materialize `adr/0686-generalize-self-improve.md`: D1 (flag location + reset-durability), D2 (URL-sniffing rejection), D3 (backfill), D7 (mode crosses the trusted-directive boundary — why a claim field and not a server-authored prompt), **D9 (retire the long-lived self-improve branch — the drift fix and its evidence)**. <!-- check-docs:ignore-path: forward reference, M6 creates this ADR -->

### D9 — Retire the long-lived `uzi/self-improve` branch → fresh-per-cycle branches
**The fixed branch is the drift source.** `SELF_IMPROVE_BRANCH = "uzi/self-improve"` (`self-improve.ts:26`, consumed at `runner.ts:2607-2608`) is reused every cycle so the worker's idempotent `createMergeRequest` (`agent/src/forge.ts:100`) extends one open MR (`pushBranch` never forces). PRD #46's rationale called this *"MR reuse is free"* (`prds/done/46-run-judge-self-improvement.md:250-256`). **It is not free:** because the branch is never rebased onto main between cycles, it drifts — measured 2026-08-25 at **~152 commits / 3 days** behind main — so a cycle plans against a stale tree (the #699 case below). *(This drift figure is a live-branch measurement, not offline-verifiable from repo code; the implementer/operator can reproduce it with `git rev-list --count origin/uzi/self-improve..origin/main` against a freshly-fetched `origin/uzi/self-improve`. It is motivation, not a load-bearing anchor — the #699 duplicate is the concrete evidence.)*

**Change:** derive the run's branch **per cycle** off current main — e.g. `uzi/self-improve/<runId>` (uzi never force-pushes, so a new cycle cannot safely reuse a name whose MR is still open). Each cycle clones current main as its base, so ADR-456's finalize workflow-align (which exists because a behind-on-workflows branch cannot push under the scope-less PAT) rarely even fires for this lane. The label + title (D4) are unchanged; the tracking-issue **body** at `self_improve.go:43-44` ("opens or extends one merge request on the `uzi/self-improve` branch") is reworded to "opens a fresh merge request each cycle".

**Considered and rejected — keep one branch, reset-to-main-when-merged + skip-when-open.** That also kills drift and keeps the single branch name, but forecloses the maintainer's chosen shape (several independent open MRs, D11). Recorded as the simpler fallback if the multi-MR shape is ever unwanted.

### D10 — Cap concurrent open self-improve MRs; skip the cycle at the cap
With per-cycle branches, nothing bounds how many open self-improve MRs accumulate if the human does not merge (PRD #46's one-branch model bounded it implicitly at exactly one). Bound it explicitly: before `fireSelfImprove` (`self_improve.go:59`) creates the run, take the **forge-sourced** open-self-improve-MR count (D12 — **not** `runs.mr_state`, which is unreliable here; see D12 for why) and if it is ≥ K (a named const `selfImproveMaxOpenMRs`, default **2**), **skip** the cycle: no run created, a `selfimprove_skipped` notification mirroring the vault-locked skip (`self_improve.go:76`), and skip semantics for `last_fire` (a skip is not a failure). Below K, the cycle fires normally.

### D11 — Independent pick: show the picker the open self-improve MRs
So concurrent MRs stay **independent** rather than racing on the same fix, the picker must see what is already proposed. #297 already carries a claim-time, untrusted-nonce-fenced **in-flight** list to the worker (`agent/src/executor.ts:162-166 inflightTargets`, shipped and **live post-#590** — it is on the executor context, not the deleted `selfimprove/engine.go`). Extend that signal (or add a sibling claim field) with the **open self-improve MRs' titles/descriptions** for the repo, rendered in the same nonce fence as untrusted data, and instruct the picker to choose an improvement that does not overlap them. **Advisory, exactly as #297 is** — the picker is an LLM applying judgment; independence in *intent* does not guarantee non-conflict in *git*. A merge conflict between two genuinely-independent MRs is the human's merge-order call (normal PR hygiene), caught by the up-to-date required-status-check + CI-on-merge; what Part B removes is the *silent* stale-accumulation and duplicate-work class, not all conflicts. **Which MRs are open comes from the forge (D12); what each proposed comes from that cycle's own run row** (its `plan_md`/`issue_description`), not the MR body — so the picker context does not need a rich forge MR-body read.

### D12 — Open-MR state is FORGE-sourced (feeds both the cap D10 and the picker D11)
**Why not `runs.mr_state`.** The obvious count — `runs WHERE kind='self_improve' AND mr_state='opened'` — is broken for this lane. `runs.mr_state` is written only by `SetRunMRState` (`runtime.sql`), fed from `ListMRWatchCandidates`, which is `DISTINCT ON (r.issue_iid)` (`forge.sql:486`) — it watches **only the latest run per tracking issue**. Part B keeps ONE tracking issue (D4) but opens **N** MRs on it, so once a newer cycle wins the DISTINCT-ON, an older cycle's `mr_state` **freezes** and never reflects a later human merge/close: the cap would count phantom-open MRs and **wedge at K forever** (self-improve silently stops). Worse, the tracking issue is permanently *open*, so the watch may never bootstrap `mr_state` for these runs at all (count always 0 → cap is a silent no-op). Either way `runs.mr_state` cannot be the source.

**The source (forge-first, matching "the forge is the source of truth").** At each fire, resolve the repo's open self-improve MRs live from the forge:
- **Candidate set (DB, bounded):** the repo's most-recent **M** completed `self_improve` runs with `mr_iid IS NOT NULL` (`ORDER BY created_at DESC LIMIT M`, M small — e.g. 10; the cap is 2, cycles are ~2 days apart, so a tiny window covers every plausibly-open MR without an unbounded historical scan). New store query, scoped by `repo_id`.
- **Live state (forge):** `GetMergeRequest(mr_iid)` per candidate (the existing `Forge` method, returns state — `MRStateOpened`/`closed`/`merged` — so **no new interface method, no 3-driver/6-fake fan-out**). Count the ones still `opened`. That count feeds the D10 cap.
- **Picker content (DB):** for each still-open candidate, its run's `plan_md`/`issue_description` is the "what was proposed" the picker (D11) needs — carried on the claim, no forge MR-body read.
- **Bounded and cheap:** ≤ M `GetMergeRequest` calls once per cycle (~2 days). If M ever proves too tight (many long-open MRs), the fallback is a single `Forge` `ListOpenMergeRequestsByBranchPrefix("uzi/self-improve/")` method (all 3 drivers + 6 fakes — a real milestone), recorded here as the escalation, not the default.

The ADR (`adr/0686`) must record D12 as the load-bearing correction to the naive `mr_state` count.

### Relationship to sibling PRDs (#297, #456, #699) — why Part B lives here and what it does NOT duplicate
- **#297 (in-flight reconciliation, shipped & live).** Its claim-time untrusted in-flight list (`executor.ts:162 inflightTargets`) is the mechanism D11 reuses; Part B does **not** reimplement it, only extends it to open self-improve MRs. #297 makes the picker skip *in-flight runs*; Part B (fresh base + open-MR context) makes it skip *already-landed* and *already-proposed* work — the halves #297 could not cover (a completed-but-open MR is not an in-flight run, and a merged fix is invisible to a stale-base agent). **Its docstrings/citations mentioning `selfimprove/engine.go` are stale post-#590** (the live anchor is `executor.ts:162`); noted, not fixed here.
- **#699 (the evidence, 2026-08-25).** The self_improve branch, 152 commits behind main, re-solved issue #326 with a change functionally identical to the already-merged **#695**, because its stale base hid #695. Fresh-per-cycle (D9) is the direct fix; #297's list alone did not prevent it (advisory + status-driven + stale base). PR #699 was closed as superseded.
- **#456 / #377 (finalize workflow-align / early-fail-on-workflow).** The machinery that lets a behind-on-workflows branch push under the scope-less PAT (ADR-456). A fresh-per-cycle branch is rarely behind on workflows, so Part B makes these **mostly moot for the self_improve lane** — it does **not** remove them (a normal issue run can still drift onto main's workflows mid-run). No file collision.

## Internet-independence (offline worker)

Entirely in-repo work (Go api + controller, TS agent, web SPA, one goose migration, **plus Part B's second store query**, one catalog string, tests, docs). **No open-web dependency** — nothing needs external API/library semantics, docs sites, or web search. Part B is likewise all in-repo (agent branch derivation, one server count query + fire-path guard, one claim field, prompt/fence edits). Safe for a restricted-egress worker. Caveat: the **live-DB** proofs (new migration/queries) need a throwaway Postgres, which the offline worker container cannot run; that proof comes from the MR's CI `test:api-store-it` job (forge egress allowed), not the worker itself.

## Workflow-scope constraint (`.claude/rules/prds.md`)

Touches **no `.github/workflows/**`** files. Keep it that way in implementation AND validation — the worker PAT lacks `workflow` scope; any workflow-file touch is an atomic push rejection that loses the branch.

## Milestones

### M1 — Per-repo capability flag: schema, backfill, read path, owner setter + PATCH wiring
- Goose migration (draft `00165`; **live head is `00164` (`00164_user_default_effort.sql`, landed in v0.64.0) — renumber to the next free number above the live head at merge**, strict goose). SQL as in D3. Do not write the literal `+goose` token in any comment except a real annotation.
- Add `r.fold_improve_uzi_backlog` to the explicit SELECT column list of `GetRepoForUser` (`forge.sql:86`, inside the `:one`/`GetRepoForUserRow` query at `:80-92`), then `sqlc generate` with the **repo's currently-pinned sqlc** (v1.31.1 as of #679 / v0.64.0 — match `Taskfile.yml`/CI, do not hardcode an older version); confirm `GetRepoForUserRow` gains `FoldImproveUziBacklog bool`. The fire path already fetches this row (`self_improve.go:63`) — **no new read query**.
- Owner setter `SetRepoFoldImproveUziBacklogForUser` in `forge.sql`, mirroring `SetRepoDevboxOptInForUser` (`:195`); regenerate.
- PATCH wiring in `api/internal/handler/forge.go`: add a `foldSet := req.RepoFoldImproveUziBacklog != nil` group to `patchRepoRequest` (`:906`) and the handler (`:958`), mirroring the devbox branch (`:997-1002`). **Also update** the mutual-exclusivity counter `b2i(devboxSet)+b2i(trustSet)+b2i(capsSet) > 1` (`:975`) and both error strings (`:979`-ish, the ">1" and the "provide X/Y/Z" empty-request messages) to include the new group. The handler pairs each owner `*ForUser` setter with an admin-unscoped variant by `user.IsAdmin` (`:992-1002`): **decide** whether the fold flag needs the admin variant (recommend: yes, for parity) or owner-only.
- **Success:** column exists; backfill flags every self_improve repo; `GetRepoForUser` returns the flag; owner (and admin) can set it via PATCH; the exclusivity guard treats the new group like its siblings.

### M2 — Server: generic vs fold branch + repo-named notification
In `fireSelfImprove` (`self_improve.go:59`), replace the always-fetch step 6 (`:106-114`) with a branch on `repo.FoldImproveUziBacklog`:
```go
var recs []store.ListOpenImproveUziRecommendationsForUserRow
var description string
if repo.FoldImproveUziBacklog {
    recs, err = e.store.ListOpenImproveUziRecommendationsForUser(ctx, /* unchanged */)
    if err != nil { return FireOutcome{}, err } // transient
    description = composeSelfImproveDescription(recs) // VERBATIM today
} else {
    description = genericSelfImproveDescription // new const, "Review this project's codebase and pick one top improvement (a bug, a feature, or a refactor)."
}
```
- Leave `composeSelfImproveDescription` (`:204`), the empty-backlog string (`:206`) and the header (`:209`) **unchanged** — fold mode only.
- Step 8 `MarkImproveUziRecommendationsAddressed` (`:131`) already guards `if len(ids) > 0`; generic path yields empty `recs` → naturally skipped.
- Notification (`:143`): name the repo in both modes, e.g. `"A self-improvement run has started on " + repo.PathWithNamespace + ". …"` (`r.path_with_namespace` is on the row).
- Catalog description (`catalog/self-improve.md:4`): "audit uzi's own codebase…" → "audit this project's codebase…". Keep `name: Self-improvement`.
- **Success:** a flagged repo folds the backlog and produces today's description verbatim; an unflagged repo produces the generic description and fetches no backlog; the notification names the target repo.

### M3 — Server→worker mode signal (crosses the poll wire)
- Add `self_improve_dogfood?: boolean` to `ClaimResponse` (`agent/src/protocol.ts:541`), documented (absent ⇒ generic).
- Populate it at server claim assembly from `repo.fold_improve_uzi_backlog`, alongside where `issue_description`/`auto_approve` are set for a self_improve claim (`api/internal/hostedsvc` claim assembly + the compose/local claim path).
- Update the controller poll-wire contract: `api/internal/hostedsvc/testdata/controller_poll_wire.json`, `controller/internal/protocol/protocol.go`, and the `-count=1` cross-module golden `controller/internal/protocol/protocol_contract_test.go` so the new field round-trips.
- **Success:** a self_improve claim carries `self_improve_dogfood` = the repo flag; the controller passes it through; the golden pins the wire; absent defaults to generic.

### M4 — Worker: generic directive + generic checks (depends on M3)
- Parameterize `buildSelfImprovePlanPrompt` (`prompt.ts:1230`) on the mode (thread `ctx.selfImproveDogfood` from `sdk-executor.ts:1147`):
  - **dogfood variant:** byte-identical to today (`:1262-1282`) **EXCEPT the branch line(s) — see the M8/Part-B carve-out below.** `prompt.ts:1263-1264` currently reads *"You are on the fixed branch `${input.branch}`, which may already carry an open merge request from a previous cycle — extend it rather than starting over."* Under M8/D9 the branch is fresh-per-cycle and carries **no** prior MR, so that sentence is now false and must be reworded (e.g. *"You are on this cycle's branch `${input.branch}`; open a new merge request for your change."*). So M4's "byte-identical" pin, M5(ii)'s regression test, and SC2 are all scoped to *"byte-identical except the branch sentence"* — the interpolated `${input.branch}` still flows through unchanged; only the static prose around it changes.
  - **generic variant:** "You are running an AUTONOMOUS self-improvement task on **this repository**."; drop the uzi-literal standing-rules block and the hardcoded `go test ./... in api/` test commands; keep the generic guardrails (never weaken the repo's guardrails/auth/secret paths, a human reviews and merges, never push to `main`), and instruct the agent to discover and run **the target repo's own** test/build gates. The branch sentence uses the same reworded (no-prior-MR) form. Keep the untrusted-fence invariant in **both** variants (recommendations fenced, trusted directive outside).
- `SELF_IMPROVE_CHECKS` (`self-improve.ts:104`) and `flagGuardPaths` (`:60`): in generic mode do **not** assert uzi's fixed suite against an arbitrary repo (today they honest-skip when the dirs are absent, `self-improve.ts:342`, so it is not a crash — but the MR evidence is uzi-shaped). Make the generic run's post-agent verification reflect the target repo's own gates, or omit the uzi-specific checks section. Dogfood mode keeps `SELF_IMPROVE_CHECKS` exactly.
- **Success:** a dogfood run's plan prompt is byte-identical to today **except the branch sentence** (`:1263-1264`), which now says "open a new MR" rather than "extend it"; a generic run's plan prompt contains none of "uzi's own repository", the uzi standing rules, the uzi guard paths, or `go test ./... in api/`, and its checks/evidence are not uzi-specific.

### M5 — Tests across all touched toolchains
- **api** (`api/internal/schedsvc/scheduler_test.go`): the harness default `repoRow` (`:358`) sets no flag → generic; set `repoRow.FoldImproveUziBacklog = true` **and** `repoRow.PathWithNamespace` there so the existing `TestTickSelfImproveFiresFoldsAndAdvances` (`:2038`, asserts "jq" at `:2074`) still models the dogfood case (**this existing test must be updated, not just augmented**). Then add: (case 2) fold ON + empty backlog → description equals the "review the uzi codebase" string; (case 3) fold OFF → `ListOpenImproveUziRecommendationsForUser` not called (observable: `siRecsUserParam` stays zero while `sched.UserID` is non-zero, `:110-112`), `MarkAddressed` not called, description equals the generic string and contains neither "improve_uzi" nor "uzi codebase". Assert the started notification body (via `fakeNotifier.notifications`, `:326`) contains `PathWithNamespace`. Add store/handler tests for `SetRepoFoldImproveUziBacklogForUser` + the PATCH group (owner and, if chosen, admin; and the exclusivity guard rejecting fold combined with another group). Assert **content**, never a bare count (repo count-trap discipline).
- **agent** (`agent/test/prompt.test.ts`, `self-improve.test.ts`): existing `buildSelfImprovePlanPrompt` assertions (`:924`,`:949`,`:1033`, fence/nonce `:1008+`) run under a dogfood context; add (i) generic variant omits the uzi strings above, (ii) dogfood variant byte-identical (regression pin, mirrors api case 1), (iii) untrusted-fence invariant holds in both; add a generic-checks case in `self-improve.test.ts` if M4 generalizes the checks. Run `task gate:agent`.
- **controller**: `task gate:controller` with the poll-wire golden updated (`-count=1` is load-bearing — the golden lives in the other module).
- **live-DB**: the new migration + `GetRepoForUser` column proven by CI `test:api-store-it` (RUN>0, zero SKIP); the offline worker cannot run it (needs docker Postgres).
- **web** (M7): a Repos-panel toggle test; run `task gate:web`.
- Full gates: `task gate:api`, `task gate:agent`, `task gate:controller`, `task gate:web`.
- **Part B:** scheduler test for the open-MR cap (fake **forge** returns K opened → cycle skipped, notification **body** "open-MR cap reached" asserted — not the shared kind, N6; K-1 opened → fires); a live-DB candidate-query test with the N1 discriminating fixtures (NULL-`mr_iid`, `merged`/`closed`, wrong-`repo_id`, non-`self_improve` rows) asserting the candidate set is exactly recent-completed-self_improve-with-mr_iid-this-repo; an agent test that the per-cycle branch derives from `runId` and two cycles yield distinct branches, **and that the dogfood prompt is byte-identical except the reworded branch line** (B1); an agent **adversarial** fence test (N2: forged-close-tag MR content cannot break out, matched-nonce ≠ forged nonce), not a benign "renders" test.
- **Success:** all api cases pass (existing fold test updated + cases 2/3 + notification + setter/handler + Part B forge-cap + candidate query), agent dogfood(-except-branch)/generic/fence-breakout/per-cycle-branch tests pass, controller golden updated only if M3/M10's N3 reconciliation says claim fields cross it, live-DB green in CI.

### M6 — Docs & decision record
- ADR `adr/0686-generalize-self-improve.md` (D1/D2/D3/D7). <!-- check-docs:ignore-path: forward reference, M6 creates this ADR -->
- `docs/scheduling.md`: self-improve is now generic ("improve this project"); folding the `improve_uzi` backlog + the uzi directive is a per-repo capability (`fold_improve_uzi_backlog`).
- `specs/ai.md`: per-repo capability flag; catalog self_improve generalized; uzi dogfooding is the flag-gated special case; the mode crosses the claim wire; **Part B — self_improve now uses fresh-per-cycle branches (`uzi/self-improve/<runId>`), a concurrent-open-MR cap, and an open-MR-aware independent pick.**
- ADR `adr/0686`: include D9 (branch model + drift evidence) **and D12 (open-MR state is forge-sourced, not `runs.mr_state`, and why)**.
- `docs/scheduling.md`: also note the **cap** (default 2 open self-improve MRs; further cycles skip until a human merges) and that on upgrade the maintainer should **close the pre-existing long-lived `uzi/self-improve` MR** once superseded so it stops occupying a cap slot.
- `CLAUDE.local.md` is machine-local (out of repo scope — do NOT edit here) but its PVC-recovery ref path gains a `<runId>` segment under D9; flag to the maintainer.
- `CHANGELOG.md` `[Unreleased]`: scheduled self-improve now targets the enabling repo generically **and opens a fresh MR per cycle instead of extending one long-lived branch**.
- **Success:** ADR committed (D1/D2/D3/D7/D9/D12); scheduling docs + specs reflect generic behavior, the flag, the branch model and the cap; CHANGELOG updated; `web/scripts/check-docs.mjs` passes.

### M7 — Web toggle in the Repos Trusted panel (D6 option a)
- Add a `fold_improve_uzi_backlog` toggle to the Trusted-repo panel in `web/src/pages/Repos.tsx`, mirroring `setRepoDevbox` (`:576`) / `setRepoSkills` (`:472`): the control, its optimistic state, and the API call.
- Plumb the DTO end-to-end: the field on the repo type + the PATCH request body in `web/src/lib/api.ts`, matching the M1 handler group (`RepoFoldImproveUziBacklog`). The toggle is owner-scoped; render it read-only/absent for a non-owner exactly as the sibling flags do.
- Add a web test (the sibling toggles have coverage) that the toggle renders and issues the PATCH; run `task gate:web` (and `web/scripts/check-docs.mjs` is already part of `npm run build`).
- **Success:** an owner can turn the fold capability on/off from the Repos panel, the change persists via PATCH, and `task gate:web` is green.

### M8 — Per-cycle self-improve branch (retire the fixed branch, D9)
- Replace the `SELF_IMPROVE_BRANCH` const (`agent/src/self-improve.ts:26`) with a per-run derivation — e.g. `selfImproveBranch(runId) => \`uzi/self-improve/${runId}\`` — mirroring the shipped `uzi/prompt-${runId}` pattern (`runner.ts:2614`). Update its two consumers at `runner.ts:2607-2608` (the branch, and the clone key `branch.replace(/\//g, "-")`), **and the adjacent fixed-branch comment block at `runner.ts:2601-2606`** ("The FIXED branch … reused every cycle so … createMergeRequest extends one open MR"), which becomes false. Grep `agent/src` for both the symbol `SELF_IMPROVE_BRANCH` and the literal `uzi/self-improve` to catch every consumer; **reread** the self_improve/fixed-branch comments in `git.ts` (`:161` "self_improve fixed branch's previous cycles", `:199` reseed `update-ref`, `:366`, and the block `:1196` / `:1545-1548` "the `self_improve` fixed branch … Force-push is denied by the guardrails") and adjust any that assume one branch. **(Fact-check note: the earlier draft cited `git.ts:1168`, which is a bare `return (`; the real comments are at `:1196` and `:1545-1548`.)**
- **The plan-prompt branch line is part of this surface (B1).** `prompt.ts:1263-1264` ("You are on the fixed branch … extend it rather than starting over") must be reworded per the M4 carve-out — it is NOT covered by grepping the literal `uzi/self-improve` (it interpolates `${input.branch}`), so it is listed here explicitly.
- Reword the tracking-issue body const at `self_improve.go:43-45` from "opens or extends one merge request on the `uzi/self-improve` branch" to "opens a fresh merge request each cycle". Keep the label (`:36`) and title (`:41`) byte-identical (D4). **This affects only newly-filed tracking issues** (`ensureTrackingIssue` sets the body on `CreateIssue`); an existing instance keeps its old body — cosmetic, note it.
- Confirm resume still targets the same cycle's branch: the resume path keys on `runId`, and the branch now derives from `runId`, so a resumed cycle reuses its own branch (no cross-cycle reseed). Call this out in the test.
- **Success:** two consecutive self_improve cycles produce two distinct branches off current main and two distinct MRs (not one extended MR); neither branch is behind main at plan time; the label / title / tracking-issue are unchanged; a mid-cycle resume reattaches to that cycle's own `uzi/self-improve/<runId>` branch.

### M9 — Cap concurrent open self-improve MRs (server, D10 + D12 forge-first source)
- **Candidate query (DB):** new store query `RecentSelfImproveMRRunsForRepo` in `api/internal/store/queries/` returning the repo's most-recent M (const, e.g. 10) completed `self_improve` runs with `mr_iid IS NOT NULL` (`WHERE kind='self_improve' AND repo_id=$1 AND mr_iid IS NOT NULL ORDER BY created_at DESC LIMIT $2`), selecting `mr_iid`, `branch`, `plan_md`/`issue_description`; `sqlc generate` with the repo's currently-pinned sqlc (v1.31.1 as of #679; match `Taskfile.yml`/CI). **Not** a `mr_state='opened'` count (D12 — that column is unreliable for this lane).
- **Live open-state (forge):** in `fireSelfImprove` (`self_improve.go:59`), call the existing `GetMergeRequest(mr_iid)` per candidate, count those still `MRStateOpened`. No new `Forge` interface method.
- If the open count ≥ `selfImproveMaxOpenMRs` (named const, default **2**), skip: no run created, a `selfimprove_skipped` notification (mirror `self_improve.go:76`) whose **body** says "open-MR cap reached", and a no-run `FireOutcome` with skip (not failure) `last_fire` semantics. Below K, fire, and pass the still-open candidates' `plan_md` to M10's picker context.
- **Upgrade/transition:** the pre-existing long-lived `uzi/self-improve` MR (if still open at rollout) is a legitimate open MR and correctly occupies one cap slot until a human merges/closes it; call this out in M6 docs so the maintainer closes it once its work is superseded, rather than it silently holding a slot.
- **Success:** with K open self-improve MRs the next cycle creates no run and its notification **body** reads "open-MR cap reached" (assert the body, not the kind — the vault skip shares the kind, N6); below K it fires; M and the cap are named consts, not literals; proven by a scheduler test (fake forge returns K opened) **and** a live-DB candidate-query test that inserts self_improve runs with mixed `mr_iid`/repo/kind and asserts the candidate set is exactly the recent-completed-self_improve-with-mr_iid-this-repo rows (N1: include a NULL-`mr_iid` run, a `merged`/`closed` one via the fake forge, a wrong-`repo_id` run, and a non-`self_improve` run, so the query pins `kind`/`repo_id`/`mr_iid IS NOT NULL`, not a looser predicate).

### M10 — Independent pick: open-MR context to the picker (D11, builds on #297, source from D12)
- Server: at claim assembly for a self_improve run, take M9/D12's still-open candidates and carry their **`plan_md`/`issue_description`** (byte-capped like #297's list — that is the "what was proposed", from the DB run row, not a forge MR-body read) on the claim, alongside #297's `inflightTargets`.
- Worker: render them in the SAME untrusted nonce-fence pattern #297 uses — `inflightFrame` / `<inflight_work_…>` in **`agent/src/prompt.ts`** (`:1171-1177`, `:1248-1251`). (The `known_improve_uzi_targets` fence is a second instance of the same pattern but lives in `agent/src/judge-runner.ts:481-482` + `protocol.ts:670`, **not** `prompt.ts` — mirror the `inflightFrame` one here.) Extend the picker instruction with: "these self-improvement MRs are already open — pick an improvement that does not overlap them." Keep the trusted directive outside the fence.
- **Claim-wire note (N3, reconcile with D7/M3):** this adds a second `ClaimResponse` field. D7/M3 assert a new claim field "crosses the controller poll-wire contract" (golden bump), but review found the controller `PollResponse`/`controller_poll_wire.json` carries only fleet desired-state (workers/templates/join_token) — none of `issue_description`/`auto_approve`/`inflight_targets` traverse it — which suggests claim fields do **not** touch the controller golden. The implementer must settle this once for BOTH M3 and M10: if claim fields do not cross the controller contract, drop the golden-bump step from M3 too; if they do, M10 bumps it as well. Do not leave the two milestones asserting opposite things.
- **Success:** a self_improve plan prompt with an open self-improve MR present contains that MR's `plan_md` subject **inside** the matched-nonce fence plus the non-overlap instruction; absent ⇒ no such block (older server / no open MRs, defaults safe); a fence-invariant test proves the MR text is data, never trusted instructions — **and specifically an adversarial one (N2): a forged-close-tag payload in the MR content stays inside the fence and the real nonce ≠ the forged one, mirroring the existing memory/inflight breakout test in `agent/test/prompt.test.ts`, not merely a benign "renders inside a fence" assertion** (MR titles/plans are model-authored, hence worker-forgeable).

## Phasing & parallelization

Part A (generalize) and Part B (branch model) are largely **sequential per shared function**, not independently parallel — four hotspots collide, so order matters more than fan-out:

| Phase | Milestones | Depends on | Shared-file hotspots (sequence within a cell) |
|---|---|---|---|
| 1 — foundation | **M1** (schema/flag), **M8** (per-cycle branch) | — | M8 is agent-side (`self-improve.ts`/`runner.ts`) + a `self_improve.go` body reword; independent of M1's schema. Run in parallel. |
| 2 — server | **M2** (generic/fold), **M9** (cap + forge source), **M3** (claim signal) | M1 | **M2 and M9 both edit `fireSelfImprove`** → sequence M2→M9. M3 (claim assembly) is parallel to them. |
| 3 — worker | **M4** (directive/checks), **M10** (open-MR picker) | M3 (+M9 for M10's source) | **M4, M8, M10 all edit `buildSelfImprovePlanPrompt`** and **M3, M10 both add a `ClaimResponse` field** → M8's branch-line carve-out (B1) must land before/with M4; M10 after M4. |
| 4 — surface | **M5** (tests), **M7** (web toggle), **M6** (docs) | all code milestones | M5 after code; M7 depends M1's DTO; M6 last. |

The N3 claim-wire question (does a new `ClaimResponse` field cross the controller golden?) must be settled once in Phase 2/3 and applied consistently to M3 and M10.

## Success Criteria

1. A `self_improve` schedule on an **unflagged** repo produces a run whose **trusted directive and untrusted data both** target that project (no "uzi's own repository", no uzi standing rules, no `go test ./... in api/`), and folds no `improve_uzi` backlog.
2. **uzi's own self-improve is transparent across the upgrade — zero manual steps:** after the migration, the maintainer's schedule on `vtmocanu/uzi` still folds the backlog, marks it addressed, reuses the same tracking issue + label + title, **and** the worker still receives the uzi directive (byte-identical except the reworded branch sentence, B1). The **branch** deliberately changes (fresh-per-cycle, D9) — that is Part B, not a regression. Pinned by api case 1 + the agent dogfood(-except-branch) regression test.
3. The `improve_uzi` judge category is unchanged (no enum/DB/type churn).
4. The capability survives a repo delete+reconnect via the owner setter (D5).
5. `self_improve_dogfood` defaults to generic when absent (older server / unflagged repo) — a non-uzi repo can never receive the uzi directive.
6. Full gate green across api, agent, controller, and web, with the migration exercised by CI's live-DB job.
7. **No drift (Part B):** every self_improve cycle branches off current `main`; two consecutive cycles produce two independent branches/MRs — directly preventing the #699 stale-duplicate class. ("Neither is behind main at plan time" holds *by construction* — the runner clones the bare's current default tip per cycle (`runnerCloneForBranch`); it is not enforced by a dedicated gate, so no reader should expect a test for the distance itself. The tested pin is the per-cycle branch derivation, M8/M5.)
8. **Bounded (Part B):** at most K (default 2) open self-improve MRs per repo; at the cap the cycle is skipped with a notification, not piled onto an existing branch.
9. **Independent (Part B):** when a self-improve MR is open, the next cycle's plan prompt carries its subject inside the untrusted fence and instructs a non-overlapping pick; the label / title / tracking issue are unchanged by the branch switch.

## Risks & mitigations

- **Multiple repos, one owner:** self-improve is per-repo (one schedule per repo), so an owner can run it on uzi **and** N of their own repos at once; each schedule reads its own repo's flag; the fire path's active-run dedup is per-repo (`CountActiveSelfImproveRunsForRepo`, `selfimprove.sql:58`), so they never block each other. The `improve_uzi` backlog is **owner-scoped**, so the intended shape is **exactly one flagged repo** (uzi) + N generic; flagging two `fold=true` would make them share (and race to mark-addressed) the single owner backlog — document "flag exactly one repo for the fold."
- **The wire field is the delicate part.** It crosses the controller poll-wire contract; the `-count=1` cross-module golden (`protocol_contract_test.go`) is what keeps the two modules' views of the claim from drifting — do not run that gate with a warm cache that could serve `(cached)`. Default-absent-⇒-generic is the safety net if any layer forgets to populate it.
- **Existing non-uzi self-hoster:** the backfill sets `fold=true` for any pre-existing self_improve schedule, including a non-uzi one — preserving today's (uzi-shaped) behavior for it rather than flipping it to generic on upgrade. This is the intended back-compat stance (no behavior change on upgrade); such a user flips the flag off to go generic. Documented, not a break. Because today's behavior on a non-uzi repo is actually *wrong* (uzi directive on their repo), the honest framing is "preserve the existing, imperfect behavior; new enables are correct."
- **Notification test is NOT gate-forcing (first-draft error).** `notifier_notify_test.go` `TestNotificationBlocksServerProseUnchanged` (`:242-268`) tests only the renderer against its own literal slice; changing `self_improve.go:143` does not redden it. Its comment cites the deleted `engine.go`. Treat updating it as **hygiene**, and pin the *real* behavior (repo-named notification) via the scheduler test's `fakeNotifier` (M5).
- **Migration number collision** — draft `00164` is collision-avoidance only; renumber above the live head at landing (strict goose refuses a below-head version at boot).
- **sqlc inference** — a plain boolean column infers cleanly; confirm `GetRepoForUserRow.FoldImproveUziBacklog bool` and run the live-DB job so Postgres executes the amended query.

## Out of scope

- Renaming the `improve_uzi` judge category or any of its plumbing.
- Renaming the `uzi-self-improve` label or `uzi self-improvement` title. (The **branch** IS changed — fresh-per-cycle, D9/M8 — but the label and title are kept.)
- Auto-resolving merge conflicts between two independent open self-improve MRs. Independence is at the *pick* (D11); a git-level conflict between two open MRs is the human's merge-order call, never auto-resolved by the worker (consistent with ADR-456's fail-typed-and-preserve stance).
- Changing the branch model of any other run lane (issue / ci_fix / prompt). Those branch per **issue** (`agent/issue-<iid>`, reused only across runs/resumes on the same issue) or per **run** (`uzi/prompt-<runId>`) off a fresh clone base; only the self_improve lane carried a single long-lived accreting branch. ("Per run" was imprecise — an issue branch is reused across a resume, but it is bounded to one issue and never accretes indefinitely the way the fixed self_improve branch did.)
- A `uzi repo` CLI setter for the flag (sibling per-repo flags have no CLI today; parity is satisfied without one).
