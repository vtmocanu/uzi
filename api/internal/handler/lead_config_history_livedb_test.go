package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// lead_config_history_livedb_test.go covers GET /api/runs/{id}/lead-config (PRD #3244 M3) against
// Postgres: ordering, visibility and the 256-row cap. Skipped unless UZI_TEST_DATABASE_URL is set
// (./e2e/run-store-it.sh). The bearer-token leg is in cli_auth_livedb_test.go.

func (e *leadOverlayEnv) history(t *testing.T, viewer store.User, run uuid.UUID) (int, apitypes.LeadConfigHistoryDTO, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	e.h.GetRunLeadConfigHistory(rec, runReq(viewer, run))
	var out apitypes.LeadConfigHistoryDTO
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode history: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, out, rec.Body.String()
}

func (e *leadOverlayEnv) mustHistory(t *testing.T, run uuid.UUID) apitypes.LeadConfigHistoryDTO {
	t.Helper()
	code, out, body := e.history(t, e.viewer, run)
	if code != http.StatusOK {
		t.Fatalf("history = %d %s", code, body)
	}
	return out
}

// Acceptance example 3 (the reclaim after a settings change lists gen 2 then gen 1) plus the
// plan-before-implement and seq ordering within a generation, with rows posted out of order.
func TestRunLeadConfigHistoryOrderingLiveDB(t *testing.T) {
	e := newLeadOverlayEnv(t)
	run := e.seed(t, overlayRun{})

	// Generation 1, posted implement first: plan entry 1 (seq 1), a second plan entry (seq 2), implement.
	e.mustPost(t, run, map[string]any{"phase": "implement", "seq": 1, "requested_model": "opus", "requested_model_source": "template"})
	e.mustPost(t, run, map[string]any{"phase": "plan", "phase_entry": 2, "seq": 2, "requested_model": "opus", "requested_model_source": "template"})
	e.mustPost(t, run, map[string]any{"phase": "plan", "seq": 1, "requested_model": "opus", "requested_model_source": "template"})
	// Reclaim after the user set their lane to sonnet.
	cliMustExec(t, e.pool, `UPDATE runs SET claim_generation = 2 WHERE id = $1`, run)
	e.mustPost(t, run, map[string]any{"claim_generation": 2, "phase": "implement", "seq": 1, "requested_model": "sonnet", "requested_model_source": "user_default"})
	e.mustPost(t, run, map[string]any{"claim_generation": 2, "phase": "plan", "seq": 1, "requested_model": "sonnet", "requested_model_source": "user_default"})

	got := e.mustHistory(t, run)
	if got.Truncated {
		t.Error("truncated = true for 5 rows")
	}
	type key struct {
		gen   int64
		phase string
		seq   int
	}
	want := []key{{2, "plan", 1}, {2, "implement", 1}, {1, "plan", 1}, {1, "plan", 2}, {1, "implement", 1}}
	if len(got.Entries) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got.Entries), len(want), got.Entries)
	}
	for i, w := range want {
		g := got.Entries[i]
		if g.ClaimGeneration != w.gen || g.Phase != w.phase || g.Seq != w.seq {
			t.Errorf("entry %d = gen %d %s seq %d, want gen %d %s seq %d", i, g.ClaimGeneration, g.Phase, g.Seq, w.gen, w.phase, w.seq)
		}
	}
	// Example 3: gen 2 shows sonnet from the user's lane, gen 1 opus from the template, with the
	// derived fixed_at carried on each entry.
	if m := strVal(got.Entries[0].RequestedModel); m != "sonnet" || got.Entries[0].RequestedModelSource != "user_default" || got.Entries[0].RequestedModelFixedAt != "claim" {
		t.Errorf("gen 2 plan entry = %s/%s/%s", m, got.Entries[0].RequestedModelSource, got.Entries[0].RequestedModelFixedAt)
	}
	if m := strVal(got.Entries[2].RequestedModel); m != "opus" || got.Entries[2].RequestedModelSource != "template" {
		t.Errorf("gen 1 plan entry = %s/%s", m, got.Entries[2].RequestedModelSource)
	}
	if got.Entries[0].Harness != "claude" || got.Entries[0].ReportedAt.IsZero() {
		t.Errorf("entry 0 = %+v, want harness and reported_at set", got.Entries[0])
	}
}

