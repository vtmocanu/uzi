package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// Materializer implements reconcile.Materializer against a real kube-apiserver.
//
// It holds the only kube credential in uzi, scoped to one namespace that contains
// nothing but hosted workers. Its Role is pinned to Decision 1's verbs, and one
// line of that list is the security boundary this whole component exists to draw:
//
//	Secrets: create, delete.  NO get. NO list. EVER.
//
// k8s has no existence-only verb for Secrets — get/list returns CONTENTS, and RBAC
// cannot scope it to a dynamic set via resourceNames. Granting either converts a
// controller compromise from "harvest the tokens that happen to flow through the
// compromise window" (this process is stateless and retains nothing) into "harvest
// every hosted worker's join token in one call, including every pre-existing one",
// each token being worker impersonation -> claim that owner's runs -> receive their
// decrypted forge PAT and Anthropic token.
//
// If you find yourself wanting to read a Secret here, the design is telling you
// something is wrong. Two of three validators independently caught M1's first
// protocol violating this line, and materializer_test.go asserts over the WHOLE
// fake-client action log that no Secret get/list ever happens.
// Cordoner is the api cordon/uncordon control-write channel for a hosted worker's
// draining_since flag (PRD #422 M4, Decision 8; uncordon added for issue #458).
// RequestDrain cordons (sets draining_since); ClearDrain uncordons (clears it).
// *apiclient.Client satisfies it structurally (its RequestDrain / ClearDrain), and
// tests substitute a fake.
//
// It is nil-safe by design: a nil Cordoner DISABLES cordoning, so a busy drifted
// worker is simply DEFERRED (never rolled) rather than hard-killed, and the no-drift
// uncordon step is skipped the same way. That keeps the whole feature fail-safe even
// in a build or config where the channel is absent — the one thing that must never
// happen is Recreate-killing a busy worker.
type Cordoner interface {
	RequestDrain(ctx context.Context, workerID string) error
	ClearDrain(ctx context.Context, workerID string) error
}

// DrainPolicy carries the two controller-side overrides that let a BUSY drifted worker
// roll despite the M4 cordon-then-defer default (PRD #422 M5). Both are controller config
// (a chart value + a controller env), not wire data, per Decision 4:
//
//   - Deadline bounds how long a cordoned busy worker delays its roll. Once
//     now-DrainingSince >= Deadline the controller rolls it anyway (its in-flight run
//     requeues), so a worker parked at an approval gate cannot hold the old image forever.
//   - ForceRoll, when true, rolls every drifted worker immediately regardless of busy —
//     the emergency override (e.g. a CVE), accepting the in-flight requeue.
type DrainPolicy struct {
	Deadline  time.Duration
	ForceRoll bool
}

type Materializer struct {
	client   kubernetes.Interface
	cfg      RenderConfig
	resolver preset.Resolver
	// cordoner is the api cordon-write channel (PRD #422 M4). Nil disables cordoning;
	// see Cordoner. Injected the same way as resolver, so tests can drive it.
	cordoner Cordoner
	// drain is the bounded-deadline + force-roll policy (PRD #422 M5). See DrainPolicy.
	drain DrainPolicy
	log   *slog.Logger
	// now is the clock seam for roll-health's age arm (PRD #113 M3). A bare
	// time.Now() at the comparison site would make stuckAge testable only by
	// sleeping for ten minutes. Defaulted in New; overridden directly by tests,
	// which live in this package.
	now func() time.Time
}

// New builds a Materializer. cordoner may be nil, which disables cordoning (a busy
// drifted worker is then deferred rather than rolled — still fail-safe); see Cordoner.
// drain carries the M5 bounded-deadline + force-roll policy (see DrainPolicy).
func New(client kubernetes.Interface, cfg RenderConfig, resolver preset.Resolver, cordoner Cordoner, drain DrainPolicy, log *slog.Logger) *Materializer {
	return &Materializer{client: client, cfg: cfg, resolver: resolver, cordoner: cordoner, drain: drain, log: log, now: time.Now}
}

// Observe lists the worker namespace and partitions it by PROVENANCE.
//
// No label selector on the list, deliberately: an orphan is by definition an object
// whose labels we do NOT expect, so selecting on our own stamp would make orphan
// detection structurally blind to exactly what it is looking for.
//
// The partition:
//   - stamped by us          -> ours, returned as ObservedWorkers
//   - uzi-hw-*-named, not stamped -> ORPHAN: logged, never adopted, never deleted,
//     and never returned (Reconcile cannot act on what it cannot see)
//   - anything else          -> not our business at all
func (m *Materializer) Observe(ctx context.Context) ([]reconcile.ObservedWorker, error) {
	byID := map[string]*reconcile.ObservedWorker{}

	// BOTH worker namespaces (PRD #83 M3): the restricted default and, when this
	// controller is configured for the docker tier, the privileged docker namespace.
	// The whole hosted fleet is desired state, so an object in EITHER namespace that
	// answers to no desired worker is an orphan (flag) or a dropped worker (teardown)
	// — a scan of one namespace could never see the other's, so both are listed and
	// the per-namespace provenance model holds independently in each.
	namespaces := []string{m.cfg.Namespace}
	if m.cfg.DockerNamespace != "" {
		namespaces = append(namespaces, m.cfg.DockerNamespace)
	}
	for _, ns := range namespaces {
		if err := m.observeNamespace(ctx, ns, byID); err != nil {
			return nil, err
		}
	}

	out := make([]reconcile.ObservedWorker, 0, len(byID))
	for _, o := range byID {
		out = append(out, *o)
	}
	return out, nil
}

