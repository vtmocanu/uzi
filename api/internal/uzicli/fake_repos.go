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
