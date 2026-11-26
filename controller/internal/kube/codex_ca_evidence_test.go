package kube

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// Reconcile-level tests for the codex-extra-ca evidence: UZI_CODEX_CA_FILE is rendered
// only when the worker's write-once Secret is known to carry the key, i.e. the /data
// claim carries uzi.dev/codex-extra-ca=written (create-time stamp) or the in-process carry
// bridges the window until that claim exists. See codex_ca.go.

const caEnvName = "UZI_CODEX_CA_FILE"

func caCfg() RenderConfig {
	cfg := testConfig()
	cfg.UIDSplit = true // no UZI_CLI_CA_FILE: the Codex env is the only extra-CA difference
	cfg.APICAPEM = []byte("api-ca")
	cfg.ExtraCAPEM = []byte("extra-ca-A")
	return cfg
}

func caMat(t *testing.T, cfg RenderConfig, objs ...runtime.Object) (*Materializer, *fake.Clientset) {
	t.Helper()
	client := enforcingClient(t, objs...)
	m := New(client, cfg, testResolver(t), &fakeCordoner{}, DrainPolicy{Deadline: 24 * time.Hour},
		RecyclePolicy{Enabled: true, Cooldown: time.Hour}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.now = func() time.Time { return m5Now }
	return m, client
}

// caWorker is a brand-new worker (first provision) that carries its join token.
func caWorker(id string) protocol.DesiredWorker {
	w := newWorkerFixture(desired(id))
	w.JoinToken = token("uzw_" + id)
	return w
}

func caTick(t *testing.T, m *Materializer, ws ...protocol.DesiredWorker) error {
	t.Helper()
	return npTick(t, m, ws...)
}

func caMustTick(t *testing.T, m *Materializer, ws ...protocol.DesiredWorker) {
	t.Helper()
	if err := caTick(t, m, ws...); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

func caDep(t *testing.T, c *fake.Clientset, ns, id string) *appsv1.Deployment {
	t.Helper()
	d, err := c.AppsV1().Deployments(ns).Get(context.Background(), deploymentName(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func caHasEnv(d *appsv1.Deployment) bool {
	_, ok := codexCABaseEnv(d)[caEnvName]
	return ok
}

func caClaim(t *testing.T, c *fake.Clientset, ns, name string) *corev1.PersistentVolumeClaim {
	t.Helper()
	p, err := c.CoreV1().PersistentVolumeClaims(ns).Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func caStamped(p *corev1.PersistentVolumeClaim) bool {
	return p != nil && p.Annotations["uzi.dev/codex-extra-ca"] == "written"
}

// caClaims seeds the worker's claims at their desired size, the data claim stamped or not.
func caClaims(id string, stamped bool) []runtime.Object {
	data := pvcFor(id, "data")
	if stamped {
		data.Annotations = map[string]string{AnnotationCodexExtraCA: codexExtraCAWritten}
	}
	return []runtime.Object{data, pvcNix(id, "20Gi")}
}

// caDeploymentWrites lists every Deployment mutation the fake apiserver saw.
func caDeploymentWrites(c *fake.Clientset) []string {
	var out []string
	for _, a := range c.Actions() {
		if a.GetResource().Resource != "deployments" {
			continue
		}
		switch a.GetVerb() {
		case "create", "update", "patch", "delete":
			out = append(out, a.GetVerb())
		}
	}
	return out
}

func caSecretActions(c *fake.Clientset) int {
	n := 0
	for _, a := range c.Actions() {
		if a.GetResource().Resource == "secrets" {
			n++
		}
	}
	return n
}

// caDataPVCCreate intercepts creates of the worker's data claim. commit stores the object
// before failing, modelling a create whose response was lost. It fails the first n calls.
func caDataPVCCreate(c *fake.Clientset, id string, n int, commit bool) {
	c.PrependReactor("create", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		obj := a.(k8stesting.CreateAction).GetObject()
		md, _ := meta.Accessor(obj)
		if md.GetName() != dataPVCName(id) || n <= 0 {
			return false, nil, nil
		}
		n--
		if commit {
			stored := obj.DeepCopyObject()
			sm, _ := meta.Accessor(stored)
			sm.SetUID(types.UID("uid-" + sm.GetName()))
			sm.SetResourceVersion("1")
			if err := c.Tracker().Create(a.GetResource(), stored, a.GetNamespace()); err != nil {
				return true, nil, err
			}
		}
		return true, nil, errors.New("lost create response")
	})
}

// AC3: a brand-new worker. Tick 1 writes the Secret with the key and stamps the data
// claim but creates no Deployment; tick 2 creates it with the env; tick 3 is a no-op.
func TestCodexCAFreshWorkerTakesThreeTicks(t *testing.T) {
	cfg := caCfg()
	ns := cfg.Namespace
	m, c := caMat(t, cfg)
	w := caWorker("w1")

	caMustTick(t, m, w)
	s, err := c.CoreV1().Secrets(ns).Get(context.Background(), secretName("w1"), metav1.GetOptions{})
	if err != nil || string(s.Data["codex-extra-ca.crt"]) != "extra-ca-A" {
		t.Fatalf("tick 1 secret = %v, %v; want the codex key", s, err)
	}
	if !caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Error("tick 1: data claim is not stamped")
	}
	if caStamped(caClaim(t, c, ns, nixPVCName("w1"))) {
		t.Error("tick 1: the nix claim must not carry the codex stamp")
	}
	if d := caDep(t, c, ns, "w1"); d != nil {
		t.Fatalf("tick 1 created the Deployment (env=%v); it must wait for the stamp to be observed", caHasEnv(d))
	}
	if m.codexCACarried("w1", ns) {
		t.Error("tick 1: the confirmed stamped create left the carry in place")
	}

	caMustTick(t, m, w)
	d := caDep(t, c, ns, "w1")
	if d == nil || !caHasEnv(d) {
		t.Fatalf("tick 2: deployment = %v, want created with %s", d, caEnvName)
	}
	hash := depHash(d)

	c.ClearActions()
	caMustTick(t, m, w)
	if got := caDeploymentWrites(c); len(got) != 0 {
		t.Fatalf("tick 3 wrote deployments: %v", got)
	}
	if got := depHash(caDep(t, c, ns, "w1")); got != hash {
		t.Fatalf("hash moved %s -> %s", hash, got)
	}
}

// AC4a: a stamped worker keeps the env across a wake, whether the Deployment is patched
// from zero or recreated.
func TestCodexCAStampedWakeKeepsTheEnv(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		name := "patch"
		if recreate {
			name = "recreate"
		}
		t.Run(name, func(t *testing.T) {
			cfg := caCfg()
			w := hibW(protocol.HibernationWaking, "wake-op")
			objs := caClaims("w1", true)
			var before string
			if !recreate {
				d := RenderDeployment(cfg.withCodexCA(true), w, testSpec(t, "base", "m"))
				before = depHash(d)
				zero := int32(0)
				d.Spec.Replicas = &zero
				d.Annotations = map[string]string{annotationHibernationOp: "old-op"}
				objs = append(objs, d)
			}
			m, c := caMat(t, cfg, objs...)
			caMustTick(t, m, w)
			d := caDep(t, c, cfg.Namespace, "w1")
			if d == nil || d.Spec.Replicas == nil || *d.Spec.Replicas != 1 || !caHasEnv(d) {
				t.Fatalf("woken deployment = %+v, want replicas 1 with %s", d, caEnvName)
			}
			if before != "" && depHash(d) != before {
				t.Errorf("hash moved %s -> %s", before, depHash(d))
			}
		})
	}
}

// newCACohort is newWakeCanary's cohort with the extra CA configured BEFORE the first roll
// tick, so every Deployment is rendered under the final config. The claims are created
// unstamped (no join token is ever sent).
func newCACohort(t *testing.T) *wakeCanary {
	t.Helper()
	m, c, workers, cordon := matrixFixture(t, 3)
	m.cfg.UIDSplit, m.cfg.APICAPEM, m.cfg.ExtraCAPEM = true, []byte("api-ca"), []byte("extra-ca-A")
	m.cfg.RollConcurrency = 2
	o := &orphanCohort{m: m, c: c, base: c.ReactionChain[1], cordon: cordon, workers: workers}
	rollTickTest(t, m, workers)
	removeOldPod(t, c, "w1")
	currentProof(t, c, &o.workers[0], m5Now)
	rollTickTest(t, m, workers)
	o.edit(t, "w1", func(d *appsv1.Deployment) {
		if d.Annotations == nil {
			d.Annotations = map[string]string{}
		}
		d.Annotations[annotationHibernationOp] = wakeCanaryOldOp
	})
	pod, err := c.CoreV1().Pods(m.cfg.Namespace).Get(context.Background(), "pod-w1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := c.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, pod.Namespace); err != nil {
		t.Fatal(err)
	}
	o.at("w1").Hibernation = &protocol.Hibernation{State: protocol.HibernationWaking, OpID: wakeCanaryOp}
	rec := &hibRec{fakeCordoner: cordon}
	m.cordoner = rec
	return &wakeCanary{orphanCohort: o, rec: rec}
}

// AC4b: an UNADMITTED waking canary with keyless claims and an existing Deployment gets no
// template or env mutation; the only write is the annotation-only op binding.
func TestCodexCAUnadmittedKeylessWakeWritesNoTemplate(t *testing.T) {
	wc := newCACohort(t)
	for _, claim := range []string{dataPVCName("w1"), nixPVCName("w1")} {
		if caStamped(caClaim(t, wc.c, wc.m.cfg.Namespace, claim)) {
			t.Fatalf("fixture claim %s is stamped", claim)
		}
	}
	wc.requireUnadmitted(t)
	before := wc.dep(t, "w1").DeepCopy()
	if caHasEnv(before) {
		t.Fatal("fixture Deployment already has the codex env")
	}
	wc.c.ClearActions()
	wc.mustTick(t)
	// Exactly the annotation-only op binding of patchWakeOpAnnotation, nothing else.
	wc.requirePatchCount(t, "w1", 1)
	requireOnlyOpBinding(t, wc.allPatches(t, "w1")[0], string(before.UID), before.ResourceVersion)
	if got := caDeploymentWrites(wc.c); len(got) != 1 || got[0] != "patch" {
		t.Errorf("deployment writes = %v, want the single op-binding patch", got)
	}
	after := wc.dep(t, "w1")
	if after.Annotations[annotationHibernationOp] != wakeCanaryOp {
		t.Errorf("op annotation = %q, want the op bound", after.Annotations[annotationHibernationOp])
	}
	if !reflect.DeepEqual(after.Spec, before.Spec) {
		t.Error("the keyless wake changed the Deployment spec (template or replicas)")
	}
	if caHasEnv(after) || depHash(after) != depHash(before) {
		t.Errorf("template changed: env=%v hash %s -> %s", caHasEnv(after), depHash(before), depHash(after))
	}
}

// AC5: the Secret already exists, so Create answers AlreadyExists. Nothing proves it holds
// the key: no carry, an unstamped claim, and the Deployment is created the same tick
// without the env.
func TestCodexCAPreexistingSecretWritesNoEvidence(t *testing.T) {
	cfg := caCfg()
	ns := cfg.Namespace
	old := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName("w1"), Namespace: ns, Labels: objectLabels("w1")},
		Data: map[string][]byte{tokenKey: []byte("uzw_old")}}
	m, c := caMat(t, cfg, old)
	w := caWorker("w1")
	caMustTick(t, m, w)
	if caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Error("data claim stamped although the Secret create was AlreadyExists")
	}
	if m.codexCACarried("w1", ns) {
		t.Error("carry set although the Secret create was AlreadyExists")
	}
	d := caDep(t, c, ns, "w1")
	if d == nil || caHasEnv(d) {
		t.Fatalf("deployment = %v; want created the same tick without the env", d)
	}
	caMustTick(t, m, w)
	if caHasEnv(caDep(t, c, ns, "w1")) {
		t.Error("env appeared on a later tick")
	}
}

