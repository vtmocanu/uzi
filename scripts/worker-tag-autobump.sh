#!/usr/bin/env bash
# Release-time hosted-worker image auto-bump.
#
# WHAT / WHY. Since PRD #422 the hosted-worker image tag (deploy/chart/values.yaml
# `workers.image.tag`) is DECOUPLED from Chart.appVersion: an app-only release does NOT
# roll the fleet. That decoupling is deliberate -- an api/web/chart change must not churn
# every worker or interrupt in-flight runs (adr/0422-decouple-worker-version.md) -- but
# it left one manual step: when a release genuinely changes the AGENT image, someone had
# to bump `workers.image.tag` by hand, and forgetting it silently shipped stale agents.
#
# This script removes that toil WITHOUT reintroducing per-release churn. Run it while
# cutting a release: it bumps `workers.image.tag` to the release version IFF the agent
# image's RUNTIME surface actually changed since the currently-pinned tag; otherwise it
# leaves the tag (and therefore the fleet) untouched. So:
#     agent/src, agent deps, agent toolchain or template changed -> bump  -> fleet rolls
#     api/web/controller/docs-only release                       -> leave -> zero roll
#
# THE RUNTIME SURFACE, and why it is NOT the whole build context. The agent Dockerfile's
# build context is the repo ROOT and it `COPY . /opt/uzi-src` (agent/templates/base/
# Dockerfile), so the built image differs byte-for-byte on EVERY release -- it bakes all
# of uzi's source. Defining "changed" as the full context would roll the fleet every
# release, exactly what #422 removed. The /opt/uzi-src bake is a self-reference snapshot
# the running agent can read (uzi's own code); a stale snapshot is benign for normal
# runs, which clone the TARGET repo fresh. So "the agent image changed in a way worth
# rolling for" is the set of paths that change how a worker EXECUTES a run:
#     agent/src  agent/package.json  agent/package-lock.json  agent/tsconfig.json
#     agent/bin  agent/templates     agent/devbox-global     agent/codex
# (agent/codex -- the pinned Codex package lock, its install/assert scripts and the Go
# supervisor the runtime stage builds -- was missing until #1720.)
# (A self-improve run DOES read /opt/uzi-src, but it is admin-only / off by default; if
# that path ever needs a fresh per-release snapshot, widen AGENT_PATHS deliberately --
# do not silently fold in the whole tree, which defeats the decoupling.)
#
# MODES.
#     worker-tag-autobump.sh <version> [ref]            edit: bump if the surface changed
#     worker-tag-autobump.sh --check <version> [ref]    report-only: exit 1 if a bump is
#                                                        owed but not applied (exit 0 clean)
# <version> is the release version (leading v optional). It keeps `workers.image.tag` and
# the decouple assert's PINNED_TAG in lockstep, so values.yaml stays the single pin and
# `task render:worker-tag-check` keeps passing.
#
# A pin naming a tag that is not PUBLISHED is never valid -- it names an image no release
# built (the 0.83.0-rc.7 incident, 2026-09-19). "Not published" means absent, OR present
# only as a LOCAL tag never pushed to origin (`git rev-parse` cannot tell those apart). We
# do NOT fail open on it: when origin answers we require the tag on origin, --check reports
# a bad pin (exit 1) and edit mode repins to the version being cut (which this release
# publishes) rather than shipping the dead pin again. Only when origin is UNREACHABLE do we
# fall back to the local check, never repinning blind offline.
#
# EXIT CODES (the convention scan-secrets.sh / assert-worker-tag-decoupled.sh set):
#     2 = the instrument is broken (not at repo root, tag unreadable, pin unparseable)
#     1 = --check only: a bump is owed but was not applied, or the pin names a missing tag
#     0 = done (bumped, repinned off a missing tag, nothing to bump, or --check clean)
set -euo pipefail

usage() { echo "usage: $0 [--check] <release-version> [ref]" >&2; exit 2; }

