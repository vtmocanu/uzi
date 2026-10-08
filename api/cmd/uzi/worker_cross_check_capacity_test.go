package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestWorkerCrossCheckCapacityDisplays(t *testing.T) {
	zero, two := 0, 2
	now := time.Now()
	for _, tc := range []struct {
		name string
		cap  *int
		want string
	}{
		{"legacy", nil, "1/?"},
		{"disabled", &zero, "1/0"},
		{"enabled", &two, "1/2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := apitypes.WorkerDTO{ID: "capacity-worker", Name: tc.name, Status: "online", Busy: true, ActiveCrossChecks: 1, MaxCrossCheckSlots: tc.cap, MaxConcurrentRuns: &two, DrainingSince: &now}
			for _, admin := range []bool{false, true} {
				fc := &uzicli.FakeClient{Workers: []apitypes.WorkerDTO{w}, AdminWorkers: []apitypes.AdminWorkerDTO{{WorkerDTO: w}}}
				args := []string{"worker", "list"}
				if admin {
					args = []string{"admin", "workers"}
				}
				out, _, code := runCLI(t, fakeEnv(fc), args...)
				if code != uzicli.ExitOK {
					t.Fatalf("exit %d: %s", code, out)
				}
				for _, want := range []string{"RUN SLOTS", "CROSS-CHECKS", "0/2", tc.want, "(draining)"} {
					if !strings.Contains(out, want) {
						t.Fatalf("CLI missing %q: %s", want, out)
					}
				}
			}
			m := workersScene(true, "workers-list-120")
			m.workers.rows = []workerRow{{w: w}}
			m.workerDetail.workerID = w.ID
			lines, _ := m.workerDetailLines(now)
			detail := stripANSI(strings.Join(lines, "\n"))
			for _, want := range []string{"0/2 runs · " + tc.want + " cross-checks", "0 runs, 1 cross-checks left"} {
				if !strings.Contains(detail, want) {
					t.Fatalf("detail missing %q: %s", want, detail)
				}
			}
			if strings.Contains(detail, "chat active") {
				t.Fatalf("cross-check mislabeled as chat: %s", detail)
			}
			summary := stripANSI(m.workersSummary(200))
			if !strings.Contains(summary, "0/2 run slots in use") || !strings.Contains(summary, "1/") || !strings.Contains(summary, "cross-checks") {
				t.Fatalf("summary: %s", summary)
			}
		})
	}
}
