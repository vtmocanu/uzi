package workersvc

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestPendingCredentialSwitchSignal is the pure unit for the worker-facing held-state switch
// signal (PRD #1247 M5, D3/D4): it fires ONLY when a switch was requested, the claim is not yet
// released, and the stamp targets the run's CURRENT claim generation — and returns nil for every
// negative, so the signal is scoped to the exact claim the switch targeted and never leaks onto
// the next claim.
func TestPendingCredentialSwitchSignal(t *testing.T) {
	now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	gen := func(g int64) pgtype.Int8 { return pgtype.Int8{Int64: g, Valid: true} }

	cases := []struct {
		name    string
		run     store.Run
		wantSig *int64 // nil = no signal; else the expected Generation
	}{
		{
			name:    "pending: requested, not released, stamp == claim_generation",
			run:     store.Run{ClaimGeneration: 7, CredentialSwitchRequestedAt: now, CredentialSwitchGeneration: gen(7)},
			wantSig: ptrInt64(7),
		},
		{
			name:    "not pending: no switch requested",
			run:     store.Run{ClaimGeneration: 7, CredentialSwitchGeneration: gen(7)},
			wantSig: nil,
		},
		{
			name:    "not pending: claim already released",
			run:     store.Run{ClaimGeneration: 7, CredentialSwitchRequestedAt: now, CredentialSwitchGeneration: gen(7), ClaimReleasedAt: now},
			wantSig: nil,
		},
		{
			name:    "not pending: stamp generation below current claim (already reclaimed past it)",
			run:     store.Run{ClaimGeneration: 8, CredentialSwitchRequestedAt: now, CredentialSwitchGeneration: gen(7)},
			wantSig: nil,
		},
		{
			name:    "not pending: stamp generation above current claim",
			run:     store.Run{ClaimGeneration: 6, CredentialSwitchRequestedAt: now, CredentialSwitchGeneration: gen(7)},
			wantSig: nil,
		},
		{
			name:    "not pending: requested but stamp generation NULL",
			run:     store.Run{ClaimGeneration: 7, CredentialSwitchRequestedAt: now, CredentialSwitchGeneration: pgtype.Int8{}},
			wantSig: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig := PendingCredentialSwitchSignal(tc.run)
			if tc.wantSig == nil {
				if sig != nil {
					t.Fatalf("signal = %+v, want nil", sig)
				}
				return
			}
			if sig == nil {
				t.Fatalf("signal = nil, want generation %d", *tc.wantSig)
			}
			if sig.Generation != *tc.wantSig {
				t.Fatalf("signal.Generation = %d, want %d", sig.Generation, *tc.wantSig)
			}
		})
	}
}

func ptrInt64(v int64) *int64 { return &v }
