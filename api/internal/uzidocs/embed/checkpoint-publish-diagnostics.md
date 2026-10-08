---
title: Checkpoint publish diagnostics
order: 68
audience: operator
---

# Checkpoint publish diagnostics

Use this guide to investigate missing or incomplete receive-pack
acknowledgements during checkpoint publication. An unknown acknowledgement
does not prove that origin refused the update: the remote may have accepted
it before confirmation was lost. For retained work, follow
[Recovering unpublished work](./run-recovery.md).

## HTTP finalization boundary

The broker observes the marked receive-pack POST for the current invocation,
not discovery or an unrelated response. It deterministically reads the HTTP
response body before replaying its bounded bytes to go-git's decoder, then
closes the **original** body. Replay does not observe the bytes a second time.
Read, close and context errors remain part of the outcome.

Finalization uses the original request context and deadline; it gets no fresh
drain timeout. Checkpoint publication has a 60-second broker ceiling covering
the budget scan, fetch and push together, or an earlier caller deadline or
cancellation. HTTP body stalls therefore consume the remaining original
budget. A broken reader stops after 100 consecutive empty reads.

The response cap is **1 MiB of actual bytes**, including pkt-line framing and
sideband progress as well as report data; it does not rely on Content-Length.
Reaching exactly 1 MiB conservatively makes the result unknown, even if the
last bytes look complete or accompany EOF. There is no extra EOF probe or
drain beyond the cap.

HTTP **200**, an attached response, clean EOF and successful original-body
close under the original context are required for authoritative HTTP report
evidence. A non-200 response cannot establish an advance or an authoritative
rejection, even if its body resembles a valid report. HTTP 200 alone cannot
establish either outcome.

The observer validates the framing selected in the outgoing request:
plain pkt-line, side-band or side-band-64k with their packet-size limits.
For sideband, channel 1 carries the inner report, channel 2 progress is not
report evidence, and channel 3 or an unexpected channel invalidates evidence.
The inner report flush and outer terminal flush are separate requirements.
Partial headers or payloads, malformed packets, duplicate/out-of-order report
records and trailing report data invalidate evidence. A command marker must
match the requested command; its presence alone does not prove success.
A complete valid report distinguishes an `ok` command from an `ng`
command, including a rejection whose reason happens to be `ok`.
A non-`ok` unpack result is rejection evidence when the report meets the
same authority boundary. An advance additionally requires no session error.

This HTTP finalization change leaves the file/SSH command lifecycle unchanged;
those sessions still use the direct exported stdout observer seam.

## Safe unknown diagnostics

The error begins with `missing or incomplete receive-pack acknowledgement:`.
Its fields come from
[report_diagnostics.go](../api/internal/pushbroker/report_diagnostics.go);
the observer and finalizer are in
[raw_report.go](../api/internal/pushbroker/raw_report.go), and classification
is in [pushbroker.go](../api/internal/pushbroker/pushbroker.go).

| Field | Meaning |
|---|---|
| `case` | First matching case in the precedence table below; not a complete list of contributing faults. |
| `http`, `attached`, `eligible` | HTTP transport seen; current invocation's response body attached; response was HTTP 200 with no RoundTrip error. |
| `bytes` | Actual response bytes observed within the cap. |
| `sideband` | Either sideband mode was selected; there is no separate sideband-64k diagnostic field. |
| `flush` | Report flush seen: inner report flush for sideband, report flush for plain pkt-line. |
| `terminal` | Outer sideband terminal flush seen; plain pkt-line does not require this flag. An outer flush cannot substitute for the inner report flush. |
| `marker` | A command marker was seen; does not identify `ok` versus `ng`, or prove valid framing or success. |
| `invalid`, `outer_invalid`, `inner_invalid` | Semantic/report invalidity (also set on byte-budget exhaustion), outer packet-parser invalidity, inner packet-parser invalidity. |
| `outer_header`, `inner_header` | Incomplete header bytes buffered (0–3); zero can also mean a complete header awaiting payload. |
| `outer_payload`, `inner_payload` | A complete four-byte header is buffered, normally awaiting payload; check invalid flags for malformed headers. Booleans, not byte counts. |
| `budget` | The actual-byte cap was reached. The error also appends `response byte limit exhausted (1 MiB)`. |
| `eof` | The HTTP finalizer observed EOF; not proof of valid report framing. |
| `finalized` | HTTP EOF with no joined transport/read/close/context error and no exhausted budget; not proof of protocol validity, eligibility or command success. |
| `read_failure`, `close_failure` | HTTP finalizer recorded a read error or original-body close error; raw error strings are not rendered. |

