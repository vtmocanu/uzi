package kube

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

var coverageGroupRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ownedMarkedDeployment is markedDeployment with the marker owner taken from the
// worker's own evidence, so two workers can carry different coverage.
func ownedMarkedDeployment(t *testing.T, w protocol.DesiredWorker, completed bool) *appsv1.Deployment {
	t.Helper()
	d := markedDeployment(t, testConfig(), w, completed)
	r, err := decodeRoll(d.Annotations[annotationRoll])
	if err != nil || r == nil {
		t.Fatalf("decode fixture marker: %v", err)
	}
	r.Coverage.Owner = w.RollEvidence.OwnerID
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	d.Annotations[annotationRoll] = string(raw)
	return d
}

func TestRollMarkerReportRetainedCompletedMarker(t *testing.T) {
	w := evidence(desired("w1"), m5Now)
	d := markedDeployment(t, testConfig(), w, true)
	m, _ := newMat(t, d)
	rollTickTest(t, m, []protocol.DesiredWorker{w})

	got := m.RollMarkerReports()[w.ID]
	if got == nil {
		t.Fatal("no roll marker report for a worker holding a retained marker")
	}
	if got.State != protocol.RollMarkerPresent || !got.Canary || !got.Completed || !got.Reservation || got.TargetVersion != "1.0.0" {
		t.Fatalf("unexpected report: %+v", got)
	}
	if !coverageGroupRE.MatchString(got.CoverageGroup) {
		t.Fatalf("coverage_group %q is not 16 lowercase hex chars", got.CoverageGroup)
	}
	// The display report must not widen PacingOutcomes, which omits completed rows.
	if p, ok := m.PacingOutcomes()[w.ID]; ok {
		t.Fatalf("completed outcome leaked into PacingOutcomes: %+v", p)
	}
	raw, _ := json.Marshal(got)
	for _, secret := range []string{"prior-registration", "owner-A", d.Annotations[AnnotationSpecHash], "worker:old", "harbor.example.com"} {
		if secret != "" && regexp.MustCompile(regexp.QuoteMeta(secret)).Match(raw) {
			t.Fatalf("report leaks %q: %s", secret, raw)
		}
	}
}

func TestRollMarkerReportAbsent(t *testing.T) {
	w := evidence(desired("w1"), m5Now)
	d := markedDeployment(t, testConfig(), w, true)
	delete(d.Annotations, annotationRoll)
	m, _ := newMat(t, d)
	rollTickTest(t, m, []protocol.DesiredWorker{w})
	got := m.RollMarkerReports()[w.ID]
	if got == nil || got.State != protocol.RollMarkerAbsent || got.Reservation || got.CoverageGroup != "" {
		t.Fatalf("unexpected report: %+v", got)
	}
}

func TestRollMarkerReportMalformedIsUnreadable(t *testing.T) {
	w := evidence(desired("w1"), m5Now)
	d := markedDeployment(t, testConfig(), w, true)
	d.Annotations[annotationRoll] = "{not json"
	m, _ := newMat(t, d)
	rollTickTest(t, m, []protocol.DesiredWorker{w})
	got := m.RollMarkerReports()[w.ID]
	if got == nil || got.State != protocol.RollMarkerUnreadable || !got.Reservation {
		t.Fatalf("unexpected report: %+v", got)
	}
	if got.Blocked == nil {
		t.Fatal("malformed marker carried no blocker")
	}
	if raw, _ := json.Marshal(got); regexp.MustCompile(`not json`).Match(raw) {
		t.Fatalf("report leaks the raw annotation: %s", raw)
	}
}

func TestRollMarkerReportListErrorIsUnavailable(t *testing.T) {
	w := evidence(desired("w1"), m5Now)
	m, c := newMat(t, markedDeployment(t, testConfig(), w, true))
	obs, err := m.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	_ = m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs)
	got := m.RollMarkerReports()[w.ID]
	if got == nil || got.State != protocol.RollMarkerUnavailable {
		t.Fatalf("unexpected report: %+v", got)
	}
	if got.Blocked == nil {
		t.Fatal("unavailable report carried no blocker")
	}
}

// A blocker assigned during planning, after the snapshot, must be the one reported.
func TestRollMarkerReportCarriesPlanningBlocker(t *testing.T) {
	w := evidence(desired("w1"), m5Now)
	m, _ := newMat(t, markedDeployment(t, testConfig(), w, true))
	m.cfg.RollTargetVersion = ""
	rollTickTest(t, m, []protocol.DesiredWorker{w})
	got := m.RollMarkerReports()[w.ID]
	if got == nil || got.Blocked == nil || *got.Blocked != "target version unknown; configure exact override" {
		t.Fatalf("unexpected report: %+v", got)
	}
}

func TestRollMarkerReportCoverageGroup(t *testing.T) {
	w1 := evidence(desired("w1"), m5Now)
	w2 := evidence(desired("w2"), m5Now)
	w3 := evidence(desired("w3"), m5Now)
	w3.RollEvidence.OwnerID = "owner-B"
	m, _ := newMat(t, ownedMarkedDeployment(t, w1, true), ownedMarkedDeployment(t, w2, true), ownedMarkedDeployment(t, w3, true))
	rollTickTest(t, m, []protocol.DesiredWorker{w1, w2, w3})
	r := m.RollMarkerReports()
	for _, id := range []string{"w1", "w2", "w3"} {
		if r[id] == nil || r[id].State != protocol.RollMarkerPresent {
			t.Fatalf("%s: %+v", id, r[id])
		}
	}
	if r["w1"].CoverageGroup != r["w2"].CoverageGroup {
		t.Fatalf("same coverage, different group: %q vs %q", r["w1"].CoverageGroup, r["w2"].CoverageGroup)
	}
	if r["w1"].CoverageGroup == r["w3"].CoverageGroup {
		t.Fatalf("different owners share group %q", r["w1"].CoverageGroup)
	}
}

func TestRollMarkerReportsReturnsCopy(t *testing.T) {
	w := evidence(desired("w1"), m5Now)
	m, _ := newMat(t, markedDeployment(t, testConfig(), w, true))
	rollTickTest(t, m, []protocol.DesiredWorker{w})
	a := m.RollMarkerReports()
	a[w.ID].State = "mutated"
	delete(a, w.ID)
	if b := m.RollMarkerReports()[w.ID]; b == nil || b.State != protocol.RollMarkerPresent {
		t.Fatalf("caller mutation reached the materializer: %+v", b)
	}
}
