#!/usr/bin/env bash
# changelog-union.sh — resolve a CHANGELOG.md rebase conflict by keeping BOTH sides, the
# answer for a shared append-only list where every PR adds a bullet under the same heading.
#
# Usage: changelog-union.sh [--collapse] [FILE]      (default: CHANGELOG.md)
#
# Every conflict block keeps its first side, then its second; a diff3 base section
# (`|||||||` .. `=======`) is dropped. FAIL-CLOSED: when a bullet, a continuation line or a
# `## [...]` heading appears on BOTH sides of one conflict block, the union would duplicate
# it, so the helper refuses and leaves the file for a human (land-prep then stops with exit
# 5). A `## ` heading anywhere inside a conflict block (either side or the base) refuses too:
# a version-heading conflict is release territory. Each block must carry a diff3 base section (land-prep rebases with
# merge.conflictStyle=diff3): a non-blank base line missing from either side means that side
# deleted or REWORDED it (a later commit editing its own earlier bullet), and a union would
# keep both wordings, so that refuses too, as does a block with no base section. A shared `### <Section>` subsection heading is allowed (each side bringing its own
# `### Fixed` is the common case) only when the block sits inside `## [Unreleased]`: there
# a repeated `### <Section>` is collapsed into its first occurrence (its bullets move under
# it). Every blank line between a section's entries is kept byte-identical. Only a join the
# union itself makes gets a chosen gap: the first side's last line against the second side's
# first gets the larger of the two sides' own blank runs there, or, when neither has one,
# [Unreleased]'s convention; a folded repeat's bullets against its first occurrence's always
# get the convention (the
# majority of item pairs each side already had; on a tie the nearest pair before the join,
# else blank when any pair is blank-separated, else none). A file with no conflict markers
# is left untouched.
#
# --collapse: for a marker-free file (a clean rebase can leave two `### Fixed` headings when a
# commit adds its own next to the base's). Only a repeated `### <Section>` inside
# `## [Unreleased]` changes: the repeat's heading and surrounding blank lines go, its lines
# are appended to the first occurrence's (after the convention's gap, as above); every other
# line stays byte-identical. No repeat, no change. Conflict markers are refused (exit 1).
#
# Verification before the file is replaced: no marker survives, the multiset of content
# lines (non-blank, non-marker, non-heading) from the resolved sides is identical before and
# after, each paired with the `## ` section and `### ` subsection heading it sits under (so
# no line changes section), the set of distinct headings is unchanged, and no `## [<version>]` heading
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

