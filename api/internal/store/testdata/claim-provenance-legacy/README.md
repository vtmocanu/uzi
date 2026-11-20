# Pre-#4476 ClaimRun

`legacy-ClaimRun.sql` is the raw `claimRun` string literal and `legacy-ClaimRun-bindings.json` the
argument order of `ClaimRun`, both taken from `api/internal/store/runtime.sql.go` at commit
`908b978ea` (the base of issue #4476, the image an operator rolls back to). Backtick concatenations
in the literal were evaluated. Do not regenerate with current sqlc. It never writes the claim
provenance columns; the rollback control test executes it.
