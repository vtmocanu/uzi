package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1255 M4a seam tests for the `pulls` list screen. The model is driven in process
// (Update ← message, View → string) with a FakeClient, exactly like the board tests.

func oneRepo() apitypes.RepoDTO {
	return apitypes.RepoDTO{ID: "r1", PathWithNamespace: "vtmocanu/uzi", Enabled: true,
		WebURL: "https://github.com/vtmocanu/uzi"}
}

// samplePulls is a small realistic fixture spanning the three bands and every row state.
func samplePulls(now time.Time) []apitypes.PullDTO {
	return []apitypes.PullDTO{
		// NEEDS YOU — changes requested.
		{IID: 1254, Title: "Completion interlock rollout switch", Author: "uzi-bot",
			SourceBranch: "agent/issue-1246", TargetBranch: "main", ReviewDecision: "changes_requested",
			Conflicts: bp(false), WebURL: "https://github.com/vtmocanu/uzi/pull/1254",
			RunID: sp("f8064ef5-1111-2222-3333-444444444444"), UpdatedAt: now.Add(-2 * time.Hour)},
		// NEEDS YOU — conflicts (approved but conflicting with the target).
		{IID: 1258, Title: "Portable mktemp on BSD", Author: "alice",
			SourceBranch: "fix/1234-mktemp", TargetBranch: "main", ReviewDecision: "approved",
			Conflicts: bp(true), WebURL: "https://github.com/vtmocanu/uzi/pull/1258",
			UpdatedAt: now.Add(-24 * time.Hour)},
		// IN FLIGHT — review pending.
		{IID: 1257, Title: "Run merged-signal coverage", Author: "bob",
			SourceBranch: "agent/issue-1253", TargetBranch: "main", ReviewDecision: "review_required",
			Conflicts: bp(false), WebURL: "https://github.com/vtmocanu/uzi/pull/1257",
			RunID: sp("9c672af9-1111-2222-3333-444444444444"), UpdatedAt: now.Add(-12 * time.Minute)},
		// IN FLIGHT — draft.
		{IID: 1260, Title: "WIP spike on the cache layer", Author: "carol",
			SourceBranch: "wip/cache-spike", TargetBranch: "main", Draft: true, ReviewDecision: "none",
			Conflicts: bp(false), WebURL: "https://github.com/vtmocanu/uzi/pull/1260",
			UpdatedAt: now.Add(-5 * time.Minute)},
		// READY — approved, no conflicts.
		{IID: 1249, Title: "Bump golangci-lint", Author: "renovate",
			SourceBranch: "renovate/golangci-lint", TargetBranch: "main", ReviewDecision: "approved",
			Conflicts: bp(false), WebURL: "https://github.com/vtmocanu/uzi/pull/1249",
			UpdatedAt: now.Add(-3 * time.Hour)},
	}
}

// loadedPulls drives a fresh model to a loaded pulls screen: the repos reply lands, the user
// navigates to the pulls screen (which mints a fetch), and the pulls reply is applied.
func loadedPulls(t *testing.T, fake *uzicli.FakeClient, pulls []apitypes.PullDTO) tuiModel {
	t.Helper()
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(reposMsg{repos: fake.Repos})
	m = next.(tuiModel)
	m = press(t, m, keyViewPulls)
	if m.view != viewPulls {
		t.Fatalf("keyViewPulls did not switch to the pulls screen (view=%v)", m.view)
	}
	next, _ = m.Update(pullsMsg{reqID: m.pulls.waitID, pulls: pulls})
	return next.(tuiModel)
}

func pullsTick(t *testing.T, m tuiModel) (tuiModel, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(pullsTickMsg{gen: m.pulls.tickGen})
	return next.(tuiModel), cmd
}