// observeNamespace folds one namespace's stamped Deployments and PVCs into byID,
// recording the namespace each worker was seen in so teardown can target it.
func (m *Materializer) observeNamespace(ctx context.Context, ns string, byID map[string]*reconcile.ObservedWorker) error {
	get := func(id string) *reconcile.ObservedWorker {
		if o, ok := byID[id]; ok {
			return o
		}
		o := &reconcile.ObservedWorker{ID: id, Namespace: ns}
		byID[id] = o
		return o
	}
	// The image each worker's Deployment currently asks for, collected in the
	// deployment pass and handed to the pod pass below. Kept in a local rather than
	// written straight onto ObservedWorker.Roll because the pod pass REPLACES Roll
	// wholesale, which would drop anything stashed there first.
	targetImages := map[string]string{}
	// The reason on each worker Deployment's ReplicaFailure condition when it is True —
	// the Deployment controller saying a pod create FAILED, as opposed to a pod merely
	// not existing yet (issue #148). Collected in the same pass as targetImages, and for
	// the same reason: the pod pass below replaces Roll wholesale.
	//
	// This is read off an object ALREADY IN HAND and needs no RBAC beyond the `get/list
	// deployments` this loop is built on. That is what makes the fix cheap: the cause of a
	// pod that can never be created is on the Deployment's own status, not in Events —
	// which PRD #113 declined to grant (`events: list` was refused) and which this does
	// not reopen.
	replicaFailures := map[string]string{}

	deployments, err := m.client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list deployments in %s: %w", ns, err)
	}
	for i := range deployments.Items {
		d := &deployments.Items[i]
		id, ours := IsOurs(d.Labels)
		if !ours {
			m.flagOrphan("deployment", d.Name, ns)
			continue
		}
		o := get(id)
		o.HasDeployment = true
		if img := workerImage(d); img != "" {
			targetImages[id] = img
		}
		if reason := replicaFailureReason(d); reason != "" {
			replicaFailures[id] = reason
		}
		// Read the drift signals off the POD TEMPLATE's annotations — where we stamped
		// them, and where they have to be for a change to roll the pod at all.
		ann := d.Spec.Template.Annotations
		o.SpecHash = ann[AnnotationSpecHash]
		if raw := ann[AnnotationGeneration]; raw != "" {
			gen, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				// Someone hand-edited it. Leaving it at 0 means "drifted from any real
				// generation", so the next reconcile re-patches it back to what we render —
				// the safe direction.
				m.log.Warn("hosted worker deployment carries an unparseable generation annotation; treating it as drifted",
					"worker_id", id, "annotation", AnnotationGeneration)
			} else {
				o.Generation = gen
			}
		}
	}

	pvcs, err := m.client.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list persistentvolumeclaims in %s: %w", ns, err)
	}
	for i := range pvcs.Items {
		p := &pvcs.Items[i]
		id, ours := IsOurs(p.Labels)
		if !ours {
			m.flagOrphan("persistentvolumeclaim", p.Name, ns)
			continue
		}
		o := get(id)
		// A Terminating PVC still EXISTS (it counts against the create-gate and must
		// not be re-created), but its size must never be sourced from it — a recycle in
		// flight would otherwise read the doomed volume's size as live. Compute the flag
		// once and let each case decide what it means for that claim.
		terminating := p.DeletionTimestamp != nil
		switch p.Name {
		case dataPVCName(id):
			o.HasDataPVC = true
		case nixPVCName(id):
			o.HasNixPVC = true // exists-in-ANY-state (UNCHANGED)
			if !terminating {  // never source size from a Terminating PVC
				sz := p.Spec.Resources.Requests[corev1.ResourceStorage]
				o.NixPVCSize = &sz // live nix size; nil while absent OR Terminating
			}
		case dindDataPVCName(id):
			// Docker workers only (issue #224 M-a). Matched unconditionally rather than
			// behind a docker check because this loop sees only a NAME and a stamp: the
			// worker's Docker flag lives in the desired set, which teardown is defined not
			// to have. A plain worker never has an object by this name, so the arm is
			// simply never taken for one.
			o.HasDinDDataPVC = true
		}
	}

	// Pods (PRD #113 M3): roll health, and the only place it is readable. LIST only —
	// the grant is `list` and nothing here may become a get-by-name, which is
	// impossible anyway (pod names are Deployment-generated).
	//
	// Unselected, for the same reason as the two lists above: a label selector would
	// make the unmanaged-pod check below structurally blind to exactly what it looks
	// for. Pods inherit the pod template's labels, so IsOurs works on them unchanged.
	// A POD-LIST FAILURE IS LOGGED AND SWALLOWED. It must never abort the cycle, and
	// this is the one asymmetry in this function that is worth reading carefully.
	//
	// The Deployment and PVC lists above return their errors, and that is CORRECT for
	// them: they drive decisions, so reconciling against a view we just admitted we
	// could not read is how a healthy worker gets clobbered. Nothing of the sort is
	// true here. `.Roll` is written below and consumed in exactly ONE place — the
	// display-only status report in reconcile.Tick. No create, patch, teardown or
	// token-delivery decision reads it.
	//
	// So returning an error here would stop provisioning, teardown, patching and token
	// delivery for the entire hosted fleet on account of a read whose only consumer is
	// a badge. MEASURED, and this is not hypothetical: the controller ServiceAccount
	// deployed on dev-cluster today answers `no` to `list pods` in both worker
	// namespaces (with `list deployments` = yes as the positive control), and nothing
	// in the chart orders worker-rbac.yaml ahead of controller-deployment.yaml — no
	// sync-wave on either. So the first tick after this image becomes ready 403s. With
	// the swallow, that is a few ticks of absent roll-health rows, which the api already
	// reads as "no signal"; without it, the fleet wedges and the symptom points nowhere
	// near roll health.
	//
	// The principle is the same one the report step in Tick states from the other side:
	// an observability feature must never be able to take down the thing it observes.
	pods, err := m.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		m.log.Warn("listing pods for roll health failed; continuing without roll health for this namespace (reconciliation is unaffected)",
			"namespace", ns, "error", err)
		return nil
	}
	byWorker := map[string][]corev1.Pod{}
	for i := range pods.Items {
		p := &pods.Items[i]
		id, ours := IsOurs(p.Labels)
		if !ours {
			m.flagUnmanagedPod(p.Name, ns)
			continue
		}
		byWorker[id] = append(byWorker[id], *p)
	}
	// Derive for every worker we have a DEPLOYMENT for, not merely for every worker we
	// found pods for. The difference is the Recreate gap: between the old pod being
	// deleted and the new one appearing there are no pods, and iterating pods alone
	// would emit NO ROW for that worker.
	//
	// A missing row is not neutral. The api reads absence as "no signal", so the only
	// thing keeping a healthy mid-roll worker from flashing `outdated` during the gap
	// would be the 60s freshness TTL still holding the PREVIOUS report alive — an
	// invisible dependency on one constant in another module, where shortening the TTL
	// would produce cry-wolf that nobody would trace back to it. Emitting an explicit
	// `rolling` row for a pod-less worker removes that coupling: the gap now asserts
	// what it actually is. It is also what design §B-4 specifies.
	now := m.now()
	for _, o := range byID {
		if o.Namespace != ns || !o.HasDeployment {
			continue
		}
		// wantHash is what this worker's DEPLOYMENT currently asks for, already read
		// off its pod template above. Passing it is what makes the derivation about
		// pod readiness rather than deployment drift — see rollhealth.go's header.
		o.Roll = deriveRollHealth(byWorker[o.ID], o.SpecHash, replicaFailures[o.ID], now)
		o.Roll.TargetImage = targetImages[o.ID]
	}
	// A worker seen ONLY as pods (its Deployment already gone, its pods still
	// terminating) gets roll health too — teardown is in flight and the row is honest
	// about what is there.
	for id, ps := range byWorker {
		o := get(id)
		if o.HasDeployment {
			continue // already derived above
		}
		// replicaFailures is empty here by construction — this loop is the workers with NO
		// Deployment, so there is no Deployment status to have read a condition off.
		// Indexed rather than passed as a literal "" so the two call sites cannot drift.
		o.Roll = deriveRollHealth(ps, o.SpecHash, replicaFailures[id], now)
		o.Roll.TargetImage = targetImages[id]
	}
	return nil
}

