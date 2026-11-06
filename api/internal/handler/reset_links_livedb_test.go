package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/oidc"
	"github.com/vtmocanu/uzi/api/internal/resettoken"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Admin-issued account reset links (PRD #3989), driven through the real h.Routes()
// router against a live database: the properties are transactional (lock order,
// single winner, supersession serialized with demotion) and a fake store cannot
// exhibit them. Skipped unless UZI_TEST_DATABASE_URL is set; ./e2e/run-store-it.sh
// sweeps this package for the LiveDB suffix.

const (
	resetOldPassword = "old-password-123456"
	resetNewPassword = "new-password-123456"
)

type resetFixture struct {
	h        *Handler
	router   http.Handler
	pool     *pgxpool.Pool
	admin    uuid.UUID
	adminJWT string
}

func newResetFixture(t *testing.T) *resetFixture {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	h.cfg.PasswordLoginEnabled = true
	admin := cliSeedUser(t, pool, true)
	return &resetFixture{h: h, router: router, pool: pool, admin: admin, adminJWT: cliMintJWT(t, pool, admin)}
}

// target seeds an active non-admin user with a real password hash.
func (f *resetFixture) target(t *testing.T) uuid.UUID {
	t.Helper()
	id := cliSeedUser(t, f.pool, false)
	setVaultFixturePassword(t, f.pool, id, resetOldPassword)
	return id
}

func (f *resetFixture) email(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var email string
	if err := f.pool.QueryRow(context.Background(), `SELECT email FROM users WHERE id=$1`, id).Scan(&email); err != nil {
		t.Fatal(err)
	}
	return email
}

func (f *resetFixture) issueAs(t *testing.T, jwt string, target uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	return cookieReq(t, f.router, http.MethodPost, "/api/admin/users/"+target.String()+"/reset-link", jwt, "")
}

// mustIssue issues a link as the fixture admin and returns the plaintext token.
func (f *resetFixture) mustIssue(t *testing.T, target uuid.UUID) string {
	t.Helper()
	return f.mustIssueAs(t, f.adminJWT, target)
}

func (f *resetFixture) mustIssueAs(t *testing.T, jwt string, target uuid.UUID) string {
	t.Helper()
	rec := f.issueAs(t, jwt, target)
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue = %d, want 201\nbody: %s", rec.Code, rec.Body.String())
	}
	var dto apitypes.AccountResetLinkDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	return dto.Token
}

func (f *resetFixture) preview(token string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(apitypes.ResetLinkPreviewRequest{Token: token})
	return cliPostJSON(f.router, "/api/auth/reset/preview", string(body))
}

func (f *resetFixture) redeem(token, password string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(apitypes.ResetLinkRedeemRequest{Token: token, Password: password}) //nolint:gosec // G117: test marshals the redeem request DTO with a fixture password
	return cliPostJSON(f.router, "/api/auth/reset/redeem", string(body))
}

type resetSnapshot struct {
	hash    *string
	version int32
}

func (f *resetFixture) snapshot(t *testing.T, id uuid.UUID) resetSnapshot {
	t.Helper()
	var s resetSnapshot
	if err := f.pool.QueryRow(context.Background(), `SELECT password_hash, token_version FROM users WHERE id=$1`, id).Scan(&s.hash, &s.version); err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *resetFixture) liveLinks(t *testing.T, target uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM account_reset_links WHERE user_id=$1 AND redeemed_at IS NULL AND superseded_at IS NULL`, target).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertDead checks that preview and redeem both answer the generic 404 and that
// redeem changed nothing about the target.
func (f *resetFixture) assertDead(t *testing.T, token string, target uuid.UUID) {
	t.Helper()
	before := f.snapshot(t, target)
	if rec := f.preview(token); rec.Code != http.StatusNotFound {
		t.Errorf("preview = %d, want 404\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := f.redeem(token, resetNewPassword); rec.Code != http.StatusNotFound {
		t.Errorf("redeem = %d, want 404\nbody: %s", rec.Code, rec.Body.String())
	}
	after := f.snapshot(t, target)
	if (before.hash == nil) != (after.hash == nil) || (before.hash != nil && *before.hash != *after.hash) || before.version != after.version {
		t.Errorf("a dead link changed the target: before %+v after %+v", before, after)
	}
	var redeemed int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM account_reset_links WHERE user_id=$1 AND redeemed_at IS NOT NULL`, target).Scan(&redeemed); err != nil {
		t.Fatal(err)
	}
	if redeemed != 0 {
		t.Errorf("a dead link was marked redeemed")
	}
}

