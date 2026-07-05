package poller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// Shared identity for the fake repo row every test detects against. Tests never
// mutate these, so package-level values are safe.
var (
	apRepoID = uuid.New()
	apConnID = uuid.New()
	apUserID = uuid.New()
)

const apLabel = "autopilot"

// ── fakes ────────────────────────────────────────────────────────────────────

type apStore struct {
	candidates []store.ListAutopilotCandidateIssuesRow
	candErr    error

	cc    store.GetAutopilotConnectionContextRow
	ccErr error

	triggers  map[int64]store.AutopilotTrigger
	trigErr   error
	upserts   []store.UpsertAutopilotTriggerParams
	upsertErr error

	active    map[int64]bool
	activeErr error

	ops *[]string // shared op-order log across store + forge + runs
}

func (s *apStore) ListAutopilotCandidateIssues(context.Context, store.ListAutopilotCandidateIssuesParams) ([]store.ListAutopilotCandidateIssuesRow, error) {
	return s.candidates, s.candErr
}
func (s *apStore) GetAutopilotConnectionContext(context.Context, uuid.UUID) (store.GetAutopilotConnectionContextRow, error) {
	return s.cc, s.ccErr
}
func (s *apStore) GetAutopilotTrigger(_ context.Context, arg store.GetAutopilotTriggerParams) (store.AutopilotTrigger, error) {
	if s.trigErr != nil {
		return store.AutopilotTrigger{}, s.trigErr
	}
	if t, ok := s.triggers[arg.IssueIid]; ok {
		return t, nil
	}
	return store.AutopilotTrigger{}, pgx.ErrNoRows
}
func (s *apStore) UpsertAutopilotTrigger(_ context.Context, arg store.UpsertAutopilotTriggerParams) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.upserts = append(s.upserts, arg)
	if s.triggers == nil {
		s.triggers = map[int64]store.AutopilotTrigger{}
	}
	s.triggers[arg.IssueIid] = store.AutopilotTrigger{RepoID: arg.RepoID, IssueIid: arg.IssueIid, LastEventID: arg.LastEventID}
	if s.ops != nil {
		*s.ops = append(*s.ops, "record")
	}
	return nil
}
func (s *apStore) HasActiveRunForIssue(_ context.Context, arg store.HasActiveRunForIssueParams) (bool, error) {
	if s.activeErr != nil {
		return false, s.activeErr
	}
	return s.active[arg.IssueIid], nil
}

type apRunCall struct {
	userID, repoID uuid.UUID
	iid            int64
	desc           string
}

type apRuns struct {
	err   error
	calls []apRunCall
	ops   *[]string
}

func (r *apRuns) CreateAutopilotRun(_ context.Context, userID, repoID uuid.UUID, iid int64, desc string) (store.Run, error) {
	r.calls = append(r.calls, apRunCall{userID, repoID, iid, desc})
	if r.ops != nil {
		*r.ops = append(*r.ops, "create")
	}
	if r.err != nil {
		return store.Run{}, r.err
	}
	return store.Run{ID: uuid.New()}, nil
}

type apLabeler struct{ label string }

func (l apLabeler) AutopilotLabel(context.Context) (string, error) { return l.label, nil }

type apNote struct {
	iid  int64
	body string
}

type apForge struct {
	events    map[int64][]forge.LabelEvent
	eventsErr error
	issue     forge.Issue
	issueErr  error
	notes     []apNote
	noteErr   error
	ops       *[]string
}

func (f *apForge) ListIssueLabelEvents(_ context.Context, _, iid int64) ([]forge.LabelEvent, error) {
	if f.eventsErr != nil {
		return nil, f.eventsErr
	}
	return f.events[iid], nil
}
func (f *apForge) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	return f.issue, f.issueErr
}
func (f *apForge) CreateIssueNote(_ context.Context, _, iid int64, body string) (forge.IssueNote, error) {
	if f.noteErr != nil {
		return forge.IssueNote{}, f.noteErr
	}
	f.notes = append(f.notes, apNote{iid, body})
	if f.ops != nil {
		*f.ops = append(*f.ops, "comment")
	}
	return forge.IssueNote{ID: 1, Body: body}, nil
}

