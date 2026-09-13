package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1255 M5 seam tests for the PR drill-in (viewPR), the pulls-row actions and the run-view
// cross-link. The model is driven in process (Update ← message, View → string) with a FakeClient,
// exactly like the pulls tests (tui_pulls_test.go).

// ---- fixtures + helpers ---------------------------------------------------

// prLinkedRunID is the run the sample PR fixtures link (RunID), so u / w have a target.
const prLinkedRunID = "f8064ef5-1111-2222-3333-444444444444"

func ckOf(name, status, conclusion string) apitypes.CheckDTO {
	return apitypes.CheckDTO{Name: name, Status: status, Conclusion: conclusion,
		StartedAt: time.Now().Add(-5 * time.Minute), CompletedAt: time.Now().Add(-1 * time.Minute)}
}
func ckPassed(name string) apitypes.CheckDTO { return ckOf(name, "completed", "success") }
func ckFailed(name string) apitypes.CheckDTO { return ckOf(name, "completed", "failure") }
func ckRunning(name string) apitypes.CheckDTO {
	c := ckOf(name, "in_progress", "")
	c.CompletedAt = time.Time{}
	return c
}
func ckSkipped(name string) apitypes.CheckDTO { return ckOf(name, "completed", "skipped") }

// prDetail builds a PR drill-in fixture linked to prLinkedRunID (so the run cross-link / w exist).
func prDetail(iid int64, decision string, checks []apitypes.CheckDTO, reviews []apitypes.PullReviewDTO, merge apitypes.MergeStateDTO) apitypes.PullDetailDTO {
	now := time.Now()
	return apitypes.PullDetailDTO{
		PullDTO: apitypes.PullDTO{IID: iid, Title: "A pull request", Author: "uzi-bot",
			SourceBranch: "agent/issue-1246", TargetBranch: "main", ReviewDecision: decision,
			WebURL: "https://github.com/vtmocanu/uzi/pull/" + itoa(int(iid)), RunID: sp(prLinkedRunID),
			Additions: 842, Deletions: 131, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-3 * time.Minute)},
		Checks: checks, Reviews: reviews, Merge: merge,
	}
}

// openPRWith drives a fresh model to a loaded PR drill-in the real way: repos load, the user opens
// the pulls list, the list lands, enter opens the PR view (minting startPRReq), and the detail
// reply is applied. The startPRReq command is discarded by press (no GetPull is executed), so the
// fake's GetPull counters stay clean for the poll-guard tests that drive the ticks themselves.
func openPRWith(t *testing.T, fake *uzicli.FakeClient, detail apitypes.PullDetailDTO) tuiModel {
	t.Helper()
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(reposMsg{repos: fake.Repos})
	m = next.(tuiModel)
	m = press(t, m, keyViewPulls)
	next, _ = m.Update(pullsMsg{reqID: m.pulls.waitID, pulls: []apitypes.PullDTO{detail.PullDTO}})
	m = next.(tuiModel)
	m = press(t, m, keyEnter)
	if m.view != viewPR {
		t.Fatalf("enter on a pulls row did not open the PR view (view=%v)", m.view)
	}
	next, _ = m.Update(prMsg{reqID: m.pr.waitID, gen: m.pr.gen, detail: detail})
	return next.(tuiModel)
}

func prTick(t *testing.T, m tuiModel) (tuiModel, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(prTickMsg{gen: m.pr.tickGen})
	return next.(tuiModel), cmd
}

// feedResults drains a command and feeds any prMsg / prActionMsg it produced back through Update,
// returning the updated model. The fetch/action closures call the FakeClient, so draining is what
// exercises the real client path.
func feedResults(t *testing.T, m tuiModel, cmd tea.Cmd) tuiModel {
	t.Helper()
	for _, msg := range drainCmd(cmd) {
		switch msg.(type) {
		case prMsg, prActionMsg:
			next, _ := m.Update(msg)
			m = next.(tuiModel)
		}
	}
	return m
}

// ---- header rollups (D3) --------------------------------------------------

func TestTUIPRHeaderRollups(t *testing.T) {
	cases := []struct {
		name   string
		detail apitypes.PullDetailDTO
		want   string
	}{
		{"checks-pending", prDetail(1, "review_required",
			[]apitypes.CheckDTO{ckRunning("a"), ckPassed("b"), ckPassed("c")}, nil, apitypes.MergeStateDTO{}),
			"● 1 pending · ✓ 2/3"},
		{"all-green", prDetail(2, "approved",
			[]apitypes.CheckDTO{ckPassed("a"), ckPassed("b")}, nil, apitypes.MergeStateDTO{}),
			"✓ 2/2"},
		{"all-green-changes-requested", prDetail(3, "changes_requested",
			[]apitypes.CheckDTO{ckPassed("a"), ckPassed("b")}, nil, apitypes.MergeStateDTO{}),
			"✓ 2/2 · ✎ changes requested"},
		{"failing", prDetail(4, "review_required",
			[]apitypes.CheckDTO{ckFailed("a"), ckPassed("b")}, nil, apitypes.MergeStateDTO{}),
			"✗ 1 failing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
			m := openPRWith(t, fake, tc.detail)
			frame := stripANSI(m.View().Content)
			if !strings.Contains(frame, tc.want) {
				t.Errorf("rollup: want %q in the header\n%s", tc.want, frame)
			}
			// The `● live · 5s` cadence tag and the `re-polled Ns ago` proof are always present.
			if !strings.Contains(frame, "● live · 5s") {
				t.Errorf("the live-cadence tag is not drawn\n%s", frame)
			}
			if !strings.Contains(frame, "re-polled") {
				t.Errorf("the re-polled proof is not drawn\n%s", frame)
			}
		})
	}
}

