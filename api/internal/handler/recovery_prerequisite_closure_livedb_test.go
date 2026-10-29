package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func newPrerequisiteClosureEnv(t *testing.T) *recoveryEnv {
	t.Helper()
	// Register the drop before newRecoveryEnvGuarded registers pool cleanup.
	ephemeralPriorityDatabase(t)
	return newRecoveryEnvGuarded(t, true)
}

func assertPrerequisiteFixtureIsolation(t *testing.T, fixture func(*testing.T) *pgxpool.Pool) {
	t.Helper()
	originalDSN := os.Getenv("UZI_TEST_DATABASE_URL")
	if originalDSN == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	shared, err := pgx.Connect(ctx, originalDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := shared.Close(ctx); err != nil {
			t.Errorf("close shared database connection: %v", err)
		}
	}()
	var originalDatabase string
	if err := shared.QueryRow(ctx, "SELECT current_database()").Scan(&originalDatabase); err != nil {
		t.Fatal(err)
	}
	t.Run("child-fixture", func(t *testing.T) {
		pool := fixture(t)
		var childDatabase string
		if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&childDatabase); err != nil {
			t.Fatal(err)
		}
		if childDatabase == originalDatabase {
			t.Fatalf("prerequisite fixture reused shared database %q", originalDatabase)
		}
	})
	if got := os.Getenv("UZI_TEST_DATABASE_URL"); got != originalDSN {
		t.Fatal("prerequisite fixture did not restore the parent database environment")
	}
}

func TestRecoveryPrerequisiteClosureFixtureIsolationLiveDB(t *testing.T) {
	assertPrerequisiteFixtureIsolation(t, func(t *testing.T) *pgxpool.Pool {
		return newPrerequisiteClosureEnv(t).pool
	})
}

func TestRecoveryPrerequisiteClosureInitializationLiveDB(t *testing.T) {
	for _, mode := range []string{"lazy", "eager"} {
		t.Run(mode, func(t *testing.T) {
			e := newPrerequisiteClosureEnv(t)
			for _, q := range []string{
				"UPDATE forge_connections SET base_url='https://github.com' WHERE user_id=$1",
				"UPDATE runs SET claim_generation=1 WHERE user_id=$1",
			} {
				if _, err := e.pool.Exec(e.ctx, q, e.user); err != nil {
					t.Fatal(err)
				}
			}
			h := e.handler()
			h.cfg.PrerequisiteClosureForges = []string{"github"}
			e.worker.ProtocolCapabilities = []string{capability.RecoveryInventoryV1, capability.RecoveryPrerequisiteClosureV1}
			if mode == "eager" {
				h.RecoveryService()
			}
			svc := h.recovery()
			if svc != h.RecoveryService() {
				t.Fatal("initialization replaced shared recovery service")
			}
			generation := int64(1)
			reserved, err := svc.Reserve(e.ctx, e.worker, e.run, apitypes.RecoveryReserveRequest{IdempotencyKey: mode, SourceSha: strings.Repeat("a", 40), Generation: &generation})
			if err != nil {
				t.Fatal(err)
			}
			id := uuid.MustParse(reserved.CaptureID)
			body := []byte("handler thin archive")
			sum := sha256.Sum256(body)
			manifest := apitypes.RecoveryUploadManifest{ByteSize: int64(len(body)), Checksum: hex.EncodeToString(sum[:]), PrerequisiteShas: []string{strings.Repeat("b", 40)}}
			ack, err := svc.Upload(e.ctx, e.worker, e.run, id, manifest, bytes.NewReader(body))
			if err != nil || ack.PrerequisiteClosure == nil || ack.PrerequisiteClosure.State != "pending" {
				t.Fatalf("%+v %v", ack, err)
			}
			row, err := h.q.GetPrerequisiteClosure(e.ctx, store.GetPrerequisiteClosureParams{UserID: e.user, CaptureID: id})
			if err != nil || row.LiveRunID.Valid {
				t.Fatalf("S0 %+v %v", row, err)
			}
		})
	}
}
