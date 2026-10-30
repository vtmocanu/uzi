package middleware

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func reservationLimiter(max int) *Limiter {
	return &Limiter{max: max, window: time.Hour, buckets: make(map[string]*bucket)}
}

func TestReservationFailuresAndRefund(t *testing.T) {
	l := reservationLimiter(2)
	first, ok := l.Reserve("owner")
	if !ok {
		t.Fatal("first attempt denied")
	}
	success, ok := l.Reserve("owner")
	if !ok {
		t.Fatal("second attempt denied")
	}
	for range 5 {
		if denied, ok := l.Reserve("owner"); ok || denied != nil {
			t.Fatal("in-flight work exceeded budget")
		}
	}
	// The first attempt failed: leave it charged. Only completed successful work refunds.
	if !success.Refund() {
		t.Fatal("successful attempt was not refunded")
	}
	copy := *success
	if copy.Refund() || success.Refund() {
		t.Fatal("duplicate refund succeeded")
	}
	if _, ok := l.Reserve("owner"); !ok {
		t.Fatal("refund did not free exactly one attempt")
	}
	if _, ok := l.Reserve("owner"); ok {
		t.Fatal("failure charge or budget lost")
	}
	// Keep the failed reservation alive without refunding it.
	if first == nil {
		t.Fatal("failure had no handle")
	}
	var absent *Reservation
	if absent.Refund() || (&Reservation{}).Refund() {
		t.Fatal("empty reservation refunded")
	}
}

func TestReservationParallel(t *testing.T) {
	l := reservationLimiter(8)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	handles := make(chan *Reservation, 64)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, ok := l.Reserve("owner"); ok {
				admitted.Add(1)
				handles <- r
			}
		}()
	}
	wg.Wait()
	close(handles)
	if admitted.Load() != 8 {
		t.Fatalf("admitted %d, want 8", admitted.Load())
	}
	var refunds atomic.Int32
	for r := range handles {
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if r.Refund() {
					refunds.Add(1)
				}
			}()
		}
	}
	wg.Wait()
	if refunds.Load() != 8 {
		t.Fatalf("refunds = %d, want 8", refunds.Load())
	}
	for range 8 {
		if _, ok := l.Reserve("owner"); !ok {
			t.Fatal("refund lost")
		}
	}
	if _, ok := l.Reserve("owner"); ok {
		t.Fatal("parallel refund created excess budget")
	}
}

func TestReservationRolloverAndReplacement(t *testing.T) {
	for _, replacement := range []string{"expired", "reserve", "allow", "swept"} {
		t.Run(replacement, func(t *testing.T) {
			l := reservationLimiter(1)
			old, ok := l.Reserve("owner")
			if !ok {
				t.Fatal("initial reserve denied")
			}
			l.mu.Lock()
			l.buckets["owner"].resetAt = time.Now().Add(-time.Second)
			if replacement == "swept" {
				delete(l.buckets, "owner")
			}
			l.mu.Unlock()
			switch replacement {
			case "reserve":
				if _, ok := l.Reserve("owner"); !ok {
					t.Fatal("new window denied")
				}
			case "allow":
				if !l.Allow("owner") {
					t.Fatal("new Allow window denied")
				}
			case "swept":
				if _, ok := l.Reserve("owner"); !ok {
					t.Fatal("swept replacement denied")
				}
			}
			if old.Refund() {
				t.Fatal("old window refunded")
			}
			if replacement == "expired" {
				if _, ok := l.Reserve("owner"); !ok {
					t.Fatal("expired window failed to reset")
				}
			}
			if _, ok := l.Reserve("owner"); ok {
				t.Fatal("old refund credited replacement bucket")
			}
		})
	}
}

func TestVaultUnlockReservationSharesRouteBudget(t *testing.T) {
	l := reservationLimiter(1)
	user := store.User{ID: uuid.New()}
	key := VaultUnlockKey(user.ID)
	if key != "/api/vault/unlock|"+user.ID.String() {
		t.Fatalf("canonical key = %q", key)
	}
	held, ok := l.Reserve(key)
	if !ok {
		t.Fatal("reserve denied")
	}
	router := chi.NewRouter()
	router.Route("/api", func(r chi.Router) {
		r.Route("/vault", func(r chi.Router) {
			r.With(l.PerUserMiddleware).Post("/unlock", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
		})
	})
	request := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/vault/unlock", nil)
		req = req.WithContext(ContextWithUser(req.Context(), user))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := request(); got != 429 {
		t.Fatalf("in-flight reservation did not block route: %d", got)
	}
	if !held.Refund() {
		t.Fatal("refund denied")
	}
	// Existing Allow counts denied calls too, so that denied route request is
	// still charged. A new window demonstrates sharing in the opposite direction.
	l.mu.Lock()
	l.buckets[key].resetAt = time.Now().Add(-time.Second)
	l.mu.Unlock()
	if got := request(); got != 204 {
		t.Fatalf("explicit route denied: %d", got)
	}
	if _, ok := l.Reserve(key); ok {
		t.Fatal("explicit unlock did not charge canonical budget")
	}
	if _, ok := l.Reserve(VaultUnlockKey(uuid.New())); !ok {
		t.Fatal("different owners share budget")
	}
}
