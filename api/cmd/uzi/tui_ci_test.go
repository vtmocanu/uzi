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

// PRD #1255 M4b seam tests for the `ci` list screen, mirroring the pulls tests (tui_pulls_test.go):
// the model is driven in process (Update ← message, View → string) with a FakeClient.

// sampleCIRuns is a small realistic fixture spanning the three bands and every row state. It is
// also the uxlab `ci-populated` scene's fixture (uxlab_gen_test.go).
func sampleCIRuns(now time.Time) []apitypes.CIRunDTO {
	return []apitypes.CIRunDTO{
		// RUNNING — in_progress with jobs progress (the ▰▱ micro-bar shows 2/5).
		{ID: 1039, Name: "CI", Number: 1039, Event: "pull_request", Branch: "agent/issue-1246",
			SHA: "deadbeefcafef00d", Status: "in_progress", Title: "PRD #1226 continuation",
			Actor: "uzi-bot", WebURL: "https://github.com/vtmocanu/uzi/actions/runs/1039",
			StartedAt: now.Add(-2 * time.Minute), CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now,
			JobsDone: 2, JobsTotal: 5},
		// RUNNING — queued, no jobs progress yet (the right cell falls back to the age).
		{ID: 1195, Name: "CodeQL", Number: 1195, Event: "pull_request", Branch: "refs/pull/1254/head",
			SHA: "aa11bb22cc33", Status: "queued", Title: "PR #1254", Actor: "github",
			WebURL:    "https://github.com/vtmocanu/uzi/actions/runs/1195",
			CreatedAt: now.Add(-1 * time.Minute), UpdatedAt: now.Add(-30 * time.Second)},
		// FAILED — a terminal failure conclusion.
		{ID: 1037, Name: "CI", Number: 1037, Event: "pull_request", Branch: "agent/issue-1251",
			SHA: "ccdd1122ee44", Status: "completed", Conclusion: "failure", Title: "TUI andon surfaces",
			Actor: "alice", WebURL: "https://github.com/vtmocanu/uzi/actions/runs/1037",
			StartedAt: now.Add(-10 * time.Minute), CreatedAt: now.Add(-10 * time.Minute), UpdatedAt: now.Add(-5 * time.Minute)},
		// RECENT — success.
		{ID: 1038, Name: "CI", Number: 1038, Event: "push", Branch: "main",
			SHA: "ee33ff4488aa", Status: "completed", Conclusion: "success", Title: "docs(prd): create PRD",
			Actor: "bob", WebURL: "https://github.com/vtmocanu/uzi/actions/runs/1038",
			StartedAt: now.Add(-3*time.Hour - 4*time.Minute), CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-3 * time.Hour)},
		// RECENT — cancelled (neutral).
		{ID: 61, Name: "release", Number: 61, Event: "push", Branch: "v0.77.0",
			SHA: "1234abcd5678", Status: "completed", Conclusion: "cancelled", Title: "chore(release): 0.77.0",
			Actor: "carol", WebURL: "https://github.com/vtmocanu/uzi/actions/runs/61",
			StartedAt: now.Add(-48*time.Hour - 11*time.Minute), CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour)},
	}
}

// loadedCI drives a fresh model to a loaded ci screen: the repos reply lands, the user navigates
// to the ci screen (which mints a fetch), and the ci reply is applied.
func loadedCI(t *testing.T, fake *uzicli.FakeClient, runs []apitypes.CIRunDTO) tuiModel {
	t.Helper()
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(reposMsg{repos: fake.Repos})
	m = next.(tuiModel)
	m = press(t, m, keyViewCI)
	if m.view != viewCI {
		t.Fatalf("keyViewCI did not switch to the ci screen (view=%v)", m.view)
	}
	next, _ = m.Update(ciMsg{reqID: m.ci.waitID, runs: runs})
	return next.(tuiModel)
}

func ciTick(t *testing.T, m tuiModel) (tuiModel, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(ciTickMsg{gen: m.ci.tickGen})
	return next.(tuiModel), cmd
}

// assertCIStatesPresent asserts the band names, each per-state glyph + word, and the jobs
// micro-bar's done/total text are present in the (ANSI-stripped) frame — the carriers that must
// survive a colour-stripped profile (D8).
func assertCIStatesPresent(t *testing.T, where, frame string) {
	t.Helper()
	for _, want := range []string{
		"RUNNING", "FAILED", "RECENT", // band eyebrows
		"●", "running", // running glyph + word
		"✗", "failed", // failed glyph + word
		"✓", "passed", // success glyph + word
		"2/5", // the jobs micro-bar keeps its done/total text (D8)
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("%s: ci frame does not carry %q (glyph/word/bar-count must survive colour stripping — D8)\n%s", where, want, frame)
		}
	}
}

