package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// Lost-canary recovery (issue #4466). These tests drive a real cohort through the
// harness (canary promoted, participants reserved), then reshape the cluster into a
// cohort whose canary evidence is gone, and assert what the controller writes.

const (
	orphanHold    = "earlier participant may hold an unresolved adoption request"
	orphanAdopted = "canary adopted from proven participant; re-observation required"
	orphanNoCan   = "participant reservation without canary; explicit recovery required"
)

type orphanCohort struct {
	m       *Materializer
	c       *fake.Clientset
	base    k8stesting.Reactor // enforcingClient's deployment patch reactor, before any test reactor
	cordon  *fakeCordoner
	workers []protocol.DesiredWorker
}

// newOrphanCohort rolls an n-worker cohort until w1 is a promoted canary and every
// other worker holds an incomplete participant reservation with its template patched.
func newOrphanCohort(t *testing.T, n, concurrency int) *orphanCohort {
	t.Helper()
	m, c, workers, cordon := matrixFixture(t, n)
	m.cfg.RollConcurrency = concurrency
	o := &orphanCohort{m: m, c: c, base: c.ReactionChain[1], cordon: cordon, workers: workers}
	rollTickTest(t, m, workers)
	removeOldPod(t, c, "w1")
	currentProof(t, c, &o.workers[0], m5Now)
	rollTickTest(t, m, workers)
	if mk := o.marker(t, "w1"); mk == nil || !mk.Canary || mk.Completed {
		t.Fatalf("w1 is not the incomplete canary: %+v", mk)
	}
	for i := 2; i <= n; i++ {
		if mk := o.marker(t, fmt.Sprintf("w%d", i)); mk == nil || mk.Canary || mk.Completed {
			t.Fatalf("w%d is not an incomplete participant: %+v", i, mk)
		}
	}
	return o
}

func (o *orphanCohort) at(id string) *protocol.DesiredWorker {
	var i int
	_, _ = fmt.Sscanf(id, "w%d", &i)
	return &o.workers[i-1]
}

func (o *orphanCohort) dep(t *testing.T, id string) *appsv1.Deployment {
	t.Helper()
	d, err := o.c.AppsV1().Deployments(o.m.cfg.Namespace).Get(context.Background(), deploymentName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (o *orphanCohort) marker(t *testing.T, id string) *rollMarker {
	t.Helper()
	mk, err := decodeRoll(o.dep(t, id).Annotations[annotationRoll])
	if err != nil {
		t.Fatal(err)
	}
	return mk
}

// edit rewrites the Deployment out of band and advances its resourceVersion, as any
// apiserver update does, so a delayed conditioned patch can no longer commit.
func (o *orphanCohort) edit(t *testing.T, id string, f func(*appsv1.Deployment)) {
	t.Helper()
	d := o.dep(t, id)
	f(d)
	d.ResourceVersion += "x"
	if err := o.c.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("deployments"), d, d.Namespace); err != nil {
		t.Fatal(err)
	}
}

func (o *orphanCohort) editClaim(t *testing.T, name string, f func(*corev1.PersistentVolumeClaim)) {
	t.Helper()
	p, err := o.c.CoreV1().PersistentVolumeClaims(o.m.cfg.Namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f(p)
	p.ResourceVersion += "x"
	if err := o.c.Tracker().Update(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), p, p.Namespace); err != nil {
		t.Fatal(err)
	}
}

// prove gives id a fresh current-incarnation registration on its replacement pod.
func (o *orphanCohort) prove(t *testing.T, id string) {
	t.Helper()
	removeOldPod(t, o.c, id)
	currentProof(t, o.c, o.at(id), m5Now)
}

// recycle leaves id as a recycled participant: scaled to zero, pod gone, claims gone.
func (o *orphanCohort) recycle(t *testing.T, id string) {
	t.Helper()
	zero := int32(0)
	o.edit(t, id, func(d *appsv1.Deployment) { d.Spec.Replicas = &zero })
	removeOldPod(t, o.c, id)
	for _, name := range []string{nixPVCName(id), dataPVCName(id)} {
		if err := o.c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), o.m.cfg.Namespace, name); err != nil {
			t.Fatal(err)
		}
	}
}

// eraseCanary models an external reset of the canary's reservation annotation.
func (o *orphanCohort) eraseCanary(t *testing.T) {
	t.Helper()
	o.edit(t, "w1", func(d *appsv1.Deployment) { delete(d.Annotations, annotationRoll) })
}

func (o *orphanCohort) tick(t *testing.T) error {
	t.Helper()
	obs, err := o.m.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return o.m.Reconcile(context.Background(), o.workers, obs)
}

func (o *orphanCohort) mustTick(t *testing.T) {
	t.Helper()
	if err := o.tick(t); err != nil {
		t.Fatal(err)
	}
}

