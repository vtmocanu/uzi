package workersvc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type policyRevisionWire struct {
	carrier apitypes.PolicyRevisionCarrier
	valid   bool
}

// Only the exact outer carrier key is authoritative. Inspect every outer key
// before sanitation, including duplicates and decoded NULs.
func decodePolicyRevisionWire(m IncomingMessage) policyRevisionWire {
	if m.Kind != "plan_revising" {
		return policyRevisionWire{}
	}
	d := json.NewDecoder(bytes.NewReader(m.Payload))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return policyRevisionWire{}
	}
	seen := map[string]bool{}
	var raw json.RawMessage
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return policyRevisionWire{}
		}
		k, ok := key.(string)
		if !ok || seen[k] || bytes.ContainsRune([]byte(k), 0) {
			return policyRevisionWire{}
		}
		seen[k] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return policyRevisionWire{}
		}
		if k == "policy_revision" {
			raw = value
		}
	}
	carrier, err := apitypes.ParsePolicyRevisionCarrier(raw)
	return policyRevisionWire{carrier: carrier, valid: err == nil}
}

// The caller supplies the same transaction that inserted the message and holds
// the authenticated worker/run locks. This recorder extends existing complete V1
// coverage; it cannot bootstrap unknown coverage from a single taken source.
func (s *Service) writePolicyRevisionSource(ctx context.Context, tx pgx.Tx, w store.Worker, r store.Run, gen *int64, seq int32, wire policyRevisionWire) error {
	if !policyEligible(r) || r.TargetRunID.Valid || len(r.PolicyStopResolution) > 0 ||
		r.ClaimGeneration <= 0 || (gen != nil && (*gen <= 0 || *gen != r.ClaimGeneration)) || r.ClaimReleasedAt.Valid ||
		!r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != w.ID || r.UserID != w.UserID ||
		!slices.Contains([]string{"claimed", "running", "awaiting_approval"}, r.Status) {
		return nil
	}
	q := store.New(tx)
	latest, err := q.LatestPlanSeqForRun(ctx, r.ID)
	if err != nil {
		return err
	}
	if int64(seq) != latest && seq <= r.LastSeq {
		return nil
	}
	// A revision newer than the locked pre-append LastSeq destroys coverage even
	// when a later plan in this batch overtakes it. That plan does not establish
	// source acceptance. Only the latest revision can certify new coverage.
	var raw []byte
	if int64(seq) == latest && gen != nil && s.p.PolicyHoldNegotiated && s.p.PolicyHoldSnapshotEnabled && s.p.TerminalPendingLease > 0 &&
		slices.Contains(w.ProtocolCapabilities, capability.ProviderPolicyHoldV1) &&
		slices.Contains(w.ProtocolCapabilities, capability.CredentialSwitchV1) {
		raw, err = policyRevisionUpdatedSource(ctx, tx, r, w, wire)
		if err != nil {
			return err
		}
	}
	rows, err := q.SetPolicyRevisionSource(ctx, store.SetPolicyRevisionSourceParams{
		Source: raw, RunID: r.ID, WorkerID: w.ID, ClaimGeneration: r.ClaimGeneration,
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("policy revision source fence: affected %d rows", rows)
	}
	return nil
}

// Capture coverage from the locked pre-publication row. Publication must not
// make an old mismatched source appear valid by replacing the gate first.
func policyRevisionSourceBeforePublication(ctx context.Context, reader any, r store.Run) (*apitypes.PolicyRevisionSourceV1, error) {
	source, err := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
	matched, matchErr := policyRevisionSourceGateMatches(ctx, reader, r, source)
	if matchErr != nil {
		return nil, matchErr
	}
	if err != nil || !matched ||
		!policyStopSelectionValid(source, stopUUID(r.PolicyCandidateCrossCheckID)) {
		return nil, nil
	}
	return &source, nil
}

// The caller has applied a publish decision in the same claim transaction.
// Only the gate changes: primary and historical associations remain evidence,
// never an approval or an acceptance of the new candidate.
func rebindPolicyRevisionSource(ctx context.Context, q *store.Queries, owned store.Run, source *apitypes.PolicyRevisionSourceV1, revision int64) error {
	var raw []byte
	if source != nil {
		published, err := q.GetRunByID(ctx, owned.ID)
		if err != nil {
			return err
		}
		source.Gate = &apitypes.PolicyStopGateContext{
			Revision:       stopDecimal(published.GateRevision),
			PresentationID: uuid.UUID(published.GatePresentationID.Bytes).String(),
			PayloadDigest:  hex.EncodeToString(published.GatePayloadDigest),
		}
		matched, err := policyRevisionGateMatches(ctx, q, published, *source)
		if err != nil {
			return err
		}
		// ID-less publications cannot represent a V1 gate. Authenticate the
		// persisted snapshot as well as the revision actually allocated.
		if published.GateRevision == revision && published.GatePresentationID.Valid &&
			uuid.UUID(published.GatePresentationID.Bytes) != uuid.Nil &&
			matched {
			raw, err = json.Marshal(source)
			if err != nil {
				return err
			}
			if _, err := apitypes.ParsePolicyRevisionSourceV1(raw); err != nil {
				raw = nil
			}
		}
	}
	rows, err := q.SetPolicyRevisionSource(ctx, store.SetPolicyRevisionSourceParams{
		Source: raw, RunID: owned.ID, WorkerID: uuid.UUID(owned.WorkerID.Bytes), ClaimGeneration: owned.ClaimGeneration,
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("policy gate source fence: affected %d rows", rows)
	}
	return nil
}

func policyRevisionSourceGateMatches(ctx context.Context, reader any, r store.Run, source apitypes.PolicyRevisionSourceV1) (bool, error) {
	if source.Gate != nil {
		return policyRevisionGateMatches(ctx, reader, r, source)
	}
	return r.GateRevision == 0 && !r.GatePresentationID.Valid && len(r.GatePayloadDigest) == 0 &&
		(len(r.GatePresentedPayload) == 0 || bytes.Equal(bytes.TrimSpace(r.GatePresentedPayload), []byte("null"))), nil
}

// The locked row is the pre-selection snapshot; created is the row returned by
// InsertPlanCrossCheck in the same transaction. Creation adds inventory, never
// takes a revision. Historical candidates need not project to the current gate.
func policyRevisionSourceWithCreatedCrossCheck(ctx context.Context, q *store.Queries, r store.Run, created store.CrossCheck) ([]byte, error) {
	source, err := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
	matched, matchErr := policyRevisionSourceGateMatches(ctx, q, r, source)
	if matchErr != nil {
		return nil, matchErr
	}
	if err != nil || !matched ||
		!policyStopSelectionValid(source, stopUUID(r.PolicyCandidateCrossCheckID)) {
		return nil, nil
	}
	refs := append([]apitypes.PolicyRevisionRef{}, source.AssociatedCrossChecks...)
	if source.PrimaryRevision != nil && source.PrimaryRevision.CrossCheckID != nil {
		refs = append(refs, *source.PrimaryRevision)
	}
	// Visit only the finite stored identities, once each, without retries.
	// A missing/foreign witness invalidates all coverage; DB errors abort creation.
	for _, ref := range refs {
		id, err := uuid.Parse(*ref.CrossCheckID)
		if err != nil {
			return nil, nil
		}
		cc, err := q.GetPolicyStopCrossCheck(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if cc.LeadRunID != r.ID || cc.Stage != "plan" || cc.Round < 1 || cc.LeadClaimGeneration <= 0 {
			return nil, nil
		}
		valid, err := policyRevisionIdentityValid(ctx, q, cc)
		if err != nil || !valid {
			return nil, err
		}
		if id == created.ID {
			return nil, fmt.Errorf("new cross-check already in pre-creation inventory")
		}
	}
	id := created.ID.String()
	source.AssociatedCrossChecks = append(source.AssociatedCrossChecks, apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &id})
	raw, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	if _, err := apitypes.ParsePolicyRevisionSourceV1(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func policyRevisionUpdatedSource(ctx context.Context, tx pgx.Tx, r store.Run, w store.Worker, wire policyRevisionWire) ([]byte, error) {
	source, err := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
	q := store.New(tx)
	matched, matchErr := policyRevisionSourceGateMatches(ctx, q, r, source)
	if matchErr != nil {
		return nil, matchErr
	}
	if err != nil || !wire.valid || !matched ||
		!policyStopSelectionValid(source, stopUUID(r.PolicyCandidateCrossCheckID)) {
		return nil, nil
	}
	carrierSource := source
	carrierSource.Gate = wire.carrier.Gate
	matched, matchErr = policyRevisionSourceGateMatches(ctx, q, r, carrierSource)
	if matchErr != nil {
		return nil, matchErr
	}
	if !matched {
		return nil, nil
	}
	// Traverse only the stored finite inventory, once per reference, without
	// retries. A negative proof stops sibling reads; database failures abort.
	refs := append([]apitypes.PolicyRevisionRef{}, source.AssociatedCrossChecks...)
	if source.PrimaryRevision != nil {
		refs = append(refs, *source.PrimaryRevision)
	}
	next := wire.carrier.Revision
	nextValidated := false
	for _, ref := range refs {
		isNext := ref.Source == next.Source &&
			((ref.InputID != nil && next.InputID != nil && *ref.InputID == *next.InputID) ||
				(ref.CrossCheckID != nil && next.CrossCheckID != nil && *ref.CrossCheckID == *next.CrossCheckID))
		requireRevision := isNext || (source.PrimaryRevision != nil && source.PrimaryRevision.CrossCheckID != nil &&
			ref.CrossCheckID != nil && *source.PrimaryRevision.CrossCheckID == *ref.CrossCheckID)
		valid, err := policyRevisionWitness(ctx, tx, r, w, ref, isNext, requireRevision)
		if err != nil || !valid {
			return nil, err
		}
		nextValidated = nextValidated || isNext
	}
	if !nextValidated {
		valid, err := policyRevisionWitness(ctx, tx, r, w, next, true, true)
		if err != nil || !valid {
			return nil, err
		}
	}
	if source.PrimaryRevision != nil && source.PrimaryRevision.CrossCheckID != nil &&
		(next.CrossCheckID == nil || *next.CrossCheckID != *source.PrimaryRevision.CrossCheckID) {
		source.AssociatedCrossChecks = append(source.AssociatedCrossChecks, *source.PrimaryRevision)
	}
	// Moving an associated revision to primary preserves its identity exactly once.
	source.AssociatedCrossChecks = slices.DeleteFunc(source.AssociatedCrossChecks, func(ref apitypes.PolicyRevisionRef) bool {
		return next.CrossCheckID != nil && *next.CrossCheckID == *ref.CrossCheckID
	})
	source.PrimaryRevision = &next
	raw, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	if _, err = apitypes.ParsePolicyRevisionSourceV1(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func policyRevisionWitness(ctx context.Context, tx pgx.Tx, r store.Run, w store.Worker, ref apitypes.PolicyRevisionRef, primary, requireRevision bool) (bool, error) {
	if ref.Source == "run_user_input" {
		id, err := strconv.ParseInt(string(*ref.InputID), 10, 64)
		if err != nil {
			return false, nil
		}
		// Exact identity lookup, never the capped run-input history.
		q := store.New(tx)
		row, err := q.GetPolicyStopRevisionInput(ctx, store.GetPolicyStopRevisionInputParams{RunID: r.ID, InputID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !primary && row.AppliedAt.Valid {
			_, valid, err := policyStopHumanHistoryFromActive(ctx, q, r, row)
			return valid, err
		}
		bound := row.GateBinding.Valid && row.GateBinding.String == "bound" &&
			row.GateRevision.Valid && row.GateRevision.Int64 == r.GateRevision && r.GateRevision > 0
		unbound := (!row.GateBinding.Valid || row.GateBinding.String == "unbound") &&
			(!row.GateRevision.Valid || row.GateRevision.Int64 == r.GateRevision)
		return row.RunID == r.ID && row.Kind == "revise_plan" && row.ConsumedAt.Valid &&
			row.ConsumedWorkerID.Valid && uuid.UUID(row.ConsumedWorkerID.Bytes) == w.ID &&
			row.ConsumedClaimGeneration.Valid && row.ConsumedClaimGeneration.Int64 == r.ClaimGeneration &&
			!row.AppliedAt.Valid && !row.IncludedAt.Valid && !row.SettledAt.Valid &&
			!row.Disposition.Valid && !row.SupersededByStopInputID.Valid && (bound || unbound), nil
	}
	id, err := uuid.Parse(*ref.CrossCheckID)
	if err != nil {
		return false, nil
	}
	cc, err := store.New(tx).GetPolicyStopCrossCheck(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if cc.LeadRunID != r.ID || cc.Stage != "plan" || cc.Round < 1 || cc.LeadClaimGeneration <= 0 || cc.LeadClaimGeneration > r.ClaimGeneration ||
		!cc.DecidedAt.Valid || cc.RevisionSettledAt.Valid || cc.RevisionDisposition.Valid ||
		cc.RevisionSupersededByStopInputID.Valid || (cc.Verdict != "revise" && cc.Verdict != "approve") {
		return false, nil
	}
	if requireRevision && cc.Verdict != "revise" {
		return false, nil
	}
	if primary && (cc.Verdict != "revise" || cc.LeadClaimGeneration != r.ClaimGeneration ||
		!r.PolicyCandidateCrossCheckID.Valid || uuid.UUID(r.PolicyCandidateCrossCheckID.Bytes) != id) {
		return false, nil
	}
	valid, err := policyRevisionIdentityValid(ctx, store.New(tx), cc)
	if err != nil || !valid {
		return false, err
	}
	if !primary {
		return true, nil
	}
	q := store.New(tx)
	var target *apitypes.SemanticCandidate
	if r.GateRevision == 0 {
		var valid bool
		target, valid, err = policyRevisionCurrentTarget(ctx, q, r)
		if err != nil || !valid {
			return false, err
		}
		// Structural gate-zero sources retain their existing admission rules.
		if target == nil {
			return true, nil
		}
		return target.Digest == hex.EncodeToString(cc.CandidateDigest), nil
	} else {
		source, parseErr := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
		if parseErr != nil {
			return false, nil
		}
		matched, err := policyRevisionGateMatches(ctx, q, r, source)
		if err != nil || !matched {
			return false, err
		}
		payload, _, _ := decodeGatePresentedPayload(r.GatePresentedPayload)
		if payload.Semantic != nil {
			target = &payload.Semantic.Candidate
		}
	}
	return policyRevisionIdentityApplicable(ctx, q, r, cc, target, r.GateRevision == 0)
}
