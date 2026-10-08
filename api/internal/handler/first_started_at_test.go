package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #2004: a terminal run's budget_used_seconds is frozen at finished_at, so the figure must
// not creep with the wall clock the reader happens to ask at.
func TestRunToDTO_TerminalBudgetUsedFrozenAtFinishedAt(t *testing.T) {
	started := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	finished := started.Add(90 * time.Minute)
	for _, status := range []string{"completed", "failed", "cancelled"} {
		r := store.Run{
			ID:         uuid.New(),
			Status:     status,
			StartedAt:  tstamp(started),
			FinishedAt: tstamp(finished),
		}
		a := runToDTO(r, "normal", 0, 0, 0, finished.Add(time.Minute), 0)
		b := runToDTO(r, "normal", 0, 0, 0, finished.Add(48*time.Hour), 0)
		if a.BudgetUsedSeconds == nil || b.BudgetUsedSeconds == nil {
			t.Fatalf("%s: budget_used_seconds must be set for a started run", status)
		}
		if *a.BudgetUsedSeconds != *b.BudgetUsedSeconds {
			t.Fatalf("%s: terminal budget_used_seconds drifted with now: %d vs %d", status, *a.BudgetUsedSeconds, *b.BudgetUsedSeconds)
		}
		if want := 90 * 60; *a.BudgetUsedSeconds != want {
			t.Fatalf("%s: budget_used_seconds = %d, want %d", status, *a.BudgetUsedSeconds, want)
		}
	}
}

// A live (running) run keeps ageing with now: the freeze is for terminal rows only.
func TestRunToDTO_RunningBudgetUsedStillAges(t *testing.T) {
	started := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	r := store.Run{ID: uuid.New(), Status: "running", StartedAt: tstamp(started)}
	a := runToDTO(r, "normal", 0, 0, 0, started.Add(time.Minute), 0)
	b := runToDTO(r, "normal", 0, 0, 0, started.Add(time.Hour), 0)
	if *b.BudgetUsedSeconds <= *a.BudgetUsedSeconds {
		t.Fatalf("running budget_used_seconds must age: %d then %d", *a.BudgetUsedSeconds, *b.BudgetUsedSeconds)
	}
}

// runToDTO maps runs.first_started_at independently of started_at (which a resume resets).
func TestRunToDTO_MapsFirstStartedAt(t *testing.T) {
	first := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	last := first.Add(5 * time.Hour)
	dto := runToDTO(store.Run{
		ID:             uuid.New(),
		Status:         "running",
		FirstStartedAt: tstamp(first),
		StartedAt:      tstamp(last),
	}, "normal", 0, 0, 0, last.Add(time.Minute), 0)
	if dto.FirstStartedAt == nil || !dto.FirstStartedAt.Equal(first) {
		t.Fatalf("first_started_at = %v, want %v", dto.FirstStartedAt, first)
	}
	if dto.StartedAt == nil || !dto.StartedAt.Equal(last) {
		t.Fatalf("started_at = %v, want %v", dto.StartedAt, last)
	}
	none := runToDTO(store.Run{ID: uuid.New(), Status: "queued"}, "normal", 0, 0, 0, last, 0)
	if none.FirstStartedAt != nil {
		t.Fatalf("a never-started run must have a null first_started_at, got %v", none.FirstStartedAt)
	}
}

// The board card carries first_started_at + finished_at for the whole-run duration (supersedes
// PRD #256 Decision 6, issue #2004).
func TestMapLatestRun_CarriesFirstStartedAtAndFinishedAt(t *testing.T) {
	first := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	started := first.Add(3 * time.Hour)
	finished := started.Add(time.Hour)
	dto := mapLatestRun(uuid.New(), uuid.New(), "completed", "issue", 1, false, pgtype.Int8{}, nullTxt(), nullTxt(), nullTxt(), nullTxt(), nullTxt(),
		"ok", nullTxt(), pgtype.Timestamptz{}, nullTxt(), tstamp(started), tstamp(first), tstamp(finished), pgtype.Int4{}, 0, 0, 0, false,
		nullTxt(), nullTxt(), 1, tstamp(first), tstamp(finished), uuid.New(), 0)
	if dto.FirstStartedAt == nil || !dto.FirstStartedAt.Equal(first) {
		t.Fatalf("first_started_at = %v, want %v", dto.FirstStartedAt, first)
	}
	if dto.FinishedAt == nil || !dto.FinishedAt.Equal(finished) {
		t.Fatalf("finished_at = %v, want %v", dto.FinishedAt, finished)
	}
	live := mapLatestRun(uuid.New(), uuid.New(), "running", "issue", 1, false, pgtype.Int8{}, nullTxt(), nullTxt(), nullTxt(), nullTxt(), nullTxt(),
		"ok", nullTxt(), pgtype.Timestamptz{}, nullTxt(), pgtype.Timestamptz{}, pgtype.Timestamptz{}, pgtype.Timestamptz{}, pgtype.Int4{}, 0, 0, 0, false,
		nullTxt(), nullTxt(), 1, tstamp(first), tstamp(first), uuid.New(), 0)
	if live.FirstStartedAt != nil || live.FinishedAt != nil {
		t.Fatalf("unset columns must map to null, got %v / %v", live.FirstStartedAt, live.FinishedAt)
	}
}
