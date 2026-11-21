package handler

import (
	"fmt"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// refusalClean is the production sanitizer, so the tests follow any change to it.
var refusalClean = cleanRefusalText

// refusalsFor builds n refusal entries for one raw size, named so their order is known.
func refusalsFor(raw string, n int) []apitypes.WorkerSizeRefusalDTO {
	out := make([]apitypes.WorkerSizeRefusalDTO, 0, n)
	for i := n - 1; i >= 0; i-- { // reverse, so selection must sort
		out = append(out, apitypes.WorkerSizeRefusalDTO{
			Size: refusalClean(raw), WorkerID: fmt.Sprintf("id-%02d", i),
			WorkerName: fmt.Sprintf("w-%02d", i), Kind: "size_refused",
		})
	}
	return out
}

// #4518: with more refused sizes than the response keeps, every LISTED size is retained
// before any unlisted one, even when the unlisted ones sort first, and the cap holds.
func TestSelectSizeRefusalsListedFirstWithinCap(t *testing.T) {
	// The issue's contract, pinned as literals so raising a constant cannot move the test too.
	const wantSizes, wantPerSize = 16, 3
	byRaw := map[string][]apitypes.WorkerSizeRefusalDTO{}
	var listed []string
	for i := 0; i < wantSizes; i++ {
		name := fmt.Sprintf("z-%02d", i)
		listed = append(listed, name)
		byRaw[name] = refusalsFor(name, 5)
	}
	for i := 0; i < 4; i++ { // unlisted, sorting before every listed name
		name := fmt.Sprintf("a-%02d", i)
		byRaw[name] = refusalsFor(name, 1)
	}
	out, totals := selectSizeRefusals(byRaw, listed, refusalClean)
	if len(totals) != wantSizes {
		t.Fatalf("totals cover %d sizes, want %d: %v", len(totals), wantSizes, totals)
	}
	for _, name := range listed {
		if totals[name] != 5 {
			t.Errorf("totals[%s] = %d, want 5", name, totals[name])
		}
	}
	perSize := map[string]int{}
	for _, r := range out {
		perSize[r.Size]++
	}
	for size, n := range perSize {
		if _, ok := totals[size]; !ok {
			t.Errorf("refusal for %s has no total", size)
		}
		if n != wantPerSize {
			t.Errorf("%s has %d entries, want %d", size, n, wantPerSize)
		}
	}
	if len(out) != wantSizes*wantPerSize {
		t.Errorf("len(out) = %d, want %d", len(out), wantSizes*wantPerSize)
	}
	if out[0].Size != "z-00" || out[0].WorkerName != "w-00" || out[2].WorkerName != "w-02" {
		t.Errorf("out not sorted by size then worker name: first %+v, third %+v", out[0], out[2])
	}
}

// An unlisted size fills free slots, but never one whose sanitized key is a listed name
// (refused or not) or an already retained key, so one raw value never adds to another's total.
func TestSelectSizeRefusalsUnlistedCollisionsSkipped(t *testing.T) {
	const bell = "\a"
	if refusalClean("m"+bell) != "m" || refusalClean("x"+bell) != "x" {
		t.Fatalf("precondition: sanitizing must strip the control character (got %q, %q)",
			refusalClean("m"+bell), refusalClean("x"+bell))
	}
	byRaw := map[string][]apitypes.WorkerSizeRefusalDTO{
		"m" + bell: refusalsFor("m"+bell, 2), // unlisted, collides with listed "m" (no refusals of its own)
		"x":        refusalsFor("x", 4),      // unlisted
		"x" + bell: refusalsFor("x"+bell, 7), // unlisted, collides with retained "x"
		"l":        refusalsFor("l", 1),      // listed
	}
	out, totals := selectSizeRefusals(byRaw, []string{"l", "m", "s"}, refusalClean)
	want := map[string]int{"l": 1, "x": 4}
	if len(totals) != len(want) || totals["l"] != 1 || totals["x"] != 4 {
		t.Fatalf("totals = %v, want %v", totals, want)
	}
	for _, r := range out {
		if r.Size == "m" {
			t.Fatalf("listed size m with no refusals of its own shows %+v from an unlisted raw value", r)
		}
	}
	if len(out) != 1+3 {
		t.Fatalf("len(out) = %d, want %d: %+v", len(out), 1+3, out)
	}
}

// Unlisted sizes fill the slots left after the listed ones strictly by name: with 15 listed
// refused sizes, only the first unlisted name by sort order gets the one free slot.
func TestSelectSizeRefusalsUnlistedByName(t *testing.T) {
	byRaw := map[string][]apitypes.WorkerSizeRefusalDTO{}
	var listed []string
	for i := 0; i < 15; i++ {
		name := fmt.Sprintf("z-%02d", i)
		listed = append(listed, name)
		byRaw[name] = refusalsFor(name, 1)
	}
	for _, name := range []string{"u-c", "u-a", "u-d", "u-b"} {
		byRaw[name] = refusalsFor(name, 2)
	}
	for run := 0; run < 20; run++ { // map order varies per run; the result must not
		_, totals := selectSizeRefusals(byRaw, listed, refusalClean)
		if len(totals) != 16 || totals["u-a"] != 2 || totals["u-b"] != 0 || totals["u-c"] != 0 || totals["u-d"] != 0 {
			t.Fatalf("run %d: totals = %v, want the 15 listed sizes plus u-a only", run, totals)
		}
	}
}

// Nothing refused: both results are empty and non-nil (they marshal as [] and {}).
func TestSelectSizeRefusalsEmpty(t *testing.T) {
	out, totals := selectSizeRefusals(map[string][]apitypes.WorkerSizeRefusalDTO{}, []string{"s"}, refusalClean)
	if out == nil || totals == nil || len(out) != 0 || len(totals) != 0 {
		t.Fatalf("got %v, %v; want empty non-nil", out, totals)
	}
}
