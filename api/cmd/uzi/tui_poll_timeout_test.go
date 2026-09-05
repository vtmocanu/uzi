package main

import (
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1130 M2: each periodic poll derives its own short-lived context.WithTimeout(m.ctx,
// boardPollTimeout) so a stalled poll fails within ~10s instead of the shared 30s
// http.Client.Timeout (client.go, untouched — D3). These tests assert on CONTENT, not timing:
// the FakeClient captures the ctx it was handed and we assert it carries a deadline in the
// short-poll window. On UNFIXED code the captured ctx has NO deadline (ok == false), so the
// assertion fails deterministically with no blocking fake and no goroutine hang.

// The board poll (fetchRunsCmd) must hand ListRuns a ctx whose deadline is ≈ now +
// boardPollTimeout and strictly < 30s (proving it is the short poll bound, not the 30s client
// timeout). boardPollTimeout is shrunk under the test to make the window tight and deterministic.
func TestTUIBoardPollDerivesShortDeadline(t *testing.T) {
	origTimeout := boardPollTimeout
	boardPollTimeout = 3 * time.Second
	t.Cleanup(func() { boardPollTimeout = origTimeout })

	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-1111-2222-3333-444444444444", Kind: "issue", Status: "running"}},
	}
	fake := &uzicli.FakeClient{Runs: runs}
	m := tuiTestModel(t, fake, "")

	before := time.Now()
	// Execute the board fetch closure directly; this is what hands the fake its ctx.
	msg := m.fetchRunsCmd(false, 0)()
	if _, ok := msg.(boardRunsMsg); !ok {
		t.Fatalf("fetchRunsCmd produced %T, want boardRunsMsg", msg)
	}

	ctx := fake.LastListRunsCtx
	if ctx == nil {
		t.Fatal("ListRuns was handed a nil ctx; the board poll did not pass its derived context")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("the board poll's ctx has NO deadline; a stalled poll would only be bounded by the 30s client timeout")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		t.Fatalf("the board poll's ctx deadline is already past (remaining %v); the WithTimeout+cancel must not fire before the call", remaining)
	}
	// The deadline sits at ≈ before + boardPollTimeout; allow small slack for execution.
	if remaining > boardPollTimeout+time.Second {
		t.Fatalf("the board poll's ctx deadline is %v out, want <= boardPollTimeout (%v) + slack", remaining, boardPollTimeout)
	}
	if deadline.Before(before) {
		t.Fatalf("the board poll's ctx deadline %v is before the call started %v", deadline, before)
	}
	// The load-bearing assertion: strictly shorter than the 30s shared client timeout.
	if remaining >= 30*time.Second {
		t.Fatalf("the board poll's ctx has a %v deadline; it must be strictly < 30s, i.e. the short poll bound, not the client timeout", remaining)
	}
}

// The detail-meta poll (refreshRunMetaCmd) must hand GetRun the same short-lived deadline,
// symmetric to the board poll above.
func TestTUIDetailMetaPollDerivesShortDeadline(t *testing.T) {
	origTimeout := boardPollTimeout
	boardPollTimeout = 3 * time.Second
	t.Cleanup(func() { boardPollTimeout = origTimeout })

	runID := "aaaaaaaa-1111-2222-3333-444444444444"
	run := apitypes.RunDTO{ID: runID, Kind: "issue", Status: "running"}
	fake := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{runID: run}}
	m := tuiTestModel(t, fake, runID)

	before := time.Now()
	msg := m.refreshRunMetaCmd(runID, 0)()
	if _, ok := msg.(detailMetaMsg); !ok {
		t.Fatalf("refreshRunMetaCmd produced %T, want detailMetaMsg", msg)
	}

	ctx := fake.LastGetRunCtx
	if ctx == nil {
		t.Fatal("GetRun was handed a nil ctx; the detail-meta poll did not pass its derived context")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("the detail-meta poll's ctx has NO deadline; a stalled poll would only be bounded by the 30s client timeout")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		t.Fatalf("the detail-meta poll's ctx deadline is already past (remaining %v)", remaining)
	}
	if remaining > boardPollTimeout+time.Second {
		t.Fatalf("the detail-meta poll's ctx deadline is %v out, want <= boardPollTimeout (%v) + slack", remaining, boardPollTimeout)
	}
	if deadline.Before(before) {
		t.Fatalf("the detail-meta poll's ctx deadline %v is before the call started %v", deadline, before)
	}
	if remaining >= 30*time.Second {
		t.Fatalf("the detail-meta poll's ctx has a %v deadline; it must be strictly < 30s, the short poll bound", remaining)
	}
}
