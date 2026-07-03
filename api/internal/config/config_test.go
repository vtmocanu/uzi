package config

import "testing"

func TestValidateSecretRejectsUnsafe(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"change-me",
		"CHANGEME",
		"uzi-dev-secret-change-in-production",
		"short",
	}
	for _, s := range bad {
		if _, err := validateSecret(s); err == nil {
			t.Errorf("validateSecret(%q) = nil error, want rejection", s)
		}
	}
}

func TestValidateSecretAcceptsGood(t *testing.T) {
	good := "0f9a1c3e5b7d9f1a3c5e7b9d1f3a5c7e" // 32 hex chars
	if _, err := validateSecret(good); err != nil {
		t.Errorf("validateSecret rejected a good secret: %v", err)
	}
}

func TestOriginIsHTTPS(t *testing.T) {
	cases := map[string]bool{
		"https://uzi.example.com": true,
		"http://127.0.0.1:8080":   false,
		"":                        false,
		"not a url":               false,
	}
	for origin, want := range cases {
		if got := originIsHTTPS(origin); got != want {
			t.Errorf("originIsHTTPS(%q) = %v, want %v", origin, got, want)
		}
	}
}
