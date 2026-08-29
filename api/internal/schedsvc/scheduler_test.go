package schedsvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/schedtmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// ── Fakes ────────────────────────────────────────────────────────────────────

type fakeStore struct {
	due []store.RunSchedule

	advanceCalls []store.AdvanceScheduleParams
	statusCalls  []store.SetRunScheduleStatusParams

	repoErr error
	repoRow store.GetRepoForUserRow

	activeIssue    bool
	activeIssueErr error
	// activeByIssue overrides activeIssue per candidate iid (issue #416 backfill tests):
	// when non-nil and the iid is present, HasActiveRunForIssue returns that value; else it
	// falls back to activeIssue/activeIssueErr, so existing single-value tests are unchanged.
	activeByIssue       map[int64]bool
	activeSchedule      bool
	sweepRows           []store.ListSweepCandidateIssuesRow
	sweepLabelParam     []byte
	sweepMaxIssuesParam pgtype.Int4

	// sweepCount is the total returned by CountSweepCandidateIssues (the Capped probe);
	// sweepCountErr forces a transient DB error on it. countLabelParam records the label
	// selector the probe was called with (to prove it matches the list query's).
	sweepCount      int64
	sweepCountErr   error
	sweepCountCalls int
	countLabelParam []byte

	// self_improve fire path (PRD #590 M1).
	activeSelfImprove      int64
	activeSelfImproveErr   error
	activeSelfImproveRepos []uuid.UUID                               // repo ids the per-repo active-run pre-check was called with
	siMRRuns               []store.RecentSelfImproveMRRunsForRepoRow // candidates for the open-MR cap (PRD #686 D10/D12)
	siMRRunsErr            error
	siMRRunsRepos          []uuid.UUID // repo ids the open-MR cap candidate query was called with
	siRecs                 []store.ListOpenImproveUziRecommendationsForUserRow
	siRecsErr              error
	siRecsUserParam        uuid.UUID // the user_id the backlog was scoped to
	markedIDs              []uuid.UUID
	markedByRun            uuid.UUID
	markErr                error
}

func (f *fakeStore) ClaimDueSchedules(context.Context) ([]store.RunSchedule, error) {
	return f.due, nil
}
func (f *fakeStore) AdvanceSchedule(_ context.Context, arg store.AdvanceScheduleParams) (store.RunSchedule, error) {
	f.advanceCalls = append(f.advanceCalls, arg)
	return store.RunSchedule{}, nil
}
func (f *fakeStore) SetRunScheduleStatus(_ context.Context, arg store.SetRunScheduleStatusParams) (store.RunSchedule, error) {
	f.statusCalls = append(f.statusCalls, arg)
	return store.RunSchedule{}, nil
}
func (f *fakeStore) ListSweepCandidateIssues(_ context.Context, arg store.ListSweepCandidateIssuesParams) ([]store.ListSweepCandidateIssuesRow, error) {
	f.sweepLabelParam = arg.Labels
	f.sweepMaxIssuesParam = arg.MaxIssues
	return f.sweepRows, nil
}
func (f *fakeStore) CountSweepCandidateIssues(_ context.Context, arg store.CountSweepCandidateIssuesParams) (int64, error) {
	f.sweepCountCalls++
	f.countLabelParam = arg.Labels
	if f.sweepCountErr != nil {
		return 0, f.sweepCountErr
	}
	return f.sweepCount, nil
}
func (f *fakeStore) HasActiveRunForIssue(_ context.Context, arg store.HasActiveRunForIssueParams) (bool, error) {
	if f.activeByIssue != nil {
		if v, ok := f.activeByIssue[arg.IssueIid.Int64]; ok {
			return v, nil
		}
	}
	return f.activeIssue, f.activeIssueErr
}
func (f *fakeStore) HasActiveRunForSchedule(_ context.Context, _ pgtype.UUID) (bool, error) {
	return f.activeSchedule, nil
}
func (f *fakeStore) GetRepoForUser(_ context.Context, _ store.GetRepoForUserParams) (store.GetRepoForUserRow, error) {
	if f.repoErr != nil {
		return store.GetRepoForUserRow{}, f.repoErr
	}
	return f.repoRow, nil
}
func (f *fakeStore) CountActiveSelfImproveRunsForRepo(_ context.Context, repoID uuid.UUID) (int64, error) {
	f.activeSelfImproveRepos = append(f.activeSelfImproveRepos, repoID)
	return f.activeSelfImprove, f.activeSelfImproveErr
}
func (f *fakeStore) RecentSelfImproveMRRunsForRepo(_ context.Context, arg store.RecentSelfImproveMRRunsForRepoParams) ([]store.RecentSelfImproveMRRunsForRepoRow, error) {
	f.siMRRunsRepos = append(f.siMRRunsRepos, arg.RepoID)
	return f.siMRRuns, f.siMRRunsErr
}
func (f *fakeStore) ListOpenImproveUziRecommendationsForUser(_ context.Context, arg store.ListOpenImproveUziRecommendationsForUserParams) ([]store.ListOpenImproveUziRecommendationsForUserRow, error) {
	f.siRecsUserParam = arg.UserID
	return f.siRecs, f.siRecsErr
}
func (f *fakeStore) MarkImproveUziRecommendationsAddressed(_ context.Context, arg store.MarkImproveUziRecommendationsAddressedParams) (int64, error) {
	if f.markErr != nil {
		return 0, f.markErr
	}
	f.markedIDs = arg.Ids
	f.markedByRun = uuid.UUID(arg.AddressedByRunID.Bytes)
	return int64(len(arg.Ids)), nil
}

type autopilotCall struct {
	userID, repoID  uuid.UUID
	issueIID        int64
	description     string
	allowWithoutPRD bool
	// waitOnLimit is nil for the poller-shaped CreateAutopilotRun (which drops the
	// per-run choice and takes the owner default) and carries the schedule's captured
	// value for CreateScheduledAutopilotRun (PRD #274 Decision 1a). A test proves the
	// auto-approve scheduled path threads the schedule's wait_on_limit by reading it.
	waitOnLimit *bool
	// model is the schedule's per-schedule model override (PRD #300), captured so a later
	// milestone (M3) can assert the frozen model was threaded onto the created run. nil for
	// the poller-shaped CreateAutopilotRun (no per-run model).
	model *string
	// overrideSubagentModel is the schedule's "apply model also to agents" opt-in (PRD
	// #305), captured so a later milestone can assert the frozen flag was threaded onto
	// the created run. false for the poller-shaped CreateAutopilotRun (no per-run opt-in).
	overrideSubagentModel bool
}

type runCall struct {
	userID, repoID  uuid.UUID
	issueIID        int64
	allowWithoutPRD bool
	waitOnLimit     *bool
	// scheduled discriminates which seam the scheduler routed to: false = CreateRun
	// (the interactive human seam, which carries PRD #196's PRD-link waiver), true =
	// CreateScheduledRun (the non-interactive scheduled seam, no waiver). This pins the
	// non-auto issue path to the waiver-free method — routing it through CreateRun
	// instead would silently reopen the M4-review HIGH.
	scheduled bool
	// model is the schedule's per-schedule model override (PRD #300), captured for an M3
	// assertion. nil for the interactive CreateRun (no per-schedule model).
	model *string
	// overrideSubagentModel is the schedule's "apply model also to agents" opt-in (PRD
	// #305), captured for a later assertion. false for the interactive CreateRun.
	overrideSubagentModel bool
}

type promptCall struct {
	userID, repoID, scheduleID uuid.UUID
	title, prompt              string
	autoApprove, waitOnLimit   bool
	// model is the schedule's per-schedule model override (PRD #300), captured for an M3
	// assertion that the frozen model was threaded onto the prompt run.
	model *string
	// overrideSubagentModel is the schedule's "apply model also to agents" opt-in (PRD
	// #305), captured for a later assertion that the frozen flag reached the prompt run.
	overrideSubagentModel bool
}

type fakeRuns struct {
	autopilot []autopilotCall
	runs      []runCall
	prompts   []promptCall
	// self_improve fire path (PRD #590 M1).
	selfImprove    []selfImproveCall
	selfImproveRun store.Run
	err            error
	// errByIssue overrides err per candidate iid (issue #416 backfill tests): when non-nil
	// and the iid is present, the scheduled create seams return that error (so one candidate
	// can be a no_prd_link skip while its neighbours start); else they fall back to err.
	errByIssue map[int64]error
}

// effErr resolves the create-seam error for one candidate iid: a per-issue override if
// present, otherwise the shared err. Keeps single-value tests (errByIssue nil) unchanged.
func (f *fakeRuns) effErr(issueIID int64) error {
	if f.errByIssue != nil {
		if e, ok := f.errByIssue[issueIID]; ok {
			return e
		}
	}
	return f.err
}

func (f *fakeRuns) CreateRun(_ context.Context, userID, repoID uuid.UUID, issueIID int64, _ string, allowWithoutPRD bool, waitOnLimit *bool, _ *workersvc.SeededPlan) (store.Run, error) {
	if f.err != nil {
		return store.Run{}, f.err
	}
	// nil model: the interactive CreateRun seam carries no per-schedule model (PRD #300).
	// false overrideSubagentModel: no per-run opt-in on the interactive seam (PRD #305).
	f.runs = append(f.runs, runCall{userID, repoID, issueIID, allowWithoutPRD, waitOnLimit, false, nil, false})
	return store.Run{ID: uuid.New()}, nil
}
func (f *fakeRuns) CreateScheduledRun(_ context.Context, userID, repoID uuid.UUID, issueIID int64, _ string, allowWithoutPRD bool, waitOnLimit *bool, model *string, overrideSubagentModel bool, _ *workersvc.SeededPlan) (store.Run, error) {
	// The non-auto-approve scheduled path (PRD #196): recorded in the same `runs`
	// bucket as CreateRun so the existing wait-on-limit / path-selection count
	// assertions still observe it, but tagged scheduled=true so a test can prove the
	// scheduler routed here (waiver-free) rather than through CreateRun.
	if err := f.effErr(issueIID); err != nil {
		return store.Run{}, err
	}
	f.runs = append(f.runs, runCall{userID, repoID, issueIID, allowWithoutPRD, waitOnLimit, true, model, overrideSubagentModel})
	return store.Run{ID: uuid.New()}, nil
}
func (f *fakeRuns) CreateAutopilotRun(_ context.Context, userID, repoID uuid.UUID, issueIID int64, description string, allowWithoutPRD bool) (store.Run, error) {
	if f.err != nil {
		return store.Run{}, f.err
	}
	// nil waitOnLimit: the poller seam has no per-run choice (owner default). nil model:
	// the poller seam has no per-run model (PRD #300). Kept so the interface stays
	// satisfied even though the scheduler no longer calls it.
	f.autopilot = append(f.autopilot, autopilotCall{userID, repoID, issueIID, description, allowWithoutPRD, nil, nil, false})
	return store.Run{ID: uuid.New()}, nil
}
func (f *fakeRuns) CreateScheduledAutopilotRun(_ context.Context, userID, repoID uuid.UUID, issueIID int64, description string, allowWithoutPRD bool, waitOnLimit *bool, model *string, overrideSubagentModel bool) (store.Run, error) {
	// The auto-approve scheduled path (PRD #274 Decision 1a): recorded in the same
	// `autopilot` bucket as CreateAutopilotRun so the existing count assertions still
	// observe it, but it CAPTURES waitOnLimit (which CreateAutopilotRun drops) and the
	// schedule's model (PRD #300) so a test can prove both are threaded through.
	if err := f.effErr(issueIID); err != nil {
		return store.Run{}, err
	}
	f.autopilot = append(f.autopilot, autopilotCall{userID, repoID, issueIID, description, allowWithoutPRD, waitOnLimit, model, overrideSubagentModel})
	return store.Run{ID: uuid.New()}, nil
}
func (f *fakeRuns) CreatePromptRun(_ context.Context, userID, repoID, scheduleID uuid.UUID, title, prompt string, autoApprove, waitOnLimit bool, model *string, overrideSubagentModel bool) (store.Run, error) {
	if f.err != nil {
		return store.Run{}, f.err
	}
	f.prompts = append(f.prompts, promptCall{userID, repoID, scheduleID, title, prompt, autoApprove, waitOnLimit, model, overrideSubagentModel})
	return store.Run{ID: uuid.New()}, nil
}

// selfImproveCall records one CreateSelfImproveRun (PRD #590 M1) so a test can assert the
// owner/repo/tracking-issue/description and the threaded per-schedule model reached the seam.
type selfImproveCall struct {
	userID, repoID        uuid.UUID
	issueIID              int64
	title, description    string
	model                 *string
	overrideSubagentModel bool
}

func (f *fakeRuns) CreateSelfImproveRun(_ context.Context, userID, repoID uuid.UUID, issueIID int64, title, description string, model *string, overrideSubagentModel bool) (store.Run, error) {
	if f.err != nil {
		return store.Run{}, f.err
	}
	f.selfImprove = append(f.selfImprove, selfImproveCall{userID, repoID, issueIID, title, description, model, overrideSubagentModel})
	if f.selfImproveRun.ID == (uuid.UUID{}) {
		f.selfImproveRun = store.Run{ID: uuid.New()}
	}
	return f.selfImproveRun, nil
}

// fakeForge embeds forge.Forge (nil) and overrides only GetIssue, so any other call
// panics loudly rather than silently passing.
type fakeForge struct {
	forge.Forge
	issue forge.Issue
	err   error
	// getIID logs every candidate iid GetIssue was called for, in order (issue #416): the
	// examined-count / forge-call-bound assertions read it. A candidate skipped by the
	// active-run pre-check never reaches GetIssue, so it is absent here.
	getIID []int64

	// self_improve tracking-issue resolution (PRD #590 M1): ListIssues returns listResult;
	// CreateIssue is only called when no OPEN listResult issue exists, and it records the
	// call so a test can assert reuse-vs-create.
	listResult    []forge.Issue
	listErr       error
	createErr     error
	createdIID    int64
	createCount   int
	createdLabels []string

	// self_improve open-MR cap (PRD #686 D10/D12): GetMergeRequest returns the forge
	// state per mr_iid. mrStateByIID maps mr_iid → one of the forge.MRState* constants
	// (a missing key scans as ""); mrErr forces a transient forge error; getMRIID records
	// every mr_iid asked for, in order, so a test can prove the cap only inspects the
	// candidate window.
	mrStateByIID map[int64]string
	mrErr        error
	getMRIID     []int64
}

func (f *fakeForge) GetIssue(_ context.Context, _ int64, iid int64) (forge.Issue, error) {
	f.getIID = append(f.getIID, iid)
	return f.issue, f.err
}
func (f *fakeForge) ListIssues(_ context.Context, _ int64, _ forge.ListIssuesOptions) ([]forge.Issue, error) {
	return f.listResult, f.listErr
}
func (f *fakeForge) CreateIssue(_ context.Context, _ int64, _, _ string, labels []string) (forge.Issue, error) {
	f.createCount++
	f.createdLabels = labels
	return forge.Issue{IID: f.createdIID}, f.createErr
}
func (f *fakeForge) GetMergeRequest(_ context.Context, _ int64, mrIID int64) (forge.MergeRequest, error) {
	f.getMRIID = append(f.getMRIID, mrIID)
	if f.mrErr != nil {
		return forge.MergeRequest{}, f.mrErr
	}
	return forge.MergeRequest{IID: mrIID, State: f.mrStateByIID[mrIID]}, nil
}

