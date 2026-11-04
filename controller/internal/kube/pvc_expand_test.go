package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// expandFx is a full-Reconcile fixture: an idle, non-drifted persistent Docker
// worker of size "l" with a live Deployment at the current hash, valid roll
// evidence and a pinned target version, whose /data and DinD claims are Bound and
// smaller than the render size.
type expandFx struct {
	t      *testing.T
	cfg    RenderConfig
	w      protocol.DesiredWorker
	m      *Materializer
	client *fake.Clientset
	logs   *bytes.Buffer
	cord   *fakeCordoner
	ns     string
}

type expandOpt struct {
	size      string
	dataReq   string // "" = 25Gi
	dindReq   string // "" = 20Gi
	class     string // "" = "standard"
	expand    *bool  // nil = class object absent; else its AllowVolumeExpansion value
	noExpand  bool   // class present with AllowVolumeExpansion left nil
	workerFn  func(*protocol.DesiredWorker)
	claimFn   func(name string, p *corev1.PersistentVolumeClaim)
	extra     []runtime.Object
	cfgFn     func(*RenderConfig)
	deployCfg func(*RenderConfig) // render the live Deployment under a different config
	plain     bool
}

func boolp(b bool) *bool { return &b }

func newExpandFx(t *testing.T, o expandOpt) *expandFx {
	t.Helper()
	cfg := dockerTestConfig()
	if o.cfgFn != nil {
		o.cfgFn(&cfg)
	}
	w := evidence(desiredDocker("w1"), m5Now)
	if o.plain {
		w = evidence(desired("w1"), m5Now)
	}
	w.Size = "l"
	if o.size != "" {
		w.Size = o.size
	}
	if o.workerFn != nil {
		o.workerFn(&w)
	}
	spec := testSpec(t, w.Template, w.Size)
	depCfg := cfg
	if o.deployCfg != nil {
		o.deployCfg(&depCfg)
	}
	dep := RenderDeployment(depCfg, w, spec)
	ns, err := cfg.Placement(w)
	if err != nil {
		t.Fatal(err)
	}
	class := o.class
	if class == "" {
		class = cfg.StorageClass
	}
	objs := []runtime.Object{dep}
	for _, p := range RenderPVCs(cfg, w, spec) {
		p.UID = types.UID("uid-" + p.Name)
		p.ResourceVersion = "7"
		p.Status.Phase = corev1.ClaimBound
		p.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: p.Spec.Resources.Requests[corev1.ResourceStorage]}
		switch p.Name {
		case dataPVCName(w.ID):
			req := o.dataReq
			if req == "" {
				req = "25Gi"
			}
			p.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse(req)
			p.Status.Capacity[corev1.ResourceStorage] = resource.MustParse(req)
		case dindDataPVCName(w.ID):
			req := o.dindReq
			if req == "" {
				req = "20Gi"
			}
			p.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse(req)
			p.Status.Capacity[corev1.ResourceStorage] = resource.MustParse(req)
		}
		sc := class
		p.Spec.StorageClassName = &sc
		if o.claimFn != nil {
			o.claimFn(p.Name, p)
		}
		objs = append(objs, p)
	}
	switch {
	case o.expand != nil:
		objs = append(objs, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: class}, AllowVolumeExpansion: o.expand})
	case o.noExpand:
		objs = append(objs, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: class}})
	}
	objs = append(objs, o.extra...)
	logs := &bytes.Buffer{}
	cord := &fakeCordoner{}
	m, client := newMatWithRecycle(t, cord, DrainPolicy{}, RecyclePolicy{Enabled: true}, logs, objs...)
	m.cfg = cfg
	return &expandFx{t: t, cfg: cfg, w: w, m: m, client: client, logs: logs, cord: cord, ns: ns}
}

