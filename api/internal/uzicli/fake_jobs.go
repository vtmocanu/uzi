package uzicli

import (
	"context"
	"io"
	"strconv"
	"strings"

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

// FakeUpload records one UploadFile call: the part name, the declared size and sha256, and the
// bytes the fake read from the stream.
type FakeUpload struct {
	Name   string
	Size   int64
	Sha256 string
	Body   string
}

func (f *FakeClient) JobFiles(_ context.Context, id string) (apitypes.V1JobFilesDTO, error) {
	if f.Err != nil {
		return apitypes.V1JobFilesDTO{}, f.Err
	}
	if d, ok := f.FilesByJob[id]; ok {
		return d, nil
	}
	return apitypes.V1JobFilesDTO{}, Exitf(ExitNotFound, "job %s not found", id)
}

func (f *FakeClient) UploadFile(_ context.Context, name string, size int64, sha256 string, r io.Reader) (apitypes.V1FileDTO, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return apitypes.V1FileDTO{}, err
	}
	f.Uploads = append(f.Uploads, FakeUpload{Name: name, Size: size, Sha256: sha256, Body: string(b)})
	if f.Err != nil {
		return apitypes.V1FileDTO{}, f.Err
	}
	id := "file-" + strconv.Itoa(len(f.Uploads))
	if n := len(f.Uploads); n <= len(f.UploadedIDs) {
		id = f.UploadedIDs[n-1]
	}
	return apitypes.V1FileDTO{ID: id, DisplayName: name, ByteSize: size, Sha256: sha256, State: "unattached"}, nil
}

func (f *FakeClient) DownloadFile(_ context.Context, id string) (*FileDownload, error) {
	f.DownloadIDs = append(f.DownloadIDs, id)
	if f.Err != nil {
		return nil, f.Err
	}
	return &FileDownload{Body: io.NopCloser(strings.NewReader(f.DownloadBody)), StorageName: f.DownloadName, Size: int64(len(f.DownloadBody))}, nil
}

func (f *FakeClient) AdminProductSkills(_ context.Context, productID string) (apitypes.ProductSkillsDTO, error) {
	if f.Err != nil {
		return apitypes.ProductSkillsDTO{}, f.Err
	}
	if d, ok := f.ProductSkills[productID]; ok {
		return d, nil
	}
	return apitypes.ProductSkillsDTO{}, Exitf(ExitNotFound, "product %s not found", productID)
}

func (f *FakeClient) AdminProductConnections(_ context.Context, productID string) (ProductConnections, error) {
	if f.Err != nil {
		return ProductConnections{}, f.Err
	}
	if d, ok := f.ProductConns[productID]; ok {
		return d, nil
	}
	return ProductConnections{}, Exitf(ExitNotFound, "product %s not found", productID)
}

func (f *FakeClient) AdminProductEgressProfiles(_ context.Context, productID string) ([]ProductEgressProfile, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	if d, ok := f.ProductEgress[productID]; ok {
		return d, nil
	}
	return nil, Exitf(ExitNotFound, "product %s not found", productID)
}
