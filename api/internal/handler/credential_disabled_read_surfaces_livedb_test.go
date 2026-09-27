package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1732 M4, the read surfaces through the real handlers on a live database: the owner
// and admin rate-limit endpoints (Anthropic and Codex) omit disabled credentials, with no
// row and no count for the admin (D9); a Codex account's labels are its enabled aliases
// (D6); GET /api/me/secrets keeps listing a disabled credential with enabled:false; and
// usage and cost history are identical before and after a disable (D9).

func readSurfacesAdminReq(user uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	return req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: user, IsAdmin: true, IsActive: true}))
}

// serveJSON runs one handler, requires a 200 and returns the body.
func serveJSON(t *testing.T, fn http.HandlerFunc, req *http.Request) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	fn(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

type readSurfacesFixture struct {
	h                    *Handler
	pool                 *pgxpool.Pool
	user, other, admin   uuid.UUID
	def, spare, lone     uuid.UUID // anthropic tokens (lone belongs to other)
	work, extra, solo    uuid.UUID // codex_auth aliases: work+extra share acctShared, solo is acctSolo
	acctShared, acctSolo uuid.UUID
	labels               map[uuid.UUID]string
}

func newReadSurfacesFixture(t *testing.T) *readSurfacesFixture {
	t.Helper()
	h, pool := secretsCRUDHandler(t)
	h.cfg = config.Config{UsagePollInterval: 5 * time.Minute, CodexUsagePollInterval: 5 * time.Minute}
	h.wsvc = workersvc.New(h.q, h.box, workersvc.Params{})
	ctx := context.Background()
	f := &readSurfacesFixture{h: h, pool: pool, labels: map[uuid.UUID]string{}}
	f.user, f.other, f.admin = mkSecretUser(t, pool), mkSecretUser(t, pool), mkSecretUser(t, pool)

	token := func(user uuid.UUID, label string, isDefault bool) uuid.UUID {
		row, err := h.q.InsertUserSecret(ctx, store.InsertUserSecretParams{
			UserID: user, Kind: store.KindAnthropicToken, Label: label, WantDefault: isDefault,
			Ciphertext: []byte("ct"), SealedWith: store.SealedWithMaster,
		})
		if err != nil {
			t.Fatalf("insert token %q: %v", label, err)
		}
		f.labels[row.ID] = label
		return row.ID
	}
	f.def = token(f.user, "rs-default", true)
	f.spare = token(f.user, "rs-spare", false)
	f.lone = token(f.other, "rs-lone", true)

	account := func() uuid.UUID {
		acc, err := h.q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
			UserID: f.user, ProviderUserID: "prov-" + uuid.NewString(), WorkspaceAccountID: "ws-" + uuid.NewString(),
			SealedLogin: []byte("sealed"), SealedWith: store.SealedWithMaster,
		})
		if err != nil {
			t.Fatalf("insert provider account: %v", err)
		}
		return acc.ID
	}
	alias := func(acc uuid.UUID, label string, isDefault bool) uuid.UUID {
		sec, err := h.q.InsertCodexSecret(ctx, store.InsertCodexSecretParams{
			UserID: f.user, Kind: store.KindCodexAuth, Label: label, WantDefault: isDefault,
			Ciphertext: []byte("sealed"), SealedWith: store.SealedWithMaster,
		})
		if err != nil {
			t.Fatalf("insert codex secret %q: %v", label, err)
		}
		if _, err := h.q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
			UserSecretID: sec.ID, UserID: f.user, Status: "staging",
		}); err != nil {
			t.Fatalf("insert credential state: %v", err)
		}
		if n, err := h.q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
			ProviderAccountID: pgconv.UUID(acc), UserSecretID: sec.ID, UserID: f.user, MaterialRevision: 0,
		}); err != nil || n != 1 {
			t.Fatalf("link %q = (%d, %v)", label, n, err)
		}
		f.labels[sec.ID] = label
		return sec.ID
	}
	f.acctShared, f.acctSolo = account(), account()
	f.work = alias(f.acctShared, "rs-work", true)
	f.extra = alias(f.acctShared, "rs-extra", false)
	f.solo = alias(f.acctSolo, "rs-solo", false)
	return f
}

