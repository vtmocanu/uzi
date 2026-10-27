### Fixed

- **Invalidated ordinary recovery inventories can settle after reconstruction (#3561).**
  Recovery creates a fresh archive only when current source ancestry is proven, retains custody when it is not, and prevents stale invalidated archives from minting a new FINAL.
