### Fixed

- **A lost forge-park reply no longer retries forever on a refused ownership check (#3856).**
  When a forge-park report's reply is lost and the follow-up ownership check or resend is permanently refused (any client error except not found, timeout or rate limiting), the worker now stops retrying instead of looping, reports nothing, keeps a resumable session and any publication record, and leaves custody unsettled.