// (a) Bands render in order with the correct glyph + word per state.
func TestTUICIBandsAndGlyphs(t *testing.T) {
	now := time.Now()
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, CIRunsResult: sampleCIRuns(now)}
	m := loadedCI(t, fake, sampleCIRuns(now))

	frame := stripANSI(m.View().Content)
	assertCIStatesPresent(t, "default profile", frame)

	// The bands render top-to-bottom: RUNNING → FAILED → RECENT.
	iRunning := strings.Index(frame, "RUNNING")
	iFailed := strings.Index(frame, "FAILED")
	iRecent := strings.Index(frame, "RECENT")
	if iRunning < 0 || iRunning >= iFailed || iFailed >= iRecent {
		t.Errorf("bands out of order: RUNNING=%d FAILED=%d RECENT=%d\n%s", iRunning, iFailed, iRecent, frame)
	}
	// The header names the scoped repo and a summary cluster over all 5 runs.
	if !strings.Contains(frame, "vtmocanu/uzi") {
		t.Errorf("header does not name the scoped repo\n%s", frame)
	}
	if !strings.Contains(frame, "5 runs") {
		t.Errorf("summary does not count the CI runs\n%s", frame)
	}
}

// (c) Under the Ascii profile the glyph + word for each state AND the jobs micro-bar's done/total
// text are still present (not merely "no SGR escapes"): colour is never the only carrier (D8).
func TestTUICIAsciiProfileCarriesGlyphAndWord(t *testing.T) {
	now := time.Now()
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, CIRunsResult: sampleCIRuns(now)}
	m := loadedCI(t, fake, sampleCIRuns(now))

	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	assertCIStatesPresent(t, "ascii profile", stripANSI(m.View().Content))
}

// (b1) A tick while a ci request is in flight issues NO second ListCIRuns; a reply clears the
// guard so the next tick fetches again.
func TestTUICITickInFlightGuard(t *testing.T) {
	orig := ciPollInterval
	ciPollInterval = time.Millisecond
	t.Cleanup(func() { ciPollInterval = orig })

	now := time.Now()
	runs := sampleCIRuns(now)
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, CIRunsResult: runs}
	m := loadedCI(t, fake, runs)
	if m.ci.waitID != 0 {
		t.Fatalf("the ci reply did not clear the guard (waitID=%d)", m.ci.waitID)
	}
	if fake.ListCIRunsCalls != 0 {
		t.Fatalf("no fetch closure has been executed yet, want 0 got %d", fake.ListCIRunsCalls)
	}

	// First tick from idle: issues one fetch and latches the guard.
	m, cmd := ciTick(t, m)
	if m.ci.waitID == 0 {
		t.Fatal("the first ci tick did not latch the in-flight guard")
	}
	drainCmd(cmd)
	if fake.ListCIRunsCalls != 1 {
		t.Fatalf("the first tick issued %d ListCIRuns, want exactly 1", fake.ListCIRunsCalls)
	}
	if fake.LastCIRunsRepoID != "r1" {
		t.Fatalf("the fetch targeted repo %q, want r1", fake.LastCIRunsRepoID)
	}

	// Second tick while the poll is in flight: must NOT issue another fetch.
	m, cmd = ciTick(t, m)
	drainCmd(cmd)
	if fake.ListCIRunsCalls != 1 {
		t.Fatalf("a tick while a poll was in flight issued another ListCIRuns (total %d)", fake.ListCIRunsCalls)
	}

	// The reply clears the guard, so the next tick fetches again.
	next, _ := m.Update(ciMsg{reqID: m.ci.waitID, runs: runs})
	m = next.(tuiModel)
	if m.ci.waitID != 0 {
		t.Fatal("the ci reply did not clear the guard")
	}
	m, cmd = ciTick(t, m)
	drainCmd(cmd)
	if fake.ListCIRunsCalls != 2 {
		t.Fatalf("after a reply cleared the guard, the next tick brought the total to %d, want 2", fake.ListCIRunsCalls)
	}
}

