package reconcile

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

func testLoop(p Poller, m Materializer) *Loop {
	return New(p, m, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
