package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"golang.org/x/sys/unix"

	"uzi.local/codex-supervisor/internal/safetree"
)

// cacheSubdirs are made inside a new per-run cache directory.
var cacheSubdirs = []string{"gomod", "gocache", "npm"}

// maxReleaseLine is the longest stdin line the holder parses; a longer one is
// ignored.
const maxReleaseLine = 4096

// Exit codes of the cache modes.
const (
	cacheExitRemoved  = 0
	cacheExitSetup    = 2
	cacheExitRetained = 3
)

// cacheCleanup reasons besides safetree.Reason's words.
const (
	// cacheReasonUnattested: no drained:true arrived before EOF, a
	// drained:false did, or stdin failed.
	cacheReasonUnattested = "unattested"
	// cacheReasonLive: --remove-cache found the lock held.
	cacheReasonLive = "live"
)

var errCacheRoot = errors.New("cache root is not a 0700 directory owned by the command uid")

// cacheErrorLine is a setup failure: reason is a short fixed word.
type cacheErrorLine struct {
	Event  string `json:"event"`
	Reason string `json:"reason"`
}

// cacheReadyLine reports the held per-run cache directory.
type cacheReadyLine struct {
	Event string `json:"event"`
	Path  string `json:"path"`
}

// cacheCleanupLine is the removal outcome: state "removed", "retained" or
// "absent"; reason "" when removed or absent.
type cacheCleanupLine struct {
	Event  string `json:"event"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

func writeLine(w io.Writer, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = w.Write(append(b, '\n'))
}

func cacheError(w io.Writer, reason string) int {
	writeLine(w, cacheErrorLine{Event: "cache_error", Reason: reason})
	return cacheExitSetup
}

func cacheCleanup(w io.Writer, state, reason string) int {
	writeLine(w, cacheCleanupLine{Event: "cache_cleanup", State: state, Reason: reason})
	if state == tmpCleanupRemoved || state == "absent" {
		return cacheExitRemoved
	}
	return cacheExitRetained
}

// openCacheRoot opens root without following a final symlink and requires a
// directory owned by uid with mode exactly 0700. The fd is close-on-exec.
func openCacheRoot(root string, uid int) (int, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || int(st.Uid) != uid || st.Mode&0o7777 != 0o700 {
		_ = unix.Close(fd)
		return -1, errCacheRoot
	}
	return fd, nil
}

// holdCache creates, locks and holds the per-run cache rootPath/token under
// rootFd, then waits on in for the worker's release. Its lines on out:
//
//	{"event":"cache_ready","path":"<root>/<token>"}
//	{"event":"cache_cleanup","state":"removed"|"retained","reason":"..."}
//	{"event":"cache_error","reason":"create"|"subdir"|"lock"|"recheck"}
//
// (runMode made the process nondumpable before this; see modeEnv.)
//
// It removes the tree only when, by EOF, a valid release line said
// drained:true and none said drained:false (the worker's attestation that
// every command root using the cache drained; see readRelease); a bare EOF,
// any drained:false or a stdin read error retains it as "unattested".
// The lock fd stays open, so the lock is held, until holdCache returns, which
// the process's exit follows. A setup failure after the create leaves the
// directory for the orphan reaper.
func holdCache(rootFd int, rootPath, token string, uid int, in io.Reader, out io.Writer) int {
	dirFd, pin, err := safetree.Create(rootFd, token, uid)
	if err != nil {
		return cacheError(out, "create")
	}
	defer func() { _ = unix.Close(dirFd) }()
	for _, sub := range cacheSubdirs {
		if err := unix.Mkdirat(dirFd, sub, 0o700); err != nil {
			return cacheError(out, "subdir")
		}
	}
	if err := flock(dirFd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return cacheError(out, "lock")
	}
	if hookAfterLock != nil {
		hookAfterLock(rootFd, token)
	}
	var st unix.Stat_t
	if err := unix.Fstatat(rootFd, token, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		st.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(st.Dev) != pin.Dev || st.Ino != pin.Ino {
		return cacheError(out, "recheck")
	}
	writeLine(out, cacheReadyLine{Event: "cache_ready", Path: rootPath + "/" + token})

	if !readRelease(in) {
		return cacheCleanup(out, tmpCleanupRetained, cacheReasonUnattested)
	}
	if err := safetree.Remove(rootFd, token, pin); err != nil {
		return cacheCleanup(out, tmpCleanupRetained, safetree.Reason(err))
	}
	return cacheCleanup(out, tmpCleanupRemoved, "")
}

// readRelease reads lines from in until EOF and reports whether the worker
// attested the drain: at least one valid release line said drained:true and
// none said drained:false. drained:false is STICKY: once a valid
// drained:false is seen, no later drained:true can flip it back. A read error
// other than EOF retains (false) whatever came before it. A line longer than
// maxReleaseLine, or anything but a release line, is ignored.
func readRelease(in io.Reader) bool {
	br := bufio.NewReaderSize(in, maxReleaseLine+1)
	drained, refused := false, false
	oversize := false
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// Discard the rest of this line.
			oversize = true
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return false
		}
		if !oversize && len(line) > 0 {
			if v, ok := parseRelease(line); ok {
				if v {
					drained = true
				} else {
					refused = true
				}
			}
		}
		oversize = false
		if err != nil {
			return drained && !refused
		}
	}
}

// parseRelease accepts exactly {"op":"release","drained":<bool>}: the two keys
// compared exactly, each exactly once (a repeated key is rejected, not
// resolved last-wins), no other key, nothing after the object.
func parseRelease(line []byte) (drained bool, ok bool) {
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	if len(line) > maxReleaseLine {
		return false, false
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false, false
	}
	var op string
	var sawOp, sawDrained bool
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return false, false
		}
		key, isKey := tok.(string)
		if !isKey {
			return false, false
		}
		switch {
		case key == "op" && !sawOp:
			if err := dec.Decode(&op); err != nil {
				return false, false
			}
			sawOp = true
		case key == "drained" && !sawDrained:
			// A JSON bool literal only (not a string, a number or null).
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return false, false
			}
			switch string(raw) {
			case "true":
				drained = true
			case "false":
				drained = false
			default:
				return false, false
			}
			sawDrained = true
		default:
			return false, false
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return false, false
	}
	// Nothing but whitespace after the object.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return false, false
	}
	if !sawOp || !sawDrained || op != "release" {
		return false, false
	}
	return drained, true
}

// removeCache removes the per-run cache token under rootFd for a worker that
// attests every command root using it drained (the holder died mid-run). It
// still refuses a held lock ("live"). Its one line is a cache_cleanup with
// state "removed", "retained" or "absent" (the directory does not exist).
func removeCache(rootFd int, token string, uid int, out io.Writer) int {
	fd, err := safetree.OpenDirNoFollow(rootFd, token)
	if err != nil {
		switch {
		case errors.Is(err, unix.ENOENT):
			return cacheCleanup(out, "absent", "")
		case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
			return cacheCleanup(out, tmpCleanupRetained, safetree.Reason(safetree.ErrMismatch))
		default:
			return cacheCleanup(out, tmpCleanupRetained, safetree.Reason(safetree.ErrIO))
		}
	}
	defer func() { _ = unix.Close(fd) }()
	pin, err := pinFromFd(fd, uid)
	if err != nil {
		return cacheCleanup(out, tmpCleanupRetained, safetree.Reason(err))
	}
	if err := flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return cacheCleanup(out, tmpCleanupRetained, cacheReasonLive)
		}
		return cacheCleanup(out, tmpCleanupRetained, safetree.Reason(safetree.ErrIO))
	}
	if err := safetree.Remove(rootFd, token, pin); err != nil {
		return cacheCleanup(out, tmpCleanupRetained, safetree.Reason(err))
	}
	return cacheCleanup(out, tmpCleanupRemoved, "")
}
