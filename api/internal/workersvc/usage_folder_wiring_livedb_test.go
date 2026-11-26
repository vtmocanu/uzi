package workersvc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4700 M2: a REAL terminal transition (Service.SetState reporting `completed`) reaches the
// store.UsageFolder through the same TeeLifecycle composition cmd/server's wireUsageFolder
// installs, and the folder's real materializer leaves the run with a fresh state and the right
// fold rows. Skipped unless UZI_TEST_DATABASE_URL is set (./e2e/run-store-it.sh).

type teeBoard struct {
	mu    sync.Mutex
	notes []autoStopEvent
}

func (b *teeBoard) Notify(id uuid.UUID, status string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.notes = append(b.notes, autoStopEvent{id, status})
}

func TestTerminalTransitionKicksUsageFolderLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	wkr, runID := env.seedNoteRun(t, "claude")
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool) // the terminal status write is fenced in a transaction
	gen := int64(1)
	if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, claudeNoteScenario(), &gen); err != nil {
		t.Fatalf("append: %v", err)
	}
	// The run was inserted before the fold existed in this fixture; mark it foldable the way
	// newFoldLockFixture does (the terminal status itself comes from the real transition below).
	env.exec(`UPDATE runs SET usage_refolded = true WHERE id = $1`, runID)
	var wantExists bool
	if err := env.pool.QueryRow(env.ctx, `SELECT EXISTS (SELECT 1 FROM run_usage_fold_state WHERE run_id = $1 AND stale)`, runID).Scan(&wantExists); err != nil || !wantExists {
		t.Fatalf("before the transition the run must have a stale state row (writer marked it): exists=%v err=%v", wantExists, err)
	}

	type folded struct {
		id  uuid.UUID
		n   int64
		err error
	}
	got := make(chan folded, 4)
	folder := store.NewUsageFolder(env.pool, store.UsageFolderOptions{
		OnFolded: func(id uuid.UUID, n int64, err error) { got <- folded{id, n, err} },
	})
	board := &teeBoard{}
	svc.SetLifecycle(TeeLifecycle(board, folder))

	run, applied, err := svc.SetState(env.ctx, hbWorker(t, env, wkr.ID), runID, StateRequest{State: "completed", ClaimGeneration: &gen})
	if err != nil || !applied || run.Status != "completed" {
		t.Fatalf("SetState completed = status %q applied %v err %v", run.Status, applied, err)
	}

	select {
	case f := <-got:
		if f.id != runID || f.n != 1 || f.err != nil {
			t.Fatalf("onFolded = %+v, want run %s n=1 err=nil", f, runID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the terminal transition never reached the usage folder")
	}
	wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := folder.Wait(wctx); err != nil {
		t.Fatal(err)
	}

	board.mu.Lock()
	if len(board.notes) != 1 || board.notes[0].id != runID || board.notes[0].status != "completed" {
		t.Errorf("the existing lifecycle saw %+v, want one completed notification", board.notes)
	}
	board.mu.Unlock()

	var stale bool
	if err := env.pool.QueryRow(env.ctx, `SELECT stale FROM run_usage_fold_state WHERE run_id = $1`, runID).Scan(&stale); err != nil || stale {
		t.Fatalf("fold state after the kick: stale=%v err=%v, want a fresh row", stale, err)
	}
	live := env.usageTotals(t, runID)
	var in, out, cr, cw int64
	var rows int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*), COALESCE(sum(input_tokens),0)::bigint, COALESCE(sum(output_tokens),0)::bigint,
		COALESCE(sum(cache_read_tokens),0)::bigint, COALESCE(sum(cache_creation_tokens),0)::bigint FROM run_usage_model_folds WHERE run_id = $1`, runID).
		Scan(&rows, &in, &out, &cr, &cw); err != nil {
		t.Fatal(err)
	}
	if rows == 0 || in != live.in || out != live.out || cr != live.cr || cw != live.cw {
		t.Fatalf("fold rows = %d (in %d out %d cr %d cw %d), want the run's live totals %+v", rows, in, out, cr, cw, live)
	}
	if live.in == 0 && live.out == 0 {
		t.Fatalf("fixture run has no usage; the comparison proves nothing: %+v", live)
	}
}