// replicaFailureReason returns the reason on a worker Deployment's ReplicaFailure
// condition when that condition is True, and "" otherwise.
//
// "" INCLUDES THE ABSENT CASE, and absence is the healthy state rather than an unknown one.
// MEASURED BY EXPERIMENT on dev-cluster, 2026-07-26: creating the missing ServiceAccount
// made one pod create succeed and Kubernetes REMOVED the condition outright — it did not set
// it False. Deleting the ServiceAccount again brought it back within ~15s. So there is no
// ConditionFalse state to handle on a real cluster, and the condition tracks CURRENT state,
// not history.
//
// That non-stickiness cuts both ways and both are load-bearing:
//
//   - It is why this is safe. The condition cannot be a stale accusation left over from a
//     failure that was already fixed, so `stuck` here always means "creates are failing NOW".
//   - It is why nothing may assume the condition persists. In the genuinely broken state it
//     was present continuously for ~32 minutes (20:10→20:42), so a controller on the 10s
//     default cadence sees it on essentially every tick — but one successful create clears
//     it, and the next tick correctly reports the gap instead.
//
// The REASON only. The condition's `message` is not returned and must not be: see the
// pod-less branch in rollhealth.go for both halves of why (free text, and a 64-byte cap
// downstream that keeps only its prefix — measured to be pure pod name).
//
// Deployment, not ReplicaSet, and that matters for RBAC: the Deployment controller copies
// its newest ReplicaSet's ReplicaFailure condition onto the Deployment, so the signal is
// readable from the object this controller already lists. Nothing here needs
// `replicasets: list`. (Confirmed on the live object: the ReplicaSet carries the identical
// condition, same reason, same message. Either would do; only one is already granted.)
//
// CONSIDERED AND REJECTED as the discriminator: `Progressing=False` /
// `ProgressDeadlineExceeded`, which sits on the same object and is Kubernetes' own canonical
// "this rollout failed" signal. It is broader — it also catches a pod that exists and never
// becomes Ready — and its message is 94 bytes rather than 185. But MEASURED on the same
// worker, it is LATE: absent at 20:14 and 20:42 with `Progressing=True`, and appearing only
// once `progressDeadlineSeconds` elapsed (600 on the live spec, the k8s default). Ten minutes
// of silence is most of what #148 is complaining about, and the arm would also fire on a
// genuinely slow cold pull of the browser-inflated agent image. ReplicaFailure is immediate
// and names the actual cause, so it is the primary signal.
//
// Adding ProgressDeadlineExceeded as a SECOND, catch-all arm is a live option and is not
// taken here — the stall shapes it would add are already covered by the pod path's three
// arms, so its marginal coverage is "pod-less AND no ReplicaFailure AND stalled past the
// deadline", against a real cry-wolf cost on slow pulls. Worth noting that its 600s deadline
// happens to equal the controller's own stuckAge; that is two independent defaults matching,
// NOT designed coupling, and neither may be tuned on the assumption the other follows.
func replicaFailureReason(d *appsv1.Deployment) string {
	for i := range d.Status.Conditions {
		c := &d.Status.Conditions[i]
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			return c.Reason
		}
	}
	return ""
}

