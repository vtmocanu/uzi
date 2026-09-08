package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The live-DB half of the PRD #1183 M3 findings endpoints: the per-status tally (repo scoping,
// ?run ignored, each count = a hand count), bulk dismiss (owner scoping, only open rows move,
// the updated count), and undo (404 on a non-dismissed row, reopen clears the reason). These are
// properties of the SQL + the owner-scoped WHERE that a fake store cannot reproduce.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

func findingsDismissLiveDB(t *testing.T) (*Handler, *pgxpool.Pool, *store.Queries) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	h := &Handler{q: q, wsvc: workersvc.New(q, nil, workersvc.Params{})}
	return h, pool, q
}

// seedFindingsUser creates a user with a forge connection and returns (userID, connID).
func seedFindingsUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	userID, connID := uuid.New(), uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("m3-%s@e2e", userID))
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	return userID, connID
}

func seedFindingsRepo(ctx context.Context, t *testing.T, pool *pgxpool.Pool, connID uuid.UUID, pid int64, path string) uuid.UUID {
	t.Helper()
	repoID := uuid.New()
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, 'https://forge.e2e/'||$4, 'main', true)`, repoID, connID, pid, path)
	return repoID
}

// mkDisposition creates an OPEN coordinate for (userID, repoID, location) and returns its id.
func mkDisposition(ctx context.Context, t *testing.T, q *store.Queries, userID, repoID uuid.UUID, location string) uuid.UUID {
	t.Helper()
	d, err := q.UpsertOpenDisposition(ctx, store.UpsertOpenDispositionParams{
		UserID: userID, RepoID: repoID, Location: location, ContentHash: "h-" + location, LastTitle: location,
	})
	if err != nil {
		t.Fatalf("UpsertOpenDisposition(%s): %v", location, err)
	}
	return d.ID
}

func dispositionStatusReason(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (string, string) {
	t.Helper()
	var status string
	var reason *string
	if err := pool.QueryRow(ctx,
		`SELECT status, dismiss_reason FROM finding_dispositions WHERE id = $1`, id).Scan(&status, &reason); err != nil {
		t.Fatalf("read disposition %s: %v", id, err)
	}
	if reason == nil {
		return status, ""
	}
	return status, *reason
}

func findingsAuthedReq(method, target, body string, userID uuid.UUID) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	return r.WithContext(mw.ContextWithUser(r.Context(), store.User{ID: userID}))
}

// ── FindingsStats: repo scoping, ?run ignored, each count equals a hand count ──
func TestFindingsStatsLiveDB(t *testing.T) {
	h, pool, q := findingsDismissLiveDB(t)
	ctx := context.Background()
	userID, connID := seedFindingsUser(ctx, t, pool)
	repoA := seedFindingsRepo(ctx, t, pool, connID, 1, "g/"+uuid.NewString()[:8])
	repoB := seedFindingsRepo(ctx, t, pool, connID, 2, "g/"+uuid.NewString()[:8])

	// repoA: open, filed, done, dismissed(wont_do), dismissed(not_an_issue). repoB: open.
	mkDisposition(ctx, t, q, userID, repoA, "a/open.go#f")
	filedA := mkDisposition(ctx, t, q, userID, repoA, "a/filed.go#f")
	mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='filed', filed_issue_iid=7, filed_issue_url='u', resolved_at=now() WHERE id=$1`, filedA)
	doneA := mkDisposition(ctx, t, q, userID, repoA, "a/done.go#f")
	mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='done', set_via='issue_close', filed_issue_iid=8, resolved_at=now(), close_synced_at=now() WHERE id=$1`, doneA)
	wontA := mkDisposition(ctx, t, q, userID, repoA, "a/wont.go#f")
	mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='dismissed', dismiss_reason='wont_do', resolved_at=now() WHERE id=$1`, wontA)
	fpA := mkDisposition(ctx, t, q, userID, repoA, "a/fp.go#f")
	mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='dismissed', dismiss_reason='not_an_issue', resolved_at=now() WHERE id=$1`, fpA)
	mkDisposition(ctx, t, q, userID, repoB, "b/open.go#f")

	statsFor := func(query string) apitypes.TriageDTO {
		t.Helper()
		rec := httptest.NewRecorder()
		h.FindingsStats(rec, findingsAuthedReq(http.MethodGet, "/api/findings/stats"+query, "", userID))
		if rec.Code != http.StatusOK {
			t.Fatalf("FindingsStats%s = %d, want 200; body=%s", query, rec.Code, rec.Body.String())
		}
		var dto apitypes.TriageDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
			t.Fatalf("decode stats: %v", err)
		}
		return dto
	}

	// No repo filter: the whole owner scope.
	all := statsFor("")
	if want := (apitypes.TriageDTO{Total: 6, Todo: 2, Filed: 1, Done: 1, Dismissed: 2, FalsePositives: 1}); all != want {
		t.Fatalf("stats(all) = %+v, want %+v", all, want)
	}

	// ?repo=repoA narrows to repoA's five coordinates (one open, not two).
	wantA := apitypes.TriageDTO{Total: 5, Todo: 1, Filed: 1, Done: 1, Dismissed: 2, FalsePositives: 1}
	if byA := statsFor("?repo=" + repoA.String()); byA != wantA {
		t.Fatalf("stats(repoA) = %+v, want %+v", byA, wantA)
	}

	// ?run= is IGNORED: adding it changes nothing (the counts are never run-scoped).
	if byARun := statsFor("?repo=" + repoA.String() + "&run=" + uuid.NewString()); byARun != wantA {
		t.Fatalf("stats(repoA, run ignored) = %+v, want %+v (run must not narrow counts)", byARun, wantA)
	}

	// The stats query equals a hand count per status (via the store directly).
	row, err := q.CountFindingsByStatusForUser(ctx, store.CountFindingsByStatusForUserParams{UserID: userID})
	if err != nil {
		t.Fatalf("CountFindingsByStatusForUser: %v", err)
	}
	if row.Total != 6 || row.Todo != 2 || row.Filed != 1 || row.Done != 1 || row.Dismissed != 2 || row.FalsePositives != 1 {
		t.Fatalf("hand count row = %+v, want total6 todo2 filed1 done1 dismissed2 fp1", row)
	}
}

// ── BulkDismissFindings: owner scoping (foreign id skipped), only open rows move, updated count ──
func TestBulkDismissFindingsLiveDB(t *testing.T) {
	h, pool, q := findingsDismissLiveDB(t)
	ctx := context.Background()
	userA, connA := seedFindingsUser(ctx, t, pool)
	userB, connB := seedFindingsUser(ctx, t, pool)
	repoA := seedFindingsRepo(ctx, t, pool, connA, 1, "g/"+uuid.NewString()[:8])
	repoB := seedFindingsRepo(ctx, t, pool, connB, 2, "g/"+uuid.NewString()[:8])

	d1 := mkDisposition(ctx, t, q, userA, repoA, "a/1.go#f")
	d2 := mkDisposition(ctx, t, q, userA, repoA, "a/2.go#f")
	d3 := mkDisposition(ctx, t, q, userA, repoA, "a/3.go#f") // open, NOT in the request → untouched
	dFiled := mkDisposition(ctx, t, q, userA, repoA, "a/filed.go#f")
	mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='filed', filed_issue_iid=7, resolved_at=now() WHERE id=$1`, dFiled)
	dForeign := mkDisposition(ctx, t, q, userB, repoB, "b/1.go#f") // userB's, open

	body := fmt.Sprintf(`{"reason":"not_an_issue","ids":[%q,%q,%q,%q]}`, d1, d2, dFiled, dForeign)
	rec := httptest.NewRecorder()
	h.BulkDismissFindings(rec, findingsAuthedReq(http.MethodPost, "/api/findings/dismiss", body, userA))
	if rec.Code != http.StatusOK {
		t.Fatalf("BulkDismissFindings = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp apitypes.BulkDismissFindingsResultDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Only d1 and d2 (owned + open) moved. dFiled (not open) and dForeign (not userA's) are skipped.
	if resp.Updated != 2 {
		t.Fatalf("updated = %d, want 2 (only the two owned open rows)", resp.Updated)
	}
	if len(resp.Findings) != 2 {
		t.Fatalf("findings len = %d, want 2", len(resp.Findings))
	}
	for _, f := range resp.Findings {
		if f.Status != "dismissed" || f.DismissReason != "not_an_issue" {
			t.Errorf("returned row = (status %s, reason %s), want (dismissed, not_an_issue)", f.Status, f.DismissReason)
		}
		if f.DispositionID != d1.String() && f.DispositionID != d2.String() {
			t.Errorf("returned row disposition_id %s, want d1 or d2", f.DispositionID)
		}
	}
	// DB truth for each coordinate.
	if s, r := dispositionStatusReason(ctx, t, pool, d1); s != "dismissed" || r != "not_an_issue" {
		t.Errorf("d1 = (%s, %s), want (dismissed, not_an_issue)", s, r)
	}
	if s, r := dispositionStatusReason(ctx, t, pool, d2); s != "dismissed" || r != "not_an_issue" {
		t.Errorf("d2 = (%s, %s), want (dismissed, not_an_issue)", s, r)
	}
	if s, _ := dispositionStatusReason(ctx, t, pool, d3); s != "open" {
		t.Errorf("d3 (not in request) = %s, want open (untouched)", s)
	}
	if s, _ := dispositionStatusReason(ctx, t, pool, dFiled); s != "filed" {
		t.Errorf("dFiled (not open) = %s, want filed (skipped)", s)
	}
	if s, _ := dispositionStatusReason(ctx, t, pool, dForeign); s != "open" {
		t.Errorf("dForeign (userB's) = %s, want open (owner scoping skipped it)", s)
	}
}

// ── UndoDismissFinding: 404 on a non-dismissed row; reopen clears the reason; owner-scoped ──
func TestUndoDismissFindingLiveDB(t *testing.T) {
	h, pool, q := findingsDismissLiveDB(t)
	ctx := context.Background()
	userA, connA := seedFindingsUser(ctx, t, pool)
	userB, connB := seedFindingsUser(ctx, t, pool)
	repoA := seedFindingsRepo(ctx, t, pool, connA, 1, "g/"+uuid.NewString()[:8])
	repoB := seedFindingsRepo(ctx, t, pool, connB, 2, "g/"+uuid.NewString()[:8])

	// An OPEN coordinate: undo must 404 (nothing dismissed to undo).
	dOpen := mkDisposition(ctx, t, q, userA, repoA, "a/open.go#f")
	rec := httptest.NewRecorder()
	h.UndoDismissFinding(rec, findingsPathReqUser(userA, dOpen))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("undo of an open row = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	// A DISMISSED coordinate: undo reopens it and clears the reason; the response is the reopened row.
	dDismissed := mkDisposition(ctx, t, q, userA, repoA, "a/dismissed.go#f")
	mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='dismissed', dismiss_reason='wont_do', resolved_at=now() WHERE id=$1`, dDismissed)
	rec = httptest.NewRecorder()
	h.UndoDismissFinding(rec, findingsPathReqUser(userA, dDismissed))
	if rec.Code != http.StatusOK {
		t.Fatalf("undo of a dismissed row = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var reopened apitypes.IncidentalFindingDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &reopened); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reopened.Status != "open" || reopened.DismissReason != "" {
		t.Errorf("reopened row = (status %s, reason %q), want (open, empty)", reopened.Status, reopened.DismissReason)
	}
	if s, r := dispositionStatusReason(ctx, t, pool, dDismissed); s != "open" || r != "" {
		t.Errorf("DB row = (%s, %q), want (open, empty)", s, r)
	}

	// Owner scoping: userA cannot undo userB's dismissed coordinate → 404, and it stays dismissed.
	dOther := mkDisposition(ctx, t, q, userB, repoB, "b/dismissed.go#f")
	mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='dismissed', dismiss_reason='wont_do', resolved_at=now() WHERE id=$1`, dOther)
	rec = httptest.NewRecorder()
	h.UndoDismissFinding(rec, findingsPathReqUser(userA, dOther))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user undo = %d, want 404 (owner scoping); body=%s", rec.Code, rec.Body.String())
	}
	if s, _ := dispositionStatusReason(ctx, t, pool, dOther); s != "dismissed" {
		t.Errorf("userB's row after a foreign undo = %s, want dismissed (untouched)", s)
	}
}

func findingsPathReqUser(userID, id uuid.UUID) *http.Request {
	u := store.User{ID: userID}
	return findingsPathReq(http.MethodDelete, &u, id.String())
}
