#!/bin/sh
# Gate repo-borne skills on SKILL.md BYTE SIZE, whole-file (PRD self-improve M2).
#
# usage: scripts/check-skill-size.sh <canary-file> [skills-root]
#   e.g. scripts/check-skill-size.sh scripts/skill-size-canary.txt .agents/skills
#
# 🔴 WHY THIS EXISTS. A repo skill whose SKILL.md exceeds the runtime byte cap is
# DROPPED SILENTLY at run time -- the worker never loads it, and nothing in CI or the
# tree says so. The enumerator's guard is a strictly-greater whole-file byte test:
# `if (fileStat.size > maxBytes)` at agent/src/repo-skills.ts:151, where maxBytes is the
# 65536-byte default (`SKILL_MAX_BYTES` in api/internal/config/config.go and
# `DEFAULT_SKILL_MAX_BYTES` in agent/src/skills-run.ts). So a skill author who lets a
# SKILL.md grow past 65536 bytes ships a skill that looks committed but never runs, and
# learns nothing until they wonder why the skill did not fire. This check makes that
# mechanical: it flags any SKILL.md whose `wc -c` size is strictly greater than the cap
# and instructs a split, so the drop reddens CI instead of vanishing at run time.
#
# 🔴 STRICTLY-GREATER, WHOLE-FILE BYTES -- MIRRORS THE RUNTIME EXACTLY. The predicate
# is `wc -c < file` (byte size, not character count) compared with `-gt` (strictly
# greater), matching repo-skills.ts:151 byte-for-byte. A file EXACTLY at the cap is
# accepted by the runtime and so is accepted here; only `> 65536` is a finding. The
# same predicate helper runs the canary and the corpus, so the two cannot drift in what
# "over cap" means.
#
# 🔴 THE .claude/skills SYMLINK IS NOT SCANNED SEPARATELY. `.claude/skills` is a symlink
# to `.agents/skills`, so scanning `.agents/skills/*/SKILL.md` covers every repo skill
# exactly once. Globbing `.claude/skills/*` too would follow the symlink and double-count.
#
# 🔴 A LIVENESS CANARY, BECAUSE A SILENT PASS IS THE FAILURE MODE. If the size predicate
# ever went blind (a bad edit to `over()`, a broken `wc` invocation), the corpus would
# read "0 over cap" and this gate would pass VACUOUSLY -- exactly the silent-drop shape
# it exists to catch. So the identical predicate is run FIRST over the canary file ($1)
# against a threshold one byte below the canary's own size, and it MUST fire, or the
# instrument is declared broken. This proves the `wc -c` + `-gt` mechanism is live
# WITHOUT committing a >64 KiB fixture. A clean run therefore PRINTS that the canary
# fired -- a positive observation, so a green here is not "the detector never looked".
#
# NO SKIP BRANCH AND NO *_REQUIRED ENV VAR, deliberately -- this check needs only
# sh/git/wc, which are always present (the asymmetry with lint-yaml.sh / lint-formula.sh
# is that those wrap brew/pip tools a contributor may lack).
#
# EXIT CODES (the check-migration-numbering.sh / check-spec-numbering.sh convention):
#     2 = the instrument is broken (the canary arg missing, the canary predicate did not
#         fire, or the corpus is missing/empty so a scan would be vacuous)
#     1 = there are findings (a SKILL.md over the cap)
#     0 = clean, and the canary was detected
# `task`'s own rc is 201 for all of them.
set -eu

if [ "$#" -lt 1 ]; then
  echo "usage: scripts/check-skill-size.sh <canary-file> [skills-root]" >&2
  echo "  e.g. scripts/check-skill-size.sh scripts/skill-size-canary.txt .agents/skills" >&2
  exit 2
fi

CANARY="$1"
SKILLS_ROOT="${2:-.agents/skills}"

# keep in sync with SKILL_MAX_BYTES default (api/internal/config/config.go) and
# DEFAULT_SKILL_MAX_BYTES (agent/src/skills-run.ts)
CAP=65536

# Run from the repo root whatever the caller's directory, like the sibling scripts.
ROOT="$(git rev-parse --show-toplevel)" || {
  echo "check-skill-size: not inside a git work tree (git rev-parse --show-toplevel failed)" >&2
  exit 2
}
cd "$ROOT" || {
  echo "check-skill-size: cannot cd to repo root: $ROOT" >&2
  exit 2
}

