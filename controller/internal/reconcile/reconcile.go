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
// The loop DOES tell the api things now — display-only roll-health (PRD #113,
// swallowed on failure so an observability channel can never take down what it
// observes) and cordon control-writes (PRD #422 M4, fail-safe: a failed cordon
// DEFERS the roll rather than proceeding). But it is still never in the trust path
// for DESTROYING a token: token delivery is settled api-side by the worker's own
// registration (proof of possession), which is the property the PRD's RBAC exists
// to bound and which neither of those writes touches.
package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
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
// A worker id enters the observed set if ANY object we stamped carries it — which
// is what lets a Deployment-less leftover PVC still be torn down.
//
// Orphans (uzi-hw-*-named but NOT stamped by us) deliberately never cross this
// interface. They are logged inside Observe and never acted on, so giving Reconcile
// the power to see them is power it should not have.
type ObservedWorker struct {
	// ID is the hosted worker's uuid, read from the uzi.dev/hosted-worker-id LABEL —
	// never parsed back out of the object's name.
	ID string
	// HasDeployment distinguishes "nothing is deployed" from "deployed at generation
	// 0", which are different states that a bare Generation field cannot tell apart.
	HasDeployment bool
	// Generation is the hosted_generation the deployed pod template was rendered from
	// (the uzi.dev/hosted-generation annotation). Comparing it against
	// DesiredWorker.Generation is half of Decision 9's drift check. 0 when
	// HasDeployment is false.
	Generation int64
	// SpecHash is the uzi.dev/spec-hash annotation: the other half of the drift check,
	// and the one that catches a new agent image tag (which the controller resolves
	// from its own config, so the generation never moves for it). "" when
	// HasDeployment is false.
	SpecHash string
	// HasDataPVC / HasNixPVC / HasDinDDataPVC let a partially-created worker (a
	// crashed reconcile between calls) converge without a read of anything sensitive.
	//
	// HasDinDDataPVC is the DinD daemon's data root (issue #224 M-a), rendered for
	// DOCKER workers only — so it stays false forever for a plain worker, which is
	// correct rather than a gap: RenderPVCs emits no such claim for one, so there is
	// nothing for the create-gate to skip and nothing for teardown to find.
	HasDataPVC     bool
	HasNixPVC      bool
	HasDinDDataPVC bool
	// Namespace is the namespace the object(s) for this worker were OBSERVED in
	// (PRD #83 M3). With two worker namespaces — the restricted default and the
	// privileged docker tier — teardown of a worker the api no longer wants must
	// target the namespace its objects actually live in, and that is knowable only
	// from where they were seen (a dropped worker's Docker flag is no longer in the
	// desired set). Empty falls back to the restricted default, which is what a
	// single-namespace (pre-#83) observation carried implicitly.
	Namespace string
	// Roll is this worker's upgrade health, derived from its POD (PRD #113 M3).
	// Display-only: nothing in Reconcile reads it, and it exists purely to be
	// reported to the api for the Workers UI. Zero value means "no pod information",
	// which RollHealth.Phase renders as the rolling gap.
	Roll RollHealth
}