// AC6a: the data claim create is rejected after the Secret was written. The same
// Materializer remembers the Secret write, so the retry stamps the claim and creates the
// Deployment with the env, over a claim stamped in the same pass.
func TestCodexCAInterruptedProvisioningSameProcessStamps(t *testing.T) {
	cfg := caCfg()
	ns := cfg.Namespace
	m, c := caMat(t, cfg)
	caDataPVCCreate(c, "w1", 1, false)
	w := caWorker("w1")
	if err := caTick(t, m, w); err == nil {
		t.Fatal("rejected claim create was not reported")
	}
	if _, err := c.CoreV1().Secrets(ns).Get(context.Background(), secretName("w1"), metav1.GetOptions{}); err != nil {
		t.Fatalf("tick 1: secret not created: %v", err)
	}
	if caClaim(t, c, ns, dataPVCName("w1")) != nil {
		t.Fatal("tick 1: the rejected data claim exists")
	}
	if d := caDep(t, c, ns, "w1"); d != nil {
		t.Fatal("deployment created beside a missing data claim")
	}
	// Retry tick: the carry from tick 1 makes the frozen decision true, so the claim is
	// stamped and the Deployment is created with the env in this same tick.
	caMustTick(t, m, w)
	if !caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Fatal("retry did not stamp the data claim")
	}
	d := caDep(t, c, ns, "w1")
	if d == nil || !caHasEnv(d) {
		t.Fatalf("retry tick: deployment = %v, want created with the env", d)
	}
	hash := depHash(d)
	// Next tick: the stamp is observed; no Deployment write and an equal hash.
	c.ClearActions()
	caMustTick(t, m, w)
	if got := caDeploymentWrites(c); len(got) != 0 {
		t.Fatalf("next tick wrote deployments: %v", got)
	}
	if got := depHash(caDep(t, c, ns, "w1")); got != hash {
		t.Fatalf("hash moved %s -> %s", hash, got)
	}
}

