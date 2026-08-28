package main

// `uzi tui --demo` — a self-contained showcase of the redesigned TUI (PRD #325 M7, D1).
//
// It drives the REAL shipped tuiModel with an in-memory uzicli.FakeClient and a ticker that
// injects synthetic frames, so the board, the plan gate, the focusable panes and follow-live
// are all drivable without a server, a DB or auth. Because it runs the shipped views (not a
// second implementation), it cannot drift from what users actually get — which is why the
// old factoryui prototype was retired in favour of this.

import (
	"context"
	"encoding/json"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// runTUIDemo runs the interactive program over the demo model. Called from `uzi tui --demo`.
func runTUIDemo(ctx context.Context, env Env) error {
	m := demoModel{tuiModel: newTUIModel(ctx, newDemoClient(), "")}
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(env.Stdin), tea.WithOutput(env.Stdout))
	if _, err := p.Run(); err != nil {
		return uzicli.Exitf(uzicli.ExitGeneric, "tui demo: %v", err)
	}
	return nil
}

// demoModel wraps the shipped tuiModel and adds a live ticker. It forwards every message to
// the real model (re-wrapping the result), and on each tick injects one synthetic frame into
// the currently-open LIVE run via the same streamEventsMsg a real socket would deliver — so
// the transcript grows under the reader and follow-live (⇣ following / ⏸ N new · g ⇣) is
// actually drivable.
type demoModel struct {
	tuiModel
	liveIdx int
}

var _ tea.Model = demoModel{}

type demoTickMsg struct{}

func demoTickCmd() tea.Cmd {
	return tea.Tick(1200*time.Millisecond, func(time.Time) tea.Msg { return demoTickMsg{} })
}

func (d demoModel) Init() tea.Cmd {
	return tea.Batch(d.tuiModel.Init(), demoTickCmd())
}

func (d demoModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(demoTickMsg); ok {
		var cmd tea.Cmd
		if d.view == viewDetail && d.detail.loaded && isLiveRunStatus(d.detail.run.Status) {
			nm, c := d.tuiModel.Update(streamEventsMsg{runID: d.detail.runID, events: []apitypes.RunEventDTO{d.nextLiveFrame()}})
			d.tuiModel = nm.(tuiModel)
			cmd = c
			d.liveIdx++
		}
		return d, tea.Batch(cmd, demoTickCmd())
	}
	nm, cmd := d.tuiModel.Update(msg)
	d.tuiModel = nm.(tuiModel)
	return d, cmd
}

// nextLiveFrame is one synthetic message for the SELECTED lane, seq-numbered above every
// frame already loaded so the model's seq-dedup keeps it.
func (d demoModel) nextLiveFrame() apitypes.RunEventDTO {
	role := laneLead
	if lane, ok := d.detail.selectedLane(); ok {
		role = lane.Role
	}
	var maxSeq int32
	for _, f := range d.detail.frames {
		if f.Seq > maxSeq {
			maxSeq = f.Seq
		}
	}
	at := time.Now()
	payload, _ := json.Marshal(map[string]string{"text": demoLiveLines[d.liveIdx%len(demoLiveLines)]})
	return apitypes.RunEventDTO{
		Type: uzicli.RunEventTypeMessage, Seq: maxSeq + 1, Kind: "text",
		Agent: &role, CreatedAt: &at, Payload: payload,
	}
}

var demoLiveLines = []string{
	"Reading api/internal/poller/scheduler.go",
	"Backoff applied; the near-cap branch clamps at 10% now.",
	"Running go test ./internal/poller/...",
	"Tests green. Preparing the diff for review.",
	"Editing scheduler_test.go, adding the near-cap case.",
}

// ---- seeded fixtures ------------------------------------------------------

// boolPtr returns a pointer to b, for the nullable *bool DTO fields (e.g.
// UserSettingsDTO.MrReworkEnabled, where nil = the default-ON state).
func boolPtr(b bool) *bool { return &b }

