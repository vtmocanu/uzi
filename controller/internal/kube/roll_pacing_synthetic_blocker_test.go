package kube

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// canonicalUUID is a strict lowercase canonical UUID; the controller module does not
// import google/uuid directly.
var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func syntheticBlockerID(i int) string {
	return fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
}

// mutationLog renders every mutating client action as verb/resource/name plus the
// patch body for patches, so admission behaviour can be compared exactly.
func mutationLog(actions []k8stesting.Action) []string {
	var out []string
	for _, a := range actions {
		switch a.GetVerb() {
		case "create", "update", "patch", "delete":
		default:
			continue
		}
		name := ""
		if n, ok := a.(interface{ GetName() string }); ok {
			name = n.GetName()
		}
		if c, ok := a.(k8stesting.CreateAction); ok {
			if om, err := meta.Accessor(c.GetObject()); err == nil {
				name = om.GetName()
			}
		}
		line := a.GetVerb() + " " + a.GetResource().Resource + " " + name
		if p, ok := a.(k8stesting.PatchAction); ok {
			sum := sha256.Sum256(p.GetPatch())
			line += fmt.Sprintf(" sha256=%x", sum[:8])
		}
		out = append(out, line)
	}
	return out
}

// Pool-level pacing reasons (pin change, missing canary) must never be reported as
// a worker identity: the API requires UUIDs and drops the whole status row (#4467).
func TestRollPacingPoolBlockersHaveNoSyntheticWorkerID(t *testing.T) {
	cases := []struct {
		name       string
		reason     string
		wantWorker string   // exact BlockingWorkerID required on every outcome, "" = unchecked
		wantCanary string   // exact CanaryID required on every outcome, "" = must be absent
		wantMut    []string // measured on the unfixed code; admission must not change
		run        func(t *testing.T) (*Materializer, *fake.Clientset, []protocol.DesiredWorker)
	}{
		{
			name:   "missing-canary",
			reason: "participant reservation without canary",
			run: func(t *testing.T) (*Materializer, *fake.Clientset, []protocol.DesiredWorker) {
				var workers []protocol.DesiredWorker
				var objects []runtime.Object
				for i := 1; i <= 2; i++ {
					w := evidence(desired(syntheticBlockerID(i)), m5Now)
					workers = append(workers, w)
					d := markedDeployment(t, testConfig(), w, false)
					d.Namespace = testConfig().Namespace
					var r rollMarker
					if err := json.Unmarshal([]byte(d.Annotations[annotationRoll]), &r); err != nil {
						t.Fatal(err)
					}
					r.Canary = false
					raw, err := json.Marshal(r)
					if err != nil {
						t.Fatal(err)
					}
					d.Annotations[annotationRoll] = string(raw)
					objects = append(objects, d)
				}
				m, c := newMat(t, objects...)
				m.now = func() time.Time { return m5Now }
				rollTickTest(t, m, workers)
				return m, c, workers
			},
		},
		{
			name:   "pin-change",
			reason: "roll target pin changed; pool held until reservations settle",
			wantMut: []string{
				"patch deployments uzi-hw-00000000-0000-0000-0000-000000000001 sha256=4d7a7d5511e62cdc",
				"create persistentvolumeclaims uzi-hw-00000000-0000-0000-0000-000000000001-data",
				"create persistentvolumeclaims uzi-hw-00000000-0000-0000-0000-000000000001-nix",
				"patch deployments uzi-hw-00000000-0000-0000-0000-000000000001 sha256=235175b7c1ab4a64",
				"patch deployments uzi-hw-00000000-0000-0000-0000-000000000001 sha256=2e8a09b6b29f4fcc",
			},
			run: func(t *testing.T) (*Materializer, *fake.Clientset, []protocol.DesiredWorker) {
				var workers []protocol.DesiredWorker
				var objects []runtime.Object
				for i := 1; i <= 3; i++ {
					id := syntheticBlockerID(i)
					workers = append(workers, evidence(desired(id), m5Now))
					objects = append(objects, physicalOld(t, deployedWorker(id, 0, "old"))...)
				}
				m, c := newMat(t, objects...)
				m.now = func() time.Time { return m5Now }
				rollTickTest(t, m, workers)
				var err error
				m.resolver, err = preset.NewResolver("harbor.example.com/uzi", "v2.0.0")
				if err != nil {
					t.Fatal(err)
				}
				m.cfg.RollTargetVersion = "2.0.0+gabcdef0"
				rollTickTest(t, m, workers)
				return m, c, workers
			},
		},
		{
			name:       "real-blocker",
			reason:     "ImagePullBackOff",
			wantWorker: syntheticBlockerID(2),
			wantCanary: syntheticBlockerID(1),
			wantMut: []string{
				"patch deployments uzi-hw-00000000-0000-0000-0000-000000000001 sha256=4d7a7d5511e62cdc",
				"create persistentvolumeclaims uzi-hw-00000000-0000-0000-0000-000000000001-data",
				"create persistentvolumeclaims uzi-hw-00000000-0000-0000-0000-000000000001-nix",
				"patch deployments uzi-hw-00000000-0000-0000-0000-000000000001 sha256=235175b7c1ab4a64",
				"patch deployments uzi-hw-00000000-0000-0000-0000-000000000002 sha256=23f5bb7af65b24d4",
				"patch deployments uzi-hw-00000000-0000-0000-0000-000000000003 sha256=a65d3072e1083bed",
				"create persistentvolumeclaims uzi-hw-00000000-0000-0000-0000-000000000002-data",
				"create persistentvolumeclaims uzi-hw-00000000-0000-0000-0000-000000000002-nix",
				"patch deployments uzi-hw-00000000-0000-0000-0000-000000000002 sha256=e469718255a08333",
				"create persistentvolumeclaims uzi-hw-00000000-0000-0000-0000-000000000003-data",
				"create persistentvolumeclaims uzi-hw-00000000-0000-0000-0000-000000000003-nix",
				"patch deployments uzi-hw-00000000-0000-0000-0000-000000000003 sha256=1aa0ebcd58b5eb65",
			},
			run: func(t *testing.T) (*Materializer, *fake.Clientset, []protocol.DesiredWorker) {
				var workers []protocol.DesiredWorker
				var objects []runtime.Object
				for i := 1; i <= 3; i++ {
					id := syntheticBlockerID(i)
					workers = append(workers, evidence(desired(id), m5Now))
					objects = append(objects, physicalOld(t, deployedWorker(id, 0, "old"))...)
				}
				m, c := newMat(t, objects...)
				m.now = func() time.Time { return m5Now }
				m.cfg.RollConcurrency = 2
				rollTickTest(t, m, workers)
				removeOldPod(t, c, workers[0].ID)
				currentProof(t, c, &workers[0], m5Now)
				rollTickTest(t, m, workers)
				removeOldPod(t, c, workers[1].ID)
				currentProof(t, c, &workers[1], m5Now)
				targetWaiting(t, c, workers[1].ID, "ImagePullBackOff")
				rollTickTest(t, m, workers)
				return m, c, workers
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, c, workers := tc.run(t)
			ids := map[string]bool{}
			for _, w := range workers {
				ids[w.ID] = true
			}
			sawReason := false
			out := m.PacingOutcomes()
			for _, w := range workers {
				p := out[w.ID]
				if p == nil {
					t.Errorf("%s: no pacing outcome", w.ID)
					continue
				}
				for label, v := range map[string]*string{"BlockingWorkerID": p.BlockingWorkerID, "CanaryID": p.CanaryID} {
					if v == nil {
						continue
					}
					if !canonicalUUID.MatchString(*v) || !ids[*v] {
						t.Errorf("%s: %s = %q, want one of the worker UUIDs", w.ID, label, *v)
					}
				}
				if p.BlockingReason != nil && strings.Contains(*p.BlockingReason, tc.reason) {
					sawReason = true
				}
				gotCanary := ""
				if p.CanaryID != nil {
					gotCanary = *p.CanaryID
				}
				if gotCanary != tc.wantCanary {
					t.Errorf("%s: CanaryID = %q, want %q", w.ID, gotCanary, tc.wantCanary)
				}
				if tc.wantWorker != "" && (p.BlockingWorkerID == nil || *p.BlockingWorkerID != tc.wantWorker) {
					t.Errorf("%s: BlockingWorkerID = %v, want %s", w.ID, p.BlockingWorkerID, tc.wantWorker)
				}
			}
			if !sawReason {
				t.Errorf("no outcome carries reason %q", tc.reason)
			}
			got := mutationLog(c.Actions())
			if strings.Join(got, "\n") != strings.Join(tc.wantMut, "\n") {
				t.Errorf("mutations:\n%s", strings.Join(got, "\n"))
			}
		})
	}
}
