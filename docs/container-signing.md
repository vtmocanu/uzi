---
title: Container signing
order: 50
audience: operator
---

# Container signing

uzi's release pipeline signs every published OCI artifact with
[cosign](https://docs.sigstore.dev/) keyless signing (Sigstore). There is no signing
key to store or rotate: each release job signs with its GitHub OIDC identity, so a
signature proves the artifact was built and pushed by uzi's own `release.yml` at a
release tag.

## What is signed

On every `vX.Y.Z` tag, `.github/workflows/release.yml` signs, by digest, after push:

| Artifact | Reference |
| --- | --- |
| api image | `ghcr.io/vtmocanu/uzi/api` |
| web image | `ghcr.io/vtmocanu/uzi/web` |
| controller image | `ghcr.io/vtmocanu/uzi/controller` |
| agent worker images | `ghcr.io/vtmocanu/uzi/agent-base`, `ghcr.io/vtmocanu/uzi/agent-jvm` |
| Helm chart (OCI) | `ghcr.io/vtmocanu/uzi/uzi` |

A cosign signature is a separate OCI artifact (a `sha256-<digest>.sig` tag in the same
repository), so signing does not change the image manifest and stays compatible with
the pipeline's `provenance: false` builds. Signing happens by digest, so the one
signature covers both tags a release pushes (`:X.Y.Z` and `:<short-sha>`).

## Verifying manually

Verification pins two things: who signed (the release workflow) and which OIDC issuer
minted its token (GitHub Actions). For an image:

```sh
cosign verify \
  --certificate-identity-regexp 'https://github.com/vtmocanu/uzi/.github/workflows/release.yml@refs/tags/.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/vtmocanu/uzi/api:<version>
```

The Helm chart is verified the same way, against its OCI reference:

```sh
cosign verify \
  --certificate-identity-regexp 'https://github.com/vtmocanu/uzi/.github/workflows/release.yml@refs/tags/.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/vtmocanu/uzi/uzi:<chart-version>
```

A fork that re-signs under its own repository substitutes its own `.../release.yml` ref
in the identity regexp.

## Enforcing at admission (optional)

Signing only pays off once something verifies. The chart ships an optional
[Kyverno](https://kyverno.io/) `ClusterPolicy` that admits a uzi image only if it
carries a valid signature. It is **off by default** and gated behind
`imageVerification.enabled`.

```yaml
# values.yaml
imageVerification:
  enabled: true
  enforce: Audit          # Audit (log only) | Enforce (block admission)
  imageGlob: "ghcr.io/vtmocanu/uzi/*"
  keyless:
    identityRegexp: "https://github.com/vtmocanu/uzi/.github/workflows/release.yml@refs/tags/.*"
    issuer: "https://token.actions.githubusercontent.com"
```

Three things to know before enabling it:

1. **Kyverno must already be installed in the cluster.** The policy is a Kyverno CRD;
   with Kyverno absent the object's kind is unknown and `helm install` / ArgoCD sync
   fails on it. This chart does not bundle Kyverno.
2. **Start in `Audit`.** Audit records a policy report and still admits the pod; flip
   to `Enforce` only after confirming every uzi image verifies. Enforcing before the
   signatures exist, or with a wrong identity or issuer, blocks the very pods the
   chart deploys.
3. **Scope is by image, not namespace.** Only images matching `imageGlob` are checked,
   so pods pulling PostgreSQL, the CNPG operator, the Docker-in-Docker sidecar, or
   Kyverno itself are admitted untouched. The policy verifies the signature and leaves
   the image reference as-is (it neither requires nor rewrites digest references,
   because uzi's workloads reference images by tag).
