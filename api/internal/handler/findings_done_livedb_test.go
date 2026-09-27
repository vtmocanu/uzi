package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The live-DB half of the issue #1723 human "Mark done" endpoints: single done over every
// settled status plus the absent-disposition fallback and the filing 409, bulk done (skips,
// dedup, re-assert), and Undo of either verdict (filed vs open target). Owner scoping is
// asserted for a second user AND an admin, since every query is keyed on the caller's user id.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// fullDisp is the whole verdict + link state of one disposition, with exact timestamps.
type fullDisp struct {
	status        string
	dismissReason pgtype.Text
	setVia        pgtype.Text
	resolvedAt    pgtype.Timestamptz
	closeSyncedAt pgtype.Timestamptz
	filingSince   pgtype.Timestamptz
	filedIID      pgtype.Int8
	filedURL      string
	contentHash   string
	lastTitle     string
}

func readFullDisp(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id uuid.UUID) fullDisp {
	t.Helper()
	var d fullDisp
	if err := pool.QueryRow(ctx,
		`SELECT status, dismiss_reason, set_via, resolved_at, close_synced_at, filing_since,
		        filed_issue_iid, filed_issue_url, content_hash, last_title
		   FROM finding_dispositions WHERE id = $1`, id).
		Scan(&d.status, &d.dismissReason, &d.setVia, &d.resolvedAt, &d.closeSyncedAt, &d.filingSince,
			&d.filedIID, &d.filedURL, &d.contentHash, &d.lastTitle); err != nil {
		t.Fatalf("read disposition %s: %v", id, err)
	}
	return d
}

func sameTS(a, b pgtype.Timestamptz) bool {
	return a.Valid == b.Valid && (!a.Valid || a.Time.Equal(b.Time))
}

// seedDoneRun creates a completed run for (userID, repoID) to hang evidence rows on.
func seedDoneRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, repoID uuid.UUID) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	mustExecT(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 1, 't', 'd', 'completed', 'issue')`, runID, userID, repoID)
	return runID
}

// mkEvidence inserts one evidence row at the coordinate and returns it.
func mkEvidence(ctx context.Context, t *testing.T, q *store.Queries, runID, userID, repoID uuid.UUID, location string) store.IncidentalFinding {
	t.Helper()
	f, err := q.InsertFinding(ctx, store.InsertFindingParams{
		RunID: runID, UserID: userID, RepoID: repoID, Location: location,
		Title:         "Leaky Ticker in " + location,
		DescriptionMd: "the sweeper   starts a ticker\nit never stops",
		Labels:        []byte(`[]`), Confidence: "high",
	})
	if err != nil {
		t.Fatalf("InsertFinding(%s): %v", location, err)
	}
	return f
}

// seedAdmin creates an ADMIN user (no connection) — admin must get no cross-user bypass.
func seedAdmin(ctx context.Context, t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1, $2, 'x', true)`,
		id, fmt.Sprintf("admin-%s@e2e", id))
	return id
}

func markDoneReq(userID, findingID uuid.UUID, isAdmin bool) *http.Request {
	u := store.User{ID: userID, IsAdmin: isAdmin}
	return findingsPathReq(http.MethodPost, &u, findingID.String())
}

func undoDispReq(userID, id uuid.UUID, isAdmin bool) *http.Request {
	u := store.User{ID: userID, IsAdmin: isAdmin}
	return findingsPathReq(http.MethodDelete, &u, id.String())
}

