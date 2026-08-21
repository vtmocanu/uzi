package forgesvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// --- fakes ------------------------------------------------------------------

// fakeProjectSyncer embeds *fakeForge for the full forge.Forge surface (so it can
// be returned by a ProjectForgeBuilder and type-asserted to ProjectBoardSyncer) and
// scripts the Projects v2 capability methods. TokenInfo is overridden to drive the
// scope preflight.
type fakeProjectSyncer struct {
	*fakeForge

	scopes   []string
	tokenErr error

	slugOwner, slugRepo string
	slugErr             error

	project    forge.ProjectV2Ref
	projectErr error

	fields   map[string]forge.ProjectV2StatusField
	fieldErr map[string]error

	repoNodeID  string
	repoNodeErr error
	linkCalls   int

	// Provisioning (M4): scripted returns + recorded calls for the create mutations.
	ownerID            string
	createProject      forge.ProjectV2Ref
	createProjectErr   error
	createProjectCalls []createProjectCall
	createField        forge.ProjectV2StatusField
	createFieldErr     error
	createFieldCalls   []createFieldCall

	live      []forge.ProjectV2ItemStatus
	readErr   error
	readCalls int

	// Live single-item read (D7 forward no-op guard). liveStatus maps an item node id
	// to its CURRENT forge option id; a nil/absent entry reads "" (No Status).
	// readItemErr forces the read to fail (falls through to the write). readItemCalls
	// records how many single-item reads ForwardMove issued.
	liveStatus    map[string]string
	readItemErr   error
	readItemCalls int

	issueNode map[int]string // issue number -> content node id
	addReturn map[string]string
	addCalls  []string // contentIDs added

	setCalls []setStatusCall
	setErr   error
}

type setStatusCall struct {
	itemID   string
	optionID string
}

type createProjectCall struct {
	ownerID string
	title   string
	repoID  string
}

type createFieldCall struct {
	name    string
	options []forge.ProjectV2NewOption
}

func (f *fakeProjectSyncer) TokenInfo(context.Context) (forge.TokenInfo, error) {
	if f.tokenErr != nil {
		return forge.TokenInfo{}, f.tokenErr
	}
	return forge.TokenInfo{Scopes: f.scopes, Active: true}, nil
}

func (f *fakeProjectSyncer) RepoSlug(context.Context, int64) (string, string, error) {
	return f.slugOwner, f.slugRepo, f.slugErr
}

func (f *fakeProjectSyncer) ResolveProjectV2(context.Context, string, int, forge.ProjectV2OwnerKind) (forge.ProjectV2Ref, error) {
	return f.project, f.projectErr
}

func (f *fakeProjectSyncer) ProjectV2StatusFieldByName(_ context.Context, _, fieldName string) (forge.ProjectV2StatusField, error) {
	if f.fieldErr != nil {
		if err, ok := f.fieldErr[fieldName]; ok {
			return forge.ProjectV2StatusField{}, err
		}
	}
	fld, ok := f.fields[fieldName]
	if !ok {
		return forge.ProjectV2StatusField{}, errors.New("no such field")
	}
	return fld, nil
}

func (f *fakeProjectSyncer) ResolveIssueNodeID(_ context.Context, _, _ string, issueNumber int) (string, error) {
	id, ok := f.issueNode[issueNumber]
	if !ok {
		return "", errors.New("no such issue")
	}
	return id, nil
}

func (f *fakeProjectSyncer) AddProjectV2Item(_ context.Context, _, contentID string) (string, error) {
	f.addCalls = append(f.addCalls, contentID)
	if id, ok := f.addReturn[contentID]; ok {
		return id, nil
	}
	return "item-" + contentID, nil
}

func (f *fakeProjectSyncer) SetProjectV2ItemStatus(_ context.Context, _, itemID, _, optionID string) error {
	f.setCalls = append(f.setCalls, setStatusCall{itemID: itemID, optionID: optionID})
	return f.setErr
}

func (f *fakeProjectSyncer) ReadProjectV2ItemStatuses(context.Context, string, string) ([]forge.ProjectV2ItemStatus, error) {
	f.readCalls++
	return f.live, f.readErr
}

func (f *fakeProjectSyncer) ReadProjectV2ItemStatus(_ context.Context, itemID, _ string) (string, error) {
	f.readItemCalls++
	if f.readItemErr != nil {
		return "", f.readItemErr
	}
	return f.liveStatus[itemID], nil
}

func (f *fakeProjectSyncer) ResolveProjectV2OwnerID(context.Context, string, forge.ProjectV2OwnerKind) (string, error) {
	if f.ownerID != "" {
		return f.ownerID, nil
	}
	return "owner-id", nil
}

func (f *fakeProjectSyncer) ResolveRepositoryNodeID(context.Context, string, string) (string, error) {
	return f.repoNodeID, f.repoNodeErr
}

func (f *fakeProjectSyncer) CreateProjectV2(_ context.Context, ownerID, title, repositoryID string) (forge.ProjectV2Ref, error) {
	f.createProjectCalls = append(f.createProjectCalls, createProjectCall{ownerID: ownerID, title: title, repoID: repositoryID})
	if f.createProjectErr != nil {
		return forge.ProjectV2Ref{}, f.createProjectErr
	}
	return f.createProject, nil
}

func (f *fakeProjectSyncer) CreateProjectV2Field(_ context.Context, _, name string, options []forge.ProjectV2NewOption) (forge.ProjectV2StatusField, error) {
	f.createFieldCalls = append(f.createFieldCalls, createFieldCall{name: name, options: options})
	if f.createFieldErr != nil {
		return forge.ProjectV2StatusField{}, f.createFieldErr
	}
	return f.createField, nil
}

func (f *fakeProjectSyncer) LinkProjectV2ToRepository(context.Context, string, string) error {
	f.linkCalls++
	return nil
}

// fakeForgeBuilder returns a fixed forge for ForgeForConnection.
type fakeForgeBuilder struct {
	f   forge.Forge
	err error
}

func (b fakeForgeBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.f, b.err
}

// fakeSyncSettings scripts the instance kill-switch.
type fakeSyncSettings struct {
	enabled bool
	err     error
}

func (s fakeSyncSettings) GithubProjectSyncEnabled(context.Context) (bool, error) {
	return s.enabled, s.err
}

