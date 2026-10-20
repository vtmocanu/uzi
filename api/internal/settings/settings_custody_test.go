package settings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCustodyHoldLimitDefaultPinned pins #3150's acceptance: unset behaves exactly as the
// former compile-time constant (8).
func TestCustodyHoldLimitDefaultPinned(t *testing.T) {
	if Defaults[KeyCustodyHoldLimit] != "8" || DefaultCustodyHoldLimit != "8" {
		t.Fatalf("custody_hold_limit default = %q, want \"8\"", Defaults[KeyCustodyHoldLimit])
	}
	n, err := New(&fakeStore{}, time.Minute).CustodyHoldLimit(context.Background())
	if err != nil || n != 8 {
		t.Fatalf("unset CustodyHoldLimit = %d, %v; want 8, nil", n, err)
	}
}

// TestValidateCustodyHoldLimit routes through the public Validate, so it also proves the key
// is wired to validateCustodyHoldLimit rather than the permissive label default.
func TestValidateCustodyHoldLimit(t *testing.T) {
	for _, v := range []string{"1", "8", "12", "1000", " 12 "} {
		if err := Validate(KeyCustodyHoldLimit, v); err != nil {
			t.Errorf("Validate(custody_hold_limit, %q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"0", "-1", "abc", "8.5", "+12", "1001", "", "1e3", "2147483648"} {
		err := Validate(KeyCustodyHoldLimit, v)
		if err == nil {
			t.Errorf("Validate(custody_hold_limit, %q) = nil, want a rejection", v)
			continue
		}
		if err.Error() != "must be a whole number between 1 and 1000" {
			t.Errorf("Validate(custody_hold_limit, %q) message = %q", v, err)
		}
	}
}

// TestCustodyHoldLimitStoredValues: a valid row is honored (above and below the default,
// trimmed), an empty row is the documented unset case, and an invalid row is an error —
// never the default and never zero.
func TestCustodyHoldLimitStoredValues(t *testing.T) {
	for _, tc := range []struct {
		stored string
		want   int32
	}{{"4", 4}, {"12", 12}, {" 12 ", 12}, {"1000", 1000}, {"", 8}} {
		c := New(&fakeStore{rows: []store.AppSetting{row(KeyCustodyHoldLimit, tc.stored)}}, time.Minute)
		n, err := c.CustodyHoldLimit(context.Background())
		if err != nil || n != tc.want {
			t.Errorf("stored %q: got %d, %v; want %d, nil", tc.stored, n, err, tc.want)
		}
	}
	for _, stored := range []string{"0", "-3", "abc", "5000", "+12", "8.5"} {
		c := New(&fakeStore{rows: []store.AppSetting{row(KeyCustodyHoldLimit, stored)}}, time.Minute)
		n, err := c.CustodyHoldLimit(context.Background())
		if err == nil || !errors.Is(err, ErrCustodyHoldLimitUnavailable) || n != 0 {
			t.Errorf("stored %q: got %d, %v; want 0 and ErrCustodyHoldLimitUnavailable", stored, n, err)
		}
	}
}

// TestCustodyHoldLimitReadFailure covers the fail-closed read policy: a cold read error is an
// error (never 8), a failed refresh over a valid snapshot serves the cached configured value
// (4, never the default), and a failed refresh right after Invalidate is an error.
func TestCustodyHoldLimitReadFailure(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("db down")

	cold := New(&fakeStore{err: boom}, time.Minute)
	if n, err := cold.CustodyHoldLimit(ctx); err == nil || !errors.Is(err, ErrCustodyHoldLimitUnavailable) || n != 0 {
		t.Fatalf("cold read error: got %d, %v; want 0 and an error", n, err)
	}

	fs := &fakeStore{rows: []store.AppSetting{row(KeyCustodyHoldLimit, "4")}}
	c := New(fs, time.Minute)
	now := time.Now()
	c.now = func() time.Time { return now }
	if n, err := c.CustodyHoldLimit(ctx); err != nil || n != 4 {
		t.Fatalf("warm read: got %d, %v; want 4", n, err)
	}
	fs.err = boom
	now = now.Add(2 * time.Minute) // expire the TTL so the next read refreshes and fails
	if n, err := c.CustodyHoldLimit(ctx); err != nil || n != 4 {
		t.Fatalf("stale-on-error: got %d, %v; want the cached 4", n, err)
	}
	c.Invalidate()
	if n, err := c.CustodyHoldLimit(ctx); !errors.Is(err, ErrCustodyHoldLimitUnavailable) || n != 0 {
		t.Fatalf("failed refresh after Invalidate: got %d, %v; want 0 and an error", n, err)
	}
}

// TestCustodyHoldLimitNilCache: a nil cache (an unwired reader behind an interface) is an
// error, not a panic and not a default.
func TestCustodyHoldLimitNilCache(t *testing.T) {
	var c *Cache
	if n, err := c.CustodyHoldLimit(context.Background()); !errors.Is(err, ErrCustodyHoldLimitUnavailable) || n != 0 {
		t.Fatalf("nil cache: got %d, %v; want 0 and ErrCustodyHoldLimitUnavailable", n, err)
	}
}

// TestCustodyHoldLimitEnvPrecedence pins the AdminView precedence (ENV over DB row).
func TestCustodyHoldLimitEnvPrecedence(t *testing.T) {
	c := New(&fakeStore{rows: []store.AppSetting{row(KeyCustodyHoldLimit, "12")}}, time.Minute)
	c.ConfigureSecrets(nil, map[string]string{KeyCustodyHoldLimit: "3"})
	if n, err := c.CustodyHoldLimit(context.Background()); err != nil || n != 3 {
		t.Fatalf("env override: got %d, %v; want 3", n, err)
	}
}