// workerImage returns the agent container's image from a worker Deployment's pod
// template — what the cluster was TOLD to run, which is the honest target to report.
// Selected by container name rather than index: the docker tier adds a DinD sidecar,
// so containers[0] is not reliably the agent.
func workerImage(d *appsv1.Deployment) string {
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == workerContainerName {
			return c.Image
		}
	}
	return ""
}

// flagUnmanagedPod warns about a pod in a worker namespace that this controller did
// not stamp (PRD #113 M3, design 5d).
//
// Deliberately WIDER than flagOrphan, which returns early for any name not prefixed
// uzi-hw- ("not ours, not named like ours: none of our business"). That early return
// means a pod simply parked in a worker namespace under any other name is
// enumerated, matched against nothing, and never logged — and nothing PREVENTS one
// being parked there. The three controls that bound blast radius (default-deny
// NetworkPolicy, PodSecurity admission, ResourceQuota) all bound what such a pod can
// DO; none of them bound admission.
//
// Free here because Observe now lists pods anyway, so this costs no RBAC. It detects
// rather than prevents, and it is noisy if anything ever legitimately lands in a
// worker namespace — which the namespace inventory says nothing should, so the noise
// IS the signal.
func (m *Materializer) flagUnmanagedPod(name, ns string) {
	m.log.Warn("unmanaged pod in a worker namespace: not stamped by this controller; flagging only, never adopting or deleting",
		"kind", "pod", "name", name, "namespace", ns,
		"expected_stamp", LabelManagedBy+"="+ValueManagedBy)
}

// flagOrphan logs a uzi-hw-*-named object we did not create. Never adopted, never
// deleted — "the controller never takes ownership of an object it did not stamp".
//
// This set is not vacuous, which is what makes Decision 9 and Decision 11 able to
// coexist: it is hand-made objects, leftovers from an older label scheme, or a
// partial create from a crashed reconcile before the labels landed.
//
// Orphan SECRETS stay undetectable, since enumerating them is precisely what
// Decision 1 refuses. That is the accepted trade, and it is no wider than accepted:
// a DELETED worker's Secret is still cleaned up, because its name is reachable from
// the worker id we recover off the observed Deployment/PVC labels.
func (m *Materializer) flagOrphan(kind, name, ns string) {
	if !strings.HasPrefix(name, NamePrefix) {
		return // not ours, not named like ours: none of our business.
	}
	m.log.Warn("orphan hosted-worker object: named like ours but not stamped by this controller; flagging only, never adopting or deleting",
		"kind", kind, "name", name, "namespace", ns,
		"expected_stamp", LabelManagedBy+"="+ValueManagedBy)
}

