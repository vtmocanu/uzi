package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The roster a healthy worker reports for a repo shipping two agents.
func twoAgents() []RepoAgent {
	return []RepoAgent{
		{Name: "coder", Description: "Implements changes."},
		{Name: "reviewer", Description: "Reviews changes."},
	}
}

func repoAgentsJSON(t *testing.T, agents []RepoAgent) []byte {
	t.Helper()
	raw, err := json.Marshal(agents)
	if err != nil {
		t.Fatalf("marshal roster: %v", err)
	}
	return raw
}

// -------------------------------------------------------------------------
// Worker-reported roster: the API re-checks every cap the worker claims to hold
// -------------------------------------------------------------------------

func TestValidateRepoAgents(t *testing.T) {
	long := strings.Repeat("a", MaxAgentDescriptionLen+1)
	// The description cap is UTF-8 BYTES (Go len()), matching the worker (F3). These
	// prove the byte basis with multibyte text: '好' is 3 bytes / 1 rune, 'ă' (the
	// PRD's ~513-Romanian-diacritic trigger) is 2 bytes / 1 rune.
	multibyteOverBytes := strings.Repeat("好", 400)                      // 1200 bytes, 400 runes: over
	diacriticAtCap := strings.Repeat("ă", MaxAgentDescriptionLen/2)     // 1024 bytes, 512 runes: at the cap
	diacriticOverCap := strings.Repeat("ă", MaxAgentDescriptionLen/2+1) // 1026 bytes: one 2-byte rune over
	many := make([]RepoAgent, MaxRepoAgents+1)
	for i := range many {
		many[i] = RepoAgent{Name: "a" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Description: "x."}
	}

	cases := []struct {
		name    string
		agents  []RepoAgent
		wantErr bool
	}{
		{name: "nil roster is not a report", agents: nil},
		{name: "empty roster means detection found none", agents: []RepoAgent{}},
		{name: "well-formed roster", agents: twoAgents()},
		{name: "at the file cap", agents: many[:MaxRepoAgents]},
		{name: "over the file cap", agents: many, wantErr: true},
		{name: "uppercase name", agents: []RepoAgent{{Name: "Coder", Description: "x."}}, wantErr: true},
		{name: "path traversal name", agents: []RepoAgent{{Name: "../etc", Description: "x."}}, wantErr: true},
		{name: "empty name", agents: []RepoAgent{{Name: "", Description: "x."}}, wantErr: true},
		{
			name:    "over-long name",
			agents:  []RepoAgent{{Name: strings.Repeat("a", 65), Description: "x."}},
			wantErr: true,
		},
		{
			name:    "duplicate names",
			agents:  []RepoAgent{{Name: "coder", Description: "one."}, {Name: "coder", Description: "two."}},
			wantErr: true,
		},
		{name: "blank description", agents: []RepoAgent{{Name: "coder", Description: "  "}}, wantErr: true},
		{name: "over-long description", agents: []RepoAgent{{Name: "coder", Description: long}}, wantErr: true},
		{name: "multibyte description over the byte cap (fewer runes)", agents: []RepoAgent{{Name: "coder", Description: multibyteOverBytes}}, wantErr: true},
		{name: "multibyte description exactly at the byte cap", agents: []RepoAgent{{Name: "coder", Description: diacriticAtCap}}},
		{name: "multibyte description one 2-byte rune over the cap", agents: []RepoAgent{{Name: "coder", Description: diacriticOverCap}}, wantErr: true},
		{
			// A newline in a description would forge structure in the run message and
			// in the approval panel it is rendered into.
			name:    "newline in description",
			agents:  []RepoAgent{{Name: "coder", Description: "line one\nline two"}},
			wantErr: true,
		},
		{
			name:    "control character in description",
			agents:  []RepoAgent{{Name: "coder", Description: "bell\x07here"}},
			wantErr: true,
		},
		{
			// ESC is a control char (Cc) — IsControl already catches the ANSI-escape
			// spoof — but assert it explicitly since it is the review's example.
			name:    "ANSI escape in description",
			agents:  []RepoAgent{{Name: "coder", Description: "\x1b[31mALERT\x1b[0m"}},
			wantErr: true,
		},
		{
			// U+202E RIGHT-TO-LEFT OVERRIDE is category Cf (format), NOT Cc, so
			// IsControl misses it: this is the gap the stricter untrusted-repo rule
			// closes. A bidi override can visually reorder text in the approval panel.
			name:    "bidirectional override in description",
			agents:  []RepoAgent{{Name: "coder", Description: "safe\u202ednammoc"}},
			wantErr: true,
		},
		{
			// A zero-width joiner (also Cf) is likewise rejected for these one-line
			// untrusted fields.
			name:    "zero-width format character in description",
			agents:  []RepoAgent{{Name: "coder", Description: "a\u200db"}},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRepoAgents(tc.agents)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidSelection) {
					t.Fatalf("want ErrInvalidSelection, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// -------------------------------------------------------------------------
// Selection validation against the roster it names
// -------------------------------------------------------------------------

func TestValidateSelection(t *testing.T) {
	roster := []string{"coder", "reviewer", "tester"}
	tooMany := make([]string, MaxAgentExclusions+1)
	for i := range tooMany {
		tooMany[i] = "coder"
	}

	cases := []struct {
		name    string
		sel     AgentSelection
		roster  []string
		wantErr bool
	}{
		{name: "repo, no exclusions", sel: AgentSelection{Source: "repo"}, roster: roster},
		{name: "own, one exclusion", sel: AgentSelection{Source: "own", Exclusions: []string{"tester"}}, roster: roster},
		{
			name:   "all but one excluded",
			sel:    AgentSelection{Source: "repo", Exclusions: []string{"coder", "reviewer"}},
			roster: roster,
		},
		{name: "unknown source", sel: AgentSelection{Source: "both"}, roster: roster, wantErr: true},
		{name: "empty source", sel: AgentSelection{}, roster: roster, wantErr: true},
		{
			// The ordering trap: picking the repo source on a run whose worker never
			// detected anything would activate an empty subagent map.
			name:    "repo source with no detected roster",
			sel:     AgentSelection{Source: "repo"},
			roster:  nil,
			wantErr: true,
		},
		{
			// A user who disabled every subagent (or has only the lead allocated, which
			// ownSubagentNames strips) approves a lead-only run — legal today, must stay
			// legal. This is the empty-own-roster regression the review caught.
			name:   "own source with an empty roster is a lead-only run",
			sel:    AgentSelection{Source: "own"},
			roster: nil,
		},
		{
			// …but an exclusion on that empty roster is still a confused request.
			name:    "own source, empty roster, with an exclusion",
			sel:     AgentSelection{Source: "own", Exclusions: []string{"coder"}},
			roster:  nil,
			wantErr: true,
		},
		{
			name:    "exclusion not in the roster",
			sel:     AgentSelection{Source: "repo", Exclusions: []string{"auditor"}},
			roster:  roster,
			wantErr: true,
		},
		{
			name:    "malformed exclusion name",
			sel:     AgentSelection{Source: "repo", Exclusions: []string{"Not Kebab"}},
			roster:  roster,
			wantErr: true,
		},
		{
			name:    "every subagent excluded",
			sel:     AgentSelection{Source: "repo", Exclusions: []string{"coder", "reviewer", "tester"}},
			roster:  roster,
			wantErr: true,
		},
		{
			// Duplicates collapse, so this excludes one of three: still legal.
			name:   "duplicate exclusions collapse",
			sel:    AgentSelection{Source: "own", Exclusions: []string{"tester", "tester"}},
			roster: roster,
		},
		{name: "over the exclusion cap", sel: AgentSelection{Source: "own", Exclusions: tooMany}, roster: roster, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSelection(tc.sel, tc.roster)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidSelection) {
					t.Fatalf("want ErrInvalidSelection, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestOwnSubagentNamesDropsTheLead(t *testing.T) {
	got := ownSubagentNames([]string{"lead", "coder", "Orchestrator", "reviewer"})
	want := []string{"coder", "reviewer"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A repo file named `lead` is just another subagent candidate (Decision 3): the
// orchestrator always comes from the claim payload, so nothing filters it out of a
// repo roster — and a user must be able to exclude it like any other.
func TestRepoRosterKeepsALeadNamedAgent(t *testing.T) {
	if err := validateSelection(AgentSelection{Source: "repo", Exclusions: []string{"lead"}}, []string{"lead", "coder"}); err != nil {
		t.Fatalf("excluding a repo agent named lead must be legal: %v", err)
	}
}

// -------------------------------------------------------------------------
// SetState: the roster + the autopilot selection ride the `running` report
// -------------------------------------------------------------------------

func runningStateFixture(t *testing.T) (*fakeStore, *Service, store.Worker, uuid.UUID) {
	t.Helper()
	wkrID, runID, userID := uuid.New(), uuid.New(), uuid.New()
	run := store.Run{ID: runID, UserID: userID, Status: "running", WorkerID: pgconv.UUID(wkrID)}
	fs := &fakeStore{runOwned: run, setRunningRows: 1}
	return fs, New(fs, newBox(t), testParams()), store.Worker{ID: wkrID}, runID
}

func TestSetStateRunningPersistsRepoAgents(t *testing.T) {
	fs, svc, wkr, runID := runningStateFixture(t)
	agents := twoAgents()

	_, applied, err := svc.SetState(context.Background(), wkr, runID, StateRequest{State: "running", RepoAgents: &agents})
	if err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if !applied {
		t.Fatal("running report should apply")
	}
	var got []RepoAgent
	if err := json.Unmarshal(fs.setRunningParams.RepoAgents, &got); err != nil {
		t.Fatalf("persisted roster is not JSON: %v", err)
	}
	if len(got) != 2 || got[0].Name != "coder" || got[1].Name != "reviewer" {
		t.Fatalf("persisted roster = %+v", got)
	}
}

// `[]` (detection ran, found none) must survive as an empty JSON array — never as
// NULL or `null`, which the run DTO reads as "pre-feature run".
func TestSetStateRunningPersistsEmptyRosterAsEmptyArray(t *testing.T) {
	fs, svc, wkr, runID := runningStateFixture(t)
	empty := []RepoAgent{}

	if _, _, err := svc.SetState(context.Background(), wkr, runID, StateRequest{State: "running", RepoAgents: &empty}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if string(fs.setRunningParams.RepoAgents) != "[]" {
		t.Fatalf("empty roster persisted as %q, want []", fs.setRunningParams.RepoAgents)
	}
}

// A report that says nothing about the roster (the session-id / iteration
// heartbeats) must leave the column alone — the query COALESCEs a nil param.
func TestSetStateRunningWithoutRosterLeavesColumnUntouched(t *testing.T) {
	fs, svc, wkr, runID := runningStateFixture(t)

	if _, _, err := svc.SetState(context.Background(), wkr, runID, StateRequest{State: "running", IterationCount: 3}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if fs.setRunningParams.RepoAgents != nil {
		t.Fatalf("absent roster must send a NULL param, got %q", fs.setRunningParams.RepoAgents)
	}
	if fs.setRunningParams.AgentSource.Valid {
		t.Fatal("absent selection must send a NULL agent_source")
	}
}

// running → running is the normal shape of a run: the claim transition, then the
// post-checkout roster report, then every heartbeat.
func TestSetStateToleratesRunningToRunning(t *testing.T) {
	fs, svc, wkr, runID := runningStateFixture(t)
	agents := twoAgents()

	for _, req := range []StateRequest{
		{State: "running"},
		{State: "running", RepoAgents: &agents},
		{State: "running", IterationCount: 1},
	} {
		if _, applied, err := svc.SetState(context.Background(), wkr, runID, req); err != nil || !applied {
			t.Fatalf("running report rejected: applied=%v err=%v", applied, err)
		}
	}
	if fs.setRunningParams.IterationCount != 1 {
		t.Fatalf("last report should carry iteration 1, got %d", fs.setRunningParams.IterationCount)
	}
}

func TestSetStateRejectsOversizedRoster(t *testing.T) {
	_, svc, wkr, runID := runningStateFixture(t)
	big := make([]RepoAgent, MaxRepoAgents+1)
	for i := range big {
		big[i] = RepoAgent{Name: "coder", Description: "x."}
	}

	_, _, err := svc.SetState(context.Background(), wkr, runID, StateRequest{State: "running", RepoAgents: &big})
	if !errors.Is(err, ErrInvalidSelection) {
		t.Fatalf("want ErrInvalidSelection, got %v", err)
	}
}

func TestSetStateRejectsMalformedRosterFromWorker(t *testing.T) {
	_, svc, wkr, runID := runningStateFixture(t)
	bad := []RepoAgent{{Name: "coder", Description: "forged\nkey: value"}}

	_, _, err := svc.SetState(context.Background(), wkr, runID, StateRequest{State: "running", RepoAgents: &bad})
	if !errors.Is(err, ErrInvalidSelection) {
		t.Fatalf("a worker payload is not trusted; want ErrInvalidSelection, got %v", err)
	}
}

// Autopilot: the worker self-approves, so its resolved default reaches the row only
// through this state report (Decision 6).
func TestSetStateRunningPersistsAutopilotSelection(t *testing.T) {
	fs, svc, wkr, runID := runningStateFixture(t)
	agents := twoAgents()
	sel := AgentSelection{Source: AgentSourceRepo}

	if _, _, err := svc.SetState(context.Background(), wkr, runID, StateRequest{
		State: "running", RepoAgents: &agents, AgentSelection: &sel,
	}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if fs.setRunningParams.AgentSource.String != "repo" {
		t.Fatalf("agent_source = %q", fs.setRunningParams.AgentSource.String)
	}
	if string(fs.setRunningParams.AgentExclusions) != "[]" {
		t.Fatalf("agent_exclusions = %q, want []", fs.setRunningParams.AgentExclusions)
	}
}

// The autopilot selection is validated against the roster reported in the SAME
// request — the worker sends both together, and the column is still NULL.
func TestSetStateAutopilotRepoSelectionNeedsARoster(t *testing.T) {
	_, svc, wkr, runID := runningStateFixture(t)
	empty := []RepoAgent{}
	sel := AgentSelection{Source: AgentSourceRepo}

	_, _, err := svc.SetState(context.Background(), wkr, runID, StateRequest{
		State: "running", RepoAgents: &empty, AgentSelection: &sel,
	})
	if !errors.Is(err, ErrInvalidSelection) {
		t.Fatalf("repo source with an empty roster must be rejected, got %v", err)
	}
}

func TestSetStateAutopilotOwnSelectionUsesTheOwnersTemplates(t *testing.T) {
	fs, svc, wkr, runID := runningStateFixture(t)
	fs.templates = []store.AgentTemplate{{Name: "lead"}, {Name: "coder"}}
	sel := AgentSelection{Source: AgentSourceOwn}

	if _, _, err := svc.SetState(context.Background(), wkr, runID, StateRequest{State: "running", AgentSelection: &sel}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if fs.setRunningParams.AgentSource.String != "own" {
		t.Fatalf("agent_source = %q", fs.setRunningParams.AgentSource.String)
	}
}

// A run whose owner has only the lead allocated is a lead-only run — legal today
// (the lead works alone against its guardrail prompt), so an autopilot `own`
// default on it must persist, not 400. Regression guard for the empty-own-roster
// bug the review caught.
func TestSetStateAutopilotOwnSelectionAcceptsLeadOnlyRoster(t *testing.T) {
	fs, svc, wkr, runID := runningStateFixture(t)
	fs.templates = []store.AgentTemplate{{Name: "lead"}}
	sel := AgentSelection{Source: AgentSourceOwn}

	if _, _, err := svc.SetState(context.Background(), wkr, runID, StateRequest{State: "running", AgentSelection: &sel}); err != nil {
		t.Fatalf("a lead-only own run must be accepted: %v", err)
	}
	if fs.setRunningParams.AgentSource.String != "own" {
		t.Fatalf("agent_source = %q", fs.setRunningParams.AgentSource.String)
	}
}

// -------------------------------------------------------------------------
// SubmitInput: the approve_plan selection contract
// -------------------------------------------------------------------------

// A run parked at the gate with a live worker — the only shape an approve can take.
func gatedRun(t *testing.T) (*fakeStore, *Service, uuid.UUID, uuid.UUID) {
	t.Helper()
	user, runID, wkrID := uuid.New(), uuid.New(), uuid.New()
	fixed := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	fs := &fakeStore{
		runByID: store.Run{
			ID: runID, UserID: user, Status: "awaiting_approval", WorkerID: pgconv.UUID(wkrID),
			RepoAgents: repoAgentsJSON(t, twoAgents()),
		},
		workerByID: store.Worker{ID: wkrID, LastHeartbeatAt: pgconv.Time(fixed)},
		templates:  []store.AgentTemplate{{Name: "lead"}, {Name: "coder"}, {Name: "tester"}},
	}
	svc := New(fs, newBox(t), testParams())
	svc.now = func() time.Time { return fixed }
	return fs, svc, user, runID
}

func TestSubmitInputApproveWithRepoSelection(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	sel := &AgentSelection{Source: AgentSourceRepo, Exclusions: []string{"reviewer"}}

	res, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", sel)
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if res.ServerSide {
		t.Fatal("approve_plan is live-poller-only; it is never applied server-side")
	}
	if fs.createdApproval == nil {
		t.Fatal("approve_plan must go through CreateApprovePlanInput (row + input in one statement)")
	}
	if fs.createdInput != nil {
		t.Fatal("a selection-bearing approve must not take the plain CreateRunInput path")
	}
	if fs.createdApproval.AgentSource.String != "repo" {
		t.Fatalf("agent_source = %q", fs.createdApproval.AgentSource.String)
	}
	if string(fs.createdApproval.AgentExclusions) != `["reviewer"]` {
		t.Fatalf("agent_exclusions = %q", fs.createdApproval.AgentExclusions)
	}
	// The body is the SERVER's canonical encoding, which the worker parses back.
	var body AgentSelection
	if err := json.Unmarshal([]byte(fs.createdApproval.Body.String), &body); err != nil {
		t.Fatalf("input body is not a selection: %v", err)
	}
	if body.Source != "repo" || len(body.Exclusions) != 1 || body.Exclusions[0] != "reviewer" {
		t.Fatalf("input body = %+v", body)
	}
}

// Exclusions are checked against the OWN roster when that is the chosen source —
// "reviewer" exists in the repo roster but not among the owner's templates.
func TestSubmitInputApproveOwnSelectionChecksTemplateNames(t *testing.T) {
	_, svc, user, runID := gatedRun(t)
	sel := &AgentSelection{Source: AgentSourceOwn, Exclusions: []string{"reviewer"}}

	_, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", sel)
	if !errors.Is(err, ErrInvalidSelection) {
		t.Fatalf("want ErrInvalidSelection, got %v", err)
	}
}

func TestSubmitInputApproveOwnSelectionExcludingATemplate(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	sel := &AgentSelection{Source: AgentSourceOwn, Exclusions: []string{"tester"}}

	if _, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", sel); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if fs.createdApproval.AgentSource.String != "own" {
		t.Fatalf("agent_source = %q", fs.createdApproval.AgentSource.String)
	}
}

func TestSubmitInputApproveRepoSelectionOnRunWithNoRoster(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	fs.runByID.RepoAgents = nil // no worker ever reported

	_, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", &AgentSelection{Source: AgentSourceRepo})
	if !errors.Is(err, ErrInvalidSelection) {
		t.Fatalf("want ErrInvalidSelection, got %v", err)
	}
}

func TestSubmitInputApproveRepoSelectionOnEmptyRoster(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	fs.runByID.RepoAgents = []byte("[]") // detection ran, found none

	_, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", &AgentSelection{Source: AgentSourceRepo})
	if !errors.Is(err, ErrInvalidSelection) {
		t.Fatalf("want ErrInvalidSelection, got %v", err)
	}
}

func TestSubmitInputApproveExcludingEverySubagent(t *testing.T) {
	_, svc, user, runID := gatedRun(t)
	sel := &AgentSelection{Source: AgentSourceRepo, Exclusions: []string{"coder", "reviewer"}}

	_, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", sel)
	if !errors.Is(err, ErrInvalidSelection) {
		t.Fatalf("want ErrInvalidSelection, got %v", err)
	}
}

func TestSubmitInputSelectionOnlyValidOnApprove(t *testing.T) {
	for _, kind := range []string{"follow_up", "reject_plan", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			_, svc, user, runID := gatedRun(t)
			_, err := svc.SubmitInput(context.Background(), user, runID, kind, "", &AgentSelection{Source: AgentSourceRepo})
			if !errors.Is(err, ErrInvalidSelection) {
				t.Fatalf("a selection on %s must be rejected, got %v", kind, err)
			}
		})
	}
}

// An approve with no selection (the Slack gate, and any older client) stays on the
// plain enqueue path and leaves the run's selection columns alone.
func TestSubmitInputApproveWithoutSelectionKeepsThePlainPath(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)

	if _, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", nil); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if fs.createdApproval != nil {
		t.Fatal("a selectionless approve must not write the selection columns")
	}
	if fs.createdInput == nil || fs.createdInput.Kind != "approve_plan" {
		t.Fatalf("approve not enqueued: %+v", fs.createdInput)
	}
}

func TestSubmitInputApproveOnTerminalRun(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	fs.runByID.Status = "completed"

	_, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", &AgentSelection{Source: AgentSourceRepo})
	if !errors.Is(err, ErrRunTerminal) {
		t.Fatalf("want ErrRunTerminal, got %v", err)
	}
}

// -------------------------------------------------------------------------
// Column decode: NULL and `[]` are different answers
// -------------------------------------------------------------------------

func TestDecodeRepoAgentsDistinguishesNullFromEmpty(t *testing.T) {
	null, err := DecodeRepoAgents(nil)
	if err != nil || null != nil {
		t.Fatalf("a NULL column must decode to a nil slice (JSON null): %v %v", null, err)
	}
	empty, err := DecodeRepoAgents([]byte("[]"))
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("`[]` must decode to an empty, non-nil slice: %v %v", empty, err)
	}
}

func TestDecodeExclusionsDistinguishesNullFromEmpty(t *testing.T) {
	null, err := DecodeExclusions(nil)
	if err != nil || null != nil {
		t.Fatalf("a NULL column must decode to nil: %v %v", null, err)
	}
	empty, err := DecodeExclusions([]byte("[]"))
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("`[]` must decode to an empty, non-nil slice: %v %v", empty, err)
	}
}

// A malformed column is data a worker wrote, not an invariant of the request: it
// degrades to "no roster", which validateSelection reports as a 400, not a 500.
func TestRepoAgentNamesOnMalformedColumn(t *testing.T) {
	if names := repoAgentNames([]byte(`{"not":"an array"}`)); names != nil {
		t.Fatalf("malformed column should yield no names, got %v", names)
	}
}
