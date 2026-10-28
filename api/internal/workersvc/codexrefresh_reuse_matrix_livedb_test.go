package workersvc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// These joined tests intentionally use production admission, resolution, rejection,
// slot persistence and promotion queries. Run only in the CI-owned LiveDB lane.
type joinedReuseProvider struct {
	mu               sync.Mutex
	tokens           []string
	identity         codexauth.Identity
	result           codexauth.RefreshResult
	retryErr         error
	entered          chan struct{}
	release          chan struct{}
	onReply          func()
	discoveryEntered chan struct{}
	discoveryRelease chan struct{}
}

func (p *joinedReuseProvider) Refresh(ctx context.Context, token string) (codexauth.RefreshResult, error) {
	p.mu.Lock()
	p.tokens = append(p.tokens, token)
	n := len(p.tokens)
	p.mu.Unlock()
	if n == 1 {
		return codexauth.RefreshResult{}, errors.New("original exchange reply lost")
	}
	if p.entered != nil {
		select {
		case p.entered <- struct{}{}:
		case <-ctx.Done():
			return codexauth.RefreshResult{}, ctx.Err()
		}
		select {
		case <-p.release:
		case <-ctx.Done():
			return codexauth.RefreshResult{}, ctx.Err()
		}
	}
	if p.onReply != nil {
		p.onReply()
	}
	return p.result, p.retryErr
}

func (p *joinedReuseProvider) DiscoverIdentity(ctx context.Context, access string) (codexauth.Identity, error) {
	if access != p.result.AccessToken {
		return codexauth.Identity{}, codexauth.ErrIdentityIncomplete
	}
	if p.discoveryEntered != nil {
		select {
		case p.discoveryEntered <- struct{}{}:
		case <-ctx.Done():
			return codexauth.Identity{}, ctx.Err()
		}
		select {
		case <-p.discoveryRelease:
		case <-ctx.Done():
			return codexauth.Identity{}, ctx.Err()
		}
	}
	return p.identity, nil
}

func (p *joinedReuseProvider) assertTokens(t *testing.T, want string, count int) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.tokens) != count {
		t.Fatalf("Refresh calls=%d, want %d", len(p.tokens), count)
	}
	for i, token := range p.tokens {
		if token != want {
			t.Fatalf("Refresh call %d did not use original token", i+1)
		}
	}
}

type joinedReuseSlot struct {
	arg store.SetCodexRecoverySlotParams
	n   int64
	row store.CodexProviderAccount
	err error
}

// Keep the real queries embedded: only fault injection and observation are fake.
type joinedReuseStore struct {
	*store.Queries
	mu          sync.Mutex
	failSlots   int
	slotCalls   int
	slots       []joinedReuseSlot
	receiptMode string
}

func (q *joinedReuseStore) RecordCodexRefreshProviderError(ctx context.Context, arg store.RecordCodexRefreshProviderErrorParams) (int64, error) {
	switch q.receiptMode {
	case "error":
		return 0, errors.New("receipt persistence unavailable")
	case "noop":
		return 0, nil
	default:
		return q.Queries.RecordCodexRefreshProviderError(ctx, arg)
	}
}

func (q *joinedReuseStore) SetCodexRecoverySlot(ctx context.Context, arg store.SetCodexRecoverySlotParams) (int64, error) {
	return q.setSlot(ctx, q.Queries, arg)
}

func (q *joinedReuseStore) setSlot(ctx context.Context, queries *store.Queries, arg store.SetCodexRecoverySlotParams) (int64, error) {
	q.mu.Lock()
	q.slotCalls++
	fail := q.slotCalls <= q.failSlots
	q.mu.Unlock()
	if fail {
		return 0, errors.New("transient slot write failure")
	}
	n, err := queries.SetCodexRecoverySlot(ctx, arg)
	row, rerr := queries.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: arg.UserID, ID: arg.ID})
	if err == nil {
		err = rerr
	}
	q.mu.Lock()
	q.slots = append(q.slots, joinedReuseSlot{arg: arg, n: n, row: row, err: err})
	q.mu.Unlock()
	return n, err
}

func (q *joinedReuseStore) observedSlots() ([]joinedReuseSlot, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]joinedReuseSlot(nil), q.slots...), q.slotCalls
}

