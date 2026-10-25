# Mid-turn recovery fixtures inherit foreign setgid groups and hide startup failures

The mid-turn recovery test fixtures create their scratch root with a raw temporary directory, so under a parent directory with a foreign setgid group the root inherits that group and the production guards refuse it. Separately, the cross-worker and same-worker tests wait for executor readiness in a way that hides a runner that fails to start.

Give the recovery tests a scratch root helper that shares the normalization of the shared fixture helper (process uid and primary gid, private mode, cleanup on failure that keeps the original error), and make the readiness wait fail by name when a runner finishes or rejects first. Add regression tests for both call sites and for the foreign-group case.
