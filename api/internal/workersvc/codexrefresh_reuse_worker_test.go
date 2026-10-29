package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type reuseWorkerStore struct {
	*rejFakeStore
	memory                                                             *memVaultStore
	intent                                                             store.CodexRefreshIntent
	accountReads, intentReads, admissions, writes, promotes, terminals int
	live                                                               bool
	writeErr                                                           error
	fenceLost                                                          bool
	lastWrite                                                          store.SetCodexRecoverySlotParams
	terminalCtxErr                                                     error
	terminalBudget                                                     time.Duration
}

func (f *reuseWorkerStore) GetCodexProviderAccountByID(context.Context, store.GetCodexProviderAccountByIDParams) (store.CodexProviderAccount, error) {
	f.accountReads++
	return f.acct, nil
}

func (f *reuseWorkerStore) ListUnresolvedCodexRefreshIntents(context.Context, store.ListUnresolvedCodexRefreshIntentsParams) ([]store.CodexRefreshIntent, error) {
	f.intentReads++
	if f.intent.State != codexIntentRotating {
		return nil, nil
	}
	return []store.CodexRefreshIntent{f.intent}, nil
}

func (f *reuseWorkerStore) CountEnabledLinkedAliasesForCodexAccount(context.Context, store.CountEnabledLinkedAliasesForCodexAccountParams) (int64, error) {
	if f.live {
		return 1, nil
	}
	return 0, nil
}

func (f *reuseWorkerStore) SetCodexRecoverySlot(ctx context.Context, arg store.SetCodexRecoverySlotParams) (int64, error) {
	f.writes++
	f.lastWrite = arg
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.fenceLost || arg.Gen != f.acct.Generation || !f.acct.CoordOperationID.Valid || uuid.UUID(f.acct.CoordOperationID.Bytes) != arg.Op {
		return 0, nil
	}
	f.acct.RecoverySealed = arg.Sealed
	f.acct.RecoverySealedWith = arg.RecoverySealedWith
	f.acct.RecoveryGeneration = pgtype.Int8{Int64: arg.Gen, Valid: true}
	return 1, nil
}

func (f *reuseWorkerStore) SetCodexRefreshIntentState(ctx context.Context, arg store.SetCodexRefreshIntentStateParams) (int64, error) {
	f.terminals++
	f.terminalCtxErr = ctx.Err()
	if d, ok := ctx.Deadline(); ok {
		f.terminalBudget = time.Until(d)
	}
	f.intent.State = arg.State
	return 1, nil
}

func (f *reuseWorkerStore) SetCodexRefreshReuseUnrecoverable(ctx context.Context, arg store.SetCodexRefreshReuseUnrecoverableParams) (int64, error) {
	if f.intent.State != codexIntentRotating || !f.intent.ProviderErrorAt.Valid || !f.intent.ReuseAttemptedAt.Valid ||
		arg.UserID != f.acct.UserID || arg.AccountID != f.acct.ID || arg.OperationID != f.intent.OperationID ||
		arg.FromGeneration != f.acct.Generation || f.intent.FromGeneration != arg.FromGeneration ||
		f.acct.CoordState != codexCoordQuarantined || !f.acct.CoordOperationID.Valid ||
		uuid.UUID(f.acct.CoordOperationID.Bytes) != arg.OperationID || len(f.acct.RecoverySealed) != 0 || f.acct.ReauthRequired {
		return 0, nil
	}
	return f.SetCodexRefreshIntentState(ctx, store.SetCodexRefreshIntentStateParams{
		UserID: arg.UserID, OperationID: arg.OperationID, State: codexIntentUnrecoverable,
	})
}

func (f *reuseWorkerStore) SetCodexRefreshIntentStateFenced(_ context.Context, arg store.SetCodexRefreshIntentStateFencedParams) (int64, error) {
	f.intent.State = arg.State
	return 1, nil
}

