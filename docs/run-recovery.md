---
title: Recovering unpublished work
order: 113
audience: user
---

# Recovering unpublished work

Sometimes a run commits real work but fails at the very last step: a
workflow or secret preflight check, a rejected push, or exhausted base
alignment against a moving default branch. Before this failure can happen,
uzi captures the run's **original committed history** — exactly what the
agent committed, not a diff or a rebased copy — into a durable, encrypted,
owner-only Git bundle stored in the database. This is your recovery path
even after the worker and its disk are gone.

uzi captures at other boundaries too, not only at final failure: when a run
is parked (a usage limit or a recovery wait) or gracefully stopped while it
holds committed work, uzi archives that exact work before its source can be
torn down. If the same boundary can *prove* the work is already published,
it releases the hold instead of archiving; if it can neither prove that nor
archive, it keeps the source held for you to decide (see below). A hard
crash with no shutdown window — a killed node, an out-of-memory kill — is
outside this guarantee.

This is a safety net for a run that already produced real commits, not a
guarantee against every possible failure. It does **not** cover uncommitted
files, the SDK transcript, the worker's HOME directory, or a worker that
died before it reached this protected boundary.

## What you'll see

The run page shows a **Recovery archives** section on any run that failed to
finalize, and on any run that already has a retained capture even before it
reaches a final status. Each retained capture is in one of these six states:

- **Preparing** — the original committed history is being captured at the
  protected boundary; nothing is downloadable yet.
- **Uploading** — the encrypted archive is being stored; nothing is
  downloadable yet.
- **Available** — the original committed history is ready to download.
- **Needs action** — the archive could not be completed (for example, a
  storage limit was hit); the source is still retained and will be retried.
- **Expired** — the ready download window passed and the bytes were purged.
- **Discarded** — the archive was explicitly discarded and its bytes deleted.

When a run has **no** retained captures at all, the section reports one of
three whole-section conditions instead of a per-capture state:

- **Preparing** — recovery is armed and a custody hold is open, but no
  capture has been registered yet (the archive is still being produced).
- **Unsupported / legacy** — the run predates durable recovery (an old
  worker or server never captured anything for it).
- **Unavailable source** — recovery was armed but nothing needed capturing,
  or the source could not be preserved.

Every download surface warns that the original may contain secrets: review
it before publishing anywhere, and if it exposed a real credential, revoke
and rotate it and remove it from the affected history — deleting the
archive alone does not undo that exposure.

## Downloading an archive

From the run page, click **Export archive** once a capture shows
**Available**. From the CLI:

```sh
uzi run export <run-id> --output ./recovered.bundle
```

- **Owner-only.** You can export only your own runs; an admin viewing
  someone else's run is refused. The download talks to the API directly, so
  it works even if the worker that ran the job no longer exists.
- **Explicit capture choice.** A run can retain more than one archive. With
  exactly one `available` capture, export uses it; with more than one, it
  refuses to guess and lists every capture's id and state so you can pick
  with `--capture <id>`.
- **Verified and atomic.** The byte count and checksum are verified against
  the server's manifest before the file is written, and the destination is
  never overwritten (an existing file or symlink there is refused).
- **No partial files.** An interrupted, corrupt, or expired download exits
  nonzero and leaves nothing at the destination.

