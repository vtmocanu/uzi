package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// credential_switch_apply.go is the SERVER side of the in-place Claude credential switch apply
// (issue #4201, capability credential_switch_in_place_v1, feature credential_switch_apply_v1).
//
// A held run whose owner ran `uzi run set-token` carries a switch stamp (an op id the database
// minted). A worker that registered the in-place capability asks THIS endpoint for the new token
// instead of releasing its claim: ApplyCredentialSwitch resolves the credential the next claim
// would spend, and in ONE transaction journals the switch (a child epoch of the claim), withdraws
// the stamp, records the credential on the run and appends the 'credential_switch' feed message
// whose seq the worker's later frames follow. Only after that commit is the token returned.
//
// The transaction is beginClaimMutation's: the worker row FOR UPDATE, then the run row FOR UPDATE,
// then the chosen token row FOR SHARE (LockAnthropicSecretForApply). The share lock is what makes
// the returned bytes the bytes of a token that could not be rotated, disabled or deleted while the
// apply decided; a rotation that lands after the commit is the replay's refused_committed case.

// Apply outcomes, the JSON "outcome" the endpoint answers.
const (
	// ApplyOutcomeApplied: the switch is journaled and the token is in the response. Replay is
	// true for a retry of an operation that already committed.
	ApplyOutcomeApplied = "applied"
	// ApplyOutcomeSuperseded: the operation named is not the pending one; a newer request replaced it.
	ApplyOutcomeSuperseded = "superseded"
	// ApplyOutcomeNotApplied: nothing was written and the worker keeps its current token; Reason says why.
	ApplyOutcomeNotApplied = "not_applied"
	// ApplyOutcomeRefusedCommitted: the operation committed earlier but the token can no longer be
	// handed out (rotated, disabled, rejected or deleted since); the worker cannot reconstruct the
	// state and must report uncertain.
	ApplyOutcomeRefusedCommitted = "refused_committed"
	// ApplyOutcomeFenced: the claim, registration, worker or run state the request names is not the
	// current one; nothing was written.
	ApplyOutcomeFenced = "fenced"
)

// Reasons a not_applied (or refused_committed) outcome carries. They are a closed vocabulary the
// worker may log; none carries credential material.
const (
	applyReasonNoPendingSwitch      = "no_pending_switch"
	applyReasonGenerationMismatch   = "generation_mismatch"
	applyReasonNoCredential         = "no_credential"          //nolint:gosec // G101: an apply-outcome reason VOCABULARY value, not a credential.
	applyReasonCredentialDisabled   = "credential_disabled"    //nolint:gosec // G101: an apply-outcome reason VOCABULARY value, not a credential.
	applyReasonCredentialRejected   = "credential_rejected"    //nolint:gosec // G101: an apply-outcome reason VOCABULARY value, not a credential.
	applyReasonCredentialUnavail    = "credential_unavailable" //nolint:gosec // G101: an apply-outcome reason VOCABULARY value, not a credential.
	applyReasonVaultLocked          = "vault_locked"
	applyReasonPoolEmpty            = "pool_empty"
	applyReasonReserveHeld          = "reserve_held"
	applyReasonStampCleared         = "stamp_cleared"
	applyReasonCredentialChanged    = "credential_changed"         //nolint:gosec // G101: an apply-outcome reason VOCABULARY value, not a credential.
	applyReasonCredentialGone       = "credential_gone"            //nolint:gosec // G101: an apply-outcome reason VOCABULARY value, not a credential.
	applyReasonCredentialNotDecrypt = "credential_not_decryptable" //nolint:gosec // G101: an apply-outcome reason VOCABULARY value, not a credential.
)

// ErrInvalidApplyRequest is a malformed apply request (the handler answers 400).
var ErrInvalidApplyRequest = errors.New("invalid credential switch apply request")

// Bounds on the informational old_cumulative snapshot of the credential being replaced.
const (
	maxApplyOldCumulativeEntries = 64
	maxApplyOldCumulativeKeyLen  = 200
)

