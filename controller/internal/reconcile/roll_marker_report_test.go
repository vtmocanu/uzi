package reconcile

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

type markerMaterializer struct {
	pacingMaterializer
	markers map[string]*protocol.RollMarker
}

func (m *markerMaterializer) RollMarkerReports() map[string]*protocol.RollMarker {
	return m.markers
}

// report() forwards a marker onto rows it already emits and never adds a row for a
// worker that has a marker but neither a phase nor a pacing outcome.
func TestReportForwardsRollMarkerOnEmittedRowsOnly(t *testing.T) {
	client, reports := statusWireClient(t)
	mk := func(group string) *protocol.RollMarker {
		return &protocol.RollMarker{State: protocol.RollMarkerPresent, Completed: true, CoverageGroup: group, Reservation: true}
	}
	m := &markerMaterializer{
		pacingMaterializer: pacingMaterializer{
			fakeMaterializer: fakeMaterializer{observed: []ObservedWorker{
				{ID: "phased", Roll: RollHealth{Phase: protocol.PhaseSettled, PodPhase: "Running"}},
				{ID: "gap"},
				{ID: "marker-only"},
			}},
			outcomes: map[string]*protocol.Pacing{"gap": {State: "admitted", TargetImage: "gap-image"}},
		},
		markers: map[string]*protocol.RollMarker{
			"phased": mk("aaaaaaaaaaaaaaaa"), "gap": mk("bbbbbbbbbbbbbbbb"), "marker-only": mk("cccccccccccccccc"),
		},
	}
	p := &fakePoller{resp: protocol.PollResponse{Workers: []protocol.DesiredWorker{{ID: "phased"}, {ID: "gap"}, {ID: "marker-only"}}}}
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
	if len(got.Workers) != 2 {
		t.Fatalf("rows = %d, want 2 (no row for a marker-only worker): %s", len(got.Workers), capture.body)
	}
	if w := byID["phased"]; w.RollMarker == nil || w.RollMarker.CoverageGroup != "aaaaaaaaaaaaaaaa" {
		t.Errorf("phased row marker = %+v", w.RollMarker)
	}
	if w := byID["gap"]; w.RollMarker == nil || w.RollMarker.CoverageGroup != "bbbbbbbbbbbbbbbb" {
		t.Errorf("rolling-gap row marker = %+v", w.RollMarker)
	}
	if _, ok := byID["marker-only"]; ok {
		t.Error("marker-only worker got a row")
	}
}
