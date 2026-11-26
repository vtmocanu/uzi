package kube

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// A worker whose join token arrives after its claims were created (the api's buffer
// delivered it late) gets its Secret written on a later tick than its /data claim. The
// claim is then already live and unstamped, so the confirmed Secret write has to stamp it
// with a patch (stampCodexCAClaim): the next observation would otherwise retire the carry
// and UZI_CODEX_CA_FILE would never be rendered although the Secret holds the key.

// caStampPatchCount counts PVC patches on the worker's data claim that carry the CA stamp.
func caStampPatchCount(c *fake.Clientset, id string) int {
	n := 0
	for _, a := range c.Actions() {
		p, ok := a.(k8stesting.PatchAction)
		if ok && a.GetResource().Resource == "persistentvolumeclaims" && p.GetName() == dataPVCName(id) &&
			strings.Contains(string(p.GetPatch()), AnnotationCodexExtraCA) {
			n++
		}
	}
	return n
}

// caIsStampPatch matches the stamp patch on the data claim for caIntercept.
func caIsStampPatch(a k8stesting.Action) bool {
	p, ok := a.(k8stesting.PatchAction)
	return ok && p.GetName() == dataPVCName("w1") && strings.Contains(string(p.GetPatch()), AnnotationCodexExtraCA)
}

// caLateStart runs tick 1 of a tokenless worker (claims and Deployment created, no Secret,
// no env) and returns the worker as it looks once its token arrives.
func caLateStart(t *testing.T, ephemeral bool) (*Materializer, *fake.Clientset, protocol.DesiredWorker) {
	t.Helper()
	cfg := caCfg()
	ns := cfg.Namespace
	m, c := caMat(t, cfg)
	w := caWorker("w1")
	w.Ephemeral = ephemeral
	tokenless := w
	tokenless.JoinToken = nil
	caMustTick(t, m, tokenless)
	if caSecretActions(c) != 0 {
		t.Fatal("fixture: a Secret was written without a token")
	}
	if p := caClaim(t, c, ns, dataPVCName("w1")); p == nil || caStamped(p) {
		t.Fatalf("fixture: data claim = %v, want live and unstamped", p)
	}
	if d := caDep(t, c, ns, "w1"); d == nil || caHasEnv(d) {
		t.Fatalf("fixture: deployment = %v, want created without the env", d)
	}
	return m, c, w
}

// caNoEnvAfter ticks n more times and requires the claim unstamped, the Deployment without
// the env and no further stamp patch.
func caNoEnvAfter(t *testing.T, m *Materializer, c *fake.Clientset, w protocol.DesiredWorker, n int) {
	t.Helper()
	ns := m.cfg.Namespace
	for i := 0; i < n; i++ {
		caMustTick(t, m, w)
	}
	if caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Error("data claim is stamped")
	}
	if d := caDep(t, c, ns, "w1"); d == nil || caHasEnv(d) {
		t.Errorf("deployment = %v, want present without the env", d)
	}
	if n := caStampPatchCount(c, "w1"); n != 1 {
		t.Errorf("stamp patches = %d, want only the single failed attempt", n)
	}
}

// R1: a late token on a worker outside the roll pools. The token tick stamps the existing
// claim (same UID, other annotations kept) and writes no Deployment template or replicas;
// the next tick renders the env.
func TestCodexCALateTokenStampsTheExistingClaim(t *testing.T) {
	m, c, w := caLateStart(t, true)
	ns := m.cfg.Namespace
	caSetClaim(t, c, ns, dataPVCName("w1"), func(p *corev1.PersistentVolumeClaim) {
		p.Annotations = map[string]string{AnnotationPlacement: placementNew, annotationRoll: "keep-roll"}
	})
	before := caClaim(t, c, ns, dataPVCName("w1"))

	c.ClearActions()
	caMustTick(t, m, w)
	s, err := c.CoreV1().Secrets(ns).Get(context.Background(), secretName("w1"), metav1.GetOptions{})
	if err != nil || len(s.Data[codexExtraCACertKey]) == 0 {
		t.Fatalf("secret = %v, %v; want the codex key", s, err)
	}
	if n := caStampPatchCount(c, "w1"); n != 1 {
		t.Fatalf("stamp patches = %d, want 1", n)
	}
	after := caClaim(t, c, ns, dataPVCName("w1"))
	if !caStamped(after) || after.UID != before.UID {
		t.Fatalf("claim stamped=%v uid %q -> %q", caStamped(after), before.UID, after.UID)
	}
	if after.Annotations[AnnotationPlacement] != placementNew || after.Annotations[annotationRoll] != "keep-roll" {
		t.Errorf("annotations = %v, want the placement and roll annotations preserved", after.Annotations)
	}
	if got := caDeploymentWrites(c); len(got) != 0 {
		t.Errorf("token tick wrote deployments: %v", got)
	}

	caMustTick(t, m, w)
	if d := caDep(t, c, ns, "w1"); d == nil || !caHasEnv(d) {
		t.Fatalf("tick 3: deployment = %v, want the env", d)
	}
}

