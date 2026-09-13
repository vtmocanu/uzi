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

// PRD #1255 M6 seam tests for the CI-run drill-in (viewCIRun): jobs + steps with a 5s live re-poll
// and `f fix ci`. The model is driven in process (Update ← message, View → string) with a
// FakeClient, exactly like the PR tests (tui_pr_test.go).

// ---- fixtures + helpers ---------------------------------------------------

func csOf(name, status, conclusion string) apitypes.CIStepDTO {
	return apitypes.CIStepDTO{Name: name, Status: status, Conclusion: conclusion,
		StartedAt: time.Now().Add(-3 * time.Minute), CompletedAt: time.Now().Add(-time.Minute)}
}

func cjOf(name, status, conclusion string, steps ...apitypes.CIStepDTO) apitypes.CIJobDTO {
	return apitypes.CIJobDTO{Name: name, Status: status, Conclusion: conclusion,
		StartedAt: time.Now().Add(-5 * time.Minute), FinishedAt: time.Now().Add(-time.Minute), Steps: steps}
}

func cjPassed(name string, steps ...apitypes.CIStepDTO) apitypes.CIJobDTO {
	return cjOf(name, "completed", "success", steps...)
}
func cjFailed(name string, steps ...apitypes.CIStepDTO) apitypes.CIJobDTO {
	return cjOf(name, "completed", "failure", steps...)
}
func cjRunning(name string) apitypes.CIJobDTO {
	c := cjOf(name, "in_progress", "")
	c.FinishedAt = time.Time{}
	return c
}

// cjAttention is a completed job in the attention tone (GitHub action_required / GitLab manual /
// Forgejo warning all map to pipelinestatus.Tone == "attention"): counted in total, but NOT in
// passed/failing/running. cjSkipped is the neutral/skipped tone, likewise counted only in total.
func cjAttention(name string) apitypes.CIJobDTO { return cjOf(name, "completed", "action_required") }
func cjSkipped(name string) apitypes.CIJobDTO   { return cjOf(name, "completed", "skipped") }

// ciRunDetailOf builds a CI-run drill-in fixture with the given branch and jobs.
func ciRunDetailOf(branch string, jobs []apitypes.CIJobDTO) apitypes.CIRunDetailDTO {
	now := time.Now()
	return apitypes.CIRunDetailDTO{
		CIRunDTO: apitypes.CIRunDTO{ID: 1039, Name: "CI", Number: 1039, Event: "pull_request",
			Branch: branch, SHA: "deadbeefcafef00d", Status: "in_progress", Title: "a ci run",
			Actor: "uzi-bot", WebURL: "https://github.com/vtmocanu/uzi/actions/runs/1039",
			CreatedAt: now.Add(-2 * time.Minute), StartedAt: now.Add(-2 * time.Minute), UpdatedAt: now},
		Jobs: jobs,
	}
}