MODE=edit
if [ "${1:-}" = "--check" ]; then MODE=check; shift; fi
[ $# -ge 1 ] || usage
VERSION="${1#v}"
REF="${2:-HEAD}"

VALUES="deploy/chart/values.yaml"
ASSERT="scripts/assert-worker-tag-decoupled.sh"
[ -f "$VALUES" ] || { echo "worker-tag-autobump: $VALUES not found (run from repo root)" >&2; exit 2; }

# The agent runtime surface -- see the header. Space-separated, repo-relative.
AGENT_PATHS="agent/src agent/package.json agent/package-lock.json agent/tsconfig.json agent/bin agent/templates agent/devbox-global agent/codex"

# Read `workers.image.tag`: the `tag:` under the FIRST `image:` that is a direct (two-space)
# child of the top-level `workers:` key. workers.docker.image / workers.controller.image
# sit deeper (four-space `image:`) and are not matched.
read_pin() {
  awk '
    /^workers:/ { inw=1; next }
    inw && /^[^[:space:]]/ { inw=0 }
    inw && /^  image:/ { inimg=1; next }
    inw && inimg && /^  [^[:space:]]/ { inimg=0 }
    inw && inimg && /^    tag:[[:space:]]/ { v=$2; gsub(/"/,"",v); print v; exit }
  ' "$VALUES"
}

OLD="$(read_pin)"
[ -n "$OLD" ] || { echo "worker-tag-autobump: could not read workers.image.tag from $VALUES" >&2; exit 2; }

# apply_bump <new-tag>: rewrite workers.image.tag -> new-tag in values.yaml, keeping
# the decouple assert's PINNED_TAG in lockstep. Extracted so both the surface-changed
# path and the missing-pin-tag repair below share one edit.
apply_bump() {
  local new="$1"
  awk -v new="$new" '
    /^workers:/ { inw=1 }
    inw && /^[^[:space:]]/ && $0 !~ /^workers:/ { inw=0 }
    inw && /^  image:/ { inimg=1 }
    inw && inimg && /^  [^[:space:]]/ && $0 !~ /^  image:/ { inimg=0 }
    inw && inimg && /^    tag:[[:space:]]/ && !done {
      match($0, /^    tag:[[:space:]]*/)
      printf "%s\"%s\"\n", substr($0, 1, RLENGTH), new
      done = 1
      next
    }
    { print }
  ' "$VALUES" > "$VALUES.tmp" && mv "$VALUES.tmp" "$VALUES"

  if [ -f "$ASSERT" ]; then
    awk -v new="$new" '
      /^PINNED_TAG=/ {
        printf "PINNED_TAG=\"%s\"     # the concrete worker tag values.yaml must pin (kept in lockstep by scripts/worker-tag-autobump.sh)\n", new
        next
      }
      { print }
    ' "$ASSERT" > "$ASSERT.tmp" && mv "$ASSERT.tmp" "$ASSERT"
    chmod +x "$ASSERT"   # mv from a fresh temp drops the exec bit; the assert is a gate script
  fi
}

PREV_TAG="v$OLD"
# Is the pinned tag safe to diff the agent surface against? It must name a PUBLISHED
# image, and a LOCAL-only tag names none -- `git rev-parse` cannot tell a pushed tag from
# a local-only one (release-cut.sh's rc_tag_on_remote makes the same point), which is the
# second half of the 0.83.0-rc.7 failure (2026-09-19): an RC cut at tip whose tag was kept
# local-only, so release.yml never built agent-*:0.83.0-rc.7, yet the pin named it and
# every new hosted worker sat in ImagePullBackOff. So when origin ANSWERS, require the tag
# THERE (a local-only or absent tag is unpublished); only when origin is UNREACHABLE fall
# back to the local rev-parse, rather than repin blind offline.
tag_usable=0
rc=0; git ls-remote --exit-code --tags origin "refs/tags/$PREV_TAG" >/dev/null 2>&1 || rc=$?
case "$rc" in
  0) # On origin -> published. It must be local too, to diff against; after release-cut's
     # fetch it is. On origin but NOT in this clone is a broken instrument (a standalone
     # run with no tag fetch), not an unpublished pin: say so and stop (exit 2) rather than
     # repin with a wrong "not published" message and an unneeded fleet roll.
     if git rev-parse -q --verify "refs/tags/$PREV_TAG" >/dev/null 2>&1; then
       tag_usable=1
     else
       echo "worker-tag-autobump: $PREV_TAG is on origin but not in this clone, so the agent surface cannot be diffed. Run  git fetch --tags origin  and re-run." >&2
       exit 2
     fi
     ;;
  2) tag_usable=0 ;;                                                                     # origin reachable, tag NOT there -> unpublished / local-only
  *) git rev-parse -q --verify "refs/tags/$PREV_TAG" >/dev/null 2>&1 && tag_usable=1 ;;  # origin unreachable -> fall back to the local check