// fakeProjectStore is a minimal ProjectSyncStore for the adopt/disable tests.
type fakeProjectStore struct {
	repo    store.GetRepoByIDRow
	repoErr error
	columns []store.BoardColumn
	issues  []store.Issue

	links          []store.UpsertGithubProjectLinkParams
	items          map[int64]store.UpsertGithubProjectItemParams
	linkErrs       []string
	linkErrCleared int

	existingItems []store.GithubProjectItem
	deletedItems  []int64
	deletedLink   int

	// Forward-sync (M5) scripting.
	link       store.GithubProjectLink
	linkErr    error
	item       store.GithubProjectItem
	itemErr    error
	markerSets []store.SetGithubProjectItemStatusMarkerParams

	// Observability (M7): count TouchGithubProjectLinkSynced calls.
	touchCalls int
}

func (s *fakeProjectStore) GetRepoByID(context.Context, uuid.UUID) (store.GetRepoByIDRow, error) {
	return s.repo, s.repoErr
}
func (s *fakeProjectStore) ListBoardColumns(context.Context, uuid.UUID) ([]store.BoardColumn, error) {
	return s.columns, nil
}
func (s *fakeProjectStore) ListIssuesByRepo(context.Context, uuid.UUID) ([]store.Issue, error) {
	return s.issues, nil
}
func (s *fakeProjectStore) UpsertGithubProjectLink(_ context.Context, arg store.UpsertGithubProjectLinkParams) (store.GithubProjectLink, error) {
	s.links = append(s.links, arg)
	return store.GithubProjectLink{RepoID: arg.RepoID, ProjectNodeID: arg.ProjectNodeID}, nil
}
func (s *fakeProjectStore) SetGithubProjectLinkError(_ context.Context, arg store.SetGithubProjectLinkErrorParams) error {
	s.linkErrs = append(s.linkErrs, arg.LastError.String)
	return nil
}
func (s *fakeProjectStore) ClearGithubProjectLinkError(context.Context, uuid.UUID) error {
	s.linkErrCleared++
	return nil
}
func (s *fakeProjectStore) UpsertGithubProjectItem(_ context.Context, arg store.UpsertGithubProjectItemParams) (store.GithubProjectItem, error) {
	if s.items == nil {
		s.items = map[int64]store.UpsertGithubProjectItemParams{}
	}
	s.items[arg.ForgeIssueIid] = arg
	return store.GithubProjectItem{RepoID: arg.RepoID, ForgeIssueIid: arg.ForgeIssueIid}, nil
}
func (s *fakeProjectStore) ListGithubProjectItems(context.Context, uuid.UUID) ([]store.GithubProjectItem, error) {
	return s.existingItems, nil
}
func (s *fakeProjectStore) DeleteGithubProjectItem(_ context.Context, arg store.DeleteGithubProjectItemParams) error {
	s.deletedItems = append(s.deletedItems, arg.ForgeIssueIid)
	return nil
}
func (s *fakeProjectStore) DeleteGithubProjectLink(context.Context, uuid.UUID) error {
	s.deletedLink++
	return nil
}
func (s *fakeProjectStore) GetGithubProjectLinkByRepo(context.Context, uuid.UUID) (store.GithubProjectLink, error) {
	return s.link, s.linkErr
}
func (s *fakeProjectStore) GetGithubProjectItem(_ context.Context, arg store.GetGithubProjectItemParams) (store.GithubProjectItem, error) {
	return s.item, s.itemErr
}
func (s *fakeProjectStore) SetGithubProjectItemStatusMarker(_ context.Context, arg store.SetGithubProjectItemStatusMarkerParams) error {
	s.markerSets = append(s.markerSets, arg)
	return nil
}
func (s *fakeProjectStore) TouchGithubProjectLinkSynced(context.Context, uuid.UUID) error {
	s.touchCalls++
	return nil
}

// --- helpers ----------------------------------------------------------------

func labelsJSON(t *testing.T, labels ...string) []byte {
	t.Helper()
	b, err := json.Marshal(labels)
	if err != nil {
		t.Fatalf("marshal labels: %v", err)
	}
	return b
}

func githubRepoRow(repoID uuid.UUID) store.GetRepoByIDRow {
	return store.GetRepoByIDRow{
		ID:              repoID,
		ForgeProjectID:  42,
		ForgeType:       string(forge.TypeGitHub),
		BaseUrl:         "https://github.com",
		TokenCiphertext: []byte("ct"),
	}
}

// --- tests ------------------------------------------------------------------