// sampleCIRunDetail is a realistic GitHub-shaped run (jobs WITH steps) spanning running + passed
// jobs, one job carrying an https URL. It is also the uxlab `cirun-running` scene's fixture
// (uxlab_gen_test.go).
func sampleCIRunDetail(now time.Time) apitypes.CIRunDetailDTO {
	return apitypes.CIRunDetailDTO{
		CIRunDTO: apitypes.CIRunDTO{
			ID: 1039, Name: "CI", Number: 1039, Event: "pull_request", Branch: "agent/issue-1246",
			SHA: "deadbeefcafef00d", Status: "in_progress", Title: "PRD #1226 continuation: rework the forge sync loop",
			Actor: "uzi-bot", WebURL: "https://github.com/vtmocanu/uzi/actions/runs/1039",
			StartedAt: now.Add(-2 * time.Minute), CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now,
			JobsDone: 2, JobsTotal: 4},
		Jobs: []apitypes.CIJobDTO{
			{ID: 1, Name: "lint-repo", Status: "completed", Conclusion: "success",
				WebURL:    "https://github.com/vtmocanu/uzi/actions/runs/1039/job/1",
				StartedAt: now.Add(-2 * time.Minute), FinishedAt: now.Add(-100 * time.Second),
				Steps: []apitypes.CIStepDTO{
					{Name: "Set up job", Status: "completed", Conclusion: "success", Number: 1, StartedAt: now.Add(-2 * time.Minute), CompletedAt: now.Add(-118 * time.Second)},
					{Name: "hadolint", Status: "completed", Conclusion: "success", Number: 2, StartedAt: now.Add(-118 * time.Second), CompletedAt: now.Add(-100 * time.Second)},
				}},
			{ID: 2, Name: "validate-api", Status: "completed", Conclusion: "success",
				WebURL:    "https://github.com/vtmocanu/uzi/actions/runs/1039/job/2",
				StartedAt: now.Add(-110 * time.Second), FinishedAt: now.Add(-20 * time.Second),
				Steps: []apitypes.CIStepDTO{{Name: "go test ./...", Status: "completed", Conclusion: "success", Number: 1, StartedAt: now.Add(-110 * time.Second), CompletedAt: now.Add(-20 * time.Second)}}},
			{ID: 3, Name: "test-api", Status: "in_progress",
				WebURL:    "https://github.com/vtmocanu/uzi/actions/runs/1039/job/3",
				StartedAt: now.Add(-90 * time.Second),
				Steps: []apitypes.CIStepDTO{
					{Name: "Set up job", Status: "completed", Conclusion: "success", Number: 1, StartedAt: now.Add(-90 * time.Second), CompletedAt: now.Add(-80 * time.Second)},
					{Name: "go test ./...", Status: "in_progress", Number: 2, StartedAt: now.Add(-80 * time.Second)},
				}},
			{ID: 4, Name: "test-web", Status: "in_progress",
				WebURL:    "https://github.com/vtmocanu/uzi/actions/runs/1039/job/4",
				StartedAt: now.Add(-70 * time.Second),
				Steps:     []apitypes.CIStepDTO{{Name: "npm test", Status: "in_progress", Number: 1, StartedAt: now.Add(-70 * time.Second)}}},
		},
	}
}

// openCIRun drives a fresh model to a loaded CI-run drill-in the real way: repos load, the user
// opens the ci list, the list lands, enter opens the CI-run view (minting startCIRunReq), and the
// detail reply is applied. The startCIRunReq command is discarded by press (no GetCIRun is
// executed), so the fake's GetCIRun counters stay clean for the poll-guard tests that drive the
// ticks themselves.
func openCIRun(t *testing.T, fake *uzicli.FakeClient, detail apitypes.CIRunDetailDTO) tuiModel {
	t.Helper()
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(reposMsg{repos: fake.Repos})
	m = next.(tuiModel)
	m = press(t, m, keyViewCI)
	next, _ = m.Update(ciMsg{reqID: m.ci.waitID, runs: []apitypes.CIRunDTO{detail.CIRunDTO}})
	m = next.(tuiModel)
	m = press(t, m, keyEnter)
	if m.view != viewCIRun {
		t.Fatalf("enter on a ci row did not open the CI-run view (view=%v)", m.view)
	}
	next, _ = m.Update(ciRunMsg{reqID: m.cirun.waitID, gen: m.cirun.gen, detail: detail})
	return next.(tuiModel)
}

func ciRunTick(t *testing.T, m tuiModel) (tuiModel, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(ciRunTickMsg{gen: m.cirun.tickGen})
	return next.(tuiModel), cmd
}

// feedCIRun drains a command and feeds any ciRunMsg / prActionMsg it produced back through Update.
// The fetch/action closures call the FakeClient, so draining is what exercises the real client path.
func feedCIRun(t *testing.T, m tuiModel, cmd tea.Cmd) tuiModel {
	t.Helper()
	for _, msg := range drainCmd(cmd) {
		switch msg.(type) {
		case ciRunMsg, prActionMsg:
			next, _ := m.Update(msg)
			m = next.(tuiModel)
		}
	}
	return m
}

// ---- header rollups (D3) --------------------------------------------------

func TestTUICIRunHeaderRollups(t *testing.T) {
	cases := []struct {
		name string
		jobs []apitypes.CIJobDTO
		want string
	}{
		{"running", []apitypes.CIJobDTO{cjPassed("a"), cjRunning("b")}, "● 1 running · ✓ 1/2"},
		{"failing", []apitypes.CIJobDTO{cjFailed("a"), cjPassed("b")}, "✗ 1 failing"},
		{"passed", []apitypes.CIJobDTO{cjPassed("a"), cjPassed("b")}, "✓ all jobs passed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
			m := openCIRun(t, fake, ciRunDetailOf("agent/issue-1", tc.jobs))
			frame := stripANSI(m.View().Content)
			if !strings.Contains(frame, tc.want) {
				t.Errorf("rollup: want %q in the header\n%s", tc.want, frame)
			}
			if !strings.Contains(frame, "● live · 5s") {
				t.Errorf("the live-cadence tag is not drawn\n%s", frame)
			}
			if !strings.Contains(frame, "re-polled") {
				t.Errorf("the re-polled proof is not drawn\n%s", frame)
			}
		})
	}
}

