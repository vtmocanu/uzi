package kube

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"gitlab.example.com/vtmocanu/uzi/controller/internal/protocol"
	"gitlab.example.com/vtmocanu/uzi/controller/internal/reconcile"
)

func newMat(t *testing.T, objs ...runtime.Object) (*Materializer, *fake.Clientset) {
	t.Helper()
	client := fake.NewSimpleClientset(objs...)
	m := New(client, testConfig(), testResolver(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return m, client
}

func token(s string) *string { return &s }

// assertNoSecretReads is THE gate on Decision 1's load-bearing line: the controller
// holds create/delete on Secrets and nothing more, because k8s has no
// existence-only verb and a get/list would let a compromised controller harvest
// every hosted worker's join token for the fleet's lifetime.
//
// It asserts over the WHOLE action log rather than one call site, so it is a gate
// rather than a reviewer's vigilance — two of three validators independently caught
// M1's first protocol violating this line, which is exactly why it is mechanical now.
func assertNoSecretReads(t *testing.T, client *fake.Clientset) {
	t.Helper()
	for _, a := range client.Actions() {
		if a.GetResource().Resource != "secrets" {
			continue
		}
		switch a.GetVerb() {
		case "get", "list", "watch":
			t.Fatalf("a %q on secrets reached the apiserver. PRD #58 Decision 1: the controller's Role is "+
				"create/delete ONLY, so this call would fail in production — and if the Role were widened to "+
				"make it pass, a controller compromise would become fleet-wide join-token disclosure.", a.GetVerb())
		}
	}
}

func secretVerbs(client *fake.Clientset) []string {
	var out []string
	for _, a := range client.Actions() {
		if a.GetResource().Resource == "secrets" {
			out = append(out, a.GetVerb())
		}
	}
	return out
}

func TestReconcileCreatesTheWholeObjectSetForANewWorker(t *testing.T) {
	m, client := newMat(t)
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", JoinToken: token("uzw_secret")}

	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	ns := testConfig().Namespace
	if _, err := client.AppsV1().Deployments(ns).Get(context.Background(), "uzi-hw-w1", metav1.GetOptions{}); err != nil {
		t.Errorf("deployment not created: %v", err)
	}
	for _, name := range []string{"uzi-hw-w1-data", "uzi-hw-w1-nix"} {
		if _, err := client.CoreV1().PersistentVolumeClaims(ns).Get(context.Background(), name, metav1.GetOptions{}); err != nil {
			t.Errorf("pvc %s not created: %v", name, err)
		}
	}
	if got := secretVerbs(client); len(got) != 1 || got[0] != "create" {
		t.Errorf("secret verbs = %v, want exactly one create", got)
	}
	assertNoSecretReads(t, client)
}

// TRAP 1, and it is the fleet-destruction path.
//
//	teardown = {ours} \ {ALL desired ids}      <- correct
//	teardown = {ours} \ {ids we rendered}      <- destroys the fleet
//
// An old controller polling a NEW api sees names its tables lack. That is the exact
// deployment skew the tolerance exists for — separately built images, so the window
// is real even under Model B pinning. If an unrenderable worker were dropped from
// the desired set, that skew would tear down EVERY worker carrying the new size or
// template. Template is equal to Size here: the controller resolves both against
// its own tables, so both skew identically.
func TestUnknownNamesSkipRenderingAndNEVERTearDownTheirObjects(t *testing.T) {
	for _, tc := range []struct{ name, template, size string }{
		{"unknown size (a newer api's preset)", "base", "xxl"},
		{"unknown template (a newer api's image)", "python", "m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The skewed worker is ALREADY deployed — this is the case that matters. An old
			// controller must leave a running worker alone, not delete it.
			existing := deployedWorker("w1", 0, "some-hash")
			m, client := newMat(t, existing, pvcFor("w1", "data"), pvcFor("w1", "nix"))

			skewed := protocol.DesiredWorker{ID: "w1", Template: tc.template, Size: tc.size}
			healthy := protocol.DesiredWorker{ID: "w2", Template: "base", Size: "m", JoinToken: token("uzw_t")}
			observed := []reconcile.ObservedWorker{{ID: "w1", HasDeployment: true, SpecHash: "some-hash", HasDataPVC: true, HasNixPVC: true}}

			// It must not crash the loop: one unknown name must never stall the fleet.
			if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{skewed, healthy}, observed); err != nil {
				t.Fatalf("Reconcile must tolerate an unresolvable name, got: %v", err)
			}

			// THE CLAUSE. Assert it explicitly.
			for _, a := range client.Actions() {
				if a.GetVerb() == "delete" {
					t.Fatalf("a delete reached the apiserver for an unrenderable-but-DESIRED worker (%s). "+
						"This is the fleet-destruction path: an old controller polling a new api would tear down "+
						"every worker carrying the new name.", a.GetResource().Resource)
				}
			}
			ns := testConfig().Namespace
			if _, err := client.AppsV1().Deployments(ns).Get(context.Background(), "uzi-hw-w1", metav1.GetOptions{}); err != nil {
				t.Errorf("the skewed worker's deployment must be left exactly as it was: %v", err)
			}

			// And the rest of the fleet still reconciles.
			if _, err := client.AppsV1().Deployments(ns).Get(context.Background(), "uzi-hw-w2", metav1.GetOptions{}); err != nil {
				t.Errorf("the healthy worker must still be reconciled: %v", err)
			}
			assertNoSecretReads(t, client)
		})
	}
}