// assertPullStatesPresent asserts the band names and every per-state glyph + word are present
// in the (ANSI-stripped) frame — the carriers that must survive a colour-stripped profile (D8).
func assertPullStatesPresent(t *testing.T, where, frame string) {
	t.Helper()
	for _, want := range []string{
		"NEEDS YOU", "IN FLIGHT", "READY", // band eyebrows
		"✎", "changes", // changes requested
		"⚠",             // conflicts
		"✓", "approved", // approved / ready
		"●", "review", // review pending
		"draft", // draft
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("%s: pulls frame does not carry %q (glyph/word must survive colour stripping — D8)\n%s", where, want, frame)
		}
	}
}

// (a) Bands render in order with the correct glyph + word per state.
func TestTUIPullsBandsAndGlyphs(t *testing.T) {
	now := time.Now()
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullsResult: samplePulls(now)}
	m := loadedPulls(t, fake, samplePulls(now))

	frame := stripANSI(m.View().Content)
	assertPullStatesPresent(t, "default profile", frame)

	// The bands render top-to-bottom: NEEDS YOU → IN FLIGHT → READY.
	iNeeds := strings.Index(frame, "NEEDS YOU")
	iFlight := strings.Index(frame, "IN FLIGHT")
	iReady := strings.Index(frame, "READY")
	if iNeeds < 0 || iNeeds >= iFlight || iFlight >= iReady {
		t.Errorf("bands out of order: NEEDS YOU=%d IN FLIGHT=%d READY=%d\n%s", iNeeds, iFlight, iReady, frame)
	}
	// The header names the scoped repo and a summary cluster over all 5 open PRs.
	if !strings.Contains(frame, "vtmocanu/uzi") {
		t.Errorf("header does not name the scoped repo\n%s", frame)
	}
	if !strings.Contains(frame, "5 open") {
		t.Errorf("summary does not count the open PRs\n%s", frame)
	}
	// The `↳ run` link shows for a PR a uzi run opened.
	if !strings.Contains(frame, "↳ f8064ef5") {
		t.Errorf("the run link is not drawn for a PR with a linked run\n%s", frame)
	}
}

// (c) Under the Ascii profile the glyph + word for each state are still present (not merely
// "no SGR escapes"): colour is never the only carrier (D8).
func TestTUIPullsAsciiProfileCarriesGlyphAndWord(t *testing.T) {
	now := time.Now()
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullsResult: samplePulls(now)}
	m := loadedPulls(t, fake, samplePulls(now))

	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	assertPullStatesPresent(t, "ascii profile", stripANSI(m.View().Content))
}

// (b1) A tick while a pulls request is in flight issues NO second ListPulls; a reply clears
// the guard so the next tick fetches again.
func TestTUIPullsTickInFlightGuard(t *testing.T) {
	orig := pullsPollInterval
	pullsPollInterval = time.Millisecond
	t.Cleanup(func() { pullsPollInterval = orig })

	now := time.Now()
	pulls := samplePulls(now)
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullsResult: pulls}
	m := loadedPulls(t, fake, pulls)
	if m.pulls.waitID != 0 {
		t.Fatalf("the pulls reply did not clear the guard (waitID=%d)", m.pulls.waitID)
	}
	if fake.ListPullsCalls != 0 {
		t.Fatalf("no fetch closure has been executed yet, want 0 got %d", fake.ListPullsCalls)
	}

	// First tick from idle: issues one fetch and latches the guard.
	m, cmd := pullsTick(t, m)
	if m.pulls.waitID == 0 {
		t.Fatal("the first pulls tick did not latch the in-flight guard")
	}
	drainCmd(cmd)
	if fake.ListPullsCalls != 1 {
		t.Fatalf("the first tick issued %d ListPulls, want exactly 1", fake.ListPullsCalls)
	}
	if fake.LastListPullsRepoID != "r1" {
		t.Fatalf("the fetch targeted repo %q, want r1", fake.LastListPullsRepoID)
	}

	// Second tick while the poll is in flight: must NOT issue another fetch.
	m, cmd = pullsTick(t, m)
	drainCmd(cmd)
	if fake.ListPullsCalls != 1 {
		t.Fatalf("a tick while a poll was in flight issued another ListPulls (total %d)", fake.ListPullsCalls)
	}

	// The reply clears the guard, so the next tick fetches again.
	next, _ := m.Update(pullsMsg{reqID: m.pulls.waitID, pulls: pulls})
	m = next.(tuiModel)
	if m.pulls.waitID != 0 {
		t.Fatal("the pulls reply did not clear the guard")
	}
	m, cmd = pullsTick(t, m)
	drainCmd(cmd)
	if fake.ListPullsCalls != 2 {
		t.Fatalf("after a reply cleared the guard, the next tick brought the total to %d, want 2", fake.ListPullsCalls)
	}
}

