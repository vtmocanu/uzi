package workersvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// reworkableSourceRun is a completed issue run whose MR is open — the happy-path shape
// StartMRReworkForRun accepts. Tests mutate one field to exercise each 409 class.
func reworkableSourceRun(runID, user, repo uuid.UUID) store.Run {
	return store.Run{
		ID:      runID,
		UserID:  user,
		RepoID:  pgconv.UUID(repo),
		Kind:    runkind.Issue,
		Status:  "completed",
		Branch:  pgconv.Text("agent/issue-7"),
		MrIid:   pgtype.Int8{Int64: 55, Valid: true},
		MrState: pgconv.Text("opened"),
	}
}

func TestStartMRReworkForRunHappyPathStampsManual(t *testing.T) {
	user, repo, runID, newRun := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	fs := &fakeStore{
		runByID:           reworkableSourceRun(runID, user, repo),
		hasAnthropicToken: true,
		mrReworkLedger:    store.MrReworkLedger{Ref: "agent/issue-7", AttemptCount: 5, HighWater: 100},
		repoRow:           aValidRepoRow(),
		mrReworkRunResult: store.Run{ID: newRun, Kind: runkind.MRRework, TriggerSource: "manual"},
	}
	svc := New(fs, newBox(t), testParams())

	run, err := svc.StartMRReworkForRun(context.Background(), user, runID, "only fix the migration thread", sampleReviewSnapshot())
	if err != nil {
		t.Fatalf("StartMRReworkForRun: %v", err)
	}
	if run.ID != newRun {
		t.Fatalf("returned run id = %s, want %s", run.ID, newRun)
	}
	// The manual/atomic path was taken: the combined create+advance call fired (its params
	// have NO TriggerSource field — trigger_source='manual' is hard-coded in the SQL). The
	// trigger-source intent is proved by the returned run (from the fake result); the exact
	// SQL literal is covered by the live-DB test.
	if fs.mrReworkAndAdvanceParams == nil {
		t.Fatal("CreateManualMRReworkRunAndAdvance store call not made (manual/atomic path not taken)")
	}
	if run.TriggerSource != "manual" {
		t.Fatalf("returned run trigger_source = %q, want manual", run.TriggerSource)
	}
	// The FULL snapshot rides the run (Decision 3).
	var snap ReviewCommentsSnapshot
	if err := json.Unmarshal(fs.mrReworkAndAdvanceParams.ReviewComments, &snap); err != nil {
		t.Fatalf("review_comments not valid jsonb: %v", err)
	}
	if len(snap.Comments) != 1 || snap.Comments[0].ID != 120 {
		t.Fatalf("snapshot did not ride the run: %+v", snap)
	}
	// Guidance is folded into the description via the shared composer.
	if !strings.Contains(fs.mrReworkAndAdvanceParams.IssueDescription, "only fix the migration thread") {
		t.Fatalf("description missing the guidance section: %q", fs.mrReworkAndAdvanceParams.IssueDescription)
	}
	// The non-counting high-water advance is folded into the SAME atomic call, carrying the
	// snapshot's max actionable id, on the ref.
	if fs.mrReworkAndAdvanceParams.HighWater != 120 || fs.mrReworkAndAdvanceParams.PipelineRef.String != "agent/issue-7" {
		t.Fatalf("combined call = %+v, want high_water 120 on agent/issue-7", fs.mrReworkAndAdvanceParams)
	}
	if fs.mrReworkAndAdvanceParams.RepoID != repo {
		t.Fatalf("combined call repo = %s, want %s", fs.mrReworkAndAdvanceParams.RepoID, repo)
	}
}

// TestStartMRReworkForRunGuidanceOnlyStillAdvances proves a guidance-only trigger with
// NOTHING new past the high-water still proceeds AND still advances the ledger
// unconditionally (GREATEST keeps the mark; the advance resets halt_notified — Decision 9).
// The advance is now INSEPARABLE from the create — one atomic call — so a guidance-only
// cycle cannot create the run without also advancing the ledger.
func TestStartMRReworkForRunGuidanceOnlyStillAdvances(t *testing.T) {
	user, repo, runID := uuid.New(), uuid.New(), uuid.New()
	fs := &fakeStore{
		runByID:           reworkableSourceRun(runID, user, repo),
		hasAnthropicToken: true,
		// high_water already above the snapshot's max actionable id (120): nothing new.
		mrReworkLedger:    store.MrReworkLedger{Ref: "agent/issue-7", AttemptCount: 5, HighWater: 500},
		repoRow:           aValidRepoRow(),
		mrReworkRunResult: store.Run{ID: uuid.New(), Kind: runkind.MRRework, TriggerSource: "manual"},
	}
	svc := New(fs, newBox(t), testParams())

	if _, err := svc.StartMRReworkForRun(context.Background(), user, runID, "please redo the naming nits", sampleReviewSnapshot()); err != nil {
		t.Fatalf("guidance-only trigger with nothing new should proceed: %v", err)
	}
	// The advance is inseparable from the create: the combined atomic call MUST have fired
	// even on a guidance-only cycle (it is what resets the halt latch — Decision 9).
	if fs.mrReworkAndAdvanceParams == nil {
		t.Fatal("CreateManualMRReworkRunAndAdvance must run even on a guidance-only cycle (halt-latch reset)")
	}
	// GREATEST(500, 120) leaves the mark at 500 in the DB; the call passes the computed max
	// actionable id (120), and the query's GREATEST preserves the higher stored value.
	if fs.mrReworkAndAdvanceParams.HighWater != 120 {
		t.Fatalf("advance passed high_water %d, want the computed max actionable id 120", fs.mrReworkAndAdvanceParams.HighWater)
	}
}

