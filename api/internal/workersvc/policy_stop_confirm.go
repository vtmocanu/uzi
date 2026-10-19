package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// policyStopConfirmation carries the accepted owner receipt to the existing input route.
type policyStopConfirmation struct {
	ScopeCeiling    int
	ResolutionID    uuid.UUID
	ScopeInputID    int64
	AlreadyAccepted bool
	Resumed         bool
	Receipt         *apitypes.PolicyStopReceipt
}

type policyStopResolutionV1 struct {
	Version      int32                       `json:"version"`
	ResolutionID uuid.UUID                   `json:"resolution_id"`
	AcceptedBy   uuid.UUID                   `json:"accepted_by"`
	AcceptedAt   time.Time                   `json:"accepted_at"`
	ScopeInputID int64                       `json:"scope_input_id"`
	IfMatch      string                      `json:"if_match"`
	Manifest     apitypes.PolicyStopManifest `json:"manifest"`
}

var policyStopConfirmationToken = regexp.MustCompile(`^"policy-stop-v1-[0-9a-f]{64}"$`)

// confirmPolicyStop owns the transaction; callers notify only after it succeeds.
// Production activation remains gated on the worker and owner-surface contract.
func (s *Service) confirmPolicyStop(ctx context.Context, owner, runID uuid.UUID, presentedToken, reason string) (policyStopConfirmation, error) {
	var out policyStopConfirmation
	if s.txBeginner == nil {
		return out, stopReviewConflict("policy_stop_review_unverifiable")
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	// A separate statement gives the child inventory a fresh READ COMMITTED snapshot.
	r, err := q.GetRunByIDForUpdate(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrRunNotOwned
	}
	if err != nil {
		return out, err
	}
	if r.UserID != owner {
		return out, ErrRunNotOwned
	}
	if presentedToken == "" {
		return out, stopReviewConflict("policy_stop_token_required")
	}
	if !policyStopConfirmationToken.MatchString(presentedToken) {
		return out, stopReviewConflict("policy_stop_token_stale")
	}
	// Accepted retries never consult mutable eligibility, inventory or claim identity.
	if len(r.PolicyStopResolution) != 0 {
		record, err := parsePolicyStopResolution(r.PolicyStopResolution, owner, runID)
		if err != nil {
			return out, err
		}
		if record.IfMatch != presentedToken {
			return out, stopReviewConflict("policy_stop_token_stale")
		}
		return policyStopConfirmation{
			ScopeCeiling: int(record.Manifest.Execution.ProposedCeiling), ResolutionID: record.ResolutionID,
			ScopeInputID: record.ScopeInputID, AlreadyAccepted: true, Receipt: policyStopReceipt(record, true),
		}, nil
	}
	if !s.p.PolicyStopNegotiated || !s.p.PolicyHoldNegotiated || !s.p.PolicyHoldSnapshotEnabled ||
		s.p.TerminalPendingLease <= 0 || !policyEligible(r) || !PolicyOwnerCanStop(r, s.p.RunTimeout) {
		return out, stopReviewConflict("policy_stop_review_unverifiable")
	}
	manifest, err := policyStopManifestLocked(ctx, q, r)
	if err != nil {
		return out, err
	}
	token, err := policyStopManifestToken(manifest)
	if err != nil {
		return out, err
	}
	if token != presentedToken {
		return out, stopReviewConflict("policy_stop_token_stale")
	}
	completedCount := len(manifest.Execution.CompletedIDs)
	if completedCount < 1 || completedCount > math.MaxInt32 {
		return out, stopReviewConflict("policy_stop_review_unverifiable")
	}
	ceiling := int32(completedCount)
	if ceiling != manifest.Execution.ProposedCeiling {
		return out, stopReviewConflict("policy_stop_review_unverifiable")
	}
	// Validate IDs before the first write. No float conversion participates in identity.
	humanIDs := make([]int64, 0, len(manifest.HumanFeedback))
	for _, feedback := range manifest.HumanFeedback {
		if feedback.InputID.Validate() != nil {
			return out, stopReviewConflict("policy_stop_review_unverifiable")
		}
		id, err := strconv.ParseInt(string(feedback.InputID), 10, 64)
		if err != nil {
			return out, stopReviewConflict("policy_stop_review_unverifiable")
		}
		humanIDs = append(humanIDs, id)
	}
	slices.Sort(humanIDs)
	ccIDs := make([]uuid.UUID, 0, len(manifest.CrossCheckFeedback))
	for _, feedback := range manifest.CrossCheckFeedback {
		if !feedback.Supersede {
			continue
		}
		id, err := uuid.Parse(feedback.CrossCheckID)
		if err != nil || id == uuid.Nil || id.String() != feedback.CrossCheckID || feedback.Verdict != "revise" {
			return out, stopReviewConflict("policy_stop_review_unverifiable")
		}
		ccIDs = append(ccIDs, id)
	}
	slices.SortFunc(ccIDs, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	resolutionID := uuid.New()
	// Owner text is bounded, valid UTF-8 and NUL-free; no provider detail is copied.
	reason = strings.TrimSpace(strings.ReplaceAll(strings.ToValidUTF8(reason, ""), "\x00", ""))
	if len(reason) > PolicyResumeGuidanceMaxBytes {
		reason = reason[:PolicyResumeGuidanceMaxBytes]
		for !utf8.ValidString(reason) {
			reason = reason[:len(reason)-1]
		}
	}
	body := fmt.Sprintf("stop → finalize %d completed milestone(s), start no further; resolution %s", completedCount, resolutionID)
	if reason != "" {
		body += ": " + reason
	}
	scope, err := q.CreateScopeCeilingInput(ctx, store.CreateScopeCeilingInputParams{
		RunID: runID, ScopeCeiling: pgtype.Int4{Int32: ceiling, Valid: true}, Body: policyTextNonempty(body),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return out, stopReviewConflict("policy_stop_token_stale")
	}
	if err != nil {
		return out, err
	}
	if scope.ID <= 0 || !scope.CreatedAt.Valid {
		return out, stopReviewConflict("policy_stop_review_unverifiable")
	}
	// Each finite manifest ID is attempted once under ctx, in deterministic order.
	// Any failure stops sibling work and rolls back the entire transaction.
	for _, id := range humanIDs {
		n, err := q.SettlePolicyStopRevisionInput(ctx, store.SettlePolicyStopRevisionInputParams{
			RunID: runID, InputID: id, ScopeInputID: scope.ID,
		})
		if err != nil {
			return out, err
		}
		if n != 1 {
			return out, stopReviewConflict("policy_stop_token_stale")
		}
	}
	for _, id := range ccIDs {
		n, err := q.SettlePolicyStopCrossCheck(ctx, store.SettlePolicyStopCrossCheckParams{
			RunID: runID, CrossCheckID: id, ScopeInputID: scope.ID,
		})
		if err != nil {
			return out, err
		}
		if n != 1 {
			return out, stopReviewConflict("policy_stop_token_stale")
		}
	}
	record := policyStopResolutionV1{
		Version: 1, ResolutionID: resolutionID, AcceptedBy: owner, AcceptedAt: scope.CreatedAt.Time,
		ScopeInputID: scope.ID, IfMatch: token, Manifest: manifest,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return out, err
	}
	holdToken := PolicyHoldActionToken(r)
	r, err = q.AcceptPolicyStopResolution(ctx, store.AcceptPolicyStopResolutionParams{
		Resolution: raw, RunID: runID, OwnerID: owner, OutcomeID: r.PolicyLastOutcomeID,
		SourceGeneration: r.PolicyLastSourceGeneration.Int64, ClaimGeneration: r.ClaimGeneration,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return out, stopReviewConflict("policy_stop_token_stale")
	}
	if err != nil {
		return out, err
	}
	// The returned row contains the new scope and resolution. Preserve reuses the
	// ordinary banking and audit path without creating a second scope.
	if _, err := s.resumePolicyHoldLocked(ctx, q, r, holdToken, PolicyResumeRequest{Mode: "preserve"}); err != nil {
		return out, err
	}
	if err := tx.Commit(ctx); err != nil {
		return out, err
	}
	return policyStopConfirmation{
		ScopeCeiling: completedCount, ResolutionID: resolutionID, ScopeInputID: scope.ID, Resumed: true, Receipt: policyStopReceipt(record, false),
	}, nil
}

func policyStopReceipt(record policyStopResolutionV1, repeated bool) *apitypes.PolicyStopReceipt {
	disposition := "accepted"
	if repeated {
		disposition = "already_accepted"
	}
	refs := make([]apitypes.PolicyRevisionRef, 0, len(record.Manifest.HumanFeedback)+len(record.Manifest.CrossCheckFeedback))
	for _, human := range record.Manifest.HumanFeedback {
		id := human.InputID
		refs = append(refs, apitypes.PolicyRevisionRef{Source: "run_user_input", InputID: &id})
	}
	for _, cc := range record.Manifest.CrossCheckFeedback {
		if cc.Supersede {
			id := cc.CrossCheckID
			refs = append(refs, apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &id})
		}
	}
	return &apitypes.PolicyStopReceipt{
		Disposition: disposition, ResolutionID: record.ResolutionID.String(),
		ScopeInputID: stopDecimal(record.ScopeInputID), ScopeCeiling: int(record.Manifest.Execution.ProposedCeiling),
		AcceptedAt: record.AcceptedAt, ReviewGeneration: record.Manifest.ReviewGeneration, SupersededRevisions: refs,
	}
}

// parsePolicyStopResolution authenticates only the immutable acceptance receipt.
// Execution, settlement and current-claim authority require separate validation.
func parsePolicyStopResolution(raw []byte, owner, runID uuid.UUID) (policyStopResolutionV1, error) {
	var record policyStopResolutionV1
	bad := func() (policyStopResolutionV1, error) {
		return policyStopResolutionV1{}, stopReviewConflict("policy_stop_review_unverifiable")
	}
	if err := policyStopUniqueJSON(raw); err != nil {
		return bad()
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&record); err != nil {
		return bad()
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 7 {
		return bad()
	}
	for _, key := range []string{"version", "resolution_id", "accepted_by", "accepted_at", "scope_input_id", "if_match", "manifest"} {
		if _, ok := fields[key]; !ok {
			return bad()
		}
	}
	var manifestFields map[string]json.RawMessage
	if err := json.Unmarshal(fields["manifest"], &manifestFields); err != nil {
		return bad()
	}
	manifestKeys := []string{"candidate_cross_check_id", "version", "run_id", "outcome_id", "source_generation", "review_generation", "source_context", "hold", "gate", "execution", "human_feedback", "cross_check_feedback"}
	if len(manifestFields) != len(manifestKeys) {
		return bad()
	}
	for _, key := range manifestKeys {
		if _, present := manifestFields[key]; !present {
			return bad()
		}
	}
	selection, present := manifestFields["candidate_cross_check_id"]
	var selected *string
	if !present || json.Unmarshal(selection, &selected) != nil {
		return bad()
	}
	// Reparse the original bytes: decoding a struct loses required-field presence.
	source, err := apitypes.ParsePolicyRevisionSourceV1(manifestFields["source_context"])
	if err != nil || !policyStopSelectionValid(source, selected) {
		return bad()
	}
	m := record.Manifest
	if (m.Gate == nil) != (source.Gate == nil) {
		return bad()
	}
	if m.Gate != nil && m.Gate.PolicyStopGateContext != *source.Gate {
		return bad()
	}
	outcome, err := uuid.Parse(m.OutcomeID)
	if record.Version != 1 || record.ResolutionID == uuid.Nil || record.AcceptedBy != owner || owner == uuid.Nil ||
		record.AcceptedAt.IsZero() || record.ScopeInputID <= 0 || m.Version != 1 || m.RunID != runID.String() ||
		err != nil || outcome == uuid.Nil || outcome.String() != m.OutcomeID ||
		m.SourceGeneration.Validate() != nil || m.ReviewGeneration.Validate() != nil ||
		m.Execution.ProposedCeiling <= 0 || !policyStopConfirmationToken.MatchString(record.IfMatch) {
		return bad()
	}
	token, err := policyStopManifestToken(m)
	if err != nil || token != record.IfMatch {
		return bad()
	}
	return record, nil
}

// policyStopUniqueJSON traverses only the supplied JSON bytes, once, without I/O.
func policyStopUniqueJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func() error
	walk = func() error {
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		seen := map[string]bool{}
		for d.More() {
			if delim == '{' {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate policy stop resolution field")
				}
				seen[name] = true
			}
			if err := walk(); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing policy stop resolution JSON")
	}
	return nil
}