if [ ! -f "$CANARY" ]; then
  echo "check-skill-size: canary file not found: $CANARY" >&2
  echo "  The canary is what proves the size detector fires; without it a clean corpus" >&2
  echo "  cannot be told from a detector that measured nothing." >&2
  exit 2
fi

# 🔴 THE SIZE PREDICATE, USED BY BOTH PHASES. True iff $1's byte size (`wc -c`) is
# STRICTLY GREATER than the threshold $2 -- byte-for-byte the runtime's
# repo-skills.ts:151 test. `wc -c < file` (redirect, not a filename arg) yields a bare
# integer with no filename, so the arithmetic comparison is clean.
over() {
  [ "$(wc -c < "$1")" -gt "$2" ]
}

# 🔴 CANARY FIRST: prove the predicate fires before trusting any corpus verdict. The
# canary is a small file; assert it registers as "over" a threshold one byte below its
# own size. If the predicate does NOT fire there, `over()` is broken and the corpus
# verdict would be meaningless, so declare the instrument broken.
canary_bytes="$(wc -c < "$CANARY")"
if [ "$canary_bytes" -lt 1 ]; then
  echo "check-skill-size: INSTRUMENT BROKEN -- canary $CANARY is empty ($canary_bytes bytes)." >&2
  echo "  The canary must be non-empty so the tiny-threshold predicate has something to fire on." >&2
  exit 2
fi
if ! over "$CANARY" "$((canary_bytes - 1))"; then
  echo "check-skill-size: ================================================================" >&2
  echo "check-skill-size: INSTRUMENT BROKEN -- the size predicate did not fire on the" >&2
  echo "check-skill-size: canary ($CANARY, $canary_bytes bytes) against a threshold of" >&2
  echo "check-skill-size: $((canary_bytes - 1)) bytes." >&2
  echo "check-skill-size:" >&2
  echo "check-skill-size: The canary is smaller than nothing it should be over, which means the" >&2
  echo "check-skill-size: wc -c + -gt mechanism is broken. A \"clean\" corpus would then mean" >&2
  echo "check-skill-size: nothing. Fix over() or restore the canary." >&2
  echo "check-skill-size: ================================================================" >&2
  exit 2
fi

if [ ! -d "$SKILLS_ROOT" ]; then
  echo "check-skill-size: skills root not found: $SKILLS_ROOT -- nothing to scan." >&2
  echo "  Scanning an absent corpus is vacuous; treating as an instrument failure." >&2
  exit 2
fi

# Scan the real corpus. A glob with no match would leave the literal pattern; guard it.
any_file=0
for skill in "$SKILLS_ROOT"/*/SKILL.md; do
  [ -f "$skill" ] || continue
  any_file=1
  break
done
if [ "$any_file" -eq 0 ]; then
  echo "check-skill-size: no */SKILL.md under $SKILLS_ROOT -- nothing to scan." >&2
  echo "  Scanning an empty corpus is vacuous; treating as an instrument failure." >&2
  exit 2
fi

# Collect every SKILL.md over the cap, one `path (N bytes)` line each.
findings=""
scanned=0
for skill in "$SKILLS_ROOT"/*/SKILL.md; do
  [ -f "$skill" ] || continue
  scanned=$((scanned + 1))
  if over "$skill" "$CAP"; then
    bytes="$(wc -c < "$skill")"
    findings="${findings}${skill} (${bytes} bytes)
"
  fi
done

if [ -n "$findings" ]; then
  echo "check-skill-size: SKILL.md file(s) over the ${CAP}-byte runtime cap in $SKILLS_ROOT:" >&2
  printf '%s' "$findings" | while IFS= read -r line; do
    [ -n "$line" ] || continue
    echo "  ${line} exceeds cap ${CAP}" >&2
  done
  echo "" >&2
  echo "  A repo skill whose SKILL.md exceeds ${CAP} bytes is dropped SILENTLY at run" >&2
  echo "  time (agent/src/repo-skills.ts:151) -- the worker never loads it and nothing" >&2
  echo "  says so. The fix is to split detail into a sibling .md referenced from" >&2
  echo "  SKILL.md, keeping the SKILL.md under ${CAP} bytes." >&2
  exit 1
fi

echo "check-skill-size: clean -- $scanned SKILL.md file(s) under $SKILLS_ROOT, all within the ${CAP}-byte cap."
echo "check-skill-size: canary DETECTED in $CANARY ($canary_bytes bytes registered as over $((canary_bytes - 1)))"
echo "check-skill-size: -- the size detector is live, so this green is a positive observation rather"
echo "check-skill-size: than a check that never looked."
exit 0
