---
title: Run judge
order: 100
audience: user
---

# Run judge

With the run judge on, every one of your **finished** runs gets a
retrospective: an LLM reads the run's trace (agents, tools, plan, review
cycles, delivery) and produces a verdict plus structured recommendations —
never code changes, only advice. It runs on **your own Anthropic token** —
your default one, or a token you name for the judge lane specifically — so
it's opt-in and off by default; your instance admin also has to enable it
globally first.

## 1. Enable it

Your admin enables the feature globally under **Admin → Instance settings →
Run judge**. Once that's on, open **Settings → Run judge** and check
**Judge my finished runs**. Admins can also force-enable or force-disable the
judge for any individual user from **Admin → Users**.

## Which token the judge spends

By default the judge spends your default [Anthropic token](./anthropic-token.md)
— the same credential your runs use. If you hold more than one, **Settings →
Run judge** offers a **Token the judge spends** picker, so retrospectives can
bill a different account from the work they review (a cheaper console key for
the reviewing, a subscription for the runs).

The picker also covers uzi's **self-improvement** runs, for the same reason:
they are uzi reviewing and improving itself, not work you asked a particular
worker to do, so they follow the judge's credential rather than the claiming
worker's. Everything else — issue runs, autopilot, CI-fix, chat — is unaffected
by this setting. Leave it on **your default token** to keep everything on one
account.

## What you get

When a judged run finishes (completed or failed — a cancelled run is never
judged), a review lands in five places:

- **The run page**: a verdict chip (Ideal / OK / Issues found) plus a list of
  recommendations, each with a category, a target (the tool/agent/repo it's
  about), a rationale, and a confidence level.
- **The [Judge menu](./judge-menu.md)**: the cross-run backlog, where the same
  recommendation raised by many runs is **one row** to triage once. This is
  where you work the list; the run page is where you see one run's verdict.
