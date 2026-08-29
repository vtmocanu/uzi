package poller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
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

	candParams []store.ListAutopilotCandidateIssuesParams

	ops *[]string // shared op-order log across store + forge + runs
}

// ListAutopilotCandidateIssues returns the scripted candidates and CAPTURES its
// params. The capture is not decoration: this fake cannot filter (the predicate is
// SQL), so the only thing a unit test can check about Decision 11b's PRD predicate
// is that the resolved label is actually handed to the query. The filtering itself
// is covered live, in store/autopilot_candidates_integration_test.go.
func (s *apStore) ListAutopilotCandidateIssues(_ context.Context, arg store.ListAutopilotCandidateIssuesParams) ([]store.ListAutopilotCandidateIssuesRow, error) {
	s.candParams = append(s.candParams, arg)
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
	return s.active[arg.IssueIid.Int64], nil
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

type apLabeler struct {
	label string
	// uziLabel is the uzi run-eligibility label the candidate query filters on
	// alongside the autopilot one (PRD #102 M6 Decision 11b; PRD #764 D7 repointed it
	// from the PRD label to the uzi label). Left zero by most cases, which is the
	// "unconfigured" path Autopilot.uziLabel resolves to the compiled-in default.
	uziLabel string
}

func (l apLabeler) AutopilotLabel(context.Context) (string, error) { return l.label, nil }
func (l apLabeler) UziLabel(context.Context) (string, error)       { return l.uziLabel, nil }

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
func (f *apForge) ListIssueComments(context.Context, int64, int64) ([]forge.IssueComment, error) {
	return nil, nil
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
func (f *apForge) ListProjects(context.Context) ([]forge.Project, error) { return nil, nil }
func (f *apForge) ProjectCIConfigPath(context.Context, int64) (string, error) {
	return "", nil
}
func (f *apForge) ListLabels(context.Context, int64) ([]forge.Label, error) { return nil, nil }
func (f *apForge) EnsureLabels(context.Context, int64, []forge.Label) error { return nil }
func (f *apForge) CreateIssue(context.Context, int64, string, string, []string) (forge.Issue, error) {
	return forge.Issue{}, nil
}

// PRD #72 M5: no-op stub — this fake's tests never patch a description.
func (f *apForge) UpdateIssueDescription(context.Context, int64, int64, string) error {
	return nil
}

func (f *apForge) UpdateIssueLabels(context.Context, int64, int64, []string, []string) error {
	return nil
}
func (f *apForge) UserExists(context.Context, string) (bool, error) { return false, nil }
func (f *apForge) GetMergeRequest(context.Context, int64, int64) (forge.MergeRequest, error) {
	return forge.MergeRequest{}, nil
}
func (f *apForge) ListMergeRequestComments(context.Context, int64, int64) ([]forge.MRComment, error) {
	return nil, nil
}
func (f *apForge) ReplyMergeRequestComment(context.Context, int64, int64, string, string) error {
	return nil
}
func (f *apForge) ResolveMergeRequestThread(context.Context, int64, int64, string) error {
	return nil
}
func (f *apForge) TokenInfo(context.Context) (forge.TokenInfo, error) { return forge.TokenInfo{}, nil }
func (f *apForge) ProjectRole(context.Context, int64, int64) (forge.Role, bool, error) {
	return forge.RoleNone, false, nil
}
func (f *apForge) DefaultBranchProtection(context.Context, int64, string, int64) (forge.BranchProtection, error) {
	return forge.BranchProtection{}, nil
}

// Pipeline reads (PRD #6) are unused by this fake — stubbed to satisfy forge.Forge.
func (f *apForge) LatestPipeline(context.Context, int64, string) (forge.Pipeline, error) {
	return forge.Pipeline{}, forge.ErrNoPipeline
}
func (f *apForge) LatestMRPipeline(context.Context, int64, int64) (forge.Pipeline, error) {
	return forge.Pipeline{}, forge.ErrNoPipeline
}
func (f *apForge) ListPipelineJobs(context.Context, int64, int64) ([]forge.Job, error) {
	return nil, nil
}
func (f *apForge) JobLogTail(context.Context, int64, int64, int) (string, error) { return "", nil }
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

// TestAutopilotRunsWithoutPRDLink pins PRD #764 M1 on the autopilot path: an issue
// with the autopilot label but NO prds/*.md link now runs unattended — the old
// PRD-link gate is gone, so no "no PRD link" comment is posted. Pre-change this issue
// (no link, no escape-hatch label) would have been refused with a comment.
func TestAutopilotRunsWithoutPRDLink(t *testing.T) {
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")},
		cc:         ccOwner("alice", true, true),
	}
	runs := &apRuns{}
	// Fresh snapshot: autopilot label, description has no PRD link.
	f := &apForge{
		events: map[int64][]forge.LabelEvent{7: {addEvt(100, "alice")}},
		issue:  forge.Issue{Description: "small typo fix, no PRD", Labels: []string{apLabel}},
	}

	NewAutopilot(st, runs, apLabeler{label: apLabel}).detect(context.Background(), repoRow(), f)

	if len(runs.calls) != 1 {
		t.Fatalf("CreateAutopilotRun calls = %d, want 1 (a link-less run must now start)", len(runs.calls))
	}
	if len(f.notes) != 0 {
		t.Fatalf("a run started; expected no no-PRD-link comment, got %+v", f.notes)
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

// ── PRD #767 M3: assignment-eligibility candidacy + label-keyed consent ────────
//
// Candidacy itself (the "OR bot-assigned" widening of ListAutopilotCandidateIssues)
// is a SQL predicate, so it is proven under a live DB in
// store.TestListAutopilotCandidateIssuesAssignmentEligibilityLiveDB. These fake-store
// tests own the POLLER half: that the poller threads the connection's
// bot_forge_user_id into the candidate query, and that consent/attribution stays keyed
// on the autopilot-label add event no matter how the issue became a candidate — an
// assignment produces no LabelEvent, so it must never start a run or mint a trigger on
// its own. Each case sets the fake's candidate set explicitly (simulating what the SQL
// selected), then asserts the label-keyed path fires (or holds) as designed.

const botForgeUserID int64 = 55

func ccOwnerWithBot(username string, opted, hasToken bool, botID int64) store.GetAutopilotConnectionContextRow {
	cc := ccOwner(username, opted, hasToken)
	cc.BotForgeUserID = botID
	return cc
}

// TestAutopilotThreadsBotIDToCandidateQuery proves the poller feeds the connection's
// bot_forge_user_id into ListAutopilotCandidateIssues (so the SQL can evaluate the
// assignment-eligibility half). Pre-change the params struct had no BotID field, so
// this is net-new plumbing. It also pins the M3 reorder: cc is fetched before the
// candidate list, so its bot id is available to the query.
func TestAutopilotThreadsBotIDToCandidateQuery(t *testing.T) {
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")},
		cc:         ccOwnerWithBot("alice", true, true, botForgeUserID),
	}
	runs := &apRuns{}
	f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(100, "alice")}}}

	detectWith(st, runs, f)

	if len(st.candParams) != 1 {
		t.Fatalf("expected exactly one candidate query, got %d", len(st.candParams))
	}
	if st.candParams[0].BotID != botForgeUserID {
		t.Fatalf("candidate query BotID = %d, want the connection bot id %d", st.candParams[0].BotID, botForgeUserID)
	}
}

