---
title: GitHub bot setup
order: 22
audience: user
---

# GitHub bot setup

**Protect `main`, or add a ruleset — uzi cannot always read, and never sets, this
for you.** GitHub has two independent protection systems: classic *branch
protection rules* and newer *rulesets*. uzi's bot holds write access and can read
whether a **ruleset** covers `main`, but a write-role bot cannot read the details
of classic branch protection at all (that needs admin). On a classic-protected
repo, uzi reports `main` as protected but shows push/merge rights as
**unverified** rather than guessing — see [Least privilege](#least-privilege-what-uzi-verifies)
below. Prefer a ruleset over classic protection so uzi's report is authoritative,
and either way, require pull-request reviews before merge.

uzi acts on the forge as **your own bot account**, never your personal identity: a
revocable, individually-scoped identity instead of one shared credential. See
[ARCHITECTURE.md](../ARCHITECTURE.md#forge-integration) for why.

## 1. Protect `main`

Repository → **Settings → Rules → Rulesets**, add a ruleset targeting `main` that
requires a pull request before merging and blocks force pushes. A ruleset is
preferred over the legacy **Settings → Branches** protection rule because uzi can
read it at write role; classic protection is invisible to the bot beyond a bare
"protected" flag.

## 2. Create the bot account

Register a second GitHub account for the bot yourself (e.g. `uzi-bot-<yourname>`).

## 3. Create a personal access token

From the bot account's **Settings → Developer settings → Personal access tokens
→ Tokens (classic)**:

- **Scope: exactly `repo`**, nothing more. `repo` is the single scope that grants
  private-repo contents write (git push), issues, pull requests, and read of
  Actions runs/jobs/logs on that repo — the coarsest and only classic scope that
  covers everything uzi needs. Anything broader (`workflow`, `delete_repo`,
  `admin:org`, …) is over-privilege and uzi refuses it at connect (see
  [Least privilege](#least-privilege-what-uzi-verifies)).
- **Only a classic token (`ghp_…`) is supported.** A fine-grained token
  (`github_pat_…`) does not expose its scopes for uzi to verify, so uzi refuses it
  at connect rather than saving an unverifiable token.
- **github.com only.** GitHub Enterprise Server is not supported.

**A `workflow`-scoped token is deliberately refused.** Requiring exactly `repo`
means an agent's push that adds or edits a file under `.github/workflows/` will
fail at push time with GitHub's own error — the bot token cannot touch CI
definitions. This is a chosen CI-integrity boundary, not a bug: an agent cannot
tamper with the workflows that guard `main`. If you need agents to edit workflow
files, uzi cannot support that on GitHub today.

## 4. Add the bot to your repo

Repository → **Settings → Collaborators**, add the bot with role **Write** (not
Read, not Admin). Write is a hard requirement: uzi's project discovery only sees
repos where the bot has at least Write access, and Admin would hand the bot power
it doesn't need — including reading the branch protection rule you just set up.

## 5. Connect the bot in uzi

1. Log in and open **Settings → Forge**.
2. Pick a base URL and forge type (only the operator-configured allowlist is
   offered — the SSRF guard; see [ARCHITECTURE.md](../ARCHITECTURE.md#forge-integration)).
3. Paste the bot's PAT and submit. uzi verifies it immediately, runs the
   least-privilege token check (see below), and shows the bot's username; the
   token itself is never shown again. An **over-privileged** token, or a
   fine-grained token, is rejected here and nothing is stored — mint a clean
   classic `ghp_…` token and retry.

## 6. Enable the repo

Open **Boards**, pick the connection, and enable each repo you added the bot to.
This makes its board appear in the sidebar and starts syncing its `PRD`-labeled
issues (see [Board](./board.md)).

If verification fails, check: the token's scope is exactly `repo`, it's a
classic token (not `github_pat_…`), it hasn't expired, and the bot is at least
Write on the target repo.

## Least privilege: what uzi verifies

Same posture as [GitLab bot setup](./gitlab-bot-setup.md#least-privilege-what-uzi-verifies),
mapped onto GitHub's model:

- **Token scope is exactly `repo`.** Anything more, including `workflow`, is
  refused at connect.
- **Only a classic PAT is accepted.** A fine-grained token is refused at connect
  — uzi cannot introspect its scopes, so it will not save one on a warning.
- **The bot is not a site admin.**
- **On every enabled repo, the bot's role is exactly Write.** Admin can push
  protected branches and change repo settings; below Write breaks sync.
- **Push/merge rights on the protected default branch, when uzi can determine
  them.** If a **ruleset** covering `main` is readable, uzi reports whether the
  write role can push or merge — authoritative. If `main` is protected only by
  **classic branch protection**, uzi cannot read who may push or merge at write
  role (that detail needs admin), so it reports the branch as protected but the
  push/merge finding as **unverified**, never a false "safe".

**Checked at the same three points as GitLab**: at connect (blocking — an
over-privilege or fine-grained-token violation rejects the save and stores
nothing), on demand (**Check privileges** in Settings → Forge), and periodically
(the background sweep, `UZI_PRIVILEGE_CHECK_INTERVAL`).

**An unverified protection finding does not yet block anything.** Today the
guardrail that would refuse to run against a repo whose protection uzi can't
confirm is not built — see [ADR-0238](../adr/0238-github-driver.md) for the
accepted limitation. Until then, the other guardrail layers hold regardless: the
worker only ever pushes the agent's own branch and never merges, so `main`
cannot be written even on a repo whose ruleset coverage uzi cannot see.
