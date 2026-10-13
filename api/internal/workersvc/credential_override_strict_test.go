package workersvc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type storedOverrideMetaStore struct {
	Store
	row   store.GetUserSecretMetaByIDOfKindRow
	err   error
	calls []store.GetUserSecretMetaByIDOfKindParams
}

func (s *storedOverrideMetaStore) GetUserSecretMetaByIDOfKind(_ context.Context, p store.GetUserSecretMetaByIDOfKindParams) (store.GetUserSecretMetaByIDOfKindRow, error) {
	s.calls = append(s.calls, p)
	return s.row, s.err
}
func TestCheckStoredCredentialOverrideEnabled(t *testing.T) {
	user, id := uuid.New(), uuid.New()
	pin := &CredentialOverride{Mode: CredentialOverrideModePinned, SecretID: &id}
	cause := errors.New("metadata outage")
	for _, tc := range []struct {
		name            string
		override        *CredentialOverride
		disabled        bool
		lookupErr, want error
		calls           int
	}{
		{"inherit", nil, false, nil, nil, 0},
		{"auto", &CredentialOverride{Mode: CredentialOverrideModeAuto}, false, nil, nil, 0},
		{"default", &CredentialOverride{Mode: CredentialOverrideModeDefault}, false, nil, nil, 0},
		{"nulled_pin", &CredentialOverride{Mode: CredentialOverrideModePinned}, false, nil, nil, 0},
		{"enabled", pin, false, nil, nil, 1},
		{"disabled", pin, true, nil, ErrCredentialDisabled, 1},
		{"deleted", pin, false, pgx.ErrNoRows, nil, 1},
		{"wrapped_deleted", pin, false, fmt.Errorf("read: %w", pgx.ErrNoRows), nil, 1},
		{"lookup_error", pin, false, cause, cause, 1},
		{"wrapped_error", pin, false, fmt.Errorf("read: %w", cause), cause, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &storedOverrideMetaStore{row: store.GetUserSecretMetaByIDOfKindRow{Disabled: tc.disabled}, err: tc.lookupErr}
			svc := New(q, newBox(t), testParams())
			strict, ok := any(svc).(interface {
				CheckStoredCredentialOverrideEnabled(context.Context, uuid.UUID, *CredentialOverride) error
			})
			if !ok {
				t.Fatal("Service lacks strict stored-override check")
			}
			for _, check := range []func(context.Context, uuid.UUID, *CredentialOverride) error{
				strict.CheckStoredCredentialOverrideEnabled,
				func(ctx context.Context, u uuid.UUID, o *CredentialOverride) error {
					return checkStoredOverrideEnabled(ctx, q, u, o)
				},
			} {
				q.calls = nil
				err := check(context.Background(), user, tc.override)
				if !errors.Is(err, tc.want) {
					t.Fatalf("error=%v want %v", err, tc.want)
				}
				if len(q.calls) != tc.calls {
					t.Fatalf("lookups=%v", q.calls)
				}
				if tc.calls > 0 && q.calls[0] != (store.GetUserSecretMetaByIDOfKindParams{ID: id, UserID: user, Kind: store.KindAnthropicToken}) {
					t.Fatalf("lookup identity=%+v", q.calls[0])
				}
			}
			want := tc.want
			if errors.Is(want, cause) {
				want = nil
			}
			if err := svc.ScheduleCredentialDisabled(context.Background(), user, tc.override, nil); !errors.Is(err, want) {
				t.Fatalf("tolerant preflight=%v want %v", err, want)
			}
		})
	}
}