// AC6b: the same interruption, but the retry runs in a fresh process: the Secret write is
// forgotten, so the claim is unstamped and the Deployment has no env.
func TestCodexCAInterruptedProvisioningRestartRendersNoEnv(t *testing.T) {
	cfg := caCfg()
	ns := cfg.Namespace
	m, c := caMat(t, cfg)
	caDataPVCCreate(c, "w1", 1, false)
	w := caWorker("w1")
	if err := caTick(t, m, w); err == nil {
		t.Fatal("rejected claim create was not reported")
	}
	m = matrixRestart(m)
	for i := 0; i < 2; i++ {
		caMustTick(t, m, w)
	}
	if caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Error("claim stamped without evidence")
	}
	if d := caDep(t, c, ns, "w1"); d == nil || caHasEnv(d) {
		t.Fatalf("deployment = %v, want created without the env", d)
	}
}

// AC6c: the stamped claim create commits but its response is lost. The next pass observes
// the stamp, so the env is rendered, with or without the carry.
func TestCodexCALostStampedCreateResponseStillRendersEnv(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "same-process"
		if restart {
			name = "restart"
		}
		t.Run(name, func(t *testing.T) {
			cfg := caCfg()
			ns := cfg.Namespace
			m, c := caMat(t, cfg)
			caDataPVCCreate(c, "w1", 1, true)
			w := caWorker("w1")
			if err := caTick(t, m, w); err == nil {
				t.Fatal("lost response was not reported")
			}
			if !caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
				t.Fatal("fixture: the stamped claim did not commit")
			}
			if restart {
				m = matrixRestart(m)
			}
			caMustTick(t, m, w)
			if d := caDep(t, c, ns, "w1"); d == nil || !caHasEnv(d) {
				t.Fatalf("deployment = %v, want the env", d)
			}
		})
	}
}

