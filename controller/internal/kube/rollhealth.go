package kube

import (
	"time"

	corev1 "k8s.io/api/core/v1"

	"gitlab.example.com/vtmocanu/uzi/controller/internal/protocol"
	"gitlab.example.com/vtmocanu/uzi/controller/internal/reconcile"
)

// Roll-health derivation (PRD #113 M3, design §B-4). Stateless: every value comes
// from fields the pod itself carries, so a controller restart changes no answer.
//
// ============================================================================
// READ THIS BEFORE CHANGING ANYTHING HERE: `rolling` COMES FROM POD READINESS,
// NEVER FROM THE DRIFT PREDICATE.
// ============================================================================
//
// The tempting implementation is one line shorter and reads better:
//
//	if obs.SpecHash != wantHash { phase = rolling } else { phase = settled }
//
// It is wrong, and it is wrong in the confident direction. Reconcile patches a
// drifted Deployment, and the patch body IS the pod template carrying
// AnnotationSpecHash — so the moment the patch returns, the live Deployment's
// annotation equals wantHash and the next tick hits the early return in
// materializer.go's drift check. MEASURED: drift is true for exactly ONE tick, ten
// seconds at the default cadence. The drift-driven version therefore reports
// `settled` for every mid-roll worker from tick two onwards, the api's convergence
// grace expires against a version that has not moved, and the whole fleet reads
// `outdated` during a perfectly healthy roll. That is the cry-wolf failure the PRD
// exists to prevent, reintroduced through the back door.
//
// A single-tick test cannot catch it, because the drift-driven version is CORRECT on
// tick one. The test that separates them advances two ticks with the pod still not
// Ready — see rollhealth_test.go.
//
// The correct question is "does a pod matching what the deployment ASKED FOR exist
// and is it Ready?", which is why every branch below reads pod status and the only
// use of the deployment is supplying wantHash to match pods against.

const (
	// stuckRestartThreshold is when repeated restarts alone mean stuck, with no
	// blocking reason visible. Three strikes, matching the three missed beats that
	// mark a worker offline.
	//
	// MEASURED 2026-07-26 on 0.11.8, and this arm is LOAD-BEARING FOR THE MOTIVATING
	// INCIDENT — not only for the exotic case this comment used to describe. It said
	// the arm "exists for a container that restarts repeatedly while flickering
	// through Running". That case is real and was reproduced, but a fast-failing
	// seed-nix ALTERNATES between waiting:CrashLoopBackOff and terminated:Error, and
	// `Error` is NOT in blockingReasons — measured steady-state split 71% / 29%. On
	// every Error tick the reason arm is silent and only restartCount >= 3 holds the
	// `stuck` verdict.
	//
	// So raising or removing this threshold makes the badge for PRD #113's own
	// motivating incident flicker indefinitely. Do not treat it as covering only a
	// rare shape.
	stuckRestartThreshold = 3
	// stuckAge is the fallback arm ONLY, for the reasonless cases: Pending with
	// FailedScheduling or FailedMount, where no container exists yet to carry a
	// waiting reason at all. MEASURED 2026-07-26 at 2s resolution on 0.11.8: the real
	// incident (seed-nix CrashLoopBackOff) first reports `stuck` at **37 seconds** from
	// pod creation, via the reason arm — this used to say "within a couple of minutes",
	// which was wrong by ~3x, though in the safe direction. The conclusion is unchanged
	// and now measured rather than assumed: the reason arm is fast, so the age arm can
	// afford to be generous. (This used to say k8s reports
	// CrashLoopBackOff "from the second restart"; per kubelet's doBackOff the backoff
	// starts on the FIRST termination. Harmless downstream — the arm fires sooner, not
	// later — but it was a false mechanism in a comment.) This governs only what that
	// arm cannot see, so it is
	// deliberately generous — a slow image pull of the browser-inflated agent image
	// must not read as stuck.
	stuckAge = 10 * time.Minute
)

