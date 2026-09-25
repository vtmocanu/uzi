package main

import (
	"errors"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"uzi.local/codex-supervisor/internal/safetree"
)

// Bounds of one reap pass. maxReapDirents bounds the entries examined between
// two batches, and maxReapCandidates the names one batch holds; the names a
// pass holds are those parsed from its one 8 KiB getdents buffer plus one
// batch (a candidate's removal walk adds bounded per-depth buffers, freed once
// it ends). Neither count limits how much of a root a pass reaches: a pass reads
// each root to its end unless the pass budget runs out first or a read error
// ends it (reap_error "list").
const (
	// maxReapDirents is the most directory entries one page examines.
	maxReapDirents = 4096
	// maxReapCandidates is the most matching names one batch holds; the batch
	// is acted on before the next page is read.
	maxReapCandidates = 64
	// reapCandidateBudget bounds one candidate's removal.
	reapCandidateBudget = 60 * time.Second
	// reapPassBudget bounds the whole pass; a candidate's removal deadline is
	// never later than the pass's.
	reapPassBudget = 5 * time.Minute
)

// Reap test seams: pinFromFd pins a candidate (a foreign owner can be
// injected); hookBeforeReapRemove runs with the lock held and the proof
// taken, right before RemoveBy; reapSeek and reapGetdents read a root;
// reapRemoveBy removes a candidate (its deadline argument can be observed and
// its error injected).
var (
	pinFromFd            = safetree.PinFromFd
	hookBeforeReapRemove func(parentFd int, name string)
	reapSeek             = unix.Seek
	reapGetdents         = unix.Getdents
	reapRemoveBy         = safetree.RemoveBy
)

// Per-candidate outcomes.
const (
	reapLive     = "live"
	reapRemoved  = "removed"
	reapRetained = "retained"
	reapForeign  = "foreign"
)

// reapResult is the reaper's one stdout line. Scanned is the number of
// candidates acted on, and always Live+Removed+Retained+Foreign. Truncated is
// true when the pass stopped before both present roots were read to their end
// (the pass budget ran out), or a candidate was retained with reason
// "deadline" (its own 60 s removal limit or the pass budget). A page boundary
// alone never sets it. It can be true even when every entry left unread would
// have been nothing to act on, because the budget ran out before the end of
// the root was seen. DirentsExamined is the number of directory entries read
// from the roots, "." and ".." excluded, so Scanned never exceeds it.
type reapResult struct {
	Event           string `json:"event"`
	Scanned         int    `json:"scanned"`
	Live            int    `json:"live"`
	Removed         int    `json:"removed"`
	Retained        int    `json:"retained"`
	Foreign         int    `json:"foreign"`
	Proof           string `json:"proof"`
	Truncated       bool   `json:"truncated"`
	DirentsExamined int    `json:"dirents_examined"`
	// lastReason is the reason of the candidate recorded last; reapWith reads
	// it to spot a "deadline" retention, so it is kept in production too.
	lastReason string
	// outcomes is each candidate's outcome, kept only when
	// recordReapOutcomes is set (tests), so a pass's memory does not grow
	// with the candidates it acts on; never serialized.
	outcomes []reapOutcome
}

// recordReapOutcomes keeps each candidate's outcome in reapResult.outcomes.
// Tests set it (TestMain, and TestReapKeepsNoOutcomesInProduction checks the
// production default is off); production leaves it off. A package global:
// a reaper test that changes it must not run in parallel.
var recordReapOutcomes bool

// reapOutcome is one candidate's outcome and, for "retained", its reason.
type reapOutcome struct {
	name   string
	state  string
	reason string
}

func (r *reapResult) record(name, state, reason string) {
	switch state {
	case reapLive:
		r.Live++
	case reapRemoved:
		r.Removed++
	case reapRetained:
		r.Retained++
	case reapForeign:
		r.Foreign++
	}
	r.Scanned++
	r.lastReason = reason
	if recordReapOutcomes {
		r.outcomes = append(r.outcomes, reapOutcome{name: name, state: state, reason: reason})
	}
}