// (b2) A stale reply (reqID != waitID) is dropped whole: it neither clears the guard nor
// applies its pulls.
func TestTUIPullsStaleReplyDropped(t *testing.T) {
	now := time.Now()
	pulls := samplePulls(now)
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullsResult: pulls}
	m := loadedPulls(t, fake, pulls)

	m, _ = pullsTick(t, m) // mint a fresh outstanding request
	waitID := m.pulls.waitID
	if waitID == 0 {
		t.Fatal("the tick did not mint a pulls request")
	}
	before := len(m.pulls.pulls)

	other := []apitypes.PullDTO{{IID: 9999, Title: "stale", ReviewDecision: "none", UpdatedAt: now}}
	next, _ := m.Update(pullsMsg{reqID: waitID + 999, pulls: other})
	m = next.(tuiModel)
	if m.pulls.waitID != waitID {
		t.Fatalf("a stale reply cleared the guard: waitID=%d, want %d", m.pulls.waitID, waitID)
	}
	if len(m.pulls.pulls) != before {
		t.Fatalf("a stale reply applied its pulls (now %d rows, was %d)", len(m.pulls.pulls), before)
	}
}

// (b3) The error streak grows on each consecutive failed poll and the reschedule interval
// backs off; a success resets both.
func TestTUIPullsErrStreakBacksOff(t *testing.T) {
	now := time.Now()
	pulls := samplePulls(now)
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullsResult: pulls}
	m := loadedPulls(t, fake, pulls)

	pollErr := uzicli.Exitf(uzicli.ExitGeneric, "context deadline exceeded")
	for want := 1; want <= 3; want++ {
		m, _ = pullsTick(t, m) // mint a request
		if m.pulls.waitID == 0 {
			t.Fatalf("tick %d did not mint a pulls request", want)
		}
		next, _ := m.Update(pullsMsg{reqID: m.pulls.waitID, err: pollErr})
		m = next.(tuiModel)
		if m.pulls.errStreak != want {
			t.Fatalf("after %d error replies errStreak = %d, want %d", want, m.pulls.errStreak, want)
		}
		if got, exp := pullsTickInterval(m.pulls.errStreak), pullsTickInterval(want); got != exp {
			t.Fatalf("interval at streak %d = %v, want %v", want, got, exp)
		}
	}
	if pullsTickInterval(m.pulls.errStreak) <= pullsPollInterval {
		t.Fatalf("the backoff did not grow past the base %v", pullsPollInterval)
	}

	// A success reply resets the streak → interval back to base.
	m, _ = pullsTick(t, m)
	next, _ := m.Update(pullsMsg{reqID: m.pulls.waitID, pulls: pulls})
	m = next.(tuiModel)
	if m.pulls.errStreak != 0 {
		t.Fatalf("a success reply left errStreak = %d, want 0", m.pulls.errStreak)
	}
}

// (rate-limit) A forge 429 (ExitUnreachable + Retry-After) renders the `~ rate-limited ·
// retry in Ns` header state, not a generic error line.
func TestTUIPullsRateLimitedHeader(t *testing.T) {
	now := time.Now()
	pulls := samplePulls(now)
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullsResult: pulls}
	m := loadedPulls(t, fake, pulls)

	m, _ = pullsTick(t, m)
	rl := &uzicli.ExitError{Code: uzicli.ExitUnreachable, Err: errors.New("forge rate limit hit"),
		RetryAfter: 30 * time.Second}
	next, _ := m.Update(pullsMsg{reqID: m.pulls.waitID, err: rl})
	m = next.(tuiModel)
	if !m.pulls.rateLimited {
		t.Fatal("a 429 reply did not set the rate-limited header state")
	}
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "rate-limited") || !strings.Contains(out, "retry in 30s") {
		t.Errorf("the rate-limit header state is not drawn\n%s", out)
	}
}

