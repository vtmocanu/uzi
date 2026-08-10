# ADR-280: A deterministic bright-line screen gates seeded plans at create time

**Status**: Accepted
**Date**: 2026-08-10
**Deciders**: Vlad + agent team

## Context

A run seeded with `uzi run create --plan-file` writes `runs.plan_source = 'seeded'`.
The server derives `plan_approved` from that column
(`api/internal/workersvc/service.go:1719`,
`PlanApproved: ... || run.PlanSource == planSourceSeeded`), and the worker's runner
treats a pre-approved seeded run as already having a human-owned plan
(`agent/src/sdk-executor.ts:730-745`): it skips both the Phase-1 planning turn and
the approval gate and goes straight to implementation. That skip is deliberate and
correct for its stated purpose — the plan is the user's own, supplied through an
authenticated create call, so there was never a gate to skip in the first place.

But it means a seeded plan that names something it should never name — a cloud
instance metadata endpoint, the cluster's own API server, an in-pod credential
mount — previously reached implementation with nothing standing between it and the
worker except the lead agent's own discretion. That is a soft, model-dependent
backstop, not a control: nothing forced the agent to notice, and nothing
enforced the outcome if it did.

## Decision

Add `api/internal/planpolicy`, a small dependency-free package that screens a
seeded plan's text for a fixed set of bright-line infrastructure-reconnaissance
targets, and call it at the single choke point that can create a seeded run:
`createRun` (`api/internal/workersvc/service.go`), inside the `if seed != nil`
block, on `secretscrub`-scrubbed plan text, after the empty-plan check and before
the run is persisted. `createRun` is the only writer of `plan_source = 'seeded'`,
so this is the one place a screen needs to live to cover every seeded run.

A match returns the `ErrPlanUnsafe` sentinel wrapped with the matched category;
the handler (`writeStartRunError` in `api/internal/handler/workers.go`) maps it to
`422 Unprocessable Entity` with a message naming the category and redirecting the
caller to the ordinary run flow. The run is never created — there is nothing to
clean up and no partial state to reconcile.

The screen runs only on seeded plans. An ordinary issue-planned run is never
screened and still goes through the human approval gate unchanged; this is
additive to that path, not a replacement for it.

### The rules

Three bright-line targets, chosen because none has a legitimate reason to appear
in a repository code-change plan:

- cloud instance metadata endpoints: the link-local IMDS literal
  `169.254.169.254` and the GCP metadata DNS name `metadata.google.internal`
- the default kube-apiserver ClusterIP literal, `10.96.0.1`
- the in-pod service-account token mount, matching both the canonical
  `/run/secrets/kubernetes.io/serviceaccount` and the `/var/run/...` FHS-symlink
  spelling

The screen deliberately does **not** match `kubernetes.default.svc` DNS (this repo
ships k8s manifests, so that name legitimately appears in a plan) or cloud
credential file paths such as `~/.aws/credentials` — those would be false
positives against ordinary repo work, not reconnaissance.

### Why reject-at-create, not the other two options the issue considered

- **Forcing the approval gate for seeded plans instead.** This was rejected
  because it only relocates the human discretion the issue is trying to replace
  with a control — the same soft backstop, just moved earlier — and it would
  require re-plumbing the seeded worker path that currently trusts
  `plan_source = 'seeded'` outright. A bright-line reject at create time *is* a
  control; forcing the gate is not.
- **An LLM classifier over the plan text.** This was rejected because it is
  exactly the model-dependent backstop the issue set out to replace: no
  guaranteed outcome, and a new inference dependency on the create path for a
  question that has a deterministic answer.

### Prior art

`api/internal/forge/github_pipelines.go`'s `isDisallowedLogIP` is a conceptual
sibling — it also blocklists the cloud metadata range — but it guards an
outbound `net.IP` dial before a log fetch, not plan text. That shape doesn't fit
a text screen, which is why `planpolicy` is a fresh, dependency-free package
rather than a reuse of that check.

## Consequences

- Every seeded-run create call is now screened before the run exists; a match
  costs the caller nothing but a 422 and a re-submit through the ordinary,
  gated flow.
- This is defense-in-depth, and openly not more than that: a text denylist is
  trivially defeated by obfuscation (splitting the literal, encoding it,
  paraphrasing the target). The real trust boundary for what a worker can
  actually reach is the network egress model in
  [ADR-285](0285-worker-egress-tier-trust-model.md); `planpolicy` complements
  that boundary by refusing the ungated fast-path for a plan that names one of
  its targets outright, but it is never a substitute for it, and a plan that
  says nothing about a target it will still try to reach is not caught here.
- Accepted false-positive: this repo's own egress-policy manifests legitimately
  contain these literals — `deploy/values/dev-cluster.yaml` carries both the
  metadata IP and the apiserver ClusterIP as `cidr:` deny entries, and
  `deploy/chart/templates/worker-fqdn-egress.yaml` names the metadata IP in its
  deny-list rationale. A *seeded* plan that
  edits that feature will be refused the fast-path and fall back to the
  ordinary gated run flow — which is unscreened — losing nothing but the
  ungated shortcut. That trade is accepted by design: a plan naming a
  bright-line recon target is exactly the kind of plan that should get a human
  look rather than skip straight to implementation, even when the reason
  turns out to be innocent.
