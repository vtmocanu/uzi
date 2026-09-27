package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1810: a terminal run's published checkpoint ref is RETAINED on origin while any of its
// custody holds is open (D1), instead of being deleted best-effort on every terminal
// transition. The record lives in checkpoint_retentions (one row per run); every forge write
// against the ref runs under a per-run SESSION advisory lock (withRetentionLock) and is
// compare-and-swap on the recorded tip.

// Checkpoint retention states (checkpoint_retentions.state; see migration 00259).
const (
	retentionRetained    = "retained"
	retentionSuperseded  = "superseded"
	retentionSettling    = "settling"
	retentionDefaultConc = 2
	// retentionOpTimeout bounds one whole locked operation (lock, re-read, forge round-trip,
	// record), detached from the caller's context.
	retentionOpTimeout = 2 * time.Minute
	// retentionRecordTimeout bounds the terminal-time record inserts run on the caller's path.
	retentionRecordTimeout = 10 * time.Second
	// retentionUnlockTimeout bounds the unlock, which runs on a fresh context because the
	// operation's own may already have expired.
	retentionUnlockTimeout = 10 * time.Second
	// Retry backoff for a failed forge write: base * 2^attempts, capped.
	retentionBackoffBase = time.Minute
	retentionBackoffCap  = time.Hour
)

// ErrRetentionLockLost is returned by the fence when the pinned session no longer holds the
// run's retention advisory lock (the backend was terminated, the connection dropped, or the
// check itself failed). The caller must not perform the forge write it was about to make.
var ErrRetentionLockLost = errors.New("checkpoint retention lock lost")

// ConnAcquirer hands out a dedicated pool connection. *pgxpool.Pool satisfies it. The
// retention lock is session-scoped, so the lock, its fence checks and its unlock must all run
// on the same pinned connection.
type ConnAcquirer interface {
	Acquire(ctx context.Context) (*pgxpool.Conn, error)
}

// retentionTestHooks are LiveDB race seams for the retention paths (nil in production, the
// claimHooks idiom). beforeForgeWrite runs under the lock, immediately BEFORE the fence that
// guards a forge write, so a test can steal the lock (terminate the backend) or interleave a
// concurrent writer at the one point it matters. op names the write ("delete").
type retentionTestHooks struct {
	beforeForgeWrite func(runID uuid.UUID, op string)
}

// SetRetentionLockPool wires the pool the per-run retention lock pins its connection from
// (PRD #1810). Call once at startup with the shared *pgxpool.Pool. Nil (the default, and every
// fake-store test) means no lock can ever be taken, so every retention path RETAINS: nothing is
// ever deleted without the lock.
func (s *Service) SetRetentionLockPool(p ConnAcquirer) { s.retentionPool = p }

// retentionWired reports whether this service can broker a retained-ref delete at all. Any
// missing seam means RETAIN: the terminal path records nothing and deletes nothing, and the
// ref simply stays on origin.
func (s *Service) retentionWired() bool {
	return s.retentionPool != nil && s.retentionSem != nil && s.deleteCheckpointFn != nil &&
		s.forgeBaseURLAllowed != nil && s.box != nil && s.background != nil
}