// A null join_token means "write no Secret for this worker" — NEVER "this worker
// has no token". Either a pod already proved it holds one (the cluster Secret is
// then the ONLY copy in existence) or the api's buffer expired unread.
//
// Asserted on the ACTION LOG, not on the absence of a diff: "no Secret call
// happened" and "a Secret call happened and changed nothing" are different, and
// only the first is the contract.
func TestANullJoinTokenTouchesTheSecretNotAtAll(t *testing.T) {
	m, client := newMat(t)
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", JoinToken: nil}

	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := secretVerbs(client); len(got) != 0 {
		t.Fatalf("a null join_token produced secret calls %v; it means WRITE NOTHING. "+
			"Clearing or recreating the Secret here would destroy the only copy of a delivered token "+
			"and strand a working worker.", got)
	}
	// The rest of the worker is still materialized: a null token is normal steady
	// state, not a reason to skip the worker.
	if _, err := client.AppsV1().Deployments(testConfig().Namespace).Get(context.Background(), "uzi-hw-w1", metav1.GetOptions{}); err != nil {
		t.Errorf("a null-token worker must still be materialized: %v", err)
	}
	assertNoSecretReads(t, client)
}

// Secret idempotence with no read available: issue the create, treat AlreadyExists
// as success. That is the whole of it.
func TestASecondReconcileTreatsAlreadyExistsAsSuccess(t *testing.T) {
	m, client := newMat(t)
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", JoinToken: token("uzw_secret")}

	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	observed, err := m.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, observed); err != nil {
		t.Fatalf("second Reconcile must tolerate AlreadyExists: %v", err)
	}
	if got := secretVerbs(client); len(got) != 2 || got[0] != "create" || got[1] != "create" {
		t.Errorf("secret verbs = %v, want two creates (the second absorbed as AlreadyExists)", got)
	}
	assertNoSecretReads(t, client)
}

