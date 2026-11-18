package forgesvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD 4173 D13: the project-sync writers that project issue numbers onto a board
// refuse a never-enabled repo before the scope preflight or any other forge call.
func TestProjectSyncWritersRefuseNeverEnabledRepo(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(svc *ProjectSyncService, repoID uuid.UUID) error
	}{
		{"Adopt", func(svc *ProjectSyncService, id uuid.UUID) error {
			return svc.Adopt(context.Background(), id, 7, forge.OwnerUser)
		}},
		{"Provision", func(svc *ProjectSyncService, id uuid.UUID) error {
			return svc.Provision(context.Background(), id, forge.OwnerUser, "")
		}},
		{"Resync", func(svc *ProjectSyncService, id uuid.UUID) error {
			_, err := svc.Resync(context.Background(), id)
			return err
		}},
		{"AutoCreateColumns", func(svc *ProjectSyncService, id uuid.UUID) error {
			_, err := svc.AutoCreateColumns(context.Background(), id)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoID := uuid.New()
			row := githubRepoRow(repoID)
			row.AwaitingFirstEnable = true
			syncer := provisionSyncer("f", nil)
			st := &fakeProjectStore{repo: row}
			svc := NewProjectSync(st, fakeForgeBuilder{f: syncer}, fakeSyncSettings{enabled: true}, nil)
			err := tc.call(svc, repoID)
			if !errors.Is(err, store.ErrRepoNeverEnabled) {
				t.Fatalf("error = %v, want store.ErrRepoNeverEnabled", err)
			}
			if syncer.tokenInfoCalls != 0 || len(syncer.createProjectCalls) != 0 {
				t.Errorf("forge calls: TokenInfo=%d CreateProject=%d; want none", syncer.tokenInfoCalls, len(syncer.createProjectCalls))
			}
			if len(st.links) != 0 {
				t.Errorf("a refused call wrote a link")
			}
		})
	}
}
