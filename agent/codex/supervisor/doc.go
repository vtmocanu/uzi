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
//	{"event":"snapshot","id":<int>,"processes":[{pid,ppid,pgid,comm}...]}
//	{"event":"dispose","id":<int>,"state":"drained",...}
//	{"event":"dispose","id":<int>,"state":"unconfirmed","reason":"...",...}
//	{"event":"abnormal","reason":"<short sanitized>","cleanup":{...}}
//
// ARGV (trusted, caller-supplied, NEVER model-controlled):
//
//	uzi-codex-supervisor --expect-uid <N> -- <child-exec-abspath> [child args...]
//
// EXIT CODE: 0 iff a dispose reached state "drained"; non-zero on abnormal,
// unconfirmed or profile-fail.
//
// The mechanism is NON-DISABLEABLE: no flag switches off the subreaper/dumpable
// establishment, the pre-fork profile verification, the descriptor closure, or
// the ECHILD+__WALL drain authority.
package main