// AC7: a Secret create response without the key proves nothing: no carry, no stamp.
func TestCodexCASecretResponseWithoutTheKeyWritesNoEvidence(t *testing.T) {
	cfg := caCfg()
	ns := cfg.Namespace
	m, c := caMat(t, cfg)
	c.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		stored := a.(k8stesting.CreateAction).GetObject().DeepCopyObject().(*corev1.Secret)
		if err := c.Tracker().Create(a.GetResource(), stored, a.GetNamespace()); err != nil {
			return true, nil, err
		}
		echo := stored.DeepCopy()
		delete(echo.Data, codexExtraCACertKey)
		return true, echo, nil
	})
	caMustTick(t, m, caWorker("w1"))
	if caStamped(caClaim(t, c, ns, dataPVCName("w1"))) || m.codexCACarried("w1", ns) {
		t.Error("evidence written from a response that lacks the key")
	}
	if d := caDep(t, c, ns, "w1"); d == nil || caHasEnv(d) {
		t.Fatalf("deployment = %v, want created without the env", d)
	}
}

// recycleFixture is the pressure-recycle scenario of TestDiskPressureRecyclesBothVolumes
// with the extra CA configured and a stamped data claim. retained keeps a zero-replica
// Deployment with a reservation marker; otherwise a plain running Deployment, which the
// recycle deletes.
func recycleFixture(t *testing.T, retained bool) (*Materializer, *fake.Clientset, protocol.DesiredWorker, RenderConfig) {
	t.Helper()
	cfg := caCfg()
	w := evidence(protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", DiskPressure: true}, m5Now)
	var dep *appsv1.Deployment
	if retained {
		dep = markedDeployment(t, cfg.withCodexCA(true), w, false)
		zero := int32(0)
		dep.Spec.Replicas = &zero
	} else {
		dep = RenderDeployment(cfg.withCodexCA(true), w, testSpec(t, "base", "m"))
		dep.UID, dep.ResourceVersion = "dep-uid", "1"
	}
	objs := append([]runtime.Object{dep}, caClaims("w1", true)...)
	m, c := caMat(t, cfg, objs...)
	return m, c, w, cfg
}

func caRecycleTick1(t *testing.T, m *Materializer, c *fake.Clientset, w protocol.DesiredWorker) {
	t.Helper()
	caMustTick(t, m, w)
	del := deletedSet(c)
	if !del["persistentvolumeclaims/"+dataPVCName("w1")] || !del["persistentvolumeclaims/"+nixPVCName("w1")] {
		t.Fatalf("recycle did not delete both claims: %v", keysOf(del))
	}
}

// AC8a: the Deployment is deleted by the recycle; the replacement claim is stamped (the
// carry survives the delete) and the recreated Deployment keeps the env.
func TestCodexCAPressureRecycleWithDeletedDeploymentKeepsTheEnv(t *testing.T) {
	m, c, w, cfg := recycleFixture(t, false)
	caRecycleTick1(t, m, c, w)
	if d := caDep(t, c, cfg.Namespace, "w1"); d != nil {
		t.Fatal("recycle did not delete the Deployment")
	}
	for i := 0; i < 3; i++ {
		caMustTick(t, m, w)
	}
	if !caStamped(caClaim(t, c, cfg.Namespace, dataPVCName("w1"))) {
		t.Error("replacement data claim is not stamped")
	}
	if d := caDep(t, c, cfg.Namespace, "w1"); d == nil || !caHasEnv(d) {
		t.Fatalf("recreated deployment = %v, want the env", d)
	}
}

// AC8b: a retained zero-replica Deployment resumes with the env kept and its hash unchanged.
func TestCodexCAPressureRecycleRetainedDeploymentKeepsTheEnv(t *testing.T) {
	m, c, w, cfg := recycleFixture(t, true)
	before := depHash(caDep(t, c, cfg.Namespace, "w1"))
	caRecycleTick1(t, m, c, w)
	w.DiskPressure = false
	for i := 0; i < 3; i++ {
		caMustTick(t, m, w)
	}
	if !caStamped(caClaim(t, c, cfg.Namespace, dataPVCName("w1"))) {
		t.Error("replacement data claim is not stamped")
	}
	d := caDep(t, c, cfg.Namespace, "w1")
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 || !caHasEnv(d) || depHash(d) != before {
		t.Fatalf("resumed deployment replicas=%v env=%v hash %s -> %s", d.Spec.Replicas, caHasEnv(d), before, depHash(d))
	}
}

// AC8c: the controller restarts between the delete and the re-mint. The retained
// Deployment's durable record survives the restart, so the replacement claim is stamped
// again, the spec hash is unchanged and the resumed Deployment keeps the env.
func TestCodexCAPressureRecycleRestartKeepsTheEnv(t *testing.T) {
	m, c, w, cfg := recycleFixture(t, true)
	before := depHash(caDep(t, c, cfg.Namespace, "w1"))
	caRecycleTick1(t, m, c, w)
	m = matrixRestart(m)
	w.DiskPressure = false
	for i := 0; i < 4; i++ {
		caMustTick(t, m, w)
	}
	if !caStamped(caClaim(t, c, cfg.Namespace, dataPVCName("w1"))) {
		t.Error("replacement claim is not stamped after a restart")
	}
	d := caDep(t, c, cfg.Namespace, "w1")
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 || !caHasEnv(d) || depHash(d) != before {
		t.Fatalf("resumed deployment replicas=%v env=%v hash %s -> %s", d.Spec.Replicas, caHasEnv(d), before, depHash(d))
	}
}

// AC8d: the data claim stays Terminating for a tick in the same process. The carry must
// survive it (a Terminating claim never clears the carry), no second delete is issued,
// the replacement is stamped, and the env is kept.
func TestCodexCAPressureRecycleTerminatingDataClaimKeepsTheCarry(t *testing.T) {
	m, c, w, cfg := recycleFixture(t, true)
	ns := cfg.Namespace
	var dataDeletes int
	c.PrependReactor("delete", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		name := a.(k8stesting.DeleteAction).GetName()
		if name != dataPVCName("w1") {
			return false, nil, nil
		}
		dataDeletes++
		p, err := c.Tracker().Get(a.GetResource(), a.GetNamespace(), name)
		if err != nil {
			return true, nil, err
		}
		keep := p.DeepCopyObject().(*corev1.PersistentVolumeClaim)
		now := metav1.NewTime(m5Now)
		keep.DeletionTimestamp = &now
		return true, nil, c.Tracker().Update(a.GetResource(), keep, a.GetNamespace())
	})
	before := depHash(caDep(t, c, ns, "w1"))
	caMustTick(t, m, w)
	if p := caClaim(t, c, ns, dataPVCName("w1")); p == nil || p.DeletionTimestamp == nil {
		t.Fatal("fixture: the data claim is not Terminating")
	}
	w.DiskPressure = false
	caMustTick(t, m, w) // the claim is still Terminating
	if dataDeletes != 1 {
		t.Fatalf("data claim deleted %d times, want once", dataDeletes)
	}
	if err := c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), ns, dataPVCName("w1")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		caMustTick(t, m, w)
	}
	if !caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Fatal("replacement data claim is not stamped")
	}
	d := caDep(t, c, ns, "w1")
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 || !caHasEnv(d) || depHash(d) != before {
		t.Fatalf("resumed deployment replicas=%v env=%v hash %s -> %s", d.Spec.Replicas, caHasEnv(d), before, depHash(d))
	}
}

