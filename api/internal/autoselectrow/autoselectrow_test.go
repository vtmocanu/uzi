package autoselectrow

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// These are pure-unit tests of the ONE row→Candidate conversion (PRD #111 D21).
// The conversion is otherwise reached only through *LiveDB integration tests
// (store/auto_select_candidates_integration_test.go), which need a database; the
// mapping logic itself is DB-free, so it is pinned here directly. Every assertion
// checks the returned autoselect.Candidate — the value a caller (handler render /
// workersvc ranker) actually sees.

// FromRateLimitRow lifts a fully-populated row: every present column reaches the
// Candidate, the nullable columns become non-nil pointers to their values, and
// InFlight stays the deliberate zero (only the ranking query sets it).
func TestFromRateLimitRow_FullyPopulated(t *testing.T) {
	sid := uuid.New()
	synced := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	fiveReset := time.Date(2026, 8, 1, 15, 0, 0, 0, time.UTC)
	sevenReset := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)

	got := FromRateLimitRow(store.ListRateLimitsForUserRow{
		UserSecretID:     sid,
		Label:            "prod",
		AutoEligible:     true,
		FiveHourPct:      pgtype.Int2{Int16: 40, Valid: true},
		FiveHourResetsAt: pgtype.Timestamptz{Time: fiveReset, Valid: true},
		SevenDayPct:      pgtype.Int2{Int16: 55, Valid: true},
		SevenDayResetsAt: pgtype.Timestamptz{Time: sevenReset, Valid: true},
		SyncedAt:         pgtype.Timestamptz{Time: synced, Valid: true},
	})

	if got.SecretID != sid {
		t.Errorf("SecretID = %v, want %v", got.SecretID, sid)
	}
	if got.Label != "prod" {
		t.Errorf("Label = %q, want %q", got.Label, "prod")
	}
	if !got.AutoEligible {
		t.Error("AutoEligible = false, want true")
	}
	if !got.HasReading {
		t.Error("HasReading = false, want true (SyncedAt is valid)")
	}
	if got.FiveHourPct == nil || *got.FiveHourPct != 40 {
		t.Errorf("FiveHourPct = %v, want *40", got.FiveHourPct)
	}
	if got.SevenDayPct == nil || *got.SevenDayPct != 55 {
		t.Errorf("SevenDayPct = %v, want *55", got.SevenDayPct)
	}
	if got.FiveResetsAt == nil || !got.FiveResetsAt.Equal(fiveReset) {
		t.Errorf("FiveResetsAt = %v, want *%v", got.FiveResetsAt, fiveReset)
	}
	if got.SevenResetsAt == nil || !got.SevenResetsAt.Equal(sevenReset) {
		t.Errorf("SevenResetsAt = %v, want *%v", got.SevenResetsAt, sevenReset)
	}
	if got.SyncedAt == nil || !got.SyncedAt.Equal(synced) {
		t.Errorf("SyncedAt = %v, want *%v", got.SyncedAt, synced)
	}
	if got.InFlight != 0 {
		t.Errorf("InFlight = %d, want 0 (FromRateLimitRow never counts in-flight)", got.InFlight)
	}
}

