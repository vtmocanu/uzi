package schedsvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"bytes"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/schedtmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func gatedSweep(h *harness, c, k int32, n pgtype.Int4) store.RunSchedule {
	s := h.sweepSchedule(n)
	s.CapacityLimit = pgtype.Int4{Int32: c, Valid: true}
	s.CapacityRoomNeeded = pgtype.Int4{Int32: k, Valid: true}
	return s
}

func TestCapacityPausedAndUngatedHold(t *testing.T) {
	t.Run("paused gated recurring", func(t *testing.T) {
		h := newHarness()
		h.pauseOwner(nil)
		h.st.due = []store.RunSchedule{gatedSweep(h, 4, 2, pgtype.Int4{})}
		h.sched.Boot(context.Background())
		if h.st.capacityCalls != 0 || h.st.sweepLabelParam != nil || forgeCalls(h.fb.f) != 0 || len(h.st.advanceCalls) != 1 {
			t.Fatal("paused gate counted or fired")
		}
		var lf lastFireRecord
		if err := json.Unmarshal(h.st.advanceCalls[0].LastFire, &lf); err != nil {
			t.Fatal(err)
		}
		if lf.Capacity != nil || len(lf.Skips) != 1 || lf.Skips[0].Reason != string(SkipSchedulesPaused) {
			t.Fatalf("paused last fire=%+v", lf)
		}
	})
	t.Run("ungated credential hold", func(t *testing.T) {
		h := newHarness()
		h.runs.credDisabledErr = workersvc.ErrCredentialDisabled
		h.st.due = []store.RunSchedule{onceOf(h.sweepSchedule(pgtype.Int4{}))}
		h.sched.Boot(context.Background())
		h.sched.Boot(context.Background())
		if h.st.capacityCalls != 0 || len(h.st.heldFireCalls) != 1 || len(h.st.advanceCalls) != 0 || forgeCalls(h.fb.f) != 0 {
			t.Fatal("ungated hold changed")
		}
		raw := h.st.heldFireCalls[0].LastFire
		if bytes.Contains(raw, []byte("capacity")) {
			t.Fatalf("ungated hold serialized capacity: %s", raw)
		}
		started, reasons := heldLastFire(t, raw)
		if started != 0 || len(reasons) != 1 || reasons[0] != string(SkipCredentialDisabled) {
			t.Fatalf("hold=%s", raw)
		}
	})
}

func TestCapacityBlockedAdvancesWithoutForgeOrCandidates(t *testing.T) {
	h := newHarness()
	h.st.inFlight = 3
	h.st.repoErr = errors.New("repo resolution must not occur")
	s := gatedSweep(h, 4, 2, pgtype.Int4{Int32: 1, Valid: true})
	h.st.due = []store.RunSchedule{s}
	h.sched.Boot(context.Background())
	if h.st.capacityCalls != 1 || len(h.st.advanceCalls) != 1 {
		t.Fatalf("count=%d advances=%d", h.st.capacityCalls, len(h.st.advanceCalls))
	}
	if h.st.sweepLabelParam != nil || len(h.fb.f.getIID) != 0 || h.fb.f.listCount != 0 || len(h.runs.autopilot) != 0 {
		t.Fatal("blocked sweep performed candidate/forge/run work")
	}
	var record struct {
		Matched  int
		Capacity *CapacityCheck
		Skips    []Skip
	}
	if err := json.Unmarshal(h.st.advanceCalls[0].LastFire, &record); err != nil {
		t.Fatal(err)
	}
	if record.Matched != 0 || len(record.Skips) != 0 || record.Capacity == nil || *record.Capacity != (CapacityCheck{InFlight: 3, Limit: 4, RoomNeeded: 2, Room: 1, Blocked: true}) {
		t.Fatalf("last fire: %+v", record)
	}
}

func TestCapacityBatchAndBackfill(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int64
		cap   pgtype.Int4
		want  int
	}{
		{"partial", 4, pgtype.Int4{Int32: 3, Valid: true}, 2},
		{"unlimited", 4, pgtype.Int4{}, 2},
		{"smaller batch", 1, pgtype.Int4{Int32: 1, Valid: true}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			h.st.inFlight = tc.count
			h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 1}, {ForgeIssueIid: 2}, {ForgeIssueIid: 3}, {ForgeIssueIid: 4}, {ForgeIssueIid: 5}}
			h.st.activeByIssue = map[int64]bool{1: true}
			h.st.sweepCount = 30
			s := gatedSweep(h, 6, 2, tc.cap)
			out, err := h.sched.RunNow(context.Background(), s)
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Started) != tc.want || out.Matched != tc.want+1 || !out.Capped || out.Capacity == nil || out.Capacity.Blocked {
				t.Fatalf("out=%+v", out)
			}
			if int(h.st.sweepMaxIssuesParam.Int32) != tc.want+backfillHeadroom {
				t.Fatalf("scan limit=%+v", h.st.sweepMaxIssuesParam)
			}
			if s.MaxIssues != tc.cap || len(h.st.advanceCalls) != 0 {
				t.Fatal("manual fire persisted or changed stored cap")
			}
		})
	}
}

func TestCapacityClampCountErrorAndCatalogDrift(t *testing.T) {
	t.Run("clamped", func(t *testing.T) {
		h := newHarness()
		h.st.inFlight = 7
		out, err := h.sched.RunNow(context.Background(), gatedSweep(h, 6, 2, pgtype.Int4{}))
		if err != nil || out.Capacity == nil || out.Capacity.Room != 0 || !out.Capacity.Blocked {
			t.Fatalf("out=%+v err=%v", out, err)
		}
	})
	t.Run("count error does not advance", func(t *testing.T) {
		h := newHarness()
		h.st.inFlightErr = errors.New("count unavailable")
		h.st.due = []store.RunSchedule{gatedSweep(h, 4, 2, pgtype.Int4{})}
		h.sched.Boot(context.Background())
		if len(h.st.advanceCalls) != 0 || len(h.st.statusCalls) != 0 || h.st.sweepLabelParam != nil {
			t.Fatal("transient count advanced/parked/queried candidates")
		}
	})
	t.Run("catalog selector drift", func(t *testing.T) {
		h := newHarness()
		s := gatedSweep(h, 4, 2, pgtype.Int4{})
		s.Origin = "default"
		s.CatalogSlug = pgtype.Text{String: "assigned-sweep", Valid: true}
		h.sched.catalog = func(string) (schedtmpl.DefaultJob, bool) {
			return schedtmpl.DefaultJob{SelectorKind: schedtmpl.SelectorAssigned}, true
		}
		h.st.repoErr = errors.New("must not resolve")
		out, err := h.sched.RunNow(context.Background(), s)
		if err != nil || out.Matched != 1 || len(out.Skips) != 1 || out.Skips[0].Reason != SkipConfigNotSupported || out.Skips[0].IssueIID != nil || out.Capacity != nil || h.st.capacityCalls != 0 {
			t.Fatalf("out=%+v count=%d err=%v", out, h.st.capacityCalls, err)
		}
	})
}