// TestTUIPRHeaderLine2KeepsDiffstatWithLongTitle pins the SHOULD-FIX: a real-world long title used
// to push the right-hand `−dels` count off the hard m.width clamp at the standard 100 cols, losing
// the deletions. The diffstat is now right-anchored, so the title truncates and the `−dels` survives.
func TestTUIPRHeaderLine2KeepsDiffstatWithLongTitle(t *testing.T) {
	detail := prDetail(55, "review_required", []apitypes.CheckDTO{ckPassed("a")}, nil, apitypes.MergeStateDTO{})
	detail.Title = "Completion interlock across M4 through M6: rollout switch plus permit finalize step"
	if n := len([]rune(detail.Title)); n < 60 {
		t.Fatalf("the fixture title must be ≥60 runes to reproduce the clamp (got %d)", n)
	}
	detail.Additions = 842
	detail.Deletions = 131

	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := openPRWith(t, fake, detail)
	m.width = 100
	frame := stripANSI(m.View().Content)
	if !strings.Contains(frame, "−131") {
		t.Errorf("the deletions diffstat (−131) was dropped at width 100 with a long title\n%s", frame)
	}
}

// TestTUIPRRendersAtNarrowWidthsWithoutPanic pins BLOCKING fix #1: a non-empty BlockedReason, an
// https check URL and a coarse mergeable-state each drove a width-derived renderer.Plain cap to ≤0,
// underflowing capCell and panicking the whole TUI at terminal width ≤14 (and ≤6). With the floors
// and the capCell guard, every width degrades gracefully — no panic, and no rendered line overflows
// m.width. (Pre-fix this panics at width 14 with `slice bounds out of range [:-1]`.)
func TestTUIPRRendersAtNarrowWidthsWithoutPanic(t *testing.T) {
	merge := apitypes.MergeStateDTO{Conflicts: bp(true), RequiredChecksPassed: false,
		MergeableState: "blocked", BlockedReason: "changes requested by a required reviewer"}
	failing := ckFailed("a-very-long-check-name-that-needs-clamping")
	failing.WebURL = "https://github.com/vtmocanu/uzi/actions/runs/1234567890/job/9876543210"
	failing.Description = "the lint job found a problem in a source file"
	checks := []apitypes.CheckDTO{failing, ckPassed("build")}
	reviews := []apitypes.PullReviewDTO{
		{Author: "a-reviewer-with-a-fairly-long-login", State: "changes_requested", SubmittedAt: time.Now().Add(-time.Minute)}}
	detail := prDetail(42, "changes_requested", checks, reviews, merge)

	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := openPRWith(t, fake, detail)

	for _, width := range []int{100, 60, 40, 20, 14, 6, 1} {
		m.width, m.height = width, 40
		content := m.View().Content // must not panic at any width
		for i, line := range strings.Split(content, "\n") {
			if w := visualWidth(line); w > width {
				t.Errorf("width %d: rendered line %d is %d cols wide (exceeds %d): %q",
					width, i, w, width, stripANSI(line))
			}
		}
	}
}

// ---- CHECKS sort + cursor + selected URL (D3/D9) --------------------------

func TestTUIPRChecksSortAndSelectedURL(t *testing.T) {
	// Unordered input; the view must present failing → pending → passed → skipped.
	failing := ckFailed("failcheck")
	failing.WebURL = "https://github.com/vtmocanu/uzi/checks/fail"
	running := ckRunning("runcheck")
	running.WebURL = "javascript:alert(1)" // must NEVER become an OSC-8 link target
	checks := []apitypes.CheckDTO{ckPassed("passcheck"), failing, running, ckSkipped("skipcheck")}
	detail := prDetail(5, "review_required", checks, nil, apitypes.MergeStateDTO{})

	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := openPRWith(t, fake, detail)

	frame := stripANSI(m.View().Content)
	iFail := strings.Index(frame, "failcheck")
	iRun := strings.Index(frame, "runcheck")
	iPass := strings.Index(frame, "passcheck")
	iSkip := strings.Index(frame, "skipcheck")
	ordered := iFail >= 0 && iFail < iRun && iRun < iPass && iPass < iSkip
	if !ordered {
		t.Errorf("checks not sorted failing→pending→passed→skipped: fail=%d run=%d pass=%d skip=%d\n%s",
			iFail, iRun, iPass, iSkip, frame)
	}

	// Cursor starts at 0 = the failing check (https): its ↗ URL line emits an OSC-8 envelope.
	raw := m.View().Content
	if !strings.Contains(raw, "\x1b]8;;https://github.com/vtmocanu/uzi/checks/fail") {
		t.Errorf("the selected https check URL was not emitted as an OSC-8 hyperlink\n%s", raw)
	}

	// Move the cursor to the pending check (javascript: URL): its ↗ text is drawn, but NO OSC-8.
	m = press(t, m, keyDown)
	raw = m.View().Content
	if strings.Contains(raw, "\x1b]8;;") {
		t.Errorf("a non-https check URL was emitted as an OSC-8 hyperlink; only https may be linked\n%s", raw)
	}
	if !strings.Contains(stripANSI(raw), "↗ javascript:alert(1)") {
		t.Errorf("the selected check's URL text is not drawn for copying\n%s", stripANSI(raw))
	}
}

