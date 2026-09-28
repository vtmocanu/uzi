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

// Checkpoint retention states (checkpoint_retentions.state; see migration 00263).
const (
	retentionRetained    = "retained"
	retentionSuperseding = "superseding"
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
	// A terminal run's publish retries a busy retention lock (publishTerminalLocked) every
	// interval for up to budget: the agent's shutdown checkpoint sink is one-shot, so a skip
	// under brief contention would lose a cancelled run's last checkpoint. The budget is a small
	// share of the agent's whole shutdown publish budget (shutdownPublishTimeoutMs, 15s by
	// default in agent/src/runner.ts), which also covers its scan and the push itself.
	terminalPublishLockRetryBudget   = 3 * time.Second
	terminalPublishLockRetryInterval = 200 * time.Millisecond
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
// guards a forge write, so a test can pause an attempt, steal the lock (terminate the backend)
// or interleave a concurrent writer at the one point it matters. op names the write: "delete"
// (a settling record's ref), "create" (supersession's recovery ref), "delete-branch"
// (supersession's branch ref), "exit-recovery"/"exit-branch" (a stuck supersession's exit, M4)
// "audit-delete" (the post-settlement audit's stray recovery ref, M4) or "attempt-delete" (a
// terminal run's late publish the attempts arm deletes, #1810 residual 2). afterLock runs once the lock is held, with the pinned
// connection, so a test can release the lock WITHOUT ending the session.
type retentionTestHooks struct {
	beforeForgeWrite func(runID uuid.UUID, op string)
	afterLock        func(runID uuid.UUID, conn *pgxpool.Conn)
}

// beforeRetentionWrite is the mandatory prelude of every retention forge write: the test hook
// (if any) and THEN the fence, so a paused attempt whose lock is lost meanwhile is stopped.
func (s *Service) beforeRetentionWrite(ctx context.Context, runID uuid.UUID, op string, fence func(context.Context) error) error {
	if h := s.retentionHooks; h != nil && h.beforeForgeWrite != nil {
		h.beforeForgeWrite(runID, op)
	}
	return fence(ctx)
}

// SetRetentionLockPool wires the pool the per-run retention lock pins its connection from
// (PRD #1810). Call once at startup with the shared *pgxpool.Pool. Nil (the default, and every
// fake-store test) means no lock can ever be taken, so every retention path RETAINS: nothing is
// ever deleted without the lock.
func (s *Service) SetRetentionLockPool(p ConnAcquirer) { s.retentionPool = p }

// retentionWired reports whether this service can broker a retained-ref delete at all. Any
// missing seam means RETAIN: the Go terminal path records nothing and deletes nothing, and the
// ref simply stays on origin. The record itself may still exist: migration 00265's trigger
// inserts it in the terminal transaction whatever this service has wired, and without the seams
// nothing ever drives it to a delete.
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
	if h := s.retentionHooks; h != nil && h.afterLock != nil {
		h.afterLock(runID, conn)
	}

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

// retainOrDeleteCheckpoint is the post-commit half of the terminal-transition checkpoint handler
// (PRD #1810 M1, D1), replacing the unconditional best-effort delete of PRD #1030 M4. It runs
// AFTER the terminal state committed and never fails or delays the caller beyond two bounded
// inserts and a re-read.
//
// The record is normally already there: migration 00265's trigger on runs.status inserts it in
// the SAME transaction as the terminal status (so no reader, claimCheckpointSlot included, ever
// sees a terminal run that published a checkpoint without its record), with the same columns,
// the same retained/settling choice and the same never-reset rule as the inserts below. Both
// inserts then move zero rows and the re-read finds the trigger's row; this path's job is to
// dispatch the settle that row owes (a completed run's hold is released after the terminal commit,
// so its row is typically `retained` at insert and settles here). The Go inserts below are reached
// only for a run the trigger could not record: one that had no persisted checkpoint tip at its
// terminal UPDATE but has one now (a publish whose tip persist committed after the terminal
// transition, before this call). The same inserts serve the sweeper's backfill arm, which finds
// the rest of that family: a terminal run's late first publish whose TrackTerminalCheckpointPublish
// insert failed, or that landed while retention was unwired in this api. The cases:
//
//   - an ineligible kind (checkpointBranch), or any missing retention seam: no forge call (RETAIN);
//   - a run that published a checkpoint and has an OPEN custody hold (any generation): a
//     `retained` record, and no forge call. The ref stays on origin for recovery;
//   - a run that published a checkpoint and has NO open hold (a completed run whose hold was
//     just released, or a failed/cancelled run on a worker without the recovery capability):
//     a `settling` record, and a background settle that deletes the ref CAS on its tip under
//     the run's retention lock;
//   - a run that already has a record (the trigger's row, or a duplicate terminal call): the
//     record is never reset;
//     one still owing work is handed to the settle, which re-reads the record under the lock
//     and moves it to settling only through the guarded SetCheckpointRetentionSettlingIfUnheld
//     (its NOT EXISTS open-hold predicate is the custody check).
//
// A run that never published (checkpoint_tip NULL) or has no repo gets no record and no forge
// call: it owns no ref, and a delete could clobber a sibling's checkpoint on the same branch.
func (s *Service) retainOrDeleteCheckpoint(ctx context.Context, runID uuid.UUID, kind string, issueIid pgtype.Int8) {
	if !s.retentionWired() {
		return
	}
	if _, settle := s.recordCheckpointRetention(ctx, runID, kind, issueIid); settle {
		s.SettleRetainedCheckpoint(runID)
	}
}

// recordCheckpointRetention is the record half of retainOrDeleteCheckpoint, shared with the
// sweeper's backfill (PRD #1810 M4): it inserts the run's `retained` or `settling` record
// (never resetting an existing one) and reports whether a record was inserted now and whether
// the run's record owes a settle. It makes no forge call. For a terminal transition committed
// with migration 00265's trigger in place the record already exists, so inserted is false and
// settle reflects the existing record's state; inserted is true only for a run the trigger never
// recorded: one whose checkpoint tip was persisted only after its terminal transition (a late first
// publish whose own track insert failed, or ran while retention was unwired).
func (s *Service) recordCheckpointRetention(ctx context.Context, runID uuid.UUID, kind string, issueIid pgtype.Int8) (inserted, settle bool) {
	branch, ok := checkpointBranch(kind, runID, issueIid)
	if !ok {
		return false, false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), retentionRecordTimeout)
	defer cancel()
	ref := checkpointRefPrefix + branch

	held, err := s.q.InsertCheckpointRetentionIfHeld(ctx, store.InsertCheckpointRetentionIfHeldParams{RunID: runID, Branch: branch, Ref: ref})
	if err != nil {
		slog.Warn("checkpoint retention: record retained", "run", runID, "error", err)
		return false, false
	}
	if held > 0 {
		slog.Info("checkpoint retention: ref retained while custody is open", "run", runID, "branch", branch)
		return true, false
	}
	settling, err := s.q.InsertCheckpointRetentionSettling(ctx, store.InsertCheckpointRetentionSettlingParams{RunID: runID, Branch: branch, Ref: ref})
	if err != nil {
		slog.Warn("checkpoint retention: record settling", "run", runID, "error", err)
		return false, false
	}
	if settling > 0 {
		return true, true
	}
	row, err := s.q.GetCheckpointRetention(ctx, runID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("checkpoint retention: re-read", "run", runID, "error", err)
		}
		// No row: the run never published, has no repo, or a hold appeared between the
		// two inserts. Either way there is nothing this path may delete.
		return false, false
	}
	switch row.State {
	case retentionRetained, retentionSuperseded, retentionSettling:
		return false, true
	}
	return false, false
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
	_, acquired, err := s.settleRetainedCheckpointOnce(ctx, runID)
	if err != nil {
		slog.Warn("checkpoint retention: settle", "run", runID, "error", secretscrub.Scrub(err.Error()))
		return
	}
	if !acquired {
		slog.Debug("checkpoint retention: lock busy; ref left for a later settle", "run", runID)
	}
}

