package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func TestM2PhysicalLaneAcrossCapabilityPools(t *testing.T) {
	for _, mode := range []string{"shared", "other-owner", "other-lane", "singleton"} {
		t.Run(mode, func(t *testing.T) {
			workers := []protocol.DesiredWorker{evidence(desired("w1"), m5Now)}
			workers[0].RollEvidence.SchedulingCapabilities = []string{"cap-A"}
			if mode != "singleton" {
				peer := evidence(desired("w2"), m5Now)
				peer.RollEvidence.ProtocolCapabilities = []string{"cap-B"}
				if mode == "other-owner" {
					peer.RollEvidence.OwnerID = "owner-B"
				}
				if mode == "other-lane" {
					peer.Docker = true
				}
				workers = append(workers, peer)
			}
			cfg := dockerTestConfig()
			var objects []runtime.Object
			for _, w := range workers {
				d := deployedWorker(w.ID, 0, "old")
				if w.Docker {
					d.Namespace = cfg.DockerNamespace
				}
				objects = append(objects, physicalOld(t, d)...)
			}
			m, c := newMat(t, objects...)
			m.cfg = cfg
			m.now = func() time.Time { return m5Now }
			rollTickTest(t, m, workers)
			want := len(workers)
			if mode == "shared" {
				want = 1
			}
			if patches := templatePatches(c); len(patches) != want {
				t.Fatalf("physical owner/lane budget violated: %v, want %d", patches, want)
			}
			if mode != "shared" {
				return
			}
			first := strings.TrimPrefix(templatePatches(c)[0], "uzi-hw-")
			for i := range workers {
				if workers[i].ID != first {
					continue
				}
				caps := workers[i].RollEvidence.SchedulingCapabilities
				protocolCaps := workers[i].RollEvidence.ProtocolCapabilities
				removeOldPod(t, c, first)
				currentProof(t, c, &workers[i], m5Now)
				workers[i].RollEvidence.SchedulingCapabilities = caps
				workers[i].RollEvidence.ProtocolCapabilities = protocolCaps
			}
			rollTickTest(t, m, workers)
			if patches := templatePatches(c); len(patches) != 2 {
				t.Fatalf("current incarnation did not restore lane capacity: %v", patches)
			}
		})
	}
}

func TestM2CompletedCoverageNewPinAndPartialCleanup(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			w := evidence(desired("w1"), m5Now)
			d := markedDeployment(t, testConfig(), w, true)
			var r rollMarker
			if err := json.Unmarshal([]byte(d.Annotations[annotationRoll]), &r); err != nil {
				t.Fatal(err)
			}
			r.Coverage.Scheduling = []string{"obsolete-cap"}
			raw, _ := json.Marshal(r)
			d.Annotations[annotationRoll] = string(raw)
			objects := []runtime.Object{d}
			workers := []protocol.DesiredWorker{w}
			if partial {
				peer := evidence(desired("w2"), m5Now)
				pd := markedDeployment(t, testConfig(), peer, false)
				r.Canary, r.Completed = false, false
				raw, _ = json.Marshal(r)
				pd.Annotations[annotationRoll] = string(raw)
				objects = append(objects, pd)
				workers = append(workers, peer)
			}
			m, c := newMat(t, objects...)
			m.now = func() time.Time { return m5Now }
			// Legitimate API capability changes after completion must not inherit history.
			workers[0].RollEvidence.SchedulingCapabilities = []string{"new-cap"}
			var err error
			m.resolver, err = preset.NewResolver("harbor.example.com/uzi", "v2.0.0")
			if err != nil {
				t.Fatal(err)
			}
			m.cfg.RollTargetVersion = "2.0.0+g123"
			rollTickTest(t, m, workers)
			got, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), d.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var marker rollMarker
			if err := json.Unmarshal([]byte(got.Annotations[annotationRoll]), &marker); err != nil {
				t.Fatal(err)
			}
			if partial {
				if !marker.Completed || !reflect.DeepEqual(marker.Coverage.Scheduling, []string{"obsolete-cap"}) || len(templatePatches(c)) != 0 {
					t.Fatalf("partial cleanup lost original evidence: %+v", marker)
				}
				peer, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), deploymentName("w2"), metav1.GetOptions{})
				if err != nil || peer.Annotations[annotationRoll] == "" {
					t.Fatal("unresolved participant cleared")
				}
			} else if marker.Completed || marker.TargetVersion != "2.0.0+g123" || !reflect.DeepEqual(marker.Coverage.Scheduling, []string{"new-cap"}) {
				t.Fatalf("new pin retained obsolete coverage: %+v", marker)
			}
		})
	}
}

