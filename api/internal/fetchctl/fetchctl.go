// Package fetchctl is the api side of uzi-fetcher's control protocol (PRD #1906 M3,
// Decisions 3, 7 and 8): the per-run fetch credential a profile-bound run's claim carries,
// the admission a fetch must pass before it happens (Begin), the source-log write and
// counter reconciliation after it (Complete), and the sweep that releases a crashed
// fetch's reservation. The wire contract is the package doc of api/internal/fetcher,
// section "Fetcher -> api"; the HTTP routes are handler.mountFetcherRoutes.
//
// The run is ALWAYS derived from the credential the fetcher forwards (its sha256 is the
// only lookup key), never from anything else in a request: the fetcher's own service
// credential authorizes it to speak for runs whose credential it holds, not to name one.
//
// Admission serializes on the run's single run_fetch_credentials row: Begin locks it
// (SELECT ... FOR UPDATE), counts the attempt, then reserves with one conditional UPDATE
// whose WHERE clause is the cap check, all in one transaction. Under READ COMMITTED the
// lock orders concurrent Begins for a run, and each sees the counters the previous one
// committed, so parallel fetches cannot jointly pass the check and overshoot a total.
//
// Lock order. Every transaction that writes these tables takes row locks in one order:
// runs, then run_fetch_credentials, then run_fetch_reservations. The revoke trigger and the
// claim-time mint start from the run; Begin, Complete and the stale sweep start from the
// credential. Complete in particular locks the credential before the reservation it
// settles, because a re-claim's mint holds the credential while it releases the prior
// claim's reservations: the reverse order would let each wait on the other. The one
// exception is implicit: the foreign-key checks of Begin's reservation insert and
// Complete's source-log insert take FOR KEY SHARE on the run row after the credential lock.
// Only a DELETE of the run (or a change of its key) conflicts with it, not a status update,
// the revoke trigger or the mint, so a run DELETE (run row, then the cascade into the
// credential) concurrent with a Begin or Complete for that run can deadlock, and Postgres
// aborts one of the two (SQLSTATE 40P01).
package fetchctl

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// CredentialPrefix marks a per-run fetch credential, distinct from a worker join token
// (uzw_), a CLI token (uzc_/uza_) and every other uzi token, so a leaked one is
// recognizable on sight. It is part of the token bytes and covered by the hash.
const CredentialPrefix = "uzf_"

// credentialBytes is the random payload length (256 bits).
const credentialBytes = 32

// The control routes, as the fetcher's HTTPControl calls them (fetcher.BeginPath and
// fetcher.CompletePath; TestControlWireMatchesFetcher pins the copies).
const (
	BeginPath    = "/api/fetcher/v1/begin"
	CompletePath = "/api/fetcher/v1/complete"
	// ReasonCredentialInvalid is the 403 body's reason the fetcher maps to its own
	// credential_invalid refusal.
	ReasonCredentialInvalid = "credential_invalid" //nolint:gosec // G101: a reason code, not a credential.
)

// Admission refusal codes (the 429 body's reason). Stable: the fetcher relays them to the
// worker as admission_reason.
const (
	AdmissionRunBytes    = "run_bytes"
	AdmissionRunFiles    = "run_files"
	AdmissionConcurrency = "concurrency"
	AdmissionAttempts    = "attempts"
)

// Write-time caps on the source log's site- or agent-controlled text, applied to the
// ESCAPED form (termsafe.EscapeBounded). A URL the fetcher accepted is at most 2048 bytes
// of printable ASCII, so these only bound a misbehaving fetcher.
const (
	maxLoggedURL         = 4096
	maxLoggedContentType = 512
	maxLoggedReason      = 64
)

// StaleReservationAge is how old an unsettled reservation must be before the sweep
// releases it. The fetcher bounds one fetch at 60 s plus two 10 s control calls, so a
// reservation this old belongs to a fetch that crashed or lost its Complete.
const StaleReservationAge = 10 * time.Minute