// settleRetainedCheckpointOnce runs one settle attempt for runID under its retention lock
// (settleLocked). done reports the forge delete succeeded; acquired is false when the lock or a
// concurrency slot was busy (nothing ran). A panic from the go-git seams is recovered into err.
func (s *Service) settleRetainedCheckpointOnce(ctx context.Context, runID uuid.UUID) (done, acquired bool, err error) {
	return s.lockedRetentionStep(ctx, runID, s.settleLocked)
}

// settleLocked is one settle under the run's retention lock: a retained/superseded record with no
// open hold moves to settling (guarded in SQL on the open-hold predicate), and a settling
// record's ref is CAS-deleted. Any other state is left alone.
func (s *Service) settleLocked(ctx context.Context, runID uuid.UUID, fence func(context.Context) error) (bool, error) {
	row, err := s.q.GetCheckpointRetention(ctx, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("read record: %w", err)
	}
	switch row.State {
	case retentionRetained, retentionSuperseded:
		n, err := s.q.SetCheckpointRetentionSettlingIfUnheld(ctx, runID)
		if err != nil {
			return false, fmt.Errorf("mark settling: %w", err)
		}
		if n == 0 {
			return false, nil // a hold is still open (or the record moved on): keep the ref
		}
		if row, err = s.q.GetCheckpointRetention(ctx, runID); err != nil {
			return false, fmt.Errorf("re-read record: %w", err)
		}
		if row.State != retentionSettling {
			return false, nil
		}
	case retentionSettling:
	default:
		return false, nil
	}
	return s.deleteSettlingRef(ctx, row, fence)
}

