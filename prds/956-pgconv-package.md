# PRD #956: pgconv package — consolidate the duplicated pgtype param helpers with split Text/TextOrNull semantics

**GitHub Issue**: [#956](https://github.com/vtmocanu/uzi/issues/956)
**Status**: Draft (created 2026-09-01)
**Priority**: Medium
**Parent**: epic #915 (Batch 2, P7; finding A3). Depends on P5 (#921, merged in PR #955 — the workersvc helpers now live in `workersvc/pgparams.go`).
**Line refs**: at `c686a7d` (current main, post-#955). Implementer re-derives at their base; anchors are identifiers, not offsets.

## Problem

The pgtype param-constructor idiom is duplicated as **51 non-test helpers across 9 packages** (49 free functions plus two `(s *Service)` methods, and ~32 test-local copies besides), with no shared package (`api/internal` has no pgconv/pgutil-like dir — verified). The duplication itself is the small problem. The big one is that the copies **disagree about what `""` means**, in every direction:

- **Same name, opposite semantics across packages**: `pgText("")` → NULL in workersvc (`pgparams.go:15`, 68 prod sites) but → *valid empty* in slacksvc (`gate.go:93`, 16 sites) and usagepoller (`engine.go:334`).
- **Opposite semantics inside one package under different names**: slacksvc carries both `pgText` (""→valid) and `textOrNull` (""→NULL, `chat_open.go:168`); workersvc's `textParam` (`pgparams.go:35`) maps `&""`→NULL while its `pgTextPtr` (`limitwait.go:672`) maps `&""`→valid empty. Neither name signals which.
- **In-package proof the ambiguity is dangerous**: `Service.originColumn` (`workersvc/service.go:3429`, invariant doc :3425-3428) hand-inlines `pgtype.Text{String: col, Valid: true}` at :3443 rather than call its own package's `pgText` — because `""` legitimately means "the implicit Open column" and NULL means "unknown, never restore". Calling the neighbor helper would be silent data corruption.
- Smaller siblings: `pgInt4` (`workersvc/upgrade.go:618`) maps `v <= 0`→NULL while `int4Ptr` seven lines above (:611) maps only nil→NULL; `pgTimePtr` and `timestamptzPtr` are identical bodies under two names in the same package (`limitwait.go:665` / `upgrade.go:604`).

**Coverage is asymmetric, and that decides the milestone order**: the ""→NULL side is pinned by behavior tests (`judge_m3_test.go:482-509`, `service_test.go:2758-2759`, `:3278-3279`), but the ""→valid-empty side has **zero** tests (slacksvc + usagepoller `pgText`: 0 test call sites; no production call site passes a literal `""` — slacksvc's values are Slack-supplied ids and package constants, and usagepoller's single site is `engine.go:302 Source: pgText(r.Source)`, a usage-source string), `pgInt4`'s zero-clamp is untested (all 5 test uses pass positives), and nothing distinguishes `textParam` from `pgTextPtr`. A wrong consolidation of those **compiles, passes the whole gate, and changes behavior only at runtime** — the most exposed site being `GetConfirmedUserBySlackID(ctx, pgText(slackID))` (`slacksvc/replier.go:153`, `gatekeeper.go:136`, `chatactions.go:180`), the documented single chokepoint for every inbound Slack action (`store/queries/slack.sql:53-60`). Both mappings fail closed there, so this is correctness, not a vulnerability — but the gate cannot see it.

## Solution

One new **leaf** package `api/internal/pgconv` (imports ONLY `pgtype`, `google/uuid`, stdlib `time` — both deps already direct in `api/go.mod:24-25`; zero `internal/` imports so no cycle is possible; even `store` could import it, and `controller/` has zero pgtype usage). **Two honestly-named text constructors, never an ambiguous `pgText`** — the name `pgText` is retired repo-wide.

Exports (each a one-liner; semantics stated in its doc comment):

| export | semantics | replaces |
|---|---|---|
| `Text(s)` | always valid, `""` included | slacksvc/usagepoller `pgText`; may also express `originColumn`'s inline literal |
| `TextOrNull(s)` | `""` → NULL | workersvc `pgText`; handler `pgtypeTextOrNull`/`pgTextOrNull`; seed `pgtypeTextOrNull`; slacksvc `textOrNull` |
| `TextPtr(p)` | nil → NULL, `&""` → valid empty | workersvc `pgTextPtr` |
| `TextPtrOrNull(p)` | nil or `&""` → NULL | workersvc `textParam` |
| `UUID(u)` | always valid (incl. `uuid.Nil`) | `pgUUID` ×3 (workersvc `pgparams.go:140`, handler `agent_templates.go:849`, schedsvc `scheduler.go:949`) |
| `UUIDOrNull(u)` | `uuid.Nil` → NULL | workersvc `nullableUUID` (`judge_backlog.go:97`) |
| `UUIDPtr(p)` | nil → NULL | notifysvc `optionalUUID` (`service.go:178`) |
| `Time(t)` | always valid | `pgTime` ×4 (workersvc/schedsvc/usagepoller/hostedsvc — hostedsvc's own comment at `ephemeral.go:214` admits the mirroring) |
| `TimePtr(p)` | nil → NULL | `pgTimePtr` ×2 + `timestamptzPtr` |
| `BoolPtr(p)` | nil → NULL | workersvc `pgBoolPtr` (`limitwait.go:683`) only — handler's behaviorally-identical `optBoolToPgtype` (`forge.go:904`; nil branch spelled `pgtype.Bool{}` vs `{Valid:false}`, same value) is DEFERRED, see D5 |
| `Int4Ptr(p *int)` | nil → NULL | workersvc `pgIntPtr` (`pgparams.go:107`) |
| `Int4Ptr32(p *int32)` (or one shape, implementer's call) | nil → NULL | workersvc `int4Ptr` (`upgrade.go:611`) |
| `Int8Ptr(p *int64)` | nil → NULL | workersvc `int8Param` (`pgparams.go:97`) |
| `Float4Ptr(p *float64)` | nil → NULL | workersvc `pgFloat4Ptr` (`pgparams.go:117`) |

**Stays put (domain logic, must NOT move; may delegate to pgconv internally where behavior is identical):** `stopReasonParam`, `sanitizeFailureReason`, `stripNULParam`, `clampWirePreservedPatch` (all `pgparams.go`), `clampWireFixVerdict`, `clampWirePRDDonePath`, `clampWireReportMd`, `findingBucketStatus`, `limitAwareFailureReason`, `numericUSD`, `healthSince`, `originColumn`, `activeRunsPriorityCutoff`, `oidcDisplayName`, the schedules column helpers (`schedules.go:1430/:1442/:1453/:1675/:1684`), `pgTextTrimNarg`, `pgInt2` (usagepoller, int16 narrowing), `durationToInterval` (hostedsvc), and forgesvc's `optionMarker` (domain name documents intent; **leave the file untouched** — also keeps this PRD file-disjoint from #954). `pgInt4` (`upgrade.go:618`) also stays: its `v <= 0`→NULL is domain semantics, not a constructor — but it gets a pin (M2).

**Test-local helpers in `_test.go` files stay as they are** — they are fixtures, not production duplication. Only test *call sites* of deleted production helpers get the mechanical rename.

## Milestones

- [ ] **M1 — the pgconv package + exhaustive tests.** All exports above with table tests covering, for every constructor: the zero/empty/nil case, a normal value, and (for text) whitespace — asserting BOTH `Valid` and the payload field. Mutation check: flip one constructor's `Valid` and confirm exactly its test reddens. `task gate:api` green. (New files are 100% ratchet-new; keep them lint-clean.)
- [ ] **M2 — pin the unpinned behaviors at their CURRENT sites, before any migration.** Commit subjects prefixed `M2:` (SC3 checks ordering by prefix). Two different strengths, stated honestly:
  - **Real gates**: workersvc `textParam(&"")`→NULL vs `pgTextPtr(&"")`→valid-empty (the pair in one test, so the split is explicit) and `pgInt4(0)`/`pgInt4(-1)`→NULL vs `int4Ptr` nil-only. These make a wrong M3 mapping of those helpers RED.
  - **Characterization of intent only**: slacksvc `pgText("")` and usagepoller `pgText("")` are valid-empty. `Text` and `TextOrNull` differ ONLY at `""` and no realistic input reaches `""` at those sites, so no test can gate their M3 call-site mapping — that mapping is **review-gated against the table, not test-gated** (the compensating facts: both mappings fail closed at the `GetConfirmedUserBySlackID` chokepoint, and the M2 test records which behavior the sites were built on).
- [ ] **M3a — migrate workersvc by SEMANTICS, per the table.** Commit subject `M3(workersvc):`. Each migrated call site maps by the helper's measured behavior, never its name; delete each emptied generic helper in the same commit that empties it. Test call sites of deleted helpers get mechanical renames only — **every pinning assertion stays byte-identical**. Optional, explicitly allowed: re-express `originColumn`'s inline literal as `pgconv.Text(col)` — the documented invariant finally wearing its name. This is the big half (~170 prod invocations + 75 `pgText` test renames). Full proof per the store-heavy rule: `task gate:api` AND `./e2e/run-store-it.sh` (named tests `--- PASS`, `RUN>0`, `SKIP=0`).
- [ ] **M3b — migrate the other 7 packages** (handler, slacksvc, usagepoller, schedsvc, seed, notifysvc, hostedsvc), one commit each, subjects `M3(<pkg>):`, same discipline. Packages are independent (each owns private helpers; no cross-package helper calls), so **N-of-7 completed packages is a valid landable state** — SC2's repo-wide remainder is only reached at the end of M3b, and an incomplete M3b leaves its unmigrated packages listed in the PRD for the resume. `task gate:api` green per commit; live-DB sweep again at the end.

## Success criteria

1. `api/internal/pgconv` exists, is a leaf (its import block contains only pgtype, uuid, stdlib), and every export has zero/empty/nil-case tests.
2. No `pgText` **production helper or call site of one** remains in `api/`; the one test-local closure named `pgText` (`api/internal/store/run_message_instance_integration_test.go:56`, with doc-comment mentions at :21/:55) is exempt per the fixture rule and is the ONLY expected hit of `git grep -w pgText -- 'api/**/*.go'` (scope the grep to `api/**/*.go` — the name also appears in agent-side comments (`agent/src/batcher.ts:313`, `config.ts:255`) and old `prds/done/*.md`, all non-hits). And no **single-return generic** pgtype constructor remains outside pgconv except the named stays-put list (which includes the deferred `optBoolToPgtype`) — check with `git grep -nE '^func .*\) pgtype\.' -- 'api' ':!*_test.go'` and read the hits; multi-return domain/validation builders that happen to return pgtype among other values (`validateModel`, `storeColumns`, `builtinColumns`, `inferredRequirementParams`, `handoffBudget`, `resolveSecretLabel`, and kin) are OUT of scope and expected survivors.
3. Ordering is mechanically checkable: `git log --oneline` shows every `M2:`-prefixed commit preceding the first `M3(`-prefixed commit.
4. Zero behavior-assertion edits: the existing ""→NULL pins (`judge_m3_test.go`, `service_test.go`) and the new M2 pins pass with assertions byte-identical (call-target renames only).
5. `task gate:api` green; live-DB sweep green with positive controls; no `.github/workflows/**` in the branch diff (implementation or validation).

## Decision Log

- **D1 — two text constructors with honest names; `pgText` is banned.** The epic's trap ("consolidation needs two distinct functions, never a naive merge") is the design: the ambiguous name is the carrier of the bug class, so it does not survive even as an alias.
- **D2 — migration maps by measured semantics, not by name.** The table above is the mapping; a reviewer checks each site against it. Name-based migration is exactly the naive merge A3 forbids.
- **D3 — domain helpers stay in their packages.** Moving `stopReasonParam` et al. would drag `truncateRunes`/`termsafe`/`store` into a leaf package; delegation inward is allowed, relocation is not.
- **D4 — characterization-before-move (M2 precedes M3).** The workersvc `*string` split and `pgInt4` pins genuinely gate their migrations. The slacksvc/usagepoller valid-empty pins are intent records: their call-site mapping cannot be test-gated (no `""` ever reaches those sites), so it is review-gated against the table, with the fail-closed chokepoint as the backstop. The epic's safety contract makes the pins a precondition either way.
- **D5 — forgesvc untouched, and `optBoolToPgtype` deferred for the same reason.** `optionMarker` is behaviorally `TextOrNull` but carries domain meaning and 12 sites in a file this epic's #954 neighbors; leaving it keeps Batch 2 PRDs file-disjoint. Likewise `handler/optBoolToPgtype` stays put this run: three of its call sites are in `handler/schedules.go` (:236, :363, :1116) — the file #954's S3 milestone edits — so retiring it would break the file-disjoint rule; it (and its `mrrework_run.go:51` site) can join `pgconv.BoolPtr` in a later sweep. Consequently #956 must NOT touch `handler/schedules.go` or `handler/mrrework_run.go` at all. (`handler/forge.go` is still in scope for `pgtypeTextOrNull`-family sites; #954 does not touch `forge.go`.)

## Risks & mitigations

- **A wrong Text/TextOrNull mapping compiles and passes.** For the workersvc helpers M2's pins catch it; for slacksvc/usagepoller the ONLY net is per-site review against the mapping table (plus the fail-closed `slack_resolved_id` chokepoint) — the reviewer looks there first and hardest, because no test can.
- **The 75 workersvc test call sites of `pgText`** make the mechanical rename noisy; the byte-identical-assertions rule (SC4) is what keeps the noise reviewable — any assertion diff in those files is a red flag, not a cleanup.
- **sqlc params take pgtype values directly** (verified on `CreateRunParams`, `store/runtime.sql.go:973-979`; NOT NULL uuid columns generate plain `uuid.UUID` via the `sqlc.yaml:22-24` override) — so no store/generated-code changes are needed or allowed; a diff touching `*.sql.go` means something went wrong.
- **Ratchet**: every touched file's latent findings gate (`whole-files: true`); run `task lint:api` after each package commit, fix or justify in separate labeled commits (the #955 precedent).
