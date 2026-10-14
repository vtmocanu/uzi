package schedsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// ciForge is the fake forge the ci_watch detector tests drive: it embeds forgetest.BaseFake and
// overrides only the reads a fire makes. It also records whether every budgeted read carried
// the per-fire CI read limits (forge.WithCIRepairReadLimits).
type ciForge struct {
	forgetest.BaseFake
	runs      []forge.WorkflowRun
	listErr   error
	jobs      map[int64][]forge.Job
	jobsErr   error
	tails     map[int64]string
	listOpts  []forge.ListWorkflowRunsOptions
	jobCalls  []int64
	pathErr   error    // ProjectCIConfigPath failure
	unlimited []string // reads that ran without the per-fire read limits
	// onList, when set, runs inside ListWorkflowRuns: a live test uses it to change the
	// schedule or repo between discovery and the locked start.
	onList func()
}

// hasCIReadLimits reports whether ctx carries the budget forge.WithCIRepairReadLimits installs.
// The key type is unexported, so the check reads the context's own description, which names the
// key type of every value layer.
func hasCIReadLimits(ctx context.Context) bool {
	return strings.Contains(fmt.Sprint(ctx), "ciRepairBudgetKey")
}

func (f *ciForge) ListWorkflowRuns(ctx context.Context, _ int64, opts forge.ListWorkflowRunsOptions) ([]forge.WorkflowRun, error) {
	if !hasCIReadLimits(ctx) {
		f.unlimited = append(f.unlimited, "ListWorkflowRuns")
	}
	f.listOpts = append(f.listOpts, opts)
	if f.onList != nil {
		f.onList()
	}
	return f.runs, f.listErr
}

func (f *ciForge) ListPipelineJobs(ctx context.Context, _, pipelineID int64) ([]forge.Job, error) {
	if !hasCIReadLimits(ctx) {
		f.unlimited = append(f.unlimited, "ListPipelineJobs")
	}
	f.jobCalls = append(f.jobCalls, pipelineID)
	return f.jobs[pipelineID], f.jobsErr
}

func (f *ciForge) JobLogTail(_ context.Context, _, jobID int64, _ int) (string, error) {
	return f.tails[jobID], nil
}

func (f *ciForge) ProjectCIConfigPath(context.Context, int64) (string, error) {
	return "", f.pathErr
}

type ciBuilder struct{ f forge.Forge }

func (b *ciBuilder) SetIssueLabel(context.Context, forge.Forge, int64, store.Issue, string, string, bool) (store.Issue, error) {
	panic("unexpected label removal")
}
func (b *ciBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) { return b.f, nil }

func (f *fakeStore) GetCIWatchEpisode(_ context.Context, arg store.GetCIWatchEpisodeParams) (store.CiWatchEpisode, error) {
	f.ciEpisodeLookup = append(f.ciEpisodeLookup, arg.WorkflowKey)
	if f.ciEpisodeErr != nil {
		return store.CiWatchEpisode{}, f.ciEpisodeErr
	}
	if ep, ok := f.ciEpisodes[arg.WorkflowKey]; ok {
		return ep, nil
	}
	return store.CiWatchEpisode{}, pgx.ErrNoRows
}

var ciT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// ciRun builds one completed default-branch execution. created offsets ciT0 in minutes.
func ciRun(key string, id int64, createdMin int, status, conclusion string) forge.WorkflowRun {
	return forge.WorkflowRun{
		WorkflowKey: key, ID: id, Name: "wf-" + key, Number: id, Event: "schedule", Branch: "main", SHA: "sha" + key,
		Status: status, Conclusion: conclusion, WebURL: fmt.Sprintf("https://forge.example/runs/%d", id),
		CreatedAt: ciT0.Add(time.Duration(createdMin) * time.Minute), UpdatedAt: ciT0.Add(time.Duration(createdMin) * time.Minute),
	}
}

func ciRed(key string, id int64, createdMin int) forge.WorkflowRun {
	return ciRun(key, id, createdMin, "completed", "failure")
}
func ciGreenRun(key string, id int64, createdMin int) forge.WorkflowRun {
	return ciRun(key, id, createdMin, "completed", "success")
}

func ciFailedJobs(id int64) map[int64][]forge.Job {
	return map[int64][]forge.Job{id: {{ID: id*10 + 1, Name: "e2e", Stage: "test", Status: "failure", WebURL: "https://forge.example/jobs"}}}
}

type ciHarness struct {
	*harness
	cf *ciForge
}