// (d) Every forge-authored field is sanitized before it reaches the frame, and a non-https
// WebURL emits no OSC-8 hyperlink envelope (D7/D9).
func TestTUIPullsStripsControlBytesAndNonHTTPSLinks(t *testing.T) {
	now := time.Now()
	// Hostile bytes at the FRONT so a tail survives truncation; distinct tails prove each
	// render path ran. A bidi override + ESC/BEL/SO control runes + a newline that would forge
	// a row.
	const titleN = "\x1b[2J\u202e\x07\x01safetitle"
	const srcN = "\x1b[2J\u202e\x07\x01srcbranch\n  FORGED"
	const tgtN = "\x1b[2J\u202e\x07\x01tgtbranch"
	const authN = "\x1b[2J\u202e\x07\x01author"
	hostile := []apitypes.PullDTO{{
		IID: 42, Title: titleN, Author: authN, SourceBranch: srcN, TargetBranch: tgtN,
		ReviewDecision: "changes_requested", Conflicts: bp(true),
		// A non-https (javascript:) URL must NEVER become an OSC-8 link target.
		WebURL:    "javascript:alert(1)",
		RunID:     sp("aaaaaaaa-1111-2222-3333-444444444444"),
		UpdatedAt: now.Add(-1 * time.Minute),
	}}
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullsResult: hostile}
	m := loadedPulls(t, fake, hostile)

	out := m.View().Content
	assertNoRawControls(t, "pulls", out)

	// The sanitized tails survive, proving the row + second-line render paths ran (the title,
	// source branch and — on the selected row's conflicts note — the target branch).
	for _, want := range []string{"safetitle", "srcbranch", "tgtbranch"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the pulls screen is not drawing a forge field (missing %q), so this test does not exercise that path\n%s", want, out)
		}
	}
	// The forged-row newline did not survive into the frame as a real break + attacker text.
	if strings.Contains(out, "\n  FORGED") {
		t.Errorf("a forge branch newline forged a row in the frame\n%s", out)
	}
	// The non-https WebURL emitted NO OSC-8 hyperlink envelope.
	if strings.Contains(out, "\x1b]8;;") {
		t.Errorf("a non-https WebURL was emitted as an OSC-8 hyperlink; only https forge URLs may be linked\n%s", out)
	}
}

// (d-https) An https WebURL IS emitted as an OSC-8 hyperlink when links are enabled — the
// positive control so the non-https assertion above is not vacuous.
func TestTUIPullsHTTPSWebURLIsLinked(t *testing.T) {
	now := time.Now()
	pulls := []apitypes.PullDTO{{IID: 7, Title: "a pr", ReviewDecision: "approved",
		Conflicts: bp(false), WebURL: "https://github.com/vtmocanu/uzi/pull/7", UpdatedAt: now}}
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullsResult: pulls}
	m := loadedPulls(t, fake, pulls)
	// Default test profile is TrueColor, so links are enabled.
	if !strings.Contains(m.View().Content, "\x1b]8;;https://github.com/vtmocanu/uzi/pull/7") {
		t.Errorf("an https WebURL was not emitted as an OSC-8 hyperlink\n%s", m.View().Content)
	}
}

// (e) Typing `q` into the pulls filter appends it and does NOT quit (D13).
func TestTUIPullsFilterSwallowsQuit(t *testing.T) {
	now := time.Now()
	pulls := samplePulls(now)
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullsResult: pulls}
	m := loadedPulls(t, fake, pulls)

	m = press(t, m, keyFilter)
	if !m.pulls.filtering {
		t.Fatal("/ did not open the pulls filter")
	}
	next, cmd := m.handleKey(keyQuit) // "q"
	m = next.(tuiModel)
	if cmd != nil {
		t.Error("q while filtering returned a command; it must type into the filter, not quit")
	}
	if m.pulls.filter != "q" {
		t.Errorf("q was not typed into the pulls filter (filter=%q)", m.pulls.filter)
	}
	if strings.Contains(m.View().Content, "Quit uzi tui?") {
		t.Error("q while filtering rendered the quit modal; filter input must swallow it (D13)")
	}
}

