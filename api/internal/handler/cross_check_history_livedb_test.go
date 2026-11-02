package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

type historyLiveFixture struct {
	t           *testing.T
	h           *Handler
	router      http.Handler
	pool        *pgxpool.Pool
	owner, repo uuid.UUID
	jwt, token  string
}

func newHistoryLiveFixture(t *testing.T) *historyLiveFixture {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	t.Cleanup(func() { cliMustExec(t, pool, "DELETE FROM users WHERE id=$1", owner) })
	repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), 3811, true)
	return &historyLiveFixture{t: t, h: h, router: router, pool: pool, owner: owner, repo: repo,
		jwt: cliMintJWT(t, pool, owner), token: cliMintToken(t, pool, owner, "user")}
}

func (f *historyLiveFixture) parent(status, title string) uuid.UUID {
	f.t.Helper()
	id := rmSeedRun(f.t, f.pool, f.owner, f.repo, status)
	cliMustExec(f.t, f.pool, `UPDATE runs SET harness='claude',claim_generation=1,issue_title=$2,
 plan_md='candidate',milestones_candidate='[]',milestones_frozen='[]',size_class='s' WHERE id=$1`, id, title)
	return id
}

func (f *historyLiveFixture) child(parent uuid.UUID, status string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	cliMustExec(f.t, f.pool, `INSERT INTO runs(id,user_id,repo_id,kind,target_run_id,harness,status,report_only,budget_wall_seconds,issue_title,issue_description)
 VALUES($1,$2,$3,'cross_check',$4,'codex',$5,true,1800,'checker','private context')`, id, f.owner, f.repo, parent, status)
	return id
}

func (f *historyLiveFixture) plan(parent uuid.UUID, child any, round int, result, reason string, created time.Time) {
	f.t.Helper()
	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}
	cliMustExec(f.t, f.pool, `INSERT INTO cross_checks(lead_run_id,checker_run_id,checker_harness,stage,round,lead_claim_generation,
 plan_md,milestones,size_class,base_commit,candidate_digest,verdict,reason_class,findings,created_at,deadline_at,decided_at)
 VALUES($1,$2,'codex','plan',$3,1,'candidate','[]','s',repeat('a',40),$4,$5,$6,
 '{"summary":"private raw findings","items":[]}',$7::timestamptz,$7::timestamptz+interval '30 minutes',
 CASE WHEN $5::text='pending' THEN NULL ELSE $7::timestamptz END)`, parent, child, round, []byte("digest"), result, reasonArg, created)
}

func (f *historyLiveFixture) code(parent uuid.UUID, child any, outcome, reason, findings string, created time.Time) {
	f.t.Helper()
	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}
	cliMustExec(f.t, f.pool, `INSERT INTO cross_checks(lead_run_id,checker_run_id,checker_harness,stage,round,lead_claim_generation,
 base_commit,head_commit,candidate_digest,verdict,outcome,reason_class,code_context,guidance_snapshot,
 guidance_text,guidance_digest,repo_instructions_text,repo_instructions_digest,findings,created_at,deadline_at)
 VALUES($1,$2,'codex','code',1,1,repeat('a',40),repeat('b',40),$3,'failed',$4,$5,
 '{"private":"context"}','private instructions','private instructions',sha256(convert_to('private instructions','UTF8')),'',
 sha256(convert_to('','UTF8')),$6::jsonb,$7::timestamptz,$7::timestamptz+interval '30 minutes')`, parent, child, []byte("digest"), outcome, reasonArg, findings, created)
}

func (f *historyLiveFixture) call(path string, cli bool) *httptest.ResponseRecorder {
	f.t.Helper()
	if cli {
		return bearerReq(f.router, http.MethodGet, path, f.token)
	}
	return cookieReq(f.t, f.router, http.MethodGet, path, f.jwt, "")
}

func (f *historyLiveFixture) list(query string, cli bool) apitypes.CrossCheckHistoryDTO {
	f.t.Helper()
	rec := f.call("/api/cross-checks?"+query, cli)
	if rec.Code != 200 {
		f.t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var dto apitypes.CrossCheckHistoryDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		f.t.Fatal(err)
	}
	for _, raw := range []string{"private raw findings", "private context", "private instructions", "candidate", "provider error"} {
		if strings.Contains(rec.Body.String(), raw) {
			f.t.Fatalf("raw evidence escaped: %s", raw)
		}
	}
	return dto
}

