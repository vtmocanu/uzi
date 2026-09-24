package workersvc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1594 M3: the status-by-code matrix for the provider-rejection exception in the
// coordinated refresh. DB-free: coordinatedRefresh and the poll path's
// handleCodexUsageUnauthorized run against an in-memory store fake, a counting refresh
// client that returns a configured error, and a RECORDING rejector injected through
// Service.codexRejector. Only an explicit material rejection (400/401 with one of the four
// rejection codes) may reach the rejector; everything else must stay the ambiguous outcome.

// rejFakeStore is the in-memory store for the matrix: an idle account at generation 0 with
// no prior intent, a lease that is always acquired, and every write recorded as a no-op. It
// embeds the (nil) broad Store only so it can sit in Service.q; the narrow surfaces the
// refresh and poll paths use are all implemented here, so the nil embed is never reached.
type rejFakeStore struct {
	Store
	acct       store.CodexProviderAccount
	markReauth int
}

func (f *rejFakeStore) GetCodexProviderAccountByID(context.Context, store.GetCodexProviderAccountByIDParams) (store.CodexProviderAccount, error) {
	return f.acct, nil
}

func (f *rejFakeStore) GetCodexRefreshIntent(context.Context, store.GetCodexRefreshIntentParams) (store.CodexRefreshIntent, error) {
	return store.CodexRefreshIntent{}, pgx.ErrNoRows
}

func (f *rejFakeStore) InsertCodexRefreshIntent(context.Context, store.InsertCodexRefreshIntentParams) (store.CodexRefreshIntent, error) {
	return store.CodexRefreshIntent{}, nil
}

func (f *rejFakeStore) SetCodexRefreshIntentState(context.Context, store.SetCodexRefreshIntentStateParams) (int64, error) {
	return 0, errors.New("rejFakeStore: unexpected SetCodexRefreshIntentState")
}

func (f *rejFakeStore) SetCodexRefreshIntentStateFenced(context.Context, store.SetCodexRefreshIntentStateFencedParams) (int64, error) {
	return 0, errors.New("rejFakeStore: unexpected SetCodexRefreshIntentStateFenced")
}

func (f *rejFakeStore) ClearMismatchedCodexRecoveryAndMarkIntents(context.Context, store.ClearMismatchedCodexRecoveryAndMarkIntentsParams) (store.ClearMismatchedCodexRecoveryAndMarkIntentsRow, error) {
	return store.ClearMismatchedCodexRecoveryAndMarkIntentsRow{}, errors.New("rejFakeStore: unexpected ClearMismatchedCodexRecoveryAndMarkIntents")
}

func (f *rejFakeStore) ListUnresolvedCodexRefreshIntents(context.Context, store.ListUnresolvedCodexRefreshIntentsParams) ([]store.CodexRefreshIntent, error) {
	return nil, nil
}

func (f *rejFakeStore) AcquireCodexRefreshLease(context.Context, store.AcquireCodexRefreshLeaseParams) (int64, error) {
	return 1, nil
}

func (f *rejFakeStore) CommitCodexRefresh(context.Context, store.CommitCodexRefreshParams) (store.CommitCodexRefreshRow, error) {
	return store.CommitCodexRefreshRow{}, errors.New("rejFakeStore: unexpected CommitCodexRefresh")
}

func (f *rejFakeStore) ResetCodexCoordIdle(context.Context, store.ResetCodexCoordIdleParams) (int64, error) {
	return 0, errors.New("rejFakeStore: unexpected ResetCodexCoordIdle")
}

func (f *rejFakeStore) SetCodexRecoverySlot(context.Context, store.SetCodexRecoverySlotParams) (int64, error) {
	return 0, errors.New("rejFakeStore: unexpected SetCodexRecoverySlot")
}

func (f *rejFakeStore) QuarantineExpiredCodexLease(context.Context, store.QuarantineExpiredCodexLeaseParams) (int64, error) {
	return 0, nil
}

