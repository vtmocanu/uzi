# shellcheck shell=bash
# sanitize.sh — the one renderer for UNTRUSTED text (PR comments, review bodies, thread
# comments, code-scanning messages, paths, logins). Sourced, never executed.
#
# Every such string is data from anyone who can comment on the PR. It is only ever handled
# inside jq and printed; never eval'd, sourced, executed or interpolated into a command.
#
# UNTRUSTED_JQ defines, for use as a prefix to a jq program:
#   untrusted_clean        strip ANSI CSI / OSC (incl. OSC 52 clipboard) / DCS / other ESC
#                          sequences, then every C0/C1 control byte and DEL and every
#                          invisible or reordering code point: soft hyphen U+00AD, U+034F,
#                          U+061C, U+180E, U+200B-U+200F, U+2028/U+2029, U+202A-U+202E,
#                          U+2060-U+2064, U+2066-U+206F, U+FE00-U+FE0F, U+FEFF, the TAG block
#                          U+E0000-U+E007F ("ASCII smuggling") and U+E0100-U+E01EF; collapse
#                          all whitespace to one space.
#   untrusted_excerpt($n)  untrusted_clean, capped at $n characters with a trailing "…".
#   untrusted_row          {kind,id,author,at,body} -> one printable line, prefixed with the
#                          fixed label "UNTRUSTED" so no fragment of it can read as a script's
#                          own output (a fake "RESULT=ready" lands mid-line). A body over 300
#                          characters is cut and labelled INCOMPLETE. It never shows the ack
#                          digest: a digest is only obtainable from the complete view.
#   untrusted_full_row     the COMPLETE sanitized body, uncapped, with the id as ID@DIGEST (the
#                          form ack-comments.sh takes); only ack-comments.sh --show prints it.
# sanitize_untrusted [N]   stdin text -> one sanitized line (cap N, default 300), with no
#                          label; for callers that print a single field.
# shellcheck disable=SC2016  # jq program text: every $ is a jq variable
UNTRUSTED_JQ='
def untrusted_invisible:
  . < 32 or . == 127 or (. >= 128 and . <= 159)
  or . == 173 or . == 847 or . == 1564 or . == 6158
  or (. >= 8203 and . <= 8207) or . == 8232 or . == 8233
  or (. >= 8234 and . <= 8238) or (. >= 8288 and . <= 8292) or (. >= 8294 and . <= 8303)
  or (. >= 65024 and . <= 65039) or . == 65279
  or (. >= 917504 and . <= 917631) or (. >= 917760 and . <= 917999);
def untrusted_clean:
  (if type == "string" then . elif . == null then "" else tostring end)
  | gsub("\u001b\\[[0-?]*[ -/]*[@-~]"; "")
  | gsub("\u001b\\][^\u0007\u001b]*(\u0007|\u001b\\\\)?"; "")
  | gsub("\u001b[PX^_][^\u001b]*(\u001b\\\\)?"; "")
  | gsub("\u001b[ -/]*[0-~]?"; "")
  | gsub("[\t\n\r\u000b\u000c]"; " ")
  | explode
  | map(select(untrusted_invisible | not))
  | implode
  | gsub("\\s+"; " ") | sub("^ "; "") | sub(" $"; "");
def untrusted_excerpt($n): untrusted_clean | if length > $n then .[0:$n] + "…" else . end;
def untrusted_head($id):
  "  UNTRUSTED [\(.kind|untrusted_excerpt(20)) \($id)] author=\(.author|untrusted_excerpt(60)) at=\((.at // "-")|untrusted_excerpt(160)) | ";
def untrusted_row:
  (.body|untrusted_clean) as $b
  | untrusted_head(.id|untrusted_excerpt(40))
    + (if ($b|length) > 300
       then $b[0:300] + "… [INCOMPLETE excerpt of \($b|length) chars: read it all with ack-comments.sh --show]"
       else $b end);
def untrusted_full_row:
  untrusted_head((.id|untrusted_excerpt(40)) + (if .digest then "@" + (.digest|untrusted_excerpt(20)) else "" end))
  + (.body|untrusted_clean);
'

sanitize_untrusted() { jq -Rrs --argjson n "${1:-300}" "$UNTRUSTED_JQ"' untrusted_excerpt($n)'; }