// Reconcile drives the cluster towards desired state.
//
// READ THIS BEFORE TOUCHING THE TEARDOWN SET:
//
//	teardown = {objects we stamped} \ {ALL desired ids}      <- correct
//	teardown = {objects we stamped} \ {ids we rendered}      <- DESTROYS THE FLEET
//
// An unknown size or template means SKIP RENDERING that worker. It must never
// remove the worker from the desired set. The tolerance exists for deployment skew
// — api and controller are separately-built images, so even under Model B's version
// pinning a rollout has a window where an OLD controller polls a NEW api — and
// getting it backwards means the very skew the tolerance exists for tears down
// every worker carrying the new size or template.
//
// The structure below is the guard: desiredIDs is built in its own pass over the
// FULL desired list, before anything is resolved or rendered, so no later failure
// can silently shrink it.
func (m *Materializer) Reconcile(ctx context.Context, desired []protocol.DesiredWorker, observed []reconcile.ObservedWorker) error {
	// Pass 1: the desired set. EVERY id, unconditionally — no resolve, no render, no
	// error can remove one from here.
	desiredIDs := make(map[string]bool, len(desired))
	for _, w := range desired {
		desiredIDs[w.ID] = true
	}

	observedByID := make(map[string]reconcile.ObservedWorker, len(observed))
	for _, o := range observed {
		observedByID[o.ID] = o
	}

	var errs []error

	// Pass 2: create / roll the renderable workers. A failure here is collected and
	// the loop continues: one worker's bad day must not stall the fleet.
	for _, w := range desired {
		if err := m.reconcileWorker(ctx, w, observedByID[w.ID]); err != nil {
			errs = append(errs, fmt.Errorf("worker %s: %w", w.ID, err))
		}
	}

	// Pass 3: teardown, against the set built in pass 1. Each dropped worker is torn
	// down in the namespace it was OBSERVED in (a docker worker lives in the docker
	// namespace) — its Docker flag is no longer in the desired set to derive it from.
	for _, o := range observed {
		if desiredIDs[o.ID] {
			continue
		}
		ns := o.Namespace
		if ns == "" {
			// A pre-#83 observation, or a fake in a test, carried no namespace: the
			// restricted default is where single-namespace workers always lived.
			ns = m.cfg.Namespace
		}
		if err := m.teardown(ctx, o.ID, ns); err != nil {
			errs = append(errs, fmt.Errorf("teardown %s: %w", o.ID, err))
		}
	}
	return errors.Join(errs...)
}

// recycleVolumes selects which of a worker's PVCs a recycle tears down: {Nix} for the
// M3 size-drift arm, {Nix, Data} for the M4 disk-pressure arm. dind is never recycled.
type recycleVolumes struct {
	Nix  bool
	Data bool
}

// recycleWorkerVolumes tears a worker's Deployment and the selected PVC(s) down so the
// next tick reprovisions them in place — a delete + recreate, NOT a live PVC resize
// (PVC specs are near-immutable). It keeps the Secret, the worker row/uuid, and any
// PVC not named in vols.
//
// It implements PHASES 0-2 only. The reprovision itself (phase 3, the PVC-create gate)
// and the strengthened Deployment guard (phases 4-5) live in reconcileWorker and run on
// a LATER tick, once Observe has seen the volumes gone — the fake clientset removes a
// deleted object immediately, which is what makes that two-tick property testable.
//
//   - Phase 0 reuses reconcileWorker's EXACT busy-guard: a busy worker with no roll
//     override is cordoned (if not already draining) and the recycle DEFERRED — nothing
//     is deleted this tick. Fail-safe on every cordoner outcome, exactly as the roll path.
//   - Phase 1 deletes the Deployment (so nothing mounts the doomed volume mid-delete).
//   - Phase 2 deletes each selected, LIVE PVC. A nil NixPVCSize means the nix PVC is
//     already Terminating or absent, so it is skipped rather than re-deleted.
func (m *Materializer) recycleWorkerVolumes(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker, ns string, vols recycleVolumes) error {
	// Phase 0: drain-if-busy. Identical guard to reconcileWorker's roll path — a busy
	// worker is cordoned + deferred rather than having its volumes torn out from under a
	// live run, unless an override (force-roll, or the elapsed drain deadline) says roll.
	rollDespiteBusy := m.drain.ForceRoll ||
		(w.DrainingSince != nil && m.now().Sub(*w.DrainingSince) >= m.drain.Deadline)
	if w.Busy && !rollDespiteBusy {
		if w.DrainingSince == nil {
			if m.cordoner != nil {
				if err := m.cordoner.RequestDrain(ctx, w.ID); err != nil {
					m.log.Warn("cordon request failed; deferring recycle (fail-safe)", "worker_id", w.ID, "error", err)
				} else {
					m.log.Info("hosted worker needs a volume recycle but is busy; cordoned, deferring", "worker_id", w.ID)
				}
			} else {
				m.log.Warn("hosted worker needs a volume recycle but is busy and no cordon channel; deferring", "worker_id", w.ID)
			}
		} else {
			m.log.Info("hosted worker needs a volume recycle, busy, already draining; deferring", "worker_id", w.ID)
		}
		return nil
	}

	var errs []error

	// Phase 1: delete the Deployment so no pod mounts the volume while it is being
	// reprovisioned. NotFound is success — a partial recycle converges next tick.
	if obs.HasDeployment {
		if err := m.client.AppsV1().Deployments(ns).Delete(ctx, deploymentName(w.ID), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete deployment for recycle: %w", err))
		} else {
			m.log.Info("recycling hosted worker volumes: deleted deployment", "worker_id", w.ID, "namespace", ns)
		}
	}

	// Phase 2: delete the selected PVC(s). Nix only when a LIVE one was observed
	// (NixPVCSize != nil); a nil means it is already Terminating/absent, so skip it
	// rather than issue a redundant delete.
	var names []string
	if vols.Nix && obs.HasNixPVC && obs.NixPVCSize != nil {
		names = append(names, nixPVCName(w.ID))
	}
	// vols.Data is the M4 disk-pressure arm and is intentionally NOT implemented here:
	// M3 recycles /nix only. M4 adds the data PVC (and its own liveness gate) to this
	// list without touching the phases around it.
	for _, name := range names {
		if err := m.client.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete pvc %s for recycle: %w", name, err))
		} else {
			m.log.Info("recycling hosted worker volumes: deleted pvc", "worker_id", w.ID, "pvc", name, "namespace", ns)
		}
	}
	return errors.Join(errs...)
}