// TestTUICIRunJobsHeadingHonestCount pins the BLOCKING fix: the JOBS heading must NOT claim
// "all jobs passed" when some jobs sit in the attention (action_required) or neutral/skipped tone —
// those are counted in total but never in passed, so passed != total. It must instead show the
// honest "✓ P/T passed" count, matching the top-right rollup. Asserted on the JOBS heading LINE
// alone (not the whole frame) so it is non-vacuous in BOTH directions: the false phrase is absent
// AND the honest count is present. Pre-fix the heading's default arm fired "all jobs passed"
// whenever failing==0 && running==0, regardless of passed==total, so this reddens pre-fix.
func TestTUICIRunJobsHeadingHonestCount(t *testing.T) {
	cases := []struct {
		name      string
		jobs      []apitypes.CIJobDTO
		wantCount string
	}{
		// passed + action_required: passed=1, total=2 → the rollup already shows ✓ 1/2 passed; the
		// heading must agree, not read "all jobs passed".
		{"passed+attention", []apitypes.CIJobDTO{cjPassed("a"), cjAttention("b")}, "✓ 1/2 passed"},
		// an all-skipped run: 0 actually passed out of 2 — the worst case of the false phrase.
		{"all-skipped", []apitypes.CIJobDTO{cjSkipped("a"), cjSkipped("b")}, "✓ 0/2 passed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
			m := openCIRun(t, fake, ciRunDetailOf("agent/issue-1", tc.jobs))
			frame := stripANSI(m.View().Content)

			var heading string
			for _, ln := range strings.Split(frame, "\n") {
				if strings.Contains(ln, "JOBS") {
					heading = ln
					break
				}
			}
			if heading == "" {
				t.Fatalf("the JOBS heading line was not found\n%s", frame)
			}
			if strings.Contains(heading, "all jobs passed") {
				t.Errorf("the JOBS heading falsely claims all jobs passed: %q", heading)
			}
			if !strings.Contains(heading, tc.wantCount) {
				t.Errorf("the JOBS heading does not show the honest count %q: %q", tc.wantCount, heading)
			}
		})
	}
}

// ---- jobs cursor + steps expansion (D3/D8) --------------------------------

func TestTUICIRunJobsCursorAndSteps(t *testing.T) {
	jobs := []apitypes.CIJobDTO{
		cjPassed("lint", csOf("checkoutstep", "completed", "success"), csOf("hadolintstep", "completed", "success")),
		cjFailed("test", csOf("setupstep", "completed", "success"), csOf("compilestep", "completed", "failure")),
	}
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := openCIRun(t, fake, ciRunDetailOf("agent/issue-2", jobs))

	// Cursor starts at job 0: its steps expand; the unselected job's steps do NOT.
	frame := stripANSI(m.View().Content)
	if !strings.Contains(frame, "checkoutstep") || !strings.Contains(frame, "hadolintstep") {
		t.Errorf("the selected job's steps did not expand\n%s", frame)
	}
	if strings.Contains(frame, "compilestep") {
		t.Errorf("an unselected job's steps were expanded\n%s", frame)
	}

	// ↓ selects the failing job: its steps expand, including the failing step with its glyph + word.
	m = press(t, m, keyDown)
	if m.cirun.cursor != 1 {
		t.Fatalf("↓ did not move the jobs cursor (cursor=%d)", m.cirun.cursor)
	}
	frame = stripANSI(m.View().Content)
	if !strings.Contains(frame, "compilestep") || !strings.Contains(frame, "setupstep") {
		t.Errorf("the newly-selected job's steps did not expand\n%s", frame)
	}
	// The failing step row is indented beneath its job and carries ✗ + the "failed" word (D8).
	var stepLine string
	for _, ln := range strings.Split(frame, "\n") {
		if strings.Contains(ln, "compilestep") {
			stepLine = ln
			break
		}
	}
	if !strings.HasPrefix(stepLine, "    ") {
		t.Errorf("the failing step row is not indented beneath its job: %q", stepLine)
	}
	if !strings.Contains(stepLine, "✗") || !strings.Contains(stepLine, "failed") {
		t.Errorf("the failing step row does not carry its glyph + word (D8): %q", stepLine)
	}
	// The failing step name is drawn in the alarm tone (its exact painted segment is in the raw frame).
	want := paintSeg(m.pal.alarm, nil, false, m.renderer.Plain("compilestep", ciRunStepNameWidth))
	if !strings.Contains(m.View().Content, want) {
		t.Errorf("the failing step name is not drawn in the alarm tone")
	}
}

