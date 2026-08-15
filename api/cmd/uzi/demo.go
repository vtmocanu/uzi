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

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
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
// the transcript grows under the reader and follow-live (● FOLLOWING / ⏸ PAUSED ↓N new) is
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
		// StreamEvents nil: NewRunStream emits nothing and stays OPEN (so the detail reads
		// "live"); the ticker above supplies the live frames.
		StreamEvents: nil,
	}
}

func demoRuns(now time.Time) []apitypes.RunListItemDTO {
	sp := func(s string) *string { return &s }
	mk := func(id, kind, status, title, health string, verdict *string, todo int, age time.Duration) apitypes.RunListItemDTO {
		return apitypes.RunListItemDTO{
			RunDTO: apitypes.RunDTO{ID: id, Kind: kind, Status: status, IssueTitle: title, Health: health,
				CreatedAt: now.Add(-age)},
			JudgeVerdict: verdict, JudgeTodoCount: todo,
		}
	}
	return []apitypes.RunListItemDTO{
		mk("a1b2c3d4-1111-2222-3333-444444444444", "issue", "running", "Add rate-limit headroom to the scheduler poll", "", nil, 0, 4*time.Minute),
		mk("b2c3d4e5-1111-2222-3333-444444444444", "ci_fix", "awaiting_approval", "Fix flaky pipeline on main", "", nil, 0, 2*time.Minute),
		mk("c3d4e5f6-1111-2222-3333-444444444444", "issue", "running", "Refactor the forge sync loop for the GitHub driver", "stalled", nil, 0, 51*time.Minute),
		mk("e5f6a7b8-1111-2222-3333-444444444444", "issue", "completed", "Wire the OIDC login button into the header", "", sp("ideal"), 0, 3*time.Hour),
		mk("f6a7b8c9-1111-2222-3333-444444444444", "issue", "limit_wait", "Port the judge to per-model usage folding", "", nil, 0, 22*time.Minute),
		mk("a7b8c9d0-1111-2222-3333-444444444444", "issue", "failed", "Migrate per-user secrets into the vault hierarchy", "", sp("issues"), 3, 5*time.Hour),
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
