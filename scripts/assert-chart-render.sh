#!/bin/sh
# Assert the rendered chart is a well-formed multi-document manifest.
#
# WHY THIS EXISTS (issue #149). A Go-template comment ending `*/ -}}` immediately
# before a `---` trims the newline and GLUES the document separator onto the
# preceding value:
#
#     - name: registry-robot-secret-uzi-workers---
#
# The separator is destroyed, so two objects merge into ONE YAML document with
# duplicate keys -- and every YAML parser silently keeps the LAST one. On
# dev-cluster that deleted the `uzi-workers` ServiceAccount and its pull-secret
# InfisicalSecret from the manifest, which made restricted-tier hosted workers
# unprovisionable for days while ArgoCD correctly reported Synced/Healthy: it was
# in sync with what the manifest actually declared.
#
# NOTHING ELSE CATCHES IT. `helm lint` passes, `helm template` exits 0, the
# rendered text still contains `kind: ServiceAccount` at column 0 so grep finds
# it, and a server-side dry-run applies the surviving object without complaint.
# The object is not malformed -- it is ABSENT, and only a parse reveals that.
#
# The check is on the SHAPE, not on a list of object names: exactly one `kind:`
# per document. That is the merge signature regardless of which objects collide,
# so it keeps working as the chart grows.
set -eu

RENDER="${1:?usage: assert-chart-render.sh <rendered.yaml>}"

# The glued separator itself first, because it names the exact line and the fix.
# Written without a negated bracket expression on purpose: `grep -E "[^-]---$"`
# does NOT match `foo---` under ugrep (verified 2026-07-27), which is what `grep`
# resolves to on some dev machines -- so that form would pass on a broken render.
if awk '/---$/ && $0 !~ /^---$/ && $0 !~ /^[[:space:]]*#/ && $0 !~ /^[- ]*$/ { print "  line " NR ": " $0; found=1 } END { exit !found }' "$RENDER"; then
  echo "FAIL: a document separator is glued to the end of a value (above)."
  echo "      A Go-template comment before it ends \`*/ -}}\`; the \`-}}\` eats the newline."
  echo "      Write \`*/}}\` when a \`---\` follows."
  exit 1
fi

# Then the general merge signature, which catches a collision however it arose.
awk '
  /^---$/ { doc++; kinds[doc] = 0; next }
  /^kind:/ { kinds[doc]++; if (kinds[doc] == 1) firstline[doc] = NR }
  END {
    bad = 0
    for (d in kinds) {
      if (kinds[d] > 1) {
        printf "FAIL: document %d holds %d `kind:` keys (first at line %d).\n", d, kinds[d], firstline[d]
        printf "      Two objects merged into one document, so a YAML parser keeps only the last\n"
        printf "      and the others are SILENTLY DELETED from the manifest.\n"
        bad = 1
      }
    }
    if (bad) exit 1
  }
' "$RENDER"

echo "OK: $(grep -c '^---$' "$RENDER") documents, one kind per document, no glued separators"
