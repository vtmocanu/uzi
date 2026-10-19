package workersvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

var (
	ErrReconciliationRequired = errors.New("changed milestone contract requires explicit owner reconciliation")
	ErrReconciliationConflict = errors.New("milestone reconciliation consent no longer matches the run")
	ErrReconciliationNotReady = errors.New("milestone reconciliation requires a current published checkpoint witness")
)

// Audit limits bound the encoded audit envelope (records and cursor), not other
// run-detail fields. At most 51 rows are examined and 50 returned. Each snapshot
// is at most 8 KiB before encoding; reasons are at most 256 runes. Even JSON
// control/HTML escaping leaves a single record below the 256 KiB page budget.
const (
	reconciliationAuditRows     = 50
	reconciliationAuditBytes    = 256 * 1024
	reconciliationSnapshotBytes = 8192
	reconciliationReasonRunes   = 256
)

type MilestoneReconciliationAuditPage struct {
	Records                []json.RawMessage `json:"milestone_reconciliations,omitempty"`
	NextBeforeGateRevision *int64            `json:"milestone_reconciliations_next_before_gate_revision,omitempty"`
}

// MilestoneReconciliationAudit returns the bounded first owner page.
func (s *Service) MilestoneReconciliationAudit(ctx context.Context, owner, runID uuid.UUID) ([]json.RawMessage, error) {
	page, err := s.MilestoneReconciliationAuditBefore(ctx, owner, runID, 0)
	return page.Records, err
}

// MilestoneReconciliationAuditBefore projects durable history without rewriting it.
// Zero selects the first page; positive cursors select strictly older revisions.
func (s *Service) MilestoneReconciliationAuditBefore(ctx context.Context, owner, runID uuid.UUID, before int64) (MilestoneReconciliationAuditPage, error) {
	page := MilestoneReconciliationAuditPage{}
	if before < 0 {
		return page, errors.New("invalid reconciliation cursor")
	}
	reader, ok := s.q.(interface {
		ListRunMilestoneReconciliations(context.Context, store.ListRunMilestoneReconciliationsParams) ([]store.ListRunMilestoneReconciliationsRow, error)
	})
	if !ok {
		return page, nil
	}
	rows, err := reader.ListRunMilestoneReconciliations(ctx, store.ListRunMilestoneReconciliationsParams{RunID: runID, OwnerID: owner, BeforeGateRevision: before})
	if err != nil {
		return page, err
	}
	count := min(len(rows), reconciliationAuditRows+1)
	var last int64
	for i := 0; i < count; i++ {
		row := rows[i]
		if row.OwnerID != owner || row.RunID != runID {
			return MilestoneReconciliationAuditPage{}, nil
		}
		if row.GateRevision <= 0 || (before > 0 && row.GateRevision >= before) || (last > 0 && row.GateRevision >= last) {
			return MilestoneReconciliationAuditPage{}, errors.New("invalid reconciliation history order")
		}
		if i == reconciliationAuditRows {
			page.NextBeforeGateRevision = &last
			break
		}
		reason := row.Reason
		// Decode only a bounded prefix even if a fake returns an enormous reason.
		end := 0
		for n := 0; end < len(reason) && n < reconciliationReasonRunes; n++ {
			_, size := utf8.DecodeRuneInString(reason[end:])
			end += size
		}
		if end < len(reason) {
			reason = reason[:end]
			row.ReasonTruncated = true
		}
		snapshots := []json.RawMessage{row.Consent, row.BeforeState, row.AfterState}
		for j, raw := range snapshots {
			if len(raw) > reconciliationSnapshotBytes {
				snapshots[j] = nil
				row.DetailsOmitted = true
			}
		}
		b, err := json.Marshal(struct {
			ID              uuid.UUID       `json:"id"`
			GateRevision    int64           `json:"gate_revision"`
			ClaimGeneration int64           `json:"claim_generation"`
			Reason          string          `json:"reason"`
			ReasonTruncated bool            `json:"reason_truncated,omitempty"`
			Consent         json.RawMessage `json:"consent"`
			BeforeState     json.RawMessage `json:"before_state"`
			AfterState      json.RawMessage `json:"after_state"`
			DetailsOmitted  bool            `json:"details_omitted,omitempty"`
			AppliedInputID  int64           `json:"applied_input_id"`
			CreatedAt       any             `json:"created_at"`
		}{row.ID, row.GateRevision, row.ClaimGeneration, reason, row.ReasonTruncated,
			snapshots[0], snapshots[1], snapshots[2], row.DetailsOmitted, row.AppliedInputID, row.CreatedAt})
		if err != nil {
			return MilestoneReconciliationAuditPage{}, err
		}
		candidate := MilestoneReconciliationAuditPage{Records: append(page.Records, b)}
		revision := row.GateRevision
		candidate.NextBeforeGateRevision = &revision
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return MilestoneReconciliationAuditPage{}, err
		}
		if len(encoded) > reconciliationAuditBytes {
			if last == 0 {
				return MilestoneReconciliationAuditPage{}, errors.New("reconciliation record exceeds page limit")
			}
			page.NextBeforeGateRevision = &last
			break
		}
		page.Records = candidate.Records
		last = row.GateRevision
	}
	return page, nil
}

func validGitObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	b, err := hex.DecodeString(s)
	return err == nil && len(b) > 0 && strings.Trim(s, "0") != ""
}

// ResetStateDigest uses only authoritative runs-row facts. Input dispositions and
// display-only checkpoint_contains_latest are deliberately outside this consent binding.
func ResetStateDigest(r store.Run) (string, error) {
	state := map[string]any{
		"frozen":    json.RawMessage(jsonOrNull(r.MilestonesFrozen)),
		"candidate": json.RawMessage(jsonOrNull(r.MilestonesCandidate)),
		"plan":      r.PlanMd, "gate_revision": r.GateRevision, "presentation": r.GatePresentationID,
		"gate_digest": r.GatePayloadDigest, "gate_payload": json.RawMessage(jsonOrNull(r.GatePresentedPayload)),
		"contract": json.RawMessage(jsonOrNull(r.CompletionContract)), "revision": r.ContractRevision,
		"contract_version": r.CompletionContractVersion,
		"completed":        json.RawMessage(jsonOrNull(r.MilestonesCompleted)),
		"in_progress":      json.RawMessage(jsonOrNull(r.MilestonesInProgress)),
		"agents":           json.RawMessage(jsonOrNull(r.MilestonesAgents)),
		"scope":            r.ScopeCeiling, "stop": r.StopKind, "stop_reason": r.StopReason,
		"worker": r.WorkerID, "generation": r.ClaimGeneration, "released": r.ClaimReleasedAt,
		"checkpoint": r.CheckpointTip, "checkpoint_at": r.CheckpointTipAt,
		"status": r.Status, "status_since": r.StatusSince, "revise_count": r.ReviseCount,
		"started_at": r.StartedAt, "claimed_at": r.ClaimedAt, "paused_seconds": r.BudgetPausedSeconds,
		"hold": r.HoldReason, "hold_head": r.HoldCapturedHead,
		"pause": r.PauseRequestedAt, "pause_mode": r.PauseMode, "pause_after": r.PauseAfterCount,
		"switch": r.CredentialSwitchRequestedAt, "switch_generation": r.CredentialSwitchGeneration,
		"run": r.ID, "kind": r.Kind, "repo": r.RepoID, "issue": r.IssueIid,
		"branch": r.Branch, "base_branch": r.BaseBranch, "session": r.SessionID,
		"recovery_wait": r.RecoveryWaitCause, "recovery_count": r.RecoveryWaitCount,
		"recovery_episode":  r.WorkerRecoveryEpisode,
		"recovery_evidence": json.RawMessage(jsonOrNull(r.WorkerRecoveryEvidence)),
		"released_worker":   r.ReleasedWorkerID, "released_nonce": r.ReleasedWorkerNonce,
	}
	b, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// reconciliationSnapshot keeps JSONB definitions readable in the owner audit.
func reconciliationSnapshot(r store.Run) ([]byte, error) {
	encoded, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		return nil, err
	}
	for key, raw := range map[string][]byte{
		"milestones_frozen":      r.MilestonesFrozen,
		"milestones_candidate":   r.MilestonesCandidate,
		"milestones_completed":   r.MilestonesCompleted,
		"milestones_in_progress": r.MilestonesInProgress,
		"milestones_agents":      r.MilestonesAgents,
		"completion_contract":    r.CompletionContract,
		"gate_presented_payload": r.GatePresentedPayload,
	} {
		snapshot[key] = json.RawMessage(jsonOrNull(raw))
	}
	return json.Marshal(snapshot)
}