func newCIHarness(t *testing.T, runs ...forge.WorkflowRun) *ciHarness {
	t.Helper()
	h := newHarness()
	h.st.repoRow.DefaultBranch = pgtype.Text{String: "main", Valid: true}
	h.st.repoRow.ForgeType = "github"
	cf := &ciForge{runs: runs, jobs: map[int64][]forge.Job{}, tails: map[int64]string{}}
	for _, r := range runs {
		for id, js := range ciFailedJobs(r.ID) {
			cf.jobs[id] = js
			cf.tails[js[0].ID] = "--- FAIL: TestNightly\nboom"
		}
	}
	h.sched = New(h.st, h.runs, &ciBuilder{f: cf}, h.set, h.notif, h.vault, time.Minute, nil)
	h.sched.now = func() time.Time { return h.now }
	return &ciHarness{harness: h, cf: cf}
}

func (h *ciHarness) schedule() store.RunSchedule {
	return store.RunSchedule{
		ID: uuid.New(), UserID: h.owner, RepoID: h.repoID, Target: "ci_watch", Timing: "recurring",
		CronExpr: pgtype.Text{String: "0 * * * *", Valid: true}, Timezone: "UTC", Origin: "default",
		CatalogSlug: pgtype.Text{String: "ci-repair", Valid: true}, Status: "active", Enabled: true,
	}
}

func (h *ciHarness) fire(t *testing.T, sched store.RunSchedule) FireOutcome {
	t.Helper()
	out, err := h.sched.fireOne(context.Background(), sched)
	if err != nil {
		t.Fatalf("fireOne: %v", err)
	}
	if out.Matched != len(out.Started)+len(out.Skips) {
		t.Fatalf("matched %d != started %d + skips %d", out.Matched, len(out.Started), len(out.Skips))
	}
	if len(h.cf.unlimited) != 0 {
		t.Fatalf("forge reads without the per-fire CI read limits: %v", h.cf.unlimited)
	}
	return out
}

func skipReasons(out FireOutcome) []SkipReason {
	var r []SkipReason
	for _, s := range out.Skips {
		r = append(r, s.Reason)
	}
	return r
}

func wantOnlySkip(t *testing.T, out FireOutcome, want SkipReason) {
	t.Helper()
	if len(out.Started) != 0 || len(out.Skips) != 1 || out.Skips[0].Reason != want {
		t.Fatalf("outcome started=%d skips=%v, want exactly one %q skip", len(out.Started), skipReasons(out), want)
	}
}

// Acceptance example 1: a green default branch starts nothing and is reported as ci_green.
func TestCIWatchGreenStartsNothing(t *testing.T) {
	h := newCIHarness(t, ciGreenRun("1", 100, 0), ciGreenRun("2", 101, 1))
	out := h.fire(t, h.schedule())
	wantOnlySkip(t, out, SkipCIGreen)
	if len(h.runs.ciRepairs) != 0 || len(h.runs.ciTransitions) != 0 {
		t.Fatalf("green fire wrote: creates=%d transitions=%d", len(h.runs.ciRepairs), len(h.runs.ciTransitions))
	}
	if len(h.cf.listOpts) != 1 || h.cf.listOpts[0].Branch != "main" || h.cf.listOpts[0].Limit != 100 {
		t.Fatalf("list calls = %+v, want one {Branch: main, Limit: 100}", h.cf.listOpts)
	}
}

// Acceptance example 2: one red workflow starts exactly one guarded repair run whose
// description holds the catalog instruction and a fenced evidence block.
func TestCIWatchRedStartsOneRun(t *testing.T) {
	h := newCIHarness(t, ciRed("1", 100, 0))
	s := h.schedule()
	out := h.fire(t, s)
	if len(out.Started) != 1 || len(out.Skips) != 0 {
		t.Fatalf("outcome = started %d skips %v, want one run", len(out.Started), skipReasons(out))
	}
	if len(h.runs.ciRepairs) != 1 {
		t.Fatalf("repair creates = %d, want 1", len(h.runs.ciRepairs))
	}
	p := h.runs.ciRepairs[0]
	if p.AutoApprove {
		t.Fatal("the catalog default must reach the run with auto-approve off")
	}
	if p.Title != "CI repair: wf-1 #100" {
		t.Fatalf("title = %q", p.Title)
	}
	if !strings.Contains(p.Description, "Reproduce the failure locally") {
		t.Fatalf("description lacks the catalog instruction:\n%s", p.Description)
	}
	for _, want := range []string{"```", "Workflow: wf-1", "https://forge.example/runs/100", "Head SHA: sha1", "--- job: e2e", "--- FAIL: TestNightly"} {
		if !strings.Contains(p.Description, want) {
			t.Fatalf("description lacks %q:\n%s", want, p.Description)
		}
	}
	if p.Episode.WorkflowKey != "1" || p.Episode.Observation.RunID != 100 || p.Episode.Signature == "" || p.Episode.ScheduleID != s.ID {
		t.Fatalf("episode start = %+v", p.Episode)
	}
	if p.Episode.Fingerprint == "" || p.Episode.ForgeType != "github" {
		t.Fatalf("episode fingerprint/forge = %q/%q", p.Episode.Fingerprint, p.Episode.ForgeType)
	}
}

