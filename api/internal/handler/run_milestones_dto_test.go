package handler

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRunToDTOMilestones pins how runToDTO surfaces PRD #122 M1's FROZEN list: the
// decoded array when the column holds one, and a nil slice (→ JSON null, the
// back-compat contract) when it is NULL or malformed.
func TestRunToDTOMilestones(t *testing.T) {
	t.Run("frozen list is exposed", func(t *testing.T) {
		dto := runToDTO(store.Run{
			ID:               uuid.New(),
			MilestonesFrozen: []byte(`[{"id":"m1","title":"First"},{"id":"m2","title":"Second"}]`),
		})
		if len(dto.Milestones) != 2 || dto.Milestones[0].ID != "m1" || dto.Milestones[1].Title != "Second" {
			t.Fatalf("dto.Milestones = %+v", dto.Milestones)
		}
	})

	t.Run("no frozen list is nil (null on the wire)", func(t *testing.T) {
		dto := runToDTO(store.Run{ID: uuid.New()})
		if dto.Milestones != nil {
			t.Fatalf("a run with no frozen list must expose nil milestones, got %+v", dto.Milestones)
		}
	})

	t.Run("malformed column degrades to nil", func(t *testing.T) {
		dto := runToDTO(store.Run{ID: uuid.New(), MilestonesFrozen: []byte(`{not an array`)})
		if dto.Milestones != nil {
			t.Fatalf("a malformed frozen column must degrade to nil, got %+v", dto.Milestones)
		}
	})
}

// TestRunToDTOMilestonesCandidate pins how runToDTO surfaces PRD #122 M3's PRE-APPROVAL
// candidate list: the decoded array when the column holds one, and a nil slice (→ JSON
// null, the back-compat contract) when it is absent.
func TestRunToDTOMilestonesCandidate(t *testing.T) {
	t.Run("candidate list is exposed", func(t *testing.T) {
		dto := runToDTO(store.Run{
			ID:                  uuid.New(),
			MilestonesCandidate: []byte(`[{"id":"m1","title":"First"}]`),
		})
		if len(dto.MilestonesCandidate) != 1 || dto.MilestonesCandidate[0].ID != "m1" || dto.MilestonesCandidate[0].Title != "First" {
			t.Fatalf("dto.MilestonesCandidate = %+v", dto.MilestonesCandidate)
		}
	})

	t.Run("no candidate list is nil (null on the wire)", func(t *testing.T) {
		dto := runToDTO(store.Run{ID: uuid.New()})
		if dto.MilestonesCandidate != nil {
			t.Fatalf("a run with no candidate list must expose nil, got %+v", dto.MilestonesCandidate)
		}
	})
}