func jsonOrNull(b []byte) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}

func changedMilestoneContract(r store.Run) bool {
	if len(r.MilestonesFrozen) == 0 {
		return false
	}
	frozen, e1 := canonicalRawJSON(r.MilestonesFrozen)
	candidate, e2 := canonicalRawJSON(r.MilestonesCandidate)
	return e1 != nil || e2 != nil || !bytes.Equal(frozen, candidate)
}

// beginClaimMutation serializes even an unstamped legacy request before reading
// the durable reconciliation predicate. Callers keep all mutations on the returned
// service and commit once; park helpers own this seam rather than nesting SetState.
func (s *Service) beginClaimMutation(ctx context.Context, w store.Worker, id uuid.UUID, g *int64) (*Service, store.Run, pgx.Tx, error) {
	if s.txBeginner == nil {
		r, err := s.runOwnedByWorker(ctx, id, w)
		return s, r, nil, err
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return nil, store.Run{}, nil, err
	}
	fail := func(err error) (*Service, store.Run, pgx.Tx, error) {
		_ = tx.Rollback(ctx)
		return nil, store.Run{}, nil, err
	}
	q := store.New(tx)
	if _, err := q.GetWorkerForUpdate(ctx, w.ID); err != nil {
		return fail(err)
	}
	r, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: id, WorkerID: pgconv.UUID(w.ID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return fail(ErrRunNotOwned)
	}
	if err != nil {
		return fail(err)
	}
	reconciled, err := reconciliationExists(ctx, tx, id)
	if err != nil {
		return fail(err)
	}
	if reconciled {
		if err := validateReconciledGeneration(r, g); err != nil {
			return fail(err)
		}
	}
	return s.boundClaimControlService(q), r, tx, nil
}

// beginReadCommitted gives post-lock audit reads a fresh statement snapshot even
// when the connection's default isolation is stricter.
func (s *Service) beginReadCommitted(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

func reconciliationExists(ctx context.Context, tx pgx.Tx, runID uuid.UUID) (bool, error) {
	var exists bool
	// This statement must execute AFTER the run lock: a waiting UPDATE's
	// statement snapshot cannot observe the audit committed by the lock holder.
	err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM run_milestone_reconciliations WHERE run_id=$1)", runID).Scan(&exists)
	return exists, err
}

func validateReconciledGeneration(r store.Run, g *int64) error {
	if g == nil || *g <= 0 {
		return ErrMissingClaimGeneration
	}
	if *g != r.ClaimGeneration || r.ClaimReleasedAt.Valid {
		return ErrStaleClaim
	}
	return nil
}