// withRetentionLock runs fn while holding the per-run checkpoint-retention SESSION advisory lock
// (store.CheckpointRetentionLockClass). It never blocks on contention: acquired is false, with a
// nil error, when the process-wide concurrency slot is full, the key is held by another session,
// or no pool is wired. The caller then leaves the ref alone (a later settle or the sweeper
// retries); nothing destructive ever happens without the lock.
//
// The whole operation is detached from ctx's cancellation and bounded by retentionOpTimeout.
// fn receives that context and a fence: fn MUST call fence immediately before every forge write
// and skip the write when it errors. The fence re-reads pg_locks on the pinned session, so a lock
// lost mid-operation (backend terminated, connection dropped) is observed before the write, not
// after.
//
// The unlock runs on a fresh bounded context. If it fails, or reports the lock was not held, the
// connection is hijacked out of the pool and closed, so a session that may still hold the lock
// is never handed to another caller.
func (s *Service) withRetentionLock(ctx context.Context, runID uuid.UUID, fn func(ctx context.Context, fence func(context.Context) error) error) (acquired bool, err error) {
	if s.retentionPool == nil || s.retentionSem == nil {
		return false, nil
	}
	select {
	case s.retentionSem <- struct{}{}:
	default:
		return false, nil
	}
	defer func() { <-s.retentionSem }()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), retentionOpTimeout)
	defer cancel()

	conn, err := s.retentionPool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("retention lock: acquire connection: %w", err)
	}
	objID := store.CheckpointRetentionLockObjID(runID)
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1, $2)", store.CheckpointRetentionLockClass, objID).Scan(&got); err != nil {
		// Unknown whether the session now holds the lock: never return it to the pool.
		destroyRetentionConn(conn)
		return false, fmt.Errorf("retention lock: try lock: %w", err)
	}
	if !got {
		conn.Release()
		return false, nil
	}
	defer releaseRetentionLock(conn, runID, objID)

	fence := func(fctx context.Context) error {
		var n int64
		// Two-int advisory locks appear in pg_locks with classid = key1, objid = key2 and
		// objsubid = 2. classid/objid are oid (unsigned); the int4 -> oid cast reinterprets the
		// key's bits, so a negative objid matches too.
		qerr := conn.QueryRow(fctx, `SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted
			  AND classid = $1::int4::oid AND objid = $2::int4::oid AND objsubid = 2`,
			store.CheckpointRetentionLockClass, objID).Scan(&n)
		if qerr != nil {
			return fmt.Errorf("%w: %s", ErrRetentionLockLost, secretscrub.Scrub(qerr.Error()))
		}
		if n == 0 {
			return ErrRetentionLockLost
		}
		return nil
	}
	return true, fn(ctx, fence)
}

// releaseRetentionLock unlocks the run's retention lock on the pinned session, returning the
// connection to the pool only when the unlock provably succeeded.
func releaseRetentionLock(conn *pgxpool.Conn, runID uuid.UUID, objID int32) {
	ctx, cancel := context.WithTimeout(context.Background(), retentionUnlockTimeout)
	defer cancel()
	var unlocked bool
	err := conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1, $2)", store.CheckpointRetentionLockClass, objID).Scan(&unlocked)
	if err != nil || !unlocked {
		if err != nil {
			slog.Warn("checkpoint retention: unlock failed; closing the session", "run", runID, "error", secretscrub.Scrub(err.Error()))
		} else {
			slog.Warn("checkpoint retention: lock was not held at unlock; closing the session", "run", runID)
		}
		destroyRetentionConn(conn)
		return
	}
	conn.Release()
}

// destroyRetentionConn removes conn from the pool and closes it, which ends its session and so
// releases any session advisory lock it may hold.
func destroyRetentionConn(conn *pgxpool.Conn) {
	c := conn.Hijack()
	ctx, cancel := context.WithTimeout(context.Background(), retentionUnlockTimeout)
	defer cancel()
	_ = c.Close(ctx)
}

// retainOrDeleteCheckpoint is the terminal-transition checkpoint handler (PRD #1810 M1, D1),
// replacing the unconditional best-effort delete of PRD #1030 M4. It runs AFTER the terminal
// state committed and never fails or delays the caller beyond two bounded inserts:
//
//   - an ineligible kind (checkpointBranch), or any missing retention seam: nothing (RETAIN);
//   - a run that published a checkpoint and has an OPEN custody hold (any generation): a
//     `retained` record, and no forge call. The ref stays on origin for recovery;
//   - a run that published a checkpoint and has NO open hold (a completed run whose hold was
//     just released, or a failed/cancelled run on a worker without the recovery capability):
//     a `settling` record, and a background settle that deletes the ref CAS on its tip;
//   - a run that already has a record (a duplicate terminal call): the record is never reset;
//     one still owing work is handed to the settle, which re-checks custody under the lock.
//
// A run that never published (checkpoint_tip NULL) or has no repo gets no record and no forge
// call: it owns no ref, and a delete could clobber a sibling's checkpoint on the same branch.
func (s *Service) retainOrDeleteCheckpoint(ctx context.Context, runID uuid.UUID, kind string, issueIid pgtype.Int8) {
	branch, ok := checkpointBranch(kind, runID, issueIid)
	if !ok || !s.retentionWired() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), retentionRecordTimeout)
	defer cancel()
	ref := checkpointRefPrefix + branch

	held, err := s.q.InsertCheckpointRetentionIfHeld(ctx, store.InsertCheckpointRetentionIfHeldParams{RunID: runID, Branch: branch, Ref: ref})
	if err != nil {
		slog.Warn("checkpoint retention: record retained", "run", runID, "error", err)
		return
	}
	if held > 0 {
		slog.Info("checkpoint retention: ref retained while custody is open", "run", runID, "branch", branch)
		return
	}
	settling, err := s.q.InsertCheckpointRetentionSettling(ctx, store.InsertCheckpointRetentionSettlingParams{RunID: runID, Branch: branch, Ref: ref})
	if err != nil {
		slog.Warn("checkpoint retention: record settling", "run", runID, "error", err)
		return
	}
	if settling == 0 {
		row, err := s.q.GetCheckpointRetention(ctx, runID)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				slog.Warn("checkpoint retention: re-read", "run", runID, "error", err)
			}
			// No row: the run never published, has no repo, or a hold appeared between the
			// two inserts. Either way there is nothing this path may delete.
			return
		}
		switch row.State {
		case retentionRetained, retentionSuperseded, retentionSettling:
		default:
			return
		}
	}
	s.SettleRetainedCheckpoint(runID)
}

