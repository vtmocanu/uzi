# shellcheck shell=bash
# sanitize.sh — the one renderer for UNTRUSTED text (PR comments, review bodies, thread
# comments, code-scanning messages, paths, logins). Sourced, never executed.
#
# Every such string is data from anyone who can comment on the PR. It is only ever handled
# inside jq and printed; never eval'd, sourced, executed or interpolated into a command.
#
# UNTRUSTED_JQ defines, for use as a prefix to a jq program:
#   untrusted_clean        strip ANSI CSI / OSC (incl. OSC 52 clipboard) / DCS / other ESC
#                          sequences, then every C0/C1 control byte and DEL, the zero-width and
#                          bidi controls (U+200B-U+200F, U+202A-U+202E, U+2066-U+2069, U+FEFF,
#                          plus U+061C, U+2028, U+2029); collapse all whitespace to one space.
#   untrusted_excerpt($n)  untrusted_clean, capped at $n characters with a trailing "…".
#   untrusted_row          {kind,id,author,at,body} -> one printable line, prefixed with the
#                          fixed label "UNTRUSTED" so no fragment of it can read as a
#                          script's own output (a fake "RESULT=ready" lands mid-line).
# sanitize_untrusted [N]   stdin text -> one sanitized line (cap N, default 300), with no
#                          label; for callers that print a single field.
# shellcheck disable=SC2016  # jq program text: every $ is a jq variable
UNTRUSTED_JQ='
def untrusted_clean:
  (if type == "string" then . elif . == null then "" else tostring end)
  | gsub("\u001b\\[[0-?]*[ -/]*[@-~]"; "")
  | gsub("\u001b\\][^\u0007\u001b]*(\u0007|\u001b\\\\)?"; "")
  | gsub("\u001b[PX^_][^\u001b]*(\u001b\\\\)?"; "")
  | gsub("\u001b[ -/]*[0-~]?"; "")
  | gsub("[\t\n\r\u000b\u000c]"; " ")
  | explode
  | map(select(
      . >= 32 and . != 127 and (. < 128 or . > 159)
      and (. < 8203 or . > 8207)
      and (. < 8234 or . > 8238)
      and (. < 8294 or . > 8297)
      and . != 65279 and . != 1564 and . != 8232 and . != 8233))
  | implode
  | gsub("\\s+"; " ") | sub("^ "; "") | sub(" $"; "");
def untrusted_excerpt($n): untrusted_clean | if length > $n then .[0:$n] + "…" else . end;
def untrusted_row:
  "  UNTRUSTED [\(.kind|untrusted_excerpt(20)) \(.id|untrusted_excerpt(40))] author=\(.author|untrusted_excerpt(60)) at=\((.at // "-")|untrusted_excerpt(160)) | \(.body|untrusted_excerpt(300))";
'

sanitize_untrusted() { jq -Rrs --argjson n "${1:-300}" "$UNTRUSTED_JQ"' untrusted_excerpt($n)'; }
