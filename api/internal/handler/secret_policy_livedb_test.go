package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type policyHTTPWriter struct {
	name, kind, method string
	call               func(*Handler, http.ResponseWriter, *http.Request)
}

func policyHTTPWriters() []policyHTTPWriter {
	return []policyHTTPWriter{
		{"compat", store.KindAnthropicToken, http.MethodPut, (*Handler).PutAnthropicToken},
		{"anthropic-create", store.KindAnthropicToken, http.MethodPost, (*Handler).CreateAnthropicToken},
		{"anthropic-patch", store.KindAnthropicToken, http.MethodPatch, (*Handler).PatchAnthropicToken},
		{"key-create", store.KindOpenAIAPIKey, http.MethodPost, (*Handler).CreateOpenAIAPIKey},
		{"key-patch", store.KindOpenAIAPIKey, http.MethodPatch, (*Handler).PatchOpenAIAPIKey},
		{"login-create", store.KindCodexAuth, http.MethodPost, (*Handler).CreateCodexAuth},
		{"login-patch", store.KindCodexAuth, http.MethodPatch, (*Handler).PatchCodexAuth},
	}
}
func policyValue(kind, suffix string) string {
	if kind == store.KindCodexAuth {
		return `{"access_token":"policy-access-` + suffix + `","refresh_token":"policy-refresh-` + suffix + `"}`
	}
	return "policy-material-" + suffix
}
func policySave(h *Handler, ctx context.Context, uid uuid.UUID, writer policyHTTPWriter, id string, suffix string) *httptest.ResponseRecorder {
	body := map[string]any{"token": policyValue(writer.kind, suffix)}
	if writer.method != http.MethodPut {
		body["label"] = "label-" + suffix
		body["default"] = true
	}
	raw, _ := json.Marshal(body)
	original := userReq(writer.method, "/", string(raw), uid, map[string]string{"id": id})
	reqctx := context.WithValue(ctx, chi.RouteCtxKey, chi.RouteContext(original.Context()))
	req := original.WithContext(mw.ContextWithUser(reqctx, store.User{ID: uid, IsActive: true}))
	rec := httptest.NewRecorder()
	writer.call(h, rec, req)
	return rec
}