// Decision 11: a worker we provisioned that the api no longer wants is torn down —
// every object, BY NAME. Deleting the Secret by name is what keeps the no-list rule
// affordable: the name is reachable from the id we recovered off the Deployment's
// labels, so no enumeration is ever needed.
func TestTeardownRemovesEveryObjectOfAWorkerTheAPIDropped(t *testing.T) {
	m, client := newMat(t, deployedWorker("gone", 0, "h"), pvcFor("gone", "data"), pvcFor("gone", "nix"))
	observed := []reconcile.ObservedWorker{{ID: "gone", HasDeployment: true, SpecHash: "h", HasDataPVC: true, HasNixPVC: true}}

	if err := m.Reconcile(context.Background(), nil, observed); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	deleted := map[string]bool{}
	for _, a := range client.Actions() {
		if d, ok := a.(k8stesting.DeleteAction); ok {
			deleted[a.GetResource().Resource+"/"+d.GetName()] = true
		}
	}
	for _, want := range []string{
		"deployments/uzi-hw-gone",
		"secrets/uzi-hw-gone-token",
		"persistentvolumeclaims/uzi-hw-gone-data",
		"persistentvolumeclaims/uzi-hw-gone-nix",
	} {
		if !deleted[want] {
			t.Errorf("%s was not deleted; got %v", want, keysOf(deleted))
		}
	}
	assertNoSecretReads(t, client)
}

// Decision 9 vs Decision 11, settled by PROVENANCE rather than by a name prefix. An
// object named uzi-hw-* that we did NOT stamp is flagged and otherwise untouched:
// "the controller never takes ownership of an object it did not stamp".
func TestAnUnstampedUziHwObjectIsFlaggedAndNeverTouched(t *testing.T) {
	// Named like ours, no managed-by stamp: a hand-made object, a leftover from an
	// older label scheme, or a partial create from a crashed reconcile.
	orphan := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      "uzi-hw-handmade",
		Namespace: testConfig().Namespace,
		Labels:    map[string]string{"app": "someone-elses"},
	}}
	var logs strings.Builder
	client := fake.NewSimpleClientset(orphan)
	m := New(client, testConfig(), testResolver(t), slog.New(slog.NewTextHandler(&logs, nil)))

	observed, err := m.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	// Orphans deliberately never cross the Materializer interface: they are never
	// acted on, so giving Reconcile the power to see them is power it should not have.
	if len(observed) != 0 {
		t.Fatalf("observed = %+v; an orphan must never be returned as one of ours", observed)
	}
	if !strings.Contains(logs.String(), "orphan") {
		t.Errorf("an orphan must be logged; got:\n%s", logs.String())
	}

	if err := m.Reconcile(context.Background(), nil, observed); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "delete" {
			t.Fatal("an orphan must NEVER be deleted — flagged only")
		}
	}
}

func TestObservePartitionsByProvenanceAndReadsTheDriftSignals(t *testing.T) {
	m, _ := newMat(t,
		deployedWorker("w1", 5, "hash-1"),
		pvcFor("w1", "data"),
		// w2 has a PVC but no Deployment: a crashed reconcile. It must still be
		// observed, which is what lets a Deployment-less leftover PVC be torn down.
		pvcFor("w2", "nix"),
	)
	observed, err := m.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	byID := map[string]reconcile.ObservedWorker{}
	for _, o := range observed {
		byID[o.ID] = o
	}
	if len(byID) != 2 {
		t.Fatalf("observed %d workers, want 2 (a PVC alone must enter the set)", len(byID))
	}
	w1 := byID["w1"]
	if !w1.HasDeployment || w1.Generation != 5 || w1.SpecHash != "hash-1" || !w1.HasDataPVC || w1.HasNixPVC {
		t.Errorf("w1 = %+v", w1)
	}
	w2 := byID["w2"]
	if w2.HasDeployment || w2.Generation != 0 || w2.SpecHash != "" || w2.HasDataPVC || !w2.HasNixPVC {
		t.Errorf("w2 = %+v", w2)
	}
}