func TestCrossCheckHistoryOwnerAuthLiveDB(t *testing.T) {
	a, b := newHistoryLiveFixture(t), newHistoryLiveFixture(t)
	for _, f := range []*historyLiveFixture{a, b} {
		for _, cli := range []bool{false, true} {
			rec := f.call("/api/cross-checks/availability?stage=code&limit=1", cli)
			if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"available":false}` {
				t.Fatalf("no evidence availability: %d %s", rec.Code, rec.Body.String())
			}
			if len(f.list("lifecycle=past", cli).Groups) != 0 {
				t.Fatal("owner saw foreign evidence")
			}
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	aParent := a.parent("completed", "owner A")
	a.plan(aParent, a.child(aParent, "completed"), 1, "approve", "approve", now)
	// B remains empty while A has a positive control, for both routes and identities.
	for _, cli := range []bool{false, true} {
		rec := b.call("/api/cross-checks/availability", cli)
		if strings.TrimSpace(rec.Body.String()) != `{"available":false}` || len(b.list("lifecycle=past", cli).Groups) != 0 {
			t.Fatal("B saw A")
		}
	}
	bParent := b.parent("completed", "owner B")
	b.plan(bParent, b.child(bParent, "completed"), 1, "block", "block", now)
	for _, f := range []*historyLiveFixture{a, b} {
		for _, path := range []string{"/api/cross-checks/availability", "/api/cross-checks?lifecycle=past"} {
			cookie, cli := f.call(path, false), f.call(path, true)
			if cookie.Code != 200 || cli.Code != 200 || cookie.Body.String() != cli.Body.String() {
				t.Fatalf("cookie/CLI mismatch %s: %d/%d", path, cookie.Code, cli.Code)
			}
			if path == "/api/cross-checks/availability" && strings.TrimSpace(cookie.Body.String()) != `{"available":true}` {
				t.Fatal("availability missed retained evidence")
			}
		}
		got := f.list("lifecycle=past", true)
		if len(got.Groups) != 1 || got.Groups[0].ParentID != map[*historyLiveFixture]uuid.UUID{a: aParent, b: bParent}[f].String() {
			t.Fatal("owner evidence not isolated")
		}
	}
	admin := cliSeedUser(t, a.pool, true)
	t.Cleanup(func() { cliMustExec(t, a.pool, "DELETE FROM users WHERE id=$1", admin) })
	for _, path := range []string{"/api/cross-checks/availability", "/api/cross-checks?lifecycle=past"} {
		unauth := bearerReq(a.router, "GET", path, "")
		if unauth.Code != 401 {
			t.Fatalf("unauth %s=%d", path, unauth.Code)
		}
		rec := cookieReq(t, a.router, "GET", path, cliMintJWT(t, a.pool, admin), "")
		if rec.Code != 200 || strings.Contains(rec.Body.String(), aParent.String()) || strings.Contains(rec.Body.String(), bParent.String()) || strings.Contains(rec.Body.String(), `"available":true`) {
			t.Fatal("nonowner admin saw private history")
		}
	}
	foreign := encodeCrossCheckHistoryCursor(crossCheckHistoryCursor{Version: 1, Kind: "attempt", Filter: "past/all/any", Parent: aParent, Round: 2})
	missing := encodeCrossCheckHistoryCursor(crossCheckHistoryCursor{Version: 1, Kind: "attempt", Filter: "past/all/any", Parent: uuid.New(), Round: 2})
	for _, cli := range []bool{false, true} {
		denied := b.call("/api/cross-checks?lifecycle=past&attempt_cursor="+foreign, cli)
		absent := b.call("/api/cross-checks?lifecycle=past&attempt_cursor="+missing, cli)
		if denied.Code != 404 || absent.Code != 404 || denied.Body.String() != absent.Body.String() {
			t.Fatal("foreign continuation not generically denied")
		}
	}
}

func TestCrossCheckHistoryWindowsAndReadPurityLiveDB(t *testing.T) {
	f := newHistoryLiveFixture(t)
	parent := f.parent("completed", "many retained rounds")
	base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	// Schema-valid legacy nonautomatic rounds: never widen the automatic budget.
	for round := 1; round <= 31; round++ {
		child := any(nil)
		if round == 1 {
			child = f.child(parent, "running")
		}
		result, reason := "revise", "revise"
		if round == 31 {
			result, reason = "pending", ""
		}
		f.plan(parent, child, round, result, reason, base.Add(time.Duration(round)*time.Second))
	}
	f.code(parent, f.child(parent, "completed"), "completed", "", "[]", base.Add(time.Hour))
	snapshot := func() string {
		var raw string
		if err := f.pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
 'checks',(SELECT jsonb_agg(to_jsonb(cc) ORDER BY stage,round) FROM cross_checks cc WHERE lead_run_id=$1),
 'lead',(SELECT to_jsonb(r) FROM runs r WHERE id=$1),
 'children',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM runs r WHERE target_run_id=$1))::text`, parent).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := snapshot()
	defaultWindow := f.list("", false).Groups[0]
	if len(defaultWindow.Attempts) != 10 || defaultWindow.Attempts[8].Round != 31 ||
		defaultWindow.Attempts[9].Stage != "code" || defaultWindow.OlderCursor == "" {
		t.Fatal("default window hid newest running Plan or retained Code")
	}
	maxWindow := f.list("attempt_limit=20", true).Groups[0]
	if len(maxWindow.Attempts) != 20 || maxWindow.Attempts[18].Round != 31 || maxWindow.Attempts[19].Stage != "code" {
		t.Fatal("maximum window exceeded cap or hid newest evidence")
	}
	got := f.list("attempt_limit=2", false)
	if len(got.Groups) != 1 {
		t.Fatal("expired persisted pending must remain active on GET")
	}
	g := got.Groups[0]
	if g.TotalAttempts != 32 || g.MatchingAttempts != 32 || g.ActiveAttempts != 2 || g.ActiveHidden != 1 || g.ActiveContext == "" ||
		len(g.Attempts) != 2 || g.Attempts[0].Stage != "plan" || g.Attempts[0].Round != 31 || g.Attempts[1].Stage != "code" ||
		g.Attempts[1].Result != "completed" || g.Attempts[1].Review != "approved" || g.OlderCursor == "" ||
		g.Omission != "Earlier rounds omitted: load older attempts" {
		t.Fatalf("initial window: %+v", g)
	}
	// Each page displays its selected window chronologically, even though older
	// windows arrive after newer ones. Pin every identity, not just membership.
	assertWindow := func(attempts []apitypes.CrossCheckHistoryAttemptDTO, first, last int, code bool) {
		t.Helper()
		wantLen := last - first + 1
		if code {
			wantLen++
		}
		if len(attempts) != wantLen {
			t.Fatalf("window %d..%d code=%t: got %d attempts want %d", first, last, code, len(attempts), wantLen)
		}
		for i, a := range attempts {
			stage, round := "plan", first+i
			if code && i == len(attempts)-1 {
				stage, round = "code", 1
			}
			if a.Stage != stage || int(a.Round) != round {
				t.Fatalf("window %d..%d index %d: got %s/%d want %s/%d", first, last, i, a.Stage, a.Round, stage, round)
			}
			if stage == "plan" && a.Historical != (round < 31) {
				t.Fatalf("plan/%d historical=%t want %t", round, a.Historical, round < 31)
			}
		}
	}
	assertWindow(defaultWindow.Attempts, 23, 31, true)
	assertWindow(maxWindow.Attempts, 13, 31, true)
	assertWindow(g.Attempts, 31, 31, true)
	seen := map[string]bool{}
	for _, a := range g.Attempts {
		seen[fmt.Sprintf("%s/%d", a.Stage, a.Round)] = true
	}
	cursor := g.OlderCursor
	pages := 0
	for cursor != "" {
		pages++
		if pages > 20 {
			t.Fatal("older traversal not bounded")
		}
		page := f.list("attempt_limit=2&attempt_cursor="+cursor, true)
		if page.NextCursor != "" || len(page.Groups) != 1 || page.Groups[0].RefreshNeeded {
			t.Fatal("continuation fetched fresh groups or invalidated")
		}
		g = page.Groups[0]
		assertWindow(g.Attempts, 31-2*pages, 32-2*pages, false)
		for _, a := range g.Attempts {
			key := fmt.Sprintf("%s/%d", a.Stage, a.Round)
			if seen[key] {
				t.Fatalf("duplicate older key: %s", key)
			}
			seen[key] = true
		}
		cursor = g.OlderCursor
	}
	if len(seen) != 32 || len(g.Attempts) != 2 || g.Attempts[0].Round != 1 || g.Attempts[1].Round != 2 {
		t.Fatal("full chronological traversal lost evidence")
	}
	filtered := f.list("stage=code", true).Groups[0]
	if filtered.MatchingAttempts != 1 || filtered.ActiveHidden != 2 || len(filtered.Attempts) != 1 {
		t.Fatal("stage filter hid active context")
	}
	if before != snapshot() {
		t.Fatal("GET mutated expired lifecycle evidence")
	}
	// Settle all active evidence, then distinguish lifecycle invalidation from exhaustion.
	stale := got.Groups[0].OlderCursor
	cliMustExec(t, f.pool, "UPDATE cross_checks SET verdict='failed',reason_class='timed_out',decided_at=deadline_at WHERE lead_run_id=$1 AND stage='plan' AND round=31", parent)
	cliMustExec(t, f.pool, "UPDATE runs SET status='completed' WHERE target_run_id=$1 AND kind='cross_check'", parent)
	refreshed := f.list("attempt_cursor="+stale, false)
	if len(refreshed.Groups) != 1 || !refreshed.Groups[0].RefreshNeeded || len(refreshed.Groups[0].Attempts) != 0 || refreshed.Groups[0].OlderCursor != "" {
		t.Fatal("invalidated lifecycle masqueraded as exhausted")
	}
	if len(f.list("", true).Groups) != 0 || len(f.list("lifecycle=past", true).Groups) != 1 {
		t.Fatal("lifecycle union did not change after persisted settlement")
	}
}

