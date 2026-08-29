#!/usr/bin/env bash
# backfill-uzi-labels.sh — ONE-TIME PRD #764 cutover migration.
#
# PRD #764 replaced uzi's two-gate run-eligibility model (an eligible label PLUS a
# PRD link / PRDLESS / waiver) with a SINGLE `uzi` label: an issue is uzi's to run
# iff it carries `uzi`. This script back-labels our own currently-runnable open
# issues so they keep running after the cutover — it ADDS the `uzi` label to every
# open issue that carries one of the OLD runnable labels (`PRD` or `bug`).
#
# SAFE TO RE-RUN. Every forge's "add label" verb is idempotent (adding a label an
# issue already carries is a no-op), so a second run adds nothing. It only ADDS
# `uzi`; it never removes a label.
#
# It is NOT a product feature — no admin endpoint, no CLI verb (PRD #764 "Out of
# scope"). A human/operator runs it once at cutover; it mutates the LIVE forge.
#
# Forge is auto-detected from `git remote get-url origin`:
#   github.com -> gh     (this checkout)
#   gitlab.*   -> glab
#   otherwise  -> tea    (Forgejo / Gitea)
# Override the forge with UZI_FORGE=gh|glab|tea, the new label with UZI_UZI_LABEL,
# and the old runnable labels with UZI_OLD_LABELS="PRD,bug".
set -euo pipefail

NEW_LABEL="${UZI_UZI_LABEL:-uzi}"
OLD_LABELS="${UZI_OLD_LABELS:-PRD,bug}"

forge="${UZI_FORGE:-}"
if [ -z "$forge" ]; then
  origin="$(git remote get-url origin 2>/dev/null || true)"
  case "$origin" in
    *github.com*) forge="gh" ;;
    *gitlab*) forge="glab" ;;
    *) forge="tea" ;;
  esac
fi

echo "Backfilling '${NEW_LABEL}' onto open issues carrying [${OLD_LABELS}] via '${forge}' ..."

# Collect the open-issue numbers carrying any of the old runnable labels.
numbers=""
IFS=',' read -r -a old_labels <<<"$OLD_LABELS"
for label in "${old_labels[@]}"; do
  label="$(printf '%s' "$label" | tr -d '[:space:]')"
  [ -n "$label" ] || continue
  case "$forge" in
    gh)
      # gh infers the repo from the checkout's origin remote.
      found="$(gh issue list --state open --label "$label" --limit 1000 --json number --jq '.[].number')"
      ;;
    glab)
      found="$(glab issue list --state opened --label "$label" -P 1000 -F json | jq -r '.[].iid')"
      ;;
    tea)
      found="$(tea issues list --state open --labels "$label" --output simple | awk '{print $1}' | tr -d '#')"
      ;;
    *)
      echo "unknown forge '${forge}' (set UZI_FORGE=gh|glab|tea)" >&2
      exit 2
      ;;
  esac
  numbers="${numbers}"$'\n'"${found}"
done

# De-duplicate and drop anything that is not a bare issue number.
uniq_numbers="$(printf '%s\n' "$numbers" | grep -E '^[0-9]+$' | sort -un || true)"

if [ -z "$uniq_numbers" ]; then
  echo "No open issues carry [${OLD_LABELS}]; nothing to back-label."
  exit 0
fi

count=0
while IFS= read -r n; do
  [ -n "$n" ] || continue
  echo "  #${n} += ${NEW_LABEL}"
  case "$forge" in
    gh) gh issue edit "$n" --add-label "$NEW_LABEL" >/dev/null ;;
    glab) glab issue update "$n" --label "$NEW_LABEL" >/dev/null ;;
    tea) tea issues edit "$n" --add-labels "$NEW_LABEL" >/dev/null ;;
  esac
  count=$((count + 1))
done <<<"$uniq_numbers"

echo "Done: added '${NEW_LABEL}' to ${count} open issue(s) (idempotent; safe to re-run)."
