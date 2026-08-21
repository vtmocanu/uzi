# PRD #499 — Suppress command-not-found pre-scan false positives on repo shell functions

**Issue**: [#499](https://github.com/vtmocanu/uzi/issues/499)
**Priority**: Medium
**Status**: Complete — landed on branch `agent/issue-499` (2026-08-21)

> **Path convention**: every path in this PRD is relative to the repo root. The change is confined to **one Go file plus its test** in the **api** module (`api/internal/workersvc/`) — no migration, no SQL, no query, no wire/DTO change, no worker (`agent/`) change. Load `.claude/rules/go.md` before starting.
>
> **This PRD is destined for an offline uzi worker.** Every fact below was verified against the codebase on 2026-08-21 with its `file:line`, so the work needs **no** internet access, no external docs, and no live cluster or DB (the target tests are pure, non-`LiveDB`). It touches **no** file under `.github/workflows/**` in either implementation or validation (`.claude/rules/prds.md`).

## Problem

The judge's **deterministic command-not-found pre-scan** reads a reviewed run's tool trace and reports the executables the shell said were missing, so the judge (and, on a judge-model failure, the review backlog) can suggest installing them. It has **no notion of shell functions**, so a command name that is actually a **repo-local bash function** — not a PATH tool — is reported as missing.

The live case: **`db_psql`**, a bash function defined in `e2e/run-e2e.sh:338` (`db_psql() { … }`, a memoized `psql`-over-`docker compose exec` wrapper) invoked ~50 times inside that one script and never a PATH executable. The run completed green. The most likely trigger: the agent `cat`/`grep`/`sed`'d `run-e2e.sh` into a `tool_result`, and that file **contains the literal string `db_psql: command not found`** in a comment at `e2e/run-e2e.sh:325-326` (explaining a past `set -e` scoping bug). The high-confidence bash regex matched that literal and reported `db_psql` as missing.

The cost is not just a noisy judge prompt. On the **judge-model fallback path** (`agent/src/judge-runner.ts:683-689`), each pre-scan miss becomes a literal `category:"install_worker_tool", target:<name>, confidence:"high"` recommendation, so a false `db_psql` enters the review / self-improve backlog as an actionable "install worker tool" item.

## Solution

Add one more **suppression source** to the pre-scan, structurally identical to the ones already there (`pinned` tools reached via `go run <mod>@<ver>`, denylisted credential CLIs, interpreter names): a set of **shell-function names defined anywhere in the run trace**. A candidate whose name matches a function definition observed in the trace is dropped at collection, exactly as `pinned[cmd]` already is. All changes are in `api/internal/workersvc/judge.go` and its test; nothing else moves.

---

## Background — current state (resolved facts)

### The pre-scan is server-side Go, trace-driven, no filesystem

- Entry: `Service.judgeSignal` (`api/internal/workersvc/judge.go:~997`) fetches the run's tool trace (`ListToolTraceForRun`, capped `judgeToolTraceRowCap=4000`) and composes:
  `misses := suppressResolved(scanCommandNotFound(rows), observedGreenTools(rows))` (`judge.go:~1006`). Result is `*JudgeSignal{ MissingTools []ToolMiss }` (`judge.go:82-91`), nil when empty.
- `scanCommandNotFound` (`judge.go:~211`) walks **`tool_result` rows only** (`judge.go:~219`; documented at `:205-210` — `tool_use` holds text the agent *typed*, which never ran) and applies four regexes via `forEachNotFound` (`judge.go:~145`):
  - `reCmdNotFound` `([A-Za-z0-9_.+-]+): command not found` — bash, **high-confidence, unfiltered** (`:108`) ← this is what matched the `db_psql` comment.
  - `reCmdNotFoundZsh` `command not found: (…)` — zsh (`:109`).
  - `reExecNotFound` `exec: "?(…)"?: executable file not found` — Go `exec.LookPath` (`:110`).
  - `reShNotFound` `(…): not found\b` — dash/busybox (regex `:111`), **low-confidence**; kept only if actually invoked (the `!invoked[cmd]` clause at `:262-263`, with `noisyShToken` filtering at `:136`).
- **It has no PATH, shell-function, or repo-script awareness — it only pattern-matches trace text.**

### The existing suppression predicate (where the fix goes)

`scanCommandNotFound`'s candidate-collection predicate at **`judge.go:~262-264`**:
```go
if cmd == "" || shellNames[cmd] || toolprofile.DeniedExecutable(cmd) || pinned[cmd] ||
    (lowConfidence && !invoked[cmd]) || seen[cmd] || len(out) >= judgeMissCandidateCap {
    return
}
```
- `shellNames` — interpreter names (`judge.go:~122`).
- `toolprofile.DeniedExecutable` — credential-bearing CLIs (`api/internal/toolprofile/toolprofile.go:203`).
- **`pinned`** — tools reached via `go run <mod>@<ver>` (`goRunPinnedTools`, `judge.go:~376`). **This is the exact structural precedent for this PRD's `funcs` index.**
- `invoked` — executables the run actually ran (`invokedExecutables`, `judge.go:~294`).

`suppressResolved` (`judge.go:~408`) then drops any candidate the run later ran green (`observedGreenTools`, `judge.go:~436`) and truncates to `judgeMaxMissingTools=20`.

There is **no shell-function handling anywhere in the file today** (grep-confirmed).

### Decoders available (no new plumbing)

- `toolResultText` (`judge.go:~802`) decodes a `tool_result` payload to text — this is what carries the `run-e2e.sh` content (the same file body holds both the `db_psql: command not found` comment **and** the `db_psql() {` definition).
- `toolUseCommand` (`judge.go:~771`) decodes a `tool_use` command string — covers an agent that defines a function inline in a Bash command.

### The two consumers of the miss list

- `agent/src/judge-runner.ts:449-453` (`signalBlock`) — renders the misses into the judge **model prompt**.
- `agent/src/judge-runner.ts:683-689` (`fallbackReview`) — on judge-model failure, turns **each** miss into a high-confidence `install_worker_tool` recommendation. This is the damaging path a false `db_psql` corrupts.

`agent/src/prompt.ts:~610` and `agent/src/sdk-executor.ts:~508` merely mention the phrase "command not found" in comments (JS-deps provisioning, PRD #121/#122) and are **not** consumers of this list — do not wire the fix to them.

### `db_psql` is a shell function, not a tool

`e2e/run-e2e.sh:338` (`db_psql() {`), invoked at `:1691`, `:3262`, etc.; the false-positive-triggering literal `db_psql: command not found` is a comment at `e2e/run-e2e.sh:325-326`.

---

## Design decisions

1. **Suppress on a trace-observed function definition, mirroring `pinned`.** Add `shellFunctionNames(rows []store.ListToolTraceForRunRow) map[string]bool` — a structural twin of `goRunPinnedTools`/`invokedExecutables` — that scans **both** `tool_result` text and `tool_use` command text for shell-function definitions and returns the set of defined names. Wire it as `funcs := shellFunctionNames(rows)` beside `invoked`/`pinned` (`judge.go:~212-213`) and add `|| funcs[cmd]` to the predicate at `judge.go:~262`. Filter **at collection**, not later, for the same candidate-slot reason the denylist is filtered there (`judge.go:~239-243`).
   - **🔴 Decode the payload — do NOT reuse the scan's own raw `payloadText`.** `scanCommandNotFound` matches its four regexes against the **raw** jsonb payload via `payloadText` (`judge.go:~225/230`), where newlines are the escaped two-char sequence `\n` — so on raw text the character before a `db_psql` at line start is the letter `n`, and the def regex's leading `^`/`\s` anchor matches **nothing**. `shellFunctionNames` must run its def regexes on **decoded** text: `toolResultText` (`:802`) for `tool_result` rows and `toolUseCommand` (`:771`) for `tool_use` rows — exactly the decoders Decision 1 already names. An implementer who copies the scan's `payloadText` decode will silently fail to suppress the live `db_psql` case.

2. **Match two definition shapes, anchored.**
   - POSIX form: `(?:^|[;&|(]\s*|\s)(?:function\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\(\s*\)\s*\{`
   - ksh/bash keyword form: `\bfunction\s+([A-Za-z_][A-Za-z0-9_]*)\b`
   Anchoring on a leading `^`/separator avoids matching a call site or a substring; the captured name is the suppressed key.

3. **Bound the scan bytes, per row-kind.** Use a check-before-add byte budget so the new indexer is a true bound and stays reachable (a dead or unbounded helper reddens the deadcode/ratchet gate). **Match the budget to the row kind `scanCommandNotFound` itself uses**: the `tool_result` portion should use `judgeScanByteBudget` (512 KB, the budget the scan applies to `tool_result` text), NOT `judgeCommandByteBudget` (128 KB, the `tool_use` budget) — a smaller budget on the `tool_result` scan would leave a def in the 128 KB–512 KB window unsuppressed while the miss it should cancel is still detected. Use `judgeCommandByteBudget` for the `tool_use` portion, and state whether the accumulator is shared across both kinds or accounted per-kind.

4. **Accept conservative under-suppression, the direction this code already prefers.** A genuinely-missing PATH tool that happens to share a name with a function defined somewhere in the trace would be dropped. That is the same trade already made for `pinned`/`invoked` (the file's stated philosophy: under-report rather than cry wolf). A false "install this tool" is worse than a missed one because it enters the backlog as high-confidence.

5. **Functions only, not helper scripts.** `./scripts/foo`-style repo-local helpers are a fuzzier category; the function-definition index is the precise, testable fix the `db_psql` case needs. Helper-script coverage is noted as a possible **future extension**, not folded in silently.

## Scope

**In scope**:
- `api/internal/workersvc/judge.go`: add `shellFunctionNames`; wire `funcs[cmd]` into the `judge.go:~262` predicate; update the `scanCommandNotFound` doc comment (`:195-210`) to name the new suppression.
- `api/internal/workersvc/judge_prescan_test.go`: a suppression test + a negative control.

**Out of scope**:
- Any change to `toolchain-preflight.ts` (a **different**, unrelated boot-time PATH check for baked worker tools — explicitly not the pre-scan).
- Any wire/DTO/worker (`agent/`) change; the miss list shape is unchanged.
- Helper-script (non-function) suppression.
- Any migration, SQL, or query change.

## Milestones

- [x] **M1 — `shellFunctionNames` indexer + suppression wire + test.** Add `shellFunctionNames(rows)` to `api/internal/workersvc/judge.go` per Decisions 1-3 (scan both `tool_result` via `toolResultText` and `tool_use` via `toolUseCommand`; two anchored regexes; byte-budget bound). Add `funcs := shellFunctionNames(rows)` beside `invoked`/`pinned` and `|| funcs[cmd]` to the predicate at `judge.go:~262`. Update the scan doc comment. Add `TestPrescanSuppressesShellFunctionDefinition` to `judge_prescan_test.go` (templates: `TestScanSuppressesDenylistedCredentialCLIs` at `:688`, `…InPathForm` at `:719`; helpers `traceUse`/`traceResult`/`rawResultRows`/`assertReported`/`prescan`): a `tool_result` carrying `db_psql() {\n …\n}` **and** a `db_psql: command not found` line, plus a second row with a genuine `jq: command not found` miss → assert `db_psql` is NOT reported and `jq` still is. **Build the fixture with `traceResult` (which marshals to jsonb, so `\n` is escaped exactly like a real payload), NOT `rawResultRows` with literal newlines** — the def regex runs on **decoded** text (Decision 1), so a literal-newline fixture would pass even against a wrong raw-`payloadText` implementation, making the test vacuous w.r.t. the decode path (the `traceResult` doc at `judge_prescan_test.go:37-40` flags this raw-vs-decoded split). Add a **negative control**: a genuine miss whose name is not defined survives (proves the index does not over-suppress). Prove each assertion non-vacuous by folding the `|| funcs[cmd]` clause out and confirming the suppression test reddens while the control stays green (`.claude/agent-team.md` mutation discipline; fold at the call site). **Validate**: ensure `origin/main` resolves for the `.golangci.yml` ratchet (`new-from-merge-base: origin/main, whole-files: true`; you are editing `judge.go`, so pre-existing findings in it would gate) — the worker's clone already carries `refs/remotes/origin/main`, so **no network fetch is needed**; run `git fetch origin main` only if `origin/main` is absent/stale (best-effort, skip when offline). Then `task gate:api` (fmt-check → vet → build → lint → deadcode → test `-race -count=1`), plus the focused `cd api && go test ./internal/workersvc -run TestPrescan -count=1 -v`.

## Success criteria

1. A run trace in which `db_psql` appears only as a shell-function definition (and/or as the `db_psql: command not found` comment literal) produces **no** `db_psql` entry in the pre-scan miss list, while a genuinely-missing tool in the same trace (e.g. `jq`) is still reported.
2. The suppression is applied at collection via a `funcs[cmd]` clause fed by `shellFunctionNames`, structurally mirroring `pinned`; the new indexer is reachable and byte-bounded.
3. `task gate:api` passes — lint (ratcheted) green, deadcode at zero, tests green — with the new behavior covered by a non-vacuous suppression test **and** a negative control, each proven by a call-site fold.
4. No wire/DTO/worker change; no migration, SQL, or query change; no `.github/workflows/**` file touched in implementation or validation.
5. `main` is never touched; delivered on a branch + PR.

## Risks & mitigations

- **Over-suppression** (dropping a real missing tool that shares a name with a trace-defined function). Mitigation: Decision 4 accepts this consciously (same trade as `pinned`); the negative control test pins that an *undefined* name still reports.
- **Regex over-match** (matching a call site or substring as a definition). Mitigation: Decision 2 anchors both forms on a leading separator and requires the `()`/`{` or `function` keyword; the test includes a call-site-only row that must NOT suppress.
- **Deadcode/ratchet redness on `judge.go`.** Mitigation: the indexer is reachable via the predicate and byte-bounded; `git fetch origin main` before the ratchet runs so it scores only this branch's additions.
- **Vacuous test** (passing regardless of the fix). Mitigation: the call-site fold in M1 proves both the positive and the control.

## Dependencies

- **No external / internet dependency.** The target tests are pure functions over in-memory `store.ListToolTraceForRunRow` slices, not `*LiveDB`, so they run with `UZI_TEST_DATABASE_URL` unset and no network. A warm Go module/build cache is the only prerequisite; no new dependency is added.
- No shared-file collision with the other PRDs in this batch: this touches only `judge.go`/`judge_prescan_test.go` in `workersvc`; the run-termination PRD (#503) touches `service.go`/`failorigin.go`/`judge_enqueue.go` in the same package but different files.

## Decision log

- **2026-08-21**: Scoped from a judge `improve_uzi` recommendation ("command not found pre scan", seen in 8 runs). Investigation **corrected the rec's premise** that the fix lives in `agent/src/toolchain-preflight.ts` — that is an unrelated boot-time PATH check; the actual pre-scan is server-side Go `scanCommandNotFound` in `api/internal/workersvc/judge.go` (PRD #46 Decision 4).
- **2026-08-21**: Chose to mirror the existing `pinned`/`goRunPinnedTools` suppression exactly (function-name index + one predicate clause), rather than add PATH/filesystem awareness — the pre-scan is deliberately trace-only and filesystem-free.
- **2026-08-21**: Scoped to shell **functions** (the precise `db_psql` case), leaving repo-local helper scripts as a future extension rather than folding a fuzzier heuristic in.
- **2026-08-21**: Next step = send to uzi (Auto). PRD authored fully internet-independent and workflow-file-free.
- **2026-08-21 (implementation)**: Landed M1. Review flagged that the two def regexes as first specified in Decision 2 matched a function-definition SHAPE in ANY language, so a JS `function serve(opts) {` or a Go `func build() {` printed into a `tool_result` (source under review) would index `serve`/`build` and over-suppress a genuine `serve`/`build: command not found`. Tightened to the shell shapes: `reShellFuncPosix` is `(?m)`-multiline and anchored only on a statement boundary (line start with `[ \t]*` indentation, or after `;&|(`) — dropping the bare-whitespace alternative — so a `func name()` whose name is not statement-leading is excluded; `reShellFuncKeyword` now requires the shell body brace `{` (optionally after empty `()`), excluding `function name(args) {` and prose. Residual accepted over-match (per Decision 4): a statement-leading zero-arg `name() {` in another language. Added negative-control tests (`TestPrescanDoesNotSuppressForeignLanguageFunctionDef`, `TestPrescanSuppressesBashKeywordFunctionForm`).