func newDemoClient() *uzicli.FakeClient {
	now := time.Now()
	runs := demoRuns(now)
	byID := map[string]apitypes.RunDTO{}
	logs := map[string][]apitypes.MessageDTO{}
	inputs := map[string][]apitypes.SteerInputDTO{}
	for _, r := range runs {
		byID[r.ID] = r.RunDTO
		logs[r.ID] = demoLogs(now)
		// Present (empty) → RunInputs succeeds → the run reads as owner-steerable.
		inputs[r.ID] = []apitypes.SteerInputDTO{}
	}
	return &uzicli.FakeClient{
		Runs: runs, RunByID: byID, LogsByID: logs, InputsByID: inputs,
		// Two Anthropic tokens so the own board clears the >1-token gate (PRD #295) and shows
		// the credential column; the demo runs above carry meta/personal labels + reasons.
		Secrets: []apitypes.SecretDTO{
			{ID: "sec-personal", Kind: "anthropic", Label: "personal", IsDefault: true},
			{ID: "sec-meta", Kind: "anthropic", Label: "meta"},
		},
		// The viewer's own per-token meters drive the factory-floor rate-limit strip (PRD #519
		// F2), mirroring the web sidebar's selection. The default token always shows; the
		// non-default "meta" shows because it is listed in SidebarTokenIds below; "unlisted" is
		// readable but NOT shown (default:false and absent from the list), and "throttled" is
		// dropped entirely (status != "ok"). Percentages land on each tone band (ok/warn/danger).
		SelfMeters: []apitypes.TokenRateLimitDTO{
			{SecretID: "sec-personal", Label: "personal", IsDefault: true, Limits: apitypes.RateLimitDTO{
				Status: "ok", FiveHour: &apitypes.RateLimitWindow{Pct: 35}, SevenDay: &apitypes.RateLimitWindow{Pct: 62}}},
			{SecretID: "sec-meta", Label: "meta", Limits: apitypes.RateLimitDTO{
				Status: "ok", FiveHour: &apitypes.RateLimitWindow{Pct: 88}, SevenDay: &apitypes.RateLimitWindow{Pct: 44}}},
			{SecretID: "sec-unlisted", Label: "unlisted", Limits: apitypes.RateLimitDTO{
				Status: "ok", FiveHour: &apitypes.RateLimitWindow{Pct: 12}, SevenDay: &apitypes.RateLimitWindow{Pct: 20}}},
			{SecretID: "sec-throttled", Label: "throttled", Limits: apitypes.RateLimitDTO{Status: "unavailable"}},
		},
		// Only "meta" is promoted into the sidebar selection; "unlisted" stays hidden.
		// MrReworkEnabled false is the explicit opt-out (PRD #700 M6); a nil pointer
		// would be the default-ON state. Carried for decode fidelity — the TUI does
		// not render it, matching the DTO's other fidelity-only fields.
		Settings: apitypes.UserSettingsDTO{
			SidebarTokenIds: []string{"sec-meta"},
			MrReworkEnabled: boolPtr(false),
		},
		// StreamEvents nil: NewRunStream emits nothing and stays OPEN (so the detail reads
		// "live"); the ticker above supplies the live frames.
		StreamEvents: nil,
	}
}

