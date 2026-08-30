---
title: Update checks
order: 73
audience: user
---

# Update checks

uzi periodically asks GitHub for the newest `vtmocanu/uzi` release and tells you
when this instance has fallen behind — no more finding out by manually checking
GitHub. The check runs **server-side**, on the api, every `UZI_RELEASE_CHECK_INTERVAL`
(default `6h`); the browser and the CLI never call GitHub themselves, they only read
what the api last found.

## Where it shows up

- **Everyone** sees a small brand-colored pip on the sidebar version badge when an
  update is available, and a "vX.Y.Z available" row in the build-info popover (hover
  or focus the version badge to open it). An admin gets an **Update guide** link from
  that row into Admin → Instance settings → Updates; a member sees "Ask your operator
  to update" instead, since only an admin can act on it. The collapsed sidebar rail
  has no room for the pip and drops it.
- **Admins** get a full **Updates** card under Admin → Instance settings: the
  current-vs-latest version delta, the release name and date, a short plain-text
  excerpt of the release notes with a link out to the full notes on GitHub, a
  copyable upgrade runbook (helm and docker compose), a **Check now** button, and
  the two toggles below.
- **Admins** also see an escalation banner at the top of the app when the instance is
  *far* behind — see [Far behind and security releases](#far-behind-and-security-releases).

`uzi version` on the CLI prints the same signal as an `update` row
("`update  v0.66.0 available`") whenever the server reports one — see
[uzi CLI](cli.md).

None of these ever run a client-side version comparison. The comparison is a known
sharp edge (a bare server version vs. a `v`-prefixed GitHub tag, and a broken
comparator that silently reports "up to date" for a version that is actually behind),
so it is done exactly once, server-side, and every surface only renders the boolean
the server already computed.

## Far behind and security releases

The escalation banner is reserved for the case worth interrupting an admin for: it
shows only when the banner toggle (below) is on **and** the instance is "far behind" —
a major version behind, three or more minor versions behind, or the latest release is
at least 30 days old. A release whose notes carry a `### Security` heading (uzi's
CHANGELOG uses [Keep a Changelog](https://keepachangelog.com/) `### Security`
subsections, and the GitHub release body is generated from that section verbatim) is
flagged as a security release on the banner and on the Updates card, in red.

Dismissing the banner snoozes it for that release **tag**, server-side — not on a
timer and not per browser session. The snooze clears itself automatically the moment
a newer release ships, so it can't accidentally silence a future warning.

## Air-gapped and privacy installs

Two independent admin toggles, both **on by default**, both editable at runtime from
Admin → Instance settings → Updates with no redeploy:

- **Enable update checks** (`release_check_enabled`, env `UZI_RELEASE_CHECK_ENABLED`)
  — the master gate. Turn it off and the api never contacts `github.com` again: no
  pip, no popover row, no card data, no banner, and no hourly poll error to silence.
  This is the switch for an offline, air-gapped, or privacy-conscious install.
- **Show the escalation banner** (`release_check_banner_enabled`, env
  `UZI_RELEASE_CHECK_BANNER_ENABLED`) — governs only the banner. The pip, the popover
  row, and the Updates card keep working with this off; it just stops the loud
  surface.

Both are seeded from environment/helm at first boot only (`UZI_RELEASE_CHECK_ENABLED`,
`UZI_RELEASE_CHECK_BANNER_ENABLED`, plus `UZI_RELEASE_CHECK_INTERVAL` for the poll
cadence, default `6h`) — see [Configuration](configuration.md). That seeding never
overwrites a value an admin has already set: flipping a toggle off in the UI survives
a redeploy even if the env var is still `true`.

## Rate limits and the optional token

GitHub's unauthenticated REST API allows 60 requests/hour per IP. One instance
polling every 6 hours is trivially inside that, but every instance sharing an egress
IP (a NAT'd fleet) shares the same budget. If you run several instances behind one
IP, or want a shorter interval, set `UZI_RELEASE_CHECK_TOKEN` to a GitHub token —
it raises the ceiling to 5,000 requests/hour and is stored as a write-only secret,
never shown again once saved. It is not needed at the default cadence.

## Admin: Check now

The **Check now** button on the Updates card runs the same check the scheduled poll
runs, immediately, and refreshes the card from the result. Use it to confirm the
toggles and connectivity are working, or to pick up a release that just shipped
without waiting for the next scheduled poll. It's disabled while update checks are
off.

The card also states plainly when there's nothing to show: **"Update checks are
turned off"** (the master toggle is off), **"No release check has run yet"** (never
checked — click Check now), or a check that failed (rate-limited or unreachable) with
whatever detail GitHub returned. Nothing else on the instance surfaces those states in
words — the pip and banner just stay quiet.

## Design and trust

The outbound-egress path this adds (api → `api.github.com`) and why it bypasses the
forge/agent-source SSRF allowlists (it targets a compile-time-constant URL, not a
user-supplied one) are recorded in
[ADR-836](../adr/0836-upstream-release-check.md).
