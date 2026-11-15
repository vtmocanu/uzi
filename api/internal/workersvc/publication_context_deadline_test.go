package workersvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// deadlineSnapshotReader fails the snapshot read with err, after waiting for ctx when wait is set.
type deadlineSnapshotReader struct {
	Store
	err  error
	wait bool
}

func (r *deadlineSnapshotReader) PublicationContextSnapshot(ctx context.Context, _ store.PublicationContextSnapshotParams) ([]byte, error) {
	if r.wait {
		<-ctx.Done()
	}
	return nil, r.err
}

type unusedTxBeginner struct{ TxBeginner }
type unusedForgeBuilder struct{ ForgeBuilder }

func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

const snapshotDeadlineEvent = "event=publication_context_snapshot_deadline"

func TestReadPublicationSnapshotLogsDeadlinePerPhase(t *testing.T) {
	for _, phase := range []string{"read", "recheck"} {
		t.Run(phase, func(t *testing.T) {
			logs := captureDefaultLog(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			params := store.PublicationContextSnapshotParams{RunID: uuid.New(), UserID: uuid.New()}
			// pgx wraps the context error, so the reader returns a wrapped deadline.
			r := &deadlineSnapshotReader{err: fmt.Errorf("timeout: %w", context.DeadlineExceeded), wait: true}
			_, err := readPublicationSnapshot(ctx, r, params, phase)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want the reader's deadline error returned unchanged", err)
			}
			out := logs.String()
			for _, want := range []string{snapshotDeadlineEvent, "phase=" + phase, "run=" + params.RunID.String(), "elapsed_ms=", "budget_at_start_ms="} {
				if !strings.Contains(out, want) {
					t.Fatalf("log missing %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestReadPublicationSnapshotIgnoresNonDeadlineError(t *testing.T) {
	logs := captureDefaultLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r := &deadlineSnapshotReader{err: errors.New("connection reset")}
	_, err := readPublicationSnapshot(ctx, r, store.PublicationContextSnapshotParams{RunID: uuid.New()}, "read")
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the reader's non-deadline error", err)
	}
	if strings.Contains(logs.String(), snapshotDeadlineEvent) {
		t.Fatalf("a non-deadline error must not emit the deadline warning:\n%s", logs.String())
	}
}

// The first read is wired through the helper and keeps its unknown reason (#4302).
func TestResolvePublicationContextReadDeadlineKeepsReason(t *testing.T) {
	logs := captureDefaultLog(t)
	s := &Service{
		q:          &deadlineSnapshotReader{err: context.DeadlineExceeded},
		txBeginner: unusedTxBeginner{},
		forges:     unusedForgeBuilder{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	req := PublicationContextRequest{ClaimGeneration: 1, Branch: "agent/issue-1", CandidateHead: strings.Repeat("a", 40)}
	if !req.Valid() {
		t.Fatalf("fixture request must be valid")
	}
	got := s.ResolvePublicationContext(ctx, w, uuid.New(), req)
	if got.Status != "unknown" || got.Reason != "association_unknown" {
		t.Fatalf("response = %+v, want unknown/association_unknown", got)
	}
	if !strings.Contains(logs.String(), snapshotDeadlineEvent) || !strings.Contains(logs.String(), "phase=read") {
		t.Fatalf("read deadline not logged:\n%s", logs.String())
	}
}