// Unused-by-autopilot interface methods.
func (f *apForge) VerifyToken(context.Context) (forge.BotIdentity, error) {
	return forge.BotIdentity{}, nil
}
func (f *apForge) ListProjects(context.Context) ([]forge.Project, error)    { return nil, nil }
func (f *apForge) ListLabels(context.Context, int64) ([]forge.Label, error) { return nil, nil }
func (f *apForge) EnsureLabels(context.Context, int64, []forge.Label) error { return nil }
func (f *apForge) CreateIssue(context.Context, int64, string, string, []string) (forge.Issue, error) {
	return forge.Issue{}, nil
}
func (f *apForge) UpdateIssueLabels(context.Context, int64, int64, []string, []string) error {
	return nil
}
func (f *apForge) UserExists(context.Context, string) (bool, error) { return false, nil }
func (f *apForge) GetMergeRequest(context.Context, int64, int64) (forge.MergeRequest, error) {
	return forge.MergeRequest{}, nil
}
func (f *apForge) ListIssues(context.Context, int64, forge.ListIssuesOptions) ([]forge.Issue, error) {
	return nil, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func repoRow() store.ListEnabledReposWithConnectionsRow {
	return store.ListEnabledReposWithConnectionsRow{
		ID:                apRepoID,
		ConnectionID:      apConnID,
		ForgeProjectID:    42,
		PathWithNamespace: "grp/proj",
	}
}

func ccOwner(username string, opted, hasToken bool) store.GetAutopilotConnectionContextRow {
	return store.GetAutopilotConnectionContextRow{
		UserID:            apUserID,
		HumanUsername:     pgtype.Text{String: username, Valid: username != ""},
		AutopilotEnabled:  opted,
		HasAnthropicToken: hasToken,
	}
}

func candIssue(iid int64, author string) store.ListAutopilotCandidateIssuesRow {
	return store.ListAutopilotCandidateIssuesRow{
		ForgeIssueIid: iid,
		Author:        pgtype.Text{String: author, Valid: author != ""},
	}
}

func addEvt(id int64, user string) forge.LabelEvent {
	return forge.LabelEvent{ID: id, Action: "add", LabelName: apLabel, Username: user}
}
func remEvt(id int64, user string) forge.LabelEvent {
	return forge.LabelEvent{ID: id, Action: "remove", LabelName: apLabel, Username: user}
}

// detectWith runs one detection pass with the given fakes and a small non-empty
// issue description on the forge (so an eligible run has something to snapshot).
func detectWith(st *apStore, runs *apRuns, f *apForge) {
	if f.issue.Description == "" && f.issueErr == nil {
		f.issue = forge.Issue{Description: "see prds/19.md"}
	}
	NewAutopilot(st, runs, apLabeler{label: apLabel}).detect(context.Background(), repoRow(), f)
}

func lastUpsert(t *testing.T, st *apStore) store.UpsertAutopilotTriggerParams {
	t.Helper()
	if len(st.upserts) == 0 {
		t.Fatal("expected a recorded autopilot trigger, got none")
	}
	return st.upserts[len(st.upserts)-1]
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestAutopilotStartsRunOnFreshTransition(t *testing.T) {
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")},
		cc:         ccOwner("alice", true, true),
	}
	runs := &apRuns{}
	f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(100, "alice")}}}

	detectWith(st, runs, f)

	if len(runs.calls) != 1 {
		t.Fatalf("CreateAutopilotRun calls = %d, want 1", len(runs.calls))
	}
	c := runs.calls[0]
	if c.userID != apUserID || c.repoID != apRepoID || c.iid != 7 || c.desc != "see prds/19.md" {
		t.Fatalf("run call = %+v, want owner/repo/iid=7/desc", c)
	}
	if got := lastUpsert(t, st); got.LastEventID != 100 {
		t.Fatalf("recorded event id = %d, want 100", got.LastEventID)
	}
	if len(f.notes) != 0 {
		t.Fatalf("expected no comment on a started run, got %+v", f.notes)
	}
}