See [the CLI reference](cli.md#recovering-unpublished-work-uzi-run-export)
for the full flag and exit-code contract.

## Importing the bundle

A downloaded archive is a real Git bundle carrying the original committed
head on a dedicated ref, importable into a clean clone of the forge repo:

```sh
git bundle verify recovered.bundle
git clone <your-forge-repo-url> recovered-repo
cd recovered-repo
git fetch ../recovered.bundle refs/heads/recovered-source:refs/heads/recovered-source
git checkout recovered-source
```

Where possible the bundle relies on your repo's existing default-branch
history as its prerequisite (so it stays small); when no such history was
reachable, it is self-contained instead. Either way it imports with
identical commit ids, parent relationships, tree, and binary contents.

## Limits

These are the shipped defaults. Every one below except the last is an
operator-configurable environment variable on the API:

| Limit | Default | Meaning |
|---|---|---|
| Max bundle size | 64 MiB | A larger archive is refused, not truncated; the source stays retained and is retried once capacity allows. |
| Ready payload per owner | 1 GiB | Total bytes of *your* ready (downloadable) archives. |
| Instance byte quota | 4 GiB | Total ready bytes across the whole deployment (scoped to the owner who breached it, never a global stop). |
| Ready-artifact retention | 7 days | How long a ready archive stays downloadable, counted from when it became ready. |
| Automatic upload-retry window | 24 hours | How long uzi keeps retrying a stalled upload before it needs your attention. |
| Captures per claim | 16 | Distinct capture attempts one worker claim can accumulate. |
| Retained captures per owner | 256 | Total captures you can have on file at once. |
| Unresolved recovery holds per owner | 8 | At the limit, uzi pauses admitting **new** runs for you — existing runs are unaffected — until you resolve or discard some. Fixed today, not yet an environment variable. |

## Custody: why a worker won't disappear

While a run's committed work is unpublished and not yet durably captured
(or already captured but not yet resolved), the worker — and its disk —
that holds it is kept around rather than torn down, even if it would
otherwise be idle or recycled. Such a worker shows a **retaining work**
badge in the worker list. Deleting that worker is refused until the work is
recovered or explicitly discarded, and the refusal names how many holds are
blocking the delete and the exact commands to clear them (`uzi run recovery`
to list, then `uzi run export` or `uzi run discard`). `uzi worker rm` never
force-discards held work for you. Removing a repo that still has a run
retaining custody this way is refused the same way, naming how many runs are
retaining unpublished work.

Each retained claim keeps its own hold, keyed to the exact claim that
produced the work, so a later run on the same worker never drops an older
claim's only copy.

## Reviewing and resolving held work

At most **8 unresolved holds per owner** (the *Unresolved recovery holds*
limit above) can accumulate before uzi pauses admitting **new** runs for you.
When that happens, the dashboard shows a full-width alert beneath the page
heading with your safety-slot use, how many held sources need a decision, how
many runs are blocked, and a **Review held work** button; if you connected
Slack, you also get one coalesced direct message per blocked episode (not one
per run) carrying the same counts and the exact discard command. The
per-worker **retaining work** pills stay as row context.

The **Workers** settings page is where you resolve them: every retained hold
is grouped by worker with the run it came from, its claim generation, and a
state that separates normal active protection from holds that actually need
you. Each row offers the right action for its state:

- **Export archive** — download a ready archive (same bundle as `uzi run
  export`). An archive-ready hold releases itself automatically once you have
  a copy; it offers Export only and does not count as needing a decision.
- **Discard held work** — for a hold whose source may be the only copy (no
  ready archive can restore it), permanently release custody so the worker
  and its disk can be torn down. The confirmation names the run, worker, and
  claim generation, and warns that this can destroy the work for good.

On the run page, the **Recovery archives** section offers **Export archive**
and, once you have a copy, **Delete archive** — archive-artifact cleanup that
is deliberately distinct from discarding a held source. Deleting an archive
while its hold is still open does not resolve custody; left untouched, an
archive simply expires after its retention window instead (see Limits above).

From the CLI, list a run's holds and captures with `uzi run recovery
<run-id>`, then discard one exact held source:

```sh
uzi run recovery <run-id>
uzi run discard <run-id> --hold <hold-id> --yes
```

`uzi run discard` targets one exact hold, prompts for confirmation
interactively, and requires `--yes` when there is no terminal. It never
touches a ready archive or any other hold. See
[the CLI reference](cli.md#recovering-unpublished-work-uzi-run-export) for the
`uzi run export` flag and exit-code contract.

## What this is not: the threat model

This is **server-side encryption at rest**, not end-to-end encryption. The
API encrypts every archive under its own master key before storing it, and
only the API ever holds that key — the worker that produced the archive
never sees it. That protects a stolen database dump or a stray backup. It
does **not** protect against an operator who already holds that key: this
system is not designed to hide your history from whoever administers the
uzi instance you're running on, only to keep it from anyone else and from
casual exposure in logs, previews, or other run data.

## Mixed fleets

Durable recovery needs a worker and an API that both support it. A run that
executed on an older worker, or against an older server, is honestly
reported as **unsupported** rather than silently promised a recovery that
was never captured. Upgrading your fleet only protects runs going forward.

Related: [Recovering from an empty turn](run-recovery-wait.md) (a different,
earlier mechanism — a transient in-place retry, not a byte archive) ·
[Hosted workers](hosted-workers.md) · [CLI](cli.md)
