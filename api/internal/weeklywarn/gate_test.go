package weeklywarn

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
)

// countingPool is a TxBeginner that records each Begin and refuses it: a reading the validity gate
// rejects must never reach it (no transaction, no lock, no seed row).
type countingPool struct{ begins int }

func (p *countingPool) Begin(context.Context) (pgx.Tx, error) {
	p.begins++
	return nil, errors.New("begin refused by the test")
}

func gateWarner(clock time.Time) (*Warner, *countingPool) {
	p := &countingPool{}
	w := New(Config{Pool: p, Queries: store.New(nil), Publisher: noPublisher{},
		ClaudeInterval: time.Hour, CodexInterval: time.Hour, Now: func() time.Time { return clock }})
	return w, p
}

type noPublisher struct{}

func (noPublisher) PublishPersisted(context.Context, store.Notification, notifysvc.Notification) {}

func fp(v float64) *float64 { return &v }
func ip(v int64) *int64     { return &v }

const weekSecs = int64(604800)

func weeklyWindow(pct *float64, resetAt int64) *apitypes.CodexRateLimitWindowDTO {
	return &apitypes.CodexRateLimitWindowDTO{UsedPercent: pct, LimitWindowSeconds: ip(weekSecs), ResetAt: ip(resetAt)}
}

func fence() notifysvc.CodexAccountFence {
	return notifysvc.CodexAccountFence{OwnerID: uuid.New(), AccountID: uuid.New(), Generation: 1, CredentialRevision: 1, EnablementSig: "sig"}
}

func TestValidityGateCodexRejectsBeforeAnyTransaction(t *testing.T) {
	synced := at(10)
	future := at(100).Unix()
	good := apitypes.CodexRateLimitBucketDTO{ID: "codex", Primary: weeklyWindow(fp(90), future)}
	bad := func(pct *float64) apitypes.CodexRateLimitBucketDTO {
		return apitypes.CodexRateLimitBucketDTO{ID: "gpt", Secondary: weeklyWindow(pct, future)}
	}
	for _, tc := range []struct {
		name    string
		now     time.Time
		buckets []apitypes.CodexRateLimitBucketDTO
		accept  bool
	}{
		{"one valid window accepted", synced, []apitypes.CodexRateLimitBucketDTO{good}, true},
		{"mixed validity: a sibling without a percent", synced, []apitypes.CodexRateLimitBucketDTO{good, bad(nil)}, false},
		{"mixed validity: a NaN sibling", synced, []apitypes.CodexRateLimitBucketDTO{good, bad(fp(math.NaN()))}, false},
		{"mixed validity: a +Inf sibling", synced, []apitypes.CodexRateLimitBucketDTO{good, bad(fp(math.Inf(1)))}, false},
		{"mixed validity: a negative sibling", synced, []apitypes.CodexRateLimitBucketDTO{good, bad(fp(-1))}, false},
		{"stale: written more than three intervals before now", synced.Add(4 * time.Hour), []apitypes.CodexRateLimitBucketDTO{good}, false},
		{"fresh: just inside three intervals", synced.Add(3 * time.Hour), []apitypes.CodexRateLimitBucketDTO{good}, true},
		{"unknown: no weekly window", synced, []apitypes.CodexRateLimitBucketDTO{{ID: "codex", Primary: &apitypes.CodexRateLimitWindowDTO{UsedPercent: fp(90), LimitWindowSeconds: ip(18000), ResetAt: ip(future)}}}, false},
		{"unknown: no buckets", synced, nil, false},
		{"pre-reset: the best reset fell between the write and now", synced.Add(2 * time.Hour),
			[]apitypes.CodexRateLimitBucketDTO{{ID: "codex", Primary: weeklyWindow(fp(90), synced.Add(time.Hour).Unix())}}, false},
		// A whole-reading-valid post-boundary reading: every reset is at or before the written instant.
		{"valid post-boundary reading is accepted", synced,
			[]apitypes.CodexRateLimitBucketDTO{{ID: "codex", Primary: weeklyWindow(fp(90), synced.Add(-time.Hour).Unix())}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, pool := gateWarner(tc.now)
			w.ObserveCodexWeekly(context.Background(), fence(), tc.buckets, synced)
			if accepted := pool.begins > 0; accepted != tc.accept {
				t.Fatalf("transactions opened = %d, want accepted=%v", pool.begins, tc.accept)
			}
		})
	}
}