type fakeBuilder struct {
	f   *fakeForge
	err error
}

func (b *fakeBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.f, nil
}

type fakeSettings struct {
	prdlessEnabled bool
	prdlessLabel   string
	prdLabel       string
}

func (s *fakeSettings) PrdlessEnabled(context.Context) (bool, error) { return s.prdlessEnabled, nil }
func (s *fakeSettings) PrdlessLabel(context.Context) (string, error) { return s.prdlessLabel, nil }
func (s *fakeSettings) PRDLabel(context.Context) (string, error)     { return s.prdLabel, nil }

type fakeNotifier struct{ notifications []notifysvc.Notification }

func (n *fakeNotifier) Notify(_ context.Context, notif notifysvc.Notification) (store.Notification, error) {
	n.notifications = append(n.notifications, notif)
	return store.Notification{}, nil
}

// fakeVault satisfies VaultGate (PRD #590 M1): unlocked controls the self_improve vault gate.
type fakeVault struct{ unlocked bool }

func (v *fakeVault) Unlocked(uuid.UUID) bool { return v.unlocked }

// ── Harness ──────────────────────────────────────────────────────────────────

type harness struct {
	st    *fakeStore
	runs  *fakeRuns
	fb    *fakeBuilder
	set   *fakeSettings
	notif *fakeNotifier
	vault *fakeVault
	sched *Scheduler
	now   time.Time

	owner  uuid.UUID
	repoID uuid.UUID
}

func newHarness() *harness {
	owner := uuid.New()
	repoID := uuid.New()
	h := &harness{
		st:     &fakeStore{repoRow: store.GetRepoForUserRow{ID: repoID, UserID: owner, ForgeProjectID: 42, ForgeType: "gitlab", PathWithNamespace: "vtmocanu/uzi", FoldImproveUziBacklog: true}},
		runs:   &fakeRuns{},
		fb:     &fakeBuilder{f: &fakeForge{issue: forge.Issue{IID: 7, Description: "body", Labels: []string{"PRD"}}, createdIID: 7}},
		set:    &fakeSettings{prdLabel: "PRD"},
		notif:  &fakeNotifier{},
		vault:  &fakeVault{unlocked: true},
		now:    time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
		owner:  owner,
		repoID: repoID,
	}
	h.sched = New(h.st, h.runs, h.fb, h.set, h.notif, h.vault, time.Minute, nil)
	h.sched.now = func() time.Time { return h.now }
	return h
}

// issueSchedule builds a due issue schedule owned by the harness owner/repo.
func (h *harness) issueSchedule() store.RunSchedule {
	return store.RunSchedule{
		ID:          uuid.New(),
		UserID:      h.owner,
		RepoID:      h.repoID,
		Target:      "issue",
		IssueIid:    pgtype.Int8{Int64: 7, Valid: true},
		Timing:      "recurring",
		CronExpr:    pgtype.Text{String: "0 * * * *", Valid: true},
		Timezone:    "UTC",
		AutoApprove: true,
		Status:      "active",
		Enabled:     true,
	}
}

// selfImproveSchedule builds a due self_improve schedule owned by the harness owner/repo
// (PRD #590 M1). It is default-origin, mirroring a catalog-enabled self-improve job.
func (h *harness) selfImproveSchedule() store.RunSchedule {
	return store.RunSchedule{
		ID:          uuid.New(),
		UserID:      h.owner,
		RepoID:      h.repoID,
		Target:      "self_improve",
		Timing:      "recurring",
		CronExpr:    pgtype.Text{String: "0 4 */2 * *", Valid: true},
		Timezone:    "UTC",
		Origin:      "default",
		CatalogSlug: pgtype.Text{String: "self-improve", Valid: true},
		AutoApprove: true,
		Status:      "active",
		Enabled:     true,
	}
}

func (h *harness) countKind(kind string) int {
	n := 0
	for _, notif := range h.notif.notifications {
		if notif.Kind == kind {
			n++
		}
	}
	return n
}

// ── Tests ────────────────────────────────────────────────────────────────────

func TestTickIssueScheduleFiresAndAdvances(t *testing.T) {
	h := newHarness()
	h.st.due = []store.RunSchedule{h.issueSchedule()}

	h.sched.Boot(context.Background())

	if len(h.runs.autopilot) != 1 {
		t.Fatalf("auto-approve issue: CreateScheduledAutopilotRun calls = %d, want 1", len(h.runs.autopilot))
	}
	if len(h.runs.runs) != 0 {
		t.Fatalf("auto-approve issue must NOT use the non-auto CreateScheduledRun path, got %d", len(h.runs.runs))
	}
	call := h.runs.autopilot[0]
	if call.userID != h.owner || call.repoID != h.repoID || call.issueIID != 7 {
		t.Fatalf("autopilot call = %+v, want owner/repo/issue 7", call)
	}
	if len(h.st.advanceCalls) != 1 {
		t.Fatalf("advance calls = %d, want 1", len(h.st.advanceCalls))
	}
	adv := h.st.advanceCalls[0]
	if adv.Status != "active" {
		t.Fatalf("recurring advance status = %q, want active", adv.Status)
	}
	if !adv.NextFireAt.Valid || !adv.NextFireAt.Time.After(h.now) {
		t.Fatalf("recurring next_fire_at = %+v, want a bumped future instant", adv.NextFireAt)
	}
}

// TestTickAutoApproveIssueThreadsWaitOnLimit pins PRD #274 Decision 1a: an auto-approve
// scheduled run must fire through CreateScheduledAutopilotRun and thread the schedule's
// persisted wait_on_limit (not the poller's CreateAutopilotRun, which drops it to the
// owner default). A nil captured value would mean the scheduler wrongly used the poller
// seam; the assertion below fails in that case.
func TestTickAutoApproveIssueThreadsWaitOnLimit(t *testing.T) {
	h := newHarness()
	s := h.issueSchedule() // AutoApprove=true
	s.WaitOnLimit = true
	h.st.due = []store.RunSchedule{s}

	h.sched.Boot(context.Background())

	if len(h.runs.autopilot) != 1 {
		t.Fatalf("auto-approve issue: CreateScheduledAutopilotRun calls = %d, want 1", len(h.runs.autopilot))
	}
	got := h.runs.autopilot[0].waitOnLimit
	if got == nil {
		t.Fatalf("auto-approve scheduled run dropped wait_on_limit (nil) — it took the poller seam instead of CreateScheduledAutopilotRun")
	}
	if !*got {
		t.Fatalf("auto-approve scheduled run threaded wait_on_limit = %v, want true (the schedule's persisted value)", *got)
	}
}

// TestTickNonAutoIssueScheduleUsesTheScheduledSeam pins the M4-review fix: a
// non-auto-approve issue schedule must fire through CreateScheduledRun (the waiver-free
// seam), NOT CreateRun (the interactive seam that carries PRD #196's PRD-link waiver).
// Routing a timer-fired sweep through CreateRun would let it start link-less
// non-primary-eligible runs unattended — the exact HIGH the fix closes. Reverting
// scheduler.go's non-auto branch to CreateRun makes this test fail (scheduled=false).
func TestTickNonAutoIssueScheduleUsesTheScheduledSeam(t *testing.T) {
	h := newHarness()
	s := h.issueSchedule()
	s.AutoApprove = false // the human-approves-the-plan path, but still no per-run click
	h.st.due = []store.RunSchedule{s}

	h.sched.Boot(context.Background())

	if len(h.runs.autopilot) != 0 {
		t.Fatalf("non-auto issue must NOT use the autopilot seam, got %d", len(h.runs.autopilot))
	}
	if len(h.runs.runs) != 1 {
		t.Fatalf("non-auto issue: run-seam calls = %d, want 1", len(h.runs.runs))
	}
	if !h.runs.runs[0].scheduled {
		t.Fatalf("non-auto issue must fire through CreateScheduledRun (waiver-free), not CreateRun (interactive, waiver-carrying)")
	}
}

// sweepSchedule builds a due sweep schedule owned by the harness owner/repo, carrying
// the given max_issues cap (Valid=false for an unlimited/NULL cap).
func (h *harness) sweepSchedule(maxIssues pgtype.Int4) store.RunSchedule {
	return store.RunSchedule{
		ID:          uuid.New(),
		UserID:      h.owner,
		RepoID:      h.repoID,
		Target:      "sweep",
		Labels:      []byte(`["PRD"]`),
		Timing:      "recurring",
		CronExpr:    pgtype.Text{String: "0 * * * *", Valid: true},
		Timezone:    "UTC",
		AutoApprove: true,
		Status:      "active",
		Enabled:     true,
		MaxIssues:   maxIssues,
	}
}

// TestTickSweepThreadsMaxIssues pins the scan-window fetch at the unit level. Issue #416
// inverts the old PRD #274 M2 contract: fireSweep no longer threads the schedule's
// max_issues verbatim — it widens the fetch to max_issues + backfillHeadroom so the loop
// can backfill slots lost to skips, then caps runs STARTED (not candidates fetched) at
// max_issues. So the threaded LIMIT is the SCAN WINDOW, not the cap (the fake store runs
// no SQL, so the threaded param is the most a unit test can pin; the real LIMIT
// truncation is covered by the live-DB test). A NULL cap still threads NULL (unlimited).
func TestTickSweepThreadsMaxIssues(t *testing.T) {
	// A set cap is threaded as max_issues + backfillHeadroom (the scan window), NOT verbatim.
	h := newHarness()
	h.st.due = []store.RunSchedule{h.sweepSchedule(pgtype.Int4{Int32: 4, Valid: true})}
	h.sched.Boot(context.Background())
	got := h.st.sweepMaxIssuesParam
	if want := int32(4 + backfillHeadroom); !got.Valid || got.Int32 != want {
		t.Fatalf("sweep max_issues param = %+v, want {Int32:%d Valid:true} (max_issues + backfillHeadroom)", got, want)
	}

	// A NULL cap threads through as NULL (unlimited).
	h2 := newHarness()
	h2.st.due = []store.RunSchedule{h2.sweepSchedule(pgtype.Int4{})}
	h2.sched.Boot(context.Background())
	if h2.st.sweepMaxIssuesParam.Valid {
		t.Fatalf("NULL max_issues must thread as an invalid (NULL) param, got %+v", h2.st.sweepMaxIssuesParam)
	}
}

func TestTickOnceScheduleFiresToStatus(t *testing.T) {
	h := newHarness()
	s := h.issueSchedule()
	s.Timing = "once"
	s.CronExpr = pgtype.Text{} // once carries run_at, not cron
	h.st.due = []store.RunSchedule{s}

	h.sched.Boot(context.Background())

	if len(h.runs.autopilot) != 1 {
		t.Fatalf("once issue: CreateScheduledAutopilotRun calls = %d, want 1", len(h.runs.autopilot))
	}
	if len(h.st.advanceCalls) != 1 {
		t.Fatalf("advance calls = %d, want 1", len(h.st.advanceCalls))
	}
	adv := h.st.advanceCalls[0]
	if adv.Status != "fired" {
		t.Fatalf("once advance status = %q, want fired", adv.Status)
	}
	if adv.NextFireAt.Valid {
		t.Fatalf("once next_fire_at = %+v, want NULL", adv.NextFireAt)
	}
}

func TestTickDedupSkipStillAdvances(t *testing.T) {
	h := newHarness()
	h.st.activeIssue = true // a prior run for the issue is still live
	h.st.due = []store.RunSchedule{h.issueSchedule()}

	h.sched.Boot(context.Background())

	if len(h.runs.autopilot) != 0 || len(h.runs.runs) != 0 {
		t.Fatalf("dedup skip must not fire the seam: autopilot=%d run=%d", len(h.runs.autopilot), len(h.runs.runs))
	}
	if len(h.st.advanceCalls) != 1 {
		t.Fatalf("dedup skip must STILL advance: advance calls = %d, want 1", len(h.st.advanceCalls))
	}
	if h.st.advanceCalls[0].Status != "active" {
		t.Fatalf("dedup skip advance status = %q, want active", h.st.advanceCalls[0].Status)
	}
}

func TestTickTransientErrorDoesNotAdvance(t *testing.T) {
	h := newHarness()
	h.fb.err = context.DeadlineExceeded // forge builder fails → transient
	h.st.due = []store.RunSchedule{h.issueSchedule()}

	h.sched.Boot(context.Background())

	if len(h.runs.autopilot) != 0 {
		t.Fatalf("transient error: no run should be created, got %d", len(h.runs.autopilot))
	}
	if len(h.st.advanceCalls) != 0 {
		t.Fatalf("transient error must NOT advance (schedule stays due): advance calls = %d, want 0", len(h.st.advanceCalls))
	}
	if len(h.st.statusCalls) != 0 {
		t.Fatalf("transient error must NOT park: status calls = %d, want 0", len(h.st.statusCalls))
	}
}

func TestTickPermanentErrorParks(t *testing.T) {
	h := newHarness()
	h.st.repoErr = pgx.ErrNoRows // repo gone / not owned → permanent
	h.st.due = []store.RunSchedule{h.issueSchedule()}

	h.sched.Boot(context.Background())

	if len(h.st.advanceCalls) != 0 {
		t.Fatalf("permanent error must NOT advance: advance calls = %d, want 0", len(h.st.advanceCalls))
	}
	if len(h.st.statusCalls) != 1 {
		t.Fatalf("permanent error must park at status='error': status calls = %d, want 1", len(h.st.statusCalls))
	}
	if h.st.statusCalls[0].Status != "error" {
		t.Fatalf("park status = %q, want error", h.st.statusCalls[0].Status)
	}
	if len(h.notif.notifications) != 1 || h.notif.notifications[0].Kind != "schedule_error" {
		t.Fatalf("park must notify the owner once with a schedule_error, got %+v", h.notif.notifications)
	}
	if h.notif.notifications[0].UserID != h.owner {
		t.Fatalf("park notification user = %v, want owner %v", h.notif.notifications[0].UserID, h.owner)
	}
}

// TestTickGuardrailBlockedParks: a scheduled fire refused by the #66 default-branch
// guardrail is PERMANENT (the bot's default-branch permissions do not change
// tick-to-tick), so the schedule parks at status='error' rather than refiring — and
// re-blocking — every tick (a self-inflicted tick-storm). The owner is notified.
func TestTickGuardrailBlockedParks(t *testing.T) {
	h := newHarness()
	h.runs.err = &workersvc.GuardrailBlockedError{Findings: []string{`the write role may push to protected "main"`}}
	h.st.due = []store.RunSchedule{h.issueSchedule()}

	h.sched.Boot(context.Background())

	if len(h.st.advanceCalls) != 0 {
		t.Fatalf("guardrail block must NOT advance: advance calls = %d, want 0", len(h.st.advanceCalls))
	}
	if len(h.st.statusCalls) != 1 || h.st.statusCalls[0].Status != "error" {
		t.Fatalf("guardrail block must PARK at status='error' (not tick-storm): status calls = %+v", h.st.statusCalls)
	}
	if len(h.notif.notifications) != 1 || h.notif.notifications[0].Kind != "schedule_error" {
		t.Fatalf("park must notify the owner once with a schedule_error, got %+v", h.notif.notifications)
	}
}

