package main

import (
	"io"
	"time"

	"uzi.local/codex-supervisor/internal/safetree"
)

// --measure-cache bounds. Compiled in: the mode is advisory and a worker
// treats an incomplete answer as "unknown", so there is nothing to tune.
const (
	measureMaxEntries = 500_000
	measureMaxDepth   = 64
	measureWall       = 5 * time.Second
)

// Exit code of an incomplete --measure-cache.
const cacheExitIncomplete = 3

// cacheMeasureLine is --measure-cache's one line; every key is always present.
// state is "complete" or "incomplete"; reason is "" when complete, else a
// short fixed word. bytes and entries are the partial counts when incomplete.
type cacheMeasureLine struct {
	Event   string `json:"event"`
	State   string `json:"state"`
	Bytes   uint64 `json:"bytes"`
	Entries uint64 `json:"entries"`
	Reason  string `json:"reason"`
}

// measureCache measures the cache directory token under rootFd as uid with the
// compiled-in limits and prints one cache_measure line. It is read-only: it
// takes no flock and changes nothing, so it may run beside the holder.
func measureCache(rootFd int, token string, uid int, out io.Writer, now func() time.Time) int {
	return measureCacheLimits(rootFd, token, uid, out, safetree.Limits{
		MaxEntries: measureMaxEntries,
		MaxDepth:   measureMaxDepth,
		Deadline:   now().Add(measureWall),
	})
}

func measureCacheLimits(rootFd int, token string, uid int, out io.Writer, lim safetree.Limits) int {
	use, err := safetree.Measure(rootFd, token, uid, lim)
	line := cacheMeasureLine{Event: "cache_measure", State: "complete", Bytes: use.Bytes, Entries: use.Entries}
	code := cacheExitComplete
	if err != nil {
		line.State, line.Reason, code = "incomplete", safetree.Reason(err), cacheExitIncomplete
	}
	writeLine(out, line)
	return code
}
