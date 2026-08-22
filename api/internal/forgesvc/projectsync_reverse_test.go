package forgesvc

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

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

// (close-prune is slug-independent) When the repo slug cannot be resolved (backfill
// is impossible), a closed issue's stale item row is STILL pruned — close-prune runs
// as a first pass before backfill needs the slug, so a degraded forge does not strand
// closed-issue rows.
func TestReverseSyncPrunesClosedEvenWhenSlugFails(t *testing.T) {
	repoID := uuid.New()
	syncer := backfillSyncer(
		map[int]string{9: "content9"},
		forge.ProjectV2ItemStatus{ItemID: "item3", IssueNumber: 3, OptionID: "opt_ip"},
	)
	syncer.slugErr = errors.New("slug boom") // backfill of the open issue cannot proceed
	mover := &fakeMover{}
	st := &fakeProjectStore{
		repo:    githubRepoRow(repoID),
		link:    forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
		columns: []store.BoardColumn{{LabelName: "In Progress", Position: 1}},
		issues: []store.Issue{
			{ForgeIssueIid: 3, State: "closed", Labels: labelsJSON(t, "In Progress")}, // tracked → prune
			{ForgeIssueIid: 9, State: "opened", Labels: labelsJSON(t, "In Progress")}, // untracked → backfill (blocked by slug)
		},
		existingItems: []store.GithubProjectItem{projectItem(repoID, 3, "item3", "opt_ip")},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	if err := svc.ReverseSync(context.Background(), repoID); err != nil {
		t.Fatalf("ReverseSync: %v", err)
	}
	// The closed issue is pruned despite the slug failure blocking backfill.
	if len(st.deletedItems) != 1 || st.deletedItems[0] != 3 {
		t.Errorf("want issue 3 pruned even though slug failed, got %v", st.deletedItems)
	}
	// Backfill could not add the open issue (no slug), so no GitHub add; slug error stamped.
	if len(syncer.addCalls) != 0 {
		t.Errorf("backfill must not add when slug fails, got %v", syncer.addCalls)
	}
	if len(st.linkErrs) == 0 {
		t.Errorf("a slug failure must stamp last_error")
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

// TestReverseSyncSuppressedWhileSeeding (PRD #576 M4 SC (d)): while the per-repo seeding
// lease is active, ReverseSync makes ZERO AutoMove calls (a reverse tick must not race a
// partially-seeded board). The zero-call assertion is NEGATIVE, so the second subtest is
// its built-in mutation control: a lease older than seedSuppressLease does NOT suppress,
// and the same fixture then drives exactly one AutoMove — proving the suppression, not a
// vacuous no-op, is what silenced the first case.
func TestReverseSyncSuppressedWhileSeeding(t *testing.T) {
	// A live status that differs from the stored marker — this WOULD drive one AutoMove
	// if the tick were not suppressed.
	newStore := func(repoID uuid.UUID, seedingAt pgtype.Timestamptz) *fakeProjectStore {
		link := forwardLink(t, map[string]string{"In Progress": "opt_ip"})
		link.SeedingStartedAt = seedingAt
		return &fakeProjectStore{
			repo:          githubRepoRow(repoID),
			link:          link,
			issues:        []store.Issue{{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t)}},
			existingItems: []store.GithubProjectItem{projectItem(repoID, 7, "item7", "opt_old")},
		}
	}

	t.Run("suppressed within the lease", func(t *testing.T) {
		repoID := uuid.New()
		syncer := reverseSyncer(forge.ProjectV2ItemStatus{ItemID: "item7", IssueNumber: 7, OptionID: "opt_ip"})
		mover := &fakeMover{}
		st := newStore(repoID, pgtype.Timestamptz{Time: time.Now(), Valid: true})
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		svc.SetMover(mover)

		if err := svc.ReverseSync(context.Background(), repoID); err != nil {
			t.Fatalf("ReverseSync: %v", err)
		}
		if len(mover.calls) != 0 {
			t.Errorf("seeding in progress must suppress reverse (zero AutoMove), got %v", mover.calls)
		}
		// The tick returned BEFORE reading live statuses or touching last_synced_at.
		if syncer.readCalls != 0 {
			t.Errorf("suppressed tick must not read live statuses, got %d reads", syncer.readCalls)
		}
		if st.touchCalls != 0 {
			t.Errorf("suppressed tick must not touch last_synced_at, got %d", st.touchCalls)
		}
	})

	t.Run("proceeds past the lease (mutation control)", func(t *testing.T) {
		repoID := uuid.New()
		syncer := reverseSyncer(forge.ProjectV2ItemStatus{ItemID: "item7", IssueNumber: 7, OptionID: "opt_ip"})
		mover := &fakeMover{}
		// A lease started 20m ago is past seedSuppressLease (10m): suppression must NOT fire.
		st := newStore(repoID, pgtype.Timestamptz{Time: time.Now().Add(-20 * time.Minute), Valid: true})
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		svc.SetMover(mover)

		if err := svc.ReverseSync(context.Background(), repoID); err != nil {
			t.Fatalf("ReverseSync: %v", err)
		}
		if len(mover.calls) != 1 || mover.calls[0].issueIID != 7 || mover.calls[0].target != "In Progress" {
			t.Fatalf("an expired lease must NOT suppress; want one AutoMove issue7->\"In Progress\", got %v", mover.calls)
		}
	})
}

// --- reverse destructive-write cap (PRD #576 M5) ----------------------------

// oneColumnLink is the standard single-column link used by the cap fixtures: board
// column "In Progress" ↔ option "opt_ip". Its inverse (opt_ip → In Progress) is what
// reverseDiff builds the reverse map from.
func oneColumnLink(t *testing.T) store.GithubProjectLink {
	return forwardLink(t, map[string]string{"In Progress": "opt_ip"})
}

// twoColumnLink adds a second valid column "Done" ↔ "opt_done" so a remap fixture has
// a valid-but-different target to move to.
func twoColumnLink(t *testing.T) store.GithubProjectLink {
	return forwardLink(t, map[string]string{"In Progress": "opt_ip", "Done": "opt_done"})
}

// (a) MASS CLEAR-TO-EMPTY. Five items each transition real→empty while currently in a
// column, so all five are DESTRUCTIVE clears; with five tracked items the count trips
// both cap gates (5 > k=3 AND 500 > 25*5=125). The whole tick aborts: ZERO AutoMove and
// last_error stamped. The zero-call assertion is NEGATIVE, so the second subtest is its
// mutation control: with the cap DISABLED (reverseCapK huge) the SAME fixture fires
// exactly five AutoMove(target="") — proving the zero is the cap, not a dead harness.
func TestReverseSyncCapMassClearToEmpty(t *testing.T) {
	newFixture := func(repoID uuid.UUID) (*fakeProjectSyncer, *fakeMover, *fakeProjectStore) {
		var live []forge.ProjectV2ItemStatus
		var issues []store.Issue
		var items []store.GithubProjectItem
		for i := int64(1); i <= 5; i++ {
			live = append(live, forge.ProjectV2ItemStatus{ItemID: itemNode(i), IssueNumber: i, OptionID: ""}) // live cleared
			issues = append(issues, store.Issue{ForgeIssueIid: i, State: "opened", Labels: labelsJSON(t, "In Progress")})
			items = append(items, projectItem(repoID, i, itemNode(i), "opt_ip")) // marker still the old option
		}
		st := &fakeProjectStore{
			repo:          githubRepoRow(repoID),
			link:          oneColumnLink(t),
			columns:       []store.BoardColumn{{LabelName: "In Progress", Position: 1}},
			issues:        issues,
			existingItems: items,
		}
		return reverseSyncer(live...), &fakeMover{}, st
	}

	t.Run("cap trips, tick aborted", func(t *testing.T) {
		repoID := uuid.New()
		syncer, mover, st := newFixture(repoID)
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		svc.SetMover(mover)

		if err := svc.ReverseSync(context.Background(), repoID); err != nil {
			t.Fatalf("ReverseSync: %v", err)
		}
		if len(mover.calls) != 0 {
			t.Errorf("mass clear must abort the tick (zero AutoMove), got %v", mover.calls)
		}
		if len(st.markerSets) != 0 || len(st.items) != 0 {
			t.Errorf("aborted tick must advance no markers, got sets=%v items=%v", st.markerSets, st.items)
		}
		if len(st.linkErrs) == 0 {
			t.Errorf("aborted tick must stamp last_error")
		}
	})

	t.Run("cap disabled fires 5 clears (mutation control)", func(t *testing.T) {
		repoID := uuid.New()
		syncer, mover, st := newFixture(repoID)
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		svc.SetMover(mover)
		svc.reverseCapK = 1 << 30 // disable the cap: destructiveCount can never exceed this

		if err := svc.ReverseSync(context.Background(), repoID); err != nil {
			t.Fatalf("ReverseSync: %v", err)
		}
		if len(mover.calls) != 5 {
			t.Fatalf("cap disabled must fire 5 AutoMove, got %d: %v", len(mover.calls), mover.calls)
		}
		for _, c := range mover.calls {
			if c.target != "" {
				t.Errorf("each move must clear to Open (target \"\"), got %v", c)
			}
		}
	})
}

// (b) MASS REMAP-TO-WRONG-VALID-OPTION. Five items each remap from "In Progress" to the
// valid-but-different column "Done" (currentColumn "In Progress" != target "Done"), so
// all five are DESTRUCTIVE (a clear-counting guard would miss these — R1b). Same shape
// as (a): ZERO AutoMove + last_error, and the cap-disabled control fires five
// AutoMove(target="Done").
func TestReverseSyncCapMassRemapToWrongColumn(t *testing.T) {
	newFixture := func(repoID uuid.UUID) (*fakeProjectSyncer, *fakeMover, *fakeProjectStore) {
		var live []forge.ProjectV2ItemStatus
		var issues []store.Issue
		var items []store.GithubProjectItem
		for i := int64(1); i <= 5; i++ {
			live = append(live, forge.ProjectV2ItemStatus{ItemID: itemNode(i), IssueNumber: i, OptionID: "opt_done"}) // live remapped to Done
			issues = append(issues, store.Issue{ForgeIssueIid: i, State: "opened", Labels: labelsJSON(t, "In Progress")})
			items = append(items, projectItem(repoID, i, itemNode(i), "opt_ip")) // marker still In Progress's option
		}
		st := &fakeProjectStore{
			repo:          githubRepoRow(repoID),
			link:          twoColumnLink(t),
			columns:       []store.BoardColumn{{LabelName: "In Progress", Position: 1}, {LabelName: "Done", Position: 2}},
			issues:        issues,
			existingItems: items,
		}
		return reverseSyncer(live...), &fakeMover{}, st
	}

	t.Run("cap trips, tick aborted", func(t *testing.T) {
		repoID := uuid.New()
		syncer, mover, st := newFixture(repoID)
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		svc.SetMover(mover)

		if err := svc.ReverseSync(context.Background(), repoID); err != nil {
			t.Fatalf("ReverseSync: %v", err)
		}
		if len(mover.calls) != 0 {
			t.Errorf("mass remap must abort the tick (zero AutoMove), got %v", mover.calls)
		}
		if len(st.markerSets) != 0 || len(st.items) != 0 {
			t.Errorf("aborted tick must advance no markers, got sets=%v items=%v", st.markerSets, st.items)
		}
		if len(st.linkErrs) == 0 {
			t.Errorf("aborted tick must stamp last_error")
		}
	})

	t.Run("cap disabled fires 5 remaps (mutation control)", func(t *testing.T) {
		repoID := uuid.New()
		syncer, mover, st := newFixture(repoID)
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		svc.SetMover(mover)
		svc.reverseCapK = 1 << 30

		if err := svc.ReverseSync(context.Background(), repoID); err != nil {
			t.Fatalf("ReverseSync: %v", err)
		}
		if len(mover.calls) != 5 {
			t.Fatalf("cap disabled must fire 5 AutoMove, got %d: %v", len(mover.calls), mover.calls)
		}
		for _, c := range mover.calls {
			if c.target != "Done" {
				t.Errorf("each move must remap to Done, got %v", c)
			}
		}
	})
}

// (c) SINGLE GENUINE CLEAR. One item clears while sitting in a column (destructiveCount
// 1) — below k=3, so it PASSES the cap: exactly one AutoMove(target="") and the marker
// advances. This is also a positive control proving the harness observes calls.
func TestReverseSyncCapSingleClearPasses(t *testing.T) {
	repoID := uuid.New()
	syncer := reverseSyncer(forge.ProjectV2ItemStatus{ItemID: "item7", IssueNumber: 7, OptionID: ""})
	mover := &fakeMover{}
	st := &fakeProjectStore{
		repo:          githubRepoRow(repoID),
		link:          oneColumnLink(t),
		columns:       []store.BoardColumn{{LabelName: "In Progress", Position: 1}},
		issues:        []store.Issue{{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t, "In Progress")}},
		existingItems: []store.GithubProjectItem{projectItem(repoID, 7, "item7", "opt_ip")},
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	if err := svc.ReverseSync(context.Background(), repoID); err != nil {
		t.Fatalf("ReverseSync: %v", err)
	}
	if len(mover.calls) != 1 || mover.calls[0].target != "" {
		t.Fatalf("a single genuine clear must pass the cap; want one AutoMove(target=\"\"), got %v", mover.calls)
	}
	if len(st.markerSets) != 1 || st.markerSets[0].LastStatusOptionID.Valid {
		t.Errorf("want marker cleared to NULL, got %+v", st.markerSets)
	}
	if len(st.linkErrs) != 0 {
		t.Errorf("a passing tick must not stamp last_error, got %v", st.linkErrs)
	}
}

// (d) BELOW-THRESHOLD via the AND-gate. Four items clear-to-empty (destructiveCount 4,
// which EXCEEDS k=3) but the board tracks 100 items, so the percentage side is NOT
// exceeded (400 > 25*100=2500 is false). Because BOTH conditions are required, the tick
// executes all four moves. An OR-gate (or a capK-only guard) would wrongly trip here —
// so this proves the AND.
func TestReverseSyncCapBelowThresholdAndGate(t *testing.T) {
	repoID := uuid.New()
	var live []forge.ProjectV2ItemStatus
	var issues []store.Issue
	var items []store.GithubProjectItem
	// Four destructive clears.
	for i := int64(1); i <= 4; i++ {
		live = append(live, forge.ProjectV2ItemStatus{ItemID: itemNode(i), IssueNumber: i, OptionID: ""})
		issues = append(issues, store.Issue{ForgeIssueIid: i, State: "opened", Labels: labelsJSON(t, "In Progress")})
		items = append(items, projectItem(repoID, i, itemNode(i), "opt_ip"))
	}
	// Pad the tracked-item set to 100 with phantom rows for issues NOT in live and NOT in
	// the issue cache (so reconcile neither prunes nor backfills them): they only inflate
	// trackedItems, driving the percentage threshold down.
	for i := int64(900); i < 996; i++ {
		items = append(items, projectItem(repoID, i, itemNode(i), "opt_ip"))
	}
	st := &fakeProjectStore{
		repo:          githubRepoRow(repoID),
		link:          oneColumnLink(t),
		columns:       []store.BoardColumn{{LabelName: "In Progress", Position: 1}},
		issues:        issues,
		existingItems: items,
	}
	if len(items) != 100 {
		t.Fatalf("fixture wants 100 tracked items, got %d", len(items))
	}
	rsyncer := reverseSyncer(live...)
	mover := &fakeMover{}
	svc := NewProjectSync(st, fakeForgeBuilder{f: rsyncer}, fakeSyncSettings{enabled: true}, nil)
	svc.SetMover(mover)

	if err := svc.ReverseSync(context.Background(), repoID); err != nil {
		t.Fatalf("ReverseSync: %v", err)
	}
	if len(mover.calls) != 4 {
		t.Fatalf("AND-gate: capK exceeded but pct not, want all 4 moves executed, got %d: %v", len(mover.calls), mover.calls)
	}
	if len(st.linkErrs) != 0 {
		t.Errorf("a below-threshold tick must not stamp last_error, got %v", st.linkErrs)
	}
}

// itemNode builds a deterministic item node id for issue iid in the cap fixtures.
func itemNode(iid int64) string { return "item" + strconv.FormatInt(iid, 10) }

// --- safe column auto-create (PRD #576 M6) ----------------------------------

// autocreateSyncer builds a ProjectBoardSyncer fake for AutoCreateColumns: it resolves a
// slug, creates a fresh field returning the given created options, and reads the given
// live statuses back (all empty for a fresh field).
func autocreateSyncer(fieldID string, createdOptions []forge.ProjectV2Option, live ...forge.ProjectV2ItemStatus) *fakeProjectSyncer {
	return &fakeProjectSyncer{
		fakeForge:   &fakeForge{},
		scopes:      []string{"repo", "project"},
		slugOwner:   "acme",
		slugRepo:    "widgets",
		createField: forge.ProjectV2StatusField{ID: fieldID, Name: "uzi Status", Options: createdOptions},
		live:        live,
	}
}

// adoptedLink is the stored link an AutoCreateColumns test starts from: an ADOPTED board
// (owned_by_uzi=false) on the built-in "Status" field whose options omit some columns.
func adoptedLink(t *testing.T, columnOption map[string]string) store.GithubProjectLink {
	l := forwardLink(t, columnOption)
	l.OwnedByUzi = false
	l.ProjectNumber = 42
	return l
}

// (M6 SC a) Fresh-field creation includes EVERY uzi column as an option, and the switch
// points the link at the new field with an empty unmatched set. Proves auto-create turns
// every skipped column into a synced one via CreateProjectV2Field (F-E), no destructive
// full-list option replace.
func TestAutoCreateColumnsCreatesFreshFieldWithEveryColumn(t *testing.T) {
	repoID := uuid.New()
	// The board has three columns; the adopted "Status" field only matched "In Progress",
	// so "Planned" and "Human Review" were skipped.
	columns := []store.BoardColumn{
		{LabelName: "In Progress", Position: 1},
		{LabelName: "Planned", Position: 2},
		{LabelName: "Human Review", Position: 3},
	}
	createdOptions := []forge.ProjectV2Option{
		{ID: "n_ip", Name: "In Progress"},
		{ID: "n_pl", Name: "Planned"},
		{ID: "n_hr", Name: "Human Review"},
	}
	syncer := autocreateSyncer("PVTSSF_NEW", createdOptions)
	st := &fakeProjectStore{
		repo:    githubRepoRow(repoID),
		link:    adoptedLink(t, map[string]string{"In Progress": "opt_ip"}), // only one matched
		columns: columns,
	}
	svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
	svc.background = syncBackground // run the async re-seed in-line, deterministically

	note, err := svc.AutoCreateColumns(context.Background(), repoID)
	if err != nil {
		t.Fatalf("AutoCreateColumns: %v", err)
	}
	if note == "" {
		t.Errorf("want a non-empty note describing the created columns")
	}

	// The fresh field was created ON THE EXISTING project with EXACTLY every board column,
	// in order, each with a valid color.
	if len(syncer.createFieldCalls) != 1 {
		t.Fatalf("want 1 CreateProjectV2Field call, got %d", len(syncer.createFieldCalls))
	}
	cf := syncer.createFieldCalls[0]
	if cf.name != "uzi Status" {
		t.Errorf("field name = %q, want \"uzi Status\"", cf.name)
	}
	// PRD #584 M1: every board column in order, then the reserved appended "Done" option.
	wantNames := []string{"In Progress", "Planned", "Human Review", "Done"}
	if len(cf.options) != len(wantNames) {
		t.Fatalf("created options = %+v, want the %d board columns plus the reserved Done option", cf.options, len(wantNames))
	}
	for i, want := range wantNames {
		if cf.options[i].Name != want {
			t.Errorf("option[%d].Name = %q, want %q", i, cf.options[i].Name, want)
		}
		if !validGithubColors[cf.options[i].Color] {
			t.Errorf("option %q has invalid color %q", cf.options[i].Name, cf.options[i].Color)
		}
	}
	// The switched link captures the reserved Done option's created id (PRD #584 M1).
	if link := st.links[0]; link.DoneOptionID != "opt-Done" {
		t.Errorf("done_option_id = %q, want the created Done option id opt-Done", link.DoneOptionID)
	}

	// The link was switched to the new field id, its option map built from the CREATED
	// option ids, unmatched now empty, and owned_by_uzi PRESERVED (adopted → still false).
	if len(st.links) != 1 {
		t.Fatalf("want 1 link upsert, got %d", len(st.links))
	}
	link := st.links[0]
	if link.StatusFieldID != "PVTSSF_NEW" {
		t.Errorf("status_field_id = %q, want the freshly created field PVTSSF_NEW", link.StatusFieldID)
	}
	if len(link.UnmatchedColumns) != 0 {
		t.Errorf("unmatched_columns must be empty after auto-create, got %v", link.UnmatchedColumns)
	}
	if link.OwnedByUzi {
		t.Errorf("owned_by_uzi must be PRESERVED false for an adopted board (uzi owns the field, not the project)")
	}
	var gotMap map[string]string
	if err := json.Unmarshal(link.StatusOptions, &gotMap); err != nil {
		t.Fatalf("status_options not valid json: %v", err)
	}
	if gotMap["In Progress"] != "n_ip" || gotMap["Planned"] != "n_pl" || gotMap["Human Review"] != "n_hr" {
		t.Errorf("column->option map = %v, want the created option ids", gotMap)
	}
	// The reset ran (F-H marker reset) exactly once.
	if st.resetMarkerCalls != 1 {
		t.Errorf("want ResetGithubProjectItemMarkers called once, got %d", st.resetMarkerCalls)
	}
}

// (M6 SC b) After the switch, a reverse tick fires ZERO AutoMove — and the marker RESET,
// NOT the M5 cap, is what prevents it. This is the subtle isolation the PRD demands:
//
// M5's per-tick destructive-write cap would ALSO abort a mass cascade, so to prove the
// RESET independently prevents it, the M5 cap is DISABLED in BOTH arms (reverseCapK huge,
// reverseCapPct 100). Then:
//   - reset PRESENT  → the reverse tick reads live("") == marker(NULL) for every item →
//     zero AutoMove (the reset is load-bearing).
//   - reset OMITTED  → markers keep the old field's ids → live("") != marker(old id) for
//     every item → mass AutoMove(target="") (the F-F cascade fires).
//
// If the cap were left ENABLED, the "omit reset" arm would ALSO show zero (the cap aborts
// it) and the test would prove nothing — the cap-disabled pair is what makes the reset's
// role non-vacuous. A simulated mis-echo here can only exercise the reverse-READ guard
// (M5's input), not a real field-update round trip, because the write path is fresh-create
// only (D3).
func TestAutoCreateColumnsResetPreventsReverseCascade(t *testing.T) {
	const n = 5

	// newFixture builds a store whose 5 items sit in "In Progress" with the OLD field's
	// option id as their marker, whose live statuses are all "" (the fresh field reads
	// empty), and the syncer/mover to drive it. skipReset toggles the marker reset off.
	newFixture := func(repoID uuid.UUID, skipReset bool) (*fakeProjectSyncer, *fakeMover, *fakeProjectStore) {
		var live []forge.ProjectV2ItemStatus
		var issues []store.Issue
		var items []store.GithubProjectItem
		for i := int64(1); i <= n; i++ {
			live = append(live, forge.ProjectV2ItemStatus{ItemID: itemNode(i), IssueNumber: i, OptionID: ""})
			issues = append(issues, store.Issue{ForgeIssueIid: i, State: "opened", Labels: labelsJSON(t, "In Progress")})
			items = append(items, projectItem(repoID, i, itemNode(i), "opt_ip")) // OLD field's option id
		}
		syncer := autocreateSyncer("PVTSSF_NEW", []forge.ProjectV2Option{{ID: "n_ip", Name: "In Progress"}}, live...)
		st := &fakeProjectStore{
			repo:            githubRepoRow(repoID),
			link:            adoptedLink(t, map[string]string{"In Progress": "opt_ip"}),
			columns:         []store.BoardColumn{{LabelName: "In Progress", Position: 1}},
			issues:          issues,
			existingItems:   items,
			skipMarkerReset: skipReset,
		}
		return syncer, &fakeMover{}, st
	}

	t.Run("reset present: cap DISABLED, still zero AutoMove", func(t *testing.T) {
		repoID := uuid.New()
		syncer, mover, st := newFixture(repoID, false)
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		svc.background = syncBackground
		svc.SetMover(mover)
		// Disable the M5 cap in BOTH arms so the reset alone is the discriminator.
		svc.reverseCapK = 1 << 30
		svc.reverseCapPct = 100

		if _, err := svc.AutoCreateColumns(context.Background(), repoID); err != nil {
			t.Fatalf("AutoCreateColumns: %v", err)
		}
		mover.calls = nil // ignore anything the re-seed did; assert only the reverse tick
		if err := svc.ReverseSync(context.Background(), repoID); err != nil {
			t.Fatalf("ReverseSync: %v", err)
		}
		if len(mover.calls) != 0 {
			t.Fatalf("reset present must yield ZERO AutoMove even with the cap disabled, got %v", mover.calls)
		}
	})

	t.Run("reset OMITTED: cap DISABLED, mass AutoMove(target=\"\") — the cascade", func(t *testing.T) {
		repoID := uuid.New()
		syncer, mover, st := newFixture(repoID, true) // reset is a no-op → markers keep old ids
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		svc.background = syncBackground
		svc.SetMover(mover)
		svc.reverseCapK = 1 << 30
		svc.reverseCapPct = 100

		if _, err := svc.AutoCreateColumns(context.Background(), repoID); err != nil {
			t.Fatalf("AutoCreateColumns: %v", err)
		}
		mover.calls = nil
		if err := svc.ReverseSync(context.Background(), repoID); err != nil {
			t.Fatalf("ReverseSync: %v", err)
		}
		if len(mover.calls) != n {
			t.Fatalf("reset omitted must fire the mass cascade of %d AutoMove, got %d: %v", n, len(mover.calls), mover.calls)
		}
		for _, c := range mover.calls {
			if c.target != "" {
				t.Errorf("cascade move must clear to Open (target \"\"), got %v", c)
			}
		}
	})
}
