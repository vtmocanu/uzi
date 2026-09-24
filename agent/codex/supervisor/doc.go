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
// lists), so the child inherits only stdio 0/1/2 even when the supervisor
// itself inherited a stray fd. When both fail it does not fork: the pre-fork
// abnormal "fd hygiene failed" (there is no numeric sweep, because
// RLIMIT_NOFILE does not bound fds that are already open).
//
// EXIT CODE: 0 iff a dispose reached state "drained"; non-zero on abnormal,
// unconfirmed or profile-fail.
//
// The mechanism is NON-DISABLEABLE: no flag switches off the subreaper/nondumpable
// establishment, the pre-fork profile verification, the descriptor closure, or
// the ECHILD+__WALL drain authority.
//
// # Standalone modes
//
// Three modes run instead of the supervisor when their flag is the FIRST
// argument. Each has its own strict, fixed-order argv, uses no fd 3/4, forks
// nothing, and writes JSON lines to STDOUT. Each first runs the fd hygiene
// described under DESCRIPTORS, then parses its argv, then requires
// getuid() == geteuid() == --expect-uid; any of those failing exits 2 with one
// error line.
//
//	uzi-codex-supervisor --reap-orphans --expect-uid <N> --cache-root <abs dir>
//	uzi-codex-supervisor --hold-cache   --expect-uid <N> --cache-root <abs dir> --cache-token <lowercase-uuid>
//	uzi-codex-supervisor --remove-cache --expect-uid <N> --cache-root <abs dir> --cache-token <lowercase-uuid>
//
// The cache root must be absolute, already clean and not "/"; the uid must be
// plain decimal.
//
// --reap-orphans removes the command tmps (/tmp/uzi-codex-command-<uuid>) and
// per-run caches (<cache root>/<uuid>) left behind by a retained cleanup, an
// unconfirmed drain, or a SIGKILLed supervisor or holder. Its principle: a held
// flock always protects, but a released one only makes a directory a
// CANDIDATE, because the lock follows the supervisor or holder process, not
// the command descendants that can outlive it (none inherits the close-on-exec
// lock fd). Deleting a candidate additionally needs a proof, taken from the
// kernel process table with the candidate's lock held, that no command user
// can be alive: no process in this PID namespace other than the reaper and its
// ancestor chain has --expect-uid as its real, effective, saved or filesystem
// uid. A command user runs as that uid with zero capabilities and
// no_new_privs, so it cannot leave it. Without the proof, the candidate is
// retained. The proof fails closed: it is "unknown" when the proc mount
// (from its self/mountinfo) has a hidepid other than 0/off or any subset=
// option (hidepid would hide a nondumpable same-uid process), when the proc
// root cannot be listed or its "self" is not this process, when a listed
// pid's status cannot be read or parsed (a pid that exited mid-scan is
// skipped), or when the scan passes 100000 pids; and it is "user_alive" when
// a matching process is found.
//
// A pass takes one proof first, reads at most 4096 entries of /tmp and of the
// cache root (a missing cache root is skipped; a present one must be a 0700
// directory owned by --expect-uid), and acts on at most 64 names
// that match exactly (uzi-codex-command-<lowercase uuid> in /tmp, a bare
// lowercase uuid in the cache root). For each, in order: a no-follow open and
// a pin that must be a directory owned by --expect-uid (else "foreign",
// untouched); flock LOCK_EX|LOCK_NB (EWOULDBLOCK is "live", any other error
// "retained"); with the lock held, a fresh proof (nothing is removed in a pass
// whose first proof was not held); then removal through the pin with a 60 s
// deadline, capped by a 5-minute pass budget. Closing the fd releases the
// lock. Its one line, with proof the result of the last proof taken:
//
//	{"event":"reap","scanned":<n>,"live":<n>,"removed":<n>,"retained":<n>,"foreign":<n>,"proof":"held"|"unknown"|"user_alive"}  // exit 0
//	{"event":"reap_error","reason":"fd_hygiene"|"args"|"uid"|"tmp_root"|"cache_root"|"list"}                                       // exit 2
//
// scanned is always live+removed+retained+foreign; a name gone before its
// open is not counted.
//
// --hold-cache runs for a whole run as the command uid. It requires the cache
// root to be a directory owned by --expect-uid with mode 0700, creates
// <root>/<token> 0700 with the subdirectories gomod, gocache and npm, takes
// LOCK_EX|LOCK_NB on it and rechecks that the name still names the pinned
// directory, then reports ready and reads STDIN lines until EOF. The only
// line it acts on is exactly {"op":"release","drained":true|false} (the last
// valid one wins; any other line, or one over 4 KiB, is ignored). At EOF it
// removes the tree only when that last release said drained:true, which is the
// worker's attestation that every command root using the cache drained;
// otherwise it retains it as "unattested". The lock is held until it exits.
//
//	{"event":"cache_ready","path":"<root>/<token>"}
//	{"event":"cache_cleanup","state":"removed","reason":""}                    // exit 0
//	{"event":"cache_cleanup","state":"retained","reason":"unattested"|<word>}  // exit 3
//	{"event":"cache_error","reason":"fd_hygiene"|"args"|"uid"|"root"|"create"|"subdir"|"lock"|"recheck"}  // exit 2
//
// --remove-cache is for a worker whose holder died mid-run once its own
// registry proved every command root drained: the worker attests, so it takes
// no process-table proof, but a held lock still means live. Its one line:
//
//	{"event":"cache_cleanup","state":"removed","reason":""}                    // exit 0
//	{"event":"cache_cleanup","state":"absent","reason":""}                     // exit 0: no such directory
//	{"event":"cache_cleanup","state":"retained","reason":"live"|<word>}        // exit 3
//	{"event":"cache_error","reason":"fd_hygiene"|"args"|"uid"|"root"}          // exit 2
//
// <word> is a tmpCleanup reason ("mismatch", "owner", "io", ...).
package main
