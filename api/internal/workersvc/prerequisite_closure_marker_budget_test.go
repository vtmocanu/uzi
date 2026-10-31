package workersvc

import (
	"context"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

type prerequisiteMarkerBudgetStore struct {
	*prerequisiteMemory
	t               *testing.T
	markerContext   context.Context
	markerCommitted bool
	postMarkerReads int
}

func (q *prerequisiteMarkerBudgetStore) GetRepoForUser(ctx context.Context, a store.GetRepoForUserParams) (store.GetRepoForUserRow, error) {
	if q.marked > 0 {
		q.postMarkerReads++
		if !q.markerCommitted || q.inTX.Load() || q.row.State != "invoked" {
			q.t.Fatal("post-marker binding must follow marker commit")
		}
		markerDeadline, ok := q.markerContext.Deadline()
		if !ok {
			q.t.Fatal("marker transaction has no deadline")
		}
		bindingDeadline, ok := ctx.Deadline()
		if !ok {
			q.t.Fatal("post-marker binding has no deadline")
		}
		if !bindingDeadline.After(markerDeadline) {
			q.t.Fatal("post-marker binding reused spent transaction deadline")
		}
		if err := ctx.Err(); err != nil {
			q.t.Fatalf("post-marker binding context already canceled: %v", err)
		}
		if err := q.markerContext.Err(); err != context.Canceled {
			q.t.Fatalf("marker context was not canceled at commit: %v", err)
		}
	}
	return q.prerequisiteMemory.GetRepoForUser(ctx, a)
}

func TestPrerequisitePostCommitBindingSurvivesCanceledMarkerContext(t *testing.T) {
	s, m, h, _ := newPrerequisiteMemory(t, 1)
	q := &prerequisiteMarkerBudgetStore{prerequisiteMemory: m, t: t}
	txCalls := 0
	s.SetPrerequisiteStore(q, func(ctx context.Context, fn func(PrerequisiteStore) error) error {
		txCalls++
		// The first transaction is eligibility preflight; the second writes the marker.
		marker := txCalls == 2
		if marker {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			defer cancel()
			q.markerContext = ctx
		}
		err := m.transaction(ctx, func(PrerequisiteStore) error {
			return fn(q)
		})
		if marker && err == nil && m.marked > 0 {
			q.markerCommitted = true
		}
		return err
	})

	if _, err := s.prerequisiteOne(context.Background(), q, m.row, newPrerequisitePass(m.now)); err != nil {
		t.Fatal(err)
	}
	if m.marked != 1 || q.postMarkerReads != 1 || m.row.State != "pinned" || h.sends != 1 {
		t.Fatalf("marked=%d post-marker reads=%d state=%s sends=%d", m.marked, q.postMarkerReads, m.row.State, h.sends)
	}
	if h.ioInTX.Load() {
		t.Fatal("forge I/O occurred inside a transaction")
	}
	if err := q.markerContext.Err(); err != context.Canceled {
		t.Fatalf("marker context was not cleaned up: %v", err)
	}
}