// anthropicSelf returns the labels GET /api/me/rate-limits lists for user.
func (f *readSurfacesFixture) anthropicSelf(t *testing.T, user uuid.UUID) []string {
	t.Helper()
	var out struct {
		Tokens []struct {
			Label string `json:"label"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(serveJSON(t, f.h.SelfRateLimits, userReq(http.MethodGet, "/x", "", user, nil)), &out); err != nil {
		t.Fatal(err)
	}
	labels := []string{}
	for _, tok := range out.Tokens {
		labels = append(labels, tok.Label)
	}
	return labels
}

// anthropicAdmin returns, per fixture user id, the token labels GET /api/admin/rate-limits
// lists (present with an empty list for a user shown with no tokens).
func (f *readSurfacesFixture) anthropicAdmin(t *testing.T) map[string][]string {
	t.Helper()
	var out struct {
		Users []struct {
			ID     string `json:"id"`
			Tokens []struct {
				Label string `json:"label"`
			} `json:"tokens"`
		} `json:"users"`
	}
	if err := json.Unmarshal(serveJSON(t, f.h.AdminRateLimits, readSurfacesAdminReq(f.admin)), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, u := range out.Users {
		if u.ID != f.user.String() && u.ID != f.other.String() {
			continue
		}
		labels := []string{}
		for _, tok := range u.Tokens {
			labels = append(labels, tok.Label)
		}
		got[u.ID] = labels
	}
	return got
}

type codexAccountAliases map[string][]string // account id -> alias labels

func (f *readSurfacesFixture) codexSelf(t *testing.T) codexAccountAliases {
	t.Helper()
	var out struct {
		Accounts []codexAccountJSON `json:"accounts"`
	}
	if err := json.Unmarshal(serveJSON(t, f.h.SelfCodexRateLimits, codexRLReq(f.user, false)), &out); err != nil {
		t.Fatal(err)
	}
	got := codexAccountAliases{}
	for _, a := range out.Accounts {
		got[a.AccountID] = a.Aliases
	}
	return got
}

// codexAdmin returns the fixture user's accounts from GET /api/admin/codex-rate-limits, and
// whether the user appears there at all.
func (f *readSurfacesFixture) codexAdmin(t *testing.T) (codexAccountAliases, bool) {
	t.Helper()
	var out struct {
		Users []struct {
			ID       string             `json:"id"`
			Accounts []codexAccountJSON `json:"accounts"`
		} `json:"users"`
	}
	if err := json.Unmarshal(serveJSON(t, f.h.AdminCodexRateLimits, readSurfacesAdminReq(f.admin)), &out); err != nil {
		t.Fatal(err)
	}
	for _, u := range out.Users {
		if u.ID != f.user.String() {
			continue
		}
		got := codexAccountAliases{}
		for _, a := range u.Accounts {
			got[a.AccountID] = a.Aliases
		}
		return got, true
	}
	return nil, false
}

func (f *readSurfacesFixture) disable(t *testing.T, user uuid.UUID, kind string, id uuid.UUID) {
	t.Helper()
	if code, out := enablementRequest(t, f.h, user, kind, id, `{"enabled":false}`); code != http.StatusOK {
		t.Fatalf("disable %s %s: %d %v", kind, f.labels[id], code, out)
	}
}

// TestRateLimitEndpointsOmitDisabledCredentialsLiveDB: the four rate-limit endpoints stop
// listing a credential the moment it is disabled, the admin ones with no row and no count,
// and a Codex account is named by its enabled aliases and disappears with its last one.
func TestRateLimitEndpointsOmitDisabledCredentialsLiveDB(t *testing.T) {
	f := newReadSurfacesFixture(t)
	u, o := f.user.String(), f.other.String()
	shared, solo := f.acctShared.String(), f.acctSolo.String()

	// Control: everything enabled, everything listed.
	if got := f.anthropicSelf(t, f.user); !slices.Equal(got, []string{"rs-default", "rs-spare"}) {
		t.Fatalf("control: self anthropic = %v", got)
	}
	if got := f.anthropicAdmin(t); !slices.Equal(got[u], []string{"rs-default", "rs-spare"}) || !slices.Equal(got[o], []string{"rs-lone"}) {
		t.Fatalf("control: admin anthropic = %v", got)
	}
	if got := f.codexSelf(t); len(got) != 2 || !slices.Equal(got[shared], []string{"rs-work", "rs-extra"}) || !slices.Equal(got[solo], []string{"rs-solo"}) {
		t.Fatalf("control: self codex = %v", got)
	}
	if got, ok := f.codexAdmin(t); !ok || len(got) != 2 {
		t.Fatalf("control: admin codex = %v (listed %v)", got, ok)
	}

	f.disable(t, f.user, "anthropic_token", f.spare)
	f.disable(t, f.other, "anthropic_token", f.lone) // the other user's last token
	f.disable(t, f.user, "codex_auth", f.extra)
	f.disable(t, f.user, "codex_auth", f.solo)

	if got := f.anthropicSelf(t, f.user); !slices.Equal(got, []string{"rs-default"}) {
		t.Fatalf("self anthropic after disable = %v, want [rs-default]", got)
	}
	if got := f.anthropicSelf(t, f.other); len(got) != 0 {
		t.Fatalf("self anthropic with every token disabled = %v, want []", got)
	}
	got := f.anthropicAdmin(t)
	if !slices.Equal(got[u], []string{"rs-default"}) {
		t.Fatalf("admin anthropic for the user = %v, want [rs-default]", got[u])
	}
	if tokens, listed := got[o]; !listed || len(tokens) != 0 {
		t.Fatalf("admin anthropic for a user with every token disabled = %v (listed %v), want listed with no tokens", tokens, listed)
	}
	if got := f.codexSelf(t); len(got) != 1 || !slices.Equal(got[shared], []string{"rs-work"}) {
		t.Fatalf("self codex after disable = %v, want only the shared account named rs-work", got)
	}
	if got, ok := f.codexAdmin(t); !ok || len(got) != 1 || !slices.Equal(got[shared], []string{"rs-work"}) {
		t.Fatalf("admin codex after disable = %v (listed %v), want only the shared account named rs-work", got, ok)
	}

	// The last enabled Codex alias: no account, and the admin view drops the user (no
	// token-less row exists on the Codex side).
	f.disable(t, f.user, "codex_auth", f.work)
	if got := f.codexSelf(t); len(got) != 0 {
		t.Fatalf("self codex with every alias disabled = %v, want none", got)
	}
	if got, ok := f.codexAdmin(t); ok {
		t.Fatalf("admin codex lists a user whose aliases are all disabled: %v", got)
	}
}

// TestListMySecretsKeepsDisabledCredentialsLiveDB: Settings still lists a disabled
// credential, of every kind, flagged enabled:false with its disabled_at.
func TestListMySecretsKeepsDisabledCredentialsLiveDB(t *testing.T) {
	f := newReadSurfacesFixture(t)
	f.disable(t, f.user, "anthropic_token", f.spare)
	f.disable(t, f.user, "codex_auth", f.extra)

	var out struct {
		Secrets []struct {
			ID         string  `json:"id"`
			Enabled    bool    `json:"enabled"`
			DisabledAt *string `json:"disabled_at"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(serveJSON(t, f.h.ListMySecrets, userReq(http.MethodGet, "/api/me/secrets", "", f.user, nil)), &out); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, s := range out.Secrets {
		id := uuid.MustParse(s.ID)
		wantEnabled := id != f.spare && id != f.extra
		if s.Enabled != wantEnabled || (s.DisabledAt != nil) == wantEnabled {
			t.Fatalf("%s: enabled=%v disabled_at=%v, want enabled=%v", f.labels[id], s.Enabled, s.DisabledAt, wantEnabled)
		}
		seen[s.ID] = true
	}
	for _, id := range []uuid.UUID{f.def, f.spare, f.work, f.extra, f.solo} {
		if !seen[id.String()] {
			t.Fatalf("GET /api/me/secrets does not list %s", f.labels[id])
		}
	}
}

// TestUsageHistoryUnchangedByDisableLiveDB (D9): the owner's and the admin's usage and cost
// totals, and the run records' credential attribution, are byte-identical before and after
// the credentials those runs spent are disabled.
func TestUsageHistoryUnchangedByDisableLiveDB(t *testing.T) {
	f := newReadSurfacesFixture(t)
	ctx := context.Background()
	conn, repo := uuid.New(), uuid.New()
	mustExecT(ctx, t, f.pool, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, conn, f.user, []byte{0x1})
	mustExecT(ctx, t, f.pool, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		VALUES ($1, $2, 1, $3, 'https://forge.e2e/g/r', 'main', true)`, repo, conn, "g/rs-"+repo.String())
	// One Claude run that spent the spare token, one Codex run that spent the extra alias.
	runs := []uuid.UUID{uuid.New(), uuid.New()}
	mustExecT(ctx, t, f.pool, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status,
		anthropic_secret_id, anthropic_secret_label, created_at)
		VALUES ($1, $2, $3, 1, 't', 'd', 'completed', $4, 'rs-spare', now())`, runs[0], f.user, repo, f.spare)
	mustExecT(ctx, t, f.pool, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status,
		harness, codex_secret_id, codex_secret_label, created_at)
		VALUES ($1, $2, $3, 2, 't', 'd', 'completed', 'codex', $4, 'rs-extra', now() - interval '10 days')`,
		runs[1], f.user, repo, f.extra)
	for i, run := range runs {
		mustExecT(ctx, t, f.pool, `INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, input_tokens, output_tokens, cost_usd)
			VALUES ($1, 's', 'm', 0, $2, $3, $4)`, run, 1000*(i+1), 100*(i+1), fmt.Sprintf("%d.25", i+1))
	}

	snapshot := func() (self, admin, records string) {
		t.Helper()
		self = string(serveJSON(t, f.h.SelfUsage, userReq(http.MethodGet, "/api/me/usage", "", f.user, nil)))
		admin = string(serveJSON(t, f.h.AdminUsage, readSurfacesAdminReq(f.admin)))
		rows, err := f.pool.Query(ctx, `SELECT id, anthropic_secret_id, anthropic_secret_label, codex_secret_id, codex_secret_label
			FROM runs WHERE user_id = $1 ORDER BY id`, f.user)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var aid, cid *uuid.UUID
			var alabel, clabel *string
			if err := rows.Scan(&id, &aid, &alabel, &cid, &clabel); err != nil {
				t.Fatal(err)
			}
			records += fmt.Sprintf("%s %v %v %v %v\n", id, deref(aid), deref(alabel), deref(cid), deref(clabel))
		}
		return self, admin, records
	}
	self0, admin0, rec0 := snapshot()
	var probe struct {
		Lifetime struct {
			InputTokens int64 `json:"input_tokens"`
		} `json:"lifetime"`
		RunCount int64 `json:"run_count"`
	}
	if err := json.Unmarshal([]byte(self0), &probe); err != nil || probe.Lifetime.InputTokens != 3000 || probe.RunCount != 2 {
		t.Fatalf("control: self usage %s (err %v), want 3000 input tokens over 2 runs", self0, err)
	}

	f.disable(t, f.user, "anthropic_token", f.spare)
	f.disable(t, f.user, "codex_auth", f.extra)
	f.disable(t, f.user, "anthropic_token", f.def) // the last one: default cleared

	self1, admin1, rec1 := snapshot()
	if self1 != self0 {
		t.Fatalf("self usage changed on disable:\nbefore %s\nafter  %s", self0, self1)
	}
	if admin1 != admin0 {
		t.Fatalf("admin usage changed on disable:\nbefore %s\nafter  %s", admin0, admin1)
	}
	if rec1 != rec0 {
		t.Fatalf("run records changed on disable:\nbefore %s\nafter  %s", rec0, rec1)
	}
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
