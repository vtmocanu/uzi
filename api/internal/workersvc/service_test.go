package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeStore embeds the Store interface so unimplemented methods panic if a test
// path reaches them unexpectedly; the tests override only what they exercise.
type fakeStore struct {
	Store

	// Claim path.
	claimRun     store.Run
	claimErr     error
	claimParams  *store.ClaimRunParams
	claimCtx     store.GetRunClaimContextRow
	claimCtxErr  error
	anthropic    []byte
	anthropicErr error
	templates    []store.AgentTemplate
	markedFailed *store.MarkRunFailedByIDParams

	// Ownership + messages + state.
	runOwned         store.Run
	runOwnedErr      error
	insertedSeqs     map[int32]bool
	insertedMessages []store.InsertRunMessageParams
	lastSeqUpdated   *int32
	setRunningParams *store.SetRunRunningParams
	setAwaiting      *store.SetRunAwaitingApprovalParams
	setCompleted     *store.SetRunCompletedParams
	setFailed        *store.SetRunFailedParams
	setRunningRows   int64
	setCompletedRows int64
	consumeRows      []store.ConsumeRunInputsRow

	// Register + heartbeat.
	failOverCap    *store.FailWorkerRunsOverCapParams
	requeueWorker  *store.RequeueWorkerRunsParams
	registerParams *store.RegisterWorkerParams
	registerResult store.Worker
	heartbeat      store.Worker
	callOrder      []string

	// Sweep.
	staleCutoff pgtype.Timestamptz
	claimCutoff pgtype.Timestamptz
	runCutoff   pgtype.Timestamptz
	sweepMax    int32

	// Submit input.
	runByID       store.Run
	runByIDErr    error
	workerByID    store.Worker
	workerByIDErr error
	createdInput  *store.CreateRunInputParams
	cancelled     *store.CancelRunServerSideParams
	rejected      *store.RejectRunServerSideParams

	// Create run.
	repoErr         error
	issueByID       store.Issue
	issueByIDErr    error
	createRunResult store.Run
	createRunErr    error
	createRunParams *store.CreateRunParams

	// Create worker.
	createWorkerResult store.Worker
	createWorkerParams *store.CreateWorkerParams
}