func TestAutopilotCreateThenRecordOrder(t *testing.T) {
	var ops []string
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")},
		cc:         ccOwner("alice", true, true),
		ops:        &ops,
	}
	runs := &apRuns{ops: &ops}
	f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(100, "alice")}}, ops: &ops}

	detectWith(st, runs, f)

	if strings.Join(ops, ",") != "create,record" {
		t.Fatalf("op order = %v, want [create record]", ops)
	}
}

func TestAutopilotTransitionOnceSkipsHandled(t *testing.T) {
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")},
		cc:         ccOwner("alice", true, true),
		triggers:   map[int64]store.AutopilotTrigger{7: {IssueIid: 7, LastEventID: 100}},
	}
	runs := &apRuns{}
	f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(100, "alice")}}}

	detectWith(st, runs, f)

	if len(runs.calls) != 0 {
		t.Fatalf("expected no run for an already-handled event, got %d", len(runs.calls))
	}
	if len(st.upserts) != 0 {
		t.Fatalf("expected no re-record, got %+v", st.upserts)
	}
	if len(f.notes) != 0 {
		t.Fatalf("expected no comment, got %+v", f.notes)
	}
}

// A FullSync evicts and re-inserts the issues cache, but autopilot_triggers
// survives. A not-eligible issue whose event was already recorded must NOT
// re-comment after the cache row reappears.
func TestAutopilotDedupSurvivesCacheEvictionNoRecomment(t *testing.T) {
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "carol")}, // re-inserted row
		cc:         ccOwner("alice", true, true),                                   // owner != adder/author → would comment if not deduped
		triggers:   map[int64]store.AutopilotTrigger{7: {IssueIid: 7, LastEventID: 100}},
	}
	runs := &apRuns{}
	f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(100, "carol")}}}

	detectWith(st, runs, f)

	if len(f.notes) != 0 || len(st.upserts) != 0 {
		t.Fatalf("expected no re-comment/re-record after eviction, notes=%+v upserts=%+v", f.notes, st.upserts)
	}
}

func TestAutopilotReAddRetriggers(t *testing.T) {
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")},
		cc:         ccOwner("alice", true, true),
		triggers:   map[int64]store.AutopilotTrigger{7: {IssueIid: 7, LastEventID: 100}},
	}
	runs := &apRuns{}
	// remove + re-add mints a larger event id: a new transition.
	f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(100, "alice"), remEvt(105, "alice"), addEvt(110, "alice")}}}

	detectWith(st, runs, f)

	if len(runs.calls) != 1 {
		t.Fatalf("expected a re-triggered run, got %d calls", len(runs.calls))
	}
	if got := lastUpsert(t, st); got.LastEventID != 110 {
		t.Fatalf("recorded event id = %d, want 110", got.LastEventID)
	}
}

func TestAutopilotAuthorFallback(t *testing.T) {
	// Adder "bob" is not the owner; author "alice" is → resolve via author.
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")},
		cc:         ccOwner("alice", true, true),
	}
	runs := &apRuns{}
	f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(100, "bob")}}}

	detectWith(st, runs, f)

	if len(runs.calls) != 1 {
		t.Fatalf("expected a run via author fallback, got %d", len(runs.calls))
	}
}

func TestAutopilotAdderMatchStartsRun(t *testing.T) {
	// Adder "alice" is the owner; author is someone else → resolve via adder.
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "dave")},
		cc:         ccOwner("alice", true, true),
	}
	runs := &apRuns{}
	f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(100, "alice")}}}

	detectWith(st, runs, f)

	if len(runs.calls) != 1 {
		t.Fatalf("expected a run via adder match, got %d", len(runs.calls))
	}
}

