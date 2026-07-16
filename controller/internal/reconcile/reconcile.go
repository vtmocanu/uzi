// Package reconcile is the controller's loop: fetch the hosted fleet's desired
// state from the api, observe what the cluster actually has, drive one towards the
// other.
//
// The loop is stateless by construction (PRD #58 Decision 2). It holds nothing
// across cycles and nothing across restarts: desired state comes from the api on
// every tick, observed state is read from the cluster on every tick, and the two
// are reconciled from scratch. A controller restart is therefore not an event —
// the next tick reconciles again from the same two sources, which is why neither
// side needs to remember what the other was last told.
//
// It tells the api nothing. Token delivery is settled api-side by the worker's own
// registration (proof of possession), so this component is never in the trust path
// for destroying a token — which matters, because it is the component the PRD's
// RBAC exists to bound.
package reconcile

import (
	"context"
	"log/slog"
	"time"

	"gitlab.example.com/vtmocanu/uzi/controller/internal/protocol"
)

// Poller fetches desired state. *apiclient.Client satisfies it; tests substitute a
// fake.
type Poller interface {
	Poll(ctx context.Context) (protocol.PollResponse, error)
}

// ObservedWorker is one hosted worker's state as it currently exists in the
// cluster, read from its DEPLOYMENT — never from its Secret.
//
// That restriction is Decision 1's RBAC, and it is not negotiable: the controller
// holds create/delete on Secrets and nothing more, because k8s has no
// existence-only verb and a `get`/`list` would let a compromised controller
// harvest every hosted worker's join token for the fleet's life. A Deployment
// references its Secret by name without embedding it, which is exactly why reading
// Deployments is boring and reading Secrets is fleet-wide token disclosure.
type ObservedWorker struct {
	// ID is the hosted worker's uuid, recovered from the object's name/labels.
	ID string
	// Generation is the hosted_generation the deployed objects were rendered from.
	// M3 sources it from the pod-template annotation it stamps (which is also what
	// makes a rotation actually roll the pod); comparing it against
	// DesiredWorker.Generation is Decision 9's drift check.
	Generation int64
}

// Materializer is the cluster side of the loop — the seam M3 implements with the
// kube client. M1 defines the contract and ships nothing that talks to a cluster.
//
// Two methods rather than one because a reconciler should read the world before it
// changes it, and because Decision 9's drift check needs observed and desired side
// by side. Note that nothing observed here is reported to the api any more: the
// api settles token delivery from the worker's own registration, so this side is
// purely "drive the cluster towards desired state".
type Materializer interface {
	// Observe returns the hosted workers currently deployed in the cluster, read
	// fresh from the apiserver via Deployments (get/list, both granted).
	//
	// It must NOT read Secrets — see ObservedWorker. An absent worker here means
	// "nothing is deployed for it", never "it has no token".
	Observe(ctx context.Context) ([]ObservedWorker, error)

	// Reconcile drives the cluster towards `desired`, given what `observed` shows:
	// it creates, updates, rolls and deletes the Secret / Deployment / PVC of each
	// hosted worker, and flags objects answering to no desired worker as orphans
	// (Decision 9).
	//
	// A worker whose JoinToken is nil needs no Secret written: either a pod already
	// proved it holds one, or the api's buffer expired and recovery is a rotation.
	// Never read the nil as "needs a token", and never clear an existing Secret on
	// account of it.
	Reconcile(ctx context.Context, desired []protocol.DesiredWorker, observed []ObservedWorker) error
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

// Tick is one cycle: fetch desired state, observe the cluster, reconcile the two.
//
// Either failure aborts the cycle before Reconcile. A failed poll carries no
// desired state, and an empty fleet would read as "delete every hosted worker"; a
// failed observation means we cannot tell what is already there, and reconciling
// against a view we just admitted we could not read is how a healthy worker gets
// clobbered. Retrying next tick costs one interval.
func (l *Loop) Tick(ctx context.Context) error {
	resp, err := l.poller.Poll(ctx)
	if err != nil {
		return err
	}
	observed, err := l.materializer.Observe(ctx)
	if err != nil {
		return err
	}
	return l.materializer.Reconcile(ctx, resp.Workers, observed)
}
