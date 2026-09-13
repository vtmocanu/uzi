package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCompletionPhaseRule pins the ONE server-side completion-phase rule (PRD #1226 M5, D8) —
// the single derived label the web and CLI both render, so the two surfaces cannot disagree
// about which of D8's three states an interlocked run is in. The table walks every arm:
// non-interlocked is always ""; a completion hold is "blocked" (winning over the running
// states); the LIVE completion-question window (awaiting_input, interlocked, with the dedicated
// completion-question marker set) is also "blocked"; a running run past its first attempt is
// "reworking" (unmet) or "checking" (none unmet); everything else (no attempt yet, not running)
// is "". The awaiting_input arm is now gated on the MARKER (completionQuestionOpen), not
// attempts: an ordinary clarification on an interlocked post-attempt run (marker unset) is "".
func TestCompletionPhaseRule(t *testing.T) {
	tests := []struct {
		name                   string
		interlocked            bool
		status                 string
		holdReason             string
		completionQuestionOpen bool
		attempts               int
		unmetCount             int
		want                   string
	}{
		{"not interlocked is empty", false, "running", "", false, 2, 1, ""},
		{"not interlocked even when blocked", false, "paused", "completion_blocked", false, 3, 1, ""},
		{"blocked hold", true, "paused", "completion_blocked", false, 3, 1, "blocked"},
		{"blocked wins over running/reworking", true, "running", "completion_blocked", false, 2, 4, "blocked"},
		{"live completion-question window (marker set) is blocked", true, "awaiting_input", "", true, 2, 1, "blocked"},
		{"awaiting_input ordinary clarification (marker unset) is empty even past an attempt", true, "awaiting_input", "", false, 2, 1, ""},
		{"awaiting_input with no marker and no attempt is empty", true, "awaiting_input", "", false, 0, 0, ""},
		{"awaiting_input not interlocked is empty even with marker", false, "awaiting_input", "", true, 2, 1, ""},
		{"running past attempt with unmet is reworking", true, "running", "", false, 1, 2, "reworking"},
		{"running past attempt none unmet is checking", true, "running", "", false, 1, 0, "checking"},
		{"running but no attempt yet is empty", true, "running", "", false, 0, 0, ""},
		{"interlocked not running is empty", true, "queued", "", false, 2, 1, ""},
		{"interlocked awaiting_approval is empty", true, "awaiting_approval", "", false, 0, 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := completionPhaseRule(tc.interlocked, tc.status, tc.holdReason, tc.completionQuestionOpen, tc.attempts, tc.unmetCount); got != tc.want {
				t.Fatalf("completionPhaseRule(interlocked=%v, status=%q, hold=%q, completionQuestionOpen=%v, attempts=%d, unmet=%d) = %q, want %q",
					tc.interlocked, tc.status, tc.holdReason, tc.completionQuestionOpen, tc.attempts, tc.unmetCount, got, tc.want)
			}
		})
	}
}