// Drift source 1: the generation. INERT IN v1 — nothing can change it, since M2
// ships no rotation path and every worker is provisioned at 0. This test is the
// ONLY place the roll is exercised.
func TestAGenerationChangeRollsTheDeploymentAndReadsNoSecret(t *testing.T) {
	m, client := newMat(t, deployedWorker("w1", 1, currentHash(t, "w1", 1)))
	observed := []reconcile.ObservedWorker{{ID: "w1", HasDeployment: true, Generation: 1, SpecHash: currentHash(t, "w1", 1)}}

	rotated := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Generation: 2}
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{rotated}, observed); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	patch := findPatch(t, client)
	if !strings.Contains(patch, `"`+AnnotationGeneration+`":"2"`) {
		t.Errorf("the patch must carry the new generation in the pod template's annotations; got:\n%s", patch)
	}
	assertNoSecretReads(t, client)
}

// Drift source 2: the spec hash. This is the one a generation can never catch — a
// new release changes the agent image tag, which the controller resolves from its
// OWN config, so the generation does not move.
func TestASpecHashChangeRollsTheDeploymentAndReadsNoSecret(t *testing.T) {
	m, client := newMat(t, deployedWorker("w1", 0, "the-old-releases-hash"))
	observed := []reconcile.ObservedWorker{{ID: "w1", HasDeployment: true, Generation: 0, SpecHash: "the-old-releases-hash"}}

	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Generation: 0}
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, observed); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	patch := findPatch(t, client)
	if !strings.Contains(patch, AnnotationSpecHash) {
		t.Errorf("the patch must restamp the spec hash; got:\n%s", patch)
	}
	assertNoSecretReads(t, client)
}

// The steady state: nothing drifted, so nothing is patched. If this breaks, every
// reconcile rolls every pod forever.
func TestNoDriftMeansNoPatch(t *testing.T) {
	hash := currentHash(t, "w1", 0)
	m, client := newMat(t, deployedWorker("w1", 0, hash))
	observed := []reconcile.ObservedWorker{{ID: "w1", HasDeployment: true, Generation: 0, SpecHash: hash}}

	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Generation: 0}
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, observed); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "patch" {
			t.Fatal("an undrifted worker was patched: every reconcile would roll every pod forever")
		}
	}
}

// Fleet-wide, across every test path in this package: no Secret get/list, ever.
func TestNoSecretReadAcrossAFullFleetReconcile(t *testing.T) {
	m, client := newMat(t,
		deployedWorker("keep", 0, "h"),
		deployedWorker("drop", 0, "h"),
		pvcFor("drop", "data"),
	)
	observed := []reconcile.ObservedWorker{
		{ID: "keep", HasDeployment: true, SpecHash: "h"},
		{ID: "drop", HasDeployment: true, SpecHash: "h", HasDataPVC: true},
	}
	desired := []protocol.DesiredWorker{
		{ID: "keep", Template: "base", Size: "m", JoinToken: token("uzw_a")},
		{ID: "new", Template: "jvm", Size: "l", JoinToken: token("uzw_b")},
		{ID: "skew", Template: "base", Size: "xxl"},
	}
	if err := m.Reconcile(context.Background(), desired, observed); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertNoSecretReads(t, client)

	// Only the dropped worker is torn down.
	var deletedDeployments []string
	for _, a := range client.Actions() {
		if d, ok := a.(k8stesting.DeleteAction); ok && a.GetResource().Resource == "deployments" {
			deletedDeployments = append(deletedDeployments, d.GetName())
		}
	}
	if len(deletedDeployments) != 1 || deletedDeployments[0] != "uzi-hw-drop" {
		t.Errorf("deleted deployments = %v, want only uzi-hw-drop", deletedDeployments)
	}
}

// --- two-namespace / docker tier (PRD #83 M3) ------------------------------

