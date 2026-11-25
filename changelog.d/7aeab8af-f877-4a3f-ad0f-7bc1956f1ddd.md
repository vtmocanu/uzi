### Changed

- **A non-zero guarded command result explains its exit code.**
  Claude runs under the memory guard now note that the code is the supervised shell's final status, so passing output from earlier commands can sit above it, and that a code above 128 does not by itself establish a signal kill.