// reapConfig is reap's inputs besides the two root fds.
type reapConfig struct {
	// uid is the command uid: the owner a candidate must have and the uid the
	// proof looks for.
	uid int
	// prove takes one fresh no-user proof.
	prove func() string
	now   func() time.Time
}

// isCommandTmpName matches exactly uzi-codex-command-<lowercase uuid>.
func isCommandTmpName(name string) bool {
	token, ok := strings.CutPrefix(name, commandTmpNamePrefix)
	return ok && validCleanupToken(token)
}

// reapRoot is one directory the reaper scans and the names it acts on.
type reapRoot struct {
	fd    int
	match func(name string) bool
}

type reapCandidate struct {
	parentFd int
	name     string
}

// reapBounds sizes one page (entries examined) and one batch (matching names
// held) of a pass.
type reapBounds struct {
	pageDirents     int
	batchCandidates int
}

// reap runs one orphan-reaping pass over tmpFd (command tmps) and cacheFd
// (per-run caches; -1 when the cache root is absent). It returns an error only
// when a root cannot be read; a name not yet read from it is then untouched,
// and the counts recorded before stay in the result.
//
// A held lock always protects. A released lock only makes a directory a
// candidate, because the lock follows the supervisor or holder process, not
// the command descendants that can outlive it: deletion additionally needs a
// fresh no-user proof taken with the candidate's lock held. Without it the
// candidate is retained.
//
// When the pass's first proof is not "held", no candidate is locked at all:
// each one that pins as ours is retained (reason "proof") without a flock, so
// a pass that cannot remove anything never contends with a concurrent setup's
// LOCK_NB (a supervisor or holder creating its directory would otherwise see
// EWOULDBLOCK). Such a pass therefore never reports "live". The worker must
// invoke the reaper only before launching runs (the no-user proof depends on
// it); this keeps even a misplaced pass from breaking a setup.
//
// Each root is read once, from its start, in pages of at most maxReapDirents
// entries, a page ending early once it holds maxReapCandidates matching names;
// that batch is acted on before the next page is read, and on the normal path
// a name read but not yet acted on is kept for the next page, never dropped.
// On a read error the current partial batch is left untouched (not acted on)
// and the pass ends with the error. There is no ceiling on the candidates a
// pass acts on: only the pass budget or a read error stops it. The budget is
// checked before each page and before each candidate; when it has run out the
// result is truncated and the rest of the pass (later roots included) is left
// untouched. A candidate retained with reason "deadline" (its removal hit its
// own 60 s limit or the pass budget) also makes the result truncated.
func reap(tmpFd, cacheFd int, cfg reapConfig) (reapResult, error) {
	return reapWith(tmpFd, cacheFd, cfg, reapBounds{pageDirents: maxReapDirents, batchCandidates: maxReapCandidates})
}

// reapWith is reap with the page and batch sizes as inputs.
func reapWith(tmpFd, cacheFd int, cfg reapConfig, bounds reapBounds) (reapResult, error) {
	res := reapResult{Event: "reap"}
	start := cfg.now()
	passDeadline := start.Add(reapPassBudget)
	res.Proof = cfg.prove()
	canRemove := res.Proof == proofHeld

	roots := []reapRoot{{fd: tmpFd, match: isCommandTmpName}}
	if cacheFd >= 0 {
		roots = append(roots, reapRoot{fd: cacheFd, match: validCleanupToken})
	}
	for _, root := range roots {
		cur, err := newDirentCursor(root.fd)
		if err != nil {
			return res, err
		}
		for !cur.done() {
			if !cfg.now().Before(passDeadline) {
				res.Truncated = true
				return res, nil
			}
			batch, err := nextBatch(cur, root.match, bounds, &res)
			if err != nil {
				return res, err
			}
			for _, name := range batch {
				if !cfg.now().Before(passDeadline) {
					res.Truncated = true
					return res, nil
				}
				state := reapOne(&res, reapCandidate{parentFd: root.fd, name: name}, cfg, canRemove, passDeadline)
				if state == reapRetained && res.lastReason == "deadline" {
					res.Truncated = true
				}
			}
		}
	}
	return res, nil
}