// FetchesPageSize is the owner read's default and largest page (GET
// /api/runs/{id}/fetches?limit=). A run's log can hold up to the attempts cap's ceiling
// (settings fetch_max_run_attempts, 100000 rows of up to ~8 KB of URL text each), so the
// read is keyset-paginated and never loads a whole log into one response.
const FetchesPageSize = 500

// Verdicts of an attempt.
const (
	VerdictAllowed = "allowed"
	VerdictRefused = "refused"
)

// ErrCredentialInvalid is Begin's and Complete's answer for a run credential that does not
// resolve (unknown), or for Begin, one that is revoked, belongs to an earlier claim, or
// whose run is not running.
var ErrCredentialInvalid = errors.New("the run fetch credential is invalid")

// ErrReservationNotFound is Complete's answer when the reservation id does not name an
// admitted fetch of the credential's run (another run's reservation included).
var ErrReservationNotFound = errors.New("no such reservation for this run")

// AdmissionRefusedError is Begin's answer when a run cap refuses the fetch.
type AdmissionRefusedError struct{ Reason string }

func (e *AdmissionRefusedError) Error() string { return "fetch admission refused: " + e.Reason }

// InvalidRequestError is Complete's answer for a malformed attempt record.
type InvalidRequestError struct{ Msg string }

func (e *InvalidRequestError) Error() string { return e.Msg }

// GenerateCredential returns a new plaintext credential and its sha256.
func GenerateCredential() (token string, hash []byte, err error) {
	buf := make([]byte, credentialBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("fetchctl: read random: %w", err)
	}
	token = CredentialPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return token, HashCredential(token), nil
}

// HashCredential is sha256(token), the only form the api stores or looks up. The lookup
// compares the full digest inside Postgres' unique index.
func HashCredential(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Snapshot is runs.egress_snapshot: the site list a run was claimed with, taken once.
type Snapshot struct {
	Profile string   `json:"profile"`
	Entries []string `json:"entries"`
}

// ParseSnapshot decodes runs.egress_snapshot. A missing or malformed snapshot is an error:
// a profile-bound run with no snapshot must never admit anything.
func ParseSnapshot(raw []byte) (Snapshot, error) {
	if len(raw) == 0 {
		return Snapshot{}, errors.New("run has no site-list snapshot")
	}
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return Snapshot{}, fmt.Errorf("decode site-list snapshot: %w", err)
	}
	if s.Entries == nil {
		s.Entries = []string{}
	}
	return s, nil
}

// TxBeginner opens a transaction; *pgxpool.Pool satisfies it.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// CapsReader reads the admin fetch caps; *settings.Cache satisfies it.
type CapsReader interface {
	FetchCaps(ctx context.Context) (settings.FetchCaps, error)
}

// Service implements Begin, Complete and the sweep.
type Service struct {
	db   TxBeginner
	caps CapsReader
	now  func() time.Time
}

// New returns a Service.
func New(db TxBeginner, caps CapsReader) *Service {
	return &Service{db: db, caps: caps, now: time.Now}
}

// Admission is Begin's success: the reservation, the run's snapshot entries and the
// per-file cap.
type Admission struct {
	ReservationID uuid.UUID
	Entries       []string
	MaxBytes      int64
}

