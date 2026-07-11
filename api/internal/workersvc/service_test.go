package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// anthropicSealedWith is the row's sealed_with (defaults to 'master' when
	// empty, so existing fixtures are unchanged); set to 'dek' for vault tests.
	anthropicSealedWith string
	// onClaimRun, if set, runs inside ClaimRun — used to simulate the vault locking
	// between the claim gate and the token open (the M3 lock race).
	onClaimRun          func()
	defaultModel        pgtype.Text
	defaultModelErr     error
	templates           []store.AgentTemplate
	skillAllocations    []store.ListRunSkillAllocationsRow
	skillAllocationsErr error
	markedFailed        *store.MarkRunFailedByIDParams
	// requeuedRun records the run id reset to queued by the vault lock-race path
	// (PRD #32 M3); nil unless RequeueClaimedRunToQueued was called.
	requeuedRun *uuid.UUID

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
	// Rows the sweep queries return (PRD #25 M3): each drives a published transition.
	sweptClaimed  []store.SweepClaimedNeverStartedRow
	sweptTimeout  []store.SweepRunningTimeoutRow
	sweptFailed   []store.FailRunsOfStaleWorkersOverCapRow
	sweptRequeued []store.RequeueRunsOfStaleWorkersRow

	// Submit input.
	runByID       store.Run
	runByIDErr    error
	workerByID    store.Worker
	workerByIDErr error
	createdInput       *store.CreateRunInputParams
	createdStopVerdict *store.CreateStopVerdictInputParams
	createdApproval    *store.CreateApprovePlanInputParams
	cancelled          *store.CancelRunServerSideParams
	rejected           *store.RejectRunServerSideParams

	// Create run.
	repoErr         error
	issueByID       store.Issue
	issueByIDErr    error
	boardCols       []store.BoardColumn
	boardColsErr    error
	createRunResult store.Run
	createRunErr    error
	createRunParams *store.CreateRunParams

	// CI-fix (PRD #6). Counts default to 0 (no active run/fix) so existing
	// CreateRun tests are unaffected by the new cross-kind checks.
	ciFixRunResult   store.Run
	ciFixRunErr      error
	ciFixRunParams   *store.CreateCIFixRunParams
	activeBranchRuns int64 // CountActiveRunsWithBranch
	activeCIFixRuns  int64 // CountActiveCIFixForRef

	// Create worker.
	createWorkerResult store.Worker
	createWorkerParams *store.CreateWorkerParams

	// Tool provisioning (PRD #18 M4). Defaults (zero profile, nil error, empty
	// allowlist) resolve to no provisioning, so claim tests that don't opt in are
	// unaffected.
	toolProfile    store.RepoToolProfile
	toolProfileErr error
	toolAllowlist  []store.ToolAllowlist

	// Delete worker.
	countActiveRuns    int64
	countActiveParams  *store.CountWorkerNonTerminalRunsParams
	deleteWorkerRows   int64
	deleteWorkerParams *store.DeleteWorkerForUserParams
}

