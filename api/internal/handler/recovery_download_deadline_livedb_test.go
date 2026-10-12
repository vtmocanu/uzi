package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Exported mutation control: in an exported copy, clear the socket deadline in
// recoveryDeadlineWriter.SetWriteDeadline after its original delegation, and change
// recoveryDeadlineUpperWantComplete to true. Run TestRecoveryDownloadOutlivesServerWriteTimeoutLiveDB
// with -race -count=1 -p 1 and UZI_TEST_DATABASE_URL set to a throwaway database.
// The upper scenario and all timing/context preconditions remain identical.
const recoveryDeadlineUpperWantComplete = false

type recoveryDeadlineWrite struct {
	at              time.Time
	contextDeadline time.Time
	contextErr      error
	err             error
}

type recoveryDeadlineObservation struct {
	mu                  sync.Mutex
	installed, deadline time.Time
	stream              context.Context
	writes              []recoveryDeadlineWrite
	flushAt             time.Time
	flushErr            error
	problems            []string
	flushed, received   chan struct{}
	stop                context.Context
}

func (o *recoveryDeadlineObservation) problem(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.problems = append(o.problems, s)
}

// Every wait is bounded by the scenario's stop context; one failed precondition
// stops this producer rather than silently proceeding with invalid timing.
func (o *recoveryDeadlineObservation) wait(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-o.stop.Done():
		o.problem("timed out waiting for first flush/client receipt")
		return false
	}
}

func recoveryDeadlinePause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

type recoveryDeadlineStore struct {
	recovery.Store
	o     *recoveryDeadlineObservation
	delay time.Duration
}

func (s recoveryDeadlineStore) GetCaptureForOwner(ctx context.Context, arg store.GetCaptureForOwnerParams) (store.RecoveryCapture, error) {
	s.o.mu.Lock()
	installed := !s.o.installed.IsZero()
	s.o.mu.Unlock()
	if !installed {
		s.o.problem("socket deadline missing before metadata lookup")
	}
	if !recoveryDeadlinePause(s.o.stop, s.delay) {
		return store.RecoveryCapture{}, s.o.stop.Err()
	}
	return s.Store.GetCaptureForOwner(ctx, arg)
}

type recoveryDeadlineRunStore struct {
	workersvc.Store
	o *recoveryDeadlineObservation
}

func (s recoveryDeadlineRunStore) GetRunByIDForUser(ctx context.Context, arg store.GetRunByIDForUserParams) (store.Run, error) {
	s.o.mu.Lock()
	installed := !s.o.installed.IsZero()
	s.o.mu.Unlock()
	if !installed {
		s.o.problem("socket deadline missing before run metadata lookup")
	}
	return s.Store.GetRunByIDForUser(ctx, arg)
}

type recoveryDeadlineDB struct {
	recovery.DB
	o     *recoveryDeadlineObservation
	delay time.Duration
}

func (db recoveryDeadlineDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := db.DB.Query(ctx, sql, args...)
	if err != nil || !strings.Contains(sql, "FROM recovery_capture_chunks") {
		return rows, err
	}
	db.o.mu.Lock()
	db.o.stream = ctx
	db.o.mu.Unlock()
	return &recoveryDeadlineRows{Rows: rows, o: db.o, delay: db.delay}, nil
}

type recoveryDeadlineRows struct {
	pgx.Rows
	o     *recoveryDeadlineObservation
	delay time.Duration
	calls int
}

func (r *recoveryDeadlineRows) Next() bool {
	r.calls++
	if r.calls == 2 {
		if !r.o.wait(r.o.flushed) || !r.o.wait(r.o.received) {
			return false
		}
		if !recoveryDeadlinePause(r.o.stop, r.delay) {
			r.o.problem("producer pause interrupted")
			return false
		}
	}
	return r.Rows.Next()
}

type recoveryDeadlineWriter struct {
	http.ResponseWriter
	o *recoveryDeadlineObservation
}

