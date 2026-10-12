package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #2347: the create paths re-validate freshness (and, for the watcher, the cap) against
// the CURRENT ledger row under the branch lock.

func recheckStartEnv(t *testing.T, ledger store.GetMRReworkLedgerRow, ledgerErr error) (*Service, *fakeStore, uuid.UUID, uuid.UUID) {
	user, repo, runID := uuid.New(), uuid.New(), uuid.New()
	fs := &fakeStore{
		runByID:           reworkableSourceRun(runID, user, repo),
		runByIDPlain:      reworkableSourceRun(runID, user, repo),
		hasAnthropicToken: true,
		mrReworkLedger:    ledger,
		mrReworkLedgerErr: ledgerErr,
		repoRow:           aValidRepoRow(),
		mrReworkRunResult: store.Run{ID: uuid.New(), Kind: runkind.MRRework, TriggerSource: "manual"},
	}
	return New(fs, newBox(t), testParams()), fs, user, runID
}

// An on-demand bare trigger stays exempt from the automatic cap: attempt_count at the cap still
// creates when the comments are fresh.
func TestStartMRReworkForRunBareTriggerIsCapExempt(t *testing.T) {
	svc, fs, user, runID := recheckStartEnv(t, store.GetMRReworkLedgerRow{Ref: "agent/issue-7", AttemptCount: 50, CommentMarks: marksJSON("100")}, nil)
	if _, err := svc.StartMRReworkForRun(context.Background(), user, runID, "", reviewResultOf(sampleReviewSnapshot())); err != nil {
		t.Fatalf("bare on-demand trigger past the cap with a fresh comment: %v", err)
	}
	if fs.mrReworkAndAdvanceParams == nil {
		t.Fatal("create not made")
	}
}

// A bare request whose comment another cycle consumed since the pre-listing read is refused,
// and nothing is created.
func TestStartMRReworkForRunBareTriggerStaleLedgerRefuses(t *testing.T) {
	svc, fs, user, runID := recheckStartEnv(t, store.GetMRReworkLedgerRow{Ref: "agent/issue-7", CommentMarks: marksJSON("500")}, nil)
	_, err := svc.StartMRReworkForRun(context.Background(), user, runID, "", reviewResultOf(sampleReviewSnapshot()))
	if !errors.Is(err, ErrReworkNothingNew) {
		t.Fatalf("err = %v, want ErrReworkNothingNew", err)
	}
	if fs.mrReworkAndAdvanceParams != nil {
		t.Fatal("a stale bare trigger must not create")
	}
}

// Guidance is a valid trigger on its own, so it skips the freshness recheck.
func TestStartMRReworkForRunGuidanceSkipsRecheck(t *testing.T) {
	svc, fs, user, runID := recheckStartEnv(t, store.GetMRReworkLedgerRow{Ref: "agent/issue-7", CommentMarks: marksJSON("500")}, nil)
	if _, err := svc.StartMRReworkForRun(context.Background(), user, runID, "rename it", reviewResultOf(sampleReviewSnapshot())); err != nil {
		t.Fatalf("guidance trigger: %v", err)
	}
	if fs.mrReworkAndAdvanceParams == nil {
		t.Fatal("create not made")
	}
}

// No ledger row yet is a zero row in the recheck: it must neither refuse nor escape as
// pgx.ErrNoRows (which the create maps to ErrBranchInUse).
func TestStartMRReworkForRunNoLedgerRowIsAZeroRow(t *testing.T) {
	svc, fs, user, runID := recheckStartEnv(t, store.GetMRReworkLedgerRow{}, pgx.ErrNoRows)
	if _, err := svc.StartMRReworkForRun(context.Background(), user, runID, "", reviewResultOf(sampleReviewSnapshot())); err != nil {
		t.Fatalf("no ledger row: %v", err)
	}
	if fs.mrReworkAndAdvanceParams == nil {
		t.Fatal("create not made")
	}
}

func TestCreateAutoMRReworkRunAndAdvanceRequiresResult(t *testing.T) {
	fs := &fakeStore{repoRow: aValidRepoRow(), runByIDPlain: reworkableSourceRun(uuid.New(), uuid.New(), uuid.New())}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateAutoMRReworkRunAndAdvance(context.Background(), uuid.New(), uuid.New(), "agent/issue-7", 55, uuid.New(), "t", "d", nil, 5); err == nil {
		t.Fatal("a nil result must be refused")
	}
}