// (b2) A stale reply (reqID != waitID) is dropped whole: it neither clears the guard nor applies
// its runs.
func TestTUICIStaleReplyDropped(t *testing.T) {
	now := time.Now()
	runs := sampleCIRuns(now)
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, CIRunsResult: runs}
	m := loadedCI(t, fake, runs)

	m, _ = ciTick(t, m) // mint a fresh outstanding request
	waitID := m.ci.waitID
	if waitID == 0 {
		t.Fatal("the tick did not mint a ci request")
	}
	before := len(m.ci.runs)

	other := []apitypes.CIRunDTO{{ID: 9999, Number: 9999, Name: "stale", Status: "completed", UpdatedAt: now}}
	next, _ := m.Update(ciMsg{reqID: waitID + 999, runs: other})
	m = next.(tuiModel)
	if m.ci.waitID != waitID {
		t.Fatalf("a stale reply cleared the guard: waitID=%d, want %d", m.ci.waitID, waitID)
	}
	if len(m.ci.runs) != before {
		t.Fatalf("a stale reply applied its runs (now %d rows, was %d)", len(m.ci.runs), before)
	}
}

// (b3) The error streak grows on each consecutive failed poll and the reschedule interval backs
// off; a success resets both.
func TestTUICIErrStreakBacksOff(t *testing.T) {
	now := time.Now()
	runs := sampleCIRuns(now)
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, CIRunsResult: runs}
	m := loadedCI(t, fake, runs)

	pollErr := uzicli.Exitf(uzicli.ExitGeneric, "context deadline exceeded")
	for want := 1; want <= 3; want++ {
		m, _ = ciTick(t, m) // mint a request
		if m.ci.waitID == 0 {
			t.Fatalf("tick %d did not mint a ci request", want)
		}
		next, _ := m.Update(ciMsg{reqID: m.ci.waitID, err: pollErr})
		m = next.(tuiModel)
		if m.ci.errStreak != want {
			t.Fatalf("after %d error replies errStreak = %d, want %d", want, m.ci.errStreak, want)
		}
		if got, exp := ciTickInterval(m.ci.errStreak), ciTickInterval(want); got != exp {
			t.Fatalf("interval at streak %d = %v, want %v", want, got, exp)
		}
	}
	if ciTickInterval(m.ci.errStreak) <= ciPollInterval {
		t.Fatalf("the backoff did not grow past the base %v", ciPollInterval)
	}

	// A success reply resets the streak → interval back to base.
	m, _ = ciTick(t, m)
	next, _ := m.Update(ciMsg{reqID: m.ci.waitID, runs: runs})
	m = next.(tuiModel)
	if m.ci.errStreak != 0 {
		t.Fatalf("a success reply left errStreak = %d, want 0", m.ci.errStreak)
	}
}

// (rate-limit) A forge 429 (ExitUnreachable + Retry-After) renders the `~ rate-limited · retry in
// Ns` header state, not a generic error line.
func TestTUICIRateLimitedHeader(t *testing.T) {
	now := time.Now()
	runs := sampleCIRuns(now)
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, CIRunsResult: runs}
	m := loadedCI(t, fake, runs)

	m, _ = ciTick(t, m)
	rl := &uzicli.ExitError{Code: uzicli.ExitUnreachable, Err: errors.New("forge rate limit hit"),
		RetryAfter: 30 * time.Second}
	next, _ := m.Update(ciMsg{reqID: m.ci.waitID, err: rl})
	m = next.(tuiModel)
	if !m.ci.rateLimited {
		t.Fatal("a 429 reply did not set the rate-limited header state")
	}
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "rate-limited") || !strings.Contains(out, "retry in 30s") {
		t.Errorf("the rate-limit header state is not drawn\n%s", out)
	}
}