// reconcileWorker converges one desired worker.
func (m *Materializer) reconcileWorker(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker) error {
	// A docker worker needs the privileged docker namespace, and this controller is
	// not always configured for it (the kind smoke, any instance with the docker tier
	// off). Skip RENDERING with a loud error rather than default the namespace —
	// exactly the unknown-preset posture below: the worker stays desired (never torn
	// down), and materializes the moment the controller is configured. This is the
	// "no silent default" rule from config.go carried to the reconcile: never render a
	// privileged pod into the restricted default because a namespace was unset.
	if w.Docker && m.cfg.DockerNamespace == "" {
		m.log.Error("desired worker requests docker but this controller has no docker namespace configured; skipping its RENDER only (set UZI_WORKER_DOCKER_NAMESPACE + UZI_WORKER_DIND_IMAGE)",
			"worker_id", w.ID)
		return nil
	}
	ns := m.cfg.namespaceFor(w)

	spec, err := m.resolver.Resolve(w.Template, w.Size)
	if err != nil {
		if preset.IsUnknown(err) {
			// Log and skip RENDERING. The worker stays desired (see Reconcile's contract),
			// so its existing objects are untouched and it is picked up the moment this
			// controller is upgraded to a build that knows the name.
			//
			// Only the worker id and the unresolvable name are logged. Both are safe: the id
			// is a uuid and the name is a server-validated registry value, never free text.
			m.log.Warn("desired worker names something this controller cannot resolve; skipping its RENDER only (its existing objects are deliberately left alone)",
				"worker_id", w.ID, "error", err.Error())
			return nil
		}
		return err
	}

	// The Secret. JoinToken == nil means "write no Secret for this worker" — NEVER
	// "this worker has no token". Either a pod already proved it holds one (in which
	// case the cluster Secret is the only copy of it in existence, and clearing it
	// would strand a working worker), or the api's buffer expired unread and recovery
	// is a rotation. Never invent one, never clear the existing Secret on account of
	// it, and never log the field.
	if w.JoinToken != nil {
		// There is no get on Secrets, so "create if absent" is not available to us — and
		// does not need to be. Issue the create and treat AlreadyExists as success. That
		// is the WHOLE of Secret idempotence, and it needs no read.
		_, err := m.client.CoreV1().Secrets(ns).Create(ctx, RenderSecret(m.cfg, w, *w.JoinToken), metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			// The error is from the apiserver about an object it rejected; it does not carry
			// the Secret's data. Still, nothing here interpolates the token.
			return fmt.Errorf("create token secret: %w", err)
		}
	}

	// M3: /nix size-drift recycle. Teardown + reprovision in place, NOT expansion. Trigger
	// STRICTLY less-than only (a PVC cannot shrink; >= must never recycle). NixPVCSize==nil
	// (absent or Terminating nix PVC) never triggers — a mid-recycle tick falls through to the
	// create-gate and the Deployment guard below. Keeps the Secret, the worker row/uuid, /data.
	if obs.HasNixPVC && obs.NixPVCSize != nil && obs.NixPVCSize.Cmp(spec.NixSize) < 0 {
		return m.recycleWorkerVolumes(ctx, w, obs, ns, recycleVolumes{Nix: true})
	}

	// The PVCs. Same create/AlreadyExists shape as the Deployment below, and never
	// patched: PVC specs are near-immutable, so a size change is delete + reprovision,
	// not a live edit.
	//
	// Gate the create on the observed flag (issue #114 BUG 3), exactly as the Deployment
	// does with obs.HasDeployment. An UNCONDITIONAL create every ~10s tick charged the
	// ResourceQuota's `used.requests.storage` at admission for each PVC, and k8s does NOT
	// decrement it when storage then rejects the create AlreadyExists (upstream #119593):
	// the redundant charges out-raced the quota-controller resync and pinned the quota at
	// its hard limit, so genuinely-new workers were then refused their PVCs. Observe
	// populates HasDataPVC/HasNixPVC/HasDinDDataPVC every tick by listing the namespace, so
	// once a PVC is seen we skip re-issuing its create. The AlreadyExists tolerance is
	// retained for the observe→create race (a PVC created since the last Observe, so its
	// flag is still false) — the gate removes the steady-state churn, the tolerance covers
	// the window. Teardown deletes the PVCs, so a reprovisioned worker observes the flag
	// false again and recreates — the delete+reprovision semantics are preserved.
	//
	// The dind data root (issue #224 M-a) is a THIRD claim, and only for a docker worker:
	// RenderPVCs emits it only when w.Docker, so this loop never sees it for a plain one
	// and its gate cannot mis-fire there.
	for _, pvc := range RenderPVCs(m.cfg, w, spec) {
		switch pvc.Name {
		case dataPVCName(w.ID):
			if obs.HasDataPVC {
				continue
			}
		case nixPVCName(w.ID):
			if obs.HasNixPVC {
				continue
			}
		case dindDataPVCName(w.ID):
			if obs.HasDinDDataPVC {
				continue
			}
		}
		_, err := m.client.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create pvc %s: %w", pvc.Name, err)
		}
	}

	// The Deployment.
	dep := RenderDeployment(m.cfg, w, spec)
	if !obs.HasDeployment {
		// Suppress the (re)create while a nix PVC EXISTS but is not LIVE-at-desired-size
		// (Terminating, or mid-recycle). Never place the Deployment over a doomed/undersized
		// volume. When the nix PVC is ABSENT (HasNixPVC==false: first-create or post-reap
		// re-mint tick) this is false, so first-provision and re-mint still create in one tick.
		nixLiveAtSize := obs.NixPVCSize != nil && obs.NixPVCSize.Cmp(spec.NixSize) >= 0
		if obs.HasNixPVC && !nixLiveAtSize {
			return nil
		}
		_, err := m.client.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create deployment: %w", err)
		}
		return nil
	}

	// Drift (Decision 9): two independent sources, either of which must roll the pod.
	wantHash := dep.Spec.Template.Annotations[AnnotationSpecHash]
	if obs.Generation == w.Generation && obs.SpecHash == wantHash {
		// No drift. But a worker cordoned on a PREVIOUS tick's drift whose drift was then
		// REVERTED (operator reset workers.image.tag mid-rollout) has nothing left to roll,
		// and draining_since is otherwise cleared ONLY by RegisterWorker on an actual roll
		// (api runtime.sql) — so without this it would stay cordoned, idled by the claim
		// gate, forever (issue #458). Clearing here is safe because draining_since has a
		// SINGLE writer and a single meaning — the controller's own drift-cordon (there is
		// no user/admin/node-maintenance drain) — so "no drift + draining" can only be a
		// reverted drift or a just-committed roll, both correct to clear. Fail-safe and
		// idempotent: on error we log and let the next tick retry (the condition persists);
		// a nil cordoner (cordoning disabled) skips it, as with RequestDrain.
		if w.DrainingSince != nil && m.cordoner != nil {
			if err := m.cordoner.ClearDrain(ctx, w.ID); err != nil {
				m.log.Warn("uncordon (clear draining_since) failed; will retry next tick", "worker_id", w.ID, "error", err)
			} else {
				m.log.Info("hosted worker no longer drifted; cleared cordon", "worker_id", w.ID)
			}
		}
		return nil
	}
	// Drift. PRD #422 M5: a busy worker is normally cordoned + deferred (M4), BUT two
	// overrides roll it anyway (accepting today's requeue-resume): an operator force-roll
	// (emergency, e.g. a CVE), and the bounded drain deadline (a worker parked at an
	// approval gate must not hold the old image forever). Both measured/flagged controller-
	// side; the deadline elapses from the api-supplied draining_since (this loop is stateless).
	rollDespiteBusy := m.drain.ForceRoll ||
		(w.DrainingSince != nil && m.now().Sub(*w.DrainingSince) >= m.drain.Deadline)
	if w.Busy && !rollDespiteBusy {
		if w.DrainingSince == nil {
			// not yet cordoned — request the cordon (fail-safe: any failure still defers)
			if m.cordoner != nil {
				if err := m.cordoner.RequestDrain(ctx, w.ID); err != nil {
					m.log.Warn("cordon request failed; deferring roll (fail-safe)", "worker_id", w.ID, "error", err)
				} else {
					m.log.Info("hosted worker drifted but busy; cordoned, deferring roll", "worker_id", w.ID)
				}
			} else {
				m.log.Warn("hosted worker drifted but busy and no cordon channel; deferring roll", "worker_id", w.ID)
			}
		} else {
			m.log.Info("hosted worker drifted, busy, already draining; deferring roll", "worker_id", w.ID)
		}
		return nil
	}
	if w.Busy {
		// rolling a busy worker: its in-flight run(s) will requeue (Decision 4 fallback).
		reason := "drain deadline exceeded"
		if m.drain.ForceRoll {
			reason = "operator force-roll"
		}
		m.log.Warn("rolling a BUSY hosted worker anyway; its in-flight run will requeue", "worker_id", w.ID, "reason", reason)
	}
	// Idle (or an override fired): safe to roll now (whether or not it was ever draining —
	// a cordoned worker that has since gone idle rolls on this tick).
	m.log.Info("hosted worker drifted; rolling",
		"worker_id", w.ID,
		"generation_observed", obs.Generation, "generation_desired", w.Generation,
		"spec_hash_changed", obs.SpecHash != wantHash)
	patch, err := patchFor(dep)
	if err != nil {
		return err
	}
	if _, err := m.client.AppsV1().Deployments(ns).Patch(ctx, dep.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch deployment: %w", err)
	}
	return nil
}