// CredentialSwitchApplyRequest is the decoded apply body. The handler decodes it strictly and
// bounded; OldCumulative is informational only (it is journaled, never used to decide anything).
type CredentialSwitchApplyRequest struct {
	ClaimGeneration int64           `json:"claim_generation"`
	RegisterNonce   string          `json:"register_nonce"`
	OpID            string          `json:"op_id"`
	OldCumulative   json.RawMessage `json:"old_cumulative,omitempty"`
}

// CredentialSwitchApplyResult is the apply endpoint's answer. Token is the plaintext Anthropic
// credential on an applied outcome and is NEVER logged: LogValue redacts it, so a result passed to
// slog (as a value or pointer) prints the outcome and coordinates only.
type CredentialSwitchApplyResult struct {
	Outcome    string
	Token      string
	Ordinal    int32
	MessageSeq int32
	Replay     bool
	Reason     string
}

// LogValue implements slog.LogValuer: the token is replaced by a fixed marker.
func (r CredentialSwitchApplyResult) LogValue() slog.Value {
	token := ""
	if r.Token != "" {
		token = "[REDACTED]"
	}
	return slog.GroupValue(
		slog.String("outcome", r.Outcome),
		slog.String("token", token),
		slog.Int("ordinal", int(r.Ordinal)),
		slog.Int("message_seq", int(r.MessageSeq)),
		slog.Bool("replay", r.Replay),
		slog.String("reason", r.Reason),
	)
}

// String and GoString keep a stray %v / %+v / %#v from printing the token.
func (r CredentialSwitchApplyResult) String() string   { return fmt.Sprint(r.LogValue()) }
func (r CredentialSwitchApplyResult) GoString() string { return r.String() }

// MarshalJSON is the wire shape. An applied outcome always states replay; the other outcomes carry
// only outcome and, for not_applied and refused_committed, reason.
func (r CredentialSwitchApplyResult) MarshalJSON() ([]byte, error) {
	if r.Outcome == ApplyOutcomeApplied {
		return json.Marshal(struct {
			Outcome    string `json:"outcome"`
			Token      string `json:"token"`
			Ordinal    int32  `json:"ordinal"`
			MessageSeq int32  `json:"message_seq"`
			Replay     bool   `json:"replay"`
		}{r.Outcome, r.Token, r.Ordinal, r.MessageSeq, r.Replay})
	}
	return json.Marshal(struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason,omitempty"`
	}{r.Outcome, r.Reason})
}

// ValidateCredentialSwitchApplyRequest checks the request shape and returns the parsed op id and the
// normalized old_cumulative bytes (nil when absent or null).
func ValidateCredentialSwitchApplyRequest(req CredentialSwitchApplyRequest) (uuid.UUID, []byte, error) {
	op, err := uuid.Parse(req.OpID)
	if err != nil || op == uuid.Nil {
		return uuid.Nil, nil, fmt.Errorf("%w: op_id must be a UUID", ErrInvalidApplyRequest)
	}
	if req.ClaimGeneration <= 0 {
		return uuid.Nil, nil, fmt.Errorf("%w: claim_generation must be positive", ErrInvalidApplyRequest)
	}
	if !validHoldRegisterNonce(req.RegisterNonce) {
		return uuid.Nil, nil, fmt.Errorf("%w: register_nonce is required", ErrInvalidApplyRequest)
	}
	cum, err := validateOldCumulative(req.OldCumulative)
	if err != nil {
		return uuid.Nil, nil, err
	}
	return op, cum, nil
}

