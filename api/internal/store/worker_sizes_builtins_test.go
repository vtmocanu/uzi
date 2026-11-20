package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// sizesFake implements workerSizeReconcilerQueries in memory: a name in existing makes the
// insert a no-op (0 rows, as ON CONFLICT DO NOTHING would).
type sizesFake struct {
	existing  map[string]WorkerSize
	inserted  []string
	refreshed []string
	insertErr error
}

func (f *sizesFake) InsertBuiltinWorkerSize(_ context.Context, arg InsertBuiltinWorkerSizeParams) (int64, error) {
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	if _, ok := f.existing[arg.Name]; ok {
		return 0, nil
	}
	f.inserted = append(f.inserted, arg.Name)
	return 1, nil
}

func (f *sizesFake) RefreshPristineBuiltinWorkerSize(_ context.Context, arg RefreshPristineBuiltinWorkerSizeParams) (int64, error) {
	f.refreshed = append(f.refreshed, arg.Name)
	return 0, nil
}

func (f *sizesFake) GetWorkerSize(_ context.Context, name string) (WorkerSize, error) {
	if r, ok := f.existing[name]; ok {
		return r, nil
	}
	return WorkerSize{}, errors.New("not found")
}

func TestReconcileWorkerSizesSeedsAllBuiltinsOnAnEmptyTable(t *testing.T) {
	f := &sizesFake{}
	logs := captureLogs(t, func() {
		if err := reconcileBuiltinWorkerSizes(context.Background(), f); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	})
	if want := []string{"s", "m", "l", "l-highmem"}; !slices.Equal(f.inserted, want) {
		t.Errorf("inserted %v, want %v", f.inserted, want)
	}
	if len(f.refreshed) != 0 {
		t.Errorf("refresh ran for a fresh insert: %v", f.refreshed)
	}
	if strings.Contains(logs, "shadowed") {
		t.Errorf("unexpected shadow warning:\n%s", logs)
	}
}

func TestReconcileWorkerSizesRefreshesExistingBuiltinsWithoutWarning(t *testing.T) {
	f := &sizesFake{existing: map[string]WorkerSize{"m": {Name: "m", Builtin: true}}}
	logs := captureLogs(t, func() {
		if err := reconcileBuiltinWorkerSizes(context.Background(), f); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	})
	if !slices.Equal(f.refreshed, []string{"m"}) {
		t.Errorf("refreshed %v, want [m]", f.refreshed)
	}
	if strings.Contains(logs, "shadowed") {
		t.Errorf("a builtin row must not warn:\n%s", logs)
	}
}

func TestReconcileWorkerSizesWarnsWhenAnAdminSizeShadowsABuiltin(t *testing.T) {
	f := &sizesFake{existing: map[string]WorkerSize{"l": {Name: "l", Builtin: false}}}
	logs := captureLogs(t, func() {
		if err := reconcileBuiltinWorkerSizes(context.Background(), f); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	})
	if strings.Count(logs, "shadowed by an admin size") != 1 || !strings.Contains(logs, `"name":"l"`) {
		t.Errorf("want exactly one shadow warning naming l:\n%s", logs)
	}
	if slices.Contains(f.inserted, "l") {
		t.Error("the shadowed builtin must not be inserted")
	}
}

func TestReconcileWorkerSizesStopsOnAnInsertError(t *testing.T) {
	boom := errors.New("db down")
	err := reconcileBuiltinWorkerSizes(context.Background(), &sizesFake{insertErr: boom})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the insert error", err)
	}
}