func (f *fakeStore) ClaimRun(_ context.Context, arg store.ClaimRunParams) (store.Run, error) {
	f.claimParams = &arg
	return f.claimRun, f.claimErr
}
func (f *fakeStore) GetRunClaimContext(context.Context, uuid.UUID) (store.GetRunClaimContextRow, error) {
	return f.claimCtx, f.claimCtxErr
}
func (f *fakeStore) GetUserSecretCiphertext(context.Context, store.GetUserSecretCiphertextParams) ([]byte, error) {
	return f.anthropic, f.anthropicErr
}
func (f *fakeStore) ListAgentTemplates(context.Context) ([]store.AgentTemplate, error) {
	return f.templates, nil
}
func (f *fakeStore) MarkRunFailedByID(_ context.Context, arg store.MarkRunFailedByIDParams) (int64, error) {
	f.markedFailed = &arg
	return 1, nil
}
func (f *fakeStore) GetRunOwnedByWorker(context.Context, store.GetRunOwnedByWorkerParams) (store.Run, error) {
	return f.runOwned, f.runOwnedErr
}
func (f *fakeStore) InsertRunMessage(_ context.Context, arg store.InsertRunMessageParams) (int64, error) {
	f.insertedMessages = append(f.insertedMessages, arg)
	if f.insertedSeqs == nil {
		f.insertedSeqs = map[int32]bool{}
	}
	if f.insertedSeqs[arg.Seq] {
		return 0, nil // ON CONFLICT DO NOTHING
	}
	f.insertedSeqs[arg.Seq] = true
	return 1, nil
}
func (f *fakeStore) UpdateRunLastSeq(_ context.Context, arg store.UpdateRunLastSeqParams) (int64, error) {
	v := arg.Seq
	f.lastSeqUpdated = &v
	return 1, nil
}
func (f *fakeStore) SetRunRunning(_ context.Context, arg store.SetRunRunningParams) (int64, error) {
	f.setRunningParams = &arg
	return f.setRunningRows, nil
}
func (f *fakeStore) SetRunAwaitingApproval(_ context.Context, arg store.SetRunAwaitingApprovalParams) (int64, error) {
	f.setAwaiting = &arg
	return 1, nil
}
func (f *fakeStore) SetRunCompleted(_ context.Context, arg store.SetRunCompletedParams) (int64, error) {
	f.setCompleted = &arg
	return f.setCompletedRows, nil
}
func (f *fakeStore) SetRunFailed(_ context.Context, arg store.SetRunFailedParams) (int64, error) {
	f.setFailed = &arg
	return 1, nil
}
func (f *fakeStore) ConsumeRunInputs(context.Context, uuid.UUID) ([]store.ConsumeRunInputsRow, error) {
	return f.consumeRows, nil
}
func (f *fakeStore) FailWorkerRunsOverCap(_ context.Context, arg store.FailWorkerRunsOverCapParams) (int64, error) {
	f.failOverCap = &arg
	f.callOrder = append(f.callOrder, "fail_over_cap")
	return 0, nil
}
func (f *fakeStore) RequeueWorkerRuns(_ context.Context, arg store.RequeueWorkerRunsParams) (int64, error) {
	f.requeueWorker = &arg
	f.callOrder = append(f.callOrder, "requeue_worker")
	return 0, nil
}
func (f *fakeStore) RegisterWorker(_ context.Context, arg store.RegisterWorkerParams) (store.Worker, error) {
	f.registerParams = &arg
	f.callOrder = append(f.callOrder, "register")
	return f.registerResult, nil
}
func (f *fakeStore) HeartbeatWorker(context.Context, uuid.UUID) (store.Worker, error) {
	return f.heartbeat, nil
}
func (f *fakeStore) MarkStaleWorkersOffline(_ context.Context, cutoff pgtype.Timestamptz) (int64, error) {
	f.staleCutoff = cutoff
	f.callOrder = append(f.callOrder, "mark_stale")
	return 0, nil
}
func (f *fakeStore) SweepClaimedNeverStarted(_ context.Context, cutoff pgtype.Timestamptz) (int64, error) {
	f.claimCutoff = cutoff
	f.callOrder = append(f.callOrder, "claimed_never_started")
	return 0, nil
}
func (f *fakeStore) SweepRunningTimeout(_ context.Context, arg store.SweepRunningTimeoutParams) (int64, error) {
	f.runCutoff = arg.Cutoff
	f.callOrder = append(f.callOrder, "running_timeout")
	return 0, nil
}
func (f *fakeStore) FailRunsOfStaleWorkersOverCap(_ context.Context, arg store.FailRunsOfStaleWorkersOverCapParams) (int64, error) {
	f.sweepMax = arg.MaxRequeues
	f.callOrder = append(f.callOrder, "stale_fail_over_cap")
	return 0, nil
}
func (f *fakeStore) RequeueRunsOfStaleWorkers(_ context.Context, arg store.RequeueRunsOfStaleWorkersParams) (int64, error) {
	f.callOrder = append(f.callOrder, "stale_requeue")
	return 0, nil
}
func (f *fakeStore) GetRunByIDForUser(context.Context, store.GetRunByIDForUserParams) (store.Run, error) {
	return f.runByID, f.runByIDErr
}
func (f *fakeStore) GetWorkerByID(context.Context, uuid.UUID) (store.Worker, error) {
	return f.workerByID, f.workerByIDErr
}
func (f *fakeStore) CreateRunInput(_ context.Context, arg store.CreateRunInputParams) (store.RunUserInput, error) {
	f.createdInput = &arg
	return store.RunUserInput{}, nil
}
func (f *fakeStore) CancelRunServerSide(_ context.Context, arg store.CancelRunServerSideParams) (int64, error) {
	f.cancelled = &arg
	return 1, nil
}
func (f *fakeStore) RejectRunServerSide(_ context.Context, arg store.RejectRunServerSideParams) (int64, error) {
	f.rejected = &arg
	return 1, nil
}
func (f *fakeStore) GetRepoForUser(context.Context, store.GetRepoForUserParams) (store.GetRepoForUserRow, error) {
	return store.GetRepoForUserRow{}, f.repoErr
}
func (f *fakeStore) GetIssueByIID(context.Context, store.GetIssueByIIDParams) (store.Issue, error) {
	return f.issueByID, f.issueByIDErr
}
func (f *fakeStore) CreateRun(_ context.Context, arg store.CreateRunParams) (store.Run, error) {
	f.createRunParams = &arg
	return f.createRunResult, f.createRunErr
}
func (f *fakeStore) CreateWorker(_ context.Context, arg store.CreateWorkerParams) (store.Worker, error) {
	f.createWorkerParams = &arg
	return f.createWorkerResult, nil
}