// TestRunToDTOCompletionFieldsInterlocked pins that runToDTO surfaces the honest-state
// completion fields for an interlocked, completion-blocked run: the discriminator, the attempt
// count, the bounded unmet list decoded from the latest_completion_attempt jsonb, the hold
// reason, the constant same-worker-only hold context, and the derived "blocked" phase.
func TestRunToDTOCompletionFieldsInterlocked(t *testing.T) {
	dto := runToDTO(store.Run{
		ID:                        uuid.New(),
		Status:                    "paused",
		CompletionContractVersion: pgtype.Int4{Int32: 1, Valid: true},
		CompletionAttempts:        3,
		LatestCompletionAttempt:   []byte(`{"unmet":["m5","m6"],"head":"abc","worktree_fingerprint":"wf","at":"2026-09-11T00:00:00Z"}`),
		HoldReason:                pgtype.Text{String: "completion_blocked", Valid: true},
	}, "normal", 0, 0, dtoTestNow)

	if !dto.CompletionInterlock {
		t.Fatal("CompletionInterlock = false, want true (non-null completion_contract_version)")
	}
	if dto.CompletionAttempts != 3 {
		t.Fatalf("CompletionAttempts = %d, want 3", dto.CompletionAttempts)
	}
	if len(dto.CompletionUnmet) != 2 || dto.CompletionUnmet[0] != "m5" || dto.CompletionUnmet[1] != "m6" {
		t.Fatalf("CompletionUnmet = %v, want [m5 m6]", dto.CompletionUnmet)
	}
	if dto.HoldReason == nil || *dto.HoldReason != "completion_blocked" {
		t.Fatalf("HoldReason = %v, want completion_blocked", dto.HoldReason)
	}
	if dto.HoldContext == nil || *dto.HoldContext != "unavailable(same_worker_only)" {
		t.Fatalf("HoldContext = %v, want unavailable(same_worker_only)", dto.HoldContext)
	}
	if dto.CompletionPhase != "blocked" {
		t.Fatalf("CompletionPhase = %q, want blocked", dto.CompletionPhase)
	}
}

// TestRunToDTOCompletionFieldsInert pins the non-interlocked default: a run with a NULL
// completion_contract_version carries the inert defaults — no interlock, zero attempts, a
// STABLE empty unmet array (never null), null hold reason/context, and an empty phase.
func TestRunToDTOCompletionFieldsInert(t *testing.T) {
	dto := runToDTO(store.Run{ID: uuid.New(), Status: "running"}, "normal", 0, 0, dtoTestNow)
	if dto.CompletionInterlock {
		t.Fatal("CompletionInterlock = true, want false for a non-interlocked run")
	}
	if dto.CompletionAttempts != 0 {
		t.Fatalf("CompletionAttempts = %d, want 0", dto.CompletionAttempts)
	}
	if dto.CompletionUnmet == nil {
		t.Fatal("CompletionUnmet = nil, want a non-nil empty slice (stable array, never null)")
	}
	if len(dto.CompletionUnmet) != 0 {
		t.Fatalf("CompletionUnmet = %v, want []", dto.CompletionUnmet)
	}
	if dto.HoldReason != nil || dto.HoldContext != nil {
		t.Fatalf("expected nil hold fields, got reason=%v context=%v", dto.HoldReason, dto.HoldContext)
	}
	if dto.CompletionPhase != "" {
		t.Fatalf("CompletionPhase = %q, want empty", dto.CompletionPhase)
	}
}

// TestRunToDTOCompletionPhaseAwaitingInputMarker pins the wiring from the dedicated
// completion-question marker (runs.completion_question_at) into the awaiting_input completion-phase
// arm: an interlocked awaiting_input run with the marker SET reads "blocked", while the SAME run
// past a completion attempt but with the marker UNSET (an ordinary clarification) reads "" — the
// imprecision fix. The marker, not the attempt count, gates the arm.
func TestRunToDTOCompletionPhaseAwaitingInputMarker(t *testing.T) {
	base := store.Run{
		ID:                        uuid.New(),
		Status:                    "awaiting_input",
		CompletionContractVersion: pgtype.Int4{Int32: 1, Valid: true},
		CompletionAttempts:        2,
	}

	withMarker := base
	withMarker.CompletionQuestionAt = pgtype.Timestamptz{Time: time.Unix(1_700_000_000, 0), Valid: true}
	if got := runToDTO(withMarker, "normal", 0, 0, dtoTestNow).CompletionPhase; got != "blocked" {
		t.Fatalf("marker-set awaiting_input CompletionPhase = %q, want blocked", got)
	}

	// Marker unset: an ordinary clarification on an interlocked post-attempt run no longer reads blocked.
	if got := runToDTO(base, "normal", 0, 0, dtoTestNow).CompletionPhase; got != "" {
		t.Fatalf("marker-unset awaiting_input CompletionPhase = %q, want empty (ordinary clarification)", got)
	}
}
