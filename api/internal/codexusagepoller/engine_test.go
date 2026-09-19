package codexusagepoller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// --- fakes ---------------------------------------------------------------------------------

// fakeStore records every write and serves canned listings. Concurrency-safe (the poll phase
// fans out). upsertRows/failureRows model the revision fence: 0 = "authority moved, discarded".
type fakeStore struct {
	mu sync.Mutex

	staged        []store.ListStagedCodexAliasesRow
	stagedForUser map[uuid.UUID][]store.ListStagedCodexAliasesForUserRow
	toPoll        []store.ListLinkedCodexAccountsToPollRow
	forUser       map[uuid.UUID][]store.ListLinkedCodexAccountsForUserRow

	upsertRows  int64
	failureRows int64

	upserts  []store.UpsertCodexAccountRateLimitsParams
	failures []store.RecordCodexAccountPollFailureParams
	order    []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		stagedForUser: map[uuid.UUID][]store.ListStagedCodexAliasesForUserRow{},
		forUser:       map[uuid.UUID][]store.ListLinkedCodexAccountsForUserRow{},
		upsertRows:    1,
		failureRows:   1,
	}
}

func (s *fakeStore) ListStagedCodexAliases(context.Context) ([]store.ListStagedCodexAliasesRow, error) {
	return s.staged, nil
}
func (s *fakeStore) ListStagedCodexAliasesForUser(_ context.Context, u uuid.UUID) ([]store.ListStagedCodexAliasesForUserRow, error) {
	return s.stagedForUser[u], nil
}
func (s *fakeStore) ListLinkedCodexAccountsToPoll(context.Context) ([]store.ListLinkedCodexAccountsToPollRow, error) {
	return s.toPoll, nil
}
func (s *fakeStore) ListLinkedCodexAccountsForUser(_ context.Context, u uuid.UUID) ([]store.ListLinkedCodexAccountsForUserRow, error) {
	return s.forUser[u], nil
}
func (s *fakeStore) UpsertCodexAccountRateLimits(_ context.Context, arg store.UpsertCodexAccountRateLimitsParams) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts = append(s.upserts, arg)
	s.order = append(s.order, "poll:"+arg.ProviderAccountID.String())
	return s.upsertRows, nil
}
func (s *fakeStore) RecordCodexAccountPollFailure(_ context.Context, arg store.RecordCodexAccountPollFailureParams) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, arg)
	s.order = append(s.order, "poll:"+arg.ProviderAccountID.String())
	return s.failureRows, nil
}

// fakeCollector returns a canned reading/failure per account and records the calls.
type fakeCollector struct {
	mu      sync.Mutex
	calls   []uuid.UUID
	results map[uuid.UUID]collectResult
}

type collectResult struct {
	reading workersvc.CodexUsageReading
	err     error
}

func (c *fakeCollector) CollectCodexAccountUsage(_ context.Context, _ uuid.UUID, accountID uuid.UUID) (workersvc.CodexUsageReading, error) {
	c.mu.Lock()
	c.calls = append(c.calls, accountID)
	c.mu.Unlock()
	r := c.results[accountID]
	return r.reading, r.err
}

// fakeReconciler records reconcile calls and serves a canned error per alias. It shares an
// ordering log with the store so a test can assert reconcile-before-poll.
type fakeReconciler struct {
	mu       sync.Mutex
	calls    []uuid.UUID
	errs     map[uuid.UUID]error
	orderRef *fakeStore
}

