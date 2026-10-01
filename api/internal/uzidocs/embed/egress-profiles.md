---
title: Egress profiles
order: 65
audience: operator
---

# Egress profiles

An egress profile is a named site list: the hosts an official-sources research
run may read from ([PRD #1906](../prds/1906-official-sources-web-research.md)).
Admins create and edit profiles; a run names one profile, never a list of
domains.

**Where a profile is used.** A [job](jobs.md#site-lists) names a profile in
`egress_profile` when it is created ([PRD #1976](../prds/done/1976-site-list-jobs.md)),
and runs on the no-internet worker lane described in [Isolated research
lane](isolated-research-lane.md), reading only the hosts on the list. Your own
`uzc_` token may name any profile. A product (`uzp_`) token may name only a
profile an admin has allowed for its product (see below). Nothing else binds a
run to a profile, and a run without one reaches the network as before.

## Allowing a profile for a product

An admin allows a product to use a profile on **Admin → Products**: each
product card has a section listing its allowed site lists, where you add and
remove one. The same actions exist as `PUT` and `DELETE`
`/api/admin/products/<id>/egress-profiles/<name>`, which are browser-session
only (a Bearer token gets `401`). `GET /api/admin/products/<id>/egress-profiles`
and `uzi admin products egress-profiles <product>` read the set and work with a
`uza_` token. Removing an allowance affects only jobs created afterwards.
Deleting a profile removes its allowances; a profile that a run is bound to
still can't be deleted (`409`).

## Managing profiles

The web page is **Admin → Site lists** (`/admin/egress-profiles`): it lists
every profile with its hosts and warnings, and creates, edits and deletes them.

| What | How |
|---|---|
| List profiles | Admin → Site lists, `uzi admin egress-profile list`, or `GET /api/admin/egress-profiles` |
| Show one profile | `uzi admin egress-profile show <name>`, or `GET /api/admin/egress-profiles/<name>` |
| Create | Admin → Site lists → New site list, or `POST /api/admin/egress-profiles` |
| Replace | Edit on the profile's row, or `PUT /api/admin/egress-profiles/<name>` |
| Delete | Delete on the profile's row (asks to confirm), or `DELETE /api/admin/egress-profiles/<name>`. A profile that any run is bound to, finished runs included, can't be deleted: the api answers `409` and keeps it. |

A run's binding to its profile is permanent, so a list that has been used stays
until every run bound to it is deleted. There is no disable switch: to retire a
list, stop choosing it for new runs. Editing it changes only runs that haven't
started yet, because a run keeps the copy of the list it started with.

Reads work from an admin browser session or an admin-scoped (`uza_`) CLI token.
Create, replace and delete are cookie-only admin writes, like every other admin
write: a CLI token gets `401`, so the CLI is read-only and the Site lists page
is where profiles are written.

On the page, hosts are typed one per line. A refused save stores nothing and
shows each problem beside the entry it names, with its line number. A
multi-publisher entry (see below) shows a checkbox, such as "Allow every
publisher on github.com", with the reason; the page sends the override only for
entries whose box is ticked, and editing a profile shows its stored overrides
already ticked.

A create body:

```json
{
  "name": "vendor-x-docs",
  "description": "Vendor X official documentation and datasheets",
  "hosts": ["docs.vendor-x.com", "*.cdn.vendor-x.com"],
  "multi_publisher_override": []
}
```

A replace (`PUT`) body is the same without `name`: the name is fixed once
created, and every `PUT` replaces the description, hosts and overrides in full.

- **Name:** 1 to 64 characters of lowercase letters, digits and hyphens,
  starting with a letter or digit.
- **Description:** optional, at most 500 characters, no control characters,
  invisible formatting characters, or leading or trailing spaces.
- **Hosts:** 1 to 200 entries, each at most 253 characters.

A refused write stores nothing and answers `422` with every problem at once:

```json
{
  "error": "the egress profile is invalid: see problems",
  "reason": "invalid_egress_profile",
  "problems": [
    {"field": "hosts[1]", "entry": "*.github.io", "code": "public_suffix_wildcard", "message": "..."}
  ]
}
```

`multi_publisher_override` may name at most 200 entries, like `hosts`.

## Host entries

An entry is an exact host (`docs.vendor-x.com`) or a wildcard over a base
domain (`*.vendor-x.com`).

**Wildcards match subdomains only.** `*.vendor-x.com` matches
`docs.vendor-x.com` and `a.b.vendor-x.com`, but not `vendor-x.com` itself. List
both when you want both.

**Entries are normalized** when a profile is written, and hosts are normalized
the same way when they are checked: lowercase, the IDNA ASCII form
(`bücher.de` is stored as `xn--bcher-kva.de`), one trailing dot removed, and
surrounding spaces trimmed. Duplicates after normalization are folded into one.

**Refused entries**, with the `code` a `422` carries:

| Entry | Code |
|---|---|
| An IP address (`10.0.0.1`, `[::1]`) | `ip_address` |
| A port (`docs.vendor-x.com:8443`) | `port` |
| A URL (`https://docs.vendor-x.com`) | `scheme` |
| A path, query or fragment (`docs.vendor-x.com/guide`) | `path` |
| User information (`user@docs.vendor-x.com`) | `userinfo` |
| A bare `*` | `bare_wildcard` |
| `*` anywhere but as the whole leftmost label (`docs.*.vendor-x.com`) | `wildcard_position` |
| An empty label (`docs..vendor-x.com`) | `empty_label` |
| A single-label name (`intranet`) | `single_label` |
| A name ending in a numeric label (`1.2.3.999`) | `numeric_tld` |
| A top-level or registry domain as a host (`co.uk`) | `public_suffix_host` |
| Anything else that is not a valid host name (`exa_mple.com`) | `invalid_host` |

## Public suffix wildcards

A wildcard that could cover a public suffix is refused
(`public_suffix_wildcard`): it would cover sites run by unrelated owners. The
check uses the Public Suffix List built into uzi (both its ICANN and private
sections, pinned with the `golang.org/x/net` module) two ways:

- The base is a public suffix: `*.com`, `*.co.uk`, `*.github.io`,
  `*.cloudfront.net` and `*.s3.amazonaws.com` are refused.
- A public suffix lies below the base: `*.kawasaki.jp` (every child of
  `kawasaki.jp` is a suffix), `*.run.app` (`a.run.app` is one) and
  `*.digitaloceanspaces.com` (`nyc3.digitaloceanspaces.com` is one) are
  refused, although the base itself is not a suffix.

A few parents hand their subdomains to many different customers. A wildcard at
or under one of these is refused with its own code (`shared_parent_wildcard`),
with no override: `amazonaws.com`, `azure.com`, `windows.net`,
`googleusercontent.com`, `fastly.net`, `sharepoint.com`. Most of them also have
public suffixes below them in the list (`amazonaws.com`, `windows.net`,
`fastly.net`), which would refuse the wildcard anyway; a few, such as
`azure.com`, `googleusercontent.com` and `sharepoint.com`, do not, so this list
is what refuses them. List the exact hosts instead.

**Entries can go stale.** A newer uzi can carry a newer Public Suffix List, so a
wildcard accepted when the profile was written can be refused later. Such an
entry matches nothing, and every read of the profile carries a warning for it
(`code: stale_entry`). Edit the profile to fix or remove it.

## Multi-publisher hosts

The fetch service checks each request's host, not its path. On a host where
many publishers serve content under the same name (code hosting, path-style
object storage, documentation and package hosting, forums), allowing the host
allows every publisher on it: allowing `github.com` allows every repository,
not just the vendor's.

uzi carries a short built-in list of such hosts. An entry that reaches one is
refused (`multi_publisher_needs_override`) unless the same entry is also listed
in `multi_publisher_override`. An overridden entry is stored, and every read of
the profile carries a warning for it (`code: multi_publisher_override`); `uzi
admin egress-profile show` prints it as a `warning:` line. An override naming
an entry that is not in `hosts` is refused (`override_not_in_hosts`); one
naming an entry that is not multi-publisher is dropped.

**The list can grow under a stored profile.** A newer uzi can flag an entry
that was clean when the profile was written. Such an entry, stored without an
override, matches nothing, and every read of the profile carries a warning for
it (`code: multi_publisher_needs_override`). Add the override to accept every
publisher on it, or remove it.

The built-in list:

- **Exact hosts** (only the host itself is flagged; a sibling that is not
  listed is not, whatever it serves, so review siblings yourself):
  `github.com`, `api.github.com`, `gist.github.com`, `codeload.github.com`,
  `raw.github.com`, `gitlab.com`, `codeberg.org`, `gitee.com`,
  `s3.amazonaws.com` and the
  regional path-style S3 endpoints (`s3.<region>.amazonaws.com`,
  `s3-<region>.amazonaws.com`, and `s3.<region>.amazonaws.com.cn` in the China
  regions), `storage.googleapis.com`, `storage.cloud.google.com`,
  `dl.dropboxusercontent.com`, `docs.google.com`, `drive.google.com`,
  `sites.google.com`, `readthedocs.io`, `readthedocs.org`, `gitbook.io`,
  `docs.rs`, `pkg.go.dev`, `pypi.org`, `test.pypi.org`, `www.npmjs.com`,
  `hub.docker.com`, `ghcr.io`, `unpkg.com`, `cdnjs.cloudflare.com`,
  `registry.npmjs.org`, `files.pythonhosted.org`, `proxy.golang.org`,
  `repo.maven.apache.org`.
- **Whole domains** (the subdomains are shared too): `githubusercontent.com`
  (`raw.`, `objects.`, `media.` and the rest), `jsdelivr.net`,
  `huggingface.co` (including `cdn-lfs.huggingface.co`), `stackoverflow.com`,
  `stackexchange.com`, `reddit.com`, `quora.com`, `medium.com`,
  `substack.com`, `wordpress.com`, `blogspot.com`, `sourceforge.net`,
  `dropbox.com`, `npmjs.com`, `crates.io` (including `static.crates.io`),
  `rubygems.org`, `maven.org` (including `repo1.maven.org`), `bitbucket.org`
  (including `api.bitbucket.org`), `archive.org` (including
  `web.archive.org`).
- **Platform apexes:** an exact host that is itself a public suffix from the
  list's private section, such as `github.io`, `gitlab.io` or
  `cloudfront.net`. The platform hands the names under it to its customers, and
  its own host may serve them by path, so it needs the override too. (An ICANN
  suffix such as `co.uk` is refused outright as a host.)

A wildcard is flagged when it could reach a listed host: `*.github.com` covers
`gist.github.com`. A platform that gives each publisher its own subdomain is
one publisher per host, so an exact subdomain such as `vendor.readthedocs.io`
or `bucket.s3.amazonaws.com` is not flagged.

The list cannot be complete: a vendor's community forum, for example, is not
on it. Review each profile for hosts where other people can publish.

## Fetch caps

Five [admin settings](./admin-settings.md#research-fetch-caps) bound what one
research run may download: 25 MiB per file, 200 MiB and 100 files per run,
4 concurrent fetches per run, and 500 fetch attempts per run (allowed or
refused) by default. Edit them on **Admin → Instance →
Research fetch caps**. Like the profiles, nothing reads them until the research
lane is enabled.
