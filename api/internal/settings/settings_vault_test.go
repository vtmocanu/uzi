package settings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestVaultPolicyRegistryAndValidation(t *testing.T) {
	if !Known(KeyVaultPolicy) || IsSecret(KeyVaultPolicy) || Defaults[KeyVaultPolicy] != "required" {
		t.Fatal("vault_policy must be a known non-secret setting defaulting to required")
	}
	for _, value := range []string{"optional", "required"} {
		if err := Validate(KeyVaultPolicy, value); err != nil {
			t.Fatalf("Validate(%q): %v", value, err)
		}
	}
	for _, value := range []string{"", "OPTIONAL", " optional", "optional ", "true", "junk"} {
		if err := Validate(KeyVaultPolicy, value); err == nil {
			t.Errorf("Validate(%q) accepted invalid policy", value)
		}
	}
	all, err := New(&fakeStore{}, time.Minute).All(context.Background())
	if err != nil || all[KeyVaultPolicy] != "required" {
		t.Fatalf("All policy = %q, %v", all[KeyVaultPolicy], err)
	}
}

func TestVaultPolicyFailClosed(t *testing.T) {
	for _, value := range []string{"optional", "required", "", "OPTIONAL", "optional ", "junk"} {
		t.Run(value, func(t *testing.T) {
			want := "required"
			if value == "optional" {
				want = "optional"
			}
			c := New(&fakeStore{rows: []store.AppSetting{row(KeyVaultPolicy, value)}}, time.Minute)
			if got, err := c.VaultPolicy(context.Background()); got != want || err != nil {
				t.Fatalf("VaultPolicy = %q, %v; want %q", got, err, want)
			}
		})
	}
	c := New(&fakeStore{}, time.Minute)
	// Even an arbitrary overlay cannot make this DB-only accessor permissive.
	c.ConfigureSecrets(nil, map[string]string{KeyVaultPolicy: "optional"})
	if got, err := c.VaultPolicy(context.Background()); got != "required" || err != nil {
		t.Fatalf("missing policy = %q, %v", got, err)
	}
}

func TestVaultPolicyReadFailure(t *testing.T) {
	ctx := context.Background()
	failure := errors.New("read failed")
	fs := &fakeStore{err: failure}
	c := New(fs, time.Minute)
	if got, err := c.VaultPolicy(ctx); got != "required" || !errors.Is(err, failure) {
		t.Fatalf("cold failure = %q, %v", got, err)
	}
	fs.err = nil
	fs.rows = []store.AppSetting{row(KeyVaultPolicy, "optional")}
	now := time.Now()
	c.now = func() time.Time { return now }
	if got, err := c.VaultPolicy(ctx); got != "optional" || err != nil {
		t.Fatalf("initial policy = %q, %v", got, err)
	}
	now = now.Add(2 * time.Minute)
	fs.err = failure
	if got, err := c.VaultPolicy(ctx); got != "required" || !errors.Is(err, failure) {
		t.Fatalf("expired optional on failure = %q, %v", got, err)
	}
}
