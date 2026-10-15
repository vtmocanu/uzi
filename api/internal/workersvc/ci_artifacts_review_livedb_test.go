package workersvc

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/ciartifacts"
)

func TestCIArtifactsStorageReasonSurvivesTruncationLiveDB(t *testing.T) {
	f := newCRFix(t)
	f.svc.SetJobFiles(NewJobFiles(f.env.pool, f.env.box, JobFileLimits{StoredFilesBudgetBytes: 1}, nil))
	p := artifactParams(t, f, []byte("over budget"))
	skips := make([]ciartifacts.Entry, ciartifacts.MaxSkipped)
	for i := range skips {
		skips[i] = ciartifacts.Entry{ArtifactID: "old-skips", EntryOrdinal: i, Label: "binary", Status: "skipped", Reason: "binary"}
	}
	p.ArtifactManifest.Entries = append(skips, p.ArtifactManifest.Entries...)
	run, err := f.svc.CreateCIRepairRun(f.env.ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	m := artifactManifest(t, run)
	durable := false
	for _, e := range m.Entries {
		if e.Reason == ciartifacts.StorageBudget {
			durable = true
		}
	}
	if !durable || !strings.Contains(run.IssueDescription, "skipped: storage budget") {
		t.Errorf("storage refusal explanation lost: durable=%v description=%q", durable, run.IssueDescription)
	}
	if m.Omitted == nil || *m.Omitted != 2 {
		t.Errorf("omitted count = %v, want 2", m.Omitted)
	}
	files, chunks, reservations := artifactCounts(t, f)
	if files != 0 || chunks != 0 || reservations != 0 {
		t.Errorf("refusal retained storage: %d/%d/%d", files, chunks, reservations)
	}
	if ep, ok := f.episode(t, "artifact"); !ok || ep.Attempts != 1 {
		t.Error("logs-only attempt did not commit")
	}
}

func TestCIArtifactsSplitFleetEligibilityLiveDB(t *testing.T) {
	f := newCRFix(t)
	f.svc.SetJobFiles(NewJobFiles(f.env.pool, f.env.box, JobFileLimits{}, nil))
	run, err := f.svc.CreateCIRepairRun(f.env.ctx, artifactParams(t, f, []byte("evidence")))
	if err != nil {
		t.Fatal(err)
	}
	guardOnly := f.guardWorker(t, 1, capability.CIConfigGuardV1)
	artifactOnly := f.guardWorker(t, 1, capability.CIArtifactsV1)
	f.refuseClaim(t, guardOnly, run.ID, "guard-only")
	f.refuseClaim(t, artifactOnly, run.ID, "artifact-only")
	n, err := f.env.q.CountOnlineWorkersSatisfyingCIArtifacts(f.env.ctx, f.user)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("split fleet counted as artifact eligible: %d", n)
	}
	both := f.guardWorker(t, 1, capability.CIConfigGuardV1, capability.CIArtifactsV1)
	n, err = f.env.q.CountOnlineWorkersSatisfyingCIArtifacts(f.env.ctx, f.user)
	if err != nil || n != 1 {
		t.Errorf("joint capability worker count: %d %v", n, err)
	}
	f.acceptClaim(t, both, run.ID, "same worker supplies both capabilities")
}

func TestCIArtifactsCountRangeLiveDB(t *testing.T) {
	for _, invalid := range []bool{true, false} {
		name := "largest representable total"
		if invalid {
			name = "unrepresentable total"
		}
		t.Run(name, func(t *testing.T) {
			f := newCRFix(t)
			f.svc.SetJobFiles(NewJobFiles(f.env.pool, f.env.box, JobFileLimits{StoredFilesBudgetBytes: 1}, nil))
			p := artifactParams(t, f, []byte("over budget"))
			skips := make([]ciartifacts.Entry, ciartifacts.MaxSkipped)
			for i := range skips {
				skips[i] = ciartifacts.Entry{ArtifactID: "old-skips", EntryOrdinal: i, Label: "binary", Status: "skipped", Reason: "binary"}
			}
			p.ArtifactManifest.Entries = append(skips, p.ArtifactManifest.Entries...)
			omitted := int64(1<<63 - 1)
			if !invalid {
				omitted -= int64(len(p.ArtifactManifest.Entries))
			}
			p.ArtifactManifest.Omitted = &omitted
			run, err := f.svc.CreateCIRepairRun(f.env.ctx, p)
			if invalid {
				if err == nil || !strings.Contains(err.Error(), "manifest count is not representable") {
					t.Fatalf("count validation: %v", err)
				}
				if f.runCount(t) != 0 || f.episodeCount(t) != 0 {
					t.Fatal("invalid accounting survived rollback")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				m := artifactManifest(t, run)
				if m.Omitted == nil || *m.Omitted != omitted+2 || !strings.Contains(run.IssueDescription, "skipped: storage budget") {
					t.Fatal("valid refusal accounting changed")
				}
			}
			files, chunks, reservations := artifactCounts(t, f)
			if files != 0 || chunks != 0 || reservations != 0 {
				t.Fatalf("count case wrote storage: %d/%d/%d", files, chunks, reservations)
			}
		})
	}
}