func TestStartMRReworkForRun409Classes(t *testing.T) {
	user, repo, runID := uuid.New(), uuid.New(), uuid.New()

	cases := []struct {
		name    string
		mutate  func(*fakeStore)
		wantErr error
	}{
		{
			name:    "run not found",
			mutate:  func(f *fakeStore) { f.runByIDErr = pgx.ErrNoRows },
			wantErr: ErrRunNotFound,
		},
		{
			name:    "unsupported kind",
			mutate:  func(f *fakeStore) { f.runByID.Kind = runkind.Task },
			wantErr: ErrReworkKindUnsupported,
		},
		{
			name:    "not completed",
			mutate:  func(f *fakeStore) { f.runByID.Status = "running" },
			wantErr: ErrReworkRunNotCompleted,
		},
		{
			name:    "no merge request",
			mutate:  func(f *fakeStore) { f.runByID.MrIid = pgtype.Int8{} },
			wantErr: ErrReworkNoMR,
		},
		{
			name:    "no branch (null)",
			mutate:  func(f *fakeStore) { f.runByID.Branch = pgtype.Text{} },
			wantErr: ErrReworkNoBranch,
		},
		{
			name:    "no branch (empty)",
			mutate:  func(f *fakeStore) { f.runByID.Branch = pgconv.Text("") },
			wantErr: ErrReworkNoBranch,
		},
		{
			name:    "MR merged (not open)",
			mutate:  func(f *fakeStore) { f.runByID.MrState = pgconv.Text("merged") },
			wantErr: ErrReworkMRNotOpen,
		},
		{
			name:    "owner has no token",
			mutate:  func(f *fakeStore) { f.hasAnthropicToken = false },
			wantErr: ErrReworkNoToken,
		},
		{
			name: "nothing new and no guidance",
			mutate: func(f *fakeStore) {
				f.mrReworkLedger = store.MrReworkLedger{Ref: "agent/issue-7", HighWater: 500}
			},
			wantErr: ErrReworkNothingNew,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{
				runByID:           reworkableSourceRun(runID, user, repo),
				hasAnthropicToken: true,
				mrReworkLedger:    store.MrReworkLedger{Ref: "agent/issue-7", HighWater: 100},
				repoRow:           aValidRepoRow(),
				mrReworkRunResult: store.Run{ID: uuid.New(), Kind: runkind.MRRework},
			}
			tc.mutate(fs)
			svc := New(fs, newBox(t), testParams())

			// A bare trigger (no guidance) so the nothing-new class fires; the other classes
			// bail before the guidance check anyway.
			_, err := svc.StartMRReworkForRun(context.Background(), user, runID, "", sampleReviewSnapshot())
			if err != tc.wantErr {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			// A refused trigger must not have created a run (the manual path goes through the
			// combined create+advance call).
			if fs.mrReworkAndAdvanceParams != nil {
				t.Fatalf("a refused trigger created a run: %+v", fs.mrReworkAndAdvanceParams)
			}
		})
	}
}

// TestStartMRReworkForRunMapsCreateErrors proves the create-path guard errors pass straight
// through to the caller (the handler maps them to 409s).
func TestStartMRReworkForRunMapsCreateErrors(t *testing.T) {
	user, repo, runID := uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"branch in use", ErrBranchInUse},
		{"active rework exists", ErrActiveMRReworkExists},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{
				runByID:           reworkableSourceRun(runID, user, repo),
				hasAnthropicToken: true,
				mrReworkLedger:    store.MrReworkLedger{Ref: "agent/issue-7", HighWater: 100},
				repoRow:           aValidRepoRow(),
				mrReworkRunErr:    tc.err,
			}
			svc := New(fs, newBox(t), testParams())
			if _, err := svc.StartMRReworkForRun(context.Background(), user, runID, "g", sampleReviewSnapshot()); err != tc.err {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
		})
	}
}