// (d) Every forge-authored field is sanitized before it reaches the frame, and a non-https WebURL
// emits no OSC-8 hyperlink envelope (D7/D9).
func TestTUICIStripsControlBytesAndNonHTTPSLinks(t *testing.T) {
	now := time.Now()
	// Hostile bytes at the FRONT so a tail survives truncation; distinct tails prove each render
	// path ran. A bidi override + ESC/BEL/SO control runes + a newline that would forge a row.
	const nameN = "\x1b[2J\u202e\x07\x01cirun"
	const eventN = "\x1b[2J\u202e\x07\x01evt"
	const branchN = "\x1b[2J\u202e\x07\x01brnch\n  FORGED"
	const shaN = "\x1b[2J\u202e\x07\x01shaval"
	const titleN = "\x1b[2J\u202e\x07\x01ttl"
	const actorN = "\x1b[2J\u202e\x07\x01actr"
	hostile := []apitypes.CIRunDTO{{
		ID: 1, Number: 42, Name: nameN, Event: eventN, Branch: branchN, SHA: shaN,
		Status: "in_progress", Title: titleN, Actor: actorN,
		// A non-https (javascript:) URL must NEVER become an OSC-8 link target.
		WebURL: "javascript:alert(1)", JobsDone: 1, JobsTotal: 3,
		CreatedAt: now.Add(-1 * time.Minute), UpdatedAt: now,
	}}
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, CIRunsResult: hostile}
	m := loadedCI(t, fake, hostile)

	out := m.View().Content
	assertNoRawControls(t, "ci", out)

	// The sanitized tails survive, proving the row + second-line render paths ran (name/event/
	// branch/status/title on the row; sha/actor/title on the selected row's second line).
	for _, want := range []string{"cirun", "evt", "brnch", "shaval", "ttl", "actr"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the ci screen is not drawing a forge field (missing %q), so this test does not exercise that path\n%s", want, out)
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

// (d-https) An https WebURL IS emitted as an OSC-8 hyperlink when links are enabled — the positive
// control so the non-https assertion above is not vacuous.
func TestTUICIHTTPSWebURLIsLinked(t *testing.T) {
	now := time.Now()
	runs := []apitypes.CIRunDTO{{ID: 7, Number: 7, Name: "CI", Status: "completed", Conclusion: "success",
		WebURL: "https://github.com/vtmocanu/uzi/actions/runs/7", CreatedAt: now, UpdatedAt: now}}
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, CIRunsResult: runs}
	m := loadedCI(t, fake, runs)
	// Default test profile is TrueColor, so links are enabled.
	if !strings.Contains(m.View().Content, "\x1b]8;;https://github.com/vtmocanu/uzi/actions/runs/7") {
		t.Errorf("an https WebURL was not emitted as an OSC-8 hyperlink\n%s", m.View().Content)
	}
}

// (unsupported) A forge version without the runs endpoint degrades to a non-empty sentence
// (ErrForgeVersionUnsupported); the screen shows it verbatim and does not crash.
func TestTUICIUnsupportedDegrade(t *testing.T) {
	const sentence = "CI runs need Forgejo v16.0.0 or newer on this connection."
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, CIRunsUnsupported: sentence}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(reposMsg{repos: fake.Repos})
	m = next.(tuiModel)
	m = press(t, m, keyViewCI)
	next, _ = m.Update(ciMsg{reqID: m.ci.waitID, unsupported: sentence})
	m = next.(tuiModel)
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "CI runs need Forgejo v16.0.0 or newer") {
		t.Errorf("the unsupported degrade sentence is not drawn\n%s", out)
	}
}

// (empty) A resolved repo with no runs shows the left-aligned, sentence-case guiding line naming
// the scoped repo (consistent with the sibling empty states), not a dead "loading…".
func TestTUICIEmptyState(t *testing.T) {
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, CIRunsResult: nil}
	m := loadedCI(t, fake, nil)
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "No CI runs yet on vtmocanu/uzi.") {
		t.Errorf("an empty-but-loaded ci screen should name the scoped repo in the empty state\n%s", out)
	}
}

// (e) Typing `q` into the ci filter appends it and does NOT quit (D13).
func TestTUICIFilterSwallowsQuit(t *testing.T) {
	now := time.Now()
	runs := sampleCIRuns(now)
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, CIRunsResult: runs}
	m := loadedCI(t, fake, runs)

	m = press(t, m, keyFilter)
	if !m.ci.filtering {
		t.Fatal("/ did not open the ci filter")
	}
	next, cmd := m.handleKey(keyQuit) // "q"
	m = next.(tuiModel)
	if cmd != nil {
		t.Error("q while filtering returned a command; it must type into the filter, not quit")
	}
	if m.ci.filter != "q" {
		t.Errorf("q was not typed into the ci filter (filter=%q)", m.ci.filter)
	}
	if strings.Contains(m.View().Content, "Quit uzi tui?") {
		t.Error("q while filtering rendered the quit modal; filter input must swallow it (D13)")
	}
}

// (nav) 3 jumps to the ci screen from the floor, R cycles the scoped repo, and esc returns to the
// floor (D1, D2).
func TestTUICINavAndRepoCycle(t *testing.T) {
	now := time.Now()
	repoA := apitypes.RepoDTO{ID: "ra", PathWithNamespace: "org/alpha", Enabled: true, WebURL: "https://x/alpha"}
	repoB := apitypes.RepoDTO{ID: "rb", PathWithNamespace: "org/bravo", Enabled: true, WebURL: "https://x/bravo"}
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{repoA, repoB}, CIRunsResult: sampleCIRuns(now)}

	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(reposMsg{repos: fake.Repos})
	m = next.(tuiModel)

	// 3 from the floor opens the ci screen (the tab strip marks it active via m.view).
	m = press(t, m, keyViewCI)
	if m.view != viewCI {
		t.Fatalf("3 from the floor did not open the ci screen (view=%v)", m.view)
	}
	before := m.repoIdx

	// R cycles the scoped repo and persists the choice (shared with the pulls screen).
	m = press(t, m, keyRepoCycle)
	if m.repoIdx == before || !m.repoChosen {
		t.Fatalf("R did not cycle the scoped repo (idx=%d chosen=%v)", m.repoIdx, m.repoChosen)
	}

	// esc returns to the floor.
	m = press(t, m, keyEsc)
	if m.view != viewBoard {
		t.Fatalf("esc on the ci screen did not return to the floor (view=%v)", m.view)
	}
}

