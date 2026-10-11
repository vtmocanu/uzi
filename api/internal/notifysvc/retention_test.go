package notifysvc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestRedeliveryRetention(t *testing.T) {
	for _, tc := range []struct {
		name string
		cap  int
		opts bool
		keep int32
	}{
		{"default", 0, false, DefaultUserCap}, {"custom", 7, true, 7},
		{"zero", 0, true, DefaultUserCap}, {"negative", -1, true, DefaultUserCap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := uuid.New()
			rows := []store.ClaimPendingSlackNotificationsRow{
				{ID: uuid.New(), UserID: user, SlackAttempts: MaxSlackAttempts, SlackRender: []byte(`{"title":"last"}`)},
				{ID: uuid.New(), UserID: user, SlackAttempts: MaxSlackAttempts, SlackRender: []byte(`{"title":42}`)},
				{ID: uuid.New(), UserID: uuid.New(), SlackAttempts: MaxSlackAttempts - 1, SlackRender: []byte(`{"title":"pending"}`)},
			}
			fc := &fakeClaimer{rows: rows, pruneErr: errors.New("prune unavailable")}
			slk := &recordingSlacker{}
			fc.onPrune = func() {
				if len(fc.pruned) == 1 && len(slk.renders) != 0 {
					t.Fatal("published before prune")
				}
			}
			var logs bytes.Buffer
			var opts []RedelivererOption
			if tc.opts {
				opts = append(opts, WithRedeliveryUserCap(tc.cap))
			}
			r := NewRedeliverer(fc, slk, slog.New(slog.NewTextHandler(&logs, nil)), opts...)
			tc.cap = 99 // configuration is captured at construction
			got, err := r.Pass(context.Background())
			if got != 2 || err != nil {
				t.Fatalf("Pass = %d, %v", got, err)
			}
			want := []store.PruneNotificationsForUserParams{
				{UserID: user, Keep: tc.keep, MaxAttempts: MaxSlackAttempts},
				{UserID: user, Keep: tc.keep, MaxAttempts: MaxSlackAttempts},
			}
			if !reflect.DeepEqual(fc.pruned, want) {
				t.Fatalf("prunes = %+v, want %+v", fc.pruned, want)
			}
			if slk.renders[0].DeliveryID != rows[0].ID || slk.renders[1].DeliveryID != rows[2].ID {
				t.Fatal("final valid and nonfinal rows must publish despite pruning failures")
			}
			if !strings.Contains(logs.String(), "prune unavailable") || !strings.Contains(logs.String(), "stored slack render is corrupt") {
				t.Fatalf("missing failure logs: %s", logs.String())
			}
		})
	}
}
