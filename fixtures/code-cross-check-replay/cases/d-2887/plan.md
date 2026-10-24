Synthetic plan (reconstructed for the replay corpus; not the historically approved plan)

1. Add a makeScratchRoot helper next to makeFixture that reuses its root normalization.
2. Switch the mid-turn recovery test scratch directory to the helper.
3. Add an awaitReady wait that names the runner that finished or rejected first, bounded by the existing helper.
4. Use it in the cross-worker and same-worker missing-source tests.
5. Add a child-process permission matrix for plain, own-group setgid and foreign-group setgid parents at two umasks.
6. Add a normalization-failure child that checks cleanup and the ledger.
7. Mark the change as not needing a changelog entry.
