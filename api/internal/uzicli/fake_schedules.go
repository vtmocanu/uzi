package uzicli

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// fake_schedules.go holds the FakeClient schedule methods (uzi schedule) split
// out of fake.go (PRD #1017).

func (f *FakeClient) ListSchedules(context.Context) ([]apitypes.ScheduleDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Schedules, nil
}

func (f *FakeClient) CreateSchedule(_ context.Context, repoID string, req apitypes.ScheduleRequest) (apitypes.ScheduleDTO, error) {
	f.LastCreateSchedRepo = repoID
	f.LastCreateSchedReq = req
	// Accumulate every call (before the error branch) so a fan-out test can assert the shared
	// sibling_group_id across all N create bodies even when a later call is modelled as failing.
	f.AllCreateSchedRepos = append(f.AllCreateSchedRepos, repoID)
	f.AllCreateSchedReqs = append(f.AllCreateSchedReqs, req)
	if f.Err != nil {
		return apitypes.ScheduleDTO{}, f.Err
	}
	return f.CreatedSchedule, nil
}

// GetSchedule mirrors the owner-scoped endpoint: a key present in ScheduleByID is the
// caller's own schedule; a key absent is a foreign/unknown id → ExitNotFound (exit 4).
func (f *FakeClient) GetSchedule(_ context.Context, id string) (apitypes.ScheduleDTO, error) {
	if f.Err != nil {
		return apitypes.ScheduleDTO{}, f.Err
	}
	if s, ok := f.ScheduleByID[id]; ok {
		return s, nil
	}
	return apitypes.ScheduleDTO{}, Exitf(ExitNotFound, "schedule %s not found", id)
}

func (f *FakeClient) SetScheduleEnabled(_ context.Context, id string, enabled bool) (apitypes.ScheduleDTO, error) {
	f.LastSchedEnabledID = id
	f.LastSchedEnabledVal = enabled
	if f.Err != nil {
		return apitypes.ScheduleDTO{}, f.Err
	}
	return f.EnabledSchedule, nil
}

func (f *FakeClient) PatchSchedule(_ context.Context, id string, req apitypes.ScheduleRequest) (apitypes.ScheduleDTO, error) {
	f.LastPatchSchedID = id
	f.LastPatchSchedReq = req
	if f.Err != nil {
		return apitypes.ScheduleDTO{}, f.Err
	}
	return f.PatchedSchedule, nil
}

func (f *FakeClient) DeleteSchedule(_ context.Context, id string) error {
	f.LastDeletedSchedID = id
	return f.Err
}

func (f *FakeClient) RunScheduleNow(_ context.Context, id string) (apitypes.RunNowResponse, error) {
	f.LastRunNowSchedID = id
	if f.Err != nil {
		return apitypes.RunNowResponse{}, f.Err
	}
	return f.RunNowResult, nil
}

func (f *FakeClient) ListScheduleCatalog(context.Context) (apitypes.ScheduleCatalogResponse, error) {
	if f.Err != nil {
		return apitypes.ScheduleCatalogResponse{}, f.Err
	}
	return f.CatalogResult, nil
}

// EnableCatalogSchedule records EACH (repo, slug) call in order before the error branch, so
// a multi-repo fan-out test can assert the exact sequence of per-repo enables the command
// issued even when a later call is modelled as failing.
func (f *FakeClient) EnableCatalogSchedule(_ context.Context, repoID, slug string) (apitypes.ScheduleDTO, bool, error) {
	f.EnabledCatalogCalls = append(f.EnabledCatalogCalls, EnableCatalogCall{RepoID: repoID, Slug: slug})
	if f.Err != nil {
		return apitypes.ScheduleDTO{}, false, f.Err
	}
	return f.EnabledCatalogDTO, f.EnabledCatalogCreated, nil
}

func (f *FakeClient) ResetSchedule(_ context.Context, id string) (apitypes.ScheduleDTO, error) {
	f.LastResetSchedID = id
	if f.Err != nil {
		return apitypes.ScheduleDTO{}, f.Err
	}
	return f.ResetScheduleDTO, nil
}

func (f *FakeClient) CloneSchedule(_ context.Context, id, repoID string) (apitypes.ScheduleDTO, error) {
	f.LastCloneSchedID = id
	f.LastCloneRepoID = repoID
	if f.Err != nil {
		return apitypes.ScheduleDTO{}, f.Err
	}
	return f.ClonedSchedule, nil
}

// AddScheduleRepo records the (source id, target repo id) it was called with and returns the
// canned sibling DTO. AddRepoErr wins over the blanket Err so a test can model the
// 409-duplicate path precisely (Exitf(ExitConflict, …)) while the capture still proves the
// call was reached.
func (f *FakeClient) AddScheduleRepo(_ context.Context, id, repoID string) (apitypes.ScheduleDTO, error) {
	f.LastAddRepoSchedID = id
	f.LastAddRepoRepoID = repoID
	if f.AddRepoErr != nil {
		return apitypes.ScheduleDTO{}, f.AddRepoErr
	}
	if f.Err != nil {
		return apitypes.ScheduleDTO{}, f.Err
	}
	return f.AddRepoSchedule, nil
}

// CheckRepoLabels records the (repo, labels) it was asked about and returns the canned
// MissingLabels set. CheckLabelsErr wins over the blanket Err so a test can model a forge
// read failure precisely while the call capture still proves the check was reached.
func (f *FakeClient) CheckRepoLabels(_ context.Context, repoID string, labels []string) ([]string, error) {
	f.CheckLabelsCalls = append(f.CheckLabelsCalls, LabelCall{RepoID: repoID, Labels: labels})
	if f.CheckLabelsErr != nil {
		return nil, f.CheckLabelsErr
	}
	if f.Err != nil {
		return nil, f.Err
	}
	return f.MissingLabels, nil
}

// EnsureRepoLabels records the (repo, labels) confirm call. EnsureLabelsErr wins over the
// blanket Err, mirroring CheckRepoLabels.
func (f *FakeClient) EnsureRepoLabels(_ context.Context, repoID string, labels []string) error {
	f.EnsureLabelsCalls = append(f.EnsureLabelsCalls, LabelCall{RepoID: repoID, Labels: labels})
	if f.EnsureLabelsErr != nil {
		return f.EnsureLabelsErr
	}
	return f.Err
}
