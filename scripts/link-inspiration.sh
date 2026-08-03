#!/usr/bin/env bash
# Populate this worktree's gitignored inspiration/ with symlinks to the three
# prior-art repos, cloning them once if they are not on the machine yet.
#
# The three (bottega, multica, dot-agent-deck) were vendored as git submodules
# under inspiration/ until 2026-08-03. They are ordinary clones outside the repo
# now, shared by every worktree, so this script is the per-worktree setup step
# that replaces `git submodule update --init`.
#
#   ./scripts/link-inspiration.sh                       # link (clone if absent)
#   UZI_PRIOR_ART_DIR=/some/where ./scripts/link-inspiration.sh
#
# Idempotent: re-running relinks and never re-clones. Safe to run from any
# worktree; each gets its own links because inspiration/ is gitignored.
set -euo pipefail

BASE="${UZI_PRIOR_ART_DIR:-$HOME/repos/external}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEST="$REPO_ROOT/inspiration"

# name<TAB>url
PROJECTS=$(
	cat <<-'EOF'
		bottega	https://github.com/vdaubry/bottega.git
		multica	https://github.com/multica-ai/multica.git
		dot-agent-deck	https://github.com/vfarcic/dot-agent-deck.git
	EOF
)

# Refuse to run against a tree that still tracks inspiration/ as submodules --
# the links would collide with real checkouts and `git status` would go noisy.
if git -C "$REPO_ROOT" ls-files --error-unmatch .gitmodules >/dev/null 2>&1; then
	echo "refusing: $REPO_ROOT still tracks .gitmodules (pre-2026-08-03 tree)" >&2
	exit 1
fi

mkdir -p "$BASE" "$DEST"

while IFS=$'\t' read -r name url; do
	[ -n "$name" ] || continue
	src="$BASE/$name"
	if [ ! -d "$src/.git" ]; then
		echo "cloning $name -> $src"
		git clone --quiet "$url" "$src"
	fi
	# A leftover REAL directory here is the normal state in any worktree that
	# still had the submodules checked out when they were unvendored. `ln -sfn`
	# would silently create the link INSIDE it ($DEST/$name/$name) instead of
	# replacing it, so refuse and say what to do -- deleting someone's tree
	# unasked is not this script's call.
	link="$DEST/$name"
	if [ -d "$link" ] && [ ! -L "$link" ]; then
		echo "refusing: $link is a real directory (leftover submodule checkout)." >&2
		echo "          Remove it first:  rm -rf '$link'" >&2
		exit 1
	fi
	ln -sfn "$src" "$link"
	printf '%-16s -> %s\n' "$name" "$src"
done <<<"$PROJECTS"

cat <<'EOF'

Linked. Two things that will otherwise cost you an hour:

  * `rg` and `grep -r` do NOT follow symlinked directories, so a repo-wide sweep
    silently returns nothing from here. Search it by explicit path
    (`rg <pattern> inspiration/`) or with `rg -L`. A clean sweep is NOT evidence
    that the prior art lacks the thing you searched for.
  * inspiration/ is gitignored on purpose (absolute paths off this machine).
    Never `git add -f` it, and never rely on it inside a worker container --
    uzi's own agents have no host filesystem, so they clone from the URLs above.
EOF
