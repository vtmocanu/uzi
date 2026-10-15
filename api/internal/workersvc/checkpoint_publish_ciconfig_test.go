package workersvc

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestPublishCIConfigGuardSkipsAndCarriesRunRow pins PRD #2865 at the service seam: the guard
// flag and paths come from the run row into pushbroker.Options, and the broker's
// ErrCIConfigGuard is a benign "ci_config_guard" skip (nil error, no tip persisted).
func TestPublishCIConfigGuardSkipsAndCarriesRunRow(t *testing.T) {
	box := newBox(t)
	sealed, err := box.Seal([]byte("pat"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	paths := []string{".github/workflows/**", "deploy/**"}
	for _, guarded := range []bool{true, false} {
		t.Run(fmt.Sprintf("guard=%v", guarded), func(t *testing.T) {
			fs := &fakeStore{
				runOwned: store.Run{
					Kind:          runkind.Issue,
					IssueIid:      pgtype.Int8{Int64: 2865, Valid: true},
					CiConfigGuard: guarded,
					CiConfigPaths: paths,
				},
				claimCtx: store.GetRunClaimContextRow{
					RepoWebUrl:      "https://gitlab.example.com/team/repo",
					DefaultBranch:   pgtype.Text{String: "main", Valid: true},
					BaseUrl:         "https://gitlab.example.com",
					BotUsername:     "uzi-bot",
					TokenCiphertext: sealed,
				},
			}
			svc := New(fs, box, testParams())
			svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://gitlab.example.com" })
			var got pushbroker.Options
			svc.SetPublishFn(func(_ context.Context, o pushbroker.Options) (pushbroker.Result, error) {
				got = o
				return pushbroker.Result{}, fmt.Errorf("wrapped: %w", pushbroker.ErrCIConfigGuard)
			})

			res, err := svc.Publish(context.Background(), worker(), uuid.New(), "0123456789abcdef0123456789abcdef01234567", []byte("pack"))
			if err != nil {
				t.Fatalf("Publish error = %v, want a benign skip", err)
			}
			if res.Published || res.Skipped != "ci_config_guard" {
				t.Fatalf("result = %+v, want Skipped ci_config_guard", res)
			}
			if res.Ref != "refs/uzi-checkpoints/agent/issue-2865" {
				t.Errorf("Ref = %q", res.Ref)
			}
			if got.CIConfigGuard != guarded || len(got.CIConfigPaths) != len(paths) || got.CIConfigPaths[1] != "deploy/**" {
				t.Errorf("broker options guard=%v paths=%v, want guard=%v paths=%v", got.CIConfigGuard, got.CIConfigPaths, guarded, paths)
			}
			if len(fs.checkpointTips) != 0 {
				t.Errorf("tip persisted %d time(s) on a guard skip, want 0", len(fs.checkpointTips))
			}
		})
	}
}

func TestPushDefinitelyRefusedCoversCIConfigGuard(t *testing.T) {
	if !pushDefinitelyRefused(fmt.Errorf("x: %w", pushbroker.ErrCIConfigGuard)) {
		t.Fatal("pushDefinitelyRefused(ErrCIConfigGuard) = false, want true: the attempt row must clear")
	}
}

func TestHeldReasonCIConfigGuardWireValue(t *testing.T) {
	if HeldReasonCIConfigGuard != "ci_config_guard" {
		t.Fatalf("HeldReasonCIConfigGuard = %q", HeldReasonCIConfigGuard)
	}
}