type joinedReuseFix struct {
	env    codexTestEnv
	fx     *codexClaimFix
	p      *joinedReuseProvider
	q      *joinedReuseStore
	op     uuid.UUID
	gen    int64
	old    string
	held   store.Run
	shadow uuid.UUID
	cap    string
	worker store.Worker
}

func newJoinedReuseFix(t *testing.T) *joinedReuseFix {
	t.Helper()
	e := setupCodexLiveDB(t)
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	t.Cleanup(cancel)
	e.ctx = ctx
	e.exec = func(sql string, args ...any) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("joined fixture SQL: %v", err)
		}
	}
	fx := newCodexClaimFix(t, e, false)
	cleanupCodexRecoveryUser(t, e.pool, fx.userID)
	acct := e.mustAccount(t, fx.userID, fx.accountID)
	p := &joinedReuseProvider{
		identity: codexauth.Identity{ProviderUserID: acct.ProviderUserID, WorkspaceAccountID: acct.WorkspaceAccountID},
		result:   codexauth.RefreshResult{AccessToken: codexToken("reuse-access")},
	}
	q := &joinedReuseStore{Queries: e.q}
	fx.svc.q, fx.svc.codexRefresh = q, p
	hash, err := auth.HashPassword("joined-fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	e.exec("UPDATE users SET vault_mode='protected', password_hash=$2 WHERE id=$1", fx.userID, hash)
	fx.svc.vlt = vault.NewPostgres(e.box, e.pool)
	if err := fx.svc.vlt.Unlock(e.ctx, fx.userID, "joined-fixture-password"); err != nil {
		t.Fatal(err)
	}
	blob, err := fx.svc.openCodexAccountLogin(e.ctx, fx.userID, codexAccountSnapshot{CodexProviderAccount: acct})
	if err != nil {
		t.Fatal(err)
	}
	return &joinedReuseFix{env: e, fx: fx, p: p, q: q, gen: acct.Generation, old: blob.RefreshToken}
}

// The receipt comes from the original production exchange; only its lease is aged.
func (f *joinedReuseFix) strand(t *testing.T, lane string) {
	t.Helper()
	e, fx := f.env, f.fx
	f.op = uuid.New()
	if lane == "callback" {
		res, err := fx.svc.coordinatedRefresh(e.ctx, fx.userID, fx.accountID, f.op, f.gen,
			time.Now().Add(codexRefreshLeaseTTL), time.Now().Add(codexRefreshLeaseTTL-codexRefreshCommitReserve))
		assertAmbiguous(t, res, err)
	} else {
		principal := codexPollPrincipal{userID: fx.userID, accountID: fx.accountID, generation: f.gen}
		_, _, failure := fx.svc.handleCodexUsageUnauthorized(e.ctx, f.q, nil, principal)
		if failure == nil {
			t.Fatal("usage401 did not report original failure")
		}
		acct := e.mustAccount(t, fx.userID, fx.accountID)
		if !acct.CoordOperationID.Valid {
			t.Fatal("usage401 operation absent")
		}
		f.op = uuid.UUID(acct.CoordOperationID.Bytes)
	}
	it := f.intent(t)
	if it.State != codexIntentRotating || it.ReuseAttemptedAt.Valid ||
		it.ProviderErrorAt.Valid != (f.q.receiptMode == "") {
		t.Fatalf("original receipt state=%s receipt=%v consumed=%v", it.State, it.ProviderErrorAt.Valid, it.ReuseAttemptedAt.Valid)
	}
	e.exec("UPDATE codex_provider_account SET lease_deadline=now()-interval '1 second' WHERE id=$1", fx.accountID)
	n, err := e.q.QuarantineExpiredCodexLease(e.ctx, store.QuarantineExpiredCodexLeaseParams{
		ID: fx.accountID, UserID: fx.userID, Now: pgconv.Time(time.Now()),
	})
	if err != nil || n != 1 {
		t.Fatalf("reap=(%d,%v)", n, err)
	}
	parkHeldCodexFix(t, e, fx)
	f.held = mustRun(t, e, fx.runID)
	// A separate claimed, frozen run has a current capability. The parked run's
	// revoked capability would prove only revocation, not account quarantine. Give
	// it a separate repo because seedCodexRun always uses issue 1, and the held
	// run still counts against the active-run uniqueness constraint.
	shadowRepo := e.seedCodexRepo(t, fx.userID)
	f.shadow = e.seedCodexRun(t, fx.userID, fx.workerA, shadowRepo)
	if err := fx.svc.FreezeCodexBinding(e.ctx, fx.userID, f.shadow, fx.aliasID, codexAuthModeSubscription); err != nil {
		t.Fatal(err)
	}
	f.cap = e.mintCap(t, f.shadow, fx.workerA)
	f.worker = wkrRow(t, e, fx.workerA)
	f.p.assertTokens(t, f.old, 1)
}

