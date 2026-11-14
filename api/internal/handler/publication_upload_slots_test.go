package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Use real shared admission with fake publication/gate work, without a database.
type uploadHeldService struct {
	*fakeHeldSvc
	slots *workersvc.Service
}

func (s *uploadHeldService) AcquireHeldPublishSlot(workerID uuid.UUID) (func(), bool) {
	return s.slots.AcquireHeldPublishSlot(workerID)
}

type uploadTerminalService struct {
	*fakeTerminalSvc
	slots *workersvc.Service
}

func (s *uploadTerminalService) AcquireHeldPublishSlot(workerID uuid.UUID) (func(), bool) {
	return s.slots.AcquireHeldPublishSlot(workerID)
}

func uploadHandlers() (*Handler, *workersvc.Service, *fakeHeldSvc, *fakeTerminalSvc) {
	slots := workersvc.New(nil, nil, workersvc.Params{})
	held, terminal := newFakeHeldSvc(), newFakeTerminalSvc()
	held.publish = func(context.Context, workersvc.HeldPublishRequest, []byte) (workersvc.HeldPublishResult, error) {
		return okResult(), nil
	}
	return &Handler{heldSvc: &uploadHeldService{held, slots}, terminalSvc: &uploadTerminalService{terminal, slots}}, slots, held, terminal
}

func uploadRequest(route string, worker store.Worker, body io.Reader) *http.Request {
	var req *http.Request
	if route == "held" {
		req = heldReq(body)
	} else {
		req = reqWithWorkerAndParam(http.MethodPost, "/api/worker/runs/x/terminal-preservation", body, uuid.New())
	}
	return req.WithContext(mw.ContextWithWorker(req.Context(), worker))
}
func serveUpload(h *Handler, route string, w http.ResponseWriter, r *http.Request) {
	if route == "held" {
		h.WorkerRunHeldPublication(w, r)
	} else {
		h.WorkerRunTerminalPreservation(w, r)
	}
}
func uploadPayload(t *testing.T, route string) string {
	t.Helper()
	if route == "held" {
		return "pack"
	}
	return termBody(t, nil)
}

type uploadReader func([]byte) (int, error)

func (f uploadReader) Read(p []byte) (int, error) { return f(p) }

func TestPublicationUploadSlotsSharedWorkerAdmission(t *testing.T) {
	for _, firstRoute := range []string{"held", "terminal"} {
		t.Run(firstRoute, func(t *testing.T) {
			h, slots, _, _ := uploadHandlers()
			first := store.Worker{ID: uuid.New(), UserID: uuid.New()}
			other := store.Worker{ID: uuid.New(), UserID: uuid.New()}
			began, proceed, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(proceed) })
			signalRead := sync.OnceFunc(func() { close(began) })
			payload := strings.NewReader(uploadPayload(t, firstRoute))
			body := uploadReader(func(p []byte) (int, error) { signalRead(); <-proceed; return payload.Read(p) })
			firstResponse := httptest.NewRecorder()
			t.Cleanup(func() {
				unblock()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("initial upload did not finish")
				}
			})
			go func() {
				defer close(done)
				serveUpload(h, firstRoute, firstResponse, uploadRequest(firstRoute, first, body))
			}()
			select {
			case <-began:
			case <-time.After(5 * time.Second):
				t.Fatal("initial upload did not begin reading")
			}
			// Both routes share worker identity while its first body remains outstanding.
			for _, route := range []string{"held", "terminal", "held", "terminal"} {
				reads := 0
				content := strings.NewReader(uploadPayload(t, route))
				body := uploadReader(func(p []byte) (int, error) { reads++; return content.Read(p) })
				response := httptest.NewRecorder()
				serveUpload(h, route, response, uploadRequest(route, first, body))
				if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" || reads != 0 {
					t.Fatalf("same-worker %s: status=%d retry=%q body_reads=%d", route, response.Code, response.Header().Get("Retry-After"), reads)
				}
			}
			otherRoute := "terminal"
			if firstRoute == "terminal" {
				otherRoute = "held"
			}
			response := httptest.NewRecorder()
			serveUpload(h, otherRoute, response, uploadRequest(otherRoute, other, strings.NewReader(uploadPayload(t, otherRoute))))
			if response.Code != http.StatusOK {
				t.Fatalf("other worker %s: status=%d body=%q", otherRoute, response.Code, response.Body.String())
			}
			unblock()
			<-done
			if firstResponse.Code != http.StatusOK {
				t.Fatalf("initial response=%d", firstResponse.Code)
			}
			release, ok := slots.AcquireHeldPublishSlot(first.ID)
			if !ok {
				t.Fatal("completed upload retained the worker reservation")
			}
			release()
		})
	}
}