// ---- poll guards (D4) -----------------------------------------------------

func TestTUICIRunTickInFlightGuard(t *testing.T) {
	orig := ciRunPollInterval
	ciRunPollInterval = time.Millisecond
	t.Cleanup(func() { ciRunPollInterval = orig })

	detail := ciRunDetailOf("agent/issue-3", []apitypes.CIJobDTO{cjRunning("a")})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, CIRunDetailResult: detail}
	m := openCIRun(t, fake, detail)
	if m.cirun.waitID != 0 {
		t.Fatalf("the CI-run reply did not clear the guard (waitID=%d)", m.cirun.waitID)
	}
	if fake.GetCIRunCalls != 0 {
		t.Fatalf("no fetch closure has been executed yet, want 0 got %d", fake.GetCIRunCalls)
	}

	// First tick from idle: one fetch, guard latched.
	m, cmd := ciRunTick(t, m)
	if m.cirun.waitID == 0 {
		t.Fatal("the first CI-run tick did not latch the in-flight guard")
	}
	drainCmd(cmd)
	if fake.GetCIRunCalls != 1 {
		t.Fatalf("the first tick issued %d GetCIRun, want exactly 1", fake.GetCIRunCalls)
	}
	if fake.LastGetCIRunID != 1039 || fake.LastGetCIRunRepoID != "r1" {
		t.Fatalf("the fetch targeted repo %q run %d, want r1/1039", fake.LastGetCIRunRepoID, fake.LastGetCIRunID)
	}

	// Second tick while the poll is in flight: no second fetch.
	m, cmd = ciRunTick(t, m)
	drainCmd(cmd)
	if fake.GetCIRunCalls != 1 {
		t.Fatalf("a tick while a poll was in flight issued another GetCIRun (total %d)", fake.GetCIRunCalls)
	}

	// The reply clears the guard, so the next tick fetches again.
	next, _ := m.Update(ciRunMsg{reqID: m.cirun.waitID, gen: m.cirun.gen, detail: detail})
	m = next.(tuiModel)
	if m.cirun.waitID != 0 {
		t.Fatal("the CI-run reply did not clear the guard")
	}
	m, cmd = ciRunTick(t, m)
	drainCmd(cmd)
	if fake.GetCIRunCalls != 2 {
		t.Fatalf("after a reply cleared the guard, the next tick brought GetCIRun to %d, want 2", fake.GetCIRunCalls)
	}
}

func TestTUICIRunStaleReplyDropped(t *testing.T) {
	orig := ciRunPollInterval
	ciRunPollInterval = time.Millisecond
	t.Cleanup(func() { ciRunPollInterval = orig })

	detail := ciRunDetailOf("agent/issue-4", []apitypes.CIJobDTO{cjRunning("a")})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, CIRunDetailResult: detail}
	m := openCIRun(t, fake, detail)

	m, _ = ciRunTick(t, m)
	waitID := m.cirun.waitID
	if waitID == 0 {
		t.Fatal("the tick did not mint a CI-run request")
	}

	other := ciRunDetailOf("agent/issue-4", []apitypes.CIJobDTO{cjPassed("x")})
	other.Title = "stale reply title"
	next, _ := m.Update(ciRunMsg{reqID: waitID + 999, gen: m.cirun.gen, detail: other})
	m = next.(tuiModel)
	if m.cirun.waitID != waitID {
		t.Fatalf("a stale reply cleared the guard: waitID=%d, want %d", m.cirun.waitID, waitID)
	}
	if m.cirun.detail.Title == "stale reply title" {
		t.Fatal("a stale reply applied its detail")
	}
}