func TestValidityGateClaudeRejectsBeforeAnyTransaction(t *testing.T) {
	synced := at(10)
	pct := int16(90)
	reset := at(100)
	pastReset := synced.Add(time.Hour)
	for _, tc := range []struct {
		name   string
		now    time.Time
		pct    *int16
		reset  *time.Time
		accept bool
	}{
		{"fresh reading accepted", synced, &pct, &reset, true},
		{"a missing reset is still a usable reading", synced, &pct, nil, true},
		{"absent SevenDayPct", synced, nil, &reset, false},
		{"stale", synced.Add(4 * time.Hour), &pct, &reset, false},
		{"pre-reset: the reset fell between the write and now", synced.Add(2 * time.Hour), &pct, &pastReset, false},
		{"post-boundary reading whose reset is not after the write is accepted", synced, &pct, ptrT(synced.Add(-time.Minute)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, pool := gateWarner(tc.now)
			w.ObserveAnthropicWeekly(context.Background(), uuid.New(), uuid.New(), 1, tc.pct, tc.reset, synced)
			if accepted := pool.begins > 0; accepted != tc.accept {
				t.Fatalf("transactions opened = %d, want accepted=%v", pool.begins, tc.accept)
			}
		})
	}
	t.Run("a poller with polling disabled (interval 0) is stale", func(t *testing.T) {
		p := &countingPool{}
		w := New(Config{Pool: p, Queries: store.New(nil), Publisher: noPublisher{}, Now: func() time.Time { return synced }})
		w.ObserveAnthropicWeekly(context.Background(), uuid.New(), uuid.New(), 1, &pct, &reset, synced)
		if p.begins != 0 {
			t.Fatal("a zero interval must reject")
		}
	})
}

// A whole-reading-valid reading whose resets are all at or before the observation instant clears an
// obsolete anchor (the post-boundary NULL path).
func TestValidPostBoundaryReadingClearsObsoleteAnchor(t *testing.T) {
	prev := Row{AnchorReset: ptrT(at(100)), AnchorPct: ptrF(90), AnchorBucket: strPtr("codex"), Warned: true}
	n := nextCodex(prev, at(101), CodexView{Windows: []usagereserve.CodexWeeklyWindow{cw("gpt", pri, 50, 100.5)}})
	if n.AnchorReset != nil || n.AnchorBucket != nil || n.Warned || n.AnchorMulti {
		t.Fatalf("obsolete anchor not cleared: %+v", n)
	}
}

// codexGate is the pure step 0: one valid window beside any malformed sibling rejects the whole reading.
func TestCodexGateTable(t *testing.T) {
	obs := at(10)
	future := at(100).Unix()
	good := apitypes.CodexRateLimitBucketDTO{ID: "codex", Primary: weeklyWindow(fp(90), future)}
	sibling := func(pct *float64) apitypes.CodexRateLimitBucketDTO {
		return apitypes.CodexRateLimitBucketDTO{ID: "gpt", Secondary: weeklyWindow(pct, future)}
	}
	noPct := apitypes.CodexRateLimitBucketDTO{ID: "gpt", Secondary: &apitypes.CodexRateLimitWindowDTO{LimitWindowSeconds: ip(weekSecs), ResetAt: ip(future)}}
	invalid := notifysvc.CodexAccountFence{}
	for _, tc := range []struct {
		name    string
		fence   notifysvc.CodexAccountFence
		buckets []apitypes.CodexRateLimitBucketDTO
		now     time.Time
		want    bool
	}{
		{"valid", fence(), []apitypes.CodexRateLimitBucketDTO{good}, obs, true},
		{"valid plus a valid sibling", fence(), []apitypes.CodexRateLimitBucketDTO{good, sibling(fp(10))}, obs, true},
		{"valid plus a NaN sibling", fence(), []apitypes.CodexRateLimitBucketDTO{good, sibling(fp(math.NaN()))}, obs, false},
		{"valid plus a negative sibling", fence(), []apitypes.CodexRateLimitBucketDTO{good, sibling(fp(-0.5))}, obs, false},
		{"valid plus a missing-percent sibling", fence(), []apitypes.CodexRateLimitBucketDTO{good, noPct}, obs, false},
		{"valid reading, invalid fence", invalid, []apitypes.CodexRateLimitBucketDTO{good}, obs, false},
		{"stale", fence(), []apitypes.CodexRateLimitBucketDTO{good}, obs.Add(4 * time.Hour), false},
		{"no buckets", fence(), nil, obs, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := codexGate(tc.fence, tc.buckets, obs, time.Hour, tc.now); ok != tc.want {
				t.Fatalf("accepted = %v, want %v", ok, tc.want)
			}
		})
	}
}
