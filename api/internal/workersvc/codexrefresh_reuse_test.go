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
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

type reuseReceiptStore struct {
	*rejFakeStore
	calls              int
	arg                store.RecordCodexRefreshProviderErrorParams
	ctxErr             error
	remaining          time.Duration
	rows               int64
	err                error
	writesAfterReceipt []string
}

func (f *reuseReceiptStore) RecordCodexRefreshProviderError(ctx context.Context, arg store.RecordCodexRefreshProviderErrorParams) (int64, error) {
	f.calls++
	f.arg = arg
	f.ctxErr = ctx.Err()
	deadline, ok := ctx.Deadline()
	if ok {
		f.remaining = time.Until(deadline)
	}
	return f.rows, f.err
}

func (f *reuseReceiptStore) wrote(name string) {
	if f.calls != 0 {
		f.writesAfterReceipt = append(f.writesAfterReceipt, name)
	}
}

func (f *reuseReceiptStore) InsertCodexRefreshIntent(ctx context.Context, arg store.InsertCodexRefreshIntentParams) (store.CodexRefreshIntent, error) {
	f.wrote("insert_intent")
	return f.rejFakeStore.InsertCodexRefreshIntent(ctx, arg)
}
func (f *reuseReceiptStore) AcquireCodexRefreshLease(ctx context.Context, arg store.AcquireCodexRefreshLeaseParams) (int64, error) {
	f.wrote("acquire_lease")
	return f.rejFakeStore.AcquireCodexRefreshLease(ctx, arg)
}
func (f *reuseReceiptStore) SetCodexRefreshIntentState(ctx context.Context, arg store.SetCodexRefreshIntentStateParams) (int64, error) {
	f.wrote("intent_state")
	return f.rejFakeStore.SetCodexRefreshIntentState(ctx, arg)
}
func (f *reuseReceiptStore) SetCodexRefreshIntentStateFenced(ctx context.Context, arg store.SetCodexRefreshIntentStateFencedParams) (int64, error) {
	f.wrote("fenced_intent_state")
	return f.rejFakeStore.SetCodexRefreshIntentStateFenced(ctx, arg)
}
func (f *reuseReceiptStore) CommitCodexRefresh(ctx context.Context, arg store.CommitCodexRefreshParams) (store.CommitCodexRefreshRow, error) {
	f.wrote("commit_refresh")
	return f.rejFakeStore.CommitCodexRefresh(ctx, arg)
}
func (f *reuseReceiptStore) SetCodexRecoverySlot(ctx context.Context, arg store.SetCodexRecoverySlotParams) (int64, error) {
	f.wrote("recovery_slot")
	return f.rejFakeStore.SetCodexRecoverySlot(ctx, arg)
}
func (f *reuseReceiptStore) QuarantineCodexAccount(ctx context.Context, arg store.QuarantineCodexAccountParams) (int64, error) {
	f.wrote("quarantine")
	return f.rejFakeStore.QuarantineCodexAccount(ctx, arg)
}

func TestCodexReuseReceiptErrorClasses(t *testing.T) {
	for _, lane := range []string{"callback", "usage401"} {
		for _, tc := range []struct {
			name string
			err  error
		}{
			{"deadline", context.DeadlineExceeded},
			{"server_error", refreshAuthErr(503, "")},
			{"rate_limited", refreshAuthErr(429, "")},
			{"malformed_success", &json.SyntaxError{Offset: 1}},
		} {
			t.Run(lane+"/"+tc.name, func(t *testing.T) {
				h := newRejHarness(t, tc.err, nil)
				receipts := &reuseReceiptStore{rejFakeStore: h.st, rows: 1}
				h.svc.q = receipts
				if lane == "callback" {
					res, err := h.refresh(context.Background(), uuid.New())
					assertAmbiguous(t, res, err)
				} else {
					principal := codexPollPrincipal{userID: h.userID, accountID: h.accountID, generation: 0}
					if _, _, failure := h.svc.handleCodexUsageUnauthorized(context.Background(), receipts, nil, principal); failure == nil {
						t.Fatal("usage rotation unexpectedly succeeded")
					}
				}
				if receipts.calls != 1 || h.client.calls != 1 {
					t.Fatalf("receipt calls=%d provider calls=%d", receipts.calls, h.client.calls)
				}
				if len(receipts.writesAfterReceipt) != 0 {
					t.Fatalf("writes after receipt: %v", receipts.writesAfterReceipt)
				}
			})
		}
	}
}