func TestTickMalformedConfigParks(t *testing.T) {
	h := newHarness()
	s := h.issueSchedule()
	s.Target = "sweep"
	s.IssueIid = pgtype.Int8{}              // sweep carries no issue
	s.Labels = []byte(`{"not":"an array"}`) // valid jsonb, invalid selector shape
	h.st.due = []store.RunSchedule{s}

	h.sched.Boot(context.Background())

	if len(h.st.advanceCalls) != 0 {
		t.Fatalf("malformed config must NOT advance: advance calls = %d, want 0", len(h.st.advanceCalls))
	}
	if len(h.st.statusCalls) != 1 || h.st.statusCalls[0].Status != "error" {
		t.Fatalf("malformed config must PARK at status='error' (not retry forever): status calls = %+v", h.st.statusCalls)
	}
}

func TestTickPromptScheduleCreatesPromptRun(t *testing.T) {
	h := newHarness()
	s := h.issueSchedule()
	s.Target = "prompt"
	s.IssueIid = pgtype.Int8{}
	s.Prompt = pgtype.Text{String: "Summarize open issues and file a report\nmore detail here", Valid: true}
	s.AutoApprove = false
	s.WaitOnLimit = true
	// PRD #305 M1/M3: the schedule's "apply model also to agents" opt-in must be read
	// off the right RunSchedule field by scheduleOverrideSubagentModel and threaded onto
	// the fired run's create seam — assert it lands on the captured prompt call.
	s.OverrideSubagentModel = true
	h.st.due = []store.RunSchedule{s}

	h.sched.Boot(context.Background())

	if len(h.runs.prompts) != 1 {
		t.Fatalf("prompt schedule: CreatePromptRun calls = %d, want 1", len(h.runs.prompts))
	}
	p := h.runs.prompts[0]
	if p.userID != h.owner || p.repoID != h.repoID || p.scheduleID != s.ID {
		t.Fatalf("prompt call ids = %+v, want owner/repo/schedule", p)
	}
	if !p.overrideSubagentModel {
		t.Fatalf("schedule OverrideSubagentModel=true must thread onto the prompt run; got %+v", p)
	}
	if p.prompt != s.Prompt.String {
		t.Fatalf("prompt body = %q, want the full prompt text", p.prompt)
	}
	// The derived title is the first line, capped — never blank.
	if p.title != "Summarize open issues and file a report" {
		t.Fatalf("derived title = %q, want the first prompt line", p.title)
	}
	if !p.waitOnLimit || p.autoApprove {
		t.Fatalf("prompt flags = wait:%v auto:%v, want wait:true auto:false", p.waitOnLimit, p.autoApprove)
	}
	if len(h.st.advanceCalls) != 1 {
		t.Fatalf("prompt fire must advance: advance calls = %d, want 1", len(h.st.advanceCalls))
	}
}

func TestPromptTitleDerivation(t *testing.T) {
	long := "x23456789012345678901234567890123456789012345678901234567890ABCDEF" // > cap
	got := promptTitle(long)
	if r := []rune(got); len(r) != promptTitleCap+1 || r[promptTitleCap] != '…' {
		t.Fatalf("long prompt title = %q (len %d runes), want cap+ellipsis", got, len([]rune(got)))
	}
	if promptTitle("   \n  ") != "Scheduled prompt run" {
		t.Fatalf("blank prompt title fallback missing")
	}
}

// ── Fire outcomes (PRD #308 M1) ──────────────────────────────────────────────

// assertBalances is the matched == started + skipped invariant every fire must satisfy
// (PRD #308 Decision 4): every candidate lands in exactly one bucket.
func assertBalances(t *testing.T, o FireOutcome) {
	t.Helper()
	if o.Matched != len(o.Started)+len(o.Skips) {
		t.Fatalf("invariant broken: Matched=%d != started(%d)+skips(%d)", o.Matched, len(o.Started), len(o.Skips))
	}
}

// TestSkipReasonForErr pins the seam-sentinel → reason mapping directly: each of the four
// benign run-creation sentinels maps to its reason; ErrActivePromptExists and an unrelated
// error are NOT mapped here (the prompt path records already_running at its own site, and
// an unknown error is left to the caller to classify as transient/permanent).
func TestSkipReasonForErr(t *testing.T) {
	cases := []struct {
		err  error
		want SkipReason
		ok   bool
	}{
		{workersvc.ErrNoPRDLink, SkipNoPRDLink, true},
		{workersvc.ErrNotPRDIssue, SkipNotEligible, true},
		{workersvc.ErrActiveRunExists, SkipAlreadyRunning, true},
		{workersvc.ErrDescriptionTooLarge, SkipDescriptionTooLarge, true},
		{workersvc.ErrActivePromptExists, "", false},
		{workersvc.ErrRepoNotFound, "", false},
		{context.DeadlineExceeded, "", false},
	}
	for _, c := range cases {
		got, ok := skipReasonForErr(c.err)
		if ok != c.ok || got != c.want {
			t.Fatalf("skipReasonForErr(%v) = (%q,%v), want (%q,%v)", c.err, got, ok, c.want, c.ok)
		}
	}
	// AllSkipReasons enumerates the full closed set (PRD #590 M1 added vault_locked;
	// PRD #686 D10 added self_improve_mr_cap_reached).
	if len(AllSkipReasons) != 7 {
		t.Fatalf("AllSkipReasons has %d reasons, want 7", len(AllSkipReasons))
	}
}

// TestFireIssueSentinelSkips proves each benign seam sentinel becomes a typed Skip on the
// single-issue create attempt (Matched=1, one Skip, no Started), carrying the fetched
// issue title and its iid — and the invariant balances.
func TestFireIssueSentinelSkips(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want SkipReason
	}{
		{"active_run", workersvc.ErrActiveRunExists, SkipAlreadyRunning},
		{"not_eligible", workersvc.ErrNotPRDIssue, SkipNotEligible},
		{"no_prd_link", workersvc.ErrNoPRDLink, SkipNoPRDLink},
		{"too_large", workersvc.ErrDescriptionTooLarge, SkipDescriptionTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			h.fb.f.issue.Title = "Fix the bug"
			h.runs.err = c.err
			out, err := h.sched.RunNow(context.Background(), h.issueSchedule())
			if err != nil {
				t.Fatalf("a benign sentinel must NOT surface as an error, got %v", err)
			}
			if out.Matched != 1 || len(out.Started) != 0 || len(out.Skips) != 1 {
				t.Fatalf("outcome = %+v, want Matched:1 Started:0 Skips:1", out)
			}
			s := out.Skips[0]
			if s.Reason != c.want {
				t.Fatalf("skip reason = %q, want %q", s.Reason, c.want)
			}
			if s.IssueIID == nil || *s.IssueIID != 7 {
				t.Fatalf("skip IssueIID = %v, want 7", s.IssueIID)
			}
			if s.Title != "Fix the bug" {
				t.Fatalf("skip Title = %q, want the fetched issue title", s.Title)
			}
			assertBalances(t, out)
		})
	}
}

// TestFireIssuePrecheckAlreadyRunning: the active-run pre-check records already_running
// WITHOUT firing the seam and without an extra forge call — so Title is left empty.
func TestFireIssuePrecheckAlreadyRunning(t *testing.T) {
	h := newHarness()
	h.st.activeIssue = true
	out, err := h.sched.RunNow(context.Background(), h.issueSchedule())
	if err != nil {
		t.Fatalf("pre-check skip is benign, got err %v", err)
	}
	if out.Matched != 1 || len(out.Skips) != 1 || len(out.Started) != 0 {
		t.Fatalf("outcome = %+v, want Matched:1 one already_running skip", out)
	}
	if out.Skips[0].Reason != SkipAlreadyRunning {
		t.Fatalf("reason = %q, want already_running", out.Skips[0].Reason)
	}
	if out.Skips[0].IssueIID == nil || *out.Skips[0].IssueIID != 7 {
		t.Fatalf("skip IssueIID = %v, want 7", out.Skips[0].IssueIID)
	}
	if out.Skips[0].Title != "" {
		t.Fatalf("pre-check skip Title = %q, want empty (no extra forge call)", out.Skips[0].Title)
	}
	if len(h.runs.autopilot) != 0 || len(h.runs.runs) != 0 {
		t.Fatalf("pre-check skip must not fire the seam")
	}
	assertBalances(t, out)
}

// TestRunNowDoesNotPersistLastFire pins Decision 3 behaviourally, not just structurally: a
// manual RunNow fire must not disturb the cadence, so it must never reach advance() and
// therefore never write last_fire. A future refactor routing RunNow through advance would
// redden here even though the fire itself succeeds.
func TestRunNowDoesNotPersistLastFire(t *testing.T) {
	h := newHarness()
	h.fb.f.issue.Title = "Ship it"
	out, err := h.sched.RunNow(context.Background(), h.issueSchedule())
	if err != nil {
		t.Fatalf("success fire err = %v", err)
	}
	if len(out.Started) != 1 {
		t.Fatalf("expected a successful fire, got outcome %+v", out)
	}
	if len(h.st.advanceCalls) != 0 {
		t.Fatalf("RunNow must NOT advance (and so must not persist last_fire): advance calls = %d, want 0", len(h.st.advanceCalls))
	}
}

// TestFireIssueSuccessStarted: a successful issue fire yields one Started pairing the issue
// (iid + fetched title) with the run it produced.
func TestFireIssueSuccessStarted(t *testing.T) {
	h := newHarness()
	h.fb.f.issue.Title = "Ship it"
	out, err := h.sched.RunNow(context.Background(), h.issueSchedule())
	if err != nil {
		t.Fatalf("success fire err = %v", err)
	}
	if out.Matched != 1 || len(out.Started) != 1 || len(out.Skips) != 0 {
		t.Fatalf("outcome = %+v, want Matched:1 Started:1 Skips:0", out)
	}
	st := out.Started[0]
	if st.IssueIID == nil || *st.IssueIID != 7 || st.Title != "Ship it" || st.RunID == (uuid.UUID{}) {
		t.Fatalf("started = %+v, want iid 7 / title / non-zero run id", st)
	}
	assertBalances(t, out)
}

// TestFireIssueForgeErrorIsTransientNoRecord: a forge GetIssue error on an ISSUE target is
// transient (retry next tick), NOT a recorded fetch_failed — the zero outcome is returned
// with the error (PRD #308: fetch_failed is a sweep-only bucketing).
func TestFireIssueForgeErrorIsTransientNoRecord(t *testing.T) {
	h := newHarness()
	h.fb.f.err = context.DeadlineExceeded
	out, err := h.sched.RunNow(context.Background(), h.issueSchedule())
	if err == nil {
		t.Fatalf("forge error on issue target must surface as a transient error")
	}
	if out.Matched != 0 || len(out.Started) != 0 || len(out.Skips) != 0 {
		t.Fatalf("transient error must return the zero outcome, got %+v", out)
	}
}

// TestFirePromptOutcomes covers the three prompt paths: pre-check already_running, the
// ErrActivePromptExists race (same reason), and success. All carry Matched=1, a nil
// IssueIID, and the derived prompt title.
func TestFirePromptOutcomes(t *testing.T) {
	promptSched := func(h *harness) store.RunSchedule {
		s := h.issueSchedule()
		s.Target = "prompt"
		s.IssueIid = pgtype.Int8{}
		s.Prompt = pgtype.Text{String: "Do the nightly report", Valid: true}
		return s
	}

	t.Run("precheck_already_running", func(t *testing.T) {
		h := newHarness()
		h.st.activeSchedule = true
		out, err := h.sched.RunNow(context.Background(), promptSched(h))
		if err != nil {
			t.Fatalf("pre-check skip err = %v", err)
		}
		if out.Matched != 1 || len(out.Skips) != 1 || out.Skips[0].Reason != SkipAlreadyRunning {
			t.Fatalf("outcome = %+v, want one already_running skip", out)
		}
		if out.Skips[0].IssueIID != nil {
			t.Fatalf("prompt skip IssueIID = %v, want nil", out.Skips[0].IssueIID)
		}
		if out.Skips[0].Title != "Do the nightly report" {
			t.Fatalf("prompt skip Title = %q, want the derived prompt title", out.Skips[0].Title)
		}
		assertBalances(t, out)
	})

	t.Run("race_already_running", func(t *testing.T) {
		h := newHarness()
		h.runs.err = workersvc.ErrActivePromptExists
		out, err := h.sched.RunNow(context.Background(), promptSched(h))
		if err != nil {
			t.Fatalf("race skip is benign, got err %v", err)
		}
		if out.Matched != 1 || len(out.Skips) != 1 || out.Skips[0].Reason != SkipAlreadyRunning {
			t.Fatalf("outcome = %+v, want one already_running skip from the race", out)
		}
		assertBalances(t, out)
	})

	t.Run("success", func(t *testing.T) {
		h := newHarness()
		out, err := h.sched.RunNow(context.Background(), promptSched(h))
		if err != nil {
			t.Fatalf("success err = %v", err)
		}
		if out.Matched != 1 || len(out.Started) != 1 || len(out.Skips) != 0 {
			t.Fatalf("outcome = %+v, want Matched:1 Started:1", out)
		}
		if out.Started[0].IssueIID != nil {
			t.Fatalf("prompt started IssueIID = %v, want nil", out.Started[0].IssueIID)
		}
		if out.Started[0].Title != "Do the nightly report" {
			t.Fatalf("prompt started Title = %q, want the derived prompt title", out.Started[0].Title)
		}
		assertBalances(t, out)
	})
}

