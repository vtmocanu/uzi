-- Initialization only, for owned throwaway harness databases. Never apply to a retained DB.
CREATE SCHEMA uzi_test_harness;
CREATE TABLE uzi_test_harness.database_marker (marker text PRIMARY KEY);
INSERT INTO uzi_test_harness.database_marker (marker) VALUES ('recovery-reset-v1');
