package issuetracker

import (
	"errors"
	"testing"
)

// serve returns a fetch over total synthetic items, recording each call.
func serve(total int, calls *[][2]int) func(skip, top int) ([]int, int, error) {
	return func(skip, top int) ([]int, int, error) {
		*calls = append(*calls, [2]int{skip, top})
		var out []int
		for i := skip; i < skip+top && i < total; i++ {
			out = append(out, i)
		}
		return out, len(out), nil
	}
}

func TestPaginateCollectsAll(t *testing.T) {
	var calls [][2]int
	got, err := paginate(serve(250, &calls))
	if err != nil || len(got) != 250 {
		t.Fatalf("len %d, err %v", len(got), err)
	}
	if len(calls) != 3 || calls[2] != [2]int{200, pageSize} {
		t.Fatalf("calls = %v", calls)
	}
}

func TestPaginateExactMultipleFetchesEmptyPage(t *testing.T) {
	var calls [][2]int
	got, err := paginate(serve(200, &calls))
	if err != nil || len(got) != 200 || len(calls) != 3 {
		t.Fatalf("len %d, calls %d, err %v", len(got), len(calls), err)
	}
}

func TestPaginateFetchError(t *testing.T) {
	want := errors.New("boom")
	_, err := paginate(func(int, int) ([]int, int, error) { return nil, 0, want })
	if !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
}

func TestPaginateCaps(t *testing.T) {
	t.Run("items", func(t *testing.T) {
		old := maxItems
		maxItems = 250
		t.Cleanup(func() { maxItems = old })
		var calls [][2]int
		if _, err := paginate(serve(10_000, &calls)); !errors.Is(err, errItemCeiling) {
			t.Fatalf("err = %v, want errItemCeiling", err)
		}
	})
	t.Run("pages", func(t *testing.T) {
		old := maxPages
		maxPages = 3
		t.Cleanup(func() { maxPages = old })
		var calls [][2]int
		if _, err := paginate(serve(10_000, &calls)); err == nil {
			t.Fatal("expected page ceiling error")
		}
		if len(calls) != 3 {
			t.Fatalf("calls = %d", len(calls))
		}
	})
	t.Run("bytes", func(t *testing.T) {
		old := maxTotalBytes
		maxTotalBytes = 1000
		t.Cleanup(func() { maxTotalBytes = old })
		calls := 0
		full := func(skip, top int) ([]int, int, error) {
			calls++
			return make([]int, top), 400, nil
		}
		if _, err := paginate(full); err == nil {
			t.Fatal("expected byte ceiling error")
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3 (stop once the budget is crossed)", calls)
		}
	})
	t.Run("server ignoring skip", func(t *testing.T) {
		calls := 0
		same := func(skip, top int) ([]int, int, error) {
			calls++
			return make([]int, top), 1 << 20, nil
		}
		if _, err := paginate(same); err == nil {
			t.Fatal("expected an error")
		}
		if calls > maxPages {
			t.Fatalf("calls = %d", calls)
		}
	})
}