// A second fire while a run is active records already_running and spends no snapshot.
func TestCIWatchActiveRunIsAlreadyRunning(t *testing.T) {
	h := newCIHarness(t, ciRed("1", 100, 0))
	h.st.activeSchedule = true
	wantOnlySkip(t, h.fire(t, h.schedule()), SkipAlreadyRunning)
	if len(h.cf.jobCalls) != 0 || len(h.runs.ciRepairs) != 0 {
		t.Fatalf("an active run must cost no evidence read and no create: jobs=%v creates=%d", h.cf.jobCalls, len(h.runs.ciRepairs))
	}
}

// The handled execution is still the newest: ci_already_handled.
func TestCIWatchAlreadyHandled(t *testing.T) {
	r := ciRed("1", 100, 0)
	h := newCIHarness(t, r)
	h.st.ciEpisodes = map[string]store.CiWatchEpisode{"1": {
		Attempts: 1, ObservedRunID: pgtype.Int8{Int64: 100, Valid: true},
		ObservedCreatedAt: pgtype.Timestamptz{Time: r.CreatedAt, Valid: true}, ObservedUpdatedAt: pgtype.Timestamptz{Time: r.UpdatedAt, Valid: true},
	}}
	wantOnlySkip(t, h.fire(t, h.schedule()), SkipCIAlreadyHandled)
	if len(h.cf.jobCalls) != 0 {
		t.Fatal("an already handled execution must not be re-snapshotted")
	}
}

// A re-run of the handled execution (same id, later update) is a new observation.
func TestCIWatchRerunRevisionIsFresh(t *testing.T) {
	r := ciRed("1", 100, 0)
	h := newCIHarness(t, r)
	old := r.UpdatedAt.Add(-time.Hour)
	h.st.ciEpisodes = map[string]store.CiWatchEpisode{"1": {
		Attempts: 1, LastSignature: pgtype.Text{String: "different", Valid: true},
		ObservedRunID: pgtype.Int8{Int64: 100, Valid: true}, ObservedCreatedAt: pgtype.Timestamptz{Time: r.CreatedAt, Valid: true},
		ObservedUpdatedAt: pgtype.Timestamptz{Time: old, Valid: true},
	}}
	out := h.fire(t, h.schedule())
	if len(out.Started) != 1 {
		t.Fatalf("a later revision of the same execution must start a run, got skips %v", skipReasons(out))
	}
}

// Pull-request executions never count, even when newer.
// MUTATION: dropping the event filter makes the newer PR failure the candidate and this fails.
func TestCIWatchIgnoresPullRequestEvents(t *testing.T) {
	pr := ciRed("1", 200, 10)
	pr.Event = "pull_request"
	h := newCIHarness(t, pr, ciGreenRun("1", 100, 0))
	wantOnlySkip(t, h.fire(t, h.schedule()), SkipCIGreen)
	for _, ev := range []string{"pull_request_target", "pull_request_review", "pull_request_review_comment", "merge_request_event", "external_pull_request_event", "workflow_run"} {
		pr.Event = ev
		h = newCIHarness(t, pr, ciGreenRun("1", 100, 0))
		wantOnlySkip(t, h.fire(t, h.schedule()), SkipCIGreen)
	}
}

// Push, schedule and manual executions of the default branch still count.
func TestCIWatchKeepsDefaultBranchEvents(t *testing.T) {
	for _, ev := range []string{"push", "schedule", "workflow_dispatch"} {
		r := ciRed("1", 100, 0)
		r.Event = ev
		h := newCIHarness(t, r)
		if out := h.fire(t, h.schedule()); len(out.Started) != 1 {
			t.Fatalf("event %q: started %d, want 1", ev, len(out.Started))
		}
	}
}

