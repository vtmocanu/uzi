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

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// prdLabels is the cached labels jsonb of an ordinary board issue: one carrying the
// configured PRD label, plus any extras a case needs.
//
// Before PRD #102 M6 these fixtures could leave labels unset, because the sync
// filter guaranteed every cached issue carried the PRD label and nothing read it
// on the run path. Decision 14's gate makes it load-bearing: a fixture WITHOUT the
// label now means "an issue that is not uzi's" and is rejected before any other
// gate is reached, which is what the six tests updated alongside this helper were
// silently asserting the opposite of.
func prdLabels(extra ...string) []byte {
	b, _ := json.Marshal(append([]string{settings.DefaultPRDLabel}, extra...))
	return b
}

// fakeStore embeds the Store interface so unimplemented methods panic if a test
// path reaches them unexpectedly; the tests override only what they exercise.
type fakeStore struct {
	Store

	// Claim path.
	claimRun    store.Run
	claimErr    error
	claimParams *store.ClaimRunParams
	claimCtx    store.GetRunClaimContextRow
	claimCtxErr error
	// claimCtxCalled records whether the repo/forge claim-context join was queried;
	// the judge lane (PRD #46) must never touch it.
	claimCtxCalled bool
	anthropic      []byte
	anthropicErr   error
	// byIDSecrets is the by-id secret lookup (PRD #104): secret id → sealed row, the
	// path a WORKER-BOUND claim takes instead of the by-kind default. Keyed by id so
	// a test can stage two credentials and prove a rebind changes which one the
	// claim payload carries. byIDLookups records every (id, user) asked for, which is
	// what proves the lookup is owner-scoped.
	byIDSecrets map[uuid.UUID]store.GetUserSecretCiphertextByIDRow
	byIDLookups []store.GetUserSecretCiphertextByIDParams
	// byIDLabels is the label each by-id credential carries (PRD #111 M1), for the
	// snapshot a claim records. Optional: an id with no entry gets a synthetic label,
	// so a fixture that does not care about labels stages nothing.
	byIDLabels map[uuid.UUID]string
	// The DEFAULT credential as PRD #111 D8 now sees it. The default used to be
	// resolved INSIDE secretopen's ciphertext query — nothing above ever learned its
	// id — and is now resolved to (id, label) first and opened BY ID, so the fake
	// models both halves:
	//   - the id resolution yields is the fakeDefaultSecretID constant below, which
	//     is what keeps every pre-#111 fixture (staging only `anthropic`) working
	//     with no edit. There is deliberately no per-fixture override: one was added
	//     with M1, nothing ever assigned it, and an unassigned knob reads as a
	//     supported option while its branch is unreachable. M4 can add it back the
	//     moment a test needs two different default ids.
	//   - anthropicErr is raised from the RESOLVE, which is where production raises
	//     it: GetDefaultUserSecretMeta returns pgx.ErrNoRows for a token-less user.
	//   - defaultCiphertextErr is the OTHER half — resolution succeeded and the open
	//     did not, which is the D8 race (the token deleted between the two).
	defaultSecretLabel   string
	defaultCiphertextErr error
	defaultMetaLookups   []store.GetDefaultUserSecretMetaParams
	// metaByIDLookups records every owner-scoped label lookup, which is what proves
	// the label snapshot comes from a row scoped to the run's owner rather than from
	// an unscoped SELECT label WHERE id = $1.
	metaByIDLookups []store.GetUserSecretMetaByIDParams
	// recordedCreds is every SetRunAnthropicSecret write (PRD #111 M1) in order.
	// recordCredErr fails the write; recordCredRows overrides the rows-affected the
	// fake reports (0 = the run vanished under the claim).
	recordedCreds  []store.SetRunAnthropicSecretParams
	recordCredErr  error
	recordCredRows *int64
	// autoCandidates is what M4's ranking query returns (PRD #111 M4) and
	// autoCandidatesErr fails it. autoCandidateLookups records the user ids asked
	// for — which is what proves the selector never ranks another tenant's tokens,
	// and that a NON-auto worker never runs the query at all.
	autoCandidates       []store.ListAutoSelectCandidatesRow
	autoCandidatesErr    error
	autoCandidateLookups []uuid.UUID
	// judgeSecret is the user's judge-lane binding (PRD #104 M4); the zero value is
	// "unbound", which is every user's state until they choose otherwise, so existing
	// judge fixtures keep resolving the default with no change.
	judgeSecret    pgtype.UUID
	judgeSecretErr error
	// anthropicSealedWith is the row's sealed_with (defaults to 'master' when
	// empty, so existing fixtures are unchanged); set to 'dek' for vault tests.
	anthropicSealedWith string
	// onClaimRun, if set, runs inside ClaimRun — used to simulate the vault locking
	// between the claim gate and the token open (the M3 lock race).
	onClaimRun      func()
	defaultModel    pgtype.Text
	defaultModelErr error
	// judgeModel is the run owner's per-user judge_model override (PRD #69 M2);
	// the zero value is NULL/inherit, so existing judge fixtures resolve the
	// instance value unchanged. judgeModelErr models a user-row read fault, which
	// must fall back to the instance value best-effort (never an empty model).
	judgeModel          pgtype.Text
	judgeModelErr       error
	templates           []store.AgentTemplate
	skillAllocations    []store.ListRunSkillAllocationsRow
	skillAllocationsErr error
	markedFailed        *store.MarkRunFailedByIDParams
	// requeuedRun records the run id reset to queued by the vault lock-race path
	// (PRD #32 M3); nil unless RequeueClaimedRunToQueued was called.
	requeuedRun *uuid.UUID

	// issue #297: the in-flight avoid-set source for a self_improve claim.
	activeRunsAll    []store.ListActiveRunsAllRow
	activeRunsAllErr error

	// Ownership + messages + state.
	runOwned    store.Run
	runOwnedErr error
	// Agent-memory write path (PRD #90/#266): memRunCount is the per-run write count
	// CountAgentMemoryForRun returns; memInsertParams captures the InsertAgentMemory
	// arg so a test can assert what was actually persisted after sanitize/normalize;
	// memInserted flags whether the insert was reached at all (a cap must reject
	// BEFORE it).
	memRunCount      int64
	memInsertParams  *store.InsertAgentMemoryParams
	memInserted      bool
	insertedSeqs     map[int32]bool
	insertedMessages []store.InsertRunMessageParams
	// upsertedUsage records every UpsertRunUsage call (PRD #40 fold); usageErr, if
	// set, makes the fold's upsert fail (to prove a DB error propagates).
	upsertedUsage    []store.UpsertRunUsageParams
	usageErr         error
	lastSeqUpdated   *int32
	setRunningParams *store.SetRunRunningParams
	setAwaiting      *store.SetRunAwaitingApprovalParams
	setCompleted     *store.SetRunCompletedParams
	setFailed        *store.SetRunFailedParams
	reconciledMR     *store.ReconcileRunMRParams
	setRunningRows   int64
	setCompletedRows int64
	reconcileMRRows  int64
	consumeRows      []store.ConsumeRunInputsRow

	// Register + heartbeat.
	failOverCap      *store.FailWorkerRunsOverCapParams
	orphanFailedRuns []uuid.UUID // ids FailWorkerRunsOverCap returns (PRD #46 register-time judge funnel)
	requeueWorker    *store.RequeueWorkerRunsParams
	registerParams   *store.RegisterWorkerParams
	registerResult   store.Worker
	heartbeat        store.Worker
	callOrder        []string

	// Sweep.
	staleCutoff pgtype.Timestamptz
	claimCutoff pgtype.Timestamptz
	// PRD #122 M2 (Decision 5b): SweepRunningTimeout takes a per-run cutoff now — the
	// server passes `now` and the global timeout, and the SQL subtracts the per-run
	// effective wall clock. These capture what the sweep passed.
	runNow                  pgtype.Timestamptz
	runGlobalTimeoutSeconds int32
	sweepMax                int32
	// Rows the sweep queries return (PRD #25 M3): each drives a published transition.
	sweptClaimed  []store.SweepClaimedNeverStartedRow
	sweptTimeout  []store.SweepRunningTimeoutRow
	sweptFailed   []store.FailRunsOfStaleWorkersOverCapRow
	sweptRequeued []store.RequeueRunsOfStaleWorkersRow

	// PRD #46 judge: enqueue funnel + trace/review authz + review upsert.
	runByIDPlain      store.Run // GetRunByID (non-user-scoped): swept-run reload + trace target
	runByIDPlainErr   error
	userByID          store.User
	userByIDErr       error
	createdJudgeRun   *store.CreateJudgeRunParams
	createJudgeRunErr error
	// PRD #69 M5 Gate 5 spend guards (best-effort, fail-open). lastJudgeAt is the
	// cooldown lookup (Valid:false ⇒ no prior judge); judgesSince is the daily-budget
	// count. The *Err fields stage a read error to prove the guard proceeds anyway.
	lastJudgeAt       pgtype.Timestamptz
	lastJudgeAtErr    error
	judgesSince       int64
	judgesSinceErr    error
	judgesSinceArgs   []store.CountJudgesSinceParams
	activeJudgeRun    store.Run
	activeJudgeRunErr error
	// The PRD #119 pending-judge read (GetActiveJudgeRunForTarget). pendingJudgeErr is
	// normally pgx.ErrNoRows — "no judge in flight" is the common case and is NOT an
	// error to the service. pendingJudgeLookups records every target asked for, so a test
	// can assert the read was never ISSUED (the not-visible case must short-circuit on
	// the visibility gate, before any pending query).
	pendingJudgeRow     store.GetActiveJudgeRunForTargetRow
	pendingJudgeErr     error
	pendingJudgeLookups []pgtype.UUID
	toolTraceRows       []store.ListToolTraceForRunRow
	toolTraceRowsErr    error
	// knownTargets is the improve_uzi menu the judge claim carries (issue #232);
	// knownTargetsErr fails the lookup (which must NOT fail the claim — the menu is an
	// optimization). knownTargetsParams records the (user, lim) asked for, proving the
	// menu is owner-scoped and capped.
	knownTargets            []string
	knownTargetsErr         error
	knownTargetsParams      *store.ListKnownImproveUziTargetsForUserParams
	runInputs               []store.RunUserInput
	workerPageMessages      []store.RunMessage
	workerPageErr           error
	upsertedReview          *store.UpsertRunReviewWithRecommendationsParams
	upsertReviewErr         error
	reviewByTarget          store.RunReview
	reviewByTargetErr       error
	judgeRunUsage           store.GetJudgeRunUsageForTargetRow
	judgeRunUsageErr        error
	recsByReview            []store.ReviewRecommendation
	recsByReviewErr         error
	filedByReview           []store.RecommendationFiledIssue
	filedByReviewErr        error
	dispositionsByReview    []store.RecommendationDisposition
	dispositionsByReviewErr error
	// PRD #94 disposition write path + global stats aggregate. upsertedDisposition /
	// deletedDisposition capture the coordinate the service wrote; dispositionDeleteRows
	// is the rows-affected the DELETE returns (0 → the handler 404s an undo of nothing);
	// judgeTriageRows backs ListJudgeTriageRowsForUser for JudgeTriageStats.
	upsertedDisposition   *store.UpsertRecommendationDispositionParams
	deletedDisposition    *store.DeleteRecommendationDispositionParams
	dispositionDeleteRows int64
	// PRD issue #167 deterministic net: systemDismissed captures every
	// SystemDismissDeniedCLIRecommendation coordinate PostReview wrote; systemDismissRows
	// is the rows-affected it returns (0 = ON CONFLICT DO NOTHING hit an existing row),
	// systemDismissErr forces the best-effort error path.
	systemDismissed    []store.SystemDismissDeniedCLIRecommendationParams
	systemDismissRows  int64
	systemDismissErr   error
	judgeTriageRows    []store.ListJudgeTriageRowsForUserRow
	judgeTriageRowsErr error
	// judgeBacklogRows backs ListJudgeRecommendationRowsForUser (the PRD #98 M1 grouped
	// read); backlogArg records the params it was called with. Both are consumed in
	// judge_backlog_test.go, which also defines the fake's method.
	judgeBacklogRows []store.ListJudgeRecommendationRowsForUserRow
	backlogArg       *store.ListJudgeRecommendationRowsForUserParams

	// Submit input.
	runByID       store.Run
	runByIDErr    error
	workerByID    store.Worker
	workerByIDErr error
	createdInput  *store.CreateRunInputParams
	// reviseCount is the number of persisted revise_plan rows the fake pretends the run
	// already has (PRD #41 plan-revision cap); reviseCountRunID captures the run id the
	// read-only cap query was asked about. reviseCapArg captures the atomic capped-enqueue
	// call, which enforces the cap against reviseCount vs arg.MaxRevisions.
	reviseCount        int64
	reviseCountRunID   *uuid.UUID
	reviseCapArg       *store.CreateRunReviseInputIfUnderCapParams
	createdStopVerdict *store.CreateStopVerdictInputParams
	createdApproval    *store.CreateApprovePlanInputParams
	cancelled          *store.CancelRunServerSideParams
	rejected           *store.RejectRunServerSideParams

	// Create run.
	repoRow         store.GetRepoForUserRow // repo GetRepoForUser returns (zero value = the pre-#191 empty row)
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
	ciFixRunResult store.Run
	ciFixRunErr    error
	ciFixRunParams *store.CreateCIFixRunParams

	// Scheduled prompt (PRD #241). promptRunParams stays nil until CreatePromptRun's
	// insert runs, so a #66 guardrail test can assert the gate blocked before the insert.
	promptRunResult  store.Run
	promptRunErr     error
	promptRunParams  *store.CreatePromptRunParams
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

	// Chat (PRD #39).
	chatClaimRun          store.Run
	chatClaimErr          error
	chatClaimParams       *store.ClaimChatRunParams
	resumeSession         pgtype.Text
	followUpCount         int64
	sweptIdleChats        []store.SweepIdleChatRunsRow
	markedProposalDismiss *uuid.UUID
	markedProposalConfirm *store.MarkProposalConfirmedParams
	proposalDismissErr    error
	proposalConfirmErr    error

	// M3 (PRD #39): proposal creation + claim-first confirm.
	claimProposalRow     store.ClaimProposalForConfirmRow
	claimProposalErr     error
	getChatProposalRow   store.GetChatProposalForConfirmRow
	getChatProposalErr   error
	pendingProposalCount int64
	createdProposal      *store.CreateIssueProposalParams
	sweptStuckProposals  []uuid.UUID
	// PRD #191 M1: the lifted ConfirmProposalForUser reverts a claimed proposal on a
	// post-claim failure. revertedProposals records every RevertProposalToPending id,
	// in order, so a test can assert the revert fired (and how many times).
	revertedProposals []uuid.UUID
	revertProposalErr error

	// PRD #35 usage-limit park. setLimitWait captures the park params (which is where
	// the computed retry_not_before is asserted); setLimitWaitRows models the SQL's
	// POSITIVE source guard, so 0 means "the guard refused" and the service must map
	// that to applied=false rather than to a park. promoteLimitWaitAt records every
	// `now` the sweeper passed the promotion pass.
	selfImproveParams   *store.CreateSelfImproveRunParams
	setLimitWait        *store.SetRunLimitWaitParams
	setLimitWaitRows    int64
	setLimitWaitErr     error
	promotedLimitWait   []store.PromoteLimitWaitRunsRow
	promoteLimitWaitErr error
	promoteLimitWaitAt  []pgtype.Timestamptz

	// PRD #217 M1: the park-time gauge write. markedFiveHour / markedSevenDay record
	// every user_secret_id MarkFiveHourExhausted / MarkSevenDayExhausted was called
	// with, in order, so a test can assert WHICH window a park marked down (and that
	// the OTHER was left untouched). markExhaustedRows overrides the rows-affected the
	// fake reports (default 1; 0 models the UPDATE-only no-op against a token that has
	// no gauge row, D7); markExhaustedErr fails the write.
	markedFiveHour    []uuid.UUID
	markedSevenDay    []uuid.UUID
	markExhaustedRows *int64
	markExhaustedErr  error
}

