# ADR 3073: Bounded ranged reads through fileop

Status: accepted design; M1 helper primitive implemented, M2 broker integration pending.

## Context

[ADR 0296](0296-codex-bounded-text-reads.md) provides bounded text excerpts after
an ordinary helper read. The ordinary operation refuses files larger than 1 MiB.
Stat supplies metadata, and read supplies an entire capped body; neither can
retrieve a prefix from a larger file. A broker pathname reopen would bypass the
helper's authority. A new read_range primitive keeps opening and streaming under
the existing dirfd and kernel containment.

## Decision and integration seam

M1 adds read_range to the Go helper. It independently requires numeric, positive,
integer offset and limit: offset is one-based and at most 9007199254740991; limit
is at most 2000. Missing, null, string, fractional and overflowing values fail with
E_MALFORMED before opening. Other operations ignore extra range fields.

M2 must preserve ordinary read first. Only an explicit range and a helper-origin
E_OVERSIZE may invoke read_range, once. Unranged calls and other errors have no
fallback. Small-file success retains whole-body binary checks from ADR 0296.
M1 does not wire the broker or claim a regression through that broker.

Every range open validates the path, then uses the existing openBeneath with
O_RDONLY, O_NOFOLLOW and O_NONBLOCK. Kernel RESOLVE_BENEATH, RESOLVE_NO_SYMLINKS
and RESOLVE_NO_MAGICLINKS remain authoritative. Fstat must identify a regular file
before clearing nonblocking mode. Descriptor ownership immediately installs close
cleanup, including type, stat, nonblock, encoding, read and timeout failures.
There is no pathname reopen or seek fallback.

Each operation's descriptor pins its inode and opening fstat size. Streaming
ignores growth past that size, accepts early EOF on shrinkage, and survives path
replacement. The response size is the opening size, not the current pathname size.
Two operations are two independent opens: the first read can observe an older
state and the range operation a newer state. All range data and metadata come from
the second descriptor; no bytes or size from the failed first operation are mixed
into it. This guarantees coherent descriptor/extent provenance, not a transactional
snapshot against in-place writes. Broker changes between opens are M2 proof.

## Bounds and text frontier

Actual bytes read, including prefetch and raw lookahead, are capped at 8 MiB.
Read chunks are at most 32 KiB and returned decoded bytes at most 64 KiB.
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
frontier is irrelevant; invalid UTF-8 or NUL within it yields E_NOT_TEXT.
Incomplete UTF-8 at observed EOF is binary; at the scan cap it is incomplete and
is omitted from output. Acceptance and excerpt metadata are independent of chunk
size; scannedBytes may vary with prefetch.

The response has id/ok and canonical base64 data, size, offset, linesReturned,
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
and are not claimed interruptible. The intended client has a separate 30-second
timer per helper request. Read first plus one fallback allows up to 60 seconds
of timer allowances plus callback overhead; neither this sum nor the cooperative
helper context is an absolute storage deadline.

M2 will validate range response metadata, expose incomplete results and record
limit activity for scan exhaustion. Those consumer changes remain unimplemented
in M1. No dependency, workflow, or ordinary read cap change is required.

## Verification boundary

Focused M1 tests exercise the production scanner, actual openat2 path and helper
NDJSON entrypoint. Byte and close instrumentation covers early large-file prefixes,
scan/output/chunk frontiers, skipped and selected UTF-8, surplus binary data,
opening growth/shrink/replacement and deterministic syscall/context failures.
One test grows an ordinary read after Fstat and measures exactly 1 MiB+1 bytes
before E_OVERSIZE, then independently opens a distant range and measures 8 MiB:
9 MiB+1 combined, without combining the operations' states.

These are in-run focused proofs, not full agent acceptance. The lead owns the
canonical supervisor test and documentation check after integration/review;
full agent suite: exact-head PR CI. Broker sequencing, helper-origin-only fallback,
consumer metadata validation, limit activity, and regressions through the broker
remain M2. Prior ADR/spec reconciliation belongs to M3. No live runtime or
exact-head CI evidence is claimed by this helper-only milestone.
