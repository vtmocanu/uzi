package forgesvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// --- reverse-sync (M6) fakes ------------------------------------------------

// fakeMover records AutoMove calls the reverse sync issues, and can be scripted to
// fail. *forgesvc.Service is the production implementation; this narrow fake lets
// the diff logic be asserted with no live forge/DB.
type fakeMover struct {
	calls []moveCall
	err   error
}

type moveCall struct {
	issueIID int64
	target   string
}

func (m *fakeMover) AutoMove(_ context.Context, _ forge.Forge, _ int64, issue store.Issue, _ []store.BoardColumn, target string) (store.Issue, error) {
	m.calls = append(m.calls, moveCall{issueIID: issue.ForgeIssueIid, target: target})
	return issue, m.err
}

// reverseSyncer builds a ProjectBoardSyncer fake whose ReadProjectV2ItemStatuses
// returns the given live item statuses.
func reverseSyncer(live ...forge.ProjectV2ItemStatus) *fakeProjectSyncer {
	return &fakeProjectSyncer{fakeForge: &fakeForge{}, live: live}
}

// projectItem builds a stored item row with the given marker (a real option id, or
// "" for a NULL/No-Status marker).
func projectItem(repoID uuid.UUID, iid int64, node, marker string) store.GithubProjectItem {
	return store.GithubProjectItem{
		RepoID:             repoID,
		ForgeIssueIid:      iid,
		ItemNodeID:         node,
		LastStatusOptionID: optionMarker(marker),
	}
}

// --- reverse-sync (M6) tests ------------------------------------------------

// (diff) An item whose live OptionID differs from its stored marker is a GitHub-side
// change: one AutoMove to the mapped column, and the marker advances to the live value.
func TestReverseSyncDiffWritesLabel(t *testing.T) {
	repoID := uuid.New()
	syncer := reverseSyncer(forge.ProjectV2ItemStatus{ItemID: "item7", IssueNumber: 7, OptionID: "opt_ip"})
	mover := &fakeMover{}
	st := &fakeProjectStore{
		repo:          githubRepoRow(repoID),
		link:          forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		issues:        []store.Issue{{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t)}},
		existingItems: []store.GithubProjectItem{projectItem(repoID, 7, "item7", "opt_old")},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	if err := svc.ReverseSync(context.Background(), repoID); err != nil {
		t.Fatalf("ReverseSync: %v", err)
	}
	if len(mover.calls) != 1 || mover.calls[0].issueIID != 7 || mover.calls[0].target != "In Progress" {
		t.Fatalf("want one AutoMove issue7->\"In Progress\", got %v", mover.calls)
	}
	// Item row present: marker advanced via SetGithubProjectItemStatusMarker (not upsert).
	if len(st.markerSets) != 1 || !st.markerSets[0].LastStatusOptionID.Valid || st.markerSets[0].LastStatusOptionID.String != "opt_ip" {
		t.Errorf("want marker advanced to opt_ip, got %+v", st.markerSets)
	}
	if len(st.items) != 0 {
		t.Errorf("existing item must not be upserted, got %v", st.items)
	}
}

// (diff, absent item) A live change for an issue with no stored item row upserts the
// item, carrying the live ItemID as the node id and the live OptionID as the marker.
func TestReverseSyncAbsentItemUpserts(t *testing.T) {
	repoID := uuid.New()
	syncer := reverseSyncer(forge.ProjectV2ItemStatus{ItemID: "item7", IssueNumber: 7, OptionID: "opt_ip"})
	mover := &fakeMover{}
	st := &fakeProjectStore{
		repo:   githubRepoRow(repoID),
		link:   forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		issues: []store.Issue{{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t)}},
		// no existingItems -> marker "" -> live differs.
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	if err := svc.ReverseSync(context.Background(), repoID); err != nil {
		t.Fatalf("ReverseSync: %v", err)
	}
	if len(mover.calls) != 1 || mover.calls[0].target != "In Progress" {
		t.Fatalf("want one AutoMove to \"In Progress\", got %v", mover.calls)
	}
	upserted, ok := st.items[7]
	if !ok {
		t.Fatalf("want item upserted for issue 7")
	}
	if upserted.ItemNodeID != "item7" || !upserted.LastStatusOptionID.Valid || upserted.LastStatusOptionID.String != "opt_ip" {
		t.Errorf("upserted item wrong: %+v", upserted)
	}
	if len(st.markerSets) != 0 {
		t.Errorf("absent item must upsert, not SetMarker, got %v", st.markerSets)
	}
}