func TestTUICIRunErrStreakBacksOff(t *testing.T) {
	orig := ciRunPollInterval
	ciRunPollInterval = time.Millisecond
	t.Cleanup(func() { ciRunPollInterval = orig })

	detail := ciRunDetailOf("agent/issue-5", []apitypes.CIJobDTO{cjRunning("a")})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}, CIRunDetailResult: detail}
	m := openCIRun(t, fake, detail)

	pollErr := uzicli.Exitf(uzicli.ExitGeneric, "context deadline exceeded")
	for want := 1; want <= 3; want++ {
		m, _ = ciRunTick(t, m)
		if m.cirun.waitID == 0 {
			t.Fatalf("tick %d did not mint a CI-run request", want)
		}
		next, _ := m.Update(ciRunMsg{reqID: m.cirun.waitID, gen: m.cirun.gen, err: pollErr})
		m = next.(tuiModel)
		if m.cirun.errStreak != want {
			t.Fatalf("after %d error replies errStreak = %d, want %d", want, m.cirun.errStreak, want)
		}
	}
	if ciRunTickInterval(m.cirun.errStreak) <= ciRunPollInterval {
		t.Fatalf("the backoff did not grow past the base %v", ciRunPollInterval)
	}

	// A success reply resets the streak.
	m, _ = ciRunTick(t, m)
	next, _ := m.Update(ciRunMsg{reqID: m.cirun.waitID, gen: m.cirun.gen, detail: detail})
	m = next.(tuiModel)
	if m.cirun.errStreak != 0 {
		t.Fatalf("a success reply left errStreak = %d, want 0", m.cirun.errStreak)
	}
}

// TestTUICIRunReopenStaleReplyDropped reproduces the CROSS-ENTITY collision: newCIRunState resets
// reqSeq on every open, so a prior run's still-in-flight reply mints the SAME reqID as the
// freshly-opened run and passes the reqID==waitID guard. Without the monotonic session gen, run A's
// late jobs/steps would be applied to the run B view. The ciRunGen guard drops it (mirrors the PR
// view's reopen-collision test). Removing the `msg.gen != m.cirun.gen` clause from the ciRunMsg
// handler reddens exactly here.
func TestTUICIRunReopenStaleReplyDropped(t *testing.T) {
	runA := ciRunDetailOf("branch-a", []apitypes.CIJobDTO{cjRunning("job-a")})
	runA.ID, runA.Number = 1039, 1039
	runB := ciRunDetailOf("branch-b", []apitypes.CIJobDTO{cjRunning("job-b")})
	runB.ID, runB.Number = 1040, 1040

	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(reposMsg{repos: fake.Repos})
	m = next.(tuiModel)

	// Open run A via the ci enter path and leave its GetCIRun in flight (no reply fed).
	m = press(t, m, keyViewCI)
	next, _ = m.Update(ciMsg{reqID: m.ci.waitID, runs: []apitypes.CIRunDTO{runA.CIRunDTO}})
	m = next.(tuiModel)
	m = press(t, m, keyEnter)
	if m.view != viewCIRun || m.cirun.runID != 1039 {
		t.Fatalf("enter did not open run A (view=%v runID=%d)", m.view, m.cirun.runID)
	}
	aWait, aGen := m.cirun.waitID, m.cirun.gen
	if aWait == 0 {
		t.Fatal("opening run A did not mint an in-flight request")
	}

	// esc back to the ci list, swap the row to run B, and open it: newCIRunState resets reqSeq, so B
	// mints the SAME reqID as A's still-in-flight request (the collision), under a NEW gen.
	m = press(t, m, keyEsc)
	if m.view != viewCI {
		t.Fatalf("esc did not return to the ci list (view=%v)", m.view)
	}
	next, _ = m.Update(ciMsg{reqID: m.ci.waitID, runs: []apitypes.CIRunDTO{runB.CIRunDTO}})
	m = next.(tuiModel)
	m = press(t, m, keyEnter)
	if m.view != viewCIRun || m.cirun.runID != 1040 {
		t.Fatalf("enter did not open run B (view=%v runID=%d)", m.view, m.cirun.runID)
	}
	if m.cirun.waitID != aWait {
		t.Fatalf("the reopen did not mint the colliding reqID (B waitID=%d, A waitID=%d)", m.cirun.waitID, aWait)
	}
	if m.cirun.gen == aGen {
		t.Fatalf("the reopen did not advance the CI-run session generation (gen still %d)", aGen)
	}

	// Run A's late reply arrives while B's request is in flight: reqID collides with B's waitID, but
	// the session gen does not — it MUST be dropped, never applied to the B view.
	next, _ = m.Update(ciRunMsg{reqID: aWait, gen: aGen, detail: runA})
	m = next.(tuiModel)
	if len(m.cirun.detail.Jobs) > 0 && m.cirun.detail.Jobs[0].Name == "job-a" {
		t.Fatal("run A's stale reply was applied to the run B view")
	}
	if m.cirun.waitID != aWait {
		t.Fatalf("the dropped stale reply cleared B's in-flight guard (waitID=%d, want %d)", m.cirun.waitID, aWait)
	}

	// B's own reply (matching gen) is still honoured, so the view loads B's jobs.
	next, _ = m.Update(ciRunMsg{reqID: m.cirun.waitID, gen: m.cirun.gen, detail: runB})
	m = next.(tuiModel)
	if len(m.cirun.detail.Jobs) == 0 || m.cirun.detail.Jobs[0].Name != "job-b" {
		t.Fatalf("run B's own reply was not applied (jobs=%+v)", m.cirun.detail.Jobs)
	}
}

