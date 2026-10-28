package codexusagepoller

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
)

// codexEarlyReset compares only the uniquely identified main weekly meter.
func codexEarlyReset(prior store.GetCodexEarlyResetBaselineRow, next []apitypes.CodexRateLimitBucketDTO, now time.Time) (time.Time, bool) {
	if !prior.LastSuccessAt.Valid || prior.LastSuccessAt.InfinityModifier != 0 || prior.LastSuccessAt.Time.IsZero() {
		return time.Time{}, false
	}
	var buckets []apitypes.CodexRateLimitBucketDTO
	if err := json.Unmarshal(prior.Buckets, &buckets); err != nil || buckets == nil {
		return time.Time{}, false
	}
	// JSON null silently zeroes non-pointer fields. Reject malformed identities
	// rather than mistaking a null display name for the normalized main marker.
	var identities []struct {
		ID          string          `json:"id"`
		DisplayName json.RawMessage `json:"display_name"`
	}
	if err := json.Unmarshal(prior.Buckets, &identities); err != nil {
		return time.Time{}, false
	}
	for _, identity := range identities {
		if identity.ID == "" || bytes.Equal(bytes.TrimSpace(identity.DisplayName), []byte("null")) {
			return time.Time{}, false
		}
	}
	old, fresh := mainWeeklyWindow(buckets), mainWeeklyWindow(next)
	if old == nil || fresh == nil || !validReset(old.ResetAt) || !validReset(fresh.ResetAt) {
		return time.Time{}, false
	}
	expected := time.Unix(*old.ResetAt, 0).UTC()
	if !expected.After(prior.LastSuccessAt.Time) || !now.Before(expected.Add(-8*time.Hour)) {
		return time.Time{}, false
	}
	observed := usagereserve.CodexResetObserved(*old.ResetAt, *fresh.ResetAt, old.UsedPercent, fresh.UsedPercent)
	return expected, observed
}

func mainWeeklyWindow(buckets []apitypes.CodexRateLimitBucketDTO) *apitypes.CodexRateLimitWindowDTO {
	var main *apitypes.CodexRateLimitBucketDTO
	count := 0
	for i := range buckets {
		if buckets[i].ID == "codex" {
			count++
			main = &buckets[i]
		}
	}
	if count != 1 || main.DisplayName != "" {
		return nil
	}
	var weekly *apitypes.CodexRateLimitWindowDTO
	count = 0
	for _, w := range []*apitypes.CodexRateLimitWindowDTO{main.Primary, main.Secondary} {
		if w != nil && w.LimitWindowSeconds != nil && *w.LimitWindowSeconds == 604800 {
			count++
			weekly = w
		}
	}
	if count != 1 {
		return nil
	}
	return weekly
}

func validReset(reset *int64) bool {
	return reset != nil && *reset > 0 && *reset <= 253402300799
}

func baselineMatches(prior store.GetCodexEarlyResetBaselineRow, t pollTarget) bool {
	return prior.UserID == t.userID && prior.ProviderAccountID == t.accountID &&
		prior.NotifyEarlyLimitReset && prior.EnablementSig.Valid &&
		strings.TrimSpace(prior.EnablementSig.String) != "" && prior.EnablementSig.String == t.enablementSig
}
