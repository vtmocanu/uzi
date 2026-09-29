package fetcher

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Control is the api side of a fetch: admission before, the source log after. The HTTP
// implementation is HTTPControl; tests use a fake.
type Control interface {
	// Begin validates the run credential and admits one fetch of url. It returns
	// ErrCredentialInvalid for an unknown, revoked or terminal-run credential, an
	// *AdmissionRefusedError when a run total or the concurrency limit is used up, and
	// any other error when the api could not answer.
	Begin(ctx context.Context, credential, url string) (Admission, error)
	// Complete records one attempt Begin admitted. A non-nil error means the attempt is
	// not logged, and the caller must not return content (fail closed).
	Complete(ctx context.Context, credential string, rec AttemptRecord) error
}

// Admission is Begin's grant.
type Admission struct {
	// ReservationID names the api-side reservation Complete settles.
	ReservationID string
	// Entries is the run's effective site-list snapshot.
	Entries []string
	// MaxBytes is the per-file cap.
	MaxBytes int64
}

// Verdicts reported in AttemptRecord.Verdict.
const (
	VerdictAllowed = "allowed"
	VerdictRefused = "refused"
)

// AttemptRecord is one source-log entry.
type AttemptRecord struct {
	ReservationID string
	URL           string
	FinalURL      string
	Verdict       string
	Reason        string
	HTTPStatus    int
	ContentType   string
	Bytes         int64
	SHA256        string
	StartedAt     time.Time
	FinishedAt    time.Time
}

// ErrCredentialInvalid is Begin's answer for a run credential the api does not accept.
var ErrCredentialInvalid = errors.New("fetch credential is invalid or revoked")

// AdmissionRefusedError is Begin's answer when the api refuses admission; Reason is the
// api's code.
type AdmissionRefusedError struct {
	Reason string
}

func (e *AdmissionRefusedError) Error() string {
	return fmt.Sprintf("fetch admission refused: %s", e.Reason)
}