// TestTUICIRunLivePollFlipsHeader mutates fake.CIRunDetailResult between ticks (running → settled)
// and asserts the header rollup flips. There is no GetCIRun hook — the fake returns
// CIRunDetailResult fresh on each call — so assigning it between Update drives is the live-flip seam.
func TestTUICIRunLivePollFlipsHeader(t *testing.T) {
	orig := ciRunPollInterval
	ciRunPollInterval = time.Millisecond
	t.Cleanup(func() { ciRunPollInterval = orig })

	running := ciRunDetailOf("agent/issue-6", []apitypes.CIJobDTO{cjPassed("a"), cjRunning("b")})
	settled := ciRunDetailOf("agent/issue-6", []apitypes.CIJobDTO{cjPassed("a"), cjPassed("b")})

	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := openCIRun(t, fake, running)
	if frame := stripANSI(m.View().Content); !strings.Contains(frame, "● 1 running") {
		t.Fatalf("the first load did not show the running rollup\n%s", frame)
	}

	// The next tick fetches the (now settled) CIRunDetailResult; the header flips.
	fake.CIRunDetailResult = settled
	m, cmd := ciRunTick(t, m)
	m = feedCIRun(t, m, cmd)
	if fake.GetCIRunCalls < 1 {
		t.Fatalf("the live re-poll did not call GetCIRun (calls=%d)", fake.GetCIRunCalls)
	}
	if frame := stripANSI(m.View().Content); !strings.Contains(frame, "all jobs passed") {
		t.Fatalf("the header did not flip to settled after the live re-poll\n%s", frame)
	}
}

// ---- f fix ci (D12) -------------------------------------------------------

func TestTUICIRunFixCISuccess(t *testing.T) {
	detail := ciRunDetailOf("agent/issue-1251", []apitypes.CIJobDTO{cjFailed("lint-api")})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()},
		CIFixRunResult: apitypes.RunDTO{ID: "cccccccc-1111-2222-3333-444444444444"}}
	m := openCIRun(t, fake, detail)

	nm, cmd := m.handleKey(keyFixCI)
	m = feedCIRun(t, nm.(tuiModel), cmd)
	if fake.LastCIFixRef != "agent/issue-1251" {
		t.Fatalf("CreateCIFixRun was not called with the run's branch (got %q)", fake.LastCIFixRef)
	}
	if fake.LastCIFixRepoID != "r1" {
		t.Fatalf("CreateCIFixRun was not scoped to the repo (got %q)", fake.LastCIFixRepoID)
	}
	if frame := stripANSI(m.View().Content); !strings.Contains(frame, "queued fix ci") {
		t.Errorf("the fix-ci confirmation is not drawn inline\n%s", frame)
	}
}

func TestTUICIRunFixCIConflict(t *testing.T) {
	detail := ciRunDetailOf("agent/issue-1252", []apitypes.CIJobDTO{cjFailed("lint-api")})
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()},
		CreateCIFixRunErr: uzicli.Exitf(uzicli.ExitConflict, "ref is not a watched failed ref")}
	m := openCIRun(t, fake, detail)

	nm, cmd := m.handleKey(keyFixCI)
	m = feedCIRun(t, nm.(tuiModel), cmd)
	if fake.LastCIFixRef != "agent/issue-1252" {
		t.Fatalf("CreateCIFixRun was not reached with the run's branch (got %q)", fake.LastCIFixRef)
	}
	if frame := stripANSI(m.View().Content); !strings.Contains(frame, "ref is not a watched failed ref") {
		t.Errorf("the 409 reason is not drawn inline\n%s", frame)
	}
}