func (f *joinedReuseFix) intent(t *testing.T) store.CodexRefreshIntent {
	t.Helper()
	it, err := f.env.q.GetCodexRefreshIntent(f.env.ctx, store.GetCodexRefreshIntentParams{UserID: f.fx.userID, OperationID: f.op})
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func (f *joinedReuseFix) reconcile(t *testing.T) {
	t.Helper()
	if _, err := f.fx.svc.ReconcileUnresolvedCodexRefresh(f.env.ctx, f.fx.userID, f.fx.accountID); err != nil {
		t.Fatal(err)
	}
}

func (f *joinedReuseFix) action(t *testing.T, want string) {
	t.Helper()
	got, err := f.fx.svc.CodexAccountActionsForRuns(f.env.ctx, []uuid.UUID{f.fx.runID})
	if err != nil || got[f.fx.runID].Action != want {
		t.Fatalf("action=%q err=%v want=%q", got[f.fx.runID].Action, err, want)
	}
}

func (f *joinedReuseFix) quarantined(t *testing.T) {
	t.Helper()
	acct := f.env.mustAccount(t, f.fx.userID, f.fx.accountID)
	if acct.CoordState != codexCoordQuarantined || acct.Generation != f.gen {
		t.Fatalf("account=%s gen=%d, want quarantined gen=%d", acct.CoordState, acct.Generation, f.gen)
	}
	if _, err := f.fx.svc.ReleaseCodexCredential(f.env.ctx, f.worker, f.shadow, f.cap); !errors.Is(err, ErrCodexAccountQuarantined) {
		t.Fatalf("fresh shadow release: %v", err)
	}
	if _, err := f.fx.svc.AuthorizeCodexCredentialOp(f.env.ctx, f.worker, f.shadow, f.cap, ScopeStartRefresh); !errors.Is(err, ErrCodexAccountQuarantined) {
		t.Fatalf("fresh shadow start: %v", err)
	}
	if n, err := promoteOnly(t, f.fx.svc, f.fx.runID); err != nil || n != 0 {
		t.Fatalf("quarantined promoter=(%d,%v)", n, err)
	}
	assertStillHeld(t, f.env, f.fx.runID, f.held)
}

func (f *joinedReuseFix) recovered(t *testing.T, wantRefresh string) {
	t.Helper()
	acct := f.env.mustAccount(t, f.fx.userID, f.fx.accountID)
	blob, err := f.fx.svc.openCodexAccountLogin(f.env.ctx, f.fx.userID, codexAccountSnapshot{CodexProviderAccount: acct})
	if err != nil || blob.AccessToken != f.p.result.AccessToken || blob.RefreshToken != wantRefresh ||
		acct.Generation != f.gen+1 || acct.CoordState == codexCoordQuarantined || len(acct.RecoverySealed) != 0 {
		t.Fatalf("recovered login/access/refresh match=%v/%v gen=%d state=%s slot=%d err=%v",
			blob.AccessToken == f.p.result.AccessToken, blob.RefreshToken == wantRefresh,
			acct.Generation, acct.CoordState, len(acct.RecoverySealed), err)
	}
	it := f.intent(t)
	if !it.ReuseAttemptedAt.Valid || !it.ProviderErrorAt.Valid || it.State != codexIntentReconciled {
		t.Fatalf("recovered intent=%s receipt=%v consumed=%v", it.State, it.ProviderErrorAt.Valid, it.ReuseAttemptedAt.Valid)
	}
	f.action(t, CodexAccountActionResuming)
	if n, err := promoteOnly(t, f.fx.svc, f.fx.runID); err != nil || n != 1 {
		t.Fatalf("recovered promoter=(%d,%v)", n, err)
	}
	assertPromoted(t, f.env, f.fx.runID, f.held)
	f.reconcile(t)
	f.p.assertTokens(t, f.old, 2)
}

func (f *joinedReuseFix) assertSlot(t *testing.T, wantN int64, wantRefresh string) {
	t.Helper()
	slots, _ := f.q.observedSlots()
	if len(slots) != 1 || slots[0].err != nil || slots[0].n != wantN {
		t.Fatalf("slot observations=%+v want one rowcount=%d", slots, wantN)
	}
	s := slots[0]
	if s.arg.UserID != f.fx.userID || s.arg.ID != f.fx.accountID || s.arg.Op != f.op || s.arg.Gen != f.gen {
		t.Fatal("slot write lost owner/operation/generation fence")
	}
	blob, err := openReuseSealed(f.fx.svc, f.fx.userID, s.arg.Sealed, s.arg.RecoverySealedWith.String)
	if err != nil || blob.AccessToken != f.p.result.AccessToken || blob.RefreshToken != wantRefresh {
		t.Fatalf("sealed slot material mismatch, err=%v", err)
	}
	if wantN == 1 && (s.row.CoordState != codexCoordQuarantined || s.row.Generation != f.gen ||
		!s.row.RecoveryGeneration.Valid || s.row.RecoveryGeneration.Int64 != f.gen ||
		s.row.RecoverySealedWith.String != s.arg.RecoverySealedWith.String ||
		string(s.row.RecoverySealed) != string(s.arg.Sealed)) {
		t.Fatal("slot was not observed protected on the real quarantined row before promotion")
	}
}

func TestCodexReuseJoinedSuccessLiveDB(t *testing.T) {
	for _, lane := range []string{"callback", "usage401"} {
		for _, replacement := range []bool{false, true} {
			name := lane + "/omitted_refresh"
			if replacement {
				name = lane + "/replacement_refresh"
			}
			t.Run(name, func(t *testing.T) {
				f := newJoinedReuseFix(t)
				want := f.old
				if replacement {
					want = codexToken("replacement")
					f.p.result.RefreshToken = &want
				}
				f.strand(t, lane)
				f.quarantined(t)
				f.action(t, CodexAccountActionReconciling)
				f.p.discoveryEntered, f.p.discoveryRelease = make(chan struct{}, 1), make(chan struct{})
				finish := f.resolverAtBarrier(t, false, f.p.discoveryEntered, f.p.discoveryRelease)
				f.assertSlot(t, 1, want)
				f.quarantined(t)
				f.action(t, CodexAccountActionReconciling)
				f.p.assertTokens(t, f.old, 2)
				finish()
				f.recovered(t, want)
			})
		}
	}
}

func TestCodexReuseJoinedProviderFailuresLiveDB(t *testing.T) {
	for _, code := range []string{codexauth.OAuthCodeRefreshTokenReused, codexauth.OAuthCodeInvalidGrant, "transport"} {
		t.Run(code, func(t *testing.T) {
			f := newJoinedReuseFix(t)
			f.p.retryErr = errors.New("second reply lost")
			if code != "transport" {
				f.p.retryErr = refreshAuthErr(400, code)
			}
			f.strand(t, "callback")
			f.reconcile(t)
			f.quarantined(t)
			f.action(t, CodexAccountActionReloginRequired)
			acct := f.env.mustAccount(t, f.fx.userID, f.fx.accountID)
			if acct.ReauthRequired != (code != "transport") ||
				(code != "transport" && acct.ReauthReason.String != "provider_rejected") {
				t.Fatalf("reauth=%v reason=%v", acct.ReauthRequired, acct.ReauthReason)
			}
			if it := f.intent(t); it.State != codexIntentUnrecoverable || !it.ReuseAttemptedAt.Valid {
				t.Fatalf("failed retry intent=%s consumed=%v", it.State, it.ReuseAttemptedAt.Valid)
			}
			f.reconcile(t)
			f.p.assertTokens(t, f.old, 2)
			if slots, calls := f.q.observedSlots(); len(slots) != 0 || calls != 0 {
				t.Fatal("failed provider reply wrote recovery material")
			}
		})
	}
}

func TestCodexReuseJoinedNegativeControlsLiveDB(t *testing.T) {
	for _, kind := range []string{"no_receipt", "receipt_error", "receipt_noop", "wrong_operation", "recovery", "reauth", "generation", "expired", "consumed", "owner_restore"} {
		t.Run(kind, func(t *testing.T) {
			f := newJoinedReuseFix(t)
			if kind == "receipt_error" {
				f.q.receiptMode = "error"
			}
			if kind == "receipt_noop" {
				f.q.receiptMode = "noop"
			}
			f.strand(t, "callback")
			e := f.env
			switch kind {
			case "no_receipt":
				e.exec("UPDATE codex_refresh_intent SET provider_error_at=NULL WHERE operation_id=$1", f.op)
			case "wrong_operation":
				f.fx.setAccount(t, "coord_operation_id=gen_random_uuid()")
			case "recovery":
				n, err := e.q.SetCodexRecoverySlot(e.ctx, store.SetCodexRecoverySlotParams{
					UserID: f.fx.userID, ID: f.fx.accountID, Op: f.op, Gen: f.gen,
					Sealed: sealedLogin(t, e, f.p.result.AccessToken), RecoverySealedWith: pgconv.Text(store.SealedWithMaster),
				})
				if err != nil || n != 1 {
					t.Fatalf("genuine recovery seed=(%d,%v)", n, err)
				}
			case "reauth":
				f.fx.setAccount(t, "reauth_required=true, reauth_generation=generation, reauth_credential_revision=credential_revision, reauth_reason='provider_rejected'")
			case "generation":
				f.fx.setAccount(t, "generation=generation+1")
			case "expired":
				e.exec("UPDATE codex_refresh_intent SET created_at=now()-interval '31 minutes' WHERE operation_id=$1", f.op)
			case "consumed":
				f.consume(t)
			case "owner_restore":
				relogin(t, e, e.q, f.fx, f.p.result.AccessToken)
			}
			f.reconcile(t)
			f.reconcile(t)
			f.p.assertTokens(t, f.old, 1)
			if kind != "consumed" && f.intent(t).ReuseAttemptedAt.Valid {
				t.Fatal("negative control consumed retry")
			}
			wantState := codexIntentRotating
			switch kind {
			case "no_receipt", "receipt_error", "receipt_noop", "expired":
				wantState = codexIntentUnrecoverable
				f.action(t, CodexAccountActionReloginRequired)
			case "recovery", "generation", "owner_restore":
				wantState = codexIntentReconciled
			}
			if got := f.intent(t).State; got != wantState {
				t.Fatalf("negative control intent=%s want=%s", got, wantState)
			}
			if kind == "recovery" || kind == "owner_restore" {
				acct := e.mustAccount(t, f.fx.userID, f.fx.accountID)
				blob, openErr := f.fx.svc.openCodexAccountLogin(f.env.ctx, f.fx.userID, codexAccountSnapshot{CodexProviderAccount: acct})
				if acct.Generation != f.gen+1 || acct.CoordState == codexCoordQuarantined ||
					acct.CoordOperationID.Valid || len(acct.RecoverySealed) != 0 || openErr != nil || blob.AccessToken != f.p.result.AccessToken {
					t.Fatal("existing genuine recovery/owner restore was not retained without Refresh")
				}
				f.action(t, CodexAccountActionResuming)
			}
			if kind != "recovery" && kind != "owner_restore" {
				acct := e.mustAccount(t, f.fx.userID, f.fx.accountID)
				blob, openErr := f.fx.svc.openCodexAccountLogin(f.env.ctx, f.fx.userID, codexAccountSnapshot{CodexProviderAccount: acct})
				if openErr != nil || blob.AccessToken != f.fx.access || blob.RefreshToken != f.old ||
					acct.CoordState != codexCoordQuarantined || len(acct.RecoverySealed) != 0 {
					t.Fatal("negative control changed original login or quarantine")
				}
				wantAction := CodexAccountActionReloginRequired
				if kind == "consumed" {
					wantAction = CodexAccountActionReconciling
				}
				f.action(t, wantAction)
			}
			if slots, calls := f.q.observedSlots(); len(slots) != 0 || calls != 0 {
				t.Fatal("negative control invoked reuse slot writer")
			}
		})
	}
}

func (f *joinedReuseFix) consume(t *testing.T) {
	t.Helper()
	ok, err := store.AdmitCodexRefreshReuse(f.env.ctx, f.env.pool, store.CodexRefreshReuseParams{
		UserID: f.fx.userID, AccountID: f.fx.accountID, OperationID: f.op, FromGeneration: f.gen,
	})
	if err != nil || !ok {
		t.Fatalf("crash-control admission=(%v,%v)", ok, err)
	}
}

func TestCodexReuseJoinedDeferralLiveDB(t *testing.T) {
	for _, kind := range []string{"disabled", "locked", "disabled_expired", "locked_expired"} {
		t.Run(kind, func(t *testing.T) {
			f := newJoinedReuseFix(t)
			f.strand(t, "callback")
			created := f.intent(t).CreatedAt
			disabled := kind == "disabled" || kind == "disabled_expired"
			expired := kind == "disabled_expired" || kind == "locked_expired"
			if disabled {
				f.env.exec("UPDATE user_secrets SET disabled_at=now() WHERE id=$1", f.fx.aliasID)
			} else {
				f.fx.svc.vlt.Lock(f.fx.userID)
			}
			f.reconcile(t)
			it := f.intent(t)
			if it.ReuseAttemptedAt.Valid || it.State != codexIntentRotating || !it.CreatedAt.Time.Equal(created.Time) {
				t.Fatal("deferral consumed/resolved/reset original intent")
			}
			wantAction := CodexAccountActionReconciling
			if disabled {
				wantAction = CodexAccountActionReloginRequired
			}
			f.action(t, wantAction)
			f.p.assertTokens(t, f.old, 1)
			if expired {
				f.env.exec("UPDATE codex_refresh_intent SET created_at=now()-interval '31 minutes' WHERE operation_id=$1", f.op)
				f.reconcile(t)
				if it = f.intent(t); it.State != codexIntentUnrecoverable || it.ReuseAttemptedAt.Valid {
					t.Fatal("expired deferred intent did not take old terminal path")
				}
				f.action(t, CodexAccountActionReloginRequired)
			}
			if disabled {
				f.env.exec("UPDATE user_secrets SET disabled_at=NULL WHERE id=$1", f.fx.aliasID)
			} else if err := f.fx.svc.vlt.Unlock(f.env.ctx, f.fx.userID, "joined-fixture-password"); err != nil {
				t.Fatal(err)
			}
			f.reconcile(t)
			if expired {
				f.p.assertTokens(t, f.old, 1)
			} else {
				f.recovered(t, f.old)
				if !f.intent(t).CreatedAt.Time.Equal(created.Time) {
					t.Fatal("retry reset created_at")
				}
			}
		})
	}
}

// Start at most one owned goroutine. Cleanup always releases, cancels, and joins
// it, including assertions that fail before the provider barrier is reached.
func (f *joinedReuseFix) blockedResolver(t *testing.T, sweep bool) func() {
	t.Helper()
	f.p.entered, f.p.release = make(chan struct{}, 1), make(chan struct{})
	return f.resolverAtBarrier(t, sweep, f.p.entered, f.p.release)
}

func (f *joinedReuseFix) resolverAtBarrier(t *testing.T, sweep bool, entered, unblock chan struct{}) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.env.ctx, 15*time.Second)
	done := make(chan struct{})
	var once sync.Once
	var result error
	release := func() { once.Do(func() { close(unblock) }) }
	go func() {
		defer close(done)
		if sweep {
			_, result = f.fx.svc.SweepUnresolvedCodexRefresh(ctx)
		} else {
			_, result = f.fx.svc.ReconcileUnresolvedCodexRefresh(ctx, f.fx.userID, f.fx.accountID)
		}
	}()
	t.Cleanup(func() {
		release()
		cancel()
		// Both provider barriers honor ctx; database calls and detached writes
		// retain their production bounds. Join rather than leave an owned worker
		// behind when an assertion aborts the parent at a barrier.
		<-done
	})
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("resolver provider barrier not reached")
	}
	return func() {
		release()
		select {
		case <-done:
			if result != nil {
				t.Fatalf("owned resolver: %v", result)
			}
		case <-ctx.Done():
			t.Fatal("owned resolver did not finish")
		}
	}
}

