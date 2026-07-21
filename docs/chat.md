---
title: Chat with uzi
order: 95
audience: user
---

# Chat with uzi

The **Chat** page lets you talk to uzi about itself: what it can do, how a
feature works, why one of your runs failed, and turn an idea into a GitLab
issue without leaving the app. The conversation is answered by an agent
running on **your own worker**, billed to **your own Anthropic token** — the
same worker that executes your runs.

Specifically, chat always spends your **default** token, even on a worker you
have bound to a different one: the binding covers that worker's *runs*, not
its chat. See [Anthropic tokens](./anthropic-token.md) if you are reconciling
a meter against what you expected to spend.

## What it can see and do

- **uzi's own source.** The agent reads a copy of uzi's code baked into the
  worker image at build time (matching the exact version you're running), so
  answers cite real files, not guesses. It cannot see any other repository.
- **Your runs.** It can list your runs and read a run's messages and plan to
  answer "why did run #57 fail?" from the actual activity, not a hallucination.
  It only ever sees **your own** runs — never another user's, even for admins.
- **Draft a GitLab issue.** Ask it to file an issue and it shows you a
  **proposal card** with the draft title, description, and labels plus
  **Create** / **Dismiss** buttons. Nothing is written to GitLab until *you*
  click Create — the agent can draft, only your click files. Dismiss writes
  nothing at all.

## What it can never do

- Touch a repository, push code, or open a merge request — chat has no git
  access and no forge credential at all.
- Spend your Anthropic tokens without your worker running (see below).
- Act on another user's data.

These are the same guardrails as the rest of uzi: the chat agent has no
Bash, no network tools, and no credentials in reach. See
[ARCHITECTURE.md](../ARCHITECTURE.md#chat-with-uzi-the-fifth-surface) for the
trust model.

## Using it

- **You need your worker running.** Chat runs on the same per-user worker as
  your runs (`docker compose --profile agent up`). If no worker is connected,
  the Chat page says so instead of hanging — start your worker and try again.
- **Conversations are turn-capped and idle-bounded.** A chat ends after a
  period with no messages, or at a per-conversation turn limit, to bound token
  spend. An ended conversation shows a **Continue** button that picks it back
  up (resuming its context when your worker still has it).
- **Admins can view chat conversations**, like all runs — treat a chat the
  way you would any run in a shared instance.

## A note on trust

The agent can read run logs and issue text that may have been written by other
people or tools. It treats all of that as untrusted evidence to report on, not
as instructions to follow. Even so, always read a proposal card before you
click Create — the draft is the agent's suggestion, and Create is your
decision.
