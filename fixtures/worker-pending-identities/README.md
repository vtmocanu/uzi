# Worker pending identity fixture

`duplicate-active.json` is the epoch-1 heartbeat/claim snapshot produced by
a real Outbox with terminal journals for run A at generations 1 and 2 and an
ActiveRunRegistry pending cap of 2. The M4 worker test compares the serialized
snapshot with this shared JSON. Both active entries retain the same run ID;
the complete pending identity list distinguishes the generations and contains
only run ID and claim generation, with no journal payload or clock.

The pending identity list is valid in isolation. The duplicate active run IDs
cause the API to reject the enclosing snapshot as a whole, so this snapshot
provides **no pending_all coverage**. API acceptance/rejection is verified
separately by API-owned tests; the worker test preserves the existing wire shape.

At cap 0, register and heartbeat snapshots have empty active arrays, overflow
true and the complete identity list. Retirement removes each identity; after
both journals retire, both builders emit an explicit empty list and overflow false.