func (f *joinedReuseFix) assertFenced(t *testing.T) {
	t.Helper()
	n, err := f.env.q.SetCodexRefreshIntentStateFenced(f.env.ctx, store.SetCodexRefreshIntentStateFencedParams{
		State: codexIntentUnrecoverable, OperationID: f.op, UserID: f.fx.userID,
		ExpectedGeneration: f.gen, ReuseInFlightMicroseconds: codexRefreshReuseInFlightBound.Microseconds(),
	})
	if err != nil || n != 0 {
		t.Fatalf("stale terminal verdict=(%d,%v), want fence refusal", n, err)
	}
}

func TestCodexReuseJoinedConcurrentResolversLiveDB(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		for _, sweepFirst := range []bool{false, true} {
			name := "success/account_first"
			if rejected {
				name = "rejected/account_first"
			}
			if sweepFirst {
				name += "/sweep_first"
			}
			t.Run(name, func(t *testing.T) {
				f := newJoinedReuseFix(t)
				if rejected {
					f.p.retryErr = refreshAuthErr(400, codexauth.OAuthCodeInvalidGrant)
				}
				f.strand(t, "callback")
				finish := f.blockedResolver(t, sweepFirst)
				if sweepFirst {
					f.reconcile(t)
				} else if _, err := f.fx.svc.SweepUnresolvedCodexRefresh(f.env.ctx); err != nil {
					t.Fatal(err)
				}
				f.quarantined(t)
				f.action(t, CodexAccountActionReconciling)
				if it := f.intent(t); it.State != codexIntentRotating || !it.ReuseAttemptedAt.Valid {
					t.Fatal("competing resolver terminated inflight retry")
				}
				f.assertFenced(t)
				f.p.assertTokens(t, f.old, 2)
				finish()
				if rejected {
					if it := f.intent(t); it.State != codexIntentUnrecoverable {
						t.Fatal("rejection did not resolve rotating admitted intent")
					}
					acct := f.env.mustAccount(t, f.fx.userID, f.fx.accountID)
					if !acct.ReauthRequired || acct.ReauthReason.String != "provider_rejected" {
						t.Fatal("rejection transaction did not apply after competing resolver")
					}
					f.action(t, CodexAccountActionReloginRequired)
				} else {
					f.recovered(t, f.old)
				}
				f.reconcile(t)
				f.p.assertTokens(t, f.old, 2)
			})
		}
	}
}