func (r *fakeReconciler) ReconcileCodexAuthIdentity(_ context.Context, _ uuid.UUID, aliasID uuid.UUID) error {
	r.mu.Lock()
	r.calls = append(r.calls, aliasID)
	r.mu.Unlock()
	if r.orderRef != nil {
		r.orderRef.mu.Lock()
		r.orderRef.order = append(r.orderRef.order, "reconcile:"+aliasID.String())
		r.orderRef.mu.Unlock()
	}
	if r.errs == nil {
		return nil
	}
	return r.errs[aliasID]
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func linkedRow(userID, acctID uuid.UUID, gen, cred int64, coord string, reauth, recovery bool) store.ListLinkedCodexAccountsToPollRow {
	return store.ListLinkedCodexAccountsToPollRow{
		UserID:             userID,
		ProviderAccountID:  acctID,
		Generation:         gen,
		CredentialRevision: cred,
		CoordState:         coord,
		ReauthRequired:     reauth,
		HasRecovery:        recovery,
	}
}

func sampleReading(gen, cred int64) workersvc.CodexUsageReading {
	pct := 33.0
	return workersvc.CodexUsageReading{
		Buckets: []apitypes.CodexRateLimitBucketDTO{{
			ID:      "codex",
			Primary: &apitypes.CodexRateLimitWindowDTO{UsedPercent: &pct},
		}},
		ObservedGeneration:         gen,
		ObservedCredentialRevision: cred,
	}
}

func newEngineFor(st *fakeStore, col *fakeCollector, rec *fakeReconciler) *Engine {
	return New(st, col, rec, 5*time.Minute, quietLogger())
}

// --- tests ---------------------------------------------------------------------------------

// TestTickReconcilesThenPolls proves each pass reconciles staged aliases FIRST, then polls the
// linked accounts (one poll per canonical account), and writes a reading under its OBSERVED
// (generation, credential_revision). It also proves there is no model dependency: the only
// reading source is the collector fake.
func TestTickReconcilesThenPolls(t *testing.T) {
	userID := uuid.New()
	alias := uuid.New()
	acct := uuid.New()

	st := newFakeStore()
	st.staged = []store.ListStagedCodexAliasesRow{{UserID: userID, UserSecretID: alias}}
	st.toPoll = []store.ListLinkedCodexAccountsToPollRow{linkedRow(userID, acct, 2, 5, "idle", false, false)}

	col := &fakeCollector{results: map[uuid.UUID]collectResult{acct: {reading: sampleReading(2, 5)}}}
	rec := &fakeReconciler{orderRef: st}
	e := newEngineFor(st, col, rec)

	e.Boot(context.Background())

	if len(rec.calls) != 1 || rec.calls[0] != alias {
		t.Fatalf("reconciler calls = %v, want [%s]", rec.calls, alias)
	}
	if len(col.calls) != 1 || col.calls[0] != acct {
		t.Fatalf("collector calls = %v, want [%s]", col.calls, acct)
	}
	if len(st.upserts) != 1 {
		t.Fatalf("want 1 upsert, got %d", len(st.upserts))
	}
	up := st.upserts[0]
	if up.ObservedGeneration != 2 || up.ObservedCredentialRevision != 5 {
		t.Fatalf("upsert observed = (%d,%d), want (2,5)", up.ObservedGeneration, up.ObservedCredentialRevision)
	}
	if up.AttemptStatus != attemptStatusOK {
		t.Fatalf("upsert status = %q, want %q", up.AttemptStatus, attemptStatusOK)
	}
	var buckets []apitypes.CodexRateLimitBucketDTO
	if err := json.Unmarshal(up.Buckets, &buckets); err != nil {
		t.Fatalf("upsert buckets not valid JSON: %v", err)
	}
	if len(buckets) != 1 || buckets[0].ID != "codex" {
		t.Fatalf("upsert buckets = %+v", buckets)
	}
	if len(st.order) != 2 || st.order[0] != "reconcile:"+alias.String() || st.order[1] != "poll:"+acct.String() {
		t.Fatalf("order = %v, want reconcile before poll", st.order)
	}
}

// TestPollObservedGenFromReadingNotListing proves the 401→new-generation retry contract at the
// poller boundary: the collector returns a reading captured under an ADVANCED generation, and
// the poller writes THAT (not the stale listing generation).
func TestPollObservedGenFromReadingNotListing(t *testing.T) {
	userID, acct := uuid.New(), uuid.New()
	st := newFakeStore()
	st.toPoll = []store.ListLinkedCodexAccountsToPollRow{linkedRow(userID, acct, 7, 1, "idle", false, false)}
	col := &fakeCollector{results: map[uuid.UUID]collectResult{acct: {reading: sampleReading(8, 1)}}} // gen advanced 7->8
	e := newEngineFor(st, col, &fakeReconciler{})
	e.Boot(context.Background())
	if len(st.upserts) != 1 || st.upserts[0].ObservedGeneration != 8 {
		t.Fatalf("upsert observed gen = %+v, want 8 (the reading's, not the listing's 7)", st.upserts)
	}
}

// TestPollSkipsNonPollable proves the poller pre-filters non-poll targets from the listing
// (in-flight refresh, quarantined, reauth-flagged, recovery slot) and never spends a collect
// on them, while still polling a healthy idle account.
func TestPollSkipsNonPollable(t *testing.T) {
	userID := uuid.New()
	good := uuid.New()
	st := newFakeStore()
	st.toPoll = []store.ListLinkedCodexAccountsToPollRow{
		linkedRow(userID, uuid.New(), 0, 0, "in_progress", false, false),
		linkedRow(userID, uuid.New(), 0, 0, "quarantined", false, false),
		linkedRow(userID, uuid.New(), 0, 0, "idle", true, false),      // reauth flagged
		linkedRow(userID, uuid.New(), 0, 0, "committed", false, true), // has recovery
		linkedRow(userID, good, 0, 0, "committed", false, false),
	}
	col := &fakeCollector{results: map[uuid.UUID]collectResult{good: {reading: sampleReading(0, 0)}}}
	e := newEngineFor(st, col, &fakeReconciler{})
	e.Boot(context.Background())
	if len(col.calls) != 1 || col.calls[0] != good {
		t.Fatalf("collector calls = %v, want only the healthy account %s", col.calls, good)
	}
}

// TestFailureStatusRecorded proves each typed failure is recorded with its canonical status
// token and the LISTING's observed counters (the failure record's fence), never a token.
func TestFailureStatusRecorded(t *testing.T) {
	tests := []struct {
		name       string
		fail       *workersvc.CodexUsageFailure
		wantStatus string
	}{
		{"reauth", &workersvc.CodexUsageFailure{Kind: workersvc.CodexUsageFailReauthRequired}, "reauth_required"},
		{"vault", &workersvc.CodexUsageFailure{Kind: workersvc.CodexUsageFailVaultLocked}, "vault_locked"},
		{"rate", &workersvc.CodexUsageFailure{Kind: workersvc.CodexUsageFailRateLimited}, "rate_limited"},
		{"mismatch", &workersvc.CodexUsageFailure{Kind: workersvc.CodexUsageFailProviderMismatch}, "provider_mismatch"},
		{"transient", &workersvc.CodexUsageFailure{Kind: workersvc.CodexUsageFailTransient}, "transient"},
		{"disabled", &workersvc.CodexUsageFailure{Kind: workersvc.CodexUsageFailDisabled}, "disabled"},
		{"not_linked", &workersvc.CodexUsageFailure{Kind: workersvc.CodexUsageFailNotLinked}, "not_linked"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			userID, acct := uuid.New(), uuid.New()
			st := newFakeStore()
			st.toPoll = []store.ListLinkedCodexAccountsToPollRow{linkedRow(userID, acct, 3, 4, "idle", false, false)}
			col := &fakeCollector{results: map[uuid.UUID]collectResult{acct: {err: tc.fail}}}
			e := newEngineFor(st, col, &fakeReconciler{})
			e.Boot(context.Background())
			if len(st.upserts) != 0 {
				t.Fatalf("a failure must not upsert a reading, got %d", len(st.upserts))
			}
			if len(st.failures) != 1 {
				t.Fatalf("want 1 failure record, got %d", len(st.failures))
			}
			f := st.failures[0]
			if f.AttemptStatus != tc.wantStatus {
				t.Fatalf("status = %q, want %q", f.AttemptStatus, tc.wantStatus)
			}
			if f.ObservedGeneration != 3 || f.ObservedCredentialRevision != 4 {
				t.Fatalf("failure observed = (%d,%d), want the listing's (3,4)", f.ObservedGeneration, f.ObservedCredentialRevision)
			}
			if f.AttemptError == "" {
				t.Fatalf("failure error text must be a bounded non-empty message")
			}
		})
	}
}