func (o *orphanCohort) canaries(t *testing.T) []string {
	t.Helper()
	list, err := o.c.AppsV1().Deployments(o.m.cfg.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range list.Items {
		if mk, err := decodeRoll(d.Annotations[annotationRoll]); err == nil && mk != nil && mk.Canary {
			out = append(out, d.Name)
		}
	}
	sort.Strings(out)
	return out
}

func (o *orphanCohort) pacing(id string) (state, blocker, reason, canary string) {
	p := o.m.PacingOutcomes()[id]
	if p == nil {
		return "<none>", "", "", ""
	}
	return p.State, txt(p.BlockingWorkerID), txt(p.BlockingReason), txt(p.CanaryID)
}

func txt(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// actionTrace lists the tick's writes as "verb resource name [spec|metadata]".
func actionTrace(c *fake.Clientset) []string {
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
		if cr, ok := a.(k8stesting.CreateAction); ok {
			if acc, err := meta.Accessor(cr.GetObject()); err == nil {
				name = acc.GetName()
			}
		}
		line := a.GetVerb() + " " + a.GetResource().Resource + " " + name
		if p, ok := a.(k8stesting.PatchAction); ok {
			var body map[string]json.RawMessage
			_ = json.Unmarshal(p.GetPatch(), &body)
			if _, ok := body["spec"]; ok {
				line += " spec"
			} else {
				line += " metadata"
			}
		}
		out = append(out, line)
	}
	return out
}

// isAdoptionPatch reports a Deployment reservation patch that sets the canary bit.
func isAdoptionPatch(a k8stesting.Action) bool {
	p, ok := a.(k8stesting.PatchAction)
	if !ok || a.GetResource().Resource != "deployments" {
		return false
	}
	var body struct {
		Metadata struct {
			Annotations map[string]*string
		}
	}
	if json.Unmarshal(p.GetPatch(), &body) != nil || body.Metadata.Annotations[annotationRoll] == nil {
		return false
	}
	mk, err := decodeRoll(*body.Metadata.Annotations[annotationRoll])
	return err == nil && mk != nil && mk.Canary
}

func adoptionPatches(c *fake.Clientset) []string {
	var out []string
	for _, a := range c.Actions() {
		if isAdoptionPatch(a) {
			out = append(out, a.(k8stesting.PatchAction).GetName())
		}
	}
	return out
}

func TestOrphanRollCohortAdoptsProvenParticipant(t *testing.T) {
	for _, tc := range []struct{ name, zero, proven string }{
		{"recycled participant has the largest id", "w3", "w2"},
		{"recycled participant has the smallest id", "w2", "w3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOrphanCohort(t, 3, 2)
			o.recycle(t, tc.zero)
			o.prove(t, tc.proven)
			o.eraseCanary(t)
			o.at(tc.proven).Busy = true
			pre, preSpec := o.marker(t, tc.proven), o.dep(t, tc.proven).Spec.DeepCopy()
			podBefore, err := o.c.CoreV1().Pods(o.m.cfg.Namespace).Get(context.Background(), "pod-"+tc.proven, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			drainBefore, clearBefore := append([]string{}, o.cordon.calls...), append([]string{}, o.cordon.clearCalls...)
			o.c.ClearActions()

			o.mustTick(t)
			podAfter, err := o.c.CoreV1().Pods(o.m.cfg.Namespace).Get(context.Background(), "pod-"+tc.proven, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(podBefore, podAfter) {
				t.Fatalf("recovery changed the busy provider's pod: before %+v after %+v", podBefore, podAfter)
			}
			if !equalStrings(o.cordon.calls, drainBefore) || !equalStrings(o.cordon.clearCalls, clearBefore) {
				t.Fatalf("recovery tick cordoned or uncordoned: drain %v -> %v, clear %v -> %v", drainBefore, o.cordon.calls, clearBefore, o.cordon.clearCalls)
			}
			want := []string{"patch deployments " + deploymentName(tc.proven) + " metadata"}
			if got := actionTrace(o.c); !equalStrings(got, want) {
				t.Fatalf("recovery tick wrote %v, want exactly %v", got, want)
			}
			wantMarker := *pre
			wantMarker.Canary = true
			if got := o.marker(t, tc.proven); !reflect.DeepEqual(*got, wantMarker) {
				t.Fatalf("adopted marker %+v, want %+v", *got, wantMarker)
			}
			if got := o.dep(t, tc.proven).Spec; !reflect.DeepEqual(got, *preSpec) {
				t.Fatalf("adoption changed the Deployment spec: %+v", got)
			}
			if o.marker(t, "w1") != nil {
				t.Fatal("recovery rewrote the lost canary's Deployment")
			}
			for _, id := range []string{"w1", "w2", "w3"} {
				state, blocker, reason, canary := o.pacing(id)
				if state != "held" || blocker != tc.proven || reason != orphanAdopted || canary != tc.proven {
					t.Fatalf("%s pacing = %s/%s/%q/%s", id, state, blocker, reason, canary)
				}
			}

			// The re-observed cohort resumes: claims first, then the replica restore.
			o.c.ClearActions()
			o.mustTick(t)
			wantClaims := []string{"create persistentvolumeclaims " + dataPVCName(tc.zero), "create persistentvolumeclaims " + nixPVCName(tc.zero)}
			if got := actionTrace(o.c); !equalStrings(got, wantClaims) {
				t.Fatalf("second tick wrote %v, want %v", got, wantClaims)
			}
			o.c.ClearActions()
			o.mustTick(t)
			if got, want := actionTrace(o.c), []string{"patch deployments " + deploymentName(tc.zero) + " spec"}; !equalStrings(got, want) {
				t.Fatalf("third tick wrote %v, want %v", got, want)
			}
			if r := o.dep(t, tc.zero).Spec.Replicas; r == nil || *r != 1 {
				t.Fatalf("recycled participant replicas = %v, want 1", r)
			}
			if got := o.canaries(t); !equalStrings(got, []string{deploymentName(tc.proven)}) {
				t.Fatalf("canaries = %v", got)
			}
		})
	}
}

func TestOrphanRollCohortRecoveryRequiresIncarnationProof(t *testing.T) {
	// Each case reshapes a cohort whose w2 is proven (so the positive control adopts it)
	// and whose w3 is an unproven in-flight participant, then runs one tick.
	type shape struct {
		name       string
		mutate     func(t *testing.T, o *orphanCohort)
		listFails  string   // resource whose LIST fails
		canaries   []string // canary Deployments allowed to exist afterwards
		wantReason string   // pacing reason of every blocked participant, when set
		wantBlock  string
		adopts     bool
	}
	past := m5Now.Add(-24 * 3600e9)
	evidenceOf := func(o *orphanCohort, id string) *protocol.RollEvidence { return o.at(id).RollEvidence }
	cases := []shape{
		{name: "positive control adopts", adopts: true},
		{name: "ready only: no fresh registration evidence", mutate: func(t *testing.T, o *orphanCohort) { o.at("w2").RollEvidence = onlineEvidence(m5Now) }},
		{name: "wrong pod uid", mutate: func(t *testing.T, o *orphanCohort) { evidenceOf(o, "w2").PodUID = token("someone-else") }},
		{name: "foreign replicaset ancestry", mutate: func(t *testing.T, o *orphanCohort) {
			rs, err := o.c.AppsV1().ReplicaSets(o.m.cfg.Namespace).Get(context.Background(), "rs-w2", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			rs.OwnerReferences[0].UID = "retired-deployment"
			if err := o.c.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("replicasets"), rs, rs.Namespace); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "template hash mismatch", mutate: func(t *testing.T, o *orphanCohort) {
			o.edit(t, "w2", func(d *appsv1.Deployment) { d.Spec.Template.Annotations[AnnotationSpecHash] = "another-hash" })
		}},
		{name: "image mismatch", mutate: func(t *testing.T, o *orphanCohort) {
			o.edit(t, "w2", func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Image = "worker:other" })
		}},
		{name: "registered version mismatch", mutate: func(t *testing.T, o *orphanCohort) { evidenceOf(o, "w2").RegisteredVersion = token("9.9.9") }},
		{name: "unchanged nonce", mutate: func(t *testing.T, o *orphanCohort) { evidenceOf(o, "w2").RegisterNonce = token("prior-registration") }},
		{name: "stale heartbeat", mutate: func(t *testing.T, o *orphanCohort) {
			evidenceOf(o, "w2").HeartbeatAt = &past
			evidenceOf(o, "w2").ValidUntil = past
		}},
		{name: "missing heartbeat", mutate: func(t *testing.T, o *orphanCohort) { evidenceOf(o, "w2").HeartbeatAt = nil }},
		{name: "owner mismatch", mutate: func(t *testing.T, o *orphanCohort) { evidenceOf(o, "w2").OwnerID = "owner-B" }},
		{name: "coverage lost: ordinary coverage withdrawn", mutate: func(t *testing.T, o *orphanCohort) {
			evidenceOf(o, "w2").OrdinaryCoverage = false
		}},
		{name: "disk pressure", mutate: func(t *testing.T, o *orphanCohort) { o.at("w2").DiskPressure = true }},
		{name: "maintenance", mutate: func(t *testing.T, o *orphanCohort) {
			o.at("w2").DindMaintenance = &protocol.DindMaintenance{ID: "op", Phase: "requested"}
		}},
		{name: "custody with drain", mutate: func(t *testing.T, o *orphanCohort) {
			o.at("w2").CustodyHeld = true
			o.at("w2").DrainingSince, o.at("w2").DrainingReason = &past, token("image_roll")
		}},
		{name: "migration fence: unresolved migration cordon in the lane", mutate: func(t *testing.T, o *orphanCohort) {
			// An unconfirmed migration cordon on w3 stays pending (custody held, a drain
			// reason without a drain time), so prepareMigrationDrains fences the lane.
			o.at("w3").CustodyHeld = true
			o.at("w3").DrainingReason = token("image_roll")
			o.m.migrationPending = map[string]rollCoverage{"w3": {Owner: "owner-A", Lane: o.m.cfg.Namespace}}
		}},
		{name: "incomplete observation: replicaset list fails", listFails: "replicasets"},
		{name: "incomplete observation: pod list fails", listFails: "pods"},
		{name: "existing canary", canaries: []string{"uzi-hw-w3"}, mutate: func(t *testing.T, o *orphanCohort) {
			mk := o.marker(t, "w3")
			mk.Canary = true
			raw, _ := json.Marshal(mk)
			o.edit(t, "w3", func(d *appsv1.Deployment) { d.Annotations[annotationRoll] = string(raw) })
		}},
		{name: "duplicate canary", canaries: []string{"uzi-hw-w1", "uzi-hw-w3"}, mutate: func(t *testing.T, o *orphanCohort) {
			for _, id := range []string{"w1", "w3"} {
				mk := o.marker(t, "w2")
				mk.Canary = true
				raw, _ := json.Marshal(mk)
				o.edit(t, id, func(d *appsv1.Deployment) {
					if d.Annotations == nil {
						d.Annotations = map[string]string{}
					}
					d.Annotations[annotationRoll] = string(raw)
				})
			}
		}},
		{name: "hibernating canary", canaries: []string{"uzi-hw-w1"}, mutate: func(t *testing.T, o *orphanCohort) {
			mk := o.marker(t, "w2")
			mk.Canary = true
			raw, _ := json.Marshal(mk)
			o.edit(t, "w1", func(d *appsv1.Deployment) { d.Annotations[annotationRoll] = string(raw) })
			o.at("w1").Hibernation = &protocol.Hibernation{State: protocol.HibernationPreparing, OpID: "op-1"}
		}},
		{name: "unplanned same-namespace worker", mutate: func(t *testing.T, o *orphanCohort) {
			bad := desired("w9")
			bad.Template = "no-such-template"
			o.workers = append(o.workers, bad)
		}},
		{name: "stray annotated deployment", mutate: func(t *testing.T, o *orphanCohort) {
			ghost := o.dep(t, "w2").DeepCopy()
			ghost.Name, ghost.UID, ghost.ResourceVersion = "uzi-hw-ghost", "ghost-uid", ""
			ghost.Labels = objectLabels("ghost")
			if _, err := o.c.AppsV1().Deployments(ghost.Namespace).Create(context.Background(), ghost, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "two deployments for one worker, duplicate listed first", mutate: func(t *testing.T, o *orphanCohort) { o.duplicate(t, "w2", "aaa-dup") }},
		{name: "two deployments for one worker, duplicate listed last", mutate: func(t *testing.T, o *orphanCohort) { o.duplicate(t, "w2", "zzz-dup") }},
		{name: "two replicas with one proven pod", mutate: func(t *testing.T, o *orphanCohort) {
			two := int32(2)
			o.edit(t, "w2", func(d *appsv1.Deployment) { d.Spec.Replicas = &two })
		}},
		{name: "malformed marker on another participant", mutate: func(t *testing.T, o *orphanCohort) {
			o.edit(t, "w3", func(d *appsv1.Deployment) { d.Annotations[annotationRoll] = "{not json" })
		}},
		{name: "positive control with a stricter reservation coverage adopts", adopts: true, mutate: func(t *testing.T, o *orphanCohort) {
			o.requireCoverage(t, []string{"gpu"}, []string{"proto-1"}, true)
		}},
		{name: "frozen scheduling capability missing from evidence", mutate: func(t *testing.T, o *orphanCohort) {
			o.requireCoverage(t, []string{"gpu"}, []string{}, false)
		}},
		{name: "frozen protocol capability missing from evidence", mutate: func(t *testing.T, o *orphanCohort) {
			o.requireCoverage(t, []string{}, []string{"proto-1"}, false)
		}},
		{name: "reservation lane differs from the observed namespace", mutate: func(t *testing.T, o *orphanCohort) {
			mk := o.marker(t, "w2")
			mk.Coverage.Lane = "another-namespace"
			raw, _ := json.Marshal(mk)
			o.edit(t, "w2", func(d *appsv1.Deployment) { d.Annotations[annotationRoll] = string(raw) })
		}},
		{name: "out-of-pool canary without coverage evidence", canaries: []string{"uzi-hw-w9"}, mutate: func(t *testing.T, o *orphanCohort) {
			o.outsider(t, true, false)
		}},
		{name: "out-of-pool incomplete reservation without coverage evidence", mutate: func(t *testing.T, o *orphanCohort) {
			o.outsider(t, false, false)
		}},
		{name: "out-of-pool incomplete reservation in another coverage pool of the same owner", mutate: func(t *testing.T, o *orphanCohort) {
			o.outsider(t, false, true)
		}},
		{name: "smaller-id participant with stale evidence only", wantReason: orphanHold, wantBlock: "w2", mutate: func(t *testing.T, o *orphanCohort) {
			o.at("w2").RollEvidence = onlineEvidence(m5Now)
			o.prove(t, "w3")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := newOrphanCohort(t, 3, 2)
			o.prove(t, "w2")
			o.eraseCanary(t)
			if tc.listFails != "" {
				o.c.PrependReactor("list", tc.listFails, func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, fmt.Errorf("%s list denied", tc.listFails)
				})
			}
			if tc.mutate != nil {
				tc.mutate(t, o)
			}
			o.c.ClearActions()
			_ = o.tick(t)
			adopted := adoptionPatches(o.c)
			if tc.adopts {
				if !equalStrings(adopted, []string{"uzi-hw-w2"}) || !equalStrings(o.canaries(t), []string{"uzi-hw-w2"}) {
					t.Fatalf("positive control did not adopt w2: patches=%v canaries=%v", adopted, o.canaries(t))
				}
				return
			}
			if len(adopted) != 0 {
				t.Fatalf("adopted without a clean proof: %v", adopted)
			}
			if got := o.canaries(t); !equalStrings(got, tc.canaries) {
				t.Fatalf("canaries = %v, want %v", got, tc.canaries)
			}
			if tc.wantReason != "" {
				for _, id := range []string{"w1", "w2", "w3"} {
					if _, blocker, reason, _ := o.pacing(id); blocker != tc.wantBlock || reason != tc.wantReason {
						t.Fatalf("%s blocked by %q for %q, want %q for %q", id, blocker, reason, tc.wantBlock, tc.wantReason)
					}
				}
			}
		})
	}
}

// requireCoverage freezes the given capability requirements into w2's and w3's
// reservations; w2's evidence advertises them only when w2Has is set.
func (o *orphanCohort) requireCoverage(t *testing.T, sched, proto []string, w2Has bool) {
	t.Helper()
	for _, id := range []string{"w2", "w3"} {
		mk := o.marker(t, id)
		mk.Coverage.Scheduling, mk.Coverage.Protocol = sched, proto
		raw, _ := json.Marshal(mk)
		o.edit(t, id, func(d *appsv1.Deployment) { d.Annotations[annotationRoll] = string(raw) })
		// The marker must stay well-formed, or the case tests decoding, not coverage.
		if got := o.marker(t, id); got == nil || !reflect.DeepEqual(got.Coverage.Scheduling, sched) || !reflect.DeepEqual(got.Coverage.Protocol, proto) {
			t.Fatalf("%s coverage reservation did not round-trip: %+v", id, got)
		}
	}
	if w2Has {
		ev := o.at("w2").RollEvidence
		ev.SchedulingCapabilities = append([]string{}, sched...)
		ev.ProtocolCapabilities = append([]string{}, proto...)
	}
}

// outsider adds a same-owner, same-namespace worker w9 outside the pool, carrying an
// incomplete reservation (a canary one when canary is set). With otherPool it has
// coverage evidence and a reservation under a different capability set, forming its own
// pool; without it, it has no coverage evidence at all.
func (o *orphanCohort) outsider(t *testing.T, canary, otherPool bool) {
	t.Helper()
	mk := o.marker(t, "w2")
	mk.Canary = canary
	w := desired("w9")
	if otherPool {
		w = evidence(w, m5Now)
		w.RollEvidence.SchedulingCapabilities = []string{"zzz"}
		mk.Coverage.Scheduling = []string{"zzz"}
		// Keep the reservation current for w9's own target so its pool leaves it alone.
		spec, err := o.m.resolver.Resolve(w.Template, w.Size)
		if err != nil {
			t.Fatal(err)
		}
		target := RenderDeployment(o.m.cfg, w, spec)
		mk.Hash, mk.Image = target.Spec.Template.Annotations[AnnotationSpecHash], workerImage(target)
	}
	raw, _ := json.Marshal(mk)
	dep := o.dep(t, "w2").DeepCopy()
	dep.Name, dep.UID, dep.ResourceVersion = deploymentName("w9"), "uid-w9", ""
	dep.Labels = objectLabels("w9")
	dep.Annotations[annotationRoll] = string(raw)
	if _, err := o.c.AppsV1().Deployments(dep.Namespace).Create(context.Background(), dep, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	o.workers = append(o.workers, w)
}

// duplicate adds a second Deployment of ours for the same worker id.
func (o *orphanCohort) duplicate(t *testing.T, id, name string) {
	t.Helper()
	dup := o.dep(t, id).DeepCopy()
	dup.Name, dup.UID, dup.ResourceVersion = name, types.UID("dup-uid-"+name), ""
	if _, err := o.c.AppsV1().Deployments(dup.Namespace).Create(context.Background(), dup, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A same-tick controller write on the pool's evidence leaves the observation stale:
// the adoption waits for a later tick that observed the settled state.
func TestOrphanRollCohortSameTickWritesDeferAdoption(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, o *orphanCohort)
		write string // the only write the first tick may make
		check func(t *testing.T, o *orphanCohort)
	}{
		{name: "mapping correction", write: "patch deployments uzi-hw-w3 metadata", setup: func(t *testing.T, o *orphanCohort) {
			mk := o.marker(t, "w3")
			mk.TargetVersion = "0.0.1"
			raw, _ := json.Marshal(mk)
			o.edit(t, "w3", func(d *appsv1.Deployment) { d.Annotations[annotationRoll] = string(raw) })
		}},
		{name: "retained claim mirror equal at observation", write: "patch persistentvolumeclaims uzi-hw-w3-data metadata", setup: func(t *testing.T, o *orphanCohort) {
			raw := o.dep(t, "w3").Annotations[annotationRoll]
			o.editClaim(t, dataPVCName("w3"), func(p *corev1.PersistentVolumeClaim) {
				p.Annotations = map[string]string{annotationRoll: raw}
			})
		}},
		{name: "mirror restoration", write: "patch deployments uzi-hw-w3 metadata", setup: func(t *testing.T, o *orphanCohort) {
			raw := o.dep(t, "w3").Annotations[annotationRoll]
			o.editClaim(t, dataPVCName("w3"), func(p *corev1.PersistentVolumeClaim) {
				p.Annotations = map[string]string{annotationRoll: raw}
			})
			o.edit(t, "w3", func(d *appsv1.Deployment) { delete(d.Annotations, annotationRoll) })
		}},
		{name: "image roll drain release", write: "", setup: func(t *testing.T, o *orphanCohort) {
			o.at("w2").DrainingSince, o.at("w2").DrainingReason = &m5Now, token("image_roll")
		}, check: func(t *testing.T, o *orphanCohort) {
			if got := o.cordon.clearCalls; !equalStrings(got, []string{"w2"}) {
				t.Fatalf("drain release calls = %v, want [w2]", got)
			}
			o.at("w2").DrainingSince, o.at("w2").DrainingReason = nil, nil
		}},
		{name: "pressure recycle release", write: "", setup: func(t *testing.T, o *orphanCohort) {
			o.at("w1").DrainingSince, o.at("w1").DrainingReason = &m5Now, token("pressure_recycle")
		}, check: func(t *testing.T, o *orphanCohort) {
			if got := o.cordon.clearCalls; !equalStrings(got, []string{"w1"}) {
				t.Fatalf("pressure release calls = %v, want [w1]", got)
			}
			o.at("w1").DrainingSince, o.at("w1").DrainingReason = nil, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := newOrphanCohort(t, 3, 2)
			o.prove(t, "w2")
			o.eraseCanary(t)
			tc.setup(t, o)
			o.c.ClearActions()
			_ = o.tick(t)
			var want []string
			if tc.write != "" {
				want = []string{tc.write}
			}
			if got := actionTrace(o.c); !equalStrings(got, want) {
				t.Fatalf("first tick wrote %v, want only %v", got, want)
			}
			if got := o.canaries(t); len(got) != 0 {
				t.Fatalf("adopted in the same tick as another write: %v", got)
			}
			if tc.check != nil {
				tc.check(t, o)
			}
			for i := 0; i < 4 && len(o.canaries(t)) == 0; i++ {
				_ = o.tick(t)
			}
			if got := o.canaries(t); !equalStrings(got, []string{"uzi-hw-w2"}) {
				t.Fatalf("settled observation did not adopt w2: %v", got)
			}
		})
	}
}

// Adoption requests are conditioned writes that can outlive their tick. A request lost
// before commit may land after a controller restart; the next tick's decisions must
// either hold (the request can still commit) or prove it cannot.
func TestOrphanRollCohortDelayedAdoptionAcrossRestart(t *testing.T) {
	type delayedCase struct {
		name string
		// mutate changes the cluster/API view after w2's adoption was lost.
		mutate func(t *testing.T, o *orphanCohort)
		// after the restart tick: who holds the pool, or which Deployment got adopted.
		held       bool
		wantAdopt  []string
		lateCommit bool // the withheld w2 patch still commits when released
	}
	stale := m5Now.Add(-24 * 3600e9)
	cases := []delayedCase{
		{name: "a: only w2's desired generation rotated", held: true, lateCommit: true, mutate: func(t *testing.T, o *orphanCohort) { o.at("w2").Generation++ }},
		{name: "b: w2 claim gains a mirror", held: true, lateCommit: true, mutate: func(t *testing.T, o *orphanCohort) {
			mk := o.marker(t, "w2")
			mk.Nonce = "mirror-nonce"
			raw, _ := json.Marshal(mk)
			o.editClaim(t, dataPVCName("w2"), func(p *corev1.PersistentVolumeClaim) { p.Annotations = map[string]string{annotationRoll: string(raw)} })
		}},
		{name: "c: w2 heartbeat stale", held: true, lateCommit: true, mutate: func(t *testing.T, o *orphanCohort) {
			o.at("w2").RollEvidence.HeartbeatAt = &stale
			o.at("w2").RollEvidence.ValidUntil = stale
		}},
		{name: "d: w2 scaled to zero", wantAdopt: []string{"uzi-hw-w3"}, mutate: func(t *testing.T, o *orphanCohort) {
			zero := int32(0)
			o.edit(t, "w2", func(d *appsv1.Deployment) { d.Spec.Replicas = &zero })
			if err := o.c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), o.m.cfg.Namespace, "pod-w2"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "e: w2 template image changed", wantAdopt: []string{"uzi-hw-w3"}, mutate: func(t *testing.T, o *orphanCohort) {
			o.edit(t, "w2", func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Image = "worker:other" })
		}},
		{name: "f: w2 Deployment deleted and recreated with a new uid", held: true, mutate: func(t *testing.T, o *orphanCohort) {
			d := o.dep(t, "w2").DeepCopy()
			if err := o.c.Tracker().Delete(appsv1.SchemeGroupVersion.WithResource("deployments"), d.Namespace, d.Name); err != nil {
				t.Fatal(err)
			}
			d.UID, d.ResourceVersion = "recreated-uid", "900"
			if err := o.c.Tracker().Create(appsv1.SchemeGroupVersion.WithResource("deployments"), d, d.Namespace); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// w4 stays unproven so the pool keeps an in-flight participant without a canary.
			o := newOrphanCohort(t, 4, 3)
			o.prove(t, "w2")
			o.prove(t, "w3")
			o.eraseCanary(t)
			var delayed k8stesting.Action
			o.c.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if delayed == nil && isAdoptionPatch(a) {
					delayed = a.DeepCopy()
					return true, nil, fmt.Errorf("adoption response lost before commit")
				}
				return false, nil, nil
			})
			if err := o.tick(t); err == nil {
				t.Fatal("lost adoption was reported as confirmed")
			}
			if delayed == nil || delayed.(k8stesting.PatchAction).GetName() != "uzi-hw-w2" {
				t.Fatalf("w2 (the smallest candidate) was not the adoption target: %v", delayed)
			}
			if len(o.canaries(t)) != 0 {
				t.Fatal("a withheld request committed")
			}
			tc.mutate(t, o)
			o.m = matrixRestart(o.m)
			o.c.ClearActions()
			_ = o.tick(t)
			if tc.held {
				if got := adoptionPatches(o.c); len(got) != 0 {
					t.Fatalf("restart adopted %v while w2's request could still commit", got)
				}
				for _, id := range []string{"w2", "w3", "w4"} {
					if _, blocker, reason, _ := o.pacing(id); blocker != "w2" || reason != orphanHold {
						t.Fatalf("%s blocked by %q for %q, want w2 for %q", id, blocker, reason, orphanHold)
					}
				}
			} else if got := adoptionPatches(o.c); !equalStrings(got, tc.wantAdopt) {
				t.Fatalf("restart adopted %v, want %v", got, tc.wantAdopt)
			}
			handled, _, err := o.base.React(delayed)
			if !handled {
				t.Fatal("the delayed request was not handled")
			}
			if tc.lateCommit != (err == nil) {
				t.Fatalf("delayed request commit error = %v, want commit=%t", err, tc.lateCommit)
			}
			wantCanaries := tc.wantAdopt
			if tc.lateCommit {
				wantCanaries = []string{"uzi-hw-w2"}
			}
			o.c.ClearActions()
			_ = o.tick(t)
			_ = o.tick(t)
			if got := o.canaries(t); !equalStrings(got, wantCanaries) {
				t.Fatalf("canaries after the late request = %v, want %v", got, wantCanaries)
			}
			if got := adoptionPatches(o.c); len(got) != 0 {
				t.Fatalf("a second adoption was issued: %v", got)
			}
		})
	}
}

// A proven participant is not in flight, so once a later participant gains fresh proof
// the pool can have no unproven reservation at all. Ordinary planning must still not
// admit a drifted worker as a second canary while a withheld adoption patch for the
// proven participant can commit.
func TestOrphanRollCohortNoNewCanaryWhileProvenReservationsRemain(t *testing.T) {
	t.Run("single owner", func(t *testing.T) { noNewCanaryWhileProven(t, false) })
	// A canary of another owner in the same namespace must not exempt the pool from
	// the hold; neither does a same owner/lane sibling-pool canary
	// (TestReviewSiblingCanaryDoesNotLiftOutstandingAdoptionFence).
	t.Run("another owner's canary in the namespace", func(t *testing.T) { noNewCanaryWhileProven(t, true) })
}

func noNewCanaryWhileProven(t *testing.T, foreign bool) {
	// w1 canary (promoted), w2 and w3 reserved, w4 drifted and unreserved.
	m, c, workers, cordon := matrixFixture(t, 4)
	m.cfg.RollConcurrency = 2
	o := &orphanCohort{m: m, c: c, base: c.ReactionChain[1], cordon: cordon, workers: workers}
	rollTickTest(t, m, workers)
	removeOldPod(t, c, "w1")
	currentProof(t, c, &o.workers[0], m5Now)
	rollTickTest(t, m, workers)
	if mk := o.marker(t, "w2"); mk == nil || mk.Canary || mk.Completed {
		t.Fatalf("w2 is not an incomplete participant: %+v", mk)
	}
	if mk := o.marker(t, "w3"); mk == nil || mk.Canary || mk.Completed {
		t.Fatalf("w3 is not an incomplete participant: %+v", mk)
	}
	if mk := o.marker(t, "w4"); mk != nil {
		t.Fatalf("w4 was reserved ahead of the scenario: %+v", mk)
	}
	o.prove(t, "w2")
	o.eraseCanary(t)
	var delayed k8stesting.Action
	o.c.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if delayed == nil && isAdoptionPatch(a) {
			delayed = a.DeepCopy()
			return true, nil, fmt.Errorf("adoption response lost before commit")
		}
		return false, nil, nil
	})
	if err := o.tick(t); err == nil {
		t.Fatal("lost adoption was reported as confirmed")
	}
	if delayed == nil || delayed.(k8stesting.PatchAction).GetName() != "uzi-hw-w2" {
		t.Fatalf("w2 was not the adoption target: %v", delayed)
	}

	// Restart: w3 now has fresh proof, so nothing in the pool is in flight.
	o.m = matrixRestart(o.m)
	o.prove(t, "w3")
	if foreign {
		o.addForeignCanary(t, "w9", "owner-B")
	}
	o.c.ClearActions()
	_ = o.tick(t)
	for _, line := range actionTrace(o.c) {
		if strings.Contains(line, "uzi-hw-w4") {
			t.Fatalf("a drifted unreserved worker was written while reservations were orphaned: %v", actionTrace(o.c))
		}
	}
	for _, name := range templatePatches(o.c) {
		if name != "uzi-hw-w9" { // the foreign owner's own pool rolls independently
			t.Fatalf("template patched while the cohort had no canary: %v", templatePatches(o.c))
		}
	}
	if mk := o.marker(t, "w4"); mk != nil {
		t.Fatalf("w4 was admitted: %+v", mk)
	}
	if foreign {
		if mk := o.marker(t, "w9"); mk == nil || !mk.Canary {
			t.Fatalf("the foreign canary did not survive the tick under test: %+v", mk)
		}
	}
	if _, _, _, canary := o.pacing("w4"); canary == "w4" {
		t.Fatal("w4 reported as canary")
	}

	if handled, _, err := o.base.React(delayed); !handled {
		t.Fatal("the delayed request was not handled")
	} else {
		_ = err // committed or rejected as stale; either way at most one canary
	}
	for i := 0; i < 2; i++ {
		_ = o.tick(t)
	}
	var got []string
	for _, name := range o.canaries(t) {
		if name != "uzi-hw-w9" { // the foreign owner's own canary is not this cohort's
			got = append(got, name)
		}
	}
	if len(got) != 1 || got[0] == "uzi-hw-w4" {
		t.Fatalf("canaries after the late request = %v, want exactly one participant canary", got)
	}
}