// patchFor builds the merge patch that re-renders a drifted Deployment's pod
// template. The selector is deliberately absent: it is immutable, and sending it
// would be a rejected request at best.
//
// KNOWN LIMIT of using a JSON MERGE patch, recorded so nobody assumes more than it
// gives: drift correction is convergent for the CONTAINERS (lists are replaced
// wholesale, so a hand-edited image or env is overwritten) but NOT for pod-level
// booleans. Go's `omitempty` drops zero-valued fields from the marshalled template,
// and a merge patch treats an absent key as "leave it alone" — so a hand-set
// `hostNetwork: true` would survive a roll rather than be reset to false.
//
// Not exploitable, and that is why it is a comment and not a fix: PodSecurity
// `restricted` rejects hostNetwork/hostPID/hostIPC at admission, and only a cluster
// admin can edit objects in the worker namespace (the workers' own SA has zero RBAC
// and no mounted token). A strategic-merge or apply-based patch would close it if
// this ever needs to be true in general.
func patchFor(dep *appsv1.Deployment) ([]byte, error) {
	patch := map[string]any{
		"spec": map[string]any{
			"template": dep.Spec.Template,
			// revisionHistoryLimit is a spec-level SIBLING of template, not inside it
			// (a merge patch treats an absent key as "leave alone", so it would never
			// reach an already-provisioned worker if omitted). Carry it here so existing
			// long-lived workers pick up the bounded RS history on their next drift-roll,
			// in lockstep with the Create path in RenderDeployment (issue #360).
			"revisionHistoryLimit": dep.Spec.RevisionHistoryLimit,
		},
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("marshal deployment patch: %w", err)
	}
	return raw, nil
}

