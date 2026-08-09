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

// fakeSettings is a SettingsReader stub for the judge gate/claim tests, and since
// PRD #102 M6 for the run-eligibility PRD-label gate too. prdLabel is left zero by
// the judge tests, which is the "unconfigured" case Service.prdLabel resolves to
// the compiled-in default — the same thing a real Cache does.
type fakeSettings struct {
	enabled        bool
	model          string
	prdLabel       string
	prdlessEnabled bool
	prdlessLabel   string
	err            error
}

func (f fakeSettings) JudgeEnabled(context.Context) (bool, error)   { return f.enabled, f.err }
func (f fakeSettings) JudgeModel(context.Context) (string, error)   { return f.model, f.err }
func (f fakeSettings) PRDLabel(context.Context) (string, error)     { return f.prdLabel, f.err }
func (f fakeSettings) PrdlessEnabled(context.Context) (bool, error) { return f.prdlessEnabled, f.err }
func (f fakeSettings) PrdlessLabel(context.Context) (string, error) { return f.prdlessLabel, f.err }

// -------------------------------------------------------------------------
// command-not-found scan (Decision 4)
// -------------------------------------------------------------------------

func TestScanCommandNotFound(t *testing.T) {
	// kubectl/helm/terraform arrive via high-confidence forms and need no corroboration;
	// jq is the low-confidence `X: not found` form, so a tool_use invoking jq is appended
	// to corroborate it — exercising the low-confidence path rather than bypassing it (a
	// run sees `jq: not found` precisely because it RAN jq).
	payloads := append(rawResultRows(
		`{"text":"bash: kubectl: command not found"}`,
		`{"text":"zsh: command not found: helm"}`,
		`{"text":"exec: \"terraform\": executable file not found in $PATH"}`,
		`{"text":"/bin/sh: 1: jq: not found"}`,
		`{"text":"kubectl: command not found"}`, // duplicate → deduped
		`{"text":"all good here, tests passed"}`,
	), traceUse(1, "u-jq", "jq -r .name package.json"))
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
	if got := scanCommandNotFound(rawResultRows(`{"text":"go test ./... ok"}`)); len(got) != 0 {
		t.Fatalf("clean output must yield no hits, got %v", got)
	}
}

// TestScanCommandNotFoundIgnoresToolUseRows pins the HARD RULE of the PRD #121 M3
// widening: the not-found regexes see tool_result rows ONLY. The trace now carries
// tool_use payloads too, and those hold the command the agent TYPED — which by
// definition never ran. An agent echoing the phrase must not report a missing tool.
//
// This is a regression the widening itself would introduce, not one it inherits, so
// it is pinned here rather than left to the suppression fixture.
func TestScanCommandNotFoundIgnoresToolUseRows(t *testing.T) {
	rows := []store.ListToolTraceForRunRow{
		traceUse(1, "tu-1", `echo "foo: command not found"`),
		traceUse(2, "tu-2", `grep -r "zsh: command not found: bar" .`),
	}
	if got := scanCommandNotFound(rows); len(got) != 0 {
		t.Fatalf("tool_use payloads must never be scanned for missing-tool evidence; got %v", got)
	}
}

// TestScanCommandNotFoundFiltersNoise: the low-confidence "X: not found" form drops
// HTTP/line numbers and shared-object/header files but keeps a real missing tool.
func TestScanCommandNotFoundFiltersNoise(t *testing.T) {
	// The noise tokens are dropped by noisyShToken BEFORE corroboration, so they stay
	// dropped regardless. shellcheck is the low-confidence real-tool case, so a tool_use
	// invoking shellcheck is appended to corroborate it — keeping the "a real missing tool
	// survives the noise filter" intent while exercising the low-confidence path.
	payloads := append(rawResultRows(
		`{"text":"/bin/sh: 1: 404: not found"}`,        // numeric → dropped
		`{"text":"ld: libssl.so.1: not found"}`,        // shared lib → dropped
		`{"text":"link error: foo.o: not found"}`,      // object file → dropped
		`{"text":"/bin/sh: 1: shellcheck: not found"}`, // real tool → kept
	), traceUse(1, "u-sc", "shellcheck x.sh"))
	got := scanCommandNotFound(payloads)
	cmds := map[string]bool{}
	for _, m := range got {
		cmds[m.Command] = true
	}
	if !cmds["shellcheck"] {
		t.Errorf("a real missing tool must survive the noise filter; got %v", cmds)
	}
	for _, noise := range []string{"404", "libssl.so.1", "foo.o"} {
		if cmds[noise] {
			t.Errorf("noise token %q should be filtered from the sh 'not found' form", noise)
		}
	}
}