func (f *expandFx) tick() error {
	f.t.Helper()
	ctx := context.Background()
	o, err := f.m.Observe(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.m.Reconcile(ctx, []protocol.DesiredWorker{f.w}, o)
}

func (f *expandFx) mustTick() {
	f.t.Helper()
	if err := f.tick(); err != nil {
		f.t.Fatalf("reconcile: %v", err)
	}
}

func (f *expandFx) claim(name string) *corev1.PersistentVolumeClaim {
	f.t.Helper()
	p, err := f.client.CoreV1().PersistentVolumeClaims(f.ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *expandFx) request(name string) string {
	q := f.claim(name).Spec.Resources.Requests[corev1.ResourceStorage]
	return q.String()
}

// pvcPatchBodies returns every PVC patch body keyed by claim name, in order.
func pvcPatches(c *fake.Clientset) (names []string, bodies []string) {
	for _, a := range c.Actions() {
		if p, ok := a.(k8stesting.PatchAction); ok && a.GetResource().Resource == "persistentvolumeclaims" {
			names = append(names, p.GetName()+"@"+p.GetNamespace())
			bodies = append(bodies, string(p.GetPatch()))
		}
	}
	return
}

// mutations lists every non-read action, "verb resource/name".
func mutations(c *fake.Clientset) []string {
	var out []string
	for _, a := range c.Actions() {
		switch a.GetVerb() {
		case "get", "list", "watch":
			continue
		}
		name := ""
		if n, ok := a.(interface{ GetName() string }); ok {
			name = n.GetName()
		}
		out = append(out, a.GetVerb()+" "+a.GetResource().Resource+"/"+name)
	}
	sort.Strings(out)
	return out
}

func storageClassGets(c *fake.Clientset) int {
	n := 0
	for _, a := range c.Actions() {
		if a.GetResource().Resource == "storageclasses" {
			n++
		}
	}
	return n
}

func expectPatchBody(uid, rv, qty string) string {
	return fmt.Sprintf(`{"metadata":{"uid":%q,"resourceVersion":%q},"spec":{"resources":{"requests":{"storage":%q}}}}`, uid, rv, qty)
}

func (f *expandFx) assertNoPodOrRollMutation() {
	f.t.Helper()
	for _, m := range mutations(f.client) {
		if strings.HasPrefix(m, "patch persistentvolumeclaims/") {
			continue
		}
		f.t.Fatalf("unexpected mutation %q (all: %v)", m, mutations(f.client))
	}
	if len(f.cord.calls) != 0 || len(f.cord.clearCalls) != 0 {
		f.t.Fatalf("cordon traffic: %v %v", f.cord.calls, f.cord.clearCalls)
	}
}

func TestExpandIdleDockerWorkerConvergesBothClaims(t *testing.T) {
	f := newExpandFx(t, expandOpt{expand: boolp(true)})
	dataUID := f.claim(dataPVCName(f.w.ID)).UID
	dindUID := f.claim(dindDataPVCName(f.w.ID)).UID
	f.mustTick()
	if p := f.m.PacingOutcomes()[f.w.ID]; p != nil {
		t.Fatalf("expected completed pacing (not surfaced), got %+v", p)
	}
	names, bodies := pvcPatches(f.client)
	want := map[string]string{
		dataPVCName(f.w.ID) + "@" + f.ns:     expectPatchBody("uid-"+dataPVCName(f.w.ID), "7", "50Gi"),
		dindDataPVCName(f.w.ID) + "@" + f.ns: expectPatchBody("uid-"+dindDataPVCName(f.w.ID), "7", "50Gi"),
	}
	if len(names) != 2 {
		t.Fatalf("want 2 PVC patches, got %v", names)
	}
	for i, n := range names {
		if bodies[i] != want[n] {
			t.Fatalf("patch %s body\n got %s\nwant %s", n, bodies[i], want[n])
		}
	}
	f.assertNoPodOrRollMutation()
	if f.claim(dataPVCName(f.w.ID)).UID != dataUID || f.claim(dindDataPVCName(f.w.ID)).UID != dindUID {
		t.Fatal("claim UID changed")
	}
	if f.request(dataPVCName(f.w.ID)) != "50Gi" || f.request(dindDataPVCName(f.w.ID)) != "50Gi" {
		t.Fatalf("requests not converged: %s %s", f.request(dataPVCName(f.w.ID)), f.request(dindDataPVCName(f.w.ID)))
	}
	if !strings.Contains(f.logs.String(), "pvc-expand-requested worker=w1 pvc="+dataPVCName("w1")) {
		t.Fatalf("missing requested log:\n%s", f.logs)
	}
	// Second tick: zero mutations and zero StorageClass gets.
	f.client.ClearActions()
	f.mustTick()
	if got := mutations(f.client); len(got) != 0 {
		t.Fatalf("second tick mutated: %v", got)
	}
	if storageClassGets(f.client) != 0 {
		t.Fatal("second tick looked up the StorageClass")
	}
}

func TestExpandBusyAndCustodyHeldWorkersStillExpand(t *testing.T) {
	for name, fn := range map[string]func(*protocol.DesiredWorker){
		"busy":    func(w *protocol.DesiredWorker) { w.Busy = true },
		"custody": func(w *protocol.DesiredWorker) { w.CustodyHeld = true },
		"both":    func(w *protocol.DesiredWorker) { w.Busy, w.CustodyHeld = true, true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newExpandFx(t, expandOpt{expand: boolp(true), workerFn: fn})
			f.mustTick()
			names, _ := pvcPatches(f.client)
			if len(names) != 2 {
				t.Fatalf("want 2 patches, got %v\nlogs:\n%s", names, f.logs)
			}
			f.assertNoPodOrRollMutation()
		})
	}
}

func podTemplateOf(t *testing.T, c *fake.Clientset, ns string) *appsv1.Deployment {
	t.Helper()
	d, err := c.AppsV1().Deployments(ns).Get(context.Background(), deploymentName("w1"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestExpandIsSizeOnlyNeutral(t *testing.T) {
	f := newExpandFx(t, expandOpt{
		expand:    boolp(true),
		cfgFn:     func(c *RenderConfig) { c.DinDDataSize = "" },
		deployCfg: func(c *RenderConfig) { c.DinDDataSize = "20Gi" },
		dindReq:   "20Gi",
	})
	before := podTemplateOf(t, f.client, f.ns)
	hashBefore := before.Spec.Template.Annotations[AnnotationSpecHash]
	genBefore := before.Spec.Template.Annotations[AnnotationGeneration]
	f.mustTick()
	after := podTemplateOf(t, f.client, f.ns)
	if !reflect.DeepEqual(before.Spec.Template, after.Spec.Template) {
		t.Fatal("pod template changed")
	}
	if after.Spec.Template.Annotations[AnnotationSpecHash] != hashBefore || after.Spec.Template.Annotations[AnnotationGeneration] != genBefore {
		t.Fatal("hash or generation annotation changed")
	}
	if !reflect.DeepEqual(before.Annotations, after.Annotations) {
		t.Fatalf("deployment annotations changed: %v -> %v", before.Annotations, after.Annotations)
	}
	for _, m := range mutations(f.client) {
		if !strings.HasPrefix(m, "patch persistentvolumeclaims/") {
			t.Fatalf("non-PVC mutation %q", m)
		}
	}
	names, bodies := pvcPatches(f.client)
	if len(names) != 2 {
		t.Fatalf("want exactly the two PVC patches, got %v", names)
	}
	for _, b := range bodies {
		if strings.Contains(b, annotationRoll) || strings.Contains(b, "annotations") {
			t.Fatalf("PVC patch touched annotations: %s", b)
		}
	}
	for _, a := range f.client.Actions() {
		if a.GetResource().Resource == "pods" && a.GetVerb() != "list" && a.GetVerb() != "watch" {
			t.Fatalf("pod action %s", a.GetVerb())
		}
	}
	if len(f.cord.calls)+len(f.cord.clearCalls) != 0 {
		t.Fatal("cordoner called")
	}
	if p := f.m.PacingOutcomes()[f.w.ID]; p != nil {
		t.Fatalf("pacing outcome %+v, want completed", p)
	}
	// Tick 2: the fake does not grow capacity, requests already converged.
	f.client.ClearActions()
	f.mustTick()
	if got := mutations(f.client); len(got) != 0 {
		t.Fatalf("second tick mutated: %v", got)
	}
}

// failClassGet makes every StorageClass get fail with err.
func failClassGet(f *expandFx, err error) {
	f.client.PrependReactor("get", "storageclasses", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, err })
}

func TestExpandControls(t *testing.T) {
	gr := schema.GroupResource{Resource: "persistentvolumeclaims"}
	type tc struct {
		name      string
		opt       expandOpt
		pre       func(f *expandFx)
		wantData  string // expected final data request, "" = unchanged 25Gi
		wantDinD  string // "" = unchanged 20Gi
		wantLog   []string
		wantNoLog []string
	}
	cases := []tc{
		{name: "equal target", opt: expandOpt{expand: boolp(true), dataReq: "50Gi", dindReq: "50Gi"}, wantData: "50Gi", wantDinD: "50Gi", wantNoLog: []string{"pvc-expand"}},
		{name: "larger than target", opt: expandOpt{expand: boolp(true), dataReq: "80Gi", dindReq: "80Gi"}, wantData: "80Gi", wantDinD: "80Gi"},
		{name: "quantity equivalent", opt: expandOpt{expand: boolp(true), dataReq: "51200Mi", dindReq: "51200Mi"}, wantData: "51200Mi", wantDinD: "51200Mi"},
		{name: "downsized target", opt: expandOpt{expand: boolp(true), size: "m", dataReq: "40Gi", dindReq: "40Gi"}, wantData: "40Gi", wantDinD: "40Gi"},
		{name: "explicit override target", opt: expandOpt{expand: boolp(true), cfgFn: func(c *RenderConfig) { c.DinDDataSize = "30Gi" }}, wantData: "50Gi", wantDinD: "30Gi"},
		{name: "class not expandable false", opt: expandOpt{expand: boolp(false)}, wantLog: []string{"pvc-expand-skipped", "reason=class-not-expandable"}},
		{name: "class expansion nil", opt: expandOpt{noExpand: true}, wantLog: []string{"reason=class-not-expandable"}},
		{name: "class absent", opt: expandOpt{}, wantLog: []string{"reason=class-lookup-failed"}},
		{name: "class forbidden", opt: expandOpt{expand: boolp(true)}, pre: func(f *expandFx) {
			failClassGet(f, apierrors.NewForbidden(schema.GroupResource{Group: "storage.k8s.io", Resource: "storageclasses"}, "standard", fmt.Errorf("no")))
		}, wantLog: []string{"reason=class-lookup-forbidden"}},
		{name: "class other error", opt: expandOpt{expand: boolp(true)}, pre: func(f *expandFx) { failClassGet(f, fmt.Errorf("boom")) }, wantLog: []string{"reason=class-lookup-failed"}},
		{name: "no storage class on claim", opt: expandOpt{expand: boolp(true), claimFn: func(_ string, p *corev1.PersistentVolumeClaim) { p.Spec.StorageClassName = nil }}, wantLog: []string{"reason=no-storage-class"}},
		{name: "pending claim", opt: expandOpt{expand: boolp(true), claimFn: func(_ string, p *corev1.PersistentVolumeClaim) {
			p.Status.Phase = corev1.ClaimPending
			p.Status.Capacity = nil
		}}, wantLog: []string{"reason=not-bound"}},
		{name: "terminating claim", opt: expandOpt{expand: boolp(true), claimFn: func(_ string, p *corev1.PersistentVolumeClaim) {
			now := metav1.Now()
			p.DeletionTimestamp = &now
			p.Finalizers = []string{"kubernetes.io/pvc-protection"}
		}}, wantLog: []string{"reason=terminating"}},
		{name: "no identity", opt: expandOpt{expand: boolp(true), claimFn: func(_ string, p *corev1.PersistentVolumeClaim) { p.UID = "" }}, pre: func(f *expandFx) {
			for _, n := range []string{dataPVCName("w1"), dindDataPVCName("w1")} {
				p := f.claim(n)
				p.UID = ""
				if _, err := f.client.CoreV1().PersistentVolumeClaims(f.ns).Update(context.Background(), p, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
		}, wantLog: []string{"reason=no-identity"}},
		{name: "request larger but capacity pending", opt: expandOpt{expand: boolp(true), dataReq: "50Gi", dindReq: "50Gi", claimFn: func(_ string, p *corev1.PersistentVolumeClaim) {
			p.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}
		}}, wantData: "50Gi", wantDinD: "50Gi", wantLog: []string{"pvc-resize-pending", "reason=capacity-below-request"}},
		{name: "409 conflict", opt: expandOpt{expand: boolp(true)}, pre: func(f *expandFx) {
			f.client.PrependReactor("patch", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewConflict(gr, "x", fmt.Errorf("stale"))
			})
		}, wantLog: []string{"pvc-expand-failed", "reason=conflict"}},
		{name: "422 replaced uid", opt: expandOpt{expand: boolp(true)}, pre: func(f *expandFx) {
			f.client.PrependReactor("patch", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewInvalid(schema.GroupKind{Kind: "PersistentVolumeClaim"}, "x", nil)
			})
		}, wantLog: []string{"pvc-expand-failed", "reason=invalid"}},
		{name: "403 quota admission", opt: expandOpt{expand: boolp(true)}, pre: func(f *expandFx) {
			f.client.PrependReactor("patch", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(gr, "x", fmt.Errorf("exceeded quota: requests.storage"))
			})
		}, wantLog: []string{"pvc-expand-failed", "reason=forbidden", "exceeded quota"}},
		{name: "unclassified error", opt: expandOpt{expand: boolp(true)}, pre: func(f *expandFx) {
			f.client.PrependReactor("patch", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, fmt.Errorf("%s", strings.Repeat("x", 500))
			})
		}, wantLog: []string{"pvc-expand-failed", "reason=error"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newExpandFx(t, c.opt)
			if c.pre != nil {
				c.pre(f)
			}
			f.client.ClearActions()
			// A failed patch must not abort the reconcile: no error returned.
			f.mustTick()
			wantData, wantDinD := c.wantData, c.wantDinD
			if wantData == "" {
				wantData = "25Gi"
			}
			if wantDinD == "" {
				wantDinD = "20Gi"
			}
			// Compare as quantities: the fake tracker canonicalises 51200Mi to 50Gi.
			for name, want := range map[string]string{dataPVCName(f.w.ID): wantData, dindDataPVCName(f.w.ID): wantDinD} {
				got := f.claim(name).Spec.Resources.Requests[corev1.ResourceStorage]
				if got.Cmp(resource.MustParse(want)) != 0 {
					t.Fatalf("%s request %s want %s\n%s", name, got.String(), want, f.logs)
				}
			}
			for _, s := range c.wantLog {
				if !strings.Contains(f.logs.String(), s) {
					t.Fatalf("log lacks %q:\n%s", s, f.logs)
				}
			}
			for _, s := range c.wantNoLog {
				if strings.Contains(f.logs.String(), s) {
					t.Fatalf("log has %q:\n%s", s, f.logs)
				}
			}
			for _, m := range mutations(f.client) {
				if !strings.HasPrefix(m, "patch persistentvolumeclaims/") {
					t.Fatalf("unexpected mutation %q", m)
				}
			}
			if c.name == "unclassified error" {
				for _, line := range strings.Split(f.logs.String(), "\n") {
					if strings.Contains(line, "pvc-expand-failed") && len(line) > 600 {
						t.Fatalf("error not truncated: %d bytes", len(line))
					}
				}
			}
		})
	}
}

