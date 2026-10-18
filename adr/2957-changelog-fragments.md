# ADR-2957: Independent changelog fragments

**Status**: Accepted
**Date**: 2026-10-16
**Issue**: #2957

## Context

Parallel PRs editing the same `[Unreleased]` headings create avoidable conflicts,
duplicate categories and stale-copy resolutions. The release oracle must still
account for shipping commits; replacing a shared authoring location must not
weaken that identity check or rewrite shipped candidate notes.

## Decision

Ordinary PRs create a new lowercase UUID-v4 `changelog.d/<uuid>.md` before a PR
exists, initially citing the relevant issue. There is no shared index. A fragment
holds one canonical category and one outcome/description block; the exact grammar
and contributor commands live in [changelog.d/README.md](../changelog.d/README.md).

Existing legacy `[Unreleased]` bullets remain until the next release, and direct
additions remain accepted during migration. The PR check retains `Changelog: none`,
docs-typed and dependency-manifest exemptions. Its
[implementation](../scripts/check-changelog-entry.sh) validates changed fragments
before accepting an entry or exemption; a valid new path absent at the merge base
can supply an entry, while editing or deleting inherited paths cannot.

The [release oracle](../scripts/assert-changelog-covers-release.sh) separately
checks shipping commit identity in the assembled release section. Fragment
touches are not an exemption. The structural parser does not establish whether
a citation covers a shipping commit.

## Deterministic assembly and consumption

[The renderer](../scripts/lib/changelog-fragments.awk) emits categories in this
order: Added, Changed, Deprecated, Removed, Fixed, Security. Within each category,
legacy blocks stay in existing order, followed by fragments in C filename order.
Duplicate categories group together; equal prose is retained. Legacy bullet
physical lines are preserved, including one-line and multiline forms, while
blank separators normalize. The renderer emits the pending body without rewriting
the changelog preamble or released sections.

[The cutter](../.agents/skills/uzi-release/scripts/release-cut.sh) validates and
captures pending inputs before mutation, requires committed mode-100644 fragments,
and records the exact incorporated paths. It assembles notes and consumes those
paths in the release commit. Later RCs append new blocks without modifying frozen
shipped blocks; amendments use new fragments. The existing
[RC block check](../scripts/changelog-section.sh) remains in force.

Promotion-only promotes the candidate's own commit, leaving main's pending
fragments for the next cut. `--changelog-file` refuses pending fragments before
mutation, avoiding silent replacement of fragment notes by a manual draft.
Explicit promotion-only and fragment-free drafts that replace the legacy body
(first RC, stable or combined promotion) bypass its category/bullet grammar because
they do not incorporate it. Later-RC and skip-promote folds keep strict legacy parsing.
Fragment inventory, byte limits,
control-byte and section checks remain in force.
The [RC train](1265-rc-release-train.md) remains the release state machine.

This preservation boundary belongs to pending assembly. The surrounding release
pipeline still runs [link maintenance](../scripts/changelog-links.sh) and a
new-base cut reconciles the previous stable heading date from its tag. It does
not promise that released links and dates remain untouched throughout preparation.

## CI metadata classifier boundary

In [scripts/release-ci-coverage.py](../scripts/release-ci-coverage.py), `metadata`
classifies a canonical fragment deletion as metadata when its raw diff has
status `D`, old mode `100644`, new mode `000000`, and the lowercase UUID-v4 path
grammar. This decision is based on path, status and mode alone. It does **not**
validate deleted content, establish that notes were incorporated, or prove
shipping identity coverage. A malformed fragment at such a deleted path can
still satisfy this classifier predicate.

Fragment validation, cutter assembly/consumption and the release oracle enforce
their separate boundaries. The classifier permits these release metadata hops
under the existing bounded CI-inheritance policy; it is not an incorporation
proof and does not exempt arbitrary fragment additions, edits or mode changes.

## Bounded input and failure handling

[The shared input library](../scripts/lib/changelog-fragments.sh) and
[CLI](../scripts/changelog-fragments.sh) enforce these current limits:

| Input | Limit |
|---|---:|
| Fragment | 65536 bytes |
| Fragment count | 1024 |
| Combined pending legacy body plus fragments | 4194304 bytes |
| Full legacy changelog input | 16777216 bytes |

Parsing uses bounded snapshots, reading at most the applicable limit plus one
byte before rejecting oversized input. Paths use the canonical ASCII grammar;
diagnostics quote unsafe path bytes in the C locale. Symlinks and nonregular
entries are rejected. NUL/control checks and the parser's UTF-8 bidi-control
rejection protect parsing and diagnostics. Directory enumeration checks its
producer's exit status and partial records before sorting; producer failure
fails closed rather than looking like an empty inventory.

Preflight failures preserve inputs. Once preparation starts, failure or
interruption can leave assembled notes and fragment deletions staged, or a
completed local release commit. The cutter prints the starting SHA and exact
owned paths: `CHANGELOG.md`, chart/worker metadata, and incorporated fragment
paths. Inspect HEAD, index and worktree before choosing to finish preparation
or deliberately restore those paths. After a completed commit, deliberate undo
first soft-resets to the starting SHA, then restores the owned paths. Inspect
any provisional stable tag separately after interruption. Verification failure
can soften the release commit with edits retained staged; this is not automatic
destructive rollback.

## Consequences and scope

Independent files remove the shared authoring hotspot while retaining legacy
landing union/collapse compatibility. Authors still need accurate outcome prose
and shipping identity citations. No shared manifest, automatic destructive
rollback, or classifier-level content/incorporation proof is introduced here.
This ADR records the implemented fragment and release boundaries; it does not
claim that a live release cycle or publication has been exercised by M3's
documentation work.
