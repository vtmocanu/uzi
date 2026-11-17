package settings

// This file holds the ephemeral/hosted worker settings of PRD #4212: their
// accessors, the lease bound and the write-time validators.
//
// Failure model shared by every accessor here: an error is returned only when the
// cache holds no valid snapshot and the refresh fails (the same cold-cache blip
// HostedWorkerQuota documents); the returned value is then the compiled-in default.
// A key set through the ENV overlay never errors, because get answers it without
// touching the store. A stored value outside what an accessor accepts (each
// accessor documents its own fallback rule) is not an error: it falls back to the
// default, since Validate cannot reach a hand-edited row. The cap accessor accepts
// any non-negative integer, because an env value above the admin bound is honoured.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/workersize"
)

// EphemeralLeaseMax is the longest ephemeral worker lease an admin may store. It
// equals config.MaxEphemeralLease; settings must not import config, so a test in
// the config package pins the two together.
const EphemeralLeaseMax = 2 * time.Hour

// EphemeralMaxPerUser returns the per-user cap on concurrent ephemeral workers; 0
// means no ephemeral provisioning for anyone. An unparseable or negative stored value
// falls back to the default. Errors only when the cache has no valid snapshot and the
// refresh fails; an env-overlaid value never errors.
func (c *Cache) EphemeralMaxPerUser(ctx context.Context) (int, error) {
	n, err := c.intSetting(ctx, KeyEphemeralMaxPerUser)
	if n < 0 {
		n, _ = strconv.Atoi(DefaultEphemeralMaxPerUser)
	}
	return n, err
}

// EphemeralDefaultSize returns the workersize preset ephemeral workers are
// provisioned at. A stored value that is not a known preset falls back to the
// default. Errors only when the cache has no valid snapshot and the refresh fails;
// an env-overlaid value never errors.
func (c *Cache) EphemeralDefaultSize(ctx context.Context) (string, error) {
	return c.sizeSetting(ctx, KeyEphemeralDefaultSize)
}

// HostedDefaultSize returns the workersize preset the persistent provision form
// preselects (the provision API still requires an explicit size). A stored value that is not a known
// preset falls back to the default. Errors only when the cache has no valid snapshot
// and the refresh fails.
func (c *Cache) HostedDefaultSize(ctx context.Context) (string, error) {
	return c.sizeSetting(ctx, KeyHostedDefaultSize)
}

func (c *Cache) sizeSetting(ctx context.Context, key string) (string, error) {
	v, err := c.get(ctx, key)
	if !workersize.Valid(v) {
		v = Defaults[key]
	}
	return v, err
}

// EphemeralLease returns how long an ephemeral worker lingers after its run
// completes; 0 means no lease. An unparseable, negative or over-EphemeralLeaseMax
// stored value falls back to the default (2h). Errors only when the cache has no
// valid snapshot and the refresh fails; an env-overlaid value never errors.
func (c *Cache) EphemeralLease(ctx context.Context) (time.Duration, error) {
	v, err := c.get(ctx, KeyEphemeralLease)
	d, perr := time.ParseDuration(strings.TrimSpace(v))
	if perr != nil || d < 0 || d > EphemeralLeaseMax {
		d, _ = time.ParseDuration(DefaultEphemeralLease)
	}
	return d, err
}

// FormatEphemeralLease renders a lease as the compact canonical string stored in the
// setting ("30m", "2h", "1h30m", "0s"), which time.ParseDuration reads back to the
// same value.
func FormatEphemeralLease(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// validateEphemeralMaxPerUser is the write-time gate for ephemeral_max_per_user: a
// base-10 integer in [0, maxHostedWorkerQuota]; "" is the reset to the default.
func validateEphemeralMaxPerUser(value string) error {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > maxHostedWorkerQuota {
		return fmt.Errorf("must be a whole number between 0 and %d", maxHostedWorkerQuota)
	}
	return nil
}

// validateWorkerSize is the write-time gate for the two default-size keys: a
// workersize preset name; "" is the reset to the default.
func validateWorkerSize(value string) error {
	v := strings.TrimSpace(value)
	if v == "" || workersize.Valid(v) {
		return nil
	}
	return fmt.Errorf("must be one of: %s", strings.Join(workersize.Names, ", "))
}

// validateEphemeralLease is the write-time gate for ephemeral_lease: a Go duration in
// [0, EphemeralLeaseMax]; "" is the reset to the default.
func validateEphemeralLease(value string) error {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 || d > EphemeralLeaseMax {
		return errors.New(`must be a duration between 0s and 2h, like "30m"`)
	}
	return nil
}