// (convergence) An item whose live OptionID equals its stored marker is the
// uzi-forward case: reverse must make ZERO AutoMove calls (this is what stops the
// oscillation, SC-2).
func TestReverseSyncConvergenceNoop(t *testing.T) {
	repoID := uuid.New()
	syncer := reverseSyncer(forge.ProjectV2ItemStatus{ItemID: "item7", IssueNumber: 7, OptionID: "opt_ip"})
	mover := &fakeMover{}
	st := &fakeProjectStore{
		repo:          githubRepoRow(repoID),
		link:          forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		issues:        []store.Issue{{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t)}},
		existingItems: []store.GithubProjectItem{projectItem(repoID, 7, "item7", "opt_ip")},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	if err := svc.ReverseSync(context.Background(), repoID); err != nil {
		t.Fatalf("ReverseSync: %v", err)
	}
	if len(mover.calls) != 0 {
		t.Errorf("live == marker must make no AutoMove calls, got %v", mover.calls)
	}
	if len(st.markerSets) != 0 {
		t.Errorf("no-op must not touch the marker, got %v", st.markerSets)
	}
}

// (convergence, Open) A NULL marker (No Status) and a live "" is a no-op too.
func TestReverseSyncConvergenceOpenNoop(t *testing.T) {
	repoID := uuid.New()
	syncer := reverseSyncer(forge.ProjectV2ItemStatus{ItemID: "item7", IssueNumber: 7, OptionID: ""})
	mover := &fakeMover{}
	st := &fakeProjectStore{
		repo:          githubRepoRow(repoID),
		link:          forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		issues:        []store.Issue{{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t)}},
		existingItems: []store.GithubProjectItem{projectItem(repoID, 7, "item7", "")}, // NULL marker
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	if err := svc.ReverseSync(context.Background(), repoID); err != nil {
		t.Fatalf("ReverseSync: %v", err)
	}
	if len(mover.calls) != 0 {
		t.Errorf("live \"\" == NULL marker must make no AutoMove calls, got %v", mover.calls)
	}
}

// (idempotent) A second ReverseSync after the marker was advanced makes ZERO calls —
// no oscillation. Simulated by advancing the stored marker to the live value between
// passes (what SetGithubProjectItemStatusMarker persists in production).
func TestReverseSyncIdempotentSecondPass(t *testing.T) {
	repoID := uuid.New()
	syncer := reverseSyncer(forge.ProjectV2ItemStatus{ItemID: "item7", IssueNumber: 7, OptionID: "opt_ip"})
	mover := &fakeMover{}
	st := &fakeProjectStore{
		repo:          githubRepoRow(repoID),
		link:          forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		issues:        []store.Issue{{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t)}},
		existingItems: []store.GithubProjectItem{projectItem(repoID, 7, "item7", "opt_old")},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	if err := svc.ReverseSync(context.Background(), repoID); err != nil {
		t.Fatalf("ReverseSync pass 1: %v", err)
	}
	if len(mover.calls) != 1 {
		t.Fatalf("pass 1 want one AutoMove, got %v", mover.calls)
	}
	// Persist the advance (production's SetGithubProjectItemStatusMarker) and re-run.
	st.existingItems = []store.GithubProjectItem{projectItem(repoID, 7, "item7", "opt_ip")}
	mover.calls = nil
	if err := svc.ReverseSync(context.Background(), repoID); err != nil {
		t.Fatalf("ReverseSync pass 2: %v", err)
	}
	if len(mover.calls) != 0 {
		t.Errorf("pass 2 (marker advanced) must make no AutoMove calls, got %v", mover.calls)
	}
}