// TestFireSweepPerCandidateBuckets pins each per-candidate branch to its bucket, one
// candidate at a time, and the invariant for each.
func TestFireSweepPerCandidateBuckets(t *testing.T) {
	oneRow := []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 96}}

	t.Run("active_run_db_error_is_fetch_failed", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = oneRow
		h.st.activeIssueErr = context.DeadlineExceeded
		out, err := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{}))
		if err != nil {
			t.Fatalf("sweep must not abort on a per-candidate error, got %v", err)
		}
		if out.Matched != 1 || len(out.Skips) != 1 || out.Skips[0].Reason != SkipFetchFailed {
			t.Fatalf("outcome = %+v, want one fetch_failed skip", out)
		}
		if out.Skips[0].IssueIID == nil || *out.Skips[0].IssueIID != 96 {
			t.Fatalf("fetch_failed skip iid = %v, want 96", out.Skips[0].IssueIID)
		}
		assertBalances(t, out)
	})

	t.Run("active_bool_is_already_running", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = oneRow
		h.st.activeIssue = true
		out, _ := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{}))
		if out.Matched != 1 || len(out.Skips) != 1 || out.Skips[0].Reason != SkipAlreadyRunning {
			t.Fatalf("outcome = %+v, want one already_running skip", out)
		}
		assertBalances(t, out)
	})

	t.Run("forge_error_is_fetch_failed", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = oneRow
		h.fb.f.err = context.DeadlineExceeded
		out, _ := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{}))
		if out.Matched != 1 || len(out.Skips) != 1 || out.Skips[0].Reason != SkipFetchFailed {
			t.Fatalf("outcome = %+v, want one fetch_failed skip", out)
		}
		assertBalances(t, out)
	})

	t.Run("benign_sentinel_becomes_its_skip", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = oneRow
		h.fb.f.issue.Title = "Broken login"
		h.runs.err = workersvc.ErrNoPRDLink
		out, _ := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{}))
		if out.Matched != 1 || len(out.Skips) != 1 || out.Skips[0].Reason != SkipNoPRDLink {
			t.Fatalf("outcome = %+v, want one no_prd_link skip", out)
		}
		if out.Skips[0].Title != "Broken login" {
			t.Fatalf("sweep skip Title = %q, want the fetched issue title", out.Skips[0].Title)
		}
		assertBalances(t, out)
	})

	t.Run("unexpected_repo_error_midsweep_is_fetch_failed", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = oneRow
		h.runs.err = workersvc.ErrRepoNotFound
		out, err := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{}))
		if err != nil {
			t.Fatalf("a mid-sweep repo error must not abort the fan-out, got %v", err)
		}
		if out.Matched != 1 || len(out.Skips) != 1 || out.Skips[0].Reason != SkipFetchFailed {
			t.Fatalf("outcome = %+v, want one fetch_failed skip", out)
		}
		assertBalances(t, out)
	})

	t.Run("success_becomes_started", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = oneRow
		h.fb.f.issue.Title = "Do it"
		out, _ := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{}))
		if out.Matched != 1 || len(out.Started) != 1 || len(out.Skips) != 0 {
			t.Fatalf("outcome = %+v, want one Started", out)
		}
		if out.Started[0].IssueIID == nil || *out.Started[0].IssueIID != 96 || out.Started[0].Title != "Do it" {
			t.Fatalf("started = %+v, want iid 96 / title", out.Started[0])
		}
		assertBalances(t, out)
	})
}

// TestFireSweepMultiCandidateBalances proves the invariant across a mixed fan-out: two
// candidates that both take the fetch_failed path still balance (Matched=2 == 2 skips).
func TestFireSweepMultiCandidateBalances(t *testing.T) {
	h := newHarness()
	h.st.sweepRows = []store.ListSweepCandidateIssuesRow{
		{ForgeIssueIid: 96},
		{ForgeIssueIid: 97},
	}
	h.st.activeIssueErr = context.DeadlineExceeded // both candidates → fetch_failed
	out, _ := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{}))
	if out.Matched != 2 || len(out.Skips) != 2 {
		t.Fatalf("outcome = %+v, want Matched:2 with 2 fetch_failed skips", out)
	}
	for _, s := range out.Skips {
		if s.Reason != SkipFetchFailed {
			t.Fatalf("skip reason = %q, want fetch_failed", s.Reason)
		}
	}
	assertBalances(t, out)
}

// TestFireSweepCapped pins the Capped truncation probe: Capped is true only when a set cap
// left more matching issues behind than were fetched; a NULL cap never counts and never caps.
func TestFireSweepCapped(t *testing.T) {
	t.Run("capped_when_total_exceeds_fetched", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 96}}
		h.st.sweepCount = 3 // 3 match, only 1 fetched under the cap
		out, _ := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{Int32: 1, Valid: true}))
		if !out.Capped {
			t.Fatalf("Capped = false, want true (total 3 > fetched 1)")
		}
		if h.st.sweepCountCalls != 1 {
			t.Fatalf("CountSweepCandidateIssues called %d times, want 1", h.st.sweepCountCalls)
		}
		// The probe must use the SAME resolved label selector as the list query.
		if string(h.st.countLabelParam) != string(h.st.sweepLabelParam) {
			t.Fatalf("count labels %q != list labels %q", h.st.countLabelParam, h.st.sweepLabelParam)
		}
	})

	t.Run("not_capped_when_total_equals_fetched", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 96}}
		h.st.sweepCount = 1
		out, _ := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{Int32: 4, Valid: true}))
		if out.Capped {
			t.Fatalf("Capped = true, want false (total 1 == fetched 1)")
		}
	})

	t.Run("null_cap_never_counts_or_caps", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 96}}
		h.st.sweepCount = 9 // would be "capped" IF the probe ran
		out, _ := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{}))
		if out.Capped {
			t.Fatalf("NULL cap can never truncate; Capped must be false")
		}
		if h.st.sweepCountCalls != 0 {
			t.Fatalf("NULL cap must skip the count probe entirely, got %d calls", h.st.sweepCountCalls)
		}
	})

	// A DB error on the count probe is TRANSIENT: fireSweep must surface it unchanged and
	// return the zero outcome, NOT swallow the error and proceed as uncapped. A set cap
	// (so the probe runs) plus at least one candidate reaches the probe; the sentinel is
	// forced there. Because RunNow does not drive the advance path, the guard here is the
	// non-nil error + zero outcome (mirroring TestFireIssueForgeErrorIsTransientNoRecord):
	// a mutation that drops the count error and continues would return a non-zero outcome
	// with a nil error and fail this case. The transient-does-not-advance discipline is
	// pinned separately by the tick-path tests.
	t.Run("count_probe_error_is_transient", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 96}}
		h.st.sweepCountErr = context.DeadlineExceeded // sentinel forced on the count probe
		out, err := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{Int32: 1, Valid: true}))
		if err == nil {
			t.Fatalf("count-probe DB error must surface as a transient error, got nil")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("count-probe error = %v, want it to wrap the sentinel (context.DeadlineExceeded)", err)
		}
		if out.Matched != 0 || len(out.Started) != 0 || len(out.Skips) != 0 || out.Capped {
			t.Fatalf("transient count-probe error must return the zero outcome, got %+v", out)
		}
		if h.st.sweepCountCalls != 1 {
			t.Fatalf("CountSweepCandidateIssues called %d times, want 1 (the probe ran and failed)", h.st.sweepCountCalls)
		}
	})
}

// TestFireMatchedPerTarget pins the per-target Matched definition: sweep = candidates
// EXAMINED (issue #416; here all 3 start, so examined == candidate count == 3), issue = 1,
// prompt = 1.
func TestFireMatchedPerTarget(t *testing.T) {
	h := newHarness()
	issueOut, _ := h.sched.RunNow(context.Background(), h.issueSchedule())
	if issueOut.Matched != 1 {
		t.Fatalf("issue Matched = %d, want 1", issueOut.Matched)
	}

	h2 := newHarness()
	h2.st.sweepRows = []store.ListSweepCandidateIssuesRow{
		{ForgeIssueIid: 1}, {ForgeIssueIid: 2}, {ForgeIssueIid: 3},
	}
	sweepOut, _ := h2.sched.RunNow(context.Background(), h2.sweepSchedule(pgtype.Int4{}))
	if sweepOut.Matched != 3 {
		t.Fatalf("sweep Matched = %d, want 3 (candidate count)", sweepOut.Matched)
	}
	assertBalances(t, sweepOut)

	h3 := newHarness()
	ps := h3.issueSchedule()
	ps.Target = "prompt"
	ps.IssueIid = pgtype.Int8{}
	ps.Prompt = pgtype.Text{String: "hello", Valid: true}
	promptOut, _ := h3.sched.RunNow(context.Background(), ps)
	if promptOut.Matched != 1 {
		t.Fatalf("prompt Matched = %d, want 1", promptOut.Matched)
	}
}

// ── Sweep backfill (issue #416) ──────────────────────────────────────────────

// startedIIDs extracts the started candidate iids in order, for the oldest-eligible-first
// assertions below.
func startedIIDs(o FireOutcome) []int64 {
	out := make([]int64, 0, len(o.Started))
	for _, s := range o.Started {
		if s.IssueIID != nil {
			out = append(out, *s.IssueIID)
		}
	}
	return out
}

func eqInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFireSweepBackfillFillsSlotPastSkip is the flagship case: a window whose 2nd candidate
// is a no_prd_link skip still starts max_issues runs by walking past the skip to the next
// eligible candidate — AND stops at max_issues (the 5th, eligible, candidate is never
// examined). The skipped candidate is still flagged, oldest-eligible ordering is preserved,
// and Matched counts only the examined prefix (issue #416).
func TestFireSweepBackfillFillsSlotPastSkip(t *testing.T) {
	h := newHarness()
	// Window the DB would return under LIMIT max_issues+backfillHeadroom, oldest-first. The
	// fake store applies no LIMIT, so we hand it exactly the window's contents.
	h.st.sweepRows = []store.ListSweepCandidateIssuesRow{
		{ForgeIssueIid: 10}, {ForgeIssueIid: 20}, {ForgeIssueIid: 30}, {ForgeIssueIid: 40}, {ForgeIssueIid: 50},
	}
	h.st.sweepCount = 5
	h.runs.errByIssue = map[int64]error{20: workersvc.ErrNoPRDLink} // 20 cannot start
	out, err := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{Int32: 3, Valid: true}))
	if err != nil {
		t.Fatalf("backfill fire must not error, got %v", err)
	}
	// Started 3: 10, 30, 40 — the slot lost to 20 is refilled by walking to 30/40, and the
	// early break stops before 50.
	if want := []int64{10, 30, 40}; !eqInt64s(startedIIDs(out), want) {
		t.Fatalf("started iids = %v, want %v (oldest-eligible first, backfilled past 20, capped at 3)", startedIIDs(out), want)
	}
	if len(out.Skips) != 1 || out.Skips[0].Reason != SkipNoPRDLink || out.Skips[0].IssueIID == nil || *out.Skips[0].IssueIID != 20 {
		t.Fatalf("skips = %+v, want exactly one no_prd_link skip for iid 20", out.Skips)
	}
	// Matched = examined prefix [10,20,30,40] = 4 (NOT 3, NOT 5): the skip is counted, 50 is not.
	if out.Matched != 4 {
		t.Fatalf("Matched = %d, want 4 (examined 10,20,30,40; 50 never reached)", out.Matched)
	}
	// 50 is never fetched: the forge is spared once the cap is filled.
	if want := []int64{10, 20, 30, 40}; !eqInt64s(h.fb.f.getIID, want) {
		t.Fatalf("GetIssue called for %v, want %v (50 not examined after the cap filled)", h.fb.f.getIID, want)
	}
	assertBalances(t, out)
}

// TestFireSweepBackfillPastAlreadyRunningAndNoPRDLink proves backfill walks past BOTH a
// pre-check already_running skip (no forge call) and a no_prd_link skip, filling the cap
// from the eligible candidates beyond them.
func TestFireSweepBackfillPastAlreadyRunningAndNoPRDLink(t *testing.T) {
	h := newHarness()
	h.st.sweepRows = []store.ListSweepCandidateIssuesRow{
		{ForgeIssueIid: 10}, {ForgeIssueIid: 20}, {ForgeIssueIid: 30}, {ForgeIssueIid: 40}, {ForgeIssueIid: 50},
	}
	h.st.sweepCount = 5
	h.st.activeByIssue = map[int64]bool{20: true}                   // 20 already running (pre-check skip)
	h.runs.errByIssue = map[int64]error{30: workersvc.ErrNoPRDLink} // 30 has no PRD link
	out, err := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{Int32: 3, Valid: true}))
	if err != nil {
		t.Fatalf("backfill fire must not error, got %v", err)
	}
	if want := []int64{10, 40, 50}; !eqInt64s(startedIIDs(out), want) {
		t.Fatalf("started iids = %v, want %v (past already_running 20 and no_prd_link 30)", startedIIDs(out), want)
	}
	if len(out.Skips) != 2 {
		t.Fatalf("skips = %+v, want two (already_running 20, no_prd_link 30)", out.Skips)
	}
	byIID := map[int64]SkipReason{}
	for _, s := range out.Skips {
		if s.IssueIID != nil {
			byIID[*s.IssueIID] = s.Reason
		}
	}
	if byIID[20] != SkipAlreadyRunning || byIID[30] != SkipNoPRDLink {
		t.Fatalf("skip reasons = %v, want {20:already_running, 30:no_prd_link}", byIID)
	}
	if out.Matched != 5 {
		t.Fatalf("Matched = %d, want 5 (examined 10,20,30,40,50)", out.Matched)
	}
	// 20 is skipped by the active-run pre-check WITHOUT a forge call; all others are fetched.
	if want := []int64{10, 30, 40, 50}; !eqInt64s(h.fb.f.getIID, want) {
		t.Fatalf("GetIssue called for %v, want %v (20 skipped pre-forge)", h.fb.f.getIID, want)
	}
	assertBalances(t, out)
}

// TestFireSweepBackfillScanBound: a head of all-ineligible candidates under-fills the fire
// (starts fewer than max_issues) and the forge cost is bounded by the scan window. Two
// fake caveats (PRD M2): the fake store applies no LIMIT, so we (a) assert the THREADED
// limit param is max_issues+backfillHeadroom (the real truncation is covered by the live-DB
// test), and (b) hand the fake exactly the window's worth of rows.
func TestFireSweepBackfillScanBound(t *testing.T) {
	const maxIssues = 3
	window := maxIssues + backfillHeadroom
	rows := make([]store.ListSweepCandidateIssuesRow, 0, window)
	errs := map[int64]error{}
	for i := 0; i < window; i++ {
		iid := int64(100 + i)
		rows = append(rows, store.ListSweepCandidateIssuesRow{ForgeIssueIid: iid})
		errs[iid] = workersvc.ErrNoPRDLink // the whole window is ineligible
	}
	h := newHarness()
	h.st.sweepRows = rows
	h.st.sweepCount = int64(window + 5) // more matching issues exist beyond the window → Capped
	h.runs.errByIssue = errs
	out, err := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{Int32: maxIssues, Valid: true}))
	if err != nil {
		t.Fatalf("scan-bound fire must not error, got %v", err)
	}
	// The threaded LIMIT is the scan window, not the cap (the bound the live-DB test verifies
	// truncates for real).
	if got := h.st.sweepMaxIssuesParam; !got.Valid || got.Int32 != int32(window) {
		t.Fatalf("threaded max_issues param = %+v, want {Int32:%d Valid:true}", got, window)
	}
	if len(out.Started) != 0 {
		t.Fatalf("all-ineligible head must start nothing, got %d started", len(out.Started))
	}
	if len(out.Skips) != window || out.Matched != window {
		t.Fatalf("Matched=%d skips=%d, want %d each (examined the whole window, no early break)", out.Matched, len(out.Skips), window)
	}
	// Forge calls are bounded by the window — the fire does not walk past it.
	if len(h.fb.f.getIID) != window {
		t.Fatalf("GetIssue calls = %d, want %d (bounded by the scan window)", len(h.fb.f.getIID), window)
	}
	if !out.Capped {
		t.Fatalf("Capped = false, want true (more matching issues than the scan window reached)")
	}
	assertBalances(t, out)
}

