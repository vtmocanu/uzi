package store

import (
	"context"
	"encoding/binary"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// storedFilesSharedObjID is the objid of the shared-budget key, the second half of the pair
// LockStoredFiles takes. A fixed ASCII constant ("shar"), not derived from any id.
const storedFilesSharedObjID int32 = 0x73686172

// storedFilesOwnerObjID derives the owner half's objid from the owner's uuid, like
// SecretMutationLockObjID: a uuid's leading bytes are random, so a collision is possible and is a
// contention non-event.
func storedFilesOwnerObjID(owner uuid.UUID) int32 {
	return int32(binary.BigEndian.Uint32(owner[:4])) //nolint:gosec // wraparound is fine: a lock key, not a number
}

// LockStoredFiles takes the stored-file admission locks inside db's transaction: the owner key
// first, then the shared-budget key (see StoredFilesLockClass). It is the ONLY way any code may
// take either key, so the order is one order. The caller must be inside a transaction (on a bare
// pool the locks would release immediately) and must run its sums and its reservation insert in
// that same transaction.
//
// Never take it while holding a lock a stored-files path can want the other way round. The global
// lock order is: (1) a recovery_captures row lock, if the path takes one; (2) the owner key; (3) the
// shared key; (4) job_files rows. No path takes a runs row lock inside the keys: Reserve's ownership check is a plain read made before them. The recovery admit and stream transactions take a capture row
// first and these keys after; the job-file Reserve and Sweep take only these keys and job_files
// rows (never a capture row), so no path waits on a capture row while holding a key. A path that
// takes the keys must therefore never afterwards wait on a recovery_captures row, and the
// sweep's reservation release SKIPs locked job_files rows rather than waiting on a writer.
func LockStoredFiles(ctx context.Context, db DBTX, owner uuid.UUID) error {
	if _, err := db.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)", StoredFilesLockClass, storedFilesOwnerObjID(owner)); err != nil {
		return err
	}
	_, err := db.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)", StoredFilesLockClass, storedFilesSharedObjID)
	return err
}

// StoredFilesSums are the byte totals an admission compares against its quotas. Job bytes count
// every job_files state except 'expired'. Recovery bytes count 'available' captures by their bound
// byte_size and 'preparing'/'uploading' captures by the reserved_bytes their upload admission
// stamped, so the shared budget sees a chunk stream that runs outside the lock.
type StoredFilesSums struct {
	OwnerJobBytes         int64
	InstanceJobBytes      int64
	OwnerRecoveryBytes    int64
	InstanceRecoveryBytes int64
}

// SharedBytes is the figure the shared budget bounds: every job-file byte plus every recovery byte.
func (s StoredFilesSums) SharedBytes() int64 { return s.InstanceJobBytes + s.InstanceRecoveryBytes }

// SumStoredFiles reads the four sums. Call it under LockStoredFiles, in the transaction that then
// reserves. excludeCapture leaves one recovery capture (the one being admitted) out of its own
// sum; uuid.Nil excludes nothing.
func SumStoredFiles(ctx context.Context, q *Queries, owner, excludeCapture uuid.UUID) (StoredFilesSums, error) {
	row, err := q.SumStoredFileBytes(ctx, SumStoredFileBytesParams{
		UserID:           owner,
		ExcludeCaptureID: pgtype.UUID{Bytes: excludeCapture, Valid: excludeCapture != uuid.Nil},
	})
	if err != nil {
		return StoredFilesSums{}, err
	}
	return StoredFilesSums(row), nil
}
