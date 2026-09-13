package main

// UX-LAB FRAME GENERATOR — see api/cmd/uzi/uxlab/README.md.
//
// This is NOT a gate test. It is an in-package renderer that drives the real tuiModel
// (the same seam tui_model_test.go uses: Update -> a message, View() -> a string) into a
// range of representative states, in BOTH the dark and light palettes, and writes each
// frame as a raw-ANSI file under uxlab/frames/. A separate step (uxlab/render.sh) turns
// those into PNGs with charmbracelet/freeze so any agent can Read them.
//
// It lives in package main because tuiModel and its ~30 render helpers are unexported and
// Go forbids importing a main package — the same reason tui.go gives for the whole TUI
// living here. It is gated behind UZI_UXLAB_GEN=1 so `task gate:api` compiles it and skips
// it instantly; it never runs in CI.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// frameWidth/frameHeight are the terminal box every frame is rendered at. 100x34 is a
// comfortable, common terminal size and keeps the split-pane detail view legible.
const (
	frameWidth  = 100
	frameHeight = 34
)

func sp(s string) *string       { return &s }
func ip(n int64) *int64         { return &n }
func tp(t time.Time) *time.Time { return &t }

// uxModel builds a model at the lab's fixed size and theme. The renderer is rebuilt
// after width+theme are set, because newTUIModel built it at the default width/dark.
func uxModel(c uzicli.Client, startRun string, dark bool) tuiModel {
	m := newTUIModel(context.Background(), c, startRun)
	m.width, m.height = frameWidth, frameHeight
	m.dark = dark
	m.pal = newPalette(dark)
	m.renderer, _ = newTUIRenderer(m.transcriptWidth(), dark)
	return m
}

// step applies one message through the real Update path and returns the new model,
// discarding the command (offline: no command is ever executed).
func step(m tuiModel, msg tea.Msg) tuiModel {
	next, _ := m.Update(msg)
	return next.(tuiModel)
}

// key drives one keypress through the real key path (no *testing.T dependency, unlike the
// sibling press helper).
func key(m tuiModel, k string) tuiModel {
	next, _ := m.handleKey(k)
	return next.(tuiModel)
}

func TestGenerateUXLabFrames(t *testing.T) {
	if os.Getenv("UZI_UXLAB_GEN") != "1" {
		t.Skip("set UZI_UXLAB_GEN=1 to (re)generate the ux-lab frames")
	}

	outDir := filepath.Join("uxlab", "frames")
	if err := os.MkdirAll(outDir, 0o755); err != nil { //nolint:gosec // G301: dev-tool frame output dir holds non-sensitive generated artifacts; 0755 keeps it browsable
		t.Fatal(err)
	}

	now := time.Now()

	// scenes maps a base name to a builder that returns the rendered frame for a theme.
	scenes := map[string]func(dark bool) string{
		"board-populated":              func(d bool) string { return boardPopulated(d, now) },
		"board-empty":                  boardEmpty,
		"board-admin":                  boardAdmin,
		"board-filter":                 func(d bool) string { return boardFilter(d, now) },
		"board-planning":               func(d bool) string { return boardPlanning(d, now) },
		"board-revising":               func(d bool) string { return boardRevising(d, now) },
		"board-milestones":             func(d bool) string { return boardMilestones(d, now) },
		"detail-running":               func(d bool) string { return detailRunning(d, now) },
		"detail-milestones-attributed": func(d bool) string { return detailMilestonesAttributed(d, now) },
		"detail-planning":              func(d bool) string { return detailPlanning(d, now) },
		"detail-focus-transcript":      func(d bool) string { return detailFocusTranscript(d, now) },
		"detail-paused":                func(d bool) string { return detailPaused(d, now) },
		"detail-stalled":               func(d bool) string { return detailStalled(d, now) },
		"detail-awaiting-approval":     func(d bool) string { return detailAwaitingApproval(d, now) },
		"detail-awaiting-input":        func(d bool) string { return detailAwaitingInput(d, now) },
		"detail-limit-wait":            func(d bool) string { return detailLimitWait(d, now) },
		"detail-degraded":              func(d bool) string { return detailDegraded(d, now) },
		"detail-steer-typing":          func(d bool) string { return detailSteerTyping(d, now) },
		"detail-steer-confirm":         func(d bool) string { return detailSteerConfirm(d, now) },
		"detail-steer-queue":           func(d bool) string { return detailSteerQueue(d, now) },
		"review-overlay":               func(d bool) string { return reviewOverlay(d, now) },
		"review-pending":               func(d bool) string { return reviewPending(d, now) },
		"pulls-populated":              func(d bool) string { return pullsPopulated(d, now) },
		"pulls-empty":                  func(d bool) string { return pullsEmpty(d, now) },
		"ci-populated":                 func(d bool) string { return ciPopulated(d, now) },
		"ci-unsupported":               func(d bool) string { return ciUnsupported(d, now) },
		"pr-live":                      func(d bool) string { return prLive(d, now) },
		"pr-changes-requested":         func(d bool) string { return prChangesRequested(d, now) },
		"pr-failing":                   func(d bool) string { return prFailing(d, now) },
		"cirun-running":                func(d bool) string { return ciRunRunning(d, now) },
		"help":                         helpFrame,
		"quit":                         quitFrame,
	}

	names := make([]string, 0, len(scenes))
	for n := range scenes {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		for _, dark := range []bool{true, false} {
			theme := "dark"
			if !dark {
				theme = "light"
			}
			body := scenes[name](dark)
			path := filepath.Join(outDir, fmt.Sprintf("%s-%s.ansi", name, theme))
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil { //nolint:gosec // G306: generated ANSI frame is non-sensitive; 0644 keeps it readable by render.sh/freeze
				t.Fatalf("write %s: %v", path, err)
			}
			t.Logf("wrote %s (%d bytes)", path, len(body))
		}
	}

	// Screenshot every Tier-A sketch from the shared registry (sketch.go), so a sketch
	// authored once is previewable live AND rendered to static frames with no per-sketch
	// generator code here. A Tier-B-only sketch (frames == nil) is live-preview-only and
	// has no static frame to render, so it is skipped; a Tier-B sketch that ALSO supplies
	// frames IS screenshotted. Filenames are sketch-<name>-<frameIndex>-<theme>.ansi —
	// theme LAST so render.sh's *-light detection works, frameIndex before it because a
	// Tier-A sketch has many frames per theme (unlike a scene, which is one string).
	sketchKeys := make([]string, 0, len(sketches))
	for name := range sketches {
		sketchKeys = append(sketchKeys, name)
	}
	sort.Strings(sketchKeys)

	for _, name := range sketchKeys {
		sk := sketches[name]
		if sk.frames == nil {
			continue // Tier-B-only: live preview only, no static frame to render.
		}
		for _, dark := range []bool{true, false} {
			theme := "dark"
			if !dark {
				theme = "light"
			}
			for i, body := range sk.frames(dark) {
				path := filepath.Join(outDir, fmt.Sprintf("sketch-%s-%d-%s.ansi", name, i, theme))
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil { //nolint:gosec // G306: generated ANSI frame is non-sensitive; 0644 keeps it readable by render.sh/freeze
					t.Fatalf("write %s: %v", path, err)
				}
				t.Logf("wrote %s (%d bytes)", path, len(body))
			}
		}
	}
}

