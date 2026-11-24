### Fixed

- **A Claude credential switch after a permanent message failure in the restore window now holds the run on the switch fence (#4652).**
  On a worker that supports in-place credential switches, a Claude run whose restore predecessor is still guarded now holds on the switch fence, instead of a plain retained stop, when an op-fenced credential switch is pending after a permanent message failure. Codex runs are unchanged.