// (Open) A live "" while the marker is a real option is a GitHub-side clear: AutoMove
// to target "" (uzi's implicit Open) and the marker is cleared to NULL.
func TestReverseSyncOpenClears(t *testing.T) {
	repoID := uuid.New()
	syncer := reverseSyncer(forge.ProjectV2ItemStatus{ItemID: "item7", IssueNumber: 7, OptionID: ""})
	mover := &fakeMover{}
	st := &fakeProjectStore{
		repo:          githubRepoRow(repoID),
		link:          forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		issues:        []store.Issue{{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t, "In Progress")}},
		existingItems: []store.GithubProjectItem{projectItem(repoID, 7, "item7", "opt_ip")},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	if err := svc.ReverseSync(context.Background(), repoID); err != nil {
		t.Fatalf("ReverseSync: %v", err)
	}
	if len(mover.calls) != 1 || mover.calls[0].target != "" {
		t.Fatalf("want one AutoMove to Open (target \"\"), got %v", mover.calls)
	}
	if len(st.markerSets) != 1 || st.markerSets[0].LastStatusOptionID.Valid {
		t.Errorf("want marker cleared to NULL, got %+v", st.markerSets)
	}
}

// (unknown option) A live option id not in the reverse map is one uzi does not
// manage (D5): skipped, no AutoMove, and the marker is left as-is (visible).
func TestReverseSyncUnknownOptionSkipped(t *testing.T) {
	repoID := uuid.New()
	syncer := reverseSyncer(forge.ProjectV2ItemStatus{ItemID: "item7", IssueNumber: 7, OptionID: "opt_unknown"})
	mover := &fakeMover{}
	st := &fakeProjectStore{
		repo:          githubRepoRow(repoID),
		link:          forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		issues:        []store.Issue{{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t)}},
		existingItems: []store.GithubProjectItem{projectItem(repoID, 7, "item7", "")},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	if err := svc.ReverseSync(context.Background(), repoID); err != nil {
		t.Fatalf("ReverseSync: %v", err)
	}
	if len(mover.calls) != 0 {
		t.Errorf("unknown option must make no AutoMove calls, got %v", mover.calls)
	}
	if len(st.markerSets) != 0 || len(st.items) != 0 {
		t.Errorf("unknown option must not touch the marker, got sets=%v items=%v", st.markerSets, st.items)
	}
}

// (skip) A closed issue, an issue absent from the cache, and a non-issue item
// (IssueNumber 0) are all skipped with no AutoMove.
func TestReverseSyncSkipItems(t *testing.T) {
	repoID := uuid.New()

	t.Run("closed issue", func(t *testing.T) {
		syncer := reverseSyncer(forge.ProjectV2ItemStatus{ItemID: "item7", IssueNumber: 7, OptionID: "opt_ip"})
		mover := &fakeMover{}
		st := &fakeProjectStore{
			repo:   githubRepoRow(repoID),
			link:   forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
			issues: []store.Issue{{ForgeIssueIid: 7, State: "closed", Labels: labelsJSON(t)}},
		}
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		svc.SetMover(mover)
		if err := svc.ReverseSync(context.Background(), repoID); err != nil {
			t.Fatalf("ReverseSync: %v", err)
		}
		if len(mover.calls) != 0 {
			t.Errorf("closed issue must be skipped, got %v", mover.calls)
		}
	})

	t.Run("issue absent from cache", func(t *testing.T) {
		syncer := reverseSyncer(forge.ProjectV2ItemStatus{ItemID: "item9", IssueNumber: 9, OptionID: "opt_ip"})
		mover := &fakeMover{}
		st := &fakeProjectStore{
			repo:   githubRepoRow(repoID),
			link:   forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
			issues: []store.Issue{{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t)}}, // no 9
		}
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		svc.SetMover(mover)
		if err := svc.ReverseSync(context.Background(), repoID); err != nil {
			t.Fatalf("ReverseSync: %v", err)
		}
		if len(mover.calls) != 0 {
			t.Errorf("issue absent from cache must be skipped, got %v", mover.calls)
		}
	})

	t.Run("non-issue item", func(t *testing.T) {
		syncer := reverseSyncer(forge.ProjectV2ItemStatus{ItemID: "itemPR", IssueNumber: 0, OptionID: "opt_ip"})
		mover := &fakeMover{}
		st := &fakeProjectStore{
			repo:   githubRepoRow(repoID),
			link:   forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
			issues: []store.Issue{{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t)}},
		}
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		svc.SetMover(mover)
		if err := svc.ReverseSync(context.Background(), repoID); err != nil {
			t.Fatalf("ReverseSync: %v", err)
		}
		if len(mover.calls) != 0 {
			t.Errorf("non-issue item (IssueNumber 0) must be skipped, got %v", mover.calls)
		}
	})
}

