// Command stub-child is the PRD #1156 M3a validation stub: a tiny STATIC binary the
// isolated Codex supervisor launches in place of the real app-server. It exists ONLY to
// exercise the supervisor's channel-boundary, disposal and process-tree controls under
// the real hardened worker image; it ships nothing and encodes no production behavior.
//
// It is deliberately a compiled STATIC Go binary (not a shell script): it runs on syscalls
// alone, so it depends on no external command and no dynamic loader, and it can dump the
// child's environment byte-for-byte. The supervisor forwards its OWN environment to the
// child (launchChild sets Env: os.Environ()), which under the isolated launcher is exactly
// the replaced-env allowlist (fresh HOME/CODEX_HOME/XDG/TMPDIR/PATH + the provider
// credential) that setpriv passed through unchanged — so the "env" mode EVIDENCES that this
// allowlist, and nothing from the host/worker, is what reaches the child.
//
// Modes (argv: stub-child <mode> <markerDir>):
//
//	marker      touch <md>/launched then block — proves whether the child was EVER
//	            forked (used by the fail-before-fork profile controls, where it must NOT).
//	probe-fd    as the runner uid, try to read /proc/<supervisor>/fd/3 and /fd/4 and
//	            record the result — the nondumpability channel-boundary control.
//	grandchild  record own pid/pgid, then fork a setsid'd grandchild in its OWN process
//	            group that outlives its parent's group — the ECHILD+__WALL disposal control.
//	gc-sleep    the grandchild body (re-exec of this binary): record pid/pgid and block.
//	env         dump the child's environment and its length — evidences env DELIVERY (the
//	            replaced-env allowlist HOME/CODEX_HOME/XDG/TMPDIR/PATH + the provider
//	            credential reach the child, with no host/worker leak) for the fresh-tree control.
package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"
)

func writeFile(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "stub-child: write %s: %v\n", path, err)
	}
}

func touch(path string) { writeFile(path, "") }

// pgid returns the caller's process-group id (own pgrp).
func pgid() int {
	p, err := syscall.Getpgid(0)
	if err != nil {
		return -1
	}
	return p
}

// blockForever keeps the process alive (killable by the supervisor's drain) without
// tripping Go's "all goroutines are asleep" deadlock detector — a timer keeps one live.
func blockForever() {
	for {
		time.Sleep(30 * time.Second)
	}
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: stub-child <mode> <markerDir>")
		os.Exit(2)
	}
	mode, md := os.Args[1], os.Args[2]

	switch mode {
	case "marker":
		touch(md + "/launched")
		blockForever()

	case "probe-fd":
		sup := os.Getppid()
		out := ""
		for _, fd := range []int{3, 4} {
			path := fmt.Sprintf("/proc/%d/fd/%d", sup, fd)
			if _, err := os.ReadFile(path); err == nil {
				out += fmt.Sprintf("fd%d rc=0 READABLE %s\n", fd, path)
			} else {
				out += fmt.Sprintf("fd%d rc=1 %v\n", fd, err)
			}
		}
		writeFile(md+"/probe.txt", out)
		touch(md + "/probe-done")
		blockForever()

	case "grandchild":
		writeFile(md+"/child.pid", strconv.Itoa(os.Getpid()))
		writeFile(md+"/child.pgid", strconv.Itoa(pgid()))
		if self, err := os.Executable(); err == nil {
			attr := &syscall.ProcAttr{
				Files: []uintptr{0, 1, 2},
				Sys:   &syscall.SysProcAttr{Setsid: true}, // new session => distinct pgid
			}
			if _, ferr := syscall.ForkExec(self, []string{self, "gc-sleep", md}, attr); ferr != nil {
				fmt.Fprintf(os.Stderr, "stub-child: fork grandchild: %v\n", ferr)
			}
		}
		touch(md + "/child-ready")
		blockForever()

	case "gc-sleep":
		writeFile(md+"/grandchild.pid", strconv.Itoa(os.Getpid()))
		writeFile(md+"/grandchild.pgid", strconv.Itoa(pgid()))
		touch(md + "/grandchild.ready")
		blockForever()

	case "env":
		body := ""
		for _, e := range os.Environ() {
			body += e + "\n"
		}
		writeFile(md+"/child-env.txt", body)
		writeFile(md+"/child-env-count", strconv.Itoa(len(os.Environ())))
		touch(md + "/env-done")
		blockForever()

	default:
		fmt.Fprintf(os.Stderr, "stub-child: unknown mode %q\n", mode)
		os.Exit(2)
	}
}