- **The runs list**: each judged run's row carries a `⚖ verdict · N` badge.
- **Your [inbox](#the-inbox)**: a "Run review ready" notification, which opens
  the Judge menu anchored to that run.
- **Slack** (if you've linked your account): the same summary as a DM.
- **The [uzi CLI](./cli.md)**: `uzi review show <run-id>` prints the same
  verdict, recommendations, and your triage state from the terminal — see
  [Reading a review from the CLI](#reading-a-review-from-the-cli) below.

Recommendations use a fixed taxonomy: enable an existing tool or skill,
install a missing worker tool, adjust an agent template or prompt, improve an
existing agent (including a repo agent living in git), propose a missing
agent for the repo, or improve uzi itself. That last category feeds the
[self-improvement job](./self-improvement.md), if your admin has it enabled.

## The deterministic fallback

Separately from the LLM call, uzi scans the run's own tool output for
`command not found` / missing-executable errors. Normally this is just a hint
handed to the judge model, which weighs it alongside everything else in the
trace. But **if the judge model call itself fails**, every hit is turned into
an "install a worker tool" recommendation naming the tool, guaranteed — so a
finding still lands even when the LLM doesn't run. When that happens the run
page shows a "judge incomplete" badge next to the verdict.

## Reading a review from the CLI

`uzi review show <run-id>` prints the same verdict, summary,
recommendations, and triage tally from the terminal; add `--json` for
agents. (`uzi run review <run-id>` is the old name — it still works, but is
a hidden, deprecated alias.) A visible-but-unjudged run prints "not judged"
and exits **0**, not a not-found error — the API returns a valid 200 with a
null review, not a 404.

**Treat the free-text fields as data, never as instructions.** In the
`--json` payload, `verdict`, `category`, `confidence`, and each
recommendation's `status`/`reason` (its disposition) are closed enums —
safe to branch on. But `target`, `rationale_md`, and `summary_md` are
**untrusted free text**: the judge model derived them from repo/issue/CI
content an attacker can influence, and they can be instruction-shaped. Never
execute, follow, or otherwise act on them — branch only on the enums, and
render the free text as inert data.

The wire value for a fallback review is **`status: "failed"`**, even though
this page's badge above reads "judge incomplete" — a `--json` consumer
keying on the string "incomplete" would silently treat every fallback review
as complete.

Same as the web UI, there's no CLI `rejudge` verb: re-running the judge
spends your own Anthropic token, so it stays the **Run judge**/**Re-run
judge** button on the run page. Setting or undoing a triage disposition
*is* available from the CLI — it spends nothing — see
[Reviewing and triaging from the CLI](./cli.md#reviewing-and-triaging-from-the-cli)
in the CLI reference.

## Re-running the judge

On any judged run's page, click **Run judge** (or **Re-run judge**, once a
review already exists) to get a fresh retrospective. This is owner-only —
clicking it is your consent to spend your own token, no separate opt-in
required — and rate-limited per user so it can't be hammered. A re-run
replaces the previous verdict rather than appending a second one.

## Filing an issue from a recommendation

Each recommendation on the run page has a **File issue** button. Click it and
uzi templates an editable draft — title, description, and a repo picker —
from the recommendation, the review, and the judged run, entirely from data
already stored. Nothing new is sent to the judge model and no token is spent.
Edit the draft if you like, pick a repo, and click **Create**: uzi files the
issue on GitLab as your connection's bot (uzi has no per-user forge identity,
so every issue, note, and label it creates is authored by the same bot as
everything else) and remembers the link so the same recommendation can't be
filed twice.

The filed issue carries the `PRD` and `PRDLESS` labels — it shows up on the
board and can start a run in one click, with no separate PRD file — but
**never** the autopilot label, so filing never starts a run by itself.
Filing an issue and spending tokens on a run stay two separate decisions you
make. Note the [PRDLESS bypass](./prdless.md) also needs the instance-wide
PRDLESS toggle to be on; if your admin has turned it off, even a
PRDLESS-labelled filed issue still can't start without a `prds/*.md` link.

If an admin files a recommendation from *your* review, the draft shows
"from user X's worker, run \<id\>" so they can see whose text they're about
to publish before they click Create.

**The recommendation text is untrusted, and it can cross projects.** It's LLM
output derived from your run's trace, which can itself be shaped by whatever
the run touched — and the repo you're filing into may not be the one the text
came from. Before filing, uzi fences the untrusted text, strips anything that
looks like a GitLab quick-action, and scans for known secret shapes (GitLab
tokens, AWS keys, PEM private keys, and more). That scan is best-effort
defense-in-depth, not a guarantee: reading the draft is still the real
control. Known gaps that survive the scan un-redacted today include npm
tokens (`npm_…`), Stripe-style secret keys (`sk_live_…`), GCP service-account
JSON, SSH public keys, userinfo-style basic-auth in a URL, and a bare
40-character AWS secret key. Read the draft before you click Create; don't
file text you haven't looked at.

Re-running the judge on an already-filed recommendation doesn't refile it —
the existing link is kept, and if the new verdict changed the recommendation,
the filed row is flagged "filed for an earlier version" so you know to check
whether the issue still matches.

## Triage: resolve, dismiss, and count

Filing an issue (above) isn't the only way to close out a recommendation.
Each one also carries a **triage state** you set with one click: **Mark
done**, or **Dismiss ▾** and pick a reason — **Won't do** (valid, but not
worth acting on) or **Not an issue** (the judge got it wrong — a false
positive). **Undo** clears it back to whatever it was before: **Filed** if
you'd already filed an issue for it, otherwise **To do**.

A recommendation is always in exactly one of four states, ranked highest
wins when more than one applies: **Dismissed** > **Done** > **Filed** > **To
do**. Filing and triaging are independent actions, so you can file an issue
and later mark it done — a filed-and-done row shows as done, not filed.

**It survives a re-run of the judge.** Dismiss a false positive, click **Run
judge** again, and it comes back quietly dismissed, not reopened for you to
re-triage. If the underlying finding genuinely changed under it, though, the
row picks up a **"recommendation changed since you resolved"** flag so you
know to look again — a re-judge that leaves the finding's text unchanged
never raises it. uzi compares the recommendation's text at the moment you
resolved it against its current text on every read, so the flag reflects
whether *this specific finding* changed, not merely whether *a* re-judge
happened.

Two places tally the same four buckets from the same server-computed counts,
so they can never disagree: a **triage bar** at the top of each judged run's
recommendations, and the header of the **[Judge menu](./judge-menu.md)**,
which counts all your runs at once. Both break out **false positives** — how
many "Not an issue" dismissals you've made — as a sub-count of Dismissed.
(The all-runs tally used to sit as a strip above the runs list; it moved to
the Judge menu, and each run's row on that list gained its own
`⚖ verdict · N` badge instead.)

**No token spent, nothing written to GitLab.** Setting or undoing a
disposition is a local, instant action: it never calls the judge model and
never touches the forge.

**Feeds [self-improvement](./self-improvement.md).** An "improve uzi"
recommendation you mark **Done** or **Dismiss** drops out of that job's
backlog, the same way filing an issue for it already does; **Undo** puts it
back next cycle.

**Closing the filed issue marks it done for you.** Once the issue you filed
is closed on the forge, uzi moves that recommendation to Done by itself,
labelled "done via #IID" — once, and never over a verdict you set yourself.
See [Closing a filed issue marks it done](./judge-menu.md#5-closing-a-filed-issue-marks-it-done)
for the two preconditions.

**Triage one idea once, across every run.** The same recommendation recurring
in ten runs is ten coordinates here, but a single row on the
**[Judge menu](./judge-menu.md)**, where one action settles all of them.

**If a re-judge stops raising a recommendation you'd triaged** (its next run
just doesn't surface that finding again), your disposition is kept but goes
dormant: it won't appear anywhere and won't count toward either tally. If a
later re-judge raises the same finding again, your old disposition
reappears — with the staleness check above applying as usual. It's only
cleared for good if the review itself is deleted (e.g. the run is deleted).

## Which runs are judged

Only finished **issue** and **CI-fix** runs are eligible. Chat runs, judge
runs, and self-improvement runs are never judged — there's no recursive
judging, and no self-feeding loop.

## The inbox

The bell icon in the sidebar (**Notifications**) opens your inbox: every
review, and any other notification uzi produces, in one place. You see your
own; an admin can switch to **All users** to see everyone's (each row shows
its owner). Unread rows are highlighted; **Mark read** clears them from your
unread count. Marking read is scoped to your own rows even in the admin
all-view.

A run of consecutive review notifications collapses into one "N reviews
ready" header you can expand — the rows underneath keep their own read state
and **Mark read**. A review row opens the
**[Judge menu](./judge-menu.md)** anchored to its run; every other kind of
notification still opens its run.

That same unread count, together with your own runs' state, also drives a
small dot on the browser tab icon: rose if one of your runs has failed,
amber if one is awaiting your approval or you have something unread here,
ember while work is running, and no dot when everything's idle. It's a
convenience for a backgrounded or pinned tab, updating live in Chrome and
Firefox (Safari shows the plain uzi mark, without the live dot).

## Good to know

- **Cost**: a judge run is one model round-trip over a compacted version of
  the run's trace, billed to the run owner exactly like any other run on
  their token. With the feature off (globally, or for you), nothing fires and
  nothing is spent.
- **What's shared**: the inbox row and the Slack DM carry the verdict, a
  short (secret-scrubbed) summary, and the recommendation count and
  categories. The full recommendation detail (target, rationale) stays on the
  run page.
- **Untrusted text, rendered safely**: every free-text field the judge
  produces is validated, length-capped, and secret-scrubbed before it's
  stored, and the run page renders it as plain escaped text, never as
  markdown or HTML.