func (f *fakeStore) ClaimRun(_ context.Context, arg store.ClaimRunParams) (store.Run, error) {
	f.claimParams = &arg
	if f.onClaimRun != nil {
		f.onClaimRun()
	}
	return f.claimRun, f.claimErr
}
func (f *fakeStore) GetRunClaimContext(context.Context, uuid.UUID) (store.GetRunClaimContextRow, error) {
	return f.claimCtx, f.claimCtxErr
}
func (f *fakeStore) GetUserSecretCiphertext(context.Context, store.GetUserSecretCiphertextParams) (store.GetUserSecretCiphertextRow, error) {
	sealedWith := f.anthropicSealedWith
	if sealedWith == "" {
		sealedWith = store.SealedWithMaster // default: fixtures seal with the master box
	}
	return store.GetUserSecretCiphertextRow{Ciphertext: f.anthropic, SealedWith: sealedWith}, f.anthropicErr
}
func (f *fakeStore) GetUserDefaultModel(context.Context, uuid.UUID) (pgtype.Text, error) {
	return f.defaultModel, f.defaultModelErr
}
func (f *fakeStore) ListClaimAgentTemplates(context.Context, pgtype.UUID) ([]store.AgentTemplate, error) {
	return f.templates, nil
}
func (f *fakeStore) ListRunSkillAllocations(context.Context, pgtype.UUID) ([]store.ListRunSkillAllocationsRow, error) {
	return f.skillAllocations, f.skillAllocationsErr
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
func (f *fakeStore) SweepClaimedNeverStarted(_ context.Context, cutoff pgtype.Timestamptz) ([]store.SweepClaimedNeverStartedRow, error) {
	f.claimCutoff = cutoff
	f.callOrder = append(f.callOrder, "claimed_never_started")
	return f.sweptClaimed, nil
}
func (f *fakeStore) RequeueClaimedRunToQueued(_ context.Context, id uuid.UUID) (int64, error) {
	f.requeuedRun = &id
	return 1, nil
}
func (f *fakeStore) SweepRunningTimeout(_ context.Context, arg store.SweepRunningTimeoutParams) ([]store.SweepRunningTimeoutRow, error) {
	f.runCutoff = arg.Cutoff
	f.callOrder = append(f.callOrder, "running_timeout")
	return f.sweptTimeout, nil
}
func (f *fakeStore) FailRunsOfStaleWorkersOverCap(_ context.Context, arg store.FailRunsOfStaleWorkersOverCapParams) ([]store.FailRunsOfStaleWorkersOverCapRow, error) {
	f.sweepMax = arg.MaxRequeues
	f.callOrder = append(f.callOrder, "stale_fail_over_cap")
	return f.sweptFailed, nil
}
func (f *fakeStore) RequeueRunsOfStaleWorkers(_ context.Context, arg store.RequeueRunsOfStaleWorkersParams) ([]store.RequeueRunsOfStaleWorkersRow, error) {
	f.callOrder = append(f.callOrder, "stale_requeue")
	return f.sweptRequeued, nil
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
func (f *fakeStore) CreateStopVerdictInput(_ context.Context, arg store.CreateStopVerdictInputParams) (store.RunUserInput, error) {
	f.createdStopVerdict = &arg
	return store.RunUserInput{}, nil
}
func (f *fakeStore) CreateApprovePlanInput(_ context.Context, arg store.CreateApprovePlanInputParams) (store.RunUserInput, error) {
	f.createdApproval = &arg
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
func (f *fakeStore) ListBoardColumns(context.Context, uuid.UUID) ([]store.BoardColumn, error) {
	return f.boardCols, f.boardColsErr
}
func (f *fakeStore) CreateRun(_ context.Context, arg store.CreateRunParams) (store.Run, error) {
	f.createRunParams = &arg
	return f.createRunResult, f.createRunErr
}
func (f *fakeStore) CreateCIFixRun(_ context.Context, arg store.CreateCIFixRunParams) (store.Run, error) {
	f.ciFixRunParams = &arg
	return f.ciFixRunResult, f.ciFixRunErr
}
func (f *fakeStore) CountActiveRunsWithBranch(context.Context, store.CountActiveRunsWithBranchParams) (int64, error) {
	return f.activeBranchRuns, nil
}
func (f *fakeStore) CountActiveCIFixForRef(context.Context, store.CountActiveCIFixForRefParams) (int64, error) {
	return f.activeCIFixRuns, nil
}
func (f *fakeStore) CreateWorker(_ context.Context, arg store.CreateWorkerParams) (store.Worker, error) {
	f.createWorkerParams = &arg
	return f.createWorkerResult, nil
}
func (f *fakeStore) CountWorkerNonTerminalRuns(_ context.Context, arg store.CountWorkerNonTerminalRunsParams) (int64, error) {
	f.countActiveParams = &arg
	return f.countActiveRuns, nil
}
func (f *fakeStore) DeleteWorkerForUser(_ context.Context, arg store.DeleteWorkerForUserParams) (int64, error) {
	f.deleteWorkerParams = &arg
	return f.deleteWorkerRows, nil
}
func (f *fakeStore) GetRepoToolProfile(_ context.Context, _ store.GetRepoToolProfileParams) (store.RepoToolProfile, error) {
	return f.toolProfile, f.toolProfileErr
}
func (f *fakeStore) ListToolAllowlist(_ context.Context) ([]store.ToolAllowlist, error) {
	return f.toolAllowlist, nil
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
		SkillMaxBytes:        65536,
		SkillsMaxPerRun:      32,
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
			ID: uuid.New(), IssueIid: pgtype.Int8{Int64: 4, Valid: true}, IssueTitle: "Do the thing",
			IssueDescription: "see prds/4.md", Status: "claimed",
			LastSeq: 7, IterationCount: 2, RequeueCount: 1,
			SessionID: pgText("sess-abc"), Branch: pgText(branch),
		},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/grp/proj", RepoPath: "grp/proj",
			DefaultBranch: pgText("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic:    sealedTok,
		defaultModel: pgText("sonnet"),
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

	// Decrypted secrets are delivered in the payload (the sole channel).
	if payload.Secrets.ForgePAT != pat {
		t.Fatal("forge PAT not decrypted into the payload")
	}
	if payload.Secrets.AnthropicOAuthToken != token {
		t.Fatal("anthropic token not decrypted into the payload")
	}
	// Resume fields carried through (flat, per the M2 wire contract).
	if payload.LastSeq != 7 || payload.IterationCount != 2 || payload.RequeueCount != 1 {
		t.Fatalf("resume counters wrong: %+v", payload)
	}
	if payload.SessionID == nil || *payload.SessionID != "sess-abc" {
		t.Fatal("session id not carried for resume")
	}
	if payload.Branch == nil || *payload.Branch != branch {
		t.Fatal("branch not carried for resume")
	}
	// Repo + clone URL.
	if payload.Repo.CloneURL != "https://gitlab.example.com/grp/proj.git" {
		t.Fatalf("clone url = %q", payload.Repo.CloneURL)
	}
	if payload.Repo.URL != "https://gitlab.example.com/grp/proj" {
		t.Fatalf("repo url = %q", payload.Repo.URL)
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
	// The run owner's per-user default model rides the config.
	if payload.Config.DefaultModel == nil || *payload.Config.DefaultModel != "sonnet" {
		t.Fatalf("owner default model not carried into config: %+v", payload.Config)
	}

	// The plaintext secrets must not appear in any log line.
	if strings.Contains(logs.String(), pat) || strings.Contains(logs.String(), token) {
		t.Fatal("a log line leaked a decrypted secret")
	}
}

func TestClaimOmitsDefaultModelWhenOwnerHasNone(t *testing.T) {
	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("bot-pat-OMITTEST-abcdef1234567890"))
	sealedTok, _ := box.Seal([]byte("anthropic-OMITTEST-abcdef1234567890"))

	fs := &fakeStore{
		claimRun: store.Run{ID: uuid.New(), IssueIid: pgtype.Int8{Int64: 9, Valid: true}, Status: "claimed"},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/g/p", RepoPath: "g/p",
			ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic: sealedTok,
		// defaultModel left zero ⇒ NULL ⇒ the owner has no per-user default.
	}

	payload, err := New(fs, box, testParams()).Claim(context.Background(), worker())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload.Config.DefaultModel != nil {
		t.Fatalf("expected nil default model, got %q", *payload.Config.DefaultModel)
	}
	// omitempty: an unset default must not appear on the wire at all, so the
	// worker sees `config.default_model === undefined` and falls back to the lead
	// template's model.
	b, err := json.Marshal(payload.Config)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if strings.Contains(string(b), "default_model") {
		t.Fatalf("unset default_model should be omitted from the payload; got %s", b)
	}
}

func TestClaimFailsOnDefaultModelLookupError(t *testing.T) {
	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("bot-pat-DEFMODELERR-abcdef1234567890"))
	sealedTok, _ := box.Seal([]byte("anthropic-DEFMODELERR-abcdef1234567890"))
	fs := &fakeStore{
		claimRun: store.Run{ID: uuid.New(), IssueIid: pgtype.Int8{Int64: 11, Valid: true}, Status: "claimed"},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/g/p", RepoPath: "g/p",
			ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic:       sealedTok,
		defaultModelErr: errors.New("db down"),
	}

	_, err := New(fs, box, testParams()).Claim(context.Background(), worker())
	if err == nil {
		t.Fatal("expected Claim to fail when the default-model lookup errors")
	}
	// The service wraps it as "default model lookup: %w" and propagates (not a
	// credential failure, so the run is not marked failed).
	if !strings.Contains(err.Error(), "default model lookup") {
		t.Fatalf("error should be wrapped as a default-model lookup failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), "db down") {
		t.Fatalf("error should wrap the underlying cause, got: %v", err)
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

// -------------------------------------------------------------------------
// Tool provisioning resolution (PRD #18 M4)
// -------------------------------------------------------------------------

func TestResolveToolingResolvesAllowedProfilePackages(t *testing.T) {
	fs := &fakeStore{
		toolProfile:   store.RepoToolProfile{Packages: []byte(`["kubectl@1.31","jq","kubectl@1.31"]`)},
		toolAllowlist: []store.ToolAllowlist{{Name: "kubectl"}, {Name: "jq"}},
	}
	svc := New(fs, newBox(t), testParams())
	pkgs, err := svc.resolveTooling(context.Background(), store.Run{UserID: uuid.New(), RepoID: uuid.New()})
	if err != nil {
		t.Fatalf("resolveTooling: %v", err)
	}
	// Deduped + sorted.
	if strings.Join(pkgs, ",") != "jq,kubectl@1.31" {
		t.Fatalf("pkgs = %v, want [jq kubectl@1.31]", pkgs)
	}
}

func TestResolveToolingRejectsPackageOutsideShrunkAllowlist(t *testing.T) {
	fs := &fakeStore{
		toolProfile:   store.RepoToolProfile{Packages: []byte(`["kubectl@1.31","terraform"]`)},
		toolAllowlist: []store.ToolAllowlist{{Name: "kubectl"}}, // terraform removed after the profile was saved
	}
	svc := New(fs, newBox(t), testParams())
	_, err := svc.resolveTooling(context.Background(), store.Run{UserID: uuid.New(), RepoID: uuid.New()})
	if !errors.Is(err, errToolPackagesRejected) {
		t.Fatalf("err = %v, want errToolPackagesRejected", err)
	}
	if !strings.Contains(err.Error(), "terraform") {
		t.Fatalf("error should name the rejected package: %v", err)
	}
}

func TestResolveToolingNoProfileMeansNoProvisioning(t *testing.T) {
	fs := &fakeStore{toolProfileErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	pkgs, err := svc.resolveTooling(context.Background(), store.Run{UserID: uuid.New(), RepoID: uuid.New()})
	if err != nil {
		t.Fatalf("resolveTooling: %v", err)
	}
	if len(pkgs) != 0 {
		t.Fatalf("no profile ⇒ no packages, got %v", pkgs)
	}
}

func TestClaimFailsRunWhenToolPackagesRejected(t *testing.T) {
	// A grandfathered package that fell out of the allowlist fails the CLAIM (the
	// run is failed, no payload handed out) with a message naming the package.
	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("bot-pat-something-long-enough"))
	sealedTok, _ := box.Seal([]byte("anthropic-oauth-something-long-enough"))
	fs := &fakeStore{
		claimRun:      store.Run{ID: uuid.New(), Status: "claimed"},
		claimCtx:      store.GetRunClaimContextRow{TokenCiphertext: sealedPAT, RepoWebUrl: "https://x/y", BotUsername: "uzi-bot"},
		anthropic:     sealedTok,
		toolProfile:   store.RepoToolProfile{Packages: []byte(`["terraform"]`)},
		toolAllowlist: []store.ToolAllowlist{}, // shrank to nothing
	}
	svc := New(fs, box, testParams())
	payload, err := svc.Claim(context.Background(), worker())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload != nil {
		t.Fatal("a rejected tool package must not hand out a payload")
	}
	if fs.markedFailed == nil {
		t.Fatal("the run should have been marked failed")
	}
	if !strings.Contains(fs.markedFailed.FailureReason.String, "terraform") {
		t.Fatalf("failure reason should name the rejected package: %+v", fs.markedFailed.FailureReason)
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

func TestAppendMessagesAllOrNothingOnInvalid(t *testing.T) {
	// A [valid, valid, invalid] batch must persist nothing: the whole batch is
	// validated before any insert, so the first two are never half-written.
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), LastSeq: 0}}
	svc := New(fs, newBox(t), testParams())

	msgs := []IncomingMessage{
		{Seq: 1, Kind: "text", Payload: json.RawMessage(`{"t":"hi"}`)},
		{Seq: 2, Kind: "tool_use", Payload: json.RawMessage(`{"tool":"Edit"}`)},
		{Seq: 3, Kind: "", Payload: json.RawMessage(`{}`)}, // invalid: empty kind
	}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err != ErrInvalidMessage {
		t.Fatalf("err = %v, want ErrInvalidMessage", err)
	}
	if len(fs.insertedMessages) != 0 {
		t.Fatalf("a batch with any invalid message must persist nothing, got %d inserts", len(fs.insertedMessages))
	}
	if fs.lastSeqUpdated != nil {
		t.Fatal("last_seq must not advance when the batch is rejected")
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
	run, applied, err := svc.SetState(context.Background(), w, fs.runOwned.ID, StateRequest{
		State: "completed", Branch: &branch, MrIID: &mr,
	})
	if err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if applied {
		t.Fatal("a report onto an already-terminal run must not be applied (handler answers 409)")
	}
	if run.Status != "cancelled" {
		t.Fatalf("status = %q, want the run's real (terminal) status 'cancelled'", run.Status)
	}
}

func TestSetStateAppliedOnLiveRun(t *testing.T) {
	w := worker()
	fs := &fakeStore{
		runOwned:       store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), Status: "running"},
		setRunningRows: 1,
	}
	svc := New(fs, newBox(t), testParams())
	_, applied, err := svc.SetState(context.Background(), w, fs.runOwned.ID, StateRequest{State: "running", IterationCount: 2})
	if err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if !applied {
		t.Fatal("a transition on a non-terminal run must be applied (handler answers 200)")
	}
}

func TestSetStateRejectsUnknownState(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), Status: "running"}}
	svc := New(fs, newBox(t), testParams())
	if _, _, err := svc.SetState(context.Background(), w, fs.runOwned.ID, StateRequest{State: "bogus"}); err != ErrInvalidState {
		t.Fatalf("err = %v, want ErrInvalidState", err)
	}
}

