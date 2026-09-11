package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These tests prove the issue #1238 secret-free phase-timing instrumentation: each per-phase
// and the service-total record carries ONLY non-secret fields (operation_id, a fixed phase
// name, elapsed integers, a finite result token), and NO token / refresh token / account id /
// raw error text ever reaches an slog attribute. They are DB-free (no UZI_TEST_DATABASE_URL,
// no build tag): the advanced happy path is driven through advanceCodexRefresh directly with
// an in-memory store fake, so a plain `go test ./internal/workersvc/` exercises them.

// captureRecord is one slog record the capturing handler recorded: its message and a
// snapshot of its attributes as a key->Value map.
type captureRecord struct {
	msg   string
	attrs map[string]slog.Value
}

// captureHandler is a minimal slog.Handler that records every record for later assertion.
// Safe for concurrent use (a background retrier is never wired on these paths, but the
// interface requires it and the mutex costs nothing here).
type captureHandler struct {
	mu      sync.Mutex
	records []captureRecord
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]slog.Value, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, captureRecord{msg: r.Message, attrs: attrs})
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *captureHandler) snapshot() []captureRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]captureRecord, len(h.records))
	copy(out, h.records)
	return out
}

// installCapture swaps the global slog default for a capturing handler and restores the
// prior default on test cleanup. These tests must NOT call t.Parallel — the slog default is
// process-global shared state.
func installCapture(t *testing.T) *captureHandler {
	t.Helper()
	cap := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return cap
}

func keysOf(attrs map[string]slog.Value) []string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// assertKeySet fails unless the record's attribute keys are EXACTLY want (no more, no less).
func assertKeySet(t *testing.T, rec captureRecord, want ...string) {
	t.Helper()
	if len(rec.attrs) != len(want) {
		t.Fatalf("record %q: got keys %v, want exactly %v", rec.msg, keysOf(rec.attrs), want)
	}
	for _, k := range want {
		if _, ok := rec.attrs[k]; !ok {
			t.Fatalf("record %q: missing key %q; have %v", rec.msg, k, keysOf(rec.attrs))
		}
	}
}

// assertNoCanary scans every captured record (message + every attribute value stringified)
// and fails if any canary substring appears.
func assertNoCanary(t *testing.T, recs []captureRecord, canaries ...string) {
	t.Helper()
	for _, rec := range recs {
		hay := rec.msg
		for _, v := range rec.attrs {
			hay += " " + v.String()
		}
		for _, c := range canaries {
			if strings.Contains(hay, c) {
				t.Fatalf("canary %q leaked in record %q (haystack=%q)", c, rec.msg, hay)
			}
		}
	}
}

// instrFakeRefresh is a small local CodexRefreshClient. Refresh drives the callback path;
// DiscoverIdentity remains available for the recovery-only half of the interface.
type instrFakeRefresh struct {
	result     codexauth.RefreshResult
	refreshErr error
	identity   codexauth.Identity
}

func (f *instrFakeRefresh) Refresh(_ context.Context, _ string) (codexauth.RefreshResult, error) {
	if f.refreshErr != nil {
		return codexauth.RefreshResult{}, f.refreshErr
	}
	return f.result, nil
}

func (f *instrFakeRefresh) DiscoverIdentity(_ context.Context, _ string) (codexauth.Identity, error) {
	return f.identity, nil
}

// instrFakeStore implements all 12 codexRefreshStore methods. Only the 5 reached on the
// advanced happy path return success; the other 7 are unreachable here and panic if hit.
type instrFakeStore struct{}

func (instrFakeStore) InsertCodexRefreshIntent(context.Context, store.InsertCodexRefreshIntentParams) (store.CodexRefreshIntent, error) {
	return store.CodexRefreshIntent{}, nil
}

func (instrFakeStore) AcquireCodexRefreshLease(context.Context, store.AcquireCodexRefreshLeaseParams) (int64, error) {
	return int64(1), nil
}

func (instrFakeStore) CommitCodexRefresh(context.Context, store.CommitCodexRefreshParams) (store.CommitCodexRefreshRow, error) {
	return store.CommitCodexRefreshRow{Generation: 1}, nil
}

func (instrFakeStore) SetCodexRefreshIntentState(context.Context, store.SetCodexRefreshIntentStateParams) (int64, error) {
	return int64(1), nil
}

