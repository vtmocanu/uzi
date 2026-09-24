// Command uzi-codex-supervisor is the immutable, trusted process-supervision
// helper for the isolated Codex launcher (PRD #1156 M3a, Unit A).
//
// It is a STATICALLY LINKED Go binary (CGO_ENABLED=0, no dynamic loader, no
// /nix dependency), installed root-owned 0555 on the worker image so the
// untrusted same-uid runner cannot rewrite it. Under the PRD #51 A1 uid split,
// /nix is runner-writable, so the toolchain Python cannot be a trust anchor;
// this Go binary is baked into the read-only image layer and is the anchor.
//
// The mechanism is the one measured by the frozen M0 characterization fixture
// e2e/codex-m0/supervisor.py: establish + verify Linux child-subreaping before
// fork, then reap the whole per-root process tree with pidfds and
// Wait4(..., __WALL) until an observed ECHILD confirms the tree is empty. This
// production port deliberately does NOT copy the fixture's fixed 30-second
// lifetime, its noSignal control, its full-command-line snapshots, or its
// unbounded/sensitive exception strings; per-op deadlines are bounded and the
// only process detail exported is /proc/<pid>/comm.
//
// # Wire protocol
//
// CONTROL (fd 3, one JSON object per line, a line > 8192 bytes is abnormal):
//
//	{"op":"snapshot","id":<int>}
//	{"op":"dispose","id":<int>,"timeoutMs":<int>}   // clamped to a bounded max
//
// EVIDENCE (fd 4, one JSON object per line, each <= 65536 bytes, <= 256 lines):
//
//	{"event":"started",...}
//	{"event":"child_exit","code":<int>}
//	{"event":"snapshot","id":<int>,"processes":[{pid,ppid,pgid,comm}...]}
//	{"event":"dispose","id":<int>,"state":"drained",...[,"tmpCleanup":{...}]}
//	{"event":"dispose","id":<int>,"state":"unconfirmed","reason":"...",...}
//	{"event":"abnormal","reason":"<short sanitized>","cleanup":{...}[,"tmpCleanup":{...}]}
//
// The optional tmpCleanup field is {"state":"removed"|"retained","reason":"<word>"},
// where reason is "" when removed and otherwise one of the fixed words
// "mismatch", "owner", "deadline", "io", "absent" or "name" (never a path or
// errno text). It appears only when a --cleanup-token was given, and then only on
// the first event whose drain reached "drained": a drained dispose, or an
// abnormal event whose best-effort cleanup drained. "retained" leaves the tree
// on disk; "absent" means the pinned directory was gone, which is not success;
// "deadline" means the removal ran out of the drain's op deadline (less a small
// margin for the reply) and kept its partial progress.
// The cleanup outcome never changes the exit code.
//
// ARGV (trusted, caller-supplied, NEVER model-controlled):
//
//	uzi-codex-supervisor --expect-uid <N> [--cleanup-token <lowercase-uuid>] [--drop-controller-caps] -- <child-exec-abspath> [child args...]
//
// The optional token names only the fixed path /tmp/uzi-codex-command-<token>;
// it is not an arbitrary path. It authorizes three things. Creation: after
// the profile verification and before fork, the supervisor creates that
// directory 0700 through an O_NOFOLLOW fd on /tmp and pins its dev/ino; any
// failure is the pre-fork abnormal "command tmp setup failed". The liveness
// lock: it holds LOCK_EX on the directory fd (close-on-exec, so no child
// inherits it) until it exits, and rechecks that the name still names the
// pinned directory after locking. Removal: the tree is removed through fds
// against that pin, only after a drain confirmed no descendant is left. When
// the drain is unconfirmed the directory is left in place, because a live
// descendant may still use it.
// The cap-drop flag is used only for worker-uid durability roots: it clears the
// entrypoint's controller-only SETUID/SETGID before the unchanged zero-cap check.
//
// DESCRIPTORS: before opening anything, the supervisor marks every fd from 5
// upward close-on-exec (close_range; when that fails, each fd /proc/self/fd
// lists; when that fails too, each fd below the RLIMIT_NOFILE soft limit, capped
// at 1<<20), so the child inherits only stdio 0/1/2 even when the
// supervisor itself inherited a stray fd; a failure is the pre-fork abnormal
// "fd hygiene failed".
//
// EXIT CODE: 0 iff a dispose reached state "drained"; non-zero on abnormal,
// unconfirmed or profile-fail.
//
// The mechanism is NON-DISABLEABLE: no flag switches off the subreaper/nondumpable
// establishment, the pre-fork profile verification, the descriptor closure, or
// the ECHILD+__WALL drain authority.
package main