// ---- poll guards (D4) -----------------------------------------------------

func TestTUIPRTickInFlightGuard(t *testing.T) {
	orig := prPollInterval
	prPollInterval = time.Millisecond
	t.Cleanup(func() { prPollInterval = orig })

	detail := prDetail(6, "review_required", []apitypes.CheckDTO{ckRunning("a")}, nil, apitypes.MergeStateDTO{})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullDetailResult: detail}
	m := openPRWith(t, fake, detail)
	if m.pr.waitID != 0 {
		t.Fatalf("the PR reply did not clear the guard (waitID=%d)", m.pr.waitID)
	}
	if fake.GetPullCalls != 0 {
		t.Fatalf("no fetch closure has been executed yet, want 0 got %d", fake.GetPullCalls)
	}

	// First tick from idle: one fetch, guard latched.
	m, cmd := prTick(t, m)
	if m.pr.waitID == 0 {
		t.Fatal("the first PR tick did not latch the in-flight guard")
	}
	drainCmd(cmd)
	if fake.GetPullCalls != 1 {
		t.Fatalf("the first tick issued %d GetPull, want exactly 1", fake.GetPullCalls)
	}
	if fake.LastGetPullIID != 6 || fake.LastGetPullRepoID != "r1" {
		t.Fatalf("the fetch targeted repo %q iid %d, want r1/6", fake.LastGetPullRepoID, fake.LastGetPullIID)
	}

	// Second tick while the poll is in flight: no second fetch.
	m, cmd = prTick(t, m)
	drainCmd(cmd)
	if fake.GetPullCalls != 1 {
		t.Fatalf("a tick while a poll was in flight issued another GetPull (total %d)", fake.GetPullCalls)
	}

	// The reply clears the guard, so the next tick fetches again.
	next, _ := m.Update(prMsg{reqID: m.pr.waitID, gen: m.pr.gen, detail: detail})
	m = next.(tuiModel)
	if m.pr.waitID != 0 {
		t.Fatal("the PR reply did not clear the guard")
	}
	m, cmd = prTick(t, m)
	drainCmd(cmd)
	if fake.GetPullCalls != 2 {
		t.Fatalf("after a reply cleared the guard, the next tick brought GetPull to %d, want 2", fake.GetPullCalls)
	}
}

func TestTUIPRStaleReplyDropped(t *testing.T) {
	orig := prPollInterval
	prPollInterval = time.Millisecond
	t.Cleanup(func() { prPollInterval = orig })

	detail := prDetail(7, "review_required", []apitypes.CheckDTO{ckRunning("a")}, nil, apitypes.MergeStateDTO{})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullDetailResult: detail}
	m := openPRWith(t, fake, detail)

	m, _ = prTick(t, m)
	waitID := m.pr.waitID
	if waitID == 0 {
		t.Fatal("the tick did not mint a PR request")
	}

	other := prDetail(7, "approved", []apitypes.CheckDTO{ckPassed("x")}, nil, apitypes.MergeStateDTO{})
	next, _ := m.Update(prMsg{reqID: waitID + 999, detail: other})
	m = next.(tuiModel)
	if m.pr.waitID != waitID {
		t.Fatalf("a stale reply cleared the guard: waitID=%d, want %d", m.pr.waitID, waitID)
	}
	if m.pr.detail.ReviewDecision == "approved" {
		t.Fatal("a stale reply applied its detail")
	}
}

