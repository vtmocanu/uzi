package uzicli

// fake_recovery.go holds the FakeClient durable-recovery archive methods (PRD #1296 M5),
// split out of fake.go like fake_runs.go.

import (
	"context"
	"io"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// RecoveryArchives returns the canned per-run summary. An absent run id yields the
// zero-value summary (Supported=false, Archives=nil), mirroring the server's empty result
// for a run with no captures — so a run-detail test exercises the honest "none" path with
// no special seeding. RecoveryArchivesErr wins over the blanket Err so a test can model a
// summary read that succeeds while a later download fails (and vice versa).
func (f *FakeClient) RecoveryArchives(_ context.Context, runID string) (apitypes.RecoveryArchiveSummaryDTO, error) {
	if f.RecoveryArchivesErr != nil {
		return apitypes.RecoveryArchiveSummaryDTO{}, f.RecoveryArchivesErr
	}
	if f.Err != nil {
		return apitypes.RecoveryArchiveSummaryDTO{}, f.Err
	}
	return f.RecoverySummaries[runID], nil
}

// DownloadRecoveryArchive records the download and streams the canned bytes into w. The
// call is recorded FIRST so a test can assert a download was (or was NOT) attempted.
// RecoveryDownloadHook, when set, drives the whole download — the seam a truncated /
// interrupted / corrupt-stream test uses (write N bytes, then return an error, or write the
// wrong bytes). Otherwise RecoveryBytes[captureID] is streamed verbatim.
func (f *FakeClient) DownloadRecoveryArchive(_ context.Context, runID, captureID string, w io.Writer) (int64, error) {
	f.RecoveryDownloadCalls = append(f.RecoveryDownloadCalls, captureID)
	if f.RecoveryDownloadHook != nil {
		return f.RecoveryDownloadHook(runID, captureID, w)
	}
	if f.RecoveryDownloadErr != nil {
		return 0, f.RecoveryDownloadErr
	}
	if f.Err != nil {
		return 0, f.Err
	}
	n, err := w.Write(f.RecoveryBytes[captureID])
	return int64(n), err
}
