package uzicli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// VaultStatusResult is the caller's current vault state.
type VaultStatusResult struct {
	Unlocked bool   `json:"unlocked"`
	Mode     string `json:"mode,omitempty"`
}

// ErrVaultIncorrectPassword marks a trusted wrong-password response.
var ErrVaultIncorrectPassword = errors.New("incorrect password")

// ErrVaultRateLimited marks a trusted HTTP 429 unlock response.
var ErrVaultRateLimited = errors.New("vault unlock rate limited; retry later")

func (c *HTTPClient) VaultStatus(ctx context.Context) (VaultStatusResult, error) {
	var result VaultStatusResult
	err := c.get(ctx, "/api/vault/status", &result)
	return result, err
}

// VaultUnlock makes one attempt; error bodies are never surfaced to the caller.
func (c *HTTPClient) VaultUnlock(ctx context.Context, password string) error {
	resp, _, err := c.doJSONRead(ctx, http.MethodPost, "/api/vault/unlock", struct {
		Password string `json:"password"`
	}{Password: password})
	if err != nil {
		return SafeVaultUnlockError(err)
	}
	if resp.StatusCode/100 == 2 {
		return nil
	}
	if resp.StatusCode == http.StatusForbidden {
		return SafeVaultUnlockError(ErrVaultIncorrectPassword)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return SafeVaultUnlockError(&ExitError{Code: ExitUnreachable, Err: ErrVaultRateLimited, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))})
	}
	return SafeVaultUnlockError(buildStatusError(resp.StatusCode, "", "", ""))
}

// SafeVaultUnlockError discards arbitrary error text and metadata, including wrapped
// errors. Only the trusted password marker, a valid exit code and RetryAfter survive.
func SafeVaultUnlockError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrVaultIncorrectPassword) {
		return &ExitError{Code: ExitAuth, Err: ErrVaultIncorrectPassword}
	}
	safe := Exitf(ExitGeneric, "vault unlock failed")
	var ee *ExitError
	if errors.As(err, &ee) && ee != nil {
		if ee.Code >= ExitGeneric && ee.Code <= ExitHealthDanger {
			safe.Code = ee.Code
		}
		if ee.RetryAfter > 0 {
			safe.RetryAfter = ee.RetryAfter
		}
	}
	if errors.Is(err, ErrVaultRateLimited) {
		safe.Code = ExitUnreachable
		safe.Err = ErrVaultRateLimited
		if safe.RetryAfter > 0 {
			safe.Err = fmt.Errorf("%w (retry after %s)", ErrVaultRateLimited, safe.RetryAfter)
		}
	}
	if safe.Code == ExitAuth {
		safe.Err = errors.New("vault unlock authentication failed")
	}
	return safe
}