The following precedence is significant: a `deadline` can mask a simultaneous
budget or close failure, so retain the accompanying flags.

| Case (in precedence order) | Trigger |
|---|---|
| `deadline` | The joined cause contains deadline expiry **or cancellation**. |
| `unattached` | HTTP was seen but no current-invocation response body was attached. |
| `budget` | Response byte budget exhausted. |
| `read_failure` | Finalizer recorded a read error, including no progress. |
| `close_failure` | Original body close failed. |
| `http_status` | HTTP response was not eligible (HTTP 200 without RoundTrip error). |
| `empty_body` | No response bytes observed. |
| `malformed` | Semantic or outer/inner parser invalidity. |
| `no_command_marker` | Framing/report completion passed but no command marker was present. |
| `session_failure` | Complete report with unpack `ok` and command `ok`, but a session cause remains. |
| `incomplete` | Fallback when none of the preceding cases matches, such as missing flush or a partial packet. |

These are bounded metadata diagnostics, not a raw protocol dump. They render
neither the command ref nor remote report/rejection text, URLs or credentials;
wrapped causes keep error identity without printing untrusted cause strings.
The workersvc error path also applies credential scrubbing. The worker publish
API response remains generic **HTTP 500 `internal error`** on this error path;
the diagnostic detail belongs in operator API logs, not the response or feed.

## Compare before and after deployment

1. Record the deployed API revision containing the finalizer, deployment time,
   worker revisions and log-retention/collection coverage. Separate rollout
   overlap from stable observation windows.
2. Choose matched **equal-duration pre/post windows**. State worker/run cohort
   coverage (including workers or runs missing logs), HTTP versus file/SSH
   coverage, and workload differences: publish volume, pack sizes, checkpoint
   cadence, forge mix, concurrency and cancellation activity. Explain material
   differences rather than attributing them to the deployment.
3. Collect timestamps, run/worker correlation identifiers and the allowlisted
   diagnostic fields above. In API logs, use `worker run publish` with the
   acknowledgement-error prefix to select these failures; its `run_id` and
   `worker_id` correlate them with worker evidence. Do not treat unrelated
   internal publish errors as acknowledgement cases.
4. On workers, use `checkpoint published to origin`,
   `checkpoint publish failed: HTTP` and
   `checkpoint publishing recovered` as anchors. Correlate by run and worker
   using log metadata or the run's worker assignment. Export selected metadata,
   not complete log records: success logs can also carry branch/tip context.
5. Report raw counts separately for API acknowledgement errors, worker failure
   feed events, recovery transitions and observed publication events. Worker
   feed failures are deduplicated by outcome key until recovery clears the set;
   recoveries are transition events. **Never derive per-attempt error rates from
   feed counts.** Worker run logs can repeat a failure even when the feed does
   not; the success anchor covers selected publication paths, not a complete
   attempt denominator. API publish error logs likewise do not enumerate all
   attempts.
6. If complete access/request logs supply a publish-attempt denominator, state
   its source, cohort, exclusions and raw total for each window. Align the
   acknowledgement-error numerator to those same attempts and distinguish
   skips, pre-invocation errors and other failures. If that denominator is
   unavailable, report counts/events per hour and explicitly state
   **per-attempt error rate unknown**.
7. Break down the remaining unknowns by `case` with raw counts and accompanying
   flags, keeping errors without these diagnostics in a separate bucket.
   Report cancellations separately from timeouts using request context or
   lifecycle metadata: `case=deadline` alone combines them and cannot make
   that distinction. Where metadata is missing, keep cancel/timeout unresolved.

Do not collect or print raw remote text, refs, URLs or credentials in the
diagnostic comparison. Keep correlation identifiers in controlled operator
records; redact them from public reports.

The shipped boundary makes HTTP acknowledgement handling deterministic and
bounded. **Production causality and resolution of
[#2475](https://github.com/vtmocanu/uzi/issues/2475) remain unproven.**
A lower event count without matched cohorts and an appropriate denominator
does not establish a lower per-attempt failure rate or identify the original
production cause.
