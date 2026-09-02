package uzicli

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// fake_review.go holds the FakeClient review + findings methods (uzi review /
// uzi findings) split out of fake.go (PRD #1017).

func (f *FakeClient) SetDisposition(_ context.Context, runID, recID, status, reason string) error {
	f.LastDispositionRunID = runID
	f.LastDispositionRecID = recID
	f.LastDispositionStatus = status
	f.LastDispositionReason = reason
	if f.SetDispositionErr != nil {
		return f.SetDispositionErr
	}
	return f.Err
}

func (f *FakeClient) DeleteDisposition(_ context.Context, runID, recID string) error {
	f.LastDispositionRunID = runID
	f.LastDispositionRecID = recID
	if f.DeleteDispositionErr != nil {
		return f.DeleteDispositionErr
	}
	return f.Err
}

func (f *FakeClient) JudgeStats(context.Context) (apitypes.TriageDTO, error) {
	if f.Err != nil {
		return apitypes.TriageDTO{}, f.Err
	}
	return f.JudgeStatsResult, nil
}

func (f *FakeClient) JudgeBacklog(_ context.Context, bucket, runAnchor, category string) (apitypes.JudgeBacklogDTO, error) {
	f.LastBacklogBucket = bucket
	f.LastBacklogRun = runAnchor
	f.LastBacklogCategory = category
	if f.Err != nil {
		return apitypes.JudgeBacklogDTO{}, f.Err
	}
	return f.JudgeBacklogResult, nil
}

// BulkSetDispositions records the fan-out and returns the canned result. It captures
// BEFORE the error branch — mirroring SetDisposition — so a test asserting a refusal can
// still prove the write was REACHED, rather than passing on any earlier failure.
func (f *FakeClient) BulkSetDispositions(_ context.Context, items []apitypes.JudgeDispositionCoordDTO, status, reason string) (apitypes.JudgeDispositionResultDTO, error) {
	f.LastBulkItems = items
	f.LastBulkStatus = status
	f.LastBulkReason = reason
	if f.BulkDispositionErr != nil {
		return apitypes.JudgeDispositionResultDTO{}, f.BulkDispositionErr
	}
	if f.Err != nil {
		return apitypes.JudgeDispositionResultDTO{}, f.Err
	}
	return f.BulkDispositionResult, nil
}

func (f *FakeClient) ListFindings(_ context.Context, bucket, repo, run string) (apitypes.IncidentalFindingBacklogDTO, error) {
	f.LastFindingsBucket = bucket
	f.LastFindingsRepo = repo
	f.LastFindingsRun = run
	if f.Err != nil {
		return apitypes.IncidentalFindingBacklogDTO{}, f.Err
	}
	return f.FindingsResult, nil
}

// FileFinding records the id it was asked to file and returns the canned result. FileFindingErr
// wins over the blanket Err so a test can model a 409/404 on the WRITE precisely, while the id
// capture still proves the write was reached.
func (f *FakeClient) FileFinding(_ context.Context, id string) (apitypes.IncidentalFindingFileResultDTO, error) {
	f.LastFileFindingID = id
	if f.FileFindingErr != nil {
		return apitypes.IncidentalFindingFileResultDTO{}, f.FileFindingErr
	}
	if f.Err != nil {
		return apitypes.IncidentalFindingFileResultDTO{}, f.Err
	}
	return f.FileFindingResult, nil
}

// DismissFinding records the id + reason it was called with. DismissFindingErr wins over Err so
// a test can model a 404/409 on the write while the capture still records what reached it.
func (f *FakeClient) DismissFinding(_ context.Context, id, reason string) error {
	f.LastDismissFindingID = id
	f.LastDismissFindingReason = reason
	if f.DismissFindingErr != nil {
		return f.DismissFindingErr
	}
	return f.Err
}

// GetReviewIssueDraft records the (run, rec) it was asked about and returns the canned draft.
// GetReviewIssueDraftErr wins over the blanket Err so a test can model a 404 on the read while
// the capture still proves the read was reached.
func (f *FakeClient) GetReviewIssueDraft(_ context.Context, runID, recID string) (apitypes.IssueDraftDTO, error) {
	f.LastReviewDraftRunID = runID
	f.LastReviewDraftRecID = recID
	if f.GetReviewIssueDraftErr != nil {
		return apitypes.IssueDraftDTO{}, f.GetReviewIssueDraftErr
	}
	if f.Err != nil {
		return apitypes.IssueDraftDTO{}, f.Err
	}
	return f.ReviewIssueDraft, nil
}

// FileReviewIssue records the (run, rec, repo, title, description) it was called with and returns
// the canned result. FileReviewIssueErr wins over Err so a test can model a 409/404 on the WRITE
// precisely, while the captures still prove the write was reached with the right arguments.
func (f *FakeClient) FileReviewIssue(_ context.Context, runID, recID, repoID, title, description string) (ReviewIssueFileResult, error) {
	f.LastFileReviewRunID = runID
	f.LastFileReviewRecID = recID
	f.LastFileReviewRepoID = repoID
	f.LastFileReviewTitle = title
	f.LastFileReviewDesc = description
	if f.FileReviewIssueErr != nil {
		return ReviewIssueFileResult{}, f.FileReviewIssueErr
	}
	if f.Err != nil {
		return ReviewIssueFileResult{}, f.Err
	}
	return f.ReviewFileResult, nil
}