func (instrFakeStore) ResetCodexCoordIdle(context.Context, store.ResetCodexCoordIdleParams) (int64, error) {
	return int64(1), nil
}

func (instrFakeStore) GetCodexProviderAccountByID(context.Context, store.GetCodexProviderAccountByIDParams) (store.CodexProviderAccount, error) {
	panic("unused")
}

func (instrFakeStore) GetCodexRefreshIntent(context.Context, store.GetCodexRefreshIntentParams) (store.CodexRefreshIntent, error) {
	panic("unused")
}

func (instrFakeStore) ListUnresolvedCodexRefreshIntents(context.Context, store.ListUnresolvedCodexRefreshIntentsParams) ([]store.CodexRefreshIntent, error) {
	panic("unused")
}

func (instrFakeStore) SetCodexRecoverySlot(context.Context, store.SetCodexRecoverySlotParams) (int64, error) {
	panic("unused")
}

func (instrFakeStore) QuarantineExpiredCodexLease(context.Context, store.QuarantineExpiredCodexLeaseParams) (int64, error) {
	panic("unused")
}

func (instrFakeStore) QuarantineCodexAccount(context.Context, store.QuarantineCodexAccountParams) (int64, error) {
	panic("unused")
}

func (instrFakeStore) PromoteCodexRecovery(context.Context, store.PromoteCodexRecoveryParams) (int64, error) {
	panic("unused")
}

// newInstrFixture builds the DB-free fixture that drives advanceCodexRefresh: a master box,
// a sealed pre-rotation login carrying canary tokens, and the matching account row.
func newInstrFixture(t *testing.T) (*secretbox.Box, store.CodexProviderAccount) {
	t.Helper()
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	blob := codexLoginBlob{AccessToken: "CANARY_OLD_ACCESS", RefreshToken: "CANARY_REFRESH"} //nolint:gosec // G101: test canary strings, not real credentials — the test asserts they are scrubbed from the timing logs
	raw, err := json.Marshal(blob)                                                           //nolint:gosec // G117: sealing a test-fixture login blob, never a real credential
	if err != nil {
		t.Fatalf("marshal login blob: %v", err)
	}
	sealed, err := box.Seal(raw)
	if err != nil {
		t.Fatalf("seal login blob: %v", err)
	}
	acct := store.CodexProviderAccount{
		SealedLogin:        sealed,
		SealedWith:         store.SealedWithMaster,
		ProviderUserID:     "canary-user",
		WorkspaceAccountID: "CANARY_ACCOUNT_ID",
		Generation:         0,
	}
	return box, acct
}

func TestCodexTimingClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "ok"},
		{"canceled", context.Canceled, "canceled"},
		{"deadline", context.DeadlineExceeded, "deadline"},
		{"other", errors.New("x"), "error"},
	}
	for _, c := range cases {
		if got := CodexTimingResult(c.err); got != c.want {
			t.Errorf("%s: CodexTimingResult()=%q want %q", c.name, got, c.want)
		}
	}
	if got := CodexTimingMS(-time.Millisecond); got != 0 {
		t.Errorf("CodexTimingMS(-1ms)=%d want 0", got)
	}
	if got := CodexTimingMS(1500 * time.Millisecond); got != 1500 {
		t.Errorf("CodexTimingMS(1500ms)=%d want 1500", got)
	}
}

