package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// milestone_lanes_livedb_test.go is the PRD #1353 M3 live-DB gate for MilestonesLiveForRun:
// it executes the three REAL runtime queries (LiveLaneFramesForRun,
// LeadAgentDispatchFramesForRun and LeadDispatchCompletionIDsForRun) against a throwaway
// Postgres, proving the DISTINCT ON per-instance selection (newest seq wins), the lead-lane
// Agent dispatch filter, the id-only completion projection, and the end-to-end back-join +
// completion drop that milestonelanes.Derive performs. Skipped unless UZI_TEST_DATABASE_URL
// points at a throwaway Postgres (run via ./e2e/run-store-it.sh); sqlc type inference is never
// trusted from a clean generate.

// seedLaneRun inserts a claimed issue run owned by workerID (the FK target for run_messages).
func (e codexTestEnv) seedLaneRun(t *testing.T, userID, workerID, repoID uuid.UUID) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
	        VALUES ($1, $2, $3, 'issue', 1, 't', 'd', 'claimed', $4)`, runID, userID, repoID, workerID)
	return runID
}

// insertLaneMessage inserts one run_messages row. agentInstance "" is stored as SQL NULL (the
// lead lane); a non-empty value is a subagent lane frame. created_at is set explicitly so the
// freshness window is deterministic.
func (e codexTestEnv) insertLaneMessage(t *testing.T, runID uuid.UUID, seq int32, kind, agent, agentInstance string, payload string, at time.Time) {
	t.Helper()
	var agentArg, instanceArg any
	if agent != "" {
		agentArg = agent
	}
	if agentInstance != "" {
		instanceArg = agentInstance
	}
	e.exec(`INSERT INTO run_messages (run_id, seq, kind, agent, agent_instance, payload, created_at)
	        VALUES ($1, $2, $3, $4, $5, $6, $7)`, runID, seq, kind, agentArg, instanceArg, []byte(payload), at)
}

// A live subagent lane on m2 is derived; a SECOND instance whose dispatch has a completion
// tool_result is dropped. This proves both queries execute, the payload->>'name' = 'Agent'
// dispatch filter binds the [m2] tag, and the tool_result back-join expires the finished lane.
func TestMilestonesLiveForRunLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	runID := env.seedLaneRun(t, userID, workerID, repoID)

	// Truncate to microseconds: Postgres timestamptz keeps microsecond precision, so a
	// nanosecond-precision Go time would not round-trip exactly through the created_at column.
	now := time.Now().UTC().Truncate(time.Microsecond)
	recent := now.Add(-2 * time.Minute)

	// Live instance: dispatch + a fresh lane frame, no completion.
	env.insertLaneMessage(t, runID, 1, "tool_use", "lead", "",
		`{"id":"inst-live","name":"Agent","input":{"subagent_type":"coder","description":"[m2] Do live work"}}`, recent)
	env.insertLaneMessage(t, runID, 2, "tool_use", "coder", "inst-live",
		`{"name":"Edit","input":{"file_path":"api/live.go"}}`, recent)

	// Finished instance: dispatch + lane frame + a completion tool_result whose tool_use_id
	// names the dispatch — Derive must drop it.
	env.insertLaneMessage(t, runID, 3, "tool_use", "lead", "",
		`{"id":"inst-done","name":"Agent","input":{"subagent_type":"reviewer","description":"[m2] Finished"}}`, recent)
	env.insertLaneMessage(t, runID, 4, "tool_use", "reviewer", "inst-done",
		`{"name":"Read","input":{"file_path":"api/done.go"}}`, recent)
	env.insertLaneMessage(t, runID, 5, "tool_result", "lead", "",
		`{"tool_use_id":"inst-done","content":"done","is_error":false}`, recent)

	// A non-Agent lead-lane tool_use must be filtered out by the lead-frame query.
	env.insertLaneMessage(t, runID, 6, "tool_use", "lead", "",
		`{"id":"noise","name":"Read","input":{"file_path":"api/noise.go"}}`, recent)

	svc := New(env.q, env.box, testParams())
	milestones := []apitypes.Milestone{{ID: "m2", Title: "Live work"}}
	live, err := svc.MilestonesLiveForRun(env.ctx, runID, milestones, []string{"m2"}, now)
	if err != nil {
		t.Fatalf("MilestonesLiveForRun: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("live milestones = %d, want 1\n got %+v", len(live), live)
	}
	if live[0].MilestoneID != "m2" {
		t.Fatalf("milestone id = %q, want m2", live[0].MilestoneID)
	}
	if len(live[0].Lanes) != 1 {
		t.Fatalf("lanes on m2 = %d, want 1 (the finished inst-done must be dropped)\n got %+v", len(live[0].Lanes), live[0].Lanes)
	}
	lane := live[0].Lanes[0]
	if lane.AgentInstance != "inst-live" {
		t.Fatalf("lane agent_instance = %q, want inst-live", lane.AgentInstance)
	}
	if lane.Agent != "coder" || lane.Tool != "Edit" || lane.Detail != "api/live.go" {
		t.Fatalf("lane = %+v, want {agent=coder tool=Edit detail=api/live.go}", lane)
	}
	if lane.AgentLabel != "Do live work" {
		t.Fatalf("lane agent_label = %q, want the dispatch label %q", lane.AgentLabel, "Do live work")
	}
	if !lane.At.Equal(recent) {
		t.Fatalf("lane at = %v, want %v", lane.At, recent)
	}
}

// A run with no live lane (only a completed instance) returns nil — the null-JSON back-compat
// contract holds through the real queries.
func TestMilestonesLiveForRunNoLanesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	runID := env.seedLaneRun(t, userID, workerID, repoID)

	now := time.Now().UTC().Truncate(time.Microsecond)
	recent := now.Add(-2 * time.Minute)
	env.insertLaneMessage(t, runID, 1, "tool_use", "lead", "",
		`{"id":"inst-x","name":"Agent","input":{"subagent_type":"coder","description":"[m2] Work"}}`, recent)
	env.insertLaneMessage(t, runID, 2, "tool_use", "coder", "inst-x",
		`{"name":"Edit","input":{"file_path":"api/x.go"}}`, recent)
	env.insertLaneMessage(t, runID, 3, "tool_result", "lead", "",
		`{"tool_use_id":"inst-x","content":"done","is_error":false}`, recent)

	svc := New(env.q, env.box, testParams())
	live, err := svc.MilestonesLiveForRun(env.ctx, runID, []apitypes.Milestone{{ID: "m2"}}, []string{"m2"}, now)
	if err != nil {
		t.Fatalf("MilestonesLiveForRun: %v", err)
	}
	if live != nil {
		t.Fatalf("live = %+v, want nil (only a completed instance)", live)
	}
}

// One instance with TWO tool_use frames pins LiveLaneFramesForRun's DISTINCT ON (agent_instance)
// ... ORDER BY agent_instance, seq DESC — the NEWEST (max-seq) frame must win. An older Read
// (lower seq) then a newer Edit (higher seq) for one instance must derive the Edit's tool/detail;
// flipping the ORDER BY to seq ASC (or dropping DESC) would surface the stale Read frame instead.
func TestMilestonesLiveForRunNewestFrameWinsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	runID := env.seedLaneRun(t, userID, workerID, repoID)

	now := time.Now().UTC().Truncate(time.Microsecond)
	older := now.Add(-4 * time.Minute)
	newer := now.Add(-1 * time.Minute)

	// Dispatch for the single live instance, then an OLDER lane frame (lower seq) and a NEWER
	// one (higher seq). DISTINCT ON must keep the higher-seq Edit NEW.go, not the Read OLD.go.
	env.insertLaneMessage(t, runID, 1, "tool_use", "lead", "",
		`{"id":"inst-live","name":"Agent","input":{"subagent_type":"coder","description":"[m2] Do live work"}}`, older)
	env.insertLaneMessage(t, runID, 2, "tool_use", "coder", "inst-live",
		`{"name":"Read","input":{"file_path":"api/OLD.go"}}`, older)
	env.insertLaneMessage(t, runID, 3, "tool_use", "coder", "inst-live",
		`{"name":"Edit","input":{"file_path":"api/NEW.go"}}`, newer)

	svc := New(env.q, env.box, testParams())
	live, err := svc.MilestonesLiveForRun(env.ctx, runID, []apitypes.Milestone{{ID: "m2"}}, []string{"m2"}, now)
	if err != nil {
		t.Fatalf("MilestonesLiveForRun: %v", err)
	}
	if len(live) != 1 || len(live[0].Lanes) != 1 {
		t.Fatalf("want one milestone with one lane, got %+v", live)
	}
	lane := live[0].Lanes[0]
	if lane.Tool != "Edit" || lane.Detail != "api/NEW.go" || !lane.At.Equal(newer) {
		t.Fatalf("lane = %+v, want the NEWEST frame {tool=Edit detail=api/NEW.go at=%v} (DISTINCT ON seq DESC)", lane, newer)
	}
}