// AC8e: the apiserver commits the data claim delete but the response is lost. The carry is
// set from the observed stamp before the delete, so the replacement claim is stamped and
// the Deployment keeps the env.
func TestCodexCAPressureRecycleLostDeleteAckKeepsTheCarry(t *testing.T) {
	m, c, w, cfg := recycleFixture(t, false)
	ns := cfg.Namespace
	c.PrependReactor("delete", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		name := a.(k8stesting.DeleteAction).GetName()
		if name != dataPVCName("w1") {
			return false, nil, nil
		}
		if err := c.Tracker().Delete(a.GetResource(), a.GetNamespace(), name); err != nil {
			return true, nil, err
		}
		return true, nil, errors.New("lost delete response")
	})
	if err := caTick(t, m, w); err == nil {
		t.Fatal("lost delete response was not reported")
	}
	if p := caClaim(t, c, ns, dataPVCName("w1")); p != nil {
		t.Fatal("fixture: the data claim delete did not commit")
	}
	// Same cadence as the other recycle tests (pressure stays on): the replacement is
	// minted on the next tick and the one after it is the next recycle.
	caMustTick(t, m, w)
	if !caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Error("replacement data claim is not stamped")
	}
	if d := caDep(t, c, ns, "w1"); d == nil || !caHasEnv(d) {
		t.Fatalf("recreated deployment = %v, want the env", d)
	}
}

// AC9: a worker whose data claim is stamped but whose Deployment is gone gets it back with the env.
func TestCodexCAStampedClaimWithoutDeploymentRecreatesWithEnv(t *testing.T) {
	cfg := caCfg()
	m, c := caMat(t, cfg, caClaims("w1", true)...)
	w := newWorkerFixture(desired("w1")) // no join token: the Secret is not touched
	caMustTick(t, m, w)
	if d := caDep(t, c, cfg.Namespace, "w1"); d == nil || !caHasEnv(d) {
		t.Fatalf("deployment = %v, want created with the env", d)
	}
	if n := caSecretActions(c); n != 0 {
		t.Errorf("secret actions = %d", n)
	}
}

