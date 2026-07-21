---
title: uzi CLI
order: 105
audience: user
---

# uzi CLI

`uzi` is the terminal control surface for the factory: it drives the same API
the web UI does, so anything you can do in a browser you can do headless —
list and follow runs, approve or reject a plan gate, read the judge's review,
manage workers and repos, and (read-only) admin state. Built for humans
(tables on a TTY) and agents (`--json`, documented exit codes) alike.

## 1. Install

```sh
brew install vtmocanu/tap/uzi-cli
uzi version
```

The formula builds from source (`vtmocanu/uzi` is a private repo), so you need
**group-read on `vtmocanu/uzi`** — unlike a public tap, `brew install` clones
the product repo over git-over-SSH using your own key.

On first run the CLI also drops a Claude Code skill at
`~/.claude/skills/uzi-cli/SKILL.md` (generated from the binary's own command
tree, so it never drifts). No manual step is needed, but you can force a
refresh — e.g. right after upgrading — with `uzi skill install --force`, and
check it with `uzi skill status` (details under **Bundled skill and
session-start hook**, below).

## 2. Point it at your instance

Every command needs a base URL: pass `--url`, set `$UZI_URL`, or run
`uzi login` (below), which saves it for you. Only `https://` URLs are
accepted — plus plain `http://` on `127.0.0.1`/`localhost`, for a local
compose stack — so a credential is never sent in the clear.

## 3. Authenticate

Two paths, one credential.

**Human — `uzi login`.** Browser-brokered, no loopback listener, so it works
over SSH and in containers:

```sh
uzi login
```

It prints a one-time code and a URL, opens your browser to **Approve CLI
login**, and waits. Type the code from your terminal, pick a scope, and
approve — the CLI's token is saved to `~/.config/uzi/credentials.toml`
(0600) the moment you do.

