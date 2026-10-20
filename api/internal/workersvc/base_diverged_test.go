package workersvc

import (
	"context"
	"strconv"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
)

func TestBaseDivergedClassification(t *testing.T) {
	origin := "base_diverged"
	if !failOriginSet[origin] || !workerReportableFailOrigins[origin] || !neverJudgeFailOrigins[origin] {
		t.Fatal("base_diverged must be stored, worker-reportable, and unconditionally unjudged")
	}
	if got := CoerceFailOrigin(&origin); got == nil || *got != origin {
		t.Fatalf("coercion: %v", got)
	}
	for name, set := range map[string]map[string]bool{
		"preStartInfraFailOrigins": preStartInfraFailOrigins,
		"envPublishFailOrigins":    envPublishFailOrigins,
		"humanLandableFailOrigins": humanLandableFailOrigins,
	} {
		if set[origin] {
			t.Fatalf("base_diverged unexpectedly belongs to %s", name)
		}
	}
	for _, iter := range []int32{0, 7} {
		for _, origin := range []string{"base_diverged", "agent_failure"} {
			t.Run(origin+"/"+strconv.Itoa(int(iter)), func(t *testing.T) {
				fs, svc, run := eligibleFixture(t)
				run.Status = "failed"
				run.FailOrigin = pgconv.TextOrNull(origin)
				run.IterationCount = iter
				svc.maybeEnqueueJudge(context.Background(), run)
				if (fs.createdJudgeRun != nil) != (origin == "agent_failure") {
					t.Fatalf("judge enqueue for %s: %+v", origin, fs.createdJudgeRun)
				}
			})
		}
	}
}