// A row with no reading at all (the LEFT JOIN missed): HasReading is false and every
// nullable field is a nil pointer, while the NOT-NULL scalars still carry through.
func TestFromRateLimitRow_AllAbsent(t *testing.T) {
	sid := uuid.New()
	got := FromRateLimitRow(store.ListRateLimitsForUserRow{
		UserSecretID:     sid,
		Label:            "new-token",
		AutoEligible:     false,
		FiveHourPct:      pgtype.Int2{Valid: false},
		FiveHourResetsAt: pgtype.Timestamptz{Valid: false},
		SevenDayPct:      pgtype.Int2{Valid: false},
		SevenDayResetsAt: pgtype.Timestamptz{Valid: false},
		SyncedAt:         pgtype.Timestamptz{Valid: false},
	})

	if got.SecretID != sid {
		t.Errorf("SecretID = %v, want %v", got.SecretID, sid)
	}
	if got.Label != "new-token" {
		t.Errorf("Label = %q, want %q", got.Label, "new-token")
	}
	if got.AutoEligible {
		t.Error("AutoEligible = true, want false")
	}
	if got.HasReading {
		t.Error("HasReading = true, want false (SyncedAt is invalid)")
	}
	if got.SyncedAt != nil {
		t.Errorf("SyncedAt = %v, want nil", got.SyncedAt)
	}
	if got.FiveHourPct != nil {
		t.Errorf("FiveHourPct = %v, want nil", got.FiveHourPct)
	}
	if got.SevenDayPct != nil {
		t.Errorf("SevenDayPct = %v, want nil", got.SevenDayPct)
	}
	if got.FiveResetsAt != nil {
		t.Errorf("FiveResetsAt = %v, want nil", got.FiveResetsAt)
	}
	if got.SevenResetsAt != nil {
		t.Errorf("SevenResetsAt = %v, want nil", got.SevenResetsAt)
	}
}

// The D16 crux: a real 0% reading (Valid=true, Int16=0) must survive as a non-nil
// pointer to 0, NOT collapse to the nil that "no reading" produces. A conversion
// that keyed on the value instead of .Valid would erase the distinction.
func TestFromRateLimitRow_PresentZeroIsNotAbsent(t *testing.T) {
	got := FromRateLimitRow(store.ListRateLimitsForUserRow{
		UserSecretID: uuid.New(),
		Label:        "exhausted",
		AutoEligible: true,
		FiveHourPct:  pgtype.Int2{Int16: 0, Valid: true},
		SevenDayPct:  pgtype.Int2{Int16: 0, Valid: true},
		SyncedAt:     pgtype.Timestamptz{Time: time.Unix(1, 0), Valid: true},
	})

	if got.FiveHourPct == nil {
		t.Fatal("FiveHourPct = nil, want a non-nil pointer to 0 (present zero, not absent)")
	}
	if *got.FiveHourPct != 0 {
		t.Errorf("*FiveHourPct = %d, want 0", *got.FiveHourPct)
	}
	if got.SevenDayPct == nil {
		t.Fatal("SevenDayPct = nil, want a non-nil pointer to 0 (present zero, not absent)")
	}
	if *got.SevenDayPct != 0 {
		t.Errorf("*SevenDayPct = %d, want 0", *got.SevenDayPct)
	}
}