func TestAutopilotConsentGatesComment(t *testing.T) {
	cases := []struct {
		name   string
		cc     store.GetAutopilotConnectionContextRow
		adder  string
		author string
	}{
		{"no mapping", ccOwner("", true, true), "alice", "alice"},
		{"opted out", ccOwner("alice", false, true), "alice", "alice"},
		{"no token", ccOwner("alice", true, false), "alice", "alice"},
		{"username matches neither", ccOwner("alice", true, true), "carol", "dave"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &apStore{
				candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, tc.author)},
				cc:         tc.cc,
			}
			runs := &apRuns{}
			f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(100, tc.adder)}}}

			detectWith(st, runs, f)

			if len(runs.calls) != 0 {
				t.Fatalf("expected no run, got %d", len(runs.calls))
			}
			if len(f.notes) != 1 {
				t.Fatalf("expected exactly one explanatory comment, got %+v", f.notes)
			}
			if !strings.Contains(f.notes[0].body, "autopilot enabled is mapped") {
				t.Fatalf("comment body = %q, want the no-eligible-user message", f.notes[0].body)
			}
			if got := lastUpsert(t, st); got.LastEventID != 100 {
				t.Fatalf("recorded event id = %d, want 100", got.LastEventID)
			}
		})
	}
}

func TestAutopilotNoPRDLinkComments(t *testing.T) {
	var ops []string
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")},
		cc:         ccOwner("alice", true, true),
		ops:        &ops,
	}
	runs := &apRuns{err: workersvc.ErrNoPRDLink, ops: &ops}
	f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(100, "alice")}}, ops: &ops}

	detectWith(st, runs, f)

	if len(f.notes) != 1 || !strings.Contains(f.notes[0].body, "no PRD link") {
		t.Fatalf("expected one no-PRD-link comment, got %+v", f.notes)
	}
	// The run-create attempt happens, then record-then-comment: create, record, comment.
	if strings.Join(ops, ",") != "create,record,comment" {
		t.Fatalf("op order = %v, want [create record comment]", ops)
	}
}

func TestAutopilotActiveRunSwallow(t *testing.T) {
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")},
		cc:         ccOwner("alice", true, true),
		active:     map[int64]bool{7: true},
	}
	runs := &apRuns{}
	f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(100, "alice")}}}

	detectWith(st, runs, f)

	if len(runs.calls) != 0 {
		t.Fatalf("expected no run while active, got %d", len(runs.calls))
	}
	if len(f.notes) != 0 {
		t.Fatalf("expected no comment while active (swallow), got %+v", f.notes)
	}
	if got := lastUpsert(t, st); got.LastEventID != 100 {
		t.Fatalf("swallow must consume the event id, recorded = %d want 100", got.LastEventID)
	}
}

func TestAutopilotRaceActiveAtCreateSwallow(t *testing.T) {
	// No active run at the pre-check, but the unique index rejects at creation
	// (a run appeared in between): swallow, no comment.
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")},
		cc:         ccOwner("alice", true, true),
	}
	runs := &apRuns{err: workersvc.ErrActiveRunExists}
	f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(100, "alice")}}}

	detectWith(st, runs, f)

	if len(f.notes) != 0 {
		t.Fatalf("expected no comment on active-run race, got %+v", f.notes)
	}
	if got := lastUpsert(t, st); got.LastEventID != 100 {
		t.Fatalf("recorded event id = %d, want 100", got.LastEventID)
	}
}

func TestAutopilotLabelRemovedRaceSkips(t *testing.T) {
	// Cache lists the label, but the events' latest state for it is a removal.
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")},
		cc:         ccOwner("alice", true, true),
	}
	runs := &apRuns{}
	f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(100, "alice"), remEvt(105, "alice")}}}

	detectWith(st, runs, f)

	if len(runs.calls) != 0 || len(f.notes) != 0 || len(st.upserts) != 0 {
		t.Fatalf("expected a clean skip, runs=%d notes=%d upserts=%d", len(runs.calls), len(f.notes), len(st.upserts))
	}
}

