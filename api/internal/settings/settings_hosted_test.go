package settings

import (
	"context"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The hosted-worker quota (PRD #58 Decision 8).

func TestValidateHostedWorkerQuota(t *testing.T) {
	ok := []string{"0", "1", "2", "20", " 3 "}
	for _, v := range ok {
		if err := Validate(KeyHostedWorkerQuota, v); err != nil {
			t.Errorf("Validate(hosted_worker_quota, %q) = %v, want nil", v, err)
		}
	}
	// {0} ∪ [1, 20]: reject negatives, over-cap, and non-integers.
	bad := []string{"-1", "21", "100", "2.5", "", " ", "abc", "1e3", "two"}
	for _, v := range bad {
		if err := Validate(KeyHostedWorkerQuota, v); err == nil {
			t.Errorf("Validate(hosted_worker_quota, %q) = nil, want a rejection", v)
		}
	}
}

// The regression this key most plausibly suffers, pinned explicitly rather than
// left implied by the table above.
//
// Validate's switch ends in a default that falls through to ValidateLabel, which
// accepts any non-empty ≤64-char string. A key that is in Defaults but missing its
// case therefore accepts "abc" at write time, and intSetting then silently reverts
// to the compiled-in default on every read — so an admin who typed 0 to disable
// self-service would be told it saved and would still be provisioning at 2. The
// failure is invisible from both ends: the write says OK, the read says 2.
//
// If someone deletes the KeyHostedWorkerQuota case from Validate, this test is what
// fails.
func TestValidateHostedWorkerQuotaDoesNotFallThroughToLabelRules(t *testing.T) {
	// A perfectly good LABEL, and a nonsense quota.
	if err := Validate(KeyHostedWorkerQuota, "PRD"); err == nil {
		t.Fatal("Validate(hosted_worker_quota, \"PRD\") = nil — the key fell through to " +
			"ValidateLabel, so a non-integer quota would be accepted and then silently " +
			"read back as the compiled-in default")
	}
}

func TestHostedWorkerQuotaAccessor(t *testing.T) {
	ctx := context.Background()

	c := New(&fakeStore{rows: []store.AppSetting{row(KeyHostedWorkerQuota, "5")}}, time.Minute)
	if got, _ := c.HostedWorkerQuota(ctx); got != 5 {
		t.Errorf("HostedWorkerQuota = %d, want 5", got)
	}

	// 0 is a real value (self-service disabled), not an absent row.
	c = New(&fakeStore{rows: []store.AppSetting{row(KeyHostedWorkerQuota, "0")}}, time.Minute)
	if got, _ := c.HostedWorkerQuota(ctx); got != 0 {
		t.Errorf("HostedWorkerQuota = %d, want 0 (disabled)", got)
	}
}

func TestHostedWorkerQuotaFallsBackToDefault(t *testing.T) {
	c := New(&fakeStore{}, time.Minute)
	if got, _ := c.HostedWorkerQuota(context.Background()); got != 2 {
		t.Errorf("HostedWorkerQuota default = %d, want 2 (Decision 8)", got)
	}
}

// Junk-tolerance on READ mirrors the other int accessors: a hand-edited row or one
// predating validation must not break provisioning. Validate is what stops junk
// getting in through the API; this is the backstop for what Validate cannot reach.
func TestHostedWorkerQuotaIsJunkTolerantOnRead(t *testing.T) {
	c := New(&fakeStore{rows: []store.AppSetting{row(KeyHostedWorkerQuota, "banana")}}, time.Minute)
	if got, _ := c.HostedWorkerQuota(context.Background()); got != 2 {
		t.Errorf("HostedWorkerQuota(banana) = %d, want the compiled-in default 2", got)
	}
}

func TestHostedWorkerQuotaKeyKnownAndInDefaults(t *testing.T) {
	if !Known(KeyHostedWorkerQuota) {
		t.Error("Known(hosted_worker_quota) = false — the admin settings PUT would reject it")
	}
	if _, ok := Defaults[KeyHostedWorkerQuota]; !ok {
		t.Error("Defaults[hosted_worker_quota] missing — All/AdminView would not surface it")
	}
	// Not a secret: the quota is operator-set policy the admin page renders.
	if IsSecret(KeyHostedWorkerQuota) {
		t.Error("IsSecret(hosted_worker_quota) = true, want false")
	}
}