func newDockerMat(t *testing.T, objs ...runtime.Object) (*Materializer, *fake.Clientset) {
	t.Helper()
	client := fake.NewSimpleClientset(objs...)
	m := New(client, dockerTestConfig(), testResolver(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return m, client
}

func deployedWorkerNS(id, ns string, generation int64, hash string) *appsv1.Deployment {
	d := deployedWorker(id, generation, hash)
	d.Namespace = ns
	return d
}

func pvcForNS(id, kind, ns string) *corev1.PersistentVolumeClaim {
	p := pvcFor(id, kind)
	p.Namespace = ns
	return p
}

// A docker worker and a plain one reconcile into SEPARATE namespaces — the whole
// point of the docker tier (Decision 7 / Q-B): the privileged namespace's blast
// radius never touches the restricted default. If they landed together the isolation
// would be a comment, not a fact.
func TestReconcilePlacesDockerAndPlainWorkersInSeparateNamespaces(t *testing.T) {
	m, client := newDockerMat(t)
	ctx := context.Background()
	plain := protocol.DesiredWorker{ID: "p1", Template: "base", Size: "m", JoinToken: token("uzw_p")}
	dock := protocol.DesiredWorker{ID: "d1", Template: "base", Size: "m", Docker: true, JoinToken: token("uzw_d")}

	if err := m.Reconcile(ctx, []protocol.DesiredWorker{plain, dock}, nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Plain worker: restricted default.
	if _, err := client.AppsV1().Deployments("uzi-workers").Get(ctx, "uzi-hw-p1", metav1.GetOptions{}); err != nil {
		t.Errorf("plain worker deployment not in the restricted namespace: %v", err)
	}
	// Docker worker: privileged namespace — deployment, secret, both PVCs.
	if _, err := client.AppsV1().Deployments("uzi-workers-docker").Get(ctx, "uzi-hw-d1", metav1.GetOptions{}); err != nil {
		t.Errorf("docker worker deployment not in the docker namespace: %v", err)
	}
	for _, name := range []string{"uzi-hw-d1-data", "uzi-hw-d1-nix"} {
		if _, err := client.CoreV1().PersistentVolumeClaims("uzi-workers-docker").Get(ctx, name, metav1.GetOptions{}); err != nil {
			t.Errorf("docker worker pvc %s not in the docker namespace: %v", name, err)
		}
	}
	// And the docker worker must NOT have leaked into the restricted namespace.
	if _, err := client.AppsV1().Deployments("uzi-workers").Get(ctx, "uzi-hw-d1", metav1.GetOptions{}); err == nil {
		t.Error("the docker worker's deployment leaked into the restricted namespace — the tiers are not isolated")
	}
	assertNoSecretReads(t, client)
}

// Teardown of a dropped worker targets the namespace it was OBSERVED in — a docker
// worker's objects live in the docker namespace, and its Docker flag is gone from the
// desired set, so the observation's namespace is the only handle on where to delete.
func TestObserveStampsNamespaceAndTeardownTargetsIt(t *testing.T) {
	m, client := newDockerMat(t,
		deployedWorkerNS("d1", "uzi-workers-docker", 0, "h"),
		pvcForNS("d1", "data", "uzi-workers-docker"),
		pvcForNS("d1", "nix", "uzi-workers-docker"),
	)
	ctx := context.Background()

	observed, err := m.Observe(ctx)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(observed) != 1 || observed[0].Namespace != "uzi-workers-docker" {
		t.Fatalf("observed = %+v, want one worker stamped with the docker namespace", observed)
	}

	// The api no longer wants it: tear down, in the docker namespace.
	if err := m.Reconcile(ctx, nil, observed); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var deletedNS []string
	for _, a := range client.Actions() {
		if d, ok := a.(k8stesting.DeleteAction); ok {
			deletedNS = append(deletedNS, d.GetNamespace())
		}
	}
	if len(deletedNS) == 0 {
		t.Fatal("nothing was torn down for a dropped docker worker")
	}
	for _, ns := range deletedNS {
		if ns != "uzi-workers-docker" {
			t.Errorf("a delete targeted namespace %q, want uzi-workers-docker — teardown hit the wrong namespace", ns)
		}
	}
	assertNoSecretReads(t, client)
}

// An orphan in the DOCKER namespace is flagged there and never touched — the
// provenance model holds per-namespace, so the privileged tier gets the same
// "flag, never adopt or delete" treatment as the restricted default.
func TestOrphanInDockerNamespaceIsFlaggedNotDeleted(t *testing.T) {
	orphan := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      "uzi-hw-handmade",
		Namespace: "uzi-workers-docker",
		Labels:    map[string]string{"app": "someone-elses"},
	}}
	var logs strings.Builder
	client := fake.NewSimpleClientset(orphan)
	m := New(client, dockerTestConfig(), testResolver(t), slog.New(slog.NewTextHandler(&logs, nil)))
	ctx := context.Background()

	observed, err := m.Observe(ctx)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(observed) != 0 {
		t.Fatalf("observed = %+v; an orphan must never be returned as one of ours", observed)
	}
	if !strings.Contains(logs.String(), "orphan") || !strings.Contains(logs.String(), "uzi-workers-docker") {
		t.Errorf("an orphan in the docker namespace must be logged with that namespace; got:\n%s", logs.String())
	}
	if err := m.Reconcile(ctx, nil, observed); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "delete" {
			t.Fatal("an orphan must NEVER be deleted — flagged only")
		}
	}
}

