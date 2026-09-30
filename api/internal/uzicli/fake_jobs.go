package uzicli

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// fake_jobs.go holds the FakeClient job methods (uzi job, PRD #1908 M7).

func (f *FakeClient) JobCreate(_ context.Context, req apitypes.V1JobCreateRequest) (apitypes.V1JobDTO, error) {
	f.JobCreateReqs = append(f.JobCreateReqs, req)
	if f.Err != nil {
		return apitypes.V1JobDTO{}, f.Err
	}
	return f.JobCreated, nil
}

func (f *FakeClient) JobGet(_ context.Context, id string) (apitypes.V1JobDTO, error) {
	if f.Err != nil {
		return apitypes.V1JobDTO{}, f.Err
	}
	if j, ok := f.JobByID[id]; ok {
		return j, nil
	}
	return apitypes.V1JobDTO{}, Exitf(ExitNotFound, "job %s not found", id)
}

func (f *FakeClient) JobResult(_ context.Context, id string) (apitypes.V1JobResultDTO, error) {
	if f.Err != nil {
		return apitypes.V1JobResultDTO{}, f.Err
	}
	if r, ok := f.JobResultByID[id]; ok {
		return r, nil
	}
	return apitypes.V1JobResultDTO{}, Exitf(ExitNotFound, "job %s not found", id)
}

func (f *FakeClient) JobCancel(_ context.Context, id string) (apitypes.V1JobDTO, error) {
	f.JobCancelIDs = append(f.JobCancelIDs, id)
	if f.Err != nil {
		return apitypes.V1JobDTO{}, f.Err
	}
	if j, ok := f.JobByID[id]; ok {
		return j, nil
	}
	return apitypes.V1JobDTO{}, Exitf(ExitNotFound, "job %s not found", id)
}

func (f *FakeClient) JobList(_ context.Context, limit int, cursor string) (apitypes.V1JobListDTO, error) {
	f.JobListCalls = append(f.JobListCalls, JobListCall{Limit: limit, Cursor: cursor})
	if f.Err != nil {
		return apitypes.V1JobListDTO{}, f.Err
	}
	return f.JobListPage, nil
}

// JobListCall records one JobList request.
type JobListCall struct {
	Limit  int
	Cursor string
}