func TestSetStateRejectsForeignRun(t *testing.T) {
	// A valid worker token but the run belongs to another worker/tenant: the
	// ownership lookup misses, so the transition is refused (handler answers 404)
	// and no state write occurs.
	fs := &fakeStore{runOwnedErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	if _, _, err := svc.SetState(context.Background(), worker(), uuid.New(), StateRequest{State: "completed"}); err != ErrRunNotOwned {
		t.Fatalf("err = %v, want ErrRunNotOwned", err)
	}
	if fs.setCompleted != nil {
		t.Fatal("no state write should occur for a run the worker does not own")
	}
}

func TestSetStatePersistsSessionIDWhenSet(t *testing.T) {
	// The worker pins its SDK session by reporting session_id alongside a state
	// transition; the service persists it in the same transition (resume plumbing).
	w := worker()
	fs := &fakeStore{
		runOwned:       store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), Status: "running"},
		setRunningRows: 1,
	}
	svc := New(fs, newBox(t), testParams())
	sess := "sess-xyz-9"
	if _, _, err := svc.SetState(context.Background(), w, fs.runOwned.ID, StateRequest{
		State: "running", IterationCount: 1, SessionID: &sess,
	}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if fs.setRunningParams == nil {
		t.Fatal("SetRunRunning not called")
	}
	if !fs.setRunningParams.SessionID.Valid || fs.setRunningParams.SessionID.String != sess {
		t.Fatalf("session_id not persisted with the transition: %+v", fs.setRunningParams.SessionID)
	}
}