func (f *joinedReuseFix) assertRestored(t *testing.T, access string) {
	t.Helper()
	acct := f.env.mustAccount(t, f.fx.userID, f.fx.accountID)
	blob, err := f.fx.svc.openCodexAccountLogin(f.env.ctx, f.fx.userID, codexAccountSnapshot{CodexProviderAccount: acct})
	if err != nil || blob.AccessToken != access || acct.Generation != f.gen+1 ||
		acct.CoordState == codexCoordQuarantined || acct.CoordOperationID.Valid || len(acct.RecoverySealed) != 0 {
		t.Fatalf("owner restore overwritten: access match=%v gen=%d state=%s op=%v slot=%d err=%v",
			blob.AccessToken == access, acct.Generation, acct.CoordState, acct.CoordOperationID.Valid, len(acct.RecoverySealed), err)
	}
	if f.intent(t).State != codexIntentReconciled {
		t.Fatal("late completion clobbered restored/reconciled intent")
	}
	f.p.assertTokens(t, f.old, 2)
}

func TestCodexReuseJoinedOwnerRestoreDuringRetryLiveDB(t *testing.T) {
	for _, genericLateError := range []bool{false, true} {
		name := "late_success"
		if genericLateError {
			name = "late_generic_error"
		}
		t.Run(name, func(t *testing.T) {
			f := newJoinedReuseFix(t)
			if genericLateError {
				f.p.retryErr = errors.New("late transport failure")
			}
			f.strand(t, "callback")
			finish := f.blockedResolver(t, false)
			access := codexToken("owner-restored")
			if gen := relogin(t, f.env, f.env.q, f.fx, access); gen != f.gen+1 {
				t.Fatal("owner restore did not advance generation during blocked OAuth")
			}
			f.reconcile(t)
			f.assertFenced(t)
			finish()
			f.assertRestored(t, access)
			if !genericLateError {
				f.assertSlot(t, 0, f.old)
			}
		})
	}
}