// AC10: the evidence is bound to the namespace the worker renders in. A stamped claim of the
// same id, or a carry, in another namespace authorizes nothing.
func TestCodexCAEvidenceIsBoundToTheNamespace(t *testing.T) {
	cfg := caCfg()
	cfg.DockerNamespace = "uzi-workers-docker"
	cfg.DinDImage = "docker:28-dind-rootless@sha256:deadbeef"
	foreign := pvcForNS("w1", "data", cfg.DockerNamespace)
	foreign.Annotations = map[string]string{AnnotationCodexExtraCA: codexExtraCAWritten}
	m, c := caMat(t, cfg, foreign, pvcNix("w1", "20Gi"))
	m.setCodexCACarry("w1", cfg.DockerNamespace) // as if the Secret had been written there
	w := newWorkerFixture(desired("w1"))
	caMustTick(t, m, w)
	d := caDep(t, c, cfg.Namespace, "w1")
	if d == nil {
		t.Fatal("deployment not created")
	}
	if caHasEnv(d) {
		t.Error("a stamp or carry in another namespace authorized the env")
	}
}

// AC11: changing the bundle (A to B) neither moves a stamped worker's hash nor touches its
// Secret; only a new worker's Secret carries B.
func TestCodexCABundleChangeReachesOnlyNewWorkers(t *testing.T) {
	cfg := caCfg()
	ns := cfg.Namespace
	w1 := newWorkerFixture(desired("w1"))
	live := RenderDeployment(cfg.withCodexCA(true), w1, testSpec(t, "base", "m"))
	objs := append(caClaims("w1", true), live)
	m, c := caMat(t, cfg, objs...)
	cfgA := m.cfg.withCodexCA(true)
	m.cfg.ExtraCAPEM = []byte("extra-ca-B")
	// The existing worker's roll target is non-vacuously unchanged: the template rendered
	// for the stamped worker under bundle B hashes the same as under bundle A.
	spec := testSpec(t, "base", "m")
	hashA := depHash(RenderDeployment(cfgA, w1, spec))
	hashB := depHash(RenderDeployment(m.cfg.withCodexCA(true), w1, spec))
	if hashA == "" || hashA != hashB {
		t.Fatalf("target hash for the stamped worker moved with the bundle: %q -> %q", hashA, hashB)
	}
	w2 := caWorker("w2")
	caMustTick(t, m, w1, w2)
	if got := depHash(caDep(t, c, ns, "w1")); got != depHash(live) {
		t.Errorf("stamped worker hash moved %s -> %s", depHash(live), got)
	}
	for _, a := range c.Actions() {
		if a.GetResource().Resource == "secrets" && a.(k8stesting.CreateAction).GetObject().(*corev1.Secret).Name != secretName("w2") {
			t.Errorf("secret action for another worker: %s", a.GetVerb())
		}
	}
	s, err := c.CoreV1().Secrets(ns).Get(context.Background(), secretName("w2"), metav1.GetOptions{})
	if err != nil || string(s.Data[codexExtraCACertKey]) != "extra-ca-B" {
		t.Fatalf("new worker secret = %v, %v; want bundle B", s, err)
	}
}

// AC12: clearing the extra CA removes the env, writes no Secret action and leaves the token
// env and volume alone.
func TestCodexCARemovalDropsTheEnvOnly(t *testing.T) {
	cfg := caCfg()
	ns := cfg.Namespace
	w := evidence(desired("w1"), m5Now)
	w.Ephemeral = true // outside the roll pools: the template is patched directly
	live := RenderDeployment(cfg.withCodexCA(true), w, testSpec(t, "base", "m"))
	liveSecret := RenderSecret(cfg, w, "uzw_w1") // written while the bundle was configured
	objs := append(caClaims("w1", true), live, liveSecret)
	m, c := caMat(t, cfg, objs...)
	m.cfg.ExtraCAPEM = nil
	caMustTick(t, m, w)
	d := caDep(t, c, ns, "w1")
	if caHasEnv(d) {
		t.Error("env survived the removal")
	}
	if n := caSecretActions(c); n != 0 {
		t.Errorf("secret actions = %d", n)
	}
	if s, err := c.CoreV1().Secrets(ns).Get(context.Background(), secretName("w1"), metav1.GetOptions{}); err != nil || string(s.Data[codexExtraCACertKey]) != "extra-ca-A" {
		t.Errorf("live secret = %v, %v; want it to keep %s", s, err, codexExtraCACertKey)
	}
	was, now := codexCABaseEnv(live), codexCABaseEnv(d)
	if was["UZI_WORKER_TOKEN_FILE"] == "" || now["UZI_WORKER_TOKEN_FILE"] != was["UZI_WORKER_TOKEN_FILE"] {
		t.Errorf("token env %q -> %q", was["UZI_WORKER_TOKEN_FILE"], now["UZI_WORKER_TOKEN_FILE"])
	}
	vols := func(d *appsv1.Deployment) []corev1.Volume {
		var out []corev1.Volume
		for _, v := range d.Spec.Template.Spec.Volumes {
			if v.Name == "token" {
				out = append(out, v)
			}
		}
		return out
	}
	if a, b := vols(live), vols(d); len(a) != 1 || len(b) != 1 || a[0].Secret.SecretName != b[0].Secret.SecretName {
		t.Errorf("token volume %v -> %v", a, b)
	}
}

