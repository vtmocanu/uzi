# ADR 0296: Bounded Codex text reads

Status: accepted; issue #296, milestone 1.

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
guards remain authoritative. Range arguments never reach the helper. It still
reads the whole allowed file and denies files over 1 MiB; this change is not a
larger-file reader or a Go protocol change.

## Helper success validation

After a successful helper response, the broker validates in this exact order:

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

The lead owns repository gates and commits. Runtime missing dependencies must be
reported as blocked checks, never as passing behavioral evidence.
