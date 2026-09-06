# M2 neutral-harness-seam differential tests

PRD #1146 (#1106 M2) milestone **m4** validation. These are **credential-free,
pure in-process unit tests** that characterize the NEW provider-neutral harness
seam that milestones m1–m3 introduced. They import the frozen seam symbols from
`agent/src` via NodeNext `.js` specifiers and exercise them over hand-built
neutral literals — no SDK query, no real tokens/secrets, no network. Any
token-shaped value in a fixture is DUMMY data constructed at runtime.

Run them with:

```sh
task test:harness-m2
```

which (1) typechecks the suites with `tsc --noEmit` against
`e2e/harness-m2/tsconfig.json` (so a drift in the frozen seam's shapes fails at
the type level, not only at runtime) and (2) runs them under
`node --import tsx --test`.

## What each suite pins

- **`harness-reducer.test.ts`** — `RunTurnReducerImpl` (`agent/src/harness-reducer.ts`)
  as a pure fold over neutral `HarnessEvent`s: the run-level first-session-id latch
  vs the per-turn last-truthy id, group-before-filter (a signal-bearing started-tool
  item is dropped, results are not), the usage+model co-gate onto the first surviving
  item, the origin-`main`-only signal fold (last-wins / latch / concat), clean-EOF
  (`finish({kind:"exhausted"})`), owner-driven `beginTurn` per-turn reset (including
  after a throw that skips `finish`), and the lead-gated context hook.
- **`limit-evidence.test.ts`** — `classifyLimitEvidence` (the NEW single decision
  core) across explicit exhaustion, the corroborated `rejected`+future-reset path,
  past/absent reset, an unknown (non-`rejected`) status, and empty evidence; plus a
  delegation pin that the frozen public `classifyLimitFailure` reaches a verdict
  consistent with the neutral core for a representative raw result frame.
- **`projection-parity.test.ts`** — that `mapSdkMessage` (chat lane) and
  `projectItem`/`projectResult` (run reducer) are ONE projection core: for an
  assistant-text, tool_use, tool_result, success-result and failed-result frame, the
  chat mapper's output is `deepStrictEqual` to the projection over the equivalent
  hand-built neutral input (same key set, order and undefined-key presence).

## Scope note

The advice adapter `ClaudeAdviceHarness` is module-local (not an exported seam
symbol) and stays covered by the frozen `agent/test/model-pass.test.ts`; it is
intentionally out of scope here. These suites add NO production code and do not
touch `agent/src` or `agent/test`.