// The watcher create refuses at the cap and on consumed comments, and otherwise records the
// cycle on the same store handle right after the insert.
func TestCreateAutoMRReworkRunAndAdvanceRecheckAndAdvance(t *testing.T) {
	user, repo, runID := uuid.New(), uuid.New(), uuid.New()
	call := func(fs *fakeStore, res *ReviewSnapshotResult, cap int) error {
		fs.runByIDPlain = reworkableSourceRun(runID, user, repo)
		fs.repoRow = aValidRepoRow()
		svc := New(fs, newBox(t), testParams())
		_, err := svc.CreateAutoMRReworkRunAndAdvance(context.Background(), user, repo, "agent/issue-7", 55, runID, "t", "d", res, cap)
		return err
	}
	res := reviewResultOf(sampleReviewSnapshot())

	fs := &fakeStore{mrReworkLedger: store.GetMRReworkLedgerRow{AttemptCount: 5}, mrReworkRunResult: store.Run{ID: uuid.New()}}
	if err := call(fs, res, 5); !errors.Is(err, ErrMRReworkCapReached) {
		t.Fatalf("at the cap: err = %v, want ErrMRReworkCapReached", err)
	}
	fs = &fakeStore{mrReworkLedger: store.GetMRReworkLedgerRow{CommentMarks: marksJSON("120")}, mrReworkRunResult: store.Run{ID: uuid.New()}}
	if err := call(fs, res, 5); !errors.Is(err, ErrReworkNothingNew) {
		t.Fatalf("consumed: err = %v, want ErrReworkNothingNew", err)
	}
	if fs.mrReworkRunParams != nil || len(fs.ledgerUpserts) != 0 {
		t.Fatal("a refused create must write nothing")
	}
	fs = &fakeStore{mrReworkLedger: store.GetMRReworkLedgerRow{AttemptCount: 4, CommentMarks: marksJSON("100")}, mrReworkRunResult: store.Run{ID: uuid.New()}}
	if err := call(fs, res, 5); err != nil {
		t.Fatalf("fresh under the cap: %v", err)
	}
	if fs.mrReworkRunParams == nil || len(fs.ledgerUpserts) != 1 || fs.ledgerUpserts[0].HighWater != 120 {
		t.Fatalf("run=%v upserts=%+v, want the run and one advance to 120", fs.mrReworkRunParams, fs.ledgerUpserts)
	}
	// An advance failure rolls the create back to the caller as an error.
	fs = &fakeStore{mrReworkLedger: store.GetMRReworkLedgerRow{CommentMarks: marksJSON("100")}, mrReworkRunResult: store.Run{ID: uuid.New()}, ledgerUpsertErr: errors.New("boom")}
	if err := call(fs, res, 5); err == nil {
		t.Fatal("an advance failure must fail the create")
	}
}

// A stored ledger that does not decode is a hard error, not a retryable ErrReworkLedgerChanged:
// a retry re-reads the same bytes.
func TestRecheckReworkLedgerUndecodableRowIsNotRetryable(t *testing.T) {
	cases := map[string]store.GetMRReworkLedgerRow{
		"non-integer mark":   {Ref: "r", CommentMarks: []byte(`{"rc":"x"}`)},
		"invalid marks JSON": {Ref: "r", CommentMarks: []byte(`{not json`)},
		"bad pending key":    {Ref: "r", CommentMarks: []byte(`{}`), PendingUnknownKeys: []string{"review:007"}},
	}
	for name, row := range cases {
		t.Run(name, func(t *testing.T) {
			err := recheckReworkLedger(row, &reworkRecheck{})
			if err == nil {
				t.Fatal("want an error")
			}
			if errors.Is(err, ErrReworkLedgerChanged) {
				t.Fatalf("err = %v, must not be the retryable ErrReworkLedgerChanged", err)
			}
			if !strings.Contains(err.Error(), "decode") {
				t.Fatalf("err = %v, want it to wrap the decode failure", err)
			}
		})
	}
}
