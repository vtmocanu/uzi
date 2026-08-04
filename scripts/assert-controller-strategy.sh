#!/bin/sh
# Assert the controller Deployment rolls with `strategy: Recreate`.
#
# WHY THIS IS A GATE AND NOT A COMMENT (issue #224, A29.1/A31). The controller refuses
# to boot when a worker PVC exceeds its namespace's LimitRange ceiling
# (kube.ValidatePVCCeilings). That refusal is only useful if the refusing pod is the
# ONLY controller running. replicaCount is 1 and the pod declares NO readiness,
# liveness or startup probe, so under RollingUpdate -- k8s defaults maxUnavailable 0,
# maxSurge 1 -- the new pod is created while the old keeps running and the old is
# terminated only once the new is Available. A lowered ceiling makes the new pod exit
# at that check and CrashLoopBackOff, so it never becomes Available, and the OLD
# controller keeps reconciling indefinitely against a ceiling it was never told about.
# The check fires and is defeated, with a crash-looping pod beside it that reads like
# the guard working.
#
# THE FAILURE IS A ROLLOUT TOPOLOGY, NOT A RENDERED VALUE, so no Go test can see it:
# the controller Deployment is a chart template, and the coupling spans two files in
# two languages.
#
# The comment on the strategy field is NOT sufficient on its own, and that is measured
# rather than assumed: the previous one said the choice "buys quiet rather than
# correctness", so anyone adding HA would have read it, taken it at face value and
# switched. A guard has to survive the person with a reason to remove it.
#
# AWK, NOT A YAML PARSER, AND THAT IS FORCED RATHER THAN PREFERRED. The helm_chart CI
# job runs alpine/helm, which has NO python3 -- verified by running that exact pinned
# image, not inferred. Its awk is busybox awk, so this stays POSIX. The sibling
# assert-chart-render.sh is awk for the same reason.
#
# The usual objection to grepping YAML -- "which object did you match?" -- is removed
# by the CALLER passing a render of this ONE template (`helm template --show-only`),
# not by this script being clever. Read the invocation before trusting the check.
set -eu

RENDER="${1:?usage: assert-controller-strategy.sh <controller-deployment.yaml>}"

# A state machine over the two lines that matter, rather than a bare grep for
# `type: Recreate`: that string could sit under any key (a volume, a probe, a future
# updateStrategy), so matching it anywhere would pass on a Deployment whose *strategy*
# had changed. Require it as the `type:` belonging to the top-level `spec.strategy`.
awk '
  # `  strategy:` at exactly the spec level.
  /^  strategy:[[:space:]]*$/ { in_strategy = 1; next }
  # Comments and blank lines inside the block are skipped.
  in_strategy && /^[[:space:]]*#/ { next }
  in_strategy && /^[[:space:]]*$/ { next }
  in_strategy && /^    type:[[:space:]]*/ {
    line = $0
    sub(/^    type:[[:space:]]*/, "", line)
    gsub(/[[:space:]]*$/, "", line)
    seen = 1
    value = line
    in_strategy = 0
    next
  }
  # Any line NOT indented at least four spaces ends the block without a type:.
  # Written with substr rather than an interval like /^[ ]{0,3}[^ ]/ -- busybox awk,
  # which is what the alpine/helm CI image ships, does NOT support interval
  # expressions and treats {0,3} as literal text, so that form silently never
  # matches. Verified by running this script in that exact pinned image.
  in_strategy && substr($0, 1, 4) != "    " { in_strategy = 0 }
  END {
    # An absent strategy block is NOT a pass. A render that never declared one means
    # this assertion checked nothing -- the "OK over zero documents" shape this repo
    # treats as an instrument failure rather than a result. It also catches a caller
    # that passed the wrong file.
    if (!seen) {
      print "FAIL: no spec.strategy.type found in the rendered controller Deployment,"
      print "      so this assertion verified NOTHING. Either the render is not the"
      print "      controller Deployment, or the strategy block was removed -- in which"
      print "      case k8s defaults to RollingUpdate, which is the failure below."
      exit 1
    }
    if (value != "Recreate") {
      printf "FAIL: controller Deployment strategy.type is %s, want Recreate.\n\n", value
      print "  Recreate is LOAD-BEARING FOR CORRECTNESS here, not a preference, and the"
      print "  coupling is invisible from the file you just edited."
      print ""
      print "  The controller refuses to boot when a worker PVC exceeds its LimitRange"
      print "  ceiling (kube.ValidatePVCCeilings, issue #224). That helps only if the"
      print "  refusing pod is the ONLY controller. replicaCount is 1 and the pod has NO"
      print "  probes, so RollingUpdate creates the new pod while the OLD one keeps"
      print "  running and only retires it once the new is Available. A pod that exits at"
      print "  the ceiling check CrashLoopBackOffs and is never Available -- so the OLD"
      print "  controller runs indefinitely, holding the OLD ceiling, against the NEW"
      print "  LimitRange the same upgrade already applied."
      print ""
      print "  Recreate makes that failure fail-STOPPED (no controller runs at all)."
      print "  RollingUpdate makes it fail-STALE (an old controller keeps provisioning"
      print "  claims the cluster will reject), which is the silent stall #224 exists to"
      print "  remove -- reintroduced, with a crash-looping pod beside it that looks like"
      print "  the guard working."
      print ""
      print "  If you are adding HA: the controller cannot usefully run more than one"
      print "  replica anyway (it holds no lease, and two would reconcile one fleet"
      print "  against one namespace). That needs leader election first, and THAT is the"
      print "  change that would let this assertion be revisited."
      exit 1
    }
    print "OK: controller Deployment strategy is Recreate"
  }
' "$RENDER"
