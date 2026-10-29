# ADR 0296: Bounded Codex text reads

Status: accepted; issue #296, milestone 1. Updated 2026-10-29.

## Context and evidence

Codex Read previously forwarded helper base64 for every file. Model consumers need
readable excerpts with bounded serialized results. Both CodexHarness.stringifyOutput
and CodexDelegationRunner.stringify JSON-encode the broker output, then the transport
JSON-encodes the inputText reply again. A near-1 MiB control-character text body can
therefore exceed the transport's 4 MiB outbound frame cap if returned whole as text.
A 64 KiB UTF-8 excerpt bounds that expansion without changing either consumer or the
transport. Quotes and backslashes also expand through both encodings.

Reachable local history was consulted: broker commit 61f28161 records the prior
verbatim-base64 decision; helper commits 6dd9282f and 2a30068e establish the openat2
boundary and path/special-file protections. The old decision correctly prevented
corruption from slicing encoded base64 but did not provide text excerpts.
The inspiration directory is absent and external prior-art clones are unavailable
in this worker; no comparative implementation claim is made.

## Read arguments and authority

Read accepts either path or file_path, retaining their existing precedence and
alias behavior. Optional offset is a positive safe integer, one-based, default 1.
Optional limit is a positive integer at most 2000, default 200. The dynamic-tool
schema advertises those bounds and defaults. The broker independently validates
both before invoking fileop: null, strings, fractions, zero, negative and unsafe
numbers are bad_args. Presence of either property is an explicit range, including
an explicitly supplied default. An explicitly present undefined is invalid.

Registry admission, grants, replay, secret-path checks and worktree/no-symlink
guards remain authoritative. The original decision kept all ranges in the broker
and retained the helper's 1 MiB ceiling. That remains the legacy path and the
unranged contract. The approved extension permits **explicit ranged text only**:
first call legacy read; only its failed E_OVERSIZE response permits retrying with
read_text_range and the validated offset/limit. Malformed or oversized successes
and all other failures never trigger a retry. Search continues using legacy raw
reads with its 64 KiB per-file and 256 KiB total budgets. Supplying either default
explicitly opts in; omitted defaults do not. (AI-synced 2026-10-29)

## Explicit large text scan

The new helper operation independently validates offset as a positive safe JS
integer and limit in 1–2000. It uses the same openat2 containment, nonblocking
open and fstat regular-file checks. It scans the observed descriptor to EOF with
a fixed 4096-byte reader, constant UTF-8 state and at most 64 KiB retained text;
no source line, file or line index accumulates. Actual consumed input, including
growth after stat, is capped at 64 MiB plus a one-byte overflow probe. Exceeding
that budget returns E_OVERSIZE with a fixed path-free explanation at the broker.
The successful size is the actual bytes scanned. No snapshot is promised.

Every observed byte is checked for UTF-8 and NUL, including omitted prefix,
suffix and bytes after the excerpt fills. Binary success carries only the binary
discriminator (besides correlation id and ok); it has no body or text metadata.
The broker maps it to the existing bad_args text-file requirement. Text success
carries kind=text, canonical base64 data, size, offset, linesReturned,
partialLastLine and truncated, with the same line semantics below.

The client tracks the pending operation and validates this raw discriminated
schema before legacy coercion, rejecting unknown/forbidden fields and malformed
types. The broker independently checks the discriminator, encoded length before
decoding, canonical base64 including padding bits, decoded 64 KiB bound, UTF-8/NUL,
safe numeric bounds and metadata consistency. Failures remain neutral E_MALFORMED
or E_OVERSIZE. An old helper's unsupported operation fails closed. The 2 MiB client
framing and 30-second request deadline remain unchanged; timeout settles the
request but does not cancel the helper scan, and late replies cannot settle a
new request. Package and supervisor authority are unchanged.

## Helper success validation

After a successful **legacy read** response, the broker validates in this exact order:

1. Missing or nonstring data gives E_MALFORMED.
2. Encoded length above 4 * ceil(1 MiB / 3) gives E_OVERSIZE, before regex or decoding.
3. Base64 must have canonical alphabet, length and padding and round-trip exactly
   through decoding/re-encoding, including zero padding bits; otherwise E_MALFORMED.
4. Decoded length above 1 MiB gives E_OVERSIZE, before inspecting metadata.
5. size must be a nonnegative safe integer at most 1 MiB and equal decoded length.
   Only truncated === true permits size greater than or equal to decoded length,
   still at most 1 MiB. Invalid metadata gives E_MALFORMED.

All such failures use the existing neutral mapFileopError vocabulary. No raw
helper payload, path or diagnostic is echoed. The production helper emits no
truncated success flag; defensive compatibility with mocks is retained.

## Text and binary results

Fatal UTF-8 decoding with ignoreBOM: true preserves an initial BOM. Valid UTF-8
without NUL is text, including other controls. Invalid UTF-8 or NUL is binary.

Unranged binary returns exactly {size, contentBase64, truncated}, with canonical
base64 unchanged and the helper's strict-true flag preserved. It is never clipped,
and base64 is never sliced. Binary with any explicit range gives bad_args.

Text returns only {size, content, offset, linesReturned, partialLastLine, truncated};
there is no duplicate base64. Logical lines are LF-separated and retain LF/CRLF
delimiters, blank lines, Unicode and BOM. A trailing LF creates no phantom final
line. Empty files and offsets past EOF return empty content and zero lines.

The selected offset/limit range is clipped to at most 64 KiB of UTF-8 bytes,
retreating to a complete codepoint when necessary. No ellipsis is appended.
linesReturned counts source lines represented, including a partially represented
final line. partialLastLine is true if the byte cap stops inside a source line or
its delimiter, including between CR and LF. It is false after a full delimiter
or at complete unterminated EOF.

truncated is true when the helper's defensive flag is strictly true or bytes
remain after the returned excerpt because of line/byte limits. Skipping earlier
lines alone does not set it. Exact EOF, empty files and past EOF are false unless
the helper flag is true. No continuation cursor is promised: a partially returned
line cannot be resumed using a line offset alone.

## Validation and scope

Focused broker and schema tests cover defaults, aliases, line boundaries, UTF-8,
BOM, invalid ranges without helper calls, ordered helper validation, binary
round-trips and access denials. Root and child integration tests use actual emitted
transport frames and complete only after receiving the Read reply. They cover
excerpt metadata with no duplicate base64, callback settlement, and near-1 MiB
U+0001 and quote/backslash bodies clipped and delivered below the 4 MiB frame cap.

The source-bound Linux regression builds the helper from the same source tree,
then drives the real client and broker for a distinct middle excerpt, unchanged
unranged denial, the production 64 MiB boundary and a following request. Non-Linux
may skip it; Linux must build and run. Root, child and cross-check tests exercise
large ranged output through their emitted replies, including escaping, fencing,
activity ranges and callback settlement.

The lead owns repository gates, base/candidate red-green proof and commits. Runtime missing dependencies must be
reported as blocked checks, never as passing behavioral evidence.
