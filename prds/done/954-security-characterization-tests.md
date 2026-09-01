# PRD #954: security characterization tests — secretscrub prefix binding, TokenInfo fail-safe pin, DecodeJSON 413

**GitHub Issue**: [#954](https://github.com/vtmocanu/uzi/issues/954)
**Status**: Done (created 2026-09-01, completed 2026-09-01)
**Priority**: Medium
**Parent**: epic #915 (Batch 2, P12; findings S2, S3, S4). No Batch dependency; authored after #918 (PR #952) settled the httpx anchors.
**Line refs**: at `c05b419` (current main). Implementer re-derives at their base; anchors are identifiers, not offsets.

## Problem

Three audit observations from the epic, each a place where a security property holds today but nothing pins it:

1. **S2** — Nothing ties the log-scrubbing pattern lists to the credential prefixes uzi mints. Worse than the epic knew: there are **two** independent lists — `api/internal/secretscrub/secretscrub.go:30-33` (4 patterns) and `snapshotSecretPatterns` in `api/internal/workersvc/ci_fix_snapshot.go:34-42` (5 patterns) — diverging in both directions, related by no test. And there is a **live gap**: neither list covers any GitHub token family (`ghp_`, `gho_`, `ghs_`, `ghu_`, `github_pat_`), though the GitHub driver has been live since 2026-08-08 and `api/internal/forge/github.go:174` itself parses `github_pat_`. A GitHub PAT reaching an outbound Slack message, a persisted clarification answer, or a failure snapshot is redacted by neither list.
2. **S4** — `forgejo.TokenInfo`'s fail-safe (`api/internal/forge/forgejo.go:1128-1200`: 0 or >1 last-eight matches ⇒ error, never scopes) is **half pinned**: the `matches > 1` arm has a real, mutation-checked test (`forgejo_test.go:1068-1104`), but the `matches == 0` arm (:1199), the multi-page scan + `maxForgePages` cap (:1181), and the caller-side downgrade behavior are all unpinned. (The epic's "unpinned by tests" was an overstatement; this PRD records the accurate state.)
3. **S3** — `httpx.DecodeJSON` (`api/internal/httpx/respond.go:71-75`) silently truncates at `maxBodyBytes` (1 MiB, :25), so an oversize body surfaces as a misleading generic 400. Its typed sibling `DecodeJSONLimited` (:98-102, `http.MaxBytesReader` + `*http.MaxBytesError`) exists at **9** call sites — but **only 1 answers 413** (`handler/worker_protocol.go:470-497`); the other 8 (`handler/labels.go:26,:57` and six `handler/schedules.go` sites) catch the typed error and still collapse it into `400 "invalid request body"`.

## Solution

Pin what holds, and make two small, **explicitly flagged** behavior changes in the safe direction (everything else is tests only):

- **Behavior change A (redaction widening):** add the GitHub token families to BOTH pattern lists, and widen `secretscrub`'s GitLab coverage from `glpat-` alone to the same 9-family set the snapshot list already carries (`gl(pat|oas|rt|cbt|ptt|soat|imt|agent|dt)-`, `ci_fix_snapshot.go:35`) — that asymmetry is a live outbound-Slack scrub gap, the third direction of the lists' divergence. Also add `xoxe-` (Slack refresh tokens fall outside `x(?:ox[bpoas]|app)-`): yes, include it. Redacting more is the safe direction; no consumer parses redacted output.
- **Behavior change B (oversize = 413 on 8 routes):** map `*http.MaxBytesError` → `413` at the 8 `DecodeJSONLimited` sites that answer 400 today, via one shared helper — **paired with a CLI exit-code preservation fix** (below), because the blast radius was measured: `api/internal/uzicli/client.go:702-742` (`statusError`) has **no 413 case**, so 400→413 would silently flip the CLI's exit code from `ExitUsage` (2) to `ExitGeneric` (1) on the CLI-reachable labels/schedules routes. Adding `413 → ExitUsage` keeps `$?` stable. Web needs nothing (`web/src/lib/api.ts` renders the server's `{"error":…}` string regardless of status); the worker's batcher already handles 413 on its own route (`agent/src/batcher.ts:99`).

**Not in scope:** migrating the 84 `DecodeJSON` call sites (46 files) to the typed variant. That is the epic's full "standardize" ambition and a separate mechanical PRD; this one makes the already-typed sites truthful and pins the contract.

