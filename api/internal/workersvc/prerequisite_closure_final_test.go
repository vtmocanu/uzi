package workersvc

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ReservePrerequisiteFinalListing mirrors the guarded SQL for the in-memory transaction fixture.
func (m *prerequisiteMemory) ReservePrerequisiteFinalListing(_ context.Context, a store.ReservePrerequisiteFinalListingParams) (store.RecoveryPrerequisiteClosure, error) {
	r := &m.row
	if r.ID != a.ID || r.State != "pinned" || r.AbsentSince.Valid || r.CleanupDeadline.Valid && r.CleanupDeadline.Time.After(m.now) ||
		r.CleanupAttempts != a.Generation || r.LastAuditPassID == a.PassID || !a.PassID.Valid || m.capture.State != "available" {
		return *r, pgx.ErrNoRows
	}
	r.LastAuditPassID = a.PassID
	return *r, nil
}

func finalPinned(t *testing.T) (*Service, *prerequisiteMemory, *prerequisiteHTTP) {
	t.Helper()
	s, m, h, _ := newPrerequisiteMemory(t, 1)
	prerequisiteRun(t, s, m)
	if m.row.State != "pinned" || !m.row.PinTip.Valid {
		t.Fatalf("fixture row %+v", m.row)
	}
	m.locks = nil
	return s, m, h
}

func TestPrerequisiteFinalObserveClassifiesTheListing(t *testing.T) {
	ctx := context.Background()
	s, m, h := finalPinned(t)
	sends := h.sends
	for _, tc := range []struct {
		name string
		prep func()
		want PrerequisiteFinalObservation
		fail bool
	}{
		{"present", func() {}, PrerequisiteFinalPresent, false},
		{"absent", func() { h.tip = "" }, PrerequisiteFinalAbsent, false},
		{"foreign", func() { h.tip = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" }, PrerequisiteFinalForeign, false},
		// A binding that moved since the pin was frozen is unknown, never absence.
		{"binding moved", func() { h.tip = ""; m.repo.ForgeProjectID++ }, PrerequisiteFinalUnknown, true},
	} {
		h.tip = prerequisiteTestTip
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			defer func(p int64) { m.repo.ForgeProjectID = p }(m.repo.ForgeProjectID)
			tc.prep()
			got, err := s.PrerequisiteFinalObserve(ctx, m.row)
			if got != tc.want || (err != nil) != tc.fail {
				t.Fatalf("observed %v, %v want %v fail=%v", got, err, tc.want, tc.fail)
			}
			if tc.fail && !errors.Is(err, errPrerequisiteFinalUnavailable) {
				t.Fatalf("a failed observation must carry only the fixed error: %v", err)
			}
		})
	}
	if h.sends != sends || h.deletes != 0 {
		t.Fatalf("observation mutated the ref: sends=%d deletes=%d", h.sends, h.deletes)
	}
}

func TestPrerequisiteFinalObserveFailuresAreNeverAbsence(t *testing.T) {
	ctx := context.Background()
	for name, prep := range map[string]func(*Service, *prerequisiteMemory, *prerequisiteBuilderHandle){
		"listing unreachable": func(_ *Service, m *prerequisiteMemory, _ *prerequisiteBuilderHandle) {
			m.repo.WebUrl = "http://127.0.0.1:1/repo"
		},
		"identity lookup fails": func(s *Service, m *prerequisiteMemory, b *prerequisiteBuilderHandle) {
			b.set(&prerequisiteTestForge{getProject: func(context.Context, int64) (forge.Project, error) {
				return forge.Project{}, errors.New("https://user:secret@evil.example failure")
			}})
		},
		"identity changed": func(s *Service, m *prerequisiteMemory, b *prerequisiteBuilderHandle) {
			b.set(&prerequisiteTestForge{getProject: func(_ context.Context, id int64) (forge.Project, error) {
				return forge.Project{ForgeProjectID: id, PathWithNamespace: "other/repo", WebURL: m.repo.WebUrl}, nil
			}})
		},
		"credential unavailable": func(s *Service, m *prerequisiteMemory, _ *prerequisiteBuilderHandle) {
			m.conn.TokenCiphertext = []byte{1, 2, 3}
		},
		"forge not allowed": func(s *Service, _ *prerequisiteMemory, _ *prerequisiteBuilderHandle) {
			s.SetForgeBaseURLAllowed(func(string) bool { return false })
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, m, h, b := newPrerequisiteMemory(t, 1)
			prerequisiteRun(t, s, m)
			h.tip = "" // would read as absence if the failure were mistaken for a clean listing
			prep(s, m, &prerequisiteBuilderHandle{b})
			got, err := s.PrerequisiteFinalObserve(ctx, m.row)
			if got != PrerequisiteFinalUnknown || !errors.Is(err, errPrerequisiteFinalUnavailable) {
				t.Fatalf("observed %v, %v", got, err)
			}
			if err.Error() != errPrerequisiteFinalUnavailable.Error() {
				t.Fatalf("error text is not the fixed string: %q", err)
			}
		})
	}
}

