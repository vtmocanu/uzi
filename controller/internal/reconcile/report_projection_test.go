package reconcile

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/controller/internal/apiclient"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func TestTickProjectsRollHealthOntoStatusWire(t *testing.T) {
	client, reports := statusWireClient(t)
	exit137, exit0 := int32(137), int32(0)
	m := &fakeMaterializer{observed: []ObservedWorker{
		{
			ID: "worker-stuck",
			Roll: RollHealth{
				Phase:             protocol.PhaseStuck,
				PhaseSince:        time.Date(2026, 10, 5, 8, 1, 2, 123000000, time.UTC),
				TargetImage:       "registry.example/uzi/agent-jvm:stuck-tag",
				PodPhase:          "Failed",
				BlockingContainer: "seed-nix",
				BlockingReason:    "CrashLoopBackOff",
				RestartCount:      7,
				LastExitCode:      &exit137,
			},
		},
		// Keep the omitted row between signals to expose accidental row reuse.
		{
			ID: "worker-no-signal",
			Roll: RollHealth{
				TargetImage:       "registry.example/uzi/agent-unused:no-signal",
				PodPhase:          "Unknown",
				BlockingContainer: "unused-container",
				BlockingReason:    "unused-reason",
				RestartCount:      99,
			},
		},
		{
			ID: "worker-settled",
			Roll: RollHealth{
				Phase:        protocol.PhaseSettled,
				PhaseSince:   time.Date(2026, 10, 5, 9, 3, 4, 456000000, time.UTC),
				TargetImage:  "registry.example/uzi/agent-base:settled-tag",
				PodPhase:     "Running",
				RestartCount: 0,
				LastExitCode: &exit0,
			},
		},
		{
			ID: "worker-rolling",
			Roll: RollHealth{
				Phase:        protocol.PhaseRolling,
				TargetImage:  "registry.example/uzi/agent-python:rolling-tag",
				PodPhase:     "Pending",
				RestartCount: 2,
			},
		},
	}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	l := New(&fakePoller{}, m, client, 37*time.Second, "controller-target", log)
	l.now = func() time.Time {
		return time.Date(2026, 10, 5, 10, 11, 12, 789000000, time.UTC)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := l.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	assertStatusWire(t, reports, `{
		"reported_at": "2026-10-05T10:11:12.789Z",
		"poll_interval_seconds": 37,
		"worker_image_tag": "controller-target",
		"workers": [
			{
				"id": "worker-stuck",
				"phase": "stuck",
				"phase_since": "2026-10-05T08:01:02.123Z",
				"target_image": "registry.example/uzi/agent-jvm:stuck-tag",
				"pod_phase": "Failed",
				"blocking_container": "seed-nix",
				"blocking_reason": "CrashLoopBackOff",
				"restart_count": 7,
				"last_exit_code": 137
			},
			{
				"id": "worker-settled",
				"phase": "settled",
				"phase_since": "2026-10-05T09:03:04.456Z",
				"target_image": "registry.example/uzi/agent-base:settled-tag",
				"pod_phase": "Running",
				"blocking_container": null,
				"blocking_reason": null,
				"restart_count": 0,
				"last_exit_code": 0
			},
			{
				"id": "worker-rolling",
				"phase": "rolling",
				"phase_since": null,
				"target_image": "registry.example/uzi/agent-python:rolling-tag",
				"pod_phase": "Pending",
				"blocking_container": null,
				"blocking_reason": null,
				"restart_count": 2,
				"last_exit_code": null
			}
		]
	}`)
}

func TestTickReplacesRollHealthStatusAcrossTicks(t *testing.T) {
	client, reports := statusWireClient(t)
	exit137 := int32(137)
	m := &fakeMaterializer{observed: []ObservedWorker{{
		ID: "worker-transition",
		Roll: RollHealth{
			Phase:             protocol.PhaseStuck,
			PhaseSince:        time.Date(2026, 10, 5, 11, 0, 1, 0, time.UTC),
			TargetImage:       "registry.example/uzi/agent-base:before",
			PodPhase:          "Pending",
			BlockingContainer: "fetch-source",
			BlockingReason:    "ImagePullBackOff",
			RestartCount:      9,
			LastExitCode:      &exit137,
		},
	}}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	l := New(&fakePoller{}, m, client, 23*time.Second, "transition-target", log)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := l.Tick(ctx); err != nil {
		t.Fatalf("stuck Tick: %v", err)
	}
	assertStatusWire(t, reports, `{
		"reported_at": "2026-10-05T12:00:00Z",
		"poll_interval_seconds": 23,
		"worker_image_tag": "transition-target",
		"workers": [{
			"id": "worker-transition",
			"phase": "stuck",
			"phase_since": "2026-10-05T11:00:01Z",
			"target_image": "registry.example/uzi/agent-base:before",
			"pod_phase": "Pending",
			"blocking_container": "fetch-source",
			"blocking_reason": "ImagePullBackOff",
			"restart_count": 9,
			"last_exit_code": 137
		}]
	}`)

	now = time.Date(2026, 10, 5, 12, 0, 23, 0, time.UTC)
	m.observed = []ObservedWorker{{
		ID: "worker-transition",
		Roll: RollHealth{
			Phase:       protocol.PhaseSettled,
			PhaseSince:  time.Date(2026, 10, 5, 12, 0, 17, 0, time.UTC),
			TargetImage: "registry.example/uzi/agent-base:after",
			PodPhase:    "Running",
		},
	}}
	if err := l.Tick(ctx); err != nil {
		t.Fatalf("settled Tick: %v", err)
	}
	assertStatusWire(t, reports, `{
		"reported_at": "2026-10-05T12:00:23Z",
		"poll_interval_seconds": 23,
		"worker_image_tag": "transition-target",
		"workers": [{
			"id": "worker-transition",
			"phase": "settled",
			"phase_since": "2026-10-05T12:00:17Z",
			"target_image": "registry.example/uzi/agent-base:after",
			"pod_phase": "Running",
			"blocking_container": null,
			"blocking_reason": null,
			"restart_count": 0,
			"last_exit_code": null
		}]
	}`)

	now = time.Date(2026, 10, 5, 12, 0, 46, 0, time.UTC)
	m.observed = nil
	if err := l.Tick(ctx); err != nil {
		t.Fatalf("empty fleet Tick: %v", err)
	}
	assertStatusWire(t, reports, `{
		"reported_at": "2026-10-05T12:00:46Z",
		"poll_interval_seconds": 23,
		"worker_image_tag": "transition-target",
		"workers": []
	}`)
}

type capturedStatusWire struct {
	body []byte
	err  error
}

func statusWireClient(t *testing.T) (*apiclient.Client, <-chan capturedStatusWire) {
	t.Helper()
	reports := make(chan capturedStatusWire, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		capture := capturedStatusWire{body: body, err: err}
		// At most three requests are expected. An extra request cannot block the handler.
		select {
		case reports <- capture:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	// apiclient.Client has a private transport and no Close method.
	// Disable keep-alives and explicitly close server connections during cleanup.
	server.Config.SetKeepAlivesEnabled(false)
	server.Start()
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return apiclient.New(server.URL, "test-controller-auth", 2*time.Second, nil, log), reports
}

func assertStatusWire(t *testing.T, reports <-chan capturedStatusWire, expected string) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	var got capturedStatusWire
	select {
	case got = <-reports:
	case <-timer.C:
		t.Fatal("timed out waiting for controller status POST")
	}
	if got.err != nil {
		t.Fatalf("read status body: %v", got.err)
	}
	// Decode both JSON documents without protocol structs so missing fields,
	// extra fields, nulls and zero values remain observable; object order does not.
	var actualJSON, expectedJSON any
	if err := json.Unmarshal(got.body, &actualJSON); err != nil {
		t.Fatalf("decode status JSON: %v; body=%s", err, got.body)
	}
	if err := json.Unmarshal([]byte(expected), &expectedJSON); err != nil {
		t.Fatalf("decode expected JSON: %v", err)
	}
	if !reflect.DeepEqual(actualJSON, expectedJSON) {
		t.Fatalf("status JSON mismatch\ngot: %s\nwant: %s", got.body, expected)
	}
}