// blockingReasons are the container waiting reasons that mean stuck immediately,
// with no age or restart threshold. Each is a state k8s does not exit on its own:
// waiting longer cannot fix a missing image or an unparseable config.
var blockingReasons = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"InvalidImageName":           true,
}

// deriveRollHealth folds a worker's pods into one RollHealth, given wantHash — the
// spec hash the worker's Deployment currently asks for.
//
// pods are this worker's pods only (label-selected by the caller). now is injected
// rather than read from the clock so the age arm is testable without sleeping.
func deriveRollHealth(pods []corev1.Pod, wantHash string, now time.Time) reconcile.RollHealth {
	// No pod at all is the Recreate gap: the old pod is terminating or gone and the
	// new one has not been created. Rolling, with no phase_since — there is nothing
	// to date it from, and inventing now() would restart the clock every tick.
	if len(pods) == 0 {
		return reconcile.RollHealth{Phase: protocol.PhaseRolling}
	}

	// Prefer a pod matching what the deployment asks for. A pod carrying a different
	// hash is the OLD one still terminating, which says nothing about the new one.
	var current *corev1.Pod
	for i := range pods {
		if pods[i].Annotations[AnnotationSpecHash] == wantHash {
			current = &pods[i]
			break
		}
	}
	if current == nil {
		// Only stale pods: the new one is not up yet. Same state as the gap.
		return reconcile.RollHealth{Phase: protocol.PhaseRolling}
	}

	health := reconcile.RollHealth{PodPhase: string(current.Status.Phase)}

	if ready, at := readyCondition(current); ready {
		health.Phase = protocol.PhaseSettled
		health.PhaseSince = at
		return health
	}

	// Not Ready. Find the container to blame, init containers first — that is where the
	// motivating incident wedged (seed-nix reseeding the browser's nix closure), and an
	// init container blocks everything behind it while an app container may not.
	//
	// THE FALLBACK IS NOT DEFENSIVE, it is the only way the restart arm below can ever
	// fire. blockingContainer returns nil unless a container is Waiting or
	// Terminated-nonzero, so for a container that is RUNNING right now this used to
	// leave RestartCount at 0 and the arm evaluated `0 >= 3` forever. That is exactly
	// the shape the arm exists for: k8s resets the restart backoff after ~10 minutes of
	// running, so a container that runs a minute, dies, and repeats — with a failing
	// readiness probe — is observed Running most of the time.
	//
	// The blanked DIAGNOSTIC was the worse half: a flapping worker reported 0 restarts
	// and no container name, so it looked pristine to whoever was trying to fix it.
	subject := blockingContainer(current)
	if subject == nil {
		subject = mostRestartedContainer(current)
	}
	if subject != nil {
		health.BlockingContainer = subject.Name
		health.RestartCount = subject.RestartCount
		if subject.State.Waiting != nil {
			health.BlockingReason = subject.State.Waiting.Reason
		} else if subject.State.Terminated != nil {
			health.BlockingReason = subject.State.Terminated.Reason
		}
		if term := lastTermination(subject); term != nil {
			code := term.ExitCode
			health.LastExitCode = &code
		}
	}

	switch {
	case blockingReasons[health.BlockingReason]:
		health.Phase = protocol.PhaseStuck
	case health.RestartCount >= stuckRestartThreshold:
		health.Phase = protocol.PhaseStuck
	case !current.CreationTimestamp.IsZero() && now.Sub(current.CreationTimestamp.Time) > stuckAge:
		// The reasonless arm: Pending with FailedScheduling or FailedMount, where no
		// container status exists to carry a reason.
		//
		// CreationTimestamp, not Status.StartTime, and the difference is load-bearing here
		// rather than cosmetic. StartTime is when the KUBELET acknowledged the pod, so it is
		// nil for a pod that never bound to a node — which is precisely the FailedScheduling
		// case this arm exists to catch. CreationTimestamp is set by the api server at
		// admission and is always present.
		//
		// MEASURED, not reasoned: swapping this arm to `current.Status.StartTime != nil &&
		// now.Sub(current.Status.StartTime.Time) > stuckAge` turns the reasonless case into
		// `Phase = "rolling"` where "stuck" is wanted, so an unschedulable worker would
		// report rolling forever. The mutation is caught — rollhealth_test.go's "a
		// reasonless pod past stuckAge is stuck" fails at once, because its pod fixture
		// sets CreationTimestamp and no StartTime, exactly like the pods this arm sees.
		// (`kubectl describe` prints its "Start Time" line from Status.StartTime, so that
		// line is absent for these pods too — noted because docs/worker-upgrades.md sends
		// an operator to read it.)
		health.Phase = protocol.PhaseStuck
	default:
		health.Phase = protocol.PhaseRolling
	}
	// Both non-settled phases REACHED HERE date from the same place, and it is NOT the
	// moment the phase began — see the PhaseSince note on reconcile.RollHealth. The two
	// early returns above are the exception and stay that way: `rolling` with no current
	// pod carries NO timestamp, because there is nothing to date it from. A stateless
	// controller has no memory of the transition, so the pod's creation is the only
	// timestamp available. It is EARLIER than the event the field name implies, so any
	// consumer computing `now - PhaseSince` overestimates the duration and any threshold
	// keyed on it fires sooner than intended. Safe for catching a stuck worker,
	// UNSAFE for anything that must not cry wolf.
	health.PhaseSince = current.CreationTimestamp.Time
	return health
}

