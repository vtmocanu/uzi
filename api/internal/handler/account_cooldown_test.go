package handler

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestAccountCooldownBoardReaders(t *testing.T) {
	owner := uuid.New()
	at := time.Now().Add(time.Hour)
	for _, mine := range []bool{true, false} {
		for _, class := range []string{"rate_limit", "overload"} {
			row := store.ListLatestRunsForRepoRow{ID: uuid.New(), UserID: owner, Status: "running", Kind: "issue", IssueIid: pgtype.Int8{Int64: 1, Valid: true}, Health: "waiting_account_cooldown", HealthReason: pgconv.TextOrNull(apitypes.AccountCooldownReason), AccountCooldownClass: pgconv.TextOrNull(class), AccountCooldownEligibleAt: pgconv.Time(at)}
			viewer := owner
			if !mine {
				viewer = uuid.New()
			}
			cards := assembleCards([]store.Issue{{ForgeIssueIid: 1}}, []store.ListLatestRunsForRepoRow{row}, nil, nil, nil, viewer, "github", nil, 0)
			dto := cards[0].LatestRun
			if mine {
				if dto.Health != "waiting_account_cooldown" || dto.AccountCooldownClass == nil || string(*dto.AccountCooldownClass) != class || !dto.AccountCooldownEligibleAt.Equal(at) {
					t.Fatalf("owner=%+v", dto)
				}
			} else {
				data, err := json.Marshal(dto)
				if err != nil {
					t.Fatal(err)
				}
				if dto.Health != "ok" || strings.Contains(string(data), "account_cooldown") || strings.Contains(string(data), "waiting for account") {
					t.Fatalf("foreign=%s", data)
				}
			}
		}
	}
}

func TestAccountCooldownReadViewsClearStaleProgress(t *testing.T) {
	for _, view := range []string{"detail", "list", "summary"} {
		t.Run(view, func(t *testing.T) {
			dto := runToDTO(store.Run{ID: uuid.New(), Kind: "issue", Status: "running", Health: "waiting_account_cooldown", HealthReason: pgconv.TextOrNull(apitypes.AccountCooldownReason)}, "", 0, 0, 0, time.Now(), 0)
			h := &Handler{}
			req := httptest.NewRequest("GET", "/api/runs?view="+view, nil)
			item := apitypes.RunListItemDTO{RunDTO: dto}
			h.overlayListDecorations(req, []apitypes.RunListItemDTO{item})
			rec := httptest.NewRecorder()
			if view == "detail" {
				data, err := json.Marshal(dto)
				if err != nil {
					t.Fatal(err)
				}
				rec.Body.Write(data)
			} else {
				writeRunList(rec, req, []apitypes.RunListItemDTO{item})
			}
			body := rec.Body.String()
			for _, stale := range []string{"waiting_account_cooldown", "waiting for account", `"state":"waiting"`} {
				if strings.Contains(body, stale) {
					t.Fatalf("%s retains %s: %s", view, stale, body)
				}
			}
		})
	}
}

func TestAccountCooldownBoardPrecedenceAndExpiry(t *testing.T) {
	owner := uuid.New()
	for _, health := range []string{"ok", "slow", "stalled", "looping", "waiting_worker"} {
		for _, future := range []bool{true, false} {
			at := time.Now().Add(time.Hour)
			if !future {
				at = time.Now().Add(-time.Second)
			}
			row := store.ListLatestRunsForRepoRow{ID: uuid.New(), UserID: owner, Kind: "issue", Status: "running", IssueIid: pgtype.Int8{Int64: 1, Valid: true}, Health: health, AccountCooldownClass: pgconv.TextOrNull("overload"), AccountCooldownEligibleAt: pgconv.Time(at)}
			cards := assembleCards([]store.Issue{{ForgeIssueIid: 1}}, []store.ListLatestRunsForRepoRow{row}, nil, nil, nil, owner, "github", nil, 0)
			dto := cards[0].LatestRun
			want := health
			if health == "ok" && future {
				want = "waiting_account_cooldown"
			}
			if dto.Health != want || (dto.AccountCooldownClass != nil) != (want == "waiting_account_cooldown") {
				t.Fatalf("%s future=%v: %+v", health, future, dto)
			}
		}
	}
}

func TestAccountCooldownMapperClearsStaleEvidence(t *testing.T) {
	dto := runToDTO(store.Run{ID: uuid.New(), Status: "running", Health: "waiting_account_cooldown", HealthReason: pgconv.TextOrNull(apitypes.AccountCooldownReason)}, "", 0, 0, 0, time.Now(), 0)
	h := &Handler{}
	h.overlayQueueReasons(context.Background(), &dto)
	applyRevising(&dto, false)
	if dto.Health != "ok" || dto.HealthReason != nil || dto.AccountCooldownClass != nil || dto.Progress == nil || dto.Progress.State == "waiting" {
		t.Fatalf("stale=%+v", dto)
	}
}
