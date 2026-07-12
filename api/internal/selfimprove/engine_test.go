package selfimprove

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/notifysvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// ── Fakes ────────────────────────────────────────────────────────────────────

type fakeSettings struct {
	enabled     bool
	interval    time.Duration
	repo        string
	userID      string
	lastRunAt   time.Time
	hasLastRun  bool
	invalidated int
}

func (f *fakeSettings) SelfimproveEnabled(context.Context) (bool, error) { return f.enabled, nil }
func (f *fakeSettings) SelfimproveInterval(context.Context) (time.Duration, error) {
	return f.interval, nil
}
func (f *fakeSettings) SelfimproveRepo(context.Context) (string, error)   { return f.repo, nil }
func (f *fakeSettings) SelfimproveUserID(context.Context) (string, error) { return f.userID, nil }
func (f *fakeSettings) SelfimproveLastRunAt(context.Context) (time.Time, bool, error) {
	return f.lastRunAt, f.hasLastRun, nil
}
func (f *fakeSettings) Invalidate() { f.invalidated++ }

type fakeStore struct {
	active        int64
	recs          []store.ListOpenImproveUziRecommendationsRow
	markedIDs     []uuid.UUID
	markedByRun   uuid.UUID
	repoErr       error
	repoRow       store.GetRepoForUserRow
	upserts       map[string]string
	getRepoParams []store.GetRepoForUserParams
}

func (f *fakeStore) CountActiveSelfImproveRuns(context.Context) (int64, error) { return f.active, nil }
func (f *fakeStore) ListOpenImproveUziRecommendations(context.Context, int32) ([]store.ListOpenImproveUziRecommendationsRow, error) {
	return f.recs, nil
}
func (f *fakeStore) MarkImproveUziRecommendationsAddressed(_ context.Context, arg store.MarkImproveUziRecommendationsAddressedParams) (int64, error) {
	f.markedIDs = arg.Ids
	f.markedByRun = uuid.UUID(arg.AddressedByRunID.Bytes)
	return int64(len(arg.Ids)), nil
}
func (f *fakeStore) GetRepoForUser(_ context.Context, arg store.GetRepoForUserParams) (store.GetRepoForUserRow, error) {
	f.getRepoParams = append(f.getRepoParams, arg)
	if f.repoErr != nil {
		return store.GetRepoForUserRow{}, f.repoErr
	}
	return f.repoRow, nil
}
func (f *fakeStore) UpsertAppSetting(_ context.Context, arg store.UpsertAppSettingParams) (store.AppSetting, error) {
	if f.upserts == nil {
		f.upserts = map[string]string{}
	}
	f.upserts[arg.Key] = arg.Value
	return store.AppSetting{}, nil
}

type fakeRuns struct {
	created    int
	lastDesc   string
	lastRepoID uuid.UUID
	lastUserID uuid.UUID
	lastIssue  int64
	err        error
	run        store.Run
}

func (f *fakeRuns) CreateSelfImproveRun(_ context.Context, userID, repoID uuid.UUID, issueIID int64, _, description string) (store.Run, error) {
	if f.err != nil {
		return store.Run{}, f.err
	}
	f.created++
	f.lastDesc = description
	f.lastRepoID = repoID
	f.lastUserID = userID
	f.lastIssue = issueIID
	if f.run.ID == (uuid.UUID{}) {
		f.run = store.Run{ID: uuid.New()}
	}
	return f.run, nil
}

// fakeForge embeds the forge.Forge interface (nil) and overrides only the two
// methods the engine uses, so any other call panics loudly rather than passing.
type fakeForge struct {
	forge.Forge
	listResult  []forge.Issue
	listErr     error
	created     int
	createdIID  int64
	createLabel []string
}

func (f *fakeForge) ListIssues(context.Context, int64, forge.ListIssuesOptions) ([]forge.Issue, error) {
	return f.listResult, f.listErr
}
func (f *fakeForge) CreateIssue(_ context.Context, _ int64, _, _ string, labels []string) (forge.Issue, error) {
	f.created++
	f.createLabel = labels
	return forge.Issue{IID: f.createdIID}, nil
}

type fakeBuilder struct{ f *fakeForge }

func (b *fakeBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.f, nil
}

type fakeVault struct{ unlocked bool }

func (v *fakeVault) Unlocked(uuid.UUID) bool { return v.unlocked }

type fakeNotifier struct{ notifications []notifysvc.Notification }

func (n *fakeNotifier) Notify(_ context.Context, notif notifysvc.Notification) (store.Notification, error) {
	n.notifications = append(n.notifications, notif)
	return store.Notification{}, nil
}

// ── Harness ──────────────────────────────────────────────────────────────────