esac
if [ "$tag_usable" -eq 0 ]; then
  # The pin names no PUBLISHED image (absent everywhere, or present only locally). We
  # cannot diff the agent surface against it, so DO NOT fail open (the old behaviour left
  # the dead pin and re-shipped it on the next cut). The safe repair is unambiguous: point
  # the pin at the image THIS cut publishes.
  if [ "$OLD" = "$VERSION" ]; then
    # The pin already names the version being cut, which this release publishes.
    echo "worker-tag-autobump: $PREV_TAG is not published, but workers.image.tag already names the version being cut ($VERSION), which this release publishes -- nothing to do."
    exit 0
  fi
  if [ "$MODE" = check ]; then
    echo "worker-tag-autobump: FAIL -- workers.image.tag is $OLD but $PREV_TAG is not a published tag (absent, or present only locally), so the pin names an image no release published (new hosted workers would ImagePullBackOff)." >&2
    echo "  A pin must name a published (or being-cut) image. Run  scripts/worker-tag-autobump.sh $VERSION  to repin it to the version being cut." >&2
    exit 1
  fi
  apply_bump "$VERSION"
  echo "worker-tag-autobump: $PREV_TAG is not published (workers.image.tag named an unpublished image); repinned $OLD -> $VERSION (the version being cut, which this release publishes)."
  echo "  The hosted fleet WILL roll to $VERSION on deploy."
  exit 0
fi

# Did the agent runtime surface change since the pinned tag?
# shellcheck disable=SC2086  # AGENT_PATHS is an intentional word list of pathspecs
if git diff --quiet "$PREV_TAG..$REF" -- $AGENT_PATHS; then
  echo "worker-tag-autobump: agent runtime surface unchanged since $PREV_TAG; workers.image.tag stays pinned at $OLD (fleet will NOT roll)."
  exit 0
fi

# shellcheck disable=SC2086
CHANGED="$(git diff --name-only "$PREV_TAG..$REF" -- $AGENT_PATHS | head -5 | tr '\n' ' ')"

if [ "$OLD" = "$VERSION" ]; then
  echo "worker-tag-autobump: agent surface changed since $PREV_TAG, but workers.image.tag is already $VERSION -- nothing to do."
  exit 0
fi

if [ "$MODE" = check ]; then
  echo "worker-tag-autobump: FAIL -- the agent runtime surface changed since $PREV_TAG (${CHANGED}...) but workers.image.tag is still $OLD, not $VERSION." >&2
  echo "  Run  scripts/worker-tag-autobump.sh $VERSION  to bump it (rolls the fleet), or pin deliberately and skip this check." >&2
  exit 1
fi

# --- edit: bump workers.image.tag -> VERSION, keeping PINNED_TAG in lockstep -----------
apply_bump "$VERSION"

echo "worker-tag-autobump: agent runtime surface changed since $PREV_TAG (${CHANGED}...); bumped workers.image.tag $OLD -> $VERSION (+ PINNED_TAG in the decouple assert)."
echo "  The hosted fleet WILL roll to $VERSION on deploy, draining in-flight runs first (force-roll off, ${PREV_TAG} drain deadline bounds it)."
