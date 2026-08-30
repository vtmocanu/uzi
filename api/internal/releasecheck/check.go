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
// check persists only the six engine-managed remote-fact keys via the existing
// generic UpsertAppSetting — it adds NO new query and never touches any other table.
type Store interface {
	UpsertAppSetting(ctx context.Context, arg store.UpsertAppSettingParams) (store.AppSetting, error)
}

// SettingsReader is the typed release-check settings surface the check reads, plus
// Invalidate so a persist is visible to the next read. *settings.Cache satisfies it.
type SettingsReader interface {
	ReleaseCheckEnabled(ctx context.Context) (bool, error)
	ReleaseCheckToken(ctx context.Context) (string, error)
	Invalidate()
}

// Facts are the persisted remote-release facts one check produces (PRD #836 M1) — the
// inputs the read-time derivation (UpdateAvailable / FarBehind / Security) consumes.
type Facts struct {
	LatestTag   string
	LatestName  string
	Body        string
	NotesURL    string
	PublishedAt string // RFC3339
	CheckedAt   string // RFC3339, from the reconciler's clock
}

// Result summarizes one check pass (PRD #836 M1). Status is "disabled", "ok", or
// "error". Facts is populated only on "ok"; on "disabled"/"error" nothing is
// persisted and Message carries the (token-scrubbed) reason.
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
//   - success → persist the six facts via UpsertAppSetting, Invalidate the cache, and
//     return Status "ok" with the parsed Facts.
//
// The returned error is always nil today (every failure is recorded in the Result,
// not propagated); the signature matches the engine convention.
func (r *Reconciler) CheckForUpdate(ctx context.Context) (Result, error) {
	enabled, err := r.settings.ReleaseCheckEnabled(ctx)
	if err != nil {
		r.logger.Error("releasecheck: read enabled", "error", err)
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

	facts := Facts{
		LatestTag:   rel.TagName,
		LatestName:  rel.Name,
		Body:        rel.Body,
		NotesURL:    rel.HTMLURL,
		PublishedAt: rel.PublishedAt,
		CheckedAt:   r.now().UTC().Format(time.RFC3339),
	}

	// Persist the six remote facts through the existing generic query. Best-effort: a
	// write failure is logged, never fatal (worst case is a stale panel).
	writes := []store.UpsertAppSettingParams{
		{Key: settings.KeyReleaseLatestTag, Value: facts.LatestTag},
		{Key: settings.KeyReleaseLatestName, Value: facts.LatestName},
		{Key: settings.KeyReleaseLatestBody, Value: facts.Body},
		{Key: settings.KeyReleaseNotesURL, Value: facts.NotesURL},
		{Key: settings.KeyReleasePublishedAt, Value: facts.PublishedAt},
		{Key: settings.KeyReleaseCheckedAt, Value: facts.CheckedAt},
	}
	for _, w := range writes {
		if _, err := r.store.UpsertAppSetting(ctx, w); err != nil {
			r.logger.Error("releasecheck: persist remote facts", "key", w.Key, "error", err)
		}
	}
	r.settings.Invalidate()

	return Result{Status: statusOK, Facts: facts}, nil
}