// Real controller writes that can reach a participant of an orphan pool must leave a
// fenced participant fenced, judged from the Deployment the fake API now holds.
func TestOrphanRollCohortRealWritesKeepFencedParticipantFenced(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, o *orphanCohort)
		want   string // the controller write the case must provoke
	}{
		{name: "version mapping correction", want: "patch deployments uzi-hw-w3 metadata", mutate: func(t *testing.T, o *orphanCohort) {
			mk := o.marker(t, "w3")
			mk.TargetVersion = "0.0.1"
			raw, _ := json.Marshal(mk)
			o.edit(t, "w3", func(d *appsv1.Deployment) { d.Annotations[annotationRoll] = string(raw) })
		}},
		{name: "pin cancellation", want: "patch deployments uzi-hw-w3 metadata", mutate: func(t *testing.T, o *orphanCohort) {
			// A new release pin makes every reservation's hash and image obsolete.
			r, err := preset.NewResolver("harbor.example.com/uzi", "v2.0.0")
			if err != nil {
				t.Fatal(err)
			}
			o.m.resolver = r
			o.m.cfg.RollTargetVersion = "2.0.0"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOrphanCohort(t, 3, 2)
			o.recycle(t, "w3")
			o.prove(t, "w2")
			o.eraseCanary(t)
			tc.mutate(t, o)
			o.c.ClearActions()
			for i := 0; i < 2; i++ {
				_ = o.tick(t)
				if i == 0 {
					wrote := false
					for _, line := range actionTrace(o.c) {
						wrote = wrote || line == tc.want
					}
					if !wrote {
						t.Fatalf("the controller never made the write under test %q: %v", tc.want, actionTrace(o.c))
					}
					if got := adoptionPatches(o.c); len(got) != 0 {
						t.Fatalf("adopted in the tick that rewrote w3: %v", got)
					}
				}
				d := o.dep(t, "w3")
				mk, err := decodeRoll(d.Annotations[annotationRoll])
				if err != nil {
					t.Fatal(err)
				}
				observed := &rollEntry{w: protocol.DesiredWorker{ID: "w3"}, dep: d, marker: mk}
				if !orphanPatchFenced(observed) {
					t.Fatalf("tick %d lifted the fence on w3: replicas=%v marker=%+v", i, d.Spec.Replicas, mk)
				}
				if mk != nil && mk.Canary {
					t.Fatalf("tick %d made the fenced w3 a canary", i)
				}
			}
		})
	}
}

