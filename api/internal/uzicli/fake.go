package uzicli

import (
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// FakeClient is an in-memory Client for command tests. It makes no network
// calls: set the canned fields, or set Err to have every method fail with it.
// GetRun returns an ExitNotFound error when the id is absent.
//
// RunReview mirrors the live client's tri-state (Reviews is keyed by run id):
//   - key present, non-nil value → the judged review
//   - key present, nil value     → a visible-but-unjudged run: (nil, nil), exit 0
//   - key absent                 → a real 404: ExitNotFound, exit 4
//
// so a test can exercise the "not judged" (nil) path WITHOUT reusing the 404
// path — the two are deliberately distinct (PRD #64 M7).
//
// PendingJudges (PRD #119) is a SEPARATE map on the same run ids, and it does not
// participate in that tri-state: only Reviews decides 404-vs-found, exactly as
// before, because the server answers both keys off one visibility gate. Its own
// two cases are:
//   - key present, non-nil value → an active judge for that run ("scheduled"/"running")
//   - key present-nil or absent  → no judge in flight
//
// Both spellings of "no pending judge" are the same reply, deliberately: the server
// sends `"pending_judge": null` and a pre-#119 one omits the key, and the CLI must
// render those identically. A test that wants a pending judge over a run with NO
// review still needs the nil Reviews entry — otherwise it is asking about a 404.
type FakeClient struct {
	User           apitypes.UserDTO
	Runs           []apitypes.RunListItemDTO
	RunByID        map[string]apitypes.RunDTO
	LogsByID       map[string][]apitypes.MessageDTO
	InputsByID     map[string][]apitypes.SteerInputDTO
	Reviews        map[string]*apitypes.ReviewDTO
	PendingJudges  map[string]*apitypes.PendingJudgeDTO
	Workers        []apitypes.WorkerDTO
	Repos          []apitypes.RepoDTO
	AdminUsers     []apitypes.UserDTO
	AdminRuns      []apitypes.RunListItemDTO
	AdminWorkers   []apitypes.AdminWorkerDTO
	AdminCLITokens []apitypes.AdminCLITokenDTO
	AdminUsageV    apitypes.AdminUsageDTO
	RateLimits     []apitypes.AdminRateLimitRowDTO
	GuardrailV     apitypes.GuardrailImpactDTO
	BlockedReposV  apitypes.AdminBlockedReposDTO
	AgentSourceV   apitypes.AgentSourceDTO

	// Build is the canned GET /api/version reply; BuildErr fails that one call
	// without failing the rest (see BuildInfo). BuildInfoCalls counts the calls —
	// no mutex, like the rest of this fake, because every consumer is a
	// single-goroutine command test.
	Build          apitypes.BuildInfoDTO
	BuildErr       error
	BuildInfoCalls int

	// Auth-flow canned replies (uzi login). StartCLIAuth returns AuthStart;
	// PollCLIAuth pops the front of AuthPolls each call (and repeats the last entry
	// once drained), so a test can script start → pending → authorized.
	AuthStart CLIAuthStartResult
	AuthPolls []CLIAuthPollResult

	// Write-verb capture + canned replies. CreateRun returns CreatedRun and records
	// its args; SubmitRunInput returns InputResp and records kind/body/selection, so
	// a test can assert the exact wire mapping of each write verb.
	CreatedRun         apitypes.RunDTO
	LastCreateRepoID   string
	LastCreateIssueIID int64
	// LastCreateWaitOnLimit keeps the POINTER rather than a bool, so a test can tell
	// the three cases apart: nil (the flag was not passed — inherit the user's
	// default), &false and &true. A bool field here would silently collapse "not
	// passed" into "passed false", which is the exact distinction the flag exists to
	// carry, and no test could then catch its loss.
	LastCreateWaitOnLimit *bool
	// LastCreateMrRework keeps the POINTER, for the SAME reason as LastCreateWaitOnLimit
	// (PRD #841 M3): a test must tell nil (flag absent → inherit the account default) from
	// &false and &true, which is exactly the tri-state `--mr-rework` carries.
	LastCreateMrRework *bool
	// LastCreateForce captures the --force flag (issue #856): a plain bool, since force
	// is not tri-state — false means "respect the open-MR guard" (the default), true means
	// "bypass ONLY that guard". A test asserts the CLI forwarded true iff --force was passed.
	LastCreateForce bool
	// LastCreateSeed captures PRD #209's optional seeded plan (nil when the run was
	// created without --plan-file), so a test can assert the plan body and the roster
	// the CLI forwarded — the assertion M3's flag parsing is proven against.
	LastCreateSeed     *CreateRunSeed
	InputResp          apitypes.RunInputResponse
	LastInputRunID     string
	LastInputKind      string
	LastInputBody      string
	LastInputSelection *apitypes.AgentSelection

	// CreateTaskRun / DispatchTaskRun capture (PRD #400 M3). CreatedTaskRun is the
	// canned create reply (its Branch is what the handoff command pushes to);
	// DispatchedRun is the canned dispatch reply. The Last* fields record the exact
	// wire args so a test can assert repo/context/base/mr and the dispatched run id.
	// TaskCalls records the ORDERED sequence of these two verbs (and can be shared
	// with the fake Git recorder) so a test can prove create → push → dispatch
	// ordering, which a per-verb capture alone cannot show. CreateTaskRunErr /
	// DispatchTaskRunErr win over the blanket Err so a test can model a create 422 or
	// a dispatch 404 on the specific verb while the capture still proves it was
	// reached.
	CreatedTaskRun            apitypes.RunDTO
	LastCreateTaskRepoID      string
	LastCreateTaskContext     string
	LastCreateTaskBaseBranch  string
	LastCreateTaskOpenMr      bool
	LastCreateTaskInteractive bool
	LastCreateTaskReview      bool
	LastCreateTaskThenFix     bool
	CreateTaskRunErr          error
	DispatchedRun             apitypes.RunDTO
	LastDispatchRunID         string
	DispatchTaskRunErr        error
	TaskCalls                 []string

	// GetTaskReview capture (PRD #400 M4a). TaskReview is the canned reply (nil ⇒ the
	// task has no review yet); GetTaskReviewErr wins over Err so a 404 can be modelled on
	// this verb; LastTaskReviewID records the requested target run id.
	TaskReview       *apitypes.TaskReviewDTO
	GetTaskReviewErr error
	LastTaskReviewID string

	// DeleteWorker capture: records the id it was asked to delete.
	LastDeletedWorkerID string

	// DeleteRepo capture (PRD #357 M3): records the id `uzi repo remove` asked to
	// delete. It stays empty when the confirm gate declines, which is what proves
	// the gate blocked the call.
	LastDeletedRepoID string

	// SetTokenAutoEligible capture (PRD #111 M2): the secret id the command
	// RESOLVED the label to, and the boolean it sent. The id is what proves the
	// label→id resolution happened client-side against the caller's own list rather
	// than the label being posted to the server.
	LastPoolSecretID string
	LastPoolValue    bool
	PoolSecret       apitypes.SecretDTO

	// SetRunPriority capture (PRD #320 M5): the run id the command targeted and the
	// expedite bool it sent (true = `uzi run expedite`, false = `--clear`). PriorityRun
	// is the canned success reply. SetRunPriorityErr wins over the blanket Err so a test
	// can model the non-queued 409 (ExitConflict) precisely while the capture still proves
	// the write was reached.
	LastPriorityRunID    string
	LastPriorityExpedite bool
	PriorityRun          apitypes.RunDTO
	SetRunPriorityErr    error

	// ResumeRunNow capture (PRD #754 M5): the run id `uzi run resume-now` targeted.
	// ResumedRun is the canned success reply; ResumeRunNowErr wins over the blanket Err so
	// a test can model the non-held 409 (ExitConflict) precisely while the capture still
	// proves the write was reached.
	LastResumeRunID string
	ResumedRun      apitypes.RunDTO
	ResumeRunNowErr error

	// SetRunMrRework capture (PRD #841 M3). LastMrReworkRunID is the run id `uzi run
	// mr-rework` targeted; LastMrReworkEnabled keeps the POINTER so a test can tell the
	// three wire states apart — &true, &false, and nil (--clear → clear to inherit).
	// MrReworkRun is the canned success reply; SetRunMrReworkErr wins over the blanket Err
	// so a test can model a 404 on the write while the capture still proves it was reached.
	LastMrReworkRunID   string
	LastMrReworkEnabled *bool
	MrReworkRun         apitypes.RunDTO
	SetRunMrReworkErr   error

	// SelfMeters drives SelfRateLimits (PRD #111 D23): the caller's own per-token
	// meters, each carrying the server-computed auto-selection status.
	SelfMeters []apitypes.TokenRateLimitDTO

	// Settings drives GetMySettings: the caller's own non-secret settings.
	Settings apitypes.UserSettingsDTO

	// Secrets drives ListSecrets (PRD #104 M2).
	Secrets []apitypes.SecretDTO

	// SetWorkerToken capture (PRD #104 M3): the worker id and the label it was asked
	// to bind. LastSetTokenLabel is "" for the clear-the-binding form, which is the
	// same value the command passes for --default, so the tests assert on both.
	LastSetTokenWorkerID string
	LastSetTokenLabel    string
	// LastSetTokenMode is the PRD #111 M3 bind mode the command sent. Recorded
	// separately from the label because the two are what a caller can get WRONG
	// together — "auto" with a leftover label is the realistic half-updated client.
	LastSetTokenMode string
	SetTokenWorker   apitypes.WorkerDTO

	// Agent memory (PRD #90): ListMemory returns Memories; DeleteMemory records the
	// id it was asked to purge.
	Memories            []apitypes.AgentMemoryDTO
	LastDeletedMemoryID string

	// Disposition triage (PRD #94): SetDisposition/DeleteDisposition record the
	// (run, rec) they were called with and the wire status/reason, so a test can
	// assert the exact mapping. JudgeStatsResult is the canned `stats` reply.
	// SetDispositionErr / DeleteDispositionErr, when set, are returned by the
	// matching write in preference to Err, so a test can model the OWNER-ONLY
	// refusal precisely — a uza_ token READS the review fine (RunReview succeeds)
	// and is refused only on the WRITE (404) — which a global Err cannot express
	// (it would fail the resolve read first). The (run, rec) capture still records,
	// so a test can assert the write was actually REACHED before being refused.
	LastDispositionRunID  string
	LastDispositionRecID  string
	LastDispositionStatus string
	LastDispositionReason string
	JudgeStatsResult      apitypes.TriageDTO
	SetDispositionErr     error
	DeleteDispositionErr  error

	// Judge backlog + bulk group disposition (PRD #98 M7). JudgeBacklogResult is the
	// canned `review backlog` reply and LastBacklogBucket records the bucket the command
	// forwarded (empty = the flag was unset and the parameter omitted, so the SERVER's
	// default applies — the fake must not substitute one, or a test could not tell the
	// two apart).
	//
	// BulkDispositionResult / LastBulk* capture the fan-out. BulkDispositionErr, like
	// SetDispositionErr above, is returned by the WRITE in preference to Err, so a test
	// can model a read-succeeds/write-fails sequence precisely; a global Err would fail
	// whichever call came first and prove nothing about the write.
	JudgeBacklogResult    apitypes.JudgeBacklogDTO
	LastBacklogBucket     string
	LastBacklogRun        string
	LastBacklogCategory   string
	BulkDispositionResult apitypes.JudgeDispositionResultDTO
	LastBulkItems         []apitypes.JudgeDispositionCoordDTO
	LastBulkStatus        string
	LastBulkReason        string
	BulkDispositionErr    error

	// Live stream (PRD #112 M2). StreamEvents is replayed to the subscriber in
	// order; StreamErr models a socket that cannot be opened at all.
	StreamEvents    []apitypes.RunEventDTO
	StreamErr       error
	LastStreamRunID string

	// Run schedules (PRD #241 M6). Schedules drives ListSchedules; ScheduleByID backs
	// GetSchedule (a key absent → ExitNotFound, mirroring the server's owner-scoped
	// 404). The write verbs capture their args so a test can assert the exact wire
	// mapping the command assembled: CreateSchedule records the repo id and the whole
	// ScheduleRequest, SetScheduleEnabled the id + bool (pause vs resume), and the
	// delete/run-now verbs their id.
	Schedules           []apitypes.ScheduleDTO
	ScheduleByID        map[string]apitypes.ScheduleDTO
	CreatedSchedule     apitypes.ScheduleDTO
	LastCreateSchedRepo string
	LastCreateSchedReq  apitypes.ScheduleRequest
	// AllCreateSchedReqs / AllCreateSchedRepos accumulate EVERY CreateSchedule call in order
	// (the Last* fields keep only the most recent). A multi-repo `schedule create` fan-out
	// stamps one shared sibling_group_id across its N create bodies (PRD #636 M4, Decision 4),
	// which is unassertable against a single-value recorder — so a test reads these slices to
	// prove all N reqs carry the same non-nil group id and a single-repo create carries none.
	AllCreateSchedReqs  []apitypes.ScheduleRequest
	AllCreateSchedRepos []string
	EnabledSchedule     apitypes.ScheduleDTO
	LastSchedEnabledID  string
	LastSchedEnabledVal bool
	// PatchSchedule capture (PRD #302): the id and the whole ScheduleRequest the edit
	// verb assembled, so a test can assert the exact wire mapping — including that
	// re-sent fields (max_issues/guidance) survive an unrelated edit and that Enabled
	// is left nil so the pause flag is untouched.
	PatchedSchedule    apitypes.ScheduleDTO
	LastPatchSchedID   string
	LastPatchSchedReq  apitypes.ScheduleRequest
	LastDeletedSchedID string
	RunNowResult       apitypes.RunNowResponse
	LastRunNowSchedID  string

	// Default-schedule catalog + clone (PRD #589 M3). CatalogResult backs
	// ListScheduleCatalog. EnableCatalogSchedule records EACH (repo, slug) it is called
	// with — EnabledCatalogCalls is the ORDERED sequence, so a test can prove the CLI's
	// client-side multi-repo fan-out issued one enable per --repo. Enable/Reset/Clone*
	// return their canned DTO; the Last* captures record the exact wire args (Clone records
	// the id AND the target repo id, empty = clone into the source repo).
	CatalogResult         apitypes.ScheduleCatalogResponse
	EnabledCatalogCalls   []EnableCatalogCall
	EnabledCatalogDTO     apitypes.ScheduleDTO
	EnabledCatalogCreated bool
	ResetScheduleDTO      apitypes.ScheduleDTO
	LastResetSchedID      string
	ClonedSchedule        apitypes.ScheduleDTO
	LastCloneSchedID      string
	LastCloneRepoID       string
	// AddScheduleRepo capture (PRD #636 M4): the source id and target repo id the add-repo
	// verb sent. AddRepoSchedule is the canned sibling DTO returned; AddRepoErr models the
	// 409-duplicate (or any) failure precisely — it wins over the blanket Err so a test can
	// prove the 409-path no-op while the call capture still shows the call was reached.
	AddRepoSchedule    apitypes.ScheduleDTO
	LastAddRepoSchedID string
	LastAddRepoRepoID  string
	AddRepoErr         error

	// Sweep-label guardrail (PRD #589 M4). MissingLabels is the canned CheckRepoLabels
	// reply (the labels reported absent). CheckLabelsCalls / EnsureLabelsCalls record EACH
	// (repo, labels) call in ORDER, so a test can prove `catalog enable` checked every
	// target repo's selector and, with --create-missing-labels, ensured the missing ones
	// BEFORE enabling. CheckLabelsErr / EnsureLabelsErr model a forge failure precisely
	// (they win over the blanket Err) while the call capture still proves the call was made.
	MissingLabels     []string
	CheckLabelsCalls  []LabelCall
	EnsureLabelsCalls []LabelCall
	CheckLabelsErr    error
	EnsureLabelsErr   error

	// Incidental findings (PRD #333 M6). FindingsResult is the canned `findings list` reply,
	// and LastFindings{Bucket,Repo,Run} record the forwarded filters (empty = the flag was
	// unset and the parameter omitted, so the SERVER's default applies — the fake must not
	// substitute one, or a test could not tell the two apart, mirroring LastBacklogBucket).
	//
	// FileFindingResult / LastFileFindingID capture the file write; DismissFinding records its
	// id + reason. FileFindingErr / DismissFindingErr, like SetDispositionErr, are returned by
	// the WRITE in preference to Err so a test can model a 404/409 precisely while the capture
	// still proves the write was REACHED with the right id.
	FindingsResult     apitypes.IncidentalFindingBacklogDTO
	LastFindingsBucket string
	LastFindingsRepo   string
	LastFindingsRun    string

	FileFindingResult apitypes.IncidentalFindingFileResultDTO
	LastFileFindingID string
	FileFindingErr    error

	LastDismissFindingID     string
	LastDismissFindingReason string
	DismissFindingErr        error

	// Review issue filing (PRD #365 M2). ReviewIssueDraft is the canned issue-draft reply;
	// ReviewFileResult / Last... capture the file write. *Err fields win over Err so a test can
	// model a 404/409 on the write while the capture still proves it was reached.
	ReviewIssueDraft       apitypes.IssueDraftDTO
	GetReviewIssueDraftErr error
	LastReviewDraftRunID   string
	LastReviewDraftRecID   string

	ReviewFileResult     ReviewIssueFileResult
	FileReviewIssueErr   error
	LastFileReviewRunID  string
	LastFileReviewRecID  string
	LastFileReviewRepoID string
	LastFileReviewTitle  string
	LastFileReviewDesc   string

	// GitHub project sync CLI reads (PRD #576 M7). ProjectSyncStatusResult is the
	// canned `project-sync status` reply and LastProjectSyncStatusRepoID records the
	// repo id it was asked about. GetProjectSyncStatusErr wins over the blanket Err so
	// a test can model the not-linked 404 (ExitNotFound) precisely while the capture
	// still proves the read was reached. ResyncProjectSync records its repo id;
	// ResyncProjectSyncErr wins over Err the same way for the resync 404.
	ProjectSyncStatusResult     ProjectSyncStatus
	LastProjectSyncStatusRepoID string
	GetProjectSyncStatusErr     error
	LastResyncProjectSyncRepoID string
	ResyncProjectSyncErr        error

	// GetRunHook, when non-nil, drives GetRun instead of the static RunByID map. It
	// is the sequencing seam `uzi run wait`'s tests need: a poll loop calls GetRun
	// repeatedly, so a test scripts a per-call STATUS SEQUENCE (and injects a
	// transient ExitUnreachable that later recovers) by popping a queue from this
	// hook. A nil hook keeps the static lookup, so every existing GetRun test is
	// unaffected.
	GetRunHook func(id string) (apitypes.RunDTO, error)

	// RunLogsHook, when non-nil, drives RunLogs instead of the static LogsByID map,
	// mirroring GetRunHook. It is the sequencing seam `uzi run wait --min-plan-seq`'s
	// tests need: the poll loop reads the logs each cycle, so a test scripts a
	// per-call MESSAGE SEQUENCE (e.g. a stale plan seq first, then a fresh one) by
	// returning different slices across calls. A nil hook keeps the static filter.
	RunLogsHook func(id string, after int32) ([]apitypes.MessageDTO, error)

	// Err, when non-nil, is returned by every method (before any lookup).
	Err error
}

// EnableCatalogCall records one per-repo catalog enable the multi-repo fan-out issued
// (PRD #589 M3), so a CLI test can assert the exact (repo, slug) sequence.
type EnableCatalogCall struct {
	RepoID string
	Slug   string
}

// LabelCall records one CheckRepoLabels / EnsureRepoLabels call (PRD #589 M4): the repo it
// targeted and the labels it was asked about, so a CLI test can assert the guardrail
// checked (and, on confirm, ensured) the right selector on the right repo.
type LabelCall struct {
	RepoID string
	Labels []string
}

var _ Client = (*FakeClient)(nil)
