# PRD #920: agent read-only model-pass consolidation (runReadOnlyModelPass)

**GitHub Issue**: [#920](https://github.com/vtmocanu/uzi/issues/920)
**Status**: Draft (created 2026-09-01)
**Priority**: High (security-relevant dedup: the isolation shape must currently stay correct in three places, and they have begun to diverge)
**Parent**: epic #915 (Batch 1, P4)
**Related**:
- `agent/src/judge-runner.ts:272` (`runModel`) / `:317` (`consumeModel`); `agent/src/review-runner.ts:187/:215`; `agent/src/summary-runner.ts:207/:236` — the triplication. The code says so itself: review-runner:189 "Same permission handling as JudgeRunner.runModel"; summary-runner:209 "Same rationale as JudgeRunner.runModel" and :247 "identical to the judge's consumeModel".
- `semgrep/settings-sources-isolation.yml` — the rule guarding `settingSources`. Fact-checked mechanism: it matches `settingSources: $X` minus `pattern-not: settingSources: []`, i.e. **it fires on widened values only and is blind to an omitted key**.
- `agent/src/guardrails.ts` — NOT touched by this PRD. The deny-all hook consolidated here is the advice-runners' local blanket hook, a different object from guardrails' main-protecting `buildPreToolUseHook`.
- Line refs at `0fdec3791dad53d28f44193290f04a139e8a0719`, fact-checked at `f8e3116`.

## Problem

The three read-only "one model pass" runners (judge, review, summary) each hand-copy:

1. **`runModel`**: mkdtemp under homeRoot with a per-runner prefix, `uidSplitActive()` chmod 0o2770, AbortController + timeout race, `finally` cleanup. Byte-nearly-identical ×3.
2. **`consumeModel`**: the entire SdkOptions isolation block — `settingSources: []`, `permissionMode: "bypassPermissions"`, `allowDangerouslySkipPermissions: true`, `includePartialMessages: false`, deny-all PreToolUse hook, `spawnClaudeCodeProcess` detached — plus the accumulate-text/break-on-isResult loop. Identical ×3.
3. The satellite cluster: `promptStream` defined identically in **5** files (also chat-executor.ts:522, sdk-executor.ts:2511), `defaultQueryFn` ×5 with the `SdkQueryFn` type defined **twice** (sdk-executor.ts:198 rich vs chat-executor.ts:59, already one optional member apart), `denyAllTools` ×3 (differ only in reason string), `safeReportFailed` ×2 with a repeated magic `.slice(0,500)` cap.

This is uzi's security-critical isolation shape (docs/security-gate.md) maintained by copy-paste: a hardening change must be made three times, and the judge copy has already grown extra rate-limit/result handling the others lack.

## Solution

One `runReadOnlyModelPass(opts)` helper (new `agent/src/model-pass.ts`) owning the temp-home lifecycle, timeout race, isolation-shaped query, and text accumulation; options `{ token, model?, systemPrompt, prompt, homeRoot, homePrefix, timeoutMs, queryFn, denyReason, log, onResult? }`. The judge's extra result-frame/rate-limit handling arrives via `onResult`. The satellite cluster consolidates alongside (promptStream + defaultQueryFn + SdkQueryFn into `sdk-messages.ts`/the richer existing type; `buildDenyAllHook(reason)` co-located with the helper).

## Milestones

- [ ] **M1 — satellite dedup.** Move `promptStream` to `sdk-messages.ts`; export one `SdkQueryFn` (the richer sdk-executor.ts:198 shape — the chat variant is covariance-compatible) and one `defaultQueryFn`; migrate all five importers. Add `buildDenyAllHook(reason)` (single factory, three reason strings preserved verbatim). Sweep anchors: `git grep -F 'async function* promptStream' -- agent/src/` and `git grep -F 'const defaultQueryFn' -- agent/src/` each return exactly one hit when done (calibrate first). Existing tests green unmodified. `task gate:agent` green (knip gates any unused leftover export).
- [ ] **M2 — runReadOnlyModelPass + migrate the three runners.** Extract the helper; judge/review/summary each shrink to prompt-assembly + one call + result handling. **Hard constraints, stated in code comments at the helper:** (a) `settingSources: []` stays a **literal at the single query site** — the semgrep rule fires on widened values only and cannot see an omitted key, so the consolidated site is now the ONLY place that literal lives and a helper that dropped the key would pass semgrep silently (that blindness gets a sentence in the helper's doc comment); (b) the deny-all hook wiring, `bypassPermissions`, and detached spawn move **verbatim**; (c) guardrails.ts untouched. Tests: existing runner tests green (they drive runModel/consumeModel through the public runner APIs); add one helper-level test asserting the SdkOptions object passed to `queryFn` carries `settingSources: []`, the PreToolUse deny hook, and `permissionMode: "bypassPermissions"` (a characterization pin on the isolation shape — the thing three files used to pin by triplication). Mutation check: remove the deny-hook wiring in the helper and confirm that test reddens before trusting it. Also add one differential assertion pinning that the review and summary paths do NOT invoke the judge-only `onResult` behavior (the "must not silently gain the judge's rate-limit handling" contract, pinned directly rather than resting on existing tests alone). `task gate:agent` green.
- [ ] **M3 — safeReportFailed + the 500 cap.** Fold the two `safeReportFailed` copies into one best-effort reporter (judge's LimitReachedError mapping preserved via an option or the judge keeping a thin wrapper). The `.slice(0,500)` cap: first CHECK `MAX_FAILURE_REASON_LEN` in runner.ts — if equal, share the constant; if not, keep the two values as-is and record the difference in the Decision Log rather than silently changing either (a cap change is a behavior change). `task gate:agent` green.

## Success criteria

1. One definition each of promptStream / SdkQueryFn / defaultQueryFn / deny-all factory / runModel-equivalent in `agent/src`.
2. `settingSources: []` present as a literal at exactly one query site in the advice lane; semgrep rules pass; the helper's doc comment names the omitted-key blindness.
3. The judge's rate-limit/result handling behavior is unchanged (its existing tests are the proof).
4. `task gate:agent` green; guardrails.ts diff is empty; no `.github/workflows/**` in the branch diff.

## Decision Log

- **D1 — consolidation over per-runner hardening.** The alternative (leave three copies, add a drift test) still requires every hardening change ×3; the epic's auditor and reviewer independently rated consolidation as net risk-reducing for a security-sensitive shape.
- **D2 — `onResult` hook rather than a richer return type.** The judge is the only consumer needing the raw result frame (rate-limit classification); an optional callback keeps review/summary call sites one-liner simple and avoids inventing a union return.
- **D3 — timeout constants stay per-runner.** JUDGE_MODEL_TIMEOUT_MS and REVIEW_MODEL_TIMEOUT_MS are both 5 minutes today, but the helper takes `timeoutMs` rather than baking one in — the values being equal is a coincidence, not a contract.
- **D4 — chat-executor and sdk-executor are M1-only consumers.** Their promptStream/defaultQueryFn copies consolidate, but their execution paths (tool-bearing, not read-only) do NOT move to runReadOnlyModelPass; the helper is for the tool-less advice lane only.

## Risks & mitigations

- **The one query site becomes a single point of failure for the isolation shape.** That is also the point: it is now testable once. Mitigated by M2's characterization test + mutation check, and by the doc comment naming the semgrep blindness so a future editor does not trust the rule to catch a dropped key.
- **Divergence already present** (judge's extra handling). Mapped explicitly into `onResult` in M2's design rather than discovered mid-refactor; review/summary must not silently gain the judge's behavior.
- **node --test tally traps** (`.claude/rules/agent.md`): read named failing tests, never a bare tally, when validating.
