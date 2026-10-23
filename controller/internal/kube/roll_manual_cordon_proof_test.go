package kube

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func TestManualCanaryStillRequiresFreshIncarnation(t *testing.T) {
	for _, refusal := range []string{"nonce", "version", "uid", "heartbeat", "owner"} {
		t.Run(refusal, func(t *testing.T) {
			m, c, ws, _ := matrixFixture(t, 1)
			manualWorker(&ws[0])
			rollTickTest(t, m, ws)
			removeOldPod(t, c, "w1")
			currentProof(t, c, &ws[0], m5Now)
			switch refusal {
			case "nonce":
				ws[0].RollEvidence.RegisterNonce = token("prior-registration")
			case "version":
				ws[0].RollEvidence.RegisteredVersion = token("0.1.0")
			case "uid":
				ws[0].RollEvidence.PodUID = token("old-pod")
			case "heartbeat":
				ws[0].RollEvidence.HeartbeatAt = nil
			case "owner":
				pod, err := c.CoreV1().Pods(testConfig().Namespace).Get(context.Background(), "pod-w1", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				pod.OwnerReferences[0].UID = "wrong-owner"
				if _, err := c.CoreV1().Pods(testConfig().Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			rollTickTest(t, matrixRestart(m), ws)
			d, err := c.AppsV1().Deployments(testConfig().Namespace).Get(context.Background(), deploymentName("w1"), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			r, err := decodeRoll(d.Annotations[annotationRoll])
			if err != nil || r == nil || r.Completed {
				t.Fatalf("invalid %s completed manual reservation: %+v %v", refusal, r, err)
			}
			if len(templatePatches(c)) != 1 {
				t.Fatal("manual proof refusal replaced worker again")
			}
		})
	}
}

func TestManualUnmarkedTargetDoesNotVetoCoveredCandidate(t *testing.T) {
	candidate, manual, provider := evidence(desired("w1"), m5Now), evidence(desired("w2"), m5Now), evidence(desired("w3"), m5Now)
	manualWorker(&manual)
	objects := physicalOld(t, deployedWorker("w1", 0, "old"))
	objects = append(objects, RenderDeployment(testConfig(), manual, testSpec(t, manual.Template, manual.Size)))
	objects = append(objects, physicalOld(t, deployedWorker("w3", 0, "old"))...)
	m, c := newMat(t, objects...)
	m.now = func() time.Time { return m5Now }
	currentProof(t, c, &manual, m5Now)
	rollTickTest(t, m, []protocol.DesiredWorker{candidate, manual, provider})
	got := templatePatches(c)
	if len(got) != 1 || got[0] != deploymentName("w1") {
		t.Fatalf("manual target proof vetoed covered candidate: %v %+v", got, m.PacingOutcomes())
	}
}

func TestManualReleaseWaitsForEffectivePoll(t *testing.T) {
	m, c, ws, _ := matrixFixture(t, 2)
	cord := &mandatoryReasonCordoner{}
	m.cordoner = cord
	rollTickTest(t, m, ws)
	removeOldPod(t, c, "w1")
	currentProof(t, c, &ws[0], m5Now)
	ws[0].DrainingSince, ws[0].DrainingReason = recentDrain(), token("image_roll")
	fenced := true
	ws[0].EffectiveCordoned = &fenced // Hidden maintenance fence, no visible maintenance operation.
	rollTickTest(t, matrixRestart(m), ws)
	if len(cord.clearCalls) != 1 || cord.clearCalls[0] != "w1" || len(cord.reasons) != 1 || cord.reasons[0] != "image_roll" {
		t.Fatalf("reason-owned clear not confirmed first: %+v", cord)
	}
	if got := templatePatches(c); len(got) != 1 {
		t.Fatalf("successful release fabricated serving canary from effective-only fence: %v", got)
	}
	// A fresh poll may still report the independent fence after automatic release.
	ws[0].DrainingSince, ws[0].DrainingReason = nil, nil
	rollTickTest(t, matrixRestart(m), ws)
	if got := templatePatches(c); len(got) != 1 {
		t.Fatalf("effective-only fence supplied coverage after restart: %v", got)
	}
	fenced = false
	rollTickTest(t, matrixRestart(m), ws)
	if got := templatePatches(c); len(got) != 2 || got[1] != deploymentName("w2") {
		t.Fatalf("explicit false poll did not restore serving canary: %v", got)
	}
}