// PRD #217 M1. The park path marks the dead credential's exhausted window down to
// 100% in the gauge. Both are :execrows UPDATEs whose zero-row result is success, so
// the default rows-affected is 1 and a fixture that wants the no-op says so.
func (f *fakeStore) MarkFiveHourExhausted(_ context.Context, userSecretID uuid.UUID) (int64, error) {
	f.markedFiveHour = append(f.markedFiveHour, userSecretID)
	return f.markExhaustedResult()
}

func (f *fakeStore) MarkSevenDayExhausted(_ context.Context, userSecretID uuid.UUID) (int64, error) {
	f.markedSevenDay = append(f.markedSevenDay, userSecretID)
	return f.markExhaustedResult()
}

func (f *fakeStore) markExhaustedResult() (int64, error) {
	if f.markExhaustedErr != nil {
		return 0, f.markExhaustedErr
	}
	if f.markExhaustedRows != nil {
		return *f.markExhaustedRows, nil
	}
	return 1, nil
}

func (f *fakeStore) SweepStuckConfirmingProposals(context.Context, pgtype.Timestamptz) ([]uuid.UUID, error) {
	return f.sweptStuckProposals, nil
}

func (f *fakeStore) ClaimProposalForConfirm(context.Context, store.ClaimProposalForConfirmParams) (store.ClaimProposalForConfirmRow, error) {
	return f.claimProposalRow, f.claimProposalErr
}
func (f *fakeStore) GetChatProposalForConfirm(context.Context, store.GetChatProposalForConfirmParams) (store.GetChatProposalForConfirmRow, error) {
	return f.getChatProposalRow, f.getChatProposalErr
}
func (f *fakeStore) CountPendingProposalsForRun(context.Context, uuid.UUID) (int64, error) {
	return f.pendingProposalCount, nil
}
func (f *fakeStore) CreateIssueProposal(_ context.Context, arg store.CreateIssueProposalParams) (store.IssueProposal, error) {
	f.createdProposal = &arg
	return store.IssueProposal{ID: uuid.New(), RunID: arg.RunID, RepoID: arg.RepoID, Title: arg.Title, Status: "pending"}, nil
}

func (f *fakeStore) ClaimChatRun(_ context.Context, arg store.ClaimChatRunParams) (store.Run, error) {
	f.chatClaimParams = &arg
	f.callOrder = append(f.callOrder, "claim_chat")
	return f.chatClaimRun, f.chatClaimErr
}
func (f *fakeStore) GetChatRunClaimContext(context.Context, uuid.UUID) (pgtype.Text, error) {
	return f.resumeSession, nil
}
func (f *fakeStore) CountChatFollowUps(context.Context, uuid.UUID) (int64, error) {
	return f.followUpCount, nil
}
func (f *fakeStore) SweepIdleChatRuns(context.Context, pgtype.Timestamptz) ([]store.SweepIdleChatRunsRow, error) {
	return f.sweptIdleChats, nil
}
func (f *fakeStore) MarkProposalConfirmed(_ context.Context, arg store.MarkProposalConfirmedParams) (store.IssueProposal, error) {
	f.markedProposalConfirm = &arg
	return store.IssueProposal{}, f.proposalConfirmErr
}
func (f *fakeStore) MarkProposalDismissed(_ context.Context, id uuid.UUID) (store.IssueProposal, error) {
	f.markedProposalDismiss = &id
	return store.IssueProposal{}, f.proposalDismissErr
}

func (f *fakeStore) ClaimRun(_ context.Context, arg store.ClaimRunParams) (store.Run, error) {
	f.claimParams = &arg
	if f.onClaimRun != nil {
		f.onClaimRun()
	}
	return f.claimRun, f.claimErr
}
func (f *fakeStore) GetRunClaimContext(context.Context, uuid.UUID) (store.GetRunClaimContextRow, error) {
	f.claimCtxCalled = true
	return f.claimCtx, f.claimCtxErr
}

// ListActiveRunsAll is the in-flight avoid-set source a self_improve claim reads
// (issue #297). The zero value is an empty set, so every pre-#297 claim fixture — which
// stages nothing here — takes the nil/empty path and never panics on the embedded Store.
func (f *fakeStore) ListActiveRunsAll(context.Context) ([]store.ListActiveRunsAllRow, error) {
	return f.activeRunsAll, f.activeRunsAllErr
}
func (f *fakeStore) GetUserSecretCiphertext(context.Context, store.GetUserSecretCiphertextParams) (store.GetUserSecretCiphertextRow, error) {
	sealedWith := f.anthropicSealedWith
	if sealedWith == "" {
		sealedWith = store.SealedWithMaster // default: fixtures seal with the master box
	}
	return store.GetUserSecretCiphertextRow{Ciphertext: f.anthropic, SealedWith: sealedWith}, f.anthropicErr
}
func (f *fakeStore) GetUserJudgeAnthropicSecret(context.Context, uuid.UUID) (pgtype.UUID, error) {
	return f.judgeSecret, f.judgeSecretErr
}
func (f *fakeStore) GetUserSecretCiphertextByID(_ context.Context, arg store.GetUserSecretCiphertextByIDParams) (store.GetUserSecretCiphertextByIDRow, error) {
	f.byIDLookups = append(f.byIDLookups, arg)
	if row, ok := f.byIDSecrets[arg.ID]; ok {
		return row, nil
	}
	// Since PRD #111 D8 the DEFAULT credential is opened by id too, so this lookup
	// serves it as well as the bound ones. Pre-#111 fixtures stage the default as
	// `anthropic` (+ `anthropicSealedWith`) and never name an id, so the default id
	// resolves here rather than through byIDSecrets. Every OTHER unknown id stays
	// pgx.ErrNoRows — that is the "bound to a vanished credential" fixture and it
	// must keep failing closed.
	if arg.ID == f.defaultCredID() {
		sealedWith := f.anthropicSealedWith
		if sealedWith == "" {
			sealedWith = store.SealedWithMaster
		}
		return store.GetUserSecretCiphertextByIDRow{
			UserID: arg.UserID, Kind: store.KindAnthropicToken,
			Ciphertext: f.anthropic, SealedWith: sealedWith,
		}, f.defaultCiphertextErr
	}
	return store.GetUserSecretCiphertextByIDRow{}, pgx.ErrNoRows
}

// fakeDefaultSecretID is the id the fake's owner-default anthropic credential has.
// A constant so a test can assert "the claim opened the DEFAULT" by id — which,
// since PRD #111 D8 made every open go by id, is the only way left to say that.
// Counting by-id lookups no longer distinguishes the default path from a bound one,
// because both take it.
var fakeDefaultSecretID = uuid.MustParse("d0000000-0000-4000-8000-00000000de17")

// defaultCredID is the id the fake's default resolution yields.
func (f *fakeStore) defaultCredID() uuid.UUID {
	return fakeDefaultSecretID
}

// defaultCredLabel is the label that rides with it. user_secrets.label is NOT NULL
// with a 1..64 CHECK since 00077 and PRD #104's compatibility path creates the row
// labelled literally 'default', so an empty label is not a state the store can be in.
func (f *fakeStore) defaultCredLabel() string {
	if f.defaultSecretLabel != "" {
		return f.defaultSecretLabel
	}
	return "default"
}

func (f *fakeStore) GetDefaultUserSecretMeta(_ context.Context, arg store.GetDefaultUserSecretMetaParams) (store.GetDefaultUserSecretMetaRow, error) {
	f.defaultMetaLookups = append(f.defaultMetaLookups, arg)
	// anthropicErr is the pre-#111 "this user has no token" knob, and it belongs
	// HERE now: production raises pgx.ErrNoRows from this resolve, one query earlier
	// than the ciphertext read it used to come from. The claim must still map it to
	// the same errCredentialUnavailable text either way.
	if f.anthropicErr != nil {
		return store.GetDefaultUserSecretMetaRow{}, f.anthropicErr
	}
	return store.GetDefaultUserSecretMetaRow{ID: f.defaultCredID(), Label: f.defaultCredLabel()}, nil
}

func (f *fakeStore) GetUserSecretMetaByID(_ context.Context, arg store.GetUserSecretMetaByIDParams) (store.GetUserSecretMetaByIDRow, error) {
	f.metaByIDLookups = append(f.metaByIDLookups, arg)
	row, ok := f.byIDSecrets[arg.ID]
	if !ok {
		return store.GetUserSecretMetaByIDRow{}, pgx.ErrNoRows
	}
	// The real query is owner-scoped in its predicate, so a foreign id yields no
	// rows rather than that user's label. Mirrored here so a fixture staging a
	// cross-user id cannot pass by accident.
	if row.UserID != uuid.Nil && row.UserID != arg.UserID {
		return store.GetUserSecretMetaByIDRow{}, pgx.ErrNoRows
	}
	label, ok := f.byIDLabels[arg.ID]
	if !ok {
		label = "token-" + arg.ID.String()[:8]
	}
	return store.GetUserSecretMetaByIDRow{ID: arg.ID, Label: label}, nil
}

func (f *fakeStore) ListAutoSelectCandidates(_ context.Context, userID uuid.UUID) ([]store.ListAutoSelectCandidatesRow, error) {
	f.autoCandidateLookups = append(f.autoCandidateLookups, userID)
	if f.autoCandidatesErr != nil {
		return nil, f.autoCandidatesErr
	}
	return f.autoCandidates, nil
}