// R2: the same on a pooled (roll-governed) worker with unrelated drift. The token tick makes
// no template or replica write; the target rendered from the next observation carries the
// env, and it reaches the Deployment through the ordinary roll.
func TestCodexCALateTokenOnAPooledWorkerRendersTheEnvAfterTheStamp(t *testing.T) {
	m, c, workers, _ := matrixFixture(t, 3)
	m.cfg.UIDSplit, m.cfg.APICAPEM, m.cfg.ExtraCAPEM = true, []byte("api-ca"), []byte("extra-ca-A")
	m.cfg.RollConcurrency = 2
	caSeedUnstamped(t, c, "w1")
	w := &workers[0]
	w.JoinToken = token("uzw_w1")
	ns := m.cfg.Namespace

	c.ClearActions()
	caMustTick(t, m, workers...)
	if n := caStampPatchCount(c, "w1"); n != 1 {
		t.Fatalf("stamp patches = %d, want 1", n)
	}
	if !caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Fatal("claim is not stamped")
	}
	if got := templatePatches(c); len(got) != 0 {
		t.Errorf("token tick wrote a template or replicas: %v", got)
	}
	if replicaPatches(c) != 0 {
		t.Error("token tick patched replicas")
	}
	if d := caDep(t, c, ns, "w1"); caHasEnv(d) {
		t.Fatal("token tick already rendered the env")
	}

	obs := caObserve(t, m, "w1")
	spec, err := m.resolveWorker(*w)
	if err != nil {
		t.Fatal(err)
	}
	if d := RenderDeployment(m.renderCfgFor(context.Background(), *w, obs), *w, spec); !caHasEnv(d) {
		t.Fatal("the desired target of the next tick has no env")
	}
	for i := 0; i < 4 && !caHasEnv(caDep(t, c, ns, "w1")); i++ {
		caMustTick(t, m, workers...)
	}
	if d := caDep(t, c, ns, "w1"); !caHasEnv(d) {
		t.Fatal("the env never reached the Deployment through the roll")
	}
}

// R3: a stamp confirmed in the same pass as a retained recycle's claim delete. The
// retained Deployment's record is synced from the pass-local view, so it reads written
// before the UID-fenced delete, and a restart before the replacement exists still renders
// the env.
func TestCodexCALateTokenStampThenSamePassRetainedRecycle(t *testing.T) {
	m, c, w, cfg := caRetained(t, false, "", false)
	ns := cfg.Namespace
	w.JoinToken = token("uzw_w1")
	uid := caClaim(t, c, ns, dataPVCName("w1")).UID
	if caRecordPatches(c) != 0 {
		t.Fatal("fixture: a record patch exists")
	}
	caMustTick(t, m, w)

	// The recycle ran in the pass that stamped: stamp, then record, then the fenced delete.
	var order []string
	for _, a := range c.Actions() {
		switch {
		case caIsStampPatch(a):
			order = append(order, "stamp")
		case caIsRecordPatch(a):
			order = append(order, "record")
		case a.GetVerb() == "delete" && a.GetResource().Resource == "persistentvolumeclaims" &&
			a.(k8stesting.DeleteAction).GetName() == dataPVCName("w1"):
			order = append(order, "delete")
			pre := a.(k8stesting.DeleteAction).GetDeleteOptions().Preconditions
			if pre == nil || pre.UID == nil || *pre.UID != uid {
				t.Errorf("data delete precondition = %v, want the claim's UID %q", pre, uid)
			}
		}
	}
	if got := strings.Join(order, ","); got != "stamp,record,delete" {
		t.Fatalf("write order = %q, want stamp,record,delete", got)
	}
	if v, ok := caRecord(caDep(t, c, ns, "w1")); !ok || v != codexExtraCAWritten {
		t.Fatalf("deployment record = %q (present=%v), want written", v, ok)
	}

	m = matrixRestart(m)
	w.DiskPressure = false
	for i := 0; i < 4; i++ {
		caMustTick(t, m, w)
	}
	if !caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Error("replacement claim is not stamped after the restart")
	}
	obs := caObserve(t, m, "w1")
	spec, err := m.resolveWorker(w)
	if err != nil {
		t.Fatal(err)
	}
	if d := RenderDeployment(m.renderCfgFor(context.Background(), w, obs), w, spec); !caHasEnv(d) {
		t.Error("the recovered worker's target has no env")
	}
}