func TestSetStateLeavesSessionIDUnsetWhenAbsent(t *testing.T) {
	// No session_id on the report → the param is NULL, so the COALESCE keeps the
	// run's existing session_id (never clobbered to empty).
	w := worker()
	fs := &fakeStore{
		runOwned:       store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), Status: "running"},
		setRunningRows: 1,
	}
	svc := New(fs, newBox(t), testParams())
	if _, _, err := svc.SetState(context.Background(), w, fs.runOwned.ID, StateRequest{
		State: "running", IterationCount: 1,
	}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if fs.setRunningParams == nil {
		t.Fatal("SetRunRunning not called")
	}
	if fs.setRunningParams.SessionID.Valid {
		t.Fatalf("session_id must be NULL (no change) when the report omits it, got %+v", fs.setRunningParams.SessionID)
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

	if _, err := svc.Register(context.Background(), w, "1.2.3", "jvm"); err != nil {
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
	// The self-reported template rides into template_reported (PRD #18).
	if !fs.registerParams.TemplateReported.Valid || fs.registerParams.TemplateReported.String != "jvm" {
		t.Fatalf("register template_reported wrong: %+v", fs.registerParams.TemplateReported)
	}
}

func TestRegisterEmptyTemplateStoresNull(t *testing.T) {
	// An older image reports no template ⇒ template_reported stays NULL, never an
	// empty string.
	w := worker()
	fs := &fakeStore{registerResult: store.Worker{ID: w.ID, Status: "online"}}
	svc := New(fs, newBox(t), testParams())

	if _, err := svc.Register(context.Background(), w, "1.2.3", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if fs.registerParams == nil || fs.registerParams.TemplateReported.Valid {
		t.Fatalf("empty template must be NULL, got %+v", fs.registerParams.TemplateReported)
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

// TestSweepPublishesTransitions is the PRD #25 M3 fix: sweeper-driven transitions
// (timeouts, worker-loss failures/requeues) must reach the Broadcaster/notifier
// fan-out — before this they returned counts only and were silently missed.
func TestSweepPublishesTransitions(t *testing.T) {
	r1, r2, r3 := uuid.New(), uuid.New(), uuid.New()
	owner := uuid.New()
	fs := &fakeStore{
		sweptTimeout:  []store.SweepRunningTimeoutRow{{ID: r1, UserID: owner, Status: "failed"}},
		sweptFailed:   []store.FailRunsOfStaleWorkersOverCapRow{{ID: r2, UserID: owner, Status: "failed"}},
		sweptRequeued: []store.RequeueRunsOfStaleWorkersRow{{ID: r3, UserID: owner, Status: "queued"}},
	}
	svc := New(fs, newBox(t), testParams())
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)

	res, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.RunningTimeout != 1 || res.StaleFailed != 1 || res.StaleRequeued != 1 {
		t.Fatalf("counts = %d/%d/%d, want 1/1/1", res.RunningTimeout, res.StaleFailed, res.StaleRequeued)
	}
	// Each swept row published its new status through the broadcaster.
	got := strings.Join(b.statuses, ",")
	if got != "failed,failed,queued" {
		t.Fatalf("published statuses = %q, want failed,failed,queued", got)
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

	res, err := svc.SubmitInput(context.Background(), user, runID, "cancel", "", nil)
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

	res, err := svc.SubmitInput(context.Background(), user, runID, "cancel", "", nil)
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if res.ServerSide {
		t.Fatal("a live worker should consume the cancel; not server-side")
	}
	// A live stop verdict goes through the dedicated CreateStopVerdictInput CTE, which
	// enqueues the input AND stamps stop_kind transactionally (PRD #33 Decision 3) —
	// not the plain CreateRunInput path.
	if fs.createdStopVerdict == nil || fs.createdStopVerdict.Kind != "cancel" {
		t.Fatalf("stop verdict not enqueued for the worker: %+v", fs.createdStopVerdict)
	}
	if fs.createdStopVerdict.StopKind.String != "cancelled" || !fs.createdStopVerdict.StopKind.Valid {
		t.Fatalf("live cancel must stamp stop_kind 'cancelled', got %+v", fs.createdStopVerdict.StopKind)
	}
	if fs.createdInput != nil {
		t.Fatal("a stop verdict must not use the plain CreateRunInput path")
	}
	if fs.cancelled != nil {
		t.Fatal("server-side cancel must not run when a worker is live")
	}
}

func TestSubmitInputLiveRejectStampsStopKind(t *testing.T) {
	user := uuid.New()
	runID := uuid.New()
	wkrID := uuid.New()
	fixed := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	fs := &fakeStore{
		runByID:    store.Run{ID: runID, UserID: user, Status: "awaiting_approval", WorkerID: pgUUID(wkrID)},
		workerByID: store.Worker{ID: wkrID, LastHeartbeatAt: pgTime(fixed)}, // fresh
	}
	svc := New(fs, newBox(t), testParams())
	svc.now = func() time.Time { return fixed }

	// A live reject carrying a VERBATIM reason string still stamps the structured
	// signal — the exact case the failed-vs-stopped heuristic could not recognise.
	res, err := svc.SubmitInput(context.Background(), user, runID, "reject_plan", "this is the wrong approach entirely", nil)
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if res.ServerSide {
		t.Fatal("a live worker should consume the reject; not server-side")
	}
	if fs.createdStopVerdict == nil || fs.createdStopVerdict.Kind != "reject_plan" {
		t.Fatalf("reject verdict not enqueued for the worker: %+v", fs.createdStopVerdict)
	}
	if fs.createdStopVerdict.StopKind.String != "plan_rejected" || !fs.createdStopVerdict.StopKind.Valid {
		t.Fatalf("live reject must stamp stop_kind 'plan_rejected', got %+v", fs.createdStopVerdict.StopKind)
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

	res, err := svc.SubmitInput(context.Background(), user, runID, "reject_plan", "wrong approach", nil)
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

	res, err := svc.SubmitInput(context.Background(), user, runID, "follow_up", "use pgx", nil)
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if res.ServerSide {
		t.Fatal("follow_up is never a server-side transition")
	}
	if fs.createdInput == nil || fs.createdInput.Kind != "follow_up" {
		t.Fatalf("follow_up not enqueued: %+v", fs.createdInput)
	}
	// A non-verdict input uses the plain path and never stamps a stop signal.
	if fs.createdStopVerdict != nil {
		t.Fatalf("follow_up must not use the stop-verdict path, got %+v", fs.createdStopVerdict)
	}
}

func TestSubmitInputRejectsTerminalRun(t *testing.T) {
	user := uuid.New()
	runID := uuid.New()
	fs := &fakeStore{runByID: store.Run{ID: runID, UserID: user, Status: "completed"}}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.SubmitInput(context.Background(), user, runID, "cancel", "", nil); err != ErrRunTerminal {
		t.Fatalf("err = %v, want ErrRunTerminal", err)
	}
}

// -------------------------------------------------------------------------
// Run + worker creation
// -------------------------------------------------------------------------

func TestCreateRunSnapshotsTitleAndRejectsMissingPRDLink(t *testing.T) {
	user, repo := uuid.New(), uuid.New()

	// No PRD link, no bypass → rejected.
	fsNoLink := &fakeStore{issueByID: store.Issue{Title: "T", HasPrdLink: false}}
	svc := New(fsNoLink, newBox(t), testParams())
	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "desc", false); err != ErrNoPRDLink {
		t.Fatalf("err = %v, want ErrNoPRDLink", err)
	}

	// Happy path → title snapshotted from the cached issue, description from arg.
	fs := &fakeStore{
		issueByID:       store.Issue{Title: "Real Title", HasPrdLink: true},
		createRunResult: store.Run{ID: uuid.New()},
	}
	svc = New(fs, newBox(t), testParams())
	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "the description", false); err != nil {
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

func TestCreateAutopilotRunSetsAutoApproveAndSharesGates(t *testing.T) {
	user, repo := uuid.New(), uuid.New()

	// Same PRD-link gate as the manual path (no bypass).
	fsNoLink := &fakeStore{issueByID: store.Issue{Title: "T", HasPrdLink: false}}
	svc := New(fsNoLink, newBox(t), testParams())
	if _, err := svc.CreateAutopilotRun(context.Background(), user, repo, 4, "desc", false); err != ErrNoPRDLink {
		t.Fatalf("err = %v, want ErrNoPRDLink (autopilot must enforce the same gate)", err)
	}

	// Happy path → auto_approve set, description snapshotted.
	fs := &fakeStore{
		issueByID:       store.Issue{Title: "Real Title", HasPrdLink: true},
		createRunResult: store.Run{ID: uuid.New()},
	}
	svc = New(fs, newBox(t), testParams())
	if _, err := svc.CreateAutopilotRun(context.Background(), user, repo, 4, "the description", false); err != nil {
		t.Fatalf("CreateAutopilotRun: %v", err)
	}
	if fs.createRunParams == nil {
		t.Fatal("CreateRun not called")
	}
	if !fs.createRunParams.AutoApprove {
		t.Fatal("autopilot run must set auto_approve = true")
	}
	if fs.createRunParams.IssueDescription != "the description" {
		t.Fatalf("description should be the snapshot passed in, got %q", fs.createRunParams.IssueDescription)
	}

	// A manual run leaves auto_approve false.
	fsManual := &fakeStore{
		issueByID:       store.Issue{Title: "T", HasPrdLink: true},
		createRunResult: store.Run{ID: uuid.New()},
	}
	svc = New(fsManual, newBox(t), testParams())
	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "d", false); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if fsManual.createRunParams.AutoApprove {
		t.Fatal("manual run must keep auto_approve = false")
	}
}

// TestCreateRunPRDLESSGateMatrix pins the shared createRun gate (PRD #22
// Decision 3): the single enforcement point is `!HasPrdLink && !allowWithoutPRD`,
// identical for the manual and autopilot paths. The callers compute
// allowWithoutPRD (handler from the fresh forge snapshot, poller from its fresh
// GetIssue); here we drive the resulting bool directly across the 2×2 of
// (HasPrdLink × allowWithoutPRD) for both entry points.
func TestCreateRunPRDLESSGateMatrix(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	cases := []struct {
		name            string
		hasPRDLink      bool
		allowWithoutPRD bool
		wantCreated     bool // false → ErrNoPRDLink
	}{
		{"link, no bypass", true, false, true},
		{"link, bypass", true, true, true},
		{"no link, no bypass", false, false, false},
		{"no link, bypass", false, true, true}, // the PRDLESS escape hatch
	}
	for _, tc := range cases {
		for _, path := range []string{"manual", "autopilot"} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				fs := &fakeStore{
					issueByID:       store.Issue{Title: "T", HasPrdLink: tc.hasPRDLink},
					createRunResult: store.Run{ID: uuid.New()},
				}
				svc := New(fs, newBox(t), testParams())

				var err error
				if path == "manual" {
					_, err = svc.CreateRun(context.Background(), user, repo, 4, "desc", tc.allowWithoutPRD)
				} else {
					_, err = svc.CreateAutopilotRun(context.Background(), user, repo, 4, "desc", tc.allowWithoutPRD)
				}

				if tc.wantCreated {
					if err != nil {
						t.Fatalf("want run created, got err %v", err)
					}
					if fs.createRunParams == nil {
						t.Fatal("run not created")
					}
					if path == "autopilot" && !fs.createRunParams.AutoApprove {
						t.Fatal("autopilot run must set auto_approve")
					}
				} else {
					if err != ErrNoPRDLink {
						t.Fatalf("want ErrNoPRDLink, got %v", err)
					}
					if fs.createRunParams != nil {
						t.Fatal("gated run must not reach CreateRun")
					}
				}
			})
		}
	}
}

func TestClaimDeliversAutoApproveTopLevelFreshAndResume(t *testing.T) {
	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("bot-pat-AUTOAPPROVETEST-abcdef1234567890"))
	sealedTok, _ := box.Seal([]byte("anthropic-oauth-AUTOAPPROVETEST-abcdef1234567890"))

	newFS := func(run store.Run) *fakeStore {
		return &fakeStore{
			claimRun: run,
			claimCtx: store.GetRunClaimContextRow{
				RepoWebUrl: "https://gitlab.example.com/grp/proj", RepoPath: "grp/proj",
				DefaultBranch: pgText("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
				BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
			},
			anthropic: sealedTok,
		}
	}

	// Fresh autopilot run: no branch/session yet, auto_approve set on the row.
	fresh := newFS(store.Run{ID: uuid.New(), IssueIid: pgtype.Int8{Int64: 4, Valid: true}, Status: "claimed", AutoApprove: true})
	p, err := New(fresh, box, testParams()).Claim(context.Background(), worker())
	if err != nil || p == nil {
		t.Fatalf("fresh Claim: payload=%v err=%v", p, err)
	}
	if !p.AutoApprove {
		t.Fatal("a fresh autopilot claim must deliver auto_approve top-level")
	}

	// Resume/requeue of the SAME run (branch + session persisted): auto_approve is
	// read from the row again, so it re-delivers — an unattended resume would
	// otherwise hang forever at the plan gate.
	resume := newFS(store.Run{
		ID: uuid.New(), IssueIid: pgtype.Int8{Int64: 4, Valid: true}, Status: "claimed", AutoApprove: true,
		Branch: pgText("agent/issue-4"), SessionID: pgText("sess-xyz"), RequeueCount: 1,
	})
	p, err = New(resume, box, testParams()).Claim(context.Background(), worker())
	if err != nil || p == nil {
		t.Fatalf("resume Claim: payload=%v err=%v", p, err)
	}
	if p.Branch == nil || !p.AutoApprove {
		t.Fatalf("a resumed autopilot claim must re-deliver auto_approve, got %+v", p)
	}

	// A manual run never carries it.
	manual := newFS(store.Run{ID: uuid.New(), IssueIid: pgtype.Int8{Int64: 4, Valid: true}, Status: "claimed"})
	p, err = New(manual, box, testParams()).Claim(context.Background(), worker())
	if err != nil || p == nil {
		t.Fatalf("manual Claim: payload=%v err=%v", p, err)
	}
	if p.AutoApprove {
		t.Fatal("a manual claim must not set auto_approve")
	}
}

func TestCreateRunRejectsOversizeDescription(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	big := strings.Repeat("x", MaxIssueDescriptionBytes+1)

	// Manual and autopilot both reject at the one shared cap, before any run is made.
	fs := &fakeStore{issueByID: store.Issue{Title: "T", HasPrdLink: true}}
	if _, err := New(fs, newBox(t), testParams()).CreateRun(context.Background(), user, repo, 4, big, false); err != ErrDescriptionTooLarge {
		t.Fatalf("CreateRun err = %v, want ErrDescriptionTooLarge", err)
	}
	if fs.createRunParams != nil {
		t.Fatal("an oversize description must be rejected before CreateRun")
	}

	fsAuto := &fakeStore{issueByID: store.Issue{Title: "T", HasPrdLink: true}}
	if _, err := New(fsAuto, newBox(t), testParams()).CreateAutopilotRun(context.Background(), user, repo, 4, big, false); err != ErrDescriptionTooLarge {
		t.Fatalf("CreateAutopilotRun err = %v, want ErrDescriptionTooLarge", err)
	}

	// Exactly at the cap is accepted (boundary).
	ok := strings.Repeat("x", MaxIssueDescriptionBytes)
	fsOK := &fakeStore{issueByID: store.Issue{Title: "T", HasPrdLink: true}, createRunResult: store.Run{ID: uuid.New()}}
	if _, err := New(fsOK, newBox(t), testParams()).CreateRun(context.Background(), user, repo, 4, ok, false); err != nil {
		t.Fatalf("a description exactly at the cap must be accepted, got %v", err)
	}
}

func TestCreateRunMapsDuplicateToActiveRunExists(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	fs := &fakeStore{
		issueByID:    store.Issue{Title: "T", HasPrdLink: true},
		createRunErr: &pgconn.PgError{Code: "23505"},
	}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "d", false); err != ErrActiveRunExists {
		t.Fatalf("err = %v, want ErrActiveRunExists", err)
	}
}

func TestCreateRunRepoNotOwned(t *testing.T) {
	fs := &fakeStore{repoErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), 4, "d", false); err != ErrRepoNotFound {
		t.Fatalf("err = %v, want ErrRepoNotFound", err)
	}
}

