package forgesvc

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

func githubAppStorageBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// This is an opaque storage canary, not a usable App key. Driver verification is
// deliberately injected so no network call or RSA parsing can mask storage bugs.
const githubAppStorageDocument = `{"auth_type":"github_app","app_id":"42","installation_id":73,"private_key":"opaque-App-storage-canary","repositories":["aifactory"]}`

func TestGitHubAppConnectionStorageCannotBeOpenedByLegacyAPI(t *testing.T) {
	box := githubAppStorageBox(t)
	service := New(nil, box, time.Second, nil)
	sealed, err := service.EncryptToken(githubAppStorageDocument)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(githubAppStorageDocument)) {
		t.Fatal("App credential was stored without encryption")
	}
	if plain, err := box.Open(sealed); err == nil || len(plain) != 0 {
		t.Fatal("legacy API could decrypt versioned App key envelope")
	}
	// Removing the visible version discriminator must not make the ciphertext
	// legacy-readable: the authenticated encryption domain must differ as well.
	const prefix = "uzi-ghapp-v1:"
	if !bytes.HasPrefix(sealed, []byte(prefix)) {
		t.Fatal("App envelope lacks its version discriminator")
	}
	if plain, err := box.Open(sealed[len(prefix):]); err == nil || len(plain) != 0 {
		t.Fatal("removing version prefix exposed App key to legacy API")
	}
}

func TestForgeForConnectionOpensAppDomainAndPreservesLegacyPAT(t *testing.T) {
	for _, credential := range []string{githubAppStorageDocument, "classic-storage-secret-canary"} {
		name := "App"
		if credential != githubAppStorageDocument {
			name = "PAT"
		}
		t.Run(name, func(t *testing.T) {
			box := githubAppStorageBox(t)
			client := &forgetest.BaseFake{}
			calls := 0
			service := NewWithForgeBuilder(nil, box, 3*time.Second, nil,
				func(kind forge.Type, baseURL, actual string, timeout time.Duration) (forge.Forge, error) {
					calls++
					if kind != forge.TypeGitHub || baseURL != "https://github.com" || timeout != 3*time.Second {
						t.Error("connection builder parameters changed")
					}
					if actual != credential {
						t.Error("connection builder did not receive the exact decrypted credential")
					}
					return client, nil
				})
			sealed, err := service.EncryptToken(credential)
			if err != nil {
				t.Fatal(err)
			}
			if name == "PAT" {
				plain, err := box.Open(sealed)
				if err != nil || string(plain) != credential {
					t.Fatal("PAT storage is no longer legacy-readable")
				}
			}
			got, err := service.ForgeForConnection("github", "https://github.com", sealed)
			if err != nil || got != client || calls != 1 {
				t.Fatalf("connection dispatch failed: calls=%d err=%v", calls, err)
			}
			scrub, err := service.CIArtifactScrubber(sealed)
			if err != nil {
				t.Fatal(err)
			}
			if got := scrub("before " + credential + " after"); got != "before [REDACTED] after" {
				t.Fatal("artifact scrubber retained domain-separated connection credential")
			}
			if calls != 1 {
				t.Fatal("evidence scrubbing invoked the forge client builder")
			}
		})
	}
}

func TestGitHubAppConnectionRejectsUnversionedAndDamagedEnvelope(t *testing.T) {
	box := githubAppStorageBox(t)
	calls := 0
	service := NewWithForgeBuilder(nil, box, time.Second, nil,
		func(forge.Type, string, string, time.Duration) (forge.Forge, error) {
			calls++
			return &forgetest.BaseFake{}, nil
		})
	old, err := box.Seal([]byte(githubAppStorageDocument))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := service.EncryptToken(githubAppStorageDocument)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), sealed...)
	corrupt[len(corrupt)-1] ^= 1
	for name, ciphertext := range map[string][]byte{"unversioned-App": old, "tampered-App": corrupt} {
		t.Run(name, func(t *testing.T) {
			client, err := service.ForgeForConnection("github", "https://github.com", ciphertext)
			if err == nil || client != nil || calls != 0 {
				t.Fatal("invalid App envelope reached a forge client")
			}
			if strings.Contains(err.Error(), githubAppStorageDocument) || strings.Contains(err.Error(), "opaque-App-storage-canary") {
				t.Fatal("connection storage error disclosed App key material")
			}
			scrub, err := service.CIArtifactScrubber(ciphertext)
			if err == nil || scrub != nil {
				t.Fatal("invalid App envelope yielded a fallback artifact scrubber")
			}
		})
	}
}
