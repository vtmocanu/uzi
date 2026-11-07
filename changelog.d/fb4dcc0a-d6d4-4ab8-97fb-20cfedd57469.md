### Fixed

- **A lost forge-park reply no longer retries forever on a refused ownership check (#3856).**
  When a forge-park report's reply is lost and the follow-up ownership check is permanently refused (any client error except not found, timeout or rate limiting), or the resend is (any client error except timeout or rate limiting; a bad-request resend still fails the run as before), the worker now stops retrying instead of looping, reports nothing, keeps a resumable session and any publication record, and leaves custody unsettled.
