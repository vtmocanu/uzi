# Contributing to uzi

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
- **Real credentials of any shape.** Test tokens must be obviously fake and carry
  a `gitleaks:allow` marker or a recognizable canary. The secret scanner runs in
  CI.

Cluster-specific operational values (real hosts and CIDRs) belong in a private
deployment repository, not here. The in-repo `deploy/values/` keeps only a
sanitized CI render file (`ci-render.yaml`) and a local smoke-test file
(`kind-smoke.yaml`); follow that pattern for anything cluster-specific.
