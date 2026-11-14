package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// Issue #4334: GET /api/admin/repo-activity through the real router. The LiveDB
// database is shared across tests, so every assertion filters the response to this
// test's own path prefix.

// raSeedConn inserts a user with one forge connection and returns (userID, connID).
func raSeedConn(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	user := cliSeedUser(t, pool, false)
	conn := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, conn, user, []byte{0x1})
	return user, conn
}

var raProjectSeq int64 = 7_000_000

func raSeedRepo(t *testing.T, pool *pgxpool.Pool, conn uuid.UUID, path string, enabled bool) uuid.UUID {
	t.Helper()
	raProjectSeq++
	id := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, 'https://forge.e2e/x', 'main', $5)`, id, conn, raProjectSeq, path, enabled)
	return id
}

func raSeedIssueRun(t *testing.T, pool *pgxpool.Pool, user, repo uuid.UUID, iid int, status string, at time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at)
		 VALUES ($1, $2, $3, $4, 't', 'd', $5, 'issue', $6)`, id, user, repo, iid, status, at)
	return id
}

// raSeedCrossCheckRun inserts a running cross_check child on repo reviewing target.
// A chat or judge run carries no repo (runs_kind_shape), so cross_check is the only
// excluded kind that can sit on a repo.
func raSeedCrossCheckRun(t *testing.T, pool *pgxpool.Pool, user, repo, target uuid.UUID, at time.Time) {
	t.Helper()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, target_run_id, issue_title, issue_description, status, kind,
		                   harness, report_only, budget_wall_seconds, created_at)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'running', 'cross_check', 'codex', true, 300, $5)`,
		uuid.New(), user, repo, target, at)
}

func TestAdminRepoActivityLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	admin := cliSeedUser(t, pool, true)
	adminRO := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	user1, conn1 := raSeedConn(t, pool)
	user2, conn2 := raSeedConn(t, pool)
	t.Cleanup(func() {
		// users cascade to connections, repos and runs.
		cliMustExec(t, pool, `DELETE FROM users WHERE id = ANY($1)`, []uuid.UUID{admin, user1, user2})
	})
	prefix := "ra4334-" + uuid.NewString()[:8] + "/"
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Seeded out of path order so the ORDER BY is what sorts the response.
	bNever := raSeedRepo(t, pool, conn2, prefix+"b-enabled-never-ran", true)
	aRan := raSeedRepo(t, pool, conn1, prefix+"a-enabled-ran", true)
	cActive := raSeedRepo(t, pool, conn1, prefix+"c-disabled-active", false)
	dXCheck := raSeedRepo(t, pool, conn1, prefix+"d-disabled-crosscheck-only", false)
	eTerminal := raSeedRepo(t, pool, conn2, prefix+"e-disabled-terminal", false)
	sameX := raSeedRepo(t, pool, conn1, prefix+"s-same-path", true)
	sameY := raSeedRepo(t, pool, conn2, prefix+"s-same-path", true)
	target := raSeedRepo(t, pool, conn1, prefix+"z-disabled-target", false)

	// aRan: two terminal issue runs, then a NEWER cross_check run, so last_run_at
	// proves the newest run of ANY kind wins (and is not the newest issue run).
	raSeedIssueRun(t, pool, user1, aRan, 1, "completed", now.Add(-3*time.Hour))
	oldIssue := raSeedIssueRun(t, pool, user1, aRan, 2, "failed", now.Add(-2*time.Hour))
	newest := now.Add(-1 * time.Hour)
	raSeedCrossCheckRun(t, pool, user1, aRan, oldIssue, newest)

	// cActive: disabled but with a queued issue run -> included with enabled=false.
	activeAt := now.Add(-30 * time.Minute)
	raSeedIssueRun(t, pool, user1, cActive, 1, "queued", activeAt)

	// dXCheck: its only non-terminal run is a cross_check -> excluded.
	targetRun := raSeedIssueRun(t, pool, user1, target, 1, "completed", now.Add(-4*time.Hour))
	raSeedCrossCheckRun(t, pool, user1, dXCheck, targetRun, now.Add(-10*time.Minute))

	// eTerminal: every run terminal -> excluded.
	for i, st := range []string{"completed", "failed", "cancelled"} {
		raSeedIssueRun(t, pool, user2, eTerminal, i+1, st, now.Add(-time.Duration(i+5)*time.Hour))
	}

	rec := bearerReq(router, http.MethodGet, "/api/admin/repo-activity", adminRO)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var dto apitypes.AdminRepoActivityDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	var got []apitypes.RepoActivityDTO
	for _, r := range dto.Repos {
		if strings.HasPrefix(r.PathWithNamespace, prefix) {
			got = append(got, r)
		}
	}

	same := []string{sameX.String(), sameY.String()}
	sort.Strings(same)
	wantIDs := []string{aRan.String(), bNever.String(), cActive.String(), same[0], same[1]}
	var gotIDs []string
	for _, r := range got {
		gotIDs = append(gotIDs, r.ID)
	}
	if fmt.Sprint(gotIDs) != fmt.Sprint(wantIDs) {
		t.Fatalf("repo ids (path then id order) = %v, want %v (excluded: crosscheck-only %s, terminal %s, target %s)",
			gotIDs, wantIDs, dXCheck, eTerminal, target)
	}

	byID := map[string]apitypes.RepoActivityDTO{}
	for _, r := range got {
		byID[r.ID] = r
	}
	if r := byID[aRan.String()]; !r.Enabled || r.LastRunAt == nil || !r.LastRunAt.Equal(newest) {
		t.Fatalf("enabled repo that ran: %+v, want enabled with last_run_at %s (the newest run, a cross_check)", r, newest)
	}
	if r := byID[cActive.String()]; r.Enabled || r.LastRunAt == nil || !r.LastRunAt.Equal(activeAt) {
		t.Fatalf("disabled repo with an active issue run: %+v, want enabled=false last_run_at %s", r, activeAt)
	}
	if r := byID[bNever.String()]; !r.Enabled || r.LastRunAt != nil {
		t.Fatalf("enabled repo that never ran: %+v, want last_run_at nil", r)
	}

	// The wire carries an explicit JSON null for the never-ran repo, not an absent key.
	var raw struct {
		Repos []map[string]json.RawMessage `json:"repos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range raw.Repos {
		var id string
		if err := json.Unmarshal(m["id"], &id); err != nil {
			t.Fatal(err)
		}
		if id != bNever.String() {
			continue
		}
		found = true
		v, ok := m["last_run_at"]
		if !ok || string(v) != "null" {
			t.Fatalf("never-ran repo last_run_at = %q (present=%v), want JSON null", v, ok)
		}
	}
	if !found {
		t.Fatal("never-ran repo missing from raw body")
	}
}

func TestAdminRepoActivityForbiddenLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	admin := cliSeedUser(t, pool, true)
	member := cliSeedUser(t, pool, false)
	t.Cleanup(func() {
		cliMustExec(t, pool, `DELETE FROM users WHERE id = ANY($1)`, []uuid.UUID{admin, member})
	})
	const path = "/api/admin/repo-activity"

	if rec := bearerReq(router, http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential = %d, want 401", rec.Code)
	}
	if rec := cookieReq(t, router, http.MethodGet, path, cliMintJWT(t, pool, member), ""); rec.Code != http.StatusForbidden {
		t.Fatalf("member cookie = %d, want 403", rec.Code)
	}
	if rec := bearerReq(router, http.MethodGet, path, cliMintToken(t, pool, member, clitoken.ScopeUser)); rec.Code != http.StatusForbidden {
		t.Fatalf("member uzc token = %d, want 403", rec.Code)
	}
	if rec := bearerReq(router, http.MethodGet, path, cliMintToken(t, pool, admin, clitoken.ScopeUser)); rec.Code != http.StatusForbidden {
		t.Fatalf("admin default-scope token = %d, want 403", rec.Code)
	}
	if rec := bearerReq(router, http.MethodGet, path, cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)); rec.Code != http.StatusOK {
		t.Fatalf("admin_ro token = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, router, http.MethodGet, path, cliMintJWT(t, pool, admin), ""); rec.Code != http.StatusOK {
		t.Fatalf("admin cookie = %d %s, want 200", rec.Code, rec.Body.String())
	}
}
