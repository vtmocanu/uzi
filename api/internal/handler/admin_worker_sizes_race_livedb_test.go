package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4213 M3: a size delete against each writer that references a size, in BOTH orders,
// driven step by step through store.SetWorkerSizeStepHookForTest. Four writers: a persistent
// provision, a stored ephemeral_default_size, a stored hosted_default_size (the ephemeral
// provisioner's race is in hostedsvc, which owns its transaction).
//
//	(a) the writer holds its FOR SHARE, uncommitted; the delete must block, and after the
//	    writer commits it must refuse as in use.
//	(b) the delete holds its FOR UPDATE, uncommitted; the writer must block, and after the
//	    delete commits it must refuse (unknown size).

const raceWait = 15 * time.Second

// raceHold pauses the first transaction that reaches (stage, name) until release, and records
// that transaction's backend pid (read on the held transaction itself, before it is parked).
type raceHold struct {
	locked  chan struct{}
	release chan struct{}
	once    sync.Once
	relOnce sync.Once
	pid     int32 // written before locked is closed
	pidErr  error // likewise
}

func holdAt(t *testing.T, stage, name string) *raceHold {
	t.Helper()
	h := &raceHold{locked: make(chan struct{}), release: make(chan struct{})}
	store.SetWorkerSizeStepHookForTest(t, func(s string, names []string, db store.DBTX) {
		if s != stage || !slices.Contains(names, name) {
			return
		}
		first := false
		h.once.Do(func() {
			first = true
			h.pidErr = db.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&h.pid)
			close(h.locked)
		})
		if first {
			<-h.release
		}
	})
	// Registered after the hook, so it runs first: the parked transaction is released before
	// the hook is cleared.
	t.Cleanup(h.Release)
	return h
}

func (h *raceHold) Release() { h.relOnce.Do(func() { close(h.release) }) }

func (h *raceHold) WaitLocked(t *testing.T) {
	t.Helper()
	select {
	case <-h.locked:
	case <-time.After(raceWait):
		t.Fatal("the held transaction never reached its lock step")
	}
	if h.pidErr != nil {
		t.Fatalf("read the held transaction's backend pid: %v", h.pidErr)
	}
}