// TestTUICIRunFixCIUsesRowBranchBeforeDetailLoads pins the fold-in fix: the ci-list open path seeds
// the selected row's CIRunDTO into the drill-in state, so pressing f in the window BEFORE the first
// GetCIRun reply lands targets the row's branch — not an empty ref (which the server 409s). It opens
// the drill-in without feeding any ciRunMsg (so loaded is false), then presses f and asserts
// CreateCIFixRun was reached with the row's branch.
func TestTUICIRunFixCIUsesRowBranchBeforeDetailLoads(t *testing.T) {
	row := ciRunDetailOf("agent/issue-1255", []apitypes.CIJobDTO{cjFailed("lint-api")}).CIRunDTO
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()},
		CIFixRunResult: apitypes.RunDTO{ID: "cccccccc-1111-2222-3333-444444444444"}}
	m := tuiTestModel(t, fake, "")
	next, _ := m.Update(reposMsg{repos: fake.Repos})
	m = next.(tuiModel)
	m = press(t, m, keyViewCI)
	next, _ = m.Update(ciMsg{reqID: m.ci.waitID, runs: []apitypes.CIRunDTO{row}})
	m = next.(tuiModel)
	m = press(t, m, keyEnter)
	if m.view != viewCIRun {
		t.Fatalf("enter on a ci row did not open the CI-run view (view=%v)", m.view)
	}
	if m.cirun.loaded {
		t.Fatalf("no GetCIRun reply was fed, so the drill-in must not be marked loaded yet")
	}

	// f in this pre-reply window must target the seeded row branch, not an empty ref.
	nm, cmd := m.handleKey(keyFixCI)
	m = feedCIRun(t, nm.(tuiModel), cmd)
	if fake.LastCIFixRef != "agent/issue-1255" {
		t.Fatalf("f before the first GetCIRun reply called CreateCIFixRun with %q, want the row's branch", fake.LastCIFixRef)
	}
	if fake.LastCIFixRepoID != "r1" {
		t.Fatalf("CreateCIFixRun was not scoped to the repo (got %q)", fake.LastCIFixRepoID)
	}
}

// ---- degrade + empty (D5/R2) ----------------------------------------------

func TestTUICIRunUnsupportedDegrade(t *testing.T) {
	detail := ciRunDetailOf("agent/issue-7", nil)
	detail.Unsupported = "CI runs need Forgejo v16.0.0 or newer on this connection."
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := openCIRun(t, fake, detail)

	frame := stripANSI(m.View().Content)
	if !strings.Contains(frame, "CI runs need Forgejo v16.0.0 or newer") {
		t.Errorf("the unsupported degrade sentence is not rendered verbatim\n%s", frame)
	}
}

func TestTUICIRunEmptyJobs(t *testing.T) {
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := openCIRun(t, fake, ciRunDetailOf("agent/issue-8", nil))

	frame := stripANSI(m.View().Content)
	if !strings.Contains(frame, "No jobs reported for this run") {
		t.Errorf("the empty-jobs sentence is not drawn\n%s", frame)
	}
}

// ---- width safety (the m5 lesson) -----------------------------------------

// TestTUICIRunRendersAtNarrowWidthsWithoutPanic renders the CI-run view (jobs + steps, a selected
// job, an https job URL) at a range of widths and asserts no panic and no rendered line overflows
// m.width.
func TestTUICIRunRendersAtNarrowWidthsWithoutPanic(t *testing.T) {
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := openCIRun(t, fake, sampleCIRunDetail(time.Now()))

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

// ---- Ascii presence (D8) --------------------------------------------------

func TestTUICIRunAsciiProfileCarriesGlyphsAndWords(t *testing.T) {
	jobs := []apitypes.CIJobDTO{
		cjFailed("lint-api", csOf("setup", "completed", "success"), csOf("lint", "completed", "failure")),
		cjRunning("test-api"),
		cjPassed("build-web"),
	}
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := openCIRun(t, fake, ciRunDetailOf("agent/issue-9", jobs))

	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	frame := stripANSI(m.View().Content)
	for _, want := range []string{
		"JOBS",        // section label
		"✗", "●", "✓", // per-state glyphs
		"failing", "running", "passed", // heading aggregate words
		"failed", // per-job/step state word (forgeState)
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("the Ascii CI-run frame is missing %q (glyph/word must survive colour stripping — D8)\n%s", want, frame)
		}
	}
}

// ---- D7 per-field hostile-value render test -------------------------------

