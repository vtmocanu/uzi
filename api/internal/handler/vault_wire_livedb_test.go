package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestAdminRateLimitDurableModeWireLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	enrollmentPolicy(t, pool, "optional")
	ctx := context.Background()
	h.settings = settings.New(h.q, time.Hour)
	h.cfg.CodexUsagePollInterval = time.Minute
	adminID := cliSeedUser(t, pool, true)
	lockedID := cliSeedUser(t, pool, false)
	unlockedID := cliSeedUser(t, pool, false)
	// Protected legacy users need not have a wrapper. Mode comes from users,
	// independently of both wrapper existence and the raw cache.
	mustExecT(ctx, t, pool, "UPDATE users SET vault_mode='protected' WHERE id=$1", lockedID)
	const password = "wire-contract-password"
	setVaultFixturePassword(t, pool, unlockedID, password)
	if err := h.vault.Enable(ctx, unlockedID, password); err != nil {
		t.Fatal(err)
	}

	for _, id := range []uuid.UUID{adminID, lockedID, unlockedID} {
		// This material is never opened by either read producer.
		for _, disabled := range []bool{false, true} {
			var disabledAt any
			label := "enabled-token"
			if disabled {
				disabledAt = time.Now()
				label = "disabled-token"
			}
			mustExecT(ctx, t, pool, "INSERT INTO user_secrets (user_id,kind,label,ciphertext,sealed_with,disabled_at) VALUES ($1,'anthropic_token',$2,$3,'master',$4)",
				id, label, []byte("opaque-read-fixture"), disabledAt)
		}
		secret, err := h.q.InsertCodexSecret(ctx, store.InsertCodexSecretParams{
			UserID: id, Kind: store.KindCodexAuth, Label: "visible-alias", WantDefault: true,
			Ciphertext: []byte("opaque-read-fixture"), SealedWith: store.SealedWithMaster,
		})
		if err != nil {
			t.Fatal(err)
		}
		account, err := h.q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
			UserID: id, ProviderUserID: "provider-" + uuid.NewString(), WorkspaceAccountID: "workspace-" + uuid.NewString(),
			SealedLogin: []byte("opaque-read-fixture"), SealedWith: store.SealedWithMaster,
		})
		if err != nil {
			t.Fatal(err)
		}
		mustExecT(ctx, t, pool, "UPDATE codex_provider_account SET reauth_required=true, reauth_generation=0, reauth_credential_revision=0, reauth_reason='provider_rejected' WHERE id=$1", account.ID)
		if _, err := h.q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
			UserSecretID: secret.ID, UserID: id, Status: "staging",
		}); err != nil {
			t.Fatal(err)
		}
		if n, err := h.q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
			UserSecretID: secret.ID, UserID: id, ProviderAccountID: pgconv.UUID(account.ID), MaterialRevision: 0,
		}); err != nil || n != 1 {
			t.Fatalf("link=%d err=%v", n, err)
		}
	}
	admin, err := h.q.GetUserByID(ctx, adminID)
	if err != nil {
		t.Fatal(err)
	}
	wantModes := map[string]string{
		adminID.String(): store.VaultModeUnprotected, lockedID.String(): store.VaultModeProtected,
		unlockedID.String(): store.VaultModeProtected,
	}
	rec := vaultRouteRequest(t, router, h, admin, http.MethodGet, "/api/admin/rate-limits", "", true, false)
	var anthropic struct {
		Users []apitypes.AdminRateLimitRowDTO `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &anthropic); err != nil || rec.Code != http.StatusOK || len(anthropic.Users) != 3 {
		t.Fatalf("anthropic=%d %s err=%v", rec.Code, rec.Body.String(), err)
	}
	for _, row := range anthropic.Users {
		mode, exists := wantModes[row.ID]
		if !exists || row.VaultMode != mode || row.VaultLocked != (row.ID == lockedID.String()) || len(row.Tokens) != 1 {
			t.Fatalf("anthropic row=%+v", row)
		}
		if row.Tokens[0].Label != "enabled-token" || row.Tokens[0].Limits.Status != rateLimitStatusUnavailable {
			t.Fatalf("anthropic enabled-token filter=%+v", row.Tokens)
		}
	}
	for _, polling := range []bool{true, false} {
		if !polling {
			h.cfg.CodexUsagePollInterval = 0
		}
		rec = vaultRouteRequest(t, router, h, admin, http.MethodGet, "/api/admin/codex-rate-limits", "", true, false)
		var codex struct {
			Users []apitypes.CodexAdminRateLimitRowDTO `json:"users"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &codex); err != nil || rec.Code != http.StatusOK || len(codex.Users) != 3 {
			t.Fatalf("codex=%d %s err=%v", rec.Code, rec.Body.String(), err)
		}
		if rec.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("missing private cache header")
		}
		for _, row := range codex.Users {
			mode, exists := wantModes[row.ID]
			if !exists || row.VaultMode != mode || row.VaultLocked != (row.ID == lockedID.String()) || len(row.Accounts) != 1 {
				t.Fatalf("codex row=%+v", row)
			}
			wantStatus := codexRateLimitStatusCredentialActionRequired
			if row.VaultLocked {
				wantStatus = codexRateLimitStatusVaultLocked
			}
			if !polling {
				wantStatus = codexRateLimitStatusPollingDisabled
			}
			if row.Accounts[0].Status != wantStatus {
				t.Fatalf("account=%+v want=%s", row.Accounts[0], wantStatus)
			}
			wantReason := codexRateLimitReasonProviderRejected
			if wantStatus == codexRateLimitStatusVaultLocked {
				wantReason = ""
			}
			if row.Accounts[0].Reason != wantReason {
				t.Fatalf("account reason=%q want=%q", row.Accounts[0].Reason, wantReason)
			}
		}
	}
	if h.vault.Unlocked(adminID) || !h.vault.Unlocked(unlockedID) {
		t.Fatal("Off must not acquire a cached key; protected unlocked must retain its key")
	}
}

func TestVaultPolicySettingsAndCountsWireLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	enrollmentPolicy(t, pool, "optional")
	ctx := context.Background()
	h.settings = settings.New(h.q, time.Hour)
	adminID := cliSeedUser(t, pool, true)
	otherID := cliSeedUser(t, pool, false)
	mustExecT(ctx, t, pool, "UPDATE users SET vault_mode='protected' WHERE id=$1", otherID)
	admin, err := h.q.GetUserByID(ctx, adminID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.q.GetUserByID(ctx, otherID)
	if err != nil {
		t.Fatal(err)
	}
	// Include Off legacy master AND master_aad, and protected master_aad:
	// only protected legacy master belongs in the alert.
	for _, seed := range []struct {
		id            uuid.UUID
		label, marker string
	}{
		{adminID, "off-legacy", store.SealedWithMaster},
		{adminID, "off-aad", store.SealedWithMasterAAD},
		{otherID, "protected-legacy", store.SealedWithMaster},
		{otherID, "protected-aad", store.SealedWithMasterAAD},
	} {
		mustExecT(ctx, t, pool, "INSERT INTO user_secrets (user_id,kind,label,ciphertext,sealed_with) VALUES ($1,'anthropic_token',$2,$3,$4)",
			seed.id, seed.label, []byte("opaque-count-fixture"), seed.marker)
	}
	type material struct {
		ID           uuid.UUID
		Mode, Marker string
		Epoch        int64
		Ciphertext   []byte
	}
	snapshot := func() []material {
		t.Helper()
		rows, err := pool.Query(ctx, "SELECT s.id,u.vault_mode,u.vault_epoch,s.sealed_with,s.ciphertext FROM user_secrets s JOIN users u ON u.id=s.user_id ORDER BY s.id")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := []material{}
		for rows.Next() {
			var m material
			if err := rows.Scan(&m.ID, &m.Mode, &m.Epoch, &m.Marker, &m.Ciphertext); err != nil {
				t.Fatal(err)
			}
			out = append(out, m)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot()
	checkSettings := func(rec *httptest.ResponseRecorder, policy, source string, oidc string) {
		t.Helper()
		var got settingsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("settings=%d %s err=%v", rec.Code, rec.Body.String(), err)
		}
		if got.UnprotectedUsers != 1 || got.Settings[settings.KeyVaultPolicy] != policy ||
			got.Sources[settings.KeyVaultPolicy] != source || got.Defaults[settings.KeyVaultPolicy] != "required" ||
			got.SlackStatus != "disabled" || got.OIDCStatus != oidc {
			t.Fatalf("settings=%+v", got)
		}
	}
	checkSettings(vaultRouteRequest(t, router, h, admin, http.MethodGet, "/api/admin/settings", "", true, false), "optional", "db", "disabled")
	bearer := cliMintToken(t, pool, adminID, clitoken.ScopeAdminRO)
	checkSettings(bearerReq(router, http.MethodGet, "/api/admin/settings", bearer), "optional", "db", "disabled")
	const body = `{"settings":{"vault_policy":"required"}}`
	if rec := bearerReqBody(router, http.MethodPut, "/api/admin/settings", bearer, body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bearer write=%d %s", rec.Code, rec.Body.String())
	}
	for _, tc := range []struct {
		user         store.User
		signed, csrf bool
		code         int
	}{
		{admin, false, false, http.StatusUnauthorized},
		{admin, true, false, http.StatusForbidden},
		{other, true, true, http.StatusForbidden},
	} {
		rec := vaultRouteRequest(t, router, h, tc.user, http.MethodPut, "/api/admin/settings", body, tc.signed, tc.csrf)
		if rec.Code != tc.code {
			t.Fatalf("write=%d want=%d %s", rec.Code, tc.code, rec.Body.String())
		}
	}
	for _, invalid := range []string{"", "invalid", "OPTIONAL"} {
		raw, err := json.Marshal(updateSettingsRequest{Settings: map[string]string{settings.KeyVaultPolicy: invalid}})
		if err != nil {
			t.Fatal(err)
		}
		rec := vaultRouteRequest(t, router, h, admin, http.MethodPut, "/api/admin/settings", string(raw), true, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid policy %q=%d %s", invalid, rec.Code, rec.Body.String())
		}
	}
	// Preserve the existing PUT OIDC metadata behavior (empty) while GET reports
	// live health. The count is explicit in both response constructors.
	checkSettings(vaultRouteRequest(t, router, h, admin, http.MethodPut, "/api/admin/settings", body, true, true), "required", "db", "")
	checkSettings(vaultRouteRequest(t, router, h, admin, http.MethodGet, "/api/admin/settings", "", true, false), "required", "db", "disabled")
	if got := snapshot(); !reflect.DeepEqual(got, before) {
		t.Fatal("policy edit changed a user's mode, epoch or secret material")
	}
	if h.vault.RequiresUnlock(ctx, adminID) || h.vault.Unlocked(adminID) {
		t.Fatal("existing Off under required must stay Off without a cached DEK")
	}
	rec := vaultRouteRequest(t, router, h, admin, http.MethodGet, "/api/admin/vault-migration", "", true, false)
	var migration struct {
		MasterSealed int64 `json:"master_sealed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &migration); err != nil || rec.Code != http.StatusOK || migration.MasterSealed != 1 {
		t.Fatalf("migration=%d %s err=%v", rec.Code, rec.Body.String(), err)
	}
	// A second cookie write checks both policy directions without changing membership.
	checkSettings(vaultRouteRequest(t, router, h, admin, http.MethodPut, "/api/admin/settings", `{"settings":{"vault_policy":"optional"}}`, true, true), "optional", "db", "")
	if got := snapshot(); !reflect.DeepEqual(got, before) {
		t.Fatal("optional policy edit resealed user material")
	}
	h.q = store.New(settingsCountFailureDB{DBTX: pool})
	rec = vaultRouteRequest(t, router, h, admin, http.MethodPut, "/api/admin/settings", body, true, true)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed PUT count=%d %s", rec.Code, rec.Body.String())
	}
}
