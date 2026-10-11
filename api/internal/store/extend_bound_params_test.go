package store_test

import "github.com/vtmocanu/uzi/api/internal/workersvc"

// Parameters the extend statements take for the technical total-time bound (#2735). They are
// what workersvc passes in production for the default config (RUN_WALL_CEILING 24h): the
// persisted-wall fallback (the ceiling), the one-time Stop finalize allowance reserve and
// workersvc.MaxRunWallSeconds (the worker timer's range in seconds). The constants are the real
// workersvc ones, so a change there moves these tests with it.
const (
	extendTestBaseWallUpper     = 86400
	extendTestFinalizeAllowance = workersvc.WallParkFinalizeSeconds
	extendTestMaxWall           = workersvc.MaxRunWallSeconds
)

// extendTestFreshRoom is the room an un-extended run with a small persisted wall has under the
// test parameters, written out as a literal (2147483 - 86400 - 1800) rather than recomputed.
const extendTestFreshRoom = 2059283
