# PRD #65: Forgejo support — the second forge driver

**GitLab Issue**: [#65](https://gitlab.example.com/vtmocanu/uzi/-/issues/65)
**Status**: Draft (created 2026-07-17; designed by the architect agent, then revised twice the same day across two review waves — reviewer + security audit + fact-check, then a second wave on the revision. The security audit invalidated D6's original mapping; the fact-checks refuted 8 claims, two of which the PRD author had personally asserted as verified; the guardrail-enforcement work was split out to #66 mid-session.)
**Priority**: Medium
**Depends on**: no uzi PRD. **One external, human-owned dependency: the self-hosted validation instance must be upgraded to Forgejo ≥16.0.0** (currently 15.0.4) before M9 can validate live — see R2.
**Spawned**: [PRD #66](66-guardrail-enforcement.md) — refuse runs when the bot can push/merge to `main`. Discovered here (a Forgejo `write` bot can merge its own PR by default), split out because it is a **GitLab** behaviour change with no Forgejo content. **#65 lands the fields and findings; #66 consumes them.**
**Touches the contracts of**: PRD #5 (privilege checks — this PRD adds fields + findings, changes no tiers), PRD #6 (CI status/fix), PRD #19 (autopilot attribution), PRD #24 (MR-close watcher), PRD #42 (worker claim wire contract).
**Related**: PRD #2 (forge integration — established the seam this PRD tests).

**This PRD changes no existing behaviour.** Every milestone lands dark: until M6b
flips the handler gate, `forgejo` is unreachable, and nothing here alters a GitLab
connection's verdicts, tiers, or runs.

## Problem

uzi has always claimed a forge-generic design with "GitLab first, Forgejo later"
(`specs/human.md:40,396`). The interface exists, the `forge_type` column exists,
and `gitlab.go` is the only driver. Forgejo never landed.

It was deferred on a recorded blocker — and **that blocker is false**:

> `specs/ai.md:259-262`: "Forgejo has no atomic add+remove label call, so
> `UpdateIssueLabels` would be non-atomic and single-column enforcement
> best-effort there."

Forgejo has `PUT /repos/{owner}/{repo}/issues/{index}/labels`, whose handler runs
its diff inside a database transaction (`models/issues/issue_label.go:447`,
`db.TxContext`; route `routers/api/v1/api.go:1139`). Single-column enforcement is
**atomic** on Forgejo. A wrong doc has been arguing against feasible work for
months — exactly the cost CLAUDE.md's doc-correction rule exists to prevent.
Correcting it is in scope (M10), and it is why this PRD exists now.

Three things are true that the specs do not record, and all are real work:

1. **The worker holds a second, unabstracted GitLab client.** `agent/src/gitlab.ts`
   (194 lines) hardcodes `/api/v4/projects/…/merge_requests` (`:82`) and
   `PRIVATE-TOKEN` (`:125`). The forge abstraction is Go-side only; the worker opens
   the MR itself and cannot know which forge it is talking to (`ClaimRepo` carries no
   `forge_type`; `runner.ts:304-305` string-parses the web URL).
2. **The web reconstructs forge URLs by string surgery.** `web/src/lib/forgeUrls.ts`
   ("Pure GitLab web-URL helpers (GitLab-only product)") rebuilds
   `/-/merge_requests/N` from the issue URL — because the DB stores `mr_iid` but not
   the MR web URL, which the driver already receives (`gitlab.go:468`) and discards.
3. **uzi checks merge permission on neither forge.** `forge.BranchProtection` models
   push only. On GitLab that is safe by platform default; **on Forgejo it is not**
   (D6a). The "an agent can only ever open an MR" guarantee is currently delegated to
   `docs/gitlab-bot-setup.md:46`, and that sentence becomes false on Forgejo.

## Solution Overview

A `forgejo` driver behind the existing `Forge` interface, at **full parity** with
GitLab: board sync, runs, MR creation and watching, privilege guardrails, and the
CI-fix loop. The Go call sites do not change — `ForgeForConnection` already threads
`forge_type` through every one of them. The work is the driver, plus the three
never-abstracted layers above (worker, web, merge checking).

## Out of Scope

- No third driver, and no plugin/registry mechanism. Two drivers is not a pattern.
- No Forgejo exclusive scoped labels (D3) — the board's label contract stays
  identical across forges.
- No renaming of `MergeRequest`/`mr_iid`/`IID` in Go, the DB, or the REST API (D2).
- No support for Forgejo < 16.0.0 (D4). No degradation path, by decision.
- No Gitea support. Forgejo is the target; gitea-sdk (D5) is an implementation
  detail, not a compatibility promise.
- uzi never **creates** branch protection (D6/D6a) — that needs admin, which D6
  forbids, and would hand the bot the power to delete the rule too.
- No blocking on principals other than the bot's PAT (D6) — a write **deploy key** can
  push to protected `main` and D6a cannot see it. uzi provisions none.

## Design Decisions

### D1 — Full parity, or the forge is not supported

A Forgejo user gets the same product as a GitLab user: same board, same guardrails,
same CI-fix loop. No "GitLab-only" asterisks in the UI. The interface audit found no
method Forgejo cannot satisfy.

### D2 — Terminology: `MergeRequest` stays internal; UI copy is per-forge

Go types, DB columns (`mr_iid`, `mr_state`), and the REST API keep `MergeRequest`
and `IID` — no migration, no churn. Only user-visible strings switch: a Forgejo card
says "Pull Request", a GitLab card says "Merge Request".

The domain type is already *structurally* neutral: Forgejo's PR `index` is exactly
GitLab's `iid` (a per-project sequential number), so `IID` carries over honestly.
Only its *name* is GitLab-flavoured, and a name is not a contract.

**The architect recommended against per-forge copy and was overruled (user, 2026-07-17).**
Its objection is real and is recorded rather than dropped: **uzi boards are
mixed-forge**, so one board can say "Merge Request" on one card and "Pull Request" on
the next. Two consequences M8 owns:

- **`forge_type` must reach the web, per card/run** — the architect's design concluded
  the web needs no `forge_type` at all, and D2 makes that false. **But the cost is
  lower than first estimated**: the queries **already** `SELECT c.forge_type`
  (`runtime.sql:247`, `forge.sql:85,134`) and `api.ts:216` already carries
  `ForgeConnection.forge_type` (Settings renders it as a badge today). So this is a
  **DTO field addition, not a query change**.
  - **One `forgeNoun(forgeType)` helper is the only mapping site.** Do not scatter
    ternaries across ~8 components. Acceptance: `grep -r '"Merge Request"' web/src`
    returns exactly one non-test hit.
  - **`slacksvc/notifier.go` needs the same mapping in Go** — an unavoidable second
    copy across the language boundary. Keep them adjacent in review.
  - The **pipeline badge stays forge-blind**: one merged status map, no per-forge
    drift. `forge_type` reaching the web does not license keying the badge off it.
- **Shared UI has no forge to key off, and the obvious default is a trap.** An earlier
  draft of this PRD wrote "shared chrome says 'merge request'". The architect's
  pressure-test killed it: if "neutral" silently means *merge request*, a Forgejo-only
  user reads "Pull Request" on every card and "merge request" in the chrome — **that
  is GitLab's term wearing a neutral label**, reintroducing exactly what D2 paid to
  remove. **Rule: name both ("merge request / pull request") or restructure the
  sentence past the noun. Never default to one forge's word.**
  - The problem is **much smaller than it looks**: nearly everything is repo- or
    connection-scoped, so per-forge just works — including Settings, already
    per-connection (`ForgeSettings.tsx:241` maps `connections.map(c => …)`). The
    genuinely forge-less surfaces are only **nav/app-shell, admin, and `docs/`**.

Rejected: renaming to a neutral third term across Go + DB + API + UI — a data
migration on live rows plus the worker wire contract and the REST surface, for zero
behavioural gain.

### D3 — Label writes: accept the lost-update window, document it

Forgejo's label API is a **full-set replace**, not GitLab's `add_labels`/
`remove_labels` delta. The single PUT is atomic server-side, but uzi must compute the
target set client-side (`current − remove + add`), and that read happens outside the
transaction. If a human adds an unrelated label in the ~1 RTT window, uzi's PUT
silently drops it. There is no ETag/If-Match to close it.

Accepted: narrow window, rare event, and the next forge sync self-corrects the cache
(though not the human's lost label). Mitigations: read immediately before the PUT;
**no-op entirely when the target set already equals the current set** (a card move
that changes nothing must issue zero PUTs); document in `specs/ai.md` and
`docs/forgejo-bot-setup.md`.

**The single-column board invariant is NOT weakened** — that was the false blocker.
It is atomic on both forges, by different mechanisms.

**The replace semantics were read from source and never executed.** The architect
flagged this as its own riskiest assumption and it survives as R3: M2 must run the
empirical probe (create issue → add unrelated label → PUT → assert the unrelated
label's fate) before `UpdateIssueLabels` is considered done. D4 forces a ≥16 instance
anyway.

Rejected: **Forgejo exclusive scoped labels** (`models/issues/label.go:89`,
`Exclusive bool`), which would enforce single-column server-side with no
read-modify-write and no lost update. They fork the board's label contract per forge
and have no free GitLab equivalent (GitLab scoped labels are Premium). Recorded as
the escape hatch if collateral label loss proves painful.

### D4 — Forgejo ≥ 16.0.0 required; refuse to connect below it

The CI-fix loop needs job logs. `GET /repos/{o}/{r}/actions/jobs/{job_id}/logs`
(`text/plain`, the exact analogue of GitLab's job trace — swagger `produces:
text/plain`, handler `routers/api/v1/repo/action.go:1539`) **first ships in Forgejo
v16.0.0, released 2026-07-16** (tag commit 08:06:16 +0200; Codeberg
`published_at: 2026-07-16T11:03:41+02:00`).

Verified by route table at both tags — **note the precise claim, an earlier draft of
this PRD overstated it**:

| | v15.0.4 | v16.0.0 |
|---|---|---|
| `actions/runs` listing | **present** (`api.go:888-893`) | present |
| `runs/{run_id}/jobs` (`ListActionRunJobs`) | **absent** | present |
| `actions/jobs/{job_id}/logs` (`GetActionJobLogs`) | **absent** | present (`api.go:891`) |

So `LatestPipeline` would in fact work on 15.x; only `ListPipelineJobs` and
`JobLogTail` — the CI-**fix** loop — do not. The ≥16 requirement is real but narrower
than "no Actions API at 15.x".

uzi **version-gates at connect** and refuses Forgejo < 16.0.0 with an error naming
the required version. No `ErrCIUnsupported` sentinel, no degraded mode, no version
branches in the driver.

- **The gate lives in `VerifyToken`, not driver construction.** `CreateConnection`
  surfaces `VerifyToken`'s error verbatim (`handler/forge.go:183`) but collapses
  driver-construction errors to "could not initialize forge client" (`:178`) — so a
  gate in `New()` would never show the user the version. One `/version` call per
  `VerifyToken`, not per driver construction (D5's W1 builds a client per call).
- **Re-check on the privilege sweep**, which already walks every connection
  periodically. An instance that connects at 16.x and later downgrades otherwise hits
  a bare 404 with no defined behaviour. The sweep re-asserts the version and raises a
  violation.
- **The version gate is a feature gate, never a security control** (L2).
  `GET /api/v1/version` is public, unauthenticated (`api.go:545-547`), and
  self-reported — a hostile instance can claim anything. Spoofing up buys a broken
  CI-fix loop; spoofing down buys a refusal. **Nothing security-relevant may ever be
  hung on it.** This sentence exists so a future reader does not mistake a passed
  version check for a trust signal.

**User decision (2026-07-17), overruling the architect**, which recommended a 15.x
floor with 404 → `ErrCIUnsupported` degradation.

- **Cost**: excludes every Forgejo instance older than 2026-07-16 — today nearly all
  of them, **including the self-hosted instance earmarked for validating this PRD,
  which runs 15.0.4**.
- **Benefit**: D1 becomes a guarantee, not a per-instance maybe. No degradation
  branch to write, test, or explain.
- **Consequence**: the self-hosted validation instance must be upgraded before uzi can connect to it at all.
  On M9's critical path (R2).

### D5 — Client library: `code.gitea.io/sdk/gitea`, not `forgejo-sdk`

`codeberg.org/mvdkleijn/forgejo-sdk` v2.2.0 is the latest and was published
2025-06-17 — **exactly 13 months stale**. It has no `issue_timeline.go` at all, and
its Actions support is **secrets only** (`repo_action.go` / `org_action.go` carry
`ListRepoActionSecret`, `CreateRepoActionSecret`, …; zero runs/jobs/logs). Both gaps
are things uzi needs, so they would be hand-rolled anyway.

`code.gitea.io/sdk/gitea` v0.25.1 (published 2026-05-12, ~2 months) has
`ListIssueTimeline` (`issue_timeline.go:34`) **and** the full Actions surface
including `GetRepoActionJobLogs` (`action_run.go:240` →
`/repos/%s/%s/actions/jobs/%d/logs`), `ListRepoActionRuns`, `GetRepoActionRun`,
`ListRepoActionRunJobs`. **So `JobLogTail` needs no hand-roll** — an earlier draft
assumed otherwise. Forgejo still ships the `+gitea-1.22.0` compatibility suffix
precisely for this (verified live on both the validation instance and codeberg.org).

One SDK wart the coder must handle: **client-scoped `context`** — `c.ctx` is set on
the client, not per request, so a long-lived shared client leaks cancellation across
requests. Mitigation: construct a client per call over a shared `*http.Client`, and
`SetGiteaVersion("")` to skip `NewClient`'s `/version` round-trip. (An earlier draft
also claimed a data-race risk; that is **false** — gitea-sdk guards `c.ctx` with a
mutex on both sides (`client.go:203-206, 301-309`) and documents its methods as
concurrency-safe. Cross-request cancellation is the only real hazard.)

One deliberate SDK bypass: **`TokenInfo` must be hand-rolled.** The SDK's
`ListAccessTokens` errors `"username" not set: only BasicAuth allowed`
(`user_app.go:89-91`) — a **client-side** gate. The server requires no basic auth for
the GET: the route group carries `reqSelfOrAdmin()` only (`api.go:597`), with
`reqBasicOrRevProxyAuth()` on POST (`:595`) and DELETE (`:596`) only.

### D6 — Guardrails: full equivalent, or refuse to connect

Every PRD #5 check maps to Forgejo or the connection is refused. No check is silently
weakened on the theory another layer covers it.

**The original mapping in this PRD was wrong and unsafe. The security audit found
that three of its six rows read `GET /repos/{o}/{r}/branch_protections/{name}`, whose
entire route group is gated `reqAdmin()`** (`api.go:867`; `req_admin.go:20-22`
requires repo-admin or site-admin) — **identical at v15.0.4 (`:872`), so not a v16
regression**. A bot at exactly `write` (which row 3 *requires*) gets **403 on every
one of them**, and `privcheck/checker.go:135-139` converts that error into a
**warning**, not a violation. As originally written, the protected-branch guardrail
would have degraded to "could not read default-branch protection" on every Forgejo
repo, forever — precisely what D6 forbids.

The corrected mapping is **better than GitLab's**, not a workaround:

| PRD #5 check | GitLab | Forgejo (corrected) |
|---|---|---|
| Bot is not an instance admin | `is_admin` on `GET /user` | same (D6d) |
| Token scopes | exactly `{api}` | exactly `{write:repository, write:issue, read:user}` (D6b) |
| Bot at exactly write level | role == 30 | `permission == "write"` (D7) |
| Default branch protected | `protected_branches/:name` (404 → unprotected) | **`GET /branches/{branch}` → `protected`** |
| Bot cannot push to it | inferred from push access levels | **`user_can_push == false`** |
| Bot has no direct push grant | per-user allow-to-push entry | **subsumed by `user_can_push`** |
| **Bot cannot merge to it** (new field, D6a-1) | `merge_access_levels` excludes Developer | **`user_can_merge == false`** |
| No unprotected-file bypass (D6e) | n/a | `unprotected_file_patterns` is empty |

`GET /repos/{o}/{r}/branches/{branch}` sits under `reqRepoReader(unit.TypeCode)`
(`api.go:852-858`) — readable by a `write` bot — and returns `protected`,
`user_can_push`, `user_can_merge` (`modules/structs/repo_branch.go:11-21`), **computed
for the calling bot by the same code path the pre-receive hook enforces**
(`services/convert/convert.go:107-108` → `bp.CanUserPush` /
`IsUserMergeWhitelisted`). That is a *direct authoritative answer* to "can this bot
push to main", rather than GitLab's inference from access levels. It collapses three
rows into one read and moots an entire class of rule-matching bugs: the
`branch_protections/{name}` lookup is an exact-name DB equality
(`protected_branch.go:284-292`) while enforcement takes the first match over all
rules, plain-names-first, case-insensitively (`protected_branch_list.go:18-36`) — so
a `main` protected by a glob rule reads as unprotected, and a permissive `MAIN` rule
created before a restrictive `main` is the one actually enforced while uzi reads the
other. `user_can_push` runs the real path.

`effective_branch_protection_name` is admin-only (`convert.go:97-99`) — uzi does not
need the rule's name.

Forgejo's **team**-whitelist gap mirrors the group-grant gap GitLab already documents
for manual audit — no new risk, same documented limitation.

**Scope the claim precisely: D6a covers the bot principal, not every principal.**
`user_can_push`/`user_can_merge` describe *the calling bot*. A write **deploy key**
can push to protected `main` invisibly to D6a (`WhitelistDeployKeys`,
`models/git/protected_branch.go:45`; the hook admits deploy keys when
`CanPush && (!EnableWhitelist || WhitelistDeployKeys)`). uzi provisions no deploy
keys, so this does not break "the bot cannot touch `main`" — but the docs must say
"the bot's PAT cannot", not "nothing can".

**Forgejo is stricter than GitLab in two places**, worth knowing and not claimed
above: deletion and force-push of a protected branch are refused unconditionally in
the pre-receive hook, before any role check and with no admin bypass
(`hook_pre_receive.go:288-313`); `ApplyToAdmins` affects only the merge path and
protected-file patterns, never direct push (`:460,:472`). A repo admin cannot bypass
protection to direct-push on Forgejo.

#### D6a — Detect that the bot can merge. **Enforcement is PRD #66.**

**This PRD reports; it does not refuse.** Findings stay at PRD #5's existing
warn-don't-block tier, so #65 changes **no** GitLab behaviour and lands dark.

That was not the original plan, and the history is worth keeping. The security audit
found that a Forgejo `write` bot can merge its own PR into protected `main` by
default (D6a-1), which means uzi's four-layer guarantee runs on three layers there.
The user's first instinct was to block, and they confirmed it twice as the
consequences surfaced — including that "block when the bot can merge" *necessarily*
blocks an unprotected `main` on **both** forges, since an unprotected branch means
the bot can push and merge (Forgejo: `services/convert/convert.go:76-85` returns
`UserCanPush: canPush, UserCanMerge: hasPerm`, both true for a write bot; GitLab
places no restriction on an unprotected branch either).

Then the architect surfaced the packaging problem, and the user split it out
(2026-07-17): **blocking is a GitLab behaviour change that has nothing to do with
Forgejo.** A user upgrading to get a Forgejo driver should not find their working
GitLab repos refused, announced as a footnote. It also has a real mechanism behind it
— three PAT-bearing run inserts, a live check, fail-closed evaluation, coded findings
— which deserves its own pre-flight impact count, release note, and rollout.

**So #65 lands the field, the check, and the finding. [PRD #66](66-guardrail-enforcement.md)
flips it to blocking.** The split is what lets #65 keep its dark-landing property:
nothing here refuses anything, on either forge.

#### D6a-1 — `WriteRoleCanMerge` + `BotCanMerge`, on both drivers (the new fields)

**On a default-configured Forgejo, a `write` bot can merge its own PR into protected
`main`.** `IsUserMergeWhitelisted` falls straight back to write permission when the
merge whitelist is off — `if !protectBranch.EnableMergeWhitelist { return
permissionInRepo.CanWrite(unit.TypeCode) }` (`models/git/protected_branch.go:155-157`)
— and `EnableMergeWhitelist` defaults to **false** (`:43`), `RequiredApprovals` to 0
(`:52`), `EnableStatusCheck` to false (`:47`). `POST /repos/{o}/{r}/pulls/{index}/merge`
is gated by `reqToken()` only (`api.go:983-984`), and the pre-receive hook explicitly
permits the path (`hook_pre_receive.go:415-445`).

GitLab's default forbids it, though **an earlier draft of this PRD quoted the wrong
default and stamped it "verified"** — the third instance of this PRD's own
methodological failure, caught by the Fable review wave. The correct fact: GitLab's
**initial default branch protection is "Fully protected"** — *"Developers cannot push
new commits, but maintainers can. No one can force push."* (GitLab docs
`repository/branches/default.html`, verified 2026-07-17). That level sets **both**
push and merge access to Maintainer, so a Developer-role bot can do neither. The
superseded quote ("No one can merge / No one can push") described the defaults for
*manually* protecting an arbitrary branch — a different setting that says nothing
about new projects.

uzi models merge on **neither** forge — `forge.BranchProtection` has no merge field —
and delegates the guarantee to `docs/gitlab-bot-setup.md:46` ("the bot … can never
merge or push there itself: the platform-enforced half of uzi's 'an agent can only
ever open an MR' guarantee"). That sentence is true on GitLab by luck of defaults and
**false on Forgejo**.

**Decision (user, 2026-07-17): add `BranchProtection.WriteRoleCanMerge` and
`BotCanMerge`, and check them on both drivers.** On Forgejo both are free
(`user_can_merge` arrives with `user_can_push`). On GitLab they read
`merge_access_levels` — which defaults to Maintainer, but **is configurable, and
"safe by default" is not "safe"**. In #65 the finding is reported like any other;
PRD #66 makes it block.

#### D6b — Per-forge required token scope set

`privcheck/checker.go:20` hardcodes `requiredScope = "api"` and
`scopesEqualRequired` demands exactly one scope (`:197-204`). Forgejo has no `api`
scope — its model is category-based (`models/auth/access_token_scope.go:56-84`). As
originally specced, **every Forgejo connection would be rejected at save**, since
PRD #5 makes token-level violations blocking (`prds/done/5-…:84`).

**Decision (user, 2026-07-17): per-forge required set, keeping "exactly" semantics.**
GitLab: exactly `{api}`. Forgejo: exactly `{write:repository, write:issue,
read:user}` (scope names at `models/auth/access_token_scope.go:81,78,83`) — PRs live
under `CategoryRepository` (the repo group closes with it at `api.go:1083`), issues
under `CategoryIssue` (`api.go:1206`), and introspection needs `CategoryUser`
(`api.go:601`).

**Verified sufficient *and* minimal** (Fable audit + fact-check, independently):
Actions runs/jobs/logs sit **inside** the repo group (`api.go:883`, within the group
ending `:1083`), so the CI-fix loop needs **no extra Actions scope**; git push over
HTTPS requires `write:repository` (`routers/web/repo/githttp.go:156`), so the same
scope covers the worker's branch push; the only dynamic owner-scope route is
`DELETE` repo (`api.go:771`), which uzi never calls. None of the three is spare, and
no fourth is needed.

**The scope wire format — raised by the Fable review as an unverified premise that
could have sunk the exact-set check, then settled from source. The check is
implementable, with two traps:**

Forgejo **normalizes at mint time**, not at read time: `Normalize()` runs on token
creation via both the API (`routers/api/v1/utils/access_token.go:154`) and the UI
(`routers/web/user/setting/access_token.go:265`). Stored scopes are therefore already
canonical, and `AccessToken.Scopes` is just
`strings.Split(string(s), ",")` (`access_token_scope.go:268-270`).

Crucially, `toScope()` **collapses, it does not expand**
(`access_token_scope.go:325-352`): it walks `allAccessTokenScopes` and emits a scope
only when it contributes new bits — *"if the reconstructed bitmap doesn't change,
then the scope is already included"*. So `write:repository` **never** drags an
explicit `read:repository` into the returned list. The feared expansion does not
happen.

1. **Compare as an unordered set, never as a string.** `toScope()` re-emits in
   `allAccessTokenScopes` order, not mint order, so a string equality check against
   `"write:repository,write:issue,read:user"` is a coin flip on ordering.
2. **`all` is a literal, produced by string substitution.** When every `write:*` scope
   is present, `toScope()` rewrites the whole run to the single token `all`
   (`:346-351`). So the god-mode token D6b rejects arrives as `["all"]` — the rejection
   must match that string, not attempt to expand it.

Rejected: requiring Forgejo's `all` scope (god-mode; throws away the least-privilege
win Forgejo's finer scopes offer) and accepting any covering superset (deletes
PRD #5's only blocking token check, which exists to stop an over-scoped token sliding
in). M3 owns the per-forge rule.

#### D6c — Unprotected default branch: warn, don't block; document loudly

GitLab auto-protects the default branch ("Fully protected"). **Forgejo creates repos
with zero protection rules**, so PRD #5's per-repo warn-don't-block policy
(`prds/done/5-…:26`) — calibrated to GitLab, where unprotected main is the exception —
meets a forge where it is the default. The common Forgejo path is "runs happily, main
unprotected, one badge".

**Decision (user, 2026-07-17): keep warn-not-block in this PRD.** This decision was
briefly superseded by a blocking rule and then **restored when enforcement was split
into PRD #66** — the supersession is recorded in D6a rather than erased, because a
future reader will otherwise re-derive the same conflict. Blocking is #66's job; #65
warns.

The **documentation** obligation is where #65 carries its weight:
**`docs/forgejo-bot-setup.md` leads with "protect `main` and enable the merge
whitelist — uzi will not do it for you."** Forgejo ships neither by default, so this
is the first thing a Forgejo user must do, not a footnote. Once #66 lands it becomes
"uzi will refuse to run until you do"; M10 should write the sentence so that upgrade
is a one-word edit.

Honest about the posture in the meantime: **on default-configured Forgejo, layer 1 is
user-supplied.** Layers 2-4 hold regardless — uzi has no merge code path
(`createMr`/`findOpenMr` are the worker's only forge calls), the agent never holds
the PAT, and the deny-hook blocks `git push`.

Rejected: having uzi create the protection rule itself (needs admin, which D6 forbids,
and would hand the bot the power to `DELETE` the rule via the same admin-gated route
group — strictly worse).

#### D6d — `is_admin` holds, for a different reason than on GitLab

`GET /user` → `ToUser(ctx, ctx.Doer(), ctx.Doer())` (`routers/api/v1/user/user.go:152`)
→ `authed = doer.ID == user.ID` → true → real `IsAdmin` (`services/convert/user.go:24,76-78`).
Unlike GitLab, Forgejo has no `omitempty`, so `is_admin: false` is always emitted and
*means* non-admin. GitLab's absent-means-pass reasoning (PRD #5's risk section) does
not transfer, but the conclusion does.

#### D6e — `unprotected_file_patterns` bypass

If any rule sets `unprotected_file_patterns` (e.g. `*.md`), the bot's PAT can push
directly to `main` for commits touching only those files, despite `enable_push=false`
(`hook_pre_receive.go:391-406`). No GitLab equivalent. `user_can_push` reflects the
rule but not this per-commit override, so **check the pattern list is empty
explicitly**; non-empty is a violation.

### D7 — Interface: `Role` enum, and it is NOT contained in `api/`

**`ProjectRole` must return a `Role` enum, not `role int`.** Today it returns a raw
GitLab access level, and `privcheck/checker.go:16` hardcodes `developerAccess = 30`,
compares `role > 30` (`:124`) / `role < 30` (`:126`), and renders **"(30)" into
user-facing violation copy** (`:125,127`). Forgejo has no numeric levels — the API
surface returns `permission ∈ none|read|write|admin|owner`
(`models/perm/access_mode.go:26-39`). Mapping `write` → `30` would be a driver lying
to satisfy a GitLab-shaped contract: exactly the leak the forge package exists to
prevent.

```go
type Role string
const (
    RoleNone Role = "none"; RoleRead Role = "read"; RoleWrite Role = "write"
    RoleAdmin Role = "admin"; RoleOwner Role = "owner"
)
ProjectRole(ctx, projectID, forgeUserID int64) (role Role, member bool, err error)
```

GitLab maps 30 → `RoleWrite`, >30 → `RoleAdmin`/`RoleOwner`, <30 → `RoleRead`;
`privcheck` asserts `role == RoleWrite` and drops the numeral. Also
`BranchProtection.DevelopersCanPush` → `WriteRoleCanPush` ("Developers" is a GitLab
role name in a neutral type).

**An earlier draft claimed this is "contained in `api/`". That is false**, and the
review caught two consequences:

- **The role is persisted and typed in the web.** `privcheck.RepoReport.Role` is
  `int` with `json:"role"` (`privcheck/report.go:54`), stored as JSONB
  (`00030_privilege_report.sql:15`, `forge_connections.privilege_report`) and typed
  `role: number` in `web/src/lib/api.ts:201` (+ `mocks/data.ts:328-329`). The REST
  field type changes `number` → `string`. **M8 owns the web side**; M3 owns the Go
  side.
- **Existing rows fail to unmarshal, silently.** `handler/forge.go:72-75` does
  `if err := json.Unmarshal(c.PrivilegeReport, &rep); err == nil` — **the error is
  discarded**. Every existing row holds `"role": 30` (a number); against a `Role`
  string field the unmarshal fails and the report vanishes from
  `GET /api/forge/connections` with no signal. **Accepted**: reports blank until the
  next privilege sweep re-stamps them (name the sweep interval in M3's commit
  message and `docs/`). **M3 additionally logs the unmarshal error** rather than
  discarding it — that wart predates this PRD and would hide the very symptom.
  The connections themselves are unaffected: `ProjectRole` is a live call, not
  stored data.

**`member` needs a Forgejo-specific derivation — 404 does not mean "not a member".**
`GET /repos/{o}/{r}/collaborators/{c}/permission` 404s only when the *user* does not
exist (`routers/api/v1/repo/collaborators.go:285-293`); a bot **removed from a public
repo** gets 200 with `permission: "read"` — `GetUserRepoPermission` → `accessLevel`
(`models/perm/access/repo_permission.go:255`) → `AccessModeRead` for any
non-restricted user on a public repo with no access row
(`models/perm/access/access.go:44-46,59-61`).
So PRD #5's "bot is no longer a Developer member; sync is broken" finding
(`checker.go:123`) would never fire on Forgejo public repos. It coincidentally works
for private repos (the repo middleware 404s the route). Fails safe-ish (`role < write`
raises a different violation) but the mapping is wrong — the driver must derive
`member` from the permission payload, not the status code. Two useful facts: the bot
querying **itself** is allowed (`collaborators.go:299`), and the endpoint deliberately
ignores fine-grained token scopes (`:303-306`), so `permission` is the *user's* role —
what uzi wants. The bot cannot introspect *other* users' roles (D6 forbids admin);
`privcheck` never needs to.

### D8 — Persist the MR web URL; stop guessing forge URL grammar

`forge.MergeRequest` already carries `WebURL` (`forge.go:148`), the driver populates
it (`gitlab.go:468`), and uzi discards it — the only consumer, `forgesvc/mr_watch.go`,
reads `.State` (`:59`) and never `.WebURL`. That is the sole reason
`web/src/lib/forgeUrls.ts` exists to rebuild `/-/merge_requests/N` by string surgery.

Add `runs.mr_web_url` (nullable) and render it directly, rather than teaching that
file a second URL grammar (`/{owner}/{repo}/pulls/{n}`). Restores "the forge is the
source of truth": uzi should not guess a URL the forge already told it.

- **Write path: the worker, at MR creation** (M7). `agent/src/gitlab.ts` already
  returns `{iid, webUrl}`; the completion payload carries only `mr_iid`
  (`protocol.ts:607` → `workersvc/service.go:958` → `runtime.sql:332`). **This is a
  second worker wire-contract change** — additive and optional, same shape as D9's —
  and **R8 covers both**. Rejected: populating from `mr_watch.go` (which already has
  the URL) — the link would appear only after the first watch tick and only for MRs
  parked in Human Review.
- **No backfill.** Old rows fall back to the existing `forgeUrls.ts` reconstruction,
  which **survives as the GitLab-only legacy path** — an earlier draft said this
  "deletes the file's reason to exist" and left an unresolved "or render a plain `!N`
  chip". It does not delete the file, and the fallback is the reconstruction.
- **`isHttpsUrl` must survive.** The guard lives *inside* `forgeUrls.ts`
  (`mergeRequestUrl`, `:20-22`). D8 widens forge control over the rendered URL from
  origin+prefix to the whole URL including query/fragment. Still https-only, so no
  `javascript:`/`data:` XSS (`isHttpsUrl` is `startsWith("https://")`,
  `api.ts:985-987`), and this is not a new class — uzi already persists and renders
  forge-supplied `issues.web_url`, `repos.web_url`, `pipeline_web_url` behind the same
  guard. **M8 must route `mr_web_url` through `isHttpsUrl` before it becomes an
  anchor.**

### D9 — Worker: a minimal TS forge seam, not a port of the Go interface

`agent/src/gitlab.ts` → `agent/src/forge.ts` with a small `ForgeClient` interface and
`GitLabClient` / `ForgejoClient`. **Only `createMr` is genuinely needed.** Do not port
the 19-method Go `Forge` interface into TypeScript.

`ClaimRepo` gains `forge_type?: "gitlab" | "forgejo"` — **additive and optional,
absent → `"gitlab"`** — so an old worker against a new api keeps working. Pinned by
`workersvc/claim_wire_contract_test.go`.

**Three transport guards are `ForgeClient` interface requirements, not
per-implementation details** — a fresh `ForgejoClient` that forgets them is a
plausible outcome:

- **Refuse a non-https base URL** before sending the PAT (`gitlab.ts:122`).
- **`redirect: "error"`** so a 3xx cannot replay the PAT cross-origin (`gitlab.ts:135`).
- **409-on-duplicate → fetch and return the existing MR** (`gitlab.ts:72-73,97-98`).
  This is what keeps a *resumed* finish step from dead-ending, so it fails only under
  retry. **Forgejo also returns 409** (`routers/api/v1/repo/pull.go:773-774`,
  `ErrPullRequestAlreadyExists`) — verified from source, so this is no longer an open
  question. **Caveat for M7**: Forgejo's 409 also covers other conflicts (`:780,:976`),
  so the fetch-existing fallback must tolerate finding no open PR. The GitLab code
  already does; the Forgejo one must too.

**Subpath-hosted Forgejo**: `gitlabBaseUrl` returns `${protocol}//${host}`
(`gitlab.ts:146-149`), discarding any `ROOT_URL` subpath — so a Forgejo at
`https://example.com/git/` yields base `https://example.com` and a project path of
`git/owner/repo`. Correctness, not security (same host, no PAT leak), but subpath
hosting is far more common on Forgejo than GitLab. M7 fixes the derivation.

**`agent/src/git.ts` needs no behavioural change** — verified twice, independently.
Forgejo's `tokenFromAuthorizationBasic` returns the **password** as the token and
**ignores the username** (`services/auth/method/util.go:45-67`), so it accepts an
arbitrary username + PAT over git-over-HTTPS exactly as GitLab does. `git.ts`'s
mechanism is URL-derived throughout (`httpScopeForUrl:605-615`,
`followRedirects=false:570-573`, Basic at `:594-597`). Comment wording generalizes;
code does not move.

**The SDK deny-hook needs no change.** `agent/src/guardrails.ts` screens by shell
structure and git subcommand, never by URL, host, or remote name (`analyzeGit` denies
on `sub === "push"` `:269`; `analyzeGitConfig` matches config *keys* `:253`; remote
mutators by verb `:270-273`). Its only GitLab reference is prose in a comment (`:7`).
A Forgejo-shaped URL has nothing to slip past, because there is no regex over the raw
URL to slip past.

**All four guardrail layers are untouched by this PRD.**

## Milestones

Eleven milestones, six phases. **The gate flip is the last code change** (M6b), so
everything before it lands dark on `main`: until `handler/forge.go` advertises
`forgejo`, the type is unreachable from the API. (Guardrail *enforcement* was a
twelfth milestone until it was split out to [PRD #66](66-guardrail-enforcement.md) —
it was the one milestone that would **not** have landed dark, refusing GitLab repos
the moment it merged.)

| M | Title | Files (collision-free within a phase) | Depends |
|---|---|---|---|
| **M1** | Interface: `TypeForgejo`, `Role` enum, `WriteRoleCanPush`, **`WriteRoleCanMerge` + `BotCanMerge`** (D6a-1); conform `gitlab.go` **and every implementer/call site**. **Not scoped to `forge/`** — `ProjectRole` has 5 fakes + a live consumer outside it, so "green `go test ./...`" is only achievable if M1 fixes them all | `forge/forge.go`, `forge/gitlab.go`, `forge/gitlab_test.go`, `privcheck/checker.go`, `privcheck/checker_test.go`, **`privcheck/service_test.go`** (`BranchProtection` literals at `:83,:111`), `handler/forge_test.go:55`, `seed/seed_test.go:57`, `poller/autopilot_test.go:171`, `forgesvc/sync_test.go:110` | — |
| **M2** | Driver core + **compiling stubs for the whole interface** + `forge.New()` arm (`forge.go:302`) + **version gate in `VerifyToken`** (D4) + **D3 empirical probe**: `VerifyToken`, `ListProjects` (**client-side `permissions.push` filter**), `ListLabels`, `EnsureLabels`, `ListIssues` (**PR filter, R4**), `GetIssue`, `CreateIssue`, `UpdateIssueLabels` (D3), `UserExists`, `ProjectRole` (**`member` derivation, D7**), `DefaultBranchProtection` (**via `GET /branches/{branch}`, D6**) | `forge/forgejo.go`, `forge/forgejo_pipelines.go` (stubs), `forge/forgejo_test.go`, `forge/forge.go` (arm only) | M1 |
| **M3** | privcheck: `Role` enum, **per-forge scope rule** (D6b), **merge + `unprotected_file_patterns` findings** (D6a-1, D6e — reported, not blocking), **one shared `evaluateRepo` with `Protected` checked first** (R12), **version re-check on sweep** (D4); drop `30`/`roleName()`; **log the ignored unmarshal error** (D7) | `privcheck/checker.go`, `privcheck/report.go`, `privcheck/checker_test.go`, `privcheck/service_test.go`, `handler/forge.go` (unmarshal logging only) | M1 |
| **M4** | Driver: `GetMergeRequest` (state mapping), `ListIssueLabelEvents` (timeline), `CreateIssueNote`, `TokenInfo` (hand-rolled, D5) | `forge/forgejo.go`†, `forge/forgejo_test.go`† | M2 |
| **M5** | Driver: `LatestPipeline`, `LatestMRPipeline`, `ListPipelineJobs`, `JobLogTail` (all ≥16; gitea-sdk has `GetRepoActionJobLogs`, no hand-roll) | `forge/forgejo_pipelines.go`†, `forgejo_pipelines_test.go` | M2 |
| **M6a** | Migration: `forge_type` CHECK gains `'forgejo'`; `runs.mr_web_url`; **the extended completion query + regenerated sqlc** (D8 — without it M7 cannot add the param); **`seed.go:74,85,112` hardcodes** | `store/migrations/`, `store/queries/`, `seed/seed.go` | — |
| **M7** | Worker: `gitlab.ts` → `forge.ts` + `ForgejoClient` (**+3 transport guards, D9**); `forge_type` on `ClaimRepo`; **`mr_web_url` on the completion payload** (D8); subpath base-URL fix; `claim.go` emits it | `agent/src/forge.ts`, `protocol.ts`, `runner.ts`, `agent/test/`, `workersvc/claim.go`, `workersvc/service.go`, `claim_wire_contract_test.go` | **M6a only** |
| **M8** | **API + web** (not web-only): `forge_type` onto board cards + runs (**D2** — DTO field only, the queries already select it), `role` → string in the web (**D7**), merged pipeline map (**R5**), `mr_web_url` rendering **through `isHttpsUrl`** (D8), `forgeNoun()` as the single mapping site + `slacksvc/notifier.go` | `handler/board.go`, run handlers, `slacksvc/notifier.go`, `web/src/lib/*`, `web/src/components/*`, `mocks/` | M6a, M7 |
| **M9** | e2e: `forge-fake` speaks `/api/v1`; `UZI_E2E_FORGE` lane; **live validation vs an upgraded validation instance** (R2) | `e2e/forge-fake/forge-fake.mjs`, `run-e2e.sh` | M4, M5, M7 |
| **M10** | Docs + ADR + `ARCHITECTURE.md:71`; **correct `specs/ai.md` §16 via spec-keeper** (R1); `specs/human.md:40,396` (user-approved 2026-07-17); `docs/forgejo-bot-setup.md` (**unique `order`** — `gitlab-bot-setup.md` is `order: 20`; a copied frontmatter fails `check-docs.mjs` inside `npm run build`) | `docs/`, `adr/0065-forgejo-driver.md`, `ARCHITECTURE.md`, `specs/` | M8, M9 |
| **M6b** | **Gate flip — go-live**: `handler/forge.go:125` advertises `forgejo`; `:156,158` accept it | `handler/forge.go` | M8, M9, M10 |

† M4 and M5 are collision-free **only because M2 lands both files as stubs**. If M2
ships one file, M4 → M5 must run sequentially.

### Execution plan (parallel agents)

| Phase | Agents | Rationale |
|---|---|---|
| **1** | **M1** ∥ **M6a** | Interface (breaks 6 packages at once) and schema+seed. Disjoint. |
| **2** | **M2** ∥ **M3** ∥ **M7** | Driver core / privcheck / worker. Disjoint. |
| **3** | **M4** ∥ **M5** ∥ **M8** | Driver MR / driver pipelines / api+web. Disjoint **given M2's stubs**. |
| **4** | **M9** alone | e2e, green before the gate. |
| **5** | **M10** alone | Docs once the as-built is known. |
| **6** | **M6b** alone | The go-live. Two lines, deliberately last. |

Peak parallelism 3.

**Why M6 is split — this was a real bug, not a tidy-up.** An earlier draft put the
whole of M6 (migration **and** gate flip) in Phase 2 while claiming "M1–M5 land dark".
Those contradict, and the failure mode is worse than "the gate opens early": with the
gate open and M7 absent, a Forgejo run would **clone fine** (git auth is
forge-agnostic), **do real work, push a branch** — and only then fire GitLab
`/api/v4` + `PRIVATE-TOKEN` at a Forgejo host and die at MR creation. The board would
populate throughout (M2 lands issues/labels), so it looks healthy right up to the
point where the user's work is stranded. On `main`, for two phases.

Root cause: M6 conflated two things with **opposite** dependency needs. The migration
is inert and wants to be early (M8 needs it); the gate flip is the go-live and must be
last. Split, the dark-landing property is finally **true**: M6a only *widens* a CHECK
(no `forgejo` row can exist while the handler rejects the type), M6a/M7 emit
`forge_type: "gitlab"` for existing connections, and the driver is unreachable.
**M6b is the only milestone that changes observable behaviour.**

**One caveat, stated because "dark" invites carelessness: M3 is not inert.** It is a
behaviour-preserving refactor of a **live GitLab path**. Verify it against the
existing privcheck tests; do not just compile it.

**Two dependency corrections, from two reviewers who disagreed.** The architect found
M7 depends on nothing (`forge_type` has existed on `forge_connections` since
`00002_forge.sql:9`, already selected at `runtime.sql:247`) and put it in Phase 1. The
Fable review found M7 *does* have one dependency: **M6a must deliver the extended
completion query and regenerated sqlc**, or M7 cannot add the `mr_web_url` param —
`store/queries/` is in M6a's file list, not M7's. The Fable reviewer is right on the
narrow point, so M7 sits in Phase 2 behind M6a. It depends on **M6a only** — never on
the gate.

## Testing Strategy

`gitlab_test.go` (683 lines) is **hand-written httptest fixtures**, not recorded — so
it ports to Forgejo with **no live instance in CI**, which is why it was built that
way. `forgejo_test.go` mirrors it with a `mockForgejo` recording `Authorization: token
<t>`.

1. **`ListIssues` filters out pull requests** (R4) — mixed issue+PR page in, issues
   only out. Forgejo models a PR as an issue (`Issue.PullRequest *PullRequestMeta`).
   *The trap most likely to ship broken.*
2. **`ListProjects` filters on `permissions.push`** — `/user/repos` has no
   min-permission filter, so a read-only repo must not reach the picker. (The swagger
   says "repos the user **owns**"; it is misleading in uzi's favour —
   `SearchRepoOptions.Collaborate` defaults true (`models/repo/repo_list.go:372`), so
   collaborated repos *are* returned. Do not let this panic a coder.)
3. **`UpdateIssueLabels` sends exactly one PUT** with the correct full set (an
   unrelated `keep-me` label survives).
4. **`UpdateIssueLabels` no-ops** when target == current: assert **zero** PUTs.
5. **Label-event mapping**: `body == "1"` → `add` (`issue_label.go:52`), `body == ""`
   → `remove` (`deleteIssueLabel` omits Content, `:186-192`). Pins an undocumented
   upstream convention so an upgrade fails loudly (R6).
6. **MR state mapping**: `{state:"open"}` → `opened`; `{state:"closed",merged:true}` →
   `merged`. Forgejo says `open`, not `opened` (`modules/structs/issue.go:21`), and
   carries `merged` separately. No `locked` state.
7. **`ProjectRole`**: `permission:"write"` → `RoleWrite`. **`member` is derived from
   the permission payload, not a 404** — assert a removed-bot-on-public-repo 200 with
   `permission:"read"` yields `member` correctly (D7). *An earlier draft's test #6
   asserted `404 → (_, false, nil)`, which pins the wrong convention.*
8. **Version gate** (D4): a mock reporting 15.0.4 is **refused in `VerifyToken`** with
   a version-naming error that survives to the user; 16.0.0 connects.
9. **Branch protection via `GET /branches/{branch}`** (D6): `user_can_push:false` +
   `user_can_merge:false` → no finding; `user_can_push:true` → finding;
   `user_can_merge:true` → finding (D6a-1); non-empty `unprotected_file_patterns` →
   finding (D6e). Assert the driver **never calls `branch_protections/`** (it would
   403).
9b. **Unprotected branch reports the strongest finding, on both drivers** (R12 — the
   inversion). Forgejo's unprotected early-return gives `protected:false` with
   `user_can_push`/`user_can_merge` **true**; the GitLab driver's 404 gives
   `Protected:false` with the push/merge fields **unevaluated (false)**. Assert that
   both produce the unprotected finding and that **no consumer can read
   `false,false` as safe** — `Protected` is checked first in the shared evaluator.
   PRD #66 blocks on these fields, so an inversion here becomes a hole there.
10. **Per-forge scope rule** (D6b): exactly `{write:repository, write:issue,
    read:user}` passes **regardless of order** (Forgejo re-emits in
    `allAccessTokenScopes` order, not mint order — a string comparison would be a coin
    flip); a superset is a **save-blocking** violation (PRD #5's existing token tier,
    unchanged by this PRD); **the literal `["all"]` is rejected**
    (Forgejo collapses every `write:*` to that one token by string substitution);
    GitLab's `{api}` rule is unchanged.
11. **CI (≥16)**: `ListPipelineJobs` parses the job list; `JobLogTail` truncates to
    `maxBytes` from the **end**.
12. **Redaction**: a Forgejo error carrying the PAT is scrubbed. Should pass unchanged
    — `redact.go` is **value-based** (`newRedactor(token)` +
    `strings.ReplaceAll(s, secret, "[REDACTED]")`, `:23-39`), so `Authorization: token`
    is covered by construction. **Two requirements**: the driver must route *every*
    error path through `redact.error` **including the hand-rolled `TokenInfo`** (D5
    puts it outside the SDK), and note `newRedactor` ignores secrets under 8 chars
    (`:26`) — fine for Forgejo's 40-char PATs.
13. **Pagination**: two-page `ListProjects`.
14. **D3 empirical probe** (M2, not a unit test): against a real ≥16 instance, create
    an issue, add an unrelated label, PUT a computed set, assert what happens to the
    unrelated label. The replace semantics are read-from-source and **never executed**
    (R3).

**Pipeline status: two enums, not one.** M8's merged map must know which it reads.
Actions run status (`models/actions/status.go:28-37`) is
`unknown|waiting|running|success|failure|cancelled|skipped|blocked`. `CommitStatusState`
(`modules/structs/commit_status.go:10-23`) is `pending|success|error|failure|warning|skipped`.
An earlier draft conflated them, importing `error`/`pending` and dropping `warning`.
The **collision claim holds** — every string shared with GitLab (`success`, `running`,
`skipped`, `pending`) means the same thing — so one map works. **Two traps**: GitLab
spells it `canceled` (one `l`), Forgejo `cancelled` (two) — the merged map needs both
keys or Forgejo-cancelled falls through `?? "neutral"`; and `failure` must map to
`failed`, or a red build renders benign (R5).

**e2e**: `e2e/forge-fake/forge-fake.mjs` (622 lines) implements `/api/v4/*` (`:416+`),
an `/_e2e` mutator (`:301,319,340,363,383`), and git smart-HTTP via `git-http-backend`
CGI (`:61,75,268,297`). The git half is forge-agnostic. Add a second `/api/v1/*` route
table to the **same** fake, selected by path prefix, sharing state and the git handler
— a separate fake would duplicate the smart-HTTP server for no benefit.
`UZI_E2E_FORGE=gitlab|forgejo`, default `gitlab`, so the existing lane is untouched.
Per CLAUDE.md, e2e stays a local pre-merge gate, not CI.

Any compose smoke uses `env -i HOME=$HOME PATH=$PATH docker compose --env-file
<dummy.env> -p <unique> up` — a bare `up` autoloads the real `./.env` and seeds real
admin/forge data.

## Risks

| # | Risk | Severity | Mitigation |
|---|---|---|---|
| **R1** | **`specs/ai.md:259-262` records a false blocker.** The first thing any future reader trusts, arguing against feasible work. | **High** | Correct in the same commit as the work disproving it (CLAUDE.md). spec-keeper owns the file. *(The redactor description there is **accurate** — an earlier draft wrongly listed it as needing correction. Nothing to fix.)* |
| **R2** | **The self-hosted instance earmarked for validation runs 15.0.4; D4 requires ≥16.0.0**, so uzi will refuse it. M9's live validation is blocked on upgrading it to a release only days old. **Nothing has ever exercised gitea-sdk against a 16.0.0 server** — no reachable instance runs one. | **High** | Prerequisite, not a bug (D4). Upgrade before M9. Until then the ≥16 CI path and the D3 probe are **fixture-only**; if the upgrade slips, M9 ships with the lane green on the fake and a **stated** live gap. Success criteria are marked fixture-satisfiable or not. |
| **R3** | **D3's replace semantics were never executed** — read from source only. The architect flagged this as its own riskiest assumption. | Medium | The M2 probe (test #14) settles it before `UpdateIssueLabels` is called done. |
| **R4** | **PRs leak onto the board as cards** — Forgejo models a PR as an issue. Silent, visible, embarrassing. | Medium | Driver filters `pull_request == null`; test #1. |
| **R5** | **A failed Forgejo build renders a benign badge.** `pipelineBadge.ts` is `PIPELINE_TONES[status] ?? "neutral"` with a `failed` key and no `failure`; `canceled` ≠ `cancelled`. | Medium | Merged map + test; both spellings. |
| **R6** | **The label add/remove convention (`body == "1"`) is undocumented upstream.** | Low | Pin with test #5. Degrades to issue-author attribution, already handled. |
| **R7** | **Migration number collision.** Live head is `00066_hosted_workers.sql`; draft is `00067`. | Medium | CLAUDE.md: renumber above the live head **at merge time**. Strict goose bricks upgraded instances otherwise. |
| **R8** | **Two worker wire-contract changes** — `forge_type` on `ClaimRepo` (D9) *and* `mr_web_url` on the completion payload (D8). An old worker against a new api. | Medium | Both additive **optional**; `forge_type` absent → `"gitlab"`, `mr_web_url` absent → null (falls back to reconstruction). Both pinned in `claim_wire_contract_test.go`. |
| **R9** | **gitea-sdk's client-scoped ctx** misused → cross-request cancellation. | Low | Per-call client construction + `SetGiteaVersion("")`. *(Not a data race — the SDK mutex-guards `c.ctx` and documents concurrency-safety. An earlier draft overstated this.)* |
| **R10** | ~~Non-admin token introspection unproven~~ — **resolved from source**: `GET /users/{username}/tokens` carries `reqSelfOrAdmin()` (`api.go:597`), which admits self. The architect's admin-token 200 was not the only evidence path. | ~~Low~~ **Closed** | Needs `read:user` scope, which D6b's required set includes. |
| **R11** | **Forgejo diverges from Gitea; gitea-sdk rots.** | Low | Forgejo still ships `+gitea-1.22.0` (verified live on both instances). The driver is one file behind an interface. |
| **R12** | **`checker.go:141` early-returns on `!bp.Protected`, so `WriteRoleCanPush`/`BotCanPush`/`BotCanMerge` are `false` because they were never *evaluated*** — indistinguishable from "evaluated and safe". Any consumer writing the obvious `if canPush \|\| canMerge { … }` reads `false, false` on an **unprotected** branch and concludes it is safe: the exact inversion of the truth, on the worst case. | **High** | #65 only reports, so this cannot mis-refuse today — but **PRD #66 consumes these fields and would invert its guardrail.** Fix here, not there: one shared `evaluateRepo` with `Protected` checked first, and a test asserting unprotected → the strongest finding. Recorded in #65 because #65 is where the fields are shaped. |

### The methodological risk, recorded because it bit three times

1. The **architect** first called the CI job-log API a hard blocker, from a complete
   swagger scan of a live 15.0.4 instance. The routes existed in v16.0.0, released the
   day before.
2. The **PRD author** then wrote "v15.0.4 has zero `actions/jobs`/`actions/runs`
   routes" as verified fact — from a grep for a literal path string that cannot match
   Forgejo's nested `m.Group("/actions")` → `m.Group("/runs")`. v15.0.4 **does** have
   `actions/runs` (`api.go:888-893`). The conclusion held; the evidence was false.
3. The **security audit** found D6's original table was written against endpoints
   whose authz gates were never checked — three rows a `write` bot gets 403 on.

**A single live instance proves what that version does. A grep proves what that
string matches. Neither proves absence.** Check the route table at the tag, and check
the gate, not just the route. This PRD exists because a similar inference sat
unchallenged in `specs/ai.md` for months.

## Success Criteria

Marked **[live]** where an upgraded validation instance is required (R2) and **[fixture]**
where the httptest/e2e fake suffices.

- **[fixture]** A Forgejo instance < 16.0.0 is **refused at connect**, with an error
  naming the required version reaching the user (D4).
- **[live]** A user connects a Forgejo ≥16.0.0 instance, and the privilege checker
  applies the **same PRD #5 verdicts** it applies to GitLab (D6) — including the
  per-forge scope rule (D6b), refusing rather than warning where a check cannot be
  satisfied.
- **[live]** A Forgejo repo whose `main` is unprotected, **or** protected but
  mergeable by the bot, **reports** that finding on the Repos page — and still runs
  (D6c; PRD #66 turns this into a refusal).
- **[fixture]** **Nothing in #65 refuses anything that ran before it.** An existing
  GitLab connection sees no new blocking behaviour — the enforcement flip is #66.
- **[fixture]** An **unprotected** branch reports the *strongest* finding, not a clean
  bill of health (R12 — the early-return inversion).
- **[fixture]** A `PRD`-labelled Forgejo issue appears on the board as a card — and
  **no pull request does** (R4).
- **[live]** Dragging that card moves the label on Forgejo, atomically, single-column
  (D3) — and an unrelated label added concurrently behaves as the M2 probe predicted.
- **[live]** A run against a Forgejo repo clones, works, pushes a branch, and opens a
  **pull request** — never touching `main`, all four guardrail layers intact.
- **[live]** The card shows the PR's CI status; a failed pipeline drives the CI-fix
  loop with real job logs (D1, D4) — and a failed build renders **failed**, not
  neutral (R5).
- **[fixture]** A Forgejo card says "Pull Request"; a GitLab card on the same board
  says "Merge Request"; shared chrome says "merge request" (D2).
- **[fixture]** An old worker (no `forge_type`, no `mr_web_url`) still claims and
  completes a GitLab run (R8).
- **[n/a]** `specs/ai.md` §16 no longer claims a blocker that does not exist (R1).

## Decision Log

| # | Decision | Date | Rationale |
|---|---|---|---|
| D1 | Full parity, not board-only | 2026-07-17 | User. No interface method Forgejo cannot satisfy. |
| D2 | `MergeRequest` internal; UI copy per-forge | 2026-07-17 | User, **overruling the architect** (mixed board mixes vocabulary). Trade-off recorded; costs M8 the `forge_type`-to-web plumbing the architect's design assumed away. Shared chrome stays neutral. |
| D3 | Accept the lost-update window | 2026-07-17 | User + architect agreed. Narrow, rare, self-correcting. Exclusive scoped labels rejected — they fork the board contract per forge. Semantics still need the M2 probe (R3). |
| D4 | Forgejo ≥16.0.0, refuse below; no degradation | 2026-07-17 | User, **overruling the architect** (15.x floor + `ErrCIUnsupported`). Guarantees parity, deletes the degradation branch; costs every pre-2026-07-16 instance, including the user's own. Gate lives in `VerifyToken`, re-checked on the sweep, and is **never a security control**. |
| D5 | `code.gitea.io/sdk/gitea`, not forgejo-sdk | 2026-07-17 | Architect, verified. forgejo-sdk is 13 months stale, has no timeline, and only Actions *secrets*. gitea-sdk has both, incl. `GetRepoActionJobLogs`. |
| D6 | Guardrails: full equivalent or refuse | 2026-07-17 | User. **Original mapping was unsafe** — three rows read an admin-gated endpoint a `write` bot 403s on, degrading to a warning. Corrected to `GET /branches/{branch}` + `user_can_push`, which is authoritative rather than inferred. |
| D6a | **Detect can-merge here; enforce in [PRD #66](66-guardrail-enforcement.md)** | 2026-07-17 | User, after three moves: "add the merge check" → "block on push or merge" (once the D6c conflict surfaced) → **split enforcement out** (once the architect surfaced that blocking is a *GitLab* behaviour change with no Forgejo content, riding in a Forgejo PRD). #65 reports and stays dark; #66 owns the flip, its impact count, and its release note. |
| D6a-1 | `WriteRoleCanMerge` + `BotCanMerge` on **both** drivers | 2026-07-17 | User. A Forgejo `write` bot can merge its own PR to protected `main` by default; GitLab's default forbids it — but `merge_access_levels` is configurable, and safe-by-default is not safe. uzi modelled merge on neither forge. |
| D6b | Per-forge required scope set, "exactly" kept | 2026-07-17 | User. Forgejo has no `api` scope; as specced every Forgejo connection was rejected at save. `all` rejected as god-mode; superset rejected as deleting PRD #5's only blocking token check. |
| D6c | Unprotected default branch: **warn, don't block** (restored) | 2026-07-17 | User chose warn-not-block; it was briefly superseded by D6a's blocking rule (unprotected-main *is* can-merge, `convert.go:76-85`), then **restored when enforcement moved to #66**. The round trip is recorded, not erased — a future reader will otherwise re-derive the same conflict. The doc obligation is what #65 carries. |
| D7 | `ProjectRole` → `Role` enum; `WriteRoleCanPush` | 2026-07-17 | Architect. Forgejo has no numeric levels; `write`→30 would be a driver lying. **Not contained in `api/`** — the role is persisted JSONB + typed in the web, and existing rows silently fail to unmarshal. `member` needs its own derivation (404 ≠ non-member). |
| D8 | Persist `runs.mr_web_url`, written by the worker | 2026-07-17 | Architect. The driver has the URL and discards it. Worker path chosen over `mr_watch` (immediate + complete vs first-tick + Human-Review-only); `isHttpsUrl` guard survives. |
| D9 | Minimal TS forge seam (`createMr` only) | 2026-07-17 | Architect. Worker needs ~2 endpoints, not 19. Three transport guards (https, `redirect:"error"`, 409-resume) are interface requirements. |

## Evidence Base

Verified against Forgejo source **at tags** (v16.0.0 / v15.0.4), the uzi tree, and
`proxy.golang.org` — not inference from a live instance (see the methodological risk).
Claims below were checked by at least two independent agents.

| Claim | Evidence |
|---|---|
| Label replace is atomic | `models/issues/issue_label.go:447` — `ReplaceIssueLabels` opens `db.TxContext`; route `api.go:1139` |
| v16.0.0 has job logs, text/plain | `api.go:891` → `repo.GetActionJobLogs`; handler `repo/action.go:1539`, swagger `produces: text/plain` |
| v15.0.4 lacks job logs | `GetActionJobLogs` and `ListActionRunJobs` absent at the tag. **`actions/runs` listing IS present** (`api.go:888-893`) — only the jobs/logs surface is missing |
| Branch protection is admin-gated | `api.go:867` — group ends `reqToken(), reqAdmin()`; `req_admin.go:20-22`. Identical at v15.0.4 (`:872`) |
| `GET /branches/{branch}` is reader-gated + authoritative | `api.go:852-858` (`reqRepoReader`); `repo_branch.go:11-21`; `convert.go:107-108` computes `user_can_push`/`user_can_merge` for the caller |
| write bot can merge protected main by default | `protected_branch.go:155-157` fallback; `EnableMergeWhitelist` default false (`:43`); merge endpoint `reqToken()` only (`api.go:983-984`) |
| GitLab's default forbids it | GitLab docs `repository/branches/default.html`: initial default branch protection is **"Fully protected"** — "Developers cannot push new commits, but maintainers can. No one can force push." Both push and merge sit at Maintainer (verified 2026-07-17). *An earlier draft quoted the manual-protection defaults instead and called it verified — see the methodological risk.* |
| Forgejo's unprotected early-return gives a write bot `UserCanMerge: true` | `services/convert/convert.go:63-64` — `hasPerm` is `HasAccessUnit(user, repo, TypeCode, AccessModeWrite)`; `UserCanPush` is `CanMaintainerWriteToBranch`, which returns true on write (`models/issues/pull_list.go:86-88`). **This is D6a's foundation and was independently verified after the PRD author asserted it from inference.** |
| No bot-principal path to `main` escapes D6a | Merge API, pre-receive merge branch (`hook_pre_receive.go:436`), and auto-merge all funnel through `IsUserAllowedToMerge` (`services/pull/merge.go:486`); auto-merge re-checks `CheckPullMergeable` at execution with the scheduler's identity (`services/automerge/automerge.go:216-227`), so scheduling is not a bypass |
| Forgejo returns 409 on duplicate PR | `routers/api/v1/repo/pull.go:773-774` (`ErrPullRequestAlreadyExists`). **Closes D9's "unverified".** Caveat: 409 also covers other conflicts (`:780,:976`), so the fetch-existing fallback must tolerate finding no open PR |
| D6b's scope set is sufficient **and** minimal | Actions runs/jobs/logs sit inside the repo group closed with `CategoryRepository` (`api.go:883` within the group ending `:1083`) — no extra Actions scope; issues under `CategoryIssue` (`:1206`); `GET /user` + `/users/{u}/tokens` under `CategoryUser` (`:601`); git push over HTTPS needs `write:repository` (`routers/web/repo/githttp.go:156`). None of the three is spare |
| Token scopes are normalized at **mint**, and collapse rather than expand | `Normalize()` at token creation (`routers/api/v1/utils/access_token.go:154`, `routers/web/user/setting/access_token.go:265`); `toScope()` emits a scope only if it adds new bits (`access_token_scope.go:325-352`), and rewrites a full `write:*` run to the literal `all` (`:346-351`). **So the exact-set check is implementable — set-based, not string-based.** Raised by the Fable review as the premise that would have sunk D6b |
| No numeric access levels | `models/perm/access_mode.go:26-39` |
| PRs are issues | `Issue.PullRequest *PullRequestMeta` (`modules/structs/issue.go`) |
| Basic-auth git-over-HTTPS ignores username | `services/auth/method/util.go:45-67` |
| Exclusive scoped labels exist | `models/issues/label.go:89` |
| `redact.go` is value-based | `redact.go:23-39`; `specs/ai.md:255-258` describes it accurately |
| gitea-sdk v0.25.1, 2026-05-12; forgejo-sdk v2.2.0, 2025-06-17 | proxy.golang.org |
| Live versions | validation instance → 15.0.4+gitea-1.22.0; codeberg.org → 15.0.0-209+gitea-1.22.0 |

**Unverifiable and not relied upon**: the architect's live-instance anecdotes (SDK
smoke test, `extraHeader` control test, admin-token 200). Each underlying fact was
independently confirmed from source, which is stronger evidence than the anecdote.
**Untestable today**: gitea-sdk against a 16.0.0 server — no reachable instance runs
it (R2).
