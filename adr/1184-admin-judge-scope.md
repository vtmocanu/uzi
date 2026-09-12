# ADR-1184: separate routes and queries for the admin "All users" judge scope, attribution hidden at four layers, one deliberate exception

**Status**: Accepted (implemented, issue #1184, M1–M5)
**Date**: 2026-09-12
**Deciders**: architect (design + decision log 2026-09-07); coder (implementation); reviewer.
**PRD**: [prds/done/1184-admin-judge-scope.md](../prds/done/1184-admin-judge-scope.md) — carries the full milestone breakdown, code anchors and Decision Log; this ADR restates the four durable decisions a future edit to the read path, the grouper, or the disposition write is most likely to erode without ever touching a line this PRD's diff touched.
**Related**: `specs/ai.md` §379 (the four-layer attribution-hiding template, first used for the admin CLI-token inventory); PRD #68 Decision 8 (an admin filing another user's recommendation shows provenance); PRD #94 (the disposition provenance model this PRD extends with a third `set_via` value).

## Decision (summary)

An instance admin can read every user's judge recommendations, deduped by `(category, target)`, and mark a coordinate done across every user who has an open occurrence of it. Four decisions make this safe without ever touching the owner-scoped judge path:

1. **Cross-user reads are separate routes and separate queries, never a parameter on the owner path.** `/admin/judge/*` and `ListJudgeRecommendationRowsAll`/`ListJudgeTriageRowsAll`/`ListRecommendationsForCoordsAll` exist beside, not inside, `/me/judge/*` and their owner-scoped twins. No owner query ever grows a nullable `user_id` sentinel to admit "all users" as a special case.
2. **Attribution is hidden at four layers**, not one: the SQL projection, the Go grouper, the admin DTOs, and a wire test plus a live-DB test that both assert the absence. The draft card is the one deliberate exception (PRD #68 Decision 8), because filing publishes a user's worker text.
3. **The cross-user "done" is a third disposition provenance, `admin`**, written `ON CONFLICT DO NOTHING` so it can never overwrite a human verdict, with `set_by_user_id` set for accountability even though the user-facing chip never names the admin — and the owner keeps Undo.
4. **Reads are CLI-reachable, writes are cookie-only** — the same split every other admin surface already uses (`specs/ai.md` §379), not a new rule invented for judge.

## Why this needs its own record

None of the four decisions above shows up as an obviously risky diff. Each looks, in isolation, like the boring choice: reuse the owner query and add a flag; put the aggregate behind one more admin route without thinking hard about whether the SQL omits something; treat "done" as "done" regardless of who set it; add a CLI flag because that's the path of least resistance. The PRD's Decision Log argues each once, at the point of writing the code; this ADR is where the next editor of `judge_recommendations.sql`, `GroupJudgeRecommendationsAll`, or `AdminMarkDone` actually looks before repeating a mistake the PRD's own Risks section named in advance.

## The decisions

### D1 — separate routes, separate queries, no user-id sentinel on the owner path

`GET /admin/judge/recommendations`, `/admin/judge/stats`, and `/admin/judge/category-stats` are mounted under the existing admin read group (`RequireUser` + `RequireAdminRO`, `api/internal/handler/routes_admin.go`) beside `/admin/users`, `/admin/runs`, and the rest — the `AdminListRuns` shape: a separate handler calling a distinct all-users store method, the owner handler untouched. They call `ListJudgeRecommendationRowsAll`, `ListJudgeTriageRowsAll`, and `ListRecommendationsForCoordsAll` (`api/internal/store/queries/judge_recommendations.sql`, `dispositions.sql`, `judge_bulk_disposition.sql` respectively) — each the owner query's spine with the owner predicate (`WHERE rv.user_id = @user_id`) *removed*, never made conditional. `judge_recommendations.go`'s "IsAdmin is never consulted" comment, and the three more comments that lean on it, stay literally true: the owner handler and the owner queries are byte-identical to before this PRD, because nothing in them changed.

The rejected alternative was a `?all=1` query parameter on the owner route, or a nullable `user_id` on the owner query that means "match everyone" when null. Either shape puts one refactor away from a leak: someone simplifies the two queries into one, the sentinel's null case silently becomes the default, and every owner-scoped call site starts seeing every user's rows. Keeping the queries structurally disjoint means that leak would require writing a *new* query that merges them — a change with its own diff to review — rather than flipping a value inside an existing one.

### D2 — attribution hidden at four layers, one exception carried by design

**Layer 1, SQL.** `ListJudgeRecommendationRowsAll`'s projection omits `run_title` (`r.issue_title`), `rec_id` (`rr.id`), `review_id` (`rv.id`), and the filed issue's iid/url — the `runs` join the owner query needs for `run_title` is dropped entirely. It projects `rv.user_id` and `rv.target_run_id` only as opaque UUIDs for the next layer to count and discard.

**Layer 2, Go.** `GroupJudgeRecommendationsAll` (`api/internal/workersvc/judge_admin_backlog.go`) counts distinct `user_id`s into `user_count` and distinct `run_id`s into `run_count`, then drops both identifiers — neither UUID reaches an occurrence.

**Layer 3, DTOs.** `JudgeAdminOccurrenceDTO` carries `{judged_at, verdict, bucket, set_via?}` and nothing else; `JudgeAdminGroupDTO` carries the counts, not the identifiers behind them. There is no field, anywhere in the admin DTO family, that *could* carry an owner, a run id, or a run title.

**Layer 4, tests.** A wire test (`TestJudgeAdminDTOsCarryNoIdentifier`, `api/internal/apitypes/wire_test.go`) serializes a populated admin DTO tree and asserts none of `run_id`, `run_title`, `review_id`, `rec_id`, `user_id`, `owner`, `owner_email`, `email`, `filed_issue`, `issue_iid`, `issue_url` appears as a key, at any nesting level. A live-DB test (`TestAdminJudgeAggregateLiveDB`, `api/internal/workersvc/judge_admin_backlog_livedb_test.go`) seeds two owners, several runs, and distinct run titles, then scans the full serialized backlog for every seeded owner/run/review UUID and every seeded run title, asserting none of them appears anywhere in the response — a check the tag test alone cannot make, because a leak riding a *permitted* field's value (a run title smuggled into `rationale_preview`, say) would pass a tag-only test.

**The exception, by design, not by omission.** `NewestOpenOccurrenceForCoord` and the admin draft/file handlers *do* project `run_id`, `rec_id`, and the requesting/producing user, because the admin issue draft (PRD #68 Decision 8) names the producing run, its repo, and its user on purpose: filing publishes that user's worker-authored text to a forge, and the admin must see whose text they are about to publish before clicking Create. This is a draft/write path, not the aggregate list — §379's four-layer hiding does not apply to it, and never should.

The filed-issue link itself stays out of the aggregate even though it is not an identifier in the same sense: the forge URL carries the owner's namespace whenever they filed into a personal repo, which is attribution by another name. The `filed` bucket state is enough for the aggregate to say.

### D3 — the `admin` disposition provenance: never overwrite a human, accountable in the row, invisible in the chip, undoable by the owner

`UpsertAdminDispositionsForResolvedCoords` (`api/internal/store/queries/judge_bulk_disposition.sql`) writes `status = 'done'`, `set_via = 'admin'`, `set_by_user_id = @admin_user_id`, with `ON CONFLICT (review_id, category, target) DO NOTHING`. `DO NOTHING`, not `DO UPDATE`, is the whole point: a human's existing verdict — done or dismissed, set by that owner — must never be overwritten by an admin fan-out, and the Go layer's own filter to open (`todo`) members is only the first line of defense; the SQL constraint is the durable one that survives a race between the resolve and the write. `DO NOTHING` alone covers only a *disposition* landing in that window; a coordinate that gets *filed* in the same window writes no disposition (`SettleRecommendationFiledIssue` only stamps `recommendation_filed_issues.filed_at`), so the write also carries a `WHERE NOT EXISTS` filed recheck against `recommendation_filed_issues` — the twin backstop that keeps an admin `'done'` off a now-filed coordinate.

`set_by_user_id` is *set* to the acting admin, unlike the two other server-side provenances (`issue_close`, `denied_cli`), both of which leave it `NULL`. Recording the admin's id is a forensic "who did this" in the row, for accountability, even though the user-facing chip never surfaces it: an owner sees "Done by an admin" (or "Done by an admin via #N" when a filed link exists), never a name. `DeleteAdminDispositionsForCoords` deletes only rows with `set_via = 'admin'`, so the admin's own Undo can never remove a human's verdict on the same coordinate, and — separately — the owner's own per-recommendation Undo (`DELETE /runs/{id}/review/recommendations/{recID}/disposition`) is left free to remove an admin row too, since it is owner-scoped and deletes whatever disposition sits on the owner's own coordinate. A later human write on that coordinate clears `set_via` back to `NULL` (`dispositions.sql`'s existing behavior, untouched) — the provenance is a transient fact about who set the *current* verdict, not a permanent record layered on top of it.

The migration (`00215_admin_disposition_provenance.sql`, validated by `00216_validate_admin_disposition_provenance.sql`) widens `recommendation_dispositions_set_via_check` to `IN ('issue_close', 'denied_cli', 'admin')`, added `NOT VALID` so the `ADD CONSTRAINT` skips the validating table scan (and the `ACCESS EXCLUSIVE` lock it would otherwise hold), with `00216` running `VALIDATE CONSTRAINT` under a lock-cheap scan — the same two-step pattern this codebase already uses elsewhere for a `CHECK` on a live table. It carries the same irreversible-Down hazard the `denied_cli` widening (`00128`) recorded first: re-narrowing fails once any row carries `'admin'`.

**Dismiss is deliberately not offered across users.** A dismissal is a judgment call about one's own work — "this recommendation is wrong" or "not worth acting on" — that only the owner is positioned to make. `Mark done` settles a pattern that clearly recurs; `Dismiss` would be the admin overruling someone else's judgment on their behalf, which this PRD's decision log explicitly rejects.

### D4 — reads are CLI-reachable, writes are cookie-only

`/admin/judge/recommendations`, `/admin/judge/stats`, `/admin/judge/category-stats`, and the issue-draft read all sit in the admin read group (`RequireUser` + `RequireAdminRO`), reachable by a session or by an `admin_ro`-scoped `uza_` CLI token — `cli_auth.go`'s masking already turns any lesser-scoped token's `IsAdmin` false before `RequireAdminRO` ever runs, so a `uzc_` token gets `403`, not a partial view. The two disposition writes (`PUT`/`DELETE /admin/judge/recommendations/disposition`) and the file-issue write sit in the admin write group (`RequireAuth` + `RequireAdmin`), which is cookie-only by construction: `RequireAuth` never reads a Bearer token, so a `uza_` token gets `401` on the writes before any handler exists to hold a flag. This is not a new rule invented for judge — it is the same split every other admin write already uses, restated here because a future "just add `--json` and a flag to the write verb too" change would otherwise look like a small, harmless CLI convenience.

The CLI itself follows the same shape as every other admin read: `uzi admin review backlog` and `uzi admin review stats` are new verbs under `uzi admin review`, not an `--all-users` flag bolted onto `uzi review backlog` — the flag would have implied a write path existed to match, and none does.

## Consequences

- A future refactor that merges the owner and admin judge queries behind a shared function with a nullable `user_id` parameter is exactly the regression D1 exists to make visible: it would need to touch a query this ADR names, not add a parameter to an unrelated one.
- Every new field added to `JudgeAdminOccurrenceDTO`, `JudgeAdminGroupDTO`, or `JudgeAdminBacklogDTO` should be checked against the wire test's identifier list (D2) before it ships, not after a leak is reported.
- A fourth disposition provenance value (beyond `issue_close`, `denied_cli`, `admin`) needs the same `NOT VALID` + `VALIDATE` migration shape and the same Down-hazard note as D3's.
- Rationale text remains free text the judge model wrote from a run's trace; it can still name a repo or a file even though the wire never carries an owner. This is not fixable at the SQL/DTO layer — the docs say "aggregated across users," never "anonymous," for exactly this reason (`docs/judge.md`).

Not linked from `ARCHITECTURE.md`: this adds an admin-only read/write surface inside the existing `api` component (new routes, queries, and DTOs), without moving a service boundary, a trust boundary, or a cross-component data flow that document tracks.