// retentionForge is the server-derived forge connection a retention forge write uses.
type retentionForge struct {
	cloneURL string
	username string
	pat      string
}

// forgeForRetention derives the forge coordinates for a run's retention write SERVER-SIDE,
// exactly as Publish derives them: GetRunClaimContext, the SSRF gate on BOTH the base URL and
// the clone host, then box.Open (never from a worker, and never decrypting the PAT for an
// un-allowlisted host). gone is true when the run, its repo or its forge connection no longer
// exists, so the write can never be brokered again. Otherwise a non-empty problem is a
// retryable failure, already safe to persist.
func (s *Service) forgeForRetention(ctx context.Context, runID uuid.UUID) (f retentionForge, problem string, gone bool) {
	rc, err := s.q.GetRunClaimContext(ctx, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return retentionForge{}, "", true
		}
		return retentionForge{}, "claim context: " + secretscrub.Scrub(err.Error()), false
	}
	cloneURL := rc.RepoWebUrl + ".git"
	if !s.forgeBaseURLAllowed(rc.BaseUrl) {
		return retentionForge{}, "forge base URL is not allowlisted", false
	}
	cloneHost, err := forgeHostFromURL(cloneURL)
	if err != nil || !s.forgeBaseURLAllowed(cloneHost) {
		return retentionForge{}, "clone host is not allowlisted", false
	}
	botPAT, err := s.box.Open(rc.TokenCiphertext)
	if err != nil {
		return retentionForge{}, "bot PAT could not be decrypted", false
	}
	return retentionForge{cloneURL: cloneURL, username: rc.BotUsername, pat: string(botPAT)}, "", false
}

// recordRetentionFailure records a failed (or refused) retention step on the record, guarded on
// the state the caller acted in: attempts+1, the already-scrubbed msg as last_error, and the
// next retry pushed out by the exponential backoff. The ref is left as it is.
func (s *Service) recordRetentionFailure(ctx context.Context, row store.CheckpointRetention, expectedState, msg string) error {
	next := time.Now().Add(retentionBackoff(row.Attempts))
	if _, err := s.q.RecordCheckpointRetentionFailure(ctx, store.RecordCheckpointRetentionFailureParams{
		RunID: row.RunID, LastError: msg, NextAttemptAt: pgtype.Timestamptz{Time: next, Valid: true}, ExpectedState: expectedState,
	}); err != nil {
		return fmt.Errorf("record failure (%s): %w", msg, err)
	}
	slog.Warn("checkpoint retention: step deferred", "run", row.RunID, "state", expectedState, "ref", row.Ref,
		"attempt", row.Attempts+1, "reason", msg)
	return nil
}