func TestExpandForeignClaimIsNeverPatched(t *testing.T) {
	// A claim carrying the right name but no ownership stamp is invisible to Observe
	// (it is an orphan), so it cannot be attributed and is never patched.
	f := newExpandFx(t, expandOpt{expand: boolp(true), claimFn: func(_ string, p *corev1.PersistentVolumeClaim) { p.Labels = nil }})
	_ = f.tick()
	if names, _ := pvcPatches(f.client); len(names) != 0 {
		t.Fatalf("patched foreign claims: %v", names)
	}
}

func TestExpandAttributionMismatchSkipsAsForeign(t *testing.T) {
	// Drive expandClaims directly with an observation whose namespace differs from the
	// worker's placement: the decision must be "foreign" and nothing is patched.
	f := newExpandFx(t, expandOpt{expand: boolp(true)})
	spec := testSpec(t, f.w.Template, f.w.Size)
	ctx := context.Background()
	o, err := f.m.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	obs := o[0]
	obs.DinDClaim.Namespace = "elsewhere"
	obs.DataClaim.WorkerID = "someone-else"
	f.m.expandClaims(ctx, f.w, obs, f.ns, spec)
	if names, _ := pvcPatches(f.client); len(names) != 0 {
		t.Fatalf("patched unattributed claims: %v", names)
	}
	if !strings.Contains(f.logs.String(), "reason=foreign") {
		t.Fatalf("no foreign skip logged:\n%s", f.logs)
	}
}

