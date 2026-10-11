package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #2680 live-DB coverage: the approve-time completion contract is bound to the milestone
// list the same statement freezes. submitApproval builds the contract in Go from a snapshot read
// and CreateApprovePlanInput freezes the source as it is at write time; a candidate republished
// between the two used to freeze list B beside a contract built from list A.
//
// The race is interleaved through the REAL service and queries: publicationStore wraps
// *store.Queries and, after a scripted pre-write GetRunMilestoneFreezeSnapshot read, runs a raw
// UPDATE that publishes a new candidate. No production hook. The post-write (#260) read finds the
// script empty, so it never triggers a publication.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.

// publicationStore runs script[i] right after the i-th snapshot read returned.
type publicationStore struct {
	*store.Queries
	script []func()
}

func (p *publicationStore) GetRunMilestoneFreezeSnapshot(ctx context.Context, id uuid.UUID) (store.GetRunMilestoneFreezeSnapshotRow, error) {
	row, err := p.Queries.GetRunMilestoneFreezeSnapshot(ctx, id)
	if len(p.script) > 0 {
		next := p.script[0]
		p.script = p.script[1:]
		next()
	}
	return row, err
}

const (
	listA = `[{"id":"a1","title":"Alpha one"},{"id":"a2","title":"Alpha two"}]`
	listB = `[{"id":"b1","title":"Beta one"}]`
)

type approveSrcFix struct {
	e     interlockLiveDB
	svc   *Service
	wid   uuid.UUID
	wrap  *publicationStore
	runID uuid.UUID
}

// newApproveSrcFix seeds an awaiting_approval run (interlocked unless legacy) whose candidate is
// `candidate` (nil = SQL NULL), owned by a worker with no capabilities.
func newApproveSrcFix(t *testing.T, interlocked bool, candidate []byte) *approveSrcFix {
	t.Helper()
	e := setupInterlockLiveDB(t)
	wid := e.seedWorker(t, nil)
	runID := e.seedOwnedRun(t, wid, "awaiting_approval", interlocked, false)
	if candidate != nil {
		e.exec(t, `UPDATE runs SET milestones_candidate = $2 WHERE id = $1`, runID, candidate)
	}
	wrap := &publicationStore{Queries: e.q}
	svc := New(wrap, newBox(t), testParams())
	svc.SetTxBeginner(e.pool)
	svc.SetBackground(func(func()) {})
	return &approveSrcFix{e: e, svc: svc, wid: wid, wrap: wrap, runID: runID}
}

// publish returns a script step that republishes the candidate (and optionally the required
// capabilities) with a raw UPDATE, exactly as a concurrent SetRunAwaitingApproval would.
func (f *approveSrcFix) publish(t *testing.T, list string, caps ...string) func() {
	return func() {
		if caps == nil {
			caps = []string{}
		}
		f.e.exec(t, `UPDATE runs SET milestones_candidate = $2::jsonb, required_capabilities = $3 WHERE id = $1`, f.runID, list, caps)
	}
}

type approveSrcRow struct {
	frozen, contract, candidate []byte
	contractRev                 pgtype.Int4
	budgetIter                  pgtype.Int4
	agentSource                 pgtype.Text
	inputs                      int
	caps                        []string
}