func (f *rejFakeStore) QuarantineCodexAccount(context.Context, store.QuarantineCodexAccountParams) (int64, error) {
	return 0, errors.New("rejFakeStore: unexpected QuarantineCodexAccount")
}

func (f *rejFakeStore) PromoteCodexRecovery(context.Context, store.PromoteCodexRecoveryParams) (int64, error) {
	return 0, errors.New("rejFakeStore: unexpected PromoteCodexRecovery")
}

func (f *rejFakeStore) CountLinkedAliasesForCodexAccount(context.Context, store.CountLinkedAliasesForCodexAccountParams) (int64, error) {
	return 1, nil
}

func (f *rejFakeStore) MarkCodexReauthRequired(context.Context, store.MarkCodexReauthRequiredParams) (int64, error) {
	f.markReauth++
	return 1, nil
}

// rejFakeRefresh counts provider exchanges and returns the configured error.
type rejFakeRefresh struct {
	calls     int
	err       error
	onRefresh func()
}

func (f *rejFakeRefresh) Refresh(context.Context, string) (codexauth.RefreshResult, error) {
	f.calls++
	if f.onRefresh != nil {
		f.onRefresh()
	}
	return codexauth.RefreshResult{}, f.err
}

func (f *rejFakeRefresh) DiscoverIdentity(context.Context, string) (codexauth.Identity, error) {
	return codexauth.Identity{}, errors.New("rejFakeRefresh: unexpected DiscoverIdentity")
}

// recordingRejector records every rejection transition and answers with a configured
// outcome/error. ctxErrs captures the context state each call saw.
type recordingRejector struct {
	outcome store.CodexRejectionOutcome
	err     error
	calls   []store.QuarantineRejectedCodexRefreshParams
	ctxErrs []error
}

func (r *recordingRejector) QuarantineRejectedCodexRefresh(ctx context.Context, arg store.QuarantineRejectedCodexRefreshParams) (store.CodexRejectionOutcome, error) {
	r.calls = append(r.calls, arg)
	r.ctxErrs = append(r.ctxErrs, ctx.Err())
	return r.outcome, r.err
}

type rejHarness struct {
	svc       *Service
	st        *rejFakeStore
	client    *rejFakeRefresh
	rejector  *recordingRejector
	userID    uuid.UUID
	accountID uuid.UUID
}

func newRejHarness(t *testing.T, providerErr error, rejector *recordingRejector) rejHarness {
	t.Helper()
	box, acct := newInstrFixture(t)
	userID, accountID := uuid.New(), uuid.New()
	acct.ID = accountID
	acct.UserID = userID
	acct.CoordState = codexCoordIdle
	st := &rejFakeStore{acct: acct}
	client := &rejFakeRefresh{err: providerErr}
	svc := &Service{q: st, box: box, codexRefresh: client}
	if rejector != nil {
		svc.codexRejector = rejector
	}
	return rejHarness{svc: svc, st: st, client: client, rejector: rejector, userID: userID, accountID: accountID}
}