// TestFireSweepBackfillNullCapUnchanged: a NULL cap threads NULL (unlimited, not widened),
// has no started ceiling (no early break), and still records skips inline — it examines
// every candidate and starts all it can, exactly as before issue #416.
func TestFireSweepBackfillNullCapUnchanged(t *testing.T) {
	h := newHarness()
	h.st.sweepRows = []store.ListSweepCandidateIssuesRow{
		{ForgeIssueIid: 10}, {ForgeIssueIid: 20}, {ForgeIssueIid: 30},
	}
	h.runs.errByIssue = map[int64]error{20: workersvc.ErrNoPRDLink}
	out, _ := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{})) // NULL cap
	if h.st.sweepMaxIssuesParam.Valid {
		t.Fatalf("NULL cap must thread an invalid (NULL, unlimited) param, not a widened window: got %+v", h.st.sweepMaxIssuesParam)
	}
	if want := []int64{10, 30}; !eqInt64s(startedIIDs(out), want) {
		t.Fatalf("started iids = %v, want %v (all eligible started, 20 skipped)", startedIIDs(out), want)
	}
	if out.Matched != 3 || len(out.Skips) != 1 {
		t.Fatalf("Matched=%d skips=%d, want 3 and 1 (examined all, no cap ceiling)", out.Matched, len(out.Skips))
	}
	assertBalances(t, out)
}

// ── Persisted last_fire (PRD #308 M2) ────────────────────────────────────────

// TestTickPersistsLastFireOnSuccess pins the M2 threading: a success/benign fire on the
// tick path must persist a NON-nil last_fire into AdvanceSchedule, and the serialized
// bytes must decode to the fire's matched/started/skips/capped. The fake store records the
// AdvanceScheduleParams, so we read the captured LastFire and unmarshal it.
func TestTickPersistsLastFireOnSuccess(t *testing.T) {
	h := newHarness()
	h.fb.f.issue.Title = "Ship it"
	h.st.due = []store.RunSchedule{h.issueSchedule()} // auto-approve issue, success

	h.sched.Boot(context.Background())

	if len(h.st.advanceCalls) != 1 {
		t.Fatalf("advance calls = %d, want 1", len(h.st.advanceCalls))
	}
	raw := h.st.advanceCalls[0].LastFire
	if raw == nil {
		t.Fatalf("success fire must persist a non-nil last_fire, got nil")
	}
	var rec lastFireRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("last_fire is not valid JSON: %v (%s)", err, raw)
	}
	if rec.Matched != 1 || len(rec.Started) != 1 || len(rec.Skips) != 0 || rec.Capped {
		t.Fatalf("last_fire = %+v, want matched:1 started:1 skips:0 capped:false", rec)
	}
	st := rec.Started[0]
	if st.IssueIID == nil || *st.IssueIID != 7 || st.Title != "Ship it" || st.RunID == "" {
		t.Fatalf("last_fire started = %+v, want iid 7 / title / non-empty run id", st)
	}
	// FiredAt is the advance instant (the scheduler's now).
	if !rec.FiredAt.Equal(h.now) {
		t.Fatalf("last_fire fired_at = %v, want the scheduler now %v", rec.FiredAt, h.now)
	}
	// The empty-slice convention: started/skips serialize as [] not null, so a client sees
	// a present array even when a bucket is empty.
	if !strings.Contains(string(raw), `"skips":[]`) {
		t.Fatalf("last_fire must encode empty skips as [] not null, got %s", raw)
	}
}

// TestTickBenignSkipPersistsLastFire proves a benign dedup skip (which STILL advances) also
// persists last_fire, carrying the typed skip reason — so a schedule that only skipped is
// still observable.
func TestTickBenignSkipPersistsLastFire(t *testing.T) {
	h := newHarness()
	h.st.activeIssue = true // prior run live → already_running skip, still advances
	h.st.due = []store.RunSchedule{h.issueSchedule()}

	h.sched.Boot(context.Background())

	if len(h.st.advanceCalls) != 1 {
		t.Fatalf("benign skip must still advance: advance calls = %d, want 1", len(h.st.advanceCalls))
	}
	raw := h.st.advanceCalls[0].LastFire
	if raw == nil {
		t.Fatalf("benign skip must still persist a last_fire, got nil")
	}
	var rec lastFireRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("last_fire is not valid JSON: %v (%s)", err, raw)
	}
	if rec.Matched != 1 || len(rec.Started) != 0 || len(rec.Skips) != 1 {
		t.Fatalf("last_fire = %+v, want matched:1 started:0 skips:1", rec)
	}
	if rec.Skips[0].Reason != string(SkipAlreadyRunning) {
		t.Fatalf("last_fire skip reason = %q, want %q", rec.Skips[0].Reason, SkipAlreadyRunning)
	}
}

// TestTickEmptySweepPersistsLastFire covers Decision 4: a matched:0 empty-label sweep is a
// legitimate, observable outcome — it must persist a valid last_fire with matched 0 and
// empty (non-null) started/skips, not skip the write.
func TestTickEmptySweepPersistsLastFire(t *testing.T) {
	h := newHarness()
	h.st.sweepRows = nil // no candidates matched
	h.st.due = []store.RunSchedule{h.sweepSchedule(pgtype.Int4{})}

	h.sched.Boot(context.Background())

	if len(h.st.advanceCalls) != 1 {
		t.Fatalf("empty sweep must advance: advance calls = %d, want 1", len(h.st.advanceCalls))
	}
	raw := h.st.advanceCalls[0].LastFire
	if raw == nil {
		t.Fatalf("empty sweep must persist a last_fire (matched:0 is legitimate), got nil")
	}
	var rec lastFireRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("last_fire is not valid JSON: %v (%s)", err, raw)
	}
	if rec.Matched != 0 || len(rec.Started) != 0 || len(rec.Skips) != 0 {
		t.Fatalf("empty sweep last_fire = %+v, want matched:0 started:[] skips:[]", rec)
	}
	if !strings.Contains(string(raw), `"started":[]`) || !strings.Contains(string(raw), `"skips":[]`) {
		t.Fatalf("empty sweep last_fire must encode [] not null, got %s", raw)
	}
}

// TestTickTransientDoesNotPersistLastFire pins Decision 5: a transient fire error does NOT
// advance, so AdvanceSchedule (the only last_fire write site) is never called and the prior
// last_fire is untouched.
func TestTickTransientDoesNotPersistLastFire(t *testing.T) {
	h := newHarness()
	h.fb.err = context.DeadlineExceeded // transient
	h.st.due = []store.RunSchedule{h.issueSchedule()}

	h.sched.Boot(context.Background())

	if len(h.st.advanceCalls) != 0 {
		t.Fatalf("transient error must NOT write last_fire (no advance): advance calls = %d, want 0", len(h.st.advanceCalls))
	}
}

// TestTickParkDoesNotPersistLastFire pins Decision 5: a permanent (park) fire error routes
// to SetRunScheduleStatus, NOT AdvanceSchedule, so last_fire is left untouched.
func TestTickParkDoesNotPersistLastFire(t *testing.T) {
	h := newHarness()
	h.st.repoErr = pgx.ErrNoRows // repo gone → permanent park
	h.st.due = []store.RunSchedule{h.issueSchedule()}

	h.sched.Boot(context.Background())

	if len(h.st.advanceCalls) != 0 {
		t.Fatalf("park must NOT write last_fire (no advance): advance calls = %d, want 0", len(h.st.advanceCalls))
	}
	if len(h.st.statusCalls) != 1 || h.st.statusCalls[0].Status != "error" {
		t.Fatalf("park must SetRunScheduleStatus to error, got %+v", h.st.statusCalls)
	}
}

