package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type salvageCoordinateLookupStore struct {
	Store
	lookupErr error
	calls     int
}

func (s *salvageCoordinateLookupStore) GetRunByID(_ context.Context, id uuid.UUID) (store.Run, error) {
	s.calls++
	return store.Run{ID: id, Kind: "task"}, s.lookupErr
}

// A failed task lookup must be reported without stopping pending/expiry work.
// Missing rows and unenrolled tasks remain benign skips.
func TestSweepSalvageTaskCoordinateLookupFailure(t *testing.T) {
	failure := errors.New("task coordinate database lookup failed")
	for _, tc := range []struct {
		name      string
		lookupErr error
		wantErr   error
	}{
		{name: "database failure", lookupErr: failure, wantErr: failure},
		{name: "deleted candidate", lookupErr: pgx.ErrNoRows},
		{name: "unenrolled task"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.New()
			fs := &salvageStore{candidates: []store.ListSalvageCandidatesRow{
				{ID: id, Kind: "task", CheckpointTip: salvageTip, ForgeType: "github"},
			}}
			svc, broker := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
			q := &salvageCoordinateLookupStore{Store: fs, lookupErr: tc.lookupErr}
			svc.q = q
			touched, err := svc.SweepSalvage(context.Background())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("lookup error=%v, want %v", err, tc.wantErr)
			}
			if q.calls != 1 || touched != 0 || len(fs.inserts) != 0 {
				t.Fatalf("lookup calls=%d touched=%d inserts=%d", q.calls, touched, len(fs.inserts))
			}
			if len(fs.pendingParams) != 1 || len(fs.expiryParams) != 1 {
				t.Fatalf("remaining arms did not continue: pending=%d expiry=%d", len(fs.pendingParams), len(fs.expiryParams))
			}
			if len(broker.creates)+len(broker.deletes) != 0 {
				t.Fatal("lookup failure or unavailable coordinates caused a forge write")
			}
		})
	}
}
