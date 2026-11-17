package store

import (
	"strings"
	"testing"
)

// TestMigrateConnConfigStripsPoolParams pins that the migration connection never forwards
// pgxpool-only DSN settings (pool_max_conns etc.) to Postgres as runtime parameters: the
// server rejects an unknown setting, so a DATABASE_URL carrying pool_max_conns would fail boot
// in Migrate before OpenPool ran.
func TestMigrateConnConfigStripsPoolParams(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:p@db:5432/uzi?pool_max_conns=5&pool_min_conns=1&application_name=uzi",
		"host=db port=5432 user=u password=p dbname=uzi pool_max_conns=5 pool_min_conns=1 application_name=uzi",
	} {
		cc, err := migrateConnConfig(dsn)
		if err != nil {
			t.Fatalf("migrateConnConfig: %v", err)
		}
		for k := range cc.RuntimeParams {
			if strings.HasPrefix(k, "pool_") {
				t.Errorf("runtime param %q would be sent to Postgres; params: %v", k, cc.RuntimeParams)
			}
		}
		if got := cc.RuntimeParams["application_name"]; got != "uzi" {
			t.Errorf("application_name = %q, want uzi (ordinary runtime params must survive)", got)
		}
	}
}

// TestMigrateConnConfigRejectsInvalidPoolParam keeps boot failing on an invalid pool setting.
func TestMigrateConnConfigRejectsInvalidPoolParam(t *testing.T) {
	if _, err := migrateConnConfig("postgres://u:p@db:5432/uzi?pool_max_conns=zero"); err == nil {
		t.Fatal("migrateConnConfig accepted an invalid pool_max_conns")
	}
}