// A docker worker on a controller with NO docker namespace configured is SKIPPED with
// a loud error, never rendered into the restricted default. This is the reconcile
// half of config.go's "no silent default": a privileged pod must never land in the
// restricted namespace because a knob was unset. The worker stays desired (never torn
// down), so it materializes the moment the controller is configured.
func TestDockerWorkerSkippedWhenDockerNamespaceUnconfigured(t *testing.T) {
	m, client := newMat(t) // testConfig has NO docker namespace
	ctx := context.Background()
	dock := protocol.DesiredWorker{ID: "d1", Template: "base", Size: "m", Docker: true, JoinToken: token("uzw_d")}

	if err := m.Reconcile(ctx, []protocol.DesiredWorker{dock}, nil); err != nil {
		t.Fatalf("Reconcile must tolerate an unconfigured docker worker, got: %v", err)
	}
	for _, a := range client.Actions() {
		if v := a.GetVerb(); v == "create" || v == "delete" || v == "patch" {
			t.Fatalf("a %q on %s reached the apiserver for a docker worker with no docker namespace configured — "+
				"it must be skipped, never rendered into the restricted default", v, a.GetResource().Resource)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func findPatch(t *testing.T, client *fake.Clientset) string {
	t.Helper()
	for _, a := range client.Actions() {
		if p, ok := a.(k8stesting.PatchAction); ok {
			return string(p.GetPatch())
		}
	}
	t.Fatal("no patch was issued, but the worker drifted")
	return ""
}

// currentHash is what the renderer WOULD stamp for a worker, so a test can set up
// an undrifted starting state.
func currentHash(t *testing.T, id string, generation int64) string {
	t.Helper()
	w := protocol.DesiredWorker{ID: id, Template: "base", Size: "m", Generation: generation}
	spec, err := testResolver(t).Resolve(w.Template, w.Size)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return SpecHashOf(testConfig(), w, spec)
}

func deployedWorker(id string, generation int64, hash string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName(id),
			Namespace: testConfig().Namespace,
			Labels:    objectLabels(id),
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: objectLabels(id),
					Annotations: map[string]string{
						AnnotationGeneration: strconv.FormatInt(generation, 10),
						AnnotationSpecHash:   hash,
					},
				},
			},
		},
	}
}

func pvcFor(id, kind string) *corev1.PersistentVolumeClaim {
	name := dataPVCName(id)
	if kind == "nix" {
		name = nixPVCName(id)
	}
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: testConfig().Namespace,
		Labels:    objectLabels(id),
	}}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
