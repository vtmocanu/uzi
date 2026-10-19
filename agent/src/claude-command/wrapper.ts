// The runner-uid wrapper for a guarded model command (issue #2471, M1). The model command is
// a positional argument of a constant bash script and is never interpolated into it.
//
// argv: bash -c WRAPPER uzi-cmd <spillPath> <spillCapBytes> <command>
// so inside the script $0 is "uzi-cmd", $1 the spill path, $2 the spill cap and $3 the command.
//
// What the script does, in order:
//   - `set -o noclobber` makes the spill open fail on an existing file instead of reusing it;
//   - the writer `tee >( { head -c cap > spill; cat >/dev/null; } ); wait` forwards every byte
//     to the wrapper's stdout (the supervisor transport) and spills at most `cap` bytes, then
//     keeps draining, so a full cap or a failed spill open never SIGPIPEs the command; its
//     trailing `wait` (the last process substitution, bash 5.1 or later) keeps the writer pid
//     alive until the spill sink has finished too, so the wrapper's bounded wait covers both;
//   - the wrapper's own stderr then goes to /dev/null, so bash job notices ("Killed") never
//     enter the output;
//   - the command runs as a FOREGROUND child with stdout and stderr on the writer and stdin
//     from /dev/null. Foreground, not `&` plus `wait`: an async command in a non-job-control
//     shell inherits ignored SIGINT/SIGQUIT (which breaks `timeout -s INT`), and the `set -m`
//     alternative with `wait -f` used about one core in the wrapper while the command was
//     stopped (measured on bash 5.3). The foreground wait is exercised by the stopped-command
//     test in claude-command-launcher.test.ts, and `$?` is the status (128+n for a signal);
//   - the wrapper then closes its own copy of the writer fd, so the writer sees EOF as soon as
//     every descendant holding the fd has closed it, and gives the writer a bounded 5 s
//     (50 x 0.1 s) to flush the tail. A descendant that keeps the fd open past that can have
//     its later bytes cut by the worker; the wrapper never waits on it unboundedly.

/** Needs bash 5.1 or later: the writer's trailing `wait` joins the last process substitution. */
export const WRAPPER = [
  "set -o noclobber",
  'spill=$1',
  'cap=$2',
  'cmd=$3',
  'mkdir -p -- "$(dirname -- "$spill")" 2>/dev/null',
  'exec {w}> >(tee >( { head -c "$cap" 2>/dev/null > "$spill"; cat >/dev/null; } ); wait)',
  'writer=$!',
  'exec 2>/dev/null',
  '/bin/bash -c "$cmd" >&"$w" 2>&1 </dev/null',
  'rc=$?',
  'exec {w}>&-',
  'for _ in $(seq 50); do kill -0 "$writer" 2>/dev/null || break; sleep 0.1; done',
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