// validateOldCumulative accepts an absent/null value or a JSON object of at most
// maxApplyOldCumulativeEntries entries whose keys are at most maxApplyOldCumulativeKeyLen bytes and
// whose values are non-negative integers. It returns the re-marshaled object.
func validateOldCumulative(raw json.RawMessage) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil || m == nil {
		return nil, fmt.Errorf("%w: old_cumulative must be an object of non-negative integers", ErrInvalidApplyRequest)
	}
	if len(m) > maxApplyOldCumulativeEntries {
		return nil, fmt.Errorf("%w: old_cumulative has more than %d entries", ErrInvalidApplyRequest, maxApplyOldCumulativeEntries)
	}
	for k, v := range m {
		if len(k) > maxApplyOldCumulativeKeyLen {
			return nil, fmt.Errorf("%w: old_cumulative key longer than %d bytes", ErrInvalidApplyRequest, maxApplyOldCumulativeKeyLen)
		}
		// A JSON number token of digits only: rejects strings, fractions, exponents and signs.
		digits := bytes.TrimSpace(v)
		if len(digits) == 0 || len(digits) > 18 || strings.Trim(string(digits), "0123456789") != "" {
			return nil, fmt.Errorf("%w: old_cumulative values must be non-negative integers", ErrInvalidApplyRequest)
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("%w: old_cumulative", ErrInvalidApplyRequest)
	}
	return out, nil
}

// applyLiveStatus reports whether a run in status can have an in-place apply decided for it.
func applyLiveStatus(status string) bool {
	switch status {
	case "running", "awaiting_approval", "awaiting_input", "awaiting_followup":
		return true
	}
	return false
}