func (f *approveSrcFix) row(t *testing.T) approveSrcRow {
	t.Helper()
	var r approveSrcRow
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT milestones_frozen, completion_contract, milestones_candidate, contract_revision,
	        budget_max_iterations, agent_source, required_capabilities,
	        (SELECT count(*) FROM run_user_inputs WHERE run_id = runs.id AND kind = 'approve_plan')
	      FROM runs WHERE id = $1`, f.runID).Scan(&r.frozen, &r.contract, &r.candidate, &r.contractRev, &r.budgetIter, &r.agentSource, &r.caps, &r.inputs); err != nil {
		t.Fatalf("read run: %v", err)
	}
	return r
}

// idsOf decodes the milestone ids of a milestone list (the same ids a contract's criteria cite).
func idsOf(t *testing.T, list []byte) []string {
	t.Helper()
	var ms []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(list, &ms); err != nil {
		t.Fatalf("decode milestones %s: %v", list, err)
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// assertFrozenMatchesContract is the #2680 invariant: the criteria cite exactly the frozen list.
func assertFrozenMatchesContract(t *testing.T, r approveSrcRow, wantFrozen string) {
	t.Helper()
	if !jsonEqual(t, r.frozen, wantFrozen) {
		t.Fatalf("milestones_frozen = %s, want %s", r.frozen, wantFrozen)
	}
	var c struct {
		Criteria []struct {
			MilestoneID string `json:"milestone_id"`
		} `json:"criteria"`
	}
	if err := json.Unmarshal(r.contract, &c); err != nil {
		t.Fatalf("decode contract %s: %v", r.contract, err)
	}
	got := make([]string, 0, len(c.Criteria))
	for _, cr := range c.Criteria {
		got = append(got, cr.MilestoneID)
	}
	if want := idsOf(t, r.frozen); !slices.Equal(got, want) {
		t.Fatalf("contract criteria ids = %v, frozen ids = %v: the contract was not built from the list the approve froze", got, want)
	}
	if !r.contractRev.Valid || r.contractRev.Int32 != 1 {
		t.Fatalf("contract_revision = %+v, want 1", r.contractRev)
	}
}

func jsonEqual(t *testing.T, got []byte, want string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("decode %s: %v", want, err)
	}
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func (f *approveSrcFix) approve(opts SubmitInputOptions) error {
	_, err := f.svc.SubmitInputWithOptions(f.e.ctx, f.e.userID, f.runID, "approve_plan", "", &AgentSelection{Source: AgentSourceOwn}, opts)
	return err
}

func assertNothingWritten(t *testing.T, r approveSrcRow, candidate string) {
	t.Helper()
	if r.frozen != nil || r.contract != nil || r.contractRev.Valid || r.budgetIter.Valid || r.agentSource.Valid || r.inputs != 0 {
		t.Fatalf("a refused approve wrote something: frozen=%s contract=%s rev=%+v budget=%+v agent_source=%+v inputs=%d",
			r.frozen, r.contract, r.contractRev, r.budgetIter, r.agentSource, r.inputs)
	}
	if candidate != "" && !jsonEqual(t, r.candidate, candidate) {
		t.Fatalf("milestones_candidate = %s, want %s", r.candidate, candidate)
	}
}

// (a) The regression: the candidate moves from a to b after the snapshot read. The approve must
// freeze b and a contract built from b, not b beside a's criteria.
func TestApproveContractBoundToFrozenListLiveDB(t *testing.T) {
	f := newApproveSrcFix(t, true, []byte(listA))
	f.wrap.script = []func(){f.publish(t, listB)}

	if err := f.approve(SubmitInputOptions{}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	r := f.row(t)
	assertFrozenMatchesContract(t, r, listB)
	if r.inputs != 1 {
		t.Fatalf("approve_plan inputs = %d, want exactly 1", r.inputs)
	}
}

// (b) The snapshot saw NO source and one was published meanwhile: the unbound approve retries and
// freezes the published list with a matching contract instead of an empty-criteria one.
func TestApproveContractNilSourceThenPublishedLiveDB(t *testing.T) {
	for _, list := range []string{`[]`, listB} {
		t.Run(list, func(t *testing.T) {
			f := newApproveSrcFix(t, true, nil)
			f.wrap.script = []func(){f.publish(t, list)}

			if err := f.approve(SubmitInputOptions{}); err != nil {
				t.Fatalf("approve: %v", err)
			}
			assertFrozenMatchesContract(t, f.row(t), list)
		})
	}
}

// (c) The list keeps moving on every pre-write read: bounded, then ErrApprovalMilestonesMoved
// with nothing written.
func TestApproveContractExhaustionWritesNothingLiveDB(t *testing.T) {
	f := newApproveSrcFix(t, true, []byte(listA))
	f.wrap.script = []func(){f.publish(t, listB), f.publish(t, listA), f.publish(t, listB)}

	err := f.approve(SubmitInputOptions{})
	if !errors.Is(err, ErrApprovalMilestonesMoved) {
		t.Fatalf("err = %v, want ErrApprovalMilestonesMoved", err)
	}
	assertNothingWritten(t, f.row(t), listB)
}

// (d)/(e) A bound approve is answered as a gate-revision mismatch and writes nothing: stale
// revision (d), and the current revision while the candidate moves (e, Current == Expected).
func TestApproveContractBoundLiveDB(t *testing.T) {
	t.Run("stale revision", func(t *testing.T) {
		f := newApproveSrcFix(t, true, []byte(listA))
		f.e.exec(t, `UPDATE runs SET gate_revision = 3 WHERE id = $1`, f.runID)
		stale := int64(2)

		err := f.approve(SubmitInputOptions{ExpectedGateRevision: &stale})
		var mm *GateRevisionMismatchError
		if !errors.As(err, &mm) || mm.Expected != 2 || mm.Current != 3 {
			t.Fatalf("err = %v, want *GateRevisionMismatchError{Expected:2, Current:3}", err)
		}
		assertNothingWritten(t, f.row(t), listA)
	})
	t.Run("current revision, candidate moves", func(t *testing.T) {
		f := newApproveSrcFix(t, true, []byte(listA))
		f.e.exec(t, `UPDATE runs SET gate_revision = 3 WHERE id = $1`, f.runID)
		f.wrap.script = []func(){f.publish(t, listB)}
		cur := int64(3)

		err := f.approve(SubmitInputOptions{ExpectedGateRevision: &cur})
		var mm *GateRevisionMismatchError
		if !errors.As(err, &mm) || mm.Expected != 3 || mm.Current != 3 {
			t.Fatalf("err = %v, want *GateRevisionMismatchError{Expected:3, Current:3}", err)
		}
		assertNothingWritten(t, f.row(t), listB)
	})
}

// (f) The predicate binds only when the statement would freeze the contract.
func TestApproveContractPredicateUntouchedCasesLiveDB(t *testing.T) {
	t.Run("legacy run approves as before", func(t *testing.T) {
		f := newApproveSrcFix(t, false, []byte(listA))
		f.wrap.script = []func(){f.publish(t, listB)}
		if err := f.approve(SubmitInputOptions{}); err != nil {
			t.Fatalf("approve: %v", err)
		}
		r := f.row(t)
		if r.contract != nil || r.contractRev.Valid || !jsonEqual(t, r.frozen, listB) {
			t.Fatalf("legacy approve: frozen=%s contract=%s rev=%+v", r.frozen, r.contract, r.contractRev)
		}
	})
	t.Run("interlocked run with no source approves as before", func(t *testing.T) {
		f := newApproveSrcFix(t, true, nil)
		if err := f.approve(SubmitInputOptions{}); err != nil {
			t.Fatalf("approve: %v", err)
		}
		r := f.row(t)
		if r.frozen != nil || r.contract != nil || r.inputs != 1 {
			t.Fatalf("no-source approve: frozen=%s contract=%s inputs=%d", r.frozen, r.contract, r.inputs)
		}
	})
	t.Run("re-approve of a frozen contract is not refused by a moved candidate", func(t *testing.T) {
		f := newApproveSrcFix(t, true, []byte(listA))
		if err := f.approve(SubmitInputOptions{}); err != nil {
			t.Fatalf("first approve: %v", err)
		}
		before := f.row(t)
		f.e.exec(t, `UPDATE runs SET milestones_candidate = $2 WHERE id = $1`, f.runID, listB)
		if err := f.approve(SubmitInputOptions{}); err != nil {
			t.Fatalf("re-approve: %v", err)
		}
		after := f.row(t)
		if string(after.contract) != string(before.contract) || string(after.frozen) != string(before.frozen) {
			t.Fatalf("re-approve changed the freeze: frozen %s -> %s, contract %s -> %s", before.frozen, after.frozen, before.contract, after.contract)
		}
		if after.inputs != 2 {
			t.Fatalf("approve_plan inputs = %d, want 2", after.inputs)
		}
	})
}

// (g) The republished plan now requires a capability the owning worker lacks: the retry re-runs
// the capability gate and the ordinary approve is blocked with nothing written; the owner override
// bypasses it, freezes b and clears the requirement after success.
func TestApproveContractRetryCapabilityGateLiveDB(t *testing.T) {
	t.Run("ordinary approve is blocked", func(t *testing.T) {
		f := newApproveSrcFix(t, true, []byte(listA))
		f.wrap.script = []func(){f.publish(t, listB, "docker")}

		err := f.approve(SubmitInputOptions{})
		var unmet *CapabilityUnmetError
		if !errors.As(err, &unmet) {
			t.Fatalf("err = %v, want *CapabilityUnmetError", err)
		}
		r := f.row(t)
		assertNothingWritten(t, r, listB)
		if !slices.Equal(r.caps, []string{"docker"}) {
			t.Fatalf("required_capabilities = %v, want the published [docker] untouched", r.caps)
		}
	})
	t.Run("owner override retries and clears", func(t *testing.T) {
		f := newApproveSrcFix(t, true, []byte(listA))
		f.wrap.script = []func(){f.publish(t, listB, "docker")}

		if err := f.approve(SubmitInputOptions{OverrideCapabilities: true}); err != nil {
			t.Fatalf("override approve: %v", err)
		}
		r := f.row(t)
		assertFrozenMatchesContract(t, r, listB)
		if len(r.caps) != 0 {
			t.Fatalf("required_capabilities = %v, want cleared after the successful override approve", r.caps)
		}
	})
}

// The query on its own: ContractSource=a while the candidate is b refuses with no row and writes
// nothing; the matching source writes.
func TestCreateApprovePlanInputContractSourceLiveDB(t *testing.T) {
	f := newApproveSrcFix(t, true, []byte(listB))
	params := func(source []byte) store.CreateApprovePlanInputParams {
		return store.CreateApprovePlanInputParams{
			RunID: f.runID, Body: pgtype.Text{String: "{}", Valid: true}, AgentSource: pgtype.Text{String: "own", Valid: true},
			AgentExclusions: []byte("[]"), ContractSource: source,
			RunMaxIterations: 5, MilestoneBudgetCap: milestoneBudgetCap, RunTimeoutSeconds: 7200, BudgetWallCeilingSeconds: 8 * 60 * 60,
			SizeBudgetFactorL: sizeBudgetFactorL,
		}
	}
	if _, err := f.e.q.CreateApprovePlanInput(f.e.ctx, params([]byte(listA))); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale source: err = %v, want pgx.ErrNoRows", err)
	}
	assertNothingWritten(t, f.row(t), listB)
	// A nil source against a present candidate is refused too ("I saw none").
	if _, err := f.e.q.CreateApprovePlanInput(f.e.ctx, params(nil)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("nil source vs present candidate: err = %v, want pgx.ErrNoRows", err)
	}
	assertNothingWritten(t, f.row(t), listB)
	contract, err := buildCompletionContract([]byte(listB))
	if err != nil {
		t.Fatal(err)
	}
	p := params([]byte(listB))
	p.CompletionContract = contract
	if _, err := f.e.q.CreateApprovePlanInput(f.e.ctx, p); err != nil {
		t.Fatalf("matching source: %v", err)
	}
	assertFrozenMatchesContract(t, f.row(t), listB)
}