// TestTUIPRReopenStaleReplyDropped reproduces the CROSS-PR collision: newPRState resets reqSeq on
// every open, so a prior PR's still-in-flight reply mints the SAME reqID as the freshly-opened PR
// and passes the reqID==waitID guard. Without the monotonic session gen, PR A's late detail
// (Title/Checks/RunID) would be applied to the PR B view — the header still reads #B, but w then
// reworks A's run and u opens A's run, an action against the wrong run. The prGen guard drops it.
func TestTUIPRReopenStaleReplyDropped(t *testing.T) {
	const (
		runA = "aaaaaaaa-1111-2222-3333-444444444444"
		runB = "bbbbbbbb-1111-2222-3333-444444444444"
	)
	prA := prDetail(100, "review_required", []apitypes.CheckDTO{ckRunning("a")}, nil, apitypes.MergeStateDTO{})
	prA.RunID = sp(runA)
	prB := prDetail(200, "review_required", []apitypes.CheckDTO{ckRunning("b")}, nil, apitypes.MergeStateDTO{})
	prB.RunID = sp(runB)

	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(reposMsg{repos: fake.Repos})
	m = next.(tuiModel)

	// Open PR A via the pulls enter path and leave its GetPull in flight (no reply fed).
	m = press(t, m, keyViewPulls)
	next, _ = m.Update(pullsMsg{reqID: m.pulls.waitID, pulls: []apitypes.PullDTO{prA.PullDTO}})
	m = next.(tuiModel)
	m = press(t, m, keyEnter)
	if m.view != viewPR || m.pr.iid != 100 {
		t.Fatalf("enter did not open PR A (view=%v iid=%d)", m.view, m.pr.iid)
	}
	aWait, aGen := m.pr.waitID, m.pr.gen
	if aWait == 0 {
		t.Fatal("opening PR A did not mint an in-flight request")
	}

	// esc back to the pulls list, swap the row to PR B, and open it: newPRState resets reqSeq, so B
	// mints the SAME reqID as A's still-in-flight request (the collision), under a NEW gen.
	m = press(t, m, keyEsc)
	if m.view != viewPulls {
		t.Fatalf("esc did not return to the pulls list (view=%v)", m.view)
	}
	next, _ = m.Update(pullsMsg{reqID: m.pulls.waitID, pulls: []apitypes.PullDTO{prB.PullDTO}})
	m = next.(tuiModel)
	m = press(t, m, keyEnter)
	if m.view != viewPR || m.pr.iid != 200 {
		t.Fatalf("enter did not open PR B (view=%v iid=%d)", m.view, m.pr.iid)
	}
	if m.pr.waitID != aWait {
		t.Fatalf("the reopen did not mint the colliding reqID (B waitID=%d, A waitID=%d)", m.pr.waitID, aWait)
	}
	if m.pr.gen == aGen {
		t.Fatalf("the reopen did not advance the PR session generation (gen still %d)", aGen)
	}

	// PR A's late reply arrives while B's request is in flight: reqID collides with B's waitID, but
	// the session gen does not — it MUST be dropped, never applied to the B view.
	next, _ = m.Update(prMsg{reqID: aWait, gen: aGen, detail: prA})
	m = next.(tuiModel)
	if m.pr.detail.RunID != nil && *m.pr.detail.RunID == runA {
		t.Fatalf("PR A's stale reply was applied to the PR B view (RunID=%s)", runA)
	}
	if m.pr.waitID != aWait {
		t.Fatalf("the dropped stale reply cleared B's in-flight guard (waitID=%d, want %d)", m.pr.waitID, aWait)
	}

	// B's own reply (matching gen) is still honoured, so the view loads B's run.
	next, _ = m.Update(prMsg{reqID: m.pr.waitID, gen: m.pr.gen, detail: prB})
	m = next.(tuiModel)
	if m.pr.detail.RunID == nil || *m.pr.detail.RunID != runB {
		t.Fatalf("PR B's own reply was not applied (RunID=%v, want %s)", m.pr.detail.RunID, runB)
	}
	if m.pr.iid != 200 {
		t.Fatalf("the PR view is no longer scoped to B (iid=%d)", m.pr.iid)
	}
}

// TestTUIPRReqIDGuardDropsStaleSameSessionReply pins the reqID defence INDEPENDENTLY of the gen
// guard. Within ONE PR session (same gen) two polls can be in flight at once — an earlier poll's
// reply can land after a newer poll was minted — and both carry the SAME session gen, so the gen
// guard (TestTUIPRReopenStaleReplyDropped) can never tell them apart: only reqID == waitID does.
// This test delivers a reply carrying the CURRENT gen (so the gen guard does NOT fire) but an
// earlier in-session reqID, and asserts it is dropped — the whole point being that removing the
// reqID!=waitID clause from the prMsg handler (keeping only the gen guard) reddens exactly here
// while TestTUIPRStaleReplyDropped / the reopen test stay green on gen alone.
func TestTUIPRReqIDGuardDropsStaleSameSessionReply(t *testing.T) {
	orig := prPollInterval
	prPollInterval = time.Millisecond
	t.Cleanup(func() { prPollInterval = orig })

	const staleRunID = "deadbeef-1111-2222-3333-444444444444"
	cur := prDetail(77, "review_required", []apitypes.CheckDTO{ckRunning("current-check")}, nil, apitypes.MergeStateDTO{})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullDetailResult: cur}
	m := openPRWith(t, fake, cur)

	// The open's first reply has landed (gen stamped, guard idle). A tick mints a SECOND in-session
	// poll under the SAME gen: reqSeq advances (≠0) so startPRReq does NOT bump gen.
	sessionGen := m.pr.gen
	m, _ = prTick(t, m)
	waitID := m.pr.waitID
	if waitID == 0 {
		t.Fatal("the tick did not mint an in-session PR request")
	}
	if m.pr.gen != sessionGen {
		t.Fatalf("a same-session re-poll advanced the session gen (gen=%d, want %d)", m.pr.gen, sessionGen)
	}

	// A stale EARLIER in-session poll replies late: reqID is a prior poll's id (≠ waitID) but the gen
	// is the current session's, so the gen guard does NOT catch it — only the reqID clause can.
	staleReqID := waitID - 1
	if staleReqID == waitID {
		t.Fatalf("the stale reqID must differ from waitID to exercise the reqID guard (both %d)", waitID)
	}
	stale := prDetail(77, "approved", []apitypes.CheckDTO{ckPassed("stale-check")}, nil, apitypes.MergeStateDTO{})
	stale.Title = "stale in-session reply"
	stale.RunID = sp(staleRunID)
	next, _ := m.Update(prMsg{reqID: staleReqID, gen: sessionGen, detail: stale})
	m = next.(tuiModel)

	// The reply was dropped: the in-flight guard is untouched AND the view still shows the current
	// session's detail (RunID / Title / Checks), never the stale reply's.
	if m.pr.waitID != waitID {
		t.Fatalf("a same-session stale reply cleared the in-flight guard: waitID=%d, want %d", m.pr.waitID, waitID)
	}
	if m.pr.detail.ReviewDecision == "approved" || m.pr.detail.Title == "stale in-session reply" {
		t.Fatalf("a same-session stale reply applied its detail (decision=%q title=%q)",
			m.pr.detail.ReviewDecision, m.pr.detail.Title)
	}
	if m.pr.detail.RunID == nil || *m.pr.detail.RunID != prLinkedRunID {
		t.Fatalf("the stale reply overwrote the current RunID (got %v, want %s)", m.pr.detail.RunID, prLinkedRunID)
	}
	if len(m.pr.detail.Checks) != 1 || m.pr.detail.Checks[0].Name != "current-check" {
		t.Fatalf("the stale reply overwrote the current session's checks (%+v)", m.pr.detail.Checks)
	}
}

