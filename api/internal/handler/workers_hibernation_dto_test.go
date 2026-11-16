package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestWorkerDTOHibernationFields(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	w := store.Worker{
		Kind: "hosted", Status: "offline", Version: pgtype.Text{String: "0.1.0", Valid: true},
		HibernationState:  "hibernated",
		HibernationReason: pgtype.Text{String: "idle", Valid: true},
		HibernatedAt:      pgtype.Timestamptz{Time: now, Valid: true},
		WakeRequestedAt:   pgtype.Timestamptz{Time: now, Valid: true},
		WakeError:         pgtype.Text{String: "timed out", Valid: true},
	}
	dto := workerDTOFromWorker(w, 0, false, "", "0.84.0", "0.84.0", now, now.Add(-24*time.Hour))
	if dto.HibernationState != "hibernated" || dto.HibernationReason == nil || *dto.HibernationReason != "idle" ||
		dto.HibernatedAt == nil || dto.WakeRequestedAt == nil || dto.WakeError == nil || *dto.WakeError != "timed out" {
		t.Fatalf("hibernation fields not carried: %+v", dto)
	}
	if dto.UpgradeStatus == "outdated" || dto.UpgradeStatus == "upgrading" {
		t.Fatalf("hibernated worker reported %q", dto.UpgradeStatus)
	}
	if !dto.EffectiveCordoned {
		t.Error("hibernated worker must not accept intake")
	}
	zero := workerDTOFromWorker(store.Worker{Kind: "external", Status: "online"}, 0, false, "", "0.84.0", "", now, now)
	if zero.HibernationState != "none" || zero.HibernationReason != nil || zero.HibernatedAt != nil || zero.WakeRequestedAt != nil || zero.WakeError != nil {
		t.Fatalf("zero worker: %+v", zero)
	}
}

// The owner /workers list builds DTOs from store.ListWorkersByUserRow, a separate path from
// workerDTOFromWorker; it must project the cleanup columns too.
func TestWorkerDTOFromRowHibernationCleanup(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	txt := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	cases := []struct {
		name                   string
		row                    store.ListWorkersByUserRow
		wantNil                bool
		state, outcome, reason string
	}{
		{name: "none is null", row: store.ListWorkersByUserRow{CleanupPhase: "none"}, wantNil: true},
		{name: "started is cleaning", row: store.ListWorkersByUserRow{CleanupPhase: "started"}, state: "cleaning"},
		{name: "settled skipped unsupported", row: store.ListWorkersByUserRow{CleanupPhase: "settled", HibernationCleanup: txt("skipped"), HibernationCleanupReason: txt("unsupported")}, state: "settled", outcome: "skipped", reason: "unsupported"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.row.Kind, c.row.Status = "hosted", "offline"
			dto := workerDTOFromRow(c.row, "0.84.0", "0.84.0", now, now)
			got := dto.HibernationCleanup
			if c.wantNil {
				if got != nil {
					t.Fatalf("want nil, got %+v", *got)
				}
				return
			}
			if got == nil || got.State != c.state {
				t.Fatalf("state: %+v want %q", got, c.state)
			}
			str := func(p *string) string {
				if p == nil {
					return ""
				}
				return *p
			}
			if str(got.Outcome) != c.outcome || str(got.CleanupReason) != c.reason {
				t.Fatalf("outcome/reason: %+v", got)
			}
		})
	}
}

