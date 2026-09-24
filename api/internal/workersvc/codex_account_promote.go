package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// codex_account_promote.go is PRD #1590 M3 (D3) and M4 (D5): the
// promote_codex_account_available sweeper pass. A run held in recovery_wait with cause
// codex_account_unavailable has no timer (both timer promoters skip the cause); it leaves the
// hold only through this pass (or an owner cancel), decided on its alias and account read under
// lock.
//
// Lock order is run -> alias -> account, one transaction per run. The re-login writer
// (CodexReconciler.reconcileTuple: RefreshCodexAccountLogin, then LinkCodexCredentialState)
// takes the account before the alias, so both alias and account FOR SHARE locks are NOWAIT: a
// held row is SQLSTATE 55P03, the transaction rolls back with the run still held, and the next
// tick retries. The promoter never waits on either row, so it cannot close a deadlock cycle.
//
// Per run, in order (D3 steps plus D5):
//  1. a held run whose alias was deleted (codex_secret_id NULL, frozen material revision set)
//     is failed credential_unavailable, before any alias or account lock;
//  2. alias lock; an alias that is not linked (a staging or failed re-login) leaves the run held
//     with nothing written, and a linked alias with no account is failed as incoherent;
//  3. account lock;
//  4. D5 re-admission (ReadmitRunCodexBinding) when the alias material is ahead of the run's,
//     plus the feed line; every D5 fence is in that statement's WHERE, and the frozen key must
//     equal the Go encoding of the locked account's tuple, so the SQL identity check agrees
//     with classifyCodexAccountHold's;
//  5. the GetRunCodexAuthContext re-read, classified by classifyCodexAccountHold into promote
//     (the unchanged evalCodexReleasePredicate passes), stay held, or terminal.
// A re-admission commits only together with its promotion. If a re-admitted run classifies as
// anything else, promoteCodexAccountRun rolls the whole transaction back (no re-admission, no
// feed line, nothing counted or published) and decides the run again in a fresh transaction
// with re-admission disabled; that second decision is the one committed (see
// promoteCodexAccountRun).

// codexAccountPromoteStore is the pass's page read. *store.Queries satisfies it; a fake Store
// that does not is reported as errCodexStoreUnavailable, like the park pass.
type codexAccountPromoteStore interface {
	ListCodexAccountWaitRunsPage(ctx context.Context, arg store.ListCodexAccountWaitRunsPageParams) ([]uuid.UUID, error)
}

// codexPromoteQueries is the statement surface of one per-run promotion transaction.
type codexPromoteQueries interface {
	LockCodexAccountWaitRunForUpdate(ctx context.Context, id uuid.UUID) (store.Run, error)
	LockCodexAliasForShareNowait(ctx context.Context, arg store.LockCodexAliasForShareNowaitParams) (store.LockCodexAliasForShareNowaitRow, error)
	LockCodexAccountForShareNowait(ctx context.Context, arg store.LockCodexAccountForShareNowaitParams) (uuid.UUID, error)
	ReadmitRunCodexBinding(ctx context.Context, arg store.ReadmitRunCodexBindingParams) (int64, error)
	InsertCodexReadmitRunMessage(ctx context.Context, arg store.InsertCodexReadmitRunMessageParams) (int32, error)
	GetRunCodexAuthContext(ctx context.Context, id uuid.UUID) (store.GetRunCodexAuthContextRow, error)
	PromoteCodexAccountWaitRun(ctx context.Context, id uuid.UUID) (int64, error)
	FailCodexAccountWaitRun(ctx context.Context, arg store.FailCodexAccountWaitRunParams) (int64, error)
}