func (h rejHarness) refresh(ctx context.Context, op uuid.UUID) (CodexRefreshResult, error) {
	return h.svc.coordinatedRefresh(ctx, h.userID, h.accountID, op, 0, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
}

func (h rejHarness) poll() *CodexUsageFailure {
	principal := codexPollPrincipal{userID: h.userID, accountID: h.accountID, generation: 0}
	_, _, f := h.svc.handleCodexUsageUnauthorized(context.Background(), h.st, nil, principal)
	return f
}

func refreshAuthErr(status int, code string) error {
	return &codexauth.AuthError{Op: "refresh", StatusCode: status, OAuthCode: code}
}

var codexRejectionCodes = []string{
	codexauth.OAuthCodeRefreshTokenExpired,
	codexauth.OAuthCodeRefreshTokenReused,
	codexauth.OAuthCodeRefreshTokenInvalidated,
	codexauth.OAuthCodeInvalidGrant,
}

// assertRejectedApplied checks the applied-rejection contract: both sentinels, the
// quarantined outcome, no token.
func assertRejectedApplied(t *testing.T, res CodexRefreshResult, err error) {
	t.Helper()
	if !errors.Is(err, ErrCodexRefreshRejected) || !errors.Is(err, ErrCodexRefreshUnrecoverable) {
		t.Fatalf("err = %v, want both ErrCodexRefreshRejected and ErrCodexRefreshUnrecoverable", err)
	}
	if res.Outcome != CodexRefreshQuarantined {
		t.Fatalf("outcome = %v, want CodexRefreshQuarantined", res.Outcome)
	}
	if res.AccessToken != "" {
		t.Fatal("a rejected refresh returned an access token")
	}
}

// assertAmbiguous checks today's ambiguous provider-failure contract.
func assertAmbiguous(t *testing.T, res CodexRefreshResult, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want the ambiguous provider-exchange error")
	}
	if errors.Is(err, ErrCodexRefreshRejected) || errors.Is(err, ErrCodexRefreshUnrecoverable) || errors.Is(err, ErrCodexRefreshQuarantined) {
		t.Fatalf("err = %v, want the ambiguous error (not rejected/unrecoverable/quarantined)", err)
	}
	if !strings.Contains(err.Error(), "provider exchange") {
		t.Fatalf("err = %v, want the provider-exchange error", err)
	}
	if res.Outcome != CodexRefreshContended {
		t.Fatalf("outcome = %v, want CodexRefreshContended", res.Outcome)
	}
	if res.AccessToken != "" {
		t.Fatal("an ambiguous refresh returned an access token")
	}
}

func TestCodexRefreshRejectionMatrixApplied(t *testing.T) {
	for _, code := range codexRejectionCodes {
		for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
			t.Run(fmt.Sprintf("%s/%d", code, status), func(t *testing.T) {
				capt := installCapture(t)
				rej := &recordingRejector{outcome: store.CodexRejectionApplied}
				h := newRejHarness(t, refreshAuthErr(status, code), rej)
				op := uuid.New()

				res, err := h.refresh(context.Background(), op)
				assertRejectedApplied(t, res, err)
				if h.client.calls != 1 {
					t.Fatalf("provider calls = %d, want exactly 1", h.client.calls)
				}
				if len(rej.calls) != 1 {
					t.Fatalf("rejector calls = %d, want exactly 1", len(rej.calls))
				}
				want := store.QuarantineRejectedCodexRefreshParams{UserID: h.userID, AccountID: h.accountID, OperationID: op, FromGeneration: 0}
				if rej.calls[0] != want {
					t.Fatalf("rejector params = %+v, want %+v", rej.calls[0], want)
				}

				// The oauth_post phase record names the closed-set code.
				found := false
				for _, rec := range capt.snapshot() {
					if rec.msg == codexTimingMsgPhase && rec.attrs["phase"].String() == codexRefreshPhaseOAuthPost {
						found = true
						if got := rec.attrs["oauth_error"].String(); got != code {
							t.Fatalf("oauth_error = %q, want %q", got, code)
						}
					}
				}
				if !found {
					t.Fatal("oauth_post phase record not captured")
				}

				// The poll path reports reauth-required and writes nothing itself.
				rej2 := &recordingRejector{outcome: store.CodexRejectionApplied}
				hp := newRejHarness(t, refreshAuthErr(status, code), rej2)
				f := hp.poll()
				if f == nil || f.Kind != CodexUsageFailReauthRequired {
					t.Fatalf("poll failure = %v, want reauth_required", f)
				}
				if len(rej2.calls) != 1 || hp.client.calls != 1 {
					t.Fatalf("poll: rejector calls = %d, provider calls = %d, want 1 and 1", len(rej2.calls), hp.client.calls)
				}
				if hp.st.markReauth != 0 {
					t.Fatalf("poll called MarkCodexReauthRequired %d times, want 0 (the transaction set it)", hp.st.markReauth)
				}
			})
		}
	}
}