func TestCodexReuseProviderReceipt(t *testing.T) {
	for _, lane := range []string{"callback", "usage401"} {
		for _, result := range []string{"receipt", "write_error", "fence_miss", "rejected_applied", "rejected_error", "rejected_noop"} {
			t.Run(lane+"/"+result, func(t *testing.T) {
				providerErr := errors.New("transport ambiguous")
				rej := &recordingRejector{}
				switch result {
				case "rejected_applied":
					providerErr = refreshAuthErr(400, codexauth.OAuthCodeInvalidGrant)
					rej.outcome = store.CodexRejectionApplied
				case "rejected_error":
					providerErr = refreshAuthErr(401, codexauth.OAuthCodeRefreshTokenExpired)
					rej.err = errors.New("rejection write failed")
				case "rejected_noop":
					providerErr = refreshAuthErr(400, codexauth.OAuthCodeInvalidGrant)
				}
				h := newRejHarness(t, providerErr, rej)
				receipts := &reuseReceiptStore{rejFakeStore: h.st, rows: 1}
				if result == "write_error" {
					receipts.err = errors.New("receipt failed")
				}
				if result == "fence_miss" {
					receipts.rows = 0
				}
				h.svc.q = receipts
				op := uuid.New()
				if lane == "callback" {
					res, err := h.refresh(context.Background(), op)
					if result == "rejected_applied" {
						assertRejectedApplied(t, res, err)
					} else {
						assertAmbiguous(t, res, err)
					}
				} else {
					principal := codexPollPrincipal{userID: h.userID, accountID: h.accountID, generation: 0}
					_, _, failure := h.svc.handleCodexUsageUnauthorized(context.Background(), receipts, nil, principal)
					if failure == nil {
						t.Fatal("usage401 unexpectedly succeeded")
					}
				}
				wantReceipt := result == "receipt" || result == "write_error" || result == "fence_miss"
				if (receipts.calls == 1) != wantReceipt || receipts.calls > 1 {
					t.Fatalf("receipt calls = %d, want receipt %v", receipts.calls, wantReceipt)
				}
				if receipts.calls != 0 {
					if receipts.arg.UserID != h.userID || receipts.arg.AccountID != h.accountID || receipts.arg.FromGeneration != 0 {
						t.Fatalf("incorrect receipt fence: %+v", receipts.arg)
					}
					if lane == "callback" && receipts.arg.OperationID != op {
						t.Fatal("wrong operation receipt")
					}
					if receipts.ctxErr != nil || receipts.remaining <= 0 || receipts.remaining > codexRecoverySlotWriteTimeout {
						t.Fatal("receipt context is not bounded and live")
					}
				}
				if h.client.calls != 1 {
					t.Fatalf("provider calls=%d", h.client.calls)
				}
			})
		}
	}
}

func TestCodexReuseReceiptDetached(t *testing.T) {
	h := newRejHarness(t, errors.New("transport"), nil)
	f := &reuseReceiptStore{rejFakeStore: h.st, rows: 1}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.svc.recordCodexRefreshProviderError(ctx, f, store.RecordCodexRefreshProviderErrorParams{})
	if f.calls != 1 || f.ctxErr != nil || f.remaining <= 0 || f.remaining > codexRecoverySlotWriteTimeout {
		t.Fatalf("receipt did not use detached bounded context: %+v", f)
	}
}

