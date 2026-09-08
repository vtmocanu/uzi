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
	if fs.mrReworkRunParams == nil {
		t.Fatal("CreateManualMRReworkRun store call not made")
	}
	if fs.mrReworkRunParams.TriggerSource != "manual" {
		t.Fatalf("trigger_source = %q, want manual", fs.mrReworkRunParams.TriggerSource)
	}
	// The FULL snapshot rides the run (Decision 3).
	var snap ReviewCommentsSnapshot
	if err := json.Unmarshal(fs.mrReworkRunParams.ReviewComments, &snap); err != nil {
		t.Fatalf("review_comments not valid jsonb: %v", err)
	}
	if len(snap.Comments) != 1 || snap.Comments[0].ID != 120 {
		t.Fatalf("snapshot did not ride the run: %+v", snap)
	}
	// Guidance is folded into the description via the shared composer.
	if !strings.Contains(fs.mrReworkRunParams.IssueDescription, "only fix the migration thread") {
		t.Fatalf("description missing the guidance section: %q", fs.mrReworkRunParams.IssueDescription)
	}
	// The non-counting high-water advance ran with the snapshot's max actionable id, on the ref.
	if fs.advanceHighWater == nil {
		t.Fatal("AdvanceMRReworkHighWater not called")
	}
	if fs.advanceHighWater.HighWater != 120 || fs.advanceHighWater.Ref != "agent/issue-7" {
		t.Fatalf("advance = %+v, want high_water 120 on agent/issue-7", fs.advanceHighWater)
	}
	if uuid.UUID(fs.advanceHighWater.RepoID) != repo {
		t.Fatalf("advance repo = %s, want %s", uuid.UUID(fs.advanceHighWater.RepoID), repo)
	}
}

// TestStartMRReworkForRunGuidanceOnlyStillAdvances proves a guidance-only trigger with
// NOTHING new past the high-water still proceeds AND still advances the ledger
// unconditionally (GREATEST keeps the mark; the call resets halt_notified — Decision 9).
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
	if fs.advanceHighWater == nil {
		t.Fatal("AdvanceMRReworkHighWater must run even on a guidance-only cycle (halt-latch reset)")
	}
	// GREATEST(500, 120) leaves the mark at 500 in the DB; the call passes the computed max
	// actionable id (120), and the query's GREATEST preserves the higher stored value.
	if fs.advanceHighWater.HighWater != 120 {
		t.Fatalf("advance passed high_water %d, want the computed max actionable id 120", fs.advanceHighWater.HighWater)
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
			// A refused trigger must not have created a run.
			if fs.mrReworkRunParams != nil {
				t.Fatalf("a refused trigger created a run: %+v", fs.mrReworkRunParams)
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
