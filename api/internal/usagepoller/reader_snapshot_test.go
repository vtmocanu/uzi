package usagepoller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/anthropic"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type epochOpener func(context.Context, uuid.UUID, string, string, []byte, int64) ([]byte, error)

func (f epochOpener) OpenSealedAtEpoch(ctx context.Context, id uuid.UUID, kind, marker string, ct []byte, epoch int64) ([]byte, error) {
	return f(ctx, id, kind, marker, ct, epoch)
}

type epochPollStore struct {
	*fakeStore
	first, fresh store.GetAnthropicTokenToPollRow
	args         []store.GetAnthropicTokenToPollParams
	lookupErr    error
}

func (s *epochPollStore) ListAnthropicTokensToPoll(context.Context) ([]store.ListAnthropicTokensToPollRow, error) {
	return []store.ListAnthropicTokensToPollRow{store.ListAnthropicTokensToPollRow(s.first)}, nil
}
func (s *epochPollStore) GetAnthropicTokenToPoll(_ context.Context, arg store.GetAnthropicTokenToPollParams) (store.GetAnthropicTokenToPollRow, error) {
	s.args = append(s.args, arg)
	if !arg.SecretID.Valid {
		return s.first, nil
	}
	return s.fresh, s.lookupErr
}

func TestPollSnapshotRetryKeepsResolvedIDAndWriteFences(t *testing.T) {
	for _, poke := range []bool{false, true} {
		for _, changed := range []string{"none", "revision", "generation", "owner", "id", "second transition"} {
			t.Run(changed+map[bool]string{false: "/tick", true: "/poke"}[poke], func(t *testing.T) {
				owner, id := uuid.New(), uuid.New()
				base := newFakeStore()
				base.rev[id] = 3
				base.generation[id] = 4
				row := store.GetAnthropicTokenToPollRow{ID: id, UserID: owner, VaultEpoch: 7,
					EnablementRev: 3, AnthropicSuccessGeneration: 4, NotifyEarlyLimitReset: false}
				st := &epochPollStore{fakeStore: base, first: row, fresh: row}
				st.fresh.VaultEpoch = 8
				st.fresh.NotifyEarlyLimitReset = true // original notification preference remains authoritative
				switch changed {
				case "revision":
					st.fresh.EnablementRev++
				case "generation":
					st.fresh.AnthropicSuccessGeneration++
				case "owner":
					st.fresh.UserID = uuid.New()
				case "id":
					st.fresh.ID = uuid.New()
				}
				opens := 0
				op := epochOpener(func(_ context.Context, uid uuid.UUID, kind, _ string, _ []byte, epoch int64) ([]byte, error) {
					opens++
					if uid != owner || kind != store.KindAnthropicToken {
						t.Fatal("changed open owner/kind")
					}
					if opens == 1 {
						if epoch != 7 {
							t.Fatal("first open lost snapshot epoch")
						}
						return nil, store.ErrVaultSealStale
					}
					if epoch != 8 {
						t.Fatal("retry attached wrong epoch")
					}
					if changed == "second transition" {
						return nil, store.ErrVaultSealStale
					}
					return []byte("original token"), nil
				})
				cl := &fakeClient{usage: func(token []byte) (anthropic.Reading, error) {
					if string(token) != "original token" {
						t.Fatal("wrong token")
					}
					return reading(10, 20, anthropic.SourceUsageEndpoint), nil
				}}
				e, _ := newEngine(t, st, op, cl, false)
				if poke {
					e.pokeUser(context.Background(), owner, uuid.Nil)
				} else {
					e.tickAll(context.Background())
				}
				wantArgs := 1
				if poke {
					wantArgs++
				}
				if len(st.args) != wantArgs {
					t.Fatalf("lookups=%d want=%d", len(st.args), wantArgs)
				}
				last := st.args[len(st.args)-1]
				if last.UserID != owner || last.SecretID != (pgtype.UUID{Bytes: id, Valid: true}) {
					t.Fatal("retry re-resolved default")
				}
				calls, _ := cl.calls()
				wantOpen, wantProvider := 1, 0
				if changed == "none" || changed == "second transition" {
					wantOpen = 2
				}
				if changed == "none" {
					wantProvider = 1
				}
				if opens != wantOpen || calls != wantProvider {
					t.Fatalf("opens=%d provider=%d", opens, calls)
				}
				write, ok := st.got(id)
				if changed == "none" {
					if !ok || write.UserID != owner || write.EnablementRev != 3 {
						t.Fatal("lost original write fence")
					}
				} else if ok || len(st.rejected) != 0 {
					t.Fatal("stale poll wrote a verdict")
				}
			})
		}
	}
}

func TestPollSnapshotFailuresNeverProbeOrReject(t *testing.T) {
	for _, tc := range []struct {
		name               string
		openErr, lookupErr error
		lookups            int
	}{
		{"infrastructure", errors.New("state unavailable"), nil, 0},
		{"wrapped stale", fmt.Errorf("wrapped: %w", store.ErrVaultSealStale), nil, 0},
		{"locked", secretopen.ErrVaultLocked, nil, 0},
		{"bad material", secretopen.ErrUndecryptable, nil, 0},
		{"refetch infrastructure", store.ErrVaultSealStale, errors.New("lookup unavailable"), 1},
		{"refetch missing", store.ErrVaultSealStale, pgx.ErrNoRows, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner, id := uuid.New(), uuid.New()
			st := &epochPollStore{fakeStore: newFakeStore(), first: store.GetAnthropicTokenToPollRow{ID: id, UserID: owner, VaultEpoch: 7}, lookupErr: tc.lookupErr}
			opens := 0
			op := epochOpener(func(context.Context, uuid.UUID, string, string, []byte, int64) ([]byte, error) {
				opens++
				return nil, tc.openErr
			})
			cl := &fakeClient{}
			e, _ := newEngine(t, st, op, cl, true)
			e.tickAll(context.Background())
			calls, probes := cl.calls()
			if _, ok := st.got(id); ok || len(st.rejected) != 0 || calls != 0 || probes != 0 || opens != 1 || len(st.args) != tc.lookups {
				t.Fatalf("failed open performed credential work: opens=%d lookups=%d calls=%d probes=%d", opens, len(st.args), calls, probes)
			}
		})
	}
}

func TestPollSnapshotSuccessHasNoRefetch(t *testing.T) {
	owner, id := uuid.New(), uuid.New()
	st := &epochPollStore{fakeStore: newFakeStore(), first: store.GetAnthropicTokenToPollRow{ID: id, UserID: owner, VaultEpoch: 12}}
	op := epochOpener(func(_ context.Context, _ uuid.UUID, _, _ string, _ []byte, epoch int64) ([]byte, error) {
		if epoch != 12 {
			t.Fatal("lost initial epoch")
		}
		return []byte("token"), nil
	})
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) { return reading(10, 20, anthropic.SourceUsageEndpoint), nil }}
	e, _ := newEngine(t, st, op, cl, false)
	e.tickAll(context.Background())
	if len(st.args) != 0 {
		t.Fatal("successful bulk open performed credential lookup")
	}
}
