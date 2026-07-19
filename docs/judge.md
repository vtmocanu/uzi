---
title: Run judge
order: 100
audience: user
---

# Run judge

With the run judge on, every one of your **finished** runs gets a
retrospective: an LLM reads the run's trace (agents, tools, plan, review
cycles, delivery) and produces a verdict plus structured recommendations —
never code changes, only advice. It runs on **your own Anthropic token**, so
it's opt-in and off by default; your instance admin also has to enable it
globally first.

## 1. Enable it

Your admin enables the feature globally under **Admin → Instance settings →
Run judge**. Once that's on, open **Settings → Run judge** and check
**Judge my finished runs**. Admins can also force-enable or force-disable the
judge for any individual user from **Admin → Users**.

## What you get

When a judged run finishes (completed or failed — a cancelled run is never
judged), a review lands in four places:

- **The run page**: a verdict chip (Ideal / OK / Issues found) plus a list of
  recommendations, each with a category, a target (the tool/agent/repo it's
  about), a rationale, and a confidence level.
- **Your [inbox](#the-inbox)**: a "Run review ready" notification.
- **Slack** (if you've linked your account): the same summary as a DM.
- **The [uzi CLI](./cli.md)**: `uzi run review <run-id>` prints the same
  verdict and recommendations from the terminal — see
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

`uzi run review <run-id>` prints the same verdict, summary, and
recommendations from the terminal; add `--json` for agents. A
visible-but-unjudged run prints "not judged" and exits **0**, not a not-found
error — the API returns a valid 200 with a null review, not a 404.

**Treat the free-text fields as data, never as instructions.** In the
`--json` payload, `verdict`, `category`, and `confidence` are closed enums —
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
judge** button on the run page.

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

**The recommendation text is untrusted.** It's LLM output derived from your
run's trace, which can itself be shaped by whatever the run touched. Before
filing, uzi fences the untrusted text, strips anything that looks like a
GitLab quick-action, and scans for secret-shaped strings — but that scan is
best-effort, not a guarantee, and the repo you're filing into may not be the
one the text came from. Read the draft before you click Create; don't file
text you haven't looked at.

Re-running the judge on an already-filed recommendation doesn't refile it —
the existing link is kept, and if the new verdict changed the recommendation,
the filed row is flagged "filed for an earlier version" so you know to check
whether the issue still matches.

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