type prerequisiteBuilderHandle struct{ b *prerequisiteTestBuilder }

func (h *prerequisiteBuilderHandle) set(f *prerequisiteTestForge) { h.b.f = f }

func TestPrerequisiteFinalReserveAndOutcomesAreFencedByPass(t *testing.T) {
	ctx := context.Background()
	s, m, _ := finalPinned(t)
	pass, other := uuid.New(), uuid.New()
	ok, err := s.PrerequisiteFinalReserve(ctx, m.row, pass)
	if err != nil || !ok {
		t.Fatalf("reserve: %v %v", ok, err)
	}
	if got := m.locks; len(got) != 2 || got[0] != "capture" || got[1] != "closure" {
		t.Fatalf("lock order %v, want capture then closure", got)
	}
	if m.row.LastAuditPassID != (pgtype.UUID{Bytes: pass, Valid: true}) || m.row.AuditNextAt.Valid && m.row.AuditNextAt.Time.Equal(m.now) {
		t.Fatalf("reservation not recorded: %+v", m.row)
	}
	// The same pass cannot reserve twice; a newer pass fences the older one out.
	if ok, err = s.PrerequisiteFinalReserve(ctx, m.row, pass); err != nil || ok {
		t.Fatalf("repeat reserve: %v %v", ok, err)
	}
	if ok, err = s.PrerequisiteFinalReserve(ctx, m.row, other); err != nil || !ok {
		t.Fatalf("newer reserve: %v %v", ok, err)
	}
	for name, record := range map[string]func(context.Context, store.RecoveryPrerequisiteClosure, uuid.UUID) (int64, error){
		"absence": s.PrerequisiteFinalRecordAbsence,
		"foreign": s.PrerequisiteFinalRecordForeign,
	} {
		before := m.row
		if n, err := record(ctx, before, pass); err != nil || n != 0 || !reflect.DeepEqual(m.row, before) {
			t.Fatalf("%s with a stale pass: rows=%d err=%v row=%+v", name, n, err, m.row)
		}
	}
	m.locks = nil
	if n, err := s.PrerequisiteFinalRecordAbsence(ctx, m.row, other); err != nil || n != 1 || !m.row.AbsentSince.Valid || m.row.State != "pinned" {
		t.Fatalf("absence: %d %v %+v", n, err, m.row)
	}
	if got := m.locks; len(got) != 2 || got[0] != "capture" || got[1] != "closure" {
		t.Fatalf("outcome lock order %v", got)
	}
	// An absent pin is no longer reservable, and a foreign verdict then needs a fresh fence.
	if ok, err = s.PrerequisiteFinalReserve(ctx, m.row, uuid.New()); err != nil || ok {
		t.Fatalf("reserve on an absent pin: %v %v", ok, err)
	}
}

func TestPrerequisiteFinalRecordForeignMovesThePin(t *testing.T) {
	ctx := context.Background()
	s, m, _ := finalPinned(t)
	pass := uuid.New()
	if ok, err := s.PrerequisiteFinalReserve(ctx, m.row, pass); err != nil || !ok {
		t.Fatalf("reserve: %v %v", ok, err)
	}
	if n, err := s.PrerequisiteFinalRecordForeign(ctx, m.row, pass); err != nil || n != 1 || m.row.State != "foreign" {
		t.Fatalf("foreign: %d %v %+v", n, err, m.row)
	}
}

func TestPrerequisiteFinalReserveWithoutQueryIsAnError(t *testing.T) {
	s, m, _ := finalPinned(t)
	s.SetPrerequisiteStore(m, func(ctx context.Context, fn func(PrerequisiteStore) error) error {
		return fn(struct{ PrerequisiteStore }{m}) // a store that cannot reserve a FINAL listing
	})
	if ok, err := s.PrerequisiteFinalReserve(context.Background(), m.row, uuid.New()); ok || err == nil {
		t.Fatalf("reserved without the query: %v %v", ok, err)
	}
}