// TestAdoptSeedsBoard is the full happy path: build the column→option map, seed
// each open issue to its target option (clearing Open issues), skip closed, add a
// missing item once, leave an already-matching item untouched, skip a column with
// no mapped option, and persist link + items + the unmatched-column note.
func TestAdoptSeedsBoard(t *testing.T) {
	repoID := uuid.New()
	syncer := &fakeProjectSyncer{
		fakeForge: &fakeForge{},
		scopes:    []string{"repo", "project"},
		slugOwner: "acme", slugRepo: "widgets",
		project:    forge.ProjectV2Ref{ID: "PVT_1", Number: 7, Title: "Board"},
		repoNodeID: "REPO_1",
		fields: map[string]forge.ProjectV2StatusField{
			"Status": {ID: "PVTSSF_1", Name: "Status", Options: []forge.ProjectV2Option{
				{ID: "opt_ip", Name: "In Progress"},
				{ID: "opt_hr", Name: "Human Review"},
			}},
		},
		// Live board: issue 2 currently has some option (to be cleared); issue 5 is
		// already In Progress (idempotent, no set).
		live: []forge.ProjectV2ItemStatus{
			{ItemID: "item2", IssueNumber: 2, OptionID: "opt_stale"},
			{ItemID: "item5", IssueNumber: 5, OptionID: "opt_ip"},
		},
		issueNode: map[int]string{1: "content1"},
	}
	st := &fakeProjectStore{
		repo: githubRepoRow(repoID),
		columns: []store.BoardColumn{
			{LabelName: "In Progress", Position: 1},
			{LabelName: "Human Review", Position: 2},
			{LabelName: "Later", Position: 3}, // unmatched: no Status option
		},
		issues: []store.Issue{
			{ForgeIssueIid: 1, State: "opened", Labels: labelsJSON(t, "In Progress")},
			{ForgeIssueIid: 2, State: "opened", Labels: labelsJSON(t)},                // Open -> clear
			{ForgeIssueIid: 3, State: "closed", Labels: labelsJSON(t, "In Progress")}, // skipped (D1)
			{ForgeIssueIid: 4, State: "opened", Labels: labelsJSON(t, "Later")},       // unmatched column -> skipped
			{ForgeIssueIid: 5, State: "opened", Labels: labelsJSON(t, "In Progress")}, // idempotent
		},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)

	if err := svc.Adopt(context.Background(), repoID, 7, forge.OwnerUser); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	// Link persisted once, owned_by_uzi false, with the two matched columns.
	if len(st.links) != 1 {
		t.Fatalf("want 1 link upsert, got %d", len(st.links))
	}
	link := st.links[0]
	if link.OwnedByUzi {
		t.Errorf("owned_by_uzi must be false on adopt")
	}
	if link.ProjectNodeID != "PVT_1" || link.StatusFieldID != "PVTSSF_1" || link.ProjectNumber != 7 {
		t.Errorf("link coordinates wrong: %+v", link)
	}
	var gotMap map[string]string
	if err := json.Unmarshal(link.StatusOptions, &gotMap); err != nil {
		t.Fatalf("status_options not valid json: %v", err)
	}
	wantMap := map[string]string{"In Progress": "opt_ip", "Human Review": "opt_hr"}
	if len(gotMap) != len(wantMap) || gotMap["In Progress"] != "opt_ip" || gotMap["Human Review"] != "opt_hr" {
		t.Errorf("column->option map = %v, want %v", gotMap, wantMap)
	}

	// Best-effort repo link attempted.
	if syncer.linkCalls != 1 {
		t.Errorf("want LinkProjectV2ToRepository called once, got %d", syncer.linkCalls)
	}

	// Issue 1 added (missing) then set to opt_ip; issue 2 cleared; issue 5 untouched.
	if len(syncer.addCalls) != 1 || syncer.addCalls[0] != "content1" {
		t.Errorf("want issue 1 content added once, got %v", syncer.addCalls)
	}
	sets := map[string]string{}
	for _, c := range syncer.setCalls {
		sets[c.itemID] = c.optionID
	}
	if got := sets["item-content1"]; got != "opt_ip" {
		t.Errorf("issue 1 set to %q, want opt_ip", got)
	}
	if got, ok := sets["item2"]; !ok || got != "" {
		t.Errorf("issue 2 (Open) should be cleared to \"\", got %q ok=%v", got, ok)
	}
	if _, ok := sets["item5"]; ok {
		t.Errorf("issue 5 already In Progress must not be re-set (idempotent)")
	}
	if len(syncer.setCalls) != 2 {
		t.Errorf("want exactly 2 status sets (issue1 + issue2 clear), got %v", syncer.setCalls)
	}

	// Items persisted for 1,2,5 only (3 closed, 4 unmatched).
	for _, iid := range []int64{1, 2, 5} {
		if _, ok := st.items[iid]; !ok {
			t.Errorf("want item persisted for issue %d", iid)
		}
	}
	for _, iid := range []int64{3, 4} {
		if _, ok := st.items[iid]; ok {
			t.Errorf("issue %d must NOT be persisted", iid)
		}
	}
	// Issue 2 marker is NULL (cleared/No Status); issue 1 marker is the mapped option.
	if m := st.items[2].LastStatusOptionID; m.Valid {
		t.Errorf("issue 2 marker should be NULL (cleared), got %+v", m)
	}
	if m := st.items[1].LastStatusOptionID; !m.Valid || m.String != "opt_ip" {
		t.Errorf("issue 1 marker = %+v, want opt_ip", m)
	}
	if st.items[1].ItemNodeID != "item-content1" {
		t.Errorf("issue 1 item node id = %q, want item-content1", st.items[1].ItemNodeID)
	}

	// An unmatched column ("Later") is a non-fatal ADVISORY, not an error: a
	// successful adopt must still clear last_error and must NOT write the note to
	// last_error (else a UI/poller reading non-null last_error would misread a
	// healthy link). The note is surfaced via logs; a health surface is M7.
	if st.linkErrCleared != 1 {
		t.Errorf("successful adopt (even with unmatched columns) must clear last_error once, got %d", st.linkErrCleared)
	}
	if len(st.linkErrs) != 0 {
		t.Errorf("advisory note must not be written to last_error, got %v", st.linkErrs)
	}
}