func TestCodexReuseJoinedCrashAfterConsumeLiveDB(t *testing.T) {
	f := newJoinedReuseFix(t)
	f.strand(t, "callback")
	f.consume(t) // Deliberate crash control: commit admission without any HTTP.
	f.reconcile(t)
	f.quarantined(t)
	f.action(t, CodexAccountActionReconciling)
	f.assertFenced(t)
	f.env.exec("UPDATE codex_refresh_intent SET reuse_attempted_at=now()-($2::bigint * interval '1 microsecond') WHERE operation_id=$1",
		f.op, (codexRefreshReuseInFlightBound + time.Second).Microseconds())
	f.reconcile(t)
	if f.intent(t).State != codexIntentUnrecoverable {
		t.Fatal("crashed admitted retry not resolved after policy bound")
	}
	f.action(t, CodexAccountActionReloginRequired)
	f.reconcile(t)
	f.p.assertTokens(t, f.old, 1)
}

func TestCodexReuseJoinedPausedBackgroundWriterLiveDB(t *testing.T) {
	for _, kind := range []string{"within_bound", "past_bound", "owner_restore"} {
		t.Run(kind, func(t *testing.T) {
			f := newJoinedReuseFix(t)
			f.strand(t, "callback")
			f.q.failSlots = 2
			queued := make(chan func(), 1)
			f.fx.svc.background = func(fn func()) { queued <- fn }
			ctx, cancel := context.WithCancel(f.env.ctx)
			defer cancel()
			f.p.onReply = cancel // Real OAuth succeeded; request cancellation must not lose its reply.
			_, err := f.fx.svc.ReconcileUnresolvedCodexRefresh(ctx, f.fx.userID, f.fx.accountID)
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			var writer func()
			select {
			case writer = <-queued:
			default:
				t.Fatal("two transient writes did not hand sealed material to background")
			}
			if slots, calls := f.q.observedSlots(); len(slots) != 0 || calls != 2 {
				t.Fatalf("pending writer calls=%d successful observations=%d", calls, len(slots))
			}
			// No scheduler claim: age only the durable attempt. Eight seconds is beyond
			// the six-second OAuth budget but inside the shared persistence bound.
			f.env.exec("UPDATE codex_refresh_intent SET reuse_attempted_at=now()-interval '8 seconds' WHERE operation_id=$1", f.op)
			f.reconcile(t)
			f.quarantined(t)
			f.action(t, CodexAccountActionReconciling)
			if f.intent(t).State != codexIntentRotating {
				t.Fatal("resolver lost material still held by paused writer")
			}
			f.assertFenced(t)
			f.p.assertTokens(t, f.old, 2)
			access := ""
			switch kind {
			case "past_bound":
				f.env.exec("UPDATE codex_refresh_intent SET reuse_attempted_at=now()-($2::bigint * interval '1 microsecond') WHERE operation_id=$1",
					f.op, (codexRefreshReuseInFlightBound + time.Second).Microseconds())
				f.reconcile(t)
				if f.intent(t).State != codexIntentUnrecoverable {
					t.Fatal("past-bound resolver did not follow approved crash policy")
				}
				f.action(t, CodexAccountActionReloginRequired)
			case "owner_restore":
				access = codexToken("owner-restored")
				relogin(t, f.env, f.env.q, f.fx, access)
				f.reconcile(t)
			}
			writer() // The queued closure performs its existing detached bounded retry.
			f.reconcile(t)
			if kind == "owner_restore" {
				f.assertSlot(t, 0, f.old)
				f.assertRestored(t, access)
			} else {
				f.assertSlot(t, 1, f.old)
				// Past-bound intents may remain terminal; late current-generation
				// material still promotes under the existing account/operation fence.
				if kind == "past_bound" {
					acct := f.env.mustAccount(t, f.fx.userID, f.fx.accountID)
					blob, openErr := f.fx.svc.openCodexAccountLogin(f.env.ctx, f.fx.userID, codexAccountSnapshot{CodexProviderAccount: acct})
					if openErr != nil || blob.AccessToken != f.p.result.AccessToken || blob.RefreshToken != f.old || acct.Generation != f.gen+1 {
						t.Fatal("late current-generation material was not promoted")
					}
					f.action(t, CodexAccountActionResuming)
					if n, promoteErr := promoteOnly(t, f.fx.svc, f.fx.runID); promoteErr != nil || n != 1 {
						t.Fatalf("late writer promoter=(%d,%v)", n, promoteErr)
					}
					assertPromoted(t, f.env, f.fx.runID, f.held)
				} else {
					f.recovered(t, f.old)
				}
			}
			f.reconcile(t)
			f.p.assertTokens(t, f.old, 2)
			if _, calls := f.q.observedSlots(); calls != 3 {
				t.Fatalf("slot calls=%d, want two failures plus one detached write", calls)
			}
		})
	}
}

type joinedReuseTxStore struct {
	*store.Queries
	root *joinedReuseStore
}

func (q *joinedReuseStore) bindCodexRefreshTx(queries *store.Queries) codexRefreshStore {
	return &joinedReuseTxStore{Queries: queries, root: q}
}

func (q *joinedReuseTxStore) SetCodexRecoverySlot(ctx context.Context, arg store.SetCodexRecoverySlotParams) (int64, error) {
	return q.root.setSlot(ctx, q.Queries, arg)
}