type harness struct {
	set   *fakeSettings
	st    *fakeStore
	runs  *fakeRuns
	forge *fakeForge
	vault *fakeVault
	notif *fakeNotifier
	eng   *Engine
	now   time.Time
}

func newHarness() *harness {
	owner := uuid.New()
	repoID := uuid.New()
	h := &harness{
		set: &fakeSettings{
			enabled:  true,
			interval: 48 * time.Hour,
			repo:     repoID.String(),
			userID:   owner.String(),
		},
		st:    &fakeStore{repoRow: store.GetRepoForUserRow{ID: repoID, UserID: owner, ForgeProjectID: 42}},
		runs:  &fakeRuns{},
		forge: &fakeForge{createdIID: 7},
		vault: &fakeVault{unlocked: true},
		notif: &fakeNotifier{},
		now:   time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC),
	}
	h.eng = New(h.st, h.set, h.runs, &fakeBuilder{f: h.forge}, h.vault, h.notif, time.Hour, nil)
	h.eng.now = func() time.Time { return h.now }
	return h
}

func (h *harness) skipKinds() []string {
	var out []string
	for _, n := range h.notif.notifications {
		if n.Kind == "selfimprove_skipped" {
			out = append(out, n.Kind)
		}
	}
	return out
}

// ── Tests ────────────────────────────────────────────────────────────────────

func TestTickDisabledDoesNothing(t *testing.T) {
	h := newHarness()
	h.set.enabled = false
	h.eng.tick(context.Background())
	if h.runs.created != 0 {
		t.Fatalf("disabled: created %d runs, want 0", h.runs.created)
	}
	if len(h.notif.notifications) != 0 {
		t.Fatalf("disabled: sent %d notifications, want 0", len(h.notif.notifications))
	}
}

func TestTickNotDueDoesNothing(t *testing.T) {
	h := newHarness()
	h.set.hasLastRun = true
	h.set.lastRunAt = h.now.Add(-1 * time.Hour) // only 1h ago, interval 48h
	h.eng.tick(context.Background())
	if h.runs.created != 0 {
		t.Fatalf("not due: created %d runs, want 0", h.runs.created)
	}
}

func TestTickDueCreatesRun(t *testing.T) {
	h := newHarness()
	rec := store.ListOpenImproveUziRecommendationsRow{ID: uuid.New(), Target: "worker: jq", RationaleMd: "install jq"}
	h.st.recs = []store.ListOpenImproveUziRecommendationsRow{rec}

	h.eng.tick(context.Background())

	if h.runs.created != 1 {
		t.Fatalf("created %d runs, want 1", h.runs.created)
	}
	if h.runs.lastIssue != 7 {
		t.Fatalf("run tracking issue iid = %d, want the freshly-created 7", h.runs.lastIssue)
	}
	if h.forge.created != 1 {
		t.Fatalf("expected a tracking issue to be filed once, got %d", h.forge.created)
	}
	// Marker label present, no PRD/autopilot trigger label (review N1).
	if len(h.forge.createLabel) != 1 || h.forge.createLabel[0] != TrackingLabel {
		t.Fatalf("tracking issue labels = %v, want only %q", h.forge.createLabel, TrackingLabel)
	}
	// The backlog rode into the run description and got stamped addressed by the run.
	if h.runs.lastDesc == "" || !strings.Contains(h.runs.lastDesc, "jq") {
		t.Fatalf("run description missing the backlog: %q", h.runs.lastDesc)
	}
	if len(h.st.markedIDs) != 1 || h.st.markedIDs[0] != rec.ID {
		t.Fatalf("marked ids = %v, want exactly the listed backlog", h.st.markedIDs)
	}
	if h.st.markedByRun != h.runs.run.ID {
		t.Fatalf("backlog marked addressed by %s, want the new run %s", h.st.markedByRun, h.runs.run.ID)
	}
	// Durable cadence advanced + cache invalidated (M1 carry-forward).
	if h.st.upserts["selfimprove_last_run_at"] == "" {
		t.Fatalf("last_run_at not persisted: %v", h.st.upserts)
	}
	if h.set.invalidated == 0 {
		t.Fatalf("settings cache not invalidated after writing last_run_at")
	}
	// A started notification to the owner.
	if !hasKind(h.notif.notifications, "selfimprove_started") {
		t.Fatalf("no selfimprove_started notification: %+v", h.notif.notifications)
	}
}

