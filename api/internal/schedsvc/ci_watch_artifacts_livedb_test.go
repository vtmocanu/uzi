package schedsvc

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/ciartifacts"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func (l *ciLive) enableArtifacts(t *testing.T) {
	t.Helper()
	if _, err := l.pool.Exec(l.ctx, `UPDATE run_schedules SET ci_artifacts='["logs*"]' WHERE id=$1`, l.sched.ID); err != nil {
		t.Fatal(err)
	}
	r := l.cf.runs[0]
	l.cf.artifacts = []forge.Artifact{ciArtifact(r, 5)}
	l.cf.download = func(context.Context, int64, int64) ([]byte, error) { return ciZIP(t, "live evidence"), nil }
}
func (l *ciLive) assertNoArtifactStorage(t *testing.T) {
	t.Helper()
	var files, chunks int
	if err := l.pool.QueryRow(l.ctx, `SELECT count(DISTINCT f.id),count(c.file_id) FROM job_files f LEFT JOIN job_file_chunks c ON c.file_id=f.id WHERE f.user_id=$1`, l.user).Scan(&files, &chunks); err != nil {
		t.Fatal(err)
	}
	if files != 0 || chunks != 0 {
		t.Fatalf("files=%d chunks=%d", files, chunks)
	}
}
func TestCIArtifactsFireStoresSealedInputsLiveDB(t *testing.T) {
	l := newCILive(t, ciRed("wf", 100, 0))
	l.enableArtifacts(t)
	out := l.fire(t)
	if len(out.Started) != 1 {
		t.Fatalf("%+v", out)
	}
	var raw, sealed []byte
	var state, name, digest string
	var size int64
	if err := l.pool.QueryRow(l.ctx, `SELECT r.ci_artifact_manifest,f.state,f.storage_name,f.sha256,f.byte_size,c.sealed FROM runs r JOIN job_files f ON f.run_id=r.id JOIN job_file_chunks c ON c.file_id=f.id WHERE r.id=$1`, out.Started[0].RunID).Scan(&raw, &state, &name, &digest, &size, &sealed); err != nil {
		t.Fatal(err)
	}
	var m ciartifacts.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	wantDigest, wantName := ciartifacts.Identity([]byte("live evidence"))
	if state != "attached" || size != 13 || name != wantName || digest != wantDigest || string(sealed) == "live evidence" || len(sealed) == 0 {
		t.Fatal("invalid sealed input")
	}
	if m.Execution == nil || m.Execution.RunID != 100 || m.Execution.Attempt == nil || *m.Execution.Attempt != 1 || len(m.Entries) != 1 || m.Entries[0].ManifestID == "" {
		t.Fatalf("%+v", m)
	}
}
func TestCIArtifactsObsoleteCandidatePreservesEpisodeLiveDB(t *testing.T) {
	for name, mutate := range ciMutations() {
		t.Run(name, func(t *testing.T) {
			l := newCILive(t, ciRed("wf", 90, -1))
			if len(l.fire(t).Started) != 1 {
				t.Fatal("seed episode")
			}
			l.endRuns(t)
			before := l.episode(t, "wf")
			r := ciRed("wf", 100, 0)
			l.setRuns(r)
			l.enableArtifacts(t)
			// The old signature would trip no-progress. The race must be observed
			// after collection, before either the halt or the create transaction.
			l.cf.tails[1001] = l.cf.tails[901]
			snap, err := workersvc.BuildFailureSnapshot(l.ctx, l.cf, 42, store.PipelineStatus{PipelineID: 100}, 5, 1000)
			if err != nil {
				t.Fatal(err)
			}
			if reason := workersvc.CILoopGuard(before, workersvc.FailureSignature(snap), 3); reason != workersvc.CIHaltNoProgress {
				t.Fatalf("positive loop guard control: %q", reason)
			}
			l.cf.download = func(context.Context, int64, int64) ([]byte, error) {
				l.assertNoArtifactStorage(t)
				changed := r
				mutate(&changed)
				l.cf.runs[0] = changed
				l.cf.getRun = func(context.Context, int64, int) (forge.WorkflowRun, error) { return changed, nil }
				return ciZIP(t, "collected but obsolete"), nil
			}
			out := l.fire(t)
			if len(out.Started) != 0 || l.runCount(t) != 1 {
				t.Fatalf("%+v runs=%d", out, l.runCount(t))
			}
			if after := l.episode(t, "wf"); !reflect.DeepEqual(before, after) {
				t.Fatalf("episode changed before=%+v after=%+v", before, after)
			}
			l.assertNoArtifactStorage(t)
		})
	}
}
func TestCIArtifactsFinalRevalidationPreservesEmptyDBLiveDB(t *testing.T) {
	for _, status := range []string{"green", "in_progress", "identity"} {
		t.Run(status, func(t *testing.T) {
			r := ciRed("wf", 100, 0)
			l := newCILive(t, r)
			l.enableArtifacts(t)
			l.cf.onPath = func() {
				changed := r
				switch status {
				case "green":
					changed.Conclusion = "success"
				case "in_progress":
					changed.Status = "in_progress"
				case "identity":
					changed.SHA = "other"
				}
				l.cf.runs[0] = changed
			}
			l.fire(t)
			if l.runCount(t) != 0 || l.episodes(t) != 0 {
				t.Fatal("obsolete write")
			}
			l.assertNoArtifactStorage(t)
		})
	}
}
