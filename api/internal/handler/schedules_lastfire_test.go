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

// TestRunNowResponseIneligibleMatched (issue #1543): the label sweep's ineligible count is
// carried through to the run-now wire, and a nil (non-label sweep) stays nil (unknown).
func TestRunNowResponseIneligibleMatched(t *testing.T) {
	n := int64(16)
	resp := runNowResponse(schedsvc.FireOutcome{IneligibleMatched: &n})
	if resp.IneligibleMatched == nil || *resp.IneligibleMatched != 16 {
		t.Errorf("ineligible_matched = %v, want 16", resp.IneligibleMatched)
	}
	if resp := runNowResponse(schedsvc.FireOutcome{}); resp.IneligibleMatched != nil {
		t.Errorf("nil IneligibleMatched must stay nil, got %d", *resp.IneligibleMatched)
	}
}

// TestScheduleDTORecentFires pins the recent_fires mapping (issue #2519): always a
// non-nil array on the wire, order preserved, a bad element dropped, a bad array empty.
func TestScheduleDTORecentFires(t *testing.T) {
	h := &Handler{}
	base := store.RunSchedule{Target: "sweep", Timing: "recurring", Timezone: "UTC"}
	recentJSON := func(t *testing.T, raw []byte) (apitypes.ScheduleDTO, string) {
		t.Helper()
		r := base
		r.RecentFires = raw
		dto := h.scheduleDTO(r, "")
		b, err := json.Marshal(dto)
		if err != nil {
			t.Fatalf("marshal dto: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal dto: %v", err)
		}
		return dto, string(m["recent_fires"])
	}

	t.Run("empty array and nil bytes serialize as []", func(t *testing.T) {
		for name, raw := range map[string][]byte{"empty": []byte(`[]`), "nil": nil, "zero-length": {}} {
			dto, got := recentJSON(t, raw)
			if got != `[]` || len(dto.RecentFires) != 0 {
				t.Errorf("%s: recent_fires = %s, want []", name, got)
			}
		}
	})

	fire := func(day int, matched int) apitypes.LastFire {
		return apitypes.LastFire{FiredAt: time.Date(2026, 8, day, 2, 0, 0, 0, time.UTC), Matched: matched}
	}
	marshalAll := func(t *testing.T, v any) []byte {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}

	t.Run("order is kept", func(t *testing.T) {
		dto, _ := recentJSON(t, marshalAll(t, []apitypes.LastFire{fire(3, 3), fire(2, 2), fire(1, 1)}))
		if len(dto.RecentFires) != 3 {
			t.Fatalf("len = %d, want 3", len(dto.RecentFires))
		}
		for i, want := range []int{3, 2, 1} {
			if dto.RecentFires[i].Matched != want {
				t.Errorf("[%d].Matched = %d, want %d", i, dto.RecentFires[i].Matched, want)
			}
		}
	})

	t.Run("malformed element is dropped", func(t *testing.T) {
		good1, good2 := marshalAll(t, fire(2, 2)), marshalAll(t, fire(1, 1))
		raw := []byte(`[` + string(good1) + `,{"fired_at":"not-a-time"},` + string(good2) + `]`)
		dto, _ := recentJSON(t, raw)
		if len(dto.RecentFires) != 2 || dto.RecentFires[0].Matched != 2 || dto.RecentFires[1].Matched != 1 {
			t.Fatalf("recent_fires = %+v, want the two good entries in order", dto.RecentFires)
		}
	})

	t.Run("malformed array is empty", func(t *testing.T) {
		dto, got := recentJSON(t, []byte(`{"not":"an array"}`))
		if got != `[]` || len(dto.RecentFires) != 0 {
			t.Errorf("recent_fires = %s, want []", got)
		}
	})
}
