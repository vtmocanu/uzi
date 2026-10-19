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
func policyRevisionSourceBeforePublication(r store.Run) *apitypes.PolicyRevisionSourceV1 {
	source, err := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
	if err != nil || !policyRevisionSourceGateMatches(r, source) ||
		!policyStopSelectionValid(source, stopUUID(r.PolicyCandidateCrossCheckID)) {
		return nil
	}
	return &source
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
		// ID-less publications cannot represent a V1 gate. Authenticate the
		// persisted snapshot as well as the revision actually allocated.
		if published.GateRevision == revision && published.GatePresentationID.Valid &&
			uuid.UUID(published.GatePresentationID.Bytes) != uuid.Nil &&
			policyRevisionGateMatches(published, *source) {
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

func policyRevisionSourceGateMatches(r store.Run, source apitypes.PolicyRevisionSourceV1) bool {
	if source.Gate != nil {
		return policyRevisionGateMatches(r, source)
	}
	return r.GateRevision == 0 && !r.GatePresentationID.Valid && len(r.GatePayloadDigest) == 0 &&
		(len(r.GatePresentedPayload) == 0 || bytes.Equal(bytes.TrimSpace(r.GatePresentedPayload), []byte("null")))
}

// The locked row is the pre-selection snapshot; created is the row returned by
// InsertPlanCrossCheck in the same transaction. Creation adds inventory, never
// takes a revision. Historical candidates need not project to the current gate.
func policyRevisionSourceWithCreatedCrossCheck(ctx context.Context, q *store.Queries, r store.Run, created store.CrossCheck) ([]byte, error) {
	source, err := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
	if err != nil || !policyRevisionSourceGateMatches(r, source) ||
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
		candidate := PlanCrossCheckCandidate{PlanMd: cc.PlanMd.String, Milestones: cc.Milestones, RequiredCapabilities: cc.RequiredCapabilities, RequiredTools: cc.RequiredTools, SizeClass: cc.SizeClass.String, BaseCommit: cc.BaseCommit.String, PlanningDiff: cc.PlanningDiff.String}
		digest, err := candidate.Digest()
		if err != nil || !bytes.Equal(digest, cc.CandidateDigest) {
			return nil, nil
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
	if err != nil || !wire.valid || !policyRevisionSourceGateMatches(r, source) ||
		!policyStopSelectionValid(source, stopUUID(r.PolicyCandidateCrossCheckID)) {
		return nil, nil
	}
	carrierSource := source
	carrierSource.Gate = wire.carrier.Gate
	if !policyRevisionSourceGateMatches(r, carrierSource) {
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
		var valid bool
		err = tx.QueryRow(ctx, `SELECT (
   run_id=$2 AND kind='revise_plan' AND consumed_at IS NOT NULL
   AND consumed_worker_id=$3 AND consumed_claim_generation=$4
   AND applied_at IS NULL AND included_at IS NULL AND settled_at IS NULL
   AND disposition IS NULL AND superseded_by_stop_input_id IS NULL
   AND ((gate_binding='bound' AND gate_revision=$5 AND $5>0)
     OR ((gate_binding IS NULL OR gate_binding='unbound')
       AND (gate_revision IS NULL OR gate_revision=$5)))
  ) IS TRUE FROM run_user_inputs WHERE id=$1`, id, r.ID, w.ID, r.ClaimGeneration, r.GateRevision).Scan(&valid)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return valid, err
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
	if cc.LeadRunID != r.ID || cc.Stage != "plan" || cc.Round < 1 || cc.LeadClaimGeneration <= 0 ||
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
	candidate := PlanCrossCheckCandidate{PlanMd: cc.PlanMd.String, Milestones: cc.Milestones, RequiredCapabilities: cc.RequiredCapabilities, RequiredTools: cc.RequiredTools, SizeClass: cc.SizeClass.String, BaseCommit: cc.BaseCommit.String, PlanningDiff: cc.PlanningDiff.String}
	digest, err := candidate.Digest()
	if err != nil || !bytes.Equal(digest, cc.CandidateDigest) {
		return false, nil
	}
	if r.GateRevision == 0 {
		return true, nil
	}
	projection := gatePresentedPayload{PlanMd: textPtr(cc.PlanMd), Milestones: cc.Milestones, RequiredCapabilities: capability.Filter(cc.RequiredCapabilities), RequiredTools: capability.FilterTools(cc.RequiredTools), SizeClass: nonEmptyPtr(cc.SizeClass.String)}
	projected, err := projection.canonicalJSON()
	current, currentErr := gatePayloadFromRow(r).canonicalJSON()
	return err == nil && currentErr == nil && bytes.Equal(projected, current), nil
}
