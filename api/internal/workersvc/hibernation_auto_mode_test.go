package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

type autoModeSettings struct {
	auto  bool
	err   error
	reads int
}

func (*autoModeSettings) WorkerHibernationEnabled(context.Context) (bool, error) { return true, nil }
func (s *autoModeSettings) WorkerHibernationAuto(context.Context) (bool, error) {
	s.reads++
	return s.auto, s.err
}
func (*autoModeSettings) WorkerHibernationIdleAfter(context.Context) (time.Duration, error) {
	return 336 * time.Hour, nil
}

func (*autoModeSettings) WorkerHibernationSurplus(context.Context) (bool, error) { return false, nil }
func (*autoModeSettings) WorkerHibernationSurplusIdleAfter(context.Context) (time.Duration, error) {
	return 2 * time.Hour, nil
}

type autoModeDB struct {
	anchor        time.Time
	reads, writes int
}

func (d *autoModeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	d.writes++
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (*autoModeDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected query")
}
func (d *autoModeDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	return autoModeRow(func(dest ...any) error {
		if !strings.HasPrefix(sql, "-- name: GetWorkerIdleness") {
			return errors.New("unexpected row query")
		}
		d.reads++
		*dest[0].(*int64) = 0
		*dest[1].(*pgtype.Timestamptz) = pgtype.Timestamptz{Time: d.anchor, Valid: true}
		return nil
	})
}

type autoModeRow func(...any) error

func (r autoModeRow) Scan(dest ...any) error { return r(dest...) }

// Even a worker that would otherwise receive a refusal marker must be left untouched after
// automatic mode is switched off or becomes unreadable. The LiveDB test covers real admissions.
func TestAutoAdmitRechecksAutomaticMode(t *testing.T) {
	for _, name := range []string{"off", "read error"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now()
			db := &autoModeDB{anchor: now.Add(-30 * 24 * time.Hour)}
			set := &autoModeSettings{auto: true}
			svc := &Service{hibernationSettings: set, hibernationAuto: set}
			if on, err := set.WorkerHibernationAuto(ctx); err != nil || !on {
				t.Fatalf("initial pass gate: %v %v", on, err)
			}
			var wantErr error
			if name == "off" {
				set.auto = false
			} else {
				wantErr = errors.New("automatic mode unavailable")
				set.err = wantErr
			}
			row := store.Worker{ID: uuid.New(), Kind: "hosted", HibernationState: "none"}
			did, stop, err := svc.autoAdmit(ctx, store.New(db), row, now, now.Add(-14*24*time.Hour), now.Add(-time.Hour), 336*time.Hour)
			if did || stop != (wantErr == nil) || !errors.Is(err, wantErr) {
				t.Fatalf("autoAdmit=%v stop=%v err=%v, want no admission, stop=%v, err=%v", did, stop, err, wantErr == nil, wantErr)
			}
			if set.reads != 2 || db.reads != 1 || db.writes != 0 {
				t.Fatalf("auto reads=%d idleness reads=%d writes=%d; want 2/1/0", set.reads, db.reads, db.writes)
			}
		})
	}
}