// (skip paths) Instance-disabled, no link row, non-github, and nil mover each make
// zero forge reads and zero mover calls.
func TestReverseSyncSkipPaths(t *testing.T) {
	repoID := uuid.New()
	live := forge.ProjectV2ItemStatus{ItemID: "item7", IssueNumber: 7, OptionID: "opt_ip"}

	t.Run("instance disabled", func(t *testing.T) {
		syncer := reverseSyncer(live)
		mover := &fakeMover{}
		st := &fakeProjectStore{repo: githubRepoRow(repoID), link: forwardLink(t, map[string]string{"In Progress": "opt_ip"})}
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: false}, nil)
		svc.SetMover(mover)
		if err := svc.ReverseSync(context.Background(), repoID); err != nil {
			t.Fatalf("ReverseSync: %v", err)
		}
		if syncer.readCalls != 0 || len(mover.calls) != 0 {
			t.Errorf("disabled must make no forge/mover calls, reads=%d moves=%v", syncer.readCalls, mover.calls)
		}
	})

	t.Run("no link row", func(t *testing.T) {
		syncer := reverseSyncer(live)
		mover := &fakeMover{}
		st := &fakeProjectStore{repo: githubRepoRow(repoID), linkErr: pgx.ErrNoRows}
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		svc.SetMover(mover)
		if err := svc.ReverseSync(context.Background(), repoID); err != nil {
			t.Fatalf("ReverseSync: %v", err)
		}
		if syncer.readCalls != 0 || len(mover.calls) != 0 {
			t.Errorf("no link row must make no forge/mover calls, reads=%d moves=%v", syncer.readCalls, mover.calls)
		}
	})

	t.Run("non-github repo", func(t *testing.T) {
		row := githubRepoRow(repoID)
		row.ForgeType = string(forge.TypeGitLab)
		syncer := reverseSyncer(live)
		mover := &fakeMover{}
		st := &fakeProjectStore{repo: row, link: forwardLink(t, map[string]string{"In Progress": "opt_ip"})}
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		svc.SetMover(mover)
		if err := svc.ReverseSync(context.Background(), repoID); err != nil {
			t.Fatalf("ReverseSync: %v", err)
		}
		if syncer.readCalls != 0 || len(mover.calls) != 0 {
			t.Errorf("non-github must make no forge/mover calls, reads=%d moves=%v", syncer.readCalls, mover.calls)
		}
	})

	t.Run("nil mover", func(t *testing.T) {
		syncer := reverseSyncer(live)
		st := &fakeProjectStore{repo: githubRepoRow(repoID), link: forwardLink(t, map[string]string{"In Progress": "opt_ip"})}
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		// No SetMover: reverse is disabled.
		if err := svc.ReverseSync(context.Background(), repoID); err != nil {
			t.Fatalf("ReverseSync: %v", err)
		}
		if syncer.readCalls != 0 {
			t.Errorf("nil mover must make no forge read, reads=%d", syncer.readCalls)
		}
	})
}

// --- reconcile / backfill / observability (M7) ------------------------------

// backfillSyncer builds a ProjectBoardSyncer fake that can resolve a slug + issue
// nodes (for backfill) and returns the given live statuses. AddProjectV2Item returns
// "item-<contentID>".
func backfillSyncer(issueNode map[int]string, live ...forge.ProjectV2ItemStatus) *fakeProjectSyncer {
	return &fakeProjectSyncer{
		fakeForge: &fakeForge{},
		slugOwner: "acme", slugRepo: "widgets",
		issueNode: issueNode,
		live:      live,
	}
}