// R4: an earlier step of the tick refreshed the claim (its resourceVersion moved after the
// observation). The stamp fences on the roll entry's newer claim and succeeds.
func TestCodexCALateTokenStampUsesTheRollEntrysNewerClaim(t *testing.T) {
	m, c, workers, _ := matrixFixture(t, 3)
	m.cfg.UIDSplit, m.cfg.APICAPEM, m.cfg.ExtraCAPEM = true, []byte("api-ca"), []byte("extra-ca-A")
	m.cfg.RollConcurrency = 2
	caSeedUnstamped(t, c, "w1")
	workers[0].JoinToken = token("uzw_w1")
	ns := m.cfg.Namespace

	obs, err := m.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	caSetClaim(t, c, ns, dataPVCName("w1"), func(p *corev1.PersistentVolumeClaim) { p.ResourceVersion = "9" })
	c.ClearActions()
	if err := m.Reconcile(context.Background(), workers, obs); err != nil {
		t.Fatal(err)
	}
	if n := caStampPatchCount(c, "w1"); n != 1 {
		t.Fatalf("stamp patches = %d, want 1", n)
	}
	for _, a := range c.Actions() {
		if caIsStampPatch(a) && !strings.Contains(string(a.(k8stesting.PatchAction).GetPatch()), `"resourceVersion":"9"`) {
			t.Errorf("stamp patch %s does not fence on the newer resourceVersion", a.(k8stesting.PatchAction).GetPatch())
		}
	}
	if !caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Error("claim is not stamped")
	}
}

// caUnconfirmedCase is one way the stamp patch can go unconfirmed. After it the carry is
// gone and the tick reports the error without any later write.
type caUnconfirmedCase struct {
	name      string
	committed bool // the claim ends up stamped although the pass could not confirm it
	react     func(c *fake.Clientset, next func() (runtime.Object, error)) (runtime.Object, error)
}

func caUnconfirmedCases() []caUnconfirmedCase {
	clone := func(o runtime.Object, f func(*corev1.PersistentVolumeClaim)) runtime.Object {
		p := o.(*corev1.PersistentVolumeClaim).DeepCopy()
		f(p)
		return p
	}
	return []caUnconfirmedCase{
		{name: "rejected", react: func(c *fake.Clientset, next func() (runtime.Object, error)) (runtime.Object, error) {
			return nil, apierrors.NewForbidden(pvcGR, dataPVCName("w1"), errors.New("rejected"))
		}},
		{name: "stale-resource-version", react: func(c *fake.Clientset, next func() (runtime.Object, error)) (runtime.Object, error) {
			// A concurrent writer moved the claim: the real precondition rejects the patch.
			caBump(c, "w1")
			return next()
		}},
		{name: "wrong-namespace-ack", committed: true, react: func(c *fake.Clientset, next func() (runtime.Object, error)) (runtime.Object, error) {
			o, err := next()
			return clone(o, func(p *corev1.PersistentVolumeClaim) { p.Namespace = "elsewhere" }), err
		}},
		{name: "unchanged-resource-version-ack", committed: true, react: func(c *fake.Clientset, next func() (runtime.Object, error)) (runtime.Object, error) {
			cur, _ := c.Tracker().Get(pvcRes, testConfig().Namespace, dataPVCName("w1"))
			rv := cur.(*corev1.PersistentVolumeClaim).ResourceVersion
			o, err := next()
			return clone(o, func(p *corev1.PersistentVolumeClaim) { p.ResourceVersion = rv }), err
		}},
		{name: "lost-response", committed: true, react: func(c *fake.Clientset, next func() (runtime.Object, error)) (runtime.Object, error) {
			if _, err := next(); err != nil {
				return nil, err
			}
			return nil, errors.New("lost patch response")
		}},
	}
}

// caSeedUnstamped creates the worker's claims unstamped, with explicit identities.
func caSeedUnstamped(t *testing.T, c *fake.Clientset, id string) {
	t.Helper()
	for _, o := range caClaims(id, false) {
		p := o.(*corev1.PersistentVolumeClaim)
		p.UID, p.ResourceVersion = types.UID("uid-"+p.Name), "7"
		if err := c.Tracker().Create(pvcRes, p, p.Namespace); err != nil {
			t.Fatal(err)
		}
	}
}

