package kube

import (
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
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
//
// ============================================================================
// AND `settled` IS NOT READINESS ALONE (issue #145). READ THIS BEFORE RESTORING
// THE UNGUARDED EARLY RETURN — IT LOOKS LIKE A SIMPLIFICATION AND IS THE BUG.
// ============================================================================
//
// The worker container has NO readiness probe — MEASURED on the live Deployment,
// `readinessProbe: null`; the only probes anywhere under controller/ are two dind
// StartupProbes (one on the `dind` sidecar, one on the `dind-init` init container). So
// `Ready` means the kubelet started the process, not that the agent works.
//
// A container that starts, dies ~40s later and repeats is Ready on a SUBSTANTIAL
// MINORITY of ticks — MEASURED at 41 of 133 samples (31%), and it DECAYS as kubelet's
// backoff grows: 100% Ready at 0 restarts, 78% at 1, 58% at 2, 44% at 3, 30% at 4, 19%
// at 5, 10% at 6. (An earlier version of this banner said "Ready for most of every
// cycle", which is true only for the first two cycles and false in aggregate.) 31% is
// still ample: the unguarded early return reported `settled` on every one of those
// ticks, and a badge only has to be wrong sometimes to be untrustworthy. Two symptoms
// from that one line: the badge alternated settled/stuck every ~40s, and — since a
// `settled` report returns before the blocking-container lookup below — the report
// carried zeroed diagnostics that overwrote the real ones, so a worker with 5 restarts
// and exit 1 persisted as pristine.
//
// THE SIGNAL WAS NEVER MISSING; THIS DERIVATION DISCARDED IT. The pod itself carries
// `restartCount` and the current instance's start time, which together say "this thing
// keeps dying" — see flappingContainer.
//
// THE OBVIOUS ALTERNATIVE WAS MEASURED AND DOES NOT WORK. The rule first proposed for
// this was "pod Ready + the worker's own heartbeat says offline, after the register
// convergence grace". Measured over 123 samples of a real crash-looping pod it fired
// ZERO times, because the two signals are anti-correlated: Ready means the container is
// Running, the container dies with the agent, so the agent is alive and beat within its
// 15s interval — it cannot be offline while Ready. (Second, independent reason:
// PhaseSince is the Ready condition's LastTransitionTime, which RESETS on every restart,
// so "after the grace elapsed" is never true for a flapping pod. That one holds
// unconditionally.)
//
// ⚠ THE ANTI-CORRELATION IS NOT UNCONDITIONAL, and an earlier version of this banner
// said "by construction" without saying so. The measurement's heartbeat half is a MODEL
// — the sampled pod was a busybox flapper, not the agent — and it depends on the worker
// registering fast enough to beat the 45s stale threshold. Measured at 1.9s, bounded at
// ~32s by `UZI_DOCKER_READY_TIMEOUT` (default 30s), which is ENV-TUNABLE AND PINNED BY
// NOTHING. Raise it above 45s and the anti-correlation weakens and this advice expires.
// So: do not reach for the heartbeat here — but if you are here because that timeout
// moved, the reasoning above is what to re-derive. It also does not belong in this
// module regardless, which reads pods and must not read api state.

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
	// flapWindow is how long a container's CURRENT instance must have been up before it
	// stops counting as flapping (issue #145). It is kubelet's own backoff-forget
	// interval: stay up this long and the CrashLoopBackOff is forgotten, which is the
	// same fact the blockingContainer-fallback comment below and rollhealth_test.go's
	// `running()` helper already assert from the other side. (NOT stuckAge's comment,
	// which an earlier version of this line cited — that one covers the reason arm's
	// 37s timing and when doBackOff starts, and never mentions the reset.)
	//
	// EQUAL TO stuckAge BY COINCIDENCE OF VALUE, NOT OF MEANING, which is why it is its
	// own constant rather than a reuse. stuckAge bounds how long a REASONLESS pod may sit
	// before it is called stuck; this bounds how recently a container must have restarted
	// for a live restart loop to still count as one. Changing one must not silently
	// change the other.
	//
	// It is also deliberately NOT part of the ControllerStuckAge relationship: that pair
	// exists because MaxUpgradingWindow gates R2 and must outlast the controller's
	// `rolling`, so the api mirrors stuckAge in its own constant and
	// TestMaxUpgradingWindowExceedsControllerStuckAge asserts a clamp between them. This
	// constant gates a `stuck` verdict, which feeds R1 — a different rule with no window
	// to order against. NOTHING api-side mirrors flapWindow, and nothing should.
	//
	// The absence of that coupling is a CONSEQUENCE OF #151, not a property of this
	// constant: if R1 were ever ceiling-gated again, flapWindow would join the
	// relationship and would need the same kind of clamp.
	//
	// 🔴 READ THIS BEFORE SHORTENING IT. Because #151 removed the INV-5 ceiling from R1,
	// an `upgrade_failed` produced by this arm has NO expiry of its own — flapWindow
	// elapsing on continuous uptime is the SOLE thing that ends it. Every other stuck
	// arm's alert clears when the pod state changes; this one clears only when the
	// container has been up this long. Shorten it and the alert clears sooner on a
	// worker that is still dying; lengthen it and a recovered worker stays red longer.
	// Neither is obviously wrong, but the choice is the alert's entire lifetime.
	flapWindow = 10 * time.Minute
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
// spec hash the worker's Deployment currently asks for — and replicaFailure, the reason
// on that Deployment's ReplicaFailure condition when it is True (empty otherwise).
//
// pods are this worker's pods only (label-selected by the caller). now is injected
// rather than read from the clock so the age arm is testable without sleeping.
func deriveRollHealth(pods []corev1.Pod, wantHash, replicaFailure string, now time.Time) reconcile.RollHealth {
	if len(pods) == 0 {
		// A POD-LESS WORKER HAS TWO CAUSES AND THEY NEED OPPOSITE ANSWERS (issue #148).
		//
		// The ordinary one is the Recreate gap: the old pod is terminating or gone and
		// the new one has not been created yet. MEASURED at ~1.4s on a healthy roll, and
		// it happens on EVERY release, so reporting `stuck` for pod-lessness as such
		// would turn the whole fleet's badge red every time uzi ships — the cry-wolf
		// failure PRD #113 exists to prevent, arriving through this branch.
		//
		// The other is a Deployment that can never produce a pod at all: no
		// ServiceAccount, an exceeded quota, an admission rejection. Before this branch
		// existed it inherited the gap's answer and was therefore NEVER alerted on — not
		// at MaxUpgradingWindow, not ever. `rolling` becomes api R2 `upgrading`, which
		// Decision 1 deliberately excludes from the attention set; the three stuck arms
		// below all need a pod and this branch returns before reaching them; and past the
		// INV-5 ceiling the row falls back to version compare, where a worker that never
		// registered reports no version and classifies `unknown` — also not an alert.
		// MEASURED on 0.11.8, worker d26fb0f9: `upgrading` for 45 minutes, then `unknown`
		// forever, both silent, while the cause sat on the Deployment the whole time.
		//
		// THE DISCRIMINATOR IS THE CONDITION, NOT THE POD-LESSNESS. ReplicaFailure=True is
		// the Deployment controller's assertion that a pod create FAILED — a different
		// claim from "no pod exists yet", which is all the gap can say. A healthy Recreate
		// gap carries no such condition, so the two cases separate cleanly and the
		// anti-cry-wolf property survives.
		//
		// Fires on the FIRST observation, with no age or restart threshold, matching how
		// blockingReasons is treated below. That admits one known false positive: a
		// create failure that resolves on its own (a quota freed, a ServiceAccount created
		// a moment later) badges `upgrade_failed` for the tick or two before the condition
		// clears. Accepted deliberately — a brief loud tick is the recoverable direction,
		// and permanent silence is the bug being fixed here.
		//
		// Only the REASON is carried, never the condition's `message`. That is the same
		// line the pod path holds (see likelyCause's comment in the web): a k8s message is
		// free text carrying namespaces, object names and paths. It would also not survive
		// the trip, and this is MEASURED rather than argued — the live message on #148's own
		// worker was 185 bytes against the api's 64-byte cap on controller-supplied display
		// strings, and the truncation lands before the cause is reached: `pods
		// "uzi-hw-d26fb0f9-…-65965d65fc-" i`, with "serviceaccount … not found" gone. See
		// rollhealth_test.go, which carries the string verbatim as a fixture.
		//
		// MEASURED the same day: reason is `FailedCreate` (the replicaset controller's, copied
		// onto the Deployment by the deployment controller), and a HEALTHY worker's Deployment
		// carries no ReplicaFailure condition at all. Nothing here hardcodes `FailedCreate`
		// though — whatever reason the condition carries is what travels.
		if replicaFailure != "" {
			// No PhaseSince, for the same reason the gap has none: there is no pod to date
			// this from. The condition's own LastTransitionTime is deliberately not used —
			// RollHealth.PhaseSince is documented as a POD timestamp on every other path,
			// and quietly making it sometimes a Deployment timestamp would break the one
			// api rule that reads it.
			return reconcile.RollHealth{Phase: protocol.PhaseStuck, BlockingReason: replicaFailure}
		}
		// The gap. Rolling, with no phase_since — there is nothing to date it from, and
		// inventing now() would restart the clock every tick.
		return reconcile.RollHealth{Phase: protocol.PhaseRolling}
	}

	// Prefer a pod matching what the deployment asks for. A pod carrying a different
	// hash is the OLD one still terminating, which says nothing about the new one.
	var current, fallback *corev1.Pod
	for i := range pods {
		if pods[i].Annotations[AnnotationSpecHash] != wantHash {
			continue
		}
		p := &pods[i]
		// A terminal or terminating pod says nothing about the live rollout; remember
		// one only so an all-terminal wantHash set keeps today's verdict rather than
		// silently becoming `rolling`. Keep the OLDEST such pod (earliest creation), not
		// the first seen, so the fallback verdict is order-independent: the age arm below
		// keys on CreationTimestamp, so retaining a list-order-arbitrary terminal pod would
		// make an all-terminal set read `rolling` or `stuck` depending on List() order.
		// Oldest is also the honest proxy for "how long has this worker been down".
		if p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded || p.DeletionTimestamp != nil {
			if fallback == nil || p.CreationTimestamp.Before(&fallback.CreationTimestamp) {
				fallback = p
			}
			continue
		}
		current = p
		break
	}
	if current == nil {
		current = fallback // only terminal pods matched: preserve status quo, do not drop to rolling
	}
	if current == nil {
		// Only stale pods: the new one is not up yet. Same state as the gap.
		//
		// DELIBERATELY NOT gated on replicaFailure, unlike the pod-less branch above. The
		// worker Deployment's strategy is Recreate — MEASURED off the live spec on
		// dev-cluster, `strategy.type: Recreate`, not merely assumed from the renderer — so
		// the old pod is deleted BEFORE the new one is created. A stale pod still being
		// present therefore means the delete is in flight and the create has not been
		// attempted for it yet, which is a transient by construction. The permanent case
		// (the create attempted and refused) always arrives at the pod-less branch, which is
		// why one gate is sufficient and two would add a cry-wolf window for no coverage.
		return reconcile.RollHealth{Phase: protocol.PhaseRolling}
	}

	health := reconcile.RollHealth{PodPhase: string(current.Status.Phase)}

	// Ready is necessary for `settled` and NOT sufficient (issue #145) — see the header.
	// A Ready pod with a container in a LIVE restart loop falls through to the
	// diagnostics lookup and the stuck arms below, which is the same code path a
	// not-Ready pod takes.
	//
	// 🔴 THE GUARD IS HERE, AT THE ENTRY, AND IT MUST STAY HERE. The tempting equivalent
	// — leave the early return alone and add a recency check to the `RestartCount >=
	// stuckRestartThreshold` arm below — is NOT equivalent and is dangerous. That arm
	// reads a RestartCount filled by mostRestartedContainer from the kubelet's LIFETIME
	// counter, which never decreases. Gating there lets a perfectly healthy worker with
	// 3 restarts accumulated over days of uptime reach the arm and report `stuck`, and
	// since issue #151 removed the INV-5 ceiling from R1 that `upgrade_failed` NEVER
	// EXPIRES. With the guard at the entry, flappingContainer returns nil for that
	// worker, the early return fires, and the arm stays as unreachable for Ready pods as
	// it is today. rollhealth_test.go's "Ready + 5 restarts + up 10m1s -> settled" case
	// is the assertion that fails if this is ever moved into the switch.
	ready, readyAt := readyCondition(current)
	var flapping *corev1.ContainerStatus
	if ready {
		flapping = flappingContainer(current, now)
		if flapping == nil {
			health.Phase = protocol.PhaseSettled
			health.PhaseSince = readyAt
			return health
		}
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
		subject = flapping // the Ready-flapping arm's container; nil on the not-Ready path, so this is a no-op there
	}
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

	// A Ready-but-flapping pod has no CURRENT-state reason to report — its container is
	// Running right now, so neither the Waiting nor the Terminated arm above fires and
	// BlockingReason is left empty. The api's stuckDetail then substitutes "not ready",
	// which is FALSE of this pod: it is Ready, it just will not stay up. So supply the
	// reason from the last termination, and a plain token when even that is absent.
	//
	// CONFINED TO THIS ARM on purpose. Every other path's reason is kubelet's own word
	// for a current state; "Restarting" is a controller-authored token, and widening it
	// would put invented vocabulary on paths that have a real one. (It is safe against
	// the switch below either way — neither the last termination's reason nor
	// "Restarting" is in blockingReasons, so the verdict still comes from the restart
	// arm, which is the arm that justified entering this branch at all.)
	if flapping != nil && subject != nil && health.BlockingReason == "" {
		health.BlockingReason = "Restarting"
		if term := lastTermination(subject); term != nil && term.Reason != "" {
			health.BlockingReason = term.Reason
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

// flappingContainer returns the first container in a LIVE restart loop, or nil when
// none is (issue #145). Both conjuncts are load-bearing and neither works alone:
//
//   - RestartCount >= stuckRestartThreshold. The count ALONE is the trap: it is the
//     kubelet's lifetime counter and never decreases, so "Ready + 3 restarts ⇒ broken"
//     pins a healthy long-lived worker red forever, and post-#151 that verdict has no
//     expiry. The threshold is deliberately the same one the not-Ready path uses, so
//     the two paths stop disagreeing about the same pod.
//   - The current instance has been up less than flapWindow. RECENCY ALONE would fire
//     on a single benign restart — one OOM, one node hiccup. Together they say "keeps
//     dying, recently", and the verdict EXPIRES on continuous uptime, which is a
//     property of the pod rather than of anyone's memory.
//
// THE ANCHOR IS state.running.startedAt, NOT lastState.terminated.finishedAt, AND THAT
// IS MEASURED RATHER THAN PREFERRED. finishedAt is the field that literally dates the
// last death and it is the obvious choice; the data refuted it. Of 41 running samples
// from a real crash-looping pod, EIGHT carried no lastState at all — four were before
// the first restart (correct), but the other four were one entire container instance
// with restartCount 6 and `"lastState": {}`. A rule needing finishedAt would have been
// BLIND for that instance's whole Ready window.
//
// Counted by INSTANCE rather than by sample: seven running instances, of which FIVE
// populated lastState, one (restartCount 0) was correctly empty because nothing had
// died yet, and one (restartCount 6) was the anomaly. So six of seven BEHAVED AS
// EXPECTED — which is not the same as "six populated it", the claim an earlier version
// of this comment made; that phrasing double-counts the restartCount-0 instance, whose
// emptiness this very paragraph depends on being correct. Either way, no amount of
// reading kubelet's documented behaviour would have shown this.
// state.running.startedAt is present on every running container by construction and
// says the same thing one step removed. Do not "simplify" it back.
//
// Init containers first, then app containers, matching blockingContainer: an init
// container blocks everything behind it, so it is the honest one to name.
//
// 🔴 A CONTAINER THAT IS NOT RUNNING IS SKIPPED, NOT SCORED, AND THE DIFFERENCE IS THE
// WHOLE GUARD. "Is this container flapping right now" is UNANSWERABLE for a container
// that is not running, so the honest answer is to exclude it. Reading its absent
// StartedAt as a zero time gives the same verdict today — now.Sub(zero) is decades, so
// it fails the recency conjunct — but for the wrong reason, and the wrong reason is
// fragile in a specific and likely way: a later defensive tidy of the form
// `if t.IsZero() { t = now }`, which looks like hardening, silently turns it into "up 0
// seconds", which SATISFIES the conjunct. Every gate would stay green while the
// container became permanently flagged. The skip cannot be flipped by that edit.
//
// THIS IS THE NORMAL CASE, NOT AN EDGE CASE: `seed-nix` is a plain init container (no
// RestartPolicy — not a native sidecar), so it runs once, exits, and is Terminated for
// the pod's ENTIRE remaining life. Every healthy worker pod in the fleet presents a
// non-Running container to this loop on every tick.
//
// The hazard that makes the skip load-bearing rather than tidy — REASONED, NOT MEASURED,
// and the guard is justified without it: a failed init container is retried and its
// RestartCount increments, and that count persists on the ContainerStatus after the
// retry finally succeeds. A worker whose nix seed was flaky-but-eventually-successful —
// PRD #113's own motivating incident — would then carry RestartCount >= 3 and no Running
// state FOREVER. Under an "up 0 seconds" reading that pod is red permanently, with no
// uptime that could ever clear it, because the container will never run again. Whether
// kubelet really persists the count that way has NOT been verified against a live pod
// and does not need to be: a non-Running container cannot be flapping, so skipping it is
// correct on its own terms and costs nothing if the state turns out to be unreachable.
//
// SCANS ALL CONTAINERS, NOT JUST THE WORKER — a deliberate, user-accepted behaviour
// change, stated out loud because nobody asked for it. A flapping dind sidecar will
// withhold `settled` for the WHOLE POD, so the worker reports failed on its account.
//
// THIS FUNCTION DECIDES THE VERDICT (whether the pod is stuck), NOT necessarily the
// reported name on its own. On the Ready-flapping path, deriveRollHealth now takes the
// SUBJECT — the reported name, restart count, reason and exit code — from the container
// this function returns, so the verdict and the report agree, including the init-first
// ordering this loop applies. Issue #159 fixed that at the `subject := blockingContainer(...)`
// site in deriveRollHealth: when nothing is currently blocking, the subject falls to this
// flapping container before falling to mostRestartedContainer, so a quiet container with a
// higher LIFETIME count no longer supplies the name for a verdict this container justified.
//
// Honest (docker runs really are broken) and it matches how blockingContainer and
// mostRestartedContainer already treat the pod, but it
// is wider than "the agent keeps dying", and the dind sidecars are `RestartPolicy:
// Always` native sidecars whose RestartCount accumulates across the pod's life exactly
// like the worker's. So a fleet-wide sidecar image or config change can satisfy this
// predicate on EVERY worker at once. That correlated case is accepted rather than
// overlooked: if every worker's docker really is broken, badging every worker is TRUE —
// which is exactly what separates it from the auth-rejection path that ruled out the
// api-side heartbeat design, where the fleet would go red for something no worker did.
//
// PERMANENCE IS BOUNDED TO GENUINE CRASH-LOOPS, which is where permanence is correct: no
// container in the worker pod is designed to exit on a periodic cadence (seed-nix exits
// once; dind and dind-init are long-running daemons), and none carries a LIVENESS probe
// that could flap it — the only probes under controller/ are two dind StartupProbes, and
// a StartupProbe failure kills the container rather than cycling it.
func flappingContainer(p *corev1.Pod, now time.Time) *corev1.ContainerStatus {
	for _, set := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
		for i := range set {
			cs := &set[i]
			if cs.RestartCount < stuckRestartThreshold {
				continue
			}
			if cs.State.Running == nil {
				continue
			}
			// `up >= 0` rejects a NEGATIVE uptime, which is what clock skew between the
			// kubelet that stamped StartedAt and this controller produces. Without it a
			// future StartedAt gives a negative duration, and negative < flapWindow is
			// TRUE, so a container reads as flapping on the strength of a clock
			// disagreement. Measured: StartedAt at now+4h with 5 restarts reports stuck.
			//
			// WHY THE GUARD IS WORTH IT — and it is NOT "the bound is small", which is
			// what this comment used to say. One controller clock serves the WHOLE
			// FLEET, so the pre-fix direction is a fleet-wide CORRELATED FALSE ALERT:
			// every worker carrying 3+ lifetime restarts reddens at once because one
			// clock slipped. That is the cry-wolf shape PRD #113 exists to prevent, and
			// it is the same objection that ruled out the api-side heartbeat design.
			// The post-fix direction is a per-container MISSED alert on a worker that
			// was already broken and already unalerted — a degradation to pre-#145
			// behaviour. Against #113's anti-cry-wolf priority the trade is not close.
			//
			// AND THE SKEW BOUND IS NOT ONLY BENIGN, which the old wording implied by
			// saying skew "shifts the window rather than pinning anything red". Shifting
			// a window also shifts things OUT of it. Measured, controller behind the
			// kubelet by δ against a 40s flapper: δ=0 → up 40s, stuck; δ=30s → up 10s,
			// stuck; δ=60s → up −20s, SETTLED; δ=5m → up −4m20s, SETTLED. The exclusion
			// goes PERMANENT once δ exceeds the container's PER-INSTANCE uptime — 60s
			// against a 40s flapper, not some huge number — because a flapper's instance
			// uptime never grows, so past that point every tick is negative forever.
			// Still the accepted direction (a missed alert), but it is a real hole and
			// not merely a shifted threshold.
			//
			// It points the same way as the nil-Running skip above — a duration we
			// cannot trust is not evidence of a restart loop — but note the two are NOT
			// symmetric in consequence, only in mechanism. The nil skip has NO
			// false-negative cost: a container with no current instance genuinely cannot
			// be flapping, so nothing is missed. This one does, exactly as measured
			// above. Read "points the same way" as a statement about the reasoning, not
			// about the cost; taken literally it makes the nil skip look over-cautious
			// or this one look free, and both readings are wrong.
			up := now.Sub(cs.State.Running.StartedAt.Time)
			if up >= 0 && up < flapWindow {
				return cs
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
