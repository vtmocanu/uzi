package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

type pacingMaterializer struct {
	fakeMaterializer
	outcomes map[string]*protocol.Pacing
}

func (m *pacingMaterializer) PacingOutcomes() map[string]*protocol.Pacing {
	return m.outcomes
}

func TestTickMergesFleetReportInSinglePOST(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "reconcile-error"
		}
		t.Run(name, func(t *testing.T) {
			client, reports := statusWireClient(t)
			boom := errors.New("patch failed")
			since := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
			exit := int32(137)
			m := &pacingMaterializer{
				fakeMaterializer: fakeMaterializer{observed: []ObservedWorker{
					{ID: "stuck", Roll: RollHealth{Phase: protocol.PhaseStuck, PhaseSince: since, TargetImage: "old-image", PodPhase: "Pending", BlockingContainer: "seed-nix", BlockingReason: "CrashLoopBackOff", RestartCount: 7, LastExitCode: &exit}},
					{ID: "settled", Roll: RollHealth{Phase: protocol.PhaseSettled, TargetImage: "retained-image", PodPhase: "Running"}},
					{ID: "gap"},
					{ID: "no-signal"},
					{ID: "health-only", Roll: RollHealth{Phase: protocol.PhaseRolling}},
				}},
				outcomes: map[string]*protocol.Pacing{
					"stuck":       {State: "admitted", TargetImage: "new-image", TargetVersion: "2"},
					"settled":     {State: "held", TargetImage: "new-image", BlockingWorkerID: strPtr("stuck"), BlockingReason: strPtr("canary stuck")},
					"gap":         {State: "admitted", TargetImage: "gap-image"},
					"absent":      {State: "held", TargetImage: "absent-image"},
					"not-desired": {State: "held", TargetImage: "excluded-image"},
				},
			}
			if fail {
				m.reconcileErr = boom
			}
			p := &fakePoller{resp: protocol.PollResponse{Workers: []protocol.DesiredWorker{
				{ID: "stuck"}, {ID: "settled"}, {ID: "gap"}, {ID: "absent"}, {ID: "no-signal"},
			}}}
			l := New(p, m, client, 37*time.Second, "target-tag", slog.New(slog.NewTextHandler(io.Discard, nil)))
			now := since.Add(time.Minute)
			l.now = func() time.Time { return now }
			err := l.Tick(context.Background())
			if fail && !errors.Is(err, boom) || !fail && err != nil {
				t.Fatalf("Tick error = %v", err)
			}
			var capture capturedStatusWire
			select {
			case capture = <-reports:
			default:
				t.Fatal("no fleet status POST")
			}
			if capture.err != nil {
				t.Fatal(capture.err)
			}
			var got protocol.StatusReport
			if err := json.Unmarshal(capture.body, &got); err != nil {
				t.Fatal(err)
			}
			want := protocol.StatusReport{
				Kind: "fleet", ReportedAt: now, PollIntervalSeconds: 37, WorkerImageTag: "target-tag",
				Workers: []protocol.WorkerStatus{
					{ID: "stuck", Phase: protocol.PhaseStuck, PhaseSince: &since, TargetImage: "old-image", PodPhase: "Pending", BlockingContainer: strPtr("seed-nix"), BlockingReason: strPtr("CrashLoopBackOff"), RestartCount: 7, LastExitCode: &exit, Pacing: m.outcomes["stuck"]},
					{ID: "settled", Phase: protocol.PhaseSettled, TargetImage: "retained-image", PodPhase: "Running", Pacing: m.outcomes["settled"]},
					{ID: "health-only", Phase: protocol.PhaseRolling},
					{ID: "gap", Phase: protocol.PhaseRolling, TargetImage: "gap-image", Pacing: m.outcomes["gap"]},
					{ID: "absent", Phase: protocol.PhaseRolling, TargetImage: "absent-image", Pacing: m.outcomes["absent"]},
				},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("fleet report = %+v\nwant %+v\nwire %s", got, want, capture.body)
			}
			select {
			case extra := <-reports:
				t.Fatalf("second status POST: %s", extra.body)
			default:
			}
		})
	}
}

// A held pacing entry for an ephemeral (run-bound) worker rides on its observed row
// with that row's own phase; when the worker has no health signal it gets NO synthetic
// rolling row, because the api stamps its upgrading_since ceiling anchor on the first
// non-terminal phase. A non-ephemeral worker in the same position still gets the
// rolling-gap row (control).
func TestEphemeralHeldPacingNeverGetsSyntheticRollingRow(t *testing.T) {
	client, reports := statusWireClient(t)
	held := func(img string) *protocol.Pacing {
		return &protocol.Pacing{State: "held", TargetImage: img, BlockingReason: strPtr("layout_migration_custody")}
	}
	m := &pacingMaterializer{
		fakeMaterializer: fakeMaterializer{observed: []ObservedWorker{
			{ID: "eph-observed", Roll: RollHealth{Phase: protocol.PhaseSettled, PodPhase: "Running"}},
			{ID: "eph-nosignal"},
			{ID: "plain-nosignal"},
		}},
		outcomes: map[string]*protocol.Pacing{
			"eph-observed":   held("img-a"),
			"eph-nosignal":   held("img-b"),
			"plain-nosignal": held("img-c"),
		},
	}
	p := &fakePoller{resp: protocol.PollResponse{Workers: []protocol.DesiredWorker{
		{ID: "eph-observed", Ephemeral: true}, {ID: "eph-nosignal", Ephemeral: true}, {ID: "plain-nosignal"},
	}}}
	l := New(p, m, client, 10*time.Second, "tag", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := l.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	var capture capturedStatusWire
	select {
	case capture = <-reports:
	default:
		t.Fatal("no fleet status POST")
	}
	var got protocol.StatusReport
	if err := json.Unmarshal(capture.body, &got); err != nil {
		t.Fatal(err)
	}
	byID := map[string]protocol.WorkerStatus{}
	for _, w := range got.Workers {
		byID[w.ID] = w
	}
	if w, ok := byID["eph-observed"]; !ok || w.Phase != protocol.PhaseSettled || w.Pacing == nil || w.Pacing.State != "held" {
		t.Errorf("observed ephemeral row = %+v, want settled phase with the held pacing attached", w)
	}
	if w, ok := byID["eph-nosignal"]; ok {
		t.Errorf("ephemeral worker without health got a synthetic row: %+v", w)
	}
	if w, ok := byID["plain-nosignal"]; !ok || w.Phase != protocol.PhaseRolling || w.Pacing == nil {
		t.Errorf("control: non-ephemeral gap row = %+v, want rolling with pacing", w)
	}
}
