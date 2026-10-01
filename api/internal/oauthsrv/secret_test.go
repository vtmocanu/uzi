package oauthsrv

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestGenerateSecret(t *testing.T) {
	secret, hash, prefix, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, SecretPrefix) || SecretPrefix != "uzs_" {
		t.Fatalf("secret %q lacks prefix %q", secret, SecretPrefix)
	}
	body := strings.TrimPrefix(secret, SecretPrefix)
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(raw) != 32 {
		t.Fatalf("body is not 256 bits of base64url: len=%d err=%v", len(raw), err)
	}
	if !bytes.Equal(hash, HashSecret(secret)) || len(hash) != 32 {
		t.Fatal("hash is not sha256 of the full secret")
	}
	if prefix != SecretPrefix+body[:4] {
		t.Fatalf("prefix = %q", prefix)
	}
	other, _, _, _ := GenerateSecret()
	if other == secret {
		t.Fatal("two secrets collided")
	}
}

func TestGenerateRefreshToken(t *testing.T) {
	tok, hash, prefix, err := GenerateRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, RefreshPrefix) || RefreshPrefix != "uzr_" {
		t.Fatalf("token %q lacks prefix %q", tok, RefreshPrefix)
	}
	body := strings.TrimPrefix(tok, RefreshPrefix)
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(raw) != 32 {
		t.Fatalf("body is not 256 bits of base64url: len=%d err=%v", len(raw), err)
	}
	if !bytes.Equal(hash, HashSecret(tok)) {
		t.Fatal("hash is not sha256 of the full token")
	}
	if prefix != RefreshPrefix+body[:4] {
		t.Fatalf("prefix = %q", prefix)
	}
	other, _, _, _ := GenerateRefreshToken()
	if other == tok {
		t.Fatal("two refresh tokens collided")
	}
}
