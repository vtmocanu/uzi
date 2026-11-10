package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestBlocksOrdinaryResume(t *testing.T) {
	resolved := pgtype.Timestamptz{Time: time.Unix(1700000000, 0), Valid: true}
	outcome := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	hold := pgtype.Text{String: "codex_policy_hold", Valid: true}
	cases := []struct {
		name string
		run  store.Run
		want bool
	}{
		{"no episode", store.Run{}, false},
		{"open episode", store.Run{PolicyOutcomeID: outcome}, true},
		{"policy hold", store.Run{HoldReason: hold, PolicyOutcomeID: outcome, PolicyResolvedAt: resolved}, true},
		{"pending fresh on an open episode", store.Run{PolicyPendingFresh: true, PolicyOutcomeID: outcome}, true},
		{"pending fresh without an episode record", store.Run{PolicyPendingFresh: true}, true},
		{"pending fresh on a resolved episode (restored claim)", store.Run{PolicyPendingFresh: true, PolicyOutcomeID: outcome, PolicyResolvedAt: resolved}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := blocksOrdinaryResume(tc.run); got != tc.want {
				t.Fatalf("blocksOrdinaryResume = %v, want %v", got, tc.want)
			}
		})
	}
}
