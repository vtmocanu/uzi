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

	"gitlab.example.com/vtmocanu/uzi/controller/internal/preset"
	"gitlab.example.com/vtmocanu/uzi/controller/internal/protocol"
	"gitlab.example.com/vtmocanu/uzi/controller/internal/reconcile"
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
type Materializer struct {
	client   kubernetes.Interface
	cfg      RenderConfig
	resolver preset.Resolver
	log      *slog.Logger
	// now is the clock seam for roll-health's age arm (PRD #113 M3). A bare
	// time.Now() at the comparison site would make stuckAge testable only by
	// sleeping for ten minutes. Defaulted in New; overridden directly by tests,
	// which live in this package.
	now func() time.Time
}

// New builds a Materializer.
func New(client kubernetes.Interface, cfg RenderConfig, resolver preset.Resolver, log *slog.Logger) *Materializer {
	return &Materializer{client: client, cfg: cfg, resolver: resolver, log: log, now: time.Now}
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
		switch p.Name {
		case dataPVCName(id):
			o.HasDataPVC = true
		case nixPVCName(id):
			o.HasNixPVC = true
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
// condition when that condition is True, and "" otherwise — including when the condition
// is absent, which is what a healthy Deployment shows (the Deployment controller REMOVES
// the condition on a successful sync rather than setting it False, so absence is the
// healthy state and this must not read it as unknown).
//
// The REASON only. The condition's `message` is not returned and must not be: see the
// pod-less branch in rollhealth.go for both halves of why (free text, and a 64-byte cap
// downstream that would keep only its prefix).
//
// Deployment, not ReplicaSet, and that matters for RBAC: the Deployment controller copies
// its newest ReplicaSet's ReplicaFailure condition onto the Deployment, so the signal is
// readable from the object this controller already lists. Nothing here needs
// `replicasets: list`.
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
	// populates HasDataPVC/HasNixPVC every tick by listing the namespace, so once a PVC is
	// seen we skip re-issuing its create. The AlreadyExists tolerance is retained for the
	// observe→create race (a PVC created since the last Observe, so its flag is still
	// false) — the gate removes the steady-state churn, the tolerance covers the window.
	// Teardown deletes the PVCs, so a reprovisioned worker observes the flag false again
	// and recreates — the delete+reprovision semantics are preserved.
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
		}
		_, err := m.client.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create pvc %s: %w", pvc.Name, err)
		}
	}

	// The Deployment.
	dep := RenderDeployment(m.cfg, w, spec)
	if !obs.HasDeployment {
		_, err := m.client.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create deployment: %w", err)
		}
		return nil
	}

	// Drift (Decision 9): two independent sources, either of which must roll the pod.
	// Note there is deliberately NO controller-side "is the worker busy?" check: the
	// active-run predicate is enforced api-side at delete, and this side has no DB to
	// check with. By the time a row leaves the fleet set the api has already checked.
	wantHash := dep.Spec.Template.Annotations[AnnotationSpecHash]
	if obs.Generation == w.Generation && obs.SpecHash == wantHash {
		return nil
	}
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
// are tracked in the DB and requeued), and a worker whose row is gone has no
// workers.token_hash, so its pod cannot authenticate and is already dead weight.
func (m *Materializer) teardown(ctx context.Context, id, ns string) error {
	m.log.Info("tearing down a hosted worker the api no longer wants", "worker_id", id, "namespace", ns)
	var errs []error

	if err := m.client.AppsV1().Deployments(ns).Delete(ctx, deploymentName(id), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete deployment: %w", err))
	}
	if err := m.client.CoreV1().Secrets(ns).Delete(ctx, secretName(id), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete secret: %w", err))
	}
	for _, name := range []string{dataPVCName(id), nixPVCName(id)} {
		if err := m.client.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete pvc %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
