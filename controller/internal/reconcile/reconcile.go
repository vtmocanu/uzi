// Package reconcile is the controller's loop: observe the cluster, tell the api
// what is durably there, fetch the hosted fleet's desired state, drive the cluster
// towards it.
//
// The loop is stateless by construction (PRD #58 Decision 2). It holds nothing
// across cycles and nothing across restarts: desired state comes from the api on
// every tick, observed state is read from the cluster on every tick, and the two
// are reconciled from scratch. A controller restart is therefore not an event —
// the next tick reconciles again from the same two sources, which is why neither
// side needs to remember what the other was last told.
package reconcile

import (
	"context"
	"log/slog"
	"time"

	"gitlab.example.com/vtmocanu/uzi/controller/internal/protocol"
)

// Poller fetches desired state and delivers acks. *apiclient.Client satisfies it;
// tests substitute a fake.
type Poller interface {
	Poll(ctx context.Context, materialized []string) (protocol.PollResponse, error)
}

// Materializer is the cluster side of the loop — the seam M3 implements with the
// kube client. M1 defines the contract and ships nothing that talks to a cluster.
//
// It is deliberately two methods rather than one. Observing is what the api's
// delivered-once handoff is settled on and must reflect the apiserver as it is
// *before* this cycle changes anything; reconciling is what changes it. Folding
// them together would make an ack a statement about a write this process just
// attempted, which is precisely the thing that must never happen.
type Materializer interface {
	// Observe returns the ids of hosted workers whose join-token Secret exists in
	// the cluster right now, read fresh from the apiserver.
	//
	// This slice is what the api destroys its only sealed copy of a token on, so it
	// must assert what the cluster durably holds and nothing else. An id reported
	// here whose Secret is absent strands that worker permanently: its token_hash
	// is committed, and no plaintext survives anywhere to authenticate against it.
	// When in doubt — a list error, a partial read — report fewer ids, never more.
	// Under-reporting costs one redundant re-delivery; over-reporting costs a
	// worker.
	Observe(ctx context.Context) (materialized []string, err error)

	// Reconcile drives the cluster towards `desired`: it creates, updates, rolls
	// and deletes the Secret / Deployment / PVC of each hosted worker, and flags
	// objects that answer to no desired worker as orphans (Decision 9).
	//
	// A worker whose JoinToken is nil has already been acked; its plaintext lives
	// only in the cluster Secret now, so reconcile it without one rather than
	// reading the nil as "needs a token".
	Reconcile(ctx context.Context, desired []protocol.DesiredWorker) error
}

// Loop is the reconcile loop.
type Loop struct {
	poller       Poller
	materializer Materializer
	interval     time.Duration
	log          *slog.Logger
}

// New constructs a Loop.
func New(p Poller, m Materializer, interval time.Duration, log *slog.Logger) *Loop {
	return &Loop{poller: p, materializer: m, interval: interval, log: log}
}

// Run ticks until ctx is cancelled. It reconciles once immediately, so a restart
// converges within an HTTP round trip rather than after a full interval.
//
// A failing cycle is logged and retried on the next tick, never fatal: the api
// being briefly unreachable is an outage of the reconcile loop, not of the workers
// already running, and exiting would convert a transient blip into a
// CrashLoopBackOff that fixes nothing.
func (l *Loop) Run(ctx context.Context) {
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()

	for {
		if err := l.Tick(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			l.log.Error("reconcile cycle failed; retrying next tick", "error", err, "interval", l.interval.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick is one cycle: observe, then ack + fetch desired state, then reconcile.
//
// The order is the safety property. Observation happens first and from the
// apiserver, so the acks this cycle sends describe the cluster as it stood before
// this cycle touched it. The acks ride the poll rather than a call of their own,
// which keeps the protocol to a single endpoint at no cost.
//
// An Observe failure aborts the cycle rather than polling with an empty ack list.
// Both are safe for the tokens — under-reporting only re-delivers — but polling on
// a failed observation would reconcile the cluster against a view we just admitted
// we could not read.
func (l *Loop) Tick(ctx context.Context) error {
	observed, err := l.materializer.Observe(ctx)
	if err != nil {
		return err
	}
	resp, err := l.poller.Poll(ctx, observed)
	if err != nil {
		return err
	}
	return l.materializer.Reconcile(ctx, resp.Workers)
}
