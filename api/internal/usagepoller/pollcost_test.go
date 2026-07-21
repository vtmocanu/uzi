package usagepoller

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/anthropic"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestPollCostScalesWithTokenCount is R3's measurement, made a test rather than a
// note: poll cost is one Anthropic call PER TOKEN per tick, so a user with N tokens
// costs N calls, not one. It asserts the scaling is exactly linear (no accidental
// N^2 from a per-token re-list, and no missed token), and records the per-token
// wall time for a 3-token user against the default poll interval so a future change
// that regresses it is visible.
//
// It is a real test (fails on wrong counts), not a benchmark, so it runs in the
// normal suite. The concurrency cap means wall time is not N × one-call latency,
// which is the point of measuring it.
func TestPollCostScalesWithTokenCount(t *testing.T) {
	const perCallLatency = 5 * time.Millisecond
	var calls atomic.Int64
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		calls.Add(1)
		time.Sleep(perCallLatency) // simulate the network round-trip
		return reading(10, 10, anthropic.SourceUsageEndpoint), nil
	}}

	for _, tokensPerUser := range []int{1, 3} {
		calls.Store(0)
		user := uuid.New()
		rows := make([]store.ListAnthropicTokensToPollRow, 0, tokensPerUser)
		for i := 0; i < tokensPerUser; i++ {
			rows = append(rows, store.ListAnthropicTokensToPollRow{
				ID: uuid.New(), UserID: user, Ciphertext: []byte("ct"), SealedWith: store.SealedWithMaster,
			})
		}
		st := &multiTokenStore{rows: rows, upserts: map[uuid.UUID]store.UpsertRateLimitsParams{}}
		e := New(st, passthroughOpener{}, cl, time.Minute, true, slog.New(slog.NewTextHandler(io.Discard, nil)))

		start := time.Now()
		e.tickAll(context.Background())
		elapsed := time.Since(start)

		if got := calls.Load(); got != int64(tokensPerUser) {
			t.Fatalf("%d tokens ⇒ %d Anthropic calls, want exactly %d (one per token, none missed, none doubled)",
				tokensPerUser, got, tokensPerUser)
		}
		if len(st.upserts) != tokensPerUser {
			t.Fatalf("%d tokens ⇒ %d gauge writes, want %d", tokensPerUser, len(st.upserts), tokensPerUser)
		}
		t.Logf("R3: %d-token user polled in %s (%s/token; cap=%d, interval=1m)",
			tokensPerUser, elapsed.Round(time.Millisecond), (elapsed / time.Duration(tokensPerUser)).Round(time.Millisecond), defaultMaxConcurrency)
	}
}
