package forgesvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func linkedGithubRow(repoID uuid.UUID) store.GetRepoByIDRow {
	row := githubRepoRow(repoID)
	row.IssueTrackerConnectionID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	return row
}

// PRD #4173 M4: a project's items are forge issue numbers, so a repo linked to an
// issue tracker is never projected onto a GitHub Projects v2 board.
func TestProjectSyncSkipsALinkedRepo(t *testing.T) {
	repoID := uuid.New()
	t.Run("forward move and reverse sync do nothing", func(t *testing.T) {
		syncer := forwardSyncer()
		st := &fakeProjectStore{
			repo:   linkedGithubRow(repoID),
			link:   forwardLink(t, map[string]string{"In Progress": "opt_ip"}),
			item:   store.GithubProjectItem{RepoID: repoID, ForgeIssueIid: 7, ItemNodeID: "item7"},
			issues: []store.Issue{{ForgeIssueIid: 7, State: "opened", Labels: labelsJSON(t)}},
		}
		svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
		mover := &fakeMover{}
		svc.SetMover(mover)

		if err := svc.ForwardMove(context.Background(), repoID, 7, "In Progress"); err != nil {
			t.Fatal(err)
		}
		if err := svc.ReverseSync(context.Background(), repoID); err != nil {
			t.Fatal(err)
		}
		if len(syncer.setCalls) != 0 || len(mover.calls) != 0 || len(st.markerSets) != 0 || len(st.items) != 0 {
			t.Fatalf("a linked repo was projected: set=%v moves=%v markers=%v items=%v", syncer.setCalls, mover.calls, st.markerSets, st.items)
		}
	})
	t.Run("creating or adopting a link is refused", func(t *testing.T) {
		for name, call := range map[string]func(*ProjectSyncService) error{
			"Adopt": func(s *ProjectSyncService) error { return s.Adopt(context.Background(), repoID, 7, forge.OwnerUser) },
			"Provision": func(s *ProjectSyncService) error {
				return s.Provision(context.Background(), repoID, forge.OwnerUser, "")
			},
			"Resync": func(s *ProjectSyncService) error { _, err := s.Resync(context.Background(), repoID); return err },
			"AutoCreateColumns": func(s *ProjectSyncService) error {
				_, err := s.AutoCreateColumns(context.Background(), repoID)
				return err
			},
		} {
			t.Run(name, func(t *testing.T) {
				syncer := provisionSyncer("f", nil)
				st := &fakeProjectStore{repo: linkedGithubRow(repoID)}
				svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
				if err := call(svc); !errors.Is(err, ErrProjectSyncLinkedRepo) {
					t.Fatalf("error = %v, want ErrProjectSyncLinkedRepo", err)
				}
				if syncer.tokenInfoCalls != 0 || len(syncer.createProjectCalls) != 0 || len(st.links) != 0 {
					t.Errorf("a refused call reached the forge or wrote a link")
				}
			})
		}
	})
}
