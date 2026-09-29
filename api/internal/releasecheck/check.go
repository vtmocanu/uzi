package releasecheck

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Status values recorded in a Result. They mirror agentsource's shape.
const (
	statusDisabled = "disabled"
	statusOK       = "ok"
	statusError    = "error"
)

// Store is the DB surface the release check writes. *store.Queries satisfies it. The
// check persists engine-managed remote-fact keys as one atomic group.
type Store interface {
	UpsertReleaseSettings(ctx context.Context, facts []store.UpsertAppSettingParams) error
}

// SettingsReader is the typed release-check settings surface the check reads, plus
// Invalidate so a persist is visible to the next read, and ReleaseCheckInterval so the
// interval Runner (PRD #836 M2) can read the poll cadence. *settings.Cache satisfies it.
type SettingsReader interface {
	ReleaseCheckEnabled(ctx context.Context) (bool, error)
	ReleaseCheckToken(ctx context.Context) (string, error)
	ReleaseCheckInterval(ctx context.Context) (time.Duration, error)
	Invalidate()
}

// Facts are the persisted remote-release facts one check produces (PRD #836 M1) — the
// inputs the read-time derivation (UpdateAvailable / FarBehind / Security) consumes.
type Facts struct {
	LatestTag     string
	RCTag         string
	RCName        string
	RCBody        string
	RCNotesURL    string
	RCPublishedAt string
	LatestName    string
	Body          string
	NotesURL      string
	PublishedAt   string // RFC3339
	CheckedAt     string // RFC3339, from the reconciler's clock
}

// Result summarizes one check pass. A stable success with an RC fetch failure
// returns error status with the newly persisted stable Facts and a scrubbed message.
type Result struct {
	Status  string
	Message string
	Facts   Facts
}

// Reconciler fetches the constant releases/latest endpoint and, when the master
// toggle is on, persists the remote facts to app_settings. It never derives at write
// time and never mutates anything but the settings KV.
type Reconciler struct {
	store    Store
	settings SettingsReader
	client   *http.Client
	now      func() time.Time
	logger   *slog.Logger
}

// NewReconciler builds a Reconciler with the dedicated guarded HTTP client. st is the
// store queries and set is the settings cache (both satisfy the narrow interfaces
// above); now is the clock (injectable for testable timestamps), defaulting to
// time.Now when nil.
func NewReconciler(st Store, set SettingsReader, now func() time.Time, logger *slog.Logger) *Reconciler {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{
		store:    st,
		settings: set,
		client:   newHTTPClient(),
		now:      now,
		logger:   logger,
	}
}

// CheckForUpdate runs one check pass (PRD #836 M1):
//
//   - master toggle OFF → Status "disabled", NO http call, persist NOTHING.
//   - fetch/parse error → Status "error", token-scrubbed message, persist NOTHING
//     (the last-good facts survive so the SPA keeps showing the previous release).
//   - stable success → fetch one RC page, persist stable facts and RC facts on RC
//     success (clearing RC keys when absent), then invalidate the cache. An RC
//     failure preserves prior RC keys and returns an error after stable persistence.
//
// The returned error is always nil today (every failure is recorded in the Result,
// not propagated); the signature matches the engine convention.
func (r *Reconciler) CheckForUpdate(ctx context.Context) (Result, error) {
	enabled, err := r.settings.ReleaseCheckEnabled(ctx)
	if err != nil {
		// Fail CLOSED. The master toggle is the AIR-GAP / privacy gate (PRD #836 D2:
		// off → the api never calls github.com), so a transient cache-read error must
		// NOT cause egress: treat an errored read as disabled — no HTTP call, nothing
		// persisted. The accessor defaults to true on a read error, so we cannot proceed
		// on the returned bool. Keep logging the error.
		r.logger.Error("releasecheck: read enabled", "error", err)
		return Result{Status: statusDisabled, Message: "release check enable read failed; treating as disabled"}, nil
	}
	if !enabled {
		return Result{Status: statusDisabled, Message: "release check is disabled"}, nil
	}

	// The token is OPTIONAL: a decrypt/read failure is not a reason to skip the check
	// (the target is a public repo), so log it and fall back to the unauthenticated
	// path rather than erroring.
	token, terr := r.settings.ReleaseCheckToken(ctx)
	if terr != nil {
		r.logger.Error("releasecheck: read token", "error", terr)
		token = ""
	}

	rel, ferr := fetchLatest(ctx, r.client, token)
	if ferr != nil {
		// Unreachable / non-200 / decode error: the message is already token-scrubbed.
		// Persist NOTHING so the last-good remote facts are preserved.
		return Result{Status: statusError, Message: ferr.Error()}, nil
	}

	rc, rcErr := fetchLatestRC(ctx, r.client, token)
	facts := Facts{
		LatestTag:     rel.TagName,
		LatestName:    rel.Name,
		Body:          rel.Body,
		NotesURL:      rel.HTMLURL,
		PublishedAt:   rel.PublishedAt,
		CheckedAt:     r.now().UTC().Format(time.RFC3339),
		RCTag:         rc.TagName,
		RCName:        rc.Name,
		RCBody:        rc.Body,
		RCNotesURL:    rc.HTMLURL,
		RCPublishedAt: rc.PublishedAt,
	}

	// Publish the stable facts and, on RC success, the RC facts in one transaction.
	// A failed RC fetch leaves the previous RC group untouched.
	writes := []store.UpsertAppSettingParams{
		{Key: settings.KeyReleaseLatestTag, Value: facts.LatestTag},
		{Key: settings.KeyReleaseLatestName, Value: facts.LatestName},
		{Key: settings.KeyReleaseLatestBody, Value: facts.Body},
		{Key: settings.KeyReleaseNotesURL, Value: facts.NotesURL},
		{Key: settings.KeyReleasePublishedAt, Value: facts.PublishedAt},
		{Key: settings.KeyReleaseCheckedAt, Value: facts.CheckedAt},
	}
	if rcErr == nil {
		writes = append(writes,
			store.UpsertAppSettingParams{Key: settings.KeyReleaseRCTag, Value: facts.RCTag},
			store.UpsertAppSettingParams{Key: settings.KeyReleaseRCName, Value: facts.RCName},
			store.UpsertAppSettingParams{Key: settings.KeyReleaseRCBody, Value: facts.RCBody},
			store.UpsertAppSettingParams{Key: settings.KeyReleaseRCNotesURL, Value: facts.RCNotesURL},
			store.UpsertAppSettingParams{Key: settings.KeyReleaseRCPublishedAt, Value: facts.RCPublishedAt},
		)
	}
	if err := r.store.UpsertReleaseSettings(ctx, writes); err != nil {
		r.logger.Error("releasecheck: persist remote facts", "error", err)
		return Result{Status: statusError, Facts: facts, Message: "persist remote facts failed: " + err.Error()}, nil
	}
	r.settings.Invalidate()
	if rcErr != nil {
		return Result{Status: statusError, Facts: facts, Message: "stable release updated; RC fetch failed: " + rcErr.Error()}, nil
	}
	return Result{Status: statusOK, Facts: facts}, nil
}
