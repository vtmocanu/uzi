### Fixed

- **A credential switch after a permanent message failure in the restore window now holds the run on the switch fence (#4652).**
  A permanent message failure in the independent-restore window followed by an op-fenced credential switch now holds the run on the switch fence instead of a plain retained stop.
