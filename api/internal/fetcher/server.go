package fetcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

const (
	// MaxRequestBody caps the worker's request body.
	MaxRequestBody = 8 << 10
	// maxCredentialLen caps the bearer credential a worker presents.
	maxCredentialLen = 512
	// completeTimeout bounds the source-log write, which runs even when the worker has
	// gone away.
	completeTimeout = 15 * time.Second
	// DefaultMaxInflight bounds concurrent fetches (Begin to response) in one fetcher
	// process. Each holds at most one body of up to the per-file cap plus one byte, in an
	// io.ReadAll buffer whose capacity can grow to about twice that, so fetch bodies take
	// roughly MaxInflight x 2 x the per-file cap at most. It bounds nothing else: requests
	// refused as busy, header and TLS buffers, and idle connections are outside it.
	DefaultMaxInflight = 16
)

// Server is the worker-facing HTTP handler.
type Server struct {
	fetcher  *Fetcher
	control  Control
	log      *slog.Logger
	inflight chan struct{}
	now      func() time.Time
	mux      *http.ServeMux
}

// NewServer returns the handler for POST /v1/fetch and GET /healthz. maxInflight <= 0
// takes DefaultMaxInflight.
func NewServer(f *Fetcher, c Control, log *slog.Logger, maxInflight int) *Server {
	if log == nil {
		log = slog.Default()
	}
	if maxInflight <= 0 {
		maxInflight = DefaultMaxInflight
	}
	s := &Server{
		fetcher:  f,
		control:  c,
		log:      log,
		inflight: make(chan struct{}, maxInflight),
		now:      time.Now,
		mux:      http.NewServeMux(),
	}
	s.mux.HandleFunc("POST /v1/fetch", s.handleFetch)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) handleFetch(w http.ResponseWriter, r *http.Request) {
	cred, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		writeRefusal(w, http.StatusUnauthorized, apitypes.FetchErrorDTO{Reason: ReasonCredentialInvalid, Error: "a fetch credential is required"})
		return
	}
	var req apitypes.FetchRequest
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, MaxRequestBody), &req); err != nil || req.URL == "" {
		writeRefusal(w, http.StatusBadRequest, apitypes.FetchErrorDTO{Reason: ReasonBadRequest, Error: `the body must be exactly {"url": "<https URL>"}`})
		return
	}
	if len(req.URL) > MaxURLLen {
		writeRefusal(w, http.StatusBadRequest, apitypes.FetchErrorDTO{Reason: ReasonURLTooLong, Error: "the URL is longer than " + strconv.Itoa(MaxURLLen) + " bytes"})
		return
	}
	select {
	case s.inflight <- struct{}{}:
		defer func() { <-s.inflight }()
	default:
		writeRefusal(w, http.StatusServiceUnavailable, apitypes.FetchErrorDTO{Reason: ReasonBusy, Error: "the fetcher is busy; retry shortly"})
		return
	}

	ctx := r.Context()
	adm, err := s.control.Begin(ctx, cred, req.URL)
	if err != nil {
		s.writeBeginError(w, req.URL, err)
		return
	}

	started := s.now()
	res, ref := s.fetcher.Fetch(ctx, req.URL, adm.Entries, adm.MaxBytes)
	rec := AttemptRecord{
		ReservationID: adm.ReservationID,
		URL:           req.URL,
		StartedAt:     started.UTC(),
		FinishedAt:    s.now().UTC(),
	}
	if ref != nil {
		rec.Verdict = VerdictRefused
		rec.Reason = ref.Reason
		rec.FinalURL = ref.FinalURL
		rec.HTTPStatus = ref.HTTPStatus
	} else {
		rec.Verdict = VerdictAllowed
		rec.FinalURL = res.FinalURL
		rec.HTTPStatus = res.HTTPStatus
		rec.ContentType = res.ContentType
		rec.Bytes = int64(len(res.Body))
		rec.SHA256 = res.SHA256
	}

	// The log write runs even if the worker has gone away: the attempt happened.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), completeTimeout)
	defer cancel()
	if err := s.control.Complete(cctx, cred, rec); err != nil {
		s.log.Error("fetch not logged; refusing", "host", hostOf(req.URL), "verdict", rec.Verdict, "reason", rec.Reason, "err", err)
		writeRefusal(w, http.StatusServiceUnavailable, apitypes.FetchErrorDTO{Reason: ReasonLogFailed, Error: "the fetch could not be recorded in the source log, so it is refused"})
		return
	}
	s.log.Info("fetch",
		"host", hostOf(req.URL),
		"final_host", hostOf(rec.FinalURL),
		"verdict", rec.Verdict,
		"reason", rec.Reason,
		"http_status", rec.HTTPStatus,
		"bytes", rec.Bytes,
		"duration_ms", rec.FinishedAt.Sub(rec.StartedAt).Milliseconds(),
	)
	if ref != nil {
		body := apitypes.FetchErrorDTO{Reason: ref.Reason, Error: ref.Message}
		if ref.Reason == ReasonUpstreamStatus {
			body.UpstreamStatus = ref.HTTPStatus
		}
		writeRefusal(w, statusFor(ref.Reason), body)
		return
	}
	h := w.Header()
	h.Set("Content-Type", res.ContentType)
	h.Set("Content-Length", strconv.Itoa(len(res.Body)))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Uzi-Final-Url", res.FinalURL)
	h.Set("X-Uzi-Sha256", res.SHA256)
	h.Set("X-Uzi-Bytes", strconv.Itoa(len(res.Body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Body)
}

func (s *Server) writeBeginError(w http.ResponseWriter, rawURL string, err error) {
	var adm *AdmissionRefusedError
	switch {
	case errors.Is(err, ErrCredentialInvalid):
		writeRefusal(w, http.StatusUnauthorized, apitypes.FetchErrorDTO{Reason: ReasonCredentialInvalid, Error: "the fetch credential is invalid or revoked"})
	case errors.As(err, &adm):
		writeRefusal(w, http.StatusTooManyRequests, apitypes.FetchErrorDTO{Reason: ReasonAdmissionRefused, AdmissionReason: adm.Reason, Error: "the run's fetch allowance does not admit this fetch"})
	default:
		s.log.Error("fetch admission failed", "host", hostOf(rawURL), "err", err)
		writeRefusal(w, http.StatusServiceUnavailable, apitypes.FetchErrorDTO{Reason: ReasonControlUnavailable, Error: "the fetch could not be admitted; retry shortly"})
	}
}

// statusFor maps a fetch refusal reason to the worker-facing HTTP status.
func statusFor(reason string) int {
	switch reason {
	case ReasonDNSFailed, ReasonConnectFailed, ReasonTLS, ReasonTimeout, ReasonHeadersTooLarge,
		ReasonUpstreamStatus, ReasonUpstreamError, ReasonTooLarge:
		return http.StatusBadGateway
	case ReasonCancelled:
		return http.StatusServiceUnavailable
	default:
		return http.StatusForbidden
	}
}

func writeRefusal(w http.ResponseWriter, status int, body apitypes.FetchErrorDTO) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// bearer extracts a bearer credential: one token, no whitespace, bounded length.
func bearer(h string) (string, bool) {
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	if tok == "" || len(tok) > maxCredentialLen || strings.ContainsFunc(tok, func(r rune) bool { return r <= ' ' || r >= 0x7f }) {
		return "", false
	}
	return tok, true
}

// decodeStrict decodes exactly one JSON value with no unknown fields.
func decodeStrict(r io.Reader, dst any) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("the body must contain exactly one JSON value")
	}
	return nil
}