func (f *fakeStore) SetRunAnthropicSecret(_ context.Context, arg store.SetRunAnthropicSecretParams) (int64, error) {
	f.recordedCreds = append(f.recordedCreds, arg)
	if f.recordCredErr != nil {
		return 0, f.recordCredErr
	}
	if f.recordCredRows != nil {
		return *f.recordCredRows, nil
	}
	return 1, nil
}
func (f *fakeStore) GetUserDefaultModel(context.Context, uuid.UUID) (pgtype.Text, error) {
	return f.defaultModel, f.defaultModelErr
}
func (f *fakeStore) GetUserJudgeModel(context.Context, uuid.UUID) (pgtype.Text, error) {
	return f.judgeModel, f.judgeModelErr
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
func (f *fakeStore) GetRunForgeConnForWorker(context.Context, store.GetRunForgeConnForWorkerParams) (store.GetRunForgeConnForWorkerRow, error) {
	return store.GetRunForgeConnForWorkerRow{}, nil
}
func (f *fakeStore) CountAgentMemoryForRun(context.Context, pgtype.UUID) (int64, error) {
	return f.memRunCount, nil
}
func (f *fakeStore) InsertAgentMemory(_ context.Context, arg store.InsertAgentMemoryParams) (store.AgentMemory, error) {
	f.memInserted = true
	f.memInsertParams = &arg
	return store.AgentMemory{ID: uuid.New(), Title: arg.Title, Body: arg.Body, Basis: arg.Basis, Evidence: arg.Evidence}, nil
}
func (f *fakeStore) EvictAgentMemoryOverCap(context.Context, store.EvictAgentMemoryOverCapParams) error {
	return nil
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
func (f *fakeStore) UpsertRunUsage(_ context.Context, arg store.UpsertRunUsageParams) error {
	if f.usageErr != nil {
		return f.usageErr
	}
	f.upsertedUsage = append(f.upsertedUsage, arg)
	return nil
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
func (f *fakeStore) ReconcileRunMR(_ context.Context, arg store.ReconcileRunMRParams) (int64, error) {
	f.reconciledMR = &arg
	return f.reconcileMRRows, nil
}

// PRD #35. setLimitWaitRows defaults to 0, which is the SQL guard refusing — so a
// fixture that wants a park to land must say so, and one that says nothing gets the
// no-op. That is the safe default for a fake whose real query carries a positive
// source guard.
func (f *fakeStore) SetRunLimitWait(_ context.Context, arg store.SetRunLimitWaitParams) (int64, error) {
	f.setLimitWait = &arg
	return f.setLimitWaitRows, f.setLimitWaitErr
}

func (f *fakeStore) PromoteLimitWaitRuns(_ context.Context, now pgtype.Timestamptz) ([]store.PromoteLimitWaitRunsRow, error) {
	f.promoteLimitWaitAt = append(f.promoteLimitWaitAt, now)
	return f.promotedLimitWait, f.promoteLimitWaitErr
}
func (f *fakeStore) ConsumeRunInputs(context.Context, uuid.UUID) ([]store.ConsumeRunInputsRow, error) {
	return f.consumeRows, nil
}
func (f *fakeStore) FailWorkerRunsOverCap(_ context.Context, arg store.FailWorkerRunsOverCapParams) ([]uuid.UUID, error) {
	f.failOverCap = &arg
	f.callOrder = append(f.callOrder, "fail_over_cap")
	return f.orphanFailedRuns, nil
}
func (f *fakeStore) RequeueWorkerRuns(_ context.Context, arg store.RequeueWorkerRunsParams) (int64, error) {
	f.requeueWorker = &arg
	f.callOrder = append(f.callOrder, "requeue_worker")
	return 0, nil
}
func (f *fakeStore) RegisterWorker(_ context.Context, arg store.RegisterWorkerParams) (store.RegisterWorkerRow, error) {
	f.registerParams = &arg
	f.callOrder = append(f.callOrder, "register")
	return store.RegisterWorkerRow(f.registerResult), nil
}
func (f *fakeStore) HeartbeatWorker(context.Context, store.HeartbeatWorkerParams) (store.Worker, error) {
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
	f.runNow = arg.Now
	f.runGlobalTimeoutSeconds = arg.GlobalTimeoutSeconds
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

// PRD #46 judge: enqueue funnel + trace/review.
func (f *fakeStore) GetRunByID(context.Context, uuid.UUID) (store.Run, error) {
	return f.runByIDPlain, f.runByIDPlainErr
}
func (f *fakeStore) GetUserByID(context.Context, uuid.UUID) (store.User, error) {
	return f.userByID, f.userByIDErr
}
func (f *fakeStore) CreateJudgeRun(_ context.Context, arg store.CreateJudgeRunParams) (store.Run, error) {
	f.createdJudgeRun = &arg
	if f.createJudgeRunErr != nil {
		return store.Run{}, f.createJudgeRunErr
	}
	return store.Run{ID: uuid.New(), Kind: RunKindJudge, UserID: arg.UserID, TargetRunID: arg.TargetRunID}, nil
}
func (f *fakeStore) GetActiveJudgeRunForWorkerTarget(context.Context, store.GetActiveJudgeRunForWorkerTargetParams) (store.Run, error) {
	return f.activeJudgeRun, f.activeJudgeRunErr
}

// PRD #69 M5 Gate 5 spend-guard reads.
func (f *fakeStore) LastJudgeEnqueuedAt(context.Context, uuid.UUID) (pgtype.Timestamptz, error) {
	return f.lastJudgeAt, f.lastJudgeAtErr
}
func (f *fakeStore) CountJudgesSince(_ context.Context, arg store.CountJudgesSinceParams) (int64, error) {
	f.judgesSinceArgs = append(f.judgesSinceArgs, arg)
	return f.judgesSince, f.judgesSinceErr
}

// The PRD #119 pending-judge read. It records the target it was asked for, which is how
// the panel tests assert the query is NOT issued for a run the viewer cannot see.
func (f *fakeStore) GetActiveJudgeRunForTarget(_ context.Context, targetRunID pgtype.UUID) (store.GetActiveJudgeRunForTargetRow, error) {
	f.pendingJudgeLookups = append(f.pendingJudgeLookups, targetRunID)
	return f.pendingJudgeRow, f.pendingJudgeErr
}
func (f *fakeStore) ListToolTraceForRun(context.Context, store.ListToolTraceForRunParams) ([]store.ListToolTraceForRunRow, error) {
	return f.toolTraceRows, f.toolTraceRowsErr
}
func (f *fakeStore) ListKnownImproveUziTargetsForUser(_ context.Context, arg store.ListKnownImproveUziTargetsForUserParams) ([]string, error) {
	f.knownTargetsParams = &arg
	return f.knownTargets, f.knownTargetsErr
}
func (f *fakeStore) ListRunInputsForRun(context.Context, store.ListRunInputsForRunParams) ([]store.RunUserInput, error) {
	return f.runInputs, nil
}
func (f *fakeStore) ListRunMessagesForWorkerPage(context.Context, store.ListRunMessagesForWorkerPageParams) ([]store.RunMessage, error) {
	return f.workerPageMessages, f.workerPageErr
}
func (f *fakeStore) UpsertRunReviewWithRecommendations(_ context.Context, arg store.UpsertRunReviewWithRecommendationsParams) (uuid.UUID, error) {
	f.upsertedReview = &arg
	if f.upsertReviewErr != nil {
		return uuid.UUID{}, f.upsertReviewErr
	}
	return uuid.New(), nil
}
func (f *fakeStore) GetRunReviewForTarget(context.Context, uuid.UUID) (store.RunReview, error) {
	return f.reviewByTarget, f.reviewByTargetErr
}
func (f *fakeStore) GetJudgeRunUsageForTarget(context.Context, uuid.UUID) (store.GetJudgeRunUsageForTargetRow, error) {
	return f.judgeRunUsage, f.judgeRunUsageErr
}
func (f *fakeStore) ListRecommendationsForReview(context.Context, uuid.UUID) ([]store.ReviewRecommendation, error) {
	return f.recsByReview, f.recsByReviewErr
}
func (f *fakeStore) ListFiledIssuesForReview(context.Context, uuid.UUID) ([]store.RecommendationFiledIssue, error) {
	return f.filedByReview, f.filedByReviewErr
}
func (f *fakeStore) ListDispositionsForReview(context.Context, uuid.UUID) ([]store.RecommendationDisposition, error) {
	return f.dispositionsByReview, f.dispositionsByReviewErr
}

// PRD #94 disposition write path + global stats aggregate.
func (f *fakeStore) UpsertRecommendationDisposition(_ context.Context, arg store.UpsertRecommendationDispositionParams) (store.RecommendationDisposition, error) {
	f.upsertedDisposition = &arg
	return store.RecommendationDisposition{
		ID: uuid.New(), ReviewID: arg.ReviewID, Category: arg.Category, Target: arg.Target,
		Status: arg.Status, DismissReason: arg.DismissReason, RationaleHash: arg.RationaleHash,
	}, nil
}
func (f *fakeStore) SystemDismissDeniedCLIRecommendation(_ context.Context, arg store.SystemDismissDeniedCLIRecommendationParams) (int64, error) {
	f.systemDismissed = append(f.systemDismissed, arg)
	if f.systemDismissErr != nil {
		return 0, f.systemDismissErr
	}
	return f.systemDismissRows, nil
}
func (f *fakeStore) DeleteRecommendationDisposition(_ context.Context, arg store.DeleteRecommendationDispositionParams) (int64, error) {
	f.deletedDisposition = &arg
	return f.dispositionDeleteRows, nil
}
func (f *fakeStore) ListJudgeTriageRowsForUser(_ context.Context, userID uuid.UUID) ([]store.ListJudgeTriageRowsForUserRow, error) {
	return f.judgeTriageRows, f.judgeTriageRowsErr
}
func (f *fakeStore) GetWorkerByID(context.Context, uuid.UUID) (store.Worker, error) {
	return f.workerByID, f.workerByIDErr
}
func (f *fakeStore) CreateRunInput(_ context.Context, arg store.CreateRunInputParams) (store.RunUserInput, error) {
	f.createdInput = &arg
	return store.RunUserInput{}, nil
}
func (f *fakeStore) CountRunReviseInputs(_ context.Context, runID uuid.UUID) (int64, error) {
	f.reviseCountRunID = &runID
	return f.reviseCount, nil
}
func (f *fakeStore) CreateRunReviseInputIfUnderCap(_ context.Context, arg store.CreateRunReviseInputIfUnderCapParams) (store.RunUserInput, error) {
	f.reviseCapArg = &arg
	// Emulate the atomic cap: the insert happens only while the already-persisted count
	// is strictly under the cap, else no row (pgx.ErrNoRows) — same as the real query.
	if f.reviseCount >= int64(arg.MaxRevisions) {
		return store.RunUserInput{}, pgx.ErrNoRows
	}
	return store.RunUserInput{ID: 1, RunID: arg.RunID, Kind: "revise_plan", Body: arg.Body}, nil
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
	return f.repoRow, f.repoErr
}
func (f *fakeStore) RevertProposalToPending(_ context.Context, id uuid.UUID) (int64, error) {
	f.revertedProposals = append(f.revertedProposals, id)
	return 1, f.revertProposalErr
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
func (f *fakeStore) CreateSelfImproveRun(_ context.Context, arg store.CreateSelfImproveRunParams) (store.Run, error) {
	f.selfImproveParams = &arg
	return store.Run{ID: uuid.New(), Kind: "self_improve"}, nil
}
func (f *fakeStore) CreateCIFixRun(_ context.Context, arg store.CreateCIFixRunParams) (store.Run, error) {
	f.ciFixRunParams = &arg
	return f.ciFixRunResult, f.ciFixRunErr
}
func (f *fakeStore) CreatePromptRun(_ context.Context, arg store.CreatePromptRunParams) (store.Run, error) {
	f.promptRunParams = &arg
	return f.promptRunResult, f.promptRunErr
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
		RunTimeout:             2 * time.Hour,
		RunIdleTimeout:         10 * time.Minute,
		RunMaxIterations:       5,
		PlanMaxRevisions:       3,
		QuestionMax:            5,     // PRD #88 clarification-question cap
		QuestionTimeoutSeconds: 86400, // PRD #88 answer deadline (24h)
		RunMaxRequeues:         1,
		WorkerHeartbeatStale:   45 * time.Second,
		WorkerAffinityGrace:    2 * time.Minute,
		WorkerSpreadGrace:      9 * time.Second,
		ClaimGrace:             5 * time.Minute,
		SkillMaxBytes:          65536,
		SkillsMaxPerRun:        32,
		ChatIdleTimeout:        70 * time.Minute,
		ChatMaxTurns:           50,
		WorkerChatIdleTimeout:  60 * time.Minute,
		WorkerChatTurnTimeout:  10 * time.Minute,
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

// scheduleModelStore builds a fakeStore whose claim will succeed, parameterised on
// the frozen per-run model (runs.model, set by a schedule at fire time — PRD #300)
// and the owner's per-user Worker default (GetUserDefaultModel). Both are pgtype.Text
// so a zero value models NULL/inherit.
func scheduleModelStore(t *testing.T, runModel, userDefault pgtype.Text) *fakeStore {
	t.Helper()
	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("bot-pat-SCHEDMODEL-abcdef1234567890"))
	sealedTok, _ := box.Seal([]byte("anthropic-SCHEDMODEL-abcdef1234567890"))
	return &fakeStore{
		claimRun: store.Run{
			ID: uuid.New(), IssueIid: pgtype.Int8{Int64: 30, Valid: true}, Status: "claimed",
			Model: runModel,
		},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/g/p", RepoPath: "g/p",
			ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic:    sealedTok,
		defaultModel: userDefault,
	}
}

// PRD #300 Success Criterion 2: a schedule that froze a per-run model onto runs.model
// beats the owner's per-user Worker default for THIS run's Config.DefaultModel. The
// frozen model also must NOT disturb a subagent template's own model: pin — that pin
// rides each agent independently (Config.DefaultModel is the LEAD default only, which
// the worker's resolveLeadModel reads; pinned subagents bypass it), so claim assembly
// only copies the pin through and the "pin wins over the schedule model" guarantee is
// entirely worker-side and unchanged here.
func TestClaimScheduleModelOverridesUserDefault(t *testing.T) {
	fs := scheduleModelStore(t, pgText("fable"), pgText("sonnet"))
	fs.templates = []store.AgentTemplate{
		{Name: "coder", Description: "writes code", PromptBody: "you code", Tools: []byte(`["Read","Edit"]`)},
		{Name: "reviewer", Description: "reviews", PromptBody: "you review", Model: pgText("claude-opus-4-8")},
	}

	payload, err := New(fs, newBox(t), testParams()).Claim(context.Background(), worker())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload.Config.DefaultModel == nil || *payload.Config.DefaultModel != "fable" {
		t.Fatalf("schedule model should beat the owner default; got %+v", payload.Config.DefaultModel)
	}
	// The subagent's own pin is orthogonal: the schedule model overrode only the LEAD
	// default, not this agent's Model.
	if payload.Agents[1].Model == nil || *payload.Agents[1].Model != "claude-opus-4-8" {
		t.Fatalf("subagent pin must survive the schedule-model override; got %+v", payload.Agents[1].Model)
	}
}

// PRD #305 M3: the flag frozen onto the run at fire time (runs.override_subagent_model,
// M1) is delivered on the claim config as OverrideSubagentModel. Flag ON → the config
// field is true, so the worker knows to apply the run model to every subagent (the
// behavior itself is M4, worker-side). Read straight off the run row.
func TestClaimDeliversOverrideSubagentModelWhenFrozenOn(t *testing.T) {
	fs := scheduleModelStore(t, pgText("fable"), pgtype.Text{})
	fs.claimRun.OverrideSubagentModel = true

	payload, err := New(fs, newBox(t), testParams()).Claim(context.Background(), worker())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !payload.Config.OverrideSubagentModel {
		t.Fatalf("frozen override_subagent_model=true must ride the claim config; got %+v", payload.Config)
	}
}

// PRD #305 M3: with the flag off (the default on the run row) the config field is false
// AND omitted from the marshaled claim JSON — omitempty keeps an off run's claim
// byte-identical to today's wire, so an un-upgraded worker sees the same shape it does
// now. Mirrors TestClaimOmitsDefaultModelWhenOwnerHasNone's omit-on-the-wire check.
func TestClaimOmitsOverrideSubagentModelWhenFrozenOff(t *testing.T) {
	fs := scheduleModelStore(t, pgText("fable"), pgtype.Text{})
	// claimRun.OverrideSubagentModel left zero ⇒ the run did not opt in.

	payload, err := New(fs, newBox(t), testParams()).Claim(context.Background(), worker())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload.Config.OverrideSubagentModel {
		t.Fatalf("an off run must deliver override_subagent_model=false; got %+v", payload.Config)
	}
	b, err := json.Marshal(payload.Config)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if strings.Contains(string(b), "override_subagent_model") {
		t.Fatalf("an off run's override_subagent_model must be omitted from the wire; got %s", b)
	}
}

// PRD #300: the override fires regardless of whether the owner has a per-user default —
// a frozen runs.model with the owner default left NULL still lands on Config.DefaultModel.
func TestClaimScheduleModelOverridesEvenWithNoUserDefault(t *testing.T) {
	fs := scheduleModelStore(t, pgText("fable"), pgtype.Text{})

	payload, err := New(fs, newBox(t), testParams()).Claim(context.Background(), worker())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload.Config.DefaultModel == nil || *payload.Config.DefaultModel != "fable" {
		t.Fatalf("schedule model should apply even without an owner default; got %+v", payload.Config.DefaultModel)
	}
}

// PRD #300 Success Criterion 4 (regression): with runs.model NULL the schedule layer
// changes nothing — Config.DefaultModel is the owner's per-user default exactly as
// before #300, byte-identical on the wire.
func TestClaimNoScheduleModelUsesUserDefault(t *testing.T) {
	fs := scheduleModelStore(t, pgtype.Text{}, pgText("sonnet"))

	payload, err := New(fs, newBox(t), testParams()).Claim(context.Background(), worker())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload.Config.DefaultModel == nil || *payload.Config.DefaultModel != "sonnet" {
		t.Fatalf("owner default must ride when no schedule model is frozen; got %+v", payload.Config.DefaultModel)
	}
	// The no-override path must leave the serialised config carrying exactly the owner
	// default — the schedule layer added nothing.
	b, err := json.Marshal(payload.Config)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if !strings.Contains(string(b), `"default_model":"sonnet"`) {
		t.Fatalf("null run.model should leave the owner default untouched on the wire; got %s", b)
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
	pkgs, err := svc.resolveTooling(context.Background(), store.Run{UserID: uuid.New(), RepoID: pgUUID(uuid.New())})
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
		toolProfile:   store.RepoToolProfile{Packages: []byte(`["kubectl@1.31","opentofu"]`)},
		toolAllowlist: []store.ToolAllowlist{{Name: "kubectl"}}, // opentofu removed after the profile was saved
	}
	svc := New(fs, newBox(t), testParams())
	_, err := svc.resolveTooling(context.Background(), store.Run{UserID: uuid.New(), RepoID: pgUUID(uuid.New())})
	if !errors.Is(err, errToolPackagesRejected) {
		t.Fatalf("err = %v, want errToolPackagesRejected", err)
	}
	if !strings.Contains(err.Error(), "opentofu") {
		t.Fatalf("error should name the rejected package: %v", err)
	}
}

func TestResolveToolingRejectsAllowlistedButUnbakedPackage(t *testing.T) {
	// PRD #123 M3 (Decision 4c): a package that is on the allowlist but NOT in the
	// baked worker toolchain (a grandfathered row that predates the write-time
	// gate) fails the claim here rather than hanging the run behind the egress
	// block. `terraform` is allowlist-shaped but not baked (swapped off for
	// opentofu, and unfree anyway), so it is not toolseed.Covered.
	fs := &fakeStore{
		toolProfile:   store.RepoToolProfile{Packages: []byte(`["jq","terraform"]`)},
		toolAllowlist: []store.ToolAllowlist{{Name: "jq"}, {Name: "terraform"}},
	}
	svc := New(fs, newBox(t), testParams())
	_, err := svc.resolveTooling(context.Background(), store.Run{UserID: uuid.New(), RepoID: pgUUID(uuid.New())})
	if !errors.Is(err, errToolPackagesRejected) {
		t.Fatalf("err = %v, want errToolPackagesRejected", err)
	}
	if !strings.Contains(err.Error(), "terraform") {
		t.Fatalf("error should name the unbaked package: %v", err)
	}
	if !strings.Contains(err.Error(), "baked toolchain") {
		t.Fatalf("error should explain the unbaked reason: %v", err)
	}
}

func TestResolveToolingNoProfileMeansNoProvisioning(t *testing.T) {
	fs := &fakeStore{toolProfileErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	pkgs, err := svc.resolveTooling(context.Background(), store.Run{UserID: uuid.New(), RepoID: pgUUID(uuid.New())})
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
		toolProfile:   store.RepoToolProfile{Packages: []byte(`["opentofu"]`)},
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
	if !strings.Contains(fs.markedFailed.FailureReason.String, "opentofu") {
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

// --- PRD #40 M2: the run_usage fold ----------------------------------------

// A success result frame's modelUsage folds one UpsertRunUsage per model, keyed by
// the run's session_id, with the SDK's camelCase fields mapped to the columns;
// non-result messages in the same batch never fold.
func TestAppendMessagesFoldsResultUsagePerModel(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), SessionID: pgText("sess-1")}}
	svc := New(fs, newBox(t), testParams())

	result := json.RawMessage(`{
		"event":"result","subtype":"success",
		"usage":{"input_tokens":1600,"output_tokens":900},
		"modelUsage":{
			"claude-fable-5":{"inputTokens":1200,"outputTokens":800,"cacheReadInputTokens":400,"cacheCreationInputTokens":50,"costUSD":0.0731},
			"claude-haiku-4-5":{"inputTokens":400,"outputTokens":100,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.0009}
		}}`)
	msgs := []IncomingMessage{
		{Seq: 1, Kind: "text", Agent: "lead", Payload: json.RawMessage(`{"text":"working"}`)},
		{Seq: 2, Kind: "status", Agent: "lead", Payload: result},
	}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	if len(fs.upsertedUsage) != 2 {
		t.Fatalf("expected 2 usage upserts (one per model), got %d", len(fs.upsertedUsage))
	}
	byModel := map[string]store.UpsertRunUsageParams{}
	for _, u := range fs.upsertedUsage {
		byModel[u.Model] = u
		if u.RunID != fs.runOwned.ID || u.SessionID != "sess-1" {
			t.Fatalf("usage row must be keyed by (run, session): got run=%v session=%q", u.RunID, u.SessionID)
		}
	}
	fable := byModel["claude-fable-5"]
	if fable.InputTokens != 1200 || fable.OutputTokens != 800 || fable.CacheReadTokens != 400 || fable.CacheCreationTokens != 50 {
		t.Fatalf("fable tokens mismatch: %+v", fable)
	}
	if fable.CostUsd.Int == nil || fable.CostUsd.Int.Int64() != 73100 || fable.CostUsd.Exp != -6 {
		t.Fatalf("fable cost should quantize 0.0731 → 73100e-6, got int=%v exp=%d", fable.CostUsd.Int, fable.CostUsd.Exp)
	}
}

// The error branch folds too (Decision 4): a failed/cancelled run's pre-death
// spend must be counted, so an `error`-kind result frame upserts its usage. Uses a
// ci_fix run to also lock the positive side of the kind guard — ci_fix is a work
// run and MUST fold (only chat is excluded).
func TestAppendMessagesFoldsErrorResultUsage(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), Kind: RunKindCIFix, SessionID: pgText("sess-err")}}
	svc := New(fs, newBox(t), testParams())

	msgs := []IncomingMessage{{Seq: 1, Kind: "error", Agent: "lead", Payload: json.RawMessage(`{
		"event":"result","subtype":"error_max_turns","errors":["cap"],
		"modelUsage":{"claude-fable-5":{"inputTokens":500,"outputTokens":120,"costUSD":0.031}}}`)}}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	if len(fs.upsertedUsage) != 1 || fs.upsertedUsage[0].InputTokens != 500 || fs.upsertedUsage[0].OutputTokens != 120 {
		t.Fatalf("error-frame usage not folded: %+v", fs.upsertedUsage)
	}
}

// Malformed / absent / non-result payloads are skipped and never fail the append:
// a plain status string, a result frame with no modelUsage, and an empty
// modelUsage all fold nothing.
func TestAppendMessagesFoldMalformedIsNoOp(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID)}}
	svc := New(fs, newBox(t), testParams())

	msgs := []IncomingMessage{
		{Seq: 1, Kind: "status", Payload: json.RawMessage(`"running"`)},                          // not an object
		{Seq: 2, Kind: "status", Payload: json.RawMessage(`{"event":"init","model":"x"}`)},       // not a result frame
		{Seq: 3, Kind: "status", Payload: json.RawMessage(`{"event":"result"}`)},                 // result, no modelUsage
		{Seq: 4, Kind: "status", Payload: json.RawMessage(`{"event":"result","modelUsage":{}}`)}, // empty modelUsage
		{Seq: 5, Kind: "error", Payload: json.RawMessage(`{"event":"result","errors":["x"]}`)},   // error, no usage
	}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err != nil {
		t.Fatalf("AppendMessages must not fail on malformed usage: %v", err)
	}
	if len(fs.upsertedUsage) != 0 {
		t.Fatalf("malformed/absent usage must fold nothing, got %d upserts", len(fs.upsertedUsage))
	}
}

