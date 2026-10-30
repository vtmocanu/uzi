package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func (m *prerequisiteMemory) ReservePrerequisiteAudit(_ context.Context, a store.ReservePrerequisiteAuditParams) (store.RecoveryPrerequisiteClosure, error) {
	r := &m.row
	if r.State != "pinned" || r.CleanupDeadline.Valid && r.CleanupDeadline.Time.After(m.now) || r.LastAuditPassID == a.PassID {
		return *r, pgx.ErrNoRows
	}
	r.LastAuditPassID = a.PassID
	r.AuditNextAt = prerequisiteTime(m.now.Add(24 * time.Hour))
	return *r, nil
}
func (m *prerequisiteMemory) ReservePrerequisiteCleanup(_ context.Context, a store.ReservePrerequisiteCleanupParams) (store.RecoveryPrerequisiteClosure, error) {
	r := &m.row
	attempt := r.CleanupAttempts
	if r.State != "pinned" || !prerequisiteCleanupEligible(m.capture) || attempt < 0 || attempt >= 8 || r.NextAttemptAt.Time.After(m.now) || r.CleanupDeadline.Valid && r.CleanupDeadline.Time.After(m.now) {
		return *r, pgx.ErrNoRows
	}
	r.CleanupAttempts++
	r.LastAuditPassID = a.PassID
	r.CleanupDeadline = prerequisiteTime(m.now.Add(8 * time.Second))
	r.NextAttemptAt = prerequisiteTime(m.now.Add(8*time.Second + min(time.Hour, 30*time.Second<<uint(attempt))))
	return *r, nil
}
func (m *prerequisiteMemory) pinnedFence(id uuid.UUID, g int32, p pgtype.UUID) bool {
	return m.row.ID == id && m.row.State == "pinned" && m.row.CleanupAttempts == g && m.row.LastAuditPassID == p
}
func (m *prerequisiteMemory) RecordPrerequisiteDeleted(_ context.Context, a store.RecordPrerequisiteDeletedParams) (int64, error) {
	if !m.pinnedFence(a.ID, a.Generation, a.PassID) || !m.row.CleanupDeadline.Time.After(m.now) || !prerequisiteCleanupEligible(m.capture) {
		return 0, nil
	}
	m.row.State = "deleted"
	m.row.LiveRunID = pgtype.UUID{}
	return 1, nil
}
func (m *prerequisiteMemory) ConfirmPrerequisiteCleanupAbsent(_ context.Context, a store.ConfirmPrerequisiteCleanupAbsentParams) (int64, error) {
	if !m.pinnedFence(a.ID, a.Generation, prerequisiteID(a.PassID)) || m.row.CleanupDeadline.Valid && m.row.CleanupDeadline.Time.After(m.now) || !prerequisiteCleanupEligible(m.capture) {
		return 0, nil
	}
	m.row.State = "deleted"
	m.row.LiveRunID = pgtype.UUID{}
	return 1, nil
}
func (m *prerequisiteMemory) RecordPrerequisiteCleanupRetry(_ context.Context, a store.RecordPrerequisiteCleanupRetryParams) (int64, error) {
	r := &m.row
	if !m.pinnedFence(a.ID, a.Generation, a.PassID) || !r.CleanupDeadline.Time.After(m.now) {
		return 0, nil
	}
	r.CleanupDeadline = pgtype.Timestamptz{}
	if r.CleanupAttempts == 8 {
		r.CleanupExhaustedAt = prerequisiteTime(m.now)
		r.Reason = "pin_cleanup_exhausted"
	}
	return 1, nil
}
func (m *prerequisiteMemory) ExpirePrerequisiteCleanup(_ context.Context, a store.ExpirePrerequisiteCleanupParams) (int64, error) {
	if !m.pinnedFence(a.ID, a.Generation, a.PassID) || a.Generation != 8 || m.row.CleanupDeadline.Time.After(m.now) {
		return 0, nil
	}
	m.row.CleanupExhaustedAt = prerequisiteTime(m.now)
	m.row.Reason = "pin_cleanup_exhausted"
	return 1, nil
}
func (m *prerequisiteMemory) DeferPrerequisiteCleanupListing(_ context.Context, a store.DeferPrerequisiteCleanupListingParams) (int64, error) {
	if !m.pinnedFence(a.ID, a.Generation, a.PassID) {
		return 0, nil
	}
	m.row.NextAttemptAt = prerequisiteTime(m.now.Add(time.Hour))
	return 1, nil
}
func (m *prerequisiteMemory) RecordPrerequisitePinnedForeign(_ context.Context, a store.RecordPrerequisitePinnedForeignParams) (int64, error) {
	if !m.pinnedFence(a.ID, a.Generation, a.PassID) {
		return 0, nil
	}
	m.row.State = "foreign"
	m.row.LiveRunID = pgtype.UUID{}
	return 1, nil
}
func (m *prerequisiteMemory) RecordPrerequisiteAbsence(_ context.Context, a store.RecordPrerequisiteAbsenceParams) (int64, error) {
	if !m.pinnedFence(a.ID, a.Generation, a.PassID) || m.row.AbsentSince.Valid || m.row.CleanupDeadline.Valid && m.row.CleanupDeadline.Time.After(m.now) {
		return 0, nil
	}
	m.row.AbsentSince = prerequisiteTime(m.now)
	m.row.AbsencePassID = a.PassID
	m.row.Reason = "pin_absence_unconfirmed"
	m.row.AuditNextAt = prerequisiteTime(m.now.Add(10 * time.Minute))
	return 1, nil
}
func (m *prerequisiteMemory) ClearPrerequisiteAbsence(_ context.Context, a store.ClearPrerequisiteAbsenceParams) (int64, error) {
	if !m.pinnedFence(a.ID, a.Generation, a.PassID) || m.row.AbsencePassID == a.PassID {
		return 0, nil
	}
	m.row.AbsentSince = pgtype.Timestamptz{}
	m.row.AbsencePassID = pgtype.UUID{}
	return 1, nil
}
func (m *prerequisiteMemory) ConfirmPrerequisiteLost(_ context.Context, a store.ConfirmPrerequisiteLostParams) (int64, error) {
	if !m.pinnedFence(a.ID, a.Generation, a.PassID) || !m.row.AbsentSince.Valid || m.row.AbsentSince.Time.Add(10*time.Minute).After(m.now) || m.row.AbsencePassID == a.PassID {
		return 0, nil
	}
	m.row.State = "lost"
	m.row.LiveRunID = pgtype.UUID{}
	return 1, nil
}

