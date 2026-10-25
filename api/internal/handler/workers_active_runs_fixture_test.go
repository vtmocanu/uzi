package handler

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// activeRunsDeleteStore builds the DeleteWorker store for the active-runs 409 tests: the
// count answers total, the listing answers the seeds, and the returned func reports
// whether the delete was reached.
func activeRunsDeleteStore(t *testing.T, runs []activeRunSeed, total int64, holds int64) (workersvc.Store, func() bool) {
	t.Helper()
	rows := make([]store.ListWorkerNonTerminalRunsRow, 0, len(runs))
	for _, r := range runs {
		row := store.ListWorkerNonTerminalRunsRow{
			ID: uuid.MustParse(r.id), Kind: r.kind, Status: r.status, DisplayTitle: r.title,
		}
		if r.iid != nil {
			row.IssueIid = pgtype.Int8{Int64: *r.iid, Valid: true}
		}
		rows = append(rows, row)
	}
	st := &deleteWorkerStore{activeRuns: total, listRuns: rows, holds: holds}
	return st, func() bool { return st.deleted }
}