// deleteSettlingRef performs the CAS delete a `settling` record owes, under the run's retention
// lock, through forgeForRetention's server-derived coordinates. A lost lock (fence error) skips
// the write and records nothing: whoever holds the lock now owns the record. A failed write is
// recorded with exponential backoff for the retry pass. done reports that the forge delete
// returned success (the ref is gone, was already absent, or had advanced past the tip).
func (s *Service) deleteSettlingRef(ctx context.Context, row store.CheckpointRetention, fence func(context.Context) error) (done bool, err error) {
	runID := row.RunID
	f, problem, gone := s.forgeForRetention(ctx, runID)
	if gone {
		// The run, its repo or its forge connection is gone: the delete can never be
		// brokered again. Keep the record as an audit row.
		if _, aerr := s.q.SetCheckpointRetentionAbandoned(ctx, store.SetCheckpointRetentionAbandonedParams{
			RunID: runID, LastError: "run, repository or forge connection no longer exists", ExpectedState: retentionSettling,
		}); aerr != nil {
			return false, fmt.Errorf("mark abandoned: %w", aerr)
		}
		return false, nil
	}
	if problem != "" {
		return false, s.recordRetentionFailure(ctx, row, retentionSettling, problem)
	}

	if err := s.beforeRetentionWrite(ctx, runID, "delete", fence); err != nil {
		return false, err
	}
	if derr := s.deleteRetainedRef(ctx, f, row); derr != nil {
		// The error is PERSISTED (last_error), so scrub it harder than a log line: the exact PAT
		// this call used, any URL userinfo (a go-git remote URL can carry the PAT there), then
		// the known credential shapes.
		return false, s.recordRetentionFailure(ctx, row, retentionSettling, "delete ref: "+scrubForgeError(derr.Error(), f.pat))
	}
	n, err := s.q.SetCheckpointRetentionDeleted(ctx, store.SetCheckpointRetentionDeletedParams{RunID: runID, Ref: row.Ref, Tip: row.Tip})
	if err != nil {
		return true, fmt.Errorf("mark deleted: %w", err)
	}
	if n > 0 {
		slog.Info("checkpoint retention: ref deleted", "run", runID, "ref", row.Ref)
	}
	return true, nil
}