// The fold runs on EVERY delivery, including a seq-deduped re-delivery (crash
// retry) — NOT only on newly-inserted messages. Re-delivering the same batch
// re-invokes UpsertRunUsage; the GREATEST merge (proven at the store layer) makes
// that idempotent, so this is what makes "re-delivery changes nothing" hold.
func TestAppendMessagesFoldsOnRedeliveredBatch(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), SessionID: pgText("s")}}
	svc := New(fs, newBox(t), testParams())

	batch := []IncomingMessage{{Seq: 1, Kind: "status", Payload: json.RawMessage(
		`{"event":"result","modelUsage":{"claude-fable-5":{"inputTokens":100,"outputTokens":50,"costUSD":0.001}}}`)}}

	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, batch); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, batch); err != nil {
		t.Fatalf("re-delivery: %v", err)
	}
	// The re-delivered message is a seq-dedup insert (rows == 0), yet the fold ran
	// both times — proving it iterates delivered messages, not inserted ones.
	if len(fs.insertedMessages) != 2 {
		t.Fatalf("expected 2 insert attempts across the two deliveries, got %d", len(fs.insertedMessages))
	}
	if len(fs.upsertedUsage) != 2 {
		t.Fatalf("fold must run on the re-delivered (deduped) batch too: got %d upserts, want 2", len(fs.upsertedUsage))
	}
}