// SettleRetainedCheckpoint dispatches, off the caller's goroutine, the settle of one run's
// retained checkpoint ref (PRD #1810): once no custody hold of the run is open, move its record
// to `settling` and delete the ref it names, compare-and-swap on the recorded tip. Safe to call
// at any time and any number of times: every step re-reads the record under the run's retention
// lock, and a busy lock, an open hold or a missing seam leaves the ref in place.
func (s *Service) SettleRetainedCheckpoint(runID uuid.UUID) {
	if !s.retentionWired() {
		return
	}
	s.background(func() {
		// s.background is a DETACHED goroutine in production and the delete drives go-git,
		// which has nil-deref panic paths on malformed forge responses; an unrecovered panic in
		// any goroutine crashes the api, so recover first (forgesvc's launchSeed idiom).
		defer func() {
			if r := recover(); r != nil {
				slog.Error("checkpoint retention: settle panicked", "run", runID, "panic", secretscrub.Scrub(fmt.Sprint(r)))
			}
		}()
		s.settleRetainedCheckpoint(context.Background(), runID)
	})
}

// settleRetainedCheckpoint is SettleRetainedCheckpoint's body, run inline.
func (s *Service) settleRetainedCheckpoint(ctx context.Context, runID uuid.UUID) {
	acquired, err := s.withRetentionLock(ctx, runID, func(ctx context.Context, fence func(context.Context) error) error {
		row, err := s.q.GetCheckpointRetention(ctx, runID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("read record: %w", err)
		}
		switch row.State {
		case retentionRetained, retentionSuperseded:
			n, err := s.q.SetCheckpointRetentionSettlingIfUnheld(ctx, runID)
			if err != nil {
				return fmt.Errorf("mark settling: %w", err)
			}
			if n == 0 {
				return nil // a hold is still open (or the record moved on): keep the ref
			}
			if row, err = s.q.GetCheckpointRetention(ctx, runID); err != nil {
				return fmt.Errorf("re-read record: %w", err)
			}
			if row.State != retentionSettling {
				return nil
			}
		case retentionSettling:
		default:
			return nil
		}
		return s.deleteSettlingRef(ctx, row, fence)
	})
	if err != nil {
		slog.Warn("checkpoint retention: settle", "run", runID, "error", secretscrub.Scrub(err.Error()))
		return
	}
	if !acquired {
		slog.Debug("checkpoint retention: lock busy; ref left for a later settle", "run", runID)
	}
}