// TestAdoptCleanRunClearsError: a board where every column matches records no note
// and clears last_error.
func TestAdoptCleanRunClearsError(t *testing.T) {
	repoID := uuid.New()
	syncer := &fakeProjectSyncer{
		fakeForge: &fakeForge{},
		scopes:    []string{"repo", "project"},
		slugOwner: "acme", slugRepo: "widgets",
		project:    forge.ProjectV2Ref{ID: "PVT_1", Number: 7},
		repoNodeID: "REPO_1",
		fields: map[string]forge.ProjectV2StatusField{
			"Status": {ID: "PVTSSF_1", Name: "Status", Options: []forge.ProjectV2Option{{ID: "opt_ip", Name: "In Progress"}}},
		},
		issueNode: map[int]string{1: "content1"},
	}
	st := &fakeProjectStore{
		repo:    githubRepoRow(repoID),
		columns: []store.BoardColumn{{LabelName: "In Progress", Position: 1}},
		issues:  []store.Issue{{ForgeIssueIid: 1, State: "opened", Labels: labelsJSON(t, "In Progress")}},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	if err := svc.Adopt(context.Background(), repoID, 7, forge.OwnerUser); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if st.linkErrCleared != 1 {
		t.Errorf("clean run should clear last_error once, got %d", st.linkErrCleared)
	}
	if len(st.linkErrs) != 0 {
		t.Errorf("clean run should record no note, got %v", st.linkErrs)
	}
}

// TestAdoptStatusFieldFallback: no built-in "Status" field, but a "uzi Status" field
// exists — the adopt path falls back to it.
func TestAdoptStatusFieldFallback(t *testing.T) {
	repoID := uuid.New()
	syncer := &fakeProjectSyncer{
		fakeForge: &fakeForge{},
		scopes:    []string{"repo", "project"},
		slugOwner: "acme", slugRepo: "widgets",
		project:    forge.ProjectV2Ref{ID: "PVT_1", Number: 7},
		repoNodeID: "REPO_1",
		fieldErr:   map[string]error{"Status": errors.New("no Status field")},
		fields: map[string]forge.ProjectV2StatusField{
			"uzi Status": {ID: "PVTSSF_UZI", Name: "uzi Status", Options: []forge.ProjectV2Option{{ID: "opt_ip", Name: "In Progress"}}},
		},
	}
	st := &fakeProjectStore{
		repo:    githubRepoRow(repoID),
		columns: []store.BoardColumn{{LabelName: "In Progress", Position: 1}},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	if err := svc.Adopt(context.Background(), repoID, 7, forge.OwnerUser); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if len(st.links) != 1 || st.links[0].StatusFieldID != "PVTSSF_UZI" {
		t.Fatalf("want fallback field PVTSSF_UZI persisted, got %+v", st.links)
	}
}

func TestAdoptDisabled(t *testing.T) {
	st := &fakeProjectStore{repo: githubRepoRow(uuid.New())}
	svc := NewProjectSync(st, fakeForgeBuilder{}, fakeSyncSettings{enabled: false}, nil)
	err := svc.Adopt(context.Background(), uuid.New(), 7, forge.OwnerUser)
	if !errors.Is(err, ErrProjectSyncDisabled) {
		t.Fatalf("want ErrProjectSyncDisabled, got %v", err)
	}
	if len(st.links) != 0 {
		t.Errorf("disabled adopt must not write a link")
	}
}

func TestAdoptNotGitHub(t *testing.T) {
	repoID := uuid.New()
	row := githubRepoRow(repoID)
	row.ForgeType = string(forge.TypeGitLab)
	st := &fakeProjectStore{repo: row}
	svc := NewProjectSync(st, fakeForgeBuilder{}, fakeSyncSettings{enabled: true}, nil)
	err := svc.Adopt(context.Background(), repoID, 7, forge.OwnerUser)
	if !errors.Is(err, ErrProjectSyncNotGitHub) {
		t.Fatalf("want ErrProjectSyncNotGitHub, got %v", err)
	}
}

func TestAdoptUnsupportedForge(t *testing.T) {
	repoID := uuid.New()
	st := &fakeProjectStore{repo: githubRepoRow(repoID)}
	// A plain *fakeForge does not implement ProjectBoardSyncer.
	svc := NewProjectSync(st, fakeForgeBuilder{f: &fakeForge{}}, fakeSyncSettings{enabled: true}, nil)
	err := svc.Adopt(context.Background(), repoID, 7, forge.OwnerUser)
	if !errors.Is(err, ErrProjectSyncUnsupported) {
		t.Fatalf("want ErrProjectSyncUnsupported, got %v", err)
	}
}

func TestAdoptMissingProjectScope(t *testing.T) {
	repoID := uuid.New()
	syncer := &fakeProjectSyncer{fakeForge: &fakeForge{}, scopes: []string{"repo"}}
	st := &fakeProjectStore{repo: githubRepoRow(repoID)}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	err := svc.Adopt(context.Background(), repoID, 7, forge.OwnerUser)
	if !errors.Is(err, ErrProjectSyncMissingScope) {
		t.Fatalf("want ErrProjectSyncMissingScope, got %v", err)
	}
	if len(st.links) != 0 {
		t.Errorf("missing-scope adopt must not write a link")
	}
}

// TestAdoptIntrospectionUnsupportedProceeds: an unintrospectable PAT proceeds
// best-effort rather than blocking on the missing scope.
func TestAdoptIntrospectionUnsupportedProceeds(t *testing.T) {
	repoID := uuid.New()
	syncer := &fakeProjectSyncer{
		fakeForge: &fakeForge{},
		tokenErr:  forge.ErrTokenIntrospectionUnsupported,
		slugOwner: "acme", slugRepo: "widgets",
		project:    forge.ProjectV2Ref{ID: "PVT_1", Number: 7},
		repoNodeID: "REPO_1",
		fields: map[string]forge.ProjectV2StatusField{
			"Status": {ID: "PVTSSF_1", Name: "Status"},
		},
	}
	st := &fakeProjectStore{repo: githubRepoRow(repoID)}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	if err := svc.Adopt(context.Background(), repoID, 7, forge.OwnerUser); err != nil {
		t.Fatalf("introspection-unsupported adopt should proceed, got %v", err)
	}
	if len(st.links) != 1 {
		t.Errorf("want a link persisted on the best-effort path")
	}
}

// TestAdoptRepoNotFound maps a store no-rows to the error the handler renders 404.
func TestAdoptRepoNotFound(t *testing.T) {
	st := &fakeProjectStore{repoErr: pgx.ErrNoRows}
	svc := NewProjectSync(st, fakeForgeBuilder{}, fakeSyncSettings{enabled: true}, nil)
	err := svc.Adopt(context.Background(), uuid.New(), 7, forge.OwnerUser)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("want pgx.ErrNoRows, got %v", err)
	}
}

// --- Provision (M4) ----------------------------------------------------------

// validGithubColors is GitHub's SINGLE_SELECT option color enum: every created
// option must carry one of these.
var validGithubColors = map[string]bool{
	"BLUE": true, "GRAY": true, "GREEN": true, "ORANGE": true,
	"PINK": true, "PURPLE": true, "RED": true, "YELLOW": true,
}

// provisionSyncer builds a fake ProjectBoardSyncer scripted to create a project +
// field with the given created option ids.
func provisionSyncer(fieldID string, createdOptions []forge.ProjectV2Option) *fakeProjectSyncer {
	return &fakeProjectSyncer{
		fakeForge: &fakeForge{},
		scopes:    []string{"repo", "project"},
		slugOwner: "acme", slugRepo: "widgets",
		ownerID:       "owner-node-1",
		repoNodeID:    "REPO_1",
		createProject: forge.ProjectV2Ref{ID: "PVT_NEW", Number: 9, Title: "created"},
		createField:   forge.ProjectV2StatusField{ID: fieldID, Name: "uzi Status", Options: createdOptions},
		issueNode:     map[int]string{1: "content1"},
	}
}

// TestProvisionCreatesBoardAndSeeds is the full happy path: create the project with
// the resolved owner/repo node ids + defaulted title, create the "uzi Status" field
// from the board columns (valid colors, names == columns), build the column→option
// map from the CREATED field's ids, persist the link with owned_by_uzi=TRUE, and seed
// each open issue.
func TestProvisionCreatesBoardAndSeeds(t *testing.T) {
	repoID := uuid.New()
	syncer := provisionSyncer("PVTSSF_NEW", []forge.ProjectV2Option{
		{ID: "opt_ip", Name: "In Progress"},
		{ID: "opt_hr", Name: "Human Review"},
	})
	st := &fakeProjectStore{
		repo:    githubRepoRow(repoID),
		linkErr: pgx.ErrNoRows, // no existing link → provision proceeds
		columns: []store.BoardColumn{
			{LabelName: "In Progress", Position: 1},
			{LabelName: "Human Review", Position: 2},
		},
		issues: []store.Issue{
			{ForgeIssueIid: 1, State: "opened", Labels: labelsJSON(t, "In Progress")},
		},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)

	if err := svc.Provision(context.Background(), repoID, forge.OwnerUser, ""); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Project created with resolved node ids and a defaulted title (name is "widgets").
	if len(syncer.createProjectCalls) != 1 {
		t.Fatalf("want 1 CreateProjectV2 call, got %d", len(syncer.createProjectCalls))
	}
	cp := syncer.createProjectCalls[0]
	if cp.ownerID != "owner-node-1" || cp.repoID != "REPO_1" {
		t.Errorf("create project used wrong node ids: %+v", cp)
	}
	if cp.title == "" {
		t.Errorf("empty title should have been defaulted, got %q", cp.title)
	}

	// Field created as "uzi Status", options == the board column names, valid colors.
	if len(syncer.createFieldCalls) != 1 {
		t.Fatalf("want 1 CreateProjectV2Field call, got %d", len(syncer.createFieldCalls))
	}
	cf := syncer.createFieldCalls[0]
	if cf.name != "uzi Status" {
		t.Errorf("field name = %q, want \"uzi Status\"", cf.name)
	}
	if len(cf.options) != 2 || cf.options[0].Name != "In Progress" || cf.options[1].Name != "Human Review" {
		t.Errorf("created options = %+v, want the two board columns in order", cf.options)
	}
	for _, o := range cf.options {
		if !validGithubColors[o.Color] {
			t.Errorf("option %q has invalid color %q", o.Name, o.Color)
		}
	}

	// Link persisted once, owned_by_uzi TRUE, coordinates from the created project +
	// field, and the map built from the CREATED option ids.
	if len(st.links) != 1 {
		t.Fatalf("want 1 link upsert, got %d", len(st.links))
	}
	link := st.links[0]
	if !link.OwnedByUzi {
		t.Errorf("owned_by_uzi must be TRUE on provision")
	}
	if link.ProjectNodeID != "PVT_NEW" || link.StatusFieldID != "PVTSSF_NEW" || link.ProjectNumber != 9 {
		t.Errorf("link coordinates wrong: %+v", link)
	}
	var gotMap map[string]string
	if err := json.Unmarshal(link.StatusOptions, &gotMap); err != nil {
		t.Fatalf("status_options not valid json: %v", err)
	}
	if gotMap["In Progress"] != "opt_ip" || gotMap["Human Review"] != "opt_hr" {
		t.Errorf("column->option map = %v, want the created option ids", gotMap)
	}

	// Best-effort repo link attempted, and issue 1 seeded to its option.
	if syncer.linkCalls != 1 {
		t.Errorf("want LinkProjectV2ToRepository called once, got %d", syncer.linkCalls)
	}
	if len(syncer.addCalls) != 1 || syncer.addCalls[0] != "content1" {
		t.Errorf("want issue 1 added once, got %v", syncer.addCalls)
	}
	if len(syncer.setCalls) != 1 || syncer.setCalls[0].optionID != "opt_ip" {
		t.Errorf("want issue 1 set to opt_ip, got %v", syncer.setCalls)
	}
	if st.linkErrCleared != 1 {
		t.Errorf("successful provision must clear last_error once, got %d", st.linkErrCleared)
	}
	if len(st.linkErrs) != 0 {
		t.Errorf("successful provision must not stamp last_error, got %v", st.linkErrs)
	}
}

// TestProvisionCustomTitle: a non-empty title is passed through to CreateProjectV2.
func TestProvisionCustomTitle(t *testing.T) {
	repoID := uuid.New()
	syncer := provisionSyncer("PVTSSF_NEW", []forge.ProjectV2Option{{ID: "opt_ip", Name: "In Progress"}})
	st := &fakeProjectStore{
		repo:    githubRepoRow(repoID),
		linkErr: pgx.ErrNoRows,
		columns: []store.BoardColumn{{LabelName: "In Progress", Position: 1}},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	if err := svc.Provision(context.Background(), repoID, forge.OwnerUser, "Roadmap"); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if syncer.createProjectCalls[0].title != "Roadmap" {
		t.Errorf("title = %q, want Roadmap", syncer.createProjectCalls[0].title)
	}
}

// TestProvisionColorsCycle: more columns than the 8-color palette still resolves —
// every option carries a valid color and the palette wraps.
func TestProvisionColorsCycle(t *testing.T) {
	repoID := uuid.New()
	var columns []store.BoardColumn
	var created []forge.ProjectV2Option
	for i := 0; i < 10; i++ {
		name := "col" + string(rune('A'+i))
		columns = append(columns, store.BoardColumn{LabelName: name, Position: int32(i)})
		created = append(created, forge.ProjectV2Option{ID: "o" + name, Name: name})
	}
	syncer := provisionSyncer("PVTSSF_NEW", created)
	st := &fakeProjectStore{repo: githubRepoRow(repoID), linkErr: pgx.ErrNoRows, columns: columns}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	if err := svc.Provision(context.Background(), repoID, forge.OwnerUser, ""); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	cf := syncer.createFieldCalls[0]
	if len(cf.options) != 10 {
		t.Fatalf("want 10 options, got %d", len(cf.options))
	}
	for _, o := range cf.options {
		if !validGithubColors[o.Color] {
			t.Errorf("option %q has invalid color %q", o.Name, o.Color)
		}
	}
}

// TestProvisionSkipPaths: the shared preconditions block provisioning with zero
// create calls — disabled, non-github, missing scope.
func TestProvisionSkipPaths(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		repoID := uuid.New()
		syncer := provisionSyncer("f", nil)
		st := &fakeProjectStore{repo: githubRepoRow(repoID)}
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: false}, nil)
		if err := svc.Provision(context.Background(), repoID, forge.OwnerUser, ""); !errors.Is(err, ErrProjectSyncDisabled) {
			t.Fatalf("want ErrProjectSyncDisabled, got %v", err)
		}
		if len(syncer.createProjectCalls) != 0 || len(st.links) != 0 {
			t.Errorf("disabled provision must create nothing")
		}
	})
	t.Run("non-github", func(t *testing.T) {
		repoID := uuid.New()
		row := githubRepoRow(repoID)
		row.ForgeType = string(forge.TypeGitLab)
		syncer := provisionSyncer("f", nil)
		st := &fakeProjectStore{repo: row}
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		if err := svc.Provision(context.Background(), repoID, forge.OwnerUser, ""); !errors.Is(err, ErrProjectSyncNotGitHub) {
			t.Fatalf("want ErrProjectSyncNotGitHub, got %v", err)
		}
		if len(syncer.createProjectCalls) != 0 {
			t.Errorf("non-github provision must create nothing")
		}
	})
	t.Run("missing scope", func(t *testing.T) {
		repoID := uuid.New()
		syncer := provisionSyncer("f", nil)
		syncer.scopes = []string{"repo"} // no project scope
		st := &fakeProjectStore{repo: githubRepoRow(repoID)}
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		if err := svc.Provision(context.Background(), repoID, forge.OwnerUser, ""); !errors.Is(err, ErrProjectSyncMissingScope) {
			t.Fatalf("want ErrProjectSyncMissingScope, got %v", err)
		}
		if len(syncer.createProjectCalls) != 0 || len(st.links) != 0 {
			t.Errorf("missing-scope provision must create nothing")
		}
	})
}

