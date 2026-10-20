package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

var errCoverageUnavailable = errors.New("semantic coverage unavailable")

// This ledger has at most three reviews, each with at most two invocations.
// Reservations are pessimistic charges, including after a lost response/reclaim.
// Only a first, known, current settlement can reduce a charge.
type semanticCoverageLedger struct {
	Version int                      `json:"version"`
	Started time.Time                `json:"started"`
	Reviews []semanticCoverageReview `json:"reviews"`
}
type semanticCoverageReview struct {
	ID          string                           `json:"id"`
	Binding     apitypes.SemanticCoverageBinding `json:"binding"`
	Started     time.Time                        `json:"started"`
	Deadline    time.Time                        `json:"deadline"`
	Invocations []semanticCoverageInvocation     `json:"invocations"`
	Report      *apitypes.SemanticCoverageReport `json:"report,omitempty"`
}
type semanticCoverageInvocation struct {
	ID         string                                  `json:"id"`
	RequestID  string                                  `json:"request_id"`
	Generation int64                                   `json:"generation"`
	WorkerID   uuid.UUID                               `json:"worker_id"`
	Reserved   int                                     `json:"reserved"`
	Charged    int                                     `json:"charged"`
	Status     string                                  `json:"status"`
	Settlement *apitypes.SemanticCoverageSettleRequest `json:"settlement,omitempty"`
}
type semanticCoverageTx struct {
	run       store.Run
	worker    uuid.UUID
	q         *store.Queries
	candidate apitypes.SemanticCandidate
	policy    apitypes.SemanticPolicy
	ledger    semanticCoverageLedger
	now       time.Time
}

// SemanticCoverageBinding derives server-owned source and criteria identities.
func SemanticCoverageBinding(c apitypes.SemanticCandidate) apitypes.SemanticCoverageBinding {
	sources, _ := json.Marshal(c.Sources)
	criteria, _ := json.Marshal(c.Criteria)
	return apitypes.SemanticCoverageBinding{CandidateDigest: c.Digest, SourceDigest: semanticHash(sources), CriteriaDigest: semanticHash(criteria)}
}
func validCoveragePolicy(p apitypes.SemanticPolicy) bool {
	return p.Version == 1 && p.Model == "sonnet" && p.MaxReviews > 0 && p.MaxReviews <= 3 &&
		p.MaxInvocations > 0 && p.MaxInvocations <= 2 && p.MaxTokens > 0 && p.MaxTokens <= 120000 &&
		p.MaxSeconds > 0 && p.MaxSeconds <= 900
}

