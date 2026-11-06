package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func (f patchRepoFixture) agentSourceDefault(ctx context.Context, t *testing.T, repoID uuid.UUID) *string {
	t.Helper()
	var v pgtype.Text
	if err := f.pool.QueryRow(ctx, `SELECT agent_source_default FROM repos WHERE id = $1`, repoID).Scan(&v); err != nil {
		t.Fatalf("read agent_source_default: %v", err)
	}
	if !v.Valid {
		return nil
	}
	return &v.String
}

func wantAgentSourceDefault(t *testing.T, got *string, want string) {
	t.Helper()
	switch {
	case want == "" && got != nil:
		t.Fatalf("agent_source_default = %q, want NULL", *got)
	case want != "" && (got == nil || *got != want):
		t.Fatalf("agent_source_default = %v, want %q", got, want)
	}
}

func TestPatchRepoAgentSourceDefaultOwnerLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newPatchRepoFixture(ctx, t)
	for _, step := range []struct{ body, want string }{
		{`{"agent_source_default":"own"}`, "own"},
		{`{"agent_source_default":"repo"}`, "repo"},
		{`{"agent_source_default":null}`, ""},
	} {
		w := f.patch(t, f.owner, f.repoID, step.body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (body %s)", step.body, w.Code, w.Body.String())
		}
		wantAgentSourceDefault(t, decodeRepoDTO(t, w).AgentSourceDefault, step.want)
		wantAgentSourceDefault(t, f.agentSourceDefault(ctx, t, f.repoID), step.want)
	}
}

func TestPatchRepoAgentSourceDefaultAdminLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newPatchRepoFixture(ctx, t)

	w := f.patch(t, f.admin, f.strangerRepoID, `{"agent_source_default":"own"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("admin own: status = %d (body %s)", w.Code, w.Body.String())
	}
	wantAgentSourceDefault(t, decodeRepoDTO(t, w).AgentSourceDefault, "own")

	w = f.patch(t, f.admin, f.strangerRepoID, `{"agent_source_default":"repo"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("admin repo on another user's row: status = %d, want 403 (body %s)", w.Code, w.Body.String())
	}
	wantAgentSourceDefault(t, f.agentSourceDefault(ctx, t, f.strangerRepoID), "own")

	mustExecT(ctx, t, f.pool, `UPDATE repos SET agent_source_default = 'repo' WHERE id = $1`, f.strangerRepoID)
	w = f.patch(t, f.admin, f.strangerRepoID, `{"agent_source_default":null}`)
	if w.Code != http.StatusOK {
		t.Fatalf("admin clear: status = %d (body %s)", w.Code, w.Body.String())
	}
	wantAgentSourceDefault(t, f.agentSourceDefault(ctx, t, f.strangerRepoID), "")

	w = f.patch(t, f.admin, uuid.New(), `{"agent_source_default":"repo"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("admin repo on an unknown id: status = %d, want 404", w.Code)
	}

	connID, adminRepo := uuid.New(), uuid.New()
	mustExecT(ctx, t, f.pool, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		SELECT $1, $2, forge_type, base_url, 'bot3', 3, token_ciphertext FROM forge_connections WHERE user_id = $3`, connID, f.admin.ID, f.owner.ID)
	mustExecT(ctx, t, f.pool, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		VALUES ($1, $2, 3, 'g/pr-admin', 'https://forge.example/g/pr-admin', 'main', true)`, adminRepo, connID)
	w = f.patch(t, f.admin, adminRepo, `{"agent_source_default":"repo"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("admin repo on the admin's own row: status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	wantAgentSourceDefault(t, f.agentSourceDefault(ctx, t, adminRepo), "repo")
}

func TestPatchRepoAgentSourceDefaultRejectsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newPatchRepoFixture(ctx, t)

	w := f.patch(t, f.stranger, f.repoID, `{"agent_source_default":"own"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("another user: status = %d, want 404", w.Code)
	}
	for _, body := range []string{
		`{"agent_source_default":"both"}`,
		`{"agent_source_default":""}`,
		`{"agent_source_default":1}`,
		`{"agent_source_default":["own"]}`,
	} {
		if w := f.patch(t, f.owner, f.repoID, body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", body, w.Code)
		}
	}
	for _, body := range []string{
		`{"agent_source_default":"own","repo_skills_enabled":true}`,
		`{"agent_source_default":null,"repo_devbox_opt_in":true}`,
	} {
		if w := f.patch(t, f.owner, f.repoID, body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", body, w.Code)
		}
	}
	wantAgentSourceDefault(t, f.agentSourceDefault(ctx, t, f.repoID), "")
}