// TestTransientBacksOffAndClears proves a transient failure arms an in-memory backoff that
// skips the account next tick, and a later success (once the clock passes) clears it.
func TestTransientBacksOffAndClears(t *testing.T) {
	userID, acct := uuid.New(), uuid.New()
	st := newFakeStore()
	st.toPoll = []store.ListLinkedCodexAccountsToPollRow{linkedRow(userID, acct, 0, 0, "idle", false, false)}
	col := &fakeCollector{results: map[uuid.UUID]collectResult{acct: {err: &workersvc.CodexUsageFailure{Kind: workersvc.CodexUsageFailTransient}}}}

	clock := time.Unix(1_000_000, 0)
	e := newEngineFor(st, col, &fakeReconciler{})
	e.now = func() time.Time { return clock }

	e.Boot(context.Background()) // tick 1: fails, backs off
	if len(col.calls) != 1 {
		t.Fatalf("tick1: want 1 collect, got %d", len(col.calls))
	}
	e.tickAll(context.Background()) // tick 2: still in backoff, skipped
	if len(col.calls) != 1 {
		t.Fatalf("tick2 (in backoff): collect count = %d, want still 1", len(col.calls))
	}

	// Advance past the backoff and make the account succeed.
	clock = clock.Add(defaultBackoff + time.Second)
	col.results[acct] = collectResult{reading: sampleReading(0, 0)}
	e.tickAll(context.Background()) // tick 3: backoff expired, polled and succeeds
	if len(col.calls) != 2 {
		t.Fatalf("tick3 (backoff expired): collect count = %d, want 2", len(col.calls))
	}
	if len(st.upserts) != 1 {
		t.Fatalf("tick3: want 1 upsert after recovery, got %d", len(st.upserts))
	}
	// A success clears the backoff, so a subsequent tick polls again.
	e.tickAll(context.Background())
	if len(col.calls) != 3 {
		t.Fatalf("tick4 (backoff cleared by success): collect count = %d, want 3", len(col.calls))
	}
}

