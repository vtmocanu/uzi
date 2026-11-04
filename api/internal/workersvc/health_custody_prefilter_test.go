package workersvc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type healthPrefilterStore struct {
	*healthFakeStore
	owners []uuid.UUID
	err    error
	reads  []store.ListHealthCustodyCandidateOwnersParams
}

func (f *healthPrefilterStore) ListHealthCustodyCandidateOwners(_ context.Context, arg store.ListHealthCustodyCandidateOwnersParams) ([]uuid.UUID, error) {
	f.reads = append(f.reads, arg)
	return f.owners, f.err
}

func TestHealthCustodyOwnerPrefilter(t *testing.T) {
	for _, tc := range []struct {
		name                                       string
		atBound, failed, exempt, maintenance, aged bool
	}{
		{name: "below bound"},
		{name: "aged below bound", aged: true},
		{name: "at bound", atBound: true},
		{name: "read failure", failed: true},
		{name: "continuation exemption", atBound: true, exempt: true},
		{name: "maintenance precedence", atBound: true, maintenance: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := queuedCustodyRun()
			if !tc.aged {
				base.StatusSince = ago(time.Second)
			}
			base.IntakePauseHeld = tc.maintenance
			fake := &healthFakeStore{custodyHolds: int64(testCustodyLimit), custodyExempt: tc.exempt}
			for range 64 {
				r := base
				r.ID = uuid.New()
				fake.active = append(fake.active, r)
			}
			q := &healthPrefilterStore{healthFakeStore: fake}
			if tc.atBound {
				q.owners = []uuid.UUID{base.UserID}
			}
			if tc.failed {
				q.err = errors.New("prefilter unavailable")
			}
			svc := healthSvc(q, defaultHealthSettings())
			svc.detectRunHealth(context.Background(), t0)
			if len(q.reads) != 1 || q.reads[0].CustodyHoldLimit != testCustodyLimit || !reflect.DeepEqual(q.reads[0].OwnerIds, []uuid.UUID{base.UserID}) {
				t.Fatalf("one deduplicated owner read expected, got %+v", q.reads)
			}
			wantReads := 0
			if (tc.atBound || tc.failed) && !tc.maintenance {
				wantReads = 64
			}
			if len(fake.custodyCalls) != wantReads {
				t.Fatalf("per-run reads=%d want=%d", len(fake.custodyCalls), wantReads)
			}
			wantBlock := (tc.atBound || tc.failed) && !tc.exempt && !tc.maintenance
			for _, r := range fake.active {
				gotBlock := false
				for _, w := range fake.writes {
					if w.ID == r.ID && w.HealthReason.String == reasonCustodyLimit {
						gotBlock = true
					}
				}
				if gotBlock != wantBlock {
					t.Fatalf("custody block=%v want=%v", gotBlock, wantBlock)
				}
			}
			// Reuse the detector's existing unchanged-health guard, not a new
			// write suppression mechanism. Each next pass gets a fresh prefilter.
			for i := range fake.active {
				for _, w := range fake.writes {
					if w.ID == fake.active[i].ID {
						fake.active[i].Health, fake.active[i].HealthReason = w.Health, w.HealthReason
					}
				}
			}
			fake.writes = nil
			svc.detectRunHealth(context.Background(), t0)
			if len(fake.writes) != 0 || len(q.reads) != 2 {
				t.Fatalf("unchanged writes=%d pass reads=%d", len(fake.writes), len(q.reads))
			}
		})
	}
}

func TestHealthCustodyOwnerPrefilterBoundaryLiveDB(t *testing.T) {
	for _, own := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprintf("holds_%d_limit_2", own), func(t *testing.T) {
			generations := make([]int64, own)
			for i := range generations {
				generations[i] = int64(i + 1)
			}
			c, _ := seededQueuedCustody(t, 3, 3, 4, generations, 2)
			owners, err := c.env.q.ListHealthCustodyCandidateOwners(c.env.ctx, store.ListHealthCustodyCandidateOwnersParams{OwnerIds: []uuid.UUID{c.userID}, CustodyHoldLimit: 2})
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if own >= 2 {
				want = 1
			}
			if len(owners) != want || (want == 1 && owners[0] != c.userID) {
				t.Fatalf("owners=%v expected count=%d", owners, want)
			}
		})
	}
}
