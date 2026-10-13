# ADR-2921: Server-settled outcomes and retained terminal evidence

Issue: [#2921](https://github.com/vtmocanu/uzi/issues/2921).

A terminal journal has two independent obligations: settle the server outcome and preserve evidence until custody permits report retirement. A positive terminal state acknowledgement, or a refused report whose authenticated response names a terminal server status, resolves the first obligation. It does not resolve the second.

The outbox records this observation only in its existing in-memory journal identity binding, scoped to the exact run, generation, authenticated payload and file lifetime sent. It re-authenticates the selected journal before marking it. A stale claim has precedence and cannot mark another generation. Unknown or nonterminal replies do not mark an outcome. Replacements or failed authentication invalidate the observation; retirement drops the binding.

Marking is best-effort at the terminal acknowledgement call sites. A rejected observation is logged at debug with run and generation only, then the existing retirement attempt still runs. Advisory admission bookkeeping must never suppress or replace custody/report retirement.

Active snapshots and pending-overflow admission exclude marked terminal outcomes. Actual live executions remain independently tracked and retain their execution semaphore until their execute promise settles. `hasPendingTerminal`, physical journals, custody holds, source retention and every report-retirement predicate remain unchanged. The observation is never deletion or publication authority.

There is no disk flag or journal format change. After restart, loaded journals conservatively advertise unresolved outcomes until replay obtains a fresh terminal acknowledgement. One successful refused-terminal replay restores the observation even when custody still prevents unlink. Temporary over-advertising at startup is conservative; no prior process's observation is trusted.

The report-retirement return contract and terminal/finalize callers remain unchanged, including the separate archive and FINAL-ACK authorities in ADR-2652 and #2883.
