package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/resettoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// vault_only account reset links for OIDC-only (passwordless) users (PRD #3989 M2),
// driven through the real router against a live database. Skipped unless
// UZI_TEST_DATABASE_URL is set, like the rest of the reset-link suite.

const resetVaultPassphrase = "vault-passphrase-123456"

// vaultOnlyTarget seeds a passwordless OIDC-linked user with a protected vault
// wrapper and one sealed secret, the state a vault_only reset must wipe.
func (f *resetFixture) vaultOnlyTarget(t *testing.T) uuid.UUID {
	t.Helper()
	id := f.passwordless(t)
	cliMustExec(t, f.pool, `UPDATE users SET vault_mode='protected' WHERE id=$1`, id)
	cliMustExec(t, f.pool, `INSERT INTO user_vaults (user_id, kek_salt, wrapped_dek) VALUES ($1, $2, $3)`,
		id, []byte("0123456789abcdef"), []byte("wrapped"))
	cliMustExec(t, f.pool, `INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES ($1, $2, 'anthropic_token', 'default', true, $3, 'dek')`, uuid.New(), id, []byte("ct"))
	return id
}

func (f *resetFixture) issueDTO(t *testing.T, target uuid.UUID) apitypes.AccountResetLinkDTO {
	t.Helper()
	rec := f.issueAs(t, f.adminJWT, target)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("issue = %d, want 2xx\n%s", rec.Code, rec.Body.String())
	}
	var dto apitypes.AccountResetLinkDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	return dto
}

func (f *resetFixture) vaultCounts(t *testing.T, id uuid.UUID) (wrappers, secrets int, mode string, marker bool) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT
	  (SELECT count(*) FROM user_vaults WHERE user_id=$1),
	  (SELECT count(*) FROM user_secrets WHERE user_id=$1),
	  vault_mode, credentials_reset_at IS NOT NULL FROM users WHERE id=$1`, id).Scan(&wrappers, &secrets, &mode, &marker); err != nil {
		t.Fatal(err)
	}
	return
}

func TestResetLinkVaultOnlyIssuePreviewRedeemLiveDB(t *testing.T) {
	f := newResetFixture(t)
	target := f.vaultOnlyTarget(t)
	oldJWT := cliMintJWT(t, f.pool, target)
	before := f.snapshot(t, target)

	dto := f.issueDTO(t, target)
	if dto.Kind != "vault_only" {
		t.Fatalf("issue kind = %q, want vault_only", dto.Kind)
	}
	token := dto.Token

	prev := f.preview(token)
	if prev.Code != http.StatusOK {
		t.Fatalf("preview = %d\n%s", prev.Code, prev.Body.String())
	}
	var pdto apitypes.ResetLinkPreviewDTO
	if err := json.Unmarshal(prev.Body.Bytes(), &pdto); err != nil {
		t.Fatal(err)
	}
	if pdto.Kind != "vault_only" || pdto.ExpiresAt.IsZero() {
		t.Fatalf("preview = %+v", pdto)
	}

	// A password on a vault_only link is the generic 404, byte-identical to an
	// invalid token's, and changes nothing.
	unknown, _, err := resettoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	invalid := f.redeem(unknown, "")
	withPassword := f.redeem(token, resetNewPassword)
	if withPassword.Code != http.StatusNotFound || invalid.Code != http.StatusNotFound {
		t.Fatalf("redeem with password = %d, invalid token = %d, want 404 both", withPassword.Code, invalid.Code)
	}
	if withPassword.Body.String() != invalid.Body.String() ||
		withPassword.Header().Get("Content-Type") != invalid.Header().Get("Content-Type") {
		t.Fatalf("password redeem differs from invalid-token 404: %q vs %q", withPassword.Body.String(), invalid.Body.String())
	}
	if w, s, mode, marker := f.vaultCounts(t, target); w != 1 || s != 1 || mode != store.VaultModeProtected || marker {
		t.Fatalf("rejected redeem changed state: wrappers=%d secrets=%d mode=%q marker=%v", w, s, mode, marker)
	}
	if after := f.snapshot(t, target); after.version != before.version || after.hash != nil {
		t.Fatalf("rejected redeem changed the user: %+v -> %+v", before, after)
	}
	if f.liveLinks(t, target) != 1 {
		t.Fatal("rejected redeem consumed the link")
	}

	// Seed one row of each kind of per-user state the reset must clear: an approved,
	// unconsumed CLI login request, a Codex provider account and a browser re-unlock slot.
	verifier := "verifier-" + uuid.NewString()
	reqApproved, code := cliStart(t, f.router, s256Challenge(verifier), "approved before reset")
	if rec := cookieReq(t, f.router, http.MethodPost, "/api/auth/cli/approve", oldJWT,
		fmt.Sprintf(`{"request_id":%q,"user_code":%q}`, reqApproved, code)); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d\n%s", rec.Code, rec.Body.String())
	}
	account := uuid.New()
	cliMustExec(t, f.pool, `INSERT INTO codex_provider_account (id,user_id,provider_user_id,workspace_account_id,sealed_login,sealed_with)
		VALUES ($1,$2,$3,$3,$4,'master')`, account, target, account.String(), []byte("fixture"))
	cliMustExec(t, f.pool, `INSERT INTO vault_browser_slots (id,user_id,browser_id,wrapped_dek,created_at,expires_at,token_version,vault_epoch,generation)
		SELECT $1,$2,$3,'\x01',now(),now()+interval '1 hour',0,vault_epoch,0 FROM users WHERE id=$2`,
		uuid.New(), target, uuid.New())

	// No password: 204, sessions ended, vault and secrets wiped, link spent.
	rec := f.redeem(token, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("redeem = %d, want 204\n%s", rec.Code, rec.Body.String())
	}
	if len(rec.Result().Cookies()) != 0 || rec.Header().Get("Set-Cookie") != "" {
		t.Fatal("redeem set a cookie; redemption must not log in")
	}
	if after := f.snapshot(t, target); after.version != before.version+1 || after.hash != nil {
		t.Fatalf("token_version %d -> %d, hash %v; want a bump and no password", before.version, after.version, after.hash)
	}
	if rec := cookieReq(t, f.router, http.MethodGet, "/api/auth/me", oldJWT, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old session = %d, want 401", rec.Code)
	}
	if w, s, mode, marker := f.vaultCounts(t, target); w != 0 || s != 0 || mode != store.VaultModeProtected || !marker {
		t.Fatalf("after redeem wrappers=%d secrets=%d mode=%q marker=%v, want 0, 0, protected, true", w, s, mode, marker)
	}
	poll := cliPostJSON(f.router, "/api/auth/cli/poll", fmt.Sprintf(`{"request_id":%q,"verifier":%q}`, reqApproved, verifier))
	if poll.Code == http.StatusOK {
		t.Fatalf("poll of a request approved before the reset minted a token: %s", poll.Body.String())
	}
	var status string
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM cli_auth_requests WHERE id=$1`, reqApproved).Scan(&status); err != nil || status != "denied" {
		t.Fatalf("approved request status = %q (%v), want denied", status, err)
	}
	var accounts, slots int
	if err := f.pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM codex_provider_account WHERE user_id=$1),
		(SELECT count(*) FROM vault_browser_slots WHERE user_id=$1)`, target).Scan(&accounts, &slots); err != nil {
		t.Fatal(err)
	}
	if accounts != 0 || slots != 0 {
		t.Fatalf("after redeem codex accounts=%d browser slots=%d, want 0 and 0", accounts, slots)
	}
	var redeemed int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM account_reset_links WHERE user_id=$1 AND redeemed_at IS NOT NULL`, target).Scan(&redeemed); err != nil || redeemed != 1 {
		t.Fatalf("redeemed links = %d (%v), want 1", redeemed, err)
	}

	// A second redeem, with or without a password, and a preview are the generic 404.
	for name, got := range map[string]*httptest.ResponseRecorder{
		"second redeem":               f.redeem(token, ""),
		"second redeem with password": f.redeem(token, resetNewPassword),
		"preview":                     f.preview(token),
	} {
		if got.Code != http.StatusNotFound || got.Body.String() != invalid.Body.String() {
			t.Errorf("%s = %d %q, want the generic 404 %q", name, got.Code, got.Body.String(), invalid.Body.String())
		}
	}
}

