package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestResumeRunNowRecordsReserveOverrideLiveDB pins the owner's manual resume of a weekly-reserve
// pause (PRD #3522 M4a, D9): it is a RECORDED override until the next observed 7-day reset. The
// exemption is anchored on the owner's current reading of the credential the run spent, capped at
// seven days, falls back to the cap with no reading, and is written ONLY for a weekly_reserve hold;
// an ordinary owner pause resumes with no exemption. Skipped unless UZI_TEST_DATABASE_URL is set.
func TestResumeRunNowRecordsReserveOverrideLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store integration runner for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	owner := store.User{ID: uuid.New()}
	connID, repoID, tok := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash, weekly_reserve_pct) VALUES ($1, $2, 'x', 50)`, owner.ID, fmt.Sprintf("reserve-resume-%s@e2e", owner.ID))
	// A defer, not t.Cleanup: it must run BEFORE the deferred pool.Close above. The owner carries a
	// threshold, so its queued runs would otherwise leak into every later package's marker pass.
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, owner.ID) }()
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner.ID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	      VALUES ($1, $2, 'anthropic_token', 'tok', true, $3, 'master')`, tok, owner.ID, []byte{0x2})

	h := &Handler{q: store.New(pool), cfg: config.Config{UsagePollInterval: 5 * time.Minute, CodexUsagePollInterval: 5 * time.Minute}}
	var iid int64 = 9000
	newPaused := func(hold string) uuid.UUID {
		iid++
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, started_at, status_since, hold_reason, anthropic_secret_id, first_started_at)
		      VALUES ($1,$2,$3,$4,'t','d','paused','issue', now() - interval '3 hours', now() - interval '5 minutes', NULLIF($5, ''), $6, now() - interval '3 hours')`,
			id, owner.ID, repoID, iid, hold, tok)
		return id
	}
	reading := func(pct int, resets time.Time) {
		exec(`INSERT INTO anthropic_rate_limits (user_secret_id, user_id, five_hour_pct, five_hour_resets_at, seven_day_pct, seven_day_resets_at, source, synced_at)
		      VALUES ($1, $2, 10, now() + interval '1 hour', $3, $4, 'usage_endpoint', now())
		      ON CONFLICT (user_secret_id) DO UPDATE SET seven_day_pct = EXCLUDED.seven_day_pct,
		          seven_day_resets_at = EXCLUDED.seven_day_resets_at, synced_at = EXCLUDED.synced_at`, tok, owner.ID, pct, resets)
	}
	resume := func(id uuid.UUID) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/runs/"+id.String()+"/resume-now", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", id.String())
		req = req.WithContext(context.WithValue(mw.ContextWithUser(req.Context(), owner), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.ResumeRunNow(rec, req)
		return rec
	}
	type exemption struct {
		status                 string
		until, granted, anchor *time.Time
		credential             *uuid.UUID
		prior                  *int16
		hold                   *string
		auditBody              string
		auditRows              int
	}
	read := func(id uuid.UUID) exemption {
		var e exemption
		if err := pool.QueryRow(ctx, `SELECT status, reserve_exempt_until, reserve_exempt_granted_at, reserve_exempt_window_reset,
		                                     reserve_exempt_credential_id, hold_reason, reserve_exempt_prior_pct FROM runs WHERE id = $1`, id).
			Scan(&e.status, &e.until, &e.granted, &e.anchor, &e.credential, &e.hold, &e.prior); err != nil {
			t.Fatalf("read run: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(body), '') FROM run_user_inputs WHERE run_id = $1 AND kind = 'resume'`, id).
			Scan(&e.auditRows, &e.auditBody); err != nil {
			t.Fatalf("read audit: %v", err)
		}
		return e
	}

	t.Run("anchored on the owner's current weekly reset", func(t *testing.T) {
		resets := time.Now().Add(30 * time.Hour).UTC().Truncate(time.Second)
		reading(80, resets)
		id := newPaused("weekly_reserve")
		if rec := resume(id); rec.Code != http.StatusOK {
			t.Fatalf("resume = %d: %s", rec.Code, rec.Body.String())
		}
		e := read(id)
		if e.status != "queued" || e.hold != nil {
			t.Fatalf("resumed run = %s hold %v, want queued with the hold cleared", e.status, e.hold)
		}
		if e.until == nil || !e.until.Equal(resets) || e.anchor == nil || !e.anchor.Equal(resets) || e.granted == nil || e.credential == nil || *e.credential != tok {
			t.Fatalf("exemption = until %v anchor %v granted %v credential %v, want anchored on %v for %s", e.until, e.anchor, e.granted, e.credential, resets, tok)
		}
		if e.prior == nil || *e.prior != 80 {
			t.Fatalf("exemption prior = %v, want the 80%% the grant read (the reset detector's baseline)", e.prior)
		}
		if e.auditRows != 1 || e.auditBody == "" {
			t.Fatalf("resume audit = %d rows, body %q, want one row recording the override", e.auditRows, e.auditBody)
		}
	})

	t.Run("no reading: no anchor and the seven-day cap", func(t *testing.T) {
		exec(`DELETE FROM anthropic_rate_limits WHERE user_secret_id = $1`, tok)
		id := newPaused("weekly_reserve")
		if rec := resume(id); rec.Code != http.StatusOK {
			t.Fatalf("resume = %d: %s", rec.Code, rec.Body.String())
		}
		e := read(id)
		if e.anchor != nil || e.prior != nil || e.until == nil || e.granted == nil {
			t.Fatalf("exemption = until %v anchor %v granted %v, want a capped, anchor-less exemption", e.until, e.anchor, e.granted)
		}
		if d := e.until.Sub(*e.granted); d < 7*24*time.Hour-time.Minute || d > 7*24*time.Hour+time.Minute {
			t.Fatalf("exemption lasts %v, want seven days", d)
		}
	})

	t.Run("an ordinary owner pause resumes with no exemption", func(t *testing.T) {
		id := newPaused("")
		if rec := resume(id); rec.Code != http.StatusOK {
			t.Fatalf("resume = %d: %s", rec.Code, rec.Body.String())
		}
		e := read(id)
		if e.status != "queued" || e.until != nil || e.granted != nil || e.credential != nil || e.anchor != nil {
			t.Fatalf("owner pause resume wrote an exemption: %+v", e)
		}
		if e.auditRows != 1 || e.auditBody != "" {
			t.Fatalf("resume audit = %d rows, body %q, want one empty row", e.auditRows, e.auditBody)
		}
	})

	t.Run("a reading far beyond a week is capped", func(t *testing.T) {
		reading(80, time.Now().Add(20*24*time.Hour))
		id := newPaused("weekly_reserve")
		if rec := resume(id); rec.Code != http.StatusOK {
			t.Fatalf("resume = %d: %s", rec.Code, rec.Body.String())
		}
		e := read(id)
		if d := e.until.Sub(*e.granted); d > 7*24*time.Hour+time.Minute {
			t.Fatalf("exemption lasts %v, want at most seven days", d)
		}
	})
}
