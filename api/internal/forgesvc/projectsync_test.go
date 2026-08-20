package forgesvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

	live    []forge.ProjectV2ItemStatus
	readErr error

	issueNode map[int]string // issue number -> content node id
	addReturn map[string]string
	addCalls  []string // contentIDs added

	setCalls []setStatusCall
}

type setStatusCall struct {
	itemID   string
	optionID string
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
	return nil
}

func (f *fakeProjectSyncer) ReadProjectV2ItemStatuses(context.Context, string, string) ([]forge.ProjectV2ItemStatus, error) {
	return f.live, f.readErr
}

func (f *fakeProjectSyncer) ResolveProjectV2OwnerID(context.Context, string, forge.ProjectV2OwnerKind) (string, error) {
	return "owner-id", nil
}

func (f *fakeProjectSyncer) ResolveRepositoryNodeID(context.Context, string, string) (string, error) {
	return f.repoNodeID, f.repoNodeErr
}

func (f *fakeProjectSyncer) CreateProjectV2(context.Context, string, string, string) (forge.ProjectV2Ref, error) {
	return forge.ProjectV2Ref{}, nil
}

func (f *fakeProjectSyncer) CreateProjectV2Field(context.Context, string, string, []forge.ProjectV2NewOption) (forge.ProjectV2StatusField, error) {
	return forge.ProjectV2StatusField{}, nil
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