// Control: an isolated worker never has the Codex key or env, whatever the evidence.
func TestCodexCAIsolatedWorkerNeverGetsTheKeyOrEnv(t *testing.T) {
	cfg := laneTestConfig()
	cfg.ExtraCAPEM = []byte("extra")
	w := desiredIsolated("w1")
	if _, ok := RenderSecret(cfg, w, "uzw_t").Data[codexExtraCACertKey]; ok {
		t.Error("isolated Secret carries the codex key")
	}
	d := RenderDeployment(cfg.withCodexCA(true), w, testSpec(t, "base", "m"))
	if caHasEnv(d) {
		t.Error("isolated worker rendered the codex env")
	}
}

// Control: the Node and Claude CLI CA wiring does not depend on the codex evidence.
func TestCodexCAEvidenceLeavesTheOtherCAEnvAlone(t *testing.T) {
	for _, split := range []bool{false, true} {
		cfg := caCfg()
		cfg.UIDSplit = split
		w := desired("w1")
		off := codexCABaseEnv(RenderDeployment(cfg, w, testSpec(t, "base", "m")))
		on := codexCABaseEnv(RenderDeployment(cfg.withCodexCA(true), w, testSpec(t, "base", "m")))
		for _, k := range []string{"NODE_EXTRA_CA_CERTS", "UZI_CLI_CA_FILE"} {
			if off[k] != on[k] {
				t.Errorf("split=%v %s %q -> %q", split, k, off[k], on[k])
			}
		}
		if off["NODE_EXTRA_CA_CERTS"] == "" {
			t.Errorf("split=%v: NODE_EXTRA_CA_CERTS missing", split)
		}
		if _, ok := off[caEnvName]; ok {
			t.Errorf("split=%v: env without evidence", split)
		}
		if on[caEnvName] == "" {
			t.Errorf("split=%v: env missing with evidence", split)
		}
	}
}

// Control: removing the worker from the desired set, or tearing it down, forgets the carry.
func TestCodexCACarryIsDroppedWithTheWorker(t *testing.T) {
	cfg := caCfg()
	ns := cfg.Namespace
	t.Run("leaves-desired-set", func(t *testing.T) {
		m, c := caMat(t, cfg)
		caDataPVCCreate(c, "w1", 1, false)
		if err := caTick(t, m, caWorker("w1")); err == nil || !m.codexCACarried("w1", ns) {
			t.Fatalf("fixture: err=%v carried=%v", err, m.codexCACarried("w1", ns))
		}
		caMustTick(t, m, caWorker("w2"))
		if m.codexCACarried("w1", ns) {
			t.Error("carry outlived the worker's place in the desired set")
		}
	})
	t.Run("teardown", func(t *testing.T) {
		m, c := caMat(t, cfg)
		caDataPVCCreate(c, "w1", 1, false)
		if err := caTick(t, m, caWorker("w1")); err == nil || !m.codexCACarried("w1", ns) {
			t.Fatalf("fixture: err=%v carried=%v", err, m.codexCACarried("w1", ns))
		}
		if err := m.teardown(context.Background(), "w1", ns); err != nil {
			t.Fatal(err)
		}
		if m.codexCACarried("w1", ns) {
			t.Error("teardown left the carry")
		}
	})
}