// nextBatch reads one page from cur: names until bounds.pageDirents entries
// are examined or bounds.batchCandidates of them match. It returns the
// matching names in directory order.
func nextBatch(cur *direntCursor, match func(string) bool, bounds reapBounds, res *reapResult) ([]string, error) {
	var batch []string
	for examined := 0; examined < bounds.pageDirents && len(batch) < bounds.batchCandidates; examined++ {
		name, ok, err := cur.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		res.DirentsExamined++
		if match(name) {
			batch = append(batch, name)
		}
	}
	return batch, nil
}

// direntCursor reads a directory once, front to back: it seeks to offset 0
// when made and never again, and keeps every name a getdents call returned
// until next hands it out.
type direntCursor struct {
	fd      int
	buf     []byte
	pending []string
	eof     bool
}

func newDirentCursor(fd int) (*direntCursor, error) {
	if _, err := reapSeek(fd, 0, unix.SEEK_SET); err != nil {
		return nil, err
	}
	return &direntCursor{fd: fd, buf: make([]byte, 8192)}, nil
}

// next returns the next name ("." and ".." are skipped), or ok false at the
// end of the directory. It calls getdents only when no read name is pending.
func (c *direntCursor) next() (string, bool, error) {
	for len(c.pending) == 0 {
		if c.eof {
			return "", false, nil
		}
		n, err := reapGetdents(c.fd, c.buf)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if n <= 0 {
			c.eof = true
			return "", false, nil
		}
		_, _, c.pending = unix.ParseDirent(c.buf[:n], -1, c.pending[:0])
	}
	name := c.pending[0]
	c.pending = c.pending[1:]
	return name, true, nil
}

// done reports that every name of the directory was handed out.
func (c *direntCursor) done() bool {
	return c.eof && len(c.pending) == 0
}

// reapOne classifies and, when proven safe, removes one candidate. It returns
// the recorded state, or "" when the name was gone before its open (nothing
// recorded).
func reapOne(res *reapResult, c reapCandidate, cfg reapConfig, canRemove bool, passDeadline time.Time) string {
	record := func(state, reason string) string {
		res.record(c.name, state, reason)
		return state
	}
	fd, err := safetree.OpenDirNoFollow(c.parentFd, c.name)
	if err != nil {
		switch {
		case errors.Is(err, unix.ENOENT):
			return "" // gone since the listing: nothing to count
		case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
			return record(reapForeign, "")
		default:
			return record(reapRetained, "io")
		}
	}
	// Closing the fd releases the lock taken below.
	defer func() { _ = unix.Close(fd) }()

	pin, err := pinFromFd(fd, cfg.uid)
	if err != nil {
		if errors.Is(err, safetree.ErrOwner) || errors.Is(err, safetree.ErrMismatch) {
			return record(reapForeign, "")
		}
		return record(reapRetained, safetree.Reason(err))
	}

	// Without a held first proof nothing can be removed: retain without
	// taking the lock, so a concurrent setup's LOCK_NB never fails on it.
	if !canRemove {
		return record(reapRetained, "proof")
	}

	if err := flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return record(reapLive, "")
		}
		return record(reapRetained, "lock")
	}

	// With the lock held, a fresh proof: a command descendant of the process
	// that held the lock may still be alive.
	res.Proof = cfg.prove()
	if res.Proof != proofHeld {
		return record(reapRetained, "proof")
	}

	now := cfg.now()
	if !now.Before(passDeadline) {
		return record(reapRetained, "deadline")
	}
	deadline := now.Add(reapCandidateBudget)
	if passDeadline.Before(deadline) {
		deadline = passDeadline
	}
	if hookBeforeReapRemove != nil {
		hookBeforeReapRemove(c.parentFd, c.name)
	}
	if err := reapRemoveBy(c.parentFd, c.name, pin, deadline); err != nil {
		return record(reapRetained, safetree.Reason(err))
	}
	return record(reapRemoved, "")
}