// TestProvisionCreateErrorStamps: a create failure after no link exists returns the
// error and best-effort stamps last_error (matching zero rows is fine).
func TestProvisionCreateErrorStamps(t *testing.T) {
	repoID := uuid.New()
	syncer := provisionSyncer("f", nil)
	syncer.createProjectErr = errors.New("graphql boom")
	st := &fakeProjectStore{repo: githubRepoRow(repoID), linkErr: pgx.ErrNoRows, columns: []store.BoardColumn{{LabelName: "In Progress"}}}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	err := svc.Provision(context.Background(), repoID, forge.OwnerUser, "")
	if err == nil {
		t.Fatalf("want an error on create failure")
	}
	if len(st.links) != 0 {
		t.Errorf("no link should be persisted when create fails")
	}
	if len(st.linkErrs) != 1 {
		t.Errorf("want last_error stamped once, got %v", st.linkErrs)
	}
}

// TestProvisionRefusesWhenAlreadyLinked: Provision creates a NEW board, so it must
// refuse (not orphan the prior one) when a link row already exists — the guard against
// duplicate GitHub projects an admin double-submit would otherwise create.
func TestProvisionRefusesWhenAlreadyLinked(t *testing.T) {
	repoID := uuid.New()
	syncer := provisionSyncer("PVTSSF_NEW", []forge.ProjectV2Option{{ID: "opt_ip", Name: "In Progress"}})
	st := &fakeProjectStore{
		repo:    githubRepoRow(repoID),
		link:    store.GithubProjectLink{RepoID: repoID, ProjectNodeID: "PVT_existing"}, // linkErr nil → a row exists
		columns: []store.BoardColumn{{LabelName: "In Progress", Position: 1}},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	if err := svc.Provision(context.Background(), repoID, forge.OwnerUser, ""); !errors.Is(err, ErrProjectSyncAlreadyLinked) {
		t.Fatalf("want ErrProjectSyncAlreadyLinked, got %v", err)
	}
	if len(syncer.createProjectCalls) != 0 {
		t.Errorf("must not create a board when a link already exists, got %d", len(syncer.createProjectCalls))
	}
	if len(st.links) != 0 {
		t.Errorf("must not repoint the link, got %d upserts", len(st.links))
	}
}

func TestDisableDeletesItemsAndLink(t *testing.T) {
	repoID := uuid.New()
	st := &fakeProjectStore{
		existingItems: []store.GithubProjectItem{
			{RepoID: repoID, ForgeIssueIid: 1},
			{RepoID: repoID, ForgeIssueIid: 2},
		},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{}, fakeSyncSettings{enabled: true}, nil)
	if err := svc.Disable(context.Background(), repoID); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if len(st.deletedItems) != 2 {
		t.Errorf("want 2 item deletes, got %v", st.deletedItems)
	}
	if st.deletedLink != 1 {
		t.Errorf("want 1 link delete, got %d", st.deletedLink)
	}
}

// (M7) Disable is local-only teardown: it deletes uzi's item + link rows for BOTH
// owned_by_uzi values and NEVER issues a destructive GitHub mutation. There is no
// archive/delete-project method to call, so we assert the syncer's mutating methods
// (AddProjectV2Item / SetProjectV2ItemStatus) are untouched as a proxy, and that only
// Delete* store calls happen.
func TestDisableTeardownLocalOnly(t *testing.T) {
	for _, ownedByUzi := range []bool{false, true} {
		name := "adopted (owned_by_uzi=false)"
		if ownedByUzi {
			name = "uzi-owned (owned_by_uzi=true)"
		}
		t.Run(name, func(t *testing.T) {
			repoID := uuid.New()
			syncer := &fakeProjectSyncer{fakeForge: &fakeForge{}}
			st := &fakeProjectStore{
				link: store.GithubProjectLink{RepoID: repoID, OwnedByUzi: ownedByUzi},
				existingItems: []store.GithubProjectItem{
					{RepoID: repoID, ForgeIssueIid: 1},
					{RepoID: repoID, ForgeIssueIid: 2},
				},
			}
			svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
			if err := svc.Disable(context.Background(), repoID); err != nil {
				t.Fatalf("Disable: %v", err)
			}
			// Local rows removed.
			if len(st.deletedItems) != 2 {
				t.Errorf("want 2 item deletes, got %v", st.deletedItems)
			}
			if st.deletedLink != 1 {
				t.Errorf("want 1 link delete, got %d", st.deletedLink)
			}
			// No destructive/mutating GitHub call on disable (no archive, no project
			// delete). The proxy: no item add, no status set.
			if len(syncer.addCalls) != 0 || len(syncer.setCalls) != 0 {
				t.Errorf("disable must make no GitHub mutation, got add=%v set=%v", syncer.addCalls, syncer.setCalls)
			}
			// No projection WRITE (upsert/marker) either — teardown only deletes.
			if len(st.items) != 0 || len(st.markerSets) != 0 {
				t.Errorf("teardown must not upsert/rewrite, got items=%v markerSets=%v", st.items, st.markerSets)
			}
		})
	}
}

// (M7) An already-disabled repo (no link row) is a no-op: no deletes, no error.
func TestDisableAlreadyDisabled(t *testing.T) {
	repoID := uuid.New()
	st := &fakeProjectStore{linkErr: pgx.ErrNoRows}
	svc := NewProjectSync(st, fakeForgeBuilder{}, fakeSyncSettings{enabled: true}, nil)
	if err := svc.Disable(context.Background(), repoID); err != nil {
		t.Fatalf("Disable on an already-disabled repo must be nil, got %v", err)
	}
	if len(st.deletedItems) != 0 || st.deletedLink != 0 {
		t.Errorf("no-link disable must delete nothing, got items=%v link=%d", st.deletedItems, st.deletedLink)
	}
}

// (M7) ProjectSyncStatus returns the link's health fields + item count, and passes
// pgx.ErrNoRows through for a repo with no link (the handler maps it to 404).
func TestProjectSyncStatus(t *testing.T) {
	repoID := uuid.New()
	synced := pgtype.Timestamptz{Time: time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC), Valid: true}
	st := &fakeProjectStore{
		link: store.GithubProjectLink{
			RepoID:        repoID,
			ProjectNumber: 42,
			OwnedByUzi:    true,
			LastSyncedAt:  synced,
			LastError:     pgtype.Text{String: "graphql boom", Valid: true},
		},
		existingItems: []store.GithubProjectItem{
			{RepoID: repoID, ForgeIssueIid: 1},
			{RepoID: repoID, ForgeIssueIid: 2},
			{RepoID: repoID, ForgeIssueIid: 3},
		},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{}, fakeSyncSettings{enabled: true}, nil)
	got, err := svc.ProjectSyncStatus(context.Background(), repoID)
	if err != nil {
		t.Fatalf("ProjectSyncStatus: %v", err)
	}
	if got.ProjectNumber != 42 || !got.OwnedByUzi || got.ItemCount != 3 {
		t.Errorf("status = %+v, want project 42 owned item_count 3", got)
	}
	if !got.LastSyncedAt.Valid || !got.LastSyncedAt.Time.Equal(synced.Time) {
		t.Errorf("last_synced_at = %+v, want %v", got.LastSyncedAt, synced.Time)
	}
	if !got.LastError.Valid || got.LastError.String != "graphql boom" {
		t.Errorf("last_error = %+v, want graphql boom", got.LastError)
	}
}