// codexPromoteTestHooks are LiveDB race seams (nil in production).
type codexPromoteTestHooks struct {
	// afterList runs for each listed run before its transaction opens.
	afterList func(ctx context.Context, runID uuid.UUID)
	// afterAliasLock runs once the alias FOR SHARE lock is held, before the account lock.
	afterAliasLock func(ctx context.Context, runID uuid.UUID)
	// afterAccountLock runs once the account FOR SHARE lock is held, before re-admission.
	afterAccountLock func(ctx context.Context, runID uuid.UUID)
	// wrapQueries wraps each per-run transaction's statement surface.
	wrapQueries func(q codexPromoteQueries) codexPromoteQueries
}

// codexAccountPromoteResult is one page's tally.
type codexAccountPromoteResult struct {
	Promoted   int64 // held runs returned to queued
	Readmitted int64 // of those, runs re-admitted (D5) in the same, committed transaction
	Failed     int64 // held runs failed credential_unavailable (deleted alias, binding change)
}

// codexHoldOutcome is what one held run's transaction decided.
type codexHoldOutcome int

const (
	codexHoldStay    codexHoldOutcome = iota // leave the run held; nothing is written
	codexHoldPromote                         // recovery_wait -> queued
	codexHoldFail                            // terminal credential_unavailable
)

// codexHoldDecision is promoteCodexAccountRunTx's result. reason is the terminal cause for
// codexHoldFail; readmit is the feed line to broadcast after commit, when the run was re-admitted.
// promoteCodexAccountRun only returns a decision with readmit set when its outcome is
// codexHoldPromote.
type codexHoldDecision struct {
	outcome codexHoldOutcome
	reason  string
	readmit *codexReadmitLine
}

// codexReadmitLine is a committed re-admission feed line, broadcast after the commit.
type codexReadmitLine struct {
	seq     int32
	payload []byte
}

// codexReadmitStatusPayload is the D5 feed status line: the alias label (the run's own snapshot)
// and the material-revision change. It carries no token, account identity or secret material.
type codexReadmitStatusPayload struct {
	Text                 string `json:"text"`
	Event                string `json:"event"`
	AliasLabel           string `json:"alias_label"`
	FromMaterialRevision int64  `json:"from_material_revision"`
	ToMaterialRevision   int64  `json:"to_material_revision"`
}

// codexReadmitEvent is the feed status line's event discriminator.
const codexReadmitEvent = "codex_binding_readmitted"

// codexAliasLabel renders the run's snapshotted alias label for a feed line or failure reason.
func codexAliasLabel(run store.Run) string {
	if run.CodexSecretLabel.Valid && run.CodexSecretLabel.String != "" {
		return fmt.Sprintf("%q", run.CodexSecretLabel.String)
	}
	return "(unlabelled)"
}

// codexReadmitPayload builds the feed line for a re-admission from old to new material revision.
func codexReadmitPayload(run store.Run, from, to int64) ([]byte, error) {
	label := ""
	if run.CodexSecretLabel.Valid {
		label = run.CodexSecretLabel.String
	}
	return json.Marshal(codexReadmitStatusPayload{
		Text: fmt.Sprintf("Codex login %s was re-logged in to the same account; this run was re-admitted "+
			"(login material revision %d to %d) and will resume", codexAliasLabel(run), from, to),
		Event:                codexReadmitEvent,
		AliasLabel:           label,
		FromMaterialRevision: from,
		ToMaterialRevision:   to,
	})
}