func TestTUIPRErrStreakBacksOff(t *testing.T) {
	orig := prPollInterval
	prPollInterval = time.Millisecond
	t.Cleanup(func() { prPollInterval = orig })

	detail := prDetail(8, "review_required", []apitypes.CheckDTO{ckRunning("a")}, nil, apitypes.MergeStateDTO{})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullDetailResult: detail}
	m := openPRWith(t, fake, detail)

	pollErr := uzicli.Exitf(uzicli.ExitGeneric, "context deadline exceeded")
	for want := 1; want <= 3; want++ {
		m, _ = prTick(t, m)
		if m.pr.waitID == 0 {
			t.Fatalf("tick %d did not mint a PR request", want)
		}
		next, _ := m.Update(prMsg{reqID: m.pr.waitID, gen: m.pr.gen, err: pollErr})
		m = next.(tuiModel)
		if m.pr.errStreak != want {
			t.Fatalf("after %d error replies errStreak = %d, want %d", want, m.pr.errStreak, want)
		}
	}
	if prTickInterval(m.pr.errStreak) <= prPollInterval {
		t.Fatalf("the backoff did not grow past the base %v", prPollInterval)
	}

	// A success reply resets the streak.
	m, _ = prTick(t, m)
	next, _ := m.Update(prMsg{reqID: m.pr.waitID, gen: m.pr.gen, detail: detail})
	m = next.(tuiModel)
	if m.pr.errStreak != 0 {
		t.Fatalf("a success reply left errStreak = %d, want 0", m.pr.errStreak)
	}
}

// TestTUIPRLivePollFlipsHeader scripts a live sequence (pending → settled) through GetPullHook and
// asserts the CHECKS heading flips from "in progress" to "all checks passed".
func TestTUIPRLivePollFlipsHeader(t *testing.T) {
	orig := prPollInterval
	prPollInterval = time.Millisecond
	t.Cleanup(func() { prPollInterval = orig })

	pending := prDetail(9, "review_required", []apitypes.CheckDTO{ckRunning("a"), ckPassed("b")}, nil, apitypes.MergeStateDTO{})
	settled := prDetail(9, "approved", []apitypes.CheckDTO{ckPassed("a"), ckPassed("b")}, nil, apitypes.MergeStateDTO{})
	calls := 0
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	fake.GetPullHook = func(string, int64) (apitypes.PullDetailDTO, error) {
		calls++
		if calls == 1 {
			return pending, nil
		}
		return settled, nil
	}

	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(reposMsg{repos: fake.Repos})
	m = next.(tuiModel)
	m = press(t, m, keyViewPulls)
	next, _ = m.Update(pullsMsg{reqID: m.pulls.waitID, pulls: []apitypes.PullDTO{pending.PullDTO}})
	m = next.(tuiModel)

	// Open via handleKey so we can drain the open fetch (which calls the hook) rather than discard it.
	nm, cmd := m.handleKey(keyEnter)
	m = nm.(tuiModel)
	m = feedResults(t, m, cmd) // hook call 1 → pending
	if frame := stripANSI(m.View().Content); !strings.Contains(frame, "in progress") {
		t.Fatalf("the first live poll did not show checks in progress\n%s", frame)
	}

	// A tick fetches again → hook call 2 → settled; the header flips.
	m, cmd = prTick(t, m)
	m = feedResults(t, m, cmd)
	if frame := stripANSI(m.View().Content); !strings.Contains(frame, "all checks passed") {
		t.Fatalf("the header did not flip to settled after the second live poll\n%s", frame)
	}
	if calls < 2 {
		t.Fatalf("the live sequence did not exercise GetPullHook twice (calls=%d)", calls)
	}
}

// ---- actions (D1/D12) -----------------------------------------------------

