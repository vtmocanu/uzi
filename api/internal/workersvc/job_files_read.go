package workersvc

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// job_files_read.go is the caller-facing read side of job files (PRD #1909 M5): the files,
// refused outputs and sources of one job (GET /api/v1/jobs/{id}/files and the result body), and
// the owner-scoped download behind GET /api/v1/files/{id}.

const (
	// maxJobResultFiles bounds the files one job lists. The store's own caps (inputs and output
	// files per job) sit far below it; it is a backstop for the query, never a silent truncation
	// in practice.
	maxJobResultFiles = 1000
	// maxJobResultRefusals bounds the refused outputs one job lists: the per-job refusal cap plus
	// the server's at most two generation rows, well under this.
	maxJobResultRefusals = 1000
	// MaxJobResultSources bounds the fetch rows the result lists (the oldest first). A job that
	// fetched more shows its first MaxJobResultSources; the complete log is the fetch service's.
	MaxJobResultSources = 500
)

// JobFileInfo is the caller-safe metadata of one file of a job. DisplayName is untrusted text.
// SourceURL is non-nil only for an output whose sha256 equals the sha256 of an allowed fetch in
// the same run (store.ListJobFilesForRun): it is never taken from anything the agent claimed.
type JobFileInfo struct {
	ID          uuid.UUID
	DisplayName string
	ContentType string
	ByteSize    int64
	Sha256      string
	Direction   string
	State       string
	ExpiresAt   *time.Time
	SourceURL   *string
}

// JobRefusedFile is an output the job offered (or the server would have generated) that was not
// stored, with the reason.
type JobRefusedFile struct {
	DisplayName string
	ByteSize    int64
	Reason      string
}

// JobFilesView is one job's files and refused outputs.
type JobFilesView struct {
	Files   []JobFileInfo
	Refused []JobRefusedFile
}

// JobSource is one row of a run's source log, as the fetch service recorded it.
type JobSource struct {
	URL         string
	FinalURL    string
	Verdict     string
	Reason      string
	HTTPStatus  int
	ContentType string
	ByteSize    int64
	Sha256      string
	FetchedAt   time.Time
}

// jobFilesReadStore is the query surface the read side needs beyond jobStore. *store.Queries
// satisfies it; a fake store that does not reads as unavailable, like jobStore.
type jobFilesReadStore interface {
	ListJobFilesForRun(ctx context.Context, arg store.ListJobFilesForRunParams) ([]store.ListJobFilesForRunRow, error)
	ListJobOutputRefusalsForRun(ctx context.Context, arg store.ListJobOutputRefusalsForRunParams) ([]store.ListJobOutputRefusalsForRunRow, error)
	ListJobSourcesForRun(ctx context.Context, arg store.ListJobSourcesForRunParams) ([]store.ListJobSourcesForRunRow, error)
}

func (s *Service) jobFilesReads() (jobFilesReadStore, error) {
	q, ok := s.q.(jobFilesReadStore)
	if !ok {
		return nil, errHarnessStoreUnavailable
	}
	return q, nil
}

// ListJobFilesForCaller returns the files and refused outputs of a job the caller can see, else
// ErrJobNotFound. The visibility check is GetJobForCaller's, the one every /api/v1 job read uses.
func (s *Service) ListJobFilesForCaller(ctx context.Context, caller JobCaller, runID uuid.UUID) (JobFilesView, error) {
	if _, err := s.GetJobForCaller(ctx, caller, runID); err != nil {
		return JobFilesView{}, err
	}
	q, err := s.jobFilesReads()
	if err != nil {
		return JobFilesView{}, err
	}
	rows, err := q.ListJobFilesForRun(ctx, store.ListJobFilesForRunParams{RunID: pgtype.UUID{Bytes: runID, Valid: true}, MaxRows: maxJobResultFiles})
	if err != nil {
		return JobFilesView{}, err
	}
	refs, err := q.ListJobOutputRefusalsForRun(ctx, store.ListJobOutputRefusalsForRunParams{RunID: runID, MaxRows: maxJobResultRefusals})
	if err != nil {
		return JobFilesView{}, err
	}
	out := JobFilesView{Files: make([]JobFileInfo, 0, len(rows)), Refused: make([]JobRefusedFile, 0, len(refs))}
	for _, f := range rows {
		fi := JobFileInfo{
			ID: f.ID, DisplayName: f.DisplayName, ContentType: f.ContentType, ByteSize: f.ByteSize,
			Sha256: f.Sha256, Direction: f.Direction, State: f.State,
		}
		if f.ExpiresAt.Valid {
			t := f.ExpiresAt.Time
			fi.ExpiresAt = &t
		}
		if f.SourceUrl != "" {
			u := f.SourceUrl
			fi.SourceURL = &u
		}
		out.Files = append(out.Files, fi)
	}
	for _, r := range refs {
		out.Refused = append(out.Refused, JobRefusedFile{DisplayName: r.DisplayName, ByteSize: r.ByteSize, Reason: r.Reason})
	}
	return out, nil
}

// ListJobSourcesForCaller returns the source log of a job the caller can see (that run's
// run_fetches rows only), else ErrJobNotFound.
func (s *Service) ListJobSourcesForCaller(ctx context.Context, caller JobCaller, runID uuid.UUID) ([]JobSource, error) {
	if _, err := s.GetJobForCaller(ctx, caller, runID); err != nil {
		return nil, err
	}
	q, err := s.jobFilesReads()
	if err != nil {
		return nil, err
	}
	rows, err := q.ListJobSourcesForRun(ctx, store.ListJobSourcesForRunParams{RunID: runID, MaxRows: MaxJobResultSources})
	if err != nil {
		return nil, err
	}
	out := make([]JobSource, 0, len(rows))
	for _, r := range rows {
		out = append(out, JobSource{
			URL: r.Url, FinalURL: r.FinalUrl, Verdict: r.Verdict, Reason: r.Reason, HTTPStatus: int(r.HttpStatus),
			ContentType: r.ContentType, ByteSize: r.Bytes, Sha256: r.Sha256, FetchedAt: r.FinishedAt.Time,
		})
	}
	return out, nil
}

// OpenForCaller is Open for a /api/v1 caller: the file must be visible to the caller
// (store.GetJobFileForCaller: the caller's own, and for a file of a job the job's visibility, for an
// unattached input the uploading product), else ErrJobFileNotFound. A file another user's or
// another product's reads exactly as one that does not exist. An expired file is
// ErrJobFileExpired. The returned reader has Open's integrity behaviour.
func (j *JobFiles) OpenForCaller(ctx context.Context, id uuid.UUID, caller JobCaller) (store.JobFile, io.Reader, error) {
	if j.box == nil {
		return store.JobFile{}, nil, ErrJobFilesUnavailable
	}
	row, err := store.New(j.db).GetJobFileForCaller(ctx, store.GetJobFileForCallerParams{ID: id, UserID: caller.UserID, ProductID: caller.product()})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.JobFile{}, nil, ErrJobFileNotFound
		}
		return store.JobFile{}, nil, err
	}
	if row.State == JobFileExpired {
		return store.JobFile{}, nil, ErrJobFileExpired
	}
	// The row is visible; Open re-reads it by owner and streams it. A file that expired or was
	// reclaimed between the two reads surfaces as Open's own ErrJobFileExpired / not found.
	return j.Open(ctx, id, row.UserID)
}