// TestAutopilotStartsRunOnAssignedUnlabelledCandidate is the M3 positive: a
// bot-assigned issue with the autopilot label but NO uzi label reaches the run-start
// path. The SQL selected it via the assignment branch (proven live in test A); here
// the candidate is set explicitly and the autopilot-label add event by the owner drives
// the run. This proves an assigned-but-uzi-unlabelled issue can auto-start — impossible
// pre-change, where such an issue would never be a candidate (the query required the uzi
// label). Consent is still label-keyed: the run is attributed to the label adder.
func TestAutopilotStartsRunOnAssignedUnlabelledCandidate(t *testing.T) {
	st := &apStore{
		// Selected by the SQL's assignment branch: autopilot + bot-assigned, no uzi label.
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "someone-else")},
		cc:         ccOwnerWithBot("alice", true, true, botForgeUserID),
	}
	runs := &apRuns{}
	// The autopilot label was added by the owner "alice" — the consent/attribution key.
	f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(100, "alice")}}}

	detectWith(st, runs, f)

	if len(runs.calls) != 1 {
		t.Fatalf("expected a run on the assigned+autopilot candidate, got %d", len(runs.calls))
	}
	// Attributed to the owner via the label-add event, not to any assignee.
	if runs.calls[0].userID != apUserID {
		t.Fatalf("run attributed to %v, want the connection owner %v", runs.calls[0].userID, apUserID)
	}
	if got := lastUpsert(t, st); got.LastEventID != 100 {
		t.Fatalf("recorded event id = %d, want the autopilot-label add event 100", got.LastEventID)
	}
	if len(f.notes) != 0 {
		t.Fatalf("expected no comment on a started run, got %+v", f.notes)
	}
}