// testParams are sane fixed knobs for the tests.
func testParams() Params {
	return Params{
		RunTimeout:           2 * time.Hour,
		RunIdleTimeout:       10 * time.Minute,
		RunMaxIterations:     5,
		RunMaxRequeues:       1,
		WorkerHeartbeatStale: 45 * time.Second,
		WorkerAffinityGrace:  2 * time.Minute,
		ClaimGrace:           5 * time.Minute,
	}
}

func newBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 1) // non-identical → passes weak-key guard when loaded elsewhere
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	return box
}

func worker() store.Worker {
	return store.Worker{ID: uuid.New(), UserID: uuid.New()}
}

// -------------------------------------------------------------------------
// Claim: the fake-worker payload + secret redaction
// -------------------------------------------------------------------------

func TestClaimAssemblesPayloadWithDecryptedSecrets(t *testing.T) {
	// Opaque fake secrets (deliberately not a real PAT/token format, so secret
	// scanners don't flag the fixtures). The code treats both as opaque bytes.
	const pat = "bot-pat-REDACTIONTEST-abcdef1234567890"
	const token = "anthropic-oauth-CLAIMTEST-abcdef1234567890"

	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte(pat))
	sealedTok, _ := box.Seal([]byte(token))

	branch := "agent/issue-4"
	fs := &fakeStore{
		claimRun: store.Run{
			ID: uuid.New(), IssueIid: 4, IssueTitle: "Do the thing",
			IssueDescription: "see prds/4.md", Status: "claimed",
			LastSeq: 7, IterationCount: 2, RequeueCount: 1,
			SessionID: pgText("sess-abc"), Branch: pgText(branch),
		},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/grp/proj", RepoPath: "grp/proj",
			DefaultBranch: pgText("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic: sealedTok,
		templates: []store.AgentTemplate{
			{Name: "coder", Description: "writes code", PromptBody: "you code", Tools: []byte(`["Read","Edit"]`)},
			{Name: "reviewer", Description: "reviews", PromptBody: "you review", Model: pgText("claude-opus-4-8")},
		},
	}

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	svc := New(fs, box, testParams())
	payload, err := svc.Claim(context.Background(), worker())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a payload, got idle")
	}

	// Decrypted credentials are delivered in the payload (the sole channel).
	if payload.Credentials.BotPAT != pat {
		t.Fatal("bot PAT not decrypted into the payload")
	}
	if payload.Credentials.AnthropicToken != token {
		t.Fatal("anthropic token not decrypted into the payload")
	}
	// Resume fields carried through.
	if payload.Run.LastSeq != 7 || payload.Run.IterationCount != 2 || payload.Run.RequeueCount != 1 {
		t.Fatalf("resume counters wrong: %+v", payload.Run)
	}
	if payload.Run.SessionID == nil || *payload.Run.SessionID != "sess-abc" {
		t.Fatal("session id not carried for resume")
	}
	if payload.Run.Branch == nil || *payload.Run.Branch != branch {
		t.Fatal("branch not carried for resume")
	}
	// Repo + clone URL.
	if payload.Repo.CloneURL != "https://gitlab.example.com/grp/proj.git" {
		t.Fatalf("clone url = %q", payload.Repo.CloneURL)
	}
	// Structured agents.
	if len(payload.Agents) != 2 || payload.Agents[0].Name != "coder" {
		t.Fatalf("agents wrong: %+v", payload.Agents)
	}
	if len(payload.Agents[0].Tools) != 2 || payload.Agents[0].Tools[0] != "Read" {
		t.Fatalf("coder tools not decoded: %+v", payload.Agents[0].Tools)
	}
	if payload.Agents[1].Model == nil || *payload.Agents[1].Model != "claude-opus-4-8" {
		t.Fatal("reviewer model not carried")
	}
	// Config caps.
	if payload.Config.RunTimeoutSeconds != 7200 || payload.Config.MaxIterations != 5 {
		t.Fatalf("config caps wrong: %+v", payload.Config)
	}

	// The plaintext secrets must not appear in any log line.
	if strings.Contains(logs.String(), pat) || strings.Contains(logs.String(), token) {
		t.Fatal("a log line leaked a decrypted secret")
	}
}

