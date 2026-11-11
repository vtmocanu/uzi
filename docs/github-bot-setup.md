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

uzi acts on the forge as a **dedicated identity**, never your personal account: a
revocable, individually-scoped credential instead of one shared one. On GitHub
that identity is either a bot account holding a personal access token, or a
GitHub App installation. See
[ARCHITECTURE.md](../ARCHITECTURE.md#forge-integration) for why.

## Two ways to authenticate

uzi accepts two GitHub credential types. Both are verified at connect, and
re-checked on demand and periodically — see
[Least privilege](#least-privilege-what-uzi-verifies).

- **[Personal access token](#personal-access-token)** — a second GitHub account
  for the bot, holding a classic PAT. Works on any plan, and is the only path
  that supports [GitHub Projects v2 sync](./github-project-sync.md).
- **[GitHub App](#github-app)** — an App installed on your organization or
  account. No second human account, per-repository scoping, and one-hour tokens
  minted automatically. Recommended for org deployments.

| | Bot account + PAT | GitHub App |
|---|---|---|
| Second human account | Required | Not required — the App is the identity |
| Credential | Classic PAT (`ghp_…`), valid until it expires or is revoked | RSA private key; uzi mints one-hour installation tokens from it |
| Permission scoping | Coarse classic scopes (`repo`, optionally `project`) | Per-permission and per-repository; every token is downscoped to the exact set uzi requests |
| Repository access | Every repo the bot is a collaborator on | Only the repositories selected at install, narrowed further by the names on the connection |
| Audit trail | Actions attributed to the bot user account | Actions attributed to `<app-slug>[bot]`; the App appears in the org audit log |
| Seat cost | Consumes a user seat on paid plans | Does not consume a user seat |
| Projects v2 sync | Supported with the `project` scope | Not supported — installation tokens carry no `project` scope |

## Protect the default branch

Repository → **Settings → Rules → Rulesets**, add a ruleset targeting your
repository's default branch (`main` in most repos) that requires a pull
request before merging and blocks force pushes. A ruleset is
preferred over the legacy **Settings → Branches** protection rule because uzi can
read it at write role; classic protection is invisible to the bot beyond a bare
"protected" flag.

## Personal access token

### 1. Create the bot account

Register a second GitHub account for the bot yourself (e.g. `uzi-bot-<yourname>`).

### 2. Create a personal access token

From the bot account's **Settings → Developer settings → Personal access tokens
→ Tokens (classic)**:

- **Scope: `repo` is required.** `repo` is the single scope that grants
  private-repo contents write (git push), issues, pull requests, and read of
  Actions runs/jobs/logs on that repo — the coarsest and only classic scope that
  covers everything uzi needs by default. **If you want [GitHub Projects v2
  sync](./github-project-sync.md), add the write `project` scope** — uzi
  accepts exactly `{repo}`, `{repo, project}`, or `{repo, read:project}` at
  connect, but `read:project` is not enough for sync: a `{repo, read:project}`
  token passes connection validation and then fails Project-sync setup with a
  missing-scope error, because the sync's write paths require `project`
  specifically. Anything broader (`workflow`, `delete_repo`, `admin:org`, …) is
  over-privilege and uzi refuses it at connect (see
  [Least privilege](#least-privilege-what-uzi-verifies)).
- **Only a classic token (`ghp_…`) is supported.** A fine-grained token
  (`github_pat_…`) does not expose its scopes for uzi to verify, so uzi refuses it
  at connect rather than saving an unverifiable token.
- **github.com is the tested host.** On GitHub Enterprise Server, set the
  connection to the web host (`https://ghe.example.com`); uzi reaches the API at
  `<host>/api/v3`, as GHES serves it. CI does not exercise a GHES instance.

**A `workflow`-scoped token is deliberately refused.** Requiring exactly `repo`
means an agent's push that adds or edits a file under `.github/workflows/` will
fail at push time with GitHub's own error — the bot token cannot touch CI
definitions. This is a chosen CI-integrity boundary, not a bug: an agent cannot
tamper with the workflows that guard `main`. If you need agents to edit workflow
files, uzi cannot support that on GitHub today.

**Symptom, and the fix that is NOT the fix.** When a run's branch touches a
`.github/workflows/*` file, uzi detects it at finalize, **before** the push, and
ends the run `failed` with a typed, actionable reason (`fail_origin =
workflow_scope_missing`) naming the offending path(s) and pointing back at this
doc — instead of the run running all the way to done and then dying opaquely at
the final push with GitHub's own rejection:

```
! [remote rejected] ... (refusing to allow a Personal Access Token to create or
update workflow `.github/workflows/<name>.yml` without `workflow` scope)
```

The agent's work is not lost: its diff is preserved (a scrubbed, size-capped
patch) and rendered on the failed run's card in the web UI, so you can apply it
and land the change yourself without reconstructing it from the run transcript.

This is expected, not a broken connection. **Do not resolve it by adding
`workflow` scope to the bot token:** that makes the token `{repo, workflow}`, which
uzi's least-privilege check treats as a save-blocking over-privilege violation, so
the periodic privilege sweep will flag the connection and refuse to start or claim
runs on those repos (see [Least privilege](#least-privilege-what-uzi-verifies)).
The correct resolution is to land the workflow change **as a human**: apply the
preserved diff from the failed run's card, commit the file yourself on a branch
with a personal token that carries `workflow` scope, and open a normal pull
request. Keep the uzi bot token at exactly `repo`.

**A second, subtler case: the run's branch never touched a workflow file at
all, but is merely BEHIND `main` on one.** GitHub enforces the `workflow`-scope
requirement on the *pushed tip*, not on what the branch's own commits changed:
if `main`'s `.github/workflows/**` files were updated (say, by a dependency
bot) after a run's clone base, that run's branch tip differs from `main` on
those files even though the run itself never edited one — and the same
rejection fires at the final push, which used to lose the entire run's work
with no recovery path. uzi now handles this automatically: immediately before
the finalize push, it fetches `main`'s current tip and, only when the
workflow trees actually differ, realigns the branch with it (a merge first,
falling back to a rebase if the merged push is still rejected) before
pushing — so the run completes normally with no user action needed. If that
realignment itself cannot resolve (a genuine conflict on both the merge and
the rebase), the run fails cleanly with a typed reason
(`fail_origin = finalize_base_align_conflict`) and, exactly as above, the
diff is preserved and rendered on the failed run's card for you to rebase and
land by hand. See [ADR-456](../adr/0456-rebase-before-finalize-push.md) for
the mechanism.

**A third case: the branch's history was rewritten below its already-published
tip.** Several kinds of run start from a branch that already exists on the
forge — a `uzi handoff` task branch, an MR being reworked, a resumed run — and
uzi always lands work there with a **plain fast-forward push, never a
force-push**: force is denied by the worker's guardrails and by the bot's
role, by design, so an agent that rebases, amends, or resets a commit at or
below that published tip leaves uzi nothing it can simply force over. Instead,
at the next checkpoint and again at finalize, uzi restores the published
commit as an ancestor of the agent's rewritten tip with a synthesized bridge
commit — its tree is exactly the agent's tip, byte-for-byte unchanged, with
the published commit added as an extra parent — so the branch **fast-forwards**
with both histories intact. A merge request opened on a bridged branch carries
a one-line note saying so; `git log --first-parent` still reads as the
agent's own work. Only when a bridge cannot be built or validated (the agent
rewrote the branch again, or the bridge fails its own consistency check)
does the run end `failed` with a typed reason (`fail_origin =
history_rewritten`) naming the published tip. Unlike the two cases above, the
diff is preserved on the failed run's card only when the push range could be
secret-scanned first, which a rewritten branch usually prevents; either way
your committed work stays on the run's branch and is recoverable with `uzi run
export`.

### 3. Add the bot to your repo

Repository → **Settings → Collaborators**, add the bot with role **Write** (not
Read, not Admin). Write is a hard requirement: uzi's project discovery only sees
repos where the bot has at least Write access, and Admin would hand the bot power
it doesn't need — including reading the branch protection rule you just set up.

### Project sync: invite the bot onto the Project (GitHub Projects v2 only)

If you plan to use [GitHub Projects v2 sync](./github-project-sync.md),
there's a **second, separate invite** beyond step 3 above. A Project has
its own membership, distinct from the repo's collaborator list — adding
the bot to the repo does not by itself let it read or move the Project's
Status field. On the Project itself, open its **⋯ menu → Manage access**
(or **Settings → Manage access**), add the bot account, and grant it
**Write**. Do this before running Adopt — see [The Adopt flow, step by
step](./github-project-sync.md#the-adopt-flow-step-by-step).

### 4. Connect the bot in uzi

1. Log in and open **Settings → Forge**.
2. Pick a base URL and forge type (only the operator-configured allowlist is
   offered — the SSRF guard; see [ARCHITECTURE.md](../ARCHITECTURE.md#forge-integration)).
3. Leave **GitHub authentication** on **Personal access token**.
4. Paste the bot's PAT and submit. uzi verifies it immediately, runs the
   least-privilege token check (see below), and shows the bot's username; the
   token itself is never shown again. An **over-privileged** token, or a
   fine-grained token, is rejected here and nothing is stored — mint a clean
   classic `ghp_…` token and retry.

### 5. Enable the repo

Open **Repos**, pick the connection, and enable each repo you added the bot to.
This makes its board appear in the sidebar and starts syncing its `uzi`-labeled
issues (see [Board](./board.md)).

If verification fails, check: the token's scope is exactly `repo`, it's a
classic token (not `github_pat_…`), it hasn't expired, and the bot is at least
Write on the target repo.

## GitHub App

A GitHub App installation replaces the bot account entirely: the App is the
identity, and uzi mints short-lived installation tokens from its private key.
Nothing here needs a second human account.

### 1. Create the App

**Settings → Developer settings → GitHub Apps → New GitHub App** — on the
organization's settings for an org-owned App, or on your personal account's
settings for a user-owned one.

- **Name:** anything you like — this doc uses `uzi-forge`. The name becomes the
  bot identity, `<app-slug>[bot]`.
- **Homepage URL:** required by GitHub; any URL you control.
- **Webhook:** uncheck **Active**. uzi does not receive webhooks.

### 2. Set repository permissions

Under **Permissions → Repository permissions**, grant exactly this set and leave
every other permission at **No access**:

| Permission | Access |
|---|---|
| Contents | Read and write |
| Issues | Read and write |
| Pull requests | Read and write |
| Metadata | Read-only |
| Actions | Read-only |
| Checks | Read-only |
| Statuses | Read-only |

`Metadata: Read-only` is mandatory on every GitHub App and is granted
automatically. uzi requests exactly this set when it mints each installation
token and refuses a token that carries anything different, so the App must be
granted at least these; granting more on the App does not widen what uzi's token
can do, because every token is downscoped to the set above.

### 3. Generate a private key

On the App's settings page, **Private keys → Generate a private key**. GitHub
downloads a `.pem` file and never shows the key again — keep it. uzi accepts an
RSA key of at least 2048 bits, in either PKCS#1 (`-----BEGIN RSA PRIVATE KEY-----`)
or PKCS#8 (`-----BEGIN PRIVATE KEY-----`) PEM form.

### 4. Install the App

On the App's settings page, **Install App**, choose the organization or account,
and select the repositories uzi should see (**All repositories**, or **Only
select repositories**).

The **Installation ID** is the number in the installation's settings URL:

```
https://github.com/organizations/<org>/settings/installations/<installation-id>
https://github.com/settings/installations/<installation-id>
```

### 5. Connect the App in uzi

1. Log in and open **Settings → Forge**.
2. Pick the `https://github.com` base URL and forge type **GitHub**. Only
   github.com is supported for App credentials — a custom or self-hosted base URL
   is refused.
3. Set **GitHub authentication** to **GitHub App**. Four fields appear:
   - **App ID** — the number shown as **App ID** on the App's settings page.
   - **Installation ID** — the number from step 4.
   - **Repository names** — comma-separated repository names within this
     installation, without the owner (e.g. `my-repo, my-other-repo`). At least
     one is required, up to 100. Only these repositories are authorized.
   - **Private key (PEM)** — paste the entire `.pem` file, including its
     `BEGIN` and `END` lines. Multiline paste is preserved and the key stays
     masked.
4. Submit. uzi signs a JWT with the key, exchanges it for an installation token,
   verifies the token's permissions and repository scope, and shows the bot
   identity (`<app-slug>[bot]`). The private key is never shown again.

### 6. Enable the repo

Open **Repos**, pick the connection, and enable each repository you named on it.
Only those repositories are visible to the connection.

If verification fails, check: the App ID and Installation ID are the numbers from
the App's own settings and installation URL, the installation is not suspended,
the App is granted at least the permissions in step 2, the installation includes
every repository you named, and the PEM is the complete file.

### Operational properties

- **Tokens expire hourly and are minted automatically.** uzi exchanges a signed
  JWT for an installation token valid for one hour, and mints a fresh one when
  fewer than five minutes remain. Workers receive only the installation token.
- **The private key never leaves the API server.** It is sealed at rest with a
  distinct encryption context from a PAT, so an App credential can never be
  opened as a PAT envelope, or the reverse.
- **The bot identity is `<app-slug>[bot]`** — for an App named `uzi-forge`,
  `uzi-forge[bot]`. No second human account exists to manage, and the App does
  not consume a user seat.
- **Projects v2 sync is not available on this path.** It needs the PAT's
  `project` scope, which an installation token does not carry — use the
  [personal access token](#personal-access-token) path if you need it.

## Least privilege: what uzi verifies

Same posture as [GitLab bot setup](./gitlab-bot-setup.md#least-privilege-what-uzi-verifies),
mapped onto GitHub's model. The two credential types are checked by different
code paths: a PAT is checked by its scopes, an App by the permissions on its
installation token.

**Personal access token:**

- **Token scope is exactly `repo`.** Anything more, including `workflow`, is
  refused at connect.
- **Only a classic PAT is accepted.** A fine-grained token is refused at connect
  — uzi cannot introspect its scopes, so it will not save one on a warning.
- **The bot is not a site admin.**
- **On every enabled repo, the bot's role is exactly Write.** Admin can push
  protected branches and change repo settings; below Write breaks sync.

**GitHub App:**

- **The installation token's permissions must equal the required minimum
  exactly** — `contents: write`, `issues: write`, `pull_requests: write`,
  `metadata: read`, `actions: read`, `checks: read`, `statuses: read`. A token
  carrying anything more or less is a violation.
- **The token's repository set must match the names configured on the
  connection exactly.** A missing or unexpected repository is refused.
- **The installation must belong to the configured App ID and must not be
  suspended.**
- **The bot identity must be a Bot account and not a site admin.**
- **Only github.com is supported.** A custom or self-hosted base URL is refused.

**Push/merge rights on the protected default branch, when uzi can determine
them.** If a **ruleset** covering `main` is readable, uzi reports whether the
credential can push or merge — authoritative. For a PAT that means the write
role; for an App it means the App itself, read from the ruleset's bypass actors
(`always` → can push and merge, `pull_request` → can merge, no bypass entry →
protected and safe). If `main` is protected only by **classic branch
protection**, uzi cannot read who may push or merge at write role (that detail
needs admin), so it reports the branch as protected but the push/merge finding
as **unverified**, never a false "safe".

**Checked at the same three points as GitLab**: at connect (blocking — an
over-privilege or fine-grained-token violation rejects the save and stores
nothing), on demand (**Check privileges** in Settings → Forge), and periodically
(the background sweep, `UZI_PRIVILEGE_CHECK_INTERVAL`).

**uzi refuses to run against a repo the bot could push or merge to `main` on —
and an unverified finding refuses too, not just a confirmed-bad one.** A
readable ruleset that lets the write role push or merge is a blocking finding
like GitLab's or Forgejo's: uzi won't let you enable the repo, and refuses to
start or claim a run against one that's already enabled. On a
**classic**-protected repo, where uzi cannot read who may push or merge at
write role, the finding comes back **unverified** rather than a guessed
answer, and uzi treats "could not confirm" as fail-closed — it refuses the
same as a confirmed violation, rather than assuming a ruleset it can't see is
safe. See [ADR-0238](../adr/0238-github-driver.md) for why classic protection
is undetectable at write role and why that case must fail closed. Because of
this, prefer a **ruleset** over classic branch protection: it's the only way
uzi's report (and the fix, once you protect `main`) is authoritative rather
than "unverified" no matter what you do to classic protection. An instance
admin can still override the refusal for one named repo, with a recorded
reason, if the risk is knowingly accepted — see [Admin settings](./admin-settings.md#guardrail-override-per-repo)
— but the override never waives the unverified case either: an admin can
accept a risk uzi told them about, never one uzi couldn't read. The other
guardrail layers hold regardless: the worker only ever pushes the agent's own
branch and never merges, so `main` cannot be written even where a ruleset's
coverage is unclear.

**A repo owner can now ask for that override themselves, but only where it's
waivable.** A blocking push/merge finding is **waivable** whenever uzi could
actually read who can reach `main` — a readable ruleset that admits the write
role, or a default branch with no protection at all. There, if the risk is
knowingly accepted, the repo owner clicks **Request admin approval** on their
Repos page (with a required reason of their own), an instance admin approves it
from **Admin → Blocked repos**, and the owner then retries Enable so the live
guard runs again. This is the common first-onboarding case on a repo whose
default branch simply isn't protected — including a **GitHub Free private
repository**, where rulesets and protected branches aren't available for private
repos, so `main` is left unprotected and the bot can reach it. What is **not**
waivable is the **unverified** case: `main` is protected by **classic** branch
protection that a write-role bot cannot read, so uzi cannot confirm who may push
or merge and fails closed. No override — requested or admin-granted — can waive
that; uzi offers no request action for it, and the only fix is to add a readable
ruleset.