// (backfill) An OPEN issue with no item row is added to the project and its Status
// seeded from its current column: a mapped column → its option (a Status write); an
// Open/unmapped column → cleared (no Status write). The same-tick diff no-ops both
// (live == seeded marker), so ZERO AutoMove, and last_synced_at is touched once.
func TestReverseSyncBackfillsNewIssues(t *testing.T) {
	repoID := uuid.New()
	syncer := backfillSyncer(
		map[int]string{7: "content7", 8: "content8"},
		forge.ProjectV2ItemStatus{ItemID: "item-content7", IssueNumber: 7, OptionID: "opt_ip"},
		forge.ProjectV2ItemStatus{ItemID: "item-content8", IssueNumber: 8, OptionID: ""},
	)
	mover := &fakeMover{}
	st := &fakeProjectStore{
		repo:    githubRepoRow(repoID),
		link:    forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		columns: []store.BoardColumn{{LabelName: "In Progress", Position: 1}},
		issues: []store.Issue{
			{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t, "In Progress")}, // mapped → opt_ip
			{ForgeIssueIid: 8, State: "opened", Labels: labelsJSON(t)},                // Open → cleared
		},
		// no existingItems -> both untracked -> backfilled.
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	if err := svc.ReverseSync(context.Background(), repoID); err != nil {
		t.Fatalf("ReverseSync: %v", err)
	}

	// Both issues added.
	added := map[string]bool{}
	for _, c := range syncer.addCalls {
		added[c] = true
	}
	if !added["content7"] || !added["content8"] || len(syncer.addCalls) != 2 {
		t.Errorf("want content7+content8 added once each, got %v", syncer.addCalls)
	}
	// Only the mapped issue drives a Status write; the Open issue is left at "No Status".
	if len(syncer.setCalls) != 1 || syncer.setCalls[0].itemID != "item-content7" || syncer.setCalls[0].optionID != "opt_ip" {
		t.Errorf("want one seed set item-content7->opt_ip, got %v", syncer.setCalls)
	}
	// Persisted markers: issue 7 = opt_ip (valid), issue 8 = NULL (cleared).
	if m := st.items[7].LastStatusOptionID; !m.Valid || m.String != "opt_ip" {
		t.Errorf("issue 7 marker = %+v, want opt_ip", m)
	}
	if st.items[7].ItemNodeID != "item-content7" {
		t.Errorf("issue 7 node = %q, want item-content7", st.items[7].ItemNodeID)
	}
	if m := st.items[8].LastStatusOptionID; m.Valid {
		t.Errorf("issue 8 marker should be NULL (Open), got %+v", m)
	}
	// Convergence: the freshly-backfilled items do NOT oscillate — no AutoMove.
	if len(mover.calls) != 0 {
		t.Errorf("backfilled items must not oscillate (zero AutoMove), got %v", mover.calls)
	}
	// Observability: a completed pass touches last_synced_at once.
	if st.touchCalls != 1 {
		t.Errorf("want last_synced_at touched once, got %d", st.touchCalls)
	}
}

