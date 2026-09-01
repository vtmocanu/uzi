---
title: Forgejo bot setup
order: 21
audience: user
---

# Forgejo bot setup

**Protect `main` and enable the merge whitelist — uzi will not do it for you.** A
fresh Forgejo repo ships with `main` completely unprotected, and even once you
protect it, the merge whitelist itself defaults **off** — meaning any Write-role
account, including the bot, can merge its own pull request into `main`. Do this
first, before you connect the bot: it's step 1 below, not a footnote. **uzi
refuses to run until you do** — it won't let you enable the repo, and it
refuses to start (or claim) a run against one that's already enabled but
still unprotected (see [Least privilege](#least-privilege-what-uzi-verifies)
below).

uzi acts on the forge as **your own bot account**, never your personal identity: a
revocable, individually-scoped identity instead of one shared credential. See
[ARCHITECTURE.md](../ARCHITECTURE.md#forge-integration) for why.

## 1. Protect `main`

Repository → **Settings → Branches**, add a protection rule for `main`, then turn
on **Enable Merge Whitelist** and leave the whitelist itself empty (don't add the
bot, or any team it belongs to). Protecting the branch already blocks direct push
by default — the merge whitelist is the one setting that isn't safe by default and
needs the explicit flip.

## 2. Create the bot account

Register a second account for the bot yourself (e.g. `uzi-bot-<yourname>`), or ask
an instance admin to create one if self-registration is closed.

## 3. Create a personal access token

From the bot account's **Settings → Applications → Manage Access Tokens**:

- **Scopes: exactly `Repository: Write`, `Issue: Write`, `User: Read`** — nothing
  more, nothing less. Fewer would not work (moving a card writes labels, opening a
  pull request needs repo write); anything more is over-privilege, and uzi rejects
  an over-scoped token (see [Least privilege](#least-privilege-what-uzi-verifies)
  below).

## 4. Add the bot to your repo

Repository → **Settings → Collaborators**, add the bot with permission **Write**
(not Read, not Admin). Write is a hard requirement: uzi's project discovery only
sees repos where the bot has at least Write access, and Admin would hand the bot
power it doesn't need — including reading and deleting the branch protection rule
you just set up.

## 5. Check your Forgejo version

uzi requires **Forgejo v16.0.0 or newer** and refuses to connect below it, naming
the version it found. That's because the CI-fix loop's job-log endpoint
(`GET /actions/jobs/{id}/logs`) first shipped in v16.0.0 — there's no degraded mode
for older instances.

## 6. Connect the bot in uzi

1. Log in and open **Settings → Forge**.
2. Pick a base URL and forge type (only the operator-configured allowlist is
   offered — the SSRF guard; see [ARCHITECTURE.md](../ARCHITECTURE.md#forge-integration)).
3. Paste the bot's PAT and submit. uzi verifies it immediately, runs the
   least-privilege token check (see below), and shows the bot's username; the
   token itself is never shown again. An **over-privileged** token, or an instance
   below v16.0.0, is rejected here and nothing is stored — mint a clean token or
   upgrade the instance, then retry.

## 7. Enable the repo

Open **Boards**, pick the connection, and enable each project you added the bot
to. This makes its board appear in the sidebar and starts syncing its
`uzi`-labeled issues (see [Board](./board.md)).

If verification fails, check: the token's scopes are exactly `write:repository,
write:issue, read:user`, it hasn't expired, the bot is at least Write on the
target project, and the instance is v16.0.0+.

## Least privilege: what uzi verifies

Same posture as [GitLab bot setup](./gitlab-bot-setup.md#least-privilege-what-uzi-verifies),
mapped onto Forgejo's model:

- **Token scopes are exactly `write:repository`, `write:issue`, `read:user`.**
- **The bot is not an instance admin.**
- **The token is active and unexpired.**
- **On every enabled repo, the bot's role is exactly Write.** Admin/Owner can push
  protected branches and change repo settings; below Write breaks sync.
- **The default branch is protected, the write role cannot push to it, and the
  write role cannot merge into it.** The last check is Forgejo-specific: unlike
  GitLab, a default-configured Forgejo lets a Write-role bot merge its own pull
  request, so uzi checks it directly rather than assuming a safe default.

**Checked at the same three points as GitLab**: at connect (blocking — an
over-privilege violation rejects the save and stores nothing), on demand
(**Check privileges** in Settings → Forge), and periodically (the background
sweep, `UZI_PRIVILEGE_CHECK_INTERVAL`).

**A missing merge whitelist (or an unprotected `main`) shows as a violation
badge, and uzi refuses to act on it**: it won't let you enable the repo, and
it refuses to start or claim a run against one that's already enabled. The
check is live and fails closed, so a Forgejo that errors or times out while
uzi reads branch protection also refuses, rather than passing by accident.
Sync itself is unaffected — the board keeps syncing so you can see and fix
the problem; only starting an agent against the repo is refused. Treat the
badge as the signal and step 1 above as the fix. An instance admin can
override the refusal for one named repo, with a recorded reason, if the risk
is knowingly accepted — see [Admin settings](./admin-settings.md#guardrail-override-per-repo).
Separately: a **deploy key** with write access bypasses Forgejo's role checks
entirely, so "the bot's PAT can't merge or push" isn't "nothing can" — uzi
provisions no deploy keys itself, but if your project already has one it sits
outside everything checked here, admin override included.

**Two gaps uzi cannot see, by design** (reading either needs repo-admin, which the
bot deliberately never holds): a **team** the bot belongs to on the push/merge
whitelist (the same class of gap [GitLab bot setup](./gitlab-bot-setup.md)
documents for GitLab's group-based grants), and an **`unprotected_file_patterns`**
rule (e.g. `*.md`) letting matching commits bypass push protection, invisible even
to the write-role bot's own branch read. Audit both by hand if you use them.

## Known limitation: the label lost-update window

Forgejo's label API replaces the whole set in one call rather than adding/removing
a delta, so uzi reads the current labels, computes the new set locally, then
writes it back. An unrelated label added by someone else in that brief window can
be silently dropped. Rare (a sub-second race), and the next sync self-corrects
uzi's cache, though not the human's lost label; uzi skips the write entirely when
a card move changes nothing, so this only bites on an actual relabel.