func TestClaimIdleReturnsNilPayload(t *testing.T) {
	fs := &fakeStore{claimErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	payload, err := svc.Claim(context.Background(), worker())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload != nil {
		t.Fatal("expected idle (nil payload)")
	}
}

func TestClaimFailsRunWhenAnthropicTokenMissing(t *testing.T) {
	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("bot-pat-something-long-enough"))
	fs := &fakeStore{
		claimRun:     store.Run{ID: uuid.New(), Status: "claimed"},
		claimCtx:     store.GetRunClaimContextRow{TokenCiphertext: sealedPAT, RepoWebUrl: "https://x/y"},
		anthropicErr: pgx.ErrNoRows,
	}
	svc := New(fs, box, testParams())
	payload, err := svc.Claim(context.Background(), worker())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload != nil {
		t.Fatal("a run with no Anthropic token must not hand out a payload")
	}
	if fs.markedFailed == nil {
		t.Fatal("the run should have been marked failed")
	}
	if !fs.markedFailed.FailureReason.Valid || !strings.Contains(fs.markedFailed.FailureReason.String, "Anthropic token") {
		t.Fatalf("failure reason unclear: %+v", fs.markedFailed.FailureReason)
	}
}

func TestClaimFailsRunWhenPATUndecryptable(t *testing.T) {
	box := newBox(t)
	fs := &fakeStore{
		claimRun: store.Run{ID: uuid.New(), Status: "claimed"},
		claimCtx: store.GetRunClaimContextRow{TokenCiphertext: []byte("not-a-valid-ciphertext"), RepoWebUrl: "https://x/y"},
	}
	svc := New(fs, box, testParams())
	payload, err := svc.Claim(context.Background(), worker())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload != nil {
		t.Fatal("an undecryptable PAT must not hand out a payload")
	}
	if fs.markedFailed == nil {
		t.Fatal("the run should have been marked failed")
	}
}

func TestClaimPassesAffinityCutoff(t *testing.T) {
	fs := &fakeStore{claimErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	fixed := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return fixed }

	if _, err := svc.Claim(context.Background(), worker()); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if fs.claimParams == nil {
		t.Fatal("ClaimRun not called")
	}
	want := fixed.Add(-2 * time.Minute)
	if !fs.claimParams.AffinityCutoff.Time.Equal(want) {
		t.Fatalf("affinity cutoff = %v, want now-grace %v", fs.claimParams.AffinityCutoff.Time, want)
	}
}

// -------------------------------------------------------------------------
// Messages + state (fake worker reporting)
// -------------------------------------------------------------------------

func TestAppendMessagesPersistsAndAdvancesLastSeq(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), LastSeq: 0}}
	svc := New(fs, newBox(t), testParams())

	msgs := []IncomingMessage{
		{Seq: 1, Kind: "text", Payload: json.RawMessage(`{"t":"hi"}`)},
		{Seq: 2, Kind: "tool_use", Agent: "coder", Payload: json.RawMessage(`{"tool":"Edit"}`)},
		{Seq: 3, Kind: "status", Payload: json.RawMessage(`"running"`)},
	}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	if len(fs.insertedMessages) != 3 {
		t.Fatalf("expected 3 inserts, got %d", len(fs.insertedMessages))
	}
	if fs.lastSeqUpdated == nil || *fs.lastSeqUpdated != 3 {
		t.Fatalf("last_seq should advance to 3, got %v", fs.lastSeqUpdated)
	}
}