// (close-prune, D1) A CLOSED issue that has an item row has that row DELETED locally,
// with NO GitHub mutation (no add, no status set) and no AutoMove; the stale card is
// left on the board. last_synced_at is still touched.
func TestReverseSyncPrunesClosedIssue(t *testing.T) {
	repoID := uuid.New()
	// The closed issue's card may still be live on the board; the diff must skip it.
	syncer := backfillSyncer(
		nil,
		forge.ProjectV2ItemStatus{ItemID: "item3", IssueNumber: 3, OptionID: "opt_ip"},
	)
	mover := &fakeMover{}
	st := &fakeProjectStore{
		repo:          githubRepoRow(repoID),
		link:          forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		columns:       []store.BoardColumn{{LabelName: "In Progress", Position: 1}},
		issues:        []store.Issue{{ForgeIssueIid: 3, State: "closed", Labels: labelsJSON(t, "In Progress")}},
		existingItems: []store.GithubProjectItem{projectItem(repoID, 3, "item3", "opt_ip")},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	if err := svc.ReverseSync(context.Background(), repoID); err != nil {
		t.Fatalf("ReverseSync: %v", err)
	}
	if len(st.deletedItems) != 1 || st.deletedItems[0] != 3 {
		t.Errorf("want issue 3 item row deleted, got %v", st.deletedItems)
	}
	// No destructive/mutating GitHub call for a close-prune.
	if len(syncer.addCalls) != 0 || len(syncer.setCalls) != 0 {
		t.Errorf("close-prune must make no GitHub add/set, got add=%v set=%v", syncer.addCalls, syncer.setCalls)
	}
	if len(mover.calls) != 0 {
		t.Errorf("closed issue must drive no AutoMove, got %v", mover.calls)
	}
	if st.touchCalls != 1 {
		t.Errorf("want last_synced_at touched once, got %d", st.touchCalls)
	}
}

// (no oscillation) A backfilled item, once its seeded marker is persisted, makes ZERO
// AutoMove and ZERO new adds on the NEXT tick — the item is tracked, so reconcile does
// not re-add it and the diff no-ops it.
func TestReverseSyncBackfillNoOscillationSecondPass(t *testing.T) {
	repoID := uuid.New()
	syncer := backfillSyncer(
		map[int]string{7: "content7"},
		forge.ProjectV2ItemStatus{ItemID: "item-content7", IssueNumber: 7, OptionID: "opt_ip"},
	)
	mover := &fakeMover{}
	st := &fakeProjectStore{
		repo:    githubRepoRow(repoID),
		link:    forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		columns: []store.BoardColumn{{LabelName: "In Progress", Position: 1}},
		issues:  []store.Issue{{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t, "In Progress")}},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	if err := svc.ReverseSync(context.Background(), repoID); err != nil {
		t.Fatalf("ReverseSync pass 1: %v", err)
	}
	if len(mover.calls) != 0 || len(syncer.addCalls) != 1 {
		t.Fatalf("pass 1 want one add + zero AutoMove, got add=%v moves=%v", syncer.addCalls, mover.calls)
	}

	// Persist the backfilled row (production's UpsertGithubProjectItem) and re-run.
	st.existingItems = []store.GithubProjectItem{projectItem(repoID, 7, "item-content7", "opt_ip")}
	mover.calls = nil
	syncer.addCalls = nil
	syncer.setCalls = nil
	if err := svc.ReverseSync(context.Background(), repoID); err != nil {
		t.Fatalf("ReverseSync pass 2: %v", err)
	}
	if len(mover.calls) != 0 {
		t.Errorf("pass 2 must make no AutoMove (no oscillation), got %v", mover.calls)
	}
	if len(syncer.addCalls) != 0 || len(syncer.setCalls) != 0 {
		t.Errorf("pass 2 must not re-add/re-set a tracked item, got add=%v set=%v", syncer.addCalls, syncer.setCalls)
	}
}

// (observability) A live-read failure aborts the pass BEFORE the touch: last_synced_at
// is NOT bumped and last_error is stamped.
func TestReverseSyncReadErrorDoesNotTouch(t *testing.T) {
	repoID := uuid.New()
	syncer := backfillSyncer(nil)
	syncer.readErr = errors.New("read boom")
	mover := &fakeMover{}
	st := &fakeProjectStore{
		repo: githubRepoRow(repoID),
		link: forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		// No issues → reconcile is a no-op, so the ONLY error is the live read.
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	if err := svc.ReverseSync(context.Background(), repoID); err != nil {
		t.Fatalf("ReverseSync must swallow read errors, got %v", err)
	}
	if st.touchCalls != 0 {
		t.Errorf("a failed read must not touch last_synced_at, got %d", st.touchCalls)
	}
	if len(st.linkErrs) != 1 {
		t.Errorf("want last_error stamped once, got %v", st.linkErrs)
	}
}

// (error) An AutoMove failure does NOT advance the marker (so it retries next tick)
// and stamps last_error.
func TestReverseSyncAutoMoveErrorRetries(t *testing.T) {
	repoID := uuid.New()
	syncer := reverseSyncer(forge.ProjectV2ItemStatus{ItemID: "item7", IssueNumber: 7, OptionID: "opt_ip"})
	mover := &fakeMover{err: errors.New("forge boom")}
	st := &fakeProjectStore{
		repo:          githubRepoRow(repoID),
		link:          forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		issues:        []store.Issue{{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t)}},
		existingItems: []store.GithubProjectItem{projectItem(repoID, 7, "item7", "opt_old")},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	if err := svc.ReverseSync(context.Background(), repoID); err != nil {
		t.Fatalf("ReverseSync must swallow mover errors, got %v", err)
	}
	if len(mover.calls) != 1 {
		t.Fatalf("want the AutoMove attempted once, got %v", mover.calls)
	}
	if len(st.markerSets) != 0 || len(st.items) != 0 {
		t.Errorf("a failed move must not advance the marker, got sets=%v items=%v", st.markerSets, st.items)
	}
	if len(st.linkErrs) != 1 {
		t.Errorf("want last_error stamped once, got %v", st.linkErrs)
	}
}