func TestTUIPRReworkSuccess(t *testing.T) {
	detail := prDetail(10, "changes_requested", []apitypes.CheckDTO{ckPassed("a")}, nil, apitypes.MergeStateDTO{})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()},
		RunByID:   map[string]apitypes.RunDTO{prLinkedRunID: {ID: prLinkedRunID, Status: "completed"}},
		ReworkRun: apitypes.RunDTO{ID: "99999999-aaaa-bbbb-cccc-dddddddddddd"}}
	m := openPRWith(t, fake, detail)

	nm, cmd := m.handleKey(keyRework)
	m = feedResults(t, nm.(tuiModel), cmd)
	if fake.LastReworkRunID != prLinkedRunID {
		t.Fatalf("RunRework was not called with the linked run id (got %q)", fake.LastReworkRunID)
	}
	if fake.LastReworkGuidance != "" {
		t.Fatalf("RunRework guidance should be empty (got %q)", fake.LastReworkGuidance)
	}
	frame := stripANSI(m.View().Content)
	if !strings.Contains(frame, "queued rework") || !strings.Contains(frame, "99999999") {
		t.Errorf("the rework success confirmation is not drawn inline\n%s", frame)
	}
}

func TestTUIPRReworkConflict(t *testing.T) {
	detail := prDetail(11, "changes_requested", []apitypes.CheckDTO{ckPassed("a")}, nil, apitypes.MergeStateDTO{})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()},
		RunReworkErr: uzicli.Exitf(uzicli.ExitConflict, "run is not completed")}
	m := openPRWith(t, fake, detail)

	nm, cmd := m.handleKey(keyRework)
	m = feedResults(t, nm.(tuiModel), cmd)
	if fake.LastReworkRunID != prLinkedRunID {
		t.Fatalf("RunRework was not reached with the linked run id (got %q)", fake.LastReworkRunID)
	}
	if frame := stripANSI(m.View().Content); !strings.Contains(frame, "run is not completed") {
		t.Errorf("the server 409 reason is not drawn inline\n%s", frame)
	}
}

func TestTUIPRFixCISuccess(t *testing.T) {
	detail := prDetail(12, "review_required", []apitypes.CheckDTO{ckFailed("a")}, nil, apitypes.MergeStateDTO{})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()},
		CIFixRunResult: apitypes.RunDTO{ID: "cccccccc-1111-2222-3333-444444444444"}}
	m := openPRWith(t, fake, detail)

	nm, cmd := m.handleKey(keyFixCI)
	m = feedResults(t, nm.(tuiModel), cmd)
	if fake.LastCIFixRef != "agent/issue-1246" {
		t.Fatalf("CreateCIFixRun was not called with the PR's head branch (got %q)", fake.LastCIFixRef)
	}
	if fake.LastCIFixRepoID != "r1" {
		t.Fatalf("CreateCIFixRun was not scoped to the repo (got %q)", fake.LastCIFixRepoID)
	}
	if frame := stripANSI(m.View().Content); !strings.Contains(frame, "queued fix ci") {
		t.Errorf("the fix-ci confirmation is not drawn inline\n%s", frame)
	}
}

func TestTUIPRFixCIConflict(t *testing.T) {
	detail := prDetail(13, "review_required", []apitypes.CheckDTO{ckFailed("a")}, nil, apitypes.MergeStateDTO{})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()},
		CreateCIFixRunErr: uzicli.Exitf(uzicli.ExitConflict, "ref is not a watched failed ref")}
	m := openPRWith(t, fake, detail)

	nm, cmd := m.handleKey(keyFixCI)
	m = feedResults(t, nm.(tuiModel), cmd)
	if fake.LastCIFixRef != "agent/issue-1246" {
		t.Fatalf("CreateCIFixRun was not reached with the head branch (got %q)", fake.LastCIFixRef)
	}
	if frame := stripANSI(m.View().Content); !strings.Contains(frame, "ref is not a watched failed ref") {
		t.Errorf("the 409 reason is not drawn inline\n%s", frame)
	}
}

// u / w are hidden and inert when the PR has no linked run.
func TestTUIPRRunActionsGatedOnRunID(t *testing.T) {
	detail := prDetail(14, "approved", []apitypes.CheckDTO{ckPassed("a")}, nil, apitypes.MergeStateDTO{})
	detail.RunID = nil
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := openPRWith(t, fake, detail)

	frame := stripANSI(m.View().Content)
	if strings.Contains(frame, "u run") || strings.Contains(frame, "w rework") {
		t.Errorf("u / w were shown in the legend with no linked run\n%s", frame)
	}
	if !strings.Contains(frame, "f fix ci") {
		t.Errorf("f fix ci should still be offered with no linked run\n%s", frame)
	}
	// u is a no-op (stays in the PR view, issues no command).
	nm, cmd := m.handleKey(keyRunLink)
	if nm.(tuiModel).view != viewPR || cmd != nil {
		t.Errorf("u with no linked run should be a no-op (view=%v cmd=%v)", nm.(tuiModel).view, cmd)
	}
	// w issues no command.
	if _, cmd := m.handleKey(keyRework); cmd != nil {
		t.Error("w with no linked run should issue no command")
	}
	if fake.LastReworkRunID != "" {
		t.Errorf("w reached RunRework with no linked run (got %q)", fake.LastReworkRunID)
	}
}

