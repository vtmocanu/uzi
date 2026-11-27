package store

import "github.com/jackc/pgx/v5/pgtype"

// CopyAnthropicUsageSampleParamsFrom maps the exact parameters of an accepted
// UpsertRateLimits write onto the history copy that fences against that same reading
// (issue #4692). It is the single mapping every accepted-write path (the usage poller
// and the test-connection handler) shares.
func CopyAnthropicUsageSampleParamsFrom(p UpsertRateLimitsParams) CopyAnthropicUsageSampleParams {
	return CopyAnthropicUsageSampleParams{
		UserSecretID:     p.UserSecretID,
		UserID:           p.UserID,
		EnablementRev:    p.EnablementRev,
		SyncedAt:         p.SyncedAt,
		Source:           p.Source,
		FiveHourPct:      p.FiveHourPct,
		FiveHourResetsAt: p.FiveHourResetsAt,
		SevenDayPct:      p.SevenDayPct,
		SevenDayResetsAt: p.SevenDayResetsAt,
	}
}

// CopyCodexUsageSampleParamsFrom maps the exact parameters of an accepted
// UpsertCodexAccountRateLimits write, plus the last_success_at that write returned, onto
// the history copy that fences against that same reading (issue #4692). The buckets bytes
// are passed through unchanged.
func CopyCodexUsageSampleParamsFrom(p UpsertCodexAccountRateLimitsParams, lastSuccess pgtype.Timestamptz) CopyCodexUsageSampleParams {
	return CopyCodexUsageSampleParams{
		ObservedGeneration:         p.ObservedGeneration,
		ObservedCredentialRevision: p.ObservedCredentialRevision,
		UserID:                     p.UserID,
		ProviderAccountID:          p.ProviderAccountID,
		EnablementSig:              p.EnablementSig,
		Buckets:                    p.Buckets,
		LastSuccessAt:              lastSuccess,
	}
}
