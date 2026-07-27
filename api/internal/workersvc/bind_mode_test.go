package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// PRD #111 M3 — the worker bind mode, and specifically the two claims the rest of
// the suite does NOT test.
//
// Measured before these were written: two mutations to production code left the
// whole api suite green.
//
//	(a) making workerSecretID ignore the mode entirely and read the id whenever it
//	    is present — every other fixture sets the two together, so ignoring one of
//	    them gives the same answer everywhere;
//	(b) deleting SetWorkerAnthropicToken's "a non-pinned mode carries no id" guard —
//	    the handler never supplies an id for a non-pinned mode, so the service-level
//	    guard was reached by nothing.
//
// Both are the same shape: the invariant only breaks on inputs where mode and id
// DISAGREE, and no fixture staged one. These do.

// TestWorkerSecretIDReadsTheIDOnlyWhenPinned is mutation (a)'s control. The
// disagreeing states are not hypothetical — 00078's FK nulls the id on a token
// delete while leaving the mode, and switching a worker to auto leaves whatever the
// column held — so "the id is read only in pinned mode" has to be true of rows the
// database really produces.
func TestWorkerSecretIDReadsTheIDOnlyWhenPinned(t *testing.T) {
	id := uuid.New()
	bound := pgtype.UUID{Bytes: id, Valid: true}

	for _, tc := range []struct {
		name string
		wkr  store.Worker
		want *uuid.UUID
	}{
		{
			name: "pinned with an id resolves that credential",
			wkr:  store.Worker{AnthropicBindMode: BindModePinned, AnthropicSecretID: bound},
			want: &id,
		},
		{
			// D9. Reachable through the shipped delete path, not a defensive case.
			name: "pinned with NO id falls back to the owner's default",
			wkr:  store.Worker{AnthropicBindMode: BindModePinned},
			want: nil,
		},
		{
			// The one mutation (a) turned green: a stale id beside a non-pinned mode
			// must not be spent. Without this, "the id is read only in pinned mode"
			// is a comment rather than a property.
			name: "default with a STALE id ignores it",
			wkr:  store.Worker{AnthropicBindMode: BindModeDefault, AnthropicSecretID: bound},
			want: nil,
		},
		{
			name: "auto with a STALE id ignores it",
			wkr:  store.Worker{AnthropicBindMode: BindModeAuto, AnthropicSecretID: bound},
			want: nil,
		},
		{
			// M3 ships the mode; M4 fills in the selector. Until then auto resolves
			// the owner's default — which is also where auto lands when the pool is
			// empty or stale (D7/R2), so the interim behaviour is a supported outcome
			// of the finished feature rather than a placeholder.
			name: "auto with no id resolves the owner's default until M4",
			wkr:  store.Worker{AnthropicBindMode: BindModeAuto},
			want: nil,
		},
		{
			// Unreachable through the API (00088's CHECK and ValidBindMode both refuse
			// it) and asserted anyway, because the safe direction is worth pinning: an
			// unknown mode must spend the owner's default, never a credential nobody
			// selected.
			name: "an unrecognised mode ignores the id",
			wkr:  store.Worker{AnthropicBindMode: "something-new", AnthropicSecretID: bound},
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := workerSecretID(tc.wkr)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("resolved %v, want the owner's default (nil)", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("resolved the owner's default, want %v", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("resolved %v, want %v", *got, *tc.want)
			}
		})
	}
}

// bindModeStore records what SetWorkerAnthropicToken writes. Narrow on purpose:
// embedding the interface makes any query beyond these two panic rather than
// return a zero value that quietly passes.
type bindModeStore struct {
	Store
	secretOwner map[uuid.UUID]uuid.UUID
	arg         store.SetWorkerAnthropicSecretParams
	called      bool
}

func (b *bindModeStore) GetUserSecretCiphertextByID(_ context.Context, arg store.GetUserSecretCiphertextByIDParams) (store.GetUserSecretCiphertextByIDRow, error) {
	owner, ok := b.secretOwner[arg.ID]
	if !ok || owner != arg.UserID {
		return store.GetUserSecretCiphertextByIDRow{}, pgx.ErrNoRows
	}
	return store.GetUserSecretCiphertextByIDRow{UserID: owner, Kind: store.KindAnthropicToken}, nil
}

