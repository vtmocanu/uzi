package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestIssue2009PendingAggregateTUI(t *testing.T) {
	for _, tc := range []struct {
		name     string
		count    *int64
		overflow *bool
		rows     int
		want     string
	}{
		{"zero overflow empty", i64(0), bp(true), 0, "0 confirmed pending; more may be pending"},
		{"aggregate only", i64(3), bp(false), 0, "3 pending outcomes"},
		{"singular", i64(1), nil, 2, "1 pending outcome"},
		{"equal membership", i64(1), bp(false), 1, "1 pending outcome"},
		{"zero overrides rows", i64(0), bp(false), 1, ""},
		{"overflow fallback", nil, bp(true), 1, "1 confirmed pending; more may be pending"},
		{"legacy", nil, nil, 1, "outcome pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := workerRenderModel(true)
			w := &m.workers.rows[0].w
			w.PendingOutcomeCount, w.PendingOverflow = tc.count, tc.overflow
			since := time.Now().Add(-2 * time.Hour)
			for i := 0; i < tc.rows; i++ {
				w.ReportedRuns = append(w.ReportedRuns, apitypes.WorkerReportedRunDTO{RunID: "known", Phase: "running", ClaimGeneration: 7, TerminalPending: true, TerminalPendingSince: &since})
			}
			items := workerAttention(m.workers.rows[0], time.Now())
			var texts []string
			for _, item := range items {
				texts = append(texts, item.attnDetail)
			}
			attention := strings.Join(texts, "\n")
			if tc.want != "" && !strings.Contains(attention, tc.want) {
				t.Fatalf("attention: %s", attention)
			}
			if tc.count != nil && strings.Contains(attention, "outcome pending") {
				t.Fatalf("aggregate got row age: %s", attention)
			}
			if tc.want == "" && strings.Contains(attention, "pending") {
				t.Fatalf("zero authority: %s", attention)
			}
			out := stripANSI(m.renderWorker())
			if tc.want != "" {
				requireWorkerText(t, out, tc.want)
			}
			if tc.rows == 0 {
				requireWorkerText(t, out, "none")
				if strings.Contains(out, "open run") || strings.Contains(out, "gen ") {
					t.Fatalf("invented navigation: %s", out)
				}
			} else {
				requireWorkerText(t, out, "known", "gen 7", "outcome pending 2h")
			}
			if len(w.ReportedRuns) != tc.rows || m.workerDetail.cursor != 0 {
				t.Fatal("aggregate changed rows/cursor")
			}
		})
	}
}