// readyCondition reports whether the pod's Ready condition is True, and when it last
// transitioned — the honest answer to "since when has this been settled?", which the
// pod records and the controller must not invent.
func readyCondition(p *corev1.Pod) (bool, time.Time) {
	for i := range p.Status.Conditions {
		c := &p.Status.Conditions[i]
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue, c.LastTransitionTime.Time
		}
	}
	return false, time.Time{}
}

// blockingContainer returns the first container holding the pod back: one that is
// Waiting, else one Terminated with a non-zero exit code. Init containers are
// searched before app containers because an init container blocks everything behind
// it, so it is the honest thing to name.
//
// A nil result means nothing identifiable is blocking — a pod Pending before any
// container status exists (FailedScheduling), which the age arm covers instead.
func blockingContainer(p *corev1.Pod) *corev1.ContainerStatus {
	for _, set := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
		for i := range set {
			if set[i].State.Waiting != nil {
				return &set[i]
			}
		}
		for i := range set {
			if t := set[i].State.Terminated; t != nil && t.ExitCode != 0 {
				return &set[i]
			}
		}
	}
	return nil
}

// mostRestartedContainer returns the container with the highest restart count, or nil
// when nothing has restarted. It is what makes a FLAPPING container visible: such a
// container is often observed Running, so blockingContainer cannot see it.
//
// Requires a non-zero count, so a pod that is simply not Ready yet gets no container
// blamed — naming a container with 0 restarts would point an operator at innocent code.
func mostRestartedContainer(p *corev1.Pod) *corev1.ContainerStatus {
	var worst *corev1.ContainerStatus
	for _, set := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
		for i := range set {
			if set[i].RestartCount == 0 {
				continue
			}
			if worst == nil || set[i].RestartCount > worst.RestartCount {
				worst = &set[i]
			}
		}
	}
	return worst
}

// lastTermination returns the container's most recent termination, preferring the
// current state over the remembered one. CrashLoopBackOff carries the exit code in
// LastTerminationState (the container is Waiting NOW, and terminated before), which
// is exactly the incident this feature was built for.
func lastTermination(c *corev1.ContainerStatus) *corev1.ContainerStateTerminated {
	if c.State.Terminated != nil {
		return c.State.Terminated
	}
	return c.LastTerminationState.Terminated
}