// A failed ci_config_path lookup is transient: no run is created, so a custom CI file is never
// left unguarded.
func TestCIWatchCIConfigPathLookupFailureCreatesNoRun(t *testing.T) {
	h := newCIHarness(t, ciRed("1", 100, 0))
	h.cf.pathErr = errors.New("gitlab 503")
	if _, err := h.sched.fireOne(context.Background(), h.schedule()); err == nil {
		t.Fatal("a ci_config_path lookup failure must be returned as a transient fire error")
	}
	if len(h.runs.ciRepairs) != 0 {
		t.Fatalf("repair creates = %d, want 0", len(h.runs.ciRepairs))
	}
}

// Executions that have not completed are not observed.
func TestCIWatchIgnoresInProgress(t *testing.T) {
	running := ciRun("1", 200, 10, "in_progress", "")
	h := newCIHarness(t, running, ciGreenRun("1", 100, 0))
	wantOnlySkip(t, h.fire(t, h.schedule()), SkipCIGreen)
}

// The newest completed execution per key wins, whatever the list order.
func TestCIWatchNewestCompletedPerKey(t *testing.T) {
	for name, runs := range map[string][]forge.WorkflowRun{
		"newest first": {ciGreenRun("1", 200, 10), ciRed("1", 100, 0)},
		"oldest first": {ciRed("1", 100, 0), ciGreenRun("1", 200, 10)},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCIHarness(t, runs...)
			wantOnlySkip(t, h.fire(t, h.schedule()), SkipCIGreen)
		})
	}
	// And the reverse: an older green under a newer red is red.
	h := newCIHarness(t, ciGreenRun("1", 100, 0), ciRed("1", 200, 10))
	if out := h.fire(t, h.schedule()); len(out.Started) != 1 {
		t.Fatalf("newer red must start a run, got %v", skipReasons(out))
	}
}

// A late-completing OLDER execution never outranks a newer one (GitHub: created order, not id).
func TestCIWatchOrderingUsesForgeOrder(t *testing.T) {
	// GitHub/GitLab: (CreatedAt, ID). id 300 was created BEFORE id 200, so 200 is the newest.
	h := newCIHarness(t, ciRed("1", 300, 0), ciGreenRun("1", 200, 5))
	wantOnlySkip(t, h.fire(t, h.schedule()), SkipCIGreen)
	// Forgejo: ID alone. 300 is the newest regardless of timestamps.
	h = newCIHarness(t, ciRed("1", 300, 0), ciGreenRun("1", 200, 5))
	h.st.repoRow.ForgeType = "forgejo"
	if out := h.fire(t, h.schedule()); len(out.Started) != 1 {
		t.Fatalf("forgejo orders by id alone: want a run, got %v", skipReasons(out))
	}
}

// Two workflows sharing a display name stay distinct by key.
func TestCIWatchDuplicateNamesDistinctKeys(t *testing.T) {
	a, b := ciRed("10", 100, 0), ciGreenRun("11", 101, 1)
	a.Name, b.Name = "CI", "CI"
	h := newCIHarness(t, a, b)
	out := h.fire(t, h.schedule())
	if len(out.Started) != 1 || h.runs.ciRepairs[0].Episode.WorkflowKey != "10" {
		t.Fatalf("want a run for key 10 only, got started=%d creates=%+v", len(out.Started), h.runs.ciRepairs)
	}
}

// Selectors match a key or a display name; a watched workflow with no completed execution is
// reported as not seen, never as green.
func TestCIWatchSelectorsAndNotSeen(t *testing.T) {
	h := newCIHarness(t, ciGreenRun("1", 100, 0), ciRed("2", 101, 1))
	s := h.schedule()

	s.CiWorkflows = []byte(`["wf-1"]`) // by display name; the red workflow 2 is not watched
	wantOnlySkip(t, h.fire(t, s), SkipCIGreen)

	s.CiWorkflows = []byte(`["2"]`) // by key
	if out := h.fire(t, s); len(out.Started) != 1 || h.runs.ciRepairs[len(h.runs.ciRepairs)-1].Episode.WorkflowKey != "2" {
		t.Fatalf("selector by key must start the run for key 2, got %v", skipReasons(out))
	}

	h2 := newCIHarness(t, ciGreenRun("1", 100, 0))
	s = h2.schedule()
	s.CiWorkflows = []byte(`["nightly-e2e"]`)
	out := h2.fire(t, s)
	wantOnlySkip(t, out, SkipCIWatchedNotSeen)
	if out.Skips[0].Title != "nightly-e2e" {
		t.Fatalf("the unseen entry must be named, got %q", out.Skips[0].Title)
	}
	// Seen plus unseen: the green one never masks the missing one as ci_green.
	s.CiWorkflows = []byte(`["wf-1", "nightly-e2e"]`)
	if r := skipReasons(h2.fire(t, s)); len(r) != 1 || r[0] != SkipCIWatchedNotSeen {
		t.Fatalf("skips = %v, want only ci_watched_not_seen", r)
	}
}