// ApplyCredentialSwitch decides one in-place credential switch apply. See the file comment for the
// transaction shape. It returns ErrRunNotOwned (the handler's exact 404) for a run the worker does
// not hold; every other refusal is an outcome, and an outcome other than applied commits nothing.
// The caller validated the request (ValidateCredentialSwitchApplyRequest) and passes the results.
func (s *Service) ApplyCredentialSwitch(ctx context.Context, wkr store.Worker, runID uuid.UUID, req CredentialSwitchApplyRequest) (CredentialSwitchApplyResult, error) {
	op, oldCumulative, err := ValidateCredentialSwitchApplyRequest(req)
	if err != nil {
		return CredentialSwitchApplyResult{}, err
	}
	if s.txBeginner == nil {
		return CredentialSwitchApplyResult{}, errors.New("credential switch apply requires a transactional store")
	}
	gen := req.ClaimGeneration
	child, run, tx, err := s.beginClaimMutation(ctx, wkr, runID, &gen)
	if err != nil {
		if errors.Is(err, ErrStaleClaim) || errors.Is(err, ErrMissingClaimGeneration) {
			return CredentialSwitchApplyResult{Outcome: ApplyOutcomeFenced}, nil
		}
		return CredentialSwitchApplyResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := store.New(tx)

	// Step 0: the fence. The worker row is read after its lock, so the capability and the nonce are
	// the committed ones, not the middleware's earlier snapshot.
	w, err := child.q.GetWorkerByID(ctx, wkr.ID)
	if err != nil {
		return CredentialSwitchApplyResult{}, err
	}
	fenced := CredentialSwitchApplyResult{Outcome: ApplyOutcomeFenced}
	if !slices.Contains(w.ProtocolCapabilities, capability.CredentialSwitchInPlaceV1) ||
		run.Kind == runkind.Job || run.Kind == runkind.CrossCheck ||
		!w.SnapshotRegisterNonce.Valid || w.SnapshotRegisterNonce.String != req.RegisterNonce ||
		!run.WorkerID.Valid || uuid.UUID(run.WorkerID.Bytes) != wkr.ID ||
		run.ClaimGeneration != gen || run.ClaimReleasedAt.Valid || !applyLiveStatus(run.Status) {
		return fenced, nil
	}

	// Step 1: replay. The operation already committed in this run and generation.
	prior, err := qtx.GetRunCredentialSwitchEpochByOp(ctx, store.GetRunCredentialSwitchEpochByOpParams{OpID: op, RunID: runID})
	switch {
	case err == nil:
		if prior.ClaimGeneration != gen || !prior.WorkerID.Valid || uuid.UUID(prior.WorkerID.Bytes) != wkr.ID {
			return fenced, nil
		}
		return s.replayCredentialSwitchApply(ctx, qtx, run, prior)
	case !errors.Is(err, pgx.ErrNoRows):
		return CredentialSwitchApplyResult{}, err
	}

	// Step 2: the operation is not the pending one.
	stampPending := run.CredentialSwitchRequestedAt.Valid
	if !run.CredentialSwitchOpID.Valid || uuid.UUID(run.CredentialSwitchOpID.Bytes) != op {
		if stampPending && run.CredentialSwitchGeneration.Valid && run.CredentialSwitchGeneration.Int64 == gen {
			return CredentialSwitchApplyResult{Outcome: ApplyOutcomeSuperseded}, nil
		}
		return CredentialSwitchApplyResult{Outcome: ApplyOutcomeNotApplied, Reason: applyReasonNoPendingSwitch}, nil
	}

	// Step 3: fresh apply.
	if !run.CredentialSwitchGeneration.Valid || run.CredentialSwitchGeneration.Int64 != gen {
		return CredentialSwitchApplyResult{Outcome: ApplyOutcomeNotApplied, Reason: applyReasonGenerationMismatch}, nil
	}
	choiceID, choice, reason, err := s.chooseApplySecret(ctx, wkr, run)
	if err != nil {
		return CredentialSwitchApplyResult{}, err
	}
	if reason != "" {
		return CredentialSwitchApplyResult{Outcome: ApplyOutcomeNotApplied, Reason: reason}, nil
	}
	sec, err := qtx.LockAnthropicSecretForApply(ctx, store.LockAnthropicSecretForApplyParams{ID: choiceID, UserID: run.UserID})
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialSwitchApplyResult{Outcome: ApplyOutcomeNotApplied, Reason: applyReasonCredentialGone}, nil
	}
	if err != nil {
		return CredentialSwitchApplyResult{}, err
	}
	if s.afterApplySecretLock != nil {
		s.afterApplySecretLock()
	}
	if sec.DisabledAt.Valid {
		return CredentialSwitchApplyResult{Outcome: ApplyOutcomeNotApplied, Reason: applyReasonCredentialDisabled}, nil
	}
	if sec.AnthropicRejectedAt.Valid {
		return CredentialSwitchApplyResult{Outcome: ApplyOutcomeNotApplied, Reason: applyReasonCredentialRejected}, nil
	}
	tok, err := s.openAnthropicSealed(run.UserID, sec.SealedWith, sec.Ciphertext)
	if err != nil {
		switch {
		case errors.Is(err, errVaultLocked):
			return CredentialSwitchApplyResult{Outcome: ApplyOutcomeNotApplied, Reason: applyReasonVaultLocked}, nil
		case errors.Is(err, errCredentialUnavailable):
			return CredentialSwitchApplyResult{Outcome: ApplyOutcomeNotApplied, Reason: applyReasonCredentialNotDecrypt}, nil
		}
		return CredentialSwitchApplyResult{}, err
	}

	// The writes, in order. The message seq is computed first because the child row carries it.
	maxSeq, err := qtx.MaxRunMessageSeq(ctx, runID)
	if err != nil {
		return CredentialSwitchApplyResult{}, fmt.Errorf("credential switch apply: max seq: %w", err)
	}
	seq := max(run.LastSeq, maxSeq) + 1

	n, err := qtx.SetRunAnthropicSecret(ctx, store.SetRunAnthropicSecretParams{
		AnthropicSecretID:     pgconv.UUID(sec.ID),
		AnthropicSecretLabel:  pgconv.TextOrNull(sec.Label),
		AnthropicSelectReason: pgconv.TextOrNull(choice.reason),
		AnthropicHeadroomPct:  headroomParam(choice),
		ID:                    run.ID,
		UserID:                run.UserID,
	})
	if err != nil {
		return CredentialSwitchApplyResult{}, fmt.Errorf("credential switch apply: record credential: %w", err)
	}
	if n == 0 {
		return CredentialSwitchApplyResult{}, errRunVanished
	}
	epoch, err := qtx.InsertRunCredentialSwitchEpoch(ctx, store.InsertRunCredentialSwitchEpochParams{
		RunID:               runID,
		ClaimGeneration:     gen,
		OpID:                op,
		WorkerID:            pgconv.UUID(wkr.ID),
		SecretID:            pgconv.UUID(sec.ID),
		SecretEnablementRev: sec.EnablementRev,
		Label:               pgconv.TextOrNull(sec.Label),
		SelectReason:        pgconv.TextOrNull(choice.reason),
		MessageSeq:          seq,
		OldCumulative:       oldCumulative,
	})
	if err != nil {
		return CredentialSwitchApplyResult{}, fmt.Errorf("credential switch apply: journal epoch: %w", err)
	}
	cleared, err := qtx.ClearCredentialSwitchStampByOp(ctx, store.ClearCredentialSwitchStampByOpParams{ID: runID, OpID: pgconv.UUID(op)})
	if err != nil {
		return CredentialSwitchApplyResult{}, fmt.Errorf("credential switch apply: clear stamp: %w", err)
	}
	if cleared != 1 {
		// Impossible under the run lock (the op was just read as pending); treat as superseded and
		// let the deferred rollback undo the writes above.
		return CredentialSwitchApplyResult{Outcome: ApplyOutcomeNotApplied, Reason: applyReasonStampCleared}, nil
	}
	payload, err := json.Marshal(credentialSwitchMessagePayload{
		Label:        labelPtr(sec.Label),
		SelectReason: choice.reason,
		SecretID:     sec.ID.String(),
		Ordinal:      epoch.Ordinal,
	})
	if err != nil {
		return CredentialSwitchApplyResult{}, fmt.Errorf("credential switch apply: marshal message: %w", err)
	}
	ins, err := qtx.InsertRunMessage(ctx, store.InsertRunMessageParams{
		RunID:           runID,
		Seq:             seq,
		Kind:            credentialSwitchMessageKind,
		Payload:         payload,
		ClaimGeneration: pgconv.Int8Ptr(&gen),
	})
	if err != nil {
		return CredentialSwitchApplyResult{}, fmt.Errorf("credential switch apply: insert message: %w", err)
	}
	if !ins.Inserted {
		return CredentialSwitchApplyResult{}, fmt.Errorf("credential switch apply: message seq %d already occupied", seq)
	}
	if _, err := qtx.UpdateRunLastSeq(ctx, store.UpdateRunLastSeqParams{ID: runID, Seq: seq, ClaimGeneration: pgconv.Int8Ptr(&gen)}); err != nil {
		return CredentialSwitchApplyResult{}, fmt.Errorf("credential switch apply: advance last_seq: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CredentialSwitchApplyResult{}, err
	}

	// After the commit only: tell the web the message exists and the switch chip is gone.
	if s.bcast != nil {
		s.bcast.PublishMessage(runID, seq, credentialSwitchMessageKind, "", "", "", payload, s.now())
		s.bcast.PublishState(runID, run.Status)
	}
	return CredentialSwitchApplyResult{
		Outcome: ApplyOutcomeApplied, Token: string(tok), Ordinal: epoch.Ordinal, MessageSeq: seq,
	}, nil
}

// replayCredentialSwitchApply answers a retry of an apply that already committed: the journaled
// ordinal and message seq with the token bytes, provided the token the epoch recorded is still the
// same enabled, unrejected row. Anything else is refused_committed; nothing is written either way.
func (s *Service) replayCredentialSwitchApply(ctx context.Context, qtx *store.Queries, run store.Run, prior store.RunCredentialSwitchEpoch) (CredentialSwitchApplyResult, error) {
	refuse := func(reason string) (CredentialSwitchApplyResult, error) {
		return CredentialSwitchApplyResult{Outcome: ApplyOutcomeRefusedCommitted, Reason: reason}, nil
	}
	if !prior.SecretID.Valid {
		return refuse(applyReasonCredentialGone)
	}
	sec, err := qtx.LockAnthropicSecretForApply(ctx, store.LockAnthropicSecretForApplyParams{ID: uuid.UUID(prior.SecretID.Bytes), UserID: run.UserID})
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse(applyReasonCredentialGone)
	}
	if err != nil {
		return CredentialSwitchApplyResult{}, err
	}
	if s.afterApplySecretLock != nil {
		s.afterApplySecretLock()
	}
	switch {
	case sec.EnablementRev != prior.SecretEnablementRev:
		return refuse(applyReasonCredentialChanged)
	case sec.DisabledAt.Valid:
		return refuse(applyReasonCredentialDisabled)
	case sec.AnthropicRejectedAt.Valid:
		return refuse(applyReasonCredentialRejected)
	}
	tok, err := s.openAnthropicSealed(run.UserID, sec.SealedWith, sec.Ciphertext)
	if err != nil {
		switch {
		case errors.Is(err, errVaultLocked):
			return refuse(applyReasonVaultLocked)
		case errors.Is(err, errCredentialUnavailable):
			return refuse(applyReasonCredentialNotDecrypt)
		}
		return CredentialSwitchApplyResult{}, err
	}
	return CredentialSwitchApplyResult{
		Outcome: ApplyOutcomeApplied, Token: string(tok), Ordinal: prior.Ordinal, MessageSeq: prior.MessageSeq, Replay: true,
	}, nil
}