func TestPublicationUploadSlotsReleasePaths(t *testing.T) {
	for _, route := range []string{"held", "terminal"} {
		for _, outcome := range []string{"success", "gate-error", "body-error", "cancelled", "publish-error", "panic"} {
			t.Run(route+"/"+outcome, func(t *testing.T) {
				h, slots, held, terminal := uploadHandlers()
				worker := store.Worker{ID: uuid.New(), UserID: uuid.New()}
				marker := errors.New("publication fixture")
				var body io.Reader = strings.NewReader(uploadPayload(t, route))
				var ctx context.Context
				var cancel context.CancelFunc
				reads := 0
				switch outcome {
				case "gate-error":
					held.gate = func(context.Context, workersvc.HeldPublishRequest) (*workersvc.HeldPublishResult, error) {
						return nil, marker
					}
					terminal.admit = marker
				case "body-error":
					body = uploadReader(func([]byte) (int, error) { reads++; return 0, marker })
				case "cancelled":
					body = uploadReader(func([]byte) (int, error) { reads++; cancel(); return 0, ctx.Err() })
				case "publish-error":
					held.publish = func(context.Context, workersvc.HeldPublishRequest, []byte) (workersvc.HeldPublishResult, error) {
						return workersvc.HeldPublishResult{}, marker
					}
					terminal.publish = func(context.Context, workersvc.TerminalPublishRequest) (workersvc.TerminalPublishResult, error) {
						return workersvc.TerminalPublishResult{}, marker
					}
				case "panic":
					held.publish = func(context.Context, workersvc.HeldPublishRequest, []byte) (workersvc.HeldPublishResult, error) {
						panic(marker)
					}
					terminal.publish = func(context.Context, workersvc.TerminalPublishRequest) (workersvc.TerminalPublishResult, error) {
						panic(marker)
					}
				}
				req := uploadRequest(route, worker, body)
				ctx, cancel = context.WithCancel(req.Context())
				defer cancel()
				req = req.WithContext(ctx)
				response := httptest.NewRecorder()
				panicked := false
				func() {
					defer func() {
						if value := recover(); value != nil {
							if value != marker {
								t.Fatalf("unexpected panic: %v", value)
							}
							panicked = true
						}
					}()
					serveUpload(h, route, response, req)
				}()
				if panicked != (outcome == "panic") {
					t.Fatalf("panic=%t for %s", panicked, outcome)
				}
				if outcome == "success" && response.Code != http.StatusOK {
					t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
				}
				if outcome == "body-error" || outcome == "cancelled" {
					if reads == 0 {
						t.Fatal("error body was not read")
					}
				}
				if outcome == "cancelled" && ctx.Err() == nil {
					t.Fatal("request context was not cancelled")
				}
				// Reacquire this worker and all remaining capacity: both reservations released.
				for i := 0; i < 4; i++ {
					id := uuid.New()
					if i == 0 {
						id = worker.ID
					}
					release, ok := slots.AcquireHeldPublishSlot(id)
					if !ok {
						t.Fatalf("slot %d not released after %s", i, outcome)
					}
					t.Cleanup(release)
				}
				if release, ok := slots.AcquireHeldPublishSlot(uuid.New()); ok {
					release()
					t.Fatal("global upload cap changed")
				}
			})
		}
	}
}