func TestResetLinkVaultOnlyThenPassphraseEnrollsVaultLiveDB(t *testing.T) {
	f := newResetFixture(t)
	target := f.vaultOnlyTarget(t)
	token := f.issueDTO(t, target).Token
	if rec := f.redeem(token, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("redeem = %d\n%s", rec.Code, rec.Body.String())
	}

	// A fresh cookie JWT is the credential an SSO callback issues after the reset.
	sso := cliMintJWT(t, f.pool, target)
	body := fmt.Sprintf(`{"passphrase":%q}`, resetVaultPassphrase)
	rec := cookieReq(t, f.router, http.MethodPost, "/api/vault/passphrase", sso, body)
	if rec.Code < 200 || rec.Code > 299 {
		t.Fatalf("passphrase after reset = %d, want 2xx\n%s", rec.Code, rec.Body.String())
	}
	if w, _, mode, _ := f.vaultCounts(t, target); w != 1 || mode != store.VaultModeProtected {
		t.Fatalf("after passphrase wrappers=%d mode=%q, want 1 and protected", w, mode)
	}

	// Create-only: a wrapper now exists, so a second call is refused and changes nothing.
	rec = cookieReq(t, f.router, http.MethodPost, "/api/vault/passphrase", sso,
		fmt.Sprintf(`{"passphrase":%q}`, "another-passphrase-123456"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second passphrase = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	// vaultToggleError has several 409 causes; this must be the existing-vault one.
	if !strings.Contains(rec.Body.String(), "a vault already exists") {
		t.Fatalf("second passphrase body = %s, want it to say a vault already exists", rec.Body.String())
	}
	if w, _, _, _ := f.vaultCounts(t, target); w != 1 {
		t.Fatalf("second passphrase left %d wrappers, want 1", w)
	}
}

func TestResetLinkLinkedSSOWithPasswordGetsPasswordKindLiveDB(t *testing.T) {
	f := newResetFixture(t)
	target := f.target(t) // password_hash set
	cliMustExec(t, f.pool, `UPDATE users SET oidc_issuer=$2, oidc_subject=$3 WHERE id=$1`,
		target, "https://idp.example", "sub-"+target.String())

	dto := f.issueDTO(t, target)
	if dto.Kind != "password" {
		t.Fatalf("linked SSO+password kind = %q, want password", dto.Kind)
	}
	f.assertLive(t, dto.Token)

	sso := cliMintJWT(t, f.pool, target)
	rec := cookieReq(t, f.router, http.MethodPost, "/api/vault/passphrase", sso,
		fmt.Sprintf(`{"passphrase":%q}`, resetVaultPassphrase))
	if rec.Code != http.StatusConflict {
		t.Fatalf("passphrase for a password account = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
}
