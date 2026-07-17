package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
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
}

// New builds a Materializer.
func New(client kubernetes.Interface, cfg RenderConfig, resolver preset.Resolver, log *slog.Logger) *Materializer {
	return &Materializer{client: client, cfg: cfg, resolver: resolver, log: log}
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
	get := func(id string) *reconcile.ObservedWorker {
		if o, ok := byID[id]; ok {
			return o
		}
		o := &reconcile.ObservedWorker{ID: id}
		byID[id] = o
		return o
	}

	deployments, err := m.client.AppsV1().Deployments(m.cfg.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list deployments in %s: %w", m.cfg.Namespace, err)
	}
	for i := range deployments.Items {
		d := &deployments.Items[i]
		id, ours := IsOurs(d.Labels)
		if !ours {
			m.flagOrphan("deployment", d.Name)
			continue
		}
		o := get(id)
		o.HasDeployment = true
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

	pvcs, err := m.client.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list persistentvolumeclaims in %s: %w", m.cfg.Namespace, err)
	}
	for i := range pvcs.Items {
		p := &pvcs.Items[i]
		id, ours := IsOurs(p.Labels)
		if !ours {
			m.flagOrphan("persistentvolumeclaim", p.Name)
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

	out := make([]reconcile.ObservedWorker, 0, len(byID))
	for _, o := range byID {
		out = append(out, *o)
	}
	return out, nil
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
func (m *Materializer) flagOrphan(kind, name string) {
	if !strings.HasPrefix(name, NamePrefix) {
		return // not ours, not named like ours: none of our business.
	}
	m.log.Warn("orphan hosted-worker object: named like ours but not stamped by this controller; flagging only, never adopting or deleting",
		"kind", kind, "name", name, "namespace", m.cfg.Namespace,
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

	// Pass 3: teardown, against the set built in pass 1.
	for _, o := range observed {
		if desiredIDs[o.ID] {
			continue
		}
		if err := m.teardown(ctx, o.ID); err != nil {
			errs = append(errs, fmt.Errorf("teardown %s: %w", o.ID, err))
		}
	}
	return errors.Join(errs...)
}

// reconcileWorker converges one desired worker.
func (m *Materializer) reconcileWorker(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker) error {
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
		_, err := m.client.CoreV1().Secrets(m.cfg.Namespace).Create(ctx, RenderSecret(m.cfg, w, *w.JoinToken), metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			// The error is from the apiserver about an object it rejected; it does not carry
			// the Secret's data. Still, nothing here interpolates the token.
			return fmt.Errorf("create token secret: %w", err)
		}
	}

	// The PVCs. Same create/AlreadyExists shape, and never patched: PVC specs are
	// near-immutable, so a size change is delete + reprovision, not a live edit.
	for _, pvc := range RenderPVCs(m.cfg, w, spec) {
		_, err := m.client.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create pvc %s: %w", pvc.Name, err)
		}
	}

	// The Deployment.
	dep := RenderDeployment(m.cfg, w, spec)
	if !obs.HasDeployment {
		_, err := m.client.AppsV1().Deployments(m.cfg.Namespace).Create(ctx, dep, metav1.CreateOptions{})
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
	if _, err := m.client.AppsV1().Deployments(m.cfg.Namespace).Patch(ctx, dep.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch deployment: %w", err)
	}
	return nil
}

// patchFor builds the merge patch that re-renders a drifted Deployment's pod
// template. The selector is deliberately absent: it is immutable, and sending it
// would be a rejected request at best.
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
func (m *Materializer) teardown(ctx context.Context, id string) error {
	m.log.Info("tearing down a hosted worker the api no longer wants", "worker_id", id)
	var errs []error

	if err := m.client.AppsV1().Deployments(m.cfg.Namespace).Delete(ctx, deploymentName(id), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete deployment: %w", err))
	}
	if err := m.client.CoreV1().Secrets(m.cfg.Namespace).Delete(ctx, secretName(id), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete secret: %w", err))
	}
	for _, name := range []string{dataPVCName(id), nixPVCName(id)} {
		if err := m.client.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete pvc %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