func TestProjectSyncStatusNoLink(t *testing.T) {
	st := &fakeProjectStore{linkErr: pgx.ErrNoRows}
	svc := NewProjectSync(st, fakeForgeBuilder{}, fakeSyncSettings{enabled: true}, nil)
	_, err := svc.ProjectSyncStatus(context.Background(), uuid.New())
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("want pgx.ErrNoRows for a repo with no link, got %v", err)
	}
}

// --- ForwardMove (M5) --------------------------------------------------------

// forwardLink builds a link row whose StatusOptions maps the given columns.
func forwardLink(t *testing.T, columnOption map[string]string) store.GithubProjectLink {
	t.Helper()
	b, err := json.Marshal(columnOption)
	if err != nil {
		t.Fatalf("marshal status options: %v", err)
	}
	return store.GithubProjectLink{
		ProjectNodeID: "PVT_1",
		StatusFieldID: "PVTSSF_1",
		StatusOptions: b,
	}
}

// forwardSyncer builds a ProjectBoardSyncer fake with the given scopes for M5.
func forwardSyncer() *fakeProjectSyncer {
	return &fakeProjectSyncer{
		fakeForge: &fakeForge{},
		slugOwner: "acme", slugRepo: "widgets",
		issueNode: map[int]string{7: "content7"},
	}
}

