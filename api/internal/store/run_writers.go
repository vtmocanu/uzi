package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrRunWritersRepoNotFound means the repo id named no repo.
var ErrRunWritersRepoNotFound = errors.New("run writers: repo not found")

// ListRunWriters returns up to maxRows active ci_fix / mr_rework runs, across all owners,
// that write the named branch of the forge project repoID belongs to (mrIID additionally
// selects mr_rework runs of that MR). Both statements run in one REPEATABLE READ READ ONLY
// transaction so the repo's forge identity and the run set come from one snapshot. Callers
// probe completeness by asking for one more row than they serve.
func ListRunWriters(ctx context.Context, db cordonTxBeginner, repoID uuid.UUID, branch string, mrIID *int64, maxRows int32) ([]ListRunWritersForTargetRow, error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)
	target, err := q.ResolveRunWritersTarget(ctx, repoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRunWritersRepoNotFound
		}
		return nil, err
	}
	var mr pgtype.Int8
	if mrIID != nil {
		mr = pgtype.Int8{Int64: *mrIID, Valid: true}
	}
	return q.ListRunWritersForTarget(ctx, ListRunWritersForTargetParams{
		ForgeType:      target.ForgeType,
		BaseUrl:        target.BaseUrl,
		ForgeProjectID: target.ForgeProjectID,
		Branch:         branch,
		MrIid:          mr,
		MaxRows:        maxRows,
	})
}