// caBump moves the worker's data claim to a new resourceVersion through the tracker.
func caBump(c *fake.Clientset, id string) {
	p, err := c.Tracker().Get(pvcRes, testConfig().Namespace, dataPVCName(id))
	if err != nil {
		panic(err)
	}
	n := p.DeepCopyObject().(*corev1.PersistentVolumeClaim)
	n.ResourceVersion += "1"
	if err := c.Tracker().Update(pvcRes, n, testConfig().Namespace); err != nil {
		panic(err)
	}
}

// R5, R6, R7, R8: a stamp that is not confirmed stops the pass before anything else is
// written, clears the carry, and leaves the decision to the next observation.
func TestCodexCALateTokenUnconfirmedStampStopsThePass(t *testing.T) {
	for _, tc := range caUnconfirmedCases() {
		t.Run(tc.name, func(t *testing.T) {
			m, c, w := caLateStart(t, true)
			ns := m.cfg.Namespace
			caIntercept(c, "patch", "persistentvolumeclaims", caIsStampPatch, func(next func() (runtime.Object, error)) (runtime.Object, error) {
				return tc.react(c, next)
			})
			c.ClearActions()
			err := caTick(t, m, w)
			if err == nil || !strings.Contains(err.Error(), "codex CA claim stamp unconfirmed") {
				t.Fatalf("tick error = %v, want the unconfirmed stamp error", err)
			}
			if n := caStampPatchCount(c, "w1"); n != 1 {
				t.Fatalf("stamp patches = %d, want the one attempt", n)
			}
			if m.codexCACarried("w1", ns) {
				t.Error("the carry survived an unconfirmed stamp")
			}
			if got := caDeploymentWrites(c); len(got) != 0 {
				t.Errorf("the failed pass wrote deployments: %v", got)
			}
			if got := caStamped(caClaim(t, c, ns, dataPVCName("w1"))); got != tc.committed {
				t.Fatalf("claim stamped = %v, want %v", got, tc.committed)
			}
			if tc.committed {
				// The next observation sees the stamp, so the env is rendered.
				caMustTick(t, m, w)
				caMustTick(t, m, w)
				if d := caDep(t, c, ns, "w1"); d == nil || !caHasEnv(d) {
					t.Fatalf("deployment = %v, want the env once the stamp is observed", d)
				}
				return
			}
			caNoEnvAfter(t, m, c, w, 3)
		})
	}
}

// R5b: after a rejected stamp the own claim disappears; the replacement is created
// unstamped and no env is rendered.
func TestCodexCALateTokenRejectedStampThenClaimDeleted(t *testing.T) {
	m, c, w := caLateStart(t, true)
	ns := m.cfg.Namespace
	caIntercept(c, "patch", "persistentvolumeclaims", caIsStampPatch, func(next func() (runtime.Object, error)) (runtime.Object, error) {
		return nil, apierrors.NewForbidden(pvcGR, dataPVCName("w1"), errors.New("rejected"))
	})
	if err := caTick(t, m, w); err == nil {
		t.Fatal("rejected stamp was not reported")
	}
	if err := c.Tracker().Delete(pvcRes, ns, dataPVCName("w1")); err != nil {
		t.Fatal(err)
	}
	caNoEnvAfter(t, m, c, w, 3)
	if caClaim(t, c, ns, dataPVCName("w1")) == nil {
		t.Error("the replacement claim was not created")
	}
}

// R5c: after a rejected stamp the own claim is Terminating; no env.
func TestCodexCALateTokenRejectedStampThenClaimTerminating(t *testing.T) {
	m, c, w := caLateStart(t, true)
	ns := m.cfg.Namespace
	caIntercept(c, "patch", "persistentvolumeclaims", caIsStampPatch, func(next func() (runtime.Object, error)) (runtime.Object, error) {
		return nil, apierrors.NewForbidden(pvcGR, dataPVCName("w1"), errors.New("rejected"))
	})
	if err := caTick(t, m, w); err == nil {
		t.Fatal("rejected stamp was not reported")
	}
	caTerminateClaim(t, c, ns, dataPVCName("w1"))
	caNoEnvAfter(t, m, c, w, 3)
}

