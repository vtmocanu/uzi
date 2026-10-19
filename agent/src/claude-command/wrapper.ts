// The runner-uid wrapper for a guarded model command (issue #2471, M1). The model command is
// a positional argument of a constant bash script and is never interpolated into it.
//
// argv: bash -c WRAPPER uzi-cmd <spillPath> <spillCapBytes> <command> [<graceTenths>]
// so inside the script $0 is "uzi-cmd", $1 the spill path, $2 the spill cap, $3 the command and
// the optional $4 the writer grace in tenths of a second (see below).
//
// What the script does, in order:
//   - any pre-existing path at the spill location (a file, FIFO, device, symlink, or a dangling
//     symlink) is refused by redirecting the spill to /dev/null: the output is then not
//     spilled, but the command still runs, drains and keeps its status. `set -o noclobber` stays
//     as a second guard for a regular file that appears between the check and the open; it does
//     NOT stop a FIFO or a device from being opened, which is why the explicit check exists;
//     Residual race: the existence check runs before the asynchronous spill open, so any
//     concurrent command at the same runner uid can plant a FIFO at a live spill path inside
//     that window and stall that command until its deadline. That includes another command of
//     this run and a sibling run on a worker with more than one concurrent run (RUNNER_UID is
//     shared), and procfs exposes the supervisor argv, spill path included. The stall is
//     bounded by the stalled command's deadline and never reaches the worker, which never
//     opens the spill path;
//   - the spill's parent directory must also be physically where the path names it
//     (`cd -P` plus `pwd` equal to the dirname, so the caller passes a path built from the
//     resolved worktree): model code at the same uid can replace `.uzi`, `.uzi/scratch` or
//     `.uzi/scratch/uzi-cmd` with a symlink to another writable directory, and the unredacted
//     spill would then land outside the scratch area, possibly in a tracked path. Any mismatch
//     (or an uncreatable parent) redirects the spill to /dev/null like the final-component
//     refusal. `mkdir -p` runs BEFORE this physical-path check, so when an ancestor of the spill
//     directory is a symlink the empty directories are still created at its target (no spill
//     file is written there). A parent swapped after this check, before the asynchronous spill
//     open, is a different race from the FIFO stall above: at worst it misplaces the capped raw
//     output of the model's own command, which same-uid model code could already write anywhere
//     it can write, and it does not stall anything;
//   - the writer `tee >( { head -c cap > spill; cat >/dev/null; } ); wait` forwards every byte
//     to the wrapper's stdout (the supervisor transport) and spills at most `cap` bytes, then
//     keeps draining, so a full cap or a failed spill open never SIGPIPEs the command; its
//     trailing `wait` (the last process substitution, bash 5.1 or later) keeps the writer pid
//     alive until the spill sink has finished too, so the wrapper's bounded wait covers both;
//   - the wrapper's own stderr then goes to /dev/null, so bash job notices ("Killed") never
//     enter the output;
//   - the command runs as a FOREGROUND child with stdout and stderr on the writer, stdin from
//     /dev/null and the wrapper's own writer fd closed (`{w}>&-` on the command's redirection
//     list closes the inherited copy for the child only). Without that, every descendant of the
//     command would inherit the writer pipe as an extra fd and a background process would hold
//     the transport open. Foreground, not `&` plus `wait`: an async command in a non-job-control
//     shell inherits ignored SIGINT/SIGQUIT (which breaks `timeout -s INT`), and the `set -m`
//     alternative with `wait -f` used about one core in the wrapper while the command was
//     stopped (measured on bash 5.3). The foreground wait is exercised by the stopped-command
//     test in claude-command-launcher.test.ts, and `$?` is the status (128+n for a signal);
//   - the wrapper then closes its own copy of the writer fd, so the writer sees EOF as soon as
//     every descendant holding the fd has closed it, and gives the writer a bounded grace to
//     flush the tail: at most 50 iterations of the external `sleep 0.1` (5 s by default). The
//     loop is bounded by count, so a missing `sleep` shortens the grace rather than hanging.
//     A descendant that keeps the fd open past the grace can have its later bytes cut by the
//     worker; the wrapper never waits on it unboundedly.
//   - `$4` is a test-only knob (digits, capped at 50, default 50); production `wrapperArgv`
//     never passes it.

/** Needs bash 5.1 or later: the writer's trailing `wait` joins the last process substitution. */
export const WRAPPER = [
  "set -o noclobber",
  'spill=$1',
  'cap=$2',
  'cmd=$3',
  'grace=${4:-50}',
  '[[ $grace =~ ^[0-9]{1,3}$ ]] || grace=50',
  '(( 10#$grace > 50 )) && grace=50',
  'dir=$(dirname -- "$spill")',
  'mkdir -p -- "$dir" 2>/dev/null',
  'if [[ -e $spill || -L $spill || "$(cd -P -- "$dir" 2>/dev/null && pwd)" != "$dir" ]]; then spill=/dev/null; fi',
  'exec {w}> >(tee >( { head -c "$cap" 2>/dev/null > "$spill"; cat >/dev/null; } ); wait)',
  'writer=$!',
  'exec 2>/dev/null',
  '/bin/bash -c "$cmd" >&"$w" 2>&1 </dev/null {w}>&-',
  'rc=$?',
  'exec {w}>&-',
  'for ((i = 0; i < 10#$grace; i++)); do kill -0 "$writer" 2>/dev/null || break; sleep 0.1; done',
  'exit "$rc"',
  "",
].join("\n");

export interface WrapperArgvSpec {
  /** Absolute spill file path (worker-minted; nothing model-supplied goes into it). */
  readonly spillPath: string;
  /** Maximum bytes written to the spill file. */
  readonly spillCapBytes: number;
  /** The model command text. */
  readonly command: string;
}

/** The child argv the supervisor runs for a guarded command. */
export function wrapperArgv(spec: WrapperArgvSpec): string[] {
  return ["/bin/bash", "-c", WRAPPER, "uzi-cmd", spec.spillPath, String(spec.spillCapBytes), spec.command];
}