func TestAutopilotLabelEventsErrorLeavesUnrecorded(t *testing.T) {
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")},
		cc:         ccOwner("alice", true, true),
	}
	runs := &apRuns{}
	f := &apForge{eventsErr: errors.New("forge unreachable")}

	detectWith(st, runs, f)

	if len(runs.calls) != 0 || len(f.notes) != 0 || len(st.upserts) != 0 {
		t.Fatalf("a forge blip must leave the issue for retry, runs=%d notes=%d upserts=%d", len(runs.calls), len(f.notes), len(st.upserts))
	}
}

func TestAutopilotGetIssueErrorLeavesUnrecorded(t *testing.T) {
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")},
		cc:         ccOwner("alice", true, true),
	}
	runs := &apRuns{}
	f := &apForge{
		events:   map[int64][]forge.LabelEvent{7: {addEvt(100, "alice")}},
		issueErr: errors.New("forge unreachable"),
	}

	detectWith(st, runs, f)

	if len(runs.calls) != 0 || len(f.notes) != 0 || len(st.upserts) != 0 {
		t.Fatalf("a description-fetch blip must leave the issue for retry, runs=%d notes=%d upserts=%d", len(runs.calls), len(f.notes), len(st.upserts))
	}
}

func TestAutopilotTooLargeDescriptionComments(t *testing.T) {
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")},
		cc:         ccOwner("alice", true, true),
	}
	// The description cap now lives once in workersvc.createRun (M5 unification); the
	// poller reacts to the ErrDescriptionTooLarge it surfaces by posting the too-large
	// comment instead of size-checking itself.
	runs := &apRuns{err: workersvc.ErrDescriptionTooLarge}
	f := &apForge{
		events: map[int64][]forge.LabelEvent{7: {addEvt(100, "alice")}},
		issue:  forge.Issue{Description: "any description; the shared cap rejects it"},
	}

	detectWith(st, runs, f)

	if len(runs.calls) != 1 {
		t.Fatalf("expected the shared create path to be attempted once, got %d", len(runs.calls))
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0].body, "too large") {
		t.Fatalf("expected one too-large comment, got %+v", f.notes)
	}
	if len(st.upserts) != 1 {
		t.Fatalf("too-large comment must record the event first (record-then-comment), upserts=%d", len(st.upserts))
	}
}

func TestAutopilotMultipleIssuesIndependent(t *testing.T) {
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice"), candIssue(8, "carol")},
		cc:         ccOwner("alice", true, true),
	}
	runs := &apRuns{}
	f := &apForge{events: map[int64][]forge.LabelEvent{
		7: {addEvt(100, "alice")}, // eligible → run
		8: {addEvt(200, "carol")}, // not the owner → comment
	}}

	detectWith(st, runs, f)

	if len(runs.calls) != 1 || runs.calls[0].iid != 7 {
		t.Fatalf("expected exactly issue 7 to run, got %+v", runs.calls)
	}
	if len(f.notes) != 1 || f.notes[0].iid != 8 {
		t.Fatalf("expected exactly issue 8 to be commented, got %+v", f.notes)
	}
	if len(st.upserts) != 2 {
		t.Fatalf("expected both issues recorded, got %d", len(st.upserts))
	}
}

func TestAutopilotNoCandidatesNoOps(t *testing.T) {
	st := &apStore{cc: ccOwner("alice", true, true)}
	runs := &apRuns{}
	f := &apForge{}

	detectWith(st, runs, f)

	if len(runs.calls) != 0 || len(f.notes) != 0 || len(st.upserts) != 0 {
		t.Fatalf("empty candidate set must be a no-op")
	}
}