func (f *resetFixture) assertLive(t *testing.T, token string) {
	t.Helper()
	rec := f.preview(token)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	var dto apitypes.ResetLinkPreviewDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	if dto.Kind != "password" || dto.ExpiresAt.IsZero() {
		t.Fatalf("preview = %+v", dto)
	}
}

func (f *resetFixture) setActive(t *testing.T, id uuid.UUID, active bool) {
	t.Helper()
	rec := cookieReq(t, f.router, http.MethodPatch, "/api/admin/users/"+id.String(), f.adminJWT, fmt.Sprintf(`{"is_active":%v}`, active))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch active=%v = %d\nbody: %s", active, rec.Code, rec.Body.String())
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureSlog routes the default slog logger into a buffer for the test.
func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func TestResetLinkIssueGuardsLiveDB(t *testing.T) {
	f := newResetFixture(t)
	ctx := context.Background()

	t.Run("uza bearer is 401", func(t *testing.T) {
		target := f.target(t)
		uza := cliMintToken(t, f.pool, f.admin, clitoken.ScopeAdminRO)
		rec := bearerReq(f.router, http.MethodPost, "/api/admin/users/"+target.String()+"/reset-link", uza)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("uza_ bearer = %d, want 401", rec.Code)
		}
		if f.liveLinks(t, target) != 0 {
			t.Fatal("a bearer request created a link")
		}
	})
	t.Run("missing CSRF is 403", func(t *testing.T) {
		target := f.target(t)
		req := httptest.NewRequest(http.MethodPost, "/api/admin/users/"+target.String()+"/reset-link", nil)
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: f.adminJWT}) //nolint:gosec // G124: test-only client cookie
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("no CSRF = %d, want 403", rec.Code)
		}
		if f.liveLinks(t, target) != 0 {
			t.Fatal("a CSRF-less request created a link")
		}
	})
	t.Run("non-admin session is 403", func(t *testing.T) {
		target := f.target(t)
		user := cliSeedUser(t, f.pool, false)
		if rec := f.issueAs(t, cliMintJWT(t, f.pool, user), target); rec.Code != http.StatusForbidden {
			t.Fatalf("non-admin = %d, want 403", rec.Code)
		}
	})
	t.Run("self is 400", func(t *testing.T) {
		if rec := f.issueAs(t, f.adminJWT, f.admin); rec.Code != http.StatusBadRequest {
			t.Fatalf("self = %d, want 400", rec.Code)
		}
		if f.liveLinks(t, f.admin) != 0 {
			t.Fatal("self link created")
		}
	})
	t.Run("seed admin is 400", func(t *testing.T) {
		seed := f.target(t)
		f.h.cfg.SeedEmail = f.email(t, seed)
		t.Cleanup(func() { f.h.cfg.SeedEmail = "" })
		if rec := f.issueAs(t, f.adminJWT, seed); rec.Code != http.StatusBadRequest {
			t.Fatalf("seed admin = %d, want 400", rec.Code)
		}
		if f.liveLinks(t, seed) != 0 {
			t.Fatal("seed link created")
		}
	})
	t.Run("inactive target is 400", func(t *testing.T) {
		target := f.target(t)
		cliMustExec(t, f.pool, `UPDATE users SET is_active=false WHERE id=$1`, target)
		if rec := f.issueAs(t, f.adminJWT, target); rec.Code != http.StatusBadRequest {
			t.Fatalf("inactive = %d, want 400", rec.Code)
		}
	})
	t.Run("passwordless target is 400 in M1", func(t *testing.T) {
		target := f.target(t)
		cliMustExec(t, f.pool, `UPDATE users SET password_hash=NULL WHERE id=$1`, target)
		if rec := f.issueAs(t, f.adminJWT, target); rec.Code != http.StatusBadRequest {
			t.Fatalf("passwordless = %d, want 400", rec.Code)
		}
		if f.liveLinks(t, target) != 0 {
			t.Fatal("passwordless link created")
		}
	})
	t.Run("unknown target is 404", func(t *testing.T) {
		if rec := f.issueAs(t, f.adminJWT, uuid.New()); rec.Code != http.StatusNotFound {
			t.Fatalf("unknown = %d, want 404", rec.Code)
		}
	})
	t.Run("issue stores only the hash and returns the documented body", func(t *testing.T) {
		target := f.target(t)
		cliMustExec(t, f.pool, `UPDATE users SET is_admin=true WHERE id=$1`, target)
		rec := f.issueAs(t, f.adminJWT, target)
		if rec.Code != http.StatusCreated {
			t.Fatalf("issue = %d\n%s", rec.Code, rec.Body.String())
		}
		var raw map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if len(raw) != 4 || raw["kind"] != "password" || raw["target_is_admin"] != true {
			t.Fatalf("body = %v", raw)
		}
		token, _ := raw["token"].(string)
		hash, ok := resettoken.Hash(token)
		if !ok {
			t.Fatalf("token %q is not a well-formed reset token", token)
		}
		var stored []byte
		var issuedBy uuid.UUID
		var expires time.Time
		if err := f.pool.QueryRow(ctx, `SELECT token_hash, issued_by, expires_at FROM account_reset_links WHERE user_id=$1`, target).Scan(&stored, &issuedBy, &expires); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(stored, hash) || issuedBy != f.admin {
			t.Fatal("stored hash or issuer wrong")
		}
		if d := time.Until(expires); d < 55*time.Minute || d > 61*time.Minute {
			t.Fatalf("expires_at in %v, want about 1h", d)
		}
	})
}

