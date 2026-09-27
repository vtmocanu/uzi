package store_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// This file is the live-DB half of PRD #1795 M5's Slack store surface: GetSlackRunContext's
// r.gate_revision column and SetSlackRunGateGen's revision stamp and its generation/revision
// guard, EXECUTED against a real Postgres. The slacksvc fakes model the guard; only this file
// proves the statement admits and refuses the rows the fakes assume. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh provides one).

// sgrAnchor seeds a run with the given gate_revision plus its Slack DM anchor, and returns the id.
// The root ts is derived from the run id: (channel_id, root_ts) is UNIQUE and the database is shared.
func sgrAnchor(t *testing.T, fx *wpFixture, gateRevision int64) uuid.UUID {
	t.Helper()
	id := fx.run(wpRun{status: "awaiting_approval"})
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET gate_revision = $2 WHERE id = $1`, id, gateRevision)
	if _, err := fx.q.UpsertSlackRunMessage(fx.ctx, store.UpsertSlackRunMessageParams{RunID: id, ChannelID: "D1", RootTs: "root-" + id.String()}); err != nil {
		t.Fatalf("UpsertSlackRunMessage: %v", err)
	}
	return id
}

// sgrWrite runs the guarded fresh-gate write; rev 0 passes a NULL revision.
func sgrWrite(fx *wpFixture, id uuid.UUID, ts string, gen int32, rev int64) (store.SlackRunMessage, error) {
	p := store.SetSlackRunGateGenParams{
		RunID: id, GateTs: pgtype.Text{String: ts, Valid: true}, GateState: pgtype.Text{String: "open", Valid: true},
		GateGeneration: pgtype.Int4{Int32: gen, Valid: true},
	}
	if rev > 0 {
		p.GateRevision = pgtype.Int8{Int64: rev, Valid: true}
	}
	return fx.q.SetSlackRunGateGen(fx.ctx, p)
}

// sgrStored reads the anchor back, so a refused write is proven to have left the row untouched.
func sgrStored(t *testing.T, fx *wpFixture, id uuid.UUID) store.SlackRunMessage {
	t.Helper()
	m, err := fx.q.GetSlackRunMessage(fx.ctx, id)
	if err != nil {
		t.Fatalf("GetSlackRunMessage: %v", err)
	}
	return m
}

func sgrWantRefused(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("%s must be refused (no row), got err=%v", what, err)
	}
}

func sgrWantAnchor(t *testing.T, m store.SlackRunMessage, ts string, gen int32, rev int64) {
	t.Helper()
	if m.GateTs.String != ts || m.GateGeneration.Int32 != gen {
		t.Fatalf("anchor = ts %q gen %d, want ts %q gen %d", m.GateTs.String, m.GateGeneration.Int32, ts, gen)
	}
	if rev == 0 {
		if m.GateRevision.Valid {
			t.Fatalf("anchor gate_revision = %d, want NULL", m.GateRevision.Int64)
		}
		return
	}
	if !m.GateRevision.Valid || m.GateRevision.Int64 != rev {
		t.Fatalf("anchor gate_revision = %+v, want %d", m.GateRevision, rev)
	}
}

// TestGetSlackRunContextGateRevisionLiveDB pins that the Slack run-context row carries the run's
// current plan-gate revision, the value a fresh gate card is stamped with.
func TestGetSlackRunContextGateRevisionLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	id := sgrAnchor(t, fx, 7)
	row, err := fx.q.GetSlackRunContext(fx.ctx, id)
	if err != nil {
		t.Fatalf("GetSlackRunContext: %v", err)
	}
	if row.GateRevision != 7 {
		t.Fatalf("gate_revision = %d, want 7", row.GateRevision)
	}
}

// TestSetSlackRunGateGenRevisionLiveDB drives the guarded fresh-gate write through every branch
// the notifier relies on.
func TestSetSlackRunGateGenRevisionLiveDB(t *testing.T) {
	t.Run("stamps the revision", func(t *testing.T) {
		fx := newWPFixture(t)
		id := sgrAnchor(t, fx, 4)
		m, err := sgrWrite(fx, id, "g1", 1, 4)
		if err != nil {
			t.Fatalf("first write: %v", err)
		}
		sgrWantAnchor(t, m, "g1", 1, 4)
		sgrWantAnchor(t, sgrStored(t, fx, id), "g1", 1, 4)
	})

	t.Run("stamps NULL for a run with no allocated revision", func(t *testing.T) {
		fx := newWPFixture(t)
		id := sgrAnchor(t, fx, 0)
		if _, err := sgrWrite(fx, id, "g1", 1, 0); err != nil {
			t.Fatalf("first write: %v", err)
		}
		sgrWantAnchor(t, sgrStored(t, fx, id), "g1", 1, 0)
	})

	t.Run("an older generation is refused and leaves the revision", func(t *testing.T) {
		fx := newWPFixture(t)
		id := sgrAnchor(t, fx, 5)
		if _, err := sgrWrite(fx, id, "g2", 2, 5); err != nil {
			t.Fatalf("seed write: %v", err)
		}
		_, err := sgrWrite(fx, id, "g1-slow", 1, 9) // a slow drain, even carrying a higher revision
		sgrWantRefused(t, err, "an older-generation write")
		sgrWantAnchor(t, sgrStored(t, fx, id), "g2", 2, 5)
	})

	t.Run("a newer generation overwrites, including a NULL revision", func(t *testing.T) {
		fx := newWPFixture(t)
		id := sgrAnchor(t, fx, 5)
		if _, err := sgrWrite(fx, id, "g1", 1, 5); err != nil {
			t.Fatalf("seed write: %v", err)
		}
		if _, err := sgrWrite(fx, id, "g2", 2, 0); err != nil {
			t.Fatalf("newer-generation write: %v", err)
		}
		sgrWantAnchor(t, sgrStored(t, fx, id), "g2", 2, 0)
	})

	// The revise-window race: a card for plan N was stamped revision N at generation N+1, and
	// the real N+1 gate re-cards at the same generation with revision N+1.
	t.Run("an equal generation with a higher revision is admitted", func(t *testing.T) {
		fx := newWPFixture(t)
		id := sgrAnchor(t, fx, 2)
		if _, err := sgrWrite(fx, id, "stale", 2, 1); err != nil {
			t.Fatalf("seed write: %v", err)
		}
		if _, err := sgrWrite(fx, id, "fresh", 2, 2); err != nil {
			t.Fatalf("equal generation, higher revision must be admitted: %v", err)
		}
		sgrWantAnchor(t, sgrStored(t, fx, id), "fresh", 2, 2)
	})

	t.Run("an equal generation with an equal or lower revision is refused", func(t *testing.T) {
		fx := newWPFixture(t)
		id := sgrAnchor(t, fx, 3)
		if _, err := sgrWrite(fx, id, "g", 2, 3); err != nil {
			t.Fatalf("seed write: %v", err)
		}
		_, err := sgrWrite(fx, id, "dup", 2, 3)
		sgrWantRefused(t, err, "an equal-revision write at the same generation")
		_, err = sgrWrite(fx, id, "older", 2, 2)
		sgrWantRefused(t, err, "a lower-revision write at the same generation")
		_, err = sgrWrite(fx, id, "null", 2, 0)
		sgrWantRefused(t, err, "a NULL-revision write at the same generation")
		sgrWantAnchor(t, sgrStored(t, fx, id), "g", 2, 3)
	})

	t.Run("a legacy anchor keeps the generation-only guard", func(t *testing.T) {
		fx := newWPFixture(t)
		id := sgrAnchor(t, fx, 4)
		if _, err := sgrWrite(fx, id, "legacy", 2, 0); err != nil {
			t.Fatalf("seed write: %v", err)
		}
		_, err := sgrWrite(fx, id, "rev", 2, 4)
		sgrWantRefused(t, err, "an equal-generation write over a NULL-revision anchor")
		sgrWantAnchor(t, sgrStored(t, fx, id), "legacy", 2, 0)
	})
}
