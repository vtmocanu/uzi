package main

import (
	"errors"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"uzi.local/codex-supervisor/internal/safetree"
)

// Bounds of one reap pass.
const (
	// maxReapDirents is the most directory entries read from each root.
	maxReapDirents = 4096
	// maxReapCandidates is the most candidates one pass acts on in EACH root
	// (/tmp and the cache root have one cap each). A candidate that pins as
	// foreign, or is gone before its open, does not consume it.
	maxReapCandidates = 64
	// reapCandidateBudget bounds one candidate's removal.
	reapCandidateBudget = 60 * time.Second
	// reapPassBudget bounds the whole pass; a candidate's removal deadline is
	// never later than the pass's.
	reapPassBudget = 5 * time.Minute
)

// Reap test seams: pinFromFd pins a candidate (a foreign owner can be
// injected); hookBeforeReapRemove runs with the lock held and the proof
// taken, right before RemoveBy.
var (
	pinFromFd            = safetree.PinFromFd
	hookBeforeReapRemove func(parentFd int, name string)
)

// Per-candidate outcomes.
const (
	reapLive     = "live"
	reapRemoved  = "removed"
	reapRetained = "retained"
	reapForeign  = "foreign"
)

// reapResult is the reaper's one stdout line. Scanned is the number of
// candidates acted on, and always Live+Removed+Retained+Foreign.
type reapResult struct {
	Event    string `json:"event"`
	Scanned  int    `json:"scanned"`
	Live     int    `json:"live"`
	Removed  int    `json:"removed"`
	Retained int    `json:"retained"`
	Foreign  int    `json:"foreign"`
	Proof    string `json:"proof"`
	// outcomes is each candidate's outcome, for tests; never serialized.
	outcomes []reapOutcome
}

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
	r.outcomes = append(r.outcomes, reapOutcome{name: name, state: state, reason: reason})
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

// reap runs one orphan-reaping pass over tmpFd (command tmps) and cacheFd
// (per-run caches; -1 when the cache root is absent). It returns an error only
// when a root cannot be listed, before any candidate of that root is touched.
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
// Each root has its own cap of maxReapCandidates acted-on candidates; a name
// that pins as foreign (a symlink, a non-directory, a foreign owner) is
// counted as foreign without consuming it, and neither does a name gone
// before its open.
func reap(tmpFd, cacheFd int, cfg reapConfig) (reapResult, error) {
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
		names, err := listCandidates(root.fd, root.match, maxReapDirents)
		if err != nil {
			return res, err
		}
		acted := 0
		for _, n := range names {
			if acted >= maxReapCandidates {
				break
			}
			state := reapOne(&res, reapCandidate{parentFd: root.fd, name: n}, cfg, canRemove, passDeadline)
			if state != "" && state != reapForeign {
				acted++
			}
		}
	}
	return res, nil
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
	if err := safetree.RemoveBy(c.parentFd, c.name, pin, deadline); err != nil {
		return record(reapRetained, safetree.Reason(err))
	}
	return record(reapRemoved, "")
}

// listCandidates reads at most maxDirents entries of the directory fd and
// returns the names match accepts. It reads from offset 0.
func listCandidates(fd int, match func(string) bool, maxDirents int) ([]string, error) {
	if _, err := unix.Seek(fd, 0, unix.SEEK_SET); err != nil {
		return nil, err
	}
	buf := make([]byte, 8192)
	var names, out []string
	read := 0
	for read < maxDirents {
		n, err := unix.Getdents(fd, buf)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			break
		}
		_, _, names = unix.ParseDirent(buf[:n], -1, names[:0])
		for _, name := range names {
			if read >= maxDirents {
				break
			}
			read++
			if match(name) {
				out = append(out, name)
			}
		}
	}
	return out, nil
}
