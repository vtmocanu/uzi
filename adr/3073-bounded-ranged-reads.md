# ADR 3073: Bounded ranged reads through fileop

Status: implemented; M1 helper primitive (69560dd5), M2 broker integration
(d6066825, hardened in 75071337).

## Context

[ADR 0296](0296-codex-bounded-text-reads.md) provides bounded text excerpts after
an ordinary helper read. The ordinary operation refuses files larger than 1 MiB.
Stat supplies metadata, and read supplies an entire capped body; neither can
retrieve a prefix from a larger file. A broker pathname reopen would bypass the
helper's authority. A new read_range primitive keeps opening and streaming under
the existing dirfd and kernel containment.

## Decision and integration seam

The Go helper's read_range independently requires numeric, positive,
integer offset and limit: offset is one-based and at most 9007199254740991; limit
is at most 2000. Missing, null, string, fractional and overflowing values fail with
E_MALFORMED before opening. Other operations ignore extra range fields.

The broker performs ordinary read first. An explicitly supplied offset or limit,
including an explicit default, plus a helper-origin E_OVERSIZE invokes read_range
exactly once. Unranged calls, other errors and broker validation failures after
read success have no fallback. Successful ordinary reads retain whole-body binary
checks and excerpt behavior from ADR 0296, even when binary bytes lie beyond the
requested excerpt.

Helper and client share failure codes; the code alone does not establish origin.
The client mints origin=helper after matching a response to a pending request ID,
with exactly ok=false and a string code. A wire origin field is ignored.
Client-generated serialization, request-size, transport, timeout and
malformed-response failures do not acquire helper provenance. Range failure or
malformed range success ends the callback without another read.

Every range open validates the path, then uses the existing openBeneath with
O_RDONLY, O_NOFOLLOW and O_NONBLOCK. Kernel RESOLVE_BENEATH, RESOLVE_NO_SYMLINKS
and RESOLVE_NO_MAGICLINKS remain authoritative. Fstat must identify a regular file
before clearing nonblocking mode. Descriptor ownership immediately installs close
cleanup, including type, stat, nonblock, encoding, read and timeout failures.
There is no pathname reopen or seek fallback.

Each operation's descriptor pins its inode. The range scanner reads only up to
its opening fstat size, ignores later growth, accepts early EOF on shrinkage, and
survives path replacement. Its response size is the opening size, not the current
pathname size. Ordinary read retains its existing size admission and LimitReader
cap, including the growth case below.
Two operations are two independent opens: the first read can observe an older
state and the range operation a newer state. All range data and metadata come from
the second descriptor; no bytes or size from the failed first operation are mixed
into it. This guarantees coherent descriptor/extent provenance, not a transactional
snapshot against in-place writes. A replacement, growth or shrinkage between the
two opens can therefore change the returned content and size; even a now-small
second file follows the fallback's prefix validation rules.

## Bounds and text frontier

Actual bytes read by read_range, including prefetch and raw lookahead, are capped
at 8 MiB.
Read chunks are at most 32 KiB and returned decoded bytes at most 64 KiB.
The first ordinary read can consume up to 1 MiB+1 if the file grows after Fstat;
combined with the fallback, this is at most 9,437,185 actual bytes (9 MiB+1).
The scanner uses fixed read/output buffers and a four-byte UTF-8 carry, with no
allocation proportional to source line length or count. Processing makes bounded
validation passes with cooperative context checks. A giant line cannot force a
line-sized allocation.

LF is included in its source line, BOM and CRLF are preserved, and terminal LF
does not create a phantom line. Validation covers skipped and selected bytes
through the requested line end, or the output ceiling rounded forward only to
finish a crossing codepoint. Output retreats to the preceding UTF-8 boundary
when that codepoint cannot fit. A raw byte after a completed boundary establishes
continuation without decoding the next character. Prefetched surplus counts
against the read budget but lies outside validation. Thus binary/NUL beyond the
frontier is irrelevant; invalid UTF-8 or NUL within it yields E_NOT_TEXT, mapped
by the broker to bad_args. This prefix-only validation applies to fallback, not
to successful ordinary reads.
Incomplete UTF-8 at observed EOF is binary; at the scan cap it is incomplete and
is omitted from output. Acceptance and excerpt metadata are independent of chunk
size; scannedBytes may vary with prefetch.

