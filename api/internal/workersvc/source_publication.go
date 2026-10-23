package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// SourcePublicationReport is independent of recovery final-head negotiation.
type SourcePublicationReport struct {
	Version       int    `json:"version"`
	PublishedHead string `json:"published_head"`
	NonClosing    bool   `json:"non_closing"`
}

func (p *SourcePublicationReport) UnmarshalJSON(b []byte) error {
	var wire struct {
		Version       int    `json:"version"`
		PublishedHead string `json:"published_head"`
		NonClosing    *bool  `json:"non_closing"`
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if len(b) > 1024 || d.Decode(&wire) != nil || wire.NonClosing == nil || wire.Version != 1 || !isSettleSHA(wire.PublishedHead) {
		return ErrInvalidState
	}
	if d.Decode(new(any)) != io.EOF {
		return ErrInvalidState
	}
	*p = SourcePublicationReport{wire.Version, wire.PublishedHead, *wire.NonClosing}
	return nil
}

// SourcePublicationIntent records historical generation, never the source's later claim.
type SourcePublicationIntent struct {
	Version            int    `json:"version"`
	ClaimGeneration    int64  `json:"claim_generation"`
	Branch             string `json:"branch"`
	MRIID              int64  `json:"mr_iid"`
	PublishedHead      string `json:"published_head"`
	ReportedNonClosing bool   `json:"reported_non_closing"`
	Intent             string `json:"intent"`
}

// Exact replay precedes recovery receipts and consumed-permit acknowledgements.
func (s *Service) replaySourcePublication(ctx context.Context, w store.Worker, id uuid.UUID, req StateRequest) (*StateReportResult, error) {
	if req.SourcePublication == nil {
		return nil, nil
	}
	if err := validateSourcePublicationRequest(req); err != nil {
		return nil, err
	}
	if s.txBeginner == nil {
		return nil, ErrInvalidState
	}
	run, err := s.runOwnedByWorker(ctx, id, w)
	if err != nil {
		return nil, err
	}
	if run.Kind != "issue" || run.ReportOnly {
		return nil, ErrInvalidState
	}
	if run.Status != "completed" && len(run.SourcePublicationIntent) == 0 {
		return nil, nil
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if _, err = q.GetWorkerForUpdate(ctx, w.ID); err != nil {
		return nil, err
	}
	run, err = q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: id, WorkerID: pgconv.UUID(w.ID)})
	if err != nil {
		return nil, err
	}
	if run.UserID != w.UserID || run.Status != "completed" || !sourcePublicationIdentity(run, req) {
		return nil, ErrInvalidState
	}
	// Release the read locks before existing recovery verification contacts the forge.
	if err = tx.Rollback(ctx); err != nil {
		return nil, err
	}
	if replay, err := s.replayCompletedPublication(ctx, w, id, req); replay != nil || err != nil {
		return replay, err
	}
	result := &StateReportResult{Run: run, Applied: true}
	if completedPublicationCapable(w, req) {
		result.CompletedPublicationReceipt, result.CompletedPublicationReason = s.verifyCompletedPublication(ctx, w, id, req)
	}
	return result, nil
}

func validateSourcePublicationRequest(req StateRequest) error {
	p := req.SourcePublication
	if p == nil {
		return nil
	}
	if req.State != "completed" || p.Version != 1 || !isSettleSHA(p.PublishedHead) ||
		req.ClaimGeneration == nil || !publicationSafeID(*req.ClaimGeneration) || req.Branch == nil ||
		len(*req.Branch) > 1024 || !safePublicationIdentifier(*req.Branch) || req.MrIID == nil || !publicationSafeID(*req.MrIID) ||
		(req.ReportOnly != nil && *req.ReportOnly) ||
		(req.Head != nil && *req.Head != p.PublishedHead) ||
		(req.CompletionFinalHead != nil && *req.CompletionFinalHead != p.PublishedHead) {
		return ErrInvalidState
	}
	return nil
}