func TestM2PartialCleanupThenFreshCoverage(t *testing.T) {
	cfg := testConfig()
	w1, w2 := evidence(desired("w1"), m5Now), evidence(desired("w2"), m5Now)
	d1, d2 := markedDeployment(t, cfg, w1, true), markedDeployment(t, cfg, w2, false)
	for _, d := range []*appsv1.Deployment{d1, d2} {
		var r rollMarker
		if err := json.Unmarshal([]byte(d.Annotations[annotationRoll]), &r); err != nil {
			t.Fatal(err)
		}
		r.Coverage.Scheduling = []string{"original"}
		if d == d2 {
			r.Canary = false
		}
		raw, _ := json.Marshal(r)
		d.Annotations[annotationRoll] = string(raw)
	}
	m, c := newMat(t, d1, d2)
	m.now = func() time.Time { return m5Now }
	currentProof(t, c, &w2, m5Now)
	w2.RollEvidence.SchedulingCapabilities = []string{"original"}
	w1.RollEvidence.SchedulingCapabilities = []string{"current"}
	var err error
	m.resolver, err = preset.NewResolver("harbor.example.com/uzi", "v2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	m.cfg.RollTargetVersion = "2.0.0+g123"
	workers := []protocol.DesiredWorker{w1, w2}
	rollTickTest(t, m, workers)
	peer, err := c.AppsV1().Deployments(cfg.Namespace).Get(context.Background(), d2.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if peer.Annotations[annotationRoll] != "" || len(templatePatches(c)) != 0 {
		t.Fatal("partial cleanup did not finish before new admission")
	}
	rollTickTest(t, m, workers)
	canary, err := c.AppsV1().Deployments(cfg.Namespace).Get(context.Background(), d1.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var r rollMarker
	if err := json.Unmarshal([]byte(canary.Annotations[annotationRoll]), &r); err != nil {
		t.Fatal(err)
	}
	if r.Completed || r.TargetVersion != m.cfg.RollTargetVersion || !reflect.DeepEqual(r.Coverage.Scheduling, []string{"current"}) {
		t.Fatalf("cleanup restart reused original coverage: %+v", r)
	}
}

func TestM2FrozenCapabilitiesNeverUnionPeers(t *testing.T) {
	workers := []protocol.DesiredWorker{evidence(desired("w1"), m5Now), evidence(desired("w2"), m5Now), evidence(desired("w3"), m5Now)}
	var objects []runtime.Object
	for i, w := range workers {
		d := deployedWorker(w.ID, 0, "old")
		if i < 2 {
			marked := markedDeployment(t, testConfig(), w, false)
			var r rollMarker
			_ = json.Unmarshal([]byte(marked.Annotations[annotationRoll]), &r)
			r.Coverage.Scheduling = []string{"a", "b"}
			r.Canary = i == 0
			raw, _ := json.Marshal(r)
			d.Annotations = map[string]string{annotationRoll: string(raw)}
		}
		objects = append(objects, physicalOld(t, d)...)
	}
	workers[0].RollEvidence.SchedulingCapabilities = []string{"a", "b"}
	workers[1].RollEvidence.SchedulingCapabilities = []string{"a"}
	workers[2].RollEvidence.SchedulingCapabilities = []string{"b"}
	m, c := newMat(t, objects...)
	m.now = func() time.Time { return m5Now }
	rollTickTest(t, m, workers)
	for _, name := range templatePatches(c) {
		if name == deploymentName("w1") || name == deploymentName("w2") {
			t.Fatal("union of peers replaced original complete capability coverage")
		}
	}
}

func TestM2UnrelatedMaintenanceDoesNotReserveCanary(t *testing.T) {
	for _, historical := range []string{"active", "completed", "partial-cleanup"} {
		t.Run(historical, func(t *testing.T) {
			cfg := dockerTestConfig()
			w := evidence(desiredDocker("w1"), m5Now)
			d := markedDeployment(t, cfg, w, historical != "active")
			peer := evidence(desiredDocker("w2"), m5Now)
			pd := RenderDeployment(cfg, peer, testSpec(t, peer.Template, peer.Size))
			pd.UID, pd.ResourceVersion = "maintenance-peer", "1"
			peer.DindMaintenance = &protocol.DindMaintenance{ID: "op", Phase: "stopping", Fenced: true, DeploymentUID: string(pd.UID), PVCUID: "old-dind"}
			objects := []runtime.Object{d, pd}
			for _, p := range RenderPVCs(cfg, peer, testSpec(t, peer.Template, peer.Size)) {
				p.UID = types.UID(p.Name)
				if p.Name == dindDataPVCName(peer.ID) {
					p.UID = "old-dind"
				}
				p.Status.Phase = corev1.ClaimBound
				objects = append(objects, p)
			}
			workers := []protocol.DesiredWorker{w, peer}
			if historical == "partial-cleanup" {
				w3 := evidence(desiredDocker("w3"), m5Now)
				d3 := markedDeployment(t, cfg, w3, false)
				var r rollMarker
				_ = json.Unmarshal([]byte(d3.Annotations[annotationRoll]), &r)
				r.Canary = false
				raw, _ := json.Marshal(r)
				d3.Annotations[annotationRoll] = string(raw)
				objects = append(objects, d3)
				workers = append(workers, w3)
			}
			m, c := newMat(t, objects...)
			m.cfg, m.cordoner = cfg, &maintenanceFake{}
			m.now = func() time.Time { return m5Now }
			rollTickTest(t, m, workers)
			claim, err := c.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Get(context.Background(), dataPVCName(peer.ID), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if claim.Annotations[annotationRoll] != "" {
				t.Fatal("unrelated maintenance synthesized a mirrored image canary")
			}
			for _, a := range c.Actions() {
				if a.GetVerb() == "patch" && a.GetResource().Resource == "deployments" {
					t.Fatal("unrelated maintenance created an image reservation")
				}
			}
			if _, err := c.AppsV1().Deployments(cfg.DockerNamespace).Get(context.Background(), pd.Name, metav1.GetOptions{}); err == nil {
				t.Fatal("independent maintenance was blocked")
			}
			rollTickTest(t, m, workers)
			for _, p := range m.PacingOutcomes() {
				if p.BlockingReason != nil && strings.Contains(*p.BlockingReason, "duplicate canary") {
					t.Fatal("false duplicate canary hold")
				}
			}
		})
	}
}

func TestM2RollMarkerStrictSchema(t *testing.T) {
	w := evidence(desired("w1"), m5Now)
	raw := markedDeployment(t, testConfig(), w, false).Annotations[annotationRoll]
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRoll(raw); err != nil {
		t.Fatal(err)
	}
	// False is evidence too: require presence without treating false as missing.
	var ordinary rollMarker
	if err := json.Unmarshal([]byte(raw), &ordinary); err != nil {
		t.Fatal(err)
	}
	ordinary.Canary, ordinary.Coverage.Ordinary, ordinary.Coverage.Checker = false, false, false
	falseRaw, _ := json.Marshal(ordinary)
	if _, err := decodeRoll(string(falseRaw)); err != nil {
		t.Fatalf("rejected explicit false evidence: %v", err)
	}
	for _, nested := range []bool{false, true} {
		source := fields
		if nested {
			source = map[string]json.RawMessage{}
			if err := json.Unmarshal(fields["coverage"], &source); err != nil {
				t.Fatal(err)
			}
		}
		for name := range source {
			for _, corrupt := range []string{"missing", "null", "wrong-type"} {
				t.Run(fmt.Sprintf("%t/%s/%s", nested, name, corrupt), func(t *testing.T) {
					copyFields := map[string]json.RawMessage{}
					for k, v := range source {
						copyFields[k] = v
					}
					switch corrupt {
					case "missing":
						delete(copyFields, name)
					case "null":
						copyFields[name] = json.RawMessage("null")
					case "wrong-type":
						copyFields[name] = json.RawMessage("{}")
					}
					body, _ := json.Marshal(copyFields)
					if nested {
						outer := map[string]json.RawMessage{}
						for k, v := range fields {
							outer[k] = v
						}
						outer["coverage"] = body
						body, _ = json.Marshal(outer)
					}
					if _, err := decodeRoll(string(body)); err == nil {
						t.Fatalf("accepted %s %s evidence", corrupt, name)
					}
				})
			}
		}
	}
	for _, malformed := range []string{
		strings.Replace(raw, "\"version\":1", "\"version\":1,\"version\":1", 1),
		strings.Replace(raw, "\"checker\":false", "\"checker\":false,\"extra\":false", 1),
		strings.Replace(raw, "\"scheduling\":[]", "\"scheduling\":[null]", 1),
		strings.Repeat("x", maxRollMarker+1),
	} {
		if _, err := decodeRoll(malformed); err == nil {
			t.Fatal("accepted malformed or unbounded schema")
		}
	}
}

func TestM2SnapshotRetainsOnlyCurrentAncestryIdentity(t *testing.T) {
	w := evidence(desired("w1"), m5Now)
	objects := physicalOld(t, deployedWorker(w.ID, 0, "old"))
	rs := objects[1].(*appsv1.ReplicaSet)
	rs.Spec.Template.Spec.Containers = []corev1.Container{{Name: "large-history", Image: strings.Repeat("x", 10000)}}
	old := rs.DeepCopy()
	old.Name, old.UID, old.OwnerReferences[0].UID = "historical", "historical-rs", "retired-deployment"
	objects = append(objects, old)
	m, _ := newMat(t, objects...)
	m.now = func() time.Time { return m5Now }
	tick, _ := m.planRoll(context.Background(), []protocol.DesiredWorker{w}, nil)
	sets := tick.entries[w.ID].sets
	if len(sets) != 1 {
		t.Fatalf("retained historical ancestry: %+v", sets)
	}
	want := appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: rs.Name, UID: rs.UID}}
	if !reflect.DeepEqual(sets[rs.Name], want) {
		t.Fatal("snapshot retained full ReplicaSet metadata or template")
	}
}

func TestM2DuplicateCanariesNeverBlindlyBothRoll(t *testing.T) {
	for _, mode := range []string{"pending", "replacement", "scaled", "maintenance", "no-physical-proof", "unknown-cancel", "no-incarnation-proof"} {
		t.Run(mode, func(t *testing.T) {
			var objects []runtime.Object
			workers := []protocol.DesiredWorker{evidence(desired("w1"), m5Now), evidence(desired("w2"), m5Now)}
			for i, w := range workers {
				marked := markedDeployment(t, testConfig(), w, false)
				old := deployedWorker(w.ID, 0, "old")
				one := int32(1)
				old.Spec.Replicas = &one
				old.Annotations = marked.Annotations
				if mode == "replacement" && i == 1 {
					old = marked
				}
				if mode == "scaled" && i == 1 {
					zero := int32(0)
					old.Spec.Replicas = &zero
				}
				physical := physicalOld(t, old)
				uid := string(physical[2].(*corev1.Pod).UID)
				workers[i].RollEvidence.PodUID = &uid
				workers[i].RollEvidence.HeartbeatAt = &m5Now
				if mode == "no-incarnation-proof" && i == 1 {
					workers[i].RollEvidence.PodUID = nil
				}
				if mode == "no-physical-proof" && i == 1 {
					physical = physical[:2]
				}
				if mode == "maintenance" && i == 1 {
					workers[i].DindMaintenance = &protocol.DindMaintenance{Phase: "stopping"}
				}
				objects = append(objects, physical...)
			}
			m, c := newMat(t, objects...)
			m.now = func() time.Time { return m5Now }
			if mode == "unknown-cancel" {
				c.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
					if a.(k8stesting.PatchAction).GetName() == deploymentName("w2") {
						return true, nil, fmt.Errorf("unknown cancellation outcome")
					}
					return false, nil, nil
				})
			}
			rollTickTest(t, m, workers)
			want := 0
			if mode == "pending" {
				want = 1
			}
			if len(templatePatches(c)) != want {
				t.Fatalf("duplicate recovery templates=%v want=%d", templatePatches(c), want)
			}
			d, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), deploymentName("w2"), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "pending" {
				if d.Annotations[annotationRoll] != "" {
					t.Fatal("harmless pending duplicate not cancelled")
				}
			} else if d.Annotations[annotationRoll] == "" {
				t.Fatal("unresolved replacement evidence discarded")
			}
			rollTickTest(t, m, workers)
			if len(templatePatches(c)) != want {
				t.Fatal("restart blindly rolled both duplicate canaries")
			}
		})
	}
}