// ---- board fixtures -------------------------------------------------------

func boardRuns(now time.Time) []apitypes.RunListItemDTO {
	// age is set via CreatedAt so relAge renders a realistic AGE column (M2). The offsets
	// are chosen to land on relAge's buckets (Nm / Nh / Nd) and mirror the mock's ages.
	mk := func(id, kind, status, title, health string, verdict *string, todo int, age time.Duration) apitypes.RunListItemDTO {
		r := apitypes.RunListItemDTO{
			RunDTO: apitypes.RunDTO{ID: id, Kind: kind, Status: status, IssueTitle: title, Health: health,
				CreatedAt: now.Add(-age)},
			JudgeVerdict: verdict, JudgeTodoCount: todo,
		}
		if kind == "chat" {
			r.Title = sp(title)
			r.IssueTitle = ""
		}
		return r
	}
	runs := []apitypes.RunListItemDTO{
		mk("a1b2c3d4-1111-2222-3333-444444444444", "issue", "running", "Add rate-limit headroom to the scheduler poll", "", nil, 0, 4*time.Minute),
		mk("b2c3d4e5-1111-2222-3333-444444444444", "ci_fix", "awaiting_approval", "Fix flaky pipeline on main", "", nil, 0, 2*time.Minute),
		mk("c3d4e5f6-1111-2222-3333-444444444444", "issue", "running", "Refactor the forge sync loop for the GitHub driver", "stalled", nil, 0, 51*time.Minute),
		mk("d4e5f6a7-1111-2222-3333-444444444444", "chat", "running", "Explain the run lifecycle state machine", "", nil, 0, time.Minute),
		mk("c9d0e1f2-1111-2222-3333-444444444444", "issue", "running", "Tighten the retry backoff jitter", "looping", nil, 0, 12*time.Minute),
		mk("e5f6a7b8-1111-2222-3333-444444444444", "issue", "completed", "Wire the OIDC login button into the header", "", sp("ideal"), 0, 3*time.Hour),
		mk("f6a7b8c9-1111-2222-3333-444444444444", "issue", "limit_wait", "Port the judge to per-model usage folding", "", nil, 0, 22*time.Minute),
		mk("a7b8c9d0-1111-2222-3333-444444444444", "issue", "failed", "Migrate per-user secrets into the vault hierarchy", "", sp("issues"), 3, 5*time.Hour),
		mk("b8c9d0e1-1111-2222-3333-444444444444", "ci_fix", "completed", "Repair the changelog assertion gate", "", sp("ok"), 0, 25*time.Hour),
	}
	// PRD #379: milestone-structured runs so the board's MILE column is exercised — one
	// mid-flight (▰▰▱▱) and one that has reported nothing yet (all-empty ▱▱▱, graphical 0/3).
	// The rest stay blank, which is the common case and the alignment worth checking.
	runs[0].Milestones = milestoneList
	runs[0].MilestonesCompleted = []string{"m1", "m2"}
	runs[0].MilestonesInProgress = []string{"m3"}
	runs[6].Milestones = []apitypes.Milestone{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}}
	// PRD #295: WHICH Anthropic credential each run spent, so the board's credential column is
	// exercised offline — meta and personal labels, drawn muted with no dot (the select reason is
	// realistic data but the board deliberately does not surface it). The rest stay blank (pre-#111
	// or unclaimed), the common case and the alignment worth checking.
	runs[0].AnthropicSecretID, runs[0].AnthropicSecretLabel, runs[0].AnthropicSelectReason = sp("sec-meta"), sp("meta"), sp("auto")
	runs[2].AnthropicSecretID, runs[2].AnthropicSecretLabel, runs[2].AnthropicSelectReason = sp("sec-personal"), sp("personal"), sp("pool_stale")
	runs[6].AnthropicSecretID, runs[6].AnthropicSecretLabel, runs[6].AnthropicSelectReason = sp("sec-personal"), sp("personal"), sp("best_of_pool")
	// PRD #519: stamp forge issue ids on the issue-kind runs so the board's clickable #<iid>
	// OSC-8 link is exercised by the generated frames. The ci_fix runs (b2c3d4e5 "Fix flaky
	// pipeline", b8c9d0e1) and the chat run keep a nil id — the no-id case, which draws no #.
	iurl := func(n int64) *string { return sp(fmt.Sprintf("https://github.com/vtmocanu/uzi/issues/%d", n)) }
	runs[0].IssueIID, runs[0].IssueWebURL = ip(452), iurl(452)
	runs[2].IssueIID, runs[2].IssueWebURL = ip(477), iurl(477)
	runs[4].IssueIID, runs[4].IssueWebURL = ip(468), iurl(468)
	runs[5].IssueIID, runs[5].IssueWebURL = ip(419), iurl(419)
	runs[6].IssueIID, runs[6].IssueWebURL = ip(463), iurl(463)
	runs[7].IssueIID, runs[7].IssueWebURL = ip(408), iurl(408)
	// PRD #650: attach Usage to a spread of runs so the board's COST column and floor total are
	// exercised with all four states plus the blank cell visible together — $N (a normal rounded
	// cost), $1187 (a big cost), <$1 (a real sub-dollar cost), — (a subscription $0), and blank
	// (nil Usage, the common case worth checking for alignment). runs[0]'s cost is kept identical
	// to the detail-running scene, which reuses this same run id.
	runs[0].Usage = &apitypes.UsageDTO{CostUSD: 9.55, InputTokens: 2_400_000, CacheReadTokens: 14_200_000, CacheCreationTokens: 120_000, OutputTokens: 88_400}          // → $10
	runs[2].Usage = &apitypes.UsageDTO{CostUSD: 1187.0, InputTokens: 40_000_000, CacheReadTokens: 900_000_000, CacheCreationTokens: 2_000_000, OutputTokens: 3_200_000} // → $1187
	runs[5].Usage = &apitypes.UsageDTO{CostUSD: 0.32, InputTokens: 8_000, CacheReadTokens: 40_000, CacheCreationTokens: 500, OutputTokens: 900}                         // → <$1
	runs[6].Usage = &apitypes.UsageDTO{CostUSD: 0, InputTokens: 120_000, CacheReadTokens: 300_000, CacheCreationTokens: 0, OutputTokens: 5_000}                         // → —
	return runs
}