func TestOrphanRollCohortAdoptionWriteOutcomes(t *testing.T) {
	prepare := func(t *testing.T) *orphanCohort {
		o := newOrphanCohort(t, 4, 3)
		o.prove(t, "w2")
		o.prove(t, "w3")
		o.eraseCanary(t)
		return o
	}
	t.Run("multiple eligible participants adopt the smallest id, also after a restart", func(t *testing.T) {
		for _, restart := range []bool{false, true} {
			o := prepare(t)
			if restart {
				o.m = matrixRestart(o.m)
			}
			o.c.ClearActions()
			o.mustTick(t)
			if got := adoptionPatches(o.c); !equalStrings(got, []string{"uzi-hw-w2"}) {
				t.Fatalf("restart=%t adopted %v, want [uzi-hw-w2]", restart, got)
			}
		}
	})
	t.Run("smallest id wins even when it is busy and a larger id is idle", func(t *testing.T) {
		// The pool is ordered idle-first; selection must still be by worker id.
		o := prepare(t)
		o.at("w2").Busy = true
		o.c.ClearActions()
		o.mustTick(t)
		if got := adoptionPatches(o.c); !equalStrings(got, []string{"uzi-hw-w2"}) {
			t.Fatalf("adopted %v, want [uzi-hw-w2]", got)
		}
	})
	t.Run("metadata conflict closes the pool without a replacement", func(t *testing.T) {
		o := prepare(t)
		o.c.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if isAdoptionPatch(a) {
				return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, a.(k8stesting.PatchAction).GetName(), fmt.Errorf("resourceVersion"))
			}
			return false, nil, nil
		})
		o.c.ClearActions()
		if err := o.tick(t); err == nil {
			t.Fatal("conflicting adoption was reported as confirmed")
		}
		if got, want := actionTrace(o.c), []string{"patch deployments uzi-hw-w2 metadata"}; !equalStrings(got, want) {
			t.Fatalf("tick wrote %v, want only the rejected request %v", got, want)
		}
		if len(o.canaries(t)) != 0 {
			t.Fatal("rejected request left a canary")
		}
		if state, _, reason, canary := o.pacing("w2"); state != "held" || reason != "canary adoption unconfirmed" || canary != "" {
			t.Fatalf("w2 pacing = %s/%q/%s", state, reason, canary)
		}
		if state, _, reason, canary := o.pacing("w3"); state != "held" || reason != "canary adoption pending re-observation" || canary != "" {
			t.Fatalf("w3 pacing = %s/%q/%s", state, reason, canary)
		}
	})
	t.Run("an unconfirmed request that committed is seen next tick, not repeated", func(t *testing.T) {
		o := prepare(t)
		lost := false
		o.c.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if lost || !isAdoptionPatch(a) {
				return false, nil, nil
			}
			lost = true
			if handled, _, err := o.base.React(a); !handled || err != nil {
				t.Fatalf("commit: %v", err)
			}
			return true, nil, fmt.Errorf("adoption response lost after commit")
		})
		o.c.ClearActions()
		if err := o.tick(t); err == nil {
			t.Fatal("lost response was reported as confirmed")
		}
		if got, want := actionTrace(o.c), []string{"patch deployments uzi-hw-w2 metadata"}; !equalStrings(got, want) {
			t.Fatalf("unconfirmed tick wrote %v, want only %v", got, want)
		}
		o.m = matrixRestart(o.m)
		o.c.ClearActions()
		_ = o.tick(t)
		if got := adoptionPatches(o.c); len(got) != 0 {
			t.Fatalf("second adoption after a committed request: %v", got)
		}
		if got := o.canaries(t); !equalStrings(got, []string{"uzi-hw-w2"}) {
			t.Fatalf("canaries = %v", got)
		}
	})
}