// classifyCodexAccountHold decides a held codex_account_unavailable run from its authority
// context, read under the run, alias and account locks AFTER any re-admission. The caller only
// reaches it with a linked alias whose account row it holds FOR SHARE.
//
// Terminal (codexHoldFail) is reserved for a DEFINITE binding mismatch on that linked alias,
// a state no later account or alias transition can undo without violating the write-once freeze:
//   - the alias kind contradicts the run's auth mode, or the run is no longer a subscription run;
//   - the run has no frozen identity (nothing freezes it after create, so it can never pass);
//   - the linked account's identity tuple differs from the frozen one (an account's tuple is
//     immutable, so the alias now names a different account);
//   - the account's credential_revision differs from the frozen one. Nothing bumps
//     credential_revision today (it stays at its DEFAULT 0), so this is defence in depth: a
//     future revocation that bumps it ends the hold, and D5 never advances it.
//
// Everything else stays held (codexHoldStay), because it can still resolve on its own:
//   - coord_state quarantined with the same identity and revision (awaiting recovery or re-login);
//   - coord_state in_progress, or a material revision still ahead of the run's (re-admission did
//     not apply: a lease is live, or a concurrent material change made its CAS match 0 rows);
//   - no account columns in the read (ErrCodexAliasAccountMissing). Unreachable from the
//     promoter, which reads under the account FOR SHARE lock of the alias's linked account;
//     kept fail-closed (held, no token) rather than terminal because it is not a definite
//     mismatch;
//   - any other predicate refusal.
//
// codexHoldPromote requires the unchanged evalCodexReleasePredicate to pass.
func classifyCodexAccountHold(in codexReleaseInputs) (codexHoldOutcome, error) {
	if err := codexCheckKindMode(in.authMode, in.boundKind); err != nil {
		return codexHoldFail, err
	}
	if in.authMode != codexAuthModeSubscription {
		return codexHoldFail, ErrCodexKindModeMismatch
	}
	if !in.frozenAccountKeyValid {
		return codexHoldFail, ErrCodexAccountKeyUnfrozen
	}
	if !in.currentProviderUserIDValid || !in.currentWorkspaceAccountIDValid || !in.currentCredentialRevValid {
		return codexHoldStay, ErrCodexAliasAccountMissing
	}
	if err := codexCheckAccountTuple(in); err != nil {
		if errors.Is(err, ErrCodexAccountTupleMismatch) {
			return codexHoldFail, err
		}
		return codexHoldStay, err // an encoding fault is not a definite mismatch
	}
	if !in.frozenAccountRevValid || in.frozenAccountRev != in.currentCredentialRev {
		return codexHoldFail, ErrCodexAccountRevisionStale
	}
	if err := evalCodexReleasePredicate(in); err != nil {
		return codexHoldStay, err
	}
	return codexHoldPromote, nil
}

// promoteCodexAccountAvailable runs one page of the promote_codex_account_available pass. Each
// listed run is decided in its own transaction (promoteCodexAccountRun); a per-run error is
// logged, the page continues, and the errors are returned joined so Sweep logs the tick as
// failed while keeping the tally. On a page-read error the cursor is left in place.
func (s *Service) promoteCodexAccountAvailable(ctx context.Context) (codexAccountPromoteResult, error) {
	var res codexAccountPromoteResult
	q, ok := s.q.(codexAccountPromoteStore)
	if !ok {
		return res, errCodexStoreUnavailable
	}
	s.codexPromote.mu.Lock()
	defer s.codexPromote.mu.Unlock()
	ids, err := q.ListCodexAccountWaitRunsPage(ctx, store.ListCodexAccountWaitRunsPageParams{
		AfterID: s.codexPromote.after,
		PageCap: s.codexPromote.pageCap(),
	})
	if err != nil {
		return res, fmt.Errorf("list codex account wait runs: %w", err)
	}
	last := uuid.Nil
	if len(ids) > 0 {
		last = ids[len(ids)-1]
	}
	s.codexPromote.advance(int64(len(ids)), last)

	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		if h := s.codexPromoteHooks; h != nil && h.afterList != nil {
			h.afterList(ctx, id)
		}
		d, err := s.promoteCodexAccountRun(ctx, id)
		if err != nil {
			slog.Warn("sweeper: promote codex account wait run failed", "run", id, "error", err)
			errs = append(errs, fmt.Errorf("run %s: %w", id, err))
			continue
		}
		// promoteCodexAccountRun returns readmit only on a committed promotion.
		if d.readmit != nil {
			res.Readmitted++
			if s.bcast != nil {
				s.bcast.PublishMessage(id, d.readmit.seq, "status", "", "", "", d.readmit.payload, s.now())
			}
		}
		switch d.outcome {
		case codexHoldPromote:
			res.Promoted++
			s.publishSwept(id, "queued")
		case codexHoldFail:
			res.Failed++
			slog.Info("sweeper: codex account hold failed terminally", "run", id, "reason", d.reason)
			s.publishSwept(id, "failed")
		}
	}
	return res, errors.Join(errs...)
}