// milestoneList is a representative frozen milestone list shared by the board's
// milestone-structured run and the detail-running scene, so the two stay coherent (#379).
var milestoneList = []apitypes.Milestone{
	{ID: "m1", Title: "Wire headroom into the poll loop"},
	{ID: "m2", Title: "Clamp the near-cap branch at 10%"},
	{ID: "m3", Title: "Add the regression sweep"},
	{ID: "m4", Title: "Update the scheduler docs"},
}

// boardMeters is a representative set of the viewer's own per-token rate-limit meters,
// mirroring demo.go's SelfMeters: the default "personal" token (ok/warn) and the non-default
// "meta" token (danger/warn) both show, "unlisted" is readable but hidden, "throttled" is
// dropped (status != "ok"). Shared by the ux-lab board frame so the strip is exercised (#519).
func boardMeters() []apitypes.TokenRateLimitDTO {
	return []apitypes.TokenRateLimitDTO{
		{SecretID: "sec-personal", Label: "personal", IsDefault: true, Limits: apitypes.RateLimitDTO{
			Status: "ok", FiveHour: &apitypes.RateLimitWindow{Pct: 35}, SevenDay: &apitypes.RateLimitWindow{Pct: 62}}},
		{SecretID: "sec-meta", Label: "meta", Limits: apitypes.RateLimitDTO{
			Status: "ok", FiveHour: &apitypes.RateLimitWindow{Pct: 88}, SevenDay: &apitypes.RateLimitWindow{Pct: 44}}},
		{SecretID: "sec-unlisted", Label: "unlisted", Limits: apitypes.RateLimitDTO{
			Status: "ok", FiveHour: &apitypes.RateLimitWindow{Pct: 12}, SevenDay: &apitypes.RateLimitWindow{Pct: 20}}},
		{SecretID: "sec-throttled", Label: "throttled", Limits: apitypes.RateLimitDTO{Status: "unavailable"}},
	}
}

func boardPopulated(dark bool, now time.Time) string {
	fake := &uzicli.FakeClient{Runs: boardRuns(now)}
	m := uxModel(fake, "", dark)
	m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
	// >1 token so the own board clears the credential gate (PRD #295) and the column renders.
	m = step(m, secretsMsg{count: 2})
	// The viewer's own rate-limit meters + the sidebar selection so the rate-limit strip renders
	// under the wordmark (#519).
	m = step(m, rateLimitsMsg{tokens: boardMeters()})
	m = step(m, settingsMsg{settings: apitypes.UserSettingsDTO{SidebarTokenIds: []string{"sec-meta"}}})
	return m.View().Content
}

func boardEmpty(dark bool) string {
	m := uxModel(&uzicli.FakeClient{}, "", dark)
	m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: nil})
	return m.View().Content
}

func boardAdmin(dark bool) string {
	fake := &uzicli.FakeClient{}
	m := uxModel(fake, "", dark)
	now := time.Now()
	m = key(m, keyAdmin)
	// The admin factory board ALWAYS shows the credential column (PRD #295), naming which account
	// each user's run billed — meta / personal labels, drawn muted with no dot.
	m = step(m, boardRunsMsg{reqID: m.board.waitID, admin: true, runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "a1b2c3d4-1111", Kind: "issue", Status: "running", IssueTitle: "Add rate-limit headroom to the scheduler poll", CreatedAt: now.Add(-4 * time.Minute), AnthropicSecretID: sp("sec-meta"), AnthropicSecretLabel: sp("meta"), AnthropicSelectReason: sp("auto")}, OwnerEmail: sp("dana@example.com")},
		{RunDTO: apitypes.RunDTO{ID: "c3d4e5f6-1111", Kind: "issue", Status: "claimed", IssueTitle: "Refactor the forge sync loop", Health: "stalled", CreatedAt: now.Add(-51 * time.Minute), AnthropicSecretID: sp("sec-personal"), AnthropicSecretLabel: sp("personal"), AnthropicSelectReason: sp("pool_stale")}, OwnerEmail: sp("priya@example.com")},
		{RunDTO: apitypes.RunDTO{ID: "b2c3d4e5-1111", Kind: "ci_fix", Status: "awaiting_approval", IssueTitle: "Fix flaky pipeline on main", CreatedAt: now.Add(-2 * time.Minute), AnthropicSecretID: sp("sec-meta"), AnthropicSecretLabel: sp("meta"), AnthropicSelectReason: sp("auto")}, OwnerEmail: sp("sam@example.com")},
	}})
	return m.View().Content
}

func boardFilter(dark bool, now time.Time) string {
	fake := &uzicli.FakeClient{Runs: boardRuns(now)}
	m := uxModel(fake, "", dark)
	m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: fake.Runs})
	m = key(m, keyFilter)
	for _, k := range []string{"f", "i", "x"} {
		m = key(m, k)
	}
	return m.View().Content
}

// boardPlanning renders a board carrying a planning run (running + IsPlanning) beside a
// plain running run and a stalled run, so the indigo "planning" chip and its hollow spine
// glyph can be compared against running's green/filled dot offline.
func boardPlanning(dark bool, now time.Time) string {
	fake := &uzicli.FakeClient{}
	m := uxModel(fake, "", dark)
	m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "d0e1f2a3-1111-2222-3333-444444444444", Kind: "issue", Status: "running", IsPlanning: true, IssueTitle: "Draft the plan for webhook delivery retries", CreatedAt: now.Add(-90 * time.Second)}},
		{RunDTO: apitypes.RunDTO{ID: "a1b2c3d4-1111-2222-3333-444444444444", Kind: "issue", Status: "running", IssueTitle: "Add rate-limit headroom to the scheduler poll", CreatedAt: now.Add(-4 * time.Minute)}},
		{RunDTO: apitypes.RunDTO{ID: "c3d4e5f6-1111-2222-3333-444444444444", Kind: "issue", Status: "running", Health: "stalled", IssueTitle: "Refactor the forge sync loop for the GitHub driver", CreatedAt: now.Add(-51 * time.Minute)}},
	}})
	return m.View().Content
}