// TestMarshalLastFire is a direct unit check on the wire shape: the exact json tags
// (the M3/CLI/web contract), the run-id-as-string and reason-as-string projections, and
// the non-nil empty-slice convention.
func TestMarshalLastFire(t *testing.T) {
	iid := int64(7)
	runID := uuid.New()
	const startedURL = "https://forge/x/-/issues/7"
	out := FireOutcome{
		Matched: 2,
		Capped:  true,
		// PRD #411 M3: a fetched issue's Started carries the snapshotted web_url; a
		// pre-fetch skip (here the prompt-style Skip) carries an empty URL.
		Started: []Started{{IssueIID: &iid, RunID: runID, Title: "Do it", WebURL: startedURL}},
		Skips:   []Skip{{IssueIID: nil, Title: "prompt", Reason: SkipAlreadyRunning, WebURL: ""}},
	}
	firedAt := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	raw, err := marshalLastFire(out, firedAt)
	if err != nil {
		t.Fatalf("marshalLastFire err = %v", err)
	}
	// Assert the exact tag keys are present (the persisted contract). web_url is the
	// PRD #411 M3 addition to both started and skip rows.
	for _, key := range []string{`"fired_at"`, `"matched"`, `"capped"`, `"started"`, `"skips"`,
		`"issue_iid"`, `"run_id"`, `"title"`, `"reason"`, `"web_url"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("last_fire JSON missing key %s, got %s", key, raw)
		}
	}
	var rec lastFireRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.Matched != 2 || !rec.Capped {
		t.Fatalf("matched/capped = %d/%v, want 2/true", rec.Matched, rec.Capped)
	}
	if len(rec.Started) != 1 || rec.Started[0].RunID != runID.String() {
		t.Fatalf("started run id = %+v, want the uuid string %s", rec.Started, runID)
	}
	if rec.Started[0].IssueIID == nil || *rec.Started[0].IssueIID != 7 {
		t.Fatalf("started issue_iid = %v, want 7", rec.Started[0].IssueIID)
	}
	// PRD #411 M3: the started row round-trips its snapshotted web_url. Non-vacuity:
	// flip startedURL to a wrong string and this fails.
	if rec.Started[0].WebURL != startedURL {
		t.Fatalf("started web_url = %q, want %q", rec.Started[0].WebURL, startedURL)
	}
	if len(rec.Skips) != 1 || rec.Skips[0].Reason != string(SkipAlreadyRunning) || rec.Skips[0].IssueIID != nil {
		t.Fatalf("skip = %+v, want already_running / nil iid", rec.Skips)
	}
	// PRD #411 M3: a pre-fetch skip carries NO web_url (degrades to a plain #<iid>).
	if rec.Skips[0].WebURL != "" {
		t.Fatalf("skip web_url = %q, want empty (pre-fetch skip has no snapshotted URL)", rec.Skips[0].WebURL)
	}

	// The empty-outcome case: non-nil [] arrays, not null.
	rawEmpty, err := marshalLastFire(FireOutcome{Matched: 0}, firedAt)
	if err != nil {
		t.Fatalf("marshalLastFire(empty) err = %v", err)
	}
	if !strings.Contains(string(rawEmpty), `"started":[]`) || !strings.Contains(string(rawEmpty), `"skips":[]`) {
		t.Fatalf("empty outcome must encode [] not null, got %s", rawEmpty)
	}
}

// TestTickWebURLPresentIffFetched pins PRD #411 M3's core invariant end-to-end through the
// persisted last_fire jsonb: a fire that FETCHES the issue snapshots its web_url onto the
// started row, and a fire whose pre-fetch active-run skip fires BEFORE GetIssue carries NO
// url (a #<iid> that degrades to a plain number). Both cases drive the real tick path
// (Boot → fireIssue → advance → marshalLastFire) and read the captured AdvanceSchedule
// bytes, so this covers the snapshot wiring, not just marshalLastFire's projection.
func TestTickWebURLPresentIffFetched(t *testing.T) {
	const url = "https://forge.e2e/-/issues/7"

	// Positive: a successful issue fire fetches the issue (fakeForge.GetIssue returns
	// h.fb.f.issue) and snapshots its WebURL onto the persisted started row.
	t.Run("fetched_started_carries_url", func(t *testing.T) {
		h := newHarness()
		h.fb.f.issue.Title = "Ship it"
		h.fb.f.issue.WebURL = url
		h.st.due = []store.RunSchedule{h.issueSchedule()} // auto-approve issue, success

		h.sched.Boot(context.Background())

		if len(h.st.advanceCalls) != 1 {
			t.Fatalf("success fire must advance once, got %d", len(h.st.advanceCalls))
		}
		var rec lastFireRecord
		if err := json.Unmarshal(h.st.advanceCalls[0].LastFire, &rec); err != nil {
			t.Fatalf("last_fire not valid JSON: %v", err)
		}
		if len(rec.Started) != 1 {
			t.Fatalf("last_fire = %+v, want one started row", rec)
		}
		// Non-vacuity: a bug that dropped issue.WebURL on the createIssueRun path (or a
		// wrong field) leaves this empty and fails here.
		if rec.Started[0].WebURL != url {
			t.Fatalf("started web_url = %q, want the fetched issue URL %q", rec.Started[0].WebURL, url)
		}
	})

	// Paired negative: the issue already has an active run, so the pre-check skip fires
	// BEFORE GetIssue is ever called (fireIssue returns already_running without fetching).
	// The benign skip still advances, so a last_fire is persisted — and its skip row must
	// carry an EMPTY web_url even though the (unfetched) fake issue has one set.
	t.Run("prefetch_skip_carries_no_url", func(t *testing.T) {
		h := newHarness()
		h.st.activeIssue = true   // prior run live → pre-check already_running skip, no GetIssue
		h.fb.f.issue.WebURL = url // set but must NOT leak: the fetch never happens
		h.st.due = []store.RunSchedule{h.issueSchedule()}

		h.sched.Boot(context.Background())

		if len(h.st.advanceCalls) != 1 {
			t.Fatalf("benign skip must still advance once, got %d", len(h.st.advanceCalls))
		}
		var rec lastFireRecord
		if err := json.Unmarshal(h.st.advanceCalls[0].LastFire, &rec); err != nil {
			t.Fatalf("last_fire not valid JSON: %v", err)
		}
		if len(rec.Skips) != 1 || rec.Skips[0].Reason != string(SkipAlreadyRunning) {
			t.Fatalf("last_fire = %+v, want one already_running skip", rec)
		}
		// The invariant: a pre-fetch skip carries no snapshotted URL. Confirm GetIssue was
		// never called, so "empty" is genuinely "unfetched" and not a fetch that returned "".
		if len(h.fb.f.getIID) != 0 {
			t.Fatalf("pre-check skip must NOT fetch the issue, GetIssue called for %v", h.fb.f.getIID)
		}
		if rec.Skips[0].WebURL != "" {
			t.Fatalf("pre-fetch skip web_url = %q, want empty (issue never fetched)", rec.Skips[0].WebURL)
		}
	})
}

// ── Guidance composition (PRD #274 M3) ───────────────────────────────────────

// TestComposeRunDescriptionEmptyGuidance pins the byte-identical invariant: a schedule
// with no guidance (empty or whitespace-only) must produce exactly the body, with no
// delimiter, so absent guidance is indistinguishable from the pre-M3 description.
func TestComposeRunDescriptionEmptyGuidance(t *testing.T) {
	body := "the issue body\n\nwith paragraphs"
	for _, g := range []string{"", "   ", "\n\t \n"} {
		if got := composeRunDescription(body, g); got != body {
			t.Fatalf("composeRunDescription(body, %q) = %q, want body unchanged", g, got)
		}
	}
}

// TestComposeRunDescriptionAppendsDelineatedSection pins the composition shape: body,
// then a delimiter, then the fixed owner-guidance header, then the guidance — with the
// guidance clearly AFTER the body.
func TestComposeRunDescriptionAppendsDelineatedSection(t *testing.T) {
	body := "Fix the flaky test in the auth package."
	guidance := "Always add a failing test first; keep the diff small."
	got := composeRunDescription(body, guidance)

	if !strings.HasPrefix(got, body) {
		t.Fatalf("composed description must start with the body, got %q", got)
	}
	if !strings.Contains(got, "\n---\n") {
		t.Fatalf("composed description missing the --- delimiter, got %q", got)
	}
	if !strings.Contains(got, "provided by the schedule owner to steer HOW") {
		t.Fatalf("composed description missing the fixed owner-guidance header, got %q", got)
	}
	if !strings.Contains(got, guidance) {
		t.Fatalf("composed description missing the guidance text, got %q", got)
	}
	// Guidance must appear after the body and after the header.
	bodyEnd := strings.Index(got, body) + len(body)
	if gi := strings.Index(got, guidance); gi <= bodyEnd {
		t.Fatalf("guidance must appear after the body; bodyEnd=%d guidanceAt=%d", bodyEnd, gi)
	}
}

// TestComposeRunDescriptionTruncatesNearCap is the silent-skip hazard: a body just under
// the cap plus guidance must TRUNCATE the guidance to keep the composed total under the
// cap, still emitting (a prefix of) the guidance rather than dropping to the body alone
// (which would skip the issue via ErrDescriptionTooLarge downstream).
func TestComposeRunDescriptionTruncatesNearCap(t *testing.T) {
	const max = workersvc.MaxIssueDescriptionBytes
	body := strings.Repeat("a", max-5000) // ~5000 bytes of headroom
	guidance := strings.Repeat("g", 8*1024)

	got := composeRunDescription(body, guidance)

	if len(got) > max {
		t.Fatalf("composed description len = %d, must be <= cap %d (never over)", len(got), max)
	}
	if got == body {
		t.Fatalf("near-cap body dropped the guidance entirely (issue would still run but guidance lost); want a truncated guidance section")
	}
	if !strings.HasPrefix(got, body) {
		t.Fatalf("composed description must still start with the full body")
	}
	if !strings.Contains(got, "g") {
		t.Fatalf("truncated composition must still contain a prefix of the guidance")
	}
	if !strings.Contains(got, "[guidance truncated]") {
		t.Fatalf("truncated composition must carry the truncation marker")
	}
}

// TestComposeRunDescriptionBodyAtCapReturnsBodyUnchanged pins the no-room edge: when the
// body alone already meets/exceeds the cap there is no room for guidance, so the body is
// returned unchanged (createRun handles the oversized body exactly as before — guidance
// must not make it worse).
func TestComposeRunDescriptionBodyAtCapReturnsBodyUnchanged(t *testing.T) {
	const max = workersvc.MaxIssueDescriptionBytes
	body := strings.Repeat("a", max)
	if got := composeRunDescription(body, "some guidance"); got != body {
		t.Fatalf("body at cap must return body unchanged (len got %d, body %d)", len(got), len(body))
	}
}

// TestComposeRunDescriptionTruncatesOnRuneBoundary pins that truncation never splits a
// multibyte rune: the composed result stays valid UTF-8 even when the byte budget falls
// mid-rune.
func TestComposeRunDescriptionTruncatesOnRuneBoundary(t *testing.T) {
	const max = workersvc.MaxIssueDescriptionBytes
	body := strings.Repeat("a", max-5000)
	guidance := strings.Repeat("世", 4000) // 3 bytes each — a byte cut will land mid-rune

	got := composeRunDescription(body, guidance)
	if !utf8.ValidString(got) {
		t.Fatalf("composed description is not valid UTF-8 — truncation split a rune")
	}
	if len(got) > max {
		t.Fatalf("composed description len = %d, must be <= cap %d", len(got), max)
	}
}

// TestTruncateUTF8BacksUpToRuneStart is the direct boundary check on the truncation
// helper: a byte budget that lands inside a multibyte rune backs up to the rune start.
func TestTruncateUTF8BacksUpToRuneStart(t *testing.T) {
	s := "世界foo" // 世,界 are 3 bytes each; then ASCII
	cases := []struct {
		n    int
		want string
	}{
		{0, ""},
		{2, ""},    // mid first rune → back to 0
		{3, "世"},   // exact boundary
		{4, "世"},   // mid second rune → back to 3
		{5, "世"},   // still mid second rune
		{6, "世界"},  // exact boundary
		{7, "世界f"}, // ASCII boundary
		{100, s},   // n >= len → whole string
	}
	for _, c := range cases {
		if got := truncateUTF8(s, c.n); got != c.want {
			t.Fatalf("truncateUTF8(%q, %d) = %q, want %q", s, c.n, got, c.want)
		}
		if !utf8.ValidString(truncateUTF8(s, c.n)) {
			t.Fatalf("truncateUTF8(%q, %d) produced invalid UTF-8", s, c.n)
		}
	}
}

// TestTickIssueScheduleComposesGuidance is the seam-level proof that a schedule carrying
// guidance passes a COMPOSED description (body + delineated guidance) to the run-creation
// seam. The fake captures the description on the auto-approve (autopilot) bucket.
func TestTickIssueScheduleComposesGuidance(t *testing.T) {
	h := newHarness() // fake forge issue Description == "body"
	s := h.issueSchedule()
	s.Guidance = pgtype.Text{String: "prefer table-driven tests", Valid: true}
	h.st.due = []store.RunSchedule{s}

	h.sched.Boot(context.Background())

	if len(h.runs.autopilot) != 1 {
		t.Fatalf("auto-approve issue: CreateScheduledAutopilotRun calls = %d, want 1", len(h.runs.autopilot))
	}
	desc := h.runs.autopilot[0].description
	if !strings.HasPrefix(desc, "body") {
		t.Fatalf("composed description must start with the issue body, got %q", desc)
	}
	if !strings.Contains(desc, "prefer table-driven tests") {
		t.Fatalf("composed description must carry the schedule's guidance, got %q", desc)
	}
	if !strings.Contains(desc, "provided by the schedule owner to steer HOW") {
		t.Fatalf("composed description must carry the delineated owner-guidance header, got %q", desc)
	}
}

// TestTickIssueScheduleNoGuidancePassesRawBody pins that a schedule WITHOUT guidance
// passes the raw issue body to the seam, byte-identical to pre-M3 behaviour.
func TestTickIssueScheduleNoGuidancePassesRawBody(t *testing.T) {
	h := newHarness()
	h.st.due = []store.RunSchedule{h.issueSchedule()} // no guidance

	h.sched.Boot(context.Background())

	if len(h.runs.autopilot) != 1 {
		t.Fatalf("auto-approve issue: CreateScheduledAutopilotRun calls = %d, want 1", len(h.runs.autopilot))
	}
	if desc := h.runs.autopilot[0].description; desc != "body" {
		t.Fatalf("no-guidance description = %q, want the raw body %q", desc, "body")
	}
}

// ── PRD #589 M2: default-origin catalog resolution at fire time ────────────────

// TestFirePromptDefaultResolvesFromCatalog is the discriminating fire-resolution test the
// plan's risk section pins: a default-origin prompt row is deliberately POISONED with a
// bogus prompt column, and the fire MUST use the catalog prompt (resolved by catalog_slug),
// never the column. A same-bytes read-compare would pass whether or not resolution goes
// through the catalog; poisoning the column is what makes this test discriminate. It uses
// the REAL catalog (schedtmpl.BySlug via New's default), not a fake.
func TestFirePromptDefaultResolvesFromCatalog(t *testing.T) {
	h := newHarness()
	want, ok := schedtmpl.BySlug("docs-hygiene")
	if !ok {
		t.Fatal("docs-hygiene catalog entry missing")
	}
	sched := store.RunSchedule{
		ID:          uuid.New(),
		UserID:      h.owner,
		RepoID:      h.repoID,
		Target:      "prompt",
		Origin:      "default",
		CatalogSlug: pgtype.Text{String: "docs-hygiene", Valid: true},
		Prompt:      pgtype.Text{String: "POISON-DO-NOT-USE", Valid: true},
		Timing:      "recurring",
		CronExpr:    pgtype.Text{String: "0 * * * *", Valid: true},
		Timezone:    "UTC",
		AutoApprove: true,
		Status:      "active",
		Enabled:     true,
	}

	out, err := h.sched.firePrompt(context.Background(), sched)
	if err != nil {
		t.Fatalf("firePrompt: %v", err)
	}
	if len(out.Started) != 1 {
		t.Fatalf("started = %d, want 1", len(out.Started))
	}
	if len(h.runs.prompts) != 1 {
		t.Fatalf("CreatePromptRun calls = %d, want 1", len(h.runs.prompts))
	}
	got := h.runs.prompts[0].prompt
	if got != want.Prompt {
		t.Fatalf("resolved prompt = %q, want the catalog prompt", got)
	}
	if strings.Contains(got, "POISON") {
		t.Fatalf("prompt was read from the row column, not the catalog: %q", got)
	}
	// The derived title must also come from the resolved prompt, not the poison column.
	if strings.Contains(h.runs.prompts[0].title, "POISON") {
		t.Fatalf("title derived from the poison column: %q", h.runs.prompts[0].title)
	}
}

// TestFirePromptDefaultUnknownSlugParks proves a default prompt row whose catalog entry is
// gone parks permanently (errBadConfig), never tick-storms.
func TestFirePromptDefaultUnknownSlugParks(t *testing.T) {
	h := newHarness()
	sched := store.RunSchedule{
		ID:          uuid.New(),
		UserID:      h.owner,
		RepoID:      h.repoID,
		Target:      "prompt",
		Origin:      "default",
		CatalogSlug: pgtype.Text{String: "no-such-slug", Valid: true},
		Timing:      "recurring",
		CronExpr:    pgtype.Text{String: "0 * * * *", Valid: true},
		Timezone:    "UTC",
		Status:      "active",
		Enabled:     true,
	}
	_, err := h.sched.firePrompt(context.Background(), sched)
	if !errors.Is(err, ErrBadConfig) {
		t.Fatalf("unknown slug error = %v, want ErrBadConfig", err)
	}
	if len(h.runs.prompts) != 0 {
		t.Fatalf("a gone-catalog prompt row must not fire, got %d runs", len(h.runs.prompts))
	}
}

// TestFireSweepDefaultResolvesFromCatalog is the sweep analogue of the discriminating
// prompt test: a default-origin sweep row is POISONED with bogus labels and guidance
// columns, and the fire MUST use the catalog labels (into the candidate query) and the
// catalog guidance (into the composed run description), never the columns.
func TestFireSweepDefaultResolvesFromCatalog(t *testing.T) {
	h := newHarness()
	want, ok := schedtmpl.BySlug("bug-triage")
	if !ok {
		t.Fatal("bug-triage catalog entry missing")
	}
	if want.Guidance == "" {
		t.Fatal("bug-triage catalog entry has no guidance to discriminate on")
	}
	h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 7}}
	sched := store.RunSchedule{
		ID:          uuid.New(),
		UserID:      h.owner,
		RepoID:      h.repoID,
		Target:      "sweep",
		Origin:      "default",
		CatalogSlug: pgtype.Text{String: "bug-triage", Valid: true},
		Labels:      []byte(`["POISON-LABEL"]`),
		Guidance:    pgtype.Text{String: "POISON-GUIDANCE-DO-NOT-USE", Valid: true},
		Timing:      "recurring",
		CronExpr:    pgtype.Text{String: "0 * * * *", Valid: true},
		Timezone:    "UTC",
		AutoApprove: true,
		Status:      "active",
		Enabled:     true,
	}

	out, err := h.sched.fireSweep(context.Background(), sched)
	if err != nil {
		t.Fatalf("fireSweep: %v", err)
	}
	if len(out.Started) != 1 {
		t.Fatalf("started = %d, want 1", len(out.Started))
	}
	// The candidate query must be driven by the CATALOG labels, not the poison column.
	wantLabels, _ := json.Marshal(want.Labels)
	if string(h.st.sweepLabelParam) != string(wantLabels) {
		t.Fatalf("sweep label selector = %s, want catalog labels %s", h.st.sweepLabelParam, wantLabels)
	}
	if strings.Contains(string(h.st.sweepLabelParam), "POISON") {
		t.Fatalf("labels were read from the row column, not the catalog: %s", h.st.sweepLabelParam)
	}
	// The per-issue description must carry the CATALOG guidance, not the poison column.
	if len(h.runs.autopilot) != 1 {
		t.Fatalf("autopilot calls = %d, want 1", len(h.runs.autopilot))
	}
	desc := h.runs.autopilot[0].description
	if !strings.Contains(desc, want.Guidance) {
		t.Fatalf("run description missing catalog guidance; got %q", desc)
	}
	if strings.Contains(desc, "POISON") {
		t.Fatalf("guidance was read from the row column, not the catalog: %q", desc)
	}
}

// TestFireSweepDefaultUnknownSlugParks proves a default sweep row whose catalog entry is
// gone parks permanently (errBadConfig) before touching the forge/DB.
func TestFireSweepDefaultUnknownSlugParks(t *testing.T) {
	h := newHarness()
	sched := store.RunSchedule{
		ID:          uuid.New(),
		UserID:      h.owner,
		RepoID:      h.repoID,
		Target:      "sweep",
		Origin:      "default",
		CatalogSlug: pgtype.Text{String: "no-such-slug", Valid: true},
		Timing:      "recurring",
		CronExpr:    pgtype.Text{String: "0 * * * *", Valid: true},
		Timezone:    "UTC",
		Status:      "active",
		Enabled:     true,
	}
	_, err := h.sched.fireSweep(context.Background(), sched)
	if !errors.Is(err, ErrBadConfig) {
		t.Fatalf("unknown slug error = %v, want ErrBadConfig", err)
	}
	if len(h.runs.autopilot) != 0 || len(h.runs.runs) != 0 {
		t.Fatalf("a gone-catalog sweep row must not fire any run")
	}
}

// TestFirePromptUserRowUnchanged pins the risk invariant that a user-origin prompt row
// takes the identical old path — the prompt straight off the column, never the catalog.
func TestFirePromptUserRowUnchanged(t *testing.T) {
	h := newHarness()
	sched := store.RunSchedule{
		ID:          uuid.New(),
		UserID:      h.owner,
		RepoID:      h.repoID,
		Target:      "prompt",
		Origin:      "user",
		Prompt:      pgtype.Text{String: "the user's own prompt", Valid: true},
		Timing:      "recurring",
		CronExpr:    pgtype.Text{String: "0 * * * *", Valid: true},
		Timezone:    "UTC",
		AutoApprove: true,
		Status:      "active",
		Enabled:     true,
	}
	if _, err := h.sched.firePrompt(context.Background(), sched); err != nil {
		t.Fatalf("firePrompt: %v", err)
	}
	if len(h.runs.prompts) != 1 {
		t.Fatalf("CreatePromptRun calls = %d, want 1", len(h.runs.prompts))
	}
	if got := h.runs.prompts[0].prompt; got != "the user's own prompt" {
		t.Fatalf("user-row prompt = %q, want the column verbatim", got)
	}
}

// ── issue #662: owner-guidance overlay for default prompt jobs ──────────────────

// TestFirePromptDefaultAppendsOwnerGuidance proves a default-origin prompt row with stored
// owner guidance fires with the catalog prompt composed with that guidance (the exact bytes
// composeRunDescription produces), while the derived title stays keyed to the RAW catalog
// prompt (not the composed instruction).
func TestFirePromptDefaultAppendsOwnerGuidance(t *testing.T) {
	h := newHarness()
	want, ok := schedtmpl.BySlug("docs-hygiene")
	if !ok {
		t.Fatal("docs-hygiene catalog entry missing")
	}
	const guidance = "Prefer small, reviewable diffs and skip generated files."
	sched := store.RunSchedule{
		ID:          uuid.New(),
		UserID:      h.owner,
		RepoID:      h.repoID,
		Target:      "prompt",
		Origin:      "default",
		CatalogSlug: pgtype.Text{String: "docs-hygiene", Valid: true},
		Guidance:    pgtype.Text{String: guidance, Valid: true},
		Timing:      "recurring",
		CronExpr:    pgtype.Text{String: "0 * * * *", Valid: true},
		Timezone:    "UTC",
		AutoApprove: true,
		Status:      "active",
		Enabled:     true,
	}

	if _, err := h.sched.firePrompt(context.Background(), sched); err != nil {
		t.Fatalf("firePrompt: %v", err)
	}
	if len(h.runs.prompts) != 1 {
		t.Fatalf("CreatePromptRun calls = %d, want 1", len(h.runs.prompts))
	}
	call := h.runs.prompts[0]
	wantInstruction := composeRunDescription(want.Prompt, guidance)
	if call.prompt != wantInstruction {
		t.Fatalf("instruction = %q, want the catalog prompt composed with guidance", call.prompt)
	}
	if wantInstruction == want.Prompt {
		t.Fatal("test is vacuous: composed instruction equals the raw prompt (guidance not applied)")
	}
	if !strings.Contains(call.prompt, guidance) {
		t.Fatalf("composed instruction missing the owner guidance: %q", call.prompt)
	}
	// Title stays keyed to the RAW catalog prompt, not the composed instruction.
	if call.title != promptTitle(want.Prompt) {
		t.Fatalf("title = %q, want the raw-prompt title", call.title)
	}
}

// TestFirePromptUserRowNoGuidanceOverlay proves the overlay is a no-op for a user-origin
// prompt row: guidance is never persisted for user rows, so guidanceOf returns "" and the
// instruction is byte-for-byte the prompt column.
func TestFirePromptUserRowNoGuidanceOverlay(t *testing.T) {
	h := newHarness()
	sched := store.RunSchedule{
		ID:          uuid.New(),
		UserID:      h.owner,
		RepoID:      h.repoID,
		Target:      "prompt",
		Origin:      "user",
		Prompt:      pgtype.Text{String: "the user's own prompt", Valid: true},
		Timing:      "recurring",
		CronExpr:    pgtype.Text{String: "0 * * * *", Valid: true},
		Timezone:    "UTC",
		AutoApprove: true,
		Status:      "active",
		Enabled:     true,
	}
	if _, err := h.sched.firePrompt(context.Background(), sched); err != nil {
		t.Fatalf("firePrompt: %v", err)
	}
	if len(h.runs.prompts) != 1 {
		t.Fatalf("CreatePromptRun calls = %d, want 1", len(h.runs.prompts))
	}
	if got := h.runs.prompts[0].prompt; got != "the user's own prompt" {
		t.Fatalf("user-row instruction = %q, want the column verbatim (no overlay)", got)
	}
}

// ── self_improve fire path (PRD #590 M1) ───────────────────────────────────────

// TestTickSelfImproveFiresFoldsAndAdvances is the happy path: a due self_improve schedule
// files the tracking issue, folds the OWNER's improve_uzi backlog into an auto-approved
// self_improve run threading the schedule's model, marks that backlog addressed, notifies
// started, and advances the schedule (recurring, stays active — no park).
func TestTickSelfImproveFiresFoldsAndAdvances(t *testing.T) {
	h := newHarness()
	rec := store.ListOpenImproveUziRecommendationsForUserRow{ID: uuid.New(), Target: "worker: jq", RationaleMd: "install jq"}
	h.st.siRecs = []store.ListOpenImproveUziRecommendationsForUserRow{rec}
	model := "fable"
	s := h.selfImproveSchedule()
	s.Model = pgtype.Text{String: model, Valid: true}
	s.OverrideSubagentModel = true
	h.st.due = []store.RunSchedule{s}

	h.sched.Boot(context.Background())

	if len(h.runs.selfImprove) != 1 {
		t.Fatalf("CreateSelfImproveRun calls = %d, want 1", len(h.runs.selfImprove))
	}
	call := h.runs.selfImprove[0]
	if call.userID != h.owner || call.repoID != h.repoID {
		t.Fatalf("self_improve call owner/repo = %v/%v, want %v/%v", call.userID, call.repoID, h.owner, h.repoID)
	}
	if call.issueIID != 7 {
		t.Fatalf("tracking issue iid = %d, want the freshly-created 7", call.issueIID)
	}
	if call.title != selfImproveTrackingTitle {
		t.Fatalf("run title = %q, want %q", call.title, selfImproveTrackingTitle)
	}
	// Tracking issue filed once (no open existing), carrying only the marker label.
	if h.fb.f.createCount != 1 {
		t.Fatalf("tracking issue created %d times, want 1", h.fb.f.createCount)
	}
	if len(h.fb.f.createdLabels) != 1 || h.fb.f.createdLabels[0] != SelfImproveTrackingLabel {
		t.Fatalf("tracking issue labels = %v, want only %q", h.fb.f.createdLabels, SelfImproveTrackingLabel)
	}
	// Backlog owner-scoped and folded into the description.
	if h.st.siRecsUserParam != h.owner {
		t.Fatalf("backlog scoped to user %v, want owner %v", h.st.siRecsUserParam, h.owner)
	}
	if !strings.Contains(call.description, "jq") {
		t.Fatalf("run description missing the backlog: %q", call.description)
	}
	// Exactly the listed backlog marked addressed by the new run.
	if len(h.st.markedIDs) != 1 || h.st.markedIDs[0] != rec.ID {
		t.Fatalf("marked ids = %v, want exactly the listed backlog", h.st.markedIDs)
	}
	if h.st.markedByRun != h.runs.selfImproveRun.ID {
		t.Fatalf("backlog marked by %v, want the new run %v", h.st.markedByRun, h.runs.selfImproveRun.ID)
	}
	// Per-schedule model + subagent opt-in threaded onto the run (PRD #300/#305).
	if call.model == nil || *call.model != model || !call.overrideSubagentModel {
		t.Fatalf("model threading = (%v, %v), want (%q, true)", call.model, call.overrideSubagentModel, model)
	}
	// Started notification, carrying the run id.
	if got := h.countKind("selfimprove_started"); got != 1 {
		t.Fatalf("selfimprove_started notifications = %d, want 1", got)
	}
	for _, n := range h.notif.notifications {
		if n.Kind == "selfimprove_started" && (n.RunID == nil || *n.RunID != h.runs.selfImproveRun.ID) {
			t.Fatalf("started notification RunID = %v, want %v", n.RunID, h.runs.selfImproveRun.ID)
		}
	}
	// Recurring advance, stays active; no park.
	if len(h.st.advanceCalls) != 1 || h.st.advanceCalls[0].Status != "active" {
		t.Fatalf("advance calls = %+v, want 1 active", h.st.advanceCalls)
	}
	if len(h.st.statusCalls) != 0 {
		t.Fatalf("self_improve happy path must not park: statusCalls = %+v", h.st.statusCalls)
	}
	// The per-repo active-run pre-check was scoped to the schedule's repo.
	if len(h.st.activeSelfImproveRepos) != 1 || h.st.activeSelfImproveRepos[0] != h.repoID {
		t.Fatalf("active pre-check repos = %v, want [%v]", h.st.activeSelfImproveRepos, h.repoID)
	}
}

// TestTickSelfImproveReusesOpenTrackingIssue pins that the newest OPEN tracking issue is
// reused rather than filing a rival.
func TestTickSelfImproveReusesOpenTrackingIssue(t *testing.T) {
	h := newHarness()
	h.fb.f.listResult = []forge.Issue{
		{IID: 3, State: "closed"},
		{IID: 11, State: "opened"},
		{IID: 9, State: "opened"},
	}
	h.st.due = []store.RunSchedule{h.selfImproveSchedule()}

	h.sched.Boot(context.Background())

	if h.fb.f.createCount != 0 {
		t.Fatalf("should reuse an open tracking issue, not create one (createCount=%d)", h.fb.f.createCount)
	}
	if len(h.runs.selfImprove) != 1 || h.runs.selfImprove[0].issueIID != 11 {
		t.Fatalf("reused issue = %+v, want the newest open one (11)", h.runs.selfImprove)
	}
}

// TestTickSelfImproveVaultLockedSkipsAndAdvances pins the vault-lock benign skip: no run,
// a selfimprove_skipped notification, and the schedule STILL advances (cadence re-fires on
// schedule once unlocked) rather than parking.
func TestTickSelfImproveVaultLockedSkipsAndAdvances(t *testing.T) {
	h := newHarness()
	h.vault.unlocked = false
	h.st.due = []store.RunSchedule{h.selfImproveSchedule()}

	h.sched.Boot(context.Background())

	if len(h.runs.selfImprove) != 0 {
		t.Fatalf("vault locked: created %d runs, want 0", len(h.runs.selfImprove))
	}
	if got := h.countKind("selfimprove_skipped"); got != 1 {
		t.Fatalf("vault locked: selfimprove_skipped notifications = %d, want 1", got)
	}
	// Item 4 (PRD #590 follow-up): the reworded body must state the next-scheduled-time retry
	// and no longer imply unlocking resumes the cycle soon (no old "to resume" phrasing). Assert
	// the positive wording too, so an empty or unrelated body cannot pass this check vacuously.
	body := selfImproveSkippedBody(t, h)
	if !strings.Contains(body, "It will try again at the next scheduled time") {
		t.Fatalf("vault-lock body = %q, want next-scheduled-time retry wording", body)
	}
	if strings.Contains(body, "to resume") {
		t.Fatalf("vault-lock body must not imply unlocking resumes it (no %q): %q", "to resume", body)
	}
	// The skip precedes any forge work.
	if h.fb.f.createCount != 0 {
		t.Fatalf("vault locked: tracking issue created %d, want 0", h.fb.f.createCount)
	}
	if len(h.st.advanceCalls) != 1 {
		t.Fatalf("vault locked: advance calls = %d, want 1 (benign skip advances)", len(h.st.advanceCalls))
	}
	if len(h.st.statusCalls) != 0 {
		t.Fatalf("vault locked must not park: statusCalls = %+v", h.st.statusCalls)
	}
}

// TestTickSelfImproveActiveRunSkipsQuietly pins the per-repo active-run skip: no run, no
// notification (mirrors firePrompt's active skip), no forge work, and a benign advance.
func TestTickSelfImproveActiveRunSkipsQuietly(t *testing.T) {
	h := newHarness()
	h.st.activeSelfImprove = 1
	h.st.due = []store.RunSchedule{h.selfImproveSchedule()}

	h.sched.Boot(context.Background())

	if len(h.runs.selfImprove) != 0 {
		t.Fatalf("active run: created %d, want 0", len(h.runs.selfImprove))
	}
	if len(h.notif.notifications) != 0 {
		t.Fatalf("active run: sent %d notifications, want 0", len(h.notif.notifications))
	}
	if h.fb.f.createCount != 0 {
		t.Fatalf("active run: tracking issue created %d, want 0 (skip precedes forge work)", h.fb.f.createCount)
	}
	if len(h.st.advanceCalls) != 1 {
		t.Fatalf("active run: advance calls = %d, want 1 (benign skip advances)", len(h.st.advanceCalls))
	}
}

// selfImproveSkippedBody returns the body of the single selfimprove_skipped notification,
// failing the test if there is not exactly one.
func selfImproveSkippedBody(t *testing.T, h *harness) string {
	t.Helper()
	var bodies []string
	for _, notif := range h.notif.notifications {
		if notif.Kind == "selfimprove_skipped" && notif.Slack != nil {
			bodies = append(bodies, notif.Slack.Body)
		}
	}
	if len(bodies) != 1 {
		t.Fatalf("selfimprove_skipped notifications = %d, want exactly 1", len(bodies))
	}
	return bodies[0]
}

// TestTickSelfImproveActiveRunWinsOverVaultLocked pins item 5's ordering (PRD #590
// follow-up): when a run is ALREADY active for the repo AND the vault is locked, the
// active-run pre-check runs first, so the fire is a quiet already_running skip — NOT a
// vault-locked skip. No selfimprove_skipped notification is emitted, no run is created, and
// the schedule still advances (benign skip). A regression that reordered these back would
// emit a spurious vault-locked notification here.
func TestTickSelfImproveActiveRunWinsOverVaultLocked(t *testing.T) {
	h := newHarness()
	h.vault.unlocked = false // vault locked
	h.st.activeSelfImprove = 1
	h.st.due = []store.RunSchedule{h.selfImproveSchedule()}

	h.sched.Boot(context.Background())

	if len(h.runs.selfImprove) != 0 {
		t.Fatalf("active run wins: created %d runs, want 0", len(h.runs.selfImprove))
	}
	if got := h.countKind("selfimprove_skipped"); got != 0 {
		t.Fatalf("active run wins: selfimprove_skipped notifications = %d, want 0 (active-run skip precedes vault-lock)", got)
	}
	if len(h.notif.notifications) != 0 {
		t.Fatalf("active run wins: sent %d notifications, want 0", len(h.notif.notifications))
	}
	if h.fb.f.createCount != 0 {
		t.Fatalf("active run wins: tracking issue created %d, want 0 (skip precedes forge work)", h.fb.f.createCount)
	}
	// Benign skip still advances, and its recorded skip reason is already_running, not the
	// vault-lock reason.
	if len(h.st.advanceCalls) != 1 {
		t.Fatalf("active run wins: advance calls = %d, want 1 (benign skip advances)", len(h.st.advanceCalls))
	}
	var rec lastFireRecord
	if err := json.Unmarshal(h.st.advanceCalls[0].LastFire, &rec); err != nil {
		t.Fatalf("last_fire not valid JSON: %v", err)
	}
	if len(rec.Skips) != 1 || rec.Skips[0].Reason != string(SkipAlreadyRunning) {
		t.Fatalf("last_fire = %+v, want exactly one already_running skip", rec)
	}
	if len(h.st.statusCalls) != 0 {
		t.Fatalf("active run wins must not park: statusCalls = %+v", h.st.statusCalls)
	}
}

// TestTickSelfImproveRepoGoneParks pins the permanent-park path: a gone/unowned repo (no
// row) parks the schedule at status='error' and does NOT advance it.
func TestTickSelfImproveRepoGoneParks(t *testing.T) {
	h := newHarness()
	h.st.repoErr = pgx.ErrNoRows
	h.st.due = []store.RunSchedule{h.selfImproveSchedule()}

	h.sched.Boot(context.Background())

	if len(h.runs.selfImprove) != 0 {
		t.Fatalf("repo gone: created %d, want 0", len(h.runs.selfImprove))
	}
	if len(h.st.statusCalls) != 1 || h.st.statusCalls[0].Status != "error" {
		t.Fatalf("repo gone: statusCalls = %+v, want 1 error park", h.st.statusCalls)
	}
	if len(h.st.advanceCalls) != 0 {
		t.Fatalf("repo gone: advance calls = %d, want 0 (parked, not advanced)", len(h.st.advanceCalls))
	}
}

// TestTickSelfImproveRaceSkips pins that a lost unique-index race (ErrActiveSelfImproveExists
// from the create seam) is a benign skip that advances, not a park.
func TestTickSelfImproveRaceSkips(t *testing.T) {
	h := newHarness()
	h.runs.err = workersvc.ErrActiveSelfImproveExists
	h.st.due = []store.RunSchedule{h.selfImproveSchedule()}

	h.sched.Boot(context.Background())

	if h.countKind("selfimprove_started") != 0 {
		t.Fatalf("a lost create race must not notify started")
	}
	if len(h.st.advanceCalls) != 1 {
		t.Fatalf("race skip: advance calls = %d, want 1 (benign skip advances)", len(h.st.advanceCalls))
	}
	if len(h.st.statusCalls) != 0 {
		t.Fatalf("race skip must not park: statusCalls = %+v", h.st.statusCalls)
	}
}

// selfImproveStartedBody returns the body of the single selfimprove_started notification,
// failing the test if there is not exactly one. It reads the Slack render body, mirroring
// selfImproveSkippedBody so the "started" and "skipped" body assertions share a shape.
func selfImproveStartedBody(t *testing.T, h *harness) string {
	t.Helper()
	var bodies []string
	for _, notif := range h.notif.notifications {
		if notif.Kind == "selfimprove_started" && notif.Slack != nil {
			bodies = append(bodies, notif.Slack.Body)
		}
	}
	if len(bodies) != 1 {
		t.Fatalf("selfimprove_started notifications = %d, want exactly 1", len(bodies))
	}
	return bodies[0]
}

// lastFireSkips unmarshals the single advance call's last_fire and returns its recorded
// skips, failing the test if there is not exactly one advance.
func lastFireSkips(t *testing.T, h *harness) []lastFireSkip {
	t.Helper()
	if len(h.st.advanceCalls) != 1 {
		t.Fatalf("advance calls = %d, want exactly 1", len(h.st.advanceCalls))
	}
	var rec lastFireRecord
	if err := json.Unmarshal(h.st.advanceCalls[0].LastFire, &rec); err != nil {
		t.Fatalf("last_fire not valid JSON: %v", err)
	}
	return rec.Skips
}

// TestTickSelfImproveFoldEmptyBacklogUsesFoldString pins PRD #686 M5 case (a): with the
// dogfood flag ON (the harness default) but an EMPTY improve_uzi backlog, the created run
// carries the fold-mode empty-backlog description ("Review the uzi codebase …") — NOT the
// generic const. This is the dogfood branch of the M2 generic-vs-fold split, exercised at
// its empty edge, so a regression that dropped fold mode into the generic const would
// redden here even though the non-empty fold test (…FiresFoldsAndAdvances) stays green.
func TestTickSelfImproveFoldEmptyBacklogUsesFoldString(t *testing.T) {
	h := newHarness()
	// Dogfood flag on (default), backlog empty (default siRecs is nil).
	if !h.st.repoRow.FoldImproveUziBacklog {
		t.Fatal("precondition: the harness default repo must be dogfood (FoldImproveUziBacklog=true)")
	}
	h.st.due = []store.RunSchedule{h.selfImproveSchedule()}

	h.sched.Boot(context.Background())

	if len(h.runs.selfImprove) != 1 {
		t.Fatalf("CreateSelfImproveRun calls = %d, want 1", len(h.runs.selfImprove))
	}
	desc := h.runs.selfImprove[0].description
	// The fold path was taken (backlog scoped to the owner), even though it was empty.
	if h.st.siRecsUserParam != h.owner {
		t.Fatalf("fold mode must query the owner backlog: siRecsUserParam = %v, want %v", h.st.siRecsUserParam, h.owner)
	}
	if !strings.Contains(desc, "Review the uzi codebase") {
		t.Fatalf("empty-fold description = %q, want the fold empty-backlog string", desc)
	}
	// It must be the fold string, not the generic const.
	if desc == genericSelfImproveDescription {
		t.Fatalf("empty-fold description must not be the generic const: %q", desc)
	}
	// Empty backlog ⇒ nothing to mark addressed.
	if len(h.st.markedIDs) != 0 {
		t.Fatalf("empty backlog: marked ids = %v, want none", h.st.markedIDs)
	}
}

// TestTickSelfImproveGenericSkipsBacklog pins PRD #686 M5 case (b): a NON-dogfood repo
// (FoldImproveUziBacklog=false) fires the GENERIC run — it never queries the improve_uzi
// backlog, never marks any addressed, and its description is the generic const carrying
// none of the uzi-specific wording. The negative assertions on the backlog query are the
// load-bearing part: the generic path must not touch the owner-scoped backlog at all.
func TestTickSelfImproveGenericSkipsBacklog(t *testing.T) {
	h := newHarness()
	h.st.repoRow.FoldImproveUziBacklog = false
	// Stage a backlog so a regression that fetched it anyway would be observable via the
	// description (it would fold "jq" in) rather than silently.
	h.st.siRecs = []store.ListOpenImproveUziRecommendationsForUserRow{
		{ID: uuid.New(), Target: "worker: jq", RationaleMd: "install jq"},
	}
	h.st.due = []store.RunSchedule{h.selfImproveSchedule()}

	h.sched.Boot(context.Background())

	if len(h.runs.selfImprove) != 1 {
		t.Fatalf("CreateSelfImproveRun calls = %d, want 1", len(h.runs.selfImprove))
	}
	// The backlog query was NOT called: siRecsUserParam stays the zero uuid while the
	// schedule's owner is non-zero (so a "== owner" regression is not masked by both
	// being zero).
	if h.owner == (uuid.UUID{}) {
		t.Fatal("precondition: the schedule owner must be non-zero for the not-called assertion to bite")
	}
	if h.st.siRecsUserParam != (uuid.UUID{}) {
		t.Fatalf("generic mode must NOT query the backlog: siRecsUserParam = %v, want the zero uuid", h.st.siRecsUserParam)
	}
	// MarkAddressed was not called.
	if len(h.st.markedIDs) != 0 {
		t.Fatalf("generic mode must not mark any backlog addressed: markedIDs = %v", h.st.markedIDs)
	}
	desc := h.runs.selfImprove[0].description
	if desc != genericSelfImproveDescription {
		t.Fatalf("generic description = %q, want the generic const %q", desc, genericSelfImproveDescription)
	}
	if strings.Contains(desc, "improve_uzi") || strings.Contains(desc, "uzi codebase") {
		t.Fatalf("generic description must carry no uzi-specific wording: %q", desc)
	}
	if strings.Contains(desc, "jq") {
		t.Fatalf("generic description leaked the staged backlog: %q", desc)
	}
}

// TestTickSelfImproveStartedNotificationNamesRepo pins PRD #686 M5 case (c) / M2: the
// started notification's BODY names the target repo (PathWithNamespace), in both modes.
// Asserted on the body, not just the kind, because the repo-named notification is the
// observable behavior the notifier-renderer test cannot reach (Risks §"Notification test
// is NOT gate-forcing").
func TestTickSelfImproveStartedNotificationNamesRepo(t *testing.T) {
	h := newHarness() // default repo path_with_namespace = "vtmocanu/uzi"
	h.st.due = []store.RunSchedule{h.selfImproveSchedule()}

	h.sched.Boot(context.Background())

	body := selfImproveStartedBody(t, h)
	if !strings.Contains(body, "vtmocanu/uzi") {
		t.Fatalf("started notification body = %q, want it to name the repo %q", body, "vtmocanu/uzi")
	}
}

// siMRRow builds a candidate row for the open-MR cap with a valid mr_iid.
func siMRRow(mrIID int64) store.RecentSelfImproveMRRunsForRepoRow {
	return store.RecentSelfImproveMRRunsForRepoRow{
		ID:    uuid.New(),
		MrIid: pgtype.Int8{Int64: mrIID, Valid: true},
	}
}

// TestTickSelfImproveOpenMRCapSkips pins PRD #686 M5 Part B / D10: with K (=2) candidate
// self-improve MRs the forge reports as OPEN, the cycle is SKIPPED — no run, no forge
// write (no tracking issue created), a selfimprove_skipped notification whose BODY says
// "open-MR cap reached" (asserted on the body, not the shared kind — the vault skip reuses
// the kind, N6), a benign advance, and a last_fire skip recording SkipSelfImproveMRCapReached.
func TestTickSelfImproveOpenMRCapSkips(t *testing.T) {
	h := newHarness()
	h.st.siMRRuns = []store.RecentSelfImproveMRRunsForRepoRow{siMRRow(101), siMRRow(102)}
	h.fb.f.mrStateByIID = map[int64]string{101: forge.MRStateOpened, 102: forge.MRStateOpened}
	h.st.due = []store.RunSchedule{h.selfImproveSchedule()}

	h.sched.Boot(context.Background())

	if len(h.runs.selfImprove) != 0 {
		t.Fatalf("cap reached: created %d runs, want 0", len(h.runs.selfImprove))
	}
	// The cap precedes any forge write.
	if h.fb.f.createCount != 0 {
		t.Fatalf("cap reached: tracking issue created %d, want 0 (cap precedes forge write)", h.fb.f.createCount)
	}
	// Both candidates were checked live against the forge.
	if len(h.fb.f.getMRIID) != 2 {
		t.Fatalf("cap: GetMergeRequest calls = %v, want both candidates checked", h.fb.f.getMRIID)
	}
	// The skip is announced on the BODY, not merely the kind (the vault skip shares the kind).
	body := selfImproveSkippedBody(t, h)
	if !strings.Contains(body, "open-MR cap reached") {
		t.Fatalf("cap skip body = %q, want it to state the open-MR cap", body)
	}
	// Benign skip: advances, does not park, and records the cap skip reason.
	skips := lastFireSkips(t, h)
	if len(skips) != 1 || skips[0].Reason != string(SkipSelfImproveMRCapReached) {
		t.Fatalf("last_fire skips = %+v, want exactly one %q", skips, SkipSelfImproveMRCapReached)
	}
	if len(h.st.statusCalls) != 0 {
		t.Fatalf("cap skip must not park: statusCalls = %+v", h.st.statusCalls)
	}
}

// TestTickSelfImproveBelowCapFires pins the K-1 edge: 1 OPEN self-improve MR plus one that
// is MERGED and one that is CLOSED (below the cap of 2) → the cycle FIRES normally. The
// merged/closed candidates prove non-open MRs are excluded from the count rather than
// wedging the cap.
func TestTickSelfImproveBelowCapFires(t *testing.T) {
	h := newHarness()
	h.st.siMRRuns = []store.RecentSelfImproveMRRunsForRepoRow{siMRRow(201), siMRRow(202), siMRRow(203)}
	h.fb.f.mrStateByIID = map[int64]string{
		201: forge.MRStateOpened,
		202: forge.MRStateMerged,
		203: forge.MRStateClosed,
	}
	h.st.due = []store.RunSchedule{h.selfImproveSchedule()}

	h.sched.Boot(context.Background())

	if len(h.runs.selfImprove) != 1 {
		t.Fatalf("below cap (1 open, 1 merged, 1 closed): created %d runs, want 1", len(h.runs.selfImprove))
	}
	if got := h.countKind("selfimprove_skipped"); got != 0 {
		t.Fatalf("below cap must not skip: selfimprove_skipped notifications = %d, want 0", got)
	}
	if got := h.countKind("selfimprove_started"); got != 1 {
		t.Fatalf("below cap: selfimprove_started notifications = %d, want 1", got)
	}
}

// TestTickSelfImproveCapForgeErrorRetriesTransiently pins PRD #686 M5 case (e): a
// GetMergeRequest error while checking a candidate's open-state abandons the whole cycle
// as TRANSIENT — fireSelfImprove returns a non-nil error, no run is created, and the tick
// path neither advances nor parks (it retries next tick). RunNow is used to read the
// returned error directly; a second harness confirms the tick-path (no advance / no park).
func TestTickSelfImproveCapForgeErrorRetriesTransiently(t *testing.T) {
	// Direct RunNow: the returned error is non-nil and transient (not a permanent sentinel).
	h := newHarness()
	h.st.siMRRuns = []store.RecentSelfImproveMRRunsForRepoRow{siMRRow(301)}
	h.fb.f.mrErr = errors.New("forge 502")
	out, err := h.sched.RunNow(context.Background(), h.selfImproveSchedule())
	if err == nil {
		t.Fatal("a GetMergeRequest error must surface as a non-nil (transient) fire error")
	}
	if errors.Is(err, workersvc.ErrRepoNotFound) || errors.Is(err, workersvc.ErrGuardrailBlocked) {
		t.Fatalf("cap forge error must be transient, not a permanent park sentinel: %v", err)
	}
	if len(out.Started) != 0 || len(h.runs.selfImprove) != 0 {
		t.Fatalf("cap forge error: created %d runs / %d started, want 0/0", len(h.runs.selfImprove), len(out.Started))
	}

	// Tick path: a transient fire error neither advances nor parks the schedule.
	h2 := newHarness()
	h2.st.siMRRuns = []store.RecentSelfImproveMRRunsForRepoRow{siMRRow(301)}
	h2.fb.f.mrErr = errors.New("forge 502")
	h2.st.due = []store.RunSchedule{h2.selfImproveSchedule()}
	h2.sched.Boot(context.Background())
	if len(h2.st.advanceCalls) != 0 {
		t.Fatalf("transient cap error must NOT advance: advanceCalls = %+v", h2.st.advanceCalls)
	}
	if len(h2.st.statusCalls) != 0 {
		t.Fatalf("transient cap error must NOT park: statusCalls = %+v", h2.st.statusCalls)
	}
}
