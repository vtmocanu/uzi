package store_test

import "github.com/vtmocanu/uzi/api/internal/workersvc"

// Parameters the extend statements take for the technical total-time bound (#2735). They are
// what workersvc passes in production: the persisted-wall fallback (the 72h
// workersvc.MaxRunWallCeiling, the largest ceiling the configuration can have), the one-time Stop finalize allowance reserve and
// workersvc.MaxRunWallSeconds (the extension-admission limit in seconds). The constants are the real
// workersvc ones, so a change there moves these tests with it.
const (
	extendTestBaseWallUpper     = 259200
	extendTestFinalizeAllowance = workersvc.WallParkFinalizeSeconds
	extendTestMaxWall           = workersvc.MaxRunWallSeconds
)

// extendTestFreshRoom is the room an un-extended run with a small persisted wall has under the
// test parameters, written out as a literal (2147483 - 259200 - 1800) rather than recomputed.
const extendTestFreshRoom = 1886483
