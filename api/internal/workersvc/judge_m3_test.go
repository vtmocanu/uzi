package workersvc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeSettings is a SettingsReader stub for the judge gate/claim tests.
type fakeSettings struct {
	enabled bool
	model   string
	err     error
}

func (f fakeSettings) JudgeEnabled(context.Context) (bool, error) { return f.enabled, f.err }
func (f fakeSettings) JudgeModel(context.Context) (string, error) { return f.model, f.err }

// -------------------------------------------------------------------------
// command-not-found scan (Decision 4)
// -------------------------------------------------------------------------

func TestScanCommandNotFound(t *testing.T) {
	payloads := [][]byte{
		[]byte(`{"text":"bash: kubectl: command not found"}`),
		[]byte(`{"text":"zsh: command not found: helm"}`),
		[]byte(`{"text":"exec: \"terraform\": executable file not found in $PATH"}`),
		[]byte(`{"text":"/bin/sh: 1: jq: not found"}`),
		[]byte(`{"text":"kubectl: command not found"}`), // duplicate → deduped
		[]byte(`{"text":"all good here, tests passed"}`),
	}
	got := scanCommandNotFound(payloads)
	cmds := map[string]bool{}
	for _, m := range got {
		cmds[m.Command] = true
		if m.Evidence == "" {
			t.Errorf("hit for %q has empty evidence", m.Command)
		}
	}
	for _, want := range []string{"kubectl", "helm", "terraform", "jq"} {
		if !cmds[want] {
			t.Errorf("expected %q flagged; got %v", want, cmds)
		}
	}
	if len(got) != 4 {
		t.Errorf("expected 4 distinct missing tools (kubectl deduped), got %d: %v", len(got), got)
	}
}

func TestScanCommandNotFoundEmptyWhenClean(t *testing.T) {
	if got := scanCommandNotFound([][]byte{[]byte(`{"text":"go test ./... ok"}`)}); len(got) != 0 {
		t.Fatalf("clean output must yield no hits, got %v", got)
	}
}

// -------------------------------------------------------------------------
// terminal-funnel enqueue gating matrix (Decision 2)
// -------------------------------------------------------------------------

// eligibleRun is a finished issue run whose owner is opted in with a token — the
// happy-path fixture the gating tests each break one gate of.
func eligibleFixture(t *testing.T) (*fakeStore, *Service, store.Run) {
	t.Helper()
	fs := &fakeStore{
		userByID:  store.User{JudgeEnabled: true},
		anthropic: []byte("sealed-token-bytes"), // present ⇒ token gate passes
	}
	svc := New(fs, newBox(t), testParams())
	svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})
	run := store.Run{ID: uuid.New(), UserID: uuid.New(), Kind: RunKindIssue, Status: "completed", IssueTitle: "Do X"}
	return fs, svc, run
}

func TestEnqueueJudgeHappyPath(t *testing.T) {
	fs, svc, run := eligibleFixture(t)
	svc.maybeEnqueueJudge(context.Background(), run)
	if fs.createdJudgeRun == nil {
		t.Fatal("expected a judge run enqueued")
	}
	if fs.createdJudgeRun.UserID != run.UserID {
		t.Errorf("judge run owner = %v, want the target's owner %v (never cross-user)", fs.createdJudgeRun.UserID, run.UserID)
	}
	if !fs.createdJudgeRun.TargetRunID.Valid || uuid.UUID(fs.createdJudgeRun.TargetRunID.Bytes) != run.ID {
		t.Errorf("judge run target = %v, want %v", fs.createdJudgeRun.TargetRunID, run.ID)
	}
}

func TestEnqueueJudgeGatesBlock(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(fs *fakeStore, svc *Service, run *store.Run)
	}{
		{"global off", func(fs *fakeStore, svc *Service, run *store.Run) { svc.SetSettings(fakeSettings{enabled: false}) }},
		{"no settings wired", func(fs *fakeStore, svc *Service, run *store.Run) { svc.settings = nil }},
		{"user opted out", func(fs *fakeStore, svc *Service, run *store.Run) { fs.userByID = store.User{JudgeEnabled: false} }},
		{"no anthropic token", func(fs *fakeStore, svc *Service, run *store.Run) { fs.anthropicErr = pgx.ErrNoRows }},
		{"cancelled status", func(fs *fakeStore, svc *Service, run *store.Run) { run.Status = "cancelled" }},
		{"non-terminal status", func(fs *fakeStore, svc *Service, run *store.Run) { run.Status = "running" }},
		{"chat kind (not eligible)", func(fs *fakeStore, svc *Service, run *store.Run) { run.Kind = RunKindChat }},
		{"judge kind (no recursion)", func(fs *fakeStore, svc *Service, run *store.Run) { run.Kind = RunKindJudge }},
		{"self_improve kind (no recursion)", func(fs *fakeStore, svc *Service, run *store.Run) { run.Kind = RunKindSelfImprove }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, svc, run := eligibleFixture(t)
			tc.mutate(fs, svc, &run)
			svc.maybeEnqueueJudge(context.Background(), run)
			if fs.createdJudgeRun != nil {
				t.Fatalf("%s: no judge run should be enqueued, got %+v", tc.name, fs.createdJudgeRun)
			}
		})
	}
}

func TestEnqueueJudgeCiFixIsEligible(t *testing.T) {
	fs, svc, run := eligibleFixture(t)
	run.Kind = RunKindCIFix
	run.Status = "failed" // a failed ci_fix is worth judging
	svc.maybeEnqueueJudge(context.Background(), run)
	if fs.createdJudgeRun == nil {
		t.Fatal("a failed ci_fix run is eligible and should enqueue a judge")
	}
}