func TestExpandPlainWorkerAndNixNeverGetDinDOrNixPatch(t *testing.T) {
	f := newExpandFx(t, expandOpt{expand: boolp(true), plain: true, size: "m", dataReq: "5Gi"})
	f.mustTick()
	names, _ := pvcPatches(f.client)
	if len(names) != 1 || !strings.HasPrefix(names[0], dataPVCName(f.w.ID)) {
		t.Fatalf("want only a data patch, got %v", names)
	}
	for _, n := range names {
		if strings.Contains(n, "nix") || strings.Contains(n, "dind") {
			t.Fatalf("patched %s", n)
		}
	}
}

func TestExpandNixIsNeverPatched(t *testing.T) {
	// Nix at the render size and the others already converged: no PVC patch at all,
	// and nix is not listed among patch targets in the earlier tests either.
	f := newExpandFx(t, expandOpt{expand: boolp(true), dataReq: "50Gi", dindReq: "50Gi"})
	f.mustTick()
	if names, _ := pvcPatches(f.client); len(names) != 0 {
		t.Fatalf("unexpected patches %v", names)
	}
}

func TestExpandDinDMaintenancePhases(t *testing.T) {
	active := &protocol.DindMaintenance{ID: "op", Phase: "ready", Fenced: false}
	f := newExpandFx(t, expandOpt{expand: boolp(true), workerFn: func(w *protocol.DesiredWorker) { w.DindMaintenance = active }})
	f.mustTick()
	if names, _ := pvcPatches(f.client); len(names) != 0 {
		t.Fatalf("expanded during active maintenance: %v", names)
	}
	if !strings.Contains(f.logs.String(), "pvc-expand-deferred worker=w1 pvc="+dataPVCName("w1")+" reason=maintenance") {
		t.Fatalf("no maintenance deferral:\n%s", f.logs)
	}
	for _, phase := range []string{"complete", "completed", "cancelled"} {
		t.Run(phase, func(t *testing.T) {
			f := newExpandFx(t, expandOpt{expand: boolp(true), workerFn: func(w *protocol.DesiredWorker) {
				w.DindMaintenance = &protocol.DindMaintenance{ID: "op", Phase: phase}
			}})
			f.mustTick()
			if names, _ := pvcPatches(f.client); len(names) != 2 {
				t.Fatalf("phase %s: want 2 patches, got %v\n%s", phase, names, f.logs)
			}
		})
	}
}

