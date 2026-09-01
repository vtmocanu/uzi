---
title: Agent source
order: 51
audience: user
---

# Agent source

An admin can point uzi at a git repository of agent role definitions and have
uzi periodically sync your [agent templates](./agent-templates.md) from it —
a shared, version-controlled roster instead of hand-editing each template in
the UI. **Off by default, with no repository URL pre-filled**: a fresh
install stays offline and runs only the shipped builtins until an admin
opts in.

Nothing a sync fetches ever reaches a run silently. Every sync only
**stages** a diff; an admin has to review it and click **Approve & apply**
before any role body actually changes — see
[Staging and approval](#staging-and-approval-nothing-applies-itself) below.

## 1. Configure it

Open **Admin → Instance settings → Agent source**:

1. **Repository URL** — an `https://` git URL. It must be on the operator's
   `AGENT_SOURCE_ALLOWED_BASE_URLS` allowlist (see
   [Configuration](./configuration.md#agent-source-repo-sync-prd-602)) or the
   save is rejected; ask whoever deployed uzi to add your host if it isn't
   listed yet. Leave it empty to keep the source disabled.
2. **Ref** — the tag, SHA, or branch to clone. **A pinned tag or SHA is the
   recommended form**; a floating branch works too, but it means every push
   to that branch flows into the next reconcile with no gate beyond the
   approval step below — the weaker posture.
3. **Source folder** — the repo-relative subfolder role files are read from.
   Leave it empty for the default `.claude/agents`; set it to point the sync
   at a subtree like `product-agents/` instead. It selects a subtree of the
   already-cloned, already-allowlisted repo, so it adds no new network reach —
   a leading `/`, a `..` segment, or a URL/scheme is rejected at save.
4. **Sync automatically on the interval below** — the enable toggle. Leaving
   it off doesn't stop you from syncing by hand (below); it only stops the
   timer.
5. **Sync interval** — how often the automatic reconcile re-checks the
   source, as a Go duration (e.g. `1h`).
6. **Access credential** — only needed for a private repo. Write-only:
   pasting a value seals it and it is never shown again; leave it blank to
   keep whatever is already stored.
7. Save.

Enabling the source is rejected unless both a repository URL and a ref are set — configure them first.

### Preset: follow the canonical roster

Instead of typing the fields above by hand, click **Use uzi skills preset**
to follow uzi's own shared roster at `github.com/vtmocanu/skills`. One click
fills:

- **Repository URL** → `https://github.com/vtmocanu/skills`.
- **Source folder** → `product-agents/`.
- **Ref** → the **latest semver tag, resolved at click time** — never a
  hardcoded version. The card asks the server to `git ls-remote` the source
  (a lightweight ref-advertisement round trip, no full clone) and fills in
  whatever tag comes back.

The preset only fills the form — nothing is saved or synced until you
review the values and click **Save**, the same as configuring the fields by
hand. Two preconditions to know before you click it:

- **The resolve needs `github.com` on this deployment's
  `AGENT_SOURCE_ALLOWED_BASE_URLS` allowlist** (see
  [Configuration](./configuration.md#agent-source-repo-sync-prd-602)) **and
  a public source.** If `github.com` isn't allowlisted, the resolve is
  refused and the URL and folder still get filled — set a ref by hand, or
  ask whoever deployed uzi to allowlist it and try again.
- **The preset is inert until the skills repo publishes a `product-agents/`
  folder.** Until then, a sync against it stages nothing — that degrades
  gracefully rather than failing — and the resolve may find no semver tag
  to fill in.

## 2. Sync, review, approve

Once a repository URL is saved, click **Sync now** (or wait for the
interval) to fetch the pinned ref and stage what it finds. The card's
**Sync status** panel shows when it last synced, the fetched commit, and
counts of roles staged, changed, and failed.

### Update checks and the update badge

Next to **Sync now** is **Check for updates**. It does the same lightweight
`git ls-remote` (a ref-advertisement round trip, no full clone) the preset
above uses, but against the **saved, configured** source with its stored
credential, and it records what it finds — a check never stages or applies
anything on its own.

The **Sync status** panel then derives an "update available" badge from
what the last check found, plus the current configuration, with **no
further network call**. How the badge clears depends on the pin type: a
**tag-pinned** badge clears when you **bump the pin** to the newer tag
(approving an apply does not change the pinned ref, so an apply alone
leaves it showing), while a **branch-moved** badge clears once an **apply**
advances the last-applied SHA to match the tip recorded by the last check.
The comparison is against that recorded tip, not the live branch, so a
branch that moves again after the check needs another **Check for updates**
to re-evaluate. What counts as
"newer" depends on how the source is pinned:

- **Tag-pinned** (e.g. `v1.2.0`) — the badge reads **"Update available:
  `<tag>`"** when the source's newest valid semver tag is strictly greater
  than the pinned one.
- **Branch-pinned**, or no ref set (the default branch) — the badge reads
  **"Source moved"** when the branch's tip has advanced past what was last
  applied. It doesn't say how far ahead; an exact "N commits behind" would
  need a full fetch and history walk, which the update check deliberately
  avoids.
- **SHA-pinned** (a full 40-character commit hash) — no badge. An exact SHA
  pin is intentionally frozen, so there's nothing to signal.

If a change is *also* already staged and awaiting your review, a note next
to the badge says so — distinguishing "the source moved past what's
running" from "a change is staged and waiting for your approval" (the
latter is the state described in
[Staging and approval](#staging-and-approval-nothing-applies-itself)
below).

When the tag-pinned badge is showing, a **Bump pin to `<tag>`** button
appears beside it. Clicking it only rewrites the saved **Ref** to that tag
— it doesn't sync or apply anything by itself. Click **Sync now**
afterward, then review and approve the staged diff as usual.

### Staging and approval: nothing applies itself

A sync clones the pinned ref, parses every `*.md`-shaped role file it finds
in the configured **Source folder** (default `.claude/agents`), and stops
there: the result is a **staged snapshot**, not a
change to your templates. The admin card shows the staged diff — which
roles are new, changed, unchanged, or removed, and why any role was skipped
— and only **Approve & apply** writes anything to `agent_templates`. This
approve-before-apply step is the primary control on this feature: a role
body is a system prompt for an agent that runs `Bash` and edits files, so a
human reads the diff before it can run anywhere.

### What approving does

- A synced role sharing a name with an existing **builtin** overrides that
  builtin's body — the synced body is what's shown and run, not the shipped
  default. It stays a builtin — still labeled and grouped as one, still
  resettable to the embedded default via **Reset to
  default** (see [Agent templates](./agent-templates.md#resetting-a-builtin-template)).
- A synced role with a **new** name (no builtin or admin-authored global
  template by that name) is added as a global template, on by default like
  any other global template.
- A synced role whose name collides with an **admin-authored global
  template** is never applied — it's staged with a visible error and
  skipped, so a sync can never silently clobber an admin's own template.
- A role that **disappears** from the source on a later sync is
  de-provisioned: a synced-only global role is deleted, and an overridden
  builtin resets to its embedded body. A role you've since edited yourself
  (an `admin`-origin builtin) or one that was never synced is never touched
  by a source's removal.

A role also fails independently of the others — an unreachable repo, one
malformed role file, or a role that ends up with every tool denied fails
just that role, with a visible reason, while every other role and your
current templates are left exactly as they were.

## Provenance: the "synced" badge

A template whose body last came from the source repo shows a **synced**
badge on the Agents list and on its detail page, in place of the usual
"differs from shipped" drift badge — see
[Agent templates](./agent-templates.md#resetting-a-builtin-template) for
what that drift badge otherwise means. It tells you the body was written by
an admin approving a sync, not typed by hand and not the shipped default,
without implying it needs resetting.

## Known limitation: an untrusted source can hide characters in a role body

The template editor renders a role's body as plain text — it is never
executed as markup, so a hidden character there cannot run anything on its
own. But a body pulled from a source repo can still carry invisible
characters (bidi overrides, zero-width characters, other control
characters) that make the rendered text look different from what it
actually says. The staged-review preview strips these before you see them
and flags **"hidden formatting characters were removed from this
preview"** when it does, so you know the raw body (the one that's actually
applied) differs from what's shown — but the flag is an honesty signal, not
a filter on what gets stored. Treat a role body from a source repo you
don't fully trust the same way you'd treat any other code you're about to
run: read it, don't skim it.

## Trust model

The design and threat model behind approve-before-apply, the SSRF
allowlist, the sealed credential, and ref pinning are recorded in
[ADR-0602](../adr/0602-agent-source-repo-sync.md).

## From the CLI: read-only

`uzi admin agent-source get` prints the config (URL, ref, folder, enabled,
interval, and whether a credential is set — never its value); `uzi admin agent-source
status` prints the sync status (last sync/apply time and SHA, staged
counts, whether a snapshot is pending review) plus the derived update
signal — `update_available`, and (once a check has run) `latest_ref` and
`update_checked_at`. These are computed the same way the web badge is: from
the last **Check for updates** result plus the live config, with no
network call from the CLI itself. Both commands are read-only, like every
other `uzi admin` command — see [uzi CLI](./cli.md#commands). Setting up
the source, and clicking **Sync now**, **Check for updates**, **Bump pin**,
and **Approve & apply** stay web-only actions, from **Admin → Instance
settings → Agent source** in the browser.