// boardRevising renders a board carrying a mid-"revise" replan run (issue #750: status stays
// awaiting_approval but IsRevising is true) beside a genuine plan-gate run. The revising run
// drops to ON THE FLOOR and is excluded from the ⚑ summary count, so the frame shows the fixed
// treatment — the cluster reads ⚑ 1 with two awaiting_approval runs on the board.
func boardRevising(dark bool, now time.Time) string {
	fake := &uzicli.FakeClient{}
	m := uxModel(fake, "", dark)
	m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "b2c3d4e5-1111-2222-3333-444444444444", Kind: "ci_fix", Status: "awaiting_approval", IssueTitle: "Fix flaky pipeline on main", CreatedAt: now.Add(-2 * time.Minute)}},
		{RunDTO: apitypes.RunDTO{ID: "d0e1f2a3-1111-2222-3333-444444444444", Kind: "issue", Status: "awaiting_approval", IssueTitle: "Re-plan webhook delivery retries after steer", CreatedAt: now.Add(-90 * time.Second)}, IsRevising: true},
		{RunDTO: apitypes.RunDTO{ID: "a1b2c3d4-1111-2222-3333-444444444444", Kind: "issue", Status: "running", IssueTitle: "Add rate-limit headroom to the scheduler poll", CreatedAt: now.Add(-4 * time.Minute)}},
	}})
	return m.View().Content
}

// boardMilestones renders an own board whose MILE micro-bar column is on show (#379): a
// run mid-flight (▰▰▱▱, 2 of 4 reported) beside one that has reported nothing yet, which
// draws an all-empty ▱▱▱ bar — the graphical 0/N, never –/N text. No credential labels, so
// the MILE column clears the width gate at the lab's 100 cols instead of being dropped.
func boardMilestones(dark bool, now time.Time) string {
	fake := &uzicli.FakeClient{}
	m := uxModel(fake, "", dark)
	m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "a1b2c3d4-1111", Kind: "issue", Status: "running", IssueTitle: "Add rate-limit headroom to the scheduler poll", CreatedAt: now.Add(-4 * time.Minute), Milestones: milestoneList, MilestonesCompleted: []string{"m1", "m2"}, MilestonesInProgress: []string{"m3", "m4"}}}, // two in flight (#1176)
		{RunDTO: apitypes.RunDTO{ID: "d4e5f6a7-1111", Kind: "issue", Status: "running", IssueTitle: "Port the judge to per-model usage folding", CreatedAt: now.Add(-1 * time.Minute), Milestones: []apitypes.Milestone{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}}}},                                                 // nil completed ⇒ never reported
		{RunDTO: apitypes.RunDTO{ID: "c9d0e1f2-1111", Kind: "issue", Status: "running", IssueTitle: "Tighten the retry backoff jitter", CreatedAt: now.Add(-12 * time.Minute)}},                                                                                                                               // no frozen list ⇒ no bar
	}})
	return m.View().Content
}

// ---- detail fixtures ------------------------------------------------------

const detailRunID = "a1b2c3d4-1111-2222-3333-444444444444"

// laneMsgs is a representative multi-lane transcript: the lead orchestrating, plus two
// live subagents with task labels.
func laneMsgs(now time.Time) []apitypes.MessageDTO {
	return []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "", "Planning the change. I'll split this into a scheduler tweak and a test, then dispatch a coder and a tester.", now.Add(-4*time.Minute)),
		msgDTO(2, "tool_use", "coder", "toolu_01aaaaaa3v6ptu", "scheduler headroom", "`Edit`", now.Add(-90*time.Second)),
		msgDTO(3, "text", "coder", "toolu_01aaaaaa3v6ptu", "scheduler headroom", "Adjusted `pollInterval` to back off when the usage window is within 10% of the cap. Running the unit tests now.", now.Add(-40*time.Second)),
		msgDTO(4, "tool_use", "tester", "toolu_01bbbbbb2k9xqf", "regression sweep", "`Bash`", now.Add(-8*time.Second)),
		// A lead usage frame carrying a cool/quiet context reading (pct 62, below the 70 molten
		// cutoff) so it renders un-accented (faint) — matching the issue's mock, which shows 62%.
		// This exercises the crew rail's inline context-window meter (#565) in the regenerated scenes.
		// Placed LAST so the fixture's slice order is ascending seq (1,2,3,4,5); addFrame appends in
		// slice order without sorting, matching production's always-ascending seq.
		leadCtxMsg(5, 124000, 200000, 62, now.Add(-30*time.Second)),
	}
}

func detailBase(dark bool, run apitypes.RunDTO, now time.Time, allow bool) tuiModel {
	fake := &uzicli.FakeClient{}
	m := uxModel(fake, detailRunID, dark)
	m = applyDetail(m, run, laneMsgs(now))
	// The viewer's own rate-limit meters + sidebar selection so the crew rail's stacked
	// account block renders under the milestones (#530). Reuses the board fixture's meters
	// (boardMeters) and the same sidebar selection, so the detail rail and the board strip
	// show the SAME accounts — the two surfaces share selectedRateMeters.
	m = step(m, rateLimitsMsg{tokens: boardMeters()})
	m = step(m, settingsMsg{settings: apitypes.UserSettingsDTO{SidebarTokenIds: []string{"sec-meta"}}})
	if allow {
		m = step(m, runInputsMsg{runID: detailRunID, err: nil})
	}
	return m
}

func withLiveStream(m tuiModel) tuiModel {
	return step(m, streamReadyMsg{runID: detailRunID, stream: uzicli.NewRunStream(context.Background(), nil)})
}