// (a) A mapped target column sets the item's Status to the option and advances the
// marker.
func TestForwardMoveSetsMappedOption(t *testing.T) {
	repoID := uuid.New()
	syncer := forwardSyncer()
	st := &fakeProjectStore{
		repo: githubRepoRow(repoID),
		link: forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		item: store.GithubProjectItem{RepoID: repoID, ForgeIssueIid: 7, ItemNodeID: "item7"},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)

	if err := svc.ForwardMove(context.Background(), repoID, 7, "In Progress"); err != nil {
		t.Fatalf("ForwardMove: %v", err)
	}
	if len(syncer.setCalls) != 1 || syncer.setCalls[0].itemID != "item7" || syncer.setCalls[0].optionID != "opt_ip" {
		t.Fatalf("want one set item7->opt_ip, got %v", syncer.setCalls)
	}
	if len(st.markerSets) != 1 || !st.markerSets[0].LastStatusOptionID.Valid || st.markerSets[0].LastStatusOptionID.String != "opt_ip" {
		t.Errorf("want marker advanced to opt_ip, got %+v", st.markerSets)
	}
	if len(st.linkErrs) != 0 {
		t.Errorf("success must not stamp last_error, got %v", st.linkErrs)
	}
}

// (b) The Open target ("") clears Status and stores a NULL marker. The live value is
// opt_ip (the card is still In Progress on the board), so the live guard sees
// live(opt_ip) != target("") and issues the clear.
func TestForwardMoveOpenClears(t *testing.T) {
	repoID := uuid.New()
	syncer := forwardSyncer()
	syncer.liveStatus = map[string]string{"item7": "opt_ip"}
	st := &fakeProjectStore{
		repo: githubRepoRow(repoID),
		link: forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		item: store.GithubProjectItem{RepoID: repoID, ForgeIssueIid: 7, ItemNodeID: "item7",
			LastStatusOptionID: optionMarker("opt_ip")},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)

	if err := svc.ForwardMove(context.Background(), repoID, 7, ""); err != nil {
		t.Fatalf("ForwardMove: %v", err)
	}
	if len(syncer.setCalls) != 1 || syncer.setCalls[0].optionID != "" {
		t.Fatalf("want one clear set (optionID \"\"), got %v", syncer.setCalls)
	}
	if len(st.markerSets) != 1 || st.markerSets[0].LastStatusOptionID.Valid {
		t.Errorf("want NULL marker on clear, got %+v", st.markerSets)
	}
}

// (c) A LIVE value already equal to the target skips the Status write entirely (D7).
// The guard is live-based, not marker-based: here the live value AND the marker both
// equal the target, so the board is genuinely already there — zero set calls, and
// (since the marker is already in step) zero marker writes.
func TestForwardMoveLiveValueNoop(t *testing.T) {
	repoID := uuid.New()
	syncer := forwardSyncer()
	syncer.liveStatus = map[string]string{"item7": "opt_ip"} // live == target
	st := &fakeProjectStore{
		repo: githubRepoRow(repoID),
		link: forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		item: store.GithubProjectItem{RepoID: repoID, ForgeIssueIid: 7, ItemNodeID: "item7",
			LastStatusOptionID: optionMarker("opt_ip")}, // marker == target
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)

	if err := svc.ForwardMove(context.Background(), repoID, 7, "In Progress"); err != nil {
		t.Fatalf("ForwardMove: %v", err)
	}
	if syncer.readItemCalls != 1 {
		t.Errorf("live no-op must consult the live value once, got %d reads", syncer.readItemCalls)
	}
	if len(syncer.setCalls) != 0 {
		t.Errorf("live == target must not set status, got %v", syncer.setCalls)
	}
	if len(st.markerSets) != 0 {
		t.Errorf("no-op with an in-step marker must not rewrite the marker, got %v", st.markerSets)
	}
}

