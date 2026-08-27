---
slug: planned-sweep
name: Planned-work sweep
description: Daily sweep over open issues labelled "Planned", starting a run for the oldest few.
target: sweep
cron: 0 2 * * *
timezone: UTC
labels: Planned
max_issues: 3
---

Implement the sweep's planned-work issue. Treat the issue description (and any
linked spec) as the specification, deliver the change end to end with tests, and
run the project's gate before finishing. Keep the work scoped to what the issue
asks for and stop to report if it turns out to depend on something not yet in
place.
