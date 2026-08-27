package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestScheduleDTOLastFire pins that scheduleDTO unmarshals the persisted last_fire jsonb
// bytes into dto.LastFire (PRD #308 M3), and leaves it nil when the column is NULL/empty.
// The persisted wire shape shares its json tags with apitypes.LastFire, so marshaling an
// apitypes.LastFire reproduces exactly the column bytes the DTO reads back.
func TestScheduleDTOLastFire(t *testing.T) {
	h := &Handler{}
	base := store.RunSchedule{
		Target:   "issue",
		IssueIid: pgtype.Int8{Int64: 7, Valid: true},
		Timing:   "once",
		Timezone: "UTC",
	}

	// NULL column ⇒ nil DTO field.
	if dto := h.scheduleDTO(base, ""); dto.LastFire != nil {
		t.Fatalf("NULL last_fire must map to nil, got %+v", dto.LastFire)
	}

	// Empty (zero-length, non-nil) column ⇒ still nil.
	base.LastFire = []byte{}
	if dto := h.scheduleDTO(base, ""); dto.LastFire != nil {
		t.Fatalf("empty last_fire must map to nil, got %+v", dto.LastFire)
	}

	iid := int64(42)
	firedAt := time.Date(2026, 8, 11, 2, 0, 0, 0, time.UTC)
	want := apitypes.LastFire{
		FiredAt: firedAt,
		Matched: 2,
		Capped:  true,
		Started: []apitypes.LastFireStarted{{IssueIID: &iid, RunID: "run-1", Title: "Fix the bug"}},
		Skips:   []apitypes.LastFireSkip{{IssueIID: nil, Title: "", Reason: "already_running"}},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal last_fire fixture: %v", err)
	}
	base.LastFire = raw

	dto := h.scheduleDTO(base, "")
	if dto.LastFire == nil {
		t.Fatal("populated last_fire must unmarshal into dto.LastFire, got nil")
	}
	got := *dto.LastFire
	if !got.FiredAt.Equal(want.FiredAt) {
		t.Errorf("fired_at = %v, want %v", got.FiredAt, want.FiredAt)
	}
	if got.Matched != want.Matched {
		t.Errorf("matched = %d, want %d", got.Matched, want.Matched)
	}
	if got.Capped != want.Capped {
		t.Errorf("capped = %v, want %v", got.Capped, want.Capped)
	}
	if len(got.Started) != 1 || got.Started[0].RunID != "run-1" ||
		got.Started[0].Title != "Fix the bug" || got.Started[0].IssueIID == nil || *got.Started[0].IssueIID != 42 {
		t.Errorf("started = %+v, want the single fixture entry", got.Started)
	}
	if len(got.Skips) != 1 || got.Skips[0].Reason != "already_running" || got.Skips[0].IssueIID != nil {
		t.Errorf("skips = %+v, want the single fixture skip", got.Skips)
	}
}

// TestScheduleDTOLastFireMalformedLeavesNil pins that a corrupt persisted payload never
// fails the DTO — it is logged and dto.LastFire is left nil (the rest of the DTO renders).
func TestScheduleDTOLastFireMalformedLeavesNil(t *testing.T) {
	h := &Handler{}
	base := store.RunSchedule{
		Target:   "issue",
		Timing:   "once",
		Timezone: "UTC",
		LastFire: []byte(`{not valid json`),
	}
	if dto := h.scheduleDTO(base, ""); dto.LastFire != nil {
		t.Fatalf("malformed last_fire must leave dto.LastFire nil, got %+v", dto.LastFire)
	}
}

// TestRunNowResponse pins the pure FireOutcome → RunNowResponse mapping (PRD #308 M3):
// Created == len(Started) and RunIDs derived from Started (back-compat), plus the
// structured Matched/Capped/Started/Skips carried through with run ids stringified and
// reasons stringified. The RunNow-does-not-persist invariant is pinned separately in
// schedsvc's TestRunNowDoesNotPersistLastFire.
func TestRunNowResponse(t *testing.T) {
	iid1 := int64(101)
	iid2 := int64(102)
	run1 := uuid.New()
	out := schedsvc.FireOutcome{
		Matched: 3,
		Capped:  true,
		Started: []schedsvc.Started{{IssueIID: &iid1, RunID: run1, Title: "started one"}},
		Skips: []schedsvc.Skip{
			{IssueIID: &iid2, Title: "skipped two", Reason: schedsvc.SkipAlreadyRunning},
			{IssueIID: nil, Title: "", Reason: schedsvc.SkipFetchFailed},
		},
	}

	resp := runNowResponse(out)

	if resp.Created != 1 {
		t.Errorf("created = %d, want 1 (len Started)", resp.Created)
	}
	if len(resp.RunIDs) != 1 || resp.RunIDs[0] != run1.String() {
		t.Errorf("run_ids = %v, want [%s]", resp.RunIDs, run1.String())
	}
	if resp.Matched != 3 {
		t.Errorf("matched = %d, want 3", resp.Matched)
	}
	if !resp.Capped {
		t.Error("capped = false, want true")
	}
	if len(resp.Started) != 1 {
		t.Fatalf("started len = %d, want 1", len(resp.Started))
	}
	if resp.Started[0].IssueIID == nil || *resp.Started[0].IssueIID != 101 ||
		resp.Started[0].RunID != run1.String() || resp.Started[0].Title != "started one" {
		t.Errorf("started[0] = %+v, want the mapped Started entry", resp.Started[0])
	}
	if len(resp.Skips) != 2 {
		t.Fatalf("skips len = %d, want 2", len(resp.Skips))
	}
	if resp.Skips[0].IssueIID == nil || *resp.Skips[0].IssueIID != 102 ||
		resp.Skips[0].Title != "skipped two" || resp.Skips[0].Reason != "already_running" {
		t.Errorf("skips[0] = %+v, want the mapped already_running skip", resp.Skips[0])
	}
	if resp.Skips[1].IssueIID != nil || resp.Skips[1].Reason != "fetch_failed" {
		t.Errorf("skips[1] = %+v, want the mapped fetch_failed skip", resp.Skips[1])
	}
}

// TestRunNowResponseEmptyOutcomeNonNilSlices pins the persisted-convention detail: an
// outcome that started nothing still yields non-nil (empty) Started/Skips slices, so the
// JSON carries [] rather than null.
func TestRunNowResponseEmptyOutcomeNonNilSlices(t *testing.T) {
	resp := runNowResponse(schedsvc.FireOutcome{Matched: 0})
	if resp.Started == nil {
		t.Error("Started must be a non-nil empty slice")
	}
	if resp.Skips == nil {
		t.Error("Skips must be a non-nil empty slice")
	}
	if resp.RunIDs == nil {
		t.Error("RunIDs must be a non-nil empty slice")
	}
	if resp.Created != 0 {
		t.Errorf("created = %d, want 0", resp.Created)
	}
}