func TestResetLinkMutualCrossIssueDoesNotDeadlockLiveDB(t *testing.T) {
	f := newResetFixture(t)
	other := cliSeedUser(t, f.pool, true)
	otherJWT := cliMintJWT(t, f.pool, other)
	done := make(chan string, 1)
	go func() {
		for round := 0; round < 15; round++ {
			var wg sync.WaitGroup
			codes := make([]int, 2)
			wg.Add(2)
			go func() { defer wg.Done(); codes[0] = f.issueAs(t, f.adminJWT, other).Code }()
			go func() { defer wg.Done(); codes[1] = f.issueAs(t, otherJWT, f.admin).Code }()
			wg.Wait()
			if codes[0] != http.StatusCreated || codes[1] != http.StatusCreated {
				done <- fmt.Sprintf("round %d: codes %v", round, codes)
				return
			}
		}
		done <- ""
	}()
	select {
	case msg := <-done:
		if msg != "" {
			t.Fatal(msg)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("mutual cross-issue did not finish: deadlock")
	}
	if f.liveLinks(t, other) != 1 || f.liveLinks(t, f.admin) != 1 {
		t.Fatal("expected exactly one live link per target")
	}
}

func TestResetLinkConcurrentIssueLeavesOneLiveLinkLiveDB(t *testing.T) {
	f := newResetFixture(t)
	target := f.target(t)
	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes[i] = f.issueAs(t, f.adminJWT, target).Code }()
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusCreated {
			t.Fatalf("issue %d = %d, want 201", i, c)
		}
	}
	if got := f.liveLinks(t, target); got != 1 {
		t.Fatalf("live links = %d, want exactly 1", got)
	}
}

func TestResetLinkConcurrentRedeemSingleWinnerLiveDB(t *testing.T) {
	f := newResetFixture(t)
	target := f.target(t)
	token := f.mustIssue(t, target)
	const n = 5
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes[i] = f.redeem(token, resetNewPassword).Code }()
	}
	wg.Wait()
	wins := 0
	for i, c := range codes {
		switch c {
		case http.StatusNoContent:
			wins++
		case http.StatusNotFound:
		default:
			t.Errorf("redeem %d = %d, want 204 or 404", i, c)
		}
	}
	if wins != 1 {
		t.Fatalf("%d redemptions succeeded (codes %v), want exactly 1", wins, codes)
	}
}

