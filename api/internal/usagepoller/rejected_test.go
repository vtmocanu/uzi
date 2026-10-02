package usagepoller

import (
	"context"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/anthropic"
	"testing"
)

func TestProbeRejectionOnlyForAuthenticationRefusal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		rejected bool
	}{
		{"unauthorized", httpErr(401), true},
		{"forbidden", httpErr(403), true},
		{"rate limited", httpErr(429), false},
		{"transport", transport(), false},
		{"malformed", malformed(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.New()
			st := newFakeStore(id)
			cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) { return anthropic.Reading{}, httpErr(429) }, probe: func([]byte) (anthropic.Reading, error) { return anthropic.Reading{}, tc.err }}
			e, _ := newEngine(t, st, &fakeOpener{}, cl, true)
			e.pollToken(context.Background(), id, id, 0, 0, true, false, func() ([]byte, error) { return []byte("fake-token"), nil })
			if st.rejected[id] != tc.rejected {
				t.Fatalf("rejected = %v", st.rejected[id])
			}
		})
	}
}

func TestRejectionUsesGenerationCapturedBeforeUsageCall(t *testing.T) {
	id := uuid.New()
	st := newFakeStore(id)
	cl := &fakeClient{
		usage: func([]byte) (anthropic.Reading, error) {
			st.mu.Lock()
			st.generation[id]++ // another successful poll commits during this request
			st.rejected[id] = false
			st.mu.Unlock()
			return anthropic.Reading{}, httpErr(429)
		},
		probe: func([]byte) (anthropic.Reading, error) { return anthropic.Reading{}, httpErr(401) },
	}
	e, _ := newEngine(t, st, &fakeOpener{}, cl, true)
	e.tickAll(context.Background())
	if st.markGeneration != 0 || st.generation[id] != 1 || st.rejected[id] {
		t.Fatalf("mark generation=%d, current=%d, rejected=%v", st.markGeneration, st.generation[id], st.rejected[id])
	}
}

func TestSuccessfulPollClearsRejection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		usageOK bool
	}{{"usage", true}, {"probe", false}} {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.New()
			st := newFakeStore(id)
			st.rejected[id] = true
			cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
				if tc.usageOK {
					return reading(10, 20, "usage_endpoint"), nil
				}
				return anthropic.Reading{}, httpErr(429)
			}, probe: func([]byte) (anthropic.Reading, error) { return reading(10, 20, "header_probe"), nil }}
			e, _ := newEngine(t, st, &fakeOpener{}, cl, true)
			e.pollToken(context.Background(), id, id, 0, 0, true, false, func() ([]byte, error) { return []byte("fake-token"), nil })
			if st.rejected[id] {
				t.Fatal("successful poll left rejection marker")
			}
		})
	}
}

func TestOldPollCannotRejectOrClearAfterReplacement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rejected bool
	}{{"rotate", true}, {"default upsert", true}, {"rotate success", false}, {"default upsert success", false}} {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.New()
			st := newFakeStore(id)
			st.rev[id] = 1
			st.rejected[id] = true
			cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
				if !tc.rejected {
					return reading(10, 20, "usage_endpoint"), nil
				}
				return anthropic.Reading{}, httpErr(429)
			}, probe: func([]byte) (anthropic.Reading, error) { return anthropic.Reading{}, httpErr(401) }}
			e, _ := newEngine(t, st, &fakeOpener{}, cl, true)
			e.pollToken(context.Background(), id, id, 0, 0, true, false, func() ([]byte, error) { return []byte("old-token"), nil })
			if !st.rejected[id] {
				t.Fatal("old revision changed current marker")
			}
			if _, ok := st.got(id); ok {
				t.Fatal("old revision wrote a gauge")
			}
		})
	}
}