// Chat runs (PRD #39) are OUT of scope for usage accounting (PRD #40), but
// mapResult is shared with the chat executor so their result frames now carry
// usage — the fold must skip them entirely, keeping chat spend out of run_usage.
func TestAppendMessagesFoldSkipsChatRuns(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), Kind: RunKindChat, SessionID: pgText("sess-chat")}}
	svc := New(fs, newBox(t), testParams())

	msgs := []IncomingMessage{{Seq: 1, Kind: "status", Agent: "lead", Payload: json.RawMessage(
		`{"event":"result","modelUsage":{"claude-fable-5":{"inputTokens":9000,"outputTokens":4000,"costUSD":0.5}}}`)}}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	// The message still persists (chat runs stream normally); only the fold is skipped.
	if len(fs.insertedMessages) != 1 {
		t.Fatalf("chat message should still persist, got %d inserts", len(fs.insertedMessages))
	}
	if len(fs.upsertedUsage) != 0 {
		t.Fatalf("chat-run usage must NOT fold into run_usage, got %d upserts", len(fs.upsertedUsage))
	}
}

// Out-of-domain fold inputs are clamped into the run_usage columns' domains so the
// append can never 22003 (a poison loop: the worker's batcher retries a failed batch
// at head forever). An absurd cost (>= the numeric(12,6) ceiling) clamps to the
// ceiling and negative token counts clamp to 0 — the append still succeeds.
func TestAppendMessagesFoldClampsOutOfRangeValues(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), SessionID: pgText("s")}}
	svc := New(fs, newBox(t), testParams())

	msgs := []IncomingMessage{{Seq: 1, Kind: "status", Payload: json.RawMessage(
		`{"event":"result","modelUsage":{"claude-fable-5":{"inputTokens":-5,"outputTokens":-1,"cacheReadInputTokens":-9,"costUSD":1000000000}}}`)}}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err != nil {
		t.Fatalf("append must not fail on out-of-range usage (poison-loop guard): %v", err)
	}
	if len(fs.upsertedUsage) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(fs.upsertedUsage))
	}
	u := fs.upsertedUsage[0]
	if u.InputTokens != 0 || u.OutputTokens != 0 || u.CacheReadTokens != 0 {
		t.Fatalf("negative token counts must clamp to 0, got %+v", u)
	}
	// costUSD 1e9 clamps to the numeric(12,6) ceiling: 999999.999999 → 999999999999e-6.
	if u.CostUsd.Int == nil || u.CostUsd.Int.Int64() != 999999999999 || u.CostUsd.Exp != -6 {
		t.Fatalf("absurd cost must clamp to the column ceiling, got int=%v exp=%d", u.CostUsd.Int, u.CostUsd.Exp)
	}
}