func TestCodexRefreshInstrumentationNoSecretLeak(t *testing.T) {
	cap := installCapture(t)
	box, acct := newInstrFixture(t)
	fake := &instrFakeRefresh{
		result: codexauth.RefreshResult{ //nolint:gosec // G101: test canary string, not a real credential
			AccessToken: "CANARY_NEW_ACCESS",
			IdentityClaims: codexauth.FreshAccessTokenIdentityClaims{
				ChatGPTAccountID: "CANARY_ACCOUNT_ID",
				ChatGPTUserID:    "canary-user",
				AuthUserID:       "canary-user",
			},
		},
	}
	svc := &Service{box: box, codexRefresh: fake}

	res, aerr := svc.advanceCodexRefresh(
		context.Background(), instrFakeStore{},
		uuid.New(), uuid.New(), uuid.New(),
		acct, time.Now().Add(time.Hour), time.Now().Add(time.Hour),
	)
	if aerr != nil {
		t.Fatalf("advanceCodexRefresh: %v", aerr)
	}
	if res.Outcome != CodexRefreshAdvanced {
		t.Fatalf("outcome=%v want CodexRefreshAdvanced", res.Outcome)
	}
	if res.AccessToken != "CANARY_NEW_ACCESS" {
		t.Fatalf("access token=%q want CANARY_NEW_ACCESS", res.AccessToken)
	}

	recs := cap.snapshot()
	wantPhases := map[string]bool{
		codexRefreshPhaseOAuthPost: false,
		codexRefreshPhaseCommit:    false,
	}
	for _, rec := range recs {
		if rec.msg != codexTimingMsgPhase {
			continue
		}
		phase := rec.attrs["phase"].String()
		if _, ok := wantPhases[phase]; !ok {
			t.Fatalf("unexpected phase %q", phase)
		}
		wantPhases[phase] = true
		assertKeySet(t, rec, "operation_id", "phase", "elapsed_ms", "result")
		if rec.attrs["elapsed_ms"].Kind() != slog.KindInt64 {
			t.Fatalf("phase %q: elapsed_ms kind=%v want Int64", phase, rec.attrs["elapsed_ms"].Kind())
		}
		if rec.attrs["elapsed_ms"].Int64() < 0 {
			t.Fatalf("phase %q: elapsed_ms=%d want >= 0", phase, rec.attrs["elapsed_ms"].Int64())
		}
		if got := rec.attrs["result"].String(); got != "ok" {
			t.Fatalf("phase %q: result=%q want ok", phase, got)
		}
	}
	for phase, seen := range wantPhases {
		if !seen {
			t.Fatalf("phase %q record not captured", phase)
		}
	}

	assertNoCanary(t, recs, "CANARY_OLD_ACCESS", "CANARY_NEW_ACCESS", "CANARY_REFRESH", "CANARY_ACCOUNT_ID")
}

func TestCodexRefreshInstrumentationProviderError(t *testing.T) {
	cap := installCapture(t)
	box, acct := newInstrFixture(t)
	fake := &instrFakeRefresh{
		refreshErr: errors.New("provider host=chatgpt.com token=CANARY_PROVIDER_ERR 401"),
	}
	svc := &Service{box: box, codexRefresh: fake}

	_, aerr := svc.advanceCodexRefresh(
		context.Background(), instrFakeStore{},
		uuid.New(), uuid.New(), uuid.New(),
		acct, time.Now().Add(time.Hour), time.Now().Add(time.Hour),
	)
	if aerr == nil {
		t.Fatalf("advanceCodexRefresh: expected a non-nil error on provider failure")
	}

	recs := cap.snapshot()
	found := false
	for _, rec := range recs {
		if rec.msg == codexTimingMsgPhase && rec.attrs["phase"].String() == codexRefreshPhaseOAuthPost {
			found = true
			if got := rec.attrs["result"].String(); got != "error" {
				t.Fatalf("oauth_post: result=%q want error", got)
			}
		}
	}
	if !found {
		t.Fatalf("oauth_post phase record not captured")
	}

	assertNoCanary(t, recs, "CANARY_PROVIDER_ERR")
}

func TestCodexRefreshServiceTotalScrubsError(t *testing.T) {
	cap := installCapture(t)
	logCodexRefreshService(uuid.New(), time.Now().Add(-1500*time.Millisecond), errors.New("boom CANARY_SVC host=x"))

	recs := cap.snapshot()
	var svc []captureRecord
	for _, rec := range recs {
		if rec.msg == codexTimingMsgService {
			svc = append(svc, rec)
		}
	}
	if len(svc) != 1 {
		t.Fatalf("got %d service records, want exactly 1", len(svc))
	}
	rec := svc[0]
	assertKeySet(t, rec, "operation_id", "service_total_ms", "result")
	if got := rec.attrs["result"].String(); got != "error" {
		t.Fatalf("service: result=%q want error", got)
	}
	if rec.attrs["service_total_ms"].Kind() != slog.KindInt64 {
		t.Fatalf("service_total_ms kind=%v want Int64", rec.attrs["service_total_ms"].Kind())
	}
	if rec.attrs["service_total_ms"].Int64() < 0 {
		t.Fatalf("service_total_ms=%d want >= 0", rec.attrs["service_total_ms"].Int64())
	}
	assertNoCanary(t, recs, "CANARY_SVC")
}
