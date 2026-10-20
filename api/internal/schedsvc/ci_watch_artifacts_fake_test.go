package schedsvc

import (
	"context"
	"fmt"
	"github.com/vtmocanu/uzi/api/internal/forge"
)

func (f *ciForge) GetWorkflowRun(ctx context.Context, _ int64, id int64) (forge.WorkflowRun, error) {
	f.mu.Lock()
	if !hasCIReadLimits(ctx) {
		f.unlimited = append(f.unlimited, "GetWorkflowRun")
	}
	f.getCalls = append(f.getCalls, id)
	n := len(f.getCalls)
	f.mu.Unlock()
	if f.getRun != nil {
		return f.getRun(ctx, id, n)
	}
	for _, r := range f.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return forge.WorkflowRun{}, fmt.Errorf("fixture run %d missing", id)
}
func (f *ciForge) ListRunArtifacts(ctx context.Context, _, _ int64) ([]forge.Artifact, error) {
	f.artifactCalls++
	if f.listArtifacts != nil {
		return f.listArtifacts(ctx)
	}
	return f.artifacts, nil
}
func (f *ciForge) DownloadArtifact(ctx context.Context, _, id, max int64) ([]byte, error) {
	f.downloadCalls = append(f.downloadCalls, id)
	if f.download != nil {
		return f.download(ctx, id, max)
	}
	return nil, fmt.Errorf("unexpected artifact download")
}