// (D2) The default scoped repo is the newest non-chat board run's repo when it is in the
// enabled set; R then cycles the enabled repos.
func TestTUIPullsDefaultRepoAndCycle(t *testing.T) {
	now := time.Now()
	repoA := apitypes.RepoDTO{ID: "ra", PathWithNamespace: "org/alpha", Enabled: true, WebURL: "https://x/alpha"}
	repoB := apitypes.RepoDTO{ID: "rb", PathWithNamespace: "org/bravo", Enabled: true, WebURL: "https://x/bravo"}
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "old", Kind: "issue", Status: "running", RepoID: sp("ra"), CreatedAt: now.Add(-1 * time.Hour)}},
		{RunDTO: apitypes.RunDTO{ID: "new", Kind: "issue", Status: "running", RepoID: sp("rb"), CreatedAt: now.Add(-1 * time.Minute)}},
		// A chat run (nil RepoID) is newest of all and must be ignored by the default rule.
		{RunDTO: apitypes.RunDTO{ID: "chat", Kind: "chat", Status: "running", CreatedAt: now}},
	}
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{repoA, repoB}, Runs: runs}

	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: runs})
	m = next.(tuiModel)
	next, _ = m.Update(reposMsg{repos: fake.Repos})
	m = next.(tuiModel)
	m = press(t, m, keyViewPulls)

	// Default = repoB (newest non-chat run's repo).
	if m.repoIdx != 1 {
		t.Fatalf("default repo idx = %d, want 1 (the newest non-chat run's repo)", m.repoIdx)
	}
	if !strings.Contains(stripANSI(m.View().Content), "org/bravo") {
		t.Errorf("the header does not name the default repo org/bravo\n%s", stripANSI(m.View().Content))
	}

	// R cycles to the other enabled repo and persists the choice.
	m = press(t, m, keyRepoCycle)
	if m.repoIdx != 0 || !m.repoChosen {
		t.Fatalf("R did not cycle to the other repo (idx=%d chosen=%v)", m.repoIdx, m.repoChosen)
	}
	if !strings.Contains(stripANSI(m.View().Content), "org/alpha") {
		t.Errorf("after R the header does not name org/alpha\n%s", stripANSI(m.View().Content))
	}
}

// (empty) A resolved repo with no open PRs shows the centered "no open PRs" line, not a dead
// "loading…".
func TestTUIPullsEmptyState(t *testing.T) {
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullsResult: nil}
	m := loadedPulls(t, fake, nil)
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "no open PRs") {
		t.Errorf("an empty-but-loaded pulls screen should say \"no open PRs\"\n%s", out)
	}
}

// (nav) tab and esc move between the floor and the pulls screen (D1).
func TestTUIPullsTabAndEscNavigation(t *testing.T) {
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, PullsResult: nil}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(reposMsg{repos: fake.Repos})
	m = next.(tuiModel)

	// tab from the floor opens the pulls screen.
	m = press(t, m, keyTab)
	if m.view != viewPulls {
		t.Fatalf("tab from the floor did not open the pulls screen (view=%v)", m.view)
	}
	// esc returns to the floor.
	m = press(t, m, keyEsc)
	if m.view != viewBoard {
		t.Fatalf("esc on the pulls screen did not return to the floor (view=%v)", m.view)
	}
	// tab back, then tab again returns to the floor (ci lands in M4b).
	m = press(t, m, keyTab)
	m = press(t, m, keyTab)
	if m.view != viewBoard {
		t.Fatalf("tab from the pulls screen did not return to the floor (view=%v)", m.view)
	}
}
