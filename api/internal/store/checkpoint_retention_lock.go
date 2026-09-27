package store

import (
	"hash/fnv"

	"github.com/google/uuid"
)

// CheckpointRetentionLockObjID derives the objid half of the CheckpointRetentionLockClass
// advisory lock from a run id. Computed in Go (an FNV-1a hash over all sixteen bytes) and passed
// as an int4 param. A collision only makes one of two unrelated runs' lock tries fail for a
// moment, which retains that run's ref for a later retry: a contention non-event, never a
// correctness one.
func CheckpointRetentionLockObjID(runID uuid.UUID) int32 {
	h := fnv.New32a()
	_, _ = h.Write(runID[:])
	return int32(h.Sum32()) //nolint:gosec // wraparound is fine: a lock key, not a number
}