// Owner and admin read; another user and an unknown run get 404.
func TestRunLeadConfigHistoryVisibilityLiveDB(t *testing.T) {
	e := newLeadOverlayEnv(t)
	run := e.seed(t, overlayRun{})
	e.mustPost(t, run, map[string]any{"requested_model": "opus"})

	admin := cliSeedUser(t, e.pool, true)
	other := cliSeedUser(t, e.pool, false)
	t.Cleanup(func() {
		cliMustExec(t, e.pool, `DELETE FROM users WHERE id = ANY($1)`, []uuid.UUID{admin, other})
	})

	for name, viewer := range map[string]store.User{
		"owner": e.viewer,
		"admin": {ID: admin, IsAdmin: true},
	} {
		code, out, body := e.history(t, viewer, run)
		if code != http.StatusOK || len(out.Entries) != 1 || strVal(out.Entries[0].RequestedModel) != "opus" {
			t.Errorf("%s read = %d %s, want 200 with the one entry", name, code, body)
		}
	}
	if code, _, body := e.history(t, store.User{ID: other}, run); code != http.StatusNotFound {
		t.Errorf("another user = %d %s, want 404", code, body)
	}
	if code, _, body := e.history(t, e.viewer, uuid.New()); code != http.StatusNotFound {
		t.Errorf("unknown run = %d %s, want 404", code, body)
	}
}

// A run without a lead agent has no rows: 200 with an empty list, never null.
func TestRunLeadConfigHistoryNonLeadRunLiveDB(t *testing.T) {
	e := newLeadOverlayEnv(t)
	for _, tc := range []struct {
		name string
		run  overlayRun
	}{
		{"job", overlayRun{kind: "job"}},
		{"lead lane without rows", overlayRun{}},
	} {
		run := e.seed(t, tc.run)
		code, out, body := e.history(t, e.viewer, run)
		if code != http.StatusOK || len(out.Entries) != 0 || out.Truncated || !strings.Contains(body, `"entries":[]`) {
			t.Errorf("%s: %d %s, want 200 with entries [] and truncated false", tc.name, code, body)
		}
	}
}

// 256 rows are returned whole; a 257th row yields the first 256 with truncated true.
func TestRunLeadConfigHistoryCapLiveDB(t *testing.T) {
	e := newLeadOverlayEnv(t)
	run := e.seed(t, overlayRun{gen: 5})

	// Generations 1 to 4 x both phases x seq 1..32 = exactly 256 rows (seq 32 is the truncated row).
	cliMustExec(t, e.pool, `
		INSERT INTO run_lead_configs (run_id, claim_generation, phase, phase_entry, seq, harness,
		                              requested_model, requested_model_source, requested_effort_mode,
		                              requested_effort_source, truncated)
		SELECT $1, g, p, 1, s, 'claude', 'opus', 'user_default', 'runtime_default', 'builtin', s = 32
		  FROM generate_series(1, 4) g, unnest(ARRAY['plan', 'implement']) p, generate_series(1, 32) s`, run)
	got := e.mustHistory(t, run)
	if len(got.Entries) != 256 || got.Truncated {
		t.Fatalf("256 rows: got %d entries truncated %v, want 256 and not truncated", len(got.Entries), got.Truncated)
	}

	// One more row (the newest generation) makes 257: the response is capped and says so.
	e.mustPost(t, run, map[string]any{"claim_generation": 5, "requested_model": "opus"})
	got = e.mustHistory(t, run)
	if len(got.Entries) != 256 || !got.Truncated {
		t.Fatalf("257 rows: got %d entries truncated %v, want 256 and truncated", len(got.Entries), got.Truncated)
	}
	if first := got.Entries[0]; first.ClaimGeneration != 5 || first.Phase != "plan" || first.Seq != 1 {
		t.Errorf("first entry = gen %d %s seq %d, want the newest generation's plan row", first.ClaimGeneration, first.Phase, first.Seq)
	}
	// The oldest row is the one dropped: generation 1 implement seq 32 is last in order, so it is absent.
	last := got.Entries[255]
	if last.ClaimGeneration != 1 || last.Phase != "implement" || last.Seq != 31 {
		t.Errorf("last entry = gen %d %s seq %d, want gen 1 implement seq 31 (the final row dropped)", last.ClaimGeneration, last.Phase, last.Seq)
	}
}