// TestRegisterEnqueuesJudgeForOrphanFailedRuns: a run failed at register time (worker
// restart, over the re-queue cap) is a worker-lost run Decision 2 wants judged — the
// same as the sweeper's over-cap fail. Register funnels it.
func TestRegisterEnqueuesJudgeForOrphanFailedRuns(t *testing.T) {
	orphan := uuid.New()
	fs := &fakeStore{
		orphanFailedRuns: []uuid.UUID{orphan},
		runByIDPlain:     store.Run{ID: orphan, UserID: uuid.New(), Kind: RunKindIssue, Status: "failed"},
		userByID:         store.User{JudgeEnabled: true},
		anthropic:        []byte("sealed"),
		registerResult:   store.Worker{ID: uuid.New(), Status: "online"},
	}
	svc := New(fs, newBox(t), testParams())
	svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})

	if _, err := svc.Register(context.Background(), worker(), "v1", "", nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if fs.createdJudgeRun == nil {
		t.Fatal("a register-time orphan-failed run must enqueue a judge (Decision 2)")
	}
	if !fs.createdJudgeRun.TargetRunID.Valid || uuid.UUID(fs.createdJudgeRun.TargetRunID.Bytes) != orphan {
		t.Errorf("judge targets %v, want the orphaned run %v", fs.createdJudgeRun.TargetRunID, orphan)
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
		claimRun:      judgeRun(uid, target),
		anthropic:     sealedTok,
		toolTraceRows: rawResultRows(`{"text":"bash: shellcheck: command not found"}`),
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
	res, err := svc.PostReview(context.Background(), worker(), target, ReviewSubmission{
		Verdict: "issues", SummaryMd: "needs a tool", JudgeModel: "haiku", Status: "complete",
		Recommendations: []ReviewRecommendation{{Category: "install_worker_tool", Target: "shellcheck", RationaleMd: "missing", Confidence: "high"}},
	})
	if err != nil {
		t.Fatalf("PostReview: %v", err)
	}
	if res.OwnerID != owner {
		t.Errorf("review result owner = %v, want %v", res.OwnerID, owner)
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

// TestSubmitInputRejectServerSideEnqueuesJudge: a server-side plan REJECT commits the
// run to 'failed' (a judged status), so it must enqueue a judge (Decision 2 — the
// cancel/reject trigger point).
func TestSubmitInputRejectServerSideEnqueuesJudge(t *testing.T) {
	user, runID := uuid.New(), uuid.New()
	fs := &fakeStore{
		runByID:      store.Run{ID: runID, UserID: user, Status: "awaiting_approval"},          // GetRun + no worker ⇒ no live poller
		runByIDPlain: store.Run{ID: runID, UserID: user, Status: "failed", Kind: RunKindIssue}, // post-reject reload
		userByID:     store.User{JudgeEnabled: true},
		anthropic:    []byte("sealed"),
	}
	svc := New(fs, newBox(t), testParams())
	svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})

	res, err := svc.SubmitInput(context.Background(), user, runID, "reject_plan", "", nil)
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if !res.ServerSide {
		t.Fatal("a reject with no live poller must be applied server-side")
	}
	if fs.rejected == nil {
		t.Fatal("RejectRunServerSide not called")
	}
	if fs.createdJudgeRun == nil {
		t.Fatal("a server-side reject → failed run must enqueue a judge")
	}
}

// TestSubmitInputCancelServerSideDoesNotEnqueueJudge: a server-side CANCEL commits
// 'cancelled', which is never judged (Decision 2).
func TestSubmitInputCancelServerSideDoesNotEnqueueJudge(t *testing.T) {
	user, runID := uuid.New(), uuid.New()
	fs := &fakeStore{
		runByID:      store.Run{ID: runID, UserID: user, Status: "queued"},
		runByIDPlain: store.Run{ID: runID, UserID: user, Status: "cancelled", Kind: RunKindIssue},
		userByID:     store.User{JudgeEnabled: true},
		anthropic:    []byte("sealed"),
	}
	svc := New(fs, newBox(t), testParams())
	svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})
	if _, err := svc.SubmitInput(context.Background(), user, runID, "cancel", "", nil); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if fs.createdJudgeRun != nil {
		t.Fatal("a cancelled run must never be judged")
	}
}

func TestPostReviewRejectsUnauthorizedWorker(t *testing.T) {
	fs := &fakeStore{activeJudgeRunErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	_, err := svc.PostReview(context.Background(), worker(), uuid.New(), ReviewSubmission{Verdict: "ok", Status: "complete"})
	if err != ErrRunNotFound {
		t.Fatalf("err = %v, want ErrRunNotFound (no active judge run for this worker/target)", err)
	}
	if fs.upsertedReview != nil {
		t.Fatal("an unauthorized review must not persist")
	}
}
