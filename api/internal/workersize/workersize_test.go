package workersize

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Names are wire strings: they go out verbatim as DesiredWorker.Size, so a shipped name
// must be lowercase and unpadded to satisfy the worker_sizes name pattern.
func TestNamesAreWireSafe(t *testing.T) {
	for _, n := range Names {
		if n == "" {
			t.Error("Names contains an empty entry")
			continue
		}
		if n != strings.ToLower(n) || n != strings.TrimSpace(n) {
			t.Errorf("Names entry %q must be lowercase and unpadded (it goes on the wire as DesiredWorker.Size)", n)
		}
	}
}

func TestNamesAreTheBuiltinsInOrder(t *testing.T) {
	want := []string{"s", "m", "l", "l-highmem"}
	if strings.Join(Names, ",") != strings.Join(want, ",") {
		t.Fatalf("Names = %v, want %v", Names, want)
	}
}

type fakeQueries struct {
	rows  map[string]store.WorkerSize
	err   error
	calls int
}

func (f *fakeQueries) ListWorkerSizes(context.Context) ([]store.WorkerSize, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	var out []store.WorkerSize
	for _, r := range f.rows {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeQueries) GetWorkerSize(_ context.Context, name string) (store.WorkerSize, error) {
	f.calls++
	if f.err != nil {
		return store.WorkerSize{}, f.err
	}
	r, ok := f.rows[name]
	if !ok {
		return store.WorkerSize{}, pgx.ErrNoRows
	}
	return r, nil
}

func TestExistsReadsTheStore(t *testing.T) {
	f := &fakeQueries{rows: map[string]store.WorkerSize{"xs": {Name: "xs"}}}
	ctx := context.Background()
	if ok, err := Exists(ctx, f, "xs"); err != nil || !ok {
		t.Errorf("Exists(xs) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := Exists(ctx, f, "xl"); err != nil || ok {
		t.Errorf("Exists(xl) = %v, %v; want false, nil", ok, err)
	}
}

func TestExistsRejectsMalformedNamesWithoutAQuery(t *testing.T) {
	f := &fakeQueries{}
	for _, bad := range []string{"", " ", "S", "s ", "../s", "1s", "a-very-long-size-name"} {
		if ok, err := Exists(context.Background(), f, bad); ok || err != nil {
			t.Errorf("Exists(%q) = %v, %v; want false, nil", bad, ok, err)
		}
	}
	if f.calls != 0 {
		t.Errorf("malformed names reached the store %d times", f.calls)
	}
}

func TestExistsPropagatesStoreErrors(t *testing.T) {
	boom := errors.New("db down")
	f := &fakeQueries{err: boom}
	ok, err := Exists(context.Background(), f, "m")
	if ok || !errors.Is(err, boom) {
		t.Errorf("Exists = %v, %v; want false and the store error (an ambiguous failure is not a no-such-size)", ok, err)
	}
}

func TestListAndGet(t *testing.T) {
	f := &fakeQueries{rows: map[string]store.WorkerSize{"m": {Name: "m", DataGib: 25}}}
	ctx := context.Background()
	rows, err := List(ctx, f)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List = %v, %v", rows, err)
	}
	row, ok, err := Get(ctx, f, "m")
	if err != nil || !ok || row.DataGib != 25 {
		t.Fatalf("Get(m) = %+v, %v, %v", row, ok, err)
	}
	if _, ok, err := Get(ctx, f, "nope"); ok || err != nil {
		t.Fatalf("Get(nope) = %v, %v; want absent without error", ok, err)
	}
}
