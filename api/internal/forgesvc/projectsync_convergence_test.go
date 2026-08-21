package forgesvc

// projectsync_convergence_test.go is PRD #364 M8: the ONE cross-cutting convergence
// test that spans the forward (M5) Status write and the reverse (M6/M7) Status→label
// poll and proves SC-2 — a round trip CONVERGES and never oscillates.
//
// Unlike the per-milestone tests (which hand-wire the reverse `live` status to match
// or differ from the marker), this test drives a SINGLE stateful fake of the GitHub
// project: AddProjectV2Item / SetProjectV2ItemStatus MUTATE the project state that
// ReadProjectV2ItemStatuses then returns, so a forward Status write is genuinely
// observed by the next reverse poll. It also shares one stateful store so the marker
// a forward write advances is genuinely read back by the reverse pass. That is what
// makes convergence a proven property here rather than an asserted fixture.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// --- deterministic node-id helpers ------------------------------------------

// contentNodeID is the issue content node id the fake resolves for an issue number,
// and itemNodeID is the project item node id derived from a content id. Both are
// deterministic and reversible (issueFromContent), so the fake needs no external
// registration ordering between ResolveIssueNodeID and AddProjectV2Item.
func contentNodeID(issueNumber int64) string { return fmt.Sprintf("content-%d", issueNumber) }
func itemNodeID(contentID string) string     { return "item-" + contentID }
func issueFromContent(contentID string) int64 {
	n, err := strconv.ParseInt(strings.TrimPrefix(contentID, "content-"), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// --- statefulProject: a real, mutating model of the GitHub project ----------

// statefulProject models the Projects v2 board with genuine state: SetProjectV2ItemStatus
// and AddProjectV2Item mutate `status`, and ReadProjectV2ItemStatuses reflects those
// mutations. It embeds *fakeForge for the full forge.Forge surface and implements
// forge.ProjectBoardSyncer. setCalls counts uzi-driven Status writes; drag() simulates a
// GitHub-side user drag WITHOUT counting, so a test can tell uzi-driven from user-driven.
type statefulProject struct {
	*fakeForge

	owner, name string

	status    map[string]string // itemID -> live option id ("" = No Status)
	itemIssue map[string]int64  // itemID -> issue number
	byContent map[string]string // contentID -> itemID (idempotent Add)

	setCalls      int // uzi-driven SetProjectV2ItemStatus calls
	addCalls      int
	readCalls     int
	readItemCalls int // single-item live reads (D7 forward guard)
}

var _ forge.ProjectBoardSyncer = (*statefulProject)(nil)

func newStatefulProject() *statefulProject {
	return &statefulProject{
		fakeForge: &fakeForge{},
		owner:     "acme",
		name:      "widgets",
		status:    map[string]string{},
		itemIssue: map[string]int64{},
		byContent: map[string]string{},
	}
}

// seedItem registers an item that is ALREADY on the board (as if a prior adopt/seed put
// it there), at the given live option. Returns its item node id.
func (p *statefulProject) seedItem(issueNumber int64, optionID string) string {
	cid := contentNodeID(issueNumber)
	iid := itemNodeID(cid)
	p.byContent[cid] = iid
	p.itemIssue[iid] = issueNumber
	p.status[iid] = optionID
	return iid
}

// drag simulates a user dragging the card to a column in GitHub: it sets the live option
// directly, WITHOUT counting a uzi-driven Set, so setCalls stays a clean uzi-only signal.
func (p *statefulProject) drag(itemID, optionID string) { p.status[itemID] = optionID }

func (p *statefulProject) TokenInfo(context.Context) (forge.TokenInfo, error) {
	return forge.TokenInfo{Scopes: []string{"repo", "project"}, Active: true}, nil
}

func (p *statefulProject) RepoSlug(context.Context, int64) (string, string, error) {
	return p.owner, p.name, nil
}

func (p *statefulProject) ResolveIssueNodeID(_ context.Context, _, _ string, issueNumber int) (string, error) {
	return contentNodeID(int64(issueNumber)), nil
}

func (p *statefulProject) AddProjectV2Item(_ context.Context, _, contentID string) (string, error) {
	p.addCalls++
	if id, ok := p.byContent[contentID]; ok {
		return id, nil // idempotent: same content -> same item id
	}
	id := itemNodeID(contentID)
	p.byContent[contentID] = id
	p.status[id] = "" // a freshly-added item has No Status
	p.itemIssue[id] = issueFromContent(contentID)
	return id, nil
}

func (p *statefulProject) SetProjectV2ItemStatus(_ context.Context, _, itemID, _, optionID string) error {
	p.setCalls++
	p.status[itemID] = optionID // "" clears to No Status
	return nil
}

func (p *statefulProject) ReadProjectV2ItemStatuses(context.Context, string, string) ([]forge.ProjectV2ItemStatus, error) {
	p.readCalls++
	out := make([]forge.ProjectV2ItemStatus, 0, len(p.status))
	for itemID, opt := range p.status {
		out = append(out, forge.ProjectV2ItemStatus{
			ItemID:      itemID,
			IssueNumber: p.itemIssue[itemID],
			OptionID:    opt,
		})
	}
	return out, nil
}

// ReadProjectV2ItemStatus returns the LIVE option for one item (D7 forward guard) —
// reading the same mutated `status` map ReadProjectV2ItemStatuses reflects, so a
// concurrent drag() is genuinely observed by ForwardMove's live no-op check.
func (p *statefulProject) ReadProjectV2ItemStatus(_ context.Context, itemID, _ string) (string, error) {
	p.readItemCalls++
	return p.status[itemID], nil
}

// Remaining ProjectBoardSyncer methods are unused by ForwardMove/ReverseSync (they are
// adopt/create-path surface); zero values keep the fake a full capability implementer.
func (p *statefulProject) ResolveProjectV2(context.Context, string, int, forge.ProjectV2OwnerKind) (forge.ProjectV2Ref, error) {
	return forge.ProjectV2Ref{ID: "PVT_1", Number: 7}, nil
}
func (p *statefulProject) ProjectV2StatusFieldByName(context.Context, string, string) (forge.ProjectV2StatusField, error) {
	return forge.ProjectV2StatusField{ID: "PVTSSF_1", Name: "Status"}, nil
}
func (p *statefulProject) ResolveProjectV2OwnerID(context.Context, string, forge.ProjectV2OwnerKind) (string, error) {
	return "owner-id", nil
}
func (p *statefulProject) ResolveRepositoryNodeID(context.Context, string, string) (string, error) {
	return "REPO_1", nil
}
func (p *statefulProject) CreateProjectV2(context.Context, string, string, string) (forge.ProjectV2Ref, error) {
	return forge.ProjectV2Ref{}, nil
}
func (p *statefulProject) CreateProjectV2Field(context.Context, string, string, []forge.ProjectV2NewOption) (forge.ProjectV2StatusField, error) {
	return forge.ProjectV2StatusField{}, nil
}
func (p *statefulProject) LinkProjectV2ToRepository(context.Context, string, string) error {
	return nil
}

// --- statefulStore: a real projection store (marker survives across calls) --

// statefulStore is a genuinely stateful ProjectSyncStore: UpsertGithubProjectItem and
// SetGithubProjectItemStatusMarker persist into `items`, and Get/List read it back — so
// a marker a forward write advances is truly read by the next reverse pass. The M6/M7
// tests fake this by hand-editing existingItems between calls; here it is real, which is
// the whole point of the convergence test.
type statefulStore struct {
	repo    store.GetRepoByIDRow
	link    store.GithubProjectLink
	linkErr error
	columns []store.BoardColumn
	issues  []store.Issue
	items   map[int64]store.GithubProjectItem

	setMarkerCalls int
	upsertCalls    int
	deletedItems   []int64
	touchCalls     int
	linkErrs       []string
}

var _ ProjectSyncStore = (*statefulStore)(nil)

func newStatefulStore(repo store.GetRepoByIDRow, link store.GithubProjectLink) *statefulStore {
	return &statefulStore{repo: repo, link: link, items: map[int64]store.GithubProjectItem{}}
}

func (s *statefulStore) GetRepoByID(context.Context, uuid.UUID) (store.GetRepoByIDRow, error) {
	return s.repo, nil
}
func (s *statefulStore) ListBoardColumns(context.Context, uuid.UUID) ([]store.BoardColumn, error) {
	return s.columns, nil
}
func (s *statefulStore) ListIssuesByRepo(context.Context, uuid.UUID) ([]store.Issue, error) {
	return s.issues, nil
}
func (s *statefulStore) UpsertGithubProjectLink(context.Context, store.UpsertGithubProjectLinkParams) (store.GithubProjectLink, error) {
	return store.GithubProjectLink{}, nil
}
func (s *statefulStore) SetGithubProjectLinkError(_ context.Context, arg store.SetGithubProjectLinkErrorParams) error {
	s.linkErrs = append(s.linkErrs, arg.LastError.String)
	return nil
}
func (s *statefulStore) ClearGithubProjectLinkError(context.Context, uuid.UUID) error { return nil }
func (s *statefulStore) UpsertGithubProjectItem(_ context.Context, arg store.UpsertGithubProjectItemParams) (store.GithubProjectItem, error) {
	s.upsertCalls++
	row := store.GithubProjectItem{
		RepoID:             arg.RepoID,
		ForgeIssueIid:      arg.ForgeIssueIid,
		ItemNodeID:         arg.ItemNodeID,
		LastStatusOptionID: arg.LastStatusOptionID,
	}
	s.items[arg.ForgeIssueIid] = row
	return row, nil
}
func (s *statefulStore) ListGithubProjectItems(context.Context, uuid.UUID) ([]store.GithubProjectItem, error) {
	out := make([]store.GithubProjectItem, 0, len(s.items))
	for _, it := range s.items {
		out = append(out, it)
	}
	return out, nil
}
func (s *statefulStore) DeleteGithubProjectItem(_ context.Context, arg store.DeleteGithubProjectItemParams) error {
	s.deletedItems = append(s.deletedItems, arg.ForgeIssueIid)
	delete(s.items, arg.ForgeIssueIid)
	return nil
}
func (s *statefulStore) DeleteGithubProjectLink(context.Context, uuid.UUID) error { return nil }
func (s *statefulStore) GetGithubProjectLinkByRepo(context.Context, uuid.UUID) (store.GithubProjectLink, error) {
	return s.link, s.linkErr
}
func (s *statefulStore) GetGithubProjectItem(_ context.Context, arg store.GetGithubProjectItemParams) (store.GithubProjectItem, error) {
	it, ok := s.items[arg.ForgeIssueIid]
	if !ok {
		return store.GithubProjectItem{}, pgx.ErrNoRows
	}
	return it, nil
}
func (s *statefulStore) SetGithubProjectItemStatusMarker(_ context.Context, arg store.SetGithubProjectItemStatusMarkerParams) error {
	s.setMarkerCalls++
	it := s.items[arg.ForgeIssueIid]
	it.RepoID = arg.RepoID
	it.ForgeIssueIid = arg.ForgeIssueIid
	it.LastStatusOptionID = arg.LastStatusOptionID
	s.items[arg.ForgeIssueIid] = it
	return nil
}
func (s *statefulStore) TouchGithubProjectLinkSynced(context.Context, uuid.UUID) error {
	s.touchCalls++
	return nil
}

func (s *statefulStore) marker(iid int64) string {
	return markerValue(s.items[iid].LastStatusOptionID)
}

// --- the convergence test ---------------------------------------------------

// TestProjectSyncRoundTripConverges is the SC-2 proof. It shares ONE stateful project
// and ONE stateful store across a forward write and multiple reverse polls, so every
// assertion is on production-observable state (the project's live map, the store marker,
// the mover call log) rather than a hand-set fixture.
//
// The full round trip:
//  1. forward move In Progress → project item becomes opt_ip, marker advances to opt_ip;
//  2. reverse poll sees live == marker → ZERO AutoMove (a uzi move does not bounce back);
//  3. a second reverse poll → still ZERO AutoMove (stable across ticks, no oscillation);
//  4. a GitHub-side drag to Human Review → one reverse poll writes one label + advances
//     the marker; a second reverse poll → ZERO AutoMove (converged the other way);
//  5. the forward projection of that same Human Review move is a marker no-op → ZERO new
//     Status writes (the forward side does not bounce the reverse change back either).
func TestProjectSyncRoundTripConverges(t *testing.T) {
	ctx := context.Background()
	repoID := uuid.New()

	project := newStatefulProject()
	// Converged initial state: issue 7 is open, its card is on the board at "No Status",
	// and the store marker matches (NULL). This is a genuine steady state.
	project.seedItem(7, "")

	link := forwardLink(t, map[string]string{"In Progress": "opt_ip", "Human Review": "opt_hr"})
	st := newStatefulStore(githubRepoRow(repoID), link)
	st.columns = []store.BoardColumn{
		{LabelName: "In Progress", Position: 1},
		{LabelName: "Human Review", Position: 2},
	}
	st.issues = []store.Issue{{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t)}}
	st.items[7] = projectItem(repoID, 7, "item-content-7", "") // tracked, NULL marker

	mover := &fakeMover{}
	svc := NewProjectSync(st, fakeForgeBuilder{f: project}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	// --- (1) forward move In Progress ---------------------------------------
	if err := svc.ForwardMove(ctx, repoID, 7, "In Progress"); err != nil {
		t.Fatalf("ForwardMove: %v", err)
	}
	// Production-observable: the live project item and the store marker both advanced,
	// and the item existed so exactly one Status write happened.
	if got := project.status["item-content-7"]; got != "opt_ip" {
		t.Fatalf("after forward move, live project status = %q, want opt_ip", got)
	}
	if got := st.marker(7); got != "opt_ip" {
		t.Fatalf("after forward move, store marker = %q, want opt_ip", got)
	}
	if project.setCalls != 1 {
		t.Fatalf("forward move on an existing item want exactly 1 Set, got %d", project.setCalls)
	}

	// --- (2) reverse poll: convergence no-op --------------------------------
	// The reverse read genuinely observes the forward write (stateful project) and the
	// forward-advanced marker (stateful store), so live == marker → no label writeback.
	if err := svc.ReverseSync(ctx, repoID); err != nil {
		t.Fatalf("ReverseSync (pass 1): %v", err)
	}
	if len(mover.calls) != 0 {
		t.Fatalf("convergence: a uzi forward move must not bounce back, got AutoMove %v", mover.calls)
	}
	if project.setCalls != 1 {
		t.Errorf("reverse must not drive a Status write, setCalls = %d, want 1", project.setCalls)
	}

	// --- (3) a second reverse poll: stable, no oscillation ------------------
	if err := svc.ReverseSync(ctx, repoID); err != nil {
		t.Fatalf("ReverseSync (pass 2): %v", err)
	}
	if len(mover.calls) != 0 {
		t.Fatalf("stability: repeated reverse ticks must stay a no-op, got AutoMove %v", mover.calls)
	}

	// --- (4) a GitHub-side drag to Human Review -----------------------------
	// Set the live option directly (user drag), NOT via uzi, so setCalls is unchanged.
	project.drag("item-content-7", "opt_hr")
	if project.setCalls != 1 {
		t.Fatalf("drag must not count as a uzi Set, setCalls = %d, want 1", project.setCalls)
	}

	if err := svc.ReverseSync(ctx, repoID); err != nil {
		t.Fatalf("ReverseSync (drag): %v", err)
	}
	if len(mover.calls) != 1 || mover.calls[0].issueIID != 7 || mover.calls[0].target != "Human Review" {
		t.Fatalf("drag must drive exactly one AutoMove issue7->\"Human Review\", got %v", mover.calls)
	}
	// The marker advanced to the live option, so the next tick reads live == marker.
	if got := st.marker(7); got != "opt_hr" {
		t.Fatalf("after reverse writeback, store marker = %q, want opt_hr", got)
	}

	// --- second reverse poll after the drag: converged, no further writeback -
	mover.calls = nil
	if err := svc.ReverseSync(ctx, repoID); err != nil {
		t.Fatalf("ReverseSync (post-drag stability): %v", err)
	}
	if len(mover.calls) != 0 {
		t.Fatalf("post-drag convergence: further ticks must be a no-op, got AutoMove %v", mover.calls)
	}

	// --- (5) forward projection of that same Human Review move is a no-op ---
	// uzi's label move to Human Review (the move the AutoMove above represents) projects
	// forward; because the shared marker already equals opt_hr, ForwardMove skips the
	// Status write entirely — the forward side does not bounce the reverse change back.
	setsBefore := project.setCalls
	markerSetsBefore := st.setMarkerCalls
	if err := svc.ForwardMove(ctx, repoID, 7, "Human Review"); err != nil {
		t.Fatalf("ForwardMove (Human Review no-op): %v", err)
	}
	if project.setCalls != setsBefore {
		t.Errorf("forward projection of an already-synced move must make no Status write, setCalls %d -> %d", setsBefore, project.setCalls)
	}
	if st.setMarkerCalls != markerSetsBefore {
		t.Errorf("a forward no-op must not rewrite the marker, setMarkerCalls %d -> %d", markerSetsBefore, st.setMarkerCalls)
	}
	if got := project.status["item-content-7"]; got != "opt_hr" {
		t.Errorf("live status must stay opt_hr through the forward no-op, got %q", got)
	}
}

// TestForwardMoveLiveGuardBeatsConcurrentDrag is the D7 regression: it pins the
// dropped-move race the live-value guard closes. A stored marker can go stale when a
// user drags a card on GitHub between two uzi syncs (no reverse poll has observed the
// drag yet), and a marker-based no-op would then DROP a uzi move that happens to
// re-target the marker's stale value. The live guard reads the board's CURRENT value
// and re-asserts the uzi target, so the uzi move wins.
//
// The interleaving:
//  1. converged: issue 7's card is at opt_ip (In Progress) and the store marker is opt_ip;
//  2. a GitHub-side drag moves the card to opt_hr (Human Review) — marker stays opt_ip
//     because no reverse poll ran;
//  3. uzi re-targets In Progress (opt_ip): marker(opt_ip) == target(opt_ip), so a
//     marker-based guard would NO-OP and drop the move, leaving the board at opt_hr;
//     the live guard reads opt_hr != opt_ip and issues the Set, landing opt_ip;
//  4. a following reverse poll reads live opt_ip == marker opt_ip → no writeback: the
//     uzi value won the race and the system is stable.
func TestForwardMoveLiveGuardBeatsConcurrentDrag(t *testing.T) {
	ctx := context.Background()
	repoID := uuid.New()

	project := newStatefulProject()
	// Converged start: uzi last drove issue 7 to In Progress (opt_ip); marker records it.
	itemID := project.seedItem(7, "opt_ip")

	link := forwardLink(t, map[string]string{"In Progress": "opt_ip", "Human Review": "opt_hr"})
	st := newStatefulStore(githubRepoRow(repoID), link)
	st.columns = []store.BoardColumn{
		{LabelName: "In Progress", Position: 1},
		{LabelName: "Human Review", Position: 2},
	}
	st.issues = []store.Issue{{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t, "In Progress")}}
	st.items[7] = projectItem(repoID, 7, itemID, "opt_ip") // marker still opt_ip (no reverse poll ran)

	mover := &fakeMover{}
	svc := NewProjectSync(st, fakeForgeBuilder{f: project}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	// A concurrent GitHub-side drag to Human Review. setCalls stays 0 (drag is not a uzi Set),
	// and the stored marker is untouched — it is now STALE relative to the live board.
	project.drag(itemID, "opt_hr")
	if project.setCalls != 0 {
		t.Fatalf("drag must not count as a uzi Set, setCalls = %d, want 0", project.setCalls)
	}

	// uzi re-targets In Progress. The stale marker equals the target; only a live read
	// can tell the board has actually moved. The move must NOT be dropped.
	if err := svc.ForwardMove(ctx, repoID, 7, "In Progress"); err != nil {
		t.Fatalf("ForwardMove: %v", err)
	}
	if project.setCalls != 1 {
		t.Fatalf("live guard must re-assert the uzi move over a stale marker: want exactly 1 Set, got %d", project.setCalls)
	}
	if got := project.status[itemID]; got != "opt_ip" {
		t.Fatalf("uzi move must land on the board, live status = %q, want opt_ip", got)
	}
	if got := st.marker(7); got != "opt_ip" {
		t.Fatalf("marker must advance to the uzi target, got %q, want opt_ip", got)
	}

	// Convergence: a following reverse poll now reads live opt_ip == marker opt_ip → no
	// label writeback. The uzi value won and the system is stable.
	if err := svc.ReverseSync(ctx, repoID); err != nil {
		t.Fatalf("ReverseSync: %v", err)
	}
	if len(mover.calls) != 0 {
		t.Fatalf("after the uzi move wins, reverse must no-op (live == marker), got AutoMove %v", mover.calls)
	}
}

// TestProjectSyncForgeIsolation is SC-4: a non-GitHub repo is short-circuited on the
// forge-type gate before any capability call, so both ForwardMove and ReverseSync return
// nil and make ZERO forge/mover calls even though the wired forge is a full syncer.
func TestProjectSyncForgeIsolation(t *testing.T) {
	ctx := context.Background()
	repoID := uuid.New()

	row := githubRepoRow(repoID)
	row.ForgeType = string(forge.TypeGitLab) // not GitHub

	project := newStatefulProject()
	project.seedItem(7, "opt_ip")

	link := forwardLink(t, map[string]string{"In Progress": "opt_ip"})
	st := newStatefulStore(row, link) // link present, so the skip is the forge-type gate
	st.columns = []store.BoardColumn{{LabelName: "In Progress", Position: 1}}
	st.issues = []store.Issue{{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t, "In Progress")}}
	st.items[7] = projectItem(repoID, 7, "item-content-7", "opt_old")

	mover := &fakeMover{}
	svc := NewProjectSync(st, fakeForgeBuilder{f: project}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	if err := svc.ForwardMove(ctx, repoID, 7, "In Progress"); err != nil {
		t.Fatalf("ForwardMove on a non-github repo must be nil, got %v", err)
	}
	if err := svc.ReverseSync(ctx, repoID); err != nil {
		t.Fatalf("ReverseSync on a non-github repo must be nil, got %v", err)
	}
	if project.setCalls != 0 || project.addCalls != 0 || project.readCalls != 0 {
		t.Errorf("non-github repo must make ZERO forge calls, got set=%d add=%d read=%d",
			project.setCalls, project.addCalls, project.readCalls)
	}
	if len(mover.calls) != 0 {
		t.Errorf("non-github repo must make ZERO AutoMove calls, got %v", mover.calls)
	}
	if len(st.deletedItems) != 0 || st.setMarkerCalls != 0 || st.upsertCalls != 0 {
		t.Errorf("non-github repo must not mutate the projection, got deleted=%v setMarker=%d upsert=%d",
			st.deletedItems, st.setMarkerCalls, st.upsertCalls)
	}
}