// chooseApplySecret picks WHICH Anthropic token id a fresh apply spends, by the same ladder the
// next claim would use (claimSecretID: self_improve judge binding, run override, worker bind mode,
// weekly reserve gate). It chooses an id only: nothing is opened here, because the token is read
// from the row the caller then locks inside the apply transaction. A non-empty reason is a
// not_applied outcome; an error is an infrastructure failure the caller returns as-is.
func (s *Service) chooseApplySecret(ctx context.Context, wkr store.Worker, run store.Run) (uuid.UUID, secretChoice, string, error) {
	gate, err := s.reserveGateFor(ctx, run)
	if err != nil {
		return uuid.Nil, secretChoice{}, "", err
	}
	ctx = gate.attach(ctx)
	choice, err := s.claimSecretID(ctx, wkr, run)
	switch {
	case err == nil:
	case errors.Is(err, errAutoPoolEmpty):
		return uuid.Nil, secretChoice{}, applyReasonPoolEmpty, nil
	case errors.Is(err, errReserveHeld):
		return uuid.Nil, secretChoice{}, applyReasonReserveHeld, nil
	case errors.Is(err, errCredentialDisabled):
		return uuid.Nil, secretChoice{}, applyReasonCredentialDisabled, nil
	case errors.Is(err, errNoModelCredential):
		return uuid.Nil, secretChoice{}, applyReasonNoCredential, nil
	case errors.Is(err, errCredentialUnavailable):
		return uuid.Nil, secretChoice{}, applyReasonCredentialUnavail, nil
	default:
		return uuid.Nil, secretChoice{}, "", err
	}
	if choice.secretID != nil {
		return *choice.secretID, choice, "", nil
	}
	// A nil id is the owner's default credential.
	meta, err := s.q.GetDefaultUserSecretMeta(ctx, store.GetDefaultUserSecretMetaParams{UserID: run.UserID, Kind: store.KindAnthropicToken})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, secretChoice{}, applyReasonNoCredential, nil
	}
	if err != nil {
		return uuid.Nil, secretChoice{}, "", fmt.Errorf("credential switch apply: default credential: %w", err)
	}
	return meta.ID, choice, "", nil
}

// labelPtr is the message payload's label: JSON null for an unlabelled token.
func labelPtr(label string) *string {
	if strings.TrimSpace(label) == "" {
		return nil
	}
	return &label
}
