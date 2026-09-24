package workersvc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1594 M3, live-DB half: the provider-rejection exception against a REAL Postgres,
// through the production store.QuarantineRejectedCodexRefresh transaction (the Service's
// txBeginner is the pool). The DB-free status-by-code matrix lives in
// codexrefresh_rejection_test.go.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// rejectedRefreshErr is the provider's explicit refresh-material rejection.
func rejectedRefreshErr() error {
	return &codexauth.AuthError{Op: "refresh", StatusCode: http.StatusUnauthorized, OAuthCode: codexauth.OAuthCodeRefreshTokenReused}
}

// assertNoRecovery fails when any recovery column is set.
func assertNoRecovery(t *testing.T, acct store.CodexProviderAccount) {
	t.Helper()
	if len(acct.RecoverySealed) != 0 || acct.RecoveryGeneration.Valid || acct.RecoverySealedWith.Valid {
		t.Fatalf("recovery columns set: sealed=%d gen=%v with=%v, want all NULL", len(acct.RecoverySealed), acct.RecoveryGeneration, acct.RecoverySealedWith)
	}
}

// assertRejectedAccount checks the durable state the rejection transaction writes.
func assertRejectedAccount(t *testing.T, acct store.CodexProviderAccount, gen int64) {
	t.Helper()
	if acct.CoordState != codexCoordQuarantined {
		t.Fatalf("coord_state = %q, want quarantined", acct.CoordState)
	}
	if !acct.ReauthRequired {
		t.Fatal("reauth_required = false, want true")
	}
	if !acct.ReauthReason.Valid || acct.ReauthReason.String != "provider_rejected" {
		t.Fatalf("reauth_reason = %v, want provider_rejected", acct.ReauthReason)
	}
	if !acct.ReauthGeneration.Valid || acct.ReauthGeneration.Int64 != gen {
		t.Fatalf("reauth_generation = %v, want %d", acct.ReauthGeneration, gen)
	}
	if acct.Generation != gen {
		t.Fatalf("generation = %d, want unchanged %d", acct.Generation, gen)
	}
	assertNoRecovery(t, acct)
}

// TestCoordinatedCodexRefreshProviderRejectionQuarantinesLiveDB proves an explicit
// provider rejection (401 refresh_token_reused) is recorded durably in one transaction,
// releases no token, and is terminal: neither a same-op retry nor a distinct new op spends
// another provider call.
func TestCoordinatedCodexRefreshProviderRejectionQuarantinesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeRefreshClient{err: rejectedRefreshErr()}
	f := newRefreshFixture(t, env, fake)
	f.svc.txBeginner = env.pool
	capw := env.mintCap(t, f.runID, f.workerID)
	op := uuid.New()

	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0)
	if !errors.Is(err, ErrCodexRefreshRejected) || !errors.Is(err, ErrCodexRefreshUnrecoverable) {
		t.Fatalf("err = %v, want ErrCodexRefreshRejected and ErrCodexRefreshUnrecoverable", err)
	}
	if res.Outcome != CodexRefreshQuarantined || res.AccessToken != "" {
		t.Fatalf("result outcome = %v token-set = %v, want quarantined with no token", res.Outcome, res.AccessToken != "")
	}
	if fake.calls != 1 || fake.lastRefreshToken != f.prevRefreshToken {
		t.Fatalf("provider calls = %d (token matched = %v), want exactly 1 with the previous refresh token", fake.calls, fake.lastRefreshToken == f.prevRefreshToken)
	}
	assertRejectedAccount(t, env.mustAccount(t, f.userID, f.accountID), 0)
	if it := mustIntent(t, env, op, f.userID); it.State != codexIntentUnrecoverable {
		t.Fatalf("intent state = %q, want unrecoverable", it.State)
	}

	// The same op again, through the public entry: authority is refused (the account is
	// quarantined) before any provider call.
	if _, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, op, 0); err == nil {
		t.Fatal("same-op retry through the public entry succeeded, want a refusal")
	}
	// And through the post-authorization core: the terminal intent answers unrecoverable.
	lease := time.Now().Add(time.Minute)
	res, err = f.svc.coordinatedRefresh(env.ctx, f.userID, f.accountID, op, 0, lease, lease)
	if !errors.Is(err, ErrCodexRefreshUnrecoverable) {
		t.Fatalf("same-op retry err = %v, want ErrCodexRefreshUnrecoverable", err)
	}
	if res.AccessToken != "" {
		t.Fatal("same-op retry returned a token")
	}
	// A distinct new op at the same observed generation cannot take the lease on a
	// quarantined account: contended, no token.
	res, err = f.svc.coordinatedRefresh(env.ctx, f.userID, f.accountID, uuid.New(), 0, lease, lease)
	if !errors.Is(err, ErrCodexRefreshContended) || res.AccessToken != "" {
		t.Fatalf("distinct op: err = %v token-set = %v, want contended with no token", err, res.AccessToken != "")
	}
	if fake.calls != 1 {
		t.Fatalf("provider calls after retries = %d, want still 1", fake.calls)
	}
	assertRejectedAccount(t, env.mustAccount(t, f.userID, f.accountID), 0)
}