// maxLoggedHost caps the host hostOf returns.
const maxLoggedHost = 255

// hostOf returns only the host of a URL, for logs: never the path or query, which can
// carry tokens. The host is worker- or site-controlled, so every byte outside printable
// ASCII (a bidi override, a C1 control) is percent-encoded, and it is cut to
// maxLoggedHost bytes.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	h := escapeQuery(u.Hostname())
	if len(h) > maxLoggedHost {
		h = h[:maxLoggedHost]
	}
	return h
}

// minStreamBytesPerSec is the slowest worker read rate HandlerBudget allows for when it
// budgets the time to write the largest body.
const minStreamBytesPerSec = 1 << 20

// HandlerBudget is the longest one POST /v1/fetch handler runs once its request is read:
// Begin (bounded by controlTimeout), the fetch (bounded by fetchTimeout), Complete
// (bounded by completeTimeout) and writing a body of maxFileBytes to a worker reading at
// minStreamBytesPerSec. A worker that reads slower than that can be cut off.
func HandlerBudget(controlTimeout, fetchTimeout time.Duration, maxFileBytes int64) time.Duration {
	stream := time.Duration((maxFileBytes+minStreamBytesPerSec-1)/minStreamBytesPerSec) * time.Second
	return controlTimeout + fetchTimeout + completeTimeout + stream
}