## Milestones

- [x] **M1 — S2: GitHub families + the binding test.**
  - Add GitHub patterns to `secretscrub.go` AND `snapshotSecretPatterns`. **Exact regexes (baked here because the repo evidences only `ghp_` and `github_pat_`, and the worker has no internet):** classic families `gh[pousr]_[A-Za-z0-9_]{16,}` (covers `ghp_`/`gho_`/`ghu_`/`ghs_`/`ghr_`), fine-grained `github_pat_[A-Za-z0-9_]{16,}`. Add `xoxe-[A-Za-z0-9-]+` alongside the Slack pattern, and the GitLab 9-family regex to `secretscrub` (copy the shape from `ci_fix_snapshot.go:35`, keeping `{16,}` anchoring).
  - New `api/internal/secretscrub/secretscrub_test.go` in an **external test package** (it must import `clitoken` + `jointoken`, and the dependency must not point the other way): iterate the exported minted-prefix constants — `clitoken.PrefixUser`/`PrefixAdmin` (`clitoken.go:41-42`), `jointoken.Prefix` (`jointoken.go:25`) — build a plausible token per prefix, and assert each is scrubbed by **both** `secretscrub.Scrub` and `workersvc.ScrubKnownTokens` (the "one literal, both paths" idiom of `handler/cli_redactor_test.go:18-37`). Same both-paths assertion for every forge PAT family (`glpat-`, the GitHub set, `sk-ant-`). The point of iterating the **constants** rather than string literals: a future fourth minted prefix without a scrub pattern reddens the gate (today's `slacksvc/redact_test.go:57-68` hardcodes `"uzc_"` etc. and would stay green).
  - Document (in the test, as assertions with comments) the deliberate per-list extras that REMAIN after change A: header-line + bare-Bearer patterns only in the snapshot list, Slack tokens only in `secretscrub` — so the divergence becomes a recorded decision instead of drift. (The third divergence — GitLab 9 families vs `glpat-` only — is CLOSED by change A, not documented as deliberate.) Do NOT force-converge the lists further.
  - Mutation check: remove one pattern from one list → the binding test reddens. `task gate:api` green.
- [x] **M2 — S4: pin the unpinned arms of `TokenInfo`.** In `api/internal/forge/forgejo_test.go`, mirroring the existing fixtures (`TestForgejoTokenInfoParsesScopes` :1016, `TestForgejoTokenInfoAmbiguousCollisionFailsSafe` :1068):
  - The `matches == 0` arm: a served token list omitting the authenticating token ⇒ error mentioning "not found", zero `TokenInfo`, and the error is **not** `forge.ErrTokenIntrospectionUnsupported` (callers branch on that sentinel — the branch is `privcheck/checker.go:46`; `:57-68` is the per-forge warn helper it dispatches to).
  - The multi-page scan: a collision **split across two pages** still fails safe (both existing tests serve one page, so the pagination loop and cross-page accumulation never execute today); plus the `maxForgePages` cap arm (:1181).
  - Deepen the `>1` arm: assert zero-valued `TokenInfo` and non-sentinel error, not just `err != nil` + PAT absence.
  - Caller consequence: a test pinning `privcheck.CheckToken`'s downgrade (`checker.go:43-52`: introspection error ⇒ warning + zero `TokenInfo{}` evaluated, never a blocking violation) and one for `forgesvc.ensureProjectScope` (`projectsync.go:2048-2054`: sentinel ⇒ nil, other errors propagate). Tests only; no production change. `task gate:api` green.
- [x] **M3 — S3: truthful 413 on the 8 typed sites + CLI exit-code preservation.**
  - One shared helper in `httpx` (e.g. `RespondDecodeError(w, err, fallbackMsg)`: `errors.As(*http.MaxBytesError)` ⇒ 413 with uzi's own prose — **never `err.Error()`**, which is a stdlib literal (`handler/worker_messages_poison_livedb_test.go:441-467` pins that discipline on the worker route) — else the caller's 400). Migrate the 8 sites (`labels.go:26,:57`, `schedules.go:198,299,542,633,778,905`); `worker_protocol.go:470-497` keeps its bespoke `NoteOversizeBatch` branch untouched. **EOF-tolerance trap:** two of the schedules sites (`:633,:778`) guard with `&& !errors.Is(derr, io.EOF)` and deliberately ACCEPT empty bodies (`:905` is a plain `derr != nil` with no EOF guard — corrected here from the draft's "three sites incl. :905", verified against `schedules.go` at implementation) — call the helper INSIDE each site's existing error branch, preserving its guard verbatim (an oversize error is never `io.EOF`, so it still reaches the 413 path); replacing the branch condition wholesale would regress empty-body acceptance.
  - `api/internal/uzicli/client.go` `statusError`: add `http.StatusRequestEntityTooLarge → ExitUsage` so the CLI's exit code for an oversize body stays **2** across the flip (constants at `uzicli/output.go:23-28`). Per the repo convention, this is the matching CLI change checked in the same MR.
  - Tests: unit-test the helper both ways (reuse `respond_test.go`'s `oversizeJSON`/`exactSizeJSON` fixtures :29/:40 and mirror `TestOversizeBodyIsTypedOnLimitedAndTruncatedOnPlain` :102-129); handler-level oversize test for at least one labels route and one schedules route asserting 413 + uzi prose; a `statusError` table entry for 413. The **84 `DecodeJSON` routes stay at 400** — the flagship truncate-vs-typed test must still pass unmodified, proving the sibling contract holds (`respond.go:83-88`'s doc comment states it: the Limited variant exists precisely so no other route's behavior changed). `task gate:api` green.

## Success criteria

1. A minted-prefix VALUE change in `clitoken`/`jointoken` without a matching pattern in BOTH lists reddens `task gate:api` (the binding test iterates the exported constants, not copies of them). Go cannot enumerate const identifiers, so a brand-new fourth prefix still needs adding to the iterated slice — mitigate by introducing a small production slice next to the constants (e.g. `clitoken.Prefixes = []string{PrefixUser, PrefixAdmin}`) that the test ranges over, so the natural place to add a new prefix is the same line that feeds the test.
2. GitHub token literals are scrubbed on both paths; the deliberate per-list extras are asserted as such.
3. All four `TokenInfo` outcome arms (1, >1, 0, page-cap) and the cross-page case are pinned, with the stated mutation checks (pick-first ⇒ `>1` test reddens; return-scopes-on-0 ⇒ `0` test reddens).
4. The 8 typed sites answer 413 with uzi prose on oversize; CLI exit code for those routes remains 2; the 84 plain sites remain 400 (flagship test unmodified and green).
5. `task gate:api` green; no `.github/workflows/**` in the branch diff (implementation or validation).

## Decision Log

- **D1 — two flagged behavior changes, both safe-direction, everything else tests.** Redaction widening (A) and truthful 413 (B) are the epic-approved S2/S3 fixes; the CLI exit-code case rides with B because the measured blast radius says omitting it silently changes `$?`.
- **D2 — do not converge the two scrub lists.** They serve different surfaces (outbound Slack/persisted text vs CI failure snapshots) and their extras differ deliberately; the test records the divergence so it stops being drift. Convergence is a separate discussion.
- **D3 — do not migrate the 84 `DecodeJSON` sites.** Out of scope; possible follow-up PRD. The sibling contract (plain stays 400) is asserted, not just assumed.
- **D4 — external test package for the binding test** keeps the dependency direction clean: `secretscrub` itself must stay import-light (it is imported from low-level paths).

## Risks & mitigations

- **A GitHub-pattern regex that over-matches** (e.g. bare `gh` words) would redact legitimate text. Anchor each family with its literal prefix + `[A-Za-z0-9_]{16,}`-style body, and include negative tests (prose containing "ghost_", an 8-char display stub) mirroring `redact_test.go:57-68`'s under-match negative.
- **The binding test imports `workersvc`** (for `ScrubKnownTokens`) — fine from an external `secretscrub_test` package, but keep it out of `secretscrub`'s own package to avoid an import cycle.
- **`schedules.go` is ratchet-exposed** (`whole-files: true`): it was touched and merged green in PR #952, so pre-existing findings there are already settled; still, run `task lint:api` early after touching it.
- **In-flight #921 overlap: none** — #921 moves `workersvc/service.go` seams; `ci_fix_snapshot.go` is not among them, and this PRD touches no `service.go` line. (If #921 has not merged when this runs, the trees still do not intersect.)
