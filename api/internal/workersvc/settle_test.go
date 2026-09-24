package workersvc

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1582 M1 rework: unit coverage for SettlePredecessorHold's decision logic against a
// fake store and a fake forge. The live-DB proofs (handler/recovery_settle_livedb_test.go)
// drive the real router and the real guarded UPDATE; these pin the branch-name gate, the
// branch_missing answer, the server-held candidate binding and the candidate dedupe, and
// count every forge call.

const (
	uHead = "dddddddddddddddddddddddddddddddddddddddd"
	uA    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	uB    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	uC    = "cccccccccccccccccccccccccccccccccccccccc"
)

type settleUnitStore struct {
	Store
	run         store.Run
	hold        store.RecoveryCustodyHold
	sources     []string
	permitHead  string
	permitErr   error
	releaseRows int64
	released    *store.ReleasePredecessorCustodyHoldByAncestryParams
}

func (s *settleUnitStore) GetRunByIDForUser(context.Context, store.GetRunByIDForUserParams) (store.Run, error) {
	return s.run, nil
}

func (s *settleUnitStore) GetCustodyHoldForSettle(context.Context, store.GetCustodyHoldForSettleParams) (store.RecoveryCustodyHold, error) {
	return s.hold, nil
}

func (s *settleUnitStore) ListCaptureSourceShasForHold(context.Context, uuid.UUID) ([]string, error) {
	return s.sources, nil
}

func (s *settleUnitStore) GetSettleCompletionPermitHead(context.Context, store.GetSettleCompletionPermitHeadParams) (string, error) {
	return s.permitHead, s.permitErr
}

func (s *settleUnitStore) GetRunClaimContext(context.Context, uuid.UUID) (store.GetRunClaimContextRow, error) {
	return store.GetRunClaimContextRow{ForgeType: "github", ForgeProjectID: 7}, nil
}

func (s *settleUnitStore) ReleasePredecessorCustodyHoldByAncestry(_ context.Context, arg store.ReleasePredecessorCustodyHoldByAncestryParams) (int64, error) {
	s.released = &arg
	return s.releaseRows, nil
}

type settleUnitForge struct {
	forgetest.BaseFake
	mu         sync.Mutex
	headErr    error
	headCalls  int
	branches   []string
	candidates []string
}

func (f *settleUnitForge) BranchHead(_ context.Context, _ int64, branch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headCalls++
	f.branches = append(f.branches, branch)
	if f.headErr != nil {
		return "", f.headErr
	}
	return uHead, nil
}

func (f *settleUnitForge) CompareAncestry(_ context.Context, _ int64, _, candidate string) (forge.Ancestry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.candidates = append(f.candidates, candidate)
	return forge.AncestryAncestor, nil
}

func (f *settleUnitForge) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headCalls + len(f.candidates)
}

type settleUnitBuilder struct{ f forge.Forge }

func (b settleUnitBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.f, nil
}

type settleUnit struct {
	st   *settleUnitStore
	fk   *settleUnitForge
	svc  *Service
	wkr  store.Worker
	run  uuid.UUID
	hold uuid.UUID
}

// newSettleUnit is a completed, NON-interlocked run held by the caller at claim generation 2
// on a valid branch, with an open generation-1 hold the caller took and no captures.
func newSettleUnit() *settleUnit {
	wkr := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	runID, holdID := uuid.New(), uuid.New()
	st := &settleUnitStore{
		run: store.Run{
			ID: runID, UserID: wkr.UserID, Status: "completed", ClaimGeneration: 2,
			WorkerID:    pgtype.UUID{Bytes: wkr.ID, Valid: true},
			Branch:      pgtype.Text{String: "agent/issue-7", Valid: true},
			StatusSince: pgtype.Timestamptz{Valid: true},
		},
		hold:        store.RecoveryCustodyHold{ID: holdID, RunID: runID, Generation: 1, State: "open", OriginalWorkerID: wkr.ID},
		releaseRows: 1,
	}
	fk := &settleUnitForge{}
	svc := New(st, nil, Params{})
	svc.SetForges(settleUnitBuilder{f: fk})
	return &settleUnit{st: st, fk: fk, svc: svc, wkr: wkr, run: runID, hold: holdID}
}

func (u *settleUnit) settle(t *testing.T, pushed, source, adopted string) apitypes.RecoverySettleResponse {
	t.Helper()
	res, err := u.svc.SettlePredecessorHold(context.Background(), u.wkr, u.run, u.hold, apitypes.RecoverySettleRequest{
		PredecessorGeneration: 1, SuccessorGeneration: 2, PushedSha: pushed, SourceSha: source, AdoptedSha: adopted,
	})
	if err != nil {
		t.Fatalf("SettlePredecessorHold: %v", err)
	}
	return res
}

func assertSettleRetained(t *testing.T, res apitypes.RecoverySettleResponse, reason string) {
	t.Helper()
	if res.Outcome != apitypes.RecoverySettleRetained || res.Reason != reason {
		t.Fatalf("settle = %+v, want retained/%s", res, reason)
	}
}

func TestIsSettleBranchName(t *testing.T) {
	valid := []string{"agent/issue-7", "main", "a", "feature/x.y", "uzi/task/0f3c", "v1.2.3", strings.Repeat("b", 255)}
	for _, b := range valid {
		if !isSettleBranchName(b) {
			t.Errorf("isSettleBranchName(%q) = false, want true", b)
		}
	}
	invalid := []string{
		"", strings.Repeat("b", 256), "a..b", "/agent", ".hidden", "agent//x", "agent/", "agent.lock",
		"a b", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", `a\b`, "a@{1}", "a\tb", "a\x01b", "a\x7fb",
		"agent/.x", "agent.", "@",
	}
	for _, b := range invalid {
		if isSettleBranchName(b) {
			t.Errorf("isSettleBranchName(%q) = true, want false", b)
		}
	}
}