// (c”) The live no-op with a STALE marker: the board is already at the target but the
// stored marker disagrees (e.g. a prior reverse write it never observed). The Status
// write is still skipped, but the marker is re-synced to the live target so the next
// reverse poll does not misread the stale marker as forge-side drift.
func TestForwardMoveLiveNoopResyncsStaleMarker(t *testing.T) {
	repoID := uuid.New()
	syncer := forwardSyncer()
	syncer.liveStatus = map[string]string{"item7": "opt_ip"} // live already at target
	st := &fakeProjectStore{
		repo: githubRepoRow(repoID),
		link: forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		item: store.GithubProjectItem{RepoID: repoID, ForgeIssueIid: 7, ItemNodeID: "item7",
			LastStatusOptionID: optionMarker("opt_stale")}, // marker disagrees with live
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)

	if err := svc.ForwardMove(context.Background(), repoID, 7, "In Progress"); err != nil {
		t.Fatalf("ForwardMove: %v", err)
	}
	if len(syncer.setCalls) != 0 {
		t.Errorf("board already at target must not set status, got %v", syncer.setCalls)
	}
	if len(st.markerSets) != 1 || !st.markerSets[0].LastStatusOptionID.Valid || st.markerSets[0].LastStatusOptionID.String != "opt_ip" {
		t.Errorf("a stale marker must be re-synced to the live target, got %+v", st.markerSets)
	}
}

// (c') The Open no-op: NULL marker + Open target skips the write.
func TestForwardMoveOpenMarkerNoop(t *testing.T) {
	repoID := uuid.New()
	syncer := forwardSyncer()
	st := &fakeProjectStore{
		repo: githubRepoRow(repoID),
		link: forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		item: store.GithubProjectItem{RepoID: repoID, ForgeIssueIid: 7, ItemNodeID: "item7"}, // NULL marker
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)

	if err := svc.ForwardMove(context.Background(), repoID, 7, ""); err != nil {
		t.Fatalf("ForwardMove: %v", err)
	}
	if len(syncer.setCalls) != 0 {
		t.Errorf("NULL marker + Open target must not set status, got %v", syncer.setCalls)
	}
}

// (d) Skip paths make zero forge calls: instance-disabled, no link row, non-github,
// unmapped column.
func TestForwardMoveSkipPaths(t *testing.T) {
	repoID := uuid.New()
	tracked := store.GithubProjectItem{RepoID: repoID, ForgeIssueIid: 7, ItemNodeID: "item7"}

	t.Run("instance disabled", func(t *testing.T) {
		syncer := forwardSyncer()
		st := &fakeProjectStore{repo: githubRepoRow(repoID), link: forwardLink(t, map[string]string{"In Progress": "opt_ip"}), item: tracked}
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: false}, nil)
		if err := svc.ForwardMove(context.Background(), repoID, 7, "In Progress"); err != nil {
			t.Fatalf("ForwardMove: %v", err)
		}
		if len(syncer.setCalls) != 0 {
			t.Errorf("disabled instance must make no forge calls, got %v", syncer.setCalls)
		}
	})

	t.Run("no link row", func(t *testing.T) {
		syncer := forwardSyncer()
		st := &fakeProjectStore{repo: githubRepoRow(repoID), linkErr: pgx.ErrNoRows}
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		if err := svc.ForwardMove(context.Background(), repoID, 7, "In Progress"); err != nil {
			t.Fatalf("ForwardMove: %v", err)
		}
		if len(syncer.setCalls) != 0 {
			t.Errorf("no link row must make no forge calls, got %v", syncer.setCalls)
		}
	})

	t.Run("non-github repo", func(t *testing.T) {
		row := githubRepoRow(repoID)
		row.ForgeType = string(forge.TypeGitLab)
		syncer := forwardSyncer()
		st := &fakeProjectStore{repo: row, link: forwardLink(t, map[string]string{"In Progress": "opt_ip"}), item: tracked}
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		if err := svc.ForwardMove(context.Background(), repoID, 7, "In Progress"); err != nil {
			t.Fatalf("ForwardMove: %v", err)
		}
		if len(syncer.setCalls) != 0 {
			t.Errorf("non-github repo must make no forge calls, got %v", syncer.setCalls)
		}
	})

	t.Run("unmapped column", func(t *testing.T) {
		syncer := forwardSyncer()
		st := &fakeProjectStore{repo: githubRepoRow(repoID), link: forwardLink(t, map[string]string{"In Progress": "opt_ip"}), item: tracked}
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		if err := svc.ForwardMove(context.Background(), repoID, 7, "Later"); err != nil {
			t.Fatalf("ForwardMove: %v", err)
		}
		if len(syncer.setCalls) != 0 {
			t.Errorf("unmapped column must make no forge calls, got %v", syncer.setCalls)
		}
		if len(st.markerSets) != 0 {
			t.Errorf("unmapped column must not touch the marker, got %v", st.markerSets)
		}
	})
}

// (e) A missing item is resolved + added, then set to the target and persisted.
func TestForwardMoveAddsMissingItem(t *testing.T) {
	repoID := uuid.New()
	syncer := forwardSyncer()
	st := &fakeProjectStore{
		repo:    githubRepoRow(repoID),
		link:    forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		itemErr: pgx.ErrNoRows,
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)

	if err := svc.ForwardMove(context.Background(), repoID, 7, "In Progress"); err != nil {
		t.Fatalf("ForwardMove: %v", err)
	}
	if len(syncer.addCalls) != 1 || syncer.addCalls[0] != "content7" {
		t.Fatalf("want issue 7 content added once, got %v", syncer.addCalls)
	}
	if len(syncer.setCalls) != 1 || syncer.setCalls[0].itemID != "item-content7" || syncer.setCalls[0].optionID != "opt_ip" {
		t.Fatalf("want set item-content7->opt_ip, got %v", syncer.setCalls)
	}
	persisted, ok := st.items[7]
	if !ok {
		t.Fatalf("want new item persisted for issue 7")
	}
	if persisted.ItemNodeID != "item-content7" || !persisted.LastStatusOptionID.Valid || persisted.LastStatusOptionID.String != "opt_ip" {
		t.Errorf("persisted item wrong: %+v", persisted)
	}
}

// (f) A forge error is swallowed (ForwardMove returns nil) and stamps last_error.
func TestForwardMoveForgeErrorSwallowed(t *testing.T) {
	repoID := uuid.New()
	syncer := forwardSyncer()
	syncer.setErr = errors.New("graphql boom")
	st := &fakeProjectStore{
		repo: githubRepoRow(repoID),
		link: forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		item: store.GithubProjectItem{RepoID: repoID, ForgeIssueIid: 7, ItemNodeID: "item7"},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)

	if err := svc.ForwardMove(context.Background(), repoID, 7, "In Progress"); err != nil {
		t.Fatalf("ForwardMove must swallow forge errors, got %v", err)
	}
	if len(st.linkErrs) != 1 {
		t.Fatalf("want last_error stamped once, got %v", st.linkErrs)
	}
	if len(st.markerSets) != 0 {
		t.Errorf("a failed write must not advance the marker, got %v", st.markerSets)
	}
}