func (b *bindModeStore) SetWorkerAnthropicSecret(_ context.Context, arg store.SetWorkerAnthropicSecretParams) (store.Worker, error) {
	b.called = true
	b.arg = arg
	return store.Worker{ID: arg.ID, UserID: arg.UserID, AnthropicBindMode: arg.AnthropicBindMode, AnthropicSecretID: arg.AnthropicSecretID}, nil
}

// TestSetWorkerAnthropicTokenDropsTheIDOffPinned is mutation (b)'s control: it
// calls the SERVICE with a combination the handler cannot produce, which is the
// only way to reach the guard at all.
//
// It matters because the guard is what makes workerSecretID's rule cheap to trust.
// A stored row where mode and id disagree is legal (no CHECK can forbid it — D9),
// so the write path is where the two are kept consistent; without it, "a default
// worker carries no id" would rest entirely on every future caller remembering.
func TestSetWorkerAnthropicTokenDropsTheIDOffPinned(t *testing.T) {
	owner, secretID, workerID := uuid.New(), uuid.New(), uuid.New()

	for _, mode := range []string{BindModeDefault, BindModeAuto} {
		t.Run(mode+" discards a supplied id", func(t *testing.T) {
			st := &bindModeStore{secretOwner: map[uuid.UUID]uuid.UUID{secretID: owner}}
			svc := New(st, newBox(t), testParams())
			if _, err := svc.SetWorkerAnthropicToken(context.Background(), owner, workerID, mode, &secretID); err != nil {
				t.Fatalf("SetWorkerAnthropicToken: %v", err)
			}
			if !st.called {
				t.Fatal("nothing was written")
			}
			if st.arg.AnthropicBindMode != mode {
				t.Fatalf("wrote mode %q, want %q", st.arg.AnthropicBindMode, mode)
			}
			if st.arg.AnthropicSecretID.Valid {
				t.Fatalf("wrote id %+v alongside mode %q; a non-pinned mode must carry none, "+
					"or the row disagrees with itself and only workerSecretID's mode check stands "+
					"between the stale id and a claim", st.arg.AnthropicSecretID, mode)
			}
		})
	}

	// The pinned path is unchanged: the id is validated against the caller and written.
	t.Run("pinned keeps the id", func(t *testing.T) {
		st := &bindModeStore{secretOwner: map[uuid.UUID]uuid.UUID{secretID: owner}}
		svc := New(st, newBox(t), testParams())
		if _, err := svc.SetWorkerAnthropicToken(context.Background(), owner, workerID, BindModePinned, &secretID); err != nil {
			t.Fatalf("SetWorkerAnthropicToken: %v", err)
		}
		if !st.arg.AnthropicSecretID.Valid || uuid.UUID(st.arg.AnthropicSecretID.Bytes) != secretID {
			t.Fatalf("pinned wrote %+v, want %v", st.arg.AnthropicSecretID, secretID)
		}
	})

	// A foreign secret is still refused BEFORE the write, so the caller gets a 404
	// naming the problem rather than a constraint violation from the composite FK.
	t.Run("pinned to a foreign secret is refused", func(t *testing.T) {
		st := &bindModeStore{secretOwner: map[uuid.UUID]uuid.UUID{secretID: uuid.New()}}
		svc := New(st, newBox(t), testParams())
		if _, err := svc.SetWorkerAnthropicToken(context.Background(), owner, workerID, BindModePinned, &secretID); err == nil {
			t.Fatal("binding to another user's secret succeeded")
		}
		if st.called {
			t.Fatal("a refused binding still reached the write")
		}
	})

	// An illegal mode is refused by the service, not only by the database CHECK, so
	// a future caller that skips ValidBindMode gets a named error rather than a 500.
	t.Run("an illegal mode is refused before the write", func(t *testing.T) {
		st := &bindModeStore{secretOwner: map[uuid.UUID]uuid.UUID{}}
		svc := New(st, newBox(t), testParams())
		if _, err := svc.SetWorkerAnthropicToken(context.Background(), owner, workerID, "whatever", nil); err == nil {
			t.Fatal("an illegal bind mode was accepted")
		}
		if st.called {
			t.Fatal("an illegal mode still reached the write")
		}
	})
}
