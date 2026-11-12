package handler

// Issue #4198 M2 live-DB proofs for the admin run debug READS: GET /api/runs/{id}/inputs,
// /archives, /fetches and GET /api/recovery/holds?user=|?run=. They drive the REAL h.Routes()
// router with really minted tokens and cookies (the property under test is the whole
// predicate: deployment flag, Bearer-only, STORED admin_ro scope, live admin), assert the
// served CONTENT, the audit records (one per newly authorized foreign read, none otherwise,
// none carrying payload text or a secret), and that the write routes on the same paths stay
// refused for the same token.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix. No t.Parallel: the audit
// capture swaps the package-level adminRunDebugLogger.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

const (
	adrdFollowUp = "adrd-follow-up-body-text"
	adrdFetchURL = "https://adrd-secret.example/page"
)

// adrdAudit captures the admin run debug audit records of one test.
type adrdAudit struct{ buf *bytes.Buffer }

func adrdCaptureAudit(t *testing.T) *adrdAudit {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := adminRunDebugLogger
	adminRunDebugLogger = func() *slog.Logger { return slog.New(slog.NewJSONHandler(buf, nil)) }
	t.Cleanup(func() { adminRunDebugLogger = prev })
	return &adrdAudit{buf: buf}
}

// records returns the parsed audit records written so far and clears the buffer.
func (a *adrdAudit) take(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(a.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("audit line is not JSON: %q: %v", line, err)
		}
		if rec["msg"] == adminRunDebugMessage {
			out = append(out, rec)
		}
	}
	a.buf.Reset()
	return out
}

type adrdFixture struct {
	h      *Handler
	router http.Handler
	pool   *pgxpool.Pool

	admin, owner, other, demoted uuid.UUID
	run, hold, capture           uuid.UUID
	run2, hold2                  uuid.UUID
	chat, judged, rec, fetch     uuid.UUID

	adminRO, adminUzc, adminJWT string
	adminROID                   uuid.UUID
	ownerUzc, otherUzc          string
	otherRO, demotedRO          string // a uza_ token of a non-admin / of a since-demoted user
	revoked, expired            string
	storedUser                  string // uza_-shaped secret whose stored scope is 'user'
}

// adrdNewFixture seeds an owner with two runs holding custody (one with an available
// capture), a follow-up, a fetch, a chat run and a judged run, plus every credential class
// the matrix needs. The handler starts with the deployment flag ON.
func adrdNewFixture(t *testing.T) *adrdFixture {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	h.cfg.AdminRunDebug = true
	f := &adrdFixture{h: h, router: router, pool: pool}
	f.admin = cliSeedUser(t, pool, true)
	f.owner = cliSeedUser(t, pool, false)
	f.other = cliSeedUser(t, pool, false)
	f.demoted = cliSeedUser(t, pool, true)

	f.judged = cliSeedJudgedRun(t, pool, f.owner) // creates the owner's one forge connection (project 1)
	var conn uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT id FROM forge_connections WHERE user_id = $1`, f.owner).Scan(&conn); err != nil {
		t.Fatalf("read connection: %v", err)
	}
	worker := custodySeedWorker(t, pool, f.owner)
	f.run, f.hold = custodySeedRunHold(t, pool, f.owner, conn, worker, 11, "failed", 1)
	f.run2, f.hold2 = custodySeedRunHold(t, pool, f.owner, conn, worker, 12, "failed", 1)
	f.capture = custodySeedCapture(t, pool, f.owner, f.run, worker, f.hold, "adrd-key", "available", 1)
	cliMustExec(t, pool, `INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'follow_up', $2)`, f.run, adrdFollowUp)
	f.fetch = uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO run_fetches (id, run_id, reservation_id, url, verdict, http_status, bytes, started_at, finished_at)
		 VALUES ($1, $2, $3, $4, 'allowed', 200, 10, now(), now())`, f.fetch, f.run, uuid.New(), adrdFetchURL)
	f.chat = uuid.New()
	cliMustExec(t, pool, `INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status) VALUES ($1, $2, 'chat', 't', 'd', 'completed')`, f.chat, f.owner)
	cliMustExec(t, pool, `INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'follow_up', $2)`, f.chat, adrdFollowUp)
	if err := pool.QueryRow(context.Background(),
		`SELECT rr.id FROM review_recommendations rr JOIN run_reviews r ON r.id = rr.review_id WHERE r.target_run_id = $1`, f.judged).Scan(&f.rec); err != nil {
		t.Fatalf("read recommendation: %v", err)
	}

	f.adminRO, f.adminROID = cliInsertToken(t, pool, f.admin, clitoken.ScopeAdminRO, nil, false)
	f.adminUzc = cliMintToken(t, pool, f.admin, clitoken.ScopeUser)
	f.adminJWT = cliMintJWT(t, pool, f.admin)
	f.ownerUzc = cliMintToken(t, pool, f.owner, clitoken.ScopeUser)
	f.otherUzc = cliMintToken(t, pool, f.other, clitoken.ScopeUser)
	f.otherRO = cliMintToken(t, pool, f.other, clitoken.ScopeAdminRO)
	f.demotedRO = cliMintToken(t, pool, f.demoted, clitoken.ScopeAdminRO)
	cliMustExec(t, pool, `UPDATE users SET is_admin = false WHERE id = $1`, f.demoted)
	past := time.Now().Add(-time.Hour)
	f.revoked, _ = cliInsertToken(t, pool, f.admin, clitoken.ScopeAdminRO, nil, true)
	f.expired, _ = cliInsertToken(t, pool, f.admin, clitoken.ScopeAdminRO, &past, false)
	secret, hash, prefix, err := clitoken.Generate(clitoken.ScopeAdminRO)
	if err != nil {
		t.Fatalf("clitoken.Generate: %v", err)
	}
	cliMustExec(t, pool,
		`INSERT INTO cli_tokens (user_id, name, token_hash, token_prefix, scope, expires_at, revoked)
		 VALUES ($1, 'test', $2, $3, 'user', NULL, false)`, f.admin, hash, prefix)
	f.storedUser = secret
	return f
}

