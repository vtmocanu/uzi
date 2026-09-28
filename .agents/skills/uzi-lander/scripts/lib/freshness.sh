# shellcheck shell=bash
# freshness.sh — is this uzi-lander checkout behind origin/main?
#
# A lander running the scripts from a long-lived `main/` checkout runs whatever that tree
# holds, not what landed since: a stale copy once lacked a merged review gate. Sourced by
# takeover.sh, which prints the result as SKILL_SCRIPTS_STALE=<0|1|unknown> plus a hint.
#
# skill_scripts_stale DIR -> prints 0 (the skill dir matches origin/main), 1 (it differs:
# older, or a local edit), or unknown (not a git checkout, or no origin/main ref). Reads
# the local origin/main ref only: no fetch, so it is as fresh as the last fetch.
skill_scripts_stale() {
  local dir="$1" top rel
  top=$(git -C "$dir" rev-parse --show-toplevel 2>/dev/null) || { echo unknown; return 0; }
  git -C "$top" rev-parse -q --verify origin/main >/dev/null 2>&1 || { echo unknown; return 0; }
  rel=$(cd "$dir/.." && pwd -P); rel=${rel#"$(cd "$top" && pwd -P)"/}
  if git -C "$top" diff --quiet origin/main -- "$rel" 2>/dev/null; then echo 0; else echo 1; fi
}
