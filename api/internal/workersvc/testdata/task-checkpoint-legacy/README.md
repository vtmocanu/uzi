# Historical admission SQL

These raw SQL literals and ordered parameter-field manifests come from commit
`03d273fde1901d4185ac7590bae4b6a9f78ef8ea`:

- `api/internal/store/runtime.sql.go`: `claimRun`, `ClaimRunParams`, `ClaimRun`.
- `api/internal/store/runtime.sql.go`: `readoptRunsFromSnapshot`, scalar worker ID.
- `api/internal/store/worker_recovery.sql.go`: `frozenReadoptRunsFromSnapshot`,
  `FrozenReadoptRunsFromSnapshotParams`.

The SQL files preserve the generated raw string bytes, including the name comment
and final newline. Extraction evaluates Go string literals and concatenation;
ClaimRun contains backticks represented by concatenated string literals. Do not
regenerate these fixtures with current sqlc. Tests read these fixtures without invoking Git,
execute via tx.Exec to discard the historic RETURNING projection, and bind fields
in the archived method argument order. Readoption cases cover the four phases declared by migration
00236_worker_active_runs_snapshot.sql: running, awaiting_approval, awaiting_input,
awaiting_followup.

The historical process never calls OptInCheckpointCoordinates. Both capable and
incapable persisted workers must be refused for an already enrolled task. Full
run and custody row snapshots pin rollback, including pre-existing custody and
the absence of a generation-2 hold.
