# outcome-discard fixtures (issue #2984)

Shared by the Go api (`api/internal/outcomeloss/outcomeloss_test.go`) and the TS worker
(`agent/test/`): both build the canonical loss manifest with their own production code and must
produce the same bytes and digest.

- `canonical.json`: the canonical manifest JSON followed by ONE trailing LF. Strip that single LF
  to get the exact canonical bytes. It exercises backslashes, non-ASCII bytes escaped as `\xNN`,
  control bytes, ignored `!!` entries with size and sha256, a symlink target, a deletion, a
  rename, kept sibling journal and finalize generations, and unpublished commits.
- `digest.txt`: the expected `loss_sha256` plus a trailing LF, `sha256("uzi-outcome-loss-v1\n" || canonical bytes)`.
- `escape-cases.json`: raw bytes (hex) and their escaped-ASCII spelling.

The digest was cross-checked with an independent Python hashlib computation when recorded.
