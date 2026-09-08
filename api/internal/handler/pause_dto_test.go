package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestPauseRequestedRule pins the ONE server-side pause-boundary rule (PRD #1190 M1,
// Decision 4) — the boolean the worker honors off the running-report ACK. The table is the
// PRD's: `now` always parks; `milestone` waits until the in-flight milestone completes
// (completed count exceeds the request-time count) OR the run has no frozen list; no pending
// pause (empty mode) is false.
func TestPauseRequestedRule(t *testing.T) {
	tests := []struct {
		name                           string
		mode                           string
		frozenLen, completedLen, after int
		want                           bool
	}{
		{"now parks immediately", "now", 6, 3, 3, true},
		{"milestone not yet at boundary (completed == after)", "milestone", 6, 3, 3, false},
		{"milestone past boundary (completed > after)", "milestone", 6, 4, 3, true},
		{"milestone with no frozen list parks at next turn", "milestone", 0, 0, 0, true},
		{"no pending pause is false", "", 6, 5, 3, false},
		{"unknown mode is false", "bogus", 6, 5, 3, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pauseRequestedRule(tc.mode, tc.frozenLen, tc.completedLen, tc.after); got != tc.want {
				t.Fatalf("pauseRequestedRule(%q, frozen=%d, completed=%d, after=%d) = %v, want %v",
					tc.mode, tc.frozenLen, tc.completedLen, tc.after, got, tc.want)
			}
		})
	}
}

// TestRunToDTOPauseFieldsOwner pins that runToDTO — the owner/admin run-detail DTO — surfaces
// a PENDING pause request (its three intent columns + the shared checkpoint stamp) and the
// server-decided PauseRequested boundary. The pause intent rides only this DTO (GetRun,
// own-scoped ListRuns, admin AdminListRuns), never the cross-user board card.
func TestRunToDTOPauseFieldsOwner(t *testing.T) {
	at := time.Date(2026, 9, 7, 11, 2, 0, 0, time.UTC)
	cp := time.Date(2026, 9, 7, 10, 53, 0, 0, time.UTC)
	dto := runToDTO(store.Run{
		ID:                  uuid.New(),
		Status:              "running",
		PauseRequestedAt:    pgtype.Timestamptz{Time: at, Valid: true},
		PauseMode:           pgtype.Text{String: "milestone", Valid: true},
		PauseAfterCount:     pgtype.Int4{Int32: 3, Valid: true},
		CheckpointTipAt:     pgtype.Timestamptz{Time: cp, Valid: true},
		MilestonesFrozen:    []byte(`[{"id":"m1","title":"a"},{"id":"m2","title":"b"},{"id":"m3","title":"c"},{"id":"m4","title":"d"}]`),
		MilestonesCompleted: []byte(`["m1","m2","m3","m4"]`),
	}, "normal")

	if dto.PauseRequestedAt == nil || !dto.PauseRequestedAt.Equal(at) {
		t.Fatalf("PauseRequestedAt = %v, want %v", dto.PauseRequestedAt, at)
	}
	if dto.PauseMode == nil || *dto.PauseMode != "milestone" {
		t.Fatalf("PauseMode = %v, want milestone", dto.PauseMode)
	}
	if dto.PauseAfterCount == nil || *dto.PauseAfterCount != 3 {
		t.Fatalf("PauseAfterCount = %v, want 3", dto.PauseAfterCount)
	}
	if dto.CheckpointTipAt == nil || !dto.CheckpointTipAt.Equal(cp) {
		t.Fatalf("CheckpointTipAt = %v, want %v", dto.CheckpointTipAt, cp)
	}
	// milestone mode, 4 completed > after-count 3 → the boundary has passed, so the worker
	// should park: PauseRequested is true.
	if !dto.PauseRequested {
		t.Fatalf("PauseRequested = false, want true (completed 4 > after-count 3)")
	}
}

// TestRunToDTOPauseFieldsAbsent pins the null case: a run with no pending pause exposes nil
// intent fields and a false boundary.
func TestRunToDTOPauseFieldsAbsent(t *testing.T) {
	dto := runToDTO(store.Run{ID: uuid.New(), Status: "running"}, "normal")
	if dto.PauseRequestedAt != nil || dto.PauseMode != nil || dto.PauseAfterCount != nil {
		t.Fatalf("expected nil pause intent fields, got at=%v mode=%v after=%v",
			dto.PauseRequestedAt, dto.PauseMode, dto.PauseAfterCount)
	}
	if dto.CheckpointTipAt != nil {
		t.Fatalf("expected nil CheckpointTipAt, got %v", dto.CheckpointTipAt)
	}
	if dto.PauseRequested {
		t.Fatalf("PauseRequested = true, want false when no pause is pending")
	}
}

// TestBoardCardCarriesNoPauseIntent pins that the cross-user board card (latestRunDTO) — the
// ONE surface a non-owner viewer receives another user's run through — never carries the
// pause intent fields (PRD #1190: latestRunDTO needs only the status). A non-owner therefore
// cannot read that an owner requested a pause; the ‖ paused pill rides the status alone.
func TestBoardCardCarriesNoPauseIntent(t *testing.T) {
	// A non-owner viewer (viewerID != ownerID) of a paused run's card.
	card := mapLatestRun(uuid.New(), uuid.New(), "paused", "issue", 3, true,
		pgtype.Int8{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{},
		"ok", pgtype.Text{}, pgtype.Timestamptz{}, pgtype.Text{}, pgtype.Text{}, 1,
		pgtype.Timestamptz{Valid: true}, pgtype.Timestamptz{Valid: true}, uuid.New())
	if card.IsMine {
		t.Fatal("test setup: card should be a non-owner viewer's")
	}
	b, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal card: %v", err)
	}
	for _, k := range []string{"pause_requested_at", "pause_mode", "pause_after_count", "pause_requested"} {
		if _, ok := m[k]; ok {
			t.Fatalf("board card must not carry pause intent field %q (non-owner surface)", k)
		}
	}
	if card.Status != "paused" {
		t.Fatalf("board card status = %q, want paused", card.Status)
	}
}