// A DB error on the fold's upsert propagates: the append fails so the worker
// re-delivers the batch and the fold retries (idempotent), rather than silently
// dropping the usage.
func TestAppendMessagesFoldDBErrorPropagates(t *testing.T) {
	w := worker()
	fs := &fakeStore{
		runOwned: store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID)},
		usageErr: errors.New("boom"),
	}
	svc := New(fs, newBox(t), testParams())
	msgs := []IncomingMessage{{Seq: 1, Kind: "status", Payload: json.RawMessage(
		`{"event":"result","modelUsage":{"claude-fable-5":{"inputTokens":1,"outputTokens":1}}}`)}}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err == nil {
		t.Fatal("a fold DB error must propagate so the worker re-delivers")
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

	if _, err := svc.Register(context.Background(), w, "1.2.3", "jvm", intp(2)); err != nil {
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
	// The advertised concurrency cap rides into max_concurrent_runs (PRD #42).
	if !fs.registerParams.MaxConcurrentRuns.Valid || fs.registerParams.MaxConcurrentRuns.Int32 != 2 {
		t.Fatalf("register max_concurrent_runs wrong: %+v", fs.registerParams.MaxConcurrentRuns)
	}
}

func TestRegisterNilCapStoresNull(t *testing.T) {
	// A worker that advertises no cap (an older image, or M3a before the M2 agent
	// sends it) ⇒ max_concurrent_runs stays NULL, never 0.
	w := worker()
	fs := &fakeStore{registerResult: store.Worker{ID: w.ID, Status: "online"}}
	svc := New(fs, newBox(t), testParams())

	if _, err := svc.Register(context.Background(), w, "1.2.3", "", nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if fs.registerParams == nil || fs.registerParams.MaxConcurrentRuns.Valid {
		t.Fatalf("nil cap must be NULL, got %+v", fs.registerParams.MaxConcurrentRuns)
	}
}

func TestRegisterEmptyTemplateStoresNull(t *testing.T) {
	// An older image reports no template ⇒ template_reported stays NULL, never an
	// empty string.
	w := worker()
	fs := &fakeStore{registerResult: store.Worker{ID: w.ID, Status: "online"}}
	svc := New(fs, newBox(t), testParams())

	if _, err := svc.Register(context.Background(), w, "1.2.3", "", nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if fs.registerParams == nil || fs.registerParams.TemplateReported.Valid {
		t.Fatalf("empty template must be NULL, got %+v", fs.registerParams.TemplateReported)
	}
}

// intp returns a pointer to v, for the optional *int register cap param (PRD #42).
func intp(v int) *int { return &v }

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
	// PRD #122 M2 (Decision 5b): the sweep passes `now` and the GLOBAL timeout (2h in
	// seconds); the per-run subtraction happens in SQL against each run's budget.
	if !fs.runNow.Time.Equal(fixed) {
		t.Fatalf("run sweep now = %v, want %v", fs.runNow.Time, fixed)
	}
	if fs.runGlobalTimeoutSeconds != 7200 {
		t.Fatalf("run global timeout = %d, want 7200", fs.runGlobalTimeoutSeconds)
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

// A follow_up posted to the generic /inputs path against a CHAT run is rejected at
// the service boundary (ErrChatInputNotAllowed) and no row is ever written: chat
// turns must ride SubmitChatMessage so the CHAT_MAX_TURNS ceiling is enforced.
// Accepting a follow_up here would burn spend past the cap.
func TestSubmitInputChatFollowUpRejected(t *testing.T) {
	user := uuid.New()
	runID := uuid.New()
	fs := &fakeStore{runByID: store.Run{ID: runID, UserID: user, Status: "running", Kind: RunKindChat}}
	svc := New(fs, newBox(t), testParams())

	_, err := svc.SubmitInput(context.Background(), user, runID, "follow_up", "keep going", nil)
	if !errors.Is(err, ErrChatInputNotAllowed) {
		t.Fatalf("err = %v, want ErrChatInputNotAllowed", err)
	}
	if fs.createdInput != nil {
		t.Fatalf("no input row must be written for a chat follow_up, got %+v", fs.createdInput)
	}
}

// A follow_up on a normal ISSUE run is untouched by the chat guard: it still enqueues
// a plain input row (regression guard for the guard's kind/Kind condition).
func TestSubmitInputFollowUpIssueRunEnqueues(t *testing.T) {
	user := uuid.New()
	runID := uuid.New()
	fs := &fakeStore{runByID: store.Run{ID: runID, UserID: user, Status: "running", Kind: RunKindIssue}}
	svc := New(fs, newBox(t), testParams())

	if _, err := svc.SubmitInput(context.Background(), user, runID, "follow_up", "use pgx", nil); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if fs.createdInput == nil || fs.createdInput.Kind != "follow_up" {
		t.Fatalf("follow_up on an issue run must enqueue a plain input: %+v", fs.createdInput)
	}
}

// The chat guard is scoped to follow_up only: a cancel on a chat run (the path EndChat
// rides) must NOT be rejected, so the conversation can still be ended.
func TestSubmitInputChatCancelAllowed(t *testing.T) {
	user := uuid.New()
	runID := uuid.New()
	fs := &fakeStore{runByID: store.Run{ID: runID, UserID: user, Status: "queued", Kind: RunKindChat}}
	svc := New(fs, newBox(t), testParams())

	res, err := svc.SubmitInput(context.Background(), user, runID, "cancel", "", nil)
	if err != nil {
		t.Fatalf("cancel on a chat run must not be rejected: %v", err)
	}
	if !res.ServerSide {
		t.Fatal("a cancel on a queued chat run (no poller) is applied server-side")
	}
	if fs.cancelled == nil {
		t.Fatal("CancelRunServerSide not called for a chat cancel")
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

// A revise_plan is a plain enqueue (PRD #41): it goes through the atomic capped-enqueue
// query with the feedback body and never stamps a stop signal, so it is NOT a
// deliberate-stop verdict like cancel/reject_plan, and it never takes the plain
// CreateRunInput path (which has no cap).
func TestSubmitInputRevisePlanEnqueuesPlain(t *testing.T) {
	user := uuid.New()
	runID := uuid.New()
	fs := &fakeStore{
		runByID:     store.Run{ID: runID, UserID: user, Status: "awaiting_approval"},
		reviseCount: 0,
	}
	svc := New(fs, newBox(t), testParams())

	res, err := svc.SubmitInput(context.Background(), user, runID, "revise_plan", "use pgx not gorm", nil)
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if res.ServerSide {
		t.Fatal("revise_plan is never a server-side transition")
	}
	if fs.reviseCapArg == nil || fs.reviseCapArg.RunID != runID {
		t.Fatalf("revise_plan not enqueued via the capped path: %+v", fs.reviseCapArg)
	}
	if fs.reviseCapArg.Body.String != "use pgx not gorm" {
		t.Fatalf("revise body = %q, want feedback text", fs.reviseCapArg.Body.String)
	}
	if fs.createdInput != nil {
		t.Fatalf("revise_plan must not use the uncapped CreateRunInput path, got %+v", fs.createdInput)
	}
	if fs.createdStopVerdict != nil {
		t.Fatalf("revise_plan must not use the stop-verdict path, got %+v", fs.createdStopVerdict)
	}
}

// The cap counts ALL revise_plan rows (no consumed_at filter): the cap query is
// asked about the run, and a count already at the limit rejects with
// ErrReviseCapReached before any enqueue.
func TestSubmitInputRevisePlanCapReached(t *testing.T) {
	user := uuid.New()
	runID := uuid.New()
	// PlanMaxRevisions is 3; a run already at 3 persisted revisions (consumed or not)
	// is at the cap, so the next revise is rejected.
	fs := &fakeStore{
		runByID:     store.Run{ID: runID, UserID: user, Status: "awaiting_approval"},
		reviseCount: 3,
	}
	svc := New(fs, newBox(t), testParams())

	if _, err := svc.SubmitInput(context.Background(), user, runID, "revise_plan", "again", nil); err != ErrReviseCapReached {
		t.Fatalf("err = %v, want ErrReviseCapReached", err)
	}
	if fs.reviseCapArg == nil || fs.reviseCapArg.RunID != runID {
		t.Fatalf("capped enqueue not scoped to run %s, got %v", runID, fs.reviseCapArg)
	}
	if fs.reviseCapArg.MaxRevisions != 3 {
		t.Fatalf("cap passed to the query = %d, want PlanMaxRevisions 3", fs.reviseCapArg.MaxRevisions)
	}
	if fs.createdInput != nil {
		t.Fatalf("no plain enqueue expected once capped, got %+v", fs.createdInput)
	}
}

// Off-by-one at the cap boundary: with PlanMaxRevisions=3, an existing count of 2
// (the 3rd revise) is accepted, but a count of 3 (the 4th) is rejected.
func TestSubmitInputRevisePlanCapBoundary(t *testing.T) {
	user := uuid.New()
	runID := uuid.New()
	svc := func(count int64) (*fakeStore, *Service) {
		fs := &fakeStore{
			runByID:     store.Run{ID: runID, UserID: user, Status: "awaiting_approval"},
			reviseCount: count,
		}
		return fs, New(fs, newBox(t), testParams())
	}

	// 3rd revise (2 already persisted) → accepted.
	fs3, s3 := svc(2)
	if _, err := s3.SubmitInput(context.Background(), user, runID, "revise_plan", "third", nil); err != nil {
		t.Fatalf("3rd revise should be accepted: %v", err)
	}
	if fs3.reviseCapArg == nil {
		t.Fatal("3rd revise should have enqueued via the capped path")
	}

	// 4th revise (3 already persisted) → rejected.
	_, s4 := svc(3)
	if _, err := s4.SubmitInput(context.Background(), user, runID, "revise_plan", "fourth", nil); err != ErrReviseCapReached {
		t.Fatalf("4th revise err = %v, want ErrReviseCapReached", err)
	}
}

// A revise on a terminal run is blocked by the existing terminal guard (before the
// cap query runs).
func TestSubmitInputRevisePlanRejectsTerminalRun(t *testing.T) {
	user := uuid.New()
	runID := uuid.New()
	fs := &fakeStore{runByID: store.Run{ID: runID, UserID: user, Status: "failed"}}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.SubmitInput(context.Background(), user, runID, "revise_plan", "x", nil); err != ErrRunTerminal {
		t.Fatalf("err = %v, want ErrRunTerminal", err)
	}
}

// -------------------------------------------------------------------------
// Run + worker creation
// -------------------------------------------------------------------------

func TestCreateRunSnapshotsTitleAndRejectsMissingPRDLink(t *testing.T) {
	user, repo := uuid.New(), uuid.New()

	// No PRD link, no bypass → rejected.
	fsNoLink := &fakeStore{issueByID: store.Issue{Title: "T", Labels: prdLabels(), HasPrdLink: false}}
	svc := New(fsNoLink, newBox(t), testParams())
	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "desc", false, nil, nil); err != ErrNoPRDLink {
		t.Fatalf("err = %v, want ErrNoPRDLink", err)
	}

	// Happy path → title snapshotted from the cached issue, description from arg.
	fs := &fakeStore{
		issueByID:       store.Issue{Title: "Real Title", Labels: prdLabels(), HasPrdLink: true},
		createRunResult: store.Run{ID: uuid.New()},
	}
	svc = New(fs, newBox(t), testParams())
	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "the description", false, nil, nil); err != nil {
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
	fsNoLink := &fakeStore{issueByID: store.Issue{Title: "T", Labels: prdLabels(), HasPrdLink: false}}
	svc := New(fsNoLink, newBox(t), testParams())
	if _, err := svc.CreateAutopilotRun(context.Background(), user, repo, 4, "desc", false); err != ErrNoPRDLink {
		t.Fatalf("err = %v, want ErrNoPRDLink (autopilot must enforce the same gate)", err)
	}

	// Happy path → auto_approve set, description snapshotted.
	fs := &fakeStore{
		issueByID:       store.Issue{Title: "Real Title", Labels: prdLabels(), HasPrdLink: true},
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
		issueByID:       store.Issue{Title: "T", Labels: prdLabels(), HasPrdLink: true},
		createRunResult: store.Run{ID: uuid.New()},
	}
	svc = New(fsManual, newBox(t), testParams())
	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "d", false, nil, nil); err != nil {
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
					issueByID:       store.Issue{Title: "T", Labels: prdLabels(), HasPrdLink: tc.hasPRDLink},
					createRunResult: store.Run{ID: uuid.New()},
				}
				svc := New(fs, newBox(t), testParams())

				var err error
				if path == "manual" {
					_, err = svc.CreateRun(context.Background(), user, repo, 4, "desc", tc.allowWithoutPRD, nil, nil)
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
	fs := &fakeStore{issueByID: store.Issue{Title: "T", Labels: prdLabels(), HasPrdLink: true}}
	if _, err := New(fs, newBox(t), testParams()).CreateRun(context.Background(), user, repo, 4, big, false, nil, nil); err != ErrDescriptionTooLarge {
		t.Fatalf("CreateRun err = %v, want ErrDescriptionTooLarge", err)
	}
	if fs.createRunParams != nil {
		t.Fatal("an oversize description must be rejected before CreateRun")
	}

	fsAuto := &fakeStore{issueByID: store.Issue{Title: "T", Labels: prdLabels(), HasPrdLink: true}}
	if _, err := New(fsAuto, newBox(t), testParams()).CreateAutopilotRun(context.Background(), user, repo, 4, big, false); err != ErrDescriptionTooLarge {
		t.Fatalf("CreateAutopilotRun err = %v, want ErrDescriptionTooLarge", err)
	}

	// Exactly at the cap is accepted (boundary).
	ok := strings.Repeat("x", MaxIssueDescriptionBytes)
	fsOK := &fakeStore{issueByID: store.Issue{Title: "T", Labels: prdLabels(), HasPrdLink: true}, createRunResult: store.Run{ID: uuid.New()}}
	if _, err := New(fsOK, newBox(t), testParams()).CreateRun(context.Background(), user, repo, 4, ok, false, nil, nil); err != nil {
		t.Fatalf("a description exactly at the cap must be accepted, got %v", err)
	}
}

func TestCreateRunMapsDuplicateToActiveRunExists(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	fs := &fakeStore{
		issueByID:    store.Issue{Title: "T", Labels: prdLabels(), HasPrdLink: true},
		createRunErr: &pgconn.PgError{Code: "23505"},
	}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "d", false, nil, nil); err != ErrActiveRunExists {
		t.Fatalf("err = %v, want ErrActiveRunExists", err)
	}
}

func TestCreateRunRepoNotOwned(t *testing.T) {
	fs := &fakeStore{repoErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), 4, "d", false, nil, nil); err != ErrRepoNotFound {
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
	_, token, err := svc.CreateWorker(context.Background(), uuid.New(), "laptop", "jvm", "")
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
	if _, _, err := svc.CreateWorker(context.Background(), uuid.New(), "laptop", "", ""); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	if fs.createWorkerParams == nil || fs.createWorkerParams.TemplateDeclared.Valid {
		t.Fatalf("no declared choice must be NULL, got %+v", fs.createWorkerParams.TemplateDeclared)
	}
}

// fakeBroadcaster records the run events the service fans out, to assert the
// persist-then-broadcast contract (dedup on retry, broadcast only on apply).
type fakeBroadcaster struct {
	msgSeqs      []int32
	statuses     []string
	healths      []string
	healthNudges []bool
	inputRuns    []uuid.UUID
}

func (b *fakeBroadcaster) PublishMessage(_ uuid.UUID, seq int32, _, _, _, _ string, _ []byte, _ time.Time) {
	b.msgSeqs = append(b.msgSeqs, seq)
}
func (b *fakeBroadcaster) PublishState(_ uuid.UUID, status string) {
	b.statuses = append(b.statuses, status)
}
func (b *fakeBroadcaster) PublishHealth(_ uuid.UUID, health, _ string, nudge bool) {
	b.healths = append(b.healths, health)
	b.healthNudges = append(b.healthNudges, nudge)
}
func (b *fakeBroadcaster) PublishInput(runID uuid.UUID) {
	b.inputRuns = append(b.inputRuns, runID)
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

// TestConsumeInputsBroadcastsOnFollowUp: consuming a batch that includes a follow_up
// fires exactly one input frame for the run (PRD #95 Decision 5) so the browser
// re-reads its steer queue and flips Queued → Delivered.
func TestConsumeInputsBroadcastsOnFollowUp(t *testing.T) {
	w := worker()
	runID := uuid.New()
	fs := &fakeStore{
		runOwned: store.Run{ID: runID, WorkerID: pgUUID(w.ID)},
		consumeRows: []store.ConsumeRunInputsRow{
			{ID: 1, Kind: "follow_up", Body: pgText("do the thing")},
		},
	}
	svc := New(fs, newBox(t), testParams())
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)

	if _, err := svc.ConsumeInputs(context.Background(), w, runID); err != nil {
		t.Fatalf("ConsumeInputs: %v", err)
	}
	if len(b.inputRuns) != 1 || b.inputRuns[0] != runID {
		t.Fatalf("input broadcast = %v, want [%s] (one frame for the run)", b.inputRuns, runID)
	}
}

// TestConsumeInputsNoBroadcastWithoutFollowUp: a consume of only non-follow_up inputs
// (approve_plan/cancel/reject own their own UI, never the queue) fires NO input frame,
// and an empty consume fires none either.
func TestConsumeInputsNoBroadcastWithoutFollowUp(t *testing.T) {
	w := worker()
	runID := uuid.New()
	fs := &fakeStore{
		runOwned: store.Run{ID: runID, WorkerID: pgUUID(w.ID)},
		consumeRows: []store.ConsumeRunInputsRow{
			{ID: 1, Kind: "approve_plan"},
		},
	}
	svc := New(fs, newBox(t), testParams())
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)

	if _, err := svc.ConsumeInputs(context.Background(), w, runID); err != nil {
		t.Fatalf("ConsumeInputs: %v", err)
	}
	if len(b.inputRuns) != 0 {
		t.Fatalf("input broadcast = %v, want none (approve_plan is not a queue entry)", b.inputRuns)
	}
}

// TestConsumeInputsNilBroadcasterFollowUp: ConsumeInputs never broadcast before PRD #95,
// so a service with no broadcaster (the common test/deploy case) must consume a follow_up
// without panicking — the nil-guard mirrors AppendMessages.
func TestConsumeInputsNilBroadcasterFollowUp(t *testing.T) {
	w := worker()
	runID := uuid.New()
	fs := &fakeStore{
		runOwned: store.Run{ID: runID, WorkerID: pgUUID(w.ID)},
		consumeRows: []store.ConsumeRunInputsRow{
			{ID: 1, Kind: "follow_up", Body: pgText("resume")},
		},
	}
	svc := New(fs, newBox(t), testParams()) // no SetBroadcaster → s.bcast is nil
	if _, err := svc.ConsumeInputs(context.Background(), w, runID); err != nil {
		t.Fatalf("ConsumeInputs with nil broadcaster: %v", err)
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

	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "desc", false, nil, nil); err != nil {
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

	if _, err := svc.CreateRun(context.Background(), user, repo, 4, "desc", false, nil, nil); err != nil {
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

// -------------------------------------------------------------------------
// Worker → token binding (PRD #104 M3)
// -------------------------------------------------------------------------

// TestClaimRebindChangesCredentialWithoutRestart is M3's headline acceptance
// criterion: flipping a worker's binding changes the credential in its NEXT claim
// payload, with no container restart and no re-minted join token.
//
// It is expressible as a test at all only because of how uzi delivers the token:
// the worker never holds it, the API decrypts it per claim and ships it in the
// claim response. So "no restart needed" is not a timing claim to be measured —
// it is the structural fact that the same worker row, with the same token_hash,
// claims twice and gets two different credentials because resolution happens on
// the server at claim time. The test drives exactly that: one worker identity,
// three claims, three bindings.
func TestClaimRebindChangesCredentialWithoutRestart(t *testing.T) {
	const defaultToken = "anthropic-DEFAULT-abcdef1234567890" //gitleaks:allow // fake Anthropic token fixture: sealed into a test box below to prove claim-time credential resolution, never a real secret
	const consoleToken = "anthropic-CONSOLE-abcdef1234567890"

	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("bot-pat-REBIND-abcdef1234567890"))
	sealedDefault, _ := box.Seal([]byte(defaultToken))
	sealedConsole, _ := box.Seal([]byte(consoleToken))

	owner := uuid.New()
	consoleID := uuid.New()
	fs := &fakeStore{
		claimRun: store.Run{
			ID: uuid.New(), UserID: owner, IssueIid: pgtype.Int8{Int64: 9, Valid: true},
			IssueTitle: "Do the thing", IssueDescription: "d", Status: "claimed",
		},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/g/p", RepoPath: "g/p",
			DefaultBranch: pgText("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		// The by-kind row is the owner's DEFAULT token; the by-id map holds the
		// specific credential a bound worker resolves.
		anthropic: sealedDefault,
		byIDSecrets: map[uuid.UUID]store.GetUserSecretCiphertextByIDRow{
			consoleID: {
				UserID: owner, Kind: store.KindAnthropicToken,
				Ciphertext: sealedConsole, SealedWith: store.SealedWithMaster,
			},
		},
	}
	svc := New(fs, box, testParams())

	// ONE worker identity throughout: same id, same owner, same join-token hash.
	// Only the binding column changes between claims.
	wkr := store.Worker{ID: uuid.New(), UserID: owner, TokenHash: []byte{0xde, 0xad}}

	claimToken := func(t *testing.T, w store.Worker) string {
		t.Helper()
		payload, err := svc.Claim(context.Background(), w)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if payload == nil {
			t.Fatal("expected a payload, got idle")
		}
		return payload.Secrets.AnthropicOAuthToken
	}

	// 1. Unbound → the owner's default.
	if got := claimToken(t, wkr); got != defaultToken {
		t.Fatalf("unbound worker claimed %q, want the owner's default token", got)
	}

	// 2. Bound to console-key → that credential, same worker row. Since PRD #111 M3
	// the MODE is what makes the id readable at all, so a rebind sets both — which is
	// exactly what SetWorkerAnthropicToken writes in one statement.
	wkr.AnthropicBindMode = BindModePinned
	wkr.AnthropicSecretID = pgtype.UUID{Bytes: consoleID, Valid: true}
	if got := claimToken(t, wkr); got != consoleToken {
		t.Fatalf("bound worker claimed %q, want the console-key token — a rebind did not reach the claim payload", got)
	}
	// WHICH id the second claim opened, not HOW MANY opens happened. The count was
	// the right assertion while the default was resolved inside secretopen's by-KIND
	// query — a by-id lookup could then only mean "a binding was followed". Since
	// PRD #111 D8 every open goes by id (the default included, so its id can be
	// recorded on the run), so this claim is lookup #2 and a count says nothing. The
	// id does.
	if len(fs.byIDLookups) != 2 {
		t.Fatalf("by-id lookups = %d, want 2 (the default claim, then the bound one)", len(fs.byIDLookups))
	}
	// The lookup must be scoped to the claiming worker's OWNER, not to some
	// caller-supplied user: this is what stops a worker row carrying a foreign
	// secret id from resolving that credential (D11).
	if fs.byIDLookups[1].UserID != owner || fs.byIDLookups[1].ID != consoleID {
		t.Fatalf("by-id lookup was (%v,%v), want (%v,%v)",
			fs.byIDLookups[1].ID, fs.byIDLookups[1].UserID, consoleID, owner)
	}
	// The first lookup's ID is ENTAILED by step 1's token assertion, not independent
	// of it: the fake serves the default plaintext only when arg.ID ==
	// f.defaultCredID(), so a passing step 1 already implies this. Kept as a
	// positional statement of the expected order, and labelled as such rather than
	// presented as a second guard.
	if fs.byIDLookups[0].ID != fs.defaultCredID() {
		t.Fatalf("the unbound claim opened %v, want the owner's default %v", fs.byIDLookups[0].ID, fs.defaultCredID())
	}
	// Its USER is not entailed, and this is the assertion that earns its place. The
	// fake's default-credential fallback echoes back whatever arg.UserID it was
	// given, and secretopen.OpenByID's owner re-check then compares that echo against
	// itself — so an openAnthropic that passed the WRONG user would still hand back
	// the right plaintext and step 1 would pass. Only this catches it.
	if fs.byIDLookups[0].UserID != owner {
		t.Fatalf("the unbound claim's open was scoped to %v, want the run owner %v", fs.byIDLookups[0].UserID, owner)
	}

	// 3. Unbound again → back to the default, no restart in between. Both fields,
	// because that is what a real unbind writes: leaving mode=pinned with a NULL id
	// would exercise D9's FALLBACK (a deleted token) rather than a deliberate
	// unbind, and those are different facts that happen to resolve the same way.
	wkr.AnthropicBindMode = BindModeDefault
	wkr.AnthropicSecretID = pgtype.UUID{}
	if got := claimToken(t, wkr); got != defaultToken {
		t.Fatalf("after clearing the binding the worker claimed %q, want the default again", got)
	}

	// The worker's identity never changed across the three claims — no re-mint.
	if string(wkr.TokenHash) != string([]byte{0xde, 0xad}) {
		t.Fatal("the worker's join token hash changed; a rebind must never re-mint it")
	}
}

// TestClaimBoundToVanishedSecretFailsClosed: a binding that no longer resolves
// must FAIL the run with the credential-unavailable reason, never silently fall
// back to the default. A silent fallback is the R4 failure mode — it would spend
// the wrong account and report success.
func TestClaimBoundToVanishedSecretFailsClosed(t *testing.T) {
	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("bot-pat-VANISH-abcdef1234567890"))
	sealedDefault, _ := box.Seal([]byte("anthropic-DEFAULT-vanish-abcdef12345"))

	fs := &fakeStore{
		claimRun: store.Run{
			ID: uuid.New(), IssueIid: pgtype.Int8{Int64: 3, Valid: true},
			IssueTitle: "t", IssueDescription: "d", Status: "claimed",
		},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/g/p", RepoPath: "g/p",
			DefaultBranch: pgText("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic:   sealedDefault,
		byIDSecrets: map[uuid.UUID]store.GetUserSecretCiphertextByIDRow{}, // the bound id resolves to nothing
	}
	svc := New(fs, box, testParams())

	wkr := worker()
	wkr.AnthropicBindMode = BindModePinned
	wkr.AnthropicSecretID = pgtype.UUID{Bytes: uuid.New(), Valid: true}

	payload, err := svc.Claim(context.Background(), wkr)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload != nil {
		t.Fatal("a claim whose bound credential does not resolve must not produce a payload — " +
			"silently falling back to the default would spend the wrong account with no error")
	}
	if fs.markedFailed == nil {
		t.Fatal("the run should have been failed with credential-unavailable")
	}
}

// TestJudgeClaimIgnoresWorkerBinding pins D1's boundary: a judge run claimed by a
// BOUND worker still spends the owner's default, because the judge lane is bound
// per user (M4), not per worker. Getting this wrong would silently bill
// retrospectives to whichever worker happened to pick them up.
func TestJudgeClaimIgnoresWorkerBinding(t *testing.T) {
	const defaultToken = "anthropic-JUDGEDEFAULT-abcdef1234567" //gitleaks:allow // fake Anthropic token fixture: sealed into a test box below to prove the judge lane spends the owner's default, never a real secret

	box := newBox(t)
	sealedDefault, _ := box.Seal([]byte(defaultToken))
	sealedConsole, _ := box.Seal([]byte("anthropic-JUDGECONSOLE-abcdef123456"))

	owner, consoleID := uuid.New(), uuid.New()
	fs := &fakeStore{
		claimRun: store.Run{
			ID: uuid.New(), Kind: RunKindJudge, Status: "claimed",
			IssueTitle: "judge", IssueDescription: "d", UserID: owner,
		},
		anthropic: sealedDefault,
		byIDSecrets: map[uuid.UUID]store.GetUserSecretCiphertextByIDRow{
			consoleID: {UserID: owner, Kind: store.KindAnthropicToken, Ciphertext: sealedConsole, SealedWith: store.SealedWithMaster},
		},
	}
	svc := New(fs, box, testParams())

	wkr := store.Worker{
		ID: uuid.New(), UserID: owner,
		AnthropicBindMode: BindModePinned,
		AnthropicSecretID: pgtype.UUID{Bytes: consoleID, Valid: true},
	}
	payload, err := svc.Claim(context.Background(), wkr)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a judge payload, got idle")
	}
	if payload.Secrets.AnthropicOAuthToken != defaultToken {
		t.Fatalf("judge claim spent %q, want the owner's DEFAULT — the judge lane binds per user (D1), not per worker",
			payload.Secrets.AnthropicOAuthToken)
	}
	// It resolved the DEFAULT, not the worker's binding. Stated as an id rather than
	// as "zero by-id lookups", which is what this asserted before PRD #111 D8 made
	// the default path open by id as well — that phrasing now reads as "the claim
	// opened nothing at all", which is not what the judge lane does.
	//
	// Split into its two halves because they are not equally strong, and the
	// composite `||` hid that. The COUNT is the independent half: a second lookup
	// means the claim also opened the worker's binding, and nothing above would
	// notice. The ID half is entailed by the token assertion above — the fake serves
	// the default plaintext only for f.defaultCredID() — so it is a positional
	// restatement, not a second guard.
	if len(fs.byIDLookups) != 1 {
		t.Fatalf("judge claim made %d by-id opens, want exactly 1 — a second means it also opened the worker's binding (%v): %+v",
			len(fs.byIDLookups), consoleID, fs.byIDLookups)
	}
	if fs.byIDLookups[0].ID != fs.defaultCredID() {
		t.Fatalf("judge claim opened %v, want the owner's default (%v)", fs.byIDLookups[0].ID, fs.defaultCredID())
	}
	// Independent, for the reason spelled out in TestClaimRebindChangesCredential…:
	// the fake's default fallback echoes arg.UserID back, so a wrong-user open still
	// yields the right plaintext and passes every assertion above.
	if fs.byIDLookups[0].UserID != owner {
		t.Fatalf("judge claim's open was scoped to %v, want the run owner %v", fs.byIDLookups[0].UserID, owner)
	}
	if fs.claimCtxCalled {
		t.Fatal("judge claim must not touch the repo/forge claim context")
	}
}

// -------------------------------------------------------------------------
// Judge-lane → token binding (PRD #104 M4)
// -------------------------------------------------------------------------

// TestJudgeClaimUsesJudgeBinding: a judge run spends the OWNER's judge-bound
// credential, not their default and not the claiming worker's binding. This is the
// milestone's whole point — retrospectives billed to a different account from the
// runs they review.
func TestJudgeClaimUsesJudgeBinding(t *testing.T) {
	const defaultToken = "anthropic-DEFAULT-judgebind-abcdef12" //gitleaks:allow // fake Anthropic token fixture: the credential this test asserts is NOT spent, never a real secret
	const judgeToken = "anthropic-JUDGEKEY-judgebind-abcdef1"   //gitleaks:allow // fake Anthropic token fixture: the judge-bound credential this test asserts IS spent, never a real secret

	box := newBox(t)
	sealedDefault, _ := box.Seal([]byte(defaultToken))
	sealedJudge, _ := box.Seal([]byte(judgeToken))

	owner := uuid.New()
	judgeID, workerBoundID := uuid.New(), uuid.New()
	fs := &fakeStore{
		claimRun: store.Run{
			ID: uuid.New(), Kind: RunKindJudge, Status: "claimed",
			IssueTitle: "judge", IssueDescription: "d", UserID: owner,
		},
		anthropic:   sealedDefault,
		judgeSecret: pgtype.UUID{Bytes: judgeID, Valid: true},
		byIDSecrets: map[uuid.UUID]store.GetUserSecretCiphertextByIDRow{
			judgeID: {UserID: owner, Kind: store.KindAnthropicToken, Ciphertext: sealedJudge, SealedWith: store.SealedWithMaster},
		},
	}
	svc := New(fs, box, testParams())

	// The claiming worker is bound to something ELSE entirely, to prove the judge
	// lane ignores it (D1).
	wkr := store.Worker{
		ID: uuid.New(), UserID: owner,
		AnthropicBindMode: BindModePinned,
		AnthropicSecretID: pgtype.UUID{Bytes: workerBoundID, Valid: true},
	}
	payload, err := svc.Claim(context.Background(), wkr)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a judge payload, got idle")
	}
	if payload.Secrets.AnthropicOAuthToken != judgeToken {
		t.Fatalf("judge claim spent %q, want the owner's JUDGE-bound token", payload.Secrets.AnthropicOAuthToken)
	}
	if len(fs.byIDLookups) != 1 || fs.byIDLookups[0].ID != judgeID {
		t.Fatalf("by-id lookups = %+v, want exactly the judge binding (%v)", fs.byIDLookups, judgeID)
	}
	// Owner-scoped, so a judge binding can never resolve someone else's credential.
	if fs.byIDLookups[0].UserID != owner {
		t.Fatalf("judge lookup scoped to %v, want the run owner %v", fs.byIDLookups[0].UserID, owner)
	}
}

// TestJudgeClaimUnboundUsesDefault: the unbound case is every user's state until
// they choose otherwise, and must keep resolving the OWNER'S DEFAULT.
//
// The last clause used to read "with no by-id lookup", which D8 made false in the
// same commit that added an inline comment 22 lines below saying so. Since D8 the
// default is resolved to an id and opened BY id like everything else, so "no by-id
// lookup" now describes a claim that opened nothing at all.
func TestJudgeClaimUnboundUsesDefault(t *testing.T) {
	const defaultToken = "anthropic-DEFAULT-unbound-abcdef1234" //gitleaks:allow // fake Anthropic token fixture: sealed into a test box below to prove an unbound judge claim falls back to the default, never a real secret
	box := newBox(t)
	sealedDefault, _ := box.Seal([]byte(defaultToken))

	owner := uuid.New()
	fs := &fakeStore{
		claimRun: store.Run{
			ID: uuid.New(), Kind: RunKindJudge, Status: "claimed",
			IssueTitle: "judge", IssueDescription: "d", UserID: owner,
		},
		anthropic:   sealedDefault,
		judgeSecret: pgtype.UUID{}, // unbound
	}
	svc := New(fs, box, testParams())

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: owner})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload == nil || payload.Secrets.AnthropicOAuthToken != defaultToken {
		t.Fatalf("unbound judge claim did not resolve the default: %+v", payload)
	}
	// Same restatement as above: since PRD #111 D8 the default is opened BY ID, so
	// "no by-id lookup" no longer means "resolved the default" — naming the id does.
	// Split for the same reason as its twin in TestJudgeClaimIgnoresWorkerBinding: the
	// count is independent, the id is entailed by the payload assertion above, and the
	// user is independent (the fake's default fallback echoes back whatever user it
	// was asked for).
	if len(fs.byIDLookups) != 1 {
		t.Fatalf("unbound judge claim made %d by-id opens, want exactly 1: %+v", len(fs.byIDLookups), fs.byIDLookups)
	}
	if fs.byIDLookups[0].ID != fs.defaultCredID() {
		t.Fatalf("unbound judge claim opened %v, want the owner's default (%v)", fs.byIDLookups[0].ID, fs.defaultCredID())
	}
	if fs.byIDLookups[0].UserID != owner {
		t.Fatalf("unbound judge claim's open was scoped to %v, want the run owner %v", fs.byIDLookups[0].UserID, owner)
	}
}

// TestJudgeBindingLookupErrorFailsClaim: a failed read of the binding must NOT be
// treated as "unbound". Falling back to the default there would spend the wrong
// account on every retrospective while a transient DB fault lasted, and nothing
// would report it — R4's failure mode exactly.
func TestJudgeBindingLookupErrorFailsClaim(t *testing.T) {
	box := newBox(t)
	sealedDefault, _ := box.Seal([]byte("anthropic-DEFAULT-lookuperr-abcdef12"))
	fs := &fakeStore{
		claimRun: store.Run{
			ID: uuid.New(), Kind: RunKindJudge, Status: "claimed",
			IssueTitle: "judge", IssueDescription: "d", UserID: uuid.New(),
		},
		anthropic:      sealedDefault,
		judgeSecretErr: errors.New("db down"),
	}
	svc := New(fs, box, testParams())

	_, err := svc.Claim(context.Background(), worker())
	if err == nil {
		t.Fatal("a failed judge-binding lookup must fail the claim, never silently fall back to the default")
	}
	if !strings.Contains(err.Error(), "judge token binding lookup") {
		t.Fatalf("error should name the binding lookup, got: %v", err)
	}
}

// TestJudgeBoundToVanishedSecretFailsClosed: same rule as M3's worker binding — a
// judge binding that no longer resolves fails the run rather than falling back.
func TestJudgeBoundToVanishedSecretFailsClosed(t *testing.T) {
	box := newBox(t)
	sealedDefault, _ := box.Seal([]byte("anthropic-DEFAULT-vanish-judge-abcd"))
	owner := uuid.New()
	fs := &fakeStore{
		claimRun: store.Run{
			ID: uuid.New(), Kind: RunKindJudge, Status: "claimed",
			IssueTitle: "judge", IssueDescription: "d", UserID: owner,
		},
		anthropic:   sealedDefault,
		judgeSecret: pgtype.UUID{Bytes: uuid.New(), Valid: true},
		byIDSecrets: map[uuid.UUID]store.GetUserSecretCiphertextByIDRow{}, // resolves to nothing
	}
	svc := New(fs, box, testParams())

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: owner})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload != nil {
		t.Fatal("a judge claim whose bound credential does not resolve must not produce a payload")
	}
	if fs.markedFailed == nil {
		t.Fatal("the judge run should have been failed with credential-unavailable")
	}
}