func TestDeleteWorkerRejectedWhileHoldingActiveRuns(t *testing.T) {
	// A worker with a non-terminal run may not be deleted: the FK is ON DELETE SET
	// NULL, so deleting would strand the run (an awaiting_approval run matches no
	// sweep once its worker_id is gone).
	fs := &fakeStore{countActiveRuns: 1}
	svc := New(fs, newBox(t), testParams())
	user, wkrID := uuid.New(), uuid.New()
	if err := svc.DeleteWorker(context.Background(), user, wkrID); err != ErrWorkerHasActiveRuns {
		t.Fatalf("err = %v, want ErrWorkerHasActiveRuns", err)
	}
	if fs.deleteWorkerParams != nil {
		t.Fatal("no delete should be issued while the worker owns active runs")
	}
	// The guard is user-scoped so a cross-tenant attempt still 404s (never 409).
	if fs.countActiveParams == nil || fs.countActiveParams.UserID != user {
		t.Fatalf("active-run count must be scoped to the requesting user: %+v", fs.countActiveParams)
	}
}

func TestDeleteWorkerSucceedsWhenIdle(t *testing.T) {
	fs := &fakeStore{countActiveRuns: 0, deleteWorkerRows: 1}
	svc := New(fs, newBox(t), testParams())
	if err := svc.DeleteWorker(context.Background(), uuid.New(), uuid.New()); err != nil {
		t.Fatalf("DeleteWorker: %v", err)
	}
	if fs.deleteWorkerParams == nil {
		t.Fatal("an idle worker should be deleted")
	}
}