// N1: a Secret that already exists proves nothing about its contents: no stamp, no env.
func TestCodexCAPreexistingSecretOnAnUnstampedClaimIsNotStamped(t *testing.T) {
	cfg := caCfg()
	ns := cfg.Namespace
	old := &corev1.Secret{}
	old.Name, old.Namespace, old.Labels = secretName("w1"), ns, objectLabels("w1")
	old.Data = map[string][]byte{tokenKey: []byte("uzw_old")}
	m, c := caMat(t, cfg, append(caClaims("w1", false), old)...)
	for i := 0; i < 3; i++ {
		caMustTick(t, m, caWorker("w1"))
	}
	if n := caStampPatchCount(c, "w1"); n != 0 {
		t.Errorf("stamp patches = %d, want none", n)
	}
	if caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Error("claim stamped on an AlreadyExists Secret")
	}
	if d := caDep(t, c, ns, "w1"); d == nil || caHasEnv(d) {
		t.Errorf("deployment = %v, want present without the env", d)
	}
}

// N2: the roll entry's claim is Terminating while the observation says live (same UID, newer
// resourceVersion): the two disagree, so no patch is sent and the tick reports it.
func TestCodexCALateTokenStampRefusesADisagreeingRollClaim(t *testing.T) {
	m, c, workers, _ := matrixFixture(t, 3)
	m.cfg.UIDSplit, m.cfg.APICAPEM, m.cfg.ExtraCAPEM = true, []byte("api-ca"), []byte("extra-ca-A")
	m.cfg.RollConcurrency = 2
	caSeedUnstamped(t, c, "w1")
	workers[0].JoinToken = token("uzw_w1")
	ns := m.cfg.Namespace
	obs, err := m.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	caSetClaim(t, c, ns, dataPVCName("w1"), func(p *corev1.PersistentVolumeClaim) {
		p.ResourceVersion = "999"
		gone := metav1.Now()
		p.DeletionTimestamp = &gone
	})
	c.ClearActions()
	err = m.Reconcile(context.Background(), workers, obs)
	if err == nil || !strings.Contains(err.Error(), "codex CA claim stamp unconfirmed") {
		t.Fatalf("tick error = %v, want the unconfirmed stamp error", err)
	}
	if n := caStampPatchCount(c, "w1"); n != 0 {
		t.Errorf("stamp patches = %d, want none", n)
	}
	if m.codexCACarried("w1", ns) {
		t.Error("the carry survived the refusal")
	}
}

// N3: an observation without a UID or without a resourceVersion cannot be fenced: no patch.
func TestCodexCALateTokenStampRefusesAnUnfencedObservation(t *testing.T) {
	for _, field := range []string{"uid", "resourceVersion"} {
		t.Run(field, func(t *testing.T) {
			m, c, w := caLateStart(t, true)
			ns := m.cfg.Namespace
			obs := caObserve(t, m, "w1")
			claims := map[string]reconcile.ClaimObservation{}
			for k, v := range obs.DataClaimsByNamespace {
				claims[k] = v
			}
			own := claims[ns]
			if field == "uid" {
				own.UID = ""
			} else {
				own.ResourceVersion = ""
			}
			claims[ns] = own
			obs.DataClaimsByNamespace = claims
			if obs.DataClaim != nil {
				cp := own
				obs.DataClaim = &cp
			}
			c.ClearActions()
			err := m.reconcileWorker(context.Background(), w, obs)
			if err == nil || !strings.Contains(err.Error(), "codex CA claim stamp unconfirmed") {
				t.Fatalf("reconcileWorker error = %v, want the unconfirmed stamp error", err)
			}
			if n := caStampPatchCount(c, "w1"); n != 0 {
				t.Errorf("stamp patches = %d, want none", n)
			}
		})
	}
}

// The stamp body is exactly the fenced annotation: no labels or other metadata ride along.
func TestCodexCALateTokenStampPatchBodyIsMinimal(t *testing.T) {
	m, c, w := caLateStart(t, true)
	c.ClearActions()
	caMustTick(t, m, w)
	for _, a := range c.Actions() {
		if !caIsStampPatch(a) {
			continue
		}
		var body map[string]map[string]any
		p := a.(k8stesting.PatchAction)
		if err := json.Unmarshal(p.GetPatch(), &body); err != nil {
			t.Fatal(err)
		}
		md := body["metadata"]
		if len(body) != 1 || len(md) != 3 || md["uid"] == "" || md["resourceVersion"] == "" || md["annotations"] == nil {
			t.Errorf("stamp patch = %s, want only uid, resourceVersion and the annotation", p.GetPatch())
		}
		if p.GetPatchType() != types.MergePatchType {
			t.Errorf("patch type = %s", p.GetPatchType())
		}
	}
}
