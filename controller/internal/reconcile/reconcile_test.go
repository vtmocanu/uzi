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
	resp protocol.PollResponse
	err  error
	// gotAcks records the ack list of each Poll call.
	gotAcks [][]string
}

func (f *fakePoller) Poll(_ context.Context, materialized []string) (protocol.PollResponse, error) {
	f.gotAcks = append(f.gotAcks, materialized)
	return f.resp, f.err
}

type fakeMaterializer struct {
	observed     []string
	observeErr   error
	reconcileErr error
	// gotDesired records the desired state of each Reconcile call.
	gotDesired [][]protocol.DesiredWorker
}

func (f *fakeMaterializer) Observe(context.Context) ([]string, error) {
	return f.observed, f.observeErr
}

func (f *fakeMaterializer) Reconcile(_ context.Context, desired []protocol.DesiredWorker) error {
	f.gotDesired = append(f.gotDesired, desired)
	return f.reconcileErr
}

func testLoop(p Poller, m Materializer) *Loop {
	return New(p, m, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// The cycle's ordering is the handoff's safety property: what the controller
// OBSERVES in the cluster is what it acks, and it acks before it acts.
func TestTickAcksWhatItObservedThenReconcilesDesiredState(t *testing.T) {
	token := "uzw_pending"
	p := &fakePoller{resp: protocol.PollResponse{Workers: []protocol.DesiredWorker{
		{ID: "w-new", Template: "base", Size: "s", Generation: 1, JoinToken: &token},
	}}}
	m := &fakeMaterializer{observed: []string{"w-existing"}}

	if err := testLoop(p, m).Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(p.gotAcks) != 1 || len(p.gotAcks[0]) != 1 || p.gotAcks[0][0] != "w-existing" {
		t.Fatalf("acks = %v, want exactly what Observe reported", p.gotAcks)
	}
	if len(m.gotDesired) != 1 || len(m.gotDesired[0]) != 1 || m.gotDesired[0][0].ID != "w-new" {
		t.Fatalf("desired = %v, want the poll's fleet passed through", m.gotDesired)
	}
}

// A failed observation must abort the cycle without polling: the acks would be
// safe (an empty list only re-delivers), but reconciling the cluster against a
// view we just failed to read is not.
func TestTickDoesNotPollWhenObserveFails(t *testing.T) {
	boom := errors.New("apiserver unreachable")
	p := &fakePoller{}
	m := &fakeMaterializer{observeErr: boom}

	if err := testLoop(p, m).Tick(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the observe error", err)
	}
	if len(p.gotAcks) != 0 {
		t.Fatal("polled despite a failed observation")
	}
}

// A failed poll must not reconcile: an error carries no desired state, and an
// empty fleet would read as "delete every hosted worker".
func TestTickDoesNotReconcileWhenPollFails(t *testing.T) {
	boom := errors.New("api down")
	p := &fakePoller{err: boom}
	m := &fakeMaterializer{}

	if err := testLoop(p, m).Tick(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the poll error", err)
	}
	if len(m.gotDesired) != 0 {
		t.Fatal("reconciled against a failed poll")
	}
}

func TestTickPropagatesReconcileErrors(t *testing.T) {
	boom := errors.New("kube write failed")
	if err := testLoop(&fakePoller{}, &fakeMaterializer{reconcileErr: boom}).Tick(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the reconcile error", err)
	}
}

// A restart observes nothing yet, so it acks nothing — the api re-delivers every
// pending token. That is the at-least-once handoff working as designed, and it is
// why a controller crash cannot strand a worker.
func TestTickAfterRestartAcksNothing(t *testing.T) {
	p := &fakePoller{}
	m := &fakeMaterializer{observed: nil}

	if err := testLoop(p, m).Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(p.gotAcks) != 1 || len(p.gotAcks[0]) != 0 {
		t.Fatalf("acks = %v, want none on a cold start", p.gotAcks)
	}
}

// signalMaterializer reports each Reconcile over a channel, so the test observes
// the loop goroutine's progress without racing on a field it writes.
type signalMaterializer struct{ reconciled chan struct{} }

func (signalMaterializer) Observe(context.Context) ([]string, error) { return nil, nil }

func (s signalMaterializer) Reconcile(context.Context, []protocol.DesiredWorker) error {
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