func TestCrossCheckHistoryChildBindingsLiveDB(t *testing.T) {
	for _, dimension := range []string{"valid", "owner", "kind", "target", "harness", "deleted", "absent"} {
		t.Run(dimension, func(t *testing.T) {
			f := newHistoryLiveFixture(t)
			parent := f.parent("completed", "binding")
			var child any
			if dimension != "absent" {
				id := f.child(parent, "running")
				child = id
				switch dimension {
				case "owner":
					other := cliSeedUser(t, f.pool, false)
					t.Cleanup(func() { cliMustExec(t, f.pool, "DELETE FROM users WHERE id=$1", other) })
					cliMustExec(t, f.pool, "UPDATE runs SET user_id=$2 WHERE id=$1", id, other)
				case "kind":
					cliMustExec(t, f.pool, "UPDATE runs SET kind='prompt' WHERE id=$1", id)
				case "target":
					cliMustExec(t, f.pool, "UPDATE runs SET target_run_id=$2 WHERE id=$1", id, f.parent("completed", "other"))
				case "harness":
					cliMustExec(t, f.pool, "UPDATE runs SET harness='claude' WHERE id=$1", id)
				}
			}
			f.plan(parent, child, 1, "approve", "approve", time.Now())
			if dimension == "deleted" {
				cliMustExec(t, f.pool, "DELETE FROM runs WHERE id=$1", child)
			}
			query := "lifecycle=past"
			if dimension == "valid" {
				query = ""
			}
			g := f.list(query, false).Groups
			if len(g) != 1 || len(g[0].Attempts) != 1 {
				t.Fatalf("binding group: %+v", g)
			}
			a := g[0].Attempts[0]
			if dimension == "valid" {
				if !g[0].Active || a.ChildID == nil || a.ChildStatus == nil || *a.ChildStatus != "running" || a.Execution != "running" || a.Review != "approved" {
					t.Fatal("terminal parent/ACK lag not active")
				}
			} else if g[0].Active || a.ChildID != nil || a.ChildStatus != nil || a.Execution != "unknown" || a.Review != "approved" || a.ChildContext != "No checker run available" {
				t.Fatalf("invalid child metadata trusted: %+v", a)
			}
		})
	}
}