// ---- cross-link provenance round trips (D1) -------------------------------

func TestTUIPRProvenanceRoundTrips(t *testing.T) {
	detail := prDetail(15, "changes_requested", []apitypes.CheckDTO{ckPassed("a")}, nil, apitypes.MergeStateDTO{})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullDetailResult: detail,
		PullsResult: []apitypes.PullDTO{detail.PullDTO}}

	// pulls → enter → PR → esc → pulls
	m := openPRWith(t, fake, detail)
	m = press(t, m, keyEsc)
	if m.view != viewPulls {
		t.Fatalf("PR esc did not return to the pulls list (view=%v)", m.view)
	}

	// pulls → u → detail → esc → pulls  (the run link on the selected row)
	m = press(t, m, keyRunLink)
	if m.view != viewDetail {
		t.Fatalf("u on a pulls row did not open the run detail (view=%v)", m.view)
	}
	if m.detailReturn != viewPulls {
		t.Fatalf("detailReturn was not set to pulls (got %v)", m.detailReturn)
	}
	m = press(t, m, keyEsc)
	if m.view != viewPulls {
		t.Fatalf("run-view esc did not return to the pulls list (view=%v)", m.view)
	}

	// pulls → enter → PR → u → detail → esc → PR → esc → pulls
	m = openPRWith(t, fake, detail)
	m = press(t, m, keyRunLink)
	if m.view != viewDetail {
		t.Fatalf("u on the PR view did not open the run detail (view=%v)", m.view)
	}
	if m.detailReturn != viewPR {
		t.Fatalf("detailReturn was not set to the PR view (got %v)", m.detailReturn)
	}
	m = press(t, m, keyEsc)
	if m.view != viewPR {
		t.Fatalf("run-view esc did not return to the PR view (view=%v)", m.view)
	}
	m = press(t, m, keyEsc)
	if m.view != viewPulls {
		t.Fatalf("PR esc did not return to the pulls list (view=%v)", m.view)
	}
}

// detail → m → PR → esc → detail → esc → board, and the run view is not clobbered.
func TestTUIDetailMToPRRoundTrip(t *testing.T) {
	runID := "abcd1234-1111-2222-3333-444444444444"
	run := apitypes.RunDTO{ID: runID, Status: "running", RepoID: sp("r1"), MrIID: ip(1254), IssueTitle: "a run"}
	detail := prDetail(1254, "approved", []apitypes.CheckDTO{ckPassed("a")}, nil, apitypes.MergeStateDTO{})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullDetailResult: detail}

	m := tuiTestModel(t, fake, runID)
	m = applyDetail(m, run, nil)
	if m.view != viewDetail {
		t.Fatal("the --run start session is not in the detail view")
	}
	// The detail footer advertises `m pr` because the run has an MR.
	if !strings.Contains(stripANSI(m.View().Content), "m pr") {
		t.Errorf("the detail footer does not advertise m when the run has an MR\n%s", stripANSI(m.View().Content))
	}

	m = press(t, m, keyPRView)
	if m.view != viewPR {
		t.Fatalf("m did not open the PR view (view=%v)", m.view)
	}
	if m.prReturn != viewDetail {
		t.Fatalf("prReturn was not set to the run view (got %v)", m.prReturn)
	}
	next, _ := m.Update(prMsg{reqID: m.pr.waitID, gen: m.pr.gen, detail: detail})
	m = next.(tuiModel)

	m = press(t, m, keyEsc)
	if m.view != viewDetail {
		t.Fatalf("PR esc did not return to the run view (view=%v)", m.view)
	}
	if m.detail.run.ID != runID {
		t.Fatalf("the run view was clobbered by the PR round trip (runID=%q)", m.detail.run.ID)
	}
	m = press(t, m, keyEsc)
	if m.view != viewBoard {
		t.Fatalf("run-view esc did not return to the board (view=%v)", m.view)
	}
}

// m is a no-op (and absent from the legend) when the run has no merge request.
func TestTUIDetailMNoOpWithoutMR(t *testing.T) {
	run := apitypes.RunDTO{ID: "run-x", Status: "running", IssueTitle: "a run"} // MrIID nil
	m := tuiTestModel(t, &uzicli.FakeClient{}, "run-x")
	m = applyDetail(m, run, nil)
	if strings.Contains(stripANSI(m.View().Content), "m pr") {
		t.Errorf("the detail footer advertised m with no merge request\n%s", stripANSI(m.View().Content))
	}
	nm, cmd := m.handleKey(keyPRView)
	if nm.(tuiModel).view != viewDetail || cmd != nil {
		t.Errorf("m with no merge request should be a no-op (view=%v cmd=%v)", nm.(tuiModel).view, cmd)
	}
}

