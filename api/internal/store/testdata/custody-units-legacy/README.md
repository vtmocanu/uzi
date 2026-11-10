# Historical ClaimRun for custody admission units

The raw SQL literal and ordered parameter-field manifest come from commit
`e7c7d897b4d0477260fc35fdfc0e2959d6f34e31`, the base of issue #4162 and the image
an operator rolls back to:

- `api/internal/store/runtime.sql.go`: `claimRun`, `ClaimRunParams`, `ClaimRun`.

`legacy-ClaimRun.sql` preserves the generated raw string bytes, including the name
comment and final newline. Extraction evaluates Go string literals and concatenation;
`claimRun` contains backticks represented by concatenated string literals. Do not
regenerate these fixtures with current sqlc.

This capture is separate from
`api/internal/workersvc/testdata/task-checkpoint-legacy`, which comes from commit
`03d273fd` and runs on the shared workersvc database. This one runs on an isolated
database (`custody_admission_units_migration_livedb_test.go`) so goose state and the
rollback script `scripts/custody-admission-units-rollback.sql` can be exercised.

The test reads the fixtures without invoking Git, executes via tx.Exec (discarding the
historic RETURNING projection) in a transaction it always rolls back, and binds fields
in the archived method argument order. It documents the hazard of the previous image on
grouped accounting: with migration 00424 applied, this ClaimRun admits a queued run
whose own open holds already equal the limit; after the rollback script it refuses.