// Nothing completed in the window is not green either.
func TestCIWatchEmptyWindowIsNotGreen(t *testing.T) {
	h := newCIHarness(t)
	wantOnlySkip(t, h.fire(t, h.schedule()), SkipCIWatchedNotSeen)
}

// Cancelled, skipped, neutral and action_required are neither red nor green: no start, no reset.
// MUTATION: classing them as red starts a run here; classing them as green resets the episode.
func TestCIWatchNeitherRedNorGreenIsNotFailed(t *testing.T) {
	for _, conclusion := range []string{"cancelled", "skipped", "neutral", "action_required"} {
		t.Run(conclusion, func(t *testing.T) {
			r := ciRun("1", 100, 0, "completed", conclusion)
			h := newCIHarness(t, r)
			h.st.ciEpisodes = map[string]store.CiWatchEpisode{"1": {Attempts: 1}}
			wantOnlySkip(t, h.fire(t, h.schedule()), SkipCINotFailed)
			if len(h.runs.ciRepairs) != 0 || len(h.runs.ciTransitions) != 0 {
				t.Fatalf("%s wrote: creates=%d transitions=%d", conclusion, len(h.runs.ciRepairs), len(h.runs.ciTransitions))
			}
		})
	}
	// GitLab/Forgejo fold the outcome into Status.
	r := ciRun("1", 100, 0, "canceled", "")
	h := newCIHarness(t, r)
	wantOnlySkip(t, h.fire(t, h.schedule()), SkipCINotFailed)
}

// Red is classified by the shared pipelinestatus helpers, including the folded-status forges.
func TestCIWatchRedClassification(t *testing.T) {
	for name, r := range map[string]forge.WorkflowRun{
		"github failure":   ciRed("1", 100, 0),
		"github timed_out": ciRun("1", 100, 0, "completed", "timed_out"),
		"gitlab failed":    ciRun("1", 100, 0, "failed", ""),
		"forgejo failure":  ciRun("1", 100, 0, "failure", ""),
	} {
		t.Run(name, func(t *testing.T) {
			h := newCIHarness(t, r)
			if out := h.fire(t, h.schedule()); len(out.Started) != 1 {
				t.Fatalf("%s must start a run, got %v", name, skipReasons(out))
			}
		})
	}
}

// A fresh green resets an existing episode; with no row, or a stale green, nothing is written.
func TestCIWatchGreenReset(t *testing.T) {
	g := ciGreenRun("1", 200, 10)
	h := newCIHarness(t, g)
	h.st.ciEpisodes = map[string]store.CiWatchEpisode{"1": {
		Attempts: 1, ObservedRunID: pgtype.Int8{Int64: 100, Valid: true}, ObservedCreatedAt: pgtype.Timestamptz{Time: ciT0, Valid: true},
		ObservedUpdatedAt: pgtype.Timestamptz{Time: ciT0, Valid: true},
	}}
	wantOnlySkip(t, h.fire(t, h.schedule()), SkipCIGreen)
	if len(h.runs.ciTransitions) != 1 || !h.runs.ciTransitions[0].Reset || h.runs.ciTransitions[0].Observation.RunID != 200 {
		t.Fatalf("transitions = %+v, want one reset at 200", h.runs.ciTransitions)
	}

	// No episode row: a green never creates one, so no transition is attempted at all.
	h = newCIHarness(t, g)
	h.fire(t, h.schedule())
	if len(h.runs.ciTransitions) != 0 {
		t.Fatalf("green with no episode attempted %d transitions", len(h.runs.ciTransitions))
	}

	// A stale green (older than the watermark) is left alone.
	h = newCIHarness(t, g)
	h.st.ciEpisodes = map[string]store.CiWatchEpisode{"1": {
		Attempts: 1, ObservedRunID: pgtype.Int8{Int64: 900, Valid: true}, ObservedCreatedAt: pgtype.Timestamptz{Time: ciT0.Add(time.Hour), Valid: true},
	}}
	h.fire(t, h.schedule())
	if len(h.runs.ciTransitions) != 0 {
		t.Fatalf("a stale green attempted %d transitions", len(h.runs.ciTransitions))
	}
}