// ── (e) single done over every settled status; absent fallback; filing 409; (h) owner scoping ──
func TestMarkFindingDoneLiveDB(t *testing.T) {
	h, pool, q := findingsDismissLiveDB(t)
	ctx := context.Background()
	userA, connA := seedFindingsUser(ctx, t, pool)
	userB, _ := seedFindingsUser(ctx, t, pool)
	admin := seedAdmin(ctx, t, pool)
	repoA := seedFindingsRepo(ctx, t, pool, connA, 1, "g/"+uuid.NewString()[:8])
	runA := seedDoneRun(ctx, t, pool, userA, repoA)

	markDone := func(t *testing.T, userID uuid.UUID, isAdmin bool, findingID uuid.UUID) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		h.MarkFindingDone(rec, markDoneReq(userID, findingID, isAdmin))
		return rec
	}
	markOK := func(t *testing.T, findingID, wantDisp uuid.UUID) {
		t.Helper()
		rec := markDone(t, userA, false, findingID)
		if rec.Code != http.StatusOK {
			t.Fatalf("MarkFindingDone = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var dto apitypes.MarkFindingDoneResultDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if dto.Status != "done" || dto.DispositionID != wantDisp.String() {
			t.Fatalf("response = %+v, want {done %s}", dto, wantDisp)
		}
	}

	t.Run("open", func(t *testing.T) {
		ev := mkEvidence(ctx, t, q, runA, userA, repoA, "a/open.go#f")
		d := mkDisposition(ctx, t, q, userA, repoA, "a/open.go#f")
		markOK(t, ev.ID, d)
		got := readFullDisp(ctx, t, pool, d)
		if got.status != "done" || got.setVia.Valid || got.dismissReason.Valid || !got.resolvedAt.Valid {
			t.Fatalf("open→done = %+v, want done, set_via NULL, reason NULL, resolved_at set", got)
		}
		// The conflict arm never touches content_hash / last_title (mkDisposition's values stand).
		if got.contentHash != "h-a/open.go#f" || got.lastTitle != "a/open.go#f" {
			t.Errorf("content_hash/last_title = (%q, %q), want unchanged", got.contentHash, got.lastTitle)
		}
	})

	t.Run("filed keeps its issue link", func(t *testing.T) {
		ev := mkEvidence(ctx, t, q, runA, userA, repoA, "a/filed.go#f")
		d := mkDisposition(ctx, t, q, userA, repoA, "a/filed.go#f")
		mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='filed', filed_issue_iid=7, filed_issue_url='https://forge.e2e/i/7', resolved_at=now() - interval '1 day' WHERE id=$1`, d)
		before := readFullDisp(ctx, t, pool, d)
		markOK(t, ev.ID, d)
		got := readFullDisp(ctx, t, pool, d)
		if got.status != "done" || got.setVia.Valid {
			t.Fatalf("filed→done = (%s, set_via %v), want (done, NULL)", got.status, got.setVia)
		}
		if !got.filedIID.Valid || got.filedIID.Int64 != 7 || got.filedURL != "https://forge.e2e/i/7" {
			t.Errorf("issue link = (%v, %q), want (7, https://forge.e2e/i/7) kept", got.filedIID, got.filedURL)
		}
		if !got.resolvedAt.Valid || !got.resolvedAt.Time.After(before.resolvedAt.Time) {
			t.Errorf("resolved_at = %v, want re-stamped to the done time (after %v)", got.resolvedAt.Time, before.resolvedAt.Time)
		}
		if got.closeSyncedAt.Valid {
			t.Errorf("close_synced_at stamped by a human done; the edge must stay unconsumed")
		}
	})

	t.Run("dismissed clears the reason", func(t *testing.T) {
		ev := mkEvidence(ctx, t, q, runA, userA, repoA, "a/dismissed.go#f")
		d := mkDisposition(ctx, t, q, userA, repoA, "a/dismissed.go#f")
		mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='dismissed', dismiss_reason='wont_do', resolved_at=now() WHERE id=$1`, d)
		markOK(t, ev.ID, d)
		if got := readFullDisp(ctx, t, pool, d); got.status != "done" || got.dismissReason.Valid {
			t.Fatalf("dismissed→done = (%s, reason %v), want (done, NULL)", got.status, got.dismissReason)
		}
	})

	t.Run("done re-asserted", func(t *testing.T) {
		ev := mkEvidence(ctx, t, q, runA, userA, repoA, "a/done.go#f")
		d := mkDisposition(ctx, t, q, userA, repoA, "a/done.go#f")
		markOK(t, ev.ID, d)
		markOK(t, ev.ID, d)
		if got := readFullDisp(ctx, t, pool, d); got.status != "done" || got.setVia.Valid {
			t.Fatalf("done→done = %+v, want a human done", got)
		}
	})

	t.Run("sync done becomes a human done, edge preserved", func(t *testing.T) {
		ev := mkEvidence(ctx, t, q, runA, userA, repoA, "a/syncdone.go#f")
		d := mkDisposition(ctx, t, q, userA, repoA, "a/syncdone.go#f")
		mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='done', set_via='issue_close', filed_issue_iid=8, filed_issue_url='u8', resolved_at=now(), close_synced_at=now() - interval '1 hour' WHERE id=$1`, d)
		before := readFullDisp(ctx, t, pool, d)
		markOK(t, ev.ID, d)
		got := readFullDisp(ctx, t, pool, d)
		if got.status != "done" || got.setVia.Valid {
			t.Fatalf("sync done→done = (%s, set_via %v), want (done, NULL)", got.status, got.setVia)
		}
		if !sameTS(got.closeSyncedAt, before.closeSyncedAt) {
			t.Errorf("close_synced_at %v -> %v, want preserved", before.closeSyncedAt.Time, got.closeSyncedAt.Time)
		}
		if !got.filedIID.Valid || got.filedIID.Int64 != 8 {
			t.Errorf("filed_issue_iid = %v, want 8 kept", got.filedIID)
		}
	})

	t.Run("absent disposition inserts a done row with the canonical hash", func(t *testing.T) {
		ev := mkEvidence(ctx, t, q, runA, userA, repoA, "a/absent.go#f")
		rec := markDone(t, userA, false, ev.ID)
		if rec.Code != http.StatusOK {
			t.Fatalf("absent → %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var dto apitypes.MarkFindingDoneResultDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
			t.Fatalf("decode: %v", err)
		}
		id, err := uuid.Parse(dto.DispositionID)
		if err != nil {
			t.Fatalf("disposition_id %q: %v", dto.DispositionID, err)
		}
		got := readFullDisp(ctx, t, pool, id)
		if got.status != "done" || got.setVia.Valid || !got.resolvedAt.Valid {
			t.Fatalf("inserted row = %+v, want a human done with resolved_at", got)
		}
		if want := workersvc.FindingContentHash(ev.Title, ev.DescriptionMd); got.contentHash != want {
			t.Errorf("content_hash = %q, want the canonical %q", got.contentHash, want)
		}
		if got.lastTitle != ev.Title {
			t.Errorf("last_title = %q, want %q", got.lastTitle, ev.Title)
		}
		// The canonical hash is what makes an identical re-report stay suppressed.
		n, err := q.ReopenDispositionOnHashMismatch(ctx, store.ReopenDispositionOnHashMismatchParams{
			UserID: userA, RepoID: repoA, Location: "a/absent.go#f",
			ContentHash: workersvc.FindingContentHash(ev.Title, ev.DescriptionMd), LastTitle: ev.Title,
		})
		if err != nil || n != 0 {
			t.Errorf("identical re-report reopened %d rows (err %v), want 0", n, err)
		}
	})

	t.Run("filing is a 409 and the row is untouched", func(t *testing.T) {
		ev := mkEvidence(ctx, t, q, runA, userA, repoA, "a/filing.go#f")
		d := mkDisposition(ctx, t, q, userA, repoA, "a/filing.go#f")
		if n, err := q.ClaimFindingForFiling(ctx, store.ClaimFindingForFilingParams{UserID: userA, RepoID: repoA, Location: "a/filing.go#f"}); err != nil || n != 1 {
			t.Fatalf("claim: n=%d err=%v", n, err)
		}
		before := readFullDisp(ctx, t, pool, d)
		rec := markDone(t, userA, false, ev.ID)
		if rec.Code != http.StatusConflict {
			t.Fatalf("filing → %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
		if after := readFullDisp(ctx, t, pool, d); after != before {
			t.Fatalf("filing row changed: %+v -> %+v", before, after)
		}
	})

	t.Run("owner scoping: second user and admin get 404", func(t *testing.T) {
		ev := mkEvidence(ctx, t, q, runA, userA, repoA, "a/scoped.go#f")
		d := mkDisposition(ctx, t, q, userA, repoA, "a/scoped.go#f")
		for _, c := range []struct {
			name    string
			user    uuid.UUID
			isAdmin bool
		}{{"second user", userB, false}, {"admin", admin, true}} {
			rec := markDone(t, c.user, c.isAdmin, ev.ID)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s → %d, want 404; body=%s", c.name, rec.Code, rec.Body.String())
			}
		}
		if got := readFullDisp(ctx, t, pool, d); got.status != "open" {
			t.Fatalf("userA's row after foreign marks = %s, want open", got.status)
		}
		// Unknown evidence id is a 404 too.
		if rec := markDone(t, userA, false, uuid.New()); rec.Code != http.StatusNotFound {
			t.Fatalf("unknown id → %d, want 404", rec.Code)
		}
	})
}

// ── (f) bulk done: skips foreign/unknown/filing, dedups, re-asserts done, returns applied rows ──
func TestBulkMarkFindingsDoneLiveDB(t *testing.T) {
	h, pool, q := findingsDismissLiveDB(t)
	ctx := context.Background()
	userA, connA := seedFindingsUser(ctx, t, pool)
	userB, connB := seedFindingsUser(ctx, t, pool)
	admin := seedAdmin(ctx, t, pool)
	repoA := seedFindingsRepo(ctx, t, pool, connA, 1, "g/"+uuid.NewString()[:8])
	repoB := seedFindingsRepo(ctx, t, pool, connB, 2, "g/"+uuid.NewString()[:8])

	dOpen := mkDisposition(ctx, t, q, userA, repoA, "a/open.go#f")
	dFiled := mkDisposition(ctx, t, q, userA, repoA, "a/filed.go#f")
	mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='filed', filed_issue_iid=7, filed_issue_url='u7', resolved_at=now() WHERE id=$1`, dFiled)
	dDismissed := mkDisposition(ctx, t, q, userA, repoA, "a/dismissed.go#f")
	mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='dismissed', dismiss_reason='not_an_issue', resolved_at=now() WHERE id=$1`, dDismissed)
	dDone := mkDisposition(ctx, t, q, userA, repoA, "a/done.go#f")
	mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='done', set_via='issue_close', filed_issue_iid=9, resolved_at=now(), close_synced_at=now() WHERE id=$1`, dDone)
	dFiling := mkDisposition(ctx, t, q, userA, repoA, "a/filing.go#f")
	mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='filing', filing_since=now() WHERE id=$1`, dFiling)
	dUntouched := mkDisposition(ctx, t, q, userA, repoA, "a/untouched.go#f")
	dForeign := mkDisposition(ctx, t, q, userB, repoB, "b/open.go#f")
	doneSyncedBefore := readFullDisp(ctx, t, pool, dDone).closeSyncedAt

	bulk := func(t *testing.T, userID uuid.UUID, isAdmin bool, ids ...uuid.UUID) apitypes.BulkMarkFindingsDoneResultDTO {
		t.Helper()
		quoted := make([]string, 0, len(ids))
		for _, id := range ids {
			quoted = append(quoted, fmt.Sprintf("%q", id.String()))
		}
		body := fmt.Sprintf(`{"ids":[%s]}`, strings.Join(quoted, ","))
		u := store.User{ID: userID, IsAdmin: isAdmin}
		rec := httptest.NewRecorder()
		h.BulkMarkFindingsDone(rec, findingsBodyReq(http.MethodPost, &u, body))
		if rec.Code != http.StatusOK {
			t.Fatalf("BulkMarkFindingsDone = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp apitypes.BulkMarkFindingsDoneResultDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Findings == nil {
			t.Fatalf("findings is nil; the wire contract is [] never null")
		}
		return resp
	}

	// Admin and second user first: nothing of userA's moves.
	for _, c := range []struct {
		name    string
		user    uuid.UUID
		isAdmin bool
	}{{"second user", userB, false}, {"admin", admin, true}} {
		if resp := bulk(t, c.user, c.isAdmin, dOpen, dFiled); resp.Updated != 0 || len(resp.Findings) != 0 {
			t.Fatalf("%s bulk on userA ids = %+v, want 0 updated", c.name, resp)
		}
	}
	if s := readFullDisp(ctx, t, pool, dOpen).status; s != "open" {
		t.Fatalf("dOpen after foreign bulk = %s, want open", s)
	}

	resp := bulk(t, userA, false, dOpen, dFiled, dDismissed, dDone, dOpen, dFiling, dForeign, uuid.New())
	if resp.Updated != 4 || len(resp.Findings) != 4 {
		t.Fatalf("updated=%d findings=%d, want 4/4 (open, filed, dismissed, done; dup, filing, foreign, unknown skipped)", resp.Updated, len(resp.Findings))
	}
	gotIDs := make([]string, 0, 4)
	for _, f := range resp.Findings {
		gotIDs = append(gotIDs, f.DispositionID)
		if f.Status != "done" || f.SetVia != "" || f.DismissReason != "" {
			t.Errorf("returned %s = (status %s, set_via %q, reason %q), want a human done", f.DispositionID, f.Status, f.SetVia, f.DismissReason)
		}
	}
	wantIDs := []string{dOpen.String(), dFiled.String(), dDismissed.String(), dDone.String()}
	sort.Strings(gotIDs)
	sort.Strings(wantIDs)
	if fmt.Sprint(gotIDs) != fmt.Sprint(wantIDs) {
		t.Fatalf("returned ids = %v, want %v", gotIDs, wantIDs)
	}

	for _, id := range []uuid.UUID{dOpen, dFiled, dDismissed, dDone} {
		if got := readFullDisp(ctx, t, pool, id); got.status != "done" || got.setVia.Valid || got.dismissReason.Valid {
			t.Errorf("%s = %+v, want a human done", id, got)
		}
	}
	if got := readFullDisp(ctx, t, pool, dFiled); !got.filedIID.Valid || got.filedIID.Int64 != 7 || got.filedURL != "u7" {
		t.Errorf("filed issue link lost: %+v", got)
	}
	if got := readFullDisp(ctx, t, pool, dDone); !sameTS(got.closeSyncedAt, doneSyncedBefore) {
		t.Errorf("sync-done close_synced_at changed %v -> %v, want preserved", doneSyncedBefore.Time, got.closeSyncedAt.Time)
	}
	if s := readFullDisp(ctx, t, pool, dFiling).status; s != "filing" {
		t.Errorf("dFiling = %s, want filing (skipped)", s)
	}
	if s := readFullDisp(ctx, t, pool, dForeign).status; s != "open" {
		t.Errorf("dForeign = %s, want open (owner scoping)", s)
	}
	if s := readFullDisp(ctx, t, pool, dUntouched).status; s != "open" {
		t.Errorf("dUntouched = %s, want open (not in request)", s)
	}

	// An empty request is a 200 with [] (never null).
	if resp := bulk(t, userA, false); resp.Updated != 0 || len(resp.Findings) != 0 {
		t.Fatalf("empty bulk = %+v, want 0/[]", resp)
	}
}

// ── (g) Undo of either verdict: filed vs open target; 404s; (h) owner scoping ──
func TestUndoFindingDispositionLiveDB(t *testing.T) {
	h, pool, q := findingsDismissLiveDB(t)
	ctx := context.Background()
	userA, connA := seedFindingsUser(ctx, t, pool)
	userB, _ := seedFindingsUser(ctx, t, pool)
	admin := seedAdmin(ctx, t, pool)
	repoA := seedFindingsRepo(ctx, t, pool, connA, 1, "g/"+uuid.NewString()[:8])
	runA := seedDoneRun(ctx, t, pool, userA, repoA)

	undo := func(t *testing.T, userID uuid.UUID, isAdmin bool, id uuid.UUID) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		h.UndoFindingDisposition(rec, undoDispReq(userID, id, isAdmin))
		return rec
	}
	undoOK := func(t *testing.T, id uuid.UUID) apitypes.IncidentalFindingDTO {
		t.Helper()
		rec := undo(t, userA, false, id)
		if rec.Code != http.StatusOK {
			t.Fatalf("undo %s = %d, want 200; body=%s", id, rec.Code, rec.Body.String())
		}
		var dto apitypes.IncidentalFindingDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if dto.DispositionID != id.String() {
			t.Fatalf("undo response disposition_id = %s, want %s", dto.DispositionID, id)
		}
		return dto
	}
	humanDone := func(t *testing.T, location string) {
		t.Helper()
		ev := mkEvidence(ctx, t, q, runA, userA, repoA, location)
		rec := httptest.NewRecorder()
		h.MarkFindingDone(rec, markDoneReq(userA, ev.ID, false))
		if rec.Code != http.StatusOK {
			t.Fatalf("MarkFindingDone(%s) = %d; body=%s", location, rec.Code, rec.Body.String())
		}
	}

	t.Run("human done on filed returns to filed", func(t *testing.T) {
		d := mkDisposition(ctx, t, q, userA, repoA, "a/filed.go#f")
		mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='filed', filed_issue_iid=7, filed_issue_url='u7', resolved_at=now() WHERE id=$1`, d)
		humanDone(t, "a/filed.go#f")
		dto := undoOK(t, d)
		if dto.Status != "filed" || dto.FiledIssueIID == nil || *dto.FiledIssueIID != 7 || dto.ResolvedAt == nil {
			t.Fatalf("undo DTO = %+v, want filed with iid 7 and resolved_at", dto)
		}
		got := readFullDisp(ctx, t, pool, d)
		if got.status != "filed" || !got.filedIID.Valid || got.filedIID.Int64 != 7 || got.filedURL != "u7" || !got.resolvedAt.Valid || got.setVia.Valid {
			t.Fatalf("DB = %+v, want filed, iid 7/u7 kept, resolved_at non-null, set_via NULL", got)
		}
	})

	t.Run("human done from open returns to open", func(t *testing.T) {
		d := mkDisposition(ctx, t, q, userA, repoA, "a/open.go#f")
		humanDone(t, "a/open.go#f")
		undoOK(t, d)
		if got := readFullDisp(ctx, t, pool, d); got.status != "open" || got.resolvedAt.Valid || got.dismissReason.Valid {
			t.Fatalf("DB = %+v, want open with resolved_at NULL, reason NULL", got)
		}
	})

	t.Run("human done from dismissed returns to open, not dismissed", func(t *testing.T) {
		d := mkDisposition(ctx, t, q, userA, repoA, "a/dismissed.go#f")
		mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='dismissed', dismiss_reason='wont_do', resolved_at=now() WHERE id=$1`, d)
		humanDone(t, "a/dismissed.go#f")
		undoOK(t, d)
		if got := readFullDisp(ctx, t, pool, d); got.status != "open" || got.resolvedAt.Valid || got.dismissReason.Valid {
			t.Fatalf("DB = %+v, want open with resolved_at NULL, reason NULL", got)
		}
	})

	t.Run("sync done returns to filed with the edge preserved", func(t *testing.T) {
		d := mkDisposition(ctx, t, q, userA, repoA, "a/syncdone.go#f")
		mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='done', set_via='issue_close', filed_issue_iid=8, filed_issue_url='u8', resolved_at=now(), close_synced_at=now() - interval '1 hour' WHERE id=$1`, d)
		before := readFullDisp(ctx, t, pool, d)
		undoOK(t, d)
		got := readFullDisp(ctx, t, pool, d)
		if got.status != "filed" || got.setVia.Valid || !got.filedIID.Valid || got.filedIID.Int64 != 8 {
			t.Fatalf("DB = %+v, want filed, set_via NULL, iid 8", got)
		}
		if !sameTS(got.closeSyncedAt, before.closeSyncedAt) {
			t.Errorf("close_synced_at %v -> %v, want preserved", before.closeSyncedAt.Time, got.closeSyncedAt.Time)
		}
	})

	t.Run("dismissed via the new route returns to open", func(t *testing.T) {
		d := mkDisposition(ctx, t, q, userA, repoA, "a/dismissed2.go#f")
		mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='dismissed', dismiss_reason='not_an_issue', resolved_at=now() WHERE id=$1`, d)
		if dto := undoOK(t, d); dto.Status != "open" || dto.DismissReason != "" {
			t.Fatalf("undo DTO = %+v, want open with no reason", dto)
		}
	})

	t.Run("open, filed, filing, foreign, admin and unknown are 404", func(t *testing.T) {
		dOpen := mkDisposition(ctx, t, q, userA, repoA, "a/404open.go#f")
		dFiled := mkDisposition(ctx, t, q, userA, repoA, "a/404filed.go#f")
		mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='filed', filed_issue_iid=11, resolved_at=now() WHERE id=$1`, dFiled)
		dFiling := mkDisposition(ctx, t, q, userA, repoA, "a/404filing.go#f")
		mustExecT(ctx, t, pool, `UPDATE finding_dispositions SET status='filing', filing_since=now() WHERE id=$1`, dFiling)
		for _, id := range []uuid.UUID{dOpen, dFiled, dFiling, uuid.New()} {
			if rec := undo(t, userA, false, id); rec.Code != http.StatusNotFound {
				t.Fatalf("undo %s = %d, want 404", id, rec.Code)
			}
		}
		dDone := mkDisposition(ctx, t, q, userA, repoA, "a/404done.go#f")
		humanDone(t, "a/404done.go#f")
		for _, c := range []struct {
			name    string
			user    uuid.UUID
			isAdmin bool
		}{{"second user", userB, false}, {"admin", admin, true}} {
			if rec := undo(t, c.user, c.isAdmin, dDone); rec.Code != http.StatusNotFound {
				t.Fatalf("%s undo = %d, want 404", c.name, rec.Code)
			}
		}
		if s := readFullDisp(ctx, t, pool, dDone).status; s != "done" {
			t.Fatalf("userA's done after foreign undo = %s, want done", s)
		}
		if s := readFullDisp(ctx, t, pool, dFiled).status; s != "filed" {
			t.Fatalf("filed after a 404 undo = %s, want filed", s)
		}
	})
}