func TestDeleteWorkerNotFoundWhenNoRowDeleted(t *testing.T) {
	// No active runs and no row deleted (unknown or other-tenant id) → 404.
	fs := &fakeStore{countActiveRuns: 0, deleteWorkerRows: 0}
	svc := New(fs, newBox(t), testParams())
	if err := svc.DeleteWorker(context.Background(), uuid.New(), uuid.New()); err != ErrWorkerNotFound {
		t.Fatalf("err = %v, want ErrWorkerNotFound", err)
	}
}

func TestCreateWorkerReturnsTokenOnce(t *testing.T) {
	fs := &fakeStore{createWorkerResult: store.Worker{ID: uuid.New(), Name: "laptop"}}
	svc := New(fs, newBox(t), testParams())
	_, token, err := svc.CreateWorker(context.Background(), uuid.New(), "laptop", "jvm")
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
	// The declared template is persisted (PRD #18).
	if !fs.createWorkerParams.TemplateDeclared.Valid || fs.createWorkerParams.TemplateDeclared.String != "jvm" {
		t.Fatalf("declared template wrong: %+v", fs.createWorkerParams.TemplateDeclared)
	}
}

func TestCreateWorkerEmptyTemplateStoresNull(t *testing.T) {
	fs := &fakeStore{createWorkerResult: store.Worker{ID: uuid.New(), Name: "laptop"}}
	svc := New(fs, newBox(t), testParams())
	if _, _, err := svc.CreateWorker(context.Background(), uuid.New(), "laptop", ""); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	if fs.createWorkerParams == nil || fs.createWorkerParams.TemplateDeclared.Valid {
		t.Fatalf("no declared choice must be NULL, got %+v", fs.createWorkerParams.TemplateDeclared)
	}
}