func TestTUICIRunStripsControlBytesPerField(t *testing.T) {
	// Hostile bytes at the FRONT so a sanitized tail survives truncation; distinct tails prove each
	// render path ran. Bidi override + ESC/BEL/control + a newline that would forge a row.
	const (
		nameN  = "\x1b[2J\u202e\x07\x01ciname"
		evtN   = "\x1b[2J\u202e\x07\x01cievt"
		brN    = "\x1b[2J\u202e\x07\x01cibr"
		shaN   = "\x1b[2J\u202e\x07\x01cish"
		titleN = "\x1b[2J\u202e\x07\x01cititle"
		actN   = "\x1b[2J\u202e\x07\x01ciact"
		jobN   = "\x1b[2J\u202e\x07\x01cijob\n  FORGED"
		stepN  = "\x1b[2J\u202e\x07\x01cistep"
	)
	detail := apitypes.CIRunDetailDTO{
		CIRunDTO: apitypes.CIRunDTO{ID: 1, Name: nameN, Number: 7, Event: evtN, Branch: brN, SHA: shaN,
			Title: titleN, Actor: actN, Status: "in_progress",
			WebURL:    "https://github.com/vtmocanu/uzi/actions/runs/1",
			CreatedAt: time.Now().Add(-time.Hour), StartedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now()},
		Jobs: []apitypes.CIJobDTO{{ID: 1, Name: jobN, Status: "completed", Conclusion: "failure",
			WebURL:    "javascript:alert(1)", // must NEVER become an OSC-8 link target
			StartedAt: time.Now().Add(-5 * time.Minute), FinishedAt: time.Now().Add(-time.Minute),
			Steps: []apitypes.CIStepDTO{{Name: stepN, Status: "completed", Conclusion: "failure", Number: 1,
				StartedAt: time.Now().Add(-3 * time.Minute), CompletedAt: time.Now().Add(-time.Minute)}}}},
	}
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := openCIRun(t, fake, detail)

	out := m.View().Content
	assertNoRawControls(t, "cirun", out)
	for _, want := range []string{"ciname", "cievt", "cibr", "cish", "cititle", "ciact", "cijob", "cistep"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the CI-run view is not drawing a forge field (missing %q), so this test does not exercise that path\n%s", want, out)
		}
	}
	if strings.Contains(out, "\n  FORGED") {
		t.Errorf("a forge job-name newline forged a row in the frame\n%s", out)
	}
	// The non-https job URL emitted NO OSC-8 envelope.
	if strings.Contains(out, "\x1b]8;;") {
		t.Errorf("a non-https job URL was emitted as an OSC-8 hyperlink; only https may be linked\n%s", out)
	}

	// Positive control: an https job URL on the selected job IS emitted as an OSC-8 hyperlink.
	detail2 := detail
	detail2.Jobs = []apitypes.CIJobDTO{{ID: 1, Name: "build", Status: "completed", Conclusion: "success",
		WebURL: "https://github.com/vtmocanu/uzi/actions/runs/1/job/1", StartedAt: time.Now().Add(-5 * time.Minute), FinishedAt: time.Now().Add(-time.Minute)}}
	m2 := openCIRun(t, fake, detail2)
	if raw := m2.View().Content; !strings.Contains(raw, "\x1b]8;;https://github.com/vtmocanu/uzi/actions/runs/1/job/1") {
		t.Errorf("the selected https job URL was not emitted as an OSC-8 hyperlink\n%s", raw)
	}
}

// ---- filter / quit / help provenance --------------------------------------

// The CI-run view is a drill-in (no `/` filter), so q quits and ? opens help from it (D13).
func TestTUICIRunQuitAndHelpNotFiltered(t *testing.T) {
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := openCIRun(t, fake, ciRunDetailOf("agent/issue-10", []apitypes.CIJobDTO{cjPassed("a")}))
	if m.filtering() {
		t.Fatal("the CI-run view reported itself as a filter screen")
	}
	if hm := press(t, m, keyHelp); !hm.showHelp {
		t.Error("? did not open help from the CI-run view")
	}
	if _, cmd := m.handleKey(keyQuit); cmd == nil {
		t.Error("q from the CI-run view did not quit")
	}
	// esc returns to the ci list, which is not clobbered.
	m = press(t, m, keyEsc)
	if m.view != viewCI {
		t.Fatalf("esc on the CI-run view did not return to the ci list (view=%v)", m.view)
	}
}
