---
title: Per-repo tools
order: 65
audience: user
---

# Per-repo tools

Beyond a worker image's baked-in tools (see [Worker setup](./worker-setup.md)),
a run can install extra CLI tools on demand — `kubectl`, `terraform`, `jq`, and
so on — with [devbox](https://www.jetify.com/devbox) (nix underneath). Tools
belong to the **work**, not the person: two users' workers picking up the same
issue install the same tools, so runs behave identically.

## The three tiers

A run's tool set is resolved from three sources, highest precedence first:

| Tier | Source | Set by | Notes |
|---|---|---|---|
| 1 | Your per-repo **tool profile** | You | A plain package list, validated against the admin allowlist. Wins version conflicts. |
| 2 | The repo's own `devbox.json` | Repo owner opts in | **Packages only**; off by default. Union with tier 1. |
| 3 | Base image | — | No profile, no opt-in → today's baked-in tools only. |

## Set a repo's tools (tier 1)

1. Open **Boards**, pick a repo, and open its **Tools** panel.
2. Add packages from the allowlist-backed picker (e.g. `kubectl@1.31`,
   `terraform`). Pin a version with `name@version`.
3. Save. Your next run on that repo has those tools on `PATH` inside the
   agent's shell; the **same worker's** later runs warm-start from cache.

A package that is not on the allowlist is rejected at save (and, if it was
grandfathered in, at claim time), stopping the run with a clear message
rather than silently dropping the tool.

## Trust a repo's own `devbox.json` (tier 2)

The same **Tools** panel has a **trust this repo's `devbox.json` packages**
toggle, **off by default**. When on, a run also installs the packages that
repo's `devbox.json` lists, unioned with your tier-1 list (yours wins a
version conflict).

**What the toggle does and doesn't protect.** Only the `packages` field is
read: the repo's `shell.init_hook`, `shell.scripts`, flake references, and
every other key are ignored and **never executed**. It does **not** re-check
those packages against the admin allowlist **or the credential-CLI denylist**
below — tier 2 is bounded by being opt-in, packages-only, and provisioned in
the scrubbed env instead. Enable it only for a repo whose review discipline
you trust, since a package can still run arbitrary build code (as any nix
package can), just never your credentials: all provisioning runs in a
subprocess scrubbed of the forge token, the Anthropic token, and the join
token.

## The allowlist (admins)

**Admin → Tool allowlist** is the set of packages a tier-1 profile may use:
an exact package name (no wildcards) plus an optional pinned-version policy.
A small **denylist**
of credential-bearing CLIs (a pre-authenticated `glab`, a kubeconfig helper)
gates **tier 1 only**: such a package is refused even if it matches the
allowlist, so an allowlist-picked tool can never hold push rights the agent
isn't meant to have. (Tier 2 is neither allowlist- nor denylist-checked — it
relies on the opt-in and the scrubbed provisioning env instead, as above.)

## Storage and egress

- **New outbound egress.** Installing tools fetches them from **nix
  substituters** (`https://cache.nixos.org` plus any you add). This is the
  one place a worker reaches beyond `api`; allow it through an egress
  firewall if you run one.
- **First-run-only.** The nix store lives on its own named volume
  (`agentnix` at `/nix`); the first run downloads, later runs on the same
  worker warm-start from it. It survives `docker compose down`/`up`.
- **Eviction.** The store only grows. To reclaim space, remove the volume
  (`docker compose down -v`, or `docker volume rm <project>_agentnix`); the
  next run re-downloads what it needs.

See [ARCHITECTURE.md](../ARCHITECTURE.md#services) for the worker's egress and
provisioning trust model, and [Worker setup](./worker-setup.md#tool-provisioning)
for the operator view.
