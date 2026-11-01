package workersvc

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPrerequisiteLateDirectListingLeavesScheduleUntouched(t *testing.T) {
	for _, state := range []string{"create_unknown", "pinned"} {
		t.Run(state, func(t *testing.T) {
			s, m, h, _ := newPrerequisiteMemory(t, 1)
			prerequisiteRun(t, s, m)
			m.row.State = state
			before := m.row
			requests := h.requests.Load()
			p := newPrerequisitePass(m.now)
			m.now = m.now.Add(prerequisitePassBudget - prerequisiteListingReserve + time.Nanosecond)
			n, err := s.prerequisiteOne(context.Background(), m, m.row, p)
			if err != nil || n != 0 || !reflect.DeepEqual(before, m.row) || h.requests.Load() != requests {
				t.Fatalf("late listing changed schedule or performed I/O: n=%d err=%v", n, err)
			}
			// An earlier caller deadline must also decline before reservation.
			short, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			n, err = s.prerequisiteOne(short, m, m.row, newPrerequisitePass(m.now))
			if err != nil || n != 0 || !reflect.DeepEqual(before, m.row) || h.requests.Load() != requests {
				t.Fatal("short caller deadline changed scheduling or performed I/O")
			}
			prerequisiteRun(t, s, m)
			if h.requests.Load() != requests+1 || m.row.State != "pinned" {
				t.Fatalf("fresh pass did not list: requests=%d state=%s", h.requests.Load()-requests, m.row.State)
			}
		})
	}
}

type prerequisiteLateListingStore struct {
	*prerequisiteMemory
	late    bool
	stale   []store.RecoveryPrerequisiteClosure
	settled int
}

func (q *prerequisiteLateListingStore) ListPrerequisitePendingDue(context.Context, store.ListPrerequisitePendingDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	return nil, nil
}
func (q *prerequisiteLateListingStore) ListPrerequisiteStaleInvokedDue(context.Context, store.ListPrerequisiteStaleInvokedDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	if q.late {
		return q.stale, nil
	}
	return nil, nil
}
func (q *prerequisiteLateListingStore) ListPrerequisiteReconcileDue(_ context.Context, a store.ListPrerequisiteReconcileDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	if q.late {
		q.now = q.now.Add(7 * time.Second)
	}
	if a.AfterID == q.row.ID {
		return nil, nil
	}
	return []store.RecoveryPrerequisiteClosure{q.row}, nil
}
func (q *prerequisiteLateListingStore) ListPrerequisiteAuditDue(context.Context, store.ListPrerequisiteAuditDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	return nil, nil
}
func (q *prerequisiteLateListingStore) ListPrerequisiteCleanupDue(context.Context, store.ListPrerequisiteCleanupDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	return nil, nil
}
func (q *prerequisiteLateListingStore) SettleStalePrerequisiteInvocation(context.Context, uuid.UUID) (int64, error) {
	q.settled++
	return 1, nil
}

func TestPrerequisiteLateSweepListingConsumesNoAdmission(t *testing.T) {
	s, m, h, _ := newPrerequisiteMemory(t, 1)
	prerequisiteRun(t, s, m)
	m.row.State = "create_unknown"
	before := m.row
	q := &prerequisiteLateListingStore{prerequisiteMemory: m, late: true}
	for i := 0; i < prerequisiteMaxItems; i++ {
		q.stale = append(q.stale, store.RecoveryPrerequisiteClosure{ID: uuid.New(), State: "invoked"})
	}
	s.SetPrerequisiteStore(q, m.transaction)
	s.prerequisite.lead = 2
	requests := h.requests.Load()
	n, err := s.SweepPrerequisiteClosures(context.Background())
	if err != nil || n != 5 || q.settled != 5 || !reflect.DeepEqual(before, m.row) || h.requests.Load() != requests {
		t.Fatalf("late candidate consumed admission or changed schedule: n=%d settled=%d err=%v", n, q.settled, err)
	}
	q.late = false
	s.prerequisite.cursor[2] = prerequisiteCursor{}
	if _, err = s.SweepPrerequisiteClosures(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.requests.Load() != requests+1 || m.row.State != "pinned" {
		t.Fatal("fresh sweep did not perform actual listing")
	}
}
