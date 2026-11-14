package main

// admin_run_debug.go holds the CLI side's shared pieces of the admin run debug exception
// (issue #4198): the hint printed when a foreign-run read is refused, and the read-only notice
// printed above a view of another user's data.

import (
	"fmt"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// adminRunDebugHint names what reading another user's run needs. It is printed on stderr after a
// 404 or 403 from a read that the admin run debug exception could have served; the original
// error is still returned, so the exit code is unchanged.
const adminRunDebugHint = "hint: reading another user's run needs the server flag UZI_ADMIN_RUN_DEBUG (chart api.adminRunDebug.enabled) and an admin_ro (uza_) token"

// adminRunDebugReadOnlyLine is printed once above a recovery view of another user's holds.
const adminRunDebugReadOnlyLine = "read-only admin debug view of another user's holds; discard and export are owner-only"

// isForeignReadRefusal reports whether err is the 404 or 403/401-class refusal a foreign-run
// read gets when admin run debug does not apply.
func isForeignReadRefusal(err error) bool {
	code := uzicli.ExitCodeFor(err)
	return code == uzicli.ExitNotFound || code == uzicli.ExitAuth
}

// hintOnForeignReadRefusal prints adminRunDebugHint to stderr when err is a foreign-read
// refusal, and returns err unchanged so callers can `return hintOnForeignReadRefusal(env, err)`.
func hintOnForeignReadRefusal(env Env, err error) error {
	if err != nil && isForeignReadRefusal(err) {
		_, _ = fmt.Fprintln(env.Stderr, adminRunDebugHint)
	}
	return err
}