// (self-heal) A transient ListRepos failure at launch has no other retry path, so the shared repo
// scope — and the ci screen — would stay stuck on "could not load repositories" for the whole TUI
// session. Being on the ci screen must retry the repos fetch on the ~10s tick until it succeeds:
// the tick re-issues fetchReposCmd while repos are not ready, and a later success resolves the
// default repo so the ci fetch proceeds (mirrors the pulls self-heal test).
func TestTUICIRepoScopeSelfHealsAfterTransientFailure(t *testing.T) {
	orig := ciPollInterval
	ciPollInterval = time.Millisecond // the tick re-arm is a tea.Tick drainCmd must not block on
	t.Cleanup(func() { ciPollInterval = orig })

	now := time.Now()
	runs := sampleCIRuns(now)
	// fake.Repos is what the SECOND (recovered) ListRepos returns; the first reply is injected as
	// an error message below, so the fake's static ListRepos models only the recovery.
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, CIRunsResult: runs}

	m := tuiTestModel(t, fake, "")
	m = press(t, m, keyViewCI)
	if m.view != viewCI {
		t.Fatalf("keyViewCI did not switch to the ci screen (view=%v)", m.view)
	}

	// First repos reply FAILS: the screen shows the repos-error scope state, and the in-flight
	// guard is released so the tick can retry.
	next, _ := m.Update(reposMsg{err: uzicli.Exitf(uzicli.ExitUnreachable, "forge unreachable")})
	m = next.(tuiModel)
	if m.reposErr == nil || m.reposReady() {
		t.Fatal("the failed repos reply did not record the error scope state")
	}
	if m.reposInFlight {
		t.Fatal("the failed repos reply did not release the in-flight guard, so the tick cannot retry")
	}
	if !strings.Contains(stripANSI(m.View().Content), "could not load repositories") {
		t.Errorf("the repos-error scope state is not drawn\n%s", stripANSI(m.View().Content))
	}

	// The ci tick self-heals: because repos are not ready it re-issues fetchReposCmd (NOT a ci
	// fetch — there is no repo yet). Draining the command yields a reposMsg, proving ListRepos was
	// re-called; the fake now returns the repo.
	m, cmd := ciTick(t, m)
	if !m.reposInFlight {
		t.Fatal("the self-heal tick did not latch the repos in-flight guard")
	}
	drained := drainCmd(cmd)
	recovered, ok := firstReposMsg(drained)
	if !ok {
		t.Fatalf("the ci tick did not re-issue fetchReposCmd while the repo scope was unresolved; it cannot self-heal\n%v", drained)
	}
	if recovered.err != nil || len(recovered.repos) != 1 {
		t.Fatalf("the recovered ListRepos reply did not carry the repo (err=%v, %d repos)", recovered.err, len(recovered.repos))
	}
	if fake.ListCIRunsCalls != 0 {
		t.Fatalf("the self-heal tick issued a ci fetch before a repo was resolved (ListCIRuns=%d)", fake.ListCIRunsCalls)
	}

	// Feeding the recovered reply clears the error, resolves the default repo, and the ci fetch
	// then proceeds (ListCIRuns is finally called against the resolved repo).
	next, cmd = m.Update(recovered)
	m = next.(tuiModel)
	if m.reposErr != nil || !m.reposReady() {
		t.Fatalf("the recovered repos reply did not clear the error scope state (err=%v)", m.reposErr)
	}
	repo, ok := m.currentRepo()
	if !ok || repo.ID != "r1" {
		t.Fatalf("the recovered repos reply did not resolve a current repo (ok=%v id=%q)", ok, repo.ID)
	}
	if m.ci.waitID == 0 {
		t.Error("the resolved repo did not latch the ci in-flight guard for the kicked-off fetch")
	}
	drainCmd(cmd)
	if fake.ListCIRunsCalls != 1 {
		t.Fatalf("the resolved repo did not kick off the ci fetch (ListCIRuns=%d, want 1)", fake.ListCIRunsCalls)
	}
}