// TestRateLimitedHonorsRetryAfter proves a 429's Retry-After sets the exact backoff window.
func TestRateLimitedHonorsRetryAfter(t *testing.T) {
	userID, acct := uuid.New(), uuid.New()
	st := newFakeStore()
	st.toPoll = []store.ListLinkedCodexAccountsToPollRow{linkedRow(userID, acct, 0, 0, "idle", false, false)}
	col := &fakeCollector{results: map[uuid.UUID]collectResult{acct: {err: &workersvc.CodexUsageFailure{Kind: workersvc.CodexUsageFailRateLimited, RetryAfter: 90 * time.Second}}}}

	clock := time.Unix(2_000_000, 0)
	e := newEngineFor(st, col, &fakeReconciler{})
	e.now = func() time.Time { return clock }

	e.Boot(context.Background())
	// 60s later still inside the 90s window → skipped.
	clock = clock.Add(60 * time.Second)
	e.tickAll(context.Background())
	if len(col.calls) != 1 {
		t.Fatalf("within Retry-After: collect count = %d, want 1", len(col.calls))
	}
	// Past 90s → polled again.
	clock = clock.Add(31 * time.Second)
	e.tickAll(context.Background())
	if len(col.calls) != 2 {
		t.Fatalf("after Retry-After: collect count = %d, want 2", len(col.calls))
	}
}

// TestReauthDoesNotBackOff proves a reauth_required outcome records the status but arms no
// backoff (the account drops out of the pollable set on the next listing anyway) — distinct
// from the transient case.
func TestReauthDoesNotBackOff(t *testing.T) {
	userID, acct := uuid.New(), uuid.New()
	st := newFakeStore()
	st.toPoll = []store.ListLinkedCodexAccountsToPollRow{linkedRow(userID, acct, 0, 0, "idle", false, false)}
	col := &fakeCollector{results: map[uuid.UUID]collectResult{acct: {err: &workersvc.CodexUsageFailure{Kind: workersvc.CodexUsageFailReauthRequired}}}}
	e := newEngineFor(st, col, &fakeReconciler{})
	e.Boot(context.Background())
	e.tickAll(context.Background())
	if len(col.calls) != 2 {
		t.Fatalf("reauth must not arm a backoff: collect count = %d, want 2", len(col.calls))
	}
}