func TestOrphanRollCohortCanaryLossEntryPoints(t *testing.T) {
	t.Run("external reset or Deployment deletion without proof stays held", func(t *testing.T) {
		for _, how := range []string{"annotation reset", "deployment deleted"} {
			o := newOrphanCohort(t, 3, 2)
			if how == "annotation reset" {
				o.eraseCanary(t)
			} else if err := o.c.Tracker().Delete(appsv1.SchemeGroupVersion.WithResource("deployments"), o.m.cfg.Namespace, deploymentName("w1")); err != nil {
				t.Fatal(err)
			}
			o.c.ClearActions()
			_ = o.tick(t)
			if got := adoptionPatches(o.c); len(got) != 0 || len(o.canaries(t)) != 0 {
				t.Fatalf("%s: adopted %v / %v without proof", how, got, o.canaries(t))
			}
			if _, _, reason, _ := o.pacing("w3"); reason != orphanNoCan {
				t.Fatalf("%s: w3 reason = %q, want %q", how, reason, orphanNoCan)
			}
		}
	})
	t.Run("hibernation retirement does not retire an incomplete canary", func(t *testing.T) {
		o := newOrphanCohort(t, 3, 2)
		o.at("w1").Hibernation = &protocol.Hibernation{State: protocol.HibernationPreparing, OpID: "op-1"}
		_ = o.tick(t)
		if mk := o.marker(t, "w1"); mk == nil || !mk.Canary || mk.Completed {
			t.Fatalf("hibernation retired or rewrote an incomplete canary: %+v", mk)
		}
	})
	t.Run("retained claim mirror restoration keeps the canary bit", func(t *testing.T) {
		o := newOrphanCohort(t, 3, 2)
		raw := o.dep(t, "w1").Annotations[annotationRoll]
		o.editClaim(t, dataPVCName("w1"), func(p *corev1.PersistentVolumeClaim) { p.Annotations = map[string]string{annotationRoll: raw} })
		o.eraseCanary(t)
		o.mustTick(t)
		if mk := o.marker(t, "w1"); mk == nil || !mk.Canary || mk.Completed {
			t.Fatalf("restored reservation lost its canary bit: %+v", mk)
		}
	})
	t.Run("completed cohort cleanup keeps the canary marker", func(t *testing.T) {
		o := newOrphanCohort(t, 3, 2)
		o.prove(t, "w2")
		o.prove(t, "w3")
		for i := 0; i < 4; i++ {
			o.mustTick(t)
		}
		mk := o.marker(t, "w1")
		if mk == nil || !mk.Canary || !mk.Completed {
			t.Fatalf("cleanup cleared or rewrote the completed canary: %+v", mk)
		}
		for _, id := range []string{"w2", "w3"} {
			if o.marker(t, id) != nil {
				t.Fatalf("%s kept its participant marker after completion cleanup", id)
			}
		}
	})
	t.Run("layout hold of a promoted canary keeps its reservation while participants remain", func(t *testing.T) {
		cObjs, canary := lmObjects(t, lmOpts{id: "d1", custody: true, layout: "old", marker: "canary", promoted: true})
		pObjs, part := lmObjects(t, lmOpts{id: "d2", layout: "new", marker: "participant"})
		f := lmCluster(t, false, append(cObjs, pObjs...)...)
		canary.Generation = 1 // a generation rotation drifts the held canary
		for i := 0; i < 3; i++ {
			f.tickAll(t, canary, part)
			d, err := f.client.AppsV1().Deployments(lmNS()).Get(context.Background(), deploymentName("d1"), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			mk, err := decodeRoll(d.Annotations[annotationRoll])
			if err != nil || mk == nil || !mk.Canary || mk.Completed {
				t.Fatalf("tick %d: held canary's reservation was withdrawn: %+v (%v)", i, mk, err)
			}
		}
	})
	t.Run("layout hold keeps the canary reservation when the only incomplete participant is hibernating", func(t *testing.T) {
		cObjs, canary := lmObjects(t, lmOpts{id: "d1", custody: true, layout: "old", marker: "canary", promoted: true})
		pObjs, part := lmObjects(t, lmOpts{id: "d2", layout: "new", marker: "participant"})
		f := lmCluster(t, false, append(cObjs, pObjs...)...)
		canary.Generation = 1
		// Preparing to hibernate, but refused (roll evidence): the participant keeps its
		// incomplete reservation and stays a hibernation entry.
		part.Hibernation = &protocol.Hibernation{State: protocol.HibernationPreparing, OpID: "op-1"}
		for i := 0; i < 3; i++ {
			f.tickAll(t, canary, part)
			d, err := f.client.AppsV1().Deployments(lmNS()).Get(context.Background(), deploymentName("d1"), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			mk, err := decodeRoll(d.Annotations[annotationRoll])
			if err != nil || mk == nil || !mk.Canary || mk.Completed {
				t.Fatalf("tick %d: held canary's reservation was withdrawn: %+v (%v)", i, mk, err)
			}
			pd, err := f.client.AppsV1().Deployments(lmNS()).Get(context.Background(), deploymentName("d2"), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if pm, err := decodeRoll(pd.Annotations[annotationRoll]); err != nil || pm == nil || pm.Completed {
				t.Fatalf("tick %d: the hibernating participant's reservation changed: %+v (%v)", i, pm, err)
			}
		}
	})

	t.Run("layout hold keeps canary history beside an unplanned participant", func(t *testing.T) {
		for _, reason := range []string{"unresolved template in the same namespace", "unresolved placement"} {
			t.Run(reason, func(t *testing.T) {
				cObjs, canary := lmObjects(t, lmOpts{id: "d1", custody: true, layout: "old", marker: "canary", promoted: true})
				pObjs, part := lmObjects(t, lmOpts{id: "d2", layout: "new", marker: "participant"})
				f := lmCluster(t, false, append(cObjs, pObjs...)...)
				canary.Generation = 1
				before, err := f.client.AppsV1().Deployments(lmNS()).Get(context.Background(), deploymentName(canary.ID), metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if reason == "unresolved placement" {
					part.Docker, part.Isolated = false, true
					if _, err := f.m.cfg.Placement(part); err == nil {
						t.Fatal("fixture placement unexpectedly resolved")
					}
				} else {
					part.Template = "unavailable-template"
					if _, err := f.m.resolver.Resolve(part.Template, part.Size); err == nil {
						t.Fatal("fixture template unexpectedly resolved")
					}
				}
				for tick := 0; tick < 3; tick++ {
					_ = f.tickErr(t, canary, part)
					after, err := f.client.AppsV1().Deployments(lmNS()).Get(context.Background(), deploymentName(canary.ID), metav1.GetOptions{})
					if err != nil {
						t.Fatal(err)
					}
					if after.Annotations[annotationRoll] != before.Annotations[annotationRoll] || !reflect.DeepEqual(after.Spec, before.Spec) {
						t.Fatalf("tick %d: unplanned participant lost its canary history or changed the held Deployment", tick)
					}
					p := f.m.PacingOutcomes()[canary.ID]
					if p == nil || p.State != "held" {
						t.Fatalf("tick %d: custody-held canary pacing = %+v, want held", tick, p)
					}
				}
			})
		}
	})
}

// addForeignCanary adds a worker of another owner in the same namespace whose
// Deployment carries that owner's incomplete canary reservation.
func (o *orphanCohort) addForeignCanary(t *testing.T, id, owner string) {
	t.Helper()
	w := evidence(desired(id), m5Now)
	w.RollEvidence.OwnerID = owner
	d := deployedWorker(id, 0, "old")
	cov, ok := coverageOf(w, o.m.cfg.Namespace)
	if !ok {
		t.Fatal("foreign worker has no coverage evidence")
	}
	// A complete, decodable reservation copied from a participant, re-owned and
	// re-pinned to w9's own rendered target, so owner-B's pool keeps it instead of
	// cancelling it as an obsolete pin (independently of pool iteration order).
	spec, err := o.m.resolver.Resolve(w.Template, w.Size)
	if err != nil {
		t.Fatal(err)
	}
	target := RenderDeployment(o.m.renderCfgFor(context.Background(), w, reconcile.ObservedWorker{}), w, spec)
	mk := o.marker(t, "w2")
	mk.Coverage, mk.Canary, mk.Completed = cov, true, false
	mk.Hash, mk.Image = target.Spec.Template.Annotations[AnnotationSpecHash], workerImage(target)
	mk.TargetVersion = targetVersion(o.m.cfg, mk.Image)
	raw, _ := json.Marshal(mk)
	if d.Annotations == nil {
		d.Annotations = map[string]string{}
	}
	d.Annotations[annotationRoll] = string(raw)
	for _, obj := range physicalOld(t, d) {
		if err := o.c.Tracker().Add(obj); err != nil {
			t.Fatal(err)
		}
	}
	o.workers = append(o.workers, w)
	if got, err := decodeRoll(o.dep(t, id).Annotations[annotationRoll]); err != nil || got == nil || !got.Canary || got.Coverage.Owner != owner {
		t.Fatalf("foreign canary reservation unreadable: %+v %v", got, err)
	}
}

// A same owner/lane canary in a sibling capability pool touches no Deployment in this
// pool, so it cannot fence an adoption patch still outstanding for a participant here:
// ordinary new-canary admission stays closed (MR !4485 review).
func TestReviewSiblingCanaryDoesNotLiftOutstandingAdoptionFence(t *testing.T) {
	m, c, workers, cordon := matrixFixture(t, 4)
	m.cfg.RollConcurrency = 2
	o := &orphanCohort{m: m, c: c, base: c.ReactionChain[1], cordon: cordon, workers: workers}
	rollTickTest(t, m, workers)
	o.prove(t, "w1")
	o.mustTick(t)
	if o.marker(t, "w4") != nil {
		t.Fatal("w4 already reserved")
	}
	o.prove(t, "w2")
	o.eraseCanary(t)
	var delayed k8stesting.Action
	c.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if delayed == nil && isAdoptionPatch(a) {
			delayed = a.DeepCopy()
			return true, nil, fmt.Errorf("withheld adoption")
		}
		return false, nil, nil
	})
	_ = o.tick(t)
	if delayed == nil {
		t.Fatal("no outstanding adoption")
	}
	o.m = matrixRestart(o.m)
	o.prove(t, "w3")
	o.outsider(t, true, true)
	c.ClearActions()
	_ = o.tick(t)
	if mk := o.marker(t, "w4"); mk != nil {
		t.Errorf("fresh canary admitted beside outstanding adoption: w4=%+v actions=%v", mk, actionTrace(c))
	}
	if handled, _, err := o.base.React(delayed); !handled || err != nil {
		t.Fatalf("delayed commit: handled=%v err=%v", handled, err)
	}
	got := o.canaries(t)
	t.Logf("canaries after delayed commit: %v", got)
	// w9 is the sibling pool's own canary; the original pool must hold exactly w2.
	if want := []string{deploymentName("w2"), deploymentName("w9")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("canaries after delayed commit = %v, want %v", got, want)
	}
}