// The snapshot includes ciphertext, timestamps, enablement/material revisions,
// defaults and every alias/account field. Compare after the lifecycle transition
// so its intentional migration cannot hide a partial stale HTTP mutation.
func policyMaterialSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, uid uuid.UUID) string {
	t.Helper()
	var snapshot string
	err := pool.QueryRow(ctx, `
 SELECT jsonb_build_array(
 (SELECT coalesce(jsonb_agg(to_jsonb(s) ORDER BY s.id),'[]'::jsonb) FROM user_secrets s WHERE s.user_id=$1),
 (SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY c.user_secret_id),'[]'::jsonb) FROM codex_credential_state c JOIN user_secrets s ON s.id=c.user_secret_id WHERE s.user_id=$1),
 (SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY a.id),'[]'::jsonb) FROM codex_provider_account a WHERE a.user_id=$1)
 )::text`, uid).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertPolicyMaterialOpens(t *testing.T, h *Handler, ctx context.Context, uid uuid.UUID, wantMarker string) {
	t.Helper()
	rows, err := h.pool.Query(ctx, "SELECT kind,ciphertext,sealed_with FROM user_secrets WHERE user_id=$1 ORDER BY id", uid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var kind, marker string
		var ct []byte
		if err := rows.Scan(&kind, &ct, &marker); err != nil {
			t.Fatal(err)
		}
		if marker != wantMarker {
			t.Fatalf("marker=%q want=%q", marker, wantMarker)
		}
		plain, err := h.vault.Open(uid, kind, marker, ct)
		if err != nil || !bytes.HasPrefix(plain, []byte("policy-material-")) && kind != store.KindCodexAuth {
			t.Fatalf("committed material cannot open: %v", err)
		}
		if kind == store.KindCodexAuth && !bytes.Contains(plain, []byte("policy-access-")) {
			t.Fatal("wrong login material")
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("no material checked")
	}
}

// Root executes this serially with the real DB. Each writer stops after sealing
// but before acquiring its transaction/advisory lock; no sleep orders the race.
func TestHTTPSecretWritersRejectLifecycleRacesLiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)
	ctx := context.Background()
	var previous string
	if err := pool.QueryRow(ctx, "SELECT value FROM app_settings WHERE key='vault_policy'").Scan(&previous); err != nil {
		t.Fatal(err)
	}
	mustExecT(ctx, t, pool, "UPDATE app_settings SET value='optional' WHERE key='vault_policy'")
	t.Cleanup(func() {
		mustExecT(context.Background(), t, pool, "UPDATE app_settings SET value=$1 WHERE key='vault_policy'", previous)
	})
	for _, writer := range policyHTTPWriters() {
		for _, transition := range []string{"enable", "disable", "disable-enable-ABA"} {
			t.Run(writer.name+"/"+transition, func(t *testing.T) {
				uid := mkSecretUser(t, pool)
				const password = "policy-fixture-password"
				setVaultFixturePassword(t, pool, uid, password)
				raceCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				// Seed a live default, and for Codex a real staging/static alias.
				seed := writer
				seed.method = http.MethodPost
				switch writer.kind {
				case store.KindAnthropicToken:
					seed.call = (*Handler).CreateAnthropicToken
				case store.KindOpenAIAPIKey:
					seed.call = (*Handler).CreateOpenAIAPIKey
				case store.KindCodexAuth:
					seed.call = (*Handler).CreateCodexAuth
				}
				created := policySave(h, raceCtx, uid, seed, "", "old")
				if created.Code != http.StatusCreated {
					t.Fatalf("seed=%d %s", created.Code, created.Body.String())
				}
				id := decodeSecret(t, created).Secret.ID
				if transition != "enable" {
					if err := h.vault.Enable(raceCtx, uid, password); err != nil {
						t.Fatal(err)
					}
				}
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				done := make(chan *httptest.ResponseRecorder, 1)
				joined := false
				h.secretBeginTx = func(ctx context.Context) (pgx.Tx, error) {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					return pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
				}
				defer func() {
					unblock()
					cancel()
					if !joined {
						<-done
					}
					h.secretBeginTx = nil
				}()
				go func() { done <- policySave(h, raceCtx, uid, writer, id, "new") }()
				select {
				case <-entered:
				case rec := <-done:
					joined = true
					t.Fatalf("writer failed before barrier: %d %s", rec.Code, rec.Body.String())
				case <-raceCtx.Done():
					t.Fatal("writer never reached post-seal barrier")
				}
				switch transition {
				case "enable":
					if err := h.vault.Enable(raceCtx, uid, password); err != nil {
						t.Fatal(err)
					}
				case "disable":
					if err := h.vault.Disable(raceCtx, uid); err != nil {
						t.Fatal(err)
					}
				case "disable-enable-ABA":
					if err := h.vault.Disable(raceCtx, uid); err != nil {
						t.Fatal(err)
					}
					if err := h.vault.Enable(raceCtx, uid, password); err != nil {
						t.Fatal(err)
					}
				}
				before := policyMaterialSnapshot(t, raceCtx, pool, uid)
				unblock()
				var rec *httptest.ResponseRecorder
				select {
				case rec = <-done:
					joined = true
				case <-raceCtx.Done():
					t.Fatal("writer did not finish")
				}
				h.secretBeginTx = nil
				if rec.Code != http.StatusConflict {
					t.Fatalf("stale save=%d %s", rec.Code, rec.Body.String())
				}
				if after := policyMaterialSnapshot(t, raceCtx, pool, uid); after != before {
					t.Fatal("stale save changed credential/default/revision/alias/account snapshot")
				}
				marker := store.SealedWithDEK
				if transition == "disable" {
					marker = store.SealedWithMasterAAD
				}
				assertPolicyMaterialOpens(t, h, raceCtx, uid, marker)
				// A fresh request succeeds with the current epoch/key after the refusal.
				fresh := policySave(h, raceCtx, uid, writer, id, "fresh")
				if fresh.Code != http.StatusOK && fresh.Code != http.StatusCreated {
					t.Fatalf("fresh save=%d %s", fresh.Code, fresh.Body.String())
				}
				assertPolicyMaterialOpens(t, h, raceCtx, uid, marker)
			})
		}
	}
}