func TestCrossCheckHistoryGroupKeysetsLiveDB(t *testing.T) {
	f := newHistoryLiveFixture(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	ids := make([]string, 0, 211)
	for i := 0; i < 211; i++ {
		id := f.parent("completed", "tied group")
		f.plan(id, nil, 1, "approve", "approve", base)
		ids = append(ids, id.String())
	}
	// Newest group does not qualify for Plan + passed. Filtering must precede LIMIT.
	extra := f.parent("completed", "unmatched")
	f.code(extra, nil, "failed", "snapshot_failed", "null", base.Add(time.Hour))
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	gotIDs := []string{}
	cursor := ""
	for page := 0; page < 10; page++ {
		q := "lifecycle=past&stage=plan&outcome=passed&limit=25"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		got := f.list(q, page%2 == 0)
		for _, g := range got.Groups {
			gotIDs = append(gotIDs, g.ParentID)
		}
		cursor = got.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(gotIDs) != len(ids) || cursor != "" {
		t.Fatalf("retained parents truncated: got %d want %d", len(gotIDs), len(ids))
	}
	for i := range ids {
		if gotIDs[i] != ids[i] {
			t.Fatalf("first keyset mismatch at %d: got %s want %s", i, gotIDs[i], ids[i])
		}
	}
}

type historyQueryTraceKey struct{}
type historyQueryTrace struct {
	mu         sync.Mutex
	queries    int
	groupReads int
	afterGroup func()
}

func (tr *historyQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "-- name: CrossCheckHistory") {
		tr.mu.Lock()
		tr.queries++
		tr.mu.Unlock()
	}
	return context.WithValue(ctx, historyQueryTraceKey{}, strings.Contains(data.SQL, "-- name: CrossCheckHistoryGroups"))
}
func (tr *historyQueryTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	group, _ := ctx.Value(historyQueryTraceKey{}).(bool)
	if !group {
		return
	}
	tr.mu.Lock()
	tr.groupReads++
	hook := tr.afterGroup
	tr.afterGroup = nil
	tr.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func TestCrossCheckHistorySnapshotAndBatchingLiveDB(t *testing.T) {
	f := newHistoryLiveFixture(t)
	parents := []uuid.UUID{}
	for i := 0; i < 8; i++ {
		parent := f.parent("completed", "batch")
		child := f.child(parent, "running")
		f.plan(parent, child, 1, "approve", "approve", time.Now())
		f.plan(parent, nil, 2, "block", "block", time.Now())
		f.plan(parent, nil, 3, "revise", "revise", time.Now())
		parents = append(parents, parent)
	}
	trace := &historyQueryTrace{}
	cfg := f.pool.Config()
	cfg.ConnConfig.Tracer = trace
	traced, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	f.h.pool = traced
	trace.afterGroup = func() {
		// This commits after the group query but before the attempts query. Both
		// response halves must retain the original nonterminal child evidence.
		cliMustExec(t, f.pool, "UPDATE runs SET status='completed' WHERE target_run_id=ANY($1::uuid[])", parents)
	}
	got := f.list("attempt_limit=3", false)
	if len(got.Groups) != 8 {
		t.Fatal("lost batch groups")
	}
	for _, g := range got.Groups {
		if !g.Active || g.ActiveAttempts != 1 || g.ActiveHidden != 0 || len(g.Attempts) != 3 || g.Attempts[0].ChildStatus == nil || *g.Attempts[0].ChildStatus != "running" || g.Attempts[0].Execution != "running" {
			t.Fatal("batch halves did not retain the same initial snapshot")
		}
	}
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 2 {
		t.Fatalf("batch reads=%d want 2", queries)
	}
	// Reload in Past, showing completed child now, then request all older windows
	// in one vector. No fresh group page is allowed.
	past := f.list("lifecycle=past&attempt_limit=2", true)
	values := url.Values{"lifecycle": {"past"}, "attempt_limit": {"2"}}
	for _, g := range past.Groups {
		if g.OlderCursor == "" {
			t.Fatal("missing older cursor")
		}
		values.Add("attempt_cursor", g.OlderCursor)
	}
	trace.mu.Lock()
	start := trace.queries
	trace.mu.Unlock()
	older := f.list(values.Encode(), false)
	trace.mu.Lock()
	queries = trace.queries - start
	trace.mu.Unlock()
	if queries != 2 || len(older.Groups) != 8 || older.NextCursor != "" {
		t.Fatalf("vector not batched: %d queries, %d groups", queries, len(older.Groups))
	}
	for _, g := range older.Groups {
		if len(g.Attempts) != 1 || g.Attempts[0].Round != 1 || g.Attempts[0].Execution != "finished" {
			t.Fatal("older vector did not return original retained evidence")
		}
	}
}