func TestCodexReuseAdmissionPrechecks(t *testing.T) {
	for _, kind := range []string{"ready", "missing_vault", "locked", "corrupt", "empty_refresh", "generation_moved", "store_denied", "store_error"} {
		t.Run(kind, func(t *testing.T) {
			box, acct := newInstrFixture(t)
			acct.UserID, acct.ID = uuid.New(), uuid.New()
			svc := &Service{box: box}
			svc.vlt = vault.New(box, newMemVaultStore())
			if kind != "locked" {
				if err := svc.vlt.Unlock(context.Background(), acct.UserID, "test-password"); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "missing_vault" {
				svc.vlt = nil
			}
			if kind == "corrupt" {
				acct.SealedLogin = []byte("corrupt")
			}
			if kind == "empty_refresh" {
				raw, err := json.Marshal(codexLoginBlob{AccessToken: "access"}) //nolint:gosec // G117: synthetic fixture
				if err != nil {
					t.Fatal(err)
				}
				acct.SealedLogin, err = box.Seal(raw)
				if err != nil {
					t.Fatal(err)
				}
			}
			it := store.CodexRefreshIntent{UserID: acct.UserID, ProviderAccountID: acct.ID, OperationID: uuid.New(), FromGeneration: acct.Generation}
			if kind == "generation_moved" {
				it.FromGeneration--
			}
			calls := 0
			login, ok, err := svc.admitCodexRefreshReuse(context.Background(), acct, it, func(_ context.Context, arg store.CodexRefreshReuseParams) (bool, error) {
				calls++
				if arg.UserID != acct.UserID || arg.AccountID != acct.ID || arg.OperationID != it.OperationID || arg.FromGeneration != acct.Generation {
					t.Fatal("wrong admission fence")
				}
				if kind == "store_error" {
					return false, errors.New("db down")
				}
				return kind != "store_denied", nil
			})
			wantCall := kind == "ready" || kind == "store_denied" || kind == "store_error"
			if (calls == 1) != wantCall {
				t.Fatalf("admission calls=%d", calls)
			}
			if kind == "ready" {
				if err != nil || !ok || login.RefreshToken != "CANARY_REFRESH" {
					t.Fatal("ready login not admitted")
				}
			} else if ok || login.RefreshToken != "" {
				t.Fatal("failed admission released login")
			}
		})
	}
}

func TestCodexReuseResolution(t *testing.T) {
	now := time.Now()
	stamp := func(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
	for _, tc := range []struct {
		name             string
		created, attempt time.Duration
		consumed         bool
		generation       int64
		recovery         bool
		want             string
	}{
		{"eligible", -time.Minute, 0, false, 0, false, codexIntentPending},
		{"expired", -30 * time.Minute, 0, false, 0, false, codexIntentUnrecoverable},
		{"inflight", -time.Hour, -time.Second, true, 0, false, codexIntentPending},
		{"spent", -time.Minute, -codexRefreshReuseInFlightBound, true, 0, false, codexIntentUnrecoverable},
		{"advanced", -time.Minute, 0, false, 1, false, codexIntentReconciled},
		{"recovery", -time.Minute, 0, false, 0, true, codexIntentReconciled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acct := store.CodexProviderAccount{Generation: tc.generation, CoordState: "quarantined"}
			if tc.recovery {
				acct.RecoverySealed = []byte("sealed")
				acct.RecoveryGeneration = pgtype.Int8{Int64: 0, Valid: true}
			}
			it := store.CodexRefreshIntent{ProviderErrorAt: stamp(now), CreatedAt: stamp(now.Add(tc.created))}
			if tc.consumed {
				it.ReuseAttemptedAt = stamp(now.Add(tc.attempt))
			}
			if got := codexRefreshResolution(now, acct, it); got != tc.want {
				t.Fatalf("resolution=%q want %q", got, tc.want)
			}
		})
	}
	if codexRefreshReuseInFlightBound != 52140*time.Millisecond {
		t.Fatalf("policy bound changed: %v; review actual retry schedule", codexRefreshReuseInFlightBound)
	}
}
