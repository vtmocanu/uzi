package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

const toggleGatewayBase = "https://gw.example.com/v1"

// PRD #4574 M2, owner decision Q3: each half of the consent toggle follows the lane decision. Every 409
// row has the OTHER half usable and every 200 row has the half that should count its source WITHOUT a
// vendor credential, so a wrong vendor fallback cannot pass. Run through the real handler, service and
// database; skipped unless UZI_TEST_DATABASE_URL is set.
func TestSetCrossCheckConsentFollowsTheLaneDecisionLiveDB(t *testing.T) {
	type owner struct {
		anthropic, vendorCodex bool
		// "" none, "usable" or "unusable".
		claudeSrc, codexSrc string
	}
	for _, tc := range []struct {
		name string
		o    owner
		want int
	}{
		{"usable codex source, no vendor codex credential, vendor anthropic token", owner{anthropic: true, codexSrc: "usable"}, http.StatusOK},
		{"usable claude source with no anthropic token, usable codex source", owner{claudeSrc: "usable", codexSrc: "usable"}, http.StatusOK},
		{"unusable codex source beats a usable vendor codex credential", owner{anthropic: true, vendorCodex: true, codexSrc: "unusable"}, http.StatusConflict},
		{"unusable claude source beats a vendor anthropic token", owner{anthropic: true, claudeSrc: "unusable", codexSrc: "usable"}, http.StatusConflict},
		{"usable claude source with no codex source stays refused despite a vendor codex credential", owner{vendorCodex: true, claudeSrc: "usable"}, http.StatusConflict},
		{"vendor and vendor", owner{anthropic: true, vendorCodex: true}, http.StatusOK},
		{"vendor and vendor, no anthropic token", owner{vendorCodex: true}, http.StatusConflict},
		{"vendor and vendor, no codex credential", owner{anthropic: true}, http.StatusConflict},
	} {
		for _, field := range []string{"plan", "code"} {
			t.Run(tc.name+"/"+field, func(t *testing.T) {
				h, pool := secretsCRUDHandler(t)
				h.wsvc = workersvc.New(h.q, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit(), ProviderAllowedBaseURLs: []string{toggleGatewayBase}})
				user := mkSecretUser(t, pool)
				ctx := context.Background()
				exec := func(sql string, args ...any) {
					t.Helper()
					if _, err := pool.Exec(ctx, sql, args...); err != nil {
						t.Fatalf("%s: %v", sql, err)
					}
				}
				t.Cleanup(func() {
					exec(`UPDATE users SET claude_provider_id=NULL,claude_provider_scope_id=NULL,codex_provider_id=NULL,codex_provider_scope_id=NULL WHERE id=$1`, user)
					exec(`DELETE FROM users WHERE id=$1`, user)
				})
				if tc.o.anthropic {
					exec(`INSERT INTO user_secrets (id,user_id,kind,label,is_default,ciphertext,sealed_with) VALUES ($1,$2,'anthropic_token','a',true,'\x01','master')`, uuid.New(), user)
				}
				if tc.o.vendorCodex {
					secret := uuid.New()
					exec(`INSERT INTO user_secrets (id,user_id,kind,label,is_default,ciphertext,sealed_with) VALUES ($1,$2,'openai_api_key','c',true,'\x01','master')`, secret, user)
					if _, err := h.q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{UserSecretID: secret, UserID: user, Status: "static"}); err != nil {
						t.Fatal(err)
					}
				}
				for _, src := range []struct{ engine, state, protocol, modelMap string }{
					{"claude", tc.o.claudeSrc, "anthropic_messages", `{"opus":{"model":"o"},"sonnet":{"model":"s"},"haiku":{"model":"h"}}`},
					{"codex", tc.o.codexSrc, "openai_responses", `{"default":{"model":"gateway"}}`},
				} {
					if src.state != "" {
						selectToggleSource(ctx, t, h, pool, user, src.engine, src.protocol, src.modelMap, src.state == "unusable")
					}
				}
				rec := httptest.NewRecorder()
				h.SetCrossCheck(rec, userReq(http.MethodPut, "/api/me/cross-check", `{"`+field+`":true}`, user, nil))
				if rec.Code != tc.want {
					t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
				}
				var enabled bool
				col := field + "_cross_check_enabled"
				if err := pool.QueryRow(ctx, `SELECT `+col+` FROM users WHERE id=$1`, user).Scan(&enabled); err != nil {
					t.Fatal(err)
				}
				if enabled != (tc.want == http.StatusOK) {
					t.Fatalf("%s = %t after status %d", col, enabled, rec.Code)
				}
			})
		}
	}
}

// selectToggleSource seeds a provider (revision 1 at the allowed gateway) with an enabled key and selects
// it as the owner's source for engine; unusable disables the key.
func selectToggleSource(ctx context.Context, t *testing.T, h *Handler, pool *pgxpool.Pool, user uuid.UUID, engine, protocol, modelMap string, unusable bool) {
	t.Helper()
	p, err := h.q.InsertModelProvider(ctx, store.InsertModelProviderParams{OwnerUserID: user, Name: "toggle-" + uuid.NewString()[:8], Engine: engine})
	if err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	if err := h.q.InsertModelProviderRevision(ctx, store.InsertModelProviderRevisionParams{
		ProviderID: p.ID, Revision: 1, Protocol: protocol, BaseUrl: toggleGatewayBase, AuthStyle: "bearer", ModelMap: []byte(modelMap)}); err != nil {
		t.Fatalf("insert revision: %v", err)
	}
	key, err := h.q.InsertProviderKeySecret(ctx, store.InsertProviderKeySecretParams{
		UserID: user, Label: p.ID.String(), Ciphertext: []byte("ct"), SealedWith: "master", ProviderID: pgtype.UUID{Bytes: p.ID, Valid: true}})
	if err != nil {
		t.Fatalf("insert key: %v", err)
	}
	if unusable {
		if _, err := pool.Exec(ctx, `UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, key.ID); err != nil {
			t.Fatal(err)
		}
	}
	col := engine + "_provider_id"
	if _, err := pool.Exec(ctx, `UPDATE users SET `+col+` = $2, `+engine+`_provider_scope_id = id WHERE id = $1`, user, p.ID); err != nil {
		t.Fatalf("select %s source: %v", engine, err)
	}
}