// completedMarker encodes a Completed canary marker valid for the worker.
func completedMarker(t *testing.T, f *expandFx, completed bool) string {
	t.Helper()
	d := markedDeployment(t, f.cfg, f.w, completed)
	return d.Annotations[annotationRoll]
}

func TestExpandCompletedCanaryMarkerRetainedStillExpands(t *testing.T) {
	f := newExpandFx(t, expandOpt{expand: boolp(true)})
	raw := completedMarker(t, f, true)
	// Retain the completed canary marker on the Deployment and the /data claim.
	d := podTemplateOf(t, f.client, f.ns)
	d.Annotations = map[string]string{annotationRoll: raw}
	if _, err := f.client.AppsV1().Deployments(f.ns).Update(context.Background(), d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
	f.mustTick()
	names, _ := pvcPatches(f.client)
	if len(names) != 2 {
		t.Fatalf("completed canary blocked expansion: patches %v\n%s", names, f.logs)
	}
	for _, n := range names {
		if !strings.HasPrefix(n, dataPVCName(f.w.ID)) && !strings.HasPrefix(n, dindDataPVCName(f.w.ID)) {
			t.Fatalf("unexpected patch target %s", n)
		}
	}
	if got := f.request(dataPVCName(f.w.ID)); got != "50Gi" {
		t.Fatalf("data not expanded: %s\n%s", got, f.logs)
	}
	for _, m := range mutations(f.client) {
		if m == "patch deployments/"+deploymentName(f.w.ID) || strings.HasPrefix(m, "delete ") || strings.HasPrefix(m, "create ") {
			t.Fatalf("unexpected mutation %s", m)
		}
	}
	// The marker annotation is preserved verbatim; expansion never writes it.
	if got := podTemplateOf(t, f.client, f.ns).Annotations[annotationRoll]; got != raw {
		t.Fatalf("deployment marker changed: %s", got)
	}
	if got := f.claim(dataPVCName(f.w.ID)).Annotations[annotationRoll]; got != "" {
		t.Fatalf("expansion wrote a claim annotation: %s", got)
	}
}

// When the completed marker is also mirrored on /data, planRoll's own cleanup
// withdraws the mirror this tick and the reservation fence holds /data back for
// exactly that tick; the next tick expands it. DinD is not behind the reservation fence.
func TestExpandMirroredCompletedMarkerWaitsOneTick(t *testing.T) {
	f := newExpandFx(t, expandOpt{expand: boolp(true)})
	raw := completedMarker(t, f, true)
	d := podTemplateOf(t, f.client, f.ns)
	d.Annotations = map[string]string{annotationRoll: raw}
	if _, err := f.client.AppsV1().Deployments(f.ns).Update(context.Background(), d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	p := f.claim(dataPVCName(f.w.ID))
	p.Annotations = map[string]string{annotationRoll: raw}
	if _, err := f.client.CoreV1().PersistentVolumeClaims(f.ns).Update(context.Background(), p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
	f.mustTick()
	if got := f.request(dataPVCName(f.w.ID)); got != "25Gi" {
		t.Fatalf("data expanded in the cleanup tick: %s", got)
	}
	f.mustTick()
	if got := f.request(dataPVCName(f.w.ID)); got != "50Gi" {
		t.Fatalf("data not expanded on the next tick: %s\n%s", got, f.logs)
	}
}

func TestExpandOpenReservationBlocksAndIsReported(t *testing.T) {
	f := newExpandFx(t, expandOpt{expand: boolp(true)})
	raw := completedMarker(t, f, false)
	// Completed:false with Canary:true is an active (non-completed) reservation.
	d := podTemplateOf(t, f.client, f.ns)
	d.Annotations = map[string]string{annotationRoll: raw}
	if _, err := f.client.AppsV1().Deployments(f.ns).Update(context.Background(), d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
	_ = f.tick()
	names, _ := pvcPatches(f.client)
	for _, n := range names {
		if strings.HasPrefix(n, dataPVCName(f.w.ID)) {
			t.Fatalf("/data expanded under an active reservation: %v", names)
		}
	}
	if f.request(dataPVCName(f.w.ID)) != "25Gi" {
		t.Fatal("/data request changed")
	}
}

func TestExpandClaimGateUnit(t *testing.T) {
	w := evidence(desiredDocker("w1"), m5Now)
	snapWith := func(annotation string, present bool) *reconcile.ClaimObservation {
		return &reconcile.ClaimObservation{RollAnnotation: annotation, RollAnnotationPresent: present}
	}
	open := func() *rollMarker { return &rollMarker{Completed: false} }
	done := func() *rollMarker { return &rollMarker{Completed: true, Canary: true} }
	completedRaw := func() string {
		f := newExpandFx(t, expandOpt{})
		return completedMarker(t, f, true)
	}()
	openRaw := func() string {
		f := newExpandFx(t, expandOpt{})
		return completedMarker(t, f, false)
	}()
	cases := []struct {
		name      string
		w         protocol.DesiredWorker
		kind      claimKind
		e         *rollEntry
		uncertain bool
		snap      *reconcile.ClaimObservation
		want      string
	}{
		{"eligible no entry", w, claimData, nil, false, snapWith("", false), ""},
		{"maintenance first", func() protocol.DesiredWorker {
			x := w
			x.DindMaintenance = &protocol.DindMaintenance{Phase: "stopping"}
			return x
		}(), claimData, &rollEntry{}, true, nil, "maintenance"},
		{"uncertain before admission", w, claimData, &rollEntry{}, true, nil, "roll-uncertain"},
		{"unadmitted", w, claimDinD, &rollEntry{}, false, nil, "roll-admission"},
		{"dind ignores reservation", w, claimDinD, &rollEntry{admitted: true, blocked: "x", marker: open()}, false, nil, ""},
		{"blocked", w, claimData, &rollEntry{admitted: true, blocked: "x"}, false, nil, "roll-reservation"},
		{"open marker", w, claimData, &rollEntry{admitted: true, marker: open()}, false, nil, "roll-reservation"},
		{"completed marker", w, claimData, &rollEntry{admitted: true, marker: done()}, false, nil, ""},
		{"malformed annotation", w, claimData, &rollEntry{admitted: true}, false, snapWith("{bad", true), "roll-reservation"},
		{"open annotation", w, claimData, &rollEntry{admitted: true}, false, snapWith(openRaw, true), "roll-reservation"},
		{"completed annotation", w, claimData, &rollEntry{admitted: true}, false, snapWith(completedRaw, true), ""},
		{"raw reservation empty annotation", w, claimData, &rollEntry{admitted: true, rawReservation: true}, false, snapWith("", true), "roll-reservation"},
		{"raw reservation absent annotation", w, claimData, &rollEntry{admitted: true, rawReservation: true}, false, snapWith("", false), "roll-reservation"},
		{"raw reservation completed annotation", w, claimData, &rollEntry{admitted: true, rawReservation: true}, false, snapWith(completedRaw, true), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := claimGate(c.w, c.kind, c.e, c.uncertain, c.snap); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestExpandRollSnapshotIsTheSoleSourceForData(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		mut    func(e *rollEntry)
		reason string
		patch  bool
	}{
		"roll snapshot larger than observe (manual growth)": {mut: func(e *rollEntry) {
			e.claim.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("60Gi")
		}},
		"roll snapshot pending": {mut: func(e *rollEntry) { e.claim.Status.Phase = corev1.ClaimPending }},
		"different uid":         {mut: func(e *rollEntry) { e.claim.UID = "someone-else" }, reason: "claim-identity-changed"},
		"claim nil":             {mut: func(e *rollEntry) { e.claim = nil }, reason: "claim-identity-changed"},
		"matching snapshot":     {mut: func(e *rollEntry) {}, patch: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newExpandFx(t, expandOpt{expand: boolp(true), dindReq: "50Gi"})
			spec := testSpec(t, f.w.Template, f.w.Size)
			o, err := f.m.Observe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			tick, _ := f.m.planRoll(ctx, []protocol.DesiredWorker{f.w}, map[string]reconcile.ObservedWorker{o[0].ID: o[0]})
			e := tick.entries[f.w.ID]
			if e == nil || !e.admitted {
				t.Fatalf("fixture not admitted: %+v", e)
			}
			tc.mut(e)
			ctx2 := context.WithValue(ctx, rollTickKey{}, tick)
			f.client.ClearActions()
			f.m.expandClaims(ctx2, f.w, e.o, f.ns, spec)
			names, _ := pvcPatches(f.client)
			if tc.patch != (len(names) == 1) {
				t.Fatalf("patches %v want patch=%v\n%s", names, tc.patch, f.logs)
			}
			if tc.patch && (e.claim == nil || e.claim.Spec.Resources.Requests.Storage().Cmp(resource.MustParse("50Gi")) != 0) {
				t.Fatal("e.claim not refreshed from the patch response")
			}
			if reason := func() string {
				_, r := claimDecision(f.w, claimData, tick, e, e.o)
				return r
			}(); tc.reason != "" && reason != tc.reason {
				t.Fatalf("decision reason %q want %q", reason, tc.reason)
			}
		})
	}
}

func TestExpandThenMirrorRollUsesRefreshedClaim(t *testing.T) {
	ctx := context.Background()
	f := newExpandFx(t, expandOpt{expand: boolp(true), dindReq: "50Gi"})
	spec := testSpec(t, f.w.Template, f.w.Size)
	o, err := f.m.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tick, _ := f.m.planRoll(ctx, []protocol.DesiredWorker{f.w}, map[string]reconcile.ObservedWorker{o[0].ID: o[0]})
	e := tick.entries[f.w.ID]
	ctx2 := context.WithValue(ctx, rollTickKey{}, tick)
	f.m.expandClaims(ctx2, f.w, e.o, f.ns, spec)
	marker := &rollMarker{Version: 1, Hash: strings.Repeat("a", 64), Image: "img", TargetVersion: "1.0.0", Coverage: rollCoverage{Owner: "owner-A", Lane: f.ns, Scheduling: []string{}, Protocol: []string{}}, Nonce: "n"}
	if err := f.m.mirrorRoll(ctx2, e, marker); err != nil {
		t.Fatalf("mirrorRoll after expansion used a stale claim identity: %v", err)
	}
}

func TestExpandDeferralReporter(t *testing.T) {
	type tc struct {
		name  string
		opt   expandOpt
		extra func(f *expandFx)
		want  string
	}
	cases := []tc{
		{name: "maintenance", opt: expandOpt{expand: boolp(true), workerFn: func(w *protocol.DesiredWorker) {
			w.DindMaintenance = &protocol.DindMaintenance{ID: "op", Phase: "requested"}
		}}, want: "pvc-expand-deferred worker=w1 pvc=" + dataPVCName("w1") + " reason=maintenance"},
		{name: "reservation", opt: expandOpt{expand: boolp(true)}, extra: func(f *expandFx) {
			d := podTemplateOf(t, f.client, f.ns)
			d.Annotations = map[string]string{annotationRoll: completedMarker(t, f, false)}
			if _, err := f.client.AppsV1().Deployments(f.ns).Update(context.Background(), d, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		}, want: "reason=roll-reservation"},
		{name: "resize pending condition unadmitted", opt: expandOpt{expand: boolp(true), dataReq: "50Gi", dindReq: "50Gi", claimFn: func(n string, p *corev1.PersistentVolumeClaim) {
			p.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{Type: corev1.PersistentVolumeClaimFileSystemResizePending, Status: corev1.ConditionTrue}}
		}}, want: "pvc-resize-pending worker=w1 pvc=" + dataPVCName("w1") + " reason=condition-FileSystemResizePending"},
		{name: "resize failed", opt: expandOpt{expand: boolp(true), dataReq: "50Gi", dindReq: "50Gi", claimFn: func(n string, p *corev1.PersistentVolumeClaim) {
			p.Status.AllocatedResourceStatuses = map[corev1.ResourceName]corev1.ClaimResourceStatus{corev1.ResourceStorage: corev1.PersistentVolumeClaimControllerResizeInfeasible}
		}}, want: "pvc-resize-failed worker=w1 pvc=" + dataPVCName("w1") + " reason=status-ControllerResizeInfeasible"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newExpandFx(t, c.opt)
			if c.extra != nil {
				c.extra(f)
			}
			f.client.ClearActions()
			_ = f.tick()
			if !strings.Contains(f.logs.String(), c.want) {
				t.Fatalf("log lacks %q:\n%s", c.want, f.logs)
			}
			for _, n := range func() []string { n, _ := pvcPatches(f.client); return n }() {
				if strings.HasPrefix(n, dataPVCName(f.w.ID)) {
					t.Fatalf("deferral patched /data: %s", n)
				}
			}
			// Deduped: an identical second tick does not repeat the token.
			before := strings.Count(f.logs.String(), c.want)
			_ = f.tick()
			if after := strings.Count(f.logs.String(), c.want); after != before {
				t.Fatalf("token repeated: %d -> %d", before, after)
			}
		})
	}
}

// A drifted peer in the same pool keeps the at-target worker unadmitted, so the
// reporter shows roll-admission for it and nothing at all is mutated. An at-target
// claim with a FileSystemResizePending condition is reported pending independently.
func TestExpandDeferredWhilePoolDrains(t *testing.T) {
	ctx := context.Background()
	f := newExpandFx(t, expandOpt{expand: boolp(true), claimFn: func(n string, p *corev1.PersistentVolumeClaim) {
		if strings.HasSuffix(n, "-dind-data") {
			p.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{Type: corev1.PersistentVolumeClaimFileSystemResizePending, Status: corev1.ConditionTrue}}
		}
	}})
	peer := evidence(desiredDocker("w2"), m5Now)
	peer.Size = "l"
	peerDep := RenderDeployment(f.cfg, peer, testSpec(t, peer.Template, peer.Size))
	peerDep.Spec.Template.Annotations[AnnotationSpecHash] = "old-release-hash"
	peerDep.UID, peerDep.ResourceVersion = "uid-peer", "3"
	if _, err := f.client.AppsV1().Deployments(f.ns).Create(ctx, peerDep, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
	o, err := f.m.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.Reconcile(ctx, []protocol.DesiredWorker{f.w, peer}, o); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	logs := f.logs.String()
	for _, want := range []string{
		"pvc-expand-deferred worker=w1 pvc=" + dataPVCName("w1") + " reason=roll-admission",
		"pvc-expand-deferred worker=w1 pvc=" + dindDataPVCName("w1") + " reason=roll-admission",
		"pvc-resize-pending worker=w1 pvc=" + dindDataPVCName("w1") + " reason=condition-FileSystemResizePending",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("log lacks %q:\n%s", want, logs)
		}
	}
	for _, m := range mutations(f.client) {
		if strings.Contains(m, "persistentvolumeclaims") {
			t.Fatalf("expanded while unadmitted: %v", mutations(f.client))
		}
	}
}

func TestExpandDeferredWhenLaneIsUncertain(t *testing.T) {
	ctx := context.Background()
	f := newExpandFx(t, expandOpt{expand: boolp(true)})
	// A pending migration drain for an absent worker whose pod is still visible keeps
	// the owner/lane fenced: unknown mutations require a new observation first.
	ghost := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ghost-pod", Namespace: f.ns, UID: "ghost-pod", Labels: objectLabels("ghost")}}
	if _, err := f.client.CoreV1().Pods(f.ns).Create(ctx, ghost, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.m.migrationPending = map[string]rollCoverage{"ghost": {Owner: "owner-A", Lane: f.ns}}
	f.client.ClearActions()
	_ = f.tick()
	if !strings.Contains(f.logs.String(), "pvc-expand-deferred worker=w1 pvc="+dataPVCName("w1")+" reason=roll-uncertain") {
		t.Fatalf("no roll-uncertain deferral:\n%s", f.logs)
	}
	if names, _ := pvcPatches(f.client); len(names) != 0 {
		t.Fatalf("expanded in an uncertain lane: %v", names)
	}
}

func TestExpandIdleRollForOwnReasonsKeepsClaims(t *testing.T) {
	// Image drift on an idle worker: the roll replaces the pod via a template patch
	// and must never delete or recreate a claim.
	f := newExpandFx(t, expandOpt{expand: boolp(true), dataReq: "50Gi", dindReq: "50Gi"})
	dataUID := f.claim(dataPVCName(f.w.ID)).UID
	dindUID := f.claim(dindDataPVCName(f.w.ID)).UID
	d := podTemplateOf(t, f.client, f.ns)
	d.Spec.Template.Annotations[AnnotationSpecHash] = "old-release-hash"
	if _, err := f.client.AppsV1().Deployments(f.ns).Update(context.Background(), d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
	_ = f.tick()
	for _, m := range mutations(f.client) {
		if strings.HasPrefix(m, "delete persistentvolumeclaims") || strings.HasPrefix(m, "create persistentvolumeclaims") {
			t.Fatalf("roll touched a claim: %s", m)
		}
	}
	if f.claim(dataPVCName(f.w.ID)).UID != dataUID || f.claim(dindDataPVCName(f.w.ID)).UID != dindUID {
		t.Fatal("claim UID changed")
	}
}

func TestExpandActionLogInvariants(t *testing.T) {
	f := newExpandFx(t, expandOpt{expand: boolp(true)})
	f.mustTick()
	f.mustTick()
	workerNamespaces := map[string]bool{f.cfg.Namespace: true, f.cfg.DockerNamespace: true}
	for _, a := range f.client.Actions() {
		if a.GetResource().Resource == "storageclasses" && a.GetVerb() != "get" {
			t.Fatalf("storageclasses verb %s", a.GetVerb())
		}
		if a.GetResource().Resource == "persistentvolumeclaims" && a.GetVerb() == "patch" && !workerNamespaces[a.GetNamespace()] {
			t.Fatalf("PVC patch outside worker namespaces: %s", a.GetNamespace())
		}
		if a.GetResource().Resource == "secrets" && (a.GetVerb() == "get" || a.GetVerb() == "list") {
			t.Fatal("secret read")
		}
	}
	// Memoised per tick: both claims share one class, one lookup in the first tick.
	if n := storageClassGets(f.client); n != 1 {
		t.Fatalf("want exactly one StorageClass get across two ticks, got %d", n)
	}
}

func TestExpandPatchBodyIsJSON(t *testing.T) {
	f := newExpandFx(t, expandOpt{expand: boolp(true)})
	f.mustTick()
	_, bodies := pvcPatches(f.client)
	for _, b := range bodies {
		var v map[string]map[string]any
		if err := json.Unmarshal([]byte(b), &v); err != nil {
			t.Fatalf("patch body is not JSON: %v: %s", err, b)
		}
	}
}

func TestExpandSecondUpsizeOfSameClaimLogsAgain(t *testing.T) {
	f := newExpandFx(t, expandOpt{expand: boolp(true)})
	f.mustTick() // converge both claims to 50Gi, logs the first request
	f.mustTick() // observed at target: the reporter forgets the request
	f.m.cfg.DinDDataSize = "60Gi"
	f.mustTick()
	if f.request(dindDataPVCName(f.w.ID)) != "60Gi" {
		t.Fatalf("dind claim not raised to 60Gi: %s", f.request(dindDataPVCName(f.w.ID)))
	}
	needle := "pvc-expand-requested worker=w1 pvc=" + dindDataPVCName("w1")
	if n := strings.Count(f.logs.String(), needle); n != 2 {
		t.Fatalf("want 2 requested lines for the dind claim, got %d:\n%s", n, f.logs)
	}
}

func TestExpandSecondResizeOfSameClaimLogsAgain(t *testing.T) {
	f := newExpandFx(t, expandOpt{expand: boolp(true), dataReq: "50Gi", dindReq: "50Gi"})
	setPending := func(on bool) {
		t.Helper()
		p := f.claim(dataPVCName(f.w.ID))
		p.Status.Conditions = nil
		if on {
			p.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{Type: corev1.PersistentVolumeClaimFileSystemResizePending, Status: corev1.ConditionTrue}}
		}
		if _, err := f.client.CoreV1().PersistentVolumeClaims(f.ns).UpdateStatus(context.Background(), p, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	setPending(true)
	f.mustTick() // resize pending: logged once
	f.mustTick() // identical state: deduped
	setPending(false)
	f.mustTick() // resize finished: the reporter forgets the pending line
	setPending(true)
	f.mustTick() // a later resize of the same claim is reported again
	needle := "pvc-resize-pending worker=w1 pvc=" + dataPVCName("w1")
	if n := strings.Count(f.logs.String(), needle); n != 2 {
		t.Fatalf("want 2 resize-pending lines for the data claim, got %d:\n%s", n, f.logs)
	}
}

func TestExpandDropsDedupStateForUnobservedClaims(t *testing.T) {
	f := newExpandFx(t, expandOpt{expand: boolp(true)})
	gone := f.ns + "/" + dindDataPVCName("gone") + "#expand"
	f.m.claimLogMu.Lock()
	f.m.claimLog = map[string]string{gone: "pvc-expand-deferred|maintenance"}
	f.m.claimLogMu.Unlock()
	f.mustTick()
	f.m.claimLogMu.Lock()
	_, kept := f.m.claimLog[gone]
	f.m.claimLogMu.Unlock()
	if kept {
		t.Fatal("dedup state for a claim that is no longer observed was kept")
	}
}