type adrdRoute struct {
	name  string
	path  func(f *adrdFixture) string
	check func(t *testing.T, f *adrdFixture, body string)
	// audit expectations of the newly authorized foreign read.
	auditRoute string
	auditKeys  func(f *adrdFixture) map[string]string
	// ownerPath is the owner's own read of the same data (no audit).
	ownerPath func(f *adrdFixture) string
}

func adrdRoutes() []adrdRoute {
	runPath := func(suffix string) func(f *adrdFixture) string {
		return func(f *adrdFixture) string { return "/api/runs/" + f.run.String() + suffix }
	}
	runKeys := func(f *adrdFixture) map[string]string {
		return map[string]string{"run_id": f.run.String(), "owner_id": f.owner.String()}
	}
	return []adrdRoute{
		{
			name: "inputs", path: runPath("/inputs"), ownerPath: runPath("/inputs"),
			check: func(t *testing.T, f *adrdFixture, body string) {
				t.Helper()
				if !strings.Contains(body, adrdFollowUp) {
					t.Errorf("inputs body lacks the owner's follow-up: %s", body)
				}
			},
			auditRoute: "runs.inputs", auditKeys: runKeys,
		},
		{
			name: "archives", path: runPath("/archives"), ownerPath: runPath("/archives"),
			check: func(t *testing.T, f *adrdFixture, body string) {
				t.Helper()
				if !strings.Contains(body, f.capture.String()) {
					t.Errorf("archives body lacks the owner's capture %s: %s", f.capture, body)
				}
			},
			auditRoute: "runs.archives", auditKeys: runKeys,
		},
		{
			name: "fetches", path: runPath("/fetches"), ownerPath: runPath("/fetches"),
			check: func(t *testing.T, f *adrdFixture, body string) {
				t.Helper()
				if !strings.Contains(body, f.fetch.String()) || !strings.Contains(body, adrdFetchURL) {
					t.Errorf("fetches body lacks the owner's fetch row: %s", body)
				}
			},
			auditRoute: "runs.fetches", auditKeys: runKeys,
		},
		{
			name:      "holds-by-user",
			path:      func(f *adrdFixture) string { return "/api/recovery/holds?user=" + f.owner.String() },
			ownerPath: func(f *adrdFixture) string { return "/api/recovery/holds?user=" + f.owner.String() },
			check: func(t *testing.T, f *adrdFixture, body string) {
				t.Helper()
				dto := adrdDecodeHolds(t, body)
				if dto.OwnerID != f.owner.String() {
					t.Errorf("owner_id = %q, want %s", dto.OwnerID, f.owner)
				}
				ids := adrdHoldIDs(dto)
				if len(ids) != 2 || !ids[f.hold.String()] || !ids[f.hold2.String()] {
					t.Errorf("holds = %v, want both of the owner's holds %s %s", ids, f.hold, f.hold2)
				}
			},
			auditRoute: "recovery.holds",
			auditKeys:  func(f *adrdFixture) map[string]string { return map[string]string{"target_owner_id": f.owner.String()} },
		},
		{
			name:      "holds-by-run",
			path:      func(f *adrdFixture) string { return "/api/recovery/holds?run=" + f.run.String() },
			ownerPath: func(f *adrdFixture) string { return "/api/recovery/holds?run=" + f.run.String() },
			check: func(t *testing.T, f *adrdFixture, body string) {
				t.Helper()
				dto := adrdDecodeHolds(t, body)
				if dto.OwnerID != f.owner.String() {
					t.Errorf("owner_id = %q, want %s", dto.OwnerID, f.owner)
				}
				ids := adrdHoldIDs(dto)
				if len(ids) != 1 || !ids[f.hold.String()] {
					t.Errorf("holds = %v, want only the run's hold %s", ids, f.hold)
				}
				if dto.Aggregate.OpenHolds != 2 {
					t.Errorf("aggregate.open_holds = %d, want the owner-wide 2", dto.Aggregate.OpenHolds)
				}
			},
			auditRoute: "recovery.holds", auditKeys: runKeys,
		},
	}
}