// fakeBroadcaster records the run events the service fans out, to assert the
// persist-then-broadcast contract (dedup on retry, broadcast only on apply).
type fakeBroadcaster struct {
	msgSeqs  []int32
	statuses []string
}

func (b *fakeBroadcaster) PublishMessage(_ uuid.UUID, seq int32, _, _ string, _ []byte, _ time.Time) {
	b.msgSeqs = append(b.msgSeqs, seq)
}
func (b *fakeBroadcaster) PublishState(_ uuid.UUID, status string) {
	b.statuses = append(b.statuses, status)
}

func TestAppendMessagesBroadcastsOnlyNewlyInserted(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), LastSeq: 0}}
	svc := New(fs, newBox(t), testParams())
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)

	first := []IncomingMessage{
		{Seq: 1, Kind: "text", Payload: json.RawMessage(`{}`)},
		{Seq: 2, Kind: "text", Payload: json.RawMessage(`{}`)},
	}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, first); err != nil {
		t.Fatalf("AppendMessages first: %v", err)
	}
	// A worker retry re-delivers 1 and 2 (idempotent no-ops) plus a new 3.
	second := []IncomingMessage{
		{Seq: 1, Kind: "text", Payload: json.RawMessage(`{}`)},
		{Seq: 2, Kind: "text", Payload: json.RawMessage(`{}`)},
		{Seq: 3, Kind: "text", Payload: json.RawMessage(`{}`)},
	}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, second); err != nil {
		t.Fatalf("AppendMessages second: %v", err)
	}
	// The re-delivered 1 and 2 must NOT be broadcast again: WS never double-emits.
	want := []int32{1, 2, 3}
	if len(b.msgSeqs) != len(want) {
		t.Fatalf("broadcast seqs = %v, want %v (dup re-delivery must not re-broadcast)", b.msgSeqs, want)
	}
	for i, s := range want {
		if b.msgSeqs[i] != s {
			t.Fatalf("broadcast seqs = %v, want %v", b.msgSeqs, want)
		}
	}
}

func TestSetStateBroadcastsAppliedStatus(t *testing.T) {
	w := worker()
	fs := &fakeStore{
		runOwned:       store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), Status: "running"},
		setRunningRows: 1,
	}
	svc := New(fs, newBox(t), testParams())
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)

	_, applied, err := svc.SetState(context.Background(), w, fs.runOwned.ID, StateRequest{State: "running"})
	if err != nil || !applied {
		t.Fatalf("SetState: applied=%v err=%v", applied, err)
	}
	if len(b.statuses) != 1 || b.statuses[0] != "running" {
		t.Fatalf("state broadcast = %v, want [running]", b.statuses)
	}
}

// -------------------------------------------------------------------------
// Run-lifecycle column-automation hook wiring
// -------------------------------------------------------------------------

// fakeLifecycle records the (runID, status) notifications the service fires, so
// the tests can assert the column automation is driven at exactly the applied
// status-write sites (and never on a no-op transition).
type fakeLifecycle struct {
	notes []lifecycleNote
}

type lifecycleNote struct {
	runID  uuid.UUID
	status string
}