func TestCodexRefreshRejectionMatrixNotApplied(t *testing.T) {
	transitions := []struct {
		name string
		rej  *recordingRejector
	}{
		{"not applied", &recordingRejector{outcome: store.CodexRejectionNotApplied}},
		{"error", &recordingRejector{outcome: store.CodexRejectionNotApplied, err: errors.New("db down")}},
		// An error paired with Applied must still be treated as not applied.
		{"error with applied", &recordingRejector{outcome: store.CodexRejectionApplied, err: errors.New("db down")}},
	}
	for _, tr := range transitions {
		for _, code := range codexRejectionCodes {
			for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
				t.Run(fmt.Sprintf("%s/%s/%d", tr.name, code, status), func(t *testing.T) {
					rej := &recordingRejector{outcome: tr.rej.outcome, err: tr.rej.err}
					h := newRejHarness(t, refreshAuthErr(status, code), rej)
					res, err := h.refresh(context.Background(), uuid.New())
					assertAmbiguous(t, res, err)
					if len(rej.calls) != 1 || h.client.calls != 1 {
						t.Fatalf("rejector calls = %d, provider calls = %d, want 1 and 1", len(rej.calls), h.client.calls)
					}

					rejp := &recordingRejector{outcome: tr.rej.outcome, err: tr.rej.err}
					hp := newRejHarness(t, refreshAuthErr(status, code), rejp)
					if f := hp.poll(); f == nil || f.Kind != CodexUsageFailTransient {
						t.Fatalf("poll failure = %v, want transient", f)
					}
					if hp.st.markReauth != 0 {
						t.Fatalf("poll set reauth %d times on a not-applied rejection, want 0", hp.st.markReauth)
					}
				})
			}
		}
	}
}

func TestCodexRefreshRejectionMatrixAmbiguous(t *testing.T) {
	type row struct {
		name string
		err  error
	}
	var rows []row
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		for _, code := range []string{
			codexauth.OAuthCodeUnauthorizedClient,
			codexauth.OAuthCodeInvalidClient,
			codexauth.OAuthCodeInvalidRequest,
			codexauth.OAuthCodeUnsupportedGrantType,
			codexauth.OAuthCodeInvalidScope,
			codexauth.OAuthCodeUnknown,
			"",
		} {
			rows = append(rows, row{fmt.Sprintf("%d/%q", status, code), refreshAuthErr(status, code)})
		}
	}
	for _, code := range codexRejectionCodes {
		rows = append(rows, row{fmt.Sprintf("403/%s", code), refreshAuthErr(http.StatusForbidden, code)})
		rows = append(rows, row{fmt.Sprintf("500/%s", code), refreshAuthErr(http.StatusInternalServerError, code)})
		// A rejection code on a non-refresh op does not prove anything about refresh material.
		rows = append(rows, row{fmt.Sprintf("op-usage/401/%s", code), &codexauth.AuthError{Op: "usage", StatusCode: http.StatusUnauthorized, OAuthCode: code}})
	}
	rows = append(rows,
		row{"500", refreshAuthErr(http.StatusInternalServerError, "")},
		row{"503", refreshAuthErr(http.StatusServiceUnavailable, "")},
		row{"429", refreshAuthErr(http.StatusTooManyRequests, "")},
		row{"transport", errors.New("dial tcp: connection refused")},
		row{"deadline", context.DeadlineExceeded},
	)

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			rej := &recordingRejector{outcome: store.CodexRejectionApplied}
			h := newRejHarness(t, r.err, rej)
			res, err := h.refresh(context.Background(), uuid.New())
			assertAmbiguous(t, res, err)
			if h.client.calls != 1 {
				t.Fatalf("provider calls = %d, want exactly 1", h.client.calls)
			}
			if len(rej.calls) != 0 {
				t.Fatalf("rejector called %d times on an ambiguous failure, want 0", len(rej.calls))
			}

			rejp := &recordingRejector{outcome: store.CodexRejectionApplied}
			hp := newRejHarness(t, r.err, rejp)
			if f := hp.poll(); f == nil || f.Kind != CodexUsageFailTransient {
				t.Fatalf("poll failure = %v, want transient", f)
			}
			if len(rejp.calls) != 0 || hp.st.markReauth != 0 {
				t.Fatalf("poll: rejector calls = %d, reauth marks = %d, want 0 and 0", len(rejp.calls), hp.st.markReauth)
			}
		})
	}
}