func TestAppendMessagesRejectsInvalid(t *testing.T) {
	w := worker()
	base := store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID)}
	bad := [][]IncomingMessage{
		{{Seq: 0, Kind: "text", Payload: json.RawMessage(`{}`)}},
		{{Seq: 1, Kind: "", Payload: json.RawMessage(`{}`)}},
		{{Seq: 1, Kind: "text", Payload: json.RawMessage(``)}},
		{{Seq: 1, Kind: "text", Payload: json.RawMessage(`{not json`)}},
	}
	for i, msgs := range bad {
		fs := &fakeStore{runOwned: base}
		svc := New(fs, newBox(t), testParams())
		if err := svc.AppendMessages(context.Background(), w, base.ID, msgs); err != ErrInvalidMessage {
			t.Fatalf("case %d: err = %v, want ErrInvalidMessage", i, err)
		}
	}
}

func TestAppendMessagesRejectsForeignRun(t *testing.T) {
	fs := &fakeStore{runOwnedErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	err := svc.AppendMessages(context.Background(), worker(), uuid.New(),
		[]IncomingMessage{{Seq: 1, Kind: "text", Payload: json.RawMessage(`{}`)}})
	if err != ErrRunNotOwned {
		t.Fatalf("err = %v, want ErrRunNotOwned", err)
	}
	if len(fs.insertedMessages) != 0 {
		t.Fatal("no messages should be inserted for a run the worker does not own")
	}
}

func TestSetStateAlreadyTerminalIsSuccess(t *testing.T) {
	w := worker()
	// The setter no-ops (0 rows) because the run was cancelled; the re-read
	// returns the true terminal status, and SetState reports success.
	fs := &fakeStore{
		runOwned:         store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), Status: "cancelled"},
		setCompletedRows: 0,
	}
	svc := New(fs, newBox(t), testParams())
	branch, mr := "agent/issue-1", int64(5)
	run, err := svc.SetState(context.Background(), w, fs.runOwned.ID, StateRequest{
		State: "completed", Branch: &branch, MrIID: &mr,
	})
	if err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if run.Status != "cancelled" {
		t.Fatalf("status = %q, want the run's real (terminal) status 'cancelled'", run.Status)
	}
}

func TestSetStateRejectsUnknownState(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), Status: "running"}}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.SetState(context.Background(), w, fs.runOwned.ID, StateRequest{State: "bogus"}); err != ErrInvalidState {
		t.Fatalf("err = %v, want ErrInvalidState", err)
	}
}

