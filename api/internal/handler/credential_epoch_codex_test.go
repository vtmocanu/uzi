package handler

import (
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"testing"
)

func TestCredentialEpochCodexSnapshots(t *testing.T) {
	mode := pgtype.Text{String: "subscription", Valid: true}
	key := pgtype.Text{String: "[\"synthetic-user\",\"synthetic-account\"]", Valid: true}
	account := pgtype.Text{String: "synthetic-account", Valid: true}
	revision := pgtype.Int8{Int64: 3, Valid: true}
	epoch := pgtype.Int8{Int64: 8, Valid: true}
	got := credentialEpochsToDTO([]store.RunCredentialEpoch{
		{ClaimGeneration: 1},
		{ClaimGeneration: 2, CodexAuthMode: mode, CodexMaterialRevision: revision, CodexAccountKey: key, CodexAccountID: account, CodexClaimEpoch: epoch},
	}, []store.RunCredentialSwitchEpoch{
		{ClaimGeneration: 2, Ordinal: 1, CodexAuthMode: mode, CodexMaterialRevision: revision, CodexAccountKey: key, CodexAccountID: account, CodexClaimEpoch: pgtype.Int8{Int64: 9, Valid: true}},
	}, nil)
	if len(got) != 3 {
		t.Fatalf("epochs=%d", len(got))
	}
	historical := got[0]
	if historical.CodexAuthMode != nil || historical.CodexMaterialRevision != nil || historical.CodexAccountKey != nil || historical.CodexAccountID != nil || historical.CodexClaimEpoch != nil {
		t.Fatal("historical epoch invented Codex authority")
	}
	for i, wantEpoch := range []int64{8, 9} {
		row := got[i+1]
		if row.CodexAuthMode == nil || *row.CodexAuthMode != mode.String ||
			row.CodexMaterialRevision == nil || *row.CodexMaterialRevision != 3 ||
			row.CodexAccountKey == nil || *row.CodexAccountKey != key.String ||
			row.CodexAccountID == nil || *row.CodexAccountID != account.String ||
			row.CodexClaimEpoch == nil || *row.CodexClaimEpoch != wantEpoch {
			t.Fatalf("snapshot %d was lost or conflated", i)
		}
	}
}
