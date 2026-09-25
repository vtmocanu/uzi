#!/usr/bin/env bash
# changelog-union.sh — resolve a CHANGELOG.md rebase conflict by keeping BOTH sides, the
# answer for a shared append-only list where every PR adds a bullet under the same heading.
#
# Usage: changelog-union.sh [--collapse] [FILE]      (default: CHANGELOG.md)
#
# Every conflict block keeps its first side, then its second; a diff3 base section
# (`|||||||` .. `=======`) is dropped. FAIL-CLOSED: when any non-blank line (bullet or
# heading) appears on BOTH sides of one conflict block, the union would duplicate it, so the
# helper refuses and leaves the file for a human (land-prep then stops with exit 5). Inside `## [Unreleased]`, a repeated `### <Section>`
# heading is collapsed into its first occurrence (its bullets move under it) and blank lines
# left between bullet items are dropped. A file with no conflict markers is left untouched.
#
# --collapse: for a marker-free file (a clean rebase can leave two `### Fixed` headings when a
# commit adds its own next to the base's). Only a repeated `### <Section>` inside
# `## [Unreleased]` changes: the repeat's heading and surrounding blank lines go, its lines
# are appended to the first occurrence's; every other line stays byte-identical. No repeat,
# no change. Conflict markers are refused (exit 1).
#
# Verification before the file is replaced: no marker survives, the multiset of content
# lines (non-blank, non-marker, non-heading) from the resolved sides is identical before and
# after, the set of distinct headings is unchanged, and no `## [<version>]` heading
# (`## [Unreleased]` included) occurs twice. Any miss leaves FILE untouched.
#
# Exit codes: 0 resolved / collapsed (or nothing to do), 1 verification failed or malformed
#             markers (or markers under --collapse),
#             2 usage / unreadable file.
set -uo pipefail

