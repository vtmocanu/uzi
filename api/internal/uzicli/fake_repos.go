package uzicli

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// fake_repos.go holds the FakeClient repo/worker/project-sync methods … PRD #1017.

func (f *FakeClient) ListWorkers(context.Context) ([]apitypes.WorkerDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Workers, nil
}

func (f *FakeClient) DeleteWorker(_ context.Context, id string) error {
	f.LastDeletedWorkerID = id
	return f.Err
}

func (f *FakeClient) SetWorkerBindMode(_ context.Context, id, mode, label string) (apitypes.WorkerDTO, error) {
	f.LastSetTokenWorkerID = id
	f.LastSetTokenLabel = label
	f.LastSetTokenMode = mode
	if f.Err != nil {
		return apitypes.WorkerDTO{}, f.Err
	}
	return f.SetTokenWorker, nil
}

func (f *FakeClient) ListRepos(context.Context) ([]apitypes.RepoDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Repos, nil
}

func (f *FakeClient) DeleteRepo(_ context.Context, id string) error {
	f.LastDeletedRepoID = id
	return f.Err
}

func (f *FakeClient) GetProjectSyncStatus(_ context.Context, repoID string) (ProjectSyncStatus, error) {
	f.LastProjectSyncStatusRepoID = repoID
	if f.GetProjectSyncStatusErr != nil {
		return ProjectSyncStatus{}, f.GetProjectSyncStatusErr
	}
	if f.Err != nil {
		return ProjectSyncStatus{}, f.Err
	}
	return f.ProjectSyncStatusResult, nil
}

func (f *FakeClient) ResyncProjectSync(_ context.Context, repoID string) error {
	f.LastResyncProjectSyncRepoID = repoID
	if f.ResyncProjectSyncErr != nil {
		return f.ResyncProjectSyncErr
	}
	return f.Err
}

// forge-view reads (PRD #1255 M3). Each records its args, prefers a per-verb Err,
// then the blanket Err, else returns the canned reply — the house fake pattern.

func (f *FakeClient) ListPulls(_ context.Context, repoID string) ([]apitypes.PullDTO, error) {
	f.ListPullsCalls++
	f.LastListPullsRepoID = repoID
	if f.ListPullsErr != nil {
		return nil, f.ListPullsErr
	}
	if f.Err != nil {
		return nil, f.Err
	}
	return f.PullsResult, nil
}

func (f *FakeClient) GetPull(_ context.Context, repoID string, iid int64) (apitypes.PullDetailDTO, error) {
	f.GetPullCalls++
	f.LastGetPullRepoID = repoID
	f.LastGetPullIID = iid
	if f.GetPullHook != nil {
		return f.GetPullHook(repoID, iid)
	}
	if f.GetPullErr != nil {
		return apitypes.PullDetailDTO{}, f.GetPullErr
	}
	if f.Err != nil {
		return apitypes.PullDetailDTO{}, f.Err
	}
	return f.PullDetailResult, nil
}

func (f *FakeClient) ListCIRuns(_ context.Context, repoID string, limit int) ([]apitypes.CIRunDTO, string, error) {
	f.ListCIRunsCalls++
	f.LastCIRunsRepoID = repoID
	f.LastCIRunsLimit = limit
	if f.ListCIRunsErr != nil {
		return nil, "", f.ListCIRunsErr
	}
	if f.Err != nil {
		return nil, "", f.Err
	}
	return f.CIRunsResult, f.CIRunsUnsupported, nil
}

func (f *FakeClient) GetCIRun(_ context.Context, repoID string, runID int64) (apitypes.CIRunDetailDTO, error) {
	f.GetCIRunCalls++
	f.LastGetCIRunRepoID = repoID
	f.LastGetCIRunID = runID
	if f.GetCIRunErr != nil {
		return apitypes.CIRunDetailDTO{}, f.GetCIRunErr
	}
	if f.Err != nil {
		return apitypes.CIRunDetailDTO{}, f.Err
	}
	return f.CIRunDetailResult, nil
}

func (f *FakeClient) CreateCIFixRun(_ context.Context, repoID, ref string) (apitypes.RunDTO, error) {
	f.LastCIFixRepoID = repoID
	f.LastCIFixRef = ref
	if f.CreateCIFixRunErr != nil {
		return apitypes.RunDTO{}, f.CreateCIFixRunErr
	}
	if f.Err != nil {
		return apitypes.RunDTO{}, f.Err
	}
	return f.CIFixRunResult, nil
}
