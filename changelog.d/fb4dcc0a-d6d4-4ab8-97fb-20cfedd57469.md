### Fixed

- **A lost forge-park reply no longer retries forever on a refused ownership check (#3856).**
  When a forge-park report's reply is lost and the follow-up ownership check is permanently refused (any client error except not found, timeout or rate limiting), the worker now stops retrying, keeps the possibly parked session, source, records and custody, and reports nothing instead of looping.