// errCodexReadmitWithoutPromote reports a transaction that re-admitted a run which then did not
// promote. promoteCodexAccountRunOnce rolls it back; it never leaves promoteCodexAccountRun.
var errCodexReadmitWithoutPromote = errors.New("codex re-admission did not promote")

// promoteCodexAccountRun decides one held run and commits only a promotion or a terminal
// failure; a run left held is untouched.
//
// A re-admission commits only with its promotion. When a transaction re-admits the run and then
// classifies it as anything else (terminal or stay), the whole transaction is rolled back: the
// re-admission and its feed line never land, and nothing is counted or published for them. The
// run is then decided again in a fresh transaction with re-admission disabled, from its
// unrelaxed binding. Re-admission only moves the material revision, and classifyCodexAccountHold
// checks kind, mode, identity and credential revision before it, so the second decision is the
// same terminal failure (committed without the re-admission) or a stay (the run remains held,
// nothing written).
func (s *Service) promoteCodexAccountRun(ctx context.Context, runID uuid.UUID) (codexHoldDecision, error) {
	d, err := s.promoteCodexAccountRunOnce(ctx, runID, true)
	if !errors.Is(err, errCodexReadmitWithoutPromote) {
		return d, err
	}
	slog.Warn("sweeper: codex re-admission did not promote; rolled back and deciding without it", "run", runID)
	return s.promoteCodexAccountRunOnce(ctx, runID, false)
}