// Begin validates the run credential and atomically reserves one fetch against the run's
// totals. A refusal by a cap still counts the attempt (and commits it).
func (s *Service) Begin(ctx context.Context, credential string) (Admission, error) {
	if credential == "" {
		return Admission{}, ErrCredentialInvalid
	}
	caps, err := s.caps.FetchCaps(ctx)
	if err != nil {
		return Admission{}, fmt.Errorf("read fetch caps: %w", err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Admission{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)

	cred, err := q.LockFetchCredentialByHash(ctx, HashCredential(credential))
	if errors.Is(err, pgx.ErrNoRows) {
		return Admission{}, ErrCredentialInvalid
	}
	if err != nil {
		return Admission{}, err
	}
	if !credentialLive(cred) {
		return Admission{}, ErrCredentialInvalid
	}
	snap, err := ParseSnapshot(cred.EgressSnapshot)
	if err != nil {
		return Admission{}, err
	}

	n, err := q.BumpFetchAttempt(ctx, store.BumpFetchAttemptParams{RunID: cred.RunID, MaxAttempts: caps.MaxRunAttempts})
	if err != nil {
		return Admission{}, err
	}
	if n == 0 {
		return Admission{}, &AdmissionRefusedError{Reason: AdmissionAttempts}
	}
	n, err = q.ReserveFetch(ctx, store.ReserveFetchParams{
		RunID:        cred.RunID,
		MaxFileBytes: caps.MaxFileBytes,
		MaxRunBytes:  caps.MaxRunBytes,
		MaxRunFiles:  caps.MaxRunFiles,
		MaxInflight:  caps.MaxConcurrentPerRun,
	})
	if err != nil {
		return Admission{}, err
	}
	if n == 0 {
		c, err := q.GetFetchCounters(ctx, cred.RunID)
		if err != nil {
			return Admission{}, err
		}
		// The attempt stays counted: commit before refusing.
		if err := tx.Commit(ctx); err != nil {
			return Admission{}, err
		}
		return Admission{}, &AdmissionRefusedError{Reason: refusalReason(c, caps)}
	}
	id, err := q.InsertFetchReservation(ctx, store.InsertFetchReservationParams{
		RunID: cred.RunID, ClaimGeneration: cred.ClaimGeneration, Bytes: caps.MaxFileBytes,
	})
	if err != nil {
		return Admission{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Admission{}, err
	}
	return Admission{ReservationID: id, Entries: snap.Entries, MaxBytes: caps.MaxFileBytes}, nil
}

// credentialLive is Begin's credential rule: not revoked, minted under the run's CURRENT
// claim, and the run is running.
func credentialLive(c store.LockFetchCredentialByHashRow) bool {
	return !c.RevokedAt.Valid && c.ClaimGeneration == c.RunClaimGeneration && c.Status == "running"
}

// refusalReason names the cap that refused a reservation. Concurrency first: it is the
// only one that frees itself.
func refusalReason(c store.GetFetchCountersRow, caps settings.FetchCaps) string {
	switch {
	case c.Inflight >= caps.MaxConcurrentPerRun:
		return AdmissionConcurrency
	case c.Files >= caps.MaxRunFiles:
		return AdmissionRunFiles
	default:
		return AdmissionRunBytes
	}
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// validateComplete checks the attempt record's shape (everything but the reservation's own
// bounds, which need the row).
func validateComplete(r apitypes.FetcherCompleteRequest) error {
	switch r.Verdict {
	case VerdictAllowed:
		if r.Reason != "" {
			return &InvalidRequestError{"an allowed attempt carries no reason"}
		}
		if !sha256Hex.MatchString(r.SHA256) {
			return &InvalidRequestError{"an allowed attempt needs a lowercase hex sha256"}
		}
	case VerdictRefused:
		if !KnownReason(r.Reason) {
			return &InvalidRequestError{"unknown refusal reason"}
		}
		if r.SHA256 != "" {
			return &InvalidRequestError{"a refused attempt carries no sha256"}
		}
	default:
		return &InvalidRequestError{"verdict must be allowed or refused"}
	}
	if strings.TrimSpace(r.URL) == "" {
		return &InvalidRequestError{"url is required"}
	}
	if r.HTTPStatus < 0 || r.HTTPStatus > 999 {
		return &InvalidRequestError{"http_status out of range"}
	}
	if r.Bytes < 0 {
		return &InvalidRequestError{"bytes must not be negative"}
	}
	if r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
		return &InvalidRequestError{"started_at and finished_at are required"}
	}
	return nil
}

// Complete records one admitted attempt in the source log and settles its reservation.
// The run is the credential's; the reservation must be that run's, of the same claim. A
// second Complete for a reservation already logged is a no-op success, so a retried
// report is never double-counted. Any error means nothing was written (one transaction),
// and the fetcher then refuses to return content.
func (s *Service) Complete(ctx context.Context, r apitypes.FetcherCompleteRequest) error {
	if r.Credential == "" {
		return ErrCredentialInvalid
	}
	if err := validateComplete(r); err != nil {
		return err
	}
	resID, err := uuid.Parse(r.ReservationID)
	if err != nil {
		return ErrReservationNotFound
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)

	// The credential row is locked FIRST, then the reservation: the same order as Begin and
	// the claim-time mint (credential, then reservations). Locking the reservation first and
	// the credential only at ReconcileFetchCounters would deadlock against a re-claim's mint,
	// which holds the credential while it releases the prior claim's reservations.
	cred, err := q.LockFetchCredentialRunByHash(ctx, HashCredential(r.Credential))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCredentialInvalid
	}
	if err != nil {
		return err
	}
	res, err := q.LockFetchReservation(ctx, store.LockFetchReservationParams{ID: resID, RunID: cred.RunID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrReservationNotFound
	}
	if err != nil {
		return err
	}
	if res.ClaimGeneration != cred.ClaimGeneration {
		return ErrReservationNotFound
	}
	logged, err := q.RunFetchLoggedForReservation(ctx, res.ID)
	if err != nil {
		return err
	}
	if logged {
		return nil
	}
	if r.Bytes > res.Bytes {
		return &InvalidRequestError{"bytes exceed the reservation"}
	}

	rec := store.ReconcileFetchCountersParams{RunID: cred.RunID, UsedBytes: r.Bytes}
	if !res.Settled {
		rec.ReleaseBytes = res.Bytes
		rec.ReleaseInflight = 1
		if err := q.SettleFetchReservation(ctx, res.ID); err != nil {
			return err
		}
	}
	if r.Verdict == VerdictRefused {
		rec.RefundFiles = 1
	}
	if err := q.ReconcileFetchCounters(ctx, rec); err != nil {
		return err
	}
	if err := q.InsertRunFetch(ctx, store.InsertRunFetchParams{
		RunID:         cred.RunID,
		ReservationID: res.ID,
		Url:           termsafe.EscapeBounded(r.URL, maxLoggedURL),
		FinalUrl:      termsafe.EscapeBounded(r.FinalURL, maxLoggedURL),
		Verdict:       r.Verdict,
		Reason:        termsafe.EscapeBounded(r.Reason, maxLoggedReason),
		HttpStatus:    int32(r.HTTPStatus), //nolint:gosec // G115: validateComplete bounds it to [0, 999].
		ContentType:   termsafe.EscapeBounded(r.ContentType, maxLoggedContentType),
		Bytes:         r.Bytes,
		Sha256:        r.SHA256,
		StartedAt:     pgtype.Timestamptz{Time: r.StartedAt, Valid: true},
		FinishedAt:    pgtype.Timestamptz{Time: r.FinishedAt, Valid: true},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SweepStale releases every reservation older than StaleReservationAge (the
// fetch_reservations_stale sweeper pass) and reports how many it released.
func (s *Service) SweepStale(ctx context.Context) (int64, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	cutoff := pgtype.Timestamptz{Time: s.now().Add(-StaleReservationAge), Valid: true}
	// Credentials first, then reservations: the lock order Complete and the mint take.
	runs, err := q.LockCredentialsWithStaleFetchReservations(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	if len(runs) == 0 {
		return 0, nil
	}
	n, err := q.ReleaseStaleFetchReservations(ctx, store.ReleaseStaleFetchReservationsParams{Cutoff: cutoff, RunIds: runs})
	if err != nil {
		return 0, err
	}
	return n, tx.Commit(ctx)
}
