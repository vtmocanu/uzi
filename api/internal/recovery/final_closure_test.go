package recovery

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func closureBackedHold() (store.RecoveryCustodyHold, store.RecoveryCapture) {
	hold := store.RecoveryCustodyHold{
		ID: uuid.New(), RunID: uuid.New(), UserID: uuid.New(), OriginalWorkerID: uuid.New(), OriginalWorkerIdentity: "w",
		InventoryGuarded: true, State: "released", Generation: 1,
		ReleasedAt:          pgtype.Timestamptz{Time: time.Now(), Valid: true},
		ReleaseEvidence:     pgtype.Text{String: "prerequisite_closure", Valid: true},
		FinalDisposition:    pgtype.Text{String: "archive", Valid: true},
		FinalSourceSha:      pgtype.Text{String: "aaaa1111", Valid: true},
		FinalCoverageDigest: pgtype.Text{String: strings.Repeat("a", 64), Valid: true},
		FinalCaptureID:      pgtype.UUID{Bytes: uuid.New(), Valid: true},
		FinalClosureID:      pgtype.UUID{Bytes: uuid.New(), Valid: true},
	}
	cap := store.RecoveryCapture{
		ID: uuid.UUID(hold.FinalCaptureID.Bytes), HoldID: hold.ID, RunID: hold.RunID, UserID: hold.UserID,
		OriginalWorkerID: pgtype.UUID{Bytes: hold.OriginalWorkerID, Valid: true}, OriginalWorkerIdentity: "w",
		ManifestBound: true, SourceSha: "aaaa1111", CoverageDigest: pgtype.Text{String: strings.Repeat("a", 64), Valid: true},
	}
	return hold, cap
}

func TestClassifyCrossRunClosureBackedArchive(t *testing.T) {
	classify := func(h store.RecoveryCustodyHold, c store.RecoveryCapture) error {
		return classifyCrossRunHold(h, 1, nil, nil, map[uuid.UUID]store.RecoveryCapture{c.ID: c})
	}
	h, c := closureBackedHold()
	if err := classify(h, c); err != nil {
		t.Fatalf("genuine closure-backed release refused: %v", err)
	}
	for name, mutate := range map[string]func(*store.RecoveryCustodyHold, *store.RecoveryCapture){
		"closure evidence without a closure id": func(h *store.RecoveryCustodyHold, _ *store.RecoveryCapture) { h.FinalClosureID = pgtype.UUID{} },
		"archive evidence carrying a closure id": func(h *store.RecoveryCustodyHold, _ *store.RecoveryCapture) {
			h.ReleaseEvidence.String = "archive"
		},
		"closure evidence on a settled FINAL": func(h *store.RecoveryCustodyHold, _ *store.RecoveryCapture) {
			h.FinalDisposition.String = "settled"
		},
		"capture of another hold": func(_ *store.RecoveryCustodyHold, c *store.RecoveryCapture) { c.HoldID = uuid.New() },
		"capture of another worker": func(_ *store.RecoveryCustodyHold, c *store.RecoveryCapture) {
			c.OriginalWorkerID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
		},
		"capture source differs": func(_ *store.RecoveryCustodyHold, c *store.RecoveryCapture) { c.SourceSha = "bbbb2222" },
		"capture digest differs": func(_ *store.RecoveryCustodyHold, c *store.RecoveryCapture) {
			c.CoverageDigest.String = strings.Repeat("b", 64)
		},
		"unbound capture": func(_ *store.RecoveryCustodyHold, c *store.RecoveryCapture) { c.ManifestBound = false },
		"unknown evidence": func(h *store.RecoveryCustodyHold, _ *store.RecoveryCapture) {
			h.ReleaseEvidence.String = "prerequisite_closure_v2"
		},
	} {
		h, c := closureBackedHold()
		mutate(&h, &c)
		if name == "archive evidence carrying a closure id" {
			h.ReleaseEvidence.String = "archive"
		}
		if err := classify(h, c); err == nil {
			t.Fatalf("%s: forged hold accepted", name)
		}
	}
	// A plain archive release is unchanged.
	h, c = closureBackedHold()
	h.ReleaseEvidence.String, h.FinalClosureID = "archive", pgtype.UUID{}
	if err := classify(h, c); err != nil {
		t.Fatalf("plain archive release refused: %v", err)
	}
}

func TestValidExactClosedObservationAcceptsClosureEvidenceForArchiveOnly(t *testing.T) {
	owner := uuid.New()
	obs := func(kind, evidence string) apitypes.RecoveryExactHold {
		f := &apitypes.RecoveryFinalDisposition{Kind: kind, SourceSha: "aaaa1111", CoverageDigest: strings.Repeat("a", 64)}
		switch kind {
		case "archive":
			f.CaptureID = uuid.NewString()
		case "held_publication":
			f.SourceSha = strings.Repeat("a", 40)
			f.PublicationID = uuid.NewString()
		}
		return apitypes.RecoveryExactHold{State: "released", ReleaseEvidence: evidence, FinalReceipt: f}
	}
	if !validExactClosedObservation(obs("archive", "prerequisite_closure"), owner) {
		t.Fatal("closure-backed archive receipt refused")
	}
	if !validExactClosedObservation(obs("archive", "archive"), owner) {
		t.Fatal("plain archive receipt refused")
	}
	if validExactClosedObservation(obs("held_publication", "prerequisite_closure"), owner) {
		t.Fatal("closure evidence accepted for a held publication")
	}
	if validExactClosedObservation(obs("archive", "publication"), owner) {
		t.Fatal("foreign evidence accepted for an archive")
	}
}

func TestPrerequisiteFinalEnabledNeedsEveryPart(t *testing.T) {
	for name, set := range map[string]func(*Service) bool{
		"nothing wired": func(*Service) bool { return false },
		"knob only":     func(s *Service) bool { s.SetPrerequisiteClosure([]string{"github"}, nil); return false },
		"non-github": func(s *Service) bool {
			s.SetPrerequisiteClosure([]string{"gitlab"}, nil)
			s.SetPrerequisiteFinalAllow(func(uuid.UUID) bool { return true })
			return false
		},
		"no port": func(s *Service) bool {
			s.SetPrerequisiteClosure([]string{"github"}, nil)
			s.SetPrerequisiteFinalAllow(func(uuid.UUID) bool { return true })
			return false
		},
		"no allow": func(s *Service) bool {
			s.SetPrerequisiteClosure([]string{"github"}, nil)
			s.SetPrerequisiteFinal(&scriptedFinalPort{})
			return false
		},
		"all wired": func(s *Service) bool {
			s.SetPrerequisiteClosure([]string{"github"}, nil)
			s.SetPrerequisiteFinal(&scriptedFinalPort{})
			s.SetPrerequisiteFinalAllow(func(uuid.UUID) bool { return true })
			return true
		},
	} {
		s := New(nil, nil, nil, Limits{}, nil)
		want := set(s)
		if got := s.PrerequisiteFinalEnabled(); got != want {
			t.Fatalf("%s: enabled=%v want %v", name, got, want)
		}
	}
}