func adrdDecodeHolds(t *testing.T, body string) apitypes.RecoveryCustodyHoldsDTO {
	t.Helper()
	var dto apitypes.RecoveryCustodyHoldsDTO
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		t.Fatalf("decode holds: %v (body %s)", err, body)
	}
	return dto
}

func adrdHoldIDs(dto apitypes.RecoveryCustodyHoldsDTO) map[string]bool {
	ids := map[string]bool{}
	for _, h := range dto.Holds {
		ids[h.ID] = true
	}
	return ids
}

// TestAdminRunDebugReadMatrixLiveDB: per read route, who is served and who is refused, and
// what each outcome audits.
func TestAdminRunDebugReadMatrixLiveDB(t *testing.T) {
	f := adrdNewFixture(t)
	audit := adrdCaptureAudit(t)

	for _, rt := range adrdRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			path := rt.path(f)
			audit.take(t)

			// Flag ON, live admin's admin_ro Bearer: served, audited exactly once.
			rec := bearerReq(f.router, http.MethodGet, path, f.adminRO)
			if rec.Code != http.StatusOK {
				t.Fatalf("admin_ro GET %s = %d, want 200\nbody: %s", path, rec.Code, rec.Body.String())
			}
			rt.check(t, f, rec.Body.String())
			recs := audit.take(t)
			if len(recs) != 1 {
				t.Fatalf("authorized foreign read wrote %d audit records, want 1: %v", len(recs), recs)
			}
			want := map[string]string{
				"actor_user_id": f.admin.String(), "token_id": f.adminROID.String(), "route": rt.auditRoute,
			}
			for k, v := range rt.auditKeys(f) {
				want[k] = v
			}
			for k, v := range want {
				if recs[0][k] != v {
					t.Errorf("audit %s = %v, want %s (record %v)", k, recs[0][k], v, recs[0])
				}
			}

			// The owner reading their own data: same content, zero audit.
			rec = bearerReq(f.router, http.MethodGet, rt.ownerPath(f), f.ownerUzc)
			if rec.Code != http.StatusOK {
				t.Fatalf("owner GET %s = %d, want 200\nbody: %s", path, rec.Code, rec.Body.String())
			}
			rt.check(t, f, rec.Body.String())
			if recs := audit.take(t); len(recs) != 0 {
				t.Errorf("owner's own read wrote %d audit records, want 0: %v", len(recs), recs)
			}

			// Flag ON, every other caller: the same 404 an unknown run gets.
			refused := map[string]*httptest.ResponseRecorder{
				"admin session cookie":           cookieReq(t, f.router, http.MethodGet, path, f.adminJWT, ""),
				"admin default uzc_":             bearerReq(f.router, http.MethodGet, path, f.adminUzc),
				"non-admin uzc_":                 bearerReq(f.router, http.MethodGet, path, f.otherUzc),
				"non-admin uza_":                 bearerReq(f.router, http.MethodGet, path, f.otherRO),
				"demoted admin's uza_":           bearerReq(f.router, http.MethodGet, path, f.demotedRO),
				"stored scope user, uza_ secret": bearerReq(f.router, http.MethodGet, path, f.storedUser),
			}
			for who, rec := range refused {
				if rec.Code != http.StatusNotFound {
					t.Errorf("%s GET %s = %d, want 404\nbody: %s", who, path, rec.Code, rec.Body.String())
				}
			}
			for who, token := range map[string]string{"revoked uza_": f.revoked, "expired uza_": f.expired, "no credential": ""} {
				if rec := bearerReq(f.router, http.MethodGet, path, token); rec.Code != http.StatusUnauthorized {
					t.Errorf("%s GET %s = %d, want 401\nbody: %s", who, path, rec.Code, rec.Body.String())
				}
			}
			if recs := audit.take(t); len(recs) != 0 {
				t.Errorf("refused requests wrote %d audit records, want 0: %v", len(recs), recs)
			}

			// Flag OFF: even the live admin's admin_ro is refused.
			f.h.cfg.AdminRunDebug = false
			rec = bearerReq(f.router, http.MethodGet, path, f.adminRO)
			f.h.cfg.AdminRunDebug = true
			if rec.Code != http.StatusNotFound {
				t.Errorf("flag off: admin_ro GET %s = %d, want 404\nbody: %s", path, rec.Code, rec.Body.String())
			}
			if recs := audit.take(t); len(recs) != 0 {
				t.Errorf("flag off wrote %d audit records, want 0: %v", len(recs), recs)
			}
		})
	}
}