func (f *reuseWorkerStore) PromoteCodexRecovery(_ context.Context, arg store.PromoteCodexRecoveryParams) (int64, error) {
	f.promotes++
	f.acct.Generation++
	f.acct.SealedLogin, f.acct.SealedWith = arg.Sealed, arg.SealedWith
	f.acct.RecoverySealed = nil
	f.acct.CoordState = codexCoordIdle
	return f.acct.Generation, nil
}

type reuseWorkerProvider struct {
	t                  *testing.T
	st                 *reuseWorkerStore
	calls, discoveries int
	result             codexauth.RefreshResult
	err                error
	onRefresh          func()
}

func (p *reuseWorkerProvider) Refresh(ctx context.Context, token string) (codexauth.RefreshResult, error) {
	p.calls++
	if p.st.admissions != 1 || token != "CANARY_REFRESH" {
		p.t.Fatal("exchange did not follow admission with original refresh token")
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 6*time.Second {
		p.t.Fatal("reuse provider context must have at most six seconds")
	}
	if p.onRefresh != nil {
		p.onRefresh()
	}
	return p.result, p.err
}

func (p *reuseWorkerProvider) DiscoverIdentity(_ context.Context, token string) (codexauth.Identity, error) {
	p.discoveries++
	if token != "reuse-access" {
		p.t.Fatal("promotion used stale access token")
	}
	return codexauth.Identity{ProviderUserID: p.st.acct.ProviderUserID, WorkspaceAccountID: p.st.acct.WorkspaceAccountID}, nil
}

func newReuseWorker(t *testing.T) (*Service, *reuseWorkerStore, *reuseWorkerProvider, codexRefreshReuseAdmitter) {
	t.Helper()
	h := newRejHarness(t, nil, nil)
	now := time.Now()
	op := uuid.New()
	h.st.acct.CoordState = codexCoordQuarantined
	h.st.acct.CoordOperationID = pgtype.UUID{Bytes: op, Valid: true}
	st := &reuseWorkerStore{rejFakeStore: h.st, live: true, intent: store.CodexRefreshIntent{
		OperationID: op, UserID: h.userID, ProviderAccountID: h.accountID, FromGeneration: h.st.acct.Generation,
		State: codexIntentRotating, CreatedAt: pgconv.Time(now.Add(-time.Minute)), ProviderErrorAt: pgconv.Time(now),
	}}
	p := &reuseWorkerProvider{t: t, st: st, result: codexauth.RefreshResult{AccessToken: "reuse-access"}}
	h.svc.q, h.svc.codexRefresh = st, p
	h.svc.background = nil // each test explicitly owns any background retry
	h.svc.now = func() time.Time { return now }
	memory := newMemVaultStore()
	h.svc.vlt = newMemoryVault(h.svc.box, memory)
	h.svc.SetTxBeginner(reuseWorkerBeginner{memory: memory})
	st.memory = memory
	if err := h.svc.vlt.Unlock(context.Background(), h.userID, "test-password"); err != nil {
		t.Fatal(err)
	}
	state, err := memory.GetUserVaultState(context.Background(), h.userID)
	if err != nil {
		t.Fatal(err)
	}
	st.epoch = state.VaultEpoch
	admit := func(_ context.Context, arg store.CodexRefreshReuseParams) (bool, error) {
		if st.intent.ReuseAttemptedAt.Valid {
			return false, nil
		}
		if arg.UserID != st.acct.UserID || arg.AccountID != st.acct.ID || arg.OperationID != st.intent.OperationID || arg.FromGeneration != st.acct.Generation {
			t.Fatal("wrong admission fences")
		}
		st.admissions++
		st.intent.ReuseAttemptedAt = pgconv.Time(now)
		return true, nil
	}
	return h.svc, st, p, admit
}

func TestCodexReuseWorkerSuccess(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		t.Run(map[bool]string{false: "omitted_refresh", true: "rotated_refresh"}[rotate], func(t *testing.T) {
			s, st, p, admit := newReuseWorker(t)
			wantRefresh := "CANARY_REFRESH"
			if rotate {
				wantRefresh = "new-refresh"
				p.result.RefreshToken = &wantRefresh
			}
			resolved, changed, err := s.reconcileCodexRefreshWithReuse(context.Background(), st.acct.UserID, st.acct.ID, admit)
			if err != nil || resolved != 1 || !changed {
				t.Fatalf("resolved=%d changed=%v err=%v", resolved, changed, err)
			}
			if p.calls != 1 || p.discoveries != 1 || st.writes != 1 || st.promotes != 1 || st.accountReads != 2 || st.intentReads != 2 {
				t.Fatalf("calls=%d discoveries=%d writes=%d promotes=%d reads=%d/%d", p.calls, p.discoveries, st.writes, st.promotes, st.accountReads, st.intentReads)
			}
			blob, err := s.openCodexAccountLogin(context.Background(), st.acct.UserID, codexAccountSnapshot{CodexProviderAccount: st.acct, VaultEpoch: st.epoch})
			if err != nil || blob.AccessToken != "reuse-access" || blob.RefreshToken != wantRefresh {
				t.Fatal("incorrect merged promotion")
			}
			if st.lastWrite.Op != st.intent.OperationID || st.lastWrite.Gen != st.intent.FromGeneration || st.lastWrite.ID != st.acct.ID || st.lastWrite.UserID != st.acct.UserID {
				t.Fatal("recovery fence lost")
			}
			if _, _, err := s.reconcileCodexRefreshWithReuse(context.Background(), st.acct.UserID, st.acct.ID, admit); err != nil || p.calls != 1 {
				t.Fatal("second pass re-exchanged")
			}
		})
	}
}