// TestSelfImproveClaimFollowsJudgeBinding pins the branch that makes the PRD's
// "self-improve runs follow the judge binding" true. A self_improve run is repo-ful
// and rides the ORDINARY run lane, not assembleJudgeClaim, so without an explicit
// branch it would silently follow the claiming worker's binding instead.
func TestSelfImproveClaimFollowsJudgeBinding(t *testing.T) {
	const judgeToken = "anthropic-JUDGEKEY-selfimprove-abcd"

	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("bot-pat-SELFIMPROVE-abcdef1234567890"))
	sealedDefault, _ := box.Seal([]byte("anthropic-DEFAULT-selfimprove-abcde"))
	sealedJudge, _ := box.Seal([]byte(judgeToken))

	owner := uuid.New()
	judgeID, workerID := uuid.New(), uuid.New()
	fs := &fakeStore{
		claimRun: store.Run{
			ID: uuid.New(), UserID: owner, Kind: RunKindSelfImprove, Status: "claimed",
			IssueIid: pgtype.Int8{Int64: 1, Valid: true}, IssueTitle: "improve uzi", IssueDescription: "d",
		},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/g/uzi", RepoPath: "g/uzi",
			DefaultBranch: pgText("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic:   sealedDefault,
		judgeSecret: pgtype.UUID{Bytes: judgeID, Valid: true},
		byIDSecrets: map[uuid.UUID]store.GetUserSecretCiphertextByIDRow{
			judgeID:  {UserID: owner, Kind: store.KindAnthropicToken, Ciphertext: sealedJudge, SealedWith: store.SealedWithMaster},
			workerID: {UserID: owner, Kind: store.KindAnthropicToken, Ciphertext: sealedDefault, SealedWith: store.SealedWithMaster},
		},
	}
	svc := New(fs, box, testParams())

	// Claimed by a worker bound to a DIFFERENT credential: the judge binding wins.
	wkr := store.Worker{
		ID: uuid.New(), UserID: owner,
		AnthropicBindMode: BindModePinned,
		AnthropicSecretID: pgtype.UUID{Bytes: workerID, Valid: true},
	}
	payload, err := svc.Claim(context.Background(), wkr)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a self_improve payload, got idle")
	}
	if payload.Secrets.AnthropicOAuthToken != judgeToken {
		t.Fatalf("self_improve claim spent %q, want the JUDGE-bound token — it rides the run lane, "+
			"so this only holds because claimSecretID branches on the kind", payload.Secrets.AnthropicOAuthToken)
	}
	// It is still a repo-ful run-lane claim, not a judge claim.
	if !fs.claimCtxCalled {
		t.Fatal("a self_improve claim must still take the ordinary repo-ful claim path")
	}
}