// FromCandidateRow is the ranking-query twin: it carries in_flight_runs into InFlight
// and delegates every other field. The pairing with FromRateLimitRow's InFlight==0
// pins that InFlight is populated ONLY on this path.
func TestFromCandidateRow_CarriesInFlightAndDelegates(t *testing.T) {
	sid := uuid.New()
	synced := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

	gotCand := FromCandidateRow(store.ListAutoSelectCandidatesRow{
		UserSecretID: sid,
		Label:        "ranked",
		AutoEligible: true,
		FiveHourPct:  pgtype.Int2{Int16: 12, Valid: true},
		SevenDayPct:  pgtype.Int2{Int16: 34, Valid: true},
		SyncedAt:     pgtype.Timestamptz{Time: synced, Valid: true},
		InFlightRuns: 3,
	})
	if gotCand.InFlight != 3 {
		t.Errorf("InFlight = %d, want 3", gotCand.InFlight)
	}

	// Same underlying values through the settings-list converter: identical Candidate
	// except InFlight, which stays 0 there.
	gotRate := FromRateLimitRow(store.ListRateLimitsForUserRow{
		UserSecretID: sid,
		Label:        "ranked",
		AutoEligible: true,
		FiveHourPct:  pgtype.Int2{Int16: 12, Valid: true},
		SevenDayPct:  pgtype.Int2{Int16: 34, Valid: true},
		SyncedAt:     pgtype.Timestamptz{Time: synced, Valid: true},
	})
	if gotRate.InFlight != 0 {
		t.Errorf("FromRateLimitRow InFlight = %d, want 0", gotRate.InFlight)
	}

	// Delegated fields must match between the two converters.
	if gotCand.SecretID != gotRate.SecretID {
		t.Errorf("SecretID differs: cand=%v rate=%v", gotCand.SecretID, gotRate.SecretID)
	}
	if gotCand.Label != gotRate.Label || gotCand.Label != "ranked" {
		t.Errorf("Label differs: cand=%q rate=%q", gotCand.Label, gotRate.Label)
	}
	if gotCand.AutoEligible != gotRate.AutoEligible || !gotCand.AutoEligible {
		t.Errorf("AutoEligible differs: cand=%v rate=%v", gotCand.AutoEligible, gotRate.AutoEligible)
	}
	if gotCand.HasReading != gotRate.HasReading || !gotCand.HasReading {
		t.Errorf("HasReading differs: cand=%v rate=%v", gotCand.HasReading, gotRate.HasReading)
	}
	if gotCand.FiveHourPct == nil || gotRate.FiveHourPct == nil || *gotCand.FiveHourPct != *gotRate.FiveHourPct || *gotCand.FiveHourPct != 12 {
		t.Errorf("FiveHourPct differs: cand=%v rate=%v", gotCand.FiveHourPct, gotRate.FiveHourPct)
	}
	if gotCand.SevenDayPct == nil || gotRate.SevenDayPct == nil || *gotCand.SevenDayPct != *gotRate.SevenDayPct || *gotCand.SevenDayPct != 34 {
		t.Errorf("SevenDayPct differs: cand=%v rate=%v", gotCand.SevenDayPct, gotRate.SevenDayPct)
	}
	if gotCand.SyncedAt == nil || gotRate.SyncedAt == nil || !gotCand.SyncedAt.Equal(*gotRate.SyncedAt) {
		t.Errorf("SyncedAt differs: cand=%v rate=%v", gotCand.SyncedAt, gotRate.SyncedAt)
	}
}

// FromAdminRateLimitRow unwraps the nullable-wrapped admin columns (the admin query
// LEFT JOINs users→user_secrets) into the flat Candidate fields.
func TestFromAdminRateLimitRow_UnwrapsNullables(t *testing.T) {
	sid := uuid.New()
	synced := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)

	got := FromAdminRateLimitRow(store.ListRateLimitsRow{
		UserSecretID: pgtype.UUID{Bytes: [16]byte(sid), Valid: true},
		Label:        pgtype.Text{String: "admin-token", Valid: true},
		AutoEligible: pgtype.Bool{Bool: true, Valid: true},
		FiveHourPct:  pgtype.Int2{Int16: 72, Valid: true},
		SevenDayPct:  pgtype.Int2{Valid: false},
		SyncedAt:     pgtype.Timestamptz{Time: synced, Valid: true},
	})

	if got.SecretID != sid {
		t.Errorf("SecretID = %v, want %v (unwrapped from pgtype.UUID.Bytes)", got.SecretID, sid)
	}
	if got.Label != "admin-token" {
		t.Errorf("Label = %q, want %q", got.Label, "admin-token")
	}
	if !got.AutoEligible {
		t.Error("AutoEligible = false, want true (unwrapped from pgtype.Bool.Bool)")
	}
	if !got.HasReading {
		t.Error("HasReading = false, want true")
	}
	if got.FiveHourPct == nil || *got.FiveHourPct != 72 {
		t.Errorf("FiveHourPct = %v, want *72", got.FiveHourPct)
	}
	if got.SevenDayPct != nil {
		t.Errorf("SevenDayPct = %v, want nil (invalid column)", got.SevenDayPct)
	}
}
