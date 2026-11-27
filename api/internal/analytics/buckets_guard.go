package analytics

import (
	"errors"
	"fmt"
)

// ErrBucketTiling is wrapped by every ValidateBuckets failure.
var ErrBucketTiling = errors.New("analytics: buckets do not tile the range")

// ValidateBuckets is the guard a sweep-based statement (issue #4731 Capacity) applies to the
// Go-computed calendar before trusting it: the SQL integrates each segment into the bucket
// that contains its start, so the buckets must tile [From, Now) exactly.
//
// Zero buckets are valid only for an empty range (From == Now). Otherwise the buckets must be
// contiguous (end[i] == start[i+1]), start at From and end at Now, with strictly increasing
// starts and non-decreasing ends. Every bucket must have start < end except the final one,
// which may be the zero-duration [Now, Now) bucket a local-midnight now produces.
func ValidateBuckets(b Bounds) error {
	n := len(b.BucketStarts)
	if n != len(b.BucketEnds) {
		return fmt.Errorf("%w: %d starts, %d ends", ErrBucketTiling, n, len(b.BucketEnds))
	}
	if n == 0 {
		if !b.From.Equal(b.Now) {
			return fmt.Errorf("%w: no buckets for a non-empty range", ErrBucketTiling)
		}
		return nil
	}
	if !b.BucketStarts[0].Equal(b.From) {
		return fmt.Errorf("%w: first bucket starts at %s, range at %s", ErrBucketTiling, b.BucketStarts[0], b.From)
	}
	if !b.BucketEnds[n-1].Equal(b.Now) {
		return fmt.Errorf("%w: last bucket ends at %s, range at %s", ErrBucketTiling, b.BucketEnds[n-1], b.Now)
	}
	for i := 0; i < n; i++ {
		s, e := b.BucketStarts[i], b.BucketEnds[i]
		if e.Before(s) {
			return fmt.Errorf("%w: bucket %d ends before it starts", ErrBucketTiling, i)
		}
		if i < n-1 && !s.Before(e) {
			return fmt.Errorf("%w: bucket %d is empty before the final bucket", ErrBucketTiling, i)
		}
		if i > 0 {
			if !b.BucketStarts[i-1].Before(s) {
				return fmt.Errorf("%w: bucket %d start is not after bucket %d start", ErrBucketTiling, i, i-1)
			}
			if b.BucketEnds[i-1].After(e) {
				return fmt.Errorf("%w: bucket %d end precedes bucket %d end", ErrBucketTiling, i, i-1)
			}
			if !b.BucketEnds[i-1].Equal(s) {
				return fmt.Errorf("%w: gap or overlap between buckets %d and %d", ErrBucketTiling, i-1, i)
			}
		}
	}
	return nil
}
