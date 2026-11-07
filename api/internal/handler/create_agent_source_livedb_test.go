package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

type createAgentSourceRow struct {
	requested, basis, implemented pgtype.Text
	exclusions                    []byte
}

func (f startRunGuardFixture) createAgentSourceRow(ctx context.Context, t *testing.T, repoID uuid.UUID) createAgentSourceRow {
	t.Helper()
	var row createAgentSourceRow
	if err := f.pool.QueryRow(ctx,
		`SELECT agent_source_requested, agent_source_requested_basis, agent_source, agent_exclusions FROM runs WHERE repo_id = $1`,
		repoID).Scan(&row.requested, &row.basis, &row.implemented, &row.exclusions); err != nil {
		t.Fatalf("read run agent columns: %v", err)
	}
	return row
}

func wantCreateRequest(t *testing.T, row createAgentSourceRow, source, basis string) {
	t.Helper()
	if source == "" {
		if row.requested.Valid || row.basis.Valid {
			t.Fatalf("requested = (%v, %v), want (NULL, NULL)", row.requested, row.basis)
		}
		return
	}
	if !row.requested.Valid || row.requested.String != source || !row.basis.Valid || row.basis.String != basis {
		t.Fatalf("requested = (%v, %v), want (%s, %s)", row.requested, row.basis, source, basis)
	}
}

func decodeCreatedRunDTO(t *testing.T, body string) apitypes.RunDTO {
	t.Helper()
	var env struct {
		Run apitypes.RunDTO `json:"run"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode created run: %v (body %s)", err, body)
	}
	return env.Run
}

func TestCreateRunAgentSelectionWithoutPlanLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newStartRunGuardFixture(ctx, t)
	for i, tc := range []struct {
		name        string
		repoDefault any
		source      string
	}{
		{"own with no default", nil, "own"},
		{"repo with no default", nil, "repo"},
		{"own overrides a repo default", "repo", "own"},
		{"repo overrides an own default", "own", "repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoID := f.seedEnabledRepoWithIssue(ctx, t, int64(5301+i), 7, protClean)
			mustExecT(ctx, t, f.pool, `UPDATE repos SET agent_source_default = $2 WHERE id = $1`, repoID, tc.repoDefault)
			w := f.createRunWithOverride(t, repoID, map[string]any{
				"issue_iid":       7,
				"agent_selection": map[string]any{"source": tc.source},
			})
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
			}
			dto := decodeCreatedRunDTO(t, w.Body.String())
			if dto.AgentSourceRequested == nil || *dto.AgentSourceRequested != tc.source ||
				dto.AgentSourceRequestedBasis == nil || *dto.AgentSourceRequestedBasis != "explicit" {
				t.Fatalf("DTO requested = (%v, %v), want (%s, explicit)", dto.AgentSourceRequested, dto.AgentSourceRequestedBasis, tc.source)
			}
			row := f.createAgentSourceRow(ctx, t, repoID)
			wantCreateRequest(t, row, tc.source, "explicit")
			if row.implemented.Valid || row.exclusions != nil {
				t.Fatalf("an unseeded create must not record an implementation selection, got (%v, %s)", row.implemented, row.exclusions)
			}
		})
	}
}

func TestCreateRunWithoutSelectionKeepsRepoDefaultLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newStartRunGuardFixture(ctx, t)

	defaulted := f.seedEnabledRepoWithIssue(ctx, t, 5311, 7, protClean)
	mustExecT(ctx, t, f.pool, `UPDATE repos SET agent_source_default = 'own' WHERE id = $1`, defaulted)
	if w := f.createRun(t, defaulted, 7); w.Code != http.StatusCreated {
		t.Fatalf("defaulted create: status = %d (body %s)", w.Code, w.Body.String())
	}
	wantCreateRequest(t, f.createAgentSourceRow(ctx, t, defaulted), "own", "repo_default")

	unset := f.seedEnabledRepoWithIssue(ctx, t, 5312, 7, protClean)
	if w := f.createRun(t, unset, 7); w.Code != http.StatusCreated {
		t.Fatalf("unset create: status = %d (body %s)", w.Code, w.Body.String())
	}
	wantCreateRequest(t, f.createAgentSourceRow(ctx, t, unset), "", "")
}

func TestCreateRunSeededSelectionUnchangedLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newStartRunGuardFixture(ctx, t)
	repoID := f.seedEnabledRepoWithIssue(ctx, t, 5321, 7, protClean)
	mustExecT(ctx, t, f.pool, `UPDATE repos SET agent_source_default = 'own' WHERE id = $1`, repoID)
	w := f.createRunWithOverride(t, repoID, map[string]any{
		"issue_iid":       7,
		"plan_md":         "# Plan\nImplement it.",
		"agent_selection": map[string]any{"source": "repo", "exclusions": []string{"tester"}},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
	}
	row := f.createAgentSourceRow(ctx, t, repoID)
	wantCreateRequest(t, row, "repo", "explicit")
	if row.implemented.String != "repo" || string(row.exclusions) != `["tester"]` {
		t.Fatalf("seeded selection = (%v, %s), want (repo, [\"tester\"])", row.implemented, row.exclusions)
	}
}

func TestCreateRunAgentSelectionWithoutPlanRefusalsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newStartRunGuardFixture(ctx, t)
	for i, tc := range []struct {
		name    string
		body    map[string]any
		wantMsg string
	}{
		{"exclusions without a plan", map[string]any{
			"agent_selection": map[string]any{"source": "own", "exclusions": []string{"tester"}},
		}, "exclusions require a plan file or an approval"},
		{"unknown source", map[string]any{
			"agent_selection": map[string]any{"source": "both"},
		}, "invalid agent selection"},
		{"empty source", map[string]any{
			"agent_selection": map[string]any{},
		}, "invalid agent selection"},
		{"selection with a planned commit and no plan", map[string]any{
			"agent_selection": map[string]any{"source": "own"},
			"planned_commit":  "abc1234",
		}, "planned_commit and require_base require plan_md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoID := f.seedEnabledRepoWithIssue(ctx, t, int64(5331+i), 7, protClean)
			tc.body["issue_iid"] = 7
			w := f.createRunWithOverride(t, repoID, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantMsg) {
				t.Fatalf("body = %s, want it to contain %q", w.Body.String(), tc.wantMsg)
			}
			if n := f.runCount(ctx, t, repoID); n != 0 {
				t.Fatalf("a refused create left %d run rows", n)
			}
		})
	}
}
