### Fixed

- **An interactive run that times out or is stopped while waiting for a follow-up now finishes and publishes its work (#4479).**
  The worker moves the parked run back to running before it finalizes, so the branch and merge request are published instead of the run waiting forever; a follow-up sent while the run is finishing is refused with a clear message instead of being silently lost.