// RollHealth is one worker's roll state as read from its pod, and it is the answer
// to a question the Deployment cannot answer (PRD #113 M3).
//
// The distinction is the whole of this type: a Deployment's pod-template annotations
// record what was ASKED FOR, and they equal the desired hash the instant the patch
// returns — measured at materializer.go's drift check, which early-returns on the
// very next tick. So the deployment says "settled" ten seconds into a roll whose pod
// is still Pending. Only the POD says what is actually running, which is why every
// field here comes off pod status rather than deployment spec.
//
// Derived fresh each tick from pod fields alone. The controller is stateless by
// construction (see the package doc), so "has this been stuck for a while?" is
// answered from timestamps the pod itself carries — CreationTimestamp, the Ready
// condition's LastTransitionTime, restartCount — never from anything remembered
// across ticks.
type RollHealth struct {
	// Phase is one of PhaseRolling, PhaseStuck, PhaseSettled. Empty when no pod was
	// observed for the worker at all.
	Phase string
	// PhaseSince is when the current phase began, as far as the pod can say: the Ready
	// condition's transition time when settled, the pod's creation time when rolling
	// with a pod. Zero during the Recreate gap, where there is no pod to ask.
	PhaseSince time.Time
	// PodPhase is the raw k8s pod phase (Pending/Running/...), passed through for
	// display. Not a substitute for Phase: a pod can be Running and still stuck.
	PodPhase string
	// TargetImage is the full image reference this worker's Deployment currently
	// asks for, e.g. harbor.example.com/uzi/agent-jvm:0.11.7. Read from the
	// deployment's pod template rather than resolved again from config, so it is
	// what the cluster was actually told to run. The UI renders the image name, so a
	// bare tag would not be enough.
	TargetImage string
	// BlockingContainer / BlockingReason name the container holding the pod back and
	// why, e.g. "seed-nix" / "CrashLoopBackOff" — the human half of the failed-worker
	// strip. Init containers are searched first, since that is where the motivating
	// incident wedged. Empty when nothing is blocking.
	BlockingContainer string
	BlockingReason    string
	// RestartCount is the blocking container's restart count, and LastExitCode its
	// last termination exit code when it has one. LastExitCode is a pointer because 0
	// is a meaningful exit code and "never terminated" must not render as a clean
	// exit.
	RestartCount int32
	LastExitCode *int32
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
	// fresh from the apiserver via Deployments and PVCs (list on both, granted).
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

// Reporter sends per-worker roll health to the api (PRD #113 M3).
//
// Separate from Poller even though *apiclient.Client satisfies both, because the two
// directions have opposite trust properties and opposite failure handling: Poll is a
// pure read whose failure aborts the cycle, Report is this controller's only
// assertion and its failure is swallowed. One interface would invite treating them
// the same way.
//
// Optional: a nil Reporter simply reports nothing.
type Reporter interface {
	Report(ctx context.Context, report protocol.StatusReport) error
}

// Loop is the reconcile loop.
type Loop struct {
	poller       Poller
	materializer Materializer
	reporter     Reporter
	interval     time.Duration
	log          *slog.Logger
	// workerImageTag is the tag this controller rolls workers to, reported so the api
	// can use it as the hosted fleet's upgrade target (Decision 9) instead of its own
	// release — values.yaml allows pinning the two independently.
	workerImageTag string
	// now is the clock seam for the report's timestamp; tests pin it.
	now func() time.Time
}

// New constructs a Loop. reporter may be nil, which disables roll-health reporting
// and leaves the loop behaving exactly as it did before PRD #113.
func New(p Poller, m Materializer, r Reporter, interval time.Duration, workerImageTag string, log *slog.Logger) *Loop {
	return &Loop{
		poller:         p,
		materializer:   m,
		reporter:       r,
		interval:       interval,
		workerImageTag: workerImageTag,
		log:            log,
		now:            time.Now,
	}
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
	if err := l.materializer.Reconcile(ctx, resp.Workers, observed); err != nil {
		return err
	}

	// Roll health goes LAST, and its error is logged-and-swallowed rather than
	// returned (PRD #113 M3). Both properties are deliberate.
	//
	// Last, because it reports what reconcile just did — reporting before the patch
	// lands would describe the previous tick's world.
	//
	// Swallowed, because it is DISPLAY-ONLY. Poll and Observe abort the cycle for
	// reasons stated above: without them we would reconcile against state we admitted
	// we could not read. Nothing of the sort is true here. A controller that stopped
	// reconciling workers because a status POST failed would be strictly worse than
	// the one we have today, which does not report at all — an observability feature
	// must never be able to take down the thing it observes.
	l.report(ctx, observed)
	return nil
}

// report sends per-worker roll health, if a reporter is wired.
//
// A nil reporter is a supported configuration, not a degraded one: it is what every
// existing test constructs, and it keeps this milestone's addition strictly additive
// to a loop whose failure modes are already understood.
func (l *Loop) report(ctx context.Context, observed []ObservedWorker) {
	if l.reporter == nil {
		return
	}
	workers := make([]protocol.WorkerStatus, 0, len(observed))
	for _, o := range observed {
		// A worker with no pod information contributes no row. Reporting a blank phase
		// would be an assertion about a worker we did not observe a pod for, and the
		// api treats an absent row as "no signal" — which is the truth here.
		if o.Roll.Phase == "" {
			continue
		}
		workers = append(workers, protocol.WorkerStatus{
			ID:                o.ID,
			Phase:             o.Roll.Phase,
			PhaseSince:        timePtr(o.Roll.PhaseSince),
			TargetImage:       o.Roll.TargetImage,
			PodPhase:          o.Roll.PodPhase,
			BlockingContainer: strPtr(o.Roll.BlockingContainer),
			BlockingReason:    strPtr(o.Roll.BlockingReason),
			RestartCount:      o.Roll.RestartCount,
			LastExitCode:      o.Roll.LastExitCode,
		})
	}
	report := protocol.StatusReport{
		ReportedAt:          l.now(),
		PollIntervalSeconds: int(l.interval.Seconds()),
		WorkerImageTag:      l.workerImageTag,
		Workers:             workers,
	}
	if err := l.reporter.Report(ctx, report); err != nil {
		l.log.Warn("reporting roll health failed; reconciliation is unaffected", "error", err, "workers", len(workers))
	}
}

// timePtr / strPtr render an empty value as JSON null rather than a zero. The
// distinction is load-bearing on this wire: "" and null mean different things to the
// api ("no blocking container" versus "a container with an empty name"), and a zero
// time would claim a phase started at the epoch.
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
