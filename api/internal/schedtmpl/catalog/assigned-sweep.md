---
slug: assigned-sweep
name: Assigned-to-uzi sweep
description: Daily sweep over open issues assigned to the uzi-bot account, starting a run for the oldest few.
target: sweep
cron: 0 2 * * *
timezone: UTC
selector: assigned
max_issues: 3
---

Implement the assigned issue. It was assigned to the uzi-bot account, which is
how a teammate says "this is yours to do" — so treat the issue description (and
any linked spec) as the specification, deliver the change end to end with tests,
and run the project's gate before finishing. Keep the work scoped to what the
issue asks for and stop to report if it turns out to depend on something not yet
in place.
