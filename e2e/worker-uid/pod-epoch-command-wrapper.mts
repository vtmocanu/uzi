// Shared by the image fixture and its host regression. All inputs are controlled
// test paths/programs; quote them so fixture paths cannot become shell syntax.
function quote(value: string): string { return "'" + value.replaceAll("'", "'\\''") + "'"; }

export function commandWrapper(options: {
  binary: string; symbol: string; check: string; log: string; node?: string;
}): string {
  const { binary, symbol, check, log, node = "/usr/local/bin/node" } = options;
  return `#!/bin/sh
check_posture() {
  ${quote(node)} -e ${quote(check)} && return 0
  printf '%s\\n' POSTURE_VIOLATION >> ${quote(log)}
  return 1
}
check_posture || exit 1
printf '%s\\n' "${symbol} $*" >> ${quote(log)}
rc=0
${quote(binary)} "$@" || rc=$?
check_posture || exit 1
exit "$rc"
`;
}