// TestCodexRefreshRejectionWrappedAuthError proves the gate classifies through wrapping
// (errors.As), so a client that wraps its *AuthError still reaches the rejector.
func TestCodexRefreshRejectionWrappedAuthError(t *testing.T) {
	rej := &recordingRejector{outcome: store.CodexRejectionApplied}
	h := newRejHarness(t, fmt.Errorf("refresh call: %w", refreshAuthErr(http.StatusUnauthorized, codexauth.OAuthCodeRefreshTokenReused)), rej)
	res, err := h.refresh(context.Background(), uuid.New())
	assertRejectedApplied(t, res, err)
	if len(rej.calls) != 1 {
		t.Fatalf("rejector calls = %d, want 1", len(rej.calls))
	}
}

// TestCodexRefreshRejectionSurvivesCancelledContext proves the rejection is recorded on a
// detached context: a request cancelled right after the provider replied still records it.
func TestCodexRefreshRejectionSurvivesCancelledContext(t *testing.T) {
	rej := &recordingRejector{outcome: store.CodexRejectionApplied}
	h := newRejHarness(t, refreshAuthErr(http.StatusUnauthorized, codexauth.OAuthCodeRefreshTokenReused), rej)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.client.onRefresh = cancel

	res, err := h.refresh(ctx, uuid.New())
	assertRejectedApplied(t, res, err)
	if len(rej.calls) != 1 {
		t.Fatalf("rejector calls = %d, want 1", len(rej.calls))
	}
	if rej.ctxErrs[0] != nil {
		t.Fatalf("rejector saw a cancelled context (%v), want a detached live one", rej.ctxErrs[0])
	}
}

// TestCodexRefreshRejectionUnwiredIsAmbiguous proves the fail-closed default: with no
// injected rejector and no transaction beginner, a material rejection applies nothing and
// stays ambiguous.
func TestCodexRefreshRejectionUnwiredIsAmbiguous(t *testing.T) {
	h := newRejHarness(t, refreshAuthErr(http.StatusUnauthorized, codexauth.OAuthCodeRefreshTokenReused), nil)
	res, err := h.refresh(context.Background(), uuid.New())
	assertAmbiguous(t, res, err)
	if h.client.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", h.client.calls)
	}
}

// TestCodexRefreshPhaseLogOAuthErrorOnlyWhenPresent proves the oauth_post record gains the
// oauth_error attribute only for an AuthError carrying a code, and never otherwise.
func TestCodexRefreshPhaseLogOAuthErrorOnlyWhenPresent(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string // "" = attribute absent
	}{
		{"code", refreshAuthErr(http.StatusBadRequest, codexauth.OAuthCodeInvalidClient), codexauth.OAuthCodeInvalidClient},
		{"no code", refreshAuthErr(http.StatusUnauthorized, ""), ""},
		{"transport", errors.New("boom"), ""},
		{"ok", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			capt := installCapture(t)
			logCodexRefreshPhase(uuid.New(), codexRefreshPhaseOAuthPost, time.Now(), c.err)
			recs := capt.snapshot()
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			v, ok := recs[0].attrs["oauth_error"]
			if c.want == "" {
				if ok {
					t.Fatalf("oauth_error = %q, want absent", v.String())
				}
				assertKeySet(t, recs[0], "operation_id", "phase", "elapsed_ms", "result")
				return
			}
			if !ok || v.String() != c.want {
				t.Fatalf("oauth_error = %v (present=%v), want %q", v, ok, c.want)
			}
			assertKeySet(t, recs[0], "operation_id", "phase", "elapsed_ms", "result", "oauth_error")
		})
	}
}