func TestCodexReuseWorkerDeferrals(t *testing.T) {
	for _, kind := range []string{"disabled", "nil_vault", "locked", "corrupt", "missing_refresh", "no_receipt", "consumed", "expired", "not_rotating", "not_quarantined", "other_operation", "generation", "recovery", "reauth", "owner", "no_provider", "admission_denied", "admission_error"} {
		t.Run(kind, func(t *testing.T) {
			s, st, p, admit := newReuseWorker(t)
			switch kind {
			case "disabled":
				st.live = false
			case "nil_vault":
				s.vlt = nil
			case "locked":
				s.vlt.Lock(st.acct.UserID)
			case "corrupt":
				st.acct.SealedLogin = []byte("corrupt")
			case "missing_refresh":
				raw, _ := json.Marshal(codexLoginBlob{AccessToken: "old"}) //nolint:gosec // G117: synthetic fixture
				var err error
				st.acct.SealedLogin, err = s.box.Seal(raw)
				if err != nil {
					t.Fatal(err)
				}
			case "no_receipt":
				st.intent.ProviderErrorAt = pgtype.Timestamptz{}
			case "consumed":
				st.intent.ReuseAttemptedAt = pgconv.Time(s.codexNow())
			case "expired":
				st.intent.CreatedAt = pgconv.Time(s.codexNow().Add(-30 * time.Minute))
			case "not_rotating":
				st.intent.State = codexIntentUnrecoverable
			case "not_quarantined":
				st.acct.CoordState = codexCoordIdle
			case "other_operation":
				st.acct.CoordOperationID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
			case "generation":
				st.intent.FromGeneration--
			case "recovery":
				st.acct.RecoverySealed = []byte("protected")
			case "reauth":
				st.acct.ReauthRequired = true
			case "owner":
				st.intent.UserID = uuid.New()
			case "no_provider":
				s.codexRefresh = nil
			case "admission_denied":
				admit = func(context.Context, store.CodexRefreshReuseParams) (bool, error) { return false, nil }
			case "admission_error":
				admit = func(context.Context, store.CodexRefreshReuseParams) (bool, error) {
					return false, errors.New("db unavailable")
				}
			}
			applied, err := s.retryCodexRefreshReuse(context.Background(), st, codexAccountSnapshot{CodexProviderAccount: st.acct, VaultEpoch: st.epoch}, st.intent, admit)
			if applied || p.calls != 0 || st.admissions != 0 || st.writes != 0 {
				t.Fatal("deferral consumed or exchanged")
			}
			if (err != nil) != (kind == "admission_error") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestCodexReuseWorkerProviderFailure(t *testing.T) {
	for _, kind := range []string{"ambiguous", "rejected", "rejection_noop", "rejection_error"} {
		t.Run(kind, func(t *testing.T) {
			s, st, p, admit := newReuseWorker(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p.onRefresh = cancel
			rej := &recordingRejector{outcome: store.CodexRejectionApplied}
			s.codexRejector = rej
			p.err = errors.New("transport ambiguous")
			if kind != "ambiguous" {
				p.err = refreshAuthErr(400, codexauth.OAuthCodeInvalidGrant)
			}
			if kind == "rejection_noop" {
				rej.outcome = store.CodexRejectionNotApplied
			}
			if kind == "rejection_error" {
				rej.err = errors.New("db unavailable")
			}
			applied, err := s.retryCodexRefreshReuse(ctx, st, codexAccountSnapshot{CodexProviderAccount: st.acct, VaultEpoch: st.epoch}, st.intent, admit)
			if !applied || err != nil || p.calls != 1 || st.writes != 0 {
				t.Fatal("wrong failed exchange handling")
			}
			if kind == "ambiguous" {
				if st.terminals != 1 || st.terminalCtxErr != nil || st.terminalBudget <= 0 || st.terminalBudget > 5*time.Second {
					t.Fatal("terminal write must be detached and bounded")
				}
			} else {
				if len(rej.calls) != 1 || rej.ctxErrs[0] != nil || st.terminals != 0 {
					t.Fatal("rejection bypassed rotating transaction")
				}
				arg := rej.calls[0]
				if arg.UserID != st.acct.UserID || arg.AccountID != st.acct.ID || arg.OperationID != st.intent.OperationID || arg.FromGeneration != st.acct.Generation {
					t.Fatal("wrong rejection fence")
				}
			}
		})
	}
}

func TestCodexReuseWorkerPendingPersistence(t *testing.T) {
	s, st, p, admit := newReuseWorker(t)
	st.writeErr = errors.New("transient")
	var bg func()
	s.background = func(fn func()) { bg = fn }
	resolved, changed, err := s.reconcileCodexRefreshWithReuse(context.Background(), st.acct.UserID, st.acct.ID, admit)
	if err != nil || resolved != 0 || !changed || bg == nil || p.calls != 1 || st.terminals != 0 || st.intent.State != codexIntentRotating {
		t.Fatal("background pending was resolved or not counted")
	}
	admitted := st.intent.ReuseAttemptedAt.Time
	if codexRefreshResolution(admitted.Add(codexRefreshReuseInFlightBound-time.Nanosecond), st.acct, st.intent) != codexIntentPending ||
		codexRefreshResolution(admitted.Add(codexRefreshReuseInFlightBound), st.acct, st.intent) != codexIntentUnrecoverable {
		t.Fatal("wrong crashed-writer policy bound")
	}
	st.writeErr = nil
	bg()
	if st.writes != 3 || st.terminals != 0 {
		t.Fatal("background did not retain material")
	}
	if _, _, err := s.reconcileCodexRefreshWithReuse(context.Background(), st.acct.UserID, st.acct.ID, admit); err != nil || st.promotes != 1 || p.calls != 1 {
		t.Fatal("pending recovery did not promote without another exchange")
	}
}

func TestCodexReuseWorkerPersistenceExhausted(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync_loss", true: "background_loss"}[background], func(t *testing.T) {
			s, st, p, admit := newReuseWorker(t)
			st.writeErr = errors.New("unavailable")
			var bg func()
			if background {
				s.background = func(fn func()) { bg = fn }
			}
			if applied, err := s.retryCodexRefreshReuse(context.Background(), st, codexAccountSnapshot{CodexProviderAccount: st.acct, VaultEpoch: st.epoch}, st.intent, admit); !applied || err != nil {
				t.Fatal("not admitted")
			}
			if background {
				if bg == nil || st.terminals != 0 {
					t.Fatal("pending retry marked terminal")
				}
				bg()
			}
			wantWrites := 2
			if background {
				wantWrites = 8
			}
			if p.calls != 1 || st.writes != wantWrites || st.terminals != 1 || st.terminalCtxErr != nil || st.terminalBudget <= 0 || st.terminalBudget > 5*time.Second {
				t.Fatalf("calls=%d writes=%d terminal=%d", p.calls, st.writes, st.terminals)
			}
		})
	}
}

func TestCodexReuseWorkerOwnerFence(t *testing.T) {
	s, st, p, admit := newReuseWorker(t)
	st.fenceLost = true
	if applied, err := s.retryCodexRefreshReuse(context.Background(), st, codexAccountSnapshot{CodexProviderAccount: st.acct, VaultEpoch: st.epoch}, st.intent, admit); !applied || err != nil {
		t.Fatal("not admitted")
	}
	if p.calls != 1 || st.writes != 1 || len(st.acct.RecoverySealed) != 0 || st.promotes != 0 {
		t.Fatal("owner fence miss persisted or retried material")
	}
}

// The original review reproduction used the actual sibling reconciler in the
// provider callback. Cover both synchronous termination and background exhaustion.
func TestCodexReuseWorkerLateTerminalPreservesRestore(t *testing.T) {
	for _, kind := range []string{"provider_error", "seal_error", "sync_loss", "background_loss"} {
		t.Run(kind, func(t *testing.T) {
			s, st, p, admit := newReuseWorker(t)
			restore := func() {
				st.acct.Generation++
				st.acct.CoordState = codexCoordIdle
				st.acct.CoordOperationID.Valid = false
				if _, _, err := s.reconcileUnresolvedCodexRefresh(context.Background(), st.acct.UserID, st.acct.ID); err != nil {
					t.Fatal(err)
				}
				if st.intent.State != codexIntentReconciled {
					t.Fatal("restored generation did not reconcile")
				}
			}
			var bg func()
			switch kind {
			case "provider_error":
				p.onRefresh = restore
				p.err = errors.New("late provider error")
			case "seal_error":
				p.onRefresh = func() {
					restore()
					s.vlt.Lock(st.acct.UserID)
					s.box = nil
				}
			case "sync_loss":
				p.onRefresh = restore
				st.writeErr = errors.New("db unavailable")
			case "background_loss":
				st.writeErr = errors.New("db unavailable")
				s.background = func(fn func()) { bg = fn }
			}
			if applied, err := s.retryCodexRefreshReuse(context.Background(), st, codexAccountSnapshot{CodexProviderAccount: st.acct, VaultEpoch: st.epoch}, st.intent, admit); !applied || err != nil {
				t.Fatalf("retry applied=%v err=%v", applied, err)
			}
			if kind == "background_loss" {
				if bg == nil {
					t.Fatal("missing retained writer")
				}
				restore()
				bg()
			}
			if st.intent.State != codexIntentReconciled || st.acct.CoordState != codexCoordIdle || st.acct.Generation != 1 {
				t.Fatal("late retry reversed owner restore reconciliation")
			}
			if p.calls != 1 || st.terminals != 0 || len(st.acct.RecoverySealed) != 0 {
				t.Fatal("late retry overwrote restored state or material")
			}
		})
	}
}

func TestCodexReuseWorkerVaultLocksAfterExchange(t *testing.T) {
	s, st, p, admit := newReuseWorker(t)
	p.onRefresh = func() { s.vlt.Lock(st.acct.UserID) }
	if applied, err := s.retryCodexRefreshReuse(context.Background(), st, codexAccountSnapshot{CodexProviderAccount: st.acct, VaultEpoch: st.epoch}, st.intent, admit); !applied || err != nil {
		t.Fatal("not admitted")
	}
	if st.lastWrite.RecoverySealedWith.String != store.SealedWithMasterAAD || st.lastWrite.RecoveryCause.String != "vault_locked" || st.terminals != 0 {
		t.Fatal("missing master recovery fallback")
	}
	blob, err := openReuseSealed(s, st.acct.UserID, st.acct.RecoverySealed, store.SealedWithMasterAAD)
	if err != nil || blob.AccessToken != "reuse-access" || blob.RefreshToken != "CANARY_REFRESH" {
		t.Fatal("master recovery not merged")
	}
}
