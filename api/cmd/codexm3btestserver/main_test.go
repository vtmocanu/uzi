package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestFakeChatGPTAccessTokenCarriesPinnedExternalAuthClaims(t *testing.T) {
	const accountID = "canary-account-test"
	token, err := fakeChatGPTAccessToken(accountID, "token-a")
	if err != nil {
		t.Fatalf("fakeChatGPTAccessToken: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		t.Fatalf("token has %d non-empty JWT segments, want 3", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims struct {
		TokenID string `json:"jti"`
		Auth    struct {
			PlanType string `json:"chatgpt_plan_type"`
			Account  string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.TokenID != "canary-token-a" || claims.Auth.PlanType != "pro" || claims.Auth.Account != accountID {
		t.Fatalf("unexpected external-auth claims: %+v", claims)
	}

	rotated, err := fakeChatGPTAccessToken(accountID, "token-b")
	if err != nil {
		t.Fatalf("rotated fakeChatGPTAccessToken: %v", err)
	}
	if rotated == token {
		t.Fatal("a rotated fake access token must differ from the initial token")
	}
}
