# CI-only proofs for PR 3888 (Refs 3847). This branch is never merged.

`.github/workflows/proof-3888.yml` runs one matrix leg per row below through `proofs-3888/run.sh`.
A fail leg applies its patch (a revert of ONE production seam of the candidate, tests untouched) and is
green only if every named test ran and FAILED and the expected assertion text is in the log. A pass leg
is green only if the named tests ran and passed with no failure or skip in the log.

| leg | patch | production hunk reverted | expected failing test(s) and text |
|---|---|---|---|
| handler-candidate | none | none | all six handler tests pass |
| handler-unfixed-all | handler-unfixed-all | worker_protocol.go WorkerRunPublish: both ResponseController deadlines, the context budget wrap, the 408 case | SlowService, OverBudget, WriteDeadlineCoversDetachedTail, StalledUpload, Combined, BodyReadErrorMapping: "the answer did not arrive" / "no answer after" / "want 408" |
| handler-no-write-deadline | handler-no-write-deadline | SetWriteDeadline | SlowService, OverBudget, WriteDeadlineCoversDetachedTail, Combined: "the answer did not arrive" |
| handler-no-read-deadline | handler-no-read-deadline | SetReadDeadline | Combined: "status 408 body" |
| handler-no-ctx-budget | handler-no-ctx-budget | context.WithTimeout(publishContextBudget) | OverBudget: "no answer after" |
| handler-no-408-mapping | handler-no-408-mapping | isUploadTimeout case | StalledUpload, BodyReadErrorMapping: "want 408" |
| livedb-candidate | none | none | both loop-guard LiveDB tests pass |
| livedb-no-claim-loop-guard (4a) | livedb-no-claim-loop-guard | ctx.Err() guard in claimCheckpointSlot only | TestSupersessionClaimStopsAtPublishDeadlineLiveDB fails; the fallback test still passes |
| livedb-no-fallback-loop-guard (4b) | livedb-no-fallback-loop-guard | ctx.Err() guard in freeCheckpointSlot only | TestSupersessionFallbackStopsAtPublishDeadlineLiveDB fails; the claim test still passes |
| stability-combined-x50 | none | none | Combined http1 and http2 each pass exactly 50 times under -race -count=50 |
| agent-candidate | none | none | all named agent tests pass |
| agent-allowance-renewed-per-attempt (a) | agent-allowance-renewed-per-attempt | runner.ts: allowance created once, shared by retries | retry tests: "one allowance signal serves every attempt" |
| agent-caller-abort-not-propagated (b) | agent-caller-abort-not-propagated | runner.ts: caller signal dropped from the allowance | "caller abort ended the retry promptly" |
| agent-timeout-not-from-allowance-state (c) | agent-timeout-not-from-allowance-state | runner.ts: knownClass "timeout" from allowance state | "did not match the regular expression /phase=publish_request, cause=timeout/" |
| agent-no-stream-reset-class (d) | agent-no-stream-reset-class | runner.ts: ERR_HTTP2_STREAM_ERROR case | runner-checkpoint-diagnostics: stream_reset tests |
| agent-no-undici-headers-timeout (e1) | agent-no-undici-headers-timeout | checkpoint-publish-transport.ts: headersTimeout only (bodyTimeout kept) | transport-probe "a stalled response header is cut by headersTimeout" fails (UND_ERR_HEADERS_TIMEOUT expected) while the body probe still passes; transport test native-fetch 1.1/2.0 subtests: "Missing expected rejection" |
| agent-no-undici-body-timeout (e2) | agent-no-undici-body-timeout | checkpoint-publish-transport.ts: bodyTimeout only (headersTimeout kept) | transport-probe "a stalled response body is cut by bodyTimeout" fails (UND_ERR_BODY_TIMEOUT expected) while the header probe still passes |
| agent-dispatcher-not-attached (e3) | agent-dispatcher-not-attached | client.ts: dispatcher passed to fetch | transport test: pool ownership/cleanup assertions |
| agent-client-uses-http-timeout (f) | agent-client-uses-http-timeout | client.ts: allowance falls back to httpTimeoutMs | client-checkpoint-publish-timeout: "ended after .* ms, expected about 400" |

Verdict rules (`run.sh`): each named failing test needs its OWN FAIL line and its OWN evidence (go: the lines
printed under its `=== RUN`; node: its TAP block) matching that entry's regex, with a real assertion
(go: no panic or timeout; node: failureType testCodeFailure or subtestsFailed, never a cancellation or timeout).
Handler legs name the `/http1` and `/http2` subtests (and the BodyReadErrorMapping cases) explicitly, so a
parent failure on one protocol cannot stand in for both. Agent legs run `npm run typecheck` after the patch is
applied, so a mutant that breaks the production typecheck fails its leg. `proofs-3888/transport-probe.test.ts`
is a CI-only probe with one test per undici timeout. Full per-leg logs are uploaded as `proof-log-<leg>` artifacts.

`proofs-3888/verdict.sh` holds the verdict logic and `proofs-3888/run_parser_test.sh` feeds it synthetic logs as the
first step of every leg: a skipped or TODO sibling, a late Go panic, fatal error or test timeout anywhere in the log,
and a cancellation each fail the verdict, while a normal expected-failure log still passes. A TAP `# SKIP` or `# TODO`
directive never counts as a pass (PASS_NAMES and candidate mode), and a Go log holding any `panic:`, `fatal error:` or
`test timed out after` credits no failure.
