package workersvc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Assert the actual JSON, so an absent DTO field or an omitted constructor
// projection fails independently of Go's zero-value decoding.
func assertClaimRecoveryEpisode(t *testing.T, payload any, want int64) {
	t.Helper()
	wire, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(wire, &fields); err != nil {
		t.Fatal(err)
	}
	raw, present := fields["worker_recovery_episode"]
	if !present {
		t.Fatal("claim wire lacks worker_recovery_episode")
	}
	var got int64
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("claim worker_recovery_episode=%d want=%d", got, want)
	}
}

type recoveryClaimJobStore struct{ *fakeStore }

func (s *recoveryClaimJobStore) ClearJobResultForRun(context.Context, uuid.UUID) error { return nil }
func (s *recoveryClaimJobStore) ListJobInputsForClaim(context.Context, uuid.UUID) ([]store.ListJobInputsForClaimRow, error) {
	return nil, nil
}
func (s *recoveryClaimJobStore) ListJobInputFilesForClaim(context.Context, pgtype.UUID) ([]store.ListJobInputFilesForClaimRow, error) {
	return nil, nil
}

func TestWorkerRecoveryClaimConstructorsJSON(t *testing.T) {
	for _, kind := range []string{"issue", "judge", "job", "chat"} {
		t.Run(kind, func(t *testing.T) {
			for _, episode := range []int64{0, 7} {
				box := newBox(t)
				pat, err := box.Seal([]byte("fixture-pat"))
				if err != nil {
					t.Fatal(err)
				}
				token, err := box.Seal([]byte("fixture-model-token"))
				if err != nil {
					t.Fatal(err)
				}
				uid := uuid.New()
				run := store.Run{ID: uuid.New(), UserID: uid, Kind: kind, Harness: "claude", Status: "claimed",
					WorkerRecoveryEpisode: episode, ClaimGeneration: 13, RequeueCount: 4, MemoryEpisode: 2,
					TargetRunID: pgconv.UUID(uuid.New()), JobType: pgconv.Text("research")}
				fs := &fakeStore{claimRun: run, chatClaimRun: run, anthropic: token,
					claimCtx: store.GetRunClaimContextRow{RepoWebUrl: "https://forge.example/g/r",
						RepoPath: "g/r", ForgeType: "github", BaseUrl: "https://forge.example", TokenCiphertext: pat}}
				svc := New(&recoveryClaimJobStore{fs}, box, testParams())
				worker := store.Worker{ID: uuid.New(), UserID: uid}
				if kind == "chat" {
					payload, err := svc.ClaimChat(context.Background(), worker)
					if err != nil || payload == nil {
						t.Fatalf("ClaimChat returned payload=%v err=%v", payload != nil, err)
					}
					assertClaimRecoveryEpisode(t, payload, episode)
				} else {
					payload, err := svc.Claim(context.Background(), worker, nil)
					if err != nil || payload == nil {
						t.Fatalf("Claim returned payload=%v err=%v", payload != nil, err)
					}
					assertClaimRecoveryEpisode(t, payload, episode)
				}
			}
		})
	}
}