func (w *recoveryDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *recoveryDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	installed := time.Now()
	err := http.NewResponseController(w.ResponseWriter).SetWriteDeadline(deadline)
	w.o.mu.Lock()
	w.o.installed, w.o.deadline = installed, deadline
	w.o.mu.Unlock()
	if err != nil {
		w.o.problem(fmt.Sprintf("install deadline: %v", err))
	}
	return err
}

func (w *recoveryDeadlineWriter) Write(p []byte) (int, error) {
	w.o.mu.Lock()
	ctx := w.o.stream
	w.o.mu.Unlock()
	var event recoveryDeadlineWrite
	if ctx != nil {
		event.contextDeadline, _ = ctx.Deadline()
		event.contextErr = ctx.Err()
	}
	event.at = time.Now() // Immediately before delegation to the real socket writer.
	n, err := w.ResponseWriter.Write(p)
	event.err = err
	w.o.mu.Lock()
	w.o.writes = append(w.o.writes, event)
	w.o.mu.Unlock()
	return n, err
}

func (w *recoveryDeadlineWriter) Flush() {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	at := time.Now()
	w.o.mu.Lock()
	first := w.o.flushAt.IsZero()
	if first {
		w.o.flushAt, w.o.flushErr = at, err
	}
	w.o.mu.Unlock()
	if first && err == nil {
		close(w.o.flushed)
	}
}

