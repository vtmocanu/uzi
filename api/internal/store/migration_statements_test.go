package store_test

import (
	"reflect"
	"testing"
)

func TestMigrationStatementsGooseBlocks(t *testing.T) {
	const function = `CREATE FUNCTION example() RETURNS text LANGUAGE sql AS $$
 SELECT ';';
$$;`
	const rollback = `DO $$
BEGIN
 PERFORM 1;
END;
$$;`
	for _, block := range []string{function, rollback} {
		// SQL on either side still splits normally. A semicolon in prose must
		// not become SQL, and every internal block semicolon must stay intact.
		sql := "-- prose; not SQL\nSELECT 1; SELECT 2;\n-- +goose StatementBegin\n" + block +
			"\n-- +goose StatementEnd\nSELECT 3;"
		want := []string{"SELECT 1", "SELECT 2", block, "SELECT 3"}
		if got := splitMigrationStatements(t, sql); !reflect.DeepEqual(got, want) {
			t.Fatalf("migration replay statements = %#v, want %#v", got, want)
		}
	}
}