// TestSettleInvalidBranchIsNotEligibleWithoutForgeCalls: a worker-reported runs.branch that is
// not a git branch name never reaches a forge URL.
func TestSettleInvalidBranchIsNotEligibleWithoutForgeCalls(t *testing.T) {
	for _, b := range []string{"../../admin", "agent/x?y=1", "a b", "/etc", "x.lock"} {
		u := newSettleUnit()
		u.st.run.Branch = pgtype.Text{String: b, Valid: true}
		assertSettleRetained(t, u.settle(t, uA, uB, uC), apitypes.RecoverySettleNotEligible)
		if n := u.fk.calls(); n != 0 {
			t.Fatalf("branch %q: %d forge calls, want 0", b, n)
		}
		if u.st.released != nil {
			t.Fatalf("branch %q: release attempted", b)
		}
	}
}

// TestSettleBranchMissingIsDistinct: a 404 on the branch read (ErrRefNotFound) is
// branch_missing, not ancestry_unknown, and no compare is asked.
func TestSettleBranchMissingIsDistinct(t *testing.T) {
	u := newSettleUnit()
	u.fk.headErr = forge.ErrRefNotFound
	assertSettleRetained(t, u.settle(t, uA, uB, uC), apitypes.RecoverySettleBranchMissing)
	if len(u.fk.candidates) != 0 || u.st.released != nil {
		t.Fatalf("compares %v / release %v after a missing branch", u.fk.candidates, u.st.released)
	}
}

// TestSettleDedupesEqualCandidates: one compare per DISTINCT candidate.
func TestSettleDedupesEqualCandidates(t *testing.T) {
	cases := []struct {
		name                    string
		pushed, source, adopted string
		want                    []string
	}{
		{"all three equal", uA, uA, uA, []string{uA}},
		{"pushed == source", uA, uA, uC, []string{uA, uC}},
		{"source == adopted", uA, uB, uB, []string{uA, uB}},
		{"all distinct", uA, uB, uC, []string{uA, uB, uC}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newSettleUnit()
			res := u.settle(t, tc.pushed, tc.source, tc.adopted)
			if res.Outcome != apitypes.RecoverySettleReleased || res.FinalHeadSha != uHead {
				t.Fatalf("settle = %+v, want released", res)
			}
			if strings.Join(u.fk.candidates, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("compared %v, want %v", u.fk.candidates, tc.want)
			}
		})
	}
}

// TestSettleBindsSourceToCaptures: when captures exist under the hold, source_sha must be one
// of their source_sha values; with none, source_sha is unconstrained.
func TestSettleBindsSourceToCaptures(t *testing.T) {
	t.Run("mismatch", func(t *testing.T) {
		u := newSettleUnit()
		u.st.sources = []string{uC, uA}
		assertSettleRetained(t, u.settle(t, uA, uB, uC), apitypes.RecoverySettleCandidateMismatch)
		if n := u.fk.calls(); n != 0 || u.st.released != nil {
			t.Fatalf("%d forge calls / release %v on a candidate mismatch", n, u.st.released)
		}
	})
	t.Run("match", func(t *testing.T) {
		u := newSettleUnit()
		u.st.sources = []string{uC, uB}
		if res := u.settle(t, uA, uB, uC); res.Outcome != apitypes.RecoverySettleReleased {
			t.Fatalf("settle = %+v, want released", res)
		}
	})
}

// TestSettleBindsPushedToCompletionPermit: an interlocked run's pushed_sha must be the head of
// the permit its completion consumed; a non-interlocked run never consults a permit.
func TestSettleBindsPushedToCompletionPermit(t *testing.T) {
	interlocked := func(u *settleUnit) {
		u.st.run.CompletionContractVersion = pgtype.Int4{Int32: 1, Valid: true}
		u.st.run.ContractRevision = pgtype.Int4{Int32: 1, Valid: true}
	}
	t.Run("mismatch", func(t *testing.T) {
		u := newSettleUnit()
		interlocked(u)
		u.st.permitHead = uHead
		assertSettleRetained(t, u.settle(t, uA, uB, uC), apitypes.RecoverySettleCandidateMismatch)
		if n := u.fk.calls(); n != 0 || u.st.released != nil {
			t.Fatalf("%d forge calls / release %v on a candidate mismatch", n, u.st.released)
		}
	})
	t.Run("match", func(t *testing.T) {
		u := newSettleUnit()
		interlocked(u)
		u.st.permitHead = uA
		if res := u.settle(t, uA, uB, uC); res.Outcome != apitypes.RecoverySettleReleased {
			t.Fatalf("settle = %+v, want released", res)
		}
	})
	t.Run("interlocked without a consumed permit", func(t *testing.T) {
		u := newSettleUnit()
		interlocked(u)
		u.st.permitErr = pgx.ErrNoRows
		assertSettleRetained(t, u.settle(t, uA, uB, uC), apitypes.RecoverySettleNotEligible)
		if n := u.fk.calls(); n != 0 {
			t.Fatalf("%d forge calls, want 0", n)
		}
	})
	t.Run("non-interlocked ignores the permit table", func(t *testing.T) {
		u := newSettleUnit()
		u.st.permitHead = uHead // would mismatch if it were consulted
		if res := u.settle(t, uA, uB, uC); res.Outcome != apitypes.RecoverySettleReleased {
			t.Fatalf("settle = %+v, want released", res)
		}
	})
}