// deleteSettlingRef performs the CAS delete a `settling` record owes, under the run's retention
// lock. The forge coordinates are derived SERVER-SIDE exactly as Publish derives them
// (GetRunClaimContext, the SSRF gate on the base URL and clone host, box.Open), never from a
// worker. A lost lock (fence error) skips the write and records nothing: whoever holds the lock
// now owns the record. A failed write is recorded with exponential backoff for the retry pass.
func (s *Service) deleteSettlingRef(ctx context.Context, row store.CheckpointRetention, fence func(context.Context) error) error {
	runID := row.RunID
	fail := func(msg string) error {
		next := time.Now().Add(retentionBackoff(row.Attempts))
		if _, err := s.q.RecordCheckpointRetentionFailure(ctx, store.RecordCheckpointRetentionFailureParams{
			RunID: runID, LastError: msg, NextAttemptAt: pgtype.Timestamptz{Time: next, Valid: true}, ExpectedState: retentionSettling,
		}); err != nil {
			return fmt.Errorf("record failure (%s): %w", msg, err)
		}
		slog.Warn("checkpoint retention: delete deferred", "run", runID, "ref", row.Ref, "attempt", row.Attempts+1, "reason", msg)
		return nil
	}

	rc, err := s.q.GetRunClaimContext(ctx, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The run, its repo or its forge connection is gone: the delete can never be
			// brokered again. Keep the record as an audit row.
			if _, aerr := s.q.SetCheckpointRetentionAbandoned(ctx, store.SetCheckpointRetentionAbandonedParams{
				RunID: runID, LastError: "run, repository or forge connection no longer exists", ExpectedState: retentionSettling,
			}); aerr != nil {
				return fmt.Errorf("mark abandoned: %w", aerr)
			}
			return nil
		}
		return fail("claim context: " + secretscrub.Scrub(err.Error()))
	}
	cloneURL := rc.RepoWebUrl + ".git"
	// Same SSRF gate as Publish, BEFORE decrypting the PAT: never point go-git at an
	// un-allowlisted host.
	if !s.forgeBaseURLAllowed(rc.BaseUrl) {
		return fail("forge base URL is not allowlisted")
	}
	cloneHost, err := forgeHostFromURL(cloneURL)
	if err != nil || !s.forgeBaseURLAllowed(cloneHost) {
		return fail("clone host is not allowlisted")
	}
	botPAT, err := s.box.Open(rc.TokenCiphertext)
	if err != nil {
		return fail("bot PAT could not be decrypted")
	}

	if h := s.retentionHooks; h != nil && h.beforeForgeWrite != nil {
		h.beforeForgeWrite(runID, "delete")
	}
	if err := fence(ctx); err != nil {
		return err
	}
	if derr := s.deleteRetainedRef(ctx, cloneURL, rc, string(botPAT), row); derr != nil {
		// The error is PERSISTED (last_error), so scrub it harder than a log line: the exact PAT
		// this call used, any URL userinfo (a go-git remote URL can carry the PAT there), then
		// the known credential shapes.
		return fail("delete ref: " + scrubForgeError(derr.Error(), string(botPAT)))
	}
	n, err := s.q.SetCheckpointRetentionDeleted(ctx, store.SetCheckpointRetentionDeletedParams{RunID: runID, Ref: row.Ref, Tip: row.Tip})
	if err != nil {
		return fmt.Errorf("mark deleted: %w", err)
	}
	if n > 0 {
		slog.Info("checkpoint retention: ref deleted", "run", runID, "ref", row.Ref)
	}
	return nil
}

// deleteRetainedRef issues the forge CAS delete of the ref a record names at its recorded tip.
// pushbroker.Delete treats a ref already absent, or advanced past the tip by another run, as
// success. M1 records only branch checkpoint refs; a record naming any other ref (a recovery
// ref, M3) is refused here until the delete is switched to DeleteOptions.Ref.
func (s *Service) deleteRetainedRef(ctx context.Context, cloneURL string, rc store.GetRunClaimContextRow, pat string, row store.CheckpointRetention) error {
	if row.Ref != checkpointRefPrefix+row.Branch {
		return fmt.Errorf("record names %q, not the branch checkpoint ref", row.Ref)
	}
	return s.deleteCheckpointFn(ctx, pushbroker.DeleteOptions{
		CloneURL:       cloneURL,
		Branch:         row.Branch,
		Username:       rc.BotUsername,
		PAT:            pat,
		ExpectedOldTip: row.Tip,
	})
}

// urlUserinfoPattern matches the userinfo of a URL (scheme://user:secret@host).
var urlUserinfoPattern = regexp.MustCompile(`://[^/@\s]+@`)

// scrubForgeError redacts a forge error before it is persisted: every occurrence of the PAT the
// call used, any URL userinfo, and the known credential shapes (secretscrub.Scrub).
func scrubForgeError(msg, pat string) string {
	if pat != "" {
		msg = strings.ReplaceAll(msg, pat, "[redacted]")
	}
	msg = urlUserinfoPattern.ReplaceAllString(msg, "://[redacted]@")
	return secretscrub.Scrub(msg)
}

// retentionBackoff is the retry delay after the attempts-th failed forge write:
// retentionBackoffBase doubled per prior failure, capped at retentionBackoffCap.
func retentionBackoff(attempts int32) time.Duration {
	d := retentionBackoffBase
	for i := int32(0); i < attempts && d < retentionBackoffCap; i++ {
		d *= 2
	}
	if d > retentionBackoffCap {
		d = retentionBackoffCap
	}
	return d
}