// waitBlockedBy polls until some backend's pg_blocking_pids contains the holder's backend pid,
// i.e. the other side is genuinely queued behind the held transaction's lock, within a
// bounded time.
func waitBlockedBy(t *testing.T, pool *pgxpool.Pool, holderPid int32) {
	t.Helper()
	deadline := time.Now().Add(raceWait)
	for time.Now().Before(deadline) {
		var n int
		err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND $1::int = ANY(pg_blocking_pids(pid))`, holderPid).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no backend was ever blocked by backend pid %d", holderPid)
}

func awaitResult[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(raceWait):
		t.Fatalf("%s did not finish", what)
		panic("unreachable")
	}
}

func notDone[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s finished while the other side held its lock", what)
	case <-time.After(150 * time.Millisecond):
	}
}

// raceWriter is one referencing write. prepare runs on the test goroutine (it may t.Fatal) and
// returns the work to run on another goroutine, which must never touch t; committed and
// refused check the outcome of an order (a) commit (the reference exists) or an order (b)
// refusal.
type raceWriter struct {
	name    string
	prepare func(t *testing.T, f *sizeFx, size string) func() any
	// committed asserts the write committed and that the delete refused because of it.
	committed func(t *testing.T, f *sizeFx, size string, writeOut any, del *httptest.ResponseRecorder)
	// refused asserts the write was refused after the delete removed the size.
	refused func(t *testing.T, f *sizeFx, size string, writeOut any)
}

func raceWriters() []raceWriter {
	settingWriter := func(key string) raceWriter {
		return raceWriter{
			name: "settings " + key,
			prepare: func(t *testing.T, f *sizeFx, size string) func() any {
				serve := cookieReqPrepared(t, f.router, http.MethodPut, "/api/admin/settings", f.jwt, `{"settings":{"`+key+`":"`+size+`"}}`)
				return func() any { return serve() }
			},
			committed: func(t *testing.T, f *sizeFx, size string, out any, del *httptest.ResponseRecorder) {
				if rec := out.(*httptest.ResponseRecorder); rec.Code != http.StatusOK {
					t.Fatalf("settings PUT = %d %s, want 200", rec.Code, rec.Body.String())
				}
				r := decodeRefusal(t, del)
				if del.Code != http.StatusConflict || r.Reason != "in_use" || len(r.UsedBy.Settings) != 1 || r.UsedBy.Settings[0] != key {
					t.Fatalf("delete after a committed %s = %d %s", key, del.Code, del.Body.String())
				}
			},
			refused: func(t *testing.T, f *sizeFx, size string, out any) {
				rec := out.(*httptest.ResponseRecorder)
				if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), key+": unknown worker size") {
					t.Fatalf("settings PUT after the delete = %d %s, want 400 unknown worker size", rec.Code, rec.Body.String())
				}
				var n int
				_ = f.h.pool.QueryRow(context.Background(), `SELECT count(*) FROM app_settings WHERE key = $1 AND value = $2`, key, size).Scan(&n)
				if n != 0 {
					t.Fatal("the refused settings PUT stored the name")
				}
			},
		}
	}
	return []raceWriter{
		{
			name: "persistent provision",
			prepare: func(t *testing.T, f *sizeFx, size string) func() any {
				return func() any {
					_, err := f.h.provisionHostedWorker(context.Background(), f.owner, "race-"+size, "base", size, false, 5)
					return err
				}
			},
			committed: func(t *testing.T, f *sizeFx, size string, out any, del *httptest.ResponseRecorder) {
				if out != nil && out.(error) != nil {
					t.Fatalf("provision = %v, want success", out)
				}
				r := decodeRefusal(t, del)
				if del.Code != http.StatusConflict || r.Reason != "in_use" || r.UsedBy.Workers != 1 {
					t.Fatalf("delete after a committed provision = %d %s", del.Code, del.Body.String())
				}
			},
			refused: func(t *testing.T, f *sizeFx, size string, out any) {
				if err, _ := out.(error); err == nil || err.Error() != errUnknownHostedSize.Error() {
					t.Fatalf("provision after the delete = %v, want errUnknownHostedSize", out)
				}
				var n int
				_ = f.h.pool.QueryRow(context.Background(), `SELECT count(*) FROM workers WHERE hosted_size = $1`, size).Scan(&n)
				if n != 0 {
					t.Fatal("a provision created a worker on a deleted size")
				}
			},
		},
		settingWriter(settings.KeyEphemeralDefaultSize),
		settingWriter(settings.KeyHostedDefaultSize),
	}
}

func TestWorkerSizeDeleteRacesReferencingWritersLiveDB(t *testing.T) {
	for _, w := range raceWriters() {
		t.Run(w.name+"/writer holds FOR SHARE, delete blocks then refuses", func(t *testing.T) {
			f := newM3Fx(t)
			f.h.settings = settings.New(f.h.q, time.Minute)
			size := m3Size
			if rec := f.put(t, size, sizeBody(500, 2000, 1024, 2048, 10, 10)); rec.Code != http.StatusCreated {
				t.Fatalf("seed size = %d %s", rec.Code, rec.Body.String())
			}
			hold := holdAt(t, store.WorkerSizeStepShared, size)

			write := w.prepare(t, f, size)
			wDone := make(chan any, 1)
			go func() { wDone <- write() }()
			hold.WaitLocked(t)

			del := cookieReqPrepared(t, f.router, http.MethodDelete, "/api/admin/worker-sizes/"+size, f.jwt, "")
			dDone := make(chan *httptest.ResponseRecorder, 1)
			go func() { dDone <- del() }()
			waitBlockedBy(t, f.h.pool, hold.pid)
			notDone(t, dDone, "the delete")

			hold.Release()
			out := awaitResult(t, wDone, "the write")
			delRec := awaitResult(t, dDone, "the delete")
			w.committed(t, f, size, out, delRec)
			if !f.exists(t, size) {
				t.Fatal("the size was deleted although a committed write references it")
			}
		})
		t.Run(w.name+"/delete holds FOR UPDATE, writer blocks then refuses", func(t *testing.T) {
			f := newM3Fx(t)
			f.h.settings = settings.New(f.h.q, time.Minute)
			size := m3Size
			if rec := f.put(t, size, sizeBody(500, 2000, 1024, 2048, 10, 10)); rec.Code != http.StatusCreated {
				t.Fatalf("seed size = %d %s", rec.Code, rec.Body.String())
			}
			hold := holdAt(t, store.WorkerSizeStepDeleteLocked, size)

			del := cookieReqPrepared(t, f.router, http.MethodDelete, "/api/admin/worker-sizes/"+size, f.jwt, "")
			dDone := make(chan *httptest.ResponseRecorder, 1)
			go func() { dDone <- del() }()
			hold.WaitLocked(t)

			write := w.prepare(t, f, size)
			wDone := make(chan any, 1)
			go func() { wDone <- write() }()
			waitBlockedBy(t, f.h.pool, hold.pid)
			notDone(t, wDone, "the write")

			hold.Release()
			delRec := awaitResult(t, dDone, "the delete")
			if delRec.Code != http.StatusOK {
				t.Fatalf("delete = %d %s, want 200", delRec.Code, delRec.Body.String())
			}
			w.refused(t, f, size, awaitResult(t, wDone, "the write"))
			if f.exists(t, size) {
				t.Fatal("the size row survived the committed delete")
			}
		})
	}
}