# The separator convention of [Unreleased]: sepcount(s, k) reads the lines of one version
# of the file (stream k) and counts adjacent list items (a bullet or its indented
# continuation, then a bullet) that a blank line separates vs. that touch; last[k] is the
# newest pair's kind (1 blank, 0 touching, "" none since the last heading). The union feeds
# its first side to stream "o" and its second to "t", the unconflicted text to both, so
# only pairs one side's own file had vote, never a join the resolution makes.
SEP_AWK='
  function sepcount(s, k) {
    if (s ~ /^[ \t]*$/) { gap[k] = 1; return }
    if (s ~ /^#/) { pl[k] = 0; gap[k] = 0; last[k] = ""; return }
    if (s ~ /^[-*] / && pl[k]) { if (gap[k]) nblank++; else ntight++; last[k] = (gap[k] ? 1 : 0) }
    pl[k] = (s ~ /^[-*] / || s ~ /^  /); gap[k] = 0
  }
  # joinstyle(near): 1 = one blank line at a join, 0 = none.
  function joinstyle(near) {
    if (nblank != ntight) return (nblank > ntight)
    if (near != "") return near + 0
    return (nblank > 0)
  }
'

# A line pass 1 prints between the two sides of a block and pass 2 always removes; its text
# cannot be a CHANGELOG line, and the verification below would catch one that survived.
SENTINEL='<!-- changelog-union:join -->'

if [ "$MODE" = collapse ]; then
cp "$FILE" "$tmpd/before" || exit 2
awk "$SEP_AWK"'
  function blank(s) { return s ~ /^[ \t]*$/ }
  # lastpair(s): the kind of the newest item pair inside section s (1 blank, 0 touching, "").
  function lastpair(s,   i, x, p, g, r) {
    r = ""; p = 0; g = 0
    for (i = 1; i <= sc[s]; i++) {
      x = S[s, i]
      if (blank(x)) { g = 1; continue }
      if (x ~ /^[-*] / && p) r = (g ? 1 : 0)
      p = (x ~ /^[-*] / || x ~ /^  /); g = 0
    }
    return r
  }
  { L[++n] = $0 }
  END {
    u = 0
    for (i = 1; i <= n; i++) if (L[i] ~ /^## \[Unreleased\]/) { u = i; break }
    if (u == 0) { for (i = 1; i <= n; i++) print L[i]; exit 0 }
    e = n + 1
    for (i = u + 1; i <= n; i++) if (L[i] ~ /^## /) { e = i; break }
    ns = 0; np = 0; dup = 0
    for (i = u + 1; i < e; i++) sepcount(L[i], "f")
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
      any = (hi > 0); sep = joinstyle(lastpair(s))
      for (t = s + 1; t <= ns; t++) {
        if (owner[t] != s) continue
        lo = 1; th = sc[t]
        while (lo <= th && blank(S[t, lo])) lo++
        while (th >= lo && blank(S[t, th])) th--
        if (lo > th) continue
        if (hi == 0) { print ""; hi = -1 }  # the first occurrence had no body
        else if (any && sep) print ""       # blank-separated entries keep one blank at the join
        for (i = lo; i <= th; i++) print S[t, i]
        any = 1
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
# Between the sides of a block in [Unreleased] it prints a JOIN sentinel line (pass 2
# replaces it with the join's gap) and records, in $tmpd/sep, the votes and each join's
# nearest earlier pair.
if ! awk -v before="$tmpd/before" -v sepfile="$tmpd/sep" -v sentinel="$SENTINEL" "$SEP_AWK"'
  BEGIN { st = 0 }  # 0 outside, 1 first side, 2 diff3 base, 3 second side
  st == 0 && /^## / { unrel = ($0 ~ /^## \[Unreleased\]/) }
  /^<<<<<<<( |$)/ { if (st != 0) { bad = "nested <<<<<<< at line " NR; exit 1 } st = 1; next }
  /^[|]{7}( |$)/  { if (st != 1) { bad = "stray ||||||| at line " NR; exit 1 } st = 2; hasbase = 1; next }
  /^=======$/     {
    if (st != 1 && st != 2) { bad = "stray ======= at line " NR; exit 1 }
    st = 3
    if (unrel) { nj++; near[nj] = (last["o"] != "" ? last["o"] : last["t"]); print sentinel " " nj }
    next
  }
  /^>>>>>>>( |$)/ {
    if (st != 3) { bad = "stray >>>>>>> at line " NR; exit 1 }
    # A line on both sides would be written twice: refuse rather than guess a dedupe. Only
    # a `### ` subsection heading may be shared, inside [Unreleased], where pass 2 folds the
    # repeat into its first occurrence.
    if (!hasbase) { bad = "conflict ending at line " NR " has no diff3 base section (rebase with -c merge.conflictStyle=diff3)"; exit 1 }
    # A `## ` version heading anywhere in a block is release territory, never an auto-union
    # (and it would make the [Unreleased] tracking below unreliable for later blocks).
    for (i = 1; i <= na; i++) if (A[i] ~ /^## /) { bad = "a `## ` heading inside the conflict ending at line " NR ": " A[i]; exit 1 }
    for (i = 1; i <= nb; i++) if (B[i] ~ /^## /) { bad = "a `## ` heading inside the conflict ending at line " NR ": " B[i]; exit 1 }
    for (i = 1; i <= nc; i++) if (C[i] ~ /^## /) { bad = "a `## ` heading inside the conflict ending at line " NR ": " C[i]; exit 1 }
    split("", seen); split("", seenb)
    for (i = 1; i <= nb; i++) if (B[i] !~ /^[ \t]*$/) seenb[B[i]] = 1
    for (i = 1; i <= na; i++) if (A[i] !~ /^[ \t]*$/) seen[A[i]] = 1
    for (i = 1; i <= nc; i++) if (C[i] !~ /^[ \t]*$/ && !((C[i] in seen) && (C[i] in seenb))) {
      bad = "a side deletes or rewords a line of the conflict ending at line " NR ": " C[i]; exit 1
    }
    for (i = 1; i <= nb; i++) if (B[i] !~ /^[ \t]*$/ && (B[i] in seen)) {
      if (B[i] ~ /^### / && unrel) continue
      bad = "line on both sides of the conflict ending at line " NR ": " B[i]; exit 1
    }
    st = 0; na = 0; nb = 0; nc = 0; hasbase = 0; next
  }
  st == 2 { C[++nc] = $0; next }
  st == 1 { A[++na] = $0 }
  st == 3 { B[++nb] = $0 }
  unrel && st != 3 { sepcount($0, "o") }
  unrel && st != 1 { sepcount($0, "t") }
  { print; print > before }
  END {
    if (bad != "") { print "changelog-union: refusing: " bad > "/dev/stderr"; exit 1 }
    if (st != 0) { print "changelog-union: unterminated conflict block" > "/dev/stderr"; exit 1 }
    print "votes", nblank + 0, ntight + 0 > sepfile
    for (j = 1; j <= nj; j++) print "join", j, near[j] > sepfile
  }
' "$FILE" > "$tmpd/union"; then
  echo "changelog-union: $FILE left untouched" >&2
  exit 1
fi

# Pass 2: collapse repeated `### ` headings inside `## [Unreleased]` and trim each
# section's body. Blank lines are copied as they are, except at a join: a JOIN sentinel
# (between the two sides of a block) or a folded repeat's start. There the blank runs on
# either side of the join become one run: a JOIN keeps the longer of the two, so no side
# loses a separator it had; a join with no blank line on either side, and every folded
# repeat, gets joinstyle()'s gap.
awk -v sepfile="$tmpd/sep" -v sentinel="$SENTINEL" "$SEP_AWK"'
  function isblank(s) { return s ~ /^[ \t]*$/ }
  function emit_body(h,   i, lo, hi, k, pend, a, injoin, jkind, jid, n, prevc) {
    lo = 1; hi = cnt[h]
    while (lo <= hi && (isblank(B[h, lo]) || B[h, lo] ~ ("^" sentinel " "))) lo++
    while (hi >= lo && (isblank(B[h, hi]) || B[h, hi] ~ ("^" sentinel " "))) hi--
    pend = 0; injoin = 0; prevc = ""
    for (i = lo; i <= hi; i++) {
      if (isblank(B[h, i])) { pend++; continue }
      if (B[h, i] ~ ("^" sentinel " ")) {
        # a = the blanks before the join; blanks after it count from zero again.
        if (!injoin) { a = pend; pend = 0 }
        injoin = 1; jkind = B[h, i]; continue
      }
      if (injoin) {
        jid = jkind; sub(/.* /, "", jid)
        if (jid == "C") n = (prevc == "" ? 0 : joinstyle(""))
        else { n = (a > pend ? a : pend); if (n == 0 && prevc != "") n = joinstyle(near[jid]) }
        pend = n; injoin = 0
      }
      for (k = 0; k < pend; k++) print ""
      pend = 0
      print B[h, i]; prevc = B[h, i]
    }
  }
  BEGIN {
    while ((getline ln < sepfile) > 0) {
      split(ln, f, " ")
      if (f[1] == "votes") { nblank = f[2] + 0; ntight = f[3] + 0 }
      else if (f[1] == "join") near[f[2]] = f[3]
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
        else { B[idx[key], ++cnt[idx[key]]] = sentinel " C" }  # a folded repeat: a join
        cur = idx[key]; continue
      }
      if (cur == 0) { if (L[i] !~ ("^" sentinel " ")) print L[i] }
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
# Each content line with the `## ` and `### ` headings it sits under (trailing blanks trimmed).
content() {
  awk '{ t = $0; sub(/[ \t]+$/, "", t) }
    t ~ /^## / { h2 = t; h3 = ""; next }
    t ~ /^### / { h3 = t; next }
    t ~ /^#/ || t == "" { next }
    { print h2 " | " h3 " | " $0 }' "$1" | LC_ALL=C sort
}
headings() { grep -E '^#' "$1" | sed -E 's/[[:space:]]+$//' | LC_ALL=C sort -u; }
if ! diff <(content "$tmpd/before") <(content "$tmpd/out") > "$tmpd/content.diff"; then
  echo "changelog-union: content lines or their sections would change (< lost, > gained); $FILE left untouched:" >&2
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