func TestConsumeInputsRequiresOwnership(t *testing.T) {
	fs := &fakeStore{runOwnedErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.ConsumeInputs(context.Background(), worker(), uuid.New()); err != ErrRunNotOwned {
		t.Fatalf("err = %v, want ErrRunNotOwned", err)
	}
}

// -------------------------------------------------------------------------
// Register-time orphan recovery
// -------------------------------------------------------------------------

func TestRegisterRecoversOrphansThenComesOnline(t *testing.T) {
	w := worker()
	fs := &fakeStore{registerResult: store.Worker{ID: w.ID, Status: "online"}}
	svc := New(fs, newBox(t), testParams())

	if _, err := svc.Register(context.Background(), w, "1.2.3"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	want := []string{"fail_over_cap", "requeue_worker", "register"}
	if strings.Join(fs.callOrder, ",") != strings.Join(want, ",") {
		t.Fatalf("call order = %v, want %v", fs.callOrder, want)
	}
	if fs.failOverCap == nil || fs.failOverCap.WorkerID.Bytes != w.ID || fs.failOverCap.MaxRequeues != 1 {
		t.Fatalf("fail-over-cap scoped wrong: %+v", fs.failOverCap)
	}
	if fs.requeueWorker == nil || fs.requeueWorker.WorkerID.Bytes != w.ID {
		t.Fatalf("requeue scoped wrong: %+v", fs.requeueWorker)
	}
	if fs.registerParams == nil || !fs.registerParams.Version.Valid || fs.registerParams.Version.String != "1.2.3" {
		t.Fatalf("register version wrong: %+v", fs.registerParams)
	}
}

// -------------------------------------------------------------------------
// Sweeper
// -------------------------------------------------------------------------

func TestSweepComputesCutoffsAndOrder(t *testing.T) {
	fs := &fakeStore{}
	svc := New(fs, newBox(t), testParams())
	fixed := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return fixed }

	if _, err := svc.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	want := []string{"mark_stale", "claimed_never_started", "running_timeout", "stale_fail_over_cap", "stale_requeue"}
	if strings.Join(fs.callOrder, ",") != strings.Join(want, ",") {
		t.Fatalf("sweep order = %v, want %v", fs.callOrder, want)
	}
	if !fs.staleCutoff.Time.Equal(fixed.Add(-45 * time.Second)) {
		t.Fatalf("stale cutoff = %v, want now-45s", fs.staleCutoff.Time)
	}
	if !fs.claimCutoff.Time.Equal(fixed.Add(-5 * time.Minute)) {
		t.Fatalf("claim cutoff = %v, want now-5m", fs.claimCutoff.Time)
	}
	if !fs.runCutoff.Time.Equal(fixed.Add(-2 * time.Hour)) {
		t.Fatalf("run cutoff = %v, want now-2h", fs.runCutoff.Time)
	}
	if fs.sweepMax != 1 {
		t.Fatalf("max requeues passed = %d, want 1", fs.sweepMax)
	}
}

// -------------------------------------------------------------------------
// Steering inputs: server-side vs enqueue
// -------------------------------------------------------------------------

func TestSubmitInputCancelServerSideWhenQueued(t *testing.T) {
	user := uuid.New()
	runID := uuid.New()
	fs := &fakeStore{runByID: store.Run{ID: runID, UserID: user, Status: "queued"}}
	svc := New(fs, newBox(t), testParams())

	res, err := svc.SubmitInput(context.Background(), user, runID, "cancel", "")
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if !res.ServerSide {
		t.Fatal("a cancel on a queued run (no poller) must be applied server-side")
	}
	if fs.cancelled == nil {
		t.Fatal("CancelRunServerSide not called")
	}
	if fs.createdInput != nil {
		t.Fatal("no input row should be enqueued on the server-side path")
	}
}

func TestSubmitInputEnqueuesWhenWorkerLive(t *testing.T) {
	user := uuid.New()
	runID := uuid.New()
	wkrID := uuid.New()
	fixed := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	fs := &fakeStore{
		runByID:    store.Run{ID: runID, UserID: user, Status: "running", WorkerID: pgUUID(wkrID)},
		workerByID: store.Worker{ID: wkrID, LastHeartbeatAt: pgTime(fixed)}, // fresh
	}
	svc := New(fs, newBox(t), testParams())
	svc.now = func() time.Time { return fixed }

	res, err := svc.SubmitInput(context.Background(), user, runID, "cancel", "")
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if res.ServerSide {
		t.Fatal("a live worker should consume the cancel; not server-side")
	}
	if fs.createdInput == nil || fs.createdInput.Kind != "cancel" {
		t.Fatalf("input not enqueued for the worker: %+v", fs.createdInput)
	}
	if fs.cancelled != nil {
		t.Fatal("server-side cancel must not run when a worker is live")
	}
}

func TestSubmitInputRejectServerSideWhenWorkerStale(t *testing.T) {
	user := uuid.New()
	runID := uuid.New()
	wkrID := uuid.New()
	fixed := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	fs := &fakeStore{
		runByID:    store.Run{ID: runID, UserID: user, Status: "awaiting_approval", WorkerID: pgUUID(wkrID)},
		workerByID: store.Worker{ID: wkrID, LastHeartbeatAt: pgTime(fixed.Add(-2 * time.Minute))}, // stale (> 45s)
	}
	svc := New(fs, newBox(t), testParams())
	svc.now = func() time.Time { return fixed }

	res, err := svc.SubmitInput(context.Background(), user, runID, "reject_plan", "wrong approach")
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if !res.ServerSide {
		t.Fatal("a reject against a stale worker must be applied server-side")
	}
	if fs.rejected == nil {
		t.Fatal("RejectRunServerSide not called")
	}
}

func TestSubmitInputFollowUpAlwaysEnqueues(t *testing.T) {
	user := uuid.New()
	runID := uuid.New()
	fs := &fakeStore{runByID: store.Run{ID: runID, UserID: user, Status: "queued"}}
	svc := New(fs, newBox(t), testParams())

	res, err := svc.SubmitInput(context.Background(), user, runID, "follow_up", "use pgx")
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if res.ServerSide {
		t.Fatal("follow_up is never a server-side transition")
	}
	if fs.createdInput == nil || fs.createdInput.Kind != "follow_up" {
		t.Fatalf("follow_up not enqueued: %+v", fs.createdInput)
	}
}

func TestSubmitInputRejectsTerminalRun(t *testing.T) {
	user := uuid.New()
	runID := uuid.New()
	fs := &fakeStore{runByID: store.Run{ID: runID, UserID: user, Status: "completed"}}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.SubmitInput(context.Background(), user, runID, "cancel", ""); err != ErrRunTerminal {
		t.Fatalf("err = %v, want ErrRunTerminal", err)
	}
}

// -------------------------------------------------------------------------
// Run + worker creation
// -------------------------------------------------------------------------

func TestCreateRunSnapshotsTitleAndRejectsMissingPRDLink(t *testing.T) {
	user, repo := uuid.New(), uuid.New()

	// No PRD link → rejected.
	fsNoLink := &fakeStore{issueByID: store.Issue{Title: "T", HasPrdLink: false}}
	svc := New(fsNoLink, newBox(t), testParams())
	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "desc"); err != ErrNoPRDLink {
		t.Fatalf("err = %v, want ErrNoPRDLink", err)
	}

	// Happy path → title snapshotted from the cached issue, description from arg.
	fs := &fakeStore{
		issueByID:       store.Issue{Title: "Real Title", HasPrdLink: true},
		createRunResult: store.Run{ID: uuid.New()},
	}
	svc = New(fs, newBox(t), testParams())
	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "the description"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if fs.createRunParams == nil {
		t.Fatal("CreateRun not called")
	}
	if fs.createRunParams.IssueTitle != "Real Title" {
		t.Fatalf("title should be snapshotted from the cache, got %q", fs.createRunParams.IssueTitle)
	}
	if fs.createRunParams.IssueDescription != "the description" {
		t.Fatalf("description should come from the request, got %q", fs.createRunParams.IssueDescription)
	}
}

