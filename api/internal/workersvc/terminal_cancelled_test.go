package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Track both delivery paths; all other unexpected queries panic through fakeStore.
type terminalCancelStore struct {
	*fakeStore
	reads int
}

func (s *terminalCancelStore) ConsumeRunInputs(context.Context, uuid.UUID) ([]store.ConsumeRunInputsRow, error) {
	s.reads++
	return nil, nil
}
func (s *terminalCancelStore) ListReplayRunInputs(context.Context, uuid.UUID) ([]store.ListReplayRunInputsRow, error) {
	s.reads++
	return nil, nil
}

func TestConsumeInputsTerminalCancelled(t *testing.T) {
	for _, capable := range []bool{false, true} {
		for _, shape := range []string{"ordinary", "released", "pending switch", "running", "failed", "completed", "zero generation", "chat", "foreign worker", "wrong lane"} {
			t.Run(shape+map[bool]string{false: " legacy", true: " receipts"}[capable], func(t *testing.T) {
				w := worker()
				if capable {
					w.ProtocolCapabilities = []string{capability.InputReceiptsV1}
				}
				run := store.Run{ID: uuid.New(), Status: "cancelled", Kind: "issue", ClaimGeneration: 17}
				stamp := pgtype.Timestamptz{Time: time.Now(), Valid: true}
				want := true
				switch shape {
				case "released":
					run.ClaimReleasedAt = stamp
				case "pending switch":
					run.CredentialSwitchRequestedAt = stamp
					run.CredentialSwitchGeneration = pgtype.Int8{Int64: 17, Valid: true}
				case "running", "failed", "completed":
					run.Status = shape
					want = false
				case "zero generation":
					run.ClaimGeneration = 0
					want = false
				case "chat":
					run.Kind = "chat"
					want = false
				case "wrong lane":
					w.IsolatedLane = true
				}
				fs := &terminalCancelStore{fakeStore: &fakeStore{runOwned: run}}
				if shape == "foreign worker" {
					fs.runOwnedErr = pgx.ErrNoRows
				}
				svc := New(fs, newBox(t), testParams())
				for poll := 0; poll < 2; poll++ {
					got, err := svc.ConsumeInputs(context.Background(), w, run.ID)
					if shape == "foreign worker" || shape == "wrong lane" {
						if !errors.Is(err, ErrRunNotOwned) || got.TerminalCancelled != nil {
							t.Fatalf("unauthorized poll = %+v, %v", got, err)
						}
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					if !want {
						if got.TerminalCancelled != nil {
							t.Fatalf("unexpected control: %+v", got)
						}
						continue
					}
					if got.TerminalCancelled == nil || got.TerminalCancelled.RunID != run.ID || got.TerminalCancelled.Generation != 17 {
						t.Fatalf("control = %+v, want run %s generation 17", got.TerminalCancelled, run.ID)
					}
					if len(got.Inputs) != 0 || got.CredentialSwitch != nil || got.Receipts || got.OwnerID != uuid.Nil || fs.reads != 0 {
						t.Fatalf("terminal poll touched delivery path: %+v, reads=%d", got, fs.reads)
					}
				}
			})
		}
	}
}