// The loop guard halts instead of starting: same signature (no progress), or the attempt cap
// (which takes precedence).
// MUTATION: dropping the no-progress comparison starts a second identical attempt here.
func TestCIWatchLoopGuard(t *testing.T) {
	probe := newCIHarness(t, ciRed("1", 200, 5))
	probe.fire(t, probe.schedule())
	sig := probe.runs.ciRepairs[0].Episode.Signature

	old := ciRed("1", 100, 0)
	for name, tc := range map[string]struct {
		ep     store.CiWatchEpisode
		reason string
		skip   SkipReason
	}{
		"no progress": {store.CiWatchEpisode{Attempts: 1, LastSignature: pgtype.Text{String: sig, Valid: true}}, workersvc.CIHaltNoProgress, SkipCIHaltedNoProgress},
		"attempt cap": {store.CiWatchEpisode{Attempts: 2, LastSignature: pgtype.Text{String: "other", Valid: true}}, workersvc.CIHaltAttemptCap, SkipCIHaltedAttemptCap},
		"cap wins":    {store.CiWatchEpisode{Attempts: 2, LastSignature: pgtype.Text{String: sig, Valid: true}}, workersvc.CIHaltAttemptCap, SkipCIHaltedAttemptCap},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCIHarness(t, ciRed("1", 200, 5))
			ep := tc.ep
			ep.ObservedRunID = pgtype.Int8{Int64: old.ID, Valid: true}
			ep.ObservedCreatedAt = pgtype.Timestamptz{Time: old.CreatedAt, Valid: true}
			ep.ObservedUpdatedAt = pgtype.Timestamptz{Time: old.UpdatedAt, Valid: true}
			h.st.ciEpisodes = map[string]store.CiWatchEpisode{"1": ep}
			wantOnlySkip(t, h.fire(t, h.schedule()), tc.skip)
			if len(h.runs.ciRepairs) != 0 {
				t.Fatal("the loop guard must not create a run")
			}
			if len(h.runs.ciTransitions) != 1 || h.runs.ciTransitions[0].Reset || h.runs.ciTransitions[0].HaltReason != tc.reason {
				t.Fatalf("transitions = %+v, want one halt for %s", h.runs.ciTransitions, tc.reason)
			}
		})
	}
	// A different failure within the cap is a new attempt.
	h := newCIHarness(t, ciRed("1", 200, 5))
	h.st.ciEpisodes = map[string]store.CiWatchEpisode{"1": {
		Attempts: 1, LastSignature: pgtype.Text{String: "other", Valid: true}, ObservedRunID: pgtype.Int8{Int64: 100, Valid: true},
		ObservedCreatedAt: pgtype.Timestamptz{Time: old.CreatedAt, Valid: true}, ObservedUpdatedAt: pgtype.Timestamptz{Time: old.UpdatedAt, Valid: true},
	}}
	if out := h.fire(t, h.schedule()); len(out.Started) != 1 {
		t.Fatalf("a different signature under the cap must start, got %v", skipReasons(out))
	}
}

// A halted episode skips with its reason and spends no evidence read.
func TestCIWatchHaltedEpisodeSkips(t *testing.T) {
	for reason, want := range map[string]SkipReason{"no_progress": SkipCIHaltedNoProgress, "attempt_cap": SkipCIHaltedAttemptCap} {
		h := newCIHarness(t, ciRed("1", 200, 5))
		h.st.ciEpisodes = map[string]store.CiWatchEpisode{"1": {
			Attempts: 2, HaltedReason: pgtype.Text{String: reason, Valid: true},
			ObservedRunID: pgtype.Int8{Int64: 100, Valid: true}, ObservedCreatedAt: pgtype.Timestamptz{Time: ciT0, Valid: true},
		}}
		wantOnlySkip(t, h.fire(t, h.schedule()), want)
		if len(h.cf.jobCalls) != 0 || len(h.runs.ciRepairs) != 0 {
			t.Fatalf("%s: halted episode cost jobs=%v creates=%d", reason, h.cf.jobCalls, len(h.runs.ciRepairs))
		}
	}
}

// A budget overrun while listing or while collecting evidence is the benign
// ci_evidence_limited skip: no run, no episode write.
func TestCIWatchEvidenceLimited(t *testing.T) {
	budget := fmt.Errorf("list: %w", forge.ErrCIReadBudget)
	h := newCIHarness(t, ciRed("1", 100, 0))
	h.cf.listErr = budget
	wantOnlySkip(t, h.fire(t, h.schedule()), SkipCIEvidenceLimited)

	h = newCIHarness(t, ciRed("1", 100, 0))
	h.cf.jobsErr = budget
	wantOnlySkip(t, h.fire(t, h.schedule()), SkipCIEvidenceLimited)
	if len(h.runs.ciRepairs) != 0 || len(h.runs.ciTransitions) != 0 {
		t.Fatalf("a read budget overrun wrote: creates=%d transitions=%d", len(h.runs.ciRepairs), len(h.runs.ciTransitions))
	}
}