func TestResetLinkValidityOnPreviewAndRedeemLiveDB(t *testing.T) {
	ctx := context.Background()
	type env struct {
		f      *resetFixture
		issuer uuid.UUID
		jwt    string
		target uuid.UUID
	}
	cases := []struct {
		name   string
		mutate func(t *testing.T, e env, token string)
	}{
		{"expired", func(t *testing.T, e env, _ string) {
			cliMustExec(t, e.f.pool, `UPDATE account_reset_links SET expires_at = now() - interval '1 minute' WHERE user_id=$1`, e.target)
		}},
		{"superseded by a newer link", func(t *testing.T, e env, _ string) {
			e.f.mustIssueAs(t, e.jwt, e.target)
		}},
		{"target deactivated then reactivated", func(t *testing.T, e env, _ string) {
			e.f.setActive(t, e.target, false)
			e.f.setActive(t, e.target, true)
		}},
		{"target deactivated then reactivated through the non-vault fallback", func(t *testing.T, e env, _ string) {
			if _, err := e.f.h.setUserActiveSuperseding(ctx, store.SetUserActiveParams{ID: e.target, IsActive: false}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.f.h.setUserActiveSuperseding(ctx, store.SetUserActiveParams{ID: e.target, IsActive: true}); err != nil {
				t.Fatal(err)
			}
		}},
		{"issuer demoted through the OIDC group sync, then re-promoted", func(t *testing.T, e env, _ string) {
			e.f.h.cfg.OIDCAdminGroups = []string{"uzi-admins"}
			t.Cleanup(func() { e.f.h.cfg.OIDCAdminGroups = nil })
			user, err := e.f.h.q.GetUserByID(ctx, e.issuer)
			if err != nil {
				t.Fatal(err)
			}
			synced, code := e.f.h.oidcSyncAdmin(ctx, user, oidc.Identity{GroupsClaimPresent: true, Groups: []string{}})
			if code != "" || synced.IsAdmin {
				t.Fatalf("oidcSyncAdmin demotion = %v %q", synced.IsAdmin, code)
			}
			cliMustExec(t, e.f.pool, `UPDATE users SET is_admin=true WHERE id=$1`, e.issuer)
		}},
		{"issuer deactivated then reactivated", func(t *testing.T, e env, _ string) {
			e.f.setActive(t, e.issuer, false)
			e.f.setActive(t, e.issuer, true)
		}},
		{"kind drift: password cleared after issue", func(t *testing.T, e env, _ string) {
			cliMustExec(t, e.f.pool, `UPDATE users SET password_hash=NULL WHERE id=$1`, e.target)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResetFixture(t)
			issuer := cliSeedUser(t, f.pool, true)
			e := env{f: f, issuer: issuer, jwt: cliMintJWT(t, f.pool, issuer), target: f.target(t)}
			token := f.mustIssueAs(t, e.jwt, e.target)
			f.assertLive(t, token)
			tc.mutate(t, e, token)
			f.assertDead(t, token, e.target)
		})
	}
}

func TestResetLinkIssuerFlippedWithoutSupersedeIsRejectedWhileFlippedLiveDB(t *testing.T) {
	f := newResetFixture(t)
	issuer := cliSeedUser(t, f.pool, true)
	target := f.target(t)
	token := f.mustIssueAs(t, cliMintJWT(t, f.pool, issuer), target)
	cliMustExec(t, f.pool, `UPDATE users SET is_admin=false WHERE id=$1`, issuer)
	f.assertDead(t, token, target)
	cliMustExec(t, f.pool, `UPDATE users SET is_admin=true WHERE id=$1`, issuer)
	f.assertLive(t, token)
}

// A demotion that waits on the issue transaction's users-row lock must supersede the
// link that transaction inserts: otherwise re-promoting the issuer revives it.
func TestResetLinkDemotionRacingIssueSupersedesLinkLiveDB(t *testing.T) {
	f := newResetFixture(t)
	ctx := context.Background()
	issuer := cliSeedUser(t, f.pool, true)
	target := f.target(t)

	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	q := f.h.q.WithTx(tx)
	if _, err := q.LockUsersForResetIssue(ctx, []uuid.UUID{target, issuer}); err != nil {
		t.Fatal(err)
	}
	demoted := make(chan error, 1)
	go func() {
		_, err := f.h.setUserAdminSuperseding(ctx, store.SetUserAdminParams{ID: issuer, IsAdmin: false})
		demoted <- err
	}()
	select {
	case err := <-demoted:
		t.Fatalf("demotion finished while the issue transaction held the issuer row: %v", err)
	case <-time.After(400 * time.Millisecond):
	}
	token, hash, err := resettoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.SupersedeLiveResetLinksForTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertResetLink(ctx, store.InsertResetLinkParams{
		UserID: target, TokenHash: hash, Kind: resetKindPassword, IssuedBy: issuer,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-demoted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("demotion never completed after the issue transaction committed")
	}
	if f.liveLinks(t, target) != 0 {
		t.Fatal("the demotion left the link it raced live")
	}
	cliMustExec(t, f.pool, `UPDATE users SET is_admin=true WHERE id=$1`, issuer)
	f.assertDead(t, token, target)
}

func TestResetLinkFailuresAreByteIdenticalLiveDB(t *testing.T) {
	f := newResetFixture(t)
	target := f.target(t)
	valid := f.mustIssue(t, target)
	expiredTarget := f.target(t)
	expired := f.mustIssue(t, expiredTarget)
	cliMustExec(t, f.pool, `UPDATE account_reset_links SET expires_at = now() - interval '1 minute' WHERE user_id=$1`, expiredTarget)
	unknown, _, err := resettoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	type probe struct {
		name string
		rec  *httptest.ResponseRecorder
	}
	probes := []probe{
		{"preview malformed token", f.preview("nope")},
		{"preview unknown token", f.preview(unknown)},
		{"preview expired", f.preview(expired)},
		{"redeem malformed token", f.redeem("nope", resetNewPassword)},
		{"redeem unknown token", f.redeem(unknown, resetNewPassword)},
		{"redeem expired", f.redeem(expired, resetNewPassword)},
		{"redeem password link without a password", f.redeem(valid, "")},
	}
	// Kind mismatch: a vault_only link row (as M2 will issue) redeemed with a password, and the
	// stored kind disagreeing with the users row.
	mismatchTarget := f.target(t)
	mismatch := f.mustIssue(t, mismatchTarget)
	cliMustExec(t, f.pool, `UPDATE account_reset_links SET kind='vault_only' WHERE user_id=$1`, mismatchTarget)
	probes = append(probes,
		probe{"preview kind mismatch", f.preview(mismatch)},
		probe{"redeem kind mismatch", f.redeem(mismatch, resetNewPassword)},
	)
	want := probes[0].rec.Body.String()
	for _, p := range probes {
		if p.rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", p.name, p.rec.Code)
		}
		if got := p.rec.Body.String(); got != want {
			t.Errorf("%s body %q differs from %q", p.name, got, want)
		}
		if ct := p.rec.Header().Get("Content-Type"); ct != probes[0].rec.Header().Get("Content-Type") {
			t.Errorf("%s content type %q differs", p.name, ct)
		}
	}
	// The valid link is untouched by the failed probes.
	f.assertLive(t, valid)
}

func TestResetLinkRedeemEndsSessionsAndCLIRequestsLiveDB(t *testing.T) {
	f := newResetFixture(t)
	ctx := context.Background()
	target := f.target(t)
	cliMustExec(t, f.pool, `INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES ($1, $2, 'anthropic_token', 'default', true, $3, 'master')`, uuid.New(), target, []byte("ct"))
	oldJWT := cliMintJWT(t, f.pool, target)

	// An approved, unconsumed CLI login request and a pending one the old session will try to approve.
	verifier1 := "verifier-" + uuid.NewString()
	reqApproved, code1 := cliStart(t, f.router, s256Challenge(verifier1), "approved before reset")
	if rec := cookieReq(t, f.router, http.MethodPost, "/api/auth/cli/approve", oldJWT,
		fmt.Sprintf(`{"request_id":%q,"user_code":%q}`, reqApproved, code1)); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d\n%s", rec.Code, rec.Body.String())
	}
	reqPending, code2 := cliStart(t, f.router, s256Challenge("verifier-"+uuid.NewString()), "pending during reset")

	token := f.mustIssue(t, target)
	rec := f.redeem(token, resetNewPassword)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("redeem = %d, want 204\n%s", rec.Code, rec.Body.String())
	}
	if len(rec.Result().Cookies()) != 0 || rec.Header().Get("Set-Cookie") != "" {
		t.Fatal("redeem set a cookie; redemption must not log in")
	}

	if rec := cookieReq(t, f.router, http.MethodGet, "/api/auth/me", oldJWT, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old session = %d, want 401", rec.Code)
	}
	// The previously approved request cannot mint.
	poll := cliPostJSON(f.router, "/api/auth/cli/poll", fmt.Sprintf(`{"request_id":%q,"verifier":%q}`, reqApproved, verifier1))
	if poll.Code == http.StatusOK {
		t.Fatalf("poll of a request approved before the reset minted a token: %s", poll.Body.String())
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM cli_auth_requests WHERE id=$1`, reqApproved).Scan(&status); err != nil || status != "denied" {
		t.Fatalf("approved request status = %q (%v), want denied", status, err)
	}
	if cliCountTokens(t, f.pool, target) != 0 {
		t.Fatal("a CLI token was minted for the reset user")
	}
	// A stale-session approve after the redemption is refused and mints nothing.
	if rec := cookieReq(t, f.router, http.MethodPost, "/api/auth/cli/approve", oldJWT,
		fmt.Sprintf(`{"request_id":%q,"user_code":%q}`, reqPending, code2)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("stale approve = %d, want 401", rec.Code)
	}
	if err := f.pool.QueryRow(ctx, `SELECT status FROM cli_auth_requests WHERE id=$1`, reqPending).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("stale-approved request status = %q (%v), want pending", status, err)
	}

	// Credentials are gone; the account is protected with no wrapper until the next login enrolls.
	var secrets int
	var mode string
	if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM user_secrets WHERE user_id=$1), vault_mode FROM users WHERE id=$1`, target).Scan(&secrets, &mode); err != nil {
		t.Fatal(err)
	}
	if secrets != 0 || mode != store.VaultModeProtected {
		t.Fatalf("after redeem secrets=%d mode=%q, want 0 and protected", secrets, mode)
	}
	var marker bool
	if err := f.pool.QueryRow(ctx, `SELECT credentials_reset_at IS NOT NULL FROM users WHERE id=$1`, target).Scan(&marker); err != nil || !marker {
		t.Fatalf("credentials_reset_at marker = %v (%v)", marker, err)
	}

	// The old password no longer works; the new one does and enrolls a fresh empty vault.
	email := f.email(t, target)
	login := func(pw string) *httptest.ResponseRecorder {
		return cliPostJSON(f.router, "/api/auth/login", fmt.Sprintf(`{"email":%q,"password":%q}`, email, pw))
	}
	if rec := login(resetOldPassword); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old password login = %d, want 401", rec.Code)
	}
	if rec := login(resetNewPassword); rec.Code != http.StatusOK {
		t.Fatalf("new password login = %d\n%s", rec.Code, rec.Body.String())
	}
	var wrapper bool
	if err := f.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_vaults WHERE user_id=$1)`, target).Scan(&wrapper); err != nil || !wrapper {
		t.Fatalf("login did not enroll a vault wrapper (%v, %v)", wrapper, err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM user_secrets WHERE user_id=$1`, target).Scan(&secrets); err != nil || secrets != 0 {
		t.Fatalf("secrets after login = %d (%v)", secrets, err)
	}
}