func TestTickReusesOpenTrackingIssue(t *testing.T) {
	h := newHarness()
	h.forge.listResult = []forge.Issue{
		{IID: 3, State: "closed"},
		{IID: 11, State: "opened"},
		{IID: 9, State: "opened"},
	}
	h.eng.tick(context.Background())
	if h.forge.created != 0 {
		t.Fatalf("should reuse an open tracking issue, not create one (created=%d)", h.forge.created)
	}
	if h.runs.lastIssue != 11 {
		t.Fatalf("reused issue iid = %d, want the newest open one (11)", h.runs.lastIssue)
	}
}

func TestTickSkipsWhenRunActive(t *testing.T) {
	h := newHarness()
	h.st.active = 1
	h.eng.tick(context.Background())
	if h.runs.created != 0 {
		t.Fatalf("active run: created %d, want 0", h.runs.created)
	}
	if len(h.skipKinds()) != 1 {
		t.Fatalf("active run: want one skip notification, got %d", len(h.skipKinds()))
	}
}

func TestTickSkipsWhenVaultLocked(t *testing.T) {
	h := newHarness()
	h.vault.unlocked = false
	h.eng.tick(context.Background())
	if h.runs.created != 0 || len(h.skipKinds()) != 1 {
		t.Fatalf("vault locked: created=%d skips=%d, want 0/1", h.runs.created, len(h.skipKinds()))
	}
}

func TestTickSkipsWhenRepoMissing(t *testing.T) {
	h := newHarness()
	h.st.repoErr = errors.New("no rows")
	h.eng.tick(context.Background())
	if h.runs.created != 0 || len(h.skipKinds()) != 1 {
		t.Fatalf("repo missing: created=%d skips=%d, want 0/1", h.runs.created, len(h.skipKinds()))
	}
}

func TestTickNotConfiguredLogsNoNotify(t *testing.T) {
	h := newHarness()
	h.set.userID = ""
	h.set.repo = ""
	h.eng.tick(context.Background())
	if h.runs.created != 0 {
		t.Fatalf("unconfigured: created %d, want 0", h.runs.created)
	}
	// No owner to notify.
	if len(h.notif.notifications) != 0 {
		t.Fatalf("unconfigured: sent %d notifications, want 0", len(h.notif.notifications))
	}
}

func TestTickEmptyBacklogStillRuns(t *testing.T) {
	h := newHarness()
	h.st.recs = nil
	h.eng.tick(context.Background())
	if h.runs.created != 1 {
		t.Fatalf("empty backlog: created %d, want 1 (pure code review)", h.runs.created)
	}
	if len(h.st.markedIDs) != 0 {
		t.Fatalf("empty backlog: marked %d, want 0", len(h.st.markedIDs))
	}
	if h.runs.lastDesc == "" {
		t.Fatalf("empty backlog: run description should still carry a review instruction")
	}
}

func TestTickActiveRaceIsQuiet(t *testing.T) {
	// The count check passed but CreateSelfImproveRun lost the unique-index race.
	h := newHarness()
	h.runs.err = workersvc.ErrActiveSelfImproveExists
	h.eng.tick(context.Background())
	if hasKind(h.notif.notifications, "selfimprove_started") {
		t.Fatalf("a lost create race must not notify started")
	}
	if h.st.upserts["selfimprove_last_run_at"] != "" {
		t.Fatalf("a lost create race must not advance last_run_at")
	}
}

func TestSkipNotificationThrottled(t *testing.T) {
	h := newHarness()
	h.vault.unlocked = false // persistent skip condition
	h.eng.tick(context.Background())
	// A second tick 1h later (< 48h interval) must NOT re-notify.
	h.now = h.now.Add(time.Hour)
	h.eng.tick(context.Background())
	if got := len(h.skipKinds()); got != 1 {
		t.Fatalf("skip notifications = %d, want 1 (throttled within the interval)", got)
	}
	// After the interval elapses, a skip notifies again.
	h.now = h.now.Add(49 * time.Hour)
	h.eng.tick(context.Background())
	if got := len(h.skipKinds()); got != 2 {
		t.Fatalf("skip notifications = %d, want 2 after the throttle window", got)
	}
}

func TestTickOwnerScopedRepoLookup(t *testing.T) {
	// The repo ownership check must use the ENABLING admin's id (from settings),
	// never anyone else's — the engine only ever acts on the owner's own repo.
	h := newHarness()
	owner, _ := uuid.Parse(h.set.userID)
	h.eng.tick(context.Background())
	if len(h.st.getRepoParams) == 0 || h.st.getRepoParams[0].UserID != owner {
		t.Fatalf("repo lookup user = %v, want the enabling admin %s", h.st.getRepoParams, owner)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func hasKind(ns []notifysvc.Notification, kind string) bool {
	for _, n := range ns {
		if n.Kind == kind {
			return true
		}
	}
	return false
}