// TestDiscardOnAuthorityMoved proves a 0-row upsert (fence rejected — authority moved) is a
// discard, not an error, and does not arm a backoff (the success path still clears it).
func TestDiscardOnAuthorityMoved(t *testing.T) {
	userID, acct := uuid.New(), uuid.New()
	st := newFakeStore()
	st.upsertRows = 0 // fence rejects every write
	st.toPoll = []store.ListLinkedCodexAccountsToPollRow{linkedRow(userID, acct, 0, 0, "idle", false, false)}
	col := &fakeCollector{results: map[uuid.UUID]collectResult{acct: {reading: sampleReading(0, 0)}}}
	e := newEngineFor(st, col, &fakeReconciler{})
	e.Boot(context.Background())
	if len(st.upserts) != 1 {
		t.Fatalf("want the upsert attempted, got %d", len(st.upserts))
	}
	// No backoff armed → a subsequent tick polls again (a discard is not a failure).
	e.tickAll(context.Background())
	if len(col.calls) != 2 {
		t.Fatalf("discard must not back off: collect count = %d, want 2", len(col.calls))
	}
}

// TestStagedReconcileErrorBacksOff proves a reconcile error arms the reconcile backoff so a
// still-'staging' alias is not re-probed every tick (only a proven rejection is terminal, and
// a terminal alias drops out of the staging listing anyway).
func TestStagedReconcileErrorBacksOff(t *testing.T) {
	userID, alias := uuid.New(), uuid.New()
	st := newFakeStore()
	st.staged = []store.ListStagedCodexAliasesRow{{UserID: userID, UserSecretID: alias}}
	rec := &fakeReconciler{errs: map[uuid.UUID]error{alias: context.DeadlineExceeded}}

	clock := time.Unix(3_000_000, 0)
	e := newEngineFor(st, &fakeCollector{}, rec)
	e.now = func() time.Time { return clock }

	e.Boot(context.Background())
	e.tickAll(context.Background())
	if len(rec.calls) != 1 {
		t.Fatalf("reconcile backoff must skip the second tick: calls = %d, want 1", len(rec.calls))
	}
	clock = clock.Add(defaultBackoff + time.Second)
	e.tickAll(context.Background())
	if len(rec.calls) != 2 {
		t.Fatalf("after backoff: reconcile calls = %d, want 2", len(rec.calls))
	}
}

// TestPokeReconcilesAndPollsUserIgnoringBackoff proves a poke drives the per-user listings and
// ignores a prior backoff (a just-saved credential may work where an earlier attempt failed).
func TestPokeReconcilesAndPollsUser(t *testing.T) {
	userID, alias, acct := uuid.New(), uuid.New(), uuid.New()
	st := newFakeStore()
	st.stagedForUser[userID] = []store.ListStagedCodexAliasesForUserRow{{UserID: userID, UserSecretID: alias}}
	st.forUser[userID] = []store.ListLinkedCodexAccountsForUserRow{{
		UserID: userID, ProviderAccountID: acct, Generation: 1, CredentialRevision: 1, CoordState: "idle",
	}}
	col := &fakeCollector{results: map[uuid.UUID]collectResult{acct: {err: &workersvc.CodexUsageFailure{Kind: workersvc.CodexUsageFailTransient}}}}
	rec := &fakeReconciler{orderRef: st}

	clock := time.Unix(4_000_000, 0)
	e := newEngineFor(st, col, rec)
	e.now = func() time.Time { return clock }

	e.pokeUser(context.Background(), userID) // fails, backs off
	if len(col.calls) != 1 || len(rec.calls) != 1 {
		t.Fatalf("poke1: collect=%d reconcile=%d, want 1/1", len(col.calls), len(rec.calls))
	}
	// A second poke (still within the backoff window) ignores the backoff and re-polls.
	col.results[acct] = collectResult{reading: sampleReading(1, 1)}
	e.pokeUser(context.Background(), userID)
	if len(col.calls) != 2 {
		t.Fatalf("poke2 must ignore backoff: collect count = %d, want 2", len(col.calls))
	}
	if len(st.upserts) != 1 {
		t.Fatalf("poke2 want 1 upsert after recovery, got %d", len(st.upserts))
	}
}
