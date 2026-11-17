# ADR-4435: Bounded exceptions for development-only npm advisories

**Status:** Proposed, requires explicit maintainer approval before merge.
**Date:** 2026-11-17
**Issue:** [#4435](https://github.com/vtmocanu/uzi/issues/4435)
**Amends:** [PRD #103](../prds/done/103-dev-loop-quality-gates.md), ruling 1.

## Context

The full dependency audit remains mandatory at high severity. As of this date,
[GHSA-fc5q-39xc-vjpf](https://github.com/advisories/GHSA-fc5q-39xc-vjpf)
marks every published picomatch version through 4.0.7 vulnerable, without a patched
release. Both audited lockfiles contain development-only picomatch, and their high
findings all reach this advisory. Major downgrades proposed by npm are not adopted.

These lockfile occurrences are omitted from the worker production install; the
web runtime serves built assets. This is not a claim about every package inside
the worker image or its bundled native CLI, nor about dependencies of run repos.
Attacker-controlled glob patterns still present a build, test and CI availability
risk. Proposed changes can execute in CI before review.

## Decision

The maintainer may approve a temporary row in scripts/npm-audit-exceptions.tsv.
The row binds the component, GHSA, package, exact advisory range, maximum accepted
severity, inclusive UTC recheck date and tracking issue. Dates more than 30 days
ahead are invalid. The file and production clock have no environment or argument
override. The initial proposed rows cover only picomatch in web and agent, at
high severity, through 2026-12-01, tracked by #4435.

A pass requires every gated root to match a row. Both root and affected-record
severity are capped. Reported lock nodes are combined with independently found
package occurrences, including real-name aliases; every occurrence must be
development-only. Missing nodes, unsupported schemas, unresolved roots and malformed
policy are instrument failures, never proof of safety. Rooted cycles are supported.
A real unexcepted finding retains findings exit 1 even if other evidence is broken.

Expired and unused rows fail, including on clean audits. Advisory disappearance
or a downgrade below the threshold requires removal or review of the row. A changed
advisory range, production occurrence or severity escalation re-arms the gate.

With rows present, the package audit command must exactly match the checked
threshold and full-tree flags. A second, retried JSON audit supplies authoritative
exception evidence after the ordinary text audit reaches a verdict. Two registry
queries can observe different snapshots; this accepted race can yield a spurious
red or a green reflecting the later snapshot. The full text report remains visible,
and success prints EXCEPTED with the policy identity, never clean. Components
without rows retain the prior audit behavior and avoid the extra query.

Agents and uzi runs may prepare exact proposed rows for review, but cannot authorize
exceptions or extensions. Only explicit maintainer approval makes a row policy
through merge. Never add or extend a row merely to clear CI.

## Consequences and removal

The level stays high and dev dependencies stay included. This narrow exception
amends the original high-and-above-at-zero ruling; it does not remove the audit or
suppress another advisory. The pure evaluator and hermetic shell tests run under
test:npm-audit-gate in gate:repo.

After 2026-12-01 UTC, expiry makes the checks red again. Earlier validation failures
can hide later checks in the same validate job; all those checks must execute on
the landing head and main. Recheck by the deadline, or sooner when a fix ships.
Then upgrade both lockfiles, remove the rows and close #4435. A range change before
a usable release requires a visible new decision, not an automatic extension.

Alternative version pins cannot avoid the currently affected range. Omitting dev
dependencies or raising the threshold would suppress unrelated findings. Fork,
git and file overrides add unreviewed code and can remove dependency coverage from
the registry audit. Adding a third-party allowlist tool would still require the
lockfile proof and would add a dependency to the gate itself.
