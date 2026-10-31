### Fixed

- **Claude meters keep refreshing for plans without a 5-hour limit.**
  Anthropic removed the 5-hour window for some plans, so their usage probe carries only 7-day data; the poller rejected that as malformed, the meter went stale and the token dropped out of auto-selection. The 5-hour window is now optional: such tokens show a single 7-day meter and are ranked on 7-day headroom. Failed polls now log the secret id, call, kind and HTTP status.