// protectedAmbiguousAccount drives op A into an ambiguous provider failure (500), reaps
// its expired lease to quarantine, and protects a matching login in the recovery slot at
// generation 0, modelling material a survivor can still promote. It returns op A and the
// sealed recovery bytes.
func protectedAmbiguousAccount(t *testing.T, env codexTestEnv, f refreshFixture, fake *fakeRefreshClient) (uuid.UUID, []byte) {
	t.Helper()
	capw := env.mintCap(t, f.runID, f.workerID)
	opA := uuid.New()
	fake.err = &codexauth.AuthError{Op: "refresh", StatusCode: http.StatusInternalServerError}
	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, opA, 0)
	if err == nil || errors.Is(err, ErrCodexRefreshRejected) || errors.Is(err, ErrCodexRefreshUnrecoverable) {
		t.Fatalf("op A err = %v, want the ambiguous provider error", err)
	}
	if res.Outcome != CodexRefreshContended {
		t.Fatalf("op A outcome = %v, want contended", res.Outcome)
	}
	if it := mustIntent(t, env, opA, f.userID); it.State != codexIntentRotating {
		t.Fatalf("op A intent = %q, want rotating", it.State)
	}

	// The lease expires; the survivor reap quarantines it (keeping op A as the owner).
	env.exec(`UPDATE codex_provider_account SET lease_deadline = now() - interval '1 hour' WHERE id = $1`, f.accountID)
	if n, err := env.q.QuarantineExpiredCodexLease(env.ctx, store.QuarantineExpiredCodexLeaseParams{
		ID: f.accountID, UserID: f.userID, Now: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil || n != 1 {
		t.Fatalf("QuarantineExpiredCodexLease = (%d,%v), want (1,nil)", n, err)
	}

	// Protected material for op A at generation 0; its access token verifies to the
	// account's own identity, so it is promotable.
	raw, err := json.Marshal(codexLoginBlob{AccessToken: codexToken("access-recovered"), RefreshToken: codexToken("refresh-recovered")})
	if err != nil {
		t.Fatalf("marshal recovery blob: %v", err)
	}
	sealed, err := env.box.Seal(raw)
	if err != nil {
		t.Fatalf("seal recovery blob: %v", err)
	}
	if n, err := env.q.SetCodexRecoverySlot(env.ctx, store.SetCodexRecoverySlotParams{
		Sealed: sealed, Gen: 0, ID: f.accountID, UserID: f.userID, Op: opA,
		RecoverySealedWith: pgtype.Text{String: store.SealedWithMaster, Valid: true},
	}); err != nil || n != 1 {
		t.Fatalf("SetCodexRecoverySlot = (%d,%v), want (1,nil)", n, err)
	}
	return opA, sealed
}

// TestLaterDistinctOpAfterAmbiguousExchangeNeverReplaysLiveDB proves a later, distinct
// operation never re-spends the refresh token after an earlier ambiguous exchange, even
// when the provider would now answer with a material rejection: the protected recovery
// material stays intact and promotable, and no re-login flag is raised.
func TestLaterDistinctOpAfterAmbiguousExchangeNeverReplaysLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeRefreshClient{}
	f := newRefreshFixture(t, env, fake)
	f.svc.txBeginner = env.pool
	opA, sealed := protectedAmbiguousAccount(t, env, f, fake)
	callsAfterA := fake.calls

	// From now on the provider would reject the refresh material if it were ever called.
	fake.err = rejectedRefreshErr()
	capw := env.mintCap(t, f.runID, f.workerID)
	opB := uuid.New()
	if res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, opB, 0); err == nil || res.AccessToken != "" {
		t.Fatalf("op B public entry: err = %v token-set = %v, want a refusal with no token", err, res.AccessToken != "")
	}
	lease := time.Now().Add(time.Minute)
	res, err := f.svc.coordinatedRefresh(env.ctx, f.userID, f.accountID, opB, 0, lease, lease)
	if !errors.Is(err, ErrCodexRefreshContended) || res.AccessToken != "" {
		t.Fatalf("op B core: err = %v token-set = %v, want contended with no token", err, res.AccessToken != "")
	}
	if errors.Is(err, ErrCodexRefreshRejected) {
		t.Fatalf("op B err = %v, must never be a rejection", err)
	}
	if fake.calls != callsAfterA {
		t.Fatalf("provider calls = %d, want unchanged %d (op B must never present the old refresh token)", fake.calls, callsAfterA)
	}

	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.CoordState != codexCoordQuarantined {
		t.Fatalf("coord_state = %q, want quarantined", acct.CoordState)
	}
	if acct.ReauthRequired || acct.ReauthReason.Valid {
		t.Fatalf("reauth_required = %v reason = %v, want false and NULL", acct.ReauthRequired, acct.ReauthReason)
	}
	if !bytes.Equal(acct.RecoverySealed, sealed) {
		t.Fatal("recovery slot bytes changed")
	}
	if !acct.RecoveryGeneration.Valid || acct.RecoveryGeneration.Int64 != acct.Generation {
		t.Fatalf("recovery_generation = %v, want %d (still promotable)", acct.RecoveryGeneration, acct.Generation)
	}
	if it := mustIntent(t, env, opA, f.userID); it.State != codexIntentRotating {
		t.Fatalf("op A intent = %q, want still rotating", it.State)
	}

	// Promotable in fact: the survivor promotes the protected material.
	if _, err := f.svc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	promoted := env.mustAccount(t, f.userID, f.accountID)
	if promoted.CoordState != codexCoordIdle || promoted.Generation != 1 {
		t.Fatalf("after promotion: coord_state = %q generation = %d, want idle at 1", promoted.CoordState, promoted.Generation)
	}
	if fake.calls != callsAfterA {
		t.Fatalf("provider calls after promotion = %d, want unchanged %d", fake.calls, callsAfterA)
	}
}

