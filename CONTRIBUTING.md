# Contributing to uzi

## Discuss the design first for larger changes

For anything beyond a small fix (a new feature, a change to how credentials,
runs or workers behave, a new setting, or work that touches several components),
open an issue before writing code:

1. Describe the problem and the approach you propose, including the main
   architecture choices and what you considered and rejected.
2. Wait until we agree on the design in the issue. We may suggest a different
   shape, or fold it into existing plans.
3. Then open the PR, linked to the issue.

This saves you from building something we then have to ask you to rework, and
it lets us agree on the architecture before anyone invests in the code. Small
fixes, typos and documentation corrections can go straight to a PR.

**Slack (optional).** For faster back-and-forth while you work out a design, join
our community Slack; the invite link is in the [README](README.md). Decisions
still land in the issue so everyone can follow them.

## Keep the repository clean of internal data

This repository is open source. Treat every tracked file as world-readable, and
do not introduce internal or company-specific data. Use generic placeholders
instead.

Please do not commit, in code, tests, docs, comments, or fixtures:

- **Internal hostnames or URLs** (a private GitLab, container registry, secret
  store, object storage, or `*.dev` cluster host). Use `example.com`,
  `uzi.example.com`, `localhost`, or `127.0.0.1`.
- **Cluster or infrastructure names** (real Kubernetes cluster names, storage
  classes, GitOps application names, database cluster names, or identity-provider
  realms). Use generic names.
- **Real cluster IP addresses or CIDRs** (node, pod, or service ranges, or
  API-server addresses). Use documentation TEST-NET ranges (`192.0.2.0/24`,
  `198.51.100.0/24`, `203.0.113.0/24`) or `10.244.0.0/16`. Universal constants
  such as `169.254.169.254` and `10.96.0.1`, and RFC1918 ranges used as
  egress-deny entries, are fine because they are not anyone's specific topology.
- **Real people**: full names, or `first.last@<company>` email addresses. Use
  demo personas. Single-name fixtures such as `alice@example.com` are fine.
- **Internal repository or organization paths, image registries, storage
  buckets, or robot and service-account names.**
- **Real credentials of any shape.** Assemble secret-shaped test values at
  runtime (for example `"glpat-" + "notAReal" + "012345678901"`) rather than
  storing complete token literals: `gitleaks:allow` does not bypass GitHub Push
  Protection, which rejects the whole push. The secret scanner and a
  token-literal check run in CI.

Cluster-specific operational values (real hosts and CIDRs) belong in a private
deployment repository, not here. The in-repo `deploy/values/` keeps only a
sanitized CI render file (`ci-render.yaml`) and a local smoke-test file
(`kind-smoke.yaml`); follow that pattern for anything cluster-specific.