// With password login disabled (an SSO-only instance) redemption still works, and the
// reset user enrolls a new vault through POST /api/vault/enable from a session that was
// not minted by password login. The session here is a freshly minted cookie JWT, the
// same credential an SSO callback issues; the SSO callback itself is not driven.
func TestResetLinkRedeemOnSSOOnlyInstanceThenVaultEnableLiveDB(t *testing.T) {
	f := newResetFixture(t)
	f.h.cfg.PasswordLoginEnabled = false
	target := f.target(t)
	token := f.mustIssue(t, target)
	if rec := f.redeem(token, resetNewPassword); rec.Code != http.StatusNoContent {
		t.Fatalf("redeem with password login disabled = %d, want 204\n%s", rec.Code, rec.Body.String())
	}
	if rec := cliPostJSON(f.router, "/api/auth/login", fmt.Sprintf(`{"email":%q,"password":%q}`, f.email(t, target), resetNewPassword)); rec.Code != http.StatusForbidden {
		t.Fatalf("password login = %d, want 403 while disabled", rec.Code)
	}
	sso := cliMintJWT(t, f.pool, target)
	rec := cookieReq(t, f.router, http.MethodPost, "/api/vault/enable", sso, fmt.Sprintf(`{"password":%q}`, resetNewPassword))
	if rec.Code != http.StatusOK {
		t.Fatalf("vault enable after reset = %d\n%s", rec.Code, rec.Body.String())
	}
	var wrapper bool
	if err := f.pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM user_vaults WHERE user_id=$1)`, target).Scan(&wrapper); err != nil || !wrapper {
		t.Fatalf("vault enable created no wrapper (%v, %v)", wrapper, err)
	}
}

func TestResetLinkRedeemUncertainOutcomeIs503LiveDB(t *testing.T) {
	f := newResetFixture(t)
	logs := captureSlog(t)
	target := f.target(t)
	token := f.mustIssue(t, target)
	f.h.vault = vault.New(f.h.box, store.New(f.pool), func(context.Context, func(context.Context, vault.Store) error) error {
		return &vault.CommitError{Err: errors.New("commit acknowledgment lost")}
	})
	rec := f.redeem(token, resetNewPassword)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("redeem = %d, want 503\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "the reset could not be confirmed; try signing in with the new password, or ask for a new link") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	out := logs.String()
	if !strings.Contains(out, "reset link redeem outcome uncertain") {
		t.Fatalf("no uncertain-outcome warning in logs:\n%s", out)
	}
	if strings.Contains(out, "reset link redeemed") {
		t.Fatal("an uncertain outcome logged a successful redemption")
	}
}

func TestResetLinkRedeemWithoutVaultIs503LiveDB(t *testing.T) {
	f := newResetFixture(t)
	target := f.target(t)
	token := f.mustIssue(t, target)
	f.h.vault = nil
	if rec := f.redeem(token, resetNewPassword); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("redeem without a vault = %d, want 503", rec.Code)
	}
}

func TestResetLinkLogsNeverCarryTokenOrPasswordLiveDB(t *testing.T) {
	f := newResetFixture(t)
	logs := captureSlog(t)
	target := f.target(t)
	token := f.mustIssue(t, target)
	if !strings.Contains(logs.String(), "reset link issued") {
		t.Fatalf("no issue log:\n%s", logs.String())
	}
	// Failed redemptions do not log success.
	if rec := f.redeem(token, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("redeem without a password = %d", rec.Code)
	}
	if strings.Contains(logs.String(), "reset link redeemed") {
		t.Fatal("a failed redemption logged success")
	}
	if rec := f.redeem(token, resetNewPassword); rec.Code != http.StatusNoContent {
		t.Fatalf("redeem = %d", rec.Code)
	}
	out := logs.String()
	if strings.Count(out, "reset link redeemed") != 1 {
		t.Fatalf("want exactly one redeemed line:\n%s", out)
	}
	if strings.Contains(out, token) || strings.Contains(out, resetNewPassword) {
		t.Fatalf("logs carry the token or the password:\n%s", out)
	}
	if rec := f.redeem(token, resetNewPassword); rec.Code != http.StatusNotFound {
		t.Fatalf("reuse = %d, want 404", rec.Code)
	}
	if strings.Count(logs.String(), "reset link redeemed") != 1 {
		t.Fatal("a reused link logged a second redemption")
	}
}

func TestResetLinkRedeemPasswordBoundsLiveDB(t *testing.T) {
	f := newResetFixture(t)
	target := f.target(t)
	token := f.mustIssue(t, target)
	for name, pw := range map[string]string{"short": "short", "huge": strings.Repeat("a", auth.MaxPasswordLen+1)} {
		if rec := f.redeem(token, pw); rec.Code != http.StatusBadRequest {
			t.Errorf("%s password = %d, want 400", name, rec.Code)
		}
	}
	f.assertLive(t, token)
}

// A CLI poll of an approved request racing a reset redeem must not deadlock. The redeem
// holds users(T) FOR UPDATE and then denies T's approved requests; the poll must take its
// users lock before claiming the request row (users -> cli_auth_requests), or it holds
// the request while CreateCLIToken's FK check waits on users and the deny waits on the
// request (40P01). Simulated with an open redeem-shaped transaction.
func TestCLIPollRacingResetRedeemDoesNotDeadlockLiveDB(t *testing.T) {
	f := newResetFixture(t)
	ctx := context.Background()
	target := f.target(t)
	oldJWT := cliMintJWT(t, f.pool, target)
	verifier := "verifier-" + uuid.NewString()
	reqID, code := cliStart(t, f.router, s256Challenge(verifier), "poll racing reset")
	if rec := cookieReq(t, f.router, http.MethodPost, "/api/auth/cli/approve", oldJWT,
		fmt.Sprintf(`{"request_id":%q,"user_code":%q}`, reqID, code)); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d\n%s", rec.Code, rec.Body.String())
	}

	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, target); err != nil {
		t.Fatal(err)
	}
	res := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		res <- cliPostJSON(f.router, "/api/auth/cli/poll", fmt.Sprintf(`{"request_id":%q,"verifier":%q}`, reqID, verifier))
	}()
	// Wait until the poll is parked on a lock rather than guessing with a sleep.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
		    WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case rec := <-res:
			t.Fatalf("poll finished (%d) while the redeem-shaped transaction held the user row", rec.Code)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("poll never blocked on a lock")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := tx.Exec(ctx, `UPDATE cli_auth_requests SET status = 'denied' WHERE user_id = $1 AND status = 'approved'`, target); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
			t.Fatalf("redeem deny deadlocked against the CLI poll: %v", err)
		}
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-res:
		if rec.Code == http.StatusOK || rec.Code >= 500 {
			t.Fatalf("poll = %d, want a terminal non-minting status\n%s", rec.Code, rec.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("poll never completed")
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM cli_auth_requests WHERE id=$1`, reqID).Scan(&status); err != nil || status != "denied" {
		t.Fatalf("request status = %q (%v), want denied", status, err)
	}
	if n := cliCountTokens(t, f.pool, target); n != 0 {
		t.Fatalf("%d CLI tokens exist for the reset user, want 0", n)
	}
}