// The automatic pass's blocker rides on every worker DTO producer: the bare-row builder (admin
// list, register, patch) and the owner list/get builder. Set values carry through; unset ones
// marshal as JSON null; an unknown reason is sanitized and bounded.
func TestWorkerDTOHibernationBlocked(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	since := now.Add(-48 * time.Hour)
	at := now.Add(-time.Hour)
	ts := func(v time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: v, Valid: true} }
	check := func(t *testing.T, dto apitypes.WorkerDTO, wantReason string) {
		t.Helper()
		if dto.HibernationBlockedReason == nil || *dto.HibernationBlockedReason != wantReason {
			t.Fatalf("reason = %v, want %q", dto.HibernationBlockedReason, wantReason)
		}
		if dto.HibernationBlockedSince == nil || !dto.HibernationBlockedSince.Equal(since) ||
			dto.HibernationBlockedAt == nil || !dto.HibernationBlockedAt.Equal(at) {
			t.Fatalf("since/at = %v / %v", dto.HibernationBlockedSince, dto.HibernationBlockedAt)
		}
	}
	checkNull := func(t *testing.T, dto apitypes.WorkerDTO) {
		t.Helper()
		if dto.HibernationBlockedReason != nil || dto.HibernationBlockedSince != nil || dto.HibernationBlockedAt != nil {
			t.Fatalf("want all null, got %v %v %v", dto.HibernationBlockedReason, dto.HibernationBlockedSince, dto.HibernationBlockedAt)
		}
		b, err := json.Marshal(dto)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"hibernation_blocked_reason", "hibernation_blocked_since", "hibernation_blocked_at"} {
			if !strings.Contains(string(b), `"`+k+`":null`) {
				t.Fatalf("%s is not an explicit null in %s", k, b)
			}
		}
	}
	reason := pgtype.Text{String: "custody_hold", Valid: true}

	t.Run("bare worker row, set and unset", func(t *testing.T) {
		w := store.Worker{Kind: "hosted", Status: "online", HibernationBlockedReason: reason,
			HibernationBlockedSince: ts(since), HibernationBlockedAt: ts(at)}
		check(t, workerDTOFromWorker(w, 0, false, "", "0.84.0", "0.84.0", now, now), "custody_hold")
		checkNull(t, workerDTOFromWorker(store.Worker{Kind: "hosted", Status: "online"}, 0, false, "", "0.84.0", "0.84.0", now, now))
	})
	t.Run("owner list and get row, set and unset", func(t *testing.T) {
		row := store.ListWorkersByUserRow{Kind: "hosted", Status: "online", HibernationBlockedReason: reason,
			HibernationBlockedSince: ts(since), HibernationBlockedAt: ts(at)}
		check(t, workerDTOFromRow(row, "0.84.0", "0.84.0", now, now), "custody_hold")
		checkNull(t, workerDTOFromRow(store.ListWorkersByUserRow{Kind: "hosted", Status: "online"}, "0.84.0", "0.84.0", now, now))
	})
	t.Run("an unknown reason is sanitized and bounded", func(t *testing.T) {
		evil := pgtype.Text{String: "x\x1b[31m\n" + strings.Repeat("世", 60), Valid: true}
		dto := workerDTOFromWorker(store.Worker{Kind: "hosted", Status: "online", HibernationBlockedReason: evil}, 0, false, "", "0.84.0", "0.84.0", now, now)
		got := dto.HibernationBlockedReason
		if got == nil || strings.ContainsAny(*got, "\x1b\n") || len(*got) > workerBlockedReasonBytes || !utf8.ValidString(*got) || !strings.HasPrefix(*got, "x") {
			t.Fatalf("got %v", got)
		}
	})
}

func TestWorkerDTOHibernationCleanup(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	txt := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	cases := []struct {
		name                   string
		w                      store.Worker
		wantNil                bool
		state, outcome, reason string
	}{
		{name: "none without outcome", w: store.Worker{CleanupPhase: "none"}, wantNil: true},
		{name: "empty phase without outcome", w: store.Worker{}, wantNil: true},
		{name: "requested is cleaning", w: store.Worker{CleanupPhase: "requested"}, state: "cleaning"},
		{name: "started is cleaning", w: store.Worker{CleanupPhase: "started"}, state: "cleaning"},
		{name: "unknown", w: store.Worker{CleanupPhase: "unknown"}, state: "unknown"},
		{name: "settled pruned", w: store.Worker{CleanupPhase: "settled", HibernationCleanup: txt("pruned")}, state: "settled", outcome: "pruned"},
		{name: "none with skipped outcome", w: store.Worker{CleanupPhase: "none", HibernationCleanup: txt("skipped"), HibernationCleanupReason: txt("unsupported")}, state: "none", outcome: "skipped", reason: "unsupported"},
		{name: "settled failed", w: store.Worker{CleanupPhase: "settled", HibernationCleanup: txt("failed"), HibernationCleanupReason: txt("interrupted")}, state: "settled", outcome: "failed", reason: "interrupted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.w.Kind, c.w.Status = "hosted", "offline"
			dto := workerDTOFromWorker(c.w, 0, false, "", "0.84.0", "0.84.0", now, now)
			if c.wantNil {
				if dto.HibernationCleanup != nil {
					t.Fatalf("want nil, got %+v", *dto.HibernationCleanup)
				}
				return
			}
			got := dto.HibernationCleanup
			if got == nil || got.State != c.state {
				t.Fatalf("state: %+v want %q", got, c.state)
			}
			str := func(p *string) string {
				if p == nil {
					return ""
				}
				return *p
			}
			if str(got.Outcome) != c.outcome || str(got.CleanupReason) != c.reason {
				t.Fatalf("outcome/reason: %+v", got)
			}
		})
	}
}
