package hostedsvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type pressureStore struct {
	*fakeStore
	calls []string
	rows  int64
	err   error
}

func (s *pressureStore) CordonHostedWorkerPressureRecycle(_ context.Context, _ uuid.UUID) (int64, error) {
	s.calls = append(s.calls, "cordon")
	return s.rows, s.err
}
func (s *pressureStore) UncordonHostedWorkerPressureRecycle(_ context.Context, _ uuid.UUID) (int64, error) {
	s.calls = append(s.calls, "uncordon")
	return s.rows, s.err
}
func TestPressureRecycleDispatch(t *testing.T) {
	ctx := context.Background()
	for _, rows := range []int64{0, 1} {
		for _, boom := range []error{nil, errors.New("store failure")} {
			f := &pressureStore{fakeStore: newFakeStore(), rows: rows, err: boom}
			s := newTestService(t, f)
			for _, call := range []func(context.Context, uuid.UUID, ...string) (bool, error){s.Cordon, s.Uncordon} {
				found, err := call(ctx, uuid.New(), "pressure_recycle")
				if found != (rows > 0) || !errors.Is(err, boom) {
					t.Fatalf("found=%v err=%v", found, err)
				}
			}
			if len(f.calls) != 2 || len(f.cordoned) != 0 || len(f.uncordoned) != 0 {
				t.Fatal("pressure dispatch fell through")
			}
		}
	}
	s := newTestService(t, newFakeStore())
	if _, err := s.Cordon(ctx, uuid.New(), "pressure_recycle"); err == nil {
		t.Fatal("unsupported cordon used generic store")
	}
	if _, err := s.Uncordon(ctx, uuid.New(), "pressure_recycle"); err == nil {
		t.Fatal("unsupported uncordon used generic store")
	}
}
func TestPollNeverRegistered(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*store.ListHostedWorkersForControllerRow)
	}{
		{"fresh", func(r *store.ListHostedWorkersForControllerRow) {}},
		{"online", func(r *store.ListHostedWorkersForControllerRow) { r.Status = "online" }},
		{"legacy_heartbeat", func(r *store.ListHostedWorkersForControllerRow) { r.LastHeartbeatAt = pgtype.Timestamptz{Valid: true} }},
		{"legacy_empty_version", func(r *store.ListHostedWorkersForControllerRow) { r.Version = pgtype.Text{Valid: true} }},
		{"empty_nonce", func(r *store.ListHostedWorkersForControllerRow) { r.SnapshotRegisterNonce = pgtype.Text{Valid: true} }},
		{"pod", func(r *store.ListHostedWorkersForControllerRow) { r.RegisteredPodUid = pgtype.UUID{Valid: true} }},
		{"roll_heartbeat", func(r *store.ListHostedWorkersForControllerRow) { r.RollHeartbeatAt = pgtype.Timestamptz{Valid: true} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := store.ListHostedWorkersForControllerRow{Status: "offline"}
			tc.mutate(&row)
			f := newFakeStore()
			f.workers = []store.ListHostedWorkersForControllerRow{row}
			out, err := New(f, nil, time.Now, time.Minute).Poll(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			e := out.Workers[0].RollEvidence
			if tc.name == "fresh" {
				if e.NeverRegistered == nil || !*e.NeverRegistered {
					t.Fatal("fresh row lacks positive evidence")
				}
			} else if e.NeverRegistered == nil || *e.NeverRegistered {
				t.Fatal("historical evidence lacks explicit negative evidence")
			}
			if e.Registered != row.SnapshotRegisterNonce.Valid {
				t.Fatal("nonce-backed registered changed")
			}
		})
	}
}
