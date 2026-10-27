package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodexAdmissionBoardProjection(t *testing.T) {
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                            string
		mine                            bool
		queue, class, prior, wantReason string
		wantFields                      bool
	}{
		{"owner rate", true, "codex_rate_limit", "rate_limit", "", "waiting: provider reported a rate limit; account admission eligible after 2026-10-27T12:00:00Z", true},
		{"owner overload", true, "codex_overload", "overload", "", "waiting: provider reported overload; account admission eligible after 2026-10-27T12:00:00Z", true},
		{"former maintenance", true, "codex_overload", "overload", "waiting: uzi is in maintenance mode", "waiting: provider reported overload; account admission eligible after 2026-10-27T12:00:00Z", true},
		{"owner maintenance", true, "maintenance", "rate_limit", "waiting: uzi is in maintenance mode", "waiting: uzi is in maintenance mode", true},
		{"owner unavailable", true, "codex_rate_limit", "rate_limit", "your Codex account is unavailable (quarantined or being re-linked), so this run can't start", "your Codex account is unavailable (quarantined or being re-linked), so this run can't start", true},
		{"owner handoff", true, "codex_overload", "overload", "handoff setup has not finished", "handoff setup has not finished", true},
		{"owner vault", true, "codex_overload", "overload", "your vault is locked, so this run can't start", "your vault is locked, so this run can't start", true},
		{"owner custody", true, "codex_overload", "overload", "you have too much unpublished work awaiting recovery; resolve or discard some recovery archives to start new runs", "you have too much unpublished work awaiting recovery; resolve or discard some recovery archives to start new runs", true},
		{"owner custody unavailable", true, "codex_overload", "overload", "new runs are paused: the custody hold limit setting could not be read; an admin should check Admin → Instance settings", "new runs are paused: the custody hold limit setting could not be read; an admin should check Admin → Instance settings", true},
		{"shared cooldown", false, "codex_rate_limit", "rate_limit", "private", "", false},
		{"shared maintenance", false, "maintenance", "overload", "private", "", false},
		{"expired", true, "", "", "waiting: provider reported overload; account admission eligible after 2026-10-26T12:00:00Z", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dto := &latestRunDTO{IsMine: tc.mine, HealthReason: queueReasonPtr(tc.prior)}
			dto.overlayQueueAdmission(tc.queue, pgtype.Text{String: tc.class, Valid: tc.class != ""}, pgtype.Timestamptz{Time: at, Valid: tc.class != ""})
			if (dto.CodexAdmissionClass != nil) != tc.wantFields || (dto.CodexAdmissionEligibleAt != nil) != tc.wantFields {
				t.Fatal("incorrect admission visibility")
			}
			if tc.wantFields && (*dto.CodexAdmissionClass != tc.class || !dto.CodexAdmissionEligibleAt.Equal(at)) {
				t.Fatal("incorrect admission values")
			}
			got := ""
			if dto.HealthReason != nil {
				got = *dto.HealthReason
			}
			if got != tc.wantReason {
				t.Fatalf("health reason = %q, want %q", got, tc.wantReason)
			}
			wantQueue := tc.queue
			if !tc.mine && wantQueue != "maintenance" {
				wantQueue = ""
			}
			got = ""
			if dto.QueueReason != nil {
				got = *dto.QueueReason
			}
			if got != wantQueue {
				t.Fatalf("queue reason = %q, want %q", got, wantQueue)
			}
			if !tc.mine {
				data, err := json.Marshal(dto)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data), "codex_admission_") || strings.Contains(string(data), "private") || strings.Contains(string(data), "codex_rate_limit") {
					t.Fatalf("private details leaked: %s", data)
				}
			}
		})
	}
}

func TestCodexAdmissionMappersStripPersistedReason(t *testing.T) {
	owner := uuid.New()
	stale := pgtype.Text{String: "waiting: provider reported a rate limit; account admission eligible after 2026-10-26T12:00:00Z", Valid: true}
	dto := runToDTO(store.Run{ID: uuid.New(), UserID: owner, Status: "queued", HealthReason: stale}, "", 0, 0, 0, time.Now(), 0)
	if dto.HealthReason != nil {
		t.Fatal("run mapper retained expired admission reason")
	}
	latest := mapLatestRun(uuid.New(), owner, "queued", "issue", 0, false, pgtype.Int8{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{},
		"waiting_worker", stale, pgtype.Timestamptz{}, pgtype.Text{}, pgtype.Timestamptz{}, pgtype.Timestamptz{}, pgtype.Timestamptz{}, pgtype.Int4{}, 0, 0, 0, false,
		pgtype.Text{}, pgtype.Text{}, 1, pgtype.Timestamptz{}, pgtype.Timestamptz{}, owner, 0, pgtype.Text{})
	if latest.HealthReason != nil {
		t.Fatal("board mapper retained expired admission reason")
	}
	current := overlayAdmissionReason(dto.HealthReason, "codex_overload", ptrAdmissionClass("overload"), ptrAdmissionTime(time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)))
	if current == nil || *current != "waiting: provider reported overload; account admission eligible after 2026-10-27T12:00:00Z" {
		t.Fatal("current projection did not replace stale class")
	}
	if !apitypes.IsCodexAdmissionReason(*current) {
		t.Fatal("current reason not detected")
	}
}

func ptrAdmissionClass(s string) *string      { return &s }
func ptrAdmissionTime(t time.Time) *time.Time { return &t }