func TestEnqueueJudgeDuplicateSwallowed(t *testing.T) {
	fs, svc, run := eligibleFixture(t)
	fs.createJudgeRunErr = &pgconn.PgError{Code: "23505"} // a judge is already active for this target
	// Must not panic or propagate — a duplicate is expected, not an error.
	svc.maybeEnqueueJudge(context.Background(), run)
}

// -------------------------------------------------------------------------
// judge claim carries model + command-not-found signal (Decisions 3, 4)
// -------------------------------------------------------------------------

func TestJudgeClaimCarriesModelAndSignal(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-judge-token-abcdef1234567890"))
	uid, target := uuid.New(), uuid.New()
	fs := &fakeStore{
		claimRun:           judgeRun(uid, target),
		anthropic:          sealedTok,
		toolResultPayloads: [][]byte{[]byte(`{"text":"bash: shellcheck: command not found"}`)},
	}
	svc := New(fs, box, testParams())
	svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil || payload == nil {
		t.Fatalf("Claim: payload=%v err=%v", payload, err)
	}
	if payload.JudgeModel == nil || *payload.JudgeModel != "haiku" {
		t.Errorf("JudgeModel = %v, want haiku", payload.JudgeModel)
	}
	if payload.JudgeSignal == nil || len(payload.JudgeSignal.MissingTools) != 1 || payload.JudgeSignal.MissingTools[0].Command != "shellcheck" {
		t.Errorf("JudgeSignal did not carry the shellcheck miss: %+v", payload.JudgeSignal)
	}
}

// -------------------------------------------------------------------------
// trace + review authorization (Decision 3, audit H1)
// -------------------------------------------------------------------------

func TestJudgeTraceRejectsWhenNoActiveJudgeRun(t *testing.T) {
	fs := &fakeStore{activeJudgeRunErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.JudgeTrace(context.Background(), worker(), uuid.New(), 0, 0); err != ErrRunNotFound {
		t.Fatalf("err = %v, want ErrRunNotFound (a worker with no active judge run for the target gets 404)", err)
	}
}

func TestJudgeTraceRejectsOwnerMismatch(t *testing.T) {
	target := uuid.New()
	fs := &fakeStore{
		activeJudgeRun: store.Run{ID: uuid.New(), UserID: uuid.New(), Kind: RunKindJudge}, // judge owner A
		runByIDPlain:   store.Run{ID: target, UserID: uuid.New()},                         // target owner B (mismatch)
	}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.JudgeTrace(context.Background(), worker(), target, 0, 0); err != ErrRunNotFound {
		t.Fatalf("err = %v, want ErrRunNotFound on target/judge owner mismatch", err)
	}
}

func TestJudgeTraceHappyPath(t *testing.T) {
	owner, target := uuid.New(), uuid.New()
	fs := &fakeStore{
		activeJudgeRun: store.Run{ID: uuid.New(), UserID: owner, Kind: RunKindJudge},
		runByIDPlain:   store.Run{ID: target, UserID: owner, Kind: RunKindIssue, Status: "completed"},
	}
	svc := New(fs, newBox(t), testParams())
	res, err := svc.JudgeTrace(context.Background(), worker(), target, 0, 0)
	if err != nil {
		t.Fatalf("JudgeTrace: %v", err)
	}
	if res.Target.ID != target {
		t.Errorf("trace target = %v, want %v", res.Target.ID, target)
	}
}

func TestPostReviewPersistsVerdictAndRecs(t *testing.T) {
	owner, target := uuid.New(), uuid.New()
	judgeID := uuid.New()
	fs := &fakeStore{
		activeJudgeRun: store.Run{ID: judgeID, UserID: owner, Kind: RunKindJudge},
		runByIDPlain:   store.Run{ID: target, UserID: owner, Kind: RunKindIssue, Status: "completed"},
	}
	svc := New(fs, newBox(t), testParams())
	err := svc.PostReview(context.Background(), worker(), target, ReviewSubmission{
		Verdict: "issues", SummaryMd: "needs a tool", JudgeModel: "haiku", Status: "complete",
		Recommendations: []ReviewRecommendation{{Category: "install_worker_tool", Target: "shellcheck", RationaleMd: "missing", Confidence: "high"}},
	})
	if err != nil {
		t.Fatalf("PostReview: %v", err)
	}
	if fs.upsertedReview == nil {
		t.Fatal("expected a review upsert")
	}
	if fs.upsertedReview.TargetRunID != target || fs.upsertedReview.UserID != owner {
		t.Errorf("review scoped wrong: target=%v user=%v", fs.upsertedReview.TargetRunID, fs.upsertedReview.UserID)
	}
	if !fs.upsertedReview.JudgeRunID.Valid || uuid.UUID(fs.upsertedReview.JudgeRunID.Bytes) != judgeID {
		t.Errorf("review judge_run_id = %v, want %v", fs.upsertedReview.JudgeRunID, judgeID)
	}
	var recs []ReviewRecommendation
	if err := json.Unmarshal(fs.upsertedReview.Recommendations, &recs); err != nil {
		t.Fatalf("recommendations not valid json: %v", err)
	}
	if len(recs) != 1 || recs[0].Category != "install_worker_tool" || recs[0].Target != "shellcheck" {
		t.Errorf("recommendation not carried: %+v", recs)
	}
}

func TestPostReviewRejectsUnauthorizedWorker(t *testing.T) {
	fs := &fakeStore{activeJudgeRunErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	err := svc.PostReview(context.Background(), worker(), uuid.New(), ReviewSubmission{Verdict: "ok", Status: "complete"})
	if err != ErrRunNotFound {
		t.Fatalf("err = %v, want ErrRunNotFound (no active judge run for this worker/target)", err)
	}
	if fs.upsertedReview != nil {
		t.Fatal("an unauthorized review must not persist")
	}
}
