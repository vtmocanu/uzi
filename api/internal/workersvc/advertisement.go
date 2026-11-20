package workersvc

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Controller capability advertisements (hibernation, admin sizes) share one freshness
// rule: the latest fleet report advertised at least the required version and was received
// by the api less than twice the controller's poll interval ago, measured on the api's
// receipt clock. The poll interval is clamped so a lying or absent interval cannot make a
// stale report look fresh.

const (
	// advertisementPollClampMin/Max bound the controller-reported poll interval, in seconds.
	advertisementPollClampMin = 1
	advertisementPollClampMax = 300
)

// AdvertisementPollInterval is the controller's reported poll interval clamped to
// [1, 300] seconds. An absent interval reads as the maximum.
func AdvertisementPollInterval(pollSeconds pgtype.Int4) time.Duration {
	poll := int64(advertisementPollClampMax)
	if pollSeconds.Valid {
		poll = min(max(int64(pollSeconds.Int32), advertisementPollClampMin), advertisementPollClampMax)
	}
	return time.Duration(poll) * time.Second
}

// AdvertisementFresh reports whether a capability advertisement of at least minVersion was
// received within 2x the clamped poll interval before now.
func AdvertisementFresh(version pgtype.Int4, advertisedAt pgtype.Timestamptz, pollSeconds pgtype.Int4, minVersion int32, now time.Time) bool {
	if !version.Valid || version.Int32 < minVersion || !advertisedAt.Valid {
		return false
	}
	return now.Sub(advertisedAt.Time) < 2*AdvertisementPollInterval(pollSeconds)
}