func demoRuns(now time.Time) []apitypes.RunListItemDTO {
	sp := func(s string) *string { return &s }
	ip := func(n int64) *int64 { return &n }
	mk := func(id, kind, status, title, health string, verdict *string, todo int, age time.Duration) apitypes.RunListItemDTO {
		created := now.Add(-age)
		r := apitypes.RunListItemDTO{
			RunDTO: apitypes.RunDTO{ID: id, Kind: kind, Status: status, IssueTitle: title, Health: health,
				CreatedAt: created},
			JudgeVerdict: verdict, JudgeTodoCount: todo,
		}
		// Give every mk-built run a start stamp so the detail header shows elapsed WORK time
		// (`● running · 4m`): runDuration deliberately won't derive it from CreatedAt, which is
		// queue-wait age, not work time. A terminal run also gets a FinishedAt, so its header
		// shows a total wall time (`✓ done · 3h`) rather than a still-ticking clock. The
		// pre-approval planning run below is built inline WITHOUT a start — its honest state, it
		// has not begun implementing.
		started := created
		r.StartedAt = &started
		if terminalRunStatuses[status] {
			finished := now
			r.FinishedAt = &finished
		}
		return r
	}
	// A milestone-structured run (PRD #122) so `tui --demo` exercises the crew rail's
	// milestone block: 2 of 4 frozen milestones reported complete, one in progress.
	scheduler := mk("a1b2c3d4-1111-2222-3333-444444444444", "issue", "running", "Add rate-limit headroom to the scheduler poll", "", nil, 0, 4*time.Minute)
	scheduler.Milestones = []apitypes.Milestone{
		{ID: "m1", Title: "Wire the rate-limit headroom into the poll loop"},
		{ID: "m2", Title: "Clamp the near-cap branch at 10%"},
		{ID: "m3", Title: "Add the regression sweep"},
		{ID: "m4", Title: "Update the scheduler docs"},
	}
	scheduler.MilestonesCompleted = []string{"m1", "m2"}
	scheduler.MilestonesInProgress = []string{"m3"}
	// A realistic spread of credentials. The TUI board and detail show just the muted LABEL
	// (meta/personal); the select reason/mode is surfaced only by `uzi run <id>`, not the TUI, so
	// these reasons are realistic data rather than something the demo draws a dot or colour for.
	// meta on an auto pick with headroom, the common case.
	headroom := 27
	scheduler.AnthropicSecretID = sp("sec-meta")
	scheduler.AnthropicSecretLabel = sp("meta")
	scheduler.AnthropicSelectReason = sp("auto")
	scheduler.AnthropicHeadroomPct = &headroom
	scheduler.IssueIID = ip(418)
	scheduler.IssueWebURL = sp("https://github.com/vtmocanu/uzi/issues/418")

	// personal on a stale-pool fallback (auto declined, the default paid).
	sync := mk("c3d4e5f6-1111-2222-3333-444444444444", "issue", "running", "Refactor the forge sync loop for the GitHub driver", "stalled", nil, 0, 51*time.Minute)
	sync.AnthropicSecretID = sp("sec-personal")
	sync.AnthropicSecretLabel = sp("personal")
	sync.AnthropicSelectReason = sp("pool_stale")
	sync.IssueIID = ip(452)
	sync.IssueWebURL = sp("https://github.com/vtmocanu/uzi/issues/452")

	// personal on a best-of-pool pick (the pool is nearly exhausted).
	portJudge := mk("f6a7b8c9-1111-2222-3333-444444444444", "issue", "limit_wait", "Port the judge to per-model usage folding", "", nil, 0, 22*time.Minute)
	portJudge.AnthropicSecretID = sp("sec-personal")
	portJudge.AnthropicSecretLabel = sp("personal")
	portJudge.AnthropicSelectReason = sp("best_of_pool")
	portJudge.IssueIID = ip(463)
	portJudge.IssueWebURL = sp("https://github.com/vtmocanu/uzi/issues/463")

	// The completed OIDC-login run and the failed vault run are ISSUE-kind, so they carry a
	// clickable forge issue id too.
	oidc := mk("e5f6a7b8-1111-2222-3333-444444444444", "issue", "completed", "Wire the OIDC login button into the header", "", sp("ideal"), 0, 3*time.Hour)
	oidc.IssueIID = ip(519)
	oidc.IssueWebURL = sp("https://github.com/vtmocanu/uzi/issues/519")
	vault := mk("a7b8c9d0-1111-2222-3333-444444444444", "issue", "failed", "Migrate per-user secrets into the vault hierarchy", "", sp("issues"), 3, 5*time.Hour)
	vault.IssueIID = ip(477)
	vault.IssueWebURL = sp("https://github.com/vtmocanu/uzi/issues/477")

	return []apitypes.RunListItemDTO{
		// The inline planning run carries an issue id but NO web URL — it exercises the
		// "id shown but NOT clickable" path.
		{RunDTO: apitypes.RunDTO{ID: "d0e1f2a3-1111-2222-3333-444444444444", Kind: "issue", Status: "running", IsPlanning: true, IssueTitle: "Draft the plan for webhook delivery retries", CreatedAt: now.Add(-90 * time.Second), IssueIID: ip(488)}},
		scheduler,
		// The ci_fix run keeps nil IssueIID/IssueWebURL — the no-issue (`#`-less) path.
		mk("b2c3d4e5-1111-2222-3333-444444444444", "ci_fix", "awaiting_approval", "Fix flaky pipeline on main", "", nil, 0, 2*time.Minute),
		sync,
		oidc,
		portJudge,
		vault,
	}
}

func demoLogs(now time.Time) []apitypes.MessageDTO {
	sp := func(s string) *string { return &s }
	msg := func(seq int32, agent, inst, label, text string, age time.Duration) apitypes.MessageDTO {
		payload, _ := json.Marshal(map[string]string{"text": text})
		m := apitypes.MessageDTO{Seq: seq, Kind: "text", Payload: payload, CreatedAt: now.Add(-age)}
		if agent != "" {
			m.Agent = sp(agent)
		}
		if inst != "" {
			m.AgentInstance = sp(inst)
		}
		if label != "" {
			m.AgentLabel = sp(label)
		}
		return m
	}
	return []apitypes.MessageDTO{
		msg(1, "lead", "", "", "Planning the change. I'll split this into a scheduler tweak and a test, then dispatch a coder and a tester.", 4*time.Minute),
		msg(2, "coder", "toolu_3v6ptu", "scheduler headroom", "Adjusting pollInterval to back off near the cap.", 90*time.Second),
		msg(3, "tester", "toolu_2k9xqf", "regression sweep", "Running the regression sweep.", 8*time.Second),
	}
}
