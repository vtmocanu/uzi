-- +goose Up

-- Run provisionable-toolchain set (PRD #84 M4). The toolchain FAMILIES the plan-time
-- inference judged the run needs (go/node/python/rust/jvm) — the PROVISIONABLE half of
-- the requirement model, deferred out of 00142 (see its line 7 comment) until the plan
-- report that emits them landed (M4 4a). Display-only in v1: it never gates a claim, so
-- unlike required_capabilities it has no fn_worker_can_claim clause. NOT NULL DEFAULT
-- '{}' so the empty set means "nothing inferred" — existing rows need NO backfill — and
-- the write path never has to distinguish NULL from empty. Every value is FilterTools-ed
-- against the server-owned tool vocabulary at the write path so a garbled worker report
-- cannot smuggle an arbitrary string into the UI.
ALTER TABLE runs ADD COLUMN required_tools text[] NOT NULL DEFAULT '{}';

-- Run size class (PRD #84 M4 unit 4b): the plan-time inferred coarse size bucket
-- (s/m/l) the worker emits on its awaiting_approval report. Display-only in v1 like
-- required_tools — it never gates a claim. NOT NULL DEFAULT '' so the empty string
-- means "nothing inferred" and existing rows need no backfill; the service clamps to
-- the {s,m,l} vocabulary at the write path (an off-vocabulary report becomes a no-op).
ALTER TABLE runs ADD COLUMN size_class text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE runs DROP COLUMN size_class;
ALTER TABLE runs DROP COLUMN required_tools;