// TestAdminRunDebugInputsChatRunRefusedLiveDB: a foreign chat run's queue is not part of the
// debug surface, even for a fully authorized admin_ro token.
func TestAdminRunDebugInputsChatRunRefusedLiveDB(t *testing.T) {
	f := adrdNewFixture(t)
	audit := adrdCaptureAudit(t)
	path := "/api/runs/" + f.chat.String() + "/inputs"
	if rec := bearerReq(f.router, http.MethodGet, path, f.adminRO); rec.Code != http.StatusNotFound {
		t.Errorf("admin_ro GET foreign chat inputs = %d, want 404\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := bearerReq(f.router, http.MethodGet, path, f.ownerUzc); rec.Code != http.StatusOK {
		t.Errorf("owner GET own chat inputs = %d, want 200 (the 404 above must be the chat refusal)", rec.Code)
	}
	if recs := audit.take(t); len(recs) != 0 {
		t.Errorf("chat refusal wrote %d audit records, want 0: %v", len(recs), recs)
	}
}

// TestAdminRunDebugUnknownRunLiveDB: an unknown run id is the same 404 on every read, with
// or without the predicate, and writes no audit.
func TestAdminRunDebugUnknownRunLiveDB(t *testing.T) {
	f := adrdNewFixture(t)
	audit := adrdCaptureAudit(t)
	unknown := uuid.New().String()
	for _, p := range []string{
		"/api/runs/" + unknown + "/inputs", "/api/runs/" + unknown + "/archives", "/api/runs/" + unknown + "/fetches",
		"/api/recovery/holds?run=" + unknown, "/api/recovery/holds?user=" + unknown,
	} {
		for who, token := range map[string]string{"admin_ro": f.adminRO, "admin uzc_": f.adminUzc} {
			if rec := bearerReq(f.router, http.MethodGet, p, token); rec.Code != http.StatusNotFound {
				t.Errorf("%s GET %s = %d, want 404\nbody: %s", who, p, rec.Code, rec.Body.String())
			}
		}
	}
	if recs := audit.take(t); len(recs) != 0 {
		t.Errorf("unknown ids wrote %d audit records, want 0: %v", len(recs), recs)
	}
}

// TestAdminRunDebugAuditHasNoPayloadLiveDB: the audit output names ids and the route, never
// follow-up text, a fetch URL or a token secret.
func TestAdminRunDebugAuditHasNoPayloadLiveDB(t *testing.T) {
	f := adrdNewFixture(t)
	audit := adrdCaptureAudit(t)
	for _, rt := range adrdRoutes() {
		if rec := bearerReq(f.router, http.MethodGet, rt.path(f), f.adminRO); rec.Code != http.StatusOK {
			t.Fatalf("admin_ro GET %s = %d, want 200", rt.path(f), rec.Code)
		}
	}
	out := audit.buf.String()
	if strings.Count(out, adminRunDebugMessage) != len(adrdRoutes()) {
		t.Fatalf("audit output has %d records, want %d:\n%s", strings.Count(out, adminRunDebugMessage), len(adrdRoutes()), out)
	}
	for _, forbidden := range []string{adrdFollowUp, adrdFetchURL, f.adminRO, f.adminUzc, "adrd-key"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("audit output contains %q:\n%s", forbidden, out)
		}
	}
}

