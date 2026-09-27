package workersvc

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestBackfillWatermark pins what a backfill page proves (PRD #1810 M4 review): NULL ("up to now")
// only for a page that held every candidate and recorded each; a full page up to its last row; an
// unrecorded run caps the watermark at its own status_since so it is re-listed and never skipped.
func TestBackfillWatermark(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	page := func(n int) []store.ListCheckpointRetentionBackfillRow {
		rows := make([]store.ListCheckpointRetentionBackfillRow, n)
		for i := range rows {
			rows[i].StatusSince = pgtype.Timestamptz{Time: base.Add(time.Duration(i) * time.Minute), Valid: true}
		}
		return rows
	}
	all := func(n int) []bool {
		r := make([]bool, n)
		for i := range r {
			r[i] = true
		}
		return r
	}
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Minute) }

	if got := backfillWatermark(nil, nil, false); got.Valid {
		t.Fatalf("empty page = %v, want NULL (up to now)", got.Time)
	}
	if got := backfillWatermark(page(3), all(3), false); got.Valid {
		t.Fatalf("partial page, all recorded = %v, want NULL (up to now)", got.Time)
	}
	if got := backfillWatermark(page(10), all(10), true); !got.Valid || !got.Time.Equal(at(9)) {
		t.Fatalf("full page = %v (valid %v), want its last row %v", got.Time, got.Valid, at(9))
	}
	rec := all(3)
	rec[1] = false
	if got := backfillWatermark(page(3), rec, false); !got.Valid || !got.Time.Equal(at(1)) {
		t.Fatalf("partial page with an unrecorded run = %v (valid %v), want capped at it %v", got.Time, got.Valid, at(1))
	}
	rec = all(10)
	rec[4], rec[7] = false, false
	if got := backfillWatermark(page(10), rec, true); !got.Valid || !got.Time.Equal(at(4)) {
		t.Fatalf("full page with unrecorded runs = %v (valid %v), want the earliest %v", got.Time, got.Valid, at(4))
	}
}