The helper response has id/ok and canonical base64 data, size, offset, linesReturned,
partialLastLine, truncated, incomplete, scannedBytes. linesReturned counts source
lines represented even partly. partialLastLine is true only when the excerpt
ends inside a continuing source line or delimiter; it is false at LF or complete
unterminated EOF. truncated means bytes remain after the excerpt; skipping alone
does not truncate. Empty and past-EOF results are completed, with zero lines and
neither partial nor truncated. Scan exhaustion before offset yields empty data,
zero lines, nonpartial, truncated, incomplete, with reason scan_budget. Exhaustion
after reaching the range returns a valid prefix with accurate partial status.
Only incomplete successes carry reason scan_budget; complete successes explicitly
carry incomplete false and omit reason. Other failures retain neutral existing
codes.

A five-second monotonic context is checked before open, before and after reads,
through EINTR retries and while processing. Cancellation/deadline gives E_TIMEOUT.
This deadline is cooperative: regular-filesystem open/stat/read calls can stall
and are not claimed interruptible. The client has a separate 30-second
timer per helper request. Read first plus one fallback allows up to 60 seconds
of sequential timer allowances plus overhead. Neither this sum nor the cooperative
helper context guarantees absolute callback or storage completion.

## Broker result and activity

The broker bounds encoded input before decoding and validates canonical base64
by round-trip, then validates size, echoed offset, scannedBytes, line counts,
partial/truncated/incomplete flags and reason. It rejects contradictory metadata,
including completed EOF at the 8 MiB scan cap when opening size is larger, for
empty or nonempty output. Returned
text must be valid UTF-8 without NUL, with line counts matching the excerpt.

Public output retains size/content/offset/linesReturned/partialLastLine/truncated
without duplicate base64 or a cursor. Only incomplete fallback successes add
incomplete=true, reason=scan_budget and scannedBytes; completed results omit those
three fields. Helper IDs, provenance and raw helper diagnostics are not exposed.
Root and child serialization retain the existing 4 MiB transport cap.

Cross-check activity settles incomplete reads as limit, excluding them from
successful read evidence even when content is nonempty. Completed empty/past-EOF
reads remain ok without line evidence; text refusal is an error, and helper/client
E_TIMEOUT is limit activity. No dependency, workflow, or ordinary read cap change
is required.

## Verification boundary

Focused M1 tests exercise the production scanner, actual openat2 path and helper
NDJSON entrypoint. Byte and close instrumentation covers early large-file prefixes,
scan/output/chunk frontiers, skipped and selected UTF-8, surplus binary data,
opening growth/shrink/replacement and deterministic syscall/context failures.
One test grows an ordinary read after Fstat and measures exactly 1 MiB+1 bytes
before E_OVERSIZE, then independently opens a distant range and measures 8 MiB:
9 MiB+1 combined, without combining the operations' states.

M2 adds focused broker/client tests for read-first sequencing, exactly one
helper-origin oversize fallback, local-error and forged-origin exclusion, range
metadata validation, and scan-budget results. The real-helper broker regression
covers growth, shrinkage, replacement, deletion, symlink/FIFO refusal and prefix
binary boundaries between the independently confined opens. Root and child tests
exercise escaping-heavy and incomplete results through emitted transport frames;
cross-check tests verify limit activity without successful read evidence.

Enforcing boundaries are dispatchFileRead/validateRangeRead in
[broker.ts](../agent/src/codex/broker.ts), response correlation/coercion in
[fileop-client.ts](../agent/src/codex/fileop-client.ts), confined scanning in
[read_range.go](../agent/codex/supervisor/fileop/read_range.go), and settleActivity in
[cross-check.ts](../agent/src/codex/cross-check.ts). Focused regressions live in
[codex-broker.test.ts](../agent/test/codex-broker.test.ts),
[codex-fileop-client.test.ts](../agent/test/codex-fileop-client.test.ts),
[codex-ranged-read.test.ts](../agent/test/codex-ranged-read.test.ts),
[codex-harness.test.ts](../agent/test/codex-harness.test.ts),
[codex-delegation.test.ts](../agent/test/codex-delegation.test.ts) and
[codex-cross-check.test.ts](../agent/test/codex-cross-check.test.ts).

Recorded M1/M2 evidence establishes the implemented contract and focused in-run
checks, including a failing original broker regression and passing integrated
budget probes. It does not establish final repository gates or live acceptance.
Final gates remain lead-owned; full agent suite: exact-head PR CI. Landing-session
worker-UID acceptance and CI step/job/critical-path timing remain separate
verification obligations; no unrun final gate, live check or CI result is claimed.
