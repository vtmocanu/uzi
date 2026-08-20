package kube

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

func newMat(t *testing.T, objs ...runtime.Object) (*Materializer, *fake.Clientset) {
	t.Helper()
	client := fake.NewSimpleClientset(objs...)
	m := New(client, testConfig(), testResolver(t), nil, DrainPolicy{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

// BUG 3 (issue #114): the PVC create is GATED on the observed flag, exactly as the
// Deployment is gated on obs.HasDeployment. The bug was an UNCONDITIONAL create every
// ~10s reconcile tick: k8s charges the ResourceQuota's used.requests.storage at
// admission for each create and does NOT decrement it when storage then rejects the
// create AlreadyExists (upstream #119593), so the redundant charges out-raced the
// quota-controller resync and pinned the quota at its hard limit until genuinely-new
// workers were refused their PVCs. Given both PVCs already observed, a reconcile must
// issue ZERO pvc creates.
//
// Positive control: delete the obs.HasDataPVC/HasNixPVC gate in reconcileWorker and this
// goes RED (two pvc creates reach the apiserver).
func TestReconcileDoesNotRecreateAlreadyObservedPVCs(t *testing.T) {
	existing := deployedWorker("w1", 0, "h")
	m, client := newMat(t, existing, pvcFor("w1", "data"), pvcFor("w1", "nix"))

	// Steady state: the pod already holds its token, so join_token is nil (write no
	// Secret). The PVCs and Deployment were seen by the last Observe.
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", JoinToken: nil}
	observed := []reconcile.ObservedWorker{{ID: "w1", HasDeployment: true, SpecHash: "h", HasDataPVC: true, HasNixPVC: true}}

	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, observed); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var pvcCreates []string
	for _, a := range client.Actions() {
		if a.GetResource().Resource != "persistentvolumeclaims" || a.GetVerb() != "create" {
			continue
		}
		c, ok := a.(k8stesting.CreateAction)
		if !ok {
			t.Fatalf("a create verb on persistentvolumeclaims was not a CreateAction: %T", a)
		}
		pvcCreates = append(pvcCreates, c.GetObject().(*corev1.PersistentVolumeClaim).Name)
	}
	if len(pvcCreates) != 0 {
		t.Fatalf("a reconcile of a worker whose PVCs are already observed issued pvc create(s) %v; want ZERO. "+
			"An unconditional create every tick re-charges the ResourceQuota (used.requests.storage) at admission "+
			"and k8s never decrements it on the AlreadyExists rejection (upstream #119593), pinning the quota at its "+
			"hard limit until new workers are refused. The create must be gated on obs.HasDataPVC/HasNixPVC.", pvcCreates)
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
	m := New(client, testConfig(), testResolver(t), nil, DrainPolicy{}, slog.New(slog.NewTextHandler(&logs, nil)))

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
	m := New(client, dockerTestConfig(), testResolver(t), nil, DrainPolicy{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return m, client
}

// pvcCreateNames lists, in order, the PVC names a reconcile actually asked the
// apiserver to create. Reading the NAMES rather than a count is what lets the tests
// below say WHICH claim was wrong, and a count alone could not tell "the dind claim
// was skipped" from "the data claim was skipped".
func pvcCreateNames(t *testing.T, client *fake.Clientset) []string {
	t.Helper()
	var out []string
	for _, a := range client.Actions() {
		if a.GetResource().Resource != "persistentvolumeclaims" || a.GetVerb() != "create" {
			continue
		}
		c, ok := a.(k8stesting.CreateAction)
		if !ok {
			t.Fatalf("a create verb on persistentvolumeclaims was not a CreateAction: %T", a)
		}
		out = append(out, c.GetObject().(*corev1.PersistentVolumeClaim).Name)
	}
	return out
}

// Issue #224 M-a, the materializer half: a docker worker gets a THIRD claim created,
// a plain one does not, and once observed neither is re-created.
//
// The create-gate is not tidiness. Issue #114 BUG 3: an unconditional create every
// ~10s tick charges the ResourceQuota's used.requests.storage at admission, and k8s
// does NOT decrement it when storage then rejects the create AlreadyExists (upstream
// #119593) — the redundant charges pin the quota at its hard limit and genuinely-new
// workers are refused their PVCs. A third claim is a third source of that charge.
func TestDinDDataPVCIsCreatedForDockerWorkersOnlyAndGatedOnObservation(t *testing.T) {
	ctx := context.Background()
	dockerNS := dockerTestConfig().DockerNamespace

	// Fresh docker worker: all three claims created.
	m, client := newDockerMat(t)
	dock := protocol.DesiredWorker{ID: "d1", Template: "base", Size: "m", Docker: true, JoinToken: token("uzw_d")}
	if err := m.Reconcile(ctx, []protocol.DesiredWorker{dock}, nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got, want := pvcCreateNames(t, client), []string{"uzi-hw-d1-data", "uzi-hw-d1-nix", "uzi-hw-d1-dind-data"}; !equalStrings(got, want) {
		t.Fatalf("docker worker pvc creates = %v, want %v", got, want)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(dockerNS).Get(ctx, "uzi-hw-d1-dind-data", metav1.GetOptions{}); err != nil {
		t.Errorf("dind-data pvc not created in the docker namespace: %v", err)
	}

	// Fresh PLAIN worker on the same (docker-capable) controller: two claims, no third.
	m, client = newDockerMat(t)
	plain := protocol.DesiredWorker{ID: "p1", Template: "base", Size: "m", JoinToken: token("uzw_p")}
	if err := m.Reconcile(ctx, []protocol.DesiredWorker{plain}, nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got, want := pvcCreateNames(t, client), []string{"uzi-hw-p1-data", "uzi-hw-p1-nix"}; !equalStrings(got, want) {
		t.Fatalf("plain worker pvc creates = %v, want %v — the dind claim is docker-only", got, want)
	}

	// Steady state: all three observed, so ZERO creates. Positive control: drop the
	// obs.HasDinDDataPVC arm from reconcileWorker's switch and this goes RED with one
	// create per tick, forever.
	m, client = newDockerMat(t,
		deployedWorkerNS("d1", dockerNS, 0, "h"),
		pvcForNS("d1", "data", dockerNS), pvcForNS("d1", "nix", dockerNS), pvcForNS("d1", "dind-data", dockerNS),
	)
	steady := protocol.DesiredWorker{ID: "d1", Template: "base", Size: "m", Docker: true}
	observed := []reconcile.ObservedWorker{{
		ID: "d1", Namespace: dockerNS, HasDeployment: true, SpecHash: "h",
		HasDataPVC: true, HasNixPVC: true, HasDinDDataPVC: true,
	}}
	if err := m.Reconcile(ctx, []protocol.DesiredWorker{steady}, observed); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := pvcCreateNames(t, client); len(got) != 0 {
		t.Fatalf("a docker worker whose three PVCs are already observed issued creates %v; want ZERO "+
			"(issue #114 BUG 3: a redundant create is charged to used.requests.storage and never decremented)", got)
	}
	assertNoSecretReads(t, client)
}

// Observe must SET HasDinDDataPVC off the claim's name, or the gate above can never
// engage: an unobserved claim looks missing forever and is re-created every tick.
func TestObserveRecordsTheDinDDataPVC(t *testing.T) {
	dockerNS := dockerTestConfig().DockerNamespace
	m, _ := newDockerMat(t,
		deployedWorkerNS("d1", dockerNS, 3, "hash-1"),
		pvcForNS("d1", "data", dockerNS), pvcForNS("d1", "nix", dockerNS), pvcForNS("d1", "dind-data", dockerNS),
	)
	observed, err := m.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(observed) != 1 {
		t.Fatalf("observed %d workers, want 1", len(observed))
	}
	o := observed[0]
	if !o.HasDataPVC || !o.HasNixPVC || !o.HasDinDDataPVC {
		t.Fatalf("observed pvc flags = data:%v nix:%v dind:%v, want all true", o.HasDataPVC, o.HasNixPVC, o.HasDinDDataPVC)
	}
}

// Teardown removes the third claim too, and does so UNCONDITIONALLY — a dropped
// worker's Docker flag is no longer in the desired set to branch on, so the delete
// cannot be gated on it. For a plain worker the delete simply NotFounds, which this
// path already tolerates; leaving the claim behind instead would strand storage that
// nothing will ever reclaim, and it counts against the tier's requests.storage quota
// for as long as it exists.
func TestTeardownRemovesTheDinDDataPVC(t *testing.T) {
	dockerNS := dockerTestConfig().DockerNamespace
	m, client := newDockerMat(t,
		deployedWorkerNS("gone", dockerNS, 0, "h"),
		pvcForNS("gone", "data", dockerNS), pvcForNS("gone", "nix", dockerNS), pvcForNS("gone", "dind-data", dockerNS),
	)
	observed := []reconcile.ObservedWorker{{
		ID: "gone", Namespace: dockerNS, HasDeployment: true, SpecHash: "h",
		HasDataPVC: true, HasNixPVC: true, HasDinDDataPVC: true,
	}}
	if err := m.Reconcile(context.Background(), nil, observed); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	deleted := map[string]bool{}
	for _, a := range client.Actions() {
		if d, ok := a.(k8stesting.DeleteAction); ok {
			deleted[a.GetResource().Resource+"/"+d.GetName()] = true
		}
	}
	if !deleted["persistentvolumeclaims/uzi-hw-gone-dind-data"] {
		t.Errorf("the dind-data pvc was not deleted; got %v", keysOf(deleted))
	}
	assertNoSecretReads(t, client)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	m := New(client, dockerTestConfig(), testResolver(t), nil, DrainPolicy{}, slog.New(slog.NewTextHandler(&logs, nil)))
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
	switch kind {
	case "nix":
		name = nixPVCName(id)
	case "dind-data":
		name = dindDataPVCName(id)
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

// --- cordon / defer-roll on drift (PRD #422 M4) ----------------------------

// fakeCordoner records the worker ids handed to RequestDrain and can force the
// write to fail, so a test can prove the fail-safe defer path.
type fakeCordoner struct {
	calls []string
	err   error
}

func (f *fakeCordoner) RequestDrain(_ context.Context, workerID string) error {
	f.calls = append(f.calls, workerID)
	return f.err
}

// m5Now is the pinned clock for the drift/drain tests, so a deadline comparison
// (now - draining_since >= Deadline, PRD #422 M5) is deterministic rather than racing
// the wall clock.
var m5Now = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// newMatWithDrain builds a Materializer wired to a fake cordoner (nil disables
// cordoning) under an explicit DrainPolicy, with its clock pinned to m5Now so the
// deadline branch is deterministic.
func newMatWithDrain(t *testing.T, c Cordoner, drain DrainPolicy, objs ...runtime.Object) (*Materializer, *fake.Clientset) {
	t.Helper()
	client := fake.NewSimpleClientset(objs...)
	m := New(client, testConfig(), testResolver(t), c, drain, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.now = func() time.Time { return m5Now }
	return m, client
}

// newMatWithCordoner builds a Materializer wired to a fake cordoner (nil disables
// cordoning, exercising the no-channel path). It uses a GENEROUS 24h deadline and no
// force-roll, so the M4 defer tests below keep their meaning: a recently cordoned
// worker never trips the deadline, and only the dedicated M5 tests exercise a roll.
func newMatWithCordoner(t *testing.T, c Cordoner, objs ...runtime.Object) (*Materializer, *fake.Clientset) {
	t.Helper()
	return newMatWithDrain(t, c, DrainPolicy{Deadline: 24 * time.Hour}, objs...)
}

// recentDrain is a draining_since inside any reasonable deadline (one minute before the
// pinned clock) — a worker that was just cordoned.
func recentDrain() *time.Time { t := m5Now.Add(-time.Minute); return &t }

// wasPatched reports whether any Deployment patch reached the fake apiserver — the
// observable proof of a roll, and its absence the proof of a defer.
func wasPatched(client *fake.Clientset) bool {
	for _, a := range client.Actions() {
		if a.GetVerb() == "patch" {
			return true
		}
	}
	return false
}

// driftedWorker sets up a worker whose deployed spec hash no longer matches what the
// renderer would stamp, so it has drifted. Busy and drainingSince (nil == not draining)
// are set by the caller.
func driftedWorker(t *testing.T, busy bool, drainingSince *time.Time) (protocol.DesiredWorker, []reconcile.ObservedWorker, *appsv1.Deployment) {
	t.Helper()
	dep := deployedWorker("w1", 0, "the-old-releases-hash")
	obs := []reconcile.ObservedWorker{{ID: "w1", HasDeployment: true, Generation: 0, SpecHash: "the-old-releases-hash"}}
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Generation: 0, Busy: busy, DrainingSince: drainingSince}
	return w, obs, dep
}

// drift + busy + not draining ⇒ cordon requested once, roll deferred (no patch).
func TestDriftBusyNotDrainingCordonsAndDefers(t *testing.T) {
	c := &fakeCordoner{}
	w, obs, dep := driftedWorker(t, true, nil)
	m, client := newMatWithCordoner(t, c, dep)
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(c.calls) != 1 || c.calls[0] != "w1" {
		t.Fatalf("RequestDrain calls = %v, want exactly [w1]", c.calls)
	}
	if wasPatched(client) {
		t.Fatal("a busy worker was rolled; it must be cordoned and deferred, never hard-killed")
	}
}

// drift + busy + already draining (recently, within the deadline) ⇒ no re-cordon, still
// deferred (no patch).
func TestDriftBusyAlreadyDrainingDefersWithoutRecordon(t *testing.T) {
	c := &fakeCordoner{}
	w, obs, dep := driftedWorker(t, true, recentDrain())
	m, client := newMatWithCordoner(t, c, dep)
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(c.calls) != 0 {
		t.Fatalf("RequestDrain calls = %v, want none (already draining)", c.calls)
	}
	if wasPatched(client) {
		t.Fatal("an already-draining busy worker was rolled; it must be deferred")
	}
}

// drift + idle ⇒ no cordon, rolled (patch issued). Covers cordoned-then-idle too:
// a later tick where busy flipped false is exactly this case.
func TestDriftIdleRollsWithoutCordon(t *testing.T) {
	c := &fakeCordoner{}
	w, obs, dep := driftedWorker(t, false, nil)
	m, client := newMatWithCordoner(t, c, dep)
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(c.calls) != 0 {
		t.Fatalf("RequestDrain calls = %v, want none for an idle worker", c.calls)
	}
	if !wasPatched(client) {
		t.Fatal("an idle drifted worker was not rolled")
	}
}

// A cordoned worker that has since gone idle rolls on the next tick even though it
// is still flagged draining — the roll gate is idleness, not the draining flag.
func TestDriftIdleWhileDrainingStillRolls(t *testing.T) {
	c := &fakeCordoner{}
	w, obs, dep := driftedWorker(t, false, recentDrain())
	m, client := newMatWithCordoner(t, c, dep)
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(c.calls) != 0 {
		t.Fatalf("RequestDrain calls = %v, want none (already idle)", c.calls)
	}
	if !wasPatched(client) {
		t.Fatal("a now-idle drifted worker was not rolled")
	}
}

// drift + busy + cordon write fails ⇒ cordon attempted, roll still deferred
// (fail-safe), no panic.
func TestDriftBusyCordonErrorStillDefers(t *testing.T) {
	c := &fakeCordoner{err: errors.New("api unreachable")}
	w, obs, dep := driftedWorker(t, true, nil)
	m, client := newMatWithCordoner(t, c, dep)
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(c.calls) != 1 {
		t.Fatalf("RequestDrain calls = %v, want exactly one attempt", c.calls)
	}
	if wasPatched(client) {
		t.Fatal("a busy worker was rolled after a failed cordon; the defer must be fail-safe")
	}
}

// drift + busy + nil cordoner ⇒ roll deferred (no patch), no panic.
func TestDriftBusyNilCordonerDefers(t *testing.T) {
	w, obs, dep := driftedWorker(t, true, nil)
	m, client := newMatWithCordoner(t, nil, dep)
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if wasPatched(client) {
		t.Fatal("a busy worker was rolled with no cordon channel; it must be deferred")
	}
}

// no drift + busy ⇒ nothing happens: no cordon, no patch. Busy alone must never
// trigger a cordon; only drift does.
func TestNoDriftBusyDoesNothing(t *testing.T) {
	c := &fakeCordoner{}
	hash := currentHash(t, "w1", 0)
	dep := deployedWorker("w1", 0, hash)
	obs := []reconcile.ObservedWorker{{ID: "w1", HasDeployment: true, Generation: 0, SpecHash: hash}}
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Generation: 0, Busy: true}
	m, client := newMatWithCordoner(t, c, dep)
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(c.calls) != 0 {
		t.Fatalf("RequestDrain calls = %v, want none (no drift)", c.calls)
	}
	if wasPatched(client) {
		t.Fatal("an undrifted worker was patched")
	}
}

// --- bounded drain deadline + force-roll override (PRD #422 M5) -------------

// drift + busy + draining RECENTLY (now - since < Deadline) + ForceRoll=false ⇒ the
// deadline has not elapsed, so the worker is still deferred: no roll, and no re-cordon
// (it is already draining).
func TestDriftBusyWithinDeadlineDefers(t *testing.T) {
	c := &fakeCordoner{}
	w, obs, dep := driftedWorker(t, true, recentDrain()) // one minute ago
	m, client := newMatWithDrain(t, c, DrainPolicy{Deadline: 24 * time.Hour}, dep)
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if wasPatched(client) {
		t.Fatal("a busy worker within its drain deadline was rolled; it must stay deferred")
	}
	if len(c.calls) != 0 {
		t.Fatalf("RequestDrain calls = %v, want none (already draining, within deadline)", c.calls)
	}
}

// drift + busy + draining LONGER than the deadline (now - since >= Deadline) ⇒ the
// controller rolls it anyway (its run requeues), and issues NO new cordon (already
// draining).
func TestDriftBusyDeadlineExceededRolls(t *testing.T) {
	c := &fakeCordoner{}
	since := m5Now.Add(-2 * time.Hour) // draining for 2h ...
	w, obs, dep := driftedWorker(t, true, &since)
	m, client := newMatWithDrain(t, c, DrainPolicy{Deadline: time.Hour}, dep) // ... against a 1h deadline
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !wasPatched(client) {
		t.Fatal("a busy worker past its drain deadline was not rolled; the deadline must force the roll")
	}
	if len(c.calls) != 0 {
		t.Fatalf("RequestDrain calls = %v, want none (already draining; no re-cordon on the deadline roll)", c.calls)
	}
}

// drift + busy + ForceRoll=true (DrainingSince nil, never cordoned) ⇒ rolled immediately,
// with NO cordon: the emergency override skips the cordon-and-defer path entirely.
func TestDriftBusyForceRollRollsImmediatelyWithoutCordon(t *testing.T) {
	c := &fakeCordoner{}
	w, obs, dep := driftedWorker(t, true, nil)
	m, client := newMatWithDrain(t, c, DrainPolicy{Deadline: 24 * time.Hour, ForceRoll: true}, dep)
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !wasPatched(client) {
		t.Fatal("force-roll did not roll a busy drifted worker")
	}
	if len(c.calls) != 0 {
		t.Fatalf("RequestDrain calls = %v, want none (force-roll skips cordoning)", c.calls)
	}
}

// drift + busy + ForceRoll=true + already draining ⇒ still rolled (no re-cordon).
func TestDriftBusyForceRollRollsAlreadyDraining(t *testing.T) {
	c := &fakeCordoner{}
	w, obs, dep := driftedWorker(t, true, recentDrain())
	m, client := newMatWithDrain(t, c, DrainPolicy{Deadline: 24 * time.Hour, ForceRoll: true}, dep)
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !wasPatched(client) {
		t.Fatal("force-roll did not roll an already-draining busy worker")
	}
	if len(c.calls) != 0 {
		t.Fatalf("RequestDrain calls = %v, want none (already draining under force-roll)", c.calls)
	}
}

// drift + IDLE under an aggressive policy (short deadline, force-roll on) ⇒ rolled with
// no cordon, exactly as any idle drift: the overrides only concern busy workers.
func TestDriftIdleRollsUnderAnyPolicy(t *testing.T) {
	c := &fakeCordoner{}
	w, obs, dep := driftedWorker(t, false, nil)
	m, client := newMatWithDrain(t, c, DrainPolicy{Deadline: time.Nanosecond, ForceRoll: true}, dep)
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !wasPatched(client) {
		t.Fatal("an idle drifted worker was not rolled")
	}
	if len(c.calls) != 0 {
		t.Fatalf("RequestDrain calls = %v, want none for an idle worker", c.calls)
	}
}

// NO drift + ForceRoll=true ⇒ NOTHING happens: force-roll only accelerates a PENDING
// roll (a drifted worker), it never invents one. An undrifted worker returns before the
// drift block is reached.
func TestNoDriftForceRollDoesNothing(t *testing.T) {
	c := &fakeCordoner{}
	hash := currentHash(t, "w1", 0)
	dep := deployedWorker("w1", 0, hash)
	obs := []reconcile.ObservedWorker{{ID: "w1", HasDeployment: true, Generation: 0, SpecHash: hash}}
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Generation: 0, Busy: true}
	m, client := newMatWithDrain(t, c, DrainPolicy{Deadline: time.Nanosecond, ForceRoll: true}, dep)
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if wasPatched(client) {
		t.Fatal("force-roll rolled an UNDRIFTED worker; it must only accelerate a pending roll, never invent one")
	}
	if len(c.calls) != 0 {
		t.Fatalf("RequestDrain calls = %v, want none (no drift)", c.calls)
	}
}