// TestAutopilotAssignmentAloneHeldWhenAutopilotRemoved is the M3 negative (D1 / R2):
// the SAME assigned issue with the autopilot label REMOVED is NOT a candidate, so no
// run. Removing the autopilot label drops it from ListAutopilotCandidateIssues (the
// autopilot label is still required — the trigger is unchanged), which the fake models
// with an empty candidate set. Assignment alone must never start a run. Mutation note:
// were the SQL to drop the @label predicate, this bot-assigned issue would wrongly be
// admitted (proven red in test A); at the poller level, a candidate with no
// autopilot-label add event is separately held by TestAutopilotAssignedButNoAddEventHeld.
func TestAutopilotAssignmentAloneHeldWhenAutopilotRemoved(t *testing.T) {
	st := &apStore{
		// Autopilot label removed → the candidate query excludes it entirely.
		candidates: nil,
		cc:         ccOwnerWithBot("alice", true, true, botForgeUserID),
	}
	runs := &apRuns{}
	f := &apForge{}

	detectWith(st, runs, f)

	if len(runs.calls) != 0 || len(f.notes) != 0 || len(st.upserts) != 0 {
		t.Fatalf("assignment without the autopilot label must be held: runs=%d notes=%d upserts=%d",
			len(runs.calls), len(f.notes), len(st.upserts))
	}
}

// TestAutopilotAssignedButNoAddEventHeld pins the poller-level guarantee behind D1: even
// if a bot-assigned issue reaches detectOne as a candidate, an assignment produces NO
// LabelEvent, so with no autopilot-label ADD event (only a removal in the events' latest
// state) lastLabelAdd is nil and nothing fires — no run, no comment, no trigger row. An
// assignment therefore never mints or advances a trigger on its own.
func TestAutopilotAssignedButNoAddEventHeld(t *testing.T) {
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "someone-else")},
		cc:         ccOwnerWithBot("alice", true, true, botForgeUserID),
	}
	runs := &apRuns{}
	// Latest state of the autopilot label is a removal: assignment is the only reason it
	// is a candidate, and assignment has no add event.
	f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(100, "alice"), remEvt(105, "alice")}}}

	detectWith(st, runs, f)

	if len(runs.calls) != 0 || len(f.notes) != 0 || len(st.upserts) != 0 {
		t.Fatalf("a bot-assigned candidate with no live autopilot-label add must be held: runs=%d notes=%d upserts=%d",
			len(runs.calls), len(f.notes), len(st.upserts))
	}
}

// TestAutopilotBotAssignedAfterLabelNotDoubleFired is the "autopilot present, bot
// assigned afterwards" case (M3): the issue was already run once on its autopilot-label
// add (trigger recorded at event 100), then the bot was assigned. Assignment produces no
// new LabelEvent, so a second tick — the issue still a candidate (now via the assignment
// branch as well) with the same latest add event 100 — must NOT double-fire: it is
// swallowed by the transition-once dedup, not treated as a fresh trigger.
func TestAutopilotBotAssignedAfterLabelNotDoubleFired(t *testing.T) {
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")},
		cc:         ccOwnerWithBot("alice", true, true, botForgeUserID),
		triggers:   map[int64]store.AutopilotTrigger{7: {IssueIid: 7, LastEventID: 100}},
	}
	runs := &apRuns{}
	// Same latest add event as the recorded trigger: assignment minted no new event id.
	f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(100, "alice")}}}

	detectWith(st, runs, f)

	if len(runs.calls) != 0 {
		t.Fatalf("assignment after an already-handled label add must not double-fire, got %d runs", len(runs.calls))
	}
	if len(st.upserts) != 0 {
		t.Fatalf("no new trigger should be written for a stale event, got %+v", st.upserts)
	}
}
