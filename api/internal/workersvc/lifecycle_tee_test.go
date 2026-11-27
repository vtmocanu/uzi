package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestTeeLifecycleFansOutInOrderAndSkipsNil(t *testing.T) {
	a, b := &fakeLifecycle{}, &fakeLifecycle{}
	tee := TeeLifecycle(a, nil, b)
	id := uuid.New()
	tee.Notify(id, "completed")
	tee.Notify(id, "failed")
	for name, l := range map[string]*fakeLifecycle{"first": a, "last": b} {
		if len(l.notes) != 2 || l.notes[0] != (lifecycleNote{id, "completed"}) || l.notes[1] != (lifecycleNote{id, "failed"}) {
			t.Fatalf("%s member notes = %+v, want both notifications in order", name, l.notes)
		}
	}
}

func TestTeeLifecycleWithNoMembersIsANoOpNotNil(t *testing.T) {
	for _, tee := range []RunLifecycle{TeeLifecycle(), TeeLifecycle(nil, nil)} {
		if tee == nil {
			t.Fatal("TeeLifecycle returned nil, want a no-op lifecycle")
		}
		tee.Notify(uuid.New(), "completed") // must not panic
	}
}

// The real notify path: Service.SetState on a terminal transition notifies the installed
// tee, which reaches both the board automation stand-in and a real UsageFolder whose kick
// materializes the run (the materializer is injected so no database is needed).
func TestTeeLifecycleReachesUsageFolderFromRealSetState(t *testing.T) {
	w := worker()
	fs := &fakeStore{
		runOwned:         store.Run{ID: uuid.New(), WorkerID: pgconv.UUID(w.ID), Status: "completed"},
		setCompletedRows: 1,
	}
	svc := New(fs, newBox(t), testParams())

	type fold struct {
		id uuid.UUID
		n  int64
	}
	folded := make(chan fold, 1)
	folder := store.NewUsageFolder(nil, store.UsageFolderOptions{
		Materialize: func(_ context.Context, ids []uuid.UUID) (int64, error) { return int64(len(ids)), nil },
		OnFolded:    func(id uuid.UUID, n int64, _ error) { folded <- fold{id, n} },
	})
	board := &fakeLifecycle{}
	svc.SetLifecycle(TeeLifecycle(board, folder))

	branch, mr := "agent/issue-1", int64(9)
	if _, applied, err := svc.SetState(context.Background(), w, fs.runOwned.ID, StateRequest{
		State: "completed", Branch: &branch, MrIID: &mr,
	}); err != nil || !applied {
		t.Fatalf("SetState: applied=%v err=%v", applied, err)
	}
	select {
	case f := <-folded:
		if f.id != fs.runOwned.ID || f.n != 1 {
			t.Fatalf("folded %+v, want run %s n=1", f, fs.runOwned.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the usage folder was never kicked by the terminal SetState")
	}
	if len(board.notes) != 1 || board.notes[0].status != "completed" {
		t.Fatalf("board lifecycle notes = %+v, want the same completed notification", board.notes)
	}
	if err := folder.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