func detailRunning(dark bool, now time.Time) string {
	// A milestone-structured run so the crew rail's milestone block renders (#379), coherent
	// with the board's M2/4 for the same run id.
	run := apitypes.RunDTO{ID: detailRunID, Kind: "issue", Status: "running", Health: "ok",
		IssueTitle: "Add rate-limit headroom to the scheduler poll",
		// PRD #519: an issue id + url so the detail header renders the clickable #<iid> beside
		// the crumb, coherent with the board's #452 for this same run id.
		IssueIID:            ip(452),
		IssueWebURL:         sp("https://github.com/vtmocanu/uzi/issues/452"),
		StartedAt:           tp(now.Add(-4 * time.Minute)), // header elapsed WORK time (`● running · 4m`)
		Milestones:          milestoneList,
		MilestonesCompleted: []string{"m1", "m2"}, MilestonesInProgress: []string{"m3", "m4"}} // two in flight (#1176)
	// The credential label rides the right of the header's first line, before the transport tag
	// (PRD #295), coherent with the board's meta label for this same run id.
	run.AnthropicSecretID, run.AnthropicSecretLabel = sp("sec-meta"), sp("meta")
	// PRD #650: usage so the header's cost tag and the crew-rail SPEND block render, coherent with
	// the board's runs[0] for this same run id (identical cost value).
	run.Usage = &apitypes.UsageDTO{CostUSD: 9.55, InputTokens: 2_400_000, CacheReadTokens: 14_200_000, CacheCreationTokens: 120_000, OutputTokens: 88_400}
	m := detailBase(dark, run, now, true)
	m = withLiveStream(m)
	return m.View().Content
}

// detailMilestonesAttributed renders the crew rail with PRD #1224 per-milestone agent attribution:
// m3 and m4 are both in progress and BOTH declare the subagent working them, so each in-progress
// milestone carries its own DECLARED role + label now-line (railMilestoneAgentLines) rather than
// the single first-in-progress line. The live activity is the tester's regression-sweep frame
// (laneMsgs' latest tool_use), so m3 (tester) is the D3 unique-matching lane and additionally shows
// the live age; m4 (coder) shows its declared role + label only. Coherent with detailRunning's run
// (same id, milestone list and completed/in-progress sets) so the two scenes read as one run.
func detailMilestonesAttributed(dark bool, now time.Time) string {
	run := apitypes.RunDTO{ID: detailRunID, Kind: "issue", Status: "running", Health: "ok",
		IssueTitle:           "Add rate-limit headroom to the scheduler poll",
		IssueIID:             ip(452),
		IssueWebURL:          sp("https://github.com/vtmocanu/uzi/issues/452"),
		StartedAt:            tp(now.Add(-4 * time.Minute)),
		Milestones:           milestoneList,
		MilestonesCompleted:  []string{"m1", "m2"},
		MilestonesInProgress: []string{"m3", "m4"},
		MilestonesAgents: []apitypes.MilestoneAgent{
			{ID: "m3", Agent: "tester", AgentLabel: "regression sweep"},
			{ID: "m4", Agent: "coder", AgentLabel: "scheduler docs"},
		}}
	run.AnthropicSecretID, run.AnthropicSecretLabel = sp("sec-meta"), sp("meta")
	run.Usage = &apitypes.UsageDTO{CostUSD: 9.55, InputTokens: 2_400_000, CacheReadTokens: 14_200_000, CacheCreationTokens: 120_000, OutputTokens: 88_400}
	m := detailBase(dark, run, now, true)
	m = withLiveStream(m)
	return m.View().Content
}

// detailPlanning mirrors detailRunning but with IsPlanning set, so the detail header
// renders the indigo "planning" chip rather than the running one.
func detailPlanning(dark bool, now time.Time) string {
	run := apitypes.RunDTO{ID: detailRunID, Kind: "issue", Status: "running", IsPlanning: true, Health: "ok",
		IssueTitle: "Draft the plan for webhook delivery retries"}
	m := detailBase(dark, run, now, true)
	m = withLiveStream(m)
	return m.View().Content
}

func detailFocusTranscript(dark bool, now time.Time) string {
	run := apitypes.RunDTO{ID: detailRunID, Kind: "issue", Status: "running", Health: "ok",
		IssueTitle: "Add rate-limit headroom to the scheduler poll",
		StartedAt:  tp(now.Add(-4 * time.Minute))} // header elapsed WORK time (`● running · 4m`)
	m := detailBase(dark, run, now, true)
	m = withLiveStream(m)
	m = key(m, "l") // focus the transcript pane
	return m.View().Content
}

func detailPaused(dark bool, now time.Time) string {
	run := apitypes.RunDTO{ID: detailRunID, Kind: "issue", Status: "running", Health: "ok",
		IssueTitle: "Add rate-limit headroom to the scheduler poll",
		StartedAt:  tp(now.Add(-4 * time.Minute))} // header elapsed WORK time (`● running · 4m`)
	lines := []string{
		"Planning the change: a scheduler backoff plus a near-cap test.",
		"Dispatched a coder and a tester; watching for the first diff.",
		"Coder reports the backoff is in. Reviewing the near-cap branch.",
		"Asked the tester to add a boundary case before I sign off.",
		"Tester is green on the near-cap case. Reading the full diff.",
		"The constant should be shared with the poller; sending it back.",
		"Coder extracted the shared constant. Re-running the sweep.",
		"Sweep green. Preparing the MR against a feature branch.",
	}
	var msgs []apitypes.MessageDTO
	for i, ln := range lines {
		msgs = append(msgs, msgDTO(int32(i+1), "text", "lead", "", "", ln, now.Add(-time.Duration(len(lines)-i)*time.Minute)))
	}
	fake := &uzicli.FakeClient{}
	m := uxModel(fake, detailRunID, dark)
	m.height = 22 // smaller viewport so the transcript overflows and can be scrolled back
	m = applyDetail(m, run, msgs)
	m = step(m, runInputsMsg{runID: detailRunID, err: nil})
	m = withLiveStream(m)
	m = key(m, "l") // focus the transcript
	for i := 0; i < 4; i++ {
		m = key(m, "k") // scroll up → detach follow → PAUSED ↓4 new
	}
	return m.View().Content
}

func detailStalled(dark bool, now time.Time) string {
	run := apitypes.RunDTO{ID: detailRunID, Kind: "issue", Status: "running", Health: "stalled",
		IssueTitle: "Refactor the forge sync loop for the GitHub driver",
		StartedAt:  tp(now.Add(-51 * time.Minute))} // header elapsed WORK time (`▲ stalled · 51m`)
	m := detailBase(dark, run, now, true)
	m = withLiveStream(m)
	return m.View().Content
}

func detailAwaitingApproval(dark bool, now time.Time) string {
	run := apitypes.RunDTO{ID: detailRunID, Kind: "issue", Status: "awaiting_approval", Health: "ok",
		IssueTitle: "Add rate-limit headroom to the scheduler poll",
		StartedAt:  tp(now.Add(-2 * time.Minute))} // header elapsed WORK time beside the ⚑ token
	msgs := []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "", "## Plan\n\n1. Back off `pollInterval` when the usage window is within 10% of the cap.\n2. Add a unit test for the near-cap case.\n3. Open an MR against a feature branch.\n\nReady for approval.", now.Add(-2*time.Minute)),
	}
	fake := &uzicli.FakeClient{}
	m := uxModel(fake, detailRunID, dark)
	m = applyDetail(m, run, msgs)
	m = step(m, runInputsMsg{runID: detailRunID, err: nil})
	m = withLiveStream(m)
	return m.View().Content
}

