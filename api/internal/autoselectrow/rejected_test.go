package autoselectrow

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"testing"
)

func TestRejectedProjectionAcrossThreeRows(t *testing.T) {
	id := uuid.New()
	if !FromRateLimitRow(store.ListRateLimitsForUserRow{UserSecretID: id, Rejected: true}).Rejected {
		t.Fatal("owner projection lost rejection")
	}
	if !FromAdminRateLimitRow(store.ListRateLimitsRow{UserSecretID: pgtype.UUID{Bytes: id, Valid: true}, Rejected: true}).Rejected {
		t.Fatal("admin projection lost rejection")
	}
	if !FromCandidateRow(store.ListAutoSelectCandidatesRow{UserSecretID: id, Rejected: true}).Rejected {
		t.Fatal("ranker projection lost rejection")
	}
}