func (s *Service) reconcileMilestoneApproval(ctx context.Context, owner uuid.UUID, observed store.Run, body string, sel *AgentSelection, opts SubmitInputOptions) (SubmitInputResult, error) {
	dec := opts.Reconciliation
	if dec.Mode != "reset" || strings.TrimSpace(dec.Reason) == "" ||
		opts.ExpectedGateRevision == nil || *opts.ExpectedGateRevision <= 0 || opts.ExpectedResetStateDigest == "" {
		return SubmitInputResult{}, ErrReconciliationConflict
	}
	consent, err := json.Marshal(struct {
		Decision  any             `json:"reconciliation"`
		Gate      int64           `json:"gate_revision"`
		Digest    string          `json:"reset_state_digest"`
		Selection *AgentSelection `json:"selection"`
		Override  bool            `json:"override_capabilities"`
	}{dec, *opts.ExpectedGateRevision, opts.ExpectedResetStateDigest, sel, opts.OverrideCapabilities})
	if err != nil {
		return SubmitInputResult{}, err
	}
	if s.txBeginner == nil {
		return SubmitInputResult{}, ErrReconciliationNotReady
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return SubmitInputResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	// Serialize against claim/release in worker -> run -> retention order.
	if observed.WorkerID.Valid {
		if _, err := q.GetWorkerForUpdate(ctx, uuid.UUID(observed.WorkerID.Bytes)); err != nil {
			return SubmitInputResult{}, err
		}
	}
	run, err := q.GetRunByIDForUpdate(ctx, observed.ID)
	if err != nil {
		return SubmitInputResult{}, err
	}
	if run.UserID != owner {
		return SubmitInputResult{}, ErrRunNotFound
	}
	if semantic, err := semanticEnrollmentPresent(ctx, q, run.ID); err != nil || semantic {
		if err != nil {
			return SubmitInputResult{}, err
		}
		return SubmitInputResult{}, ErrSemanticResetUnsupported
	}
	var priorConsent []byte
	var inputID int64
	var created time.Time
	err = tx.QueryRow(ctx, "SELECT consent, applied_input_id, created_at FROM run_milestone_reconciliations WHERE run_id=$1 AND gate_revision=$2", run.ID, *opts.ExpectedGateRevision).Scan(&priorConsent, &inputID, &created)
	if err == nil {
		a, _ := canonicalRawJSON(priorConsent)
		b, _ := canonicalRawJSON(consent)
		if !bytes.Equal(a, b) {
			return SubmitInputResult{}, ErrReconciliationConflict
		}
		return SubmitInputResult{ServerSide: true, ID: inputID, CreatedAt: created}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return SubmitInputResult{}, err
	}
	digest, err := ResetStateDigest(run)
	if err != nil {
		return SubmitInputResult{}, err
	}
	if run.Status != "awaiting_approval" || run.ClaimReleasedAt.Valid || !run.WorkerID.Valid ||
		run.WorkerID != observed.WorkerID || run.ClaimGeneration <= 0 ||
		run.GateRevision != *opts.ExpectedGateRevision || !run.GatePresentationID.Valid ||
		digest != opts.ExpectedResetStateDigest || !changedMilestoneContract(run) ||
		(run.ContractRevision.Valid != (dec.ExpectedCompletionRevision != nil)) ||
		(dec.ExpectedCompletionRevision != nil && int(run.ContractRevision.Int32) != *dec.ExpectedCompletionRevision) {
		return SubmitInputResult{}, ErrReconciliationConflict
	}
	if err := requireNoPendingRevision(ctx, tx, run); err != nil {
		return SubmitInputResult{}, err
	}
	payload, ok, err := decodeGatePresentedPayload(run.GatePresentedPayload)
	if err != nil || !ok {
		return SubmitInputResult{}, ErrReconciliationConflict
	}
	_, sum, err := payload.digest()
	candidate, cerr := canonicalRawJSON(run.MilestonesCandidate)
	presented, perr := canonicalRawJSON(payload.Milestones)
	if err != nil || cerr != nil || perr != nil || !bytes.Equal(sum, run.GatePayloadDigest) ||
		!bytes.Equal(candidate, presented) || payload.PlanMd == nil || *payload.PlanMd != run.PlanMd.String {
		return SubmitInputResult{}, ErrReconciliationConflict
	}
	// An absent list is not an explicit empty replacement.
	if len(candidate) == 0 || bytes.Equal(candidate, []byte("null")) {
		return SubmitInputResult{}, ErrReconciliationConflict
	}
	ms, err := DecodeMilestones(candidate)
	if err != nil {
		return SubmitInputResult{}, ErrReconciliationConflict
	}
	if _, valid := validateMilestones(ms); !valid && len(ms) > 0 {
		return SubmitInputResult{}, ErrReconciliationConflict
	}
	if run.CompletionContractVersion.Valid != run.ContractRevision.Valid ||
		(!run.CompletionContractVersion.Valid && len(bytes.TrimSpace(run.CompletionContract)) > 0 && !bytes.Equal(bytes.TrimSpace(run.CompletionContract), []byte("null"))) {
		return SubmitInputResult{}, ErrReconciliationConflict
	}
	if run.CompletionContractVersion.Valid {
		if oldMilestones, err := DecodeMilestones(run.MilestonesFrozen); err != nil {
			return SubmitInputResult{}, ErrReconciliationConflict
		} else if len(oldMilestones) > 0 {
			if _, verifiable := scopeCapEligibility(run); !verifiable {
				return SubmitInputResult{}, ErrReconciliationConflict
			}
		}
		prior, err := parseCompletionContract(run.CompletionContract)
		if err != nil || prior.Profile != contractProfileStructural || !run.ContractRevision.Valid ||
			prior.Revision != int(run.ContractRevision.Int32) || prior.Revision < 1 ||
			run.ContractRevision.Int32 == 2147483647 {
			return SubmitInputResult{}, ErrReconciliationConflict
		}
		expectedBytes, err := buildCompletionContract(run.MilestonesFrozen)
		if err != nil {
			return SubmitInputResult{}, ErrReconciliationConflict
		}
		expected, err := parseCompletionContract(expectedBytes)
		if err != nil {
			return SubmitInputResult{}, ErrReconciliationConflict
		}
		actualCriteria, err := json.Marshal(prior.Criteria)
		if err != nil {
			return SubmitInputResult{}, ErrReconciliationConflict
		}
		expectedCriteria, err := json.Marshal(expected.Criteria)
		if err != nil || !bytes.Equal(actualCriteria, expectedCriteria) {
			return SubmitInputResult{}, ErrReconciliationConflict
		}
	}
	witness := payload.CheckpointWitness
	if witness == nil || payload.ReviseCount == nil || *payload.ReviseCount != run.ReviseCount ||
		payload.WitnessWorkerID == nil || payload.WitnessClaimGeneration == nil ||
		*payload.WitnessWorkerID != uuid.UUID(run.WorkerID.Bytes) || *payload.WitnessClaimGeneration != run.ClaimGeneration ||
		!validGitObjectID(witness.SourceHead) || !validGitObjectID(witness.PublishedCheckpoint) ||
		!run.CheckpointTip.Valid || run.CheckpointTip.String != witness.PublishedCheckpoint ||
		run.PauseRequestedAt.Valid || run.CredentialSwitchRequestedAt.Valid ||
		s.retentionPool == nil || s.listRefTipsFn == nil || s.box == nil || s.forgeBaseURLAllowed == nil {
		return SubmitInputResult{}, ErrReconciliationNotReady
	}
	// The existing two-key retention lock is taken on this transaction's connection;
	// every row read/write below uses the same transaction, including forge context.
	var retained bool
	err = tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1,$2)", store.CheckpointRetentionLockClass, store.CheckpointRetentionLockObjID(run.ID)).Scan(&retained)
	if err != nil {
		return SubmitInputResult{}, err
	}
	if !retained {
		return SubmitInputResult{}, ErrReconciliationNotReady
	}
	lockedService := s.boundClaimControlService(q)
	forge, problem, gone := lockedService.forgeForRetention(ctx, run.ID)
	if problem != "" || gone {
		return SubmitInputResult{}, ErrReconciliationNotReady
	}
	branch, branchOK := checkpointBranch(run.Kind, run.ID, run.IssueIid)
	if !branchOK {
		return SubmitInputResult{}, ErrReconciliationNotReady
	}
	ref := checkpointRefPrefix + branch
	tips, err := s.listRefTipsFn(ctx, pushbroker.ListRefsOptions{CloneURL: forge.cloneURL, Username: forge.username, PAT: forge.pat}, ref)
	if err != nil || tips[ref] != witness.PublishedCheckpoint {
		return SubmitInputResult{}, ErrReconciliationNotReady
	}
	if !opts.OverrideCapabilities {
		if err := lockedService.capabilityGate(ctx, run); err != nil {
			return SubmitInputResult{}, err
		}
	}
	var selectionJSON []byte
	if sel != nil {
		roster, err := lockedService.rosterFor(ctx, run, sel.Source, nil)
		if err != nil {
			return SubmitInputResult{}, err
		}
		if err := validateSelection(*sel, roster); err != nil {
			return SubmitInputResult{}, err
		}
		selectionJSON, err = json.Marshal(sel)
		if err != nil {
			return SubmitInputResult{}, err
		}
		body = string(selectionJSON)
	}
	var contract []byte
	if run.CompletionContractVersion.Valid {
		contract, err = buildCompletionContract(candidate)
		if err != nil {
			return SubmitInputResult{}, err
		}
		c, err := parseCompletionContract(contract)
		if err != nil {
			return SubmitInputResult{}, err
		}
		c.Revision = int(run.ContractRevision.Int32) + 1
		contract, err = json.Marshal(c)
		if err != nil {
			return SubmitInputResult{}, err
		}
	}
	before, err := reconciliationSnapshot(run)
	if err != nil {
		return SubmitInputResult{}, err
	}
	var cutoff int64
	if err := tx.QueryRow(ctx, "SELECT COALESCE(max(id),0) FROM run_user_inputs WHERE run_id=$1", run.ID).Scan(&cutoff); err != nil {
		return SubmitInputResult{}, err
	}
	cleanBody, _ := stripNUL(body)
	err = tx.QueryRow(ctx, `INSERT INTO run_user_inputs(run_id,kind,body,gate_binding,gate_revision,consumed_at,applied_at,disposition)
 VALUES($1,'approve_plan',$2,'bound',$3,now(),now(),'applied') RETURNING id,created_at`, run.ID, cleanBody, run.GateRevision).Scan(&inputID, &created)
	if err != nil {
		return SubmitInputResult{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE runs SET milestones_frozen=$2, milestones_completed='[]', milestones_in_progress='[]', milestones_agents='[]',
 completion_contract=$3, contract_revision=CASE WHEN completion_contract_version IS NOT NULL THEN COALESCE(contract_revision,0)+1 ELSE NULL END,
 scope_ceiling=NULL, stop_kind=NULL, stop_reason=NULL, latest_completion_attempt=NULL,
 agent_source=CASE WHEN $4::jsonb IS NULL THEN agent_source ELSE $4::jsonb->>'source' END,
 agent_exclusions=CASE WHEN $4::jsonb IS NULL THEN agent_exclusions ELSE $4::jsonb->'exclusions' END,
 required_capabilities=CASE WHEN $5 THEN '{}'::text[] ELSE required_capabilities END,
 status='queued', status_since=now(), claim_released_at=now(),
 budget_paused_seconds=budget_paused_seconds+GREATEST(0,EXTRACT(EPOCH FROM(now()-status_since))::int),
 codex_cap_hash=NULL, codex_claim_epoch=codex_claim_epoch+1, health='ok', health_reason=NULL, health_since=NULL, updated_at=now()
 WHERE id=$1`, run.ID, candidate, contract, selectionJSON, opts.OverrideCapabilities)
	if err != nil {
		return SubmitInputResult{}, err
	}
	_, err = tx.Exec(ctx, "UPDATE run_completion_permits SET consumed_at=now() WHERE run_id=$1 AND consumed_at IS NULL", run.ID)
	if err != nil {
		return SubmitInputResult{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE run_user_inputs SET disposition='superseded' WHERE run_id=$1 AND id<=$2 AND disposition IS NULL AND kind IN ('stop','cancel','reject_plan','scope','approve_plan')`, run.ID, cutoff)
	if err != nil {
		return SubmitInputResult{}, err
	}
	// Pin an existing branch ref without resetting a conflicting retention record.
	tag, err := tx.Exec(ctx, `INSERT INTO checkpoint_retentions(run_id,user_id,repo_id,branch,tip,ref,state)
 VALUES($1,$2,$3,$4,$5,$6,'retained')
 ON CONFLICT(run_id) DO UPDATE SET state='retained',updated_at=now()
 WHERE checkpoint_retentions.ref=EXCLUDED.ref AND checkpoint_retentions.tip=EXCLUDED.tip
 AND checkpoint_retentions.state='retained'`, run.ID, owner, run.RepoID, branch, witness.PublishedCheckpoint, ref)
	if err != nil {
		return SubmitInputResult{}, err
	}
	if tag.RowsAffected() != 1 {
		return SubmitInputResult{}, ErrReconciliationNotReady
	}
	afterRun, err := q.GetRunByIDForUpdate(ctx, run.ID)
	if err != nil {
		return SubmitInputResult{}, err
	}
	after, err := reconciliationSnapshot(afterRun)
	if err != nil {
		return SubmitInputResult{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO run_milestone_reconciliations(id,run_id,owner_id,gate_revision,presentation_id,gate_digest,reset_state_digest,
 worker_id,claim_generation,reason,consent,before_state,after_state,applied_input_id,stop_cutoff)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		uuid.New(), run.ID, owner, run.GateRevision, run.GatePresentationID, run.GatePayloadDigest, digest, run.WorkerID, run.ClaimGeneration,
		dec.Reason, consent, before, after, inputID, cutoff)
	if err != nil {
		return SubmitInputResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SubmitInputResult{}, err
	}
	// Durable release already fenced the old attempt. Cleanup follows commit and
	// matches only its generation, preserving a concurrently recorded successor.
	s.persistFail.evict(run.ID, run.ClaimGeneration)
	if s.bcast != nil {
		s.bcast.PublishState(run.ID, "queued")
	}
	s.notify(run.ID, "queued")
	return SubmitInputResult{ServerSide: true, ID: inputID, CreatedAt: created}, nil
}