func detailAwaitingInput(dark bool, now time.Time) string {
	run := apitypes.RunDTO{ID: detailRunID, Kind: "issue", Status: "awaiting_input", Health: "ok",
		IssueTitle: "Clarify the target branch for the fix",
		StartedAt:  tp(now.Add(-1 * time.Minute))} // header elapsed WORK time beside the ✎ token
	msgs := []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "", "Which branch should the MR target: the default branch, or a release branch? I'll wait for your answer before opening it.", now.Add(-1*time.Minute)),
	}
	fake := &uzicli.FakeClient{}
	m := uxModel(fake, detailRunID, dark)
	m = applyDetail(m, run, msgs)
	m = step(m, runInputsMsg{runID: detailRunID, err: nil})
	m = withLiveStream(m)
	return m.View().Content
}

func detailLimitWait(dark bool, now time.Time) string {
	run := apitypes.RunDTO{ID: detailRunID, Kind: "issue", Status: "limit_wait", Health: "ok",
		IssueTitle:    "Port the judge to per-model usage folding",
		StartedAt:     tp(now.Add(-22 * time.Minute)), // header elapsed WORK time beside the ~ token
		RateLimitType: sp("five_hour"), RetryNotBefore: tp(now.Add(42 * time.Minute)), LimitWaitCount: 2}
	m := detailBase(dark, run, now, true)
	m = withLiveStream(m)
	return m.View().Content
}

func detailDegraded(dark bool, now time.Time) string {
	run := apitypes.RunDTO{ID: detailRunID, Kind: "issue", Status: "running", Health: "ok",
		IssueTitle: "Add rate-limit headroom to the scheduler poll",
		StartedAt:  tp(now.Add(-4 * time.Minute))} // header elapsed WORK time (`● running · 4m`)
	m := detailBase(dark, run, now, true)
	m = step(m, streamReadyMsg{runID: detailRunID, err: uzicli.Exitf(uzicli.ExitUnreachable, "dial tcp: connection refused")})
	return m.View().Content
}

func detailSteerTyping(dark bool, now time.Time) string {
	run := apitypes.RunDTO{ID: detailRunID, Kind: "issue", Status: "running", Health: "ok",
		IssueTitle: "Add rate-limit headroom to the scheduler poll",
		StartedAt:  tp(now.Add(-4 * time.Minute))} // header elapsed WORK time (`● running · 4m`)
	m := detailBase(dark, run, now, true)
	m = withLiveStream(m)
	m = key(m, "f")
	for _, r := range "add a test for the near-cap boundary" {
		if r == ' ' {
			m = key(m, keySpaceName)
			continue
		}
		m = key(m, string(r))
	}
	return m.View().Content
}

func detailSteerConfirm(dark bool, now time.Time) string {
	run := apitypes.RunDTO{ID: detailRunID, Kind: "issue", Status: "running", Health: "ok",
		IssueTitle: "Add rate-limit headroom to the scheduler poll",
		StartedAt:  tp(now.Add(-4 * time.Minute))} // header elapsed WORK time (`● running · 4m`)
	m := detailBase(dark, run, now, true)
	m = withLiveStream(m)
	m = key(m, "x")
	return m.View().Content
}

func detailSteerQueue(dark bool, now time.Time) string {
	run := apitypes.RunDTO{ID: detailRunID, Kind: "issue", Status: "running", Health: "ok",
		IssueTitle: "Add rate-limit headroom to the scheduler poll",
		StartedAt:  tp(now.Add(-4 * time.Minute))} // header elapsed WORK time (`● running · 4m`)
	m := detailBase(dark, run, now, false)
	m = step(m, runInputsMsg{runID: detailRunID, err: nil, inputs: []apitypes.SteerInputDTO{
		{ID: 1, Body: sp("prefer table-driven tests here"), CreatedAt: now.Add(-3 * time.Minute), ConsumedAt: tp(now.Add(-2 * time.Minute))},
		{ID: 2, Body: sp("also cover the seven-day window"), CreatedAt: now.Add(-30 * time.Second)},
	}})
	m = withLiveStream(m)
	return m.View().Content
}

// ---- review fixtures ------------------------------------------------------

func reviewOverlay(dark bool, now time.Time) string {
	run := apitypes.RunDTO{ID: detailRunID, Kind: "issue", Status: "completed", Health: "ok",
		IssueTitle: "Add rate-limit headroom to the scheduler poll"}
	review := &apitypes.ReviewDTO{
		ID: "rev-1", TargetRunID: detailRunID, Verdict: "issues",
		SummaryMd: "The change is sound and well-tested, but the near-cap backoff constant is duplicated between the scheduler and the poller, and one new export is never used outside its test.",
		Recommendations: []apitypes.RecommendationDTO{
			{ID: "9f2a1b3c", Category: "improve_uzi", Target: "api/internal/poller/scheduler.go", Confidence: "high", RationaleMd: "`nearCapRatio` is defined here and again in `poller.go`. Extract one constant so the two cannot drift."},
			{ID: "7c4d2e1a", Category: "adjust_template", Target: "coder", Confidence: "medium", RationaleMd: "The coder added an exported helper used only by its own test. Prefer an unexported one."},
			{ID: "3b8e5f0d", Category: "enable_tool", Target: "worker: gofumpt", Confidence: "low", RationaleMd: "Formatting drift showed up twice this run; consider enabling the formatter tool."},
		},
		Dispositions: []apitypes.DispositionDTO{
			{Category: "improve_uzi", Target: "api/internal/poller/scheduler.go", Status: "done", SetAt: now.Add(-time.Minute)},
		},
		Triage: apitypes.TriageDTO{Total: 3, Todo: 2, Done: 1},
	}
	fake := &uzicli.FakeClient{}
	m := uxModel(fake, detailRunID, dark)
	m = applyDetail(m, run, laneMsgs(now))
	m = key(m, "v")
	m = step(m, reviewLoadedMsg{runID: detailRunID, review: review})
	return m.View().Content
}