// The PR view is a drill-in (no `/` filter), so q quits and ? opens help from it (D13).
func TestTUIPRQuitAndHelpNotFiltered(t *testing.T) {
	detail := prDetail(17, "approved", []apitypes.CheckDTO{ckPassed("a")}, nil, apitypes.MergeStateDTO{})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := openPRWith(t, fake, detail)
	if m.filtering() {
		t.Fatal("the PR view reported itself as a filter screen")
	}
	if hm := press(t, m, keyHelp); !hm.showHelp {
		t.Error("? did not open help from the PR view")
	}
	if _, cmd := m.handleKey(keyQuit); cmd == nil {
		t.Error("q from the PR view did not quit")
	}
}

// ---- Ascii presence (D8) --------------------------------------------------

func TestTUIPRAsciiProfileCarriesGlyphsAndWords(t *testing.T) {
	detail := prDetail(16, "changes_requested",
		[]apitypes.CheckDTO{ckFailed("lint"), ckRunning("test"), ckPassed("build"), ckSkipped("deploy")},
		[]apitypes.PullReviewDTO{{Author: "coderabbitai", State: "changes_requested", SubmittedAt: time.Now().Add(-time.Minute)}},
		apitypes.MergeStateDTO{Conflicts: bp(true), RequiredChecksPassed: false, BlockedReason: "changes requested"})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := openPRWith(t, fake, detail)

	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	frame := stripANSI(m.View().Content)
	for _, want := range []string{
		"CHECKS", "REVIEWS", "MERGE", // section labels
		"✗", "●", "✓", "·", "⚠", // per-state glyphs
		"failing", "passed", "pending", // rollup / heading words
		"failed", "running", "skipped", // per-check state words (forgeState)
		"changes requested", // review word + blocked reason
		"conflicts with",    // merge conflicts word
		"waiting on checks", // required-checks word
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("the Ascii PR frame is missing %q (glyph/word must survive colour stripping — D8)\n%s", want, frame)
		}
	}
}

// ---- D7 per-field hostile-value render test -------------------------------

func TestTUIPRStripsControlBytesPerField(t *testing.T) {
	// Hostile bytes at the FRONT so a sanitized tail survives truncation; distinct tails prove each
	// render path ran. Bidi override + ESC/BEL/control + a newline that would forge a row.
	const (
		titleN = "\x1b[2J\u202e\x07\x01prtitle"
		srcN   = "\x1b[2J\u202e\x07\x01prsrc\n  FORGED"
		tgtN   = "\x1b[2J\u202e\x07\x01prtgt"
		authN  = "\x1b[2J\u202e\x07\x01prauth"
		ckName = "\x1b[2J\u202e\x07\x01ckname"
		ckDesc = "\x1b[2J\u202e\x07\x01ckdesc"
		rvAuth = "\x1b[2J\u202e\x07\x01rvauthor"
		rvSt   = "\x1b[2J\u202e\x07\x01rvstate"
		blockN = "\x1b[2J\u202e\x07\x01blockreason"
	)
	detail := apitypes.PullDetailDTO{
		PullDTO: apitypes.PullDTO{IID: 42, Title: titleN, Author: authN, SourceBranch: srcN, TargetBranch: tgtN,
			ReviewDecision: "changes_requested", Conflicts: bp(true),
			WebURL: "https://github.com/vtmocanu/uzi/pull/42", RunID: sp(prLinkedRunID),
			CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now().Add(-time.Minute)},
		Checks: []apitypes.CheckDTO{{Name: ckName, Description: ckDesc, Status: "completed", Conclusion: "failure",
			WebURL: "javascript:alert(1)", StartedAt: time.Now().Add(-5 * time.Minute), CompletedAt: time.Now().Add(-time.Minute)}},
		Reviews: []apitypes.PullReviewDTO{{Author: rvAuth, State: rvSt, SubmittedAt: time.Now().Add(-time.Minute)}},
		Merge:   apitypes.MergeStateDTO{Conflicts: bp(true), BlockedReason: blockN},
	}
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := openPRWith(t, fake, detail)

	out := m.View().Content
	assertNoRawControls(t, "pr", out)
	for _, want := range []string{"prtitle", "prsrc", "prtgt", "prauth", "ckname", "ckdesc", "rvauthor", "rvstate", "blockreason"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the PR view is not drawing a forge field (missing %q), so this test does not exercise that path\n%s", want, out)
		}
	}
	if strings.Contains(out, "\n  FORGED") {
		t.Errorf("a forge branch newline forged a row in the frame\n%s", out)
	}
	// The non-https check URL emitted NO OSC-8 envelope.
	if strings.Contains(out, "\x1b]8;;") {
		t.Errorf("a non-https check URL was emitted as an OSC-8 hyperlink\n%s", out)
	}

	// The MergeableState field (drawn only when no blocked reason AND nothing else explains the
	// merge state, i.e. required checks passed) is sanitized on its own path.
	const mergeableN = "\x1b[2J\u202e\x07\x01mergeablestate"
	detail2 := detail
	detail2.Merge = apitypes.MergeStateDTO{Conflicts: bp(false), RequiredChecksPassed: true, MergeableState: mergeableN}
	m2 := openPRWith(t, fake, detail2)
	out2 := m2.View().Content
	assertNoRawControls(t, "pr merge state", out2)
	if !strings.Contains(out2, "mergeablestate") {
		t.Fatalf("the PR view is not drawing MergeableState, so this test does not exercise that path\n%s", out2)
	}
}
