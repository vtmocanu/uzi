package reconcile

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/controller/internal/protocol"
)

type fakePoller struct {
	resp  protocol.PollResponse
	err   error
	calls int
}

func (f *fakePoller) Poll(context.Context) (protocol.PollResponse, error) {
	f.calls++
	return f.resp, f.err
}

type fakeMaterializer struct {
	observed     []ObservedWorker
	observeErr   error
	reconcileErr error
	// gotDesired / gotObserved record what each Reconcile was handed.
	gotDesired  [][]protocol.DesiredWorker
	gotObserved [][]ObservedWorker
}

func (f *fakeMaterializer) Observe(context.Context) ([]ObservedWorker, error) {
	return f.observed, f.observeErr
}

func (f *fakeMaterializer) Reconcile(_ context.Context, desired []protocol.DesiredWorker, observed []ObservedWorker) error {
	f.gotDesired = append(f.gotDesired, desired)
	f.gotObserved = append(f.gotObserved, observed)
	return f.reconcileErr
}

// testLoop wires no Reporter: roll-health reporting is optional, and the pre-#113
// cycle behaviour these tests pin must stay exactly as it was with it absent.
func testLoop(p Poller, m Materializer) *Loop {
	return New(p, m, nil, time.Hour, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A cycle hands Reconcile both sides: desired from the api, observed from the
// cluster. Decision 9's drift check needs them side by side.
func TestTickReconcilesDesiredAgainstObserved(t *testing.T) {
	token := "uzw_pending"
	p := &fakePoller{resp: protocol.PollResponse{Workers: []protocol.DesiredWorker{
		{ID: "w-new", Template: "base", Size: "s", Generation: 2, JoinToken: &token},
	}}}
	m := &fakeMaterializer{observed: []ObservedWorker{{ID: "w-existing", Generation: 1}}}

	if err := testLoop(p, m).Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(m.gotDesired) != 1 || len(m.gotDesired[0]) != 1 || m.gotDesired[0][0].ID != "w-new" {
		t.Fatalf("desired = %v, want the poll's fleet passed through", m.gotDesired)
	}
	if len(m.gotObserved) != 1 || len(m.gotObserved[0]) != 1 || m.gotObserved[0][0].ID != "w-existing" {
		t.Fatalf("observed = %v, want what Observe reported", m.gotObserved)
	}
	if m.gotObserved[0][0].Generation != 1 {
		t.Fatalf("observed generation = %d, want it carried through for the drift check", m.gotObserved[0][0].Generation)
	}
}

// The controller reports NOTHING to the api: the poll is a pure read, and delivery
// is proved by the worker's own registration. The compile-time assertion is the
// tripwire — a Poll that took a controller assertion again would not satisfy it.
func TestPollTakesNoControllerAssertion(t *testing.T) {
	p := &fakePoller{}
	if err := testLoop(p, &fakeMaterializer{}).Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if p.calls != 1 {
		t.Fatalf("poll calls = %d, want 1", p.calls)
	}
	var _ func(context.Context) (protocol.PollResponse, error) = p.Poll
}

// A failed poll must not reconcile: an error carries no desired state, and an empty
// fleet would read as "delete every hosted worker".
func TestTickDoesNotReconcileWhenPollFails(t *testing.T) {
	boom := errors.New("api down")
	m := &fakeMaterializer{}
	if err := testLoop(&fakePoller{err: boom}, m).Tick(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the poll error", err)
	}
	if len(m.gotDesired) != 0 {
		t.Fatal("reconciled against a failed poll")
	}
}

// A failed observation must not reconcile either: acting on a view we just admitted
// we could not read is how a healthy worker gets clobbered.
func TestTickDoesNotReconcileWhenObserveFails(t *testing.T) {
	boom := errors.New("apiserver unreachable")
	m := &fakeMaterializer{observeErr: boom}
	if err := testLoop(&fakePoller{}, m).Tick(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the observe error", err)
	}
	if len(m.gotDesired) != 0 {
		t.Fatal("reconciled despite a failed observation")
	}
}

func TestTickPropagatesReconcileErrors(t *testing.T) {
	boom := errors.New("kube write failed")
	if err := testLoop(&fakePoller{}, &fakeMaterializer{reconcileErr: boom}).Tick(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the reconcile error", err)
	}
}

// signalMaterializer reports each Reconcile over a channel, so the test observes
// the loop goroutine's progress without racing on a field it writes.
type signalMaterializer struct{ reconciled chan struct{} }

func (signalMaterializer) Observe(context.Context) ([]ObservedWorker, error) { return nil, nil }

func (s signalMaterializer) Reconcile(context.Context, []protocol.DesiredWorker, []ObservedWorker) error {
	select {
	case s.reconciled <- struct{}{}:
	default: // never block the loop if the test has stopped listening
	}
	return nil
}

// Run reconciles immediately rather than sleeping out the first interval, so a
// restart converges in a round trip. (testLoop's interval is an hour: if the
// immediate cycle did not happen, no reconcile would arrive.)
func TestRunReconcilesImmediatelyAndStopsOnCancel(t *testing.T) {
	m := signalMaterializer{reconciled: make(chan struct{}, 1)}
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		testLoop(&fakePoller{}, m).Run(ctx)
		close(done)
	}()

	select {
	case <-m.reconciled:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("Run did not reconcile before its first tick elapsed")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on context cancellation")
	}
}

// ---------------------------------------------------------------------------
// Roll-health reporting (PRD #113 M3).
// ---------------------------------------------------------------------------

type fakeReporter struct {
	got  []protocol.StatusReport
	err  error
	when int // len(materializer calls) at the moment Report was invoked
	mat  *fakeMaterializer
}

func (f *fakeReporter) Report(_ context.Context, r protocol.StatusReport) error {
	f.got = append(f.got, r)
	if f.mat != nil {
		f.when = len(f.mat.gotDesired)
	}
	return f.err
}

func TestTickReportsRollHealthAfterReconciling(t *testing.T) {
	m := &fakeMaterializer{observed: []ObservedWorker{
		{
			ID: "w1",
			Roll: RollHealth{
				Phase:             protocol.PhaseStuck,
				PhaseSince:        time.Date(2026, 7, 26, 9, 46, 0, 0, time.UTC),
				PodPhase:          "Pending",
				TargetImage:       "harbor.example/uzi/agent-base:0.11.7",
				BlockingContainer: "seed-nix",
				BlockingReason:    "CrashLoopBackOff",
				RestartCount:      6,
			},
		},
	}}
	r := &fakeReporter{mat: m}
	l := New(&fakePoller{}, m, r, 10*time.Second, "0.11.7", slog.New(slog.NewTextHandler(io.Discard, nil)))
	l.now = func() time.Time { return time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC) }

	if err := l.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(r.got) != 1 {
		t.Fatalf("Report called %d times, want 1", len(r.got))
	}
	// LAST, not first: the report describes what Reconcile just did. Reporting before
	// the patch lands would describe the previous tick's world.
	if r.when != 1 {
		t.Errorf("Report ran before Reconcile (saw %d reconcile calls); roll health must describe the "+
			"world AFTER this tick's patch", r.when)
	}
	rep := r.got[0]
	if rep.WorkerImageTag != "0.11.7" {
		t.Errorf("WorkerImageTag = %q, want the tag this controller rolls to (Decision 9's hosted target)", rep.WorkerImageTag)
	}
	if rep.PollIntervalSeconds != 10 {
		t.Errorf("PollIntervalSeconds = %d, want 10 — the api derives its staleness window from the real cadence", rep.PollIntervalSeconds)
	}
	if len(rep.Workers) != 1 {
		t.Fatalf("reported %d workers, want 1", len(rep.Workers))
	}
	w := rep.Workers[0]
	if w.Phase != protocol.PhaseStuck || w.PodPhase != "Pending" {
		t.Errorf("phase/pod_phase = %q/%q, want stuck/Pending", w.Phase, w.PodPhase)
	}
	if w.BlockingContainer == nil || *w.BlockingContainer != "seed-nix" {
		t.Errorf("blocking_container = %v, want seed-nix", w.BlockingContainer)
	}
	if w.RestartCount != 6 {
		t.Errorf("restart_count = %d, want 6", w.RestartCount)
	}
	// Never terminated, so null rather than a fabricated clean exit.
	if w.LastExitCode != nil {
		t.Errorf("last_exit_code = %v, want null when the container never terminated", *w.LastExitCode)
	}
}