// withSemanticCoverage owns the run lock through accounting persistence and commit.
// No callback performs model calls. A caller may call the model only after a new
// "reserved" response; "replay" never grants another invocation.
func (s *Service) withSemanticCoverage(ctx context.Context, workerID, runID uuid.UUID, generation int64, f func(*semanticCoverageTx) error) error {
	if s.txBeginner == nil {
		return fmt.Errorf("semantic coverage requires transaction")
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	run, err := q.GetRunByIDForUpdate(ctx, runID)
	if err != nil {
		return err
	}
	if !run.WorkerID.Valid || run.WorkerID.Bytes != workerID {
		return ErrRunNotOwned
	}
	if run.ClaimGeneration != generation || run.ClaimReleasedAt.Valid {
		return ErrStaleClaim
	}
	if terminalStatuses[run.Status] {
		return ErrRunTerminal
	}
	if run.Status != "running" && run.Status != "awaiting_approval" {
		return ErrInvalidState
	}
	if !run.CompletionContractVersion.Valid || run.CompletionContractVersion.Int32 != 1 {
		return errCoverageUnavailable
	}
	planning, err := q.GetSemanticPlanning(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errCoverageUnavailable
	}
	if err != nil {
		return err
	}
	var enrollment apitypes.SemanticEnrollment
	if err = strictSemanticJSON(planning.Enrollment, &enrollment); err != nil {
		return err
	}
	if enrollment.Profile != "semantic" || !validCoveragePolicy(enrollment.Policy) {
		return errCoverageUnavailable
	}
	c := &semanticCoverageTx{run: run, worker: workerID, q: q, policy: enrollment.Policy, now: time.Now().UTC(), ledger: semanticCoverageLedger{Version: 1}}
	if s.now != nil {
		c.now = s.now().UTC()
	}
	if err = strictSemanticJSON(planning.Candidate, &c.candidate); err != nil {
		return err
	}
	if c.candidate.Revision != planning.Revision || c.candidate.CandidateIdentityVersion != semanticIdentityVersion || c.candidate.Digest == "" {
		return errCoverageUnavailable
	}
	if len(planning.ReviewState) > 0 {
		if err = strictSemanticJSON(planning.ReviewState, &c.ledger); err != nil {
			return err
		}
	}
	if c.ledger.Version != 1 || len(c.ledger.Reviews) > 3 || (len(c.ledger.Reviews) > 0 && c.ledger.Started.IsZero()) {
		return fmt.Errorf("invalid coverage ledger")
	}
	for _, r := range c.ledger.Reviews {
		if len(r.Invocations) < 1 || len(r.Invocations) > 2 || uuid.Validate(r.ID) != nil || r.Started.IsZero() || !r.Deadline.After(r.Started) || r.Deadline.After(r.Started.Add(5*time.Minute)) {
			return fmt.Errorf("invalid coverage ledger")
		}
		total := 0
		for _, i := range r.Invocations {
			if uuid.Validate(i.ID) != nil || uuid.Validate(i.RequestID) != nil || i.Reserved <= 0 || i.Reserved > 40000 || i.Charged < 0 || i.Charged > i.Reserved {
				return fmt.Errorf("invalid coverage ledger")
			}
			total += i.Charged
		}
		if total > 40000 {
			return fmt.Errorf("invalid coverage ledger")
		}
	}
	before, err := json.Marshal(c.ledger)
	if err != nil {
		return err
	}
	if err = f(c); err != nil {
		return err
	}
	raw, err := json.Marshal(c.ledger)
	if err != nil {
		return err
	}
	if !bytes.Equal(before, raw) {
		if err = q.UpdateSemanticReviewState(ctx, store.UpdateSemanticReviewStateParams{RunID: runID, ReviewState: raw}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// SemanticCandidatePayload loads persisted inputs without reserving a model call.
// The expected digest fences the gap after SubmitSemanticCandidate commits; a
// replacement candidate is refused, never silently substituted.
func (s *Service) SemanticCandidatePayload(ctx context.Context, workerID, runID uuid.UUID, generation int64, expectedDigest string) (apitypes.SemanticCandidateResponse, error) {
	var out apitypes.SemanticCandidateResponse
	err := s.withSemanticCoverage(ctx, workerID, runID, generation, func(c *semanticCoverageTx) error {
		if expectedDigest == "" || c.candidate.Digest != expectedDigest {
			return fmt.Errorf("stale semantic candidate")
		}
		payload, err := c.payload(ctx, runID)
		if err != nil {
			return err
		}
		out = payload
		return nil
	})
	if err != nil {
		return apitypes.SemanticCandidateResponse{}, err
	}
	return out, nil
}

// payload reads at most nine source blobs and one plan. Any missing or corrupt
// blob aborts the transaction; unknown completeness and empty bytes are preserved.
func (c *semanticCoverageTx) payload(ctx context.Context, runID uuid.UUID) (apitypes.SemanticCandidateResponse, error) {
	out := apitypes.SemanticCandidateResponse{Candidate: c.candidate, Binding: SemanticCoverageBinding(c.candidate), Sources: make([]apitypes.SemanticSourceCapture, 0, len(c.candidate.Sources))}
	for _, ref := range c.candidate.Sources {
		blob, err := c.q.GetSemanticSnapshot(ctx, store.GetSemanticSnapshotParams{RunID: runID, Hash: ref.Hash})
		if err != nil {
			return out, err
		}
		if blob == nil || semanticHash(blob) != ref.Hash {
			return out, fmt.Errorf("corrupt semantic snapshot")
		}
		out.Sources = append(out.Sources, apitypes.SemanticSourceCapture{ID: ref.ID, Kind: ref.Kind, Provenance: ref.Provenance, Completeness: ref.Completeness, Bytes: blob})
	}
	plan, err := c.q.GetSemanticSnapshot(ctx, store.GetSemanticSnapshotParams{RunID: runID, Hash: c.candidate.PlanHash})
	if err != nil {
		return out, err
	}
	if plan == nil || semanticHash(plan) != c.candidate.PlanHash {
		return out, fmt.Errorf("corrupt semantic plan")
	}
	out.Plan = plan
	return out, nil
}

func (c *semanticCoverageTx) charged() int {
	total := 0
	for _, r := range c.ledger.Reviews {
		for _, i := range r.Invocations {
			total += i.Charged
		}
	}
	return total
}
func (c *semanticCoverageTx) review(id string) *semanticCoverageReview {
	for i := range c.ledger.Reviews {
		if c.ledger.Reviews[i].ID == id {
			return &c.ledger.Reviews[i]
		}
	}
	return nil
}
func (r *semanticCoverageReview) invocation(id string) *semanticCoverageInvocation {
	for i := range r.Invocations {
		if r.Invocations[i].ID == id {
			return &r.Invocations[i]
		}
	}
	return nil
}

// ReserveSemanticCoverage strictly decodes the request, loads bytea snapshots and
// commits the invocation identity and full token reservation before returning.
func (s *Service) ReserveSemanticCoverage(ctx context.Context, workerID, runID uuid.UUID, generation int64, raw []byte) (apitypes.SemanticCoverageReservation, error) {
	out := apitypes.SemanticCoverageReservation{Outcome: "unavailable"}
	var req apitypes.SemanticCoverageReserveRequest
	if len(raw) > 4096 {
		return out, fmt.Errorf("coverage request too large")
	}
	if err := strictSemanticJSON(raw, &req); err != nil {
		return out, err
	}
	if uuid.Validate(req.RequestID) != nil || (req.ReviewID != "" && uuid.Validate(req.ReviewID) != nil) {
		return out, fmt.Errorf("invalid coverage request identity")
	}
	err := s.withSemanticCoverage(ctx, workerID, runID, generation, func(c *semanticCoverageTx) error {
		out.Reviews = len(c.ledger.Reviews)
		out.ChargedTokens = c.charged()
		if req.SemanticCoverageBinding != SemanticCoverageBinding(c.candidate) {
			return fmt.Errorf("stale coverage binding")
		}
		if !req.Supported {
			return nil
		}
		return c.reserve(ctx, runID, workerID, generation, req, &out)
	})
	if errors.Is(err, errCoverageUnavailable) {
		return out, nil
	}
	return out, err
}
func (c *semanticCoverageTx) reserve(ctx context.Context, runID, workerID uuid.UUID, generation int64, req apitypes.SemanticCoverageReserveRequest, out *apitypes.SemanticCoverageReservation) error {
	// At most six entries are scanned. A request ID is single use across the run.
	for _, r := range c.ledger.Reviews {
		for _, i := range r.Invocations {
			if i.RequestID == req.RequestID {
				if r.Binding != req.SemanticCoverageBinding || (req.ReviewID != "" && req.ReviewID != r.ID) {
					return fmt.Errorf("coverage request identity conflict")
				}
				out.Outcome = "replay"
				out.ReviewID = r.ID
				out.InvocationID = i.ID
				out.SemanticCoverageBinding = r.Binding
				out.Deadline = r.Deadline
				out.MaxTokens = i.Reserved
				out.Invocations = len(r.Invocations)
				return nil
			}
		}
	}
	if err := requireSemanticMutation(ctx, c.q, c.run, c.worker); err != nil {
		return err
	}
	r := c.review(req.ReviewID)
	if req.ReviewID != "" && r == nil {
		return fmt.Errorf("unknown coverage review")
	}
	if r == nil {
		if len(c.ledger.Reviews) >= c.policy.MaxReviews {
			return nil
		}
	} else {
		if r.Binding != req.SemanticCoverageBinding {
			return fmt.Errorf("stale coverage review")
		}
		if r.Report != nil || len(r.Invocations) >= c.policy.MaxInvocations || !c.now.Before(r.Deadline) {
			return nil
		}
		last := r.Invocations[len(r.Invocations)-1]
		if last.Status != "transport_error" && last.Status != "schema_error" {
			return nil
		}
	}
	reviewTokens := 0
	if r != nil {
		for _, i := range r.Invocations {
			reviewTokens += i.Charged
		}
	}
	reserve := min(40000-reviewTokens, c.policy.MaxTokens-c.charged())
	if reserve <= 0 {
		return nil
	}
	// Verify all persisted payloads before charging; failed reads roll back.
	payload, err := c.payload(ctx, runID)
	if err != nil {
		return err
	}
	if c.ledger.Started.IsZero() {
		c.ledger.Started = c.now
	}
	if r == nil {
		// Review allowances, rather than human waiting, consume the run time budget.
		remaining := time.Duration(c.policy.MaxSeconds) * time.Second
		for _, prior := range c.ledger.Reviews {
			remaining -= prior.Deadline.Sub(prior.Started)
		}
		if remaining <= 0 {
			return nil
		}
		deadline := minTime(c.now.Add(5*time.Minute), c.now.Add(remaining))
		c.ledger.Reviews = append(c.ledger.Reviews, semanticCoverageReview{ID: uuid.NewString(), Binding: req.SemanticCoverageBinding, Started: c.now, Deadline: deadline})
		r = &c.ledger.Reviews[len(c.ledger.Reviews)-1]
	}
	invocation := semanticCoverageInvocation{ID: uuid.NewString(), RequestID: req.RequestID, WorkerID: workerID, Generation: generation, Reserved: reserve, Charged: reserve, Status: "pending"}
	r.Invocations = append(r.Invocations, invocation)
	*out = apitypes.SemanticCoverageReservation{Outcome: "reserved", ReviewID: r.ID, InvocationID: invocation.ID, SemanticCoverageBinding: r.Binding, Model: c.policy.Model, Deadline: r.Deadline, MaxTokens: reserve, Reviews: len(c.ledger.Reviews), Invocations: len(r.Invocations), ChargedTokens: c.charged(), Candidate: &c.candidate, Plan: payload.Plan, Sources: payload.Sources}
	return nil
}

// SemanticCoverageStatus returns current-candidate evidence and run-wide charges.
// Historical reports never authorize a replacement candidate.
func (s *Service) SemanticCoverageStatus(ctx context.Context, workerID, runID uuid.UUID, generation int64) (apitypes.SemanticCoverageStatus, error) {
	out := apitypes.SemanticCoverageStatus{Outcome: "unavailable"}
	err := s.withSemanticCoverage(ctx, workerID, runID, generation, func(c *semanticCoverageTx) error {
		out = evaluateSemanticCoverage(c, c.run).Status
		// A status deadline describes the latest review; it is never a run wall clock.
		out.Deadline = time.Time{}
		if len(c.ledger.Reviews) > 0 {
			out.Deadline = c.ledger.Reviews[len(c.ledger.Reviews)-1].Deadline
		}
		// The worker wire enumerates unavailable, complete, incomplete and uncertain; an exhausted
		// budget is the same unavailable class there (the gate presentation keeps the finer value).
		if out.Outcome == "exhausted" {
			out.Outcome = "unavailable"
		}
		return nil
	})
	if errors.Is(err, errCoverageUnavailable) {
		return out, nil
	}
	return out, err
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// SettleSemanticCoverage accounts usage independently of parsing/publishing a
// report. Lost/unknown, expired, superseded and reclaimed work stays fully charged.
func (s *Service) SettleSemanticCoverage(ctx context.Context, workerID, runID uuid.UUID, generation int64, raw []byte) error {
	var req apitypes.SemanticCoverageSettleRequest
	if len(raw) > 4096 {
		return fmt.Errorf("coverage settlement too large")
	}
	if err := strictSemanticJSON(raw, &req); err != nil {
		return err
	}
	switch req.Status {
	case "success", "transport_error", "schema_error", "unknown":
	default:
		return fmt.Errorf("invalid coverage settlement")
	}
	if uuid.Validate(req.ReviewID) != nil || uuid.Validate(req.InvocationID) != nil || (req.Tokens != nil && *req.Tokens < 0) {
		return fmt.Errorf("invalid coverage settlement")
	}
	return s.withSemanticCoverage(ctx, workerID, runID, generation, func(c *semanticCoverageTx) error {
		r := c.review(req.ReviewID)
		if r == nil {
			return fmt.Errorf("unknown coverage review")
		}
		i := r.invocation(req.InvocationID)
		if i == nil {
			return fmt.Errorf("unknown coverage invocation")
		}
		if i.Settlement != nil {
			a, _ := json.Marshal(i.Settlement)
			b, _ := json.Marshal(req)
			if string(a) != string(b) {
				return fmt.Errorf("coverage settlement conflict")
			}
			return nil
		}
		if err := requireSemanticMutation(ctx, c.q, c.run, c.worker); err != nil {
			return err
		}
		if req.Tokens != nil && *req.Tokens > i.Reserved {
			return fmt.Errorf("coverage usage exceeds reservation")
		}
		if req.Status != "unknown" && req.Tokens != nil && r.Binding == SemanticCoverageBinding(c.candidate) &&
			i.WorkerID == workerID && i.Generation == generation && c.now.Before(r.Deadline) {
			i.Charged = *req.Tokens
		}
		i.Status = req.Status
		i.Settlement = &req
		return nil
	})
}

// PublishSemanticCoverage does not settle usage or grant approval authority.
func (s *Service) PublishSemanticCoverage(ctx context.Context, workerID, runID uuid.UUID, generation int64, raw []byte) error {
	var req apitypes.SemanticCoveragePublishRequest
	if len(raw) > 64*1024 {
		return fmt.Errorf("coverage report too large")
	}
	if err := strictSemanticJSON(raw, &req); err != nil {
		return err
	}
	return s.withSemanticCoverage(ctx, workerID, runID, generation, func(c *semanticCoverageTx) error {
		r := c.review(req.ReviewID)
		if r == nil {
			return fmt.Errorf("unknown coverage review")
		}
		i := r.invocation(req.InvocationID)
		if i == nil || i.Status != "success" || i.WorkerID != workerID || i.Generation != generation ||
			r.Binding != SemanticCoverageBinding(c.candidate) || req.Report.SemanticCoverageBinding != r.Binding {
			return fmt.Errorf("stale coverage report")
		}
		sources := make([]apitypes.SemanticSourceCapture, 0, len(c.candidate.Sources))
		for _, ref := range c.candidate.Sources {
			blob, err := c.q.GetSemanticSnapshot(ctx, store.GetSemanticSnapshotParams{RunID: runID, Hash: ref.Hash})
			if err != nil {
				return err
			}
			sources = append(sources, apitypes.SemanticSourceCapture{ID: ref.ID, Bytes: blob})
		}
		reportRaw, err := json.Marshal(req.Report)
		if err != nil {
			return err
		}
		if _, err = DecodeSemanticCoverageReport(reportRaw, c.candidate, sources); err != nil {
			return err
		}
		if r.Report != nil {
			a, _ := json.Marshal(r.Report)
			b, _ := json.Marshal(req.Report)
			if string(a) != string(b) {
				return fmt.Errorf("coverage report conflict")
			}
			return nil
		}
		if err := requireSemanticMutation(ctx, c.q, c.run, c.worker); err != nil {
			return err
		}
		if !c.now.Before(r.Deadline) {
			return fmt.Errorf("coverage deadline expired")
		}
		r.Report = &req.Report
		return nil
	})
}
func validateSemanticCoverageReport(report apitypes.SemanticCoverageReport, candidate apitypes.SemanticCandidate) error {
	if report.SemanticCoverageBinding != SemanticCoverageBinding(candidate) || len(report.Requirements) < 1 || len(report.Requirements) > 512 {
		return fmt.Errorf("invalid coverage report")
	}
	refs := map[string]apitypes.SemanticSourceRef{}
	for _, r := range candidate.Sources {
		refs[r.ID] = r
	}
	ids := map[string]bool{}
	for _, c := range candidate.Criteria {
		ids[c.ID] = true
	}
	if len(report.Criteria) != len(candidate.Criteria) {
		return fmt.Errorf("omitted coverage atomicity")
	}
	assessed := map[string]bool{}
	for _, assessment := range report.Criteria {
		if !ids[assessment.CriterionID] || assessed[assessment.CriterionID] || !semanticText(assessment.Explanation, 2048) {
			return fmt.Errorf("invalid coverage atomicity")
		}
		assessed[assessment.CriterionID] = true
	}
	requirements := map[string]bool{}
	cited := map[string]bool{}
	for _, result := range report.Requirements {
		if len(result.RequirementID) < 2 || len(result.RequirementID) > 7 || result.RequirementID[0] != 'r' {
			return fmt.Errorf("invalid coverage requirement identity")
		}
		n, err := strconv.Atoi(result.RequirementID[1:])
		if err != nil || n <= 0 || strconv.Itoa(n) != result.RequirementID[1:] || requirements[result.RequirementID] {
			return fmt.Errorf("invalid coverage requirement identity")
		}
		requirements[result.RequirementID] = true
		switch result.Status {
		case "represented", "missing", "uncertain":
		default:
			return fmt.Errorf("invalid coverage outcome")
		}
		if !semanticText(result.Explanation, 2048) || len(result.Citations) < 1 || len(result.Citations) > 8 || len(result.CriterionIDs) > 8 {
			return fmt.Errorf("invalid coverage result")
		}
		if result.Status == "represented" && len(result.CriterionIDs) == 0 {
			return fmt.Errorf("represented coverage requires criterion")
		}
		seen := map[string]bool{}
		for _, id := range result.CriterionIDs {
			if !ids[id] || seen[id] {
				return fmt.Errorf("invalid coverage criterion reference")
			}
			seen[id] = true
		}
		for _, cite := range result.Citations {
			cited[cite.SourceID] = true
			ref, ok := refs[cite.SourceID]
			if !ok || ref.Hash != cite.Hash || ref.Completeness != "complete" || cite.Start < 0 || cite.End <= cite.Start {
				return fmt.Errorf("invalid coverage citation")
			}
		}
	}
	for _, ref := range candidate.Sources {
		if ref.Completeness == "complete" && !cited[ref.ID] {
			return fmt.Errorf("omitted coverage source")
		}
	}
	return nil
}

func validSemanticCoverageCitationBytes(cite apitypes.SemanticCitation, blob []byte) bool {
	return cite.Start >= 0 && cite.End > cite.Start && cite.End <= len(blob) && utf8.Valid(blob[:cite.Start]) && utf8.Valid(blob[:cite.End])
}

// DecodeSemanticCoverageReport is the strict bounded report parser shared with
// trusted server callers. Sources must be loaded from persisted snapshots.
func DecodeSemanticCoverageReport(raw []byte, candidate apitypes.SemanticCandidate, sources []apitypes.SemanticSourceCapture) (apitypes.SemanticCoverageReport, error) {
	var report apitypes.SemanticCoverageReport
	if len(raw) > 64*1024 {
		return report, fmt.Errorf("coverage report too large")
	}
	if err := strictSemanticJSON(raw, &report); err != nil {
		return report, err
	}
	if err := validateSemanticCoverageReport(report, candidate); err != nil {
		return report, err
	}
	blobs := map[string][]byte{}
	for _, source := range sources {
		if _, ok := blobs[source.ID]; ok {
			return report, fmt.Errorf("duplicate coverage source")
		}
		blobs[source.ID] = source.Bytes
	}
	for _, result := range report.Requirements {
		for _, cite := range result.Citations {
			blob, ok := blobs[cite.SourceID]
			if !ok || semanticHash(blob) != cite.Hash || !validSemanticCoverageCitationBytes(cite, blob) {
				return report, fmt.Errorf("invalid coverage citation range")
			}
		}
	}
	return report, nil
}