func TestRecoveryDownloadOutlivesServerWriteTimeoutLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		limit, metadata, producer time.Duration
		complete                  bool
	}{
		{"past_base_timeout", 5 * time.Second, 0, 750 * time.Millisecond, true},
		// 4s effective deadline, 2s lookup pause, 3s producer pause: the first flush lands ~2s before the
		// socket deadline and the second write ~1s after it and ~1s before the streaming context
		// deadline (lookup + limit), so CI scheduling noise cannot erase either 300ms margin.
		{"absolute_upper_bound", 4 * time.Second, 2 * time.Second, 3 * time.Second, recoveryDeadlineUpperWantComplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRecoveryEnv(t)
			seedSvc := e.service(recoveryTestLimits())
			data := make([]byte, 2*(1<<20)+12345)
			for i := range data {
				data[i] = byte((i*31 + 7) % 251)
			}
			reserved := e.reserve(seedSvc, e.worker, "deadline-"+uuid.NewString(), "aaaa1111")
			capture := uuid.MustParse(reserved.CaptureID)
			if _, err := seedSvc.Upload(e.ctx, e.worker, e.run, capture, manifestFor(data), cappedBody(data, e.svcCap())); err != nil {
				t.Fatalf("seed encrypted multichunk capture: %v", err)
			}
			h, router, pool := cliLiveDB(t)
			stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			o := &recoveryDeadlineObservation{flushed: make(chan struct{}), received: make(chan struct{}), stop: stop}
			limits := recoveryTestLimits()
			limits.RequestDeadline = tc.limit
			h.cfg.RecoveryRequestDeadline = 100 * time.Millisecond
			h.wsvc = workersvc.New(recoveryDeadlineRunStore{Store: store.New(pool), o: o}, e.box, workersvc.Params{})
			h.recoverySvc = recovery.New(
				recoveryDeadlineStore{Store: store.New(pool), o: o, delay: tc.metadata},
				recoveryDeadlineDB{DB: pool, o: o, delay: tc.producer}, e.box, limits, nil)
			token := cliMintToken(t, pool, e.user, "user")
			done := make(chan struct{})
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				router.ServeHTTP(&recoveryDeadlineWriter{ResponseWriter: w, o: o}, r)
			}))
			server.Config.WriteTimeout = 250 * time.Millisecond
			server.Config.ReadHeaderTimeout = time.Second
			server.Start()
			defer server.Close()
			client := server.Client()
			client.Timeout = 8 * time.Second
			defer client.CloseIdleConnections()
			req, err := http.NewRequestWithContext(stop, http.MethodGet,
				server.URL+"/api/runs/"+e.run.String()+"/archives/"+capture.String()+"/download", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			started := time.Now()
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			first := make([]byte, 1<<20)
			_, firstErr := io.ReadFull(resp.Body, first)
			if firstErr == nil {
				close(o.received)
			} else {
				cancel()
			}
			rest, readErr := io.ReadAll(resp.Body)
			completedAt := time.Now()
			got := append(first, rest...)
			select {
			case <-done:
			case <-stop.Done():
				t.Fatal("handler/producer did not finish within scenario timeout")
			}
			o.mu.Lock()
			defer o.mu.Unlock()
			t.Logf("elapsed=%s installation=%s socket=%s firstFlush=%s writes=%d", time.Since(started),
				o.installed.Sub(started), o.deadline.Sub(started), o.flushAt.Sub(started), len(o.writes))
			if tc.complete && (readErr != nil || len(got) != len(data) || sha256hex(got) != sha256hex(data)) {
				t.Errorf("full transfer: bytes=%d/%d error=%v checksum=%s", len(got), len(data), readErr, sha256hex(got))
			}
			if len(o.problems) != 0 {
				t.Fatalf("preconditions: %v", o.problems)
			}
			if firstErr != nil || !bytes.Equal(first, data[:len(first)]) {
				t.Fatalf("first chunk not received intact: %v", firstErr)
			}
			if o.installed.IsZero() || o.deadline.Sub(o.installed) < tc.limit-100*time.Millisecond ||
				o.deadline.Sub(o.installed) > tc.limit+100*time.Millisecond {
				t.Fatalf("socket limit does not match effective service limit %s", tc.limit)
			}
			if o.flushErr != nil || o.flushAt.IsZero() || o.deadline.Sub(o.flushAt) < 300*time.Millisecond {
				t.Fatalf("first real flush must succeed with >=300ms socket margin: at=%s error=%v", o.flushAt, o.flushErr)
			}
			if len(o.writes) < 2 {
				t.Fatalf("missing second socket write: %d", len(o.writes))
			}
			second := o.writes[1]
			t.Logf("secondWrite=%s socketMargin=%s contextMargin=%s contextError=%v socketError=%v bytes=%d/%d readError=%v",
				second.at.Sub(started), o.deadline.Sub(second.at), second.contextDeadline.Sub(second.at),
				second.contextErr, second.err, len(got), len(data), readErr)
			if second.contextErr != nil || second.contextDeadline.IsZero() ||
				second.contextDeadline.Sub(second.at) < 300*time.Millisecond {
				t.Fatalf("second socket write lacks live streaming context/margin: %+v", second)
			}
			t.Logf("streamDeadline=%s completedAt=%s", second.contextDeadline.Sub(started), completedAt.Sub(started))
			if tc.metadata > 0 && second.at.Sub(o.deadline) < 300*time.Millisecond {
				t.Fatalf("second write did not occur after absolute socket deadline: %+v", second)
			}
			if tc.metadata == 0 && second.at.Sub(started) <= server.Config.WriteTimeout {
				t.Fatal("transfer did not outlast base server timeout")
			}
			if tc.complete {
				if !completedAt.Before(second.contextDeadline) || (tc.metadata == 0 && !completedAt.Before(o.deadline)) {
					t.Fatal("complete transfer exceeded the effective deadline")
				}
				t.Logf("complete bytes=%d sha256=%s", len(got), sha256hex(got))
				for _, event := range o.writes {
					if event.err != nil {
						t.Fatalf("socket write failed: %v", event.err)
					}
				}
			} else {
				if second.err == nil {
					t.Fatal("socket write after absolute deadline succeeded")
				}
				if readErr == nil || len(got) >= len(data) {
					t.Fatalf("expected incomplete response: bytes=%d error=%v", len(got), readErr)
				}
			}
		})
	}
}
