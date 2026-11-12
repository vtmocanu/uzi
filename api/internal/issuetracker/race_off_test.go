//go:build !race

package issuetracker

// raceEnabled reports whether the race detector is on; it changes allocation
// counts (sync.Pool drops entries), so allocation bounds read it.
const raceEnabled = false