func reviewPending(dark bool, now time.Time) string {
	run := apitypes.RunDTO{ID: detailRunID, Kind: "issue", Status: "completed", Health: "ok",
		IssueTitle: "Add rate-limit headroom to the scheduler poll"}
	fake := &uzicli.FakeClient{}
	m := uxModel(fake, detailRunID, dark)
	m = applyDetail(m, run, laneMsgs(now))
	m = key(m, "v")
	m = step(m, reviewLoadedMsg{runID: detailRunID, review: nil, pendingJudge: &apitypes.PendingJudgeDTO{State: "running", EnqueuedAt: now.Add(-30 * time.Second)}})
	return m.View().Content
}

// ---- pulls fixtures -------------------------------------------------------

// pullsPopulated renders the forge `pulls` list (PRD #1255 M4a) with a realistic spread of
// open PRs across the three bands (NEEDS YOU / IN FLIGHT / READY), scoped to one enabled repo.
func pullsPopulated(dark bool, now time.Time) string {
	repo := apitypes.RepoDTO{ID: "r1", PathWithNamespace: "vtmocanu/uzi", Enabled: true,
		WebURL: "https://github.com/vtmocanu/uzi"}
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{repo}, PullsResult: samplePulls(now)}
	m := uxModel(fake, "", dark)
	m = step(m, reposMsg{repos: fake.Repos})
	m = key(m, keyViewPulls)
	m = step(m, pullsMsg{reqID: m.pulls.waitID, pulls: samplePulls(now)})
	return m.View().Content
}

// pullsEmpty renders the `pulls` list for a repo with no open PRs (the empty state).
func pullsEmpty(dark bool, now time.Time) string {
	repo := apitypes.RepoDTO{ID: "r1", PathWithNamespace: "vtmocanu/uzi", Enabled: true,
		WebURL: "https://github.com/vtmocanu/uzi"}
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{repo}}
	m := uxModel(fake, "", dark)
	m = step(m, reposMsg{repos: fake.Repos})
	m = key(m, keyViewPulls)
	m = step(m, pullsMsg{reqID: m.pulls.waitID, pulls: nil})
	return m.View().Content
}

// ---- ci fixtures ----------------------------------------------------------

// ciPopulated renders the forge `ci` list (PRD #1255 M4b) with a realistic spread of CI runs
// across the three bands (RUNNING / FAILED / RECENT), scoped to one enabled repo. The RUNNING
// rows carry the `▰▱ done/total` jobs micro-bar (D8).
func ciPopulated(dark bool, now time.Time) string {
	repo := apitypes.RepoDTO{ID: "r1", PathWithNamespace: "vtmocanu/uzi", Enabled: true,
		WebURL: "https://github.com/vtmocanu/uzi"}
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{repo}, CIRunsResult: sampleCIRuns(now)}
	m := uxModel(fake, "", dark)
	m = step(m, reposMsg{repos: fake.Repos})
	m = key(m, keyViewCI)
	m = step(m, ciMsg{reqID: m.ci.waitID, runs: sampleCIRuns(now)})
	return m.View().Content
}

// ciUnsupported renders the `ci` list's degrade state: a forge version without the runs endpoint
// returns an empty list plus a sentence (ErrForgeVersionUnsupported), drawn verbatim (D5/R2).
func ciUnsupported(dark bool, now time.Time) string {
	repo := apitypes.RepoDTO{ID: "r1", PathWithNamespace: "acme/legacy", Enabled: true,
		WebURL: "https://forge.example/acme/legacy"}
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{repo},
		CIRunsUnsupported: "CI runs need Forgejo v16.0.0 or newer on this connection."}
	m := uxModel(fake, "", dark)
	m = step(m, reposMsg{repos: fake.Repos})
	m = key(m, keyViewCI)
	m = step(m, ciMsg{reqID: m.ci.waitID, unsupported: fake.CIRunsUnsupported})
	return m.View().Content
}

// ---- pr drill-in fixtures -------------------------------------------------

// openPRScene drives the model to the PR drill-in (PRD #1255 M5) the real way: repos load, the
// user opens the pulls list, the list lands, enter opens the PR view (minting the first fetch), and
// the detail reply is applied. It returns the rendered frame.
func openPRScene(dark bool, detail apitypes.PullDetailDTO) string {
	repo := apitypes.RepoDTO{ID: "r1", PathWithNamespace: "vtmocanu/uzi", Enabled: true,
		WebURL: "https://github.com/vtmocanu/uzi"}
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{repo}, PullDetailResult: detail}
	m := uxModel(fake, "", dark)
	m = step(m, reposMsg{repos: fake.Repos})
	m = key(m, keyViewPulls)
	m = step(m, pullsMsg{reqID: m.pulls.waitID, pulls: []apitypes.PullDTO{detail.PullDTO}})
	m = key(m, keyEnter) // open the PR drill-in
	m = step(m, prMsg{reqID: m.pr.waitID, detail: detail})
	return m.View().Content
}

func prBasePull(now time.Time) apitypes.PullDTO {
	return apitypes.PullDTO{
		IID: 1254, Title: "Completion interlock M4–M6: rollout switch + permit finalize",
		Author: "uzi-bot", SourceBranch: "agent/issue-1246", TargetBranch: "main",
		HeadSHA: "f8064ef5deadbeef", WebURL: "https://github.com/vtmocanu/uzi/pull/1254",
		RunID:     sp("f8064ef5-1111-2222-3333-444444444444"),
		Additions: 842, Deletions: 131, Commits: 14,
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-3 * time.Minute),
	}
}

func ckURL(job string) string {
	return "https://github.com/vtmocanu/uzi/actions/runs/34677104577/job/" + job
}

