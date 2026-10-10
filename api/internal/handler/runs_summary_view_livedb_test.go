package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// summaryLiveRun is one seeded owner run.
type summaryLiveRun struct {
	id       uuid.UUID
	repoID   uuid.UUID
	issueIID int64
}

// seedSummaryRun inserts a forge connection, repo and NON-terminal run for ownerID with all
// four heavy fields populated. Every unique value derives from a fresh uuid because the
// store-IT harness shares one database across packages. ageSeconds sets a distinct
// created_at (the list order is created_at DESC with no tie-break).
func seedSummaryRun(t *testing.T, pool *pgxpool.Pool, ownerID uuid.UUID, ageSeconds int) summaryLiveRun {
	t.Helper()
	connID, repoID, runID, tag := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	// A positive int64 from the uuid bytes; collisions across runs are ~2^-31.
	iid := int64(uuid.New().ID()>>1) + 1
	cliMustExec(t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, $4, $5, $6)`,
		connID, ownerID, "https://forge-"+tag.String()+".e2e", "bot-"+tag.String(), int64(tag.ID()>>1)+1, []byte{0x1})
	cliMustExec(t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, $5, 'main', true)`,
		repoID, connID, int64(tag.ID()>>1)+1, "g-"+tag.String()+"/r", "https://forge-"+tag.String()+".e2e/r")
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind,
		                   plan_md, preserved_patch, repo_agents, created_at)
		 VALUES ($1, $2, $3, $4, 'Summary view', 'a long issue description', 'queued', 'issue',
		         '# a long plan', 'diff --git a/x b/x', '[{"name":"coder","description":"writes code"}]'::jsonb,
		         now() - make_interval(secs => $5::float8))`,
		runID, ownerID, repoID, iid, float64(ageSeconds))
	return summaryLiveRun{id: runID, repoID: repoID, issueIID: iid}
}

type liveListBody struct {
	Runs []map[string]json.RawMessage `json:"runs"`
}

func liveList(t *testing.T, router http.Handler, path, token string) liveListBody {
	t.Helper()
	rec := bearerReq(router, http.MethodGet, path, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
	}
	var b liveListBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return b
}

// idsIn returns the row ids (in order) that are in the wanted set.
func (b liveListBody) idsIn(t *testing.T, want map[string]bool) []string {
	t.Helper()
	var out []string
	for _, row := range b.Runs {
		var id string
		if err := json.Unmarshal(row["id"], &id); err != nil {
			t.Fatalf("row id: %v", err)
		}
		if want[id] {
			out = append(out, id)
		}
	}
	return out
}

func (b liveListBody) assertHeavy(t *testing.T, label string, present bool) {
	t.Helper()
	for i, row := range b.Runs {
		for _, k := range summaryHeavyKeys {
			if _, ok := row[k]; ok != present {
				t.Errorf("%s row %d: key %q present=%v, want %v", label, i, k, ok, present)
			}
		}
	}
}

// TestRunListSummaryViewLiveDB drives ?view=summary through the real router against Postgres:
// the real queries, auth chain and scoping. The two views must return the same rows in the
// same order, and differ only by the four heavy keys.
func TestRunListSummaryViewLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)

	ownerID, otherID, adminID, memberID := cliSeedUser(t, pool, false), cliSeedUser(t, pool, false), cliSeedUser(t, pool, true), cliSeedUser(t, pool, false)
	t.Cleanup(func() {
		// users cascade to their connections, repos and runs.
		if _, err := pool.Exec(context.Background(), `DELETE FROM users WHERE id = ANY($1)`,
			[]uuid.UUID{ownerID, otherID, adminID, memberID}); err != nil {
			t.Logf("cleanup users: %v", err)
		}
	})

	// Ages 30/20/10 seconds: strictly distinct created_at, newest is the last seeded.
	var owned []summaryLiveRun
	for _, age := range []int{30, 20, 10} {
		owned = append(owned, seedSummaryRun(t, pool, ownerID, age))
	}
	foreign := seedSummaryRun(t, pool, otherID, 5)

	ownedSet := map[string]bool{}
	wantOrder := []string{owned[2].id.String(), owned[1].id.String(), owned[0].id.String()}
	for _, r := range owned {
		ownedSet[r.id.String()] = true
	}

	ownerTok := cliMintToken(t, pool, ownerID, clitoken.ScopeUser)

	// Owner: unfiltered.
	full := liveList(t, router, "/api/runs", ownerTok)
	summary := liveList(t, router, "/api/runs?view=summary", ownerTok)
	full.assertHeavy(t, "owner default", true)
	summary.assertHeavy(t, "owner summary", false)
	for label, b := range map[string]liveListBody{"default": full, "summary": summary} {
		got := b.idsIn(t, ownedSet)
		if fmt.Sprint(got) != fmt.Sprint(wantOrder) {
			t.Errorf("owner %s ids/order = %v, want %v", label, got, wantOrder)
		}
		if other := b.idsIn(t, map[string]bool{foreign.id.String(): true}); len(other) != 0 {
			t.Errorf("owner %s view leaked another user's run %v", label, other)
		}
	}
	if len(full.Runs) != len(summary.Runs) {
		t.Errorf("owner row counts differ: default %d, summary %d", len(full.Runs), len(summary.Runs))
	}

	// Owner: repo_id and issue_iid filters.
	target := owned[1]
	q := fmt.Sprintf("/api/runs?repo_id=%s&issue_iid=%d", target.repoID, target.issueIID)
	fullF := liveList(t, router, q, ownerTok)
	sumF := liveList(t, router, q+"&view=summary", ownerTok)
	fullF.assertHeavy(t, "filtered default", true)
	sumF.assertHeavy(t, "filtered summary", false)
	wantF := []string{target.id.String()}
	if len(fullF.Runs) != 1 || len(sumF.Runs) != 1 ||
		fmt.Sprint(fullF.idsIn(t, ownedSet)) != fmt.Sprint(wantF) || fmt.Sprint(sumF.idsIn(t, ownedSet)) != fmt.Sprint(wantF) {
		t.Errorf("filtered views = default %d rows %v, summary %d rows %v; want exactly %v",
			len(fullF.Runs), fullF.idsIn(t, ownedSet), len(sumF.Runs), sumF.idsIn(t, ownedSet), wantF)
	}

	// Admin: the shared DB may hold other non-terminal runs, so compare the seeded subset.
	adminTok := cliMintToken(t, pool, adminID, clitoken.ScopeAdminRO)
	allSet := map[string]bool{foreign.id.String(): true}
	for id := range ownedSet {
		allSet[id] = true
	}
	aFull := liveList(t, router, "/api/admin/runs", adminTok)
	aSum := liveList(t, router, "/api/admin/runs?view=summary", adminTok)
	aSum.assertHeavy(t, "admin summary", false)
	gotFull, gotSum := aFull.idsIn(t, allSet), aSum.idsIn(t, allSet)
	if len(gotFull) != 4 || fmt.Sprint(gotFull) != fmt.Sprint(gotSum) {
		t.Errorf("admin seeded-subset ids/order: default %v, summary %v; want 4 identical ids", gotFull, gotSum)
	}
	for i, row := range aSum.Runs {
		if _, ok := row["owner_email"]; !ok {
			t.Errorf("admin summary row %d lacks owner_email", i)
		}
	}
	for _, row := range aFull.Runs {
		var id string
		_ = json.Unmarshal(row["id"], &id)
		if allSet[id] {
			for _, k := range summaryHeavyKeys {
				if _, ok := row[k]; !ok {
					t.Errorf("admin default row %s lacks %q", id, k)
				}
			}
		}
	}

	// A non-admin token is refused on the admin route in both views.
	memberTok := cliMintToken(t, pool, memberID, clitoken.ScopeUser)
	for _, p := range []string{"/api/admin/runs", "/api/admin/runs?view=summary"} {
		if rec := bearerReq(router, http.MethodGet, p, memberTok); rec.Code != http.StatusForbidden {
			t.Errorf("non-admin GET %s = %d, want 403", p, rec.Code)
		}
	}
}