**Agent or CI — a static token.** Mint one in **Settings → Access**, copy it
once (it's shown exactly once), and set it:

```sh
export UZI_URL=https://your-uzi-instance
export UZI_TOKEN=uzc_...
uzi run list --json
```

`UZI_URL`/`UZI_TOKEN` need no `$HOME`, no browser, no cookie — the whole
headless path. **In GitLab CI, `UZI_TOKEN` must be a masked variable.**

## Commands

```
uzi login | logout | auth token [--with-token] | auth status | whoami
uzi run list | get <id> | logs <id> [--follow] [--after <seq>]
uzi run create --repo <id> --issue <iid>
uzi run approve <id> [--agent-source own|repo] [--exclude-agents a,b]
uzi run reject <id> [--message <text>]
uzi run cancel <id>
uzi run follow-up <id> [--message <text>]
uzi run inputs <id> [--json]
uzi review show <id> | resolve <id> <rec> | dismiss <id> <rec> --reason wont-do|not-an-issue
uzi review undo <id> <rec> | stats [--json]
uzi token list
uzi worker list | rm <id> | set-token <worker-id> <label> | set-token <worker-id> --default
uzi repo list
uzi admin users | runs | workers | usage | rate-limits
uzi skill status | install [--force] | install-hook | uninstall-hook
uzi version
```

Global flags: `--json`, `--url <url>`, `--quiet`, `--no-color`.

A few worth knowing:

- **No `worker create` and no `admin` writes.** Minting a worker join token
  returns a credential that reads decrypted secrets, and every admin write
  stays cookie-only — both are web UI actions by design.
- **`token` is list-only, and `worker set-token` is the one write near it.**
  See [Anthropic tokens](#anthropic-tokens) below for why the split falls
  exactly there.
- **`run approve` picks the subagent roster explicitly.** By default a run
  uses its own default roster; `--agent-source own|repo` overrides it
  (`own` = your template roster, `repo` = the agents the worker detected in
  the clone's `.claude/agents/`), and `--exclude-agents a,b` drops individual
  subagents from that source. `--exclude-agents` requires `--agent-source`.
- **`review show <id>`** (formerly `run review <id>`, still around as a
  hidden, deprecated alias) prints the judge's verdict, summary,
  recommendations, and triage tally for a run — see
  [Run judge](./judge.md#reading-a-review-from-the-cli) for the full `--json`
  contract. The rest of the `review` group (`resolve`/`dismiss`/`undo`/
  `stats`) triages recommendations — see
  [Reviewing and triaging from the CLI](#reviewing-and-triaging-from-the-cli)
  below. There's still no `rejudge` verb: re-running the judge spends the
  owner's Anthropic budget and stays a web action.
- **`run inputs <id>`** lists the steer queue — a table of `body` / `state`
  / `age`, newest first — same delivery states as the web pane, see
  [Run activity pane](./run-activity.md#steer-queue). `--json` prints the raw
  DTO instead. **Chat caveat**: a chat run seeds every turn as a follow-up
  row, so `run inputs` against a chat run lists the whole conversation, not
  just steering messages; an issue or CI-fix run's queue starts empty and
  only ever holds what you actually sent mid-run.
- **`admin` needs an admin-scoped token.** A default (`uzc_`) token gets
  exit 3 with an actionable message; mint an `admin_ro` (`uza_`) token in
  Settings → Access to use it. `uzi whoami` over a `uzc_` token reports
  `is_admin: false` even for an admin — that's the credential's own
  authority, not your résumé.
- **`uzi logout` is local-only.** It removes the stored credential; it does
  **not** revoke it server-side (see [Managing tokens](#managing-tokens)
  below).

## Reviewing and triaging from the CLI

`uzi review` reads a run's judge output and sets the same **Mark done** /
**Dismiss** triage the [run judge](./judge.md#triage-resolve-dismiss-and-count)
page does, from the terminal:

```sh
uzi review show <run-id>                                    # verdict + recommendations + triage
uzi review resolve <run-id> <rec-id>                         # mark a recommendation done
uzi review dismiss <run-id> <rec-id> --reason wont-do        # valid, not worth doing
uzi review dismiss <run-id> <rec-id> --reason not-an-issue   # false positive
uzi review undo <run-id> <rec-id>                            # clear a disposition
uzi review stats [--json]                                    # your triage tally, across all runs
```

`<rec-id>` is the short, git-style id `show` prints as the first column of
each recommendation (or the full UUID from `--json`); an unambiguous prefix
resolves against the run's **current** review, so you can paste straight out
of `show`'s output. An ambiguous prefix is a usage error (exit 2, "use a
longer id"); an id that matches nothing is a not-found (exit 4) with a
refresh hint — the review may have changed under a re-judge.
`dismiss` requires `--reason wont-do` or `--reason not-an-issue`; anything
else is a usage error (exit 2), raised before any request is sent. `undo` on
a recommendation with no disposition is treated as already-undone (a
friendly line, exit 0), not a failure.

`uzi run review <id>` still works — it's a hidden, deprecated alias for
`uzi review show <id>`.

**The mutation verbs are owner-only, whatever the token.** A `uzc_` token
drives `resolve`/`dismiss`/`undo`/`stats` on its own runs, same as clicking
the buttons yourself. A read-only `uza_` token can `show` anyone's review
(the same admin reach other judge reads get) and `stats` always reports the
token owner's *own* tally, but the read-only ceiling still holds for writes:
`resolve`/`dismiss`/`undo` against **another** user's review is refused
(exit 4, not found), exactly as for a non-admin `uzc_` token. On its
**own** runs, though, a `uza_` token can triage same as any owner — the
ceiling blocks reaching into someone else's review, not every write
everywhere.

## Anthropic tokens

You can hold several named [Anthropic credentials](./anthropic-token.md) and
point individual workers at them. The CLI can **read** that set and **move a
worker between its members** — it cannot change the set itself:

```sh
uzi token list                                 # labels, default flag, timestamps
uzi worker set-token <worker-id> console-key   # bind a worker to a named token
uzi worker set-token <worker-id> --default     # clear the binding
```

`set-token` takes a **label** (the name from `token list`), not an id, and
takes effect on that worker's next claim — no restart and no re-minted join
token. Passing both a label and `--default`, or neither, is a usage error
(exit 2) rather than a guess; an unknown label is refused rather than stored.
A bound worker's **chat** runs still spend your default token: the binding
covers the run lane only.

**Adding, renaming, re-defaulting and deleting a token are web-only, and
that is a security boundary rather than an unfinished feature.** A CLI token
is a bearer credential — a stolen `uzc_` is meant to be able to read and to
drive runs, but never to *replace the credentials it runs on*. If token
writes were reachable over Bearer auth, an attacker holding a leaked `uzc_`
could swap a user's Anthropic credential for their own and quietly redirect
every future run's spend. That is exactly the escalation the split prevents,
and it is the same reasoning that keeps `worker create` out of the CLI.
`set-token` sits on the allowed side because it mints nothing and hands back
no credential: it re-points a worker between tokens the caller already owns.

**Driving the API directly?** The old kind-path routes still work as
deprecated aliases over your *default* token: `PUT
/api/me/secrets/anthropic_token` rotates-or-creates it, and `DELETE
/api/me/secrets/anthropic_token` removes it. The DELETE alias now answers
**409** once you hold more than one token, because "delete the anthropic
token" stopped naming one row — delete by id instead
(`DELETE /api/me/secrets/anthropic_token/{id}`). All of these are cookie-only,
for the reason above.

## Agents: `--json` and exit codes

Every command prints a human table by default; pass `--json` for a stable
document instead. A pipe does **not** auto-switch formats — that would be a
silent contract change — so an agent always passes `--json` explicitly.

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | generic error |
| 2 | usage error (bad flags/args) |
| 3 | auth required / invalid / wrong scope |
| 4 | not found |
| 5 | conflict (e.g. the run already finished) |
| 6 | server unreachable / 5xx |

Branch on the exit code, not on stderr text — the wording is for humans and
can change. There's also no `--token` flag: a credential must never land on
`argv`, readable via `ps`/`/proc`. Use `$UZI_TOKEN`, or `uzi auth token`,
which reads a token from stdin.

## Bundled skill and session-start hook

**The skill itself.** The CLI installs (and self-upgrades)
`~/.claude/skills/uzi-cli/SKILL.md` on first run, generated from the binary's
own command tree — it never drifts from the CLI you actually have installed.
Every `uzi` command refreshes it best-effort before it runs (set
`UZI_SKILL_AUTO_UPGRADE=0` to disable that). `uzi skill install [--force]`
refreshes it explicitly — `--force` overwrites even a file you edited (your
edit is preserved to `SKILL.md.bak` first) — and `uzi skill status` reports
its path and whether it's installed and current.

**The session-start hook, opt-in.** The per-command refresh above only helps
once a `uzi` command has run — right after `brew upgrade uzi-cli`, a fresh
Claude Code session can still read the OLD skill before that happens. Run
`uzi skill install-hook` to narrow that window: it wires a Claude Code
`SessionStart` hook into `~/.claude/settings.json` whose command is
`uzi skill install`, so the skill is refreshed at session start rather than
waiting for your next `uzi` command.

The write is surgical and non-destructive:

- **Opt-in.** Nothing installs this for you; you run it yourself, once.
- **Merged, not clobbered.** It adds just our one hook entry to
  `~/.claude/settings.json`, alongside any hooks other tools already put
  there — those are left untouched.
- **Backed up first.** The prior file is copied to `settings.json.bak` before
  the first write.
- **Abort on malformed JSON.** If `settings.json` exists but doesn't parse,
  `install-hook`/`uninstall-hook` refuse to touch it rather than risk
  clobbering a hand-maintained file.
- **Idempotent.** Running `uzi skill install-hook` again is a no-op — it
  detects the hook is already present.
- **Visible in status.** `uzi skill status` (and `--json`) reports whether
  the hook is installed and current, alongside the skill's own state.
- **Reversible.** `uzi skill uninstall-hook` removes it, leaving every
  sibling hook intact.

The hook is best-effort and near-free to run: a failed refresh never blocks
session start, and `uzi skill install` is a version-gated no-op once the
skill is already current, so the hook costs almost nothing on a normal
session start.

## Config and credentials

`~/.config/uzi/config.toml` (URL, 0644) and `credentials.toml` (token, 0600 —
the CLI refuses to read it if it's group/world-readable). `$UZI_URL` and
`$UZI_TOKEN` override both files, which is why the headless path needs
neither.

⚠️ This path is fixed at `~/.config/uzi/` and does not honour
`$XDG_CONFIG_HOME` — deliberately: on at least one machine on this team that
variable points into a git-tracked, synced directory, and honouring it would
write a live token into version control.

## Managing tokens

> **A password change is NOT an incident-response control for CLI tokens. You
> must enumerate and revoke each one.**

If a laptop is lost, **Settings → Access → Revoke all** is the one-click
answer — it stops every `uzi` CLI and CI job using one of your tokens at
once. If you'd rather keep some, the token list gives you what you need to
decide: `token_prefix`, `last_used_at`, and `last_used_ip`. Revoke anything
you don't recognise, and treat an unfamiliar `last_used_ip` as the signal to
revoke, not just a curiosity.

![Settings → Access, the CLI token list with the Revoke all button](img/cli-access-settings.png)

There is no per-request audit log for CLI tokens — `last_used_ip` (updated at
most once a minute) is the only detection control the design has, not a full
trail.