// teardown removes a worker we provisioned that the api no longer wants
// (Decision 11). Deployment -> Secret -> PVCs, tolerating NotFound on each so a
// partial teardown converges on the next tick.
//
// Deleting the Secret BY NAME is what keeps Decision 1's Secrets line affordable:
// the name is derived from the worker id we recovered off the Deployment/PVC
// labels, so no enumeration — and therefore no read — is ever needed.
//
// Safe by construction even though a hosted worker's volumes are destroyed here:
// /nix is a cache (re-seedable from the image), /data is the clone cache plus
// per-run workspaces (the forge holds the durable output — branch and MR — and runs
// are tracked in the DB and requeued), the dind data root is the daemon's image and
// build cache (re-pullable, and none of it worker state — issue #224 M-a), and a
// worker whose row is gone has no workers.token_hash, so its pod cannot
// authenticate and is already dead weight.
//
// The dind claim is deleted UNCONDITIONALLY, not behind a docker check, and that is
// the only correct shape: teardown runs against a worker the api has DROPPED, so its
// Docker flag is no longer in the desired set to branch on. A plain worker never had
// one, and the delete NotFounds — which this loop already tolerates.
func (m *Materializer) teardown(ctx context.Context, id, ns string) error {
	m.log.Info("tearing down a hosted worker the api no longer wants", "worker_id", id, "namespace", ns)
	var errs []error

	if err := m.client.AppsV1().Deployments(ns).Delete(ctx, deploymentName(id), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete deployment: %w", err))
	}
	if err := m.client.CoreV1().Secrets(ns).Delete(ctx, secretName(id), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete secret: %w", err))
	}
	for _, name := range []string{dataPVCName(id), nixPVCName(id), dindDataPVCName(id)} {
		if err := m.client.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete pvc %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
