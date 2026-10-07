---
slug: ondeck-sweep
name: On-deck sweep
description: Drain a triaged on-deck backlog a little at a time when there is room, then remove the label.
target: sweep
cron: */10 * * * *
timezone: UTC
labels: on-deck
max_issues: 1
capacity_limit: 4
capacity_room_needed: 2
remove_label_on_dispatch: true
---

Work the next on-deck issue. Keep the change focused and verify it before opening a merge request.