func TestPrerequisiteReconcileListOnly(t *testing.T) {
	for _, mode := range []string{"same", "absent", "foreign", "binding"} {
		t.Run(mode, func(t *testing.T) {
			s, m, h, _ := newPrerequisiteMemory(t, 1)
			prerequisiteRun(t, s, m)
			m.row.State = "create_unknown"
			sends := h.sends
			if mode == "absent" {
				h.tip = ""
			}
			if mode == "foreign" {
				h.tip = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			}
			if mode == "binding" {
				m.repo.ForgeProjectID++
			}
			prerequisiteRun(t, s, m)
			want := "create_unknown"
			if mode == "same" {
				want = "pinned"
			}
			if mode == "foreign" {
				want = "foreign"
			}
			if m.row.State != want || h.sends != sends || h.deletes != 0 {
				t.Fatalf("row=%+v sends=%d", m.row, h.sends)
			}
			if mode == "absent" && !m.row.LiveRunID.Valid {
				t.Fatal("absence cleared live pointer")
			}
		})
	}
}
func TestPrerequisiteCleanupOnlyUnavailableCapture(t *testing.T) {
	for _, state := range []string{"available", "expired", "discarded"} {
		t.Run(state, func(t *testing.T) {
			s, m, h, _ := newPrerequisiteMemory(t, 1)
			prerequisiteRun(t, s, m)
			m.capture.State = state
			m.hold.State = "released"
			prerequisiteRun(t, s, m)
			want := "pinned"
			deletes := 0
			if state != "available" {
				want = "deleted"
				deletes = 1
			}
			if m.row.State != want || h.deletes != deletes || m.row.CleanupAttempts != int32(deletes) {
				t.Fatalf("row=%+v deletes=%d", m.row, h.deletes)
			}
			if h.ioInTX.Load() {
				t.Fatal("cleanup I/O inside transaction")
			}
		})
	}
}
func TestPrerequisiteCleanupCeilingListingsAndLivePointer(t *testing.T) {
	s, m, h, _ := newPrerequisiteMemory(t, 1)
	prerequisiteRun(t, s, m)
	m.capture.State = "expired"
	m.row.CleanupAttempts = 8
	m.row.CleanupDeadline = prerequisiteTime(m.now.Add(-time.Second))
	m.row.NextAttemptAt = prerequisiteTime(m.now)
	prerequisiteRun(t, s, m)
	if m.row.State != "pinned" || !m.row.CleanupExhaustedAt.Valid || !m.row.LiveRunID.Valid || h.deletes != 0 {
		t.Fatalf("row=%+v", m.row)
	}
	h.tip = ""
	m.now = m.row.NextAttemptAt.Time
	prerequisiteRun(t, s, m)
	if m.row.State != "deleted" || m.row.LiveRunID.Valid {
		t.Fatalf("row=%+v", m.row)
	}
}
func TestPrerequisiteAbsenceNeedsTenMinutesDifferentPass(t *testing.T) {
	s, m, h, _ := newPrerequisiteMemory(t, 1)
	prerequisiteRun(t, s, m)
	h.tip = ""
	p := newPrerequisitePass(m.now)
	if _, err := s.prerequisiteOne(context.Background(), m, m.row, p); err != nil {
		t.Fatal(err)
	}
	if !m.row.AbsentSince.Valid || m.row.State != "pinned" {
		t.Fatalf("row=%+v", m.row)
	}
	// An early later pass cannot turn an absence into loss.
	m.now = m.now.Add(9 * time.Minute)
	prerequisiteRun(t, s, m)
	if m.row.State != "pinned" {
		t.Fatal("early loss")
	}
	m.now = m.now.Add(time.Minute)
	// A fresh test-only budget with the original pass ID isolates the durable
	// pass fence. Production Sweep never extends a pass deadline.
	samePass := newPrerequisitePass(m.now)
	samePass.id = p.id
	if _, err := s.prerequisiteOne(context.Background(), m, m.row, samePass); err != nil {
		t.Fatal(err)
	}
	if m.row.State != "pinned" {
		t.Fatal("same-pass loss")
	}
	prerequisiteRun(t, s, m)
	if m.row.State != "lost" || m.row.LiveRunID.Valid {
		t.Fatalf("row=%+v", m.row)
	}
}
func TestPrerequisitePositiveAuditClearsCandidateAndForeignUntouched(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		s, m, h, _ := newPrerequisiteMemory(t, 1)
		prerequisiteRun(t, s, m)
		h.tip = ""
		prerequisiteRun(t, s, m)
		m.now = m.now.Add(10 * time.Minute)
		h.tip = prerequisiteTestTip
		if foreign {
			h.tip = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}
		prerequisiteRun(t, s, m)
		if foreign {
			if m.row.State != "foreign" || h.deletes != 0 {
				t.Fatal("foreign pin touched")
			}
		} else if m.row.AbsentSince.Valid || m.row.State != "pinned" {
			t.Fatal("candidate not cleared")
		}
	}
}
func TestPrerequisiteInterruptedEighthAttemptDoesNotProveAgain(t *testing.T) {
	s, m, h, b := newPrerequisiteMemory(t, 1)
	m.row.Attempts = 8
	m.row.AttemptDeadline = prerequisiteTime(m.now.Add(-time.Second))
	prerequisiteRun(t, s, m)
	if m.row.State != "refused" || m.row.Reason != "prerequisite_ancestry_unknown" || b.builds != 0 || h.sends != 0 {
		t.Fatalf("row=%+v", m.row)
	}
}
func TestPrerequisiteKickSlotAndDrain(t *testing.T) {
	s, m, _, b := newPrerequisiteMemory(t, 1)
	entered := make(chan struct{})
	b.f = &prerequisiteTestForge{getProject: prerequisiteProjectStub(t, m), compare: func(ctx context.Context, _ int64, _, _ string) (forge.Ancestry, error) {
		close(entered)
		<-ctx.Done()
		return forge.AncestryUnknown, ctx.Err()
	}}
	s.StartPrerequisiteClosures(context.Background())
	// Held slots are independent and can all be occupied.
	for i := 0; i < cap(s.heldSlots); i++ {
		s.heldSlots <- struct{}{}
	}
	if !s.KickPrerequisiteClosure(m.row.UserID, m.row.CaptureID) {
		t.Fatal("kick declined")
	}
	finished := make(chan struct{})
	go func() { s.prerequisite.wg.Wait(); close(finished) }()
	select {
	case <-entered:
	case <-finished:
		t.Fatal("kick completed before ancestry comparison")
	}
	if s.KickPrerequisiteClosure(m.row.UserID, m.row.CaptureID) {
		t.Fatal("busy kick admitted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.DrainPrerequisiteClosures(ctx); err != nil {
		t.Fatal(err)
	}
	if s.KickPrerequisiteClosure(m.row.UserID, m.row.CaptureID) {
		t.Fatal("kick after drain")
	}
	if m.row.Attempts != 1 || m.marked != 0 || m.row.State != "pending" {
		t.Fatalf("row=%+v", m.row)
	}
	if !errors.Is(ctx.Err(), nil) {
		t.Fatal("drain exceeded bound")
	}
}