// promoteCodexAccountRunOnce is one transaction of promoteCodexAccountRun: it commits a
// promotion or a terminal failure, and rolls back a stay or a re-admission that did not promote
// (errCodexReadmitWithoutPromote).
func (s *Service) promoteCodexAccountRunOnce(ctx context.Context, runID uuid.UUID, allowReadmit bool) (codexHoldDecision, error) {
	if s.txBeginner == nil {
		return codexHoldDecision{}, errClaimRecoveryNoTx
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return codexHoldDecision{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var q codexPromoteQueries = store.New(tx)
	if h := s.codexPromoteHooks; h != nil && h.wrapQueries != nil {
		q = h.wrapQueries(q)
	}
	d, err := s.promoteCodexAccountRunTx(ctx, q, runID, allowReadmit)
	if err != nil {
		return codexHoldDecision{}, err
	}
	if d.readmit != nil && d.outcome != codexHoldPromote {
		return codexHoldDecision{}, errCodexReadmitWithoutPromote
	}
	if d.outcome == codexHoldStay {
		return codexHoldDecision{}, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return codexHoldDecision{}, err
	}
	return d, nil
}

// promoteCodexAccountRunTx is promoteCodexAccountRunOnce's body over the open transaction (see
// the file comment for the steps). allowReadmit=false skips D5 re-admission. A result may carry
// writes the caller must roll back: a stay after a re-admission, or a re-admission that did not
// promote.
func (s *Service) promoteCodexAccountRunTx(ctx context.Context, q codexPromoteQueries, runID uuid.UUID, allowReadmit bool) (codexHoldDecision, error) {
	stay := codexHoldDecision{}
	run, err := q.LockCodexAccountWaitRunForUpdate(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return stay, nil // cancelled or promoted since the list, or locked by another tx
	}
	if err != nil {
		return stay, err
	}
	if !run.CodexSecretID.Valid {
		if !run.CodexMaterialRevision.Valid {
			// Never bound. Unreachable: a run enters this cause only through a park whose gate
			// joins its alias (the sweeper park, or the claim-time park of a bound run), and the
			// freeze writes codex_secret_id and codex_material_revision together. Fail-closed:
			// the run stays held, and with no binding no token can be released for it.
			return stay, nil
		}
		// D5: the alias was deleted after the park (the FK nulled codex_secret_id). Decided
		// on the locked run row alone, before any alias or account lock.
		return s.failCodexAccountHold(ctx, q, run, fmt.Sprintf(
			"%v: the Codex login %s bound to this run was deleted while the run waited for its account",
			errCredentialUnavailable, codexAliasLabel(run)))
	}
	alias, err := q.LockCodexAliasForShareNowait(ctx, store.LockCodexAliasForShareNowaitParams{
		UserSecretID: uuid.UUID(run.CodexSecretID.Bytes), UserID: run.UserID,
	})
	// A missing state row (pgx.ErrNoRows) stays held. Unreachable: no statement deletes a
	// codex_credential_state row except the cascade from its alias, and deleting the alias also
	// nulls the run's codex_secret_id (handled above). Fail-closed: held, no token.
	if held, err := codexPromoteLockOutcome(err); held || err != nil {
		return stay, err
	}
	if h := s.codexPromoteHooks; h != nil && h.afterAliasLock != nil {
		h.afterAliasLock(ctx, runID)
	}
	if alias.Status != "linked" {
		return stay, nil // re-login staging or failed: identity unknown, stays held
	}
	if !alias.ProviderAccountID.Valid {
		// Incoherent alias (PRD #1590 D1, terminal): linked, but to no account. No writer
		// produces it (a link sets the status and the account together, a re-login PATCH
		// clears both, and deleting the account demotes the alias to failed through the
		// orphan trigger), so it can only come from a direct write, and no later transition
		// repairs it.
		return s.failCodexAccountHold(ctx, q, run, fmt.Sprintf(
			"%v: the Codex login %s bound to this run changed while the run waited for its account: %v",
			errCredentialUnavailable, codexAliasLabel(run), ErrCodexAliasAccountMissing))
	}
	_, err = q.LockCodexAccountForShareNowait(ctx, store.LockCodexAccountForShareNowaitParams{
		ID: uuid.UUID(alias.ProviderAccountID.Bytes), UserID: run.UserID,
	})
	// A missing account row stays held. Unreachable: the owner-scoped foreign key keeps
	// provider_account_id pointing at an existing account (a deletion nulls it and the orphan
	// trigger demotes the alias to failed). Fail-closed: held, no token.
	if held, err := codexPromoteLockOutcome(err); held || err != nil {
		return stay, err
	}
	if h := s.codexPromoteHooks; h != nil && h.afterAccountLock != nil {
		h.afterAccountLock(ctx, runID)
	}

	var readmit *codexReadmitLine
	if allowReadmit && run.CodexMaterialRevision.Valid && alias.MaterialRevision > run.CodexMaterialRevision.Int64 {
		readmit, err = s.readmitCodexAccountHold(ctx, q, run, alias.MaterialRevision)
		if err != nil {
			return stay, err
		}
	}
	// Every outcome carries the re-admission, so promoteCodexAccountRunOnce can see one that
	// did not promote and roll it back.
	d, err := s.decideCodexAccountHold(ctx, q, run)
	d.readmit = readmit
	return d, err
}

// decideCodexAccountHold is step 5: the GetRunCodexAuthContext re-read under the locks,
// classified and applied (promote, terminal failure, or stay with nothing written).
func (s *Service) decideCodexAccountHold(ctx context.Context, q codexPromoteQueries, run store.Run) (codexHoldDecision, error) {
	stay := codexHoldDecision{}
	row, err := q.GetRunCodexAuthContext(ctx, run.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return stay, nil
	}
	if err != nil {
		return stay, err
	}
	outcome, cause := classifyCodexAccountHold(codexReleaseInputsFromAuthRow(row))
	switch outcome {
	case codexHoldPromote:
		n, err := q.PromoteCodexAccountWaitRun(ctx, run.ID)
		if err != nil || n != 1 {
			return stay, err
		}
		return codexHoldDecision{outcome: codexHoldPromote}, nil
	case codexHoldFail:
		return s.failCodexAccountHold(ctx, q, run, fmt.Sprintf(
			"%v: the Codex login %s bound to this run changed while the run waited for its account: %v",
			errCredentialUnavailable, codexAliasLabel(run), cause))
	default:
		return stay, nil
	}
}

// readmitCodexAccountHold runs D5's ReadmitRunCodexBinding from the run's frozen material
// revision to the alias's locked one, and on success writes the feed line in the same
// transaction. The statement's account_key is codexAccountKey over the locked account's tuple
// (read here, under the caller's locks), the exact text codexCheckAccountTuple compares, so a
// frozen key the Go check would reject is never re-admitted. It returns nil (no error) when the
// read finds no account tuple or the CAS matched 0 rows: a fence failed or the material moved,
// and the caller's re-read decides.
func (s *Service) readmitCodexAccountHold(ctx context.Context, q codexPromoteQueries, run store.Run, aliasMaterial int64) (*codexReadmitLine, error) {
	cur, err := q.GetRunCodexAuthContext(ctx, run.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !cur.ProviderUserID.Valid || !cur.WorkspaceAccountID.Valid {
		return nil, nil
	}
	key, err := codexAccountKey(cur.ProviderUserID.String, cur.WorkspaceAccountID.String)
	if err != nil {
		return nil, fmt.Errorf("encode readmit account key: %w", err)
	}
	from := run.CodexMaterialRevision.Int64
	n, err := q.ReadmitRunCodexBinding(ctx, store.ReadmitRunCodexBindingParams{
		ID: run.ID, SecretID: uuid.UUID(run.CodexSecretID.Bytes), AccountKey: key,
		OldMaterialRevision: from, NewMaterialRevision: aliasMaterial,
	})
	if err != nil || n != 1 {
		return nil, err
	}
	payload, err := codexReadmitPayload(run, from, aliasMaterial)
	if err != nil {
		return nil, fmt.Errorf("marshal readmit feed line: %w", err)
	}
	seq, err := q.InsertCodexReadmitRunMessage(ctx, store.InsertCodexReadmitRunMessageParams{RunID: run.ID, Payload: payload})
	if err != nil {
		// Mandatory: a re-admission never commits without its feed line. The caller rolls back.
		return nil, fmt.Errorf("insert readmit feed line: %w", err)
	}
	return &codexReadmitLine{seq: seq, payload: payload}, nil
}

// failCodexAccountHold ends the held run credential_unavailable, fenced on the hold. A 0-row
// result (the run left the hold) is not an error; the run is left as it is.
func (s *Service) failCodexAccountHold(ctx context.Context, q codexPromoteQueries, run store.Run, reason string) (codexHoldDecision, error) {
	n, err := q.FailCodexAccountWaitRun(ctx, store.FailCodexAccountWaitRunParams{
		ID: run.ID, FailureReason: pgconv.TextOrNull(reason), FailOrigin: pgconv.TextOrNull("credential_unavailable"),
	})
	if err != nil || n != 1 {
		return codexHoldDecision{}, err
	}
	return codexHoldDecision{outcome: codexHoldFail, reason: reason}, nil
}

// codexPromoteLockOutcome maps a NOWAIT lock result: a missing row or a 55P03 leaves the run
// held for a later tick (held=true, no error); any other error is returned.
func codexPromoteLockOutcome(err error) (held bool, _ error) {
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, pgx.ErrNoRows), isLockNotAvailable(err):
		return true, nil
	default:
		return false, err
	}
}