func TestCreateRunMapsDuplicateToActiveRunExists(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	fs := &fakeStore{
		issueByID:    store.Issue{Title: "T", HasPrdLink: true},
		createRunErr: &pgconn.PgError{Code: "23505"},
	}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "d"); err != ErrActiveRunExists {
		t.Fatalf("err = %v, want ErrActiveRunExists", err)
	}
}

func TestCreateRunRepoNotOwned(t *testing.T) {
	fs := &fakeStore{repoErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), 4, "d"); err != ErrRepoNotFound {
		t.Fatalf("err = %v, want ErrRepoNotFound", err)
	}
}

func TestCreateWorkerReturnsTokenOnce(t *testing.T) {
	fs := &fakeStore{createWorkerResult: store.Worker{ID: uuid.New(), Name: "laptop"}}
	svc := New(fs, newBox(t), testParams())
	_, token, err := svc.CreateWorker(context.Background(), uuid.New(), "laptop")
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	if token == "" || !strings.HasPrefix(token, "uzw_") {
		t.Fatalf("expected a uzw_ token, got %q", token)
	}
	// The stored hash must not be the plaintext token.
	if fs.createWorkerParams == nil || bytes.Contains(fs.createWorkerParams.TokenHash, []byte(token)) {
		t.Fatal("stored token_hash must not contain the plaintext token")
	}
}
