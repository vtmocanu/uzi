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
| agent-no-undici-dispatch-timeouts (e) | agent-no-undici-dispatch-timeouts | checkpoint-publish-transport.ts: per-dispatch headersTimeout/bodyTimeout | transport test: "Missing expected rejection" |
| agent-dispatcher-not-attached (e2) | agent-dispatcher-not-attached | client.ts: dispatcher passed to fetch | transport test: pool ownership/cleanup assertions |
| agent-client-uses-http-timeout (f) | agent-client-uses-http-timeout | client.ts: allowance falls back to httpTimeoutMs | client-checkpoint-publish-timeout: "ended after .* ms, expected about 400" |