func (l *fakeLifecycle) Notify(runID uuid.UUID, status string) {
	l.notes = append(l.notes, lifecycleNote{runID, status})
}

func TestSetStateNotifiesAppliedStatus(t *testing.T) {
	w := worker()
	fs := &fakeStore{
		runOwned:         store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), Status: "completed"},
		setCompletedRows: 1,
	}
	svc := New(fs, newBox(t), testParams())
	lc := &fakeLifecycle{}
	svc.SetLifecycle(lc)

	branch, mr := "agent/issue-1", int64(9)
	if _, applied, err := svc.SetState(context.Background(), w, fs.runOwned.ID, StateRequest{
		State: "completed", Branch: &branch, MrIID: &mr,
	}); err != nil || !applied {
		t.Fatalf("SetState: applied=%v err=%v", applied, err)
	}
	if len(lc.notes) != 1 || lc.notes[0].status != "completed" || lc.notes[0].runID != fs.runOwned.ID {
		t.Fatalf("expected one 'completed' notification for the run, got %+v", lc.notes)
	}
}

func TestSetStateTerminalRaceDoesNotNotify(t *testing.T) {
	// A completed report onto an already-terminal (cancelled) run applies 0 rows;
	// the automation must NOT be driven — otherwise a lost race would re-move a
	// card whose real terminal state is elsewhere.
	w := worker()
	fs := &fakeStore{
		runOwned:         store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), Status: "cancelled"},
		setCompletedRows: 0,
	}
	svc := New(fs, newBox(t), testParams())
	lc := &fakeLifecycle{}
	svc.SetLifecycle(lc)

	branch, mr := "agent/issue-1", int64(9)
	if _, applied, err := svc.SetState(context.Background(), w, fs.runOwned.ID, StateRequest{
		State: "completed", Branch: &branch, MrIID: &mr,
	}); err != nil || applied {
		t.Fatalf("SetState: applied=%v err=%v (want not-applied)", applied, err)
	}
	if len(lc.notes) != 0 {
		t.Fatalf("a no-op transition must not notify the automation, got %+v", lc.notes)
	}
}

func TestCreateRunNotifiesQueuedWithOriginSnapshot(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	labels, _ := json.Marshal([]string{"PRD", "Later"})
	runID := uuid.New()
	fs := &fakeStore{
		issueByID:       store.Issue{Title: "T", State: "opened", Labels: labels, HasPrdLink: true},
		boardCols:       []store.BoardColumn{{LabelName: "In Progress", Position: 0}, {LabelName: "Upcoming", Position: 1}, {LabelName: "Later", Position: 2}},
		createRunResult: store.Run{ID: runID},
	}
	svc := New(fs, newBox(t), testParams())
	lc := &fakeLifecycle{}
	svc.SetLifecycle(lc)

	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "desc", false); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// origin_column snapshots the issue's current column ("Later"), always a valid
	// text value (never NULL for a run created from the cache).
	if fs.createRunParams == nil || !fs.createRunParams.OriginColumn.Valid || fs.createRunParams.OriginColumn.String != "Later" {
		t.Fatalf("origin_column should snapshot 'Later', got %+v", fs.createRunParams.OriginColumn)
	}
	if len(lc.notes) != 1 || lc.notes[0].status != "queued" || lc.notes[0].runID != runID {
		t.Fatalf("expected one 'queued' notification, got %+v", lc.notes)
	}
}

func TestCreateRunOriginNullWhenColumnsUnavailable(t *testing.T) {
	// A DB error listing the board columns must NOT degrade the origin snapshot to
	// "" (Open): that is a confident guess a later failed/cancelled restore would
	// act on, stripping the card to Open. Unknown must stay NULL so the restore
	// skips instead.
	user, repo := uuid.New(), uuid.New()
	labels, _ := json.Marshal([]string{"PRD", "Later"})
	fs := &fakeStore{
		issueByID:       store.Issue{Title: "T", State: "opened", Labels: labels, HasPrdLink: true},
		boardColsErr:    errors.New("db unavailable"),
		createRunResult: store.Run{ID: uuid.New()},
	}
	svc := New(fs, newBox(t), testParams())

	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "desc", false); err != nil {
		t.Fatalf("CreateRun should not be blocked by a column-list error: %v", err)
	}
	if fs.createRunParams == nil {
		t.Fatal("CreateRun not called")
	}
	if fs.createRunParams.OriginColumn.Valid {
		t.Fatalf("origin_column must be NULL (unknown) when columns can't be listed, got %+v", fs.createRunParams.OriginColumn)
	}
}

func TestSubmitInputServerSideCancelNotifiesCancelled(t *testing.T) {
	user, runID := uuid.New(), uuid.New()
	fs := &fakeStore{runByID: store.Run{ID: runID, UserID: user, Status: "queued"}}
	svc := New(fs, newBox(t), testParams())
	lc := &fakeLifecycle{}
	svc.SetLifecycle(lc)

	if _, err := svc.SubmitInput(context.Background(), user, runID, "cancel", "", nil); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if len(lc.notes) != 1 || lc.notes[0].status != "cancelled" || lc.notes[0].runID != runID {
		t.Fatalf("server-side cancel must notify 'cancelled', got %+v", lc.notes)
	}
}

func TestClaimCredentialFailureNotifiesFailed(t *testing.T) {
	// A claim whose Anthropic token is missing fails the run server-side; the
	// automation restores the origin column for the dead run.
	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("bot-pat-something-long-enough"))
	runID := uuid.New()
	fs := &fakeStore{
		claimRun:     store.Run{ID: runID, Status: "claimed"},
		claimCtx:     store.GetRunClaimContextRow{TokenCiphertext: sealedPAT, RepoWebUrl: "https://x/y"},
		anthropicErr: pgx.ErrNoRows,
	}
	svc := New(fs, box, testParams())
	lc := &fakeLifecycle{}
	svc.SetLifecycle(lc)

	if _, err := svc.Claim(context.Background(), worker()); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(lc.notes) != 1 || lc.notes[0].status != "failed" || lc.notes[0].runID != runID {
		t.Fatalf("a credential-failed claim must notify 'failed', got %+v", lc.notes)
	}
}
