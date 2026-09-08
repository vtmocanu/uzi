// Command uzi-codex-fileop is the race-safe, KERNEL-anchored file-operation helper
// for the isolated Codex adapter (PRD #1171 M2, the [B2] mechanism). It is a second
// command in the uzi.local/codex-supervisor module, distinct from the trusted
// process supervisor and built the same way (static, vendored, no VCS stamping).
//
// The helper is started with the worktree-root path and opens it ONCE to a dirfd
// (O_DIRECTORY|O_PATH). Every model-selected operation resolves its RELATIVE path
// with openat2 from that dirfd under
// RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS, so traversal cannot
// escape the worktree and a symlink/".."/magic-link swap cannot race the open. This
// is deliberately NOT a realpath-check-then-pathname-I/O pattern: there is no
// check-then-use window because the kernel resolves and opens atomically.
//
// # Wire protocol
//
// REQUEST (stdin, one JSON object per line, a line > 2 MiB is rejected E_OVERSIZE):
//
//	{"id":<int>,"op":"stat","path":"rel"}
//	{"id":<int>,"op":"read","path":"rel"}
//	{"id":<int>,"op":"write","path":"rel","data":"<base64>"}
//	{"id":<int>,"op":"mkdir","path":"rel"}
//	{"id":<int>,"op":"rename","path":"old","newPath":"new"}
//	{"id":<int>,"op":"unlink","path":"rel"}
//	{"id":<int>,"op":"rmdir","path":"rel"}
//	{"id":<int>,"op":"list","path":"rel"}
//
// RESPONSE (stdout, one JSON object per line):
//
//	{"id":<int>,"ok":true, ...op-specific fields...}
//	{"id":<int>,"ok":false,"code":"E_ESCAPE|E_SYMLINK|E_DENIED|E_OVERSIZE|..."}
//
// A malformed or oversized request yields a typed error and the server STAYS
// HEALTHY. Responses carry only bounded codes: never a raw errno string, a path,
// or file content in any log or on stderr.
//
// ARGV (trusted, caller-supplied, NEVER model-controlled):
//
//	uzi-codex-fileop --root <abs-worktree-root>
package main

import (
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run parses the trusted argv, opens the worktree root ONCE as an O_DIRECTORY|O_PATH
// dirfd, and services the request/response loop. Every failure is a static,
// path-free stderr line and a non-zero exit.
func run(args []string, in io.Reader, out io.Writer, errw io.Writer) int {
	root, err := parseRootArg(args)
	if err != nil {
		io.WriteString(errw, "fileop: invalid arguments\n")
		return 2
	}
	rootFD, oerr := unix.Open(root, unix.O_DIRECTORY|unix.O_PATH|unix.O_CLOEXEC, 0)
	if oerr != nil {
		io.WriteString(errw, "fileop: cannot open worktree root\n")
		return 2
	}
	defer unix.Close(rootFD)
	server := newServer(rootFD)
	// Fail before accepting requests if the load-bearing containment syscall is
	// unavailable or blocked. Serving first and discovering this per operation would
	// make the helper look healthy while every model effect fails.
	probeFD, perr := server.openBeneath(".", unix.O_PATH, 0)
	if perr != nil {
		if errors.Is(perr, unix.ENOSYS) {
			io.WriteString(errw, "fileop: openat2 unavailable\n")
		} else {
			io.WriteString(errw, "fileop: openat2 probe failed\n")
		}
		return 2
	}
	unix.Close(probeFD)
	if serr := serve(server, in, out); serr != nil {
		io.WriteString(errw, "fileop: transport error\n")
		return 1
	}
	return 0
}

// parseRootArg parses the trusted argv "--root <abspath>". The root must be an
// absolute path (the anchor is a real filesystem location, never model-relative).
func parseRootArg(args []string) (string, error) {
	if len(args) == 2 && args[0] == "--root" && strings.HasPrefix(args[1], "/") {
		return args[1], nil
	}
	return "", errBadArgs
}