// TestProviderRejectionAfterVerifiedPromotionLiveDB is the variant: once a verified
// promotion returns the account to idle at a new generation (no recovery slot left), a
// new op that the provider rejects IS recorded as a rejection, and there was no recovery
// material to lose.
func TestProviderRejectionAfterVerifiedPromotionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeRefreshClient{}
	f := newRefreshFixture(t, env, fake)
	f.svc.txBeginner = env.pool
	protectedAmbiguousAccount(t, env, f, fake)
	if _, err := f.svc.ReconcileUnresolvedCodexRefresh(env.ctx, f.userID, f.accountID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	before := env.mustAccount(t, f.userID, f.accountID)
	if before.CoordState != codexCoordIdle || before.Generation != 1 {
		t.Fatalf("precondition: coord_state = %q generation = %d, want idle at 1", before.CoordState, before.Generation)
	}
	assertNoRecovery(t, before)
	callsBefore := fake.calls

	fake.err = rejectedRefreshErr()
	opC := uuid.New()
	lease := time.Now().Add(time.Minute)
	res, err := f.svc.coordinatedRefresh(env.ctx, f.userID, f.accountID, opC, 1, lease, lease)
	if !errors.Is(err, ErrCodexRefreshRejected) || !errors.Is(err, ErrCodexRefreshUnrecoverable) {
		t.Fatalf("op C err = %v, want ErrCodexRefreshRejected and ErrCodexRefreshUnrecoverable", err)
	}
	if res.AccessToken != "" {
		t.Fatal("op C returned a token")
	}
	if fake.calls != callsBefore+1 {
		t.Fatalf("provider calls = %d, want exactly one more than %d", fake.calls, callsBefore)
	}
	assertRejectedAccount(t, env.mustAccount(t, f.userID, f.accountID), 1)
	if it := mustIntent(t, env, opC, f.userID); it.State != codexIntentUnrecoverable {
		t.Fatalf("op C intent = %q, want unrecoverable", it.State)
	}
}

// TestCoordinatedCodexRefreshRealClientRejectionLiveDB drives the rejection end to end
// through the PRODUCTION *codexauth.Client against an httptest token endpoint that answers
// 401 {"error":{"code":"refresh_token_reused","message":<echo of the request>}}: the
// account is quarantined, and neither the returned error nor any log record carries the
// echoed refresh token; the phase record names the closed-set code.
func TestCoordinatedCodexRefreshRealClientRejectionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	capt := installCapture(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "refresh_token_reused", "message": string(body)},
		})
	}))
	defer srv.Close()
	client, err := realCodexUsageClient(srv.URL)
	if err != nil {
		t.Fatalf("real client: %v", err)
	}

	f := newRefreshFixture(t, env, &fakeRefreshClient{})
	f.svc.codexRefresh = client
	f.svc.txBeginner = env.pool
	capw := env.mintCap(t, f.runID, f.workerID)

	res, rerr := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, uuid.New(), 0)
	if !errors.Is(rerr, ErrCodexRefreshRejected) || !errors.Is(rerr, ErrCodexRefreshUnrecoverable) {
		t.Fatalf("err = %v, want ErrCodexRefreshRejected and ErrCodexRefreshUnrecoverable", rerr)
	}
	if res.AccessToken != "" {
		t.Fatal("a rejected refresh returned a token")
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("token endpoint hits = %d, want exactly 1", n)
	}
	if strings.Contains(rerr.Error(), f.prevRefreshToken) {
		t.Fatal("the returned error carries the refresh token")
	}
	assertRejectedAccount(t, env.mustAccount(t, f.userID, f.accountID), 0)

	recs := capt.snapshot()
	found := false
	for _, rec := range recs {
		if rec.msg == codexTimingMsgPhase && rec.attrs["phase"].String() == codexRefreshPhaseOAuthPost {
			found = true
			if got := rec.attrs["oauth_error"].String(); got != codexauth.OAuthCodeRefreshTokenReused {
				t.Fatalf("oauth_error = %q, want %q", got, codexauth.OAuthCodeRefreshTokenReused)
			}
		}
	}
	if !found {
		t.Fatal("oauth_post phase record not captured")
	}
	assertNoCanary(t, recs, f.prevRefreshToken, f.accessToken)
}