// runWithRepo builds an owned run whose repo scope is valid, the precondition
// SaveMemory requires past the repo-less 409 gate.
func memRunWithRepo(w store.Worker) store.Run {
	return store.Run{ID: uuid.New(), UserID: w.UserID, WorkerID: pgUUID(w.ID), RepoID: pgUUID(uuid.New())}
}

// TestSaveMemoryCapsEvidenceServerSide proves the evidence byte cap is enforced in
// the SERVICE, not just the client (PRD #266): a direct worker POST that skips the
// client's own 200-byte cap must still be rejected with ErrMemoryTooLarge, before
// the insert is reached. The boundary (exactly the cap) is accepted.
func TestSaveMemoryCapsEvidenceServerSide(t *testing.T) {
	w := worker()

	// One byte over the cap → rejected, no insert.
	fs := &fakeStore{runOwned: memRunWithRepo(w)}
	svc := New(fs, newBox(t), testParams())
	over := strings.Repeat("x", MemoryMaxEvidenceBytes+1)
	if _, err := svc.SaveMemory(context.Background(), w, fs.runOwned.ID, "t", "b", "observed", over); err != ErrMemoryTooLarge {
		t.Fatalf("SaveMemory(evidence %d bytes) err = %v, want ErrMemoryTooLarge", len(over), err)
	}
	if fs.memInserted {
		t.Fatalf("oversize evidence must be rejected BEFORE the insert, but InsertAgentMemory was reached")
	}

	// Exactly the cap → accepted.
	fs = &fakeStore{runOwned: memRunWithRepo(w)}
	svc = New(fs, newBox(t), testParams())
	atCap := strings.Repeat("x", MemoryMaxEvidenceBytes)
	if _, err := svc.SaveMemory(context.Background(), w, fs.runOwned.ID, "t", "b", "observed", atCap); err != nil {
		t.Fatalf("SaveMemory(evidence exactly %d bytes) err = %v, want nil", len(atCap), err)
	}
	if !fs.memInserted || fs.memInsertParams == nil {
		t.Fatalf("evidence at the cap must be accepted and inserted")
	}
	if fs.memInsertParams.Evidence.String != atCap {
		t.Errorf("stored evidence = %q, want the at-cap value", fs.memInsertParams.Evidence.String)
	}
}

// TestSaveMemoryTrimsAndSanitizesEvidence proves evidence is TrimSpace'd and
// single-line sanitized (keepWhitespace=false) at write (PRD #266): whitespace-only
// evidence stores NULL, and an embedded newline/tab is dropped so it cannot forge a
// fake marker line where the lead prompt renders evidence inline.
func TestSaveMemoryTrimsAndSanitizesEvidence(t *testing.T) {
	w := worker()

	// Whitespace-only evidence → stored NULL.
	fs := &fakeStore{runOwned: memRunWithRepo(w)}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.SaveMemory(context.Background(), w, fs.runOwned.ID, "t", "b", "observed", "  \n\t "); err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	if fs.memInsertParams == nil || fs.memInsertParams.Evidence.Valid {
		t.Errorf("whitespace-only evidence must store NULL, got %+v", fs.memInsertParams.Evidence)
	}

	// Embedded newline/tab dropped; surrounding whitespace trimmed.
	fs = &fakeStore{runOwned: memRunWithRepo(w)}
	svc = New(fs, newBox(t), testParams())
	if _, err := svc.SaveMemory(context.Background(), w, fs.runOwned.ID, "t", "b", "observed", "  see\nfile\ttail  "); err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	if got, want := fs.memInsertParams.Evidence.String, "seefiletail"; got != want {
		t.Errorf("stored evidence = %q, want %q (trimmed + newline/tab dropped)", got, want)
	}
}

// TestSaveMemoryNormalizesBasisAtWrite proves an unknown/garbage/oversized basis is
// coerced to empty (→ NULL) at WRITE (PRD #266), closing the basis-amplification
// path so no untrusted basis string is ever persisted; a known label passes through.
func TestSaveMemoryNormalizesBasisAtWrite(t *testing.T) {
	w := worker()
	for _, tc := range []struct {
		name     string
		in       string
		wantNull bool
		want     string
	}{
		{"unknown", "bogus", true, ""},
		{"wrong case", "OBSERVED", true, ""},
		{"oversized garbage", strings.Repeat("z", 5000), true, ""},
		{"observed", "observed", false, "observed"},
		{"inferred", "inferred", false, "inferred"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{runOwned: memRunWithRepo(w)}
			svc := New(fs, newBox(t), testParams())
			if _, err := svc.SaveMemory(context.Background(), w, fs.runOwned.ID, "t", "b", tc.in, ""); err != nil {
				t.Fatalf("SaveMemory: %v (a bad basis must never fail the write)", err)
			}
			if fs.memInsertParams == nil {
				t.Fatalf("insert not reached")
			}
			if tc.wantNull {
				if fs.memInsertParams.Basis.Valid {
					t.Errorf("basis %q must store NULL, got %q", tc.in, fs.memInsertParams.Basis.String)
				}
			} else if fs.memInsertParams.Basis.String != tc.want {
				t.Errorf("basis %q stored as %q, want %q", tc.in, fs.memInsertParams.Basis.String, tc.want)
			}
		})
	}
}