func TestCIWatchForgeErrors(t *testing.T) {
	h := newCIHarness(t, ciRed("1", 100, 0))
	h.cf.listErr = fmt.Errorf("actions: %w", forge.ErrForgeVersionUnsupported)
	wantOnlySkip(t, h.fire(t, h.schedule()), SkipCIUnsupported)

	h = newCIHarness(t, ciRed("1", 100, 0))
	h.cf.listErr = errors.New("503")
	if _, err := h.sched.fireOne(context.Background(), h.schedule()); err == nil {
		t.Fatal("an ordinary forge error must be transient (returned)")
	}
}

func TestCIWatchNoDefaultBranch(t *testing.T) {
	h := newCIHarness(t, ciRed("1", 100, 0))
	h.st.repoRow.DefaultBranch = pgtype.Text{}
	wantOnlySkip(t, h.fire(t, h.schedule()), SkipNoDefaultBranch)
	if len(h.cf.listOpts) != 0 {
		t.Fatal("no default branch must make no forge call")
	}
}

// At most three snapshots are built per fire, oldest newest-red first.
func TestCIWatchSnapshotBoundAndOrder(t *testing.T) {
	var runs []forge.WorkflowRun
	for i := int64(1); i <= 5; i++ {
		// Key "k5" has the OLDEST execution; creation order is descending in the key number.
		runs = append(runs, ciRed(fmt.Sprintf("k%d", i), 100+i, int(10-i)))
	}
	h := newCIHarness(t, runs...)
	h.runs.ciCreateErr = workersvc.ErrCIWatchStale // every create is refused, so the walk continues
	out := h.fire(t, h.schedule())
	if len(h.cf.jobCalls) != 3 {
		t.Fatalf("snapshots built = %d (%v), want 3", len(h.cf.jobCalls), h.cf.jobCalls)
	}
	if got := []int64{h.cf.jobCalls[0], h.cf.jobCalls[1], h.cf.jobCalls[2]}; got[0] != 105 || got[1] != 104 || got[2] != 103 {
		t.Fatalf("snapshot order = %v, want oldest first [105 104 103]", got)
	}
	if len(out.Skips) != 3 {
		t.Fatalf("skips = %v, want three ci_already_handled", skipReasons(out))
	}
}

// The run is created for the OLDEST red; the fire stops at the first start.
func TestCIWatchStopsAtFirstStart(t *testing.T) {
	h := newCIHarness(t, ciRed("a", 101, 5), ciRed("b", 100, 0))
	out := h.fire(t, h.schedule())
	if len(out.Started) != 1 || len(h.runs.ciRepairs) != 1 || h.runs.ciRepairs[0].Episode.WorkflowKey != "b" {
		t.Fatalf("want one run for the oldest red (b), got started=%d creates=%+v", len(out.Started), h.runs.ciRepairs)
	}
	if len(h.cf.jobCalls) != 1 {
		t.Fatalf("snapshots = %d, want 1 (stop at the first start)", len(h.cf.jobCalls))
	}
}

// Locked-recheck refusals from the seam map to benign skips.
func TestCIWatchSeamRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want SkipReason
	}{
		"config changed":  {workersvc.ErrCIWatchConfigChanged, SkipCIConfigChanged},
		"capacity":        {workersvc.ErrCIWatchCapacity, SkipCICapacity},
		"stale":           {workersvc.ErrCIWatchStale, SkipCIAlreadyHandled},
		"already handled": {workersvc.ErrCIWatchAlreadyHandled, SkipCIAlreadyHandled},
		"active race":     {workersvc.ErrActivePromptExists, SkipAlreadyRunning},
		"too large":       {workersvc.ErrDescriptionTooLarge, SkipDescriptionTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCIHarness(t, ciRed("1", 100, 0))
			h.runs.ciCreateErr = tc.err
			wantOnlySkip(t, h.fire(t, h.schedule()), tc.want)
		})
	}
	// A transient create error is returned for retry, not recorded.
	h := newCIHarness(t, ciRed("1", 100, 0))
	h.runs.ciCreateErr = errors.New("db down")
	if _, err := h.sched.fireOne(context.Background(), h.schedule()); err == nil {
		t.Fatal("a transient create error must be returned")
	}
}