// A CLI approve in flight while a reset commits: the approve holds the row FOR SHARE
// check, so it waits on the redemption's row lock and is refused once the session's
// token_version no longer matches. Simulated with an open transaction that holds the
// users row and bumps token_version, as the redemption does.
func TestCLIApproveWaitsForConcurrentResetAndIsRefusedLiveDB(t *testing.T) {
	f := newResetFixture(t)
	ctx := context.Background()
	target := f.target(t)
	oldJWT := cliMintJWT(t, f.pool, target)
	reqID, code := cliStart(t, f.router, s256Challenge("verifier-"+uuid.NewString()), "racing approve")

	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	if _, err := tx.Exec(ctx, `UPDATE users SET token_version = token_version + 1 WHERE id=$1`, target); err != nil {
		t.Fatal(err)
	}
	res := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		res <- cookieReq(t, f.router, http.MethodPost, "/api/auth/cli/approve", oldJWT,
			fmt.Sprintf(`{"request_id":%q,"user_code":%q}`, reqID, code))
	}()
	select {
	case rec := <-res:
		t.Fatalf("approve finished (%d) while the reset transaction held the user row", rec.Code)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-res:
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("approve after the reset = %d, want 401\n%s", rec.Code, rec.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("approve never completed")
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM cli_auth_requests WHERE id=$1`, reqID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("request status = %q (%v), want pending", status, err)
	}
}
