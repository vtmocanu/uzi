package store_test

import (
	"github.com/vtmocanu/uzi/api/internal/store"
	"os"
	"strings"
	"testing"
)

func TestTerminalHeldMigrationPreservesGuard(t *testing.T) {
	read := func(b []byte, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	old := read(os.ReadFile("migrations/00338_merged_predecessor_discard.sql"))
	start := strings.Index(old, "-- +goose StatementBegin\nCREATE OR REPLACE FUNCTION recovery_inventory_hold_guard()")
	old = old[start:]
	old = old[:strings.Index(old, "-- +goose StatementEnd")+len("-- +goose StatementEnd")]
	newer := read(os.ReadFile("migrations/00340_terminal_held_publication.sql"))
	parts := strings.Split(newer, "-- +goose Down\n")
	if len(parts) != 2 || strings.TrimSpace(parts[1]) != old {
		t.Fatal("Down does not restore preceding live Up guard")
	}
	// Compare all other branches literally, including the universal merged predecessor arm.
	strip := func(s string) string {
		a := strings.Index(s, "        -- Held publication")
		b := strings.Index(s[a:], "        IF NEW.final_disposition = 'archive'") + a
		return s[:a] + s[b:]
	}
	up := strings.TrimSpace(strings.TrimPrefix(parts[0], "-- +goose Up\n"))
	if strip(up) != strip(old) {
		t.Fatal("unrelated guard branches changed")
	}
}

func TestHeldPublicationTerminalGuardLiveDB(t *testing.T) {
	ctx, _, pool := heldMigratedPool(t, "held_terminal_")
	for _, status := range []string{"failed", "cancelled", "completed"} {
		for _, gen := range []int{1, 2} {
			f := heldNewFixture(ctx, t, pool, true, status, nil)
			mustExec(ctx, t, pool, "UPDATE runs SET claim_generation=$2,worker_id=NULL WHERE id=$1", f.run, gen)
			n, err := f.release(ctx, t, f.pub, heldDigest, heldTip)
			if err != nil || n != 1 {
				t.Fatalf("%s gen %d: rows=%d err=%v", status, gen, n, err)
			}
		}
	}
}

func TestHeldTerminalRollbackPreservesReceiptLiveDB(t *testing.T) {
	ctx, dsn, pool := heldMigratedPool(t, "held_terminal_down_")
	f := heldNewFixture(ctx, t, pool, true, "cancelled", nil)
	mustExec(ctx, t, pool, "UPDATE runs SET claim_generation=2,worker_id=NULL WHERE id=$1", f.run)
	if n, err := f.release(ctx, t, f.pub, heldDigest, heldTip); err != nil || n != 1 {
		t.Fatalf("release: rows=%d err=%v", n, err)
	}
	if err := store.MigrateDownTo(ctx, dsn, 339); err != nil {
		t.Fatal(err)
	}
	var state, evidence, tip, digest string
	var publication string
	if err := pool.QueryRow(ctx, "SELECT state,release_evidence,final_source_sha,final_coverage_digest,final_publication_id::text FROM recovery_custody_holds WHERE id=$1", f.hold).Scan(&state, &evidence, &tip, &digest, &publication); err != nil {
		t.Fatal(err)
	}
	if state != "released" || evidence != "held_publication" || tip != heldTip || digest != heldDigest || publication != f.pub.String() {
		t.Fatalf("rollback rewrote receipt: %s %s %s %s %s", state, evidence, tip, digest, publication)
	}
	g := heldNewFixture(ctx, t, pool, true, "cancelled", nil)
	if n, err := g.release(ctx, t, g.pub, heldDigest, heldTip); err != nil || n != 0 {
		t.Fatalf("old guard not restored: rows=%d err=%v", n, err)
	}
}
