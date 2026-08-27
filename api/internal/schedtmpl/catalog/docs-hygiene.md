---
slug: docs-hygiene
name: Docs hygiene
description: Weekly sweep for mechanical documentation defects — dead links, stale references, drift.
target: prompt
cron: 0 3 * * 1
timezone: UTC
---

Audit the project's documentation for mechanical defects: broken or moved links,
references to files or commands that no longer exist, stale frontmatter, and
obvious typos. Focus on correctness, not rewriting for style. Verify each problem
against the actual repository before fixing it — follow the link, check that the
referenced path exists. When a broken link has more than one plausible correct
target, describe it in your report rather than guessing the repoint.

Apply the mechanical corrections, commit them, and open a merge request.
Guardrail: fix ONLY broken or moved links, stale references, frontmatter, and
obvious typos, and restrict edits to documentation files — do NOT rewrite prose
for style, do NOT modify source, build, CI, or agent-configuration files, and
keep the diff to mechanical corrections.

If there is nothing to fix, make no change and open no merge request: leave a
short note saying the docs are clean.