// deleteRetainedRef issues the forge CAS delete of the ref a record names at its recorded tip,
// through DeleteOptions.Ref so the same path serves the branch checkpoint ref and the run's
// recovery ref. pushbroker.Delete treats a ref already absent, or advanced past the tip by
// another run, as success. A record naming any other ref is refused.
func (s *Service) deleteRetainedRef(ctx context.Context, f retentionForge, row store.CheckpointRetention) error {
	isBranch := row.Ref == checkpointRefPrefix+row.Branch
	isRecovery := row.RecoveryRef.Valid && row.Ref == row.RecoveryRef.String &&
		row.Ref == pushbroker.RecoveryRefPrefix+row.RunID.String()
	if !isBranch && !isRecovery {
		return fmt.Errorf("record names %q, neither the branch checkpoint ref nor the run's recovery ref", row.Ref)
	}
	return s.deleteCheckpointFn(ctx, pushbroker.DeleteOptions{
		CloneURL:       f.cloneURL,
		Branch:         row.Branch,
		Ref:            row.Ref,
		Username:       f.username,
		PAT:            f.pat,
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

// terminalPublishSuperseded reports whether a TERMINAL run's publish must be refused because its
// branch slot was handed (or is being handed) to a newer run: its record is superseding or
// superseded, or names a recovery ref at all (a supersession began: the record may since have
// settled, exited or closed as tip-gone). Re-creating the branch ref for such a run would take
// the slot back from the run it was handed to. Inert (false) when retention is not wired, and for
// a run with no record. A read error is returned: the publish is best-effort, and a refused one is
// safer than one that could leave an untracked ref.
func (s *Service) terminalPublishSuperseded(ctx context.Context, runID uuid.UUID) (bool, error) {
	if !s.retentionWired() {
		return false, nil
	}
	row, err := s.q.GetCheckpointRetention(ctx, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("publish: read checkpoint retention: %s", secretscrub.Scrub(err.Error()))
	}
	return row.State == retentionSuperseding || row.State == retentionSuperseded || row.RecoveryRef.Valid, nil
}

// trackPublishedCheckpoint makes the run's retention record track the branch-ref tip a successful
// publish just advanced (PRD #1810), so no ref outlives its record:
//
//   - a TERMINAL run (TrackTerminalCheckpointPublish, one guarded statement): no record inserts
//     one, retained with an open hold else settling; a retained/settling record advances its
//     tip; a deleted/abandoned record naming the branch ref is REOPENED (retained with an open
//     hold, else settling). A settling result reports settle, and the caller dispatches the
//     settle (SettleRetainedCheckpoint) once it no longer holds the run's retention lock, so a
//     publish after the run's record settled is deleted again and a new run on the branch is not
//     blocked;
//   - a LIVE run (no row from the statement, whose terminal-status predicate proposes nothing):
//     unchanged, the tip-lag advance of a retained/settling record naming the branch ref
//     (AdvanceCheckpointRetentionTip).
//
// Neither statement is gated on terminal (the second runs only when the first matched no row):
// TrackTerminalCheckpointPublish
// reads the run's CURRENT status in SQL, so a run that turned terminal after Publish's top read is
// tracked as terminal here. terminal (the status Publish read at its top) only gates the Warn (and tracked) below
// and records how the publish was routed.
//
// A publish Publish routed as terminal reaches here under the run's retention lock, after
// terminalPublishSuperseded refused a run whose slot was handed on (publishTerminalLocked), so a
// supersession cannot interleave with it. One routed LIVE arrives without the lock, possibly after
// the run turned terminal. Its push was sent within livePublishPrePushBudget of the routing, and no
// supersession intent is recorded until the run has been terminal for checkpointSupersessionCooling,
// so an ordinary in-flight push is tracked here before any supersession binds the record's tip.
// When a run's publish moved no row in either statement and the run is terminal (as read at
// Publish's top, or, for terminal false, as re-read here), it published a branch ref no record
// tracks (a record in a state neither statement moves: superseding, superseded, or one whose
// recovery ref is set), or its status could not be re-read. A Warn names the run, branch and tip,
// and tracked is false: the caller then keeps the push's checkpoint_publish_attempts row, and the
// sweeper's attempts arm (reconcilePublishAttempts) re-records the tip or, once no custody hold of
// the run is open, CAS-deletes the branch ref at it.
//
// Residual: the tip persist (SetRunCheckpointTip, publishOutcome) and this track are best-effort
// writes AFTER the push. If they fail, or are delayed past the cooling period's slack, the record's
// tip lags the branch ref: a supersession then stops in resolveMissingSource ("branch ref is not at
// the recorded tip") and holds the branch slot until the stuck exit clears it once the run's
// custody holds release (the push's attempt row lets that exit recognise the tip as the run's own).
//
// Best-effort: a failure is logged and the publish still reports success (the ref already moved).
// tracked reports the tip is accounted for: recorded on the run's record, or the run is live (its
// record, inserted at its terminal transition, then reads the tip persisted before it).
func (s *Service) trackPublishedCheckpoint(ctx context.Context, runID uuid.UUID, terminal bool, branch, ref, tip string) (settle, tracked bool) {
	if !s.retentionWired() {
		return false, true
	}
	state, err := s.q.TrackTerminalCheckpointPublish(ctx, store.TrackTerminalCheckpointPublishParams{
		RunID: runID, Branch: branch, Ref: ref, Tip: tip,
	})
	switch {
	case err == nil:
		return state == retentionSettling, true
	case !errors.Is(err, pgx.ErrNoRows):
		slog.Warn("checkpoint retention: track terminal publish", "run", runID, "tip", tip, "error", err)
		return false, false
	}
	n, aerr := s.q.AdvanceCheckpointRetentionTip(ctx, store.AdvanceCheckpointRetentionTipParams{
		RunID: runID, Ref: ref, Tip: tip,
	})
	if aerr != nil {
		slog.Warn("checkpoint retention: advance tip", "run", runID, "tip", tip, "error", aerr)
		return false, false
	}
	if n == 0 && s.terminalAtTrack(ctx, runID, terminal) {
		slog.Warn("checkpoint retention: a terminal run's publish is tracked by no record; left to the sweeper's "+
			"publish-attempt reconciliation", "run", runID, "branch", branch, "ref", ref, "tip", tip)
		return false, false
	}
	return false, true
}

// terminalAtTrack reports whether a publish that moved no retention row belongs to a terminal run:
// true when Publish routed it as terminal, else the run's CURRENT status is re-read, since a run
// live at Publish's top read may have turned terminal while its push was in flight. A read failure
// is logged at Warn and reports TRUE: an unknown status must be treated as untracked, so the
// push's attempt row is kept for the sweeper rather than cleared on a guess.
func (s *Service) terminalAtTrack(ctx context.Context, runID uuid.UUID, terminal bool) bool {
	if terminal {
		return true
	}
	run, err := s.q.GetRunByID(ctx, runID)
	if err != nil {
		slog.Warn("checkpoint retention: re-read run status for an untracked publish; treated as untracked", "run", runID, "error", err)
		return true
	}
	return terminalStatuses[run.Status]
}