MODE=union
if [ "${1:-}" = --collapse ]; then MODE=collapse; shift; fi
FILE="${1:-CHANGELOG.md}"
[ $# -le 1 ] || { echo "usage: changelog-union.sh [--collapse] [FILE]" >&2; exit 2; }
[ -f "$FILE" ] && [ -r "$FILE" ] || { echo "changelog-union: cannot read $FILE" >&2; exit 2; }

MARKER_RE='^(<<<<<<<( |$)|>>>>>>>( |$)|[|]{7}( |$)|=======$)'
if [ "$MODE" = collapse ]; then
  if grep -Eq "$MARKER_RE" "$FILE"; then
    echo "changelog-union: --collapse refuses a file with conflict markers; $FILE left untouched" >&2; exit 1
  fi
elif ! grep -Eq "$MARKER_RE" "$FILE"; then
  echo "changelog-union: no conflict markers in $FILE; nothing to do"
  exit 0
fi

tmpd=$(mktemp -d "${TMPDIR:-/tmp}/changelog-union.XXXXXX") || exit 2
trap 'rm -rf "$tmpd"' EXIT

if [ "$MODE" = collapse ]; then
cp "$FILE" "$tmpd/before" || exit 2
awk '
  function blank(s) { return s ~ /^[ \t]*$/ }
  { L[++n] = $0 }
  END {
    u = 0
    for (i = 1; i <= n; i++) if (L[i] ~ /^## \[Unreleased\]/) { u = i; break }
    if (u == 0) { for (i = 1; i <= n; i++) print L[i]; exit 0 }
    e = n + 1
    for (i = u + 1; i <= n; i++) if (L[i] ~ /^## /) { e = i; break }
    ns = 0; np = 0; dup = 0
    for (i = u + 1; i < e; i++) {
      if (L[i] ~ /^### /) {
        key = L[i]; sub(/[ \t]+$/, "", key)
        if (!(key in first)) { first[key] = ns + 1 } else dup = 1
        ns++; hd[ns] = L[i]; owner[ns] = first[key]; sc[ns] = 0; continue
      }
      if (ns == 0) P[++np] = L[i]
      else S[ns, ++sc[ns]] = L[i]
    }
    if (!dup) { for (i = 1; i <= n; i++) print L[i]; exit 0 }
    for (i = 1; i <= u; i++) print L[i]
    for (i = 1; i <= np; i++) print P[i]
    for (s = 1; s <= ns; s++) {
      if (owner[s] != s) continue
      print hd[s]
      hi = sc[s]; while (hi >= 1 && blank(S[s, hi])) hi--
      for (i = 1; i <= hi; i++) print S[s, i]
      for (t = s + 1; t <= ns; t++) {
        if (owner[t] != s) continue
        lo = 1; th = sc[t]
        while (lo <= th && blank(S[t, lo])) lo++
        while (th >= lo && blank(S[t, th])) th--
        if (lo <= th && hi == 0) { print ""; hi = -1 }  # the first occurrence had no body
        for (i = lo; i <= th; i++) print S[t, i]
      }
      for (i = (hi > 0 ? hi : 0) + 1; i <= sc[s]; i++) print S[s, i]
    }
    for (i = e; i <= n; i++) print L[i]
  }
' "$FILE" > "$tmpd/out" || { echo "changelog-union: collapse failed; $FILE left untouched" >&2; exit 1; }
if cmp -s "$FILE" "$tmpd/out"; then
  echo "changelog-union: no duplicate headings under [Unreleased] in $FILE; nothing to do"
  exit 0
fi
else

# Pass 1: union the conflict blocks. Also emits, into $tmpd/before, every line the result
# must keep (both sides and the unconflicted text; the diff3 base is dropped on purpose).
if ! awk -v before="$tmpd/before" '
  BEGIN { st = 0 }  # 0 outside, 1 first side, 2 diff3 base, 3 second side
  /^<<<<<<<( |$)/ { if (st != 0) { bad = "nested <<<<<<< at line " NR; exit 1 } st = 1; next }
  /^[|]{7}( |$)/  { if (st != 1) { bad = "stray ||||||| at line " NR; exit 1 } st = 2; next }
  /^=======$/     { if (st != 1 && st != 2) { bad = "stray ======= at line " NR; exit 1 } st = 3; next }
  /^>>>>>>>( |$)/ {
    if (st != 3) { bad = "stray >>>>>>> at line " NR; exit 1 }
    # A line on both sides would be written twice: refuse rather than guess a dedupe.
    split("", seen)
    for (i = 1; i <= na; i++) if (A[i] !~ /^[ \t]*$/) seen[A[i]] = 1
    for (i = 1; i <= nb; i++) if (B[i] !~ /^[ \t]*$/ && (B[i] in seen)) {
      bad = "line on both sides of the conflict ending at line " NR ": " B[i]; exit 1
    }
    st = 0; na = 0; nb = 0; next
  }
  st == 2 { next }
  st == 1 { A[++na] = $0 }
  st == 3 { B[++nb] = $0 }
  { print; print > before }
  END {
    if (bad != "") { print "changelog-union: refusing: " bad > "/dev/stderr"; exit 1 }
    if (st != 0) { print "changelog-union: unterminated conflict block" > "/dev/stderr"; exit 1 }
  }
' "$FILE" > "$tmpd/union"; then
  echo "changelog-union: $FILE left untouched" >&2
  exit 1
fi

# Pass 2: collapse repeated `### ` headings inside `## [Unreleased]`, trim each section's
# body, and drop blank lines between list items (a bullet line or its indented
# continuation, followed after the blanks by another bullet).
awk '
  function islist(s) { return s ~ /^[-*] / || s ~ /^  / }
  function isbullet(s) { return s ~ /^[-*] / }
  function emit_body(h,   i, lo, hi, j, k, prev) {
    lo = 1; hi = cnt[h]
    while (lo <= hi && B[h, lo] ~ /^[ \t]*$/) lo++
    while (hi >= lo && B[h, hi] ~ /^[ \t]*$/) hi--
    prev = ""
    for (i = lo; i <= hi; i++) {
      if (B[h, i] ~ /^[ \t]*$/) {
        j = i; while (j <= hi && B[h, j] ~ /^[ \t]*$/) j++
        if (islist(prev) && isbullet(B[h, j])) { i = j - 1; continue }
        for (k = i; k < j; k++) print B[h, k]
        i = j - 1; continue
      }
      print B[h, i]; prev = B[h, i]
    }
  }
  { L[++n] = $0 }
  END {
    u = 0
    for (i = 1; i <= n; i++) if (L[i] ~ /^## \[Unreleased\]/) { u = i; break }
    if (u == 0) { for (i = 1; i <= n; i++) print L[i]; exit 0 }
    e = n + 1
    for (i = u + 1; i <= n; i++) if (L[i] ~ /^## /) { e = i; break }
    for (i = 1; i <= u; i++) print L[i]
    cur = 0; nh = 0
    for (i = u + 1; i < e; i++) {
      if (L[i] ~ /^### /) {
        key = L[i]; sub(/[ \t]+$/, "", key)
        if (!(key in idx)) { idx[key] = ++nh; hd[nh] = L[i]; cnt[nh] = 0 }
        else { B[idx[key], ++cnt[idx[key]]] = "" }  # a later occurrence joins after a gap the trim/blank rule removes
        cur = idx[key]; continue
      }
      if (cur == 0) print L[i]
      else B[cur, ++cnt[cur]] = L[i]
    }
    for (h = 1; h <= nh; h++) {
      print hd[h]; print ""
      emit_body(h)
      print ""
    }
    for (i = e; i <= n; i++) print L[i]
  }
' "$tmpd/union" > "$tmpd/out" || { echo "changelog-union: collapse failed; $FILE left untouched" >&2; exit 1; }
fi

# ---- verification ---------------------------------------------------------------------------
if grep -Eq "$MARKER_RE" "$tmpd/out"; then
  echo "changelog-union: conflict markers remain; $FILE left untouched" >&2; exit 1
fi
content() { grep -Ev '^[[:space:]]*$' "$1" | grep -Ev '^#' | LC_ALL=C sort; }
headings() { grep -E '^#' "$1" | sed -E 's/[[:space:]]+$//' | LC_ALL=C sort -u; }
if ! diff <(content "$tmpd/before") <(content "$tmpd/out") > "$tmpd/content.diff"; then
  echo "changelog-union: content lines would change (< lost, > gained); $FILE left untouched:" >&2
  cat "$tmpd/content.diff" >&2; exit 1
fi
if ! diff <(headings "$tmpd/before") <(headings "$tmpd/out") > "$tmpd/headings.diff"; then
  echo "changelog-union: headings would change; $FILE left untouched:" >&2
  cat "$tmpd/headings.diff" >&2; exit 1
fi
dup_versions=$(awk 'match($0, /^## \[[^]]*\]/) { k = substr($0, RSTART, RLENGTH); if (seen[k]++ == 1) print k }' "$tmpd/out")
if [ -n "$dup_versions" ]; then
  echo "changelog-union: the result repeats version heading(s) $(printf '%s' "$dup_versions" | tr '\n' ' '); $FILE left untouched" >&2
  exit 1
fi

cat "$tmpd/out" > "$FILE" || { echo "changelog-union: cannot write $FILE" >&2; exit 2; }
if [ "$MODE" = collapse ]; then echo "changelog-union: collapsed duplicate headings in $FILE"; else echo "changelog-union: resolved $FILE (both sides kept)"; fi
exit 0
