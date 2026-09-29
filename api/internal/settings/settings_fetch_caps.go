package settings

// This file holds the fetch caps for official-sources research runs (PRD #1906 Open
// question 1): the accessor the api's per-run fetch admission reads (a later milestone)
// and the write-time validator. Every cap is a positive integer with a per-key ceiling:
// zero would make every fetch fail, and there is no "unlimited" value, because the caps
// are what bound a run's downloads.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// fetchCapBounds is the accepted [min, max] per fetch-cap key. The ceilings are sanity
// bounds against a fat-fingered value, well above the defaults (25 MiB, 200 MiB, 100
// files, 4 concurrent).
var fetchCapBounds = map[string][2]int64{
	KeyFetchMaxFileBytes:     {1, 1 << 30},  // 1 GiB
	KeyFetchMaxRunBytes:      {1, 10 << 30}, // 10 GiB
	KeyFetchMaxRunFiles:      {1, 10000},
	KeyFetchMaxConcurrentRun: {1, 32},
}

// FetchCaps is the effective set of fetch caps.
type FetchCaps struct {
	// MaxFileBytes is the largest single download, counted in decoded bytes.
	MaxFileBytes int64
	// MaxRunBytes is the total a run may download.
	MaxRunBytes int64
	// MaxRunFiles is how many downloads a run may make.
	MaxRunFiles int64
	// MaxConcurrentPerRun is how many fetches one run may have in flight.
	MaxConcurrentPerRun int64
}

// FetchCaps returns the effective fetch caps. A stored value that does not parse or is
// outside its bounds (written around the validator) reads as the compiled default, so a
// bad row can never turn a cap off. A cold read error is returned beside the defaults.
func (c *Cache) FetchCaps(ctx context.Context) (FetchCaps, error) {
	var firstErr error
	get := func(key string) int64 {
		v, err := c.get(ctx, key)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if n, ok := parseFetchCap(key, v); ok {
			return n
		}
		n, _ := parseFetchCap(key, Defaults[key])
		return n
	}
	caps := FetchCaps{
		MaxFileBytes:        get(KeyFetchMaxFileBytes),
		MaxRunBytes:         get(KeyFetchMaxRunBytes),
		MaxRunFiles:         get(KeyFetchMaxRunFiles),
		MaxConcurrentPerRun: get(KeyFetchMaxConcurrentRun),
	}
	return caps, firstErr
}

// parseFetchCap parses value as a base-10 integer inside key's bounds.
func parseFetchCap(key, value string) (int64, bool) {
	b, ok := fetchCapBounds[key]
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || n < b[0] || n > b[1] {
		return 0, false
	}
	return n, true
}

// validateFetchCap is the write-time gate for a fetch cap: a base-10 integer within the
// key's [min, max].
func validateFetchCap(key, value string) error {
	b, ok := fetchCapBounds[key]
	if !ok {
		return errors.New("unknown fetch cap")
	}
	if _, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err != nil {
		return errors.New("must be a whole number")
	}
	if _, ok := parseFetchCap(key, value); !ok {
		return fmt.Errorf("must be between %d and %d", b[0], b[1])
	}
	return nil
}