// A worker with no pod information contributes NO ROW. An absent row means "no
// signal" to the api, which is the truth; a row with a blank phase would be an
// assertion about a worker whose pod was never observed.
func TestTickOmitsWorkersWithNoRollSignal(t *testing.T) {
	m := &fakeMaterializer{observed: []ObservedWorker{
		{ID: "no-pod"},
		{ID: "has-pod", Roll: RollHealth{Phase: protocol.PhaseRolling}},
	}}
	r := &fakeReporter{}
	l := New(&fakePoller{}, m, r, time.Minute, "0.11.7", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := l.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(r.got[0].Workers) != 1 || r.got[0].Workers[0].ID != "has-pod" {
		t.Fatalf("reported %+v, want only the worker with a pod signal", r.got[0].Workers)
	}
	// PhaseSince is zero for that worker, and must serialize as null rather than the
	// epoch — a phase that began in 1970 would read as permanently stale.
	if r.got[0].Workers[0].PhaseSince != nil {
		t.Errorf("phase_since = %v, want null for a rolling worker with no dated pod", *r.got[0].Workers[0].PhaseSince)
	}
}

// A failed report must NOT fail the tick. Poll and Observe abort the cycle because
// reconciling against state we could not read clobbers healthy workers; nothing of
// the sort is true of a display-only report, and an observability feature that can
// stop the thing it observes is worse than no feature.
func TestTickSucceedsWhenReportingFails(t *testing.T) {
	m := &fakeMaterializer{observed: []ObservedWorker{{ID: "w1", Roll: RollHealth{Phase: protocol.PhaseSettled}}}}
	r := &fakeReporter{err: errors.New("api exploded")}
	var logged strings.Builder
	l := New(&fakePoller{}, m, r, time.Minute, "0.11.7", slog.New(slog.NewTextHandler(&logged, nil)))

	if err := l.Tick(context.Background()); err != nil {
		t.Fatalf("Tick returned %v; a display-only report failure must never abort reconciliation", err)
	}
	if len(m.gotDesired) != 1 {
		t.Errorf("Reconcile ran %d times, want 1: the report must not prevent reconciling", len(m.gotDesired))
	}
	if !strings.Contains(logged.String(), "reporting roll health failed") {
		t.Errorf("the failure was swallowed without a log line; got:\n%s", logged.String())
	}
}

// No reporter wired is a supported configuration, not a degraded one: it is what
// every pre-#113 caller and every other test in this file constructs.
func TestTickWithoutAReporterBehavesExactlyAsBefore(t *testing.T) {
	m := &fakeMaterializer{observed: []ObservedWorker{{ID: "w1", Roll: RollHealth{Phase: protocol.PhaseSettled}}}}
	if err := testLoop(&fakePoller{}, m).Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(m.gotDesired) != 1 {
		t.Errorf("Reconcile ran %d times, want 1", len(m.gotDesired))
	}
}