func sourcePublicationIdentity(run store.Run, req StateRequest) bool {
	p := req.SourcePublication
	if p == nil {
		return false
	}
	var stamp SourcePublicationIntent
	d := json.NewDecoder(bytes.NewReader(run.SourcePublicationIntent))
	d.DisallowUnknownFields()
	return d.Decode(&stamp) == nil && stamp.Version == 1 && stamp.ClaimGeneration == *req.ClaimGeneration &&
		stamp.Branch == *req.Branch && stamp.MRIID == *req.MrIID && stamp.PublishedHead == p.PublishedHead &&
		stamp.ReportedNonClosing == p.NonClosing && (stamp.Intent == "closing" || stamp.Intent == "non_closing") &&
		(!stamp.ReportedNonClosing || stamp.Intent == "non_closing") &&
		run.Kind == "issue" && run.Branch.String == stamp.Branch && run.MrIid.Int64 == stamp.MRIID
}

// Restriction checks never interpret a failed authority decode as permission to close.
// A legacy unenrolled issue may have no contract; an enrolled issue must have one.
func sourcePublicationRestriction(run store.Run, appliedCap bool) (bool, error) {
	restricted := run.CompletionNonClosingReason.Valid || run.StopKind.String == "scope_capped" || appliedCap
	if run.CompletionContractVersion.Valid && run.CompletionContractVersion.Int32 != 1 {
		return restricted, ErrSemanticCompletionAuthority
	}
	if len(run.CompletionContract) == 0 {
		if run.CompletionContractVersion.Valid {
			return restricted, ErrSemanticCompletionAuthority
		}
		return restricted, nil
	}
	c, err := parseCompletionContract(run.CompletionContract)
	if err != nil {
		return restricted, err
	}
	if !run.CompletionContractVersion.Valid || !run.ContractRevision.Valid ||
		(c.Profile != "structural" && c.Profile != "semantic") || c.Revision < 1 || c.Criteria == nil ||
		int(run.ContractRevision.Int32) != c.Revision {
		return restricted, ErrSemanticCompletionAuthority
	}
	if c.Profile == "structural" {
		ms, err := DecodeMilestones(run.MilestonesFrozen)
		if err != nil || ms == nil {
			return restricted, ErrSemanticCompletionAuthority
		}
		if _, valid := structuralAuthority(run, ms); !valid {
			return restricted, ErrSemanticCompletionAuthority
		}
	}
	pending, err := completionPendingFromContract(run, c)
	if err != nil {
		return restricted, err
	}
	return restricted || (c.Scope != nil && len(c.Scope.Out) > 0) || (pending != nil && len(pending.Criteria) > 0), nil
}

func validateSourcePublicationLive(run store.Run, w store.Worker, req StateRequest) error {
	if req.SourcePublication == nil {
		return nil
	}
	if run.Kind != "issue" || run.ReportOnly || !run.IssueIid.Valid || run.IssueIid.Int64 <= 0 {
		return ErrInvalidState
	}
	if run.UserID != w.UserID || run.ClaimGeneration <= 0 || run.ClaimGeneration != *req.ClaimGeneration || run.ClaimReleasedAt.Valid {
		return ErrStaleClaim
	}
	if run.Status != "claimed" && run.Status != "running" {
		return ErrInvalidState
	}
	if len(run.SourcePublicationIntent) != 0 {
		return ErrInvalidState
	}
	return nil
}

func bindSourcePublication(ctx context.Context, tx pgx.Tx, w store.Worker, run store.Run, req StateRequest, appliedCap bool) error {
	if req.SourcePublication == nil {
		return nil
	}
	if tx == nil {
		return fmt.Errorf("source publication requires completion transaction")
	}
	if err := validateSourcePublicationLive(run, w, req); err != nil {
		return err
	}
	restricted, err := sourcePublicationRestriction(run, appliedCap)
	if err != nil {
		return err
	}
	intent := "closing"
	if restricted || req.SourcePublication.NonClosing {
		intent = "non_closing"
	}
	stamp := SourcePublicationIntent{1, *req.ClaimGeneration, *req.Branch, *req.MrIID, req.SourcePublication.PublishedHead, req.SourcePublication.NonClosing, intent}
	b, err := json.Marshal(stamp)
	if err != nil {
		return err
	}
	n, err := store.New(tx).BindSourcePublicationIntent(ctx, store.BindSourcePublicationIntentParams{
		ID: run.ID, UserID: w.UserID, WorkerID: pgconv.UUID(w.ID), Generation: *req.ClaimGeneration, Branch: pgconv.Text(*req.Branch), MrIid: pgconv.Int8Ptr(req.MrIID), Intent: b,
	})
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrInvalidState
	}
	return nil
}