// prLive renders the PR view while checks are still landing (the mock's live frame): five in
// progress, five passed — so the header rollup reads `● 5 pending · ✓ 5/10`.
func prLive(dark bool, now time.Time) string {
	pull := prBasePull(now)
	pull.ReviewDecision = "review_required"
	checks := []apitypes.CheckDTO{
		{Name: "CodeRabbit", Status: "in_progress", Description: "Waiting for status — Review in progress", Source: "coderabbitai", WebURL: ckURL("103508797991"), StartedAt: now.Add(-3 * time.Minute)},
		{Name: "CodeQL / Analyze (javascript-typescript)", Status: "in_progress", Description: "in progress", StartedAt: now.Add(-68 * time.Second)},
		{Name: "CI / test-api", Status: "in_progress", Description: "in progress", StartedAt: now.Add(-130 * time.Second)},
		{Name: "CI / test-web", Status: "in_progress", Description: "in progress", StartedAt: now.Add(-96 * time.Second)},
		{Name: "CodeQL", Status: "queued", Description: "waiting on 1 analysis"},
		{Name: "CodeQL / Analyze (actions)", Status: "completed", Conclusion: "success", StartedAt: now.Add(-90 * time.Second), CompletedAt: now.Add(-45 * time.Second)},
		{Name: "CodeQL / Analyze (go)", Status: "completed", Conclusion: "success", StartedAt: now.Add(-5 * time.Minute), CompletedAt: now.Add(-3 * time.Minute)},
		{Name: "CodeQL / Analyze (python)", Status: "completed", Conclusion: "success", StartedAt: now.Add(-110 * time.Second), CompletedAt: now.Add(-55 * time.Second)},
		{Name: "CI / lint-api", Status: "completed", Conclusion: "success", StartedAt: now.Add(-4 * time.Minute), CompletedAt: now.Add(-90 * time.Second)},
		{Name: "KinD Smoke", Status: "completed", Conclusion: "success", StartedAt: now.Add(-30 * time.Second), CompletedAt: now.Add(-14 * time.Second)},
	}
	detail := apitypes.PullDetailDTO{
		PullDTO: pull,
		Checks:  checks,
		Reviews: []apitypes.PullReviewDTO{
			{Author: "coderabbitai", State: "commented", SubmittedAt: now.Add(-3 * time.Minute)},
			{Author: "vtmocanu", State: "pending", SubmittedAt: now.Add(-2 * time.Hour)},
		},
		Merge: apitypes.MergeStateDTO{Conflicts: bp(false), RequiredChecksPassed: false, MergeableState: "blocked"},
	}
	return openPRScene(dark, detail)
}

// prChangesRequested renders the settled frame: all ten checks green, CodeRabbit requested changes
// — so the rollup reads `✓ 10/10 · ✎ changes requested` and MERGE is blocked on the review.
func prChangesRequested(dark bool, now time.Time) string {
	pull := prBasePull(now)
	pull.ReviewDecision = "changes_requested"
	var checks []apitypes.CheckDTO
	names := []string{"CodeRabbit", "CodeQL", "CodeQL / Analyze (actions)", "CodeQL / Analyze (go)",
		"CodeQL / Analyze (python)", "CI / lint-api", "CI / test-api", "CI / test-web",
		"CI / lint-web", "KinD Smoke"}
	for i, n := range names {
		checks = append(checks, apitypes.CheckDTO{Name: n, Status: "completed", Conclusion: "success",
			StartedAt: now.Add(-time.Duration(5+i) * time.Minute), CompletedAt: now.Add(-time.Duration(i) * time.Minute)})
	}
	checks[0].Description = "Review completed"
	checks[0].WebURL = ckURL("103508797991")
	detail := apitypes.PullDetailDTO{
		PullDTO: pull,
		Checks:  checks,
		Reviews: []apitypes.PullReviewDTO{
			{Author: "coderabbitai", State: "changes_requested", SubmittedAt: now.Add(-30 * time.Second)},
			{Author: "vtmocanu", State: "pending", SubmittedAt: now.Add(-2 * time.Hour)},
		},
		Merge: apitypes.MergeStateDTO{Conflicts: bp(false), RequiredChecksPassed: true,
			BlockedReason: "changes requested · admin override", MergeableState: "blocked"},
	}
	return openPRScene(dark, detail)
}

// prFailing renders a PR with a failing check (the rollup reads `✗ 1 failing`) and a conflict with
// the target branch, so both NEEDS-YOU carriers are on show.
func prFailing(dark bool, now time.Time) string {
	pull := prBasePull(now)
	pull.ReviewDecision = "review_required"
	pull.Conflicts = bp(true)
	checks := []apitypes.CheckDTO{
		{Name: "CI / lint-api", Status: "completed", Conclusion: "failure", Description: "golangci-lint: 2 issues", WebURL: ckURL("103508700001"), StartedAt: now.Add(-6 * time.Minute), CompletedAt: now.Add(-4 * time.Minute)},
		{Name: "CI / test-api", Status: "in_progress", Description: "in progress", StartedAt: now.Add(-3 * time.Minute)},
		{Name: "CodeQL / Analyze (go)", Status: "completed", Conclusion: "success", StartedAt: now.Add(-5 * time.Minute), CompletedAt: now.Add(-3 * time.Minute)},
		{Name: "KinD Smoke", Status: "completed", Conclusion: "skipped", Description: "skipped"},
	}
	detail := apitypes.PullDetailDTO{
		PullDTO: pull,
		Checks:  checks,
		Reviews: []apitypes.PullReviewDTO{
			{Author: "vtmocanu", State: "pending", SubmittedAt: now.Add(-90 * time.Minute)},
		},
		Merge: apitypes.MergeStateDTO{Conflicts: bp(true), RequiredChecksPassed: false,
			BlockedReason: "1 required check failing", MergeableState: "dirty"},
	}
	return openPRScene(dark, detail)
}

// ---- ci run drill-in fixtures ---------------------------------------------

// ciRunRunning drives the model to the CI-run drill-in (PRD #1255 M6) the real way: repos load, the
// user opens the ci list, the list lands, enter opens the CI-run view (minting the first fetch), and
// the detail reply (jobs + steps, one running) is applied. The selected job (cursor 0) expands its
// steps beneath it and draws its faint ↗ URL line.
func ciRunRunning(dark bool, now time.Time) string {
	repo := apitypes.RepoDTO{ID: "r1", PathWithNamespace: "vtmocanu/uzi", Enabled: true,
		WebURL: "https://github.com/vtmocanu/uzi"}
	detail := sampleCIRunDetail(now)
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{repo}, CIRunDetailResult: detail,
		CIRunsResult: []apitypes.CIRunDTO{detail.CIRunDTO}}
	m := uxModel(fake, "", dark)
	m = step(m, reposMsg{repos: fake.Repos})
	m = key(m, keyViewCI)
	m = step(m, ciMsg{reqID: m.ci.waitID, runs: []apitypes.CIRunDTO{detail.CIRunDTO}})
	m = key(m, keyEnter) // open the CI-run drill-in
	m = step(m, ciRunMsg{reqID: m.cirun.waitID, gen: m.cirun.gen, detail: detail})
	return m.View().Content
}

// ---- overlays -------------------------------------------------------------

func helpFrame(dark bool) string {
	m := uxModel(&uzicli.FakeClient{}, "", dark)
	m = key(m, keyHelp)
	return m.View().Content
}

func quitFrame(dark bool) string {
	m := uxModel(&uzicli.FakeClient{}, "", dark)
	m = key(m, keyQuit)
	return m.View().Content
}