// The locked row halting between the read and the lock rolls the create back; the fire then
// records the halt in its own transaction.
func TestCIWatchLockedHaltIsRecorded(t *testing.T) {
	h := newCIHarness(t, ciRed("1", 100, 0))
	h.runs.ciCreateErr = &workersvc.CIWatchHaltedError{Reason: workersvc.CIHaltNoProgress}
	wantOnlySkip(t, h.fire(t, h.schedule()), SkipCIHaltedNoProgress)
	if len(h.runs.ciTransitions) != 1 || h.runs.ciTransitions[0].HaltReason != workersvc.CIHaltNoProgress {
		t.Fatalf("transitions = %+v, want the halt recorded", h.runs.ciTransitions)
	}
}

// A config change detected by a green reset ends the fire with the benign skip.
func TestCIWatchConfigChangedOnReset(t *testing.T) {
	h := newCIHarness(t, ciGreenRun("1", 200, 10))
	h.st.ciEpisodes = map[string]store.CiWatchEpisode{"1": {Attempts: 1}}
	h.runs.ciTransitionErr = workersvc.ErrCIWatchConfigChanged
	wantOnlySkip(t, h.fire(t, h.schedule()), SkipCIConfigChanged)
}

// The configuration fingerprint covers the base instruction the fire resolved.
func TestCIWatchUserPromptIsTheBaseInstruction(t *testing.T) {
	h := newCIHarness(t, ciRed("1", 100, 0))
	s := h.schedule()
	s.Origin = "user"
	s.CatalogSlug = pgtype.Text{}
	s.Prompt = pgtype.Text{String: "my own repair instruction", Valid: true}
	s.Guidance = pgtype.Text{String: "prefer small diffs", Valid: true}
	h.fire(t, s)
	d := h.runs.ciRepairs[0].Description
	if !strings.HasPrefix(d, "my own repair instruction") || !strings.Contains(d, "prefer small diffs") {
		t.Fatalf("description = %q", d)
	}
	if strings.Index(d, "prefer small diffs") > strings.Index(d, "```") {
		t.Fatal("the owner guidance must precede the untrusted evidence block")
	}
}

// A gone catalog entry can never resolve: permanent park.
func TestCIWatchMissingCatalogIsBadConfig(t *testing.T) {
	h := newCIHarness(t, ciRed("1", 100, 0))
	s := h.schedule()
	s.CatalogSlug = pgtype.Text{String: "nope", Valid: true}
	if _, err := h.sched.fireOne(context.Background(), s); !errors.Is(err, errBadConfig) {
		t.Fatalf("err = %v, want errBadConfig", err)
	}
	s = h.schedule()
	s.CiWorkflows = []byte(`{"not":"an array"}`)
	if _, err := h.sched.fireOne(context.Background(), s); !errors.Is(err, errBadConfig) {
		t.Fatalf("err = %v, want errBadConfig for malformed ci_workflows", err)
	}
}

// SetCIRepair bounds reach the snapshot and the run's CI-config paths.
func TestCIWatchConfigThreaded(t *testing.T) {
	h := newCIHarness(t, ciRed("1", 100, 0))
	h.sched.SetCIRepair(CIRepairConfig{MaxJobs: 3, LogTailBytes: 100, MaxAttempts: 5, ConfigPaths: []string{"custom/ci/**"}})
	h.fire(t, h.schedule())
	p := h.runs.ciRepairs[0]
	if p.Episode.MaxAttempts != 5 {
		t.Fatalf("max attempts = %d", p.Episode.MaxAttempts)
	}
	if len(p.CIConfigPaths) != 1 || p.CIConfigPaths[0] != "custom/ci/**" {
		t.Fatalf("configured paths = %v (the floor is unioned by the creation seam)", p.CIConfigPaths)
	}
}

// PRD acceptance example 2: while the repair run is active, a fire that finds the same
// (handled) execution still newest reports already_running; once the run ends it reports
// already handled.
func TestCIWatchHandledExecutionWhileRunActive(t *testing.T) {
	r := ciRed("1", 100, 0)
	h := newCIHarness(t, r)
	h.st.ciEpisodes = map[string]store.CiWatchEpisode{"1": {
		Attempts: 1, ObservedRunID: pgtype.Int8{Int64: 100, Valid: true},
		ObservedCreatedAt: pgtype.Timestamptz{Time: r.CreatedAt, Valid: true}, ObservedUpdatedAt: pgtype.Timestamptz{Time: r.UpdatedAt, Valid: true},
	}}
	h.st.activeSchedule = true
	wantOnlySkip(t, h.fire(t, h.schedule()), SkipAlreadyRunning)
	h.st.activeSchedule = false
	wantOnlySkip(t, h.fire(t, h.schedule()), SkipCIAlreadyHandled)
}