// Control: observing a live claim of the worker, stamped or not, retires the carry.
func TestCodexCALiveUnstampedClaimClearsTheCarry(t *testing.T) {
	cfg := caCfg()
	ns := cfg.Namespace
	m, c := caMat(t, cfg)
	caDataPVCCreate(c, "w1", 1, false)
	w := caWorker("w1")
	if err := caTick(t, m, w); err == nil || !m.codexCACarried("w1", ns) {
		t.Fatalf("fixture: err=%v carried=%v", err, m.codexCACarried("w1", ns))
	}
	for _, o := range caClaims("w1", false) {
		if _, err := c.CoreV1().PersistentVolumeClaims(ns).Create(context.Background(), o.(*corev1.PersistentVolumeClaim), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	caMustTick(t, m, w)
	if m.codexCACarried("w1", ns) {
		t.Error("an observed live unstamped claim left the carry in place")
	}
	if d := caDep(t, c, ns, "w1"); d == nil || caHasEnv(d) {
		t.Fatalf("deployment = %v, want created without the env", d)
	}
}

// The hibernation branch honours the same guard: a Waking worker with nothing provisioned
// gets its Secret and stamped claim on tick 1 and no Deployment; tick 2 creates it with the env.
func TestCodexCAHibernationWakeDefersTheDeploymentOneTick(t *testing.T) {
	cfg := caCfg()
	ns := cfg.Namespace
	m, c := caMat(t, cfg)
	w := hibW(protocol.HibernationWaking, "wake-op")
	w.JoinToken = token("uzw_w1")

	caMustTick(t, m, w)
	s, err := c.CoreV1().Secrets(ns).Get(context.Background(), secretName("w1"), metav1.GetOptions{})
	if err != nil || len(s.Data[codexExtraCACertKey]) == 0 {
		t.Fatalf("tick 1 secret = %v, %v; want the codex key", s, err)
	}
	if !caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Error("tick 1: data claim is not stamped")
	}
	if d := caDep(t, c, ns, "w1"); d != nil {
		t.Fatalf("tick 1 created the Deployment (env=%v); it must wait for the stamp to be observed", caHasEnv(d))
	}

	caMustTick(t, m, w)
	if d := caDep(t, c, ns, "w1"); d == nil || !caHasEnv(d) {
		t.Fatalf("tick 2: deployment = %v, want created with %s", d, caEnvName)
	}
}

// A carry left by an earlier tick must not hold the guard: the worker's own unstamped claim
// is Terminating (it never clears the carry), and the Preparing stop of its live Deployment
// still has to proceed.
func TestCodexCALeftoverCarryDoesNotStickTheGuard(t *testing.T) {
	cfg := caCfg()
	ns := cfg.Namespace
	data := pvcFor("w1", "data")
	gone := metav1.NewTime(m5Now)
	data.DeletionTimestamp = &gone
	data.Finalizers = []string{"kubernetes.io/pvc-protection"}
	m, c, _ := hibEnv(t, hibDeployment(1, "op-1"))
	if err := c.Tracker().Update(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), data, ns); err != nil {
		t.Fatal(err)
	}
	m.setCodexCACarry("w1", ns) // left by an earlier tick's Secret create
	if err := hibTick(t, m, hibW(protocol.HibernationPreparing, "op-1")); err != nil {
		t.Fatal(err)
	}
	if n := replicaPatches(c); n != 1 {
		t.Fatalf("replica patches = %d, want the Preparing stop to proceed once", n)
	}
	if r := hibGet(t, c).Spec.Replicas; r == nil || *r != 0 {
		t.Fatalf("replicas = %v, want 0", r)
	}
}

// A stamped create whose response lacks the stamp is not evidence: the carry stays.
func TestCodexCAStampedCreateResponseWithoutTheStampKeepsTheCarry(t *testing.T) {
	cfg := caCfg()
	ns := cfg.Namespace
	m, c := caMat(t, cfg)
	c.PrependReactor("create", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pvc := a.(k8stesting.CreateAction).GetObject().(*corev1.PersistentVolumeClaim)
		if pvc.Name != dataPVCName("w1") {
			return false, nil, nil
		}
		stored := pvc.DeepCopy()
		stored.UID, stored.ResourceVersion = "uid-data", "1"
		if err := c.Tracker().Create(a.GetResource(), stored, a.GetNamespace()); err != nil {
			return true, nil, err
		}
		echo := stored.DeepCopy()
		delete(echo.Annotations, AnnotationCodexExtraCA)
		return true, echo, nil
	})
	caMustTick(t, m, caWorker("w1"))
	if !caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Fatal("fixture: the stored claim is not stamped")
	}
	if !m.codexCACarried("w1", ns) {
		t.Error("a create response without the stamp cleared the carry")
	}
	// The confirmed Secret write alone (secretWritten) must still defer the Deployment:
	// the tick was frozen without evidence, and an env-less create would drift-roll next tick.
	if d := caDep(t, c, ns, "w1"); d != nil {
		t.Errorf("deployment created on the tick the Secret key was first confirmed (env=%v)", caHasEnv(d))
	}
}

// Control: the direct reconcileWorker fallback (no tick context) decides like the tick.
func TestCodexCADirectReconcileWorkerMatchesTheTick(t *testing.T) {
	for _, stamped := range []bool{false, true} {
		cfg := caCfg()
		w := newWorkerFixture(desired("w1"))
		viaTick, c1 := caMat(t, cfg, caClaims("w1", stamped)...)
		caMustTick(t, viaTick, w)
		direct, c2 := caMat(t, cfg, caClaims("w1", stamped)...)
		obs, err := direct.Observe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(obs) != 1 {
			t.Fatalf("observed %d workers", len(obs))
		}
		if err := direct.reconcileWorker(context.Background(), w, obs[0]); err != nil {
			t.Fatal(err)
		}
		a, b := caDep(t, c1, cfg.Namespace, "w1"), caDep(t, c2, cfg.Namespace, "w1")
		if a == nil || b == nil {
			t.Fatalf("stamped=%v: deployments %v %v", stamped, a, b)
		}
		if caHasEnv(a) != stamped || caHasEnv(b) != stamped || depHash(a) != depHash(b) {
			t.Errorf("stamped=%v: tick env=%v direct env=%v hashes %s %s", stamped, caHasEnv(a), caHasEnv(b), depHash(a), depHash(b))
		}
	}
}