// adrdSnapshot reads every row the write attempts below could change.
func adrdSnapshot(t *testing.T, f *adrdFixture) string {
	t.Helper()
	runs := []uuid.UUID{f.run, f.run2, f.judged, f.chat}
	var snap string
	err := f.pool.QueryRow(context.Background(), `SELECT concat_ws('|',
	  (SELECT count(*) FROM run_user_inputs WHERE run_id = ANY($1)),
	  (SELECT string_agg(id::text || status, ',' ORDER BY id) FROM runs WHERE id = ANY($1)),
	  (SELECT string_agg(id::text || state, ',' ORDER BY id) FROM recovery_custody_holds WHERE run_id = ANY($1)),
	  (SELECT string_agg(id::text || state, ',' ORDER BY id) FROM recovery_captures WHERE run_id = ANY($1)),
	  (SELECT count(*) FROM recommendation_dispositions d JOIN run_reviews r ON r.id = d.review_id WHERE r.target_run_id = ANY($1)),
	  (SELECT count(*) FROM run_user_inputs))`, runs).Scan(&snap)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return snap
}

// TestAdminRunDebugWritesStayRefusedLiveDB: the debug predicate never reaches a write. With
// the flag ON, an admin_ro Bearer on another user's run gets exactly what any non-owner gets,
// is never a success, changes nothing and audits nothing.
func TestAdminRunDebugWritesStayRefusedLiveDB(t *testing.T) {
	f := adrdNewFixture(t)
	audit := adrdCaptureAudit(t)
	run, judged, rec := f.run.String(), f.judged.String(), f.rec.String()
	type write struct{ name, method, path, body string }
	writes := []write{
		{"follow-up input", http.MethodPost, "/api/runs/" + run + "/inputs", `{"kind":"follow_up","body":"adrd-write"}`},
		{"cancel input", http.MethodPost, "/api/runs/" + run + "/inputs", `{"kind":"cancel"}`},
		{"set disposition", http.MethodPut, "/api/runs/" + judged + "/review/recommendations/" + rec + "/disposition", `{"status":"done"}`},
		{"delete disposition", http.MethodDelete, "/api/runs/" + judged + "/review/recommendations/" + rec + "/disposition", ""},
		{"discard archive", http.MethodDelete, "/api/runs/" + run + "/archives/" + f.capture.String(), ""},
		{"download archive", http.MethodGet, "/api/runs/" + run + "/archives/" + f.capture.String() + "/download", ""},
		{"discard hold", http.MethodDelete, "/api/runs/" + run + "/recovery-holds/" + f.hold.String() + "?confirm=discard", ""},
		{"expire held publications", http.MethodPost, "/api/runs/" + run + "/held-publications/expire", ""},
		{"rework", http.MethodPost, "/api/runs/" + run + "/rework", `{}`},
	}
	before := adrdSnapshot(t, f)
	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			ref := bearerReqBody(f.router, w.method, w.path, f.otherUzc, w.body)
			got := bearerReqBody(f.router, w.method, w.path, f.adminRO, w.body)
			if got.Code < 400 || got.Code >= 500 {
				t.Errorf("admin_ro %s %s = %d, want a 4xx refusal\nbody: %s", w.method, w.path, got.Code, got.Body.String())
			}
			if got.Code != ref.Code {
				t.Errorf("admin_ro %s %s = %d, a plain non-owner gets %d; the debug token must get no extra reach", w.method, w.path, got.Code, ref.Code)
			}
			if after := adrdSnapshot(t, f); after != before {
				t.Errorf("%s changed the database:\nbefore %s\nafter  %s", w.name, before, after)
			}
		})
	}
	if recs := audit.take(t); len(recs) != 0 {
		t.Errorf("refused writes wrote %d audit records, want 0: %v", len(recs), recs)
	}
}
