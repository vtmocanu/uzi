package schedsvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/notifysvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// ── Fakes ────────────────────────────────────────────────────────────────────

type fakeStore struct {
	due []store.RunSchedule

	advanceCalls []store.AdvanceScheduleParams
	statusCalls  []store.SetRunScheduleStatusParams

	repoErr error
	repoRow store.GetRepoForUserRow

	activeIssue     bool
	activeIssueErr  error
	activeSchedule  bool
	sweepRows       []store.ListSweepCandidateIssuesRow
	sweepLabelParam []byte
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
	return f.sweepRows, nil
}
func (f *fakeStore) HasActiveRunForIssue(_ context.Context, _ store.HasActiveRunForIssueParams) (bool, error) {
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
}

type promptCall struct {
	userID, repoID, scheduleID uuid.UUID
	title, prompt              string
	autoApprove, waitOnLimit   bool
}

type fakeRuns struct {
	autopilot []autopilotCall
	runs      []runCall
	prompts   []promptCall
	err       error
}

func (f *fakeRuns) CreateRun(_ context.Context, userID, repoID uuid.UUID, issueIID int64, _ string, allowWithoutPRD bool, waitOnLimit *bool, _ *workersvc.SeededPlan) (store.Run, error) {
	if f.err != nil {
		return store.Run{}, f.err
	}
	f.runs = append(f.runs, runCall{userID, repoID, issueIID, allowWithoutPRD, waitOnLimit, false})
	return store.Run{ID: uuid.New()}, nil
}
func (f *fakeRuns) CreateScheduledRun(_ context.Context, userID, repoID uuid.UUID, issueIID int64, _ string, allowWithoutPRD bool, waitOnLimit *bool, _ *workersvc.SeededPlan) (store.Run, error) {
	// The non-auto-approve scheduled path (PRD #196): recorded in the same `runs`
	// bucket as CreateRun so the existing wait-on-limit / path-selection count
	// assertions still observe it, but tagged scheduled=true so a test can prove the
	// scheduler routed here (waiver-free) rather than through CreateRun.
	if f.err != nil {
		return store.Run{}, f.err
	}
	f.runs = append(f.runs, runCall{userID, repoID, issueIID, allowWithoutPRD, waitOnLimit, true})
	return store.Run{ID: uuid.New()}, nil
}
func (f *fakeRuns) CreateAutopilotRun(_ context.Context, userID, repoID uuid.UUID, issueIID int64, description string, allowWithoutPRD bool) (store.Run, error) {
	if f.err != nil {
		return store.Run{}, f.err
	}
	// nil waitOnLimit: the poller seam has no per-run choice (owner default). Kept so the
	// interface stays satisfied even though the scheduler no longer calls it.
	f.autopilot = append(f.autopilot, autopilotCall{userID, repoID, issueIID, description, allowWithoutPRD, nil})
	return store.Run{ID: uuid.New()}, nil
}
func (f *fakeRuns) CreateScheduledAutopilotRun(_ context.Context, userID, repoID uuid.UUID, issueIID int64, description string, allowWithoutPRD bool, waitOnLimit *bool) (store.Run, error) {
	// The auto-approve scheduled path (PRD #274 Decision 1a): recorded in the same
	// `autopilot` bucket as CreateAutopilotRun so the existing count assertions still
	// observe it, but it CAPTURES waitOnLimit (which CreateAutopilotRun drops) so a test
	// can prove the schedule's persisted wait_on_limit is threaded through.
	if f.err != nil {
		return store.Run{}, f.err
	}
	f.autopilot = append(f.autopilot, autopilotCall{userID, repoID, issueIID, description, allowWithoutPRD, waitOnLimit})
	return store.Run{ID: uuid.New()}, nil
}
func (f *fakeRuns) CreatePromptRun(_ context.Context, userID, repoID, scheduleID uuid.UUID, title, prompt string, autoApprove, waitOnLimit bool) (store.Run, error) {
	if f.err != nil {
		return store.Run{}, f.err
	}
	f.prompts = append(f.prompts, promptCall{userID, repoID, scheduleID, title, prompt, autoApprove, waitOnLimit})
	return store.Run{ID: uuid.New()}, nil
}

// fakeForge embeds forge.Forge (nil) and overrides only GetIssue, so any other call
// panics loudly rather than silently passing.
type fakeForge struct {
	forge.Forge
	issue forge.Issue
	err   error
}

func (f *fakeForge) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	return f.issue, f.err
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

// ── Harness ──────────────────────────────────────────────────────────────────

type harness struct {
	st    *fakeStore
	runs  *fakeRuns
	fb    *fakeBuilder
	set   *fakeSettings
	notif *fakeNotifier
	sched *Scheduler
	now   time.Time

	owner  uuid.UUID
	repoID uuid.UUID
}

func newHarness() *harness {
	owner := uuid.New()
	repoID := uuid.New()
	h := &harness{
		st:     &fakeStore{repoRow: store.GetRepoForUserRow{ID: repoID, UserID: owner, ForgeProjectID: 42, ForgeType: "gitlab"}},
		runs:   &fakeRuns{},
		fb:     &fakeBuilder{f: &fakeForge{issue: forge.Issue{IID: 7, Description: "body", Labels: []string{"PRD"}}}},
		set:    &fakeSettings{prdLabel: "PRD"},
		notif:  &fakeNotifier{},
		now:    time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
		owner:  owner,
		repoID: repoID,
	}
	h.sched = New(h.st, h.runs, h.fb, h.set, h.notif, time.Minute, nil)
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
	h.st.due = []store.RunSchedule{s}

	h.sched.Boot(context.Background())

	if len(h.runs.prompts) != 1 {
		t.Fatalf("prompt schedule: CreatePromptRun calls = %d, want 1", len(h.runs.prompts))
	}
	p := h.runs.prompts[0]
	if p.userID != h.owner || p.repoID != h.repoID || p.scheduleID != s.ID {
		t.Fatalf("prompt call ids = %+v, want owner/repo/schedule", p)
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
