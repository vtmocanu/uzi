package workersvc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Drive the public publication seam with the shared, literal coordinate oracle.
func TestPublishEnrolledCoordinateFixture(t *testing.T) {
	for _, fixture := range readCoordinateCases(t) {
		if !*fixture.Eligible {
			continue
		}
		t.Run(fixture.Name, func(t *testing.T) {
			root := fixture.Nodes[0]
			run := store.Run{
				ID: uuid.MustParse(root["id"].(string)), UserID: uuid.New(),
				RepoID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, Kind: root["kind"].(string),
				Status: "running",
			}
			if branch, ok := root["branch"].(string); ok {
				run.Branch = pgtype.Text{String: branch, Valid: true}
			}
			if iid, ok := root["issue_iid"].(float64); ok {
				run.IssueIid = pgtype.Int8{Int64: int64(iid), Valid: true}
			}
			if run.Kind == "task" {
				var iid *int64
				if run.IssueIid.Valid {
					value := run.IssueIid.Int64
					iid = &value
				}
				provenance := make([]uuid.UUID, len(fixture.Nodes))
				for i, node := range fixture.Nodes {
					provenance[i] = uuid.MustParse(node["id"].(string))
				}
				descriptor := CheckpointCoordinates{
					Version: 1, RunID: run.ID, UserID: run.UserID, RepoID: uuid.UUID(run.RepoID.Bytes),
					Kind: run.Kind, IssueIID: iid, WorkBranch: *fixture.WorkBranch,
					CheckpointKey: *fixture.CheckpointKey, Provenance: provenance,
				}
				var err error
				run.CheckpointCoordinates, err = json.Marshal(descriptor)
				if err != nil {
					t.Fatal(err)
				}
			}
			box := newBox(t)
			sealed, err := box.Seal([]byte("fixture-pat"))
			if err != nil {
				t.Fatal(err)
			}
			fs := &fakeStore{runOwned: run, claimCtx: store.GetRunClaimContextRow{
				RepoWebUrl: "https://gitlab.example.com/team/repo", BaseUrl: "https://gitlab.example.com",
				DefaultBranch: pgtype.Text{String: "main", Valid: true}, TokenCiphertext: sealed,
			}}
			svc := New(fs, box, testParams())
			svc.SetForgeBaseURLAllowed(func(string) bool { return true })
			calls := 0
			svc.SetPublishFn(func(_ context.Context, options pushbroker.Options) (pushbroker.Result, error) {
				calls++
				if options.WorkBranch != *fixture.WorkBranch || options.CheckpointKey != *fixture.CheckpointKey {
					t.Fatalf("coordinates=(%s,%s) want=(%s,%s)", options.WorkBranch, options.CheckpointKey, *fixture.WorkBranch, *fixture.CheckpointKey)
				}
				return pushbroker.Result{}, nil
			})
			registered := worker()
			registered.UserID = run.UserID
			registered.ProtocolCapabilities = []string{capability.TaskCheckpointCoordinatesV1}
			result, err := svc.Publish(context.Background(), registered, run.ID, testCheckpointTip, []byte("pack"))
			if err != nil || !result.Published || calls != 1 || result.Ref != "refs/uzi-checkpoints/"+*fixture.CheckpointKey {
				t.Fatalf("result=%+v calls=%d err=%v", result, calls, err)
			}
			if run.Kind == "task" {
				legacyWorker := registered
				legacyWorker.ProtocolCapabilities = nil
				result, err = svc.Publish(context.Background(), legacyWorker, run.ID, testCheckpointTip, []byte("pack"))
				if err != nil || result.Skipped != "unsupported" || calls != 1 {
					t.Fatalf("incompatible worker result=%+v calls=%d err=%v", result, calls, err)
				}
				fs.runOwned.CheckpointCoordinates = nil
				result, err = svc.Publish(context.Background(), registered, run.ID, testCheckpointTip, []byte("pack"))
				if err != nil || result.Skipped != "unsupported" || calls != 1 {
					t.Fatalf("unenrolled result=%+v calls=%d err=%v", result, calls, err)
				}
				fs.runOwned.CheckpointCoordinates = run.CheckpointCoordinates
				fs.runOwned.Branch = pgtype.Text{String: "wrong/branch", Valid: true}
				_, err = svc.Publish(context.Background(), registered, run.ID, testCheckpointTip, []byte("pack"))
				if err == nil || calls != 1 {
					t.Fatalf("drift accepted: calls=%d err=%v", calls, err)
				}
			}
		})
	}
}
