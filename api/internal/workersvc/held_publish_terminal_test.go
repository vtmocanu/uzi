package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type terminalHeldStore struct {
	Store
	run  store.Run
	hold store.RecoveryCustodyHold
}

func (s *terminalHeldStore) GetRunByIDForUser(_ context.Context, p store.GetRunByIDForUserParams) (store.Run, error) {
	if p.ID != s.run.ID || p.UserID != s.run.UserID {
		return store.Run{}, pgx.ErrNoRows
	}
	return s.run, nil
}
func (s *terminalHeldStore) GetRunOwnedByWorker(_ context.Context, p store.GetRunOwnedByWorkerParams) (store.Run, error) {
	if p.ID != s.run.ID || p.WorkerID != s.run.WorkerID {
		return store.Run{}, pgx.ErrNoRows
	}
	return s.run, nil
}
func (s *terminalHeldStore) GetHeldPublicationHold(context.Context, store.GetHeldPublicationHoldParams) (store.RecoveryCustodyHold, error) {
	return s.hold, nil
}
func (s *terminalHeldStore) GetHeldPublicationByRunGeneration(context.Context, store.GetHeldPublicationByRunGenerationParams) (store.RunHeldPublication, error) {
	return store.RunHeldPublication{}, pgx.ErrNoRows
}

func TestHeldTerminalPublicGate(t *testing.T) {
	for _, status := range []string{"failed", "cancelled", "completed"} {
		for _, older := range []bool{false, true} {
			for _, kind := range heldHoldKinds {
				t.Run(status+"/"+kind+map[bool]string{false: "/current", true: "/older"}[older], func(t *testing.T) {
					w := store.Worker{ID: uuid.New(), UserID: uuid.New(), ProtocolCapabilities: []string{capability.RecoveryHeldPublicationV2}}
					r := store.Run{ID: uuid.New(), UserID: w.UserID, RepoID: pgconv.UUID(uuid.New()), WorkerID: pgconv.UUID(uuid.New()), Status: status, Kind: kind, ClaimGeneration: 1}
					if older {
						r.ClaimGeneration = 2
					}
					h := store.RecoveryCustodyHold{ID: uuid.New(), RunID: r.ID, UserID: w.UserID, RepoID: r.RepoID, OriginalWorkerID: w.ID, LiveWorkerID: pgconv.UUID(w.ID), LiveRunID: pgconv.UUID(r.ID), Generation: 1, State: "open", InventoryGuarded: true}
					q := &terminalHeldStore{run: r, hold: h}
					p := testParams()
					p.HeldPublication = true
					s := New(q, newBox(t), p)
					req := HeldPublishRequest{Generation: 1, Tip: heldTestTip, Coverage: heldTestCov}
					if res, err := s.HeldPublishGate(context.Background(), w, r.ID, req); err != nil || res != nil {
						t.Fatalf("gate=%+v %v", res, err)
					}
					for _, bad := range []string{"original", "live", "user", "repo", "run", "live_run", "generation", "closed", "unguarded", "future", "zero", "lane", "origin"} {
						t.Run(bad, func(t *testing.T) {
							q.run, q.hold = r, h
							request := req
							switch bad {
							case "original":
								q.hold.OriginalWorkerID = uuid.New()
							case "live":
								q.hold.LiveWorkerID = pgconv.UUID(uuid.New())
							case "user":
								q.hold.UserID = uuid.New()
							case "repo":
								q.hold.RepoID = pgconv.UUID(uuid.New())
							case "run":
								q.hold.RunID = uuid.New()
							case "live_run":
								q.hold.LiveRunID = pgconv.UUID(uuid.New())
							case "generation":
								q.hold.Generation = 2
							case "closed":
								q.hold.State = "discarded"
							case "unguarded":
								q.hold.InventoryGuarded = false
							case "future":
								request.Generation = 3
							case "zero":
								request.Generation = 0
							case "lane":
								q.run.EgressProfileID = pgconv.UUID(uuid.New())
							case "origin":
								q.run.FailOrigin = pgconv.Text("push_secret_blocked")
							}
							if _, err := s.HeldPublishGate(context.Background(), w, r.ID, request); err == nil {
								t.Fatal("accepted invalid custody")
							}
						})
					}
					q.run, q.hold = r, h
					w.ProtocolCapabilities = []string{capability.RecoveryHeldPublicationV1}
					if _, err := s.HeldPublishGate(context.Background(), w, r.ID, req); !errors.Is(err, ErrRunNotOwned) {
						t.Fatalf("V1 reassigned: %v", err)
					}
					q.run.WorkerID = pgconv.UUID(w.ID)
					_, err := s.HeldPublishGate(context.Background(), w, r.ID, req)
					if status == "failed" && !older {
						if err != nil {
							t.Fatal(err)
						}
					} else if err == nil {
						t.Fatal("V1 widened")
					}
				})
			}
		}
	}
}
