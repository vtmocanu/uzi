package agentsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Store is the DB surface the reconcile job reads and writes. *store.Queries
// satisfies it. The job reads the current templates (to compute the diff) and the
// prior staged snapshot (for SHA idempotency), and writes the new snapshot plus the
// engine-managed last-sync status keys. It NEVER writes agent_templates — applying is
// M4, gated on an admin approval.
type Store interface {
	ListAgentTemplates(ctx context.Context) ([]store.AgentTemplate, error)
	GetAgentSourceStaged(ctx context.Context) (store.AgentSourceStaged, error)
	UpsertAgentSourceStaged(ctx context.Context, arg store.UpsertAgentSourceStagedParams) (store.AgentSourceStaged, error)
	UpsertAppSetting(ctx context.Context, arg store.UpsertAppSettingParams) (store.AppSetting, error)
}

// SettingsReader is the typed agent-source settings surface the reconcile reads,
// plus Invalidate so a status write is visible to the next read. *settings.Cache
// satisfies it.
type SettingsReader interface {
	AgentSourceEnabled(ctx context.Context) (bool, error)
	AgentSourceRepoURL(ctx context.Context) (string, error)
	AgentSourceRef(ctx context.Context) (string, error)
	// AgentSourceFolder is the repo-relative subfolder to read role files from (PRD
	// #702 M1); empty/unset resolves to the default ".claude/agents".
	AgentSourceFolder(ctx context.Context) (string, error)
	AgentSourceInterval(ctx context.Context) (time.Duration, error)
	AgentSourceCredential(ctx context.Context) (string, error)
	// AgentSourceLastAppliedSHA is the fetched SHA of the snapshot last applied by M4
	// (empty when nothing has been applied). Apply reads it to short-circuit a repeat
	// apply of an already-applied snapshot.
	AgentSourceLastAppliedSHA(ctx context.Context) (string, error)
	Invalidate()
}

// TxBeginner opens a transaction for the M4 apply, which mutates agent_templates in
// a single atomic unit (a real DB error rolls the WHOLE apply back). *pgxpool.Pool
// satisfies it. It is nil in the M3 reconcile-only tests, which never call Apply.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Allowlist re-checks the stored URL against the SSRF allowlist at the clone seam
// (TOCTOU — the env allowlist can shrink after the URL was stored). config.Config
// satisfies it via AgentSourceBaseURLAllowed.
type Allowlist interface {
	AgentSourceBaseURLAllowed(raw string) bool
}

// FetchFunc is the clone-and-read seam; FetchRoleFiles is the production impl. It is
// a field on the Reconciler so a test can inject a stub source without a network or a
// non-https URL (which the allowlist re-check would reject).
type FetchFunc func(ctx context.Context, opts CloneOptions) (sha string, files []SourceFile, err error)

// ListRefsFunc is the ls-remote seam; ListRemoteRefs is the production impl. Like
// FetchFunc, it is a field on the Reconciler so an update-check test can inject a fake
// ref advertisement without a network or a real git remote (PRD #702 M4).
type ListRefsFunc func(ctx context.Context, opts CloneOptions) (RemoteRefs, error)

// Status values recorded in agent_source_last_sync_status.
const (
	statusOK    = "ok"
	statusError = "error"
)

// DiffAction is the per-name classification of what M4 will do when it applies the
// staged snapshot. It is a PREVIEW; nothing here mutates agent_templates.
const (
	DiffAdd       = "add"       // synced-only new name → M4 inserts a global row
	DiffOverride  = "override"  // matches a builtin (or a changed synced row) → M4 updates it to origin='synced'
	DiffConflict  = "conflict"  // matches an existing admin global row (origin != synced) → M4 stages-error/skips
	DiffUnchanged = "unchanged" // already origin='synced' with identical content
	DiffRemove    = "remove"    // previously-synced name absent from the fetched set → M4 deletes/resets
)

// StagedRole is one entry in the staged snapshot's roles array: a parsed-OK role
// (OK=true, full definition, plus any non-fatal clamp notes) or a skipped/failed one
// (OK=false, Reason carries the NoteReason).
type StagedRole struct {
	Name        string   `json:"name"`
	OK          bool     `json:"ok"`
	Reason      string   `json:"reason,omitempty"`
	Description string   `json:"description,omitempty"`
	Model       string   `json:"model,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	PromptBody  string   `json:"prompt_body,omitempty"`
	Notes       []string `json:"notes,omitempty"`
}

// DiffEntry is one per-name classification in the staged snapshot's diff array.
type DiffEntry struct {
	Name   string `json:"name"`
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
}

// Result summarizes one reconcile pass for the caller (M4 status / logs). Status is
// "disabled", "ok", or "error". Restaged is false when an idempotent same-SHA run
// left the snapshot untouched.
type Result struct {
	Status   string
	SHA      string
	Staged   int
	Changed  int
	Failed   int
	Restaged bool
	Message  string
}

// Reconciler fetches the configured source, parses it with the shared M3a parser,
// computes a preview diff against the current templates, and STAGES a snapshot for
// M4 to apply. It never writes agent_templates.
type Reconciler struct {
	store    Store
	settings SettingsReader
	cfg      Allowlist
	fetch    FetchFunc
	// lsRemote is the ls-remote seam CheckForUpdate (PRD #702 M4) uses to read the
	// source's advertised refs without a pack fetch; defaulted to ListRemoteRefs and
	// injectable in tests.
	lsRemote ListRefsFunc
	// db opens the transaction Apply (M4) runs its provenance-aware upsert in. nil in
	// the reconcile-only tests, which never call Apply; Apply guards on it.
	db     TxBeginner
	now    func() time.Time
	logger *slog.Logger
}

// NewReconciler builds a Reconciler with the production FetchRoleFiles seam. db is the
// transaction beginner Apply uses (the pgx pool in production); it may be nil for
// callers that only Reconcile (stage) and never Apply.
func NewReconciler(st Store, set SettingsReader, cfg Allowlist, db TxBeginner, logger *slog.Logger) *Reconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{
		store:    st,
		settings: set,
		cfg:      cfg,
		fetch:    FetchRoleFiles,
		lsRemote: ListRemoteRefs,
		db:       db,
		now:      time.Now,
		logger:   logger,
	}
}

// Reconcile runs one pass: fetch → parse → diff → stage. It is idempotent (a repeat
// on the same SHA does not re-stage) and never fatal — an unreachable/misconfigured
// source records an error status and leaves the last-good snapshot in place. The
// returned error is always nil today (every failure is recorded, not propagated); the
// signature keeps the door open and matches the engine convention.
func (r *Reconciler) Reconcile(ctx context.Context) (Result, error) {
	enabled, err := r.settings.AgentSourceEnabled(ctx)
	if err != nil {
		r.logger.Error("agentsource: read enabled", "error", err)
	}
	rawURL, _ := r.settings.AgentSourceRepoURL(ctx)
	url := strings.TrimSpace(rawURL)
	if !enabled || url == "" {
		return Result{Status: "disabled", Message: "agent source disabled or unconfigured"}, nil
	}

	// TOCTOU re-check: the env allowlist can have shrunk since the URL was stored, so
	// re-validate at the clone seam. On a miss, record an error and DO NOT clone or
	// touch the staged snapshot.
	if !r.cfg.AgentSourceBaseURLAllowed(url) {
		msg := "source url is not on the AGENT_SOURCE_ALLOWED_BASE_URLS allowlist"
		r.recordStatus(ctx, statusError, "", msg, 0, 0, 0)
		return Result{Status: statusError, Message: msg}, nil
	}

	ref, _ := r.settings.AgentSourceRef(ctx)
	folder, _ := r.settings.AgentSourceFolder(ctx)
	token, terr := r.settings.AgentSourceCredential(ctx)
	if terr != nil {
		// A decrypt failure is a misconfiguration, not a reason to clone anonymously
		// against a private repo. Record and bail, keeping last-good.
		msg := "could not read the source credential"
		r.recordStatus(ctx, statusError, "", msg, 0, 0, 0)
		return Result{Status: statusError, Message: msg}, nil
	}

	sha, files, ferr := r.fetch(ctx, CloneOptions{
		CloneURL: url,
		Ref:      strings.TrimSpace(ref),
		Dir:      strings.TrimSpace(folder),
		Token:    token,
		// Thread the SSRF allowlist into the clone so a redirect TARGET is re-checked
		// (FINDING 2): an allowlisted host that answers 302 to an internal/off-allowlist
		// host must not be dialed. The clone builds a per-operation http.Client whose
		// CheckRedirect calls this predicate.
		RedirectAllowed: r.cfg.AgentSourceBaseURLAllowed,
	})
	if ferr != nil {
		// Unreachable / timeout / auth / bad-ref: record the PAT-scrubbed message and
		// LEAVE agent_source_staged untouched (keep last-good). Never fatal at boot.
		msg := ferr.Error()
		r.recordStatus(ctx, statusError, "", msg, 0, 0, 0)
		return Result{Status: statusError, Message: msg}, nil
	}

	// Idempotency: if the fetched SHA equals the already-staged SHA, do not re-stage.
	if prior, perr := r.store.GetAgentSourceStaged(ctx); perr == nil {
		if prior.FetchedSha == sha {
			// Carry the CURRENTLY-STAGED snapshot's counts rather than zeroing them: an
			// unchanged (same-SHA) tick is the steady state, so writing {0,0,0} here
			// would make the counts panel read zero almost always. The counts must
			// describe the set that is actually staged, which is the prior snapshot.
			staged, changed, failed := countsFromStaged(prior)
			r.recordStatus(ctx, statusOK, sha, "", staged, changed, failed)
			return Result{
				Status:   statusOK,
				SHA:      sha,
				Staged:   staged,
				Changed:  changed,
				Failed:   failed,
				Restaged: false,
				Message:  "unchanged",
			}, nil
		}
	} else if !errors.Is(perr, pgx.ErrNoRows) {
		// A read error is not fatal — proceed to (re)stage rather than skip a real update.
		r.logger.Warn("agentsource: read prior staged snapshot", "error", perr)
	}

	set := ParseSet(files)
	stagedRoles := buildStagedRoles(set)

	current, lerr := r.store.ListAgentTemplates(ctx)
	if lerr != nil {
		msg := "could not read current templates for diff"
		r.recordStatus(ctx, statusError, "", msg, 0, 0, 0)
		return Result{Status: statusError, Message: msg}, nil
	}
	diff := computeDiff(set.Roles, current)

	rolesJSON, err := json.Marshal(stagedRoles)
	if err != nil {
		r.recordStatus(ctx, statusError, "", "marshal roles", 0, 0, 0)
		return Result{Status: statusError, Message: "marshal roles"}, nil
	}
	diffJSON, err := json.Marshal(diff)
	if err != nil {
		r.recordStatus(ctx, statusError, "", "marshal diff", 0, 0, 0)
		return Result{Status: statusError, Message: "marshal diff"}, nil
	}

	staged := len(set.Roles)
	failed := countFailed(stagedRoles)
	changed := countChanged(diff)

	if _, uerr := r.store.UpsertAgentSourceStaged(ctx, store.UpsertAgentSourceStagedParams{
		FetchedAt:  pgtype.Timestamptz{Time: r.now().UTC(), Valid: true},
		FetchedSha: sha,
		SourceUrl:  url,
		SourceRef:  strings.TrimSpace(ref),
		Roles:      rolesJSON,
		Diff:       diffJSON,
	}); uerr != nil {
		msg := "could not persist the staged snapshot"
		r.recordStatus(ctx, statusError, "", msg, 0, 0, 0)
		return Result{Status: statusError, Message: msg}, nil
	}

	r.recordStatus(ctx, statusOK, sha, "", staged, changed, failed)
	return Result{
		Status:   statusOK,
		SHA:      sha,
		Staged:   staged,
		Changed:  changed,
		Failed:   failed,
		Restaged: true,
		Message:  fmt.Sprintf("staged %d role(s), %d change(s), %d failure(s)", staged, changed, failed),
	}, nil
}

// buildStagedRoles turns a ParseSet result into the roles array: the kept roles
// (with any non-fatal clamp notes) followed by the skipped/failed ones by reason.
func buildStagedRoles(set SetResult) []StagedRole {
	// Clamp notes are keyed by role name so a kept role carries its own notes.
	clampNotes := map[string][]string{}
	var failures []StagedRole
	for _, n := range set.Notes {
		switch n.Reason {
		case NoteToolsFiltered, NoteModelIgnored:
			clampNotes[n.Name] = append(clampNotes[n.Name], string(n.Reason))
		default:
			// invalid / tools_all_denied / too_large / duplicate / over_limit
			failures = append(failures, StagedRole{Name: n.Name, OK: false, Reason: string(n.Reason)})
		}
	}

	out := make([]StagedRole, 0, len(set.Roles)+len(failures))
	for _, role := range set.Roles {
		out = append(out, StagedRole{
			Name:        role.Name,
			OK:          true,
			Description: role.Description,
			Model:       role.Model,
			Tools:       role.Tools,
			PromptBody:  role.PromptBody,
			Notes:       clampNotes[role.Name],
		})
	}
	out = append(out, failures...)
	return out
}

// computeDiff classifies each parsed role against the current templates and emits the
// removal previews. The comparison basis is the SHARED namespace (scope != 'user'):
// a synced role can match a builtin row (override case) or a global row. A synced-only
// role that de-provisions (a previously-synced name now absent) becomes a remove.
func computeDiff(parsed []agenttmpl.Definition, current []store.AgentTemplate) []DiffEntry {
	shared := map[string]store.AgentTemplate{}
	for _, t := range current {
		if t.Scope == "user" {
			continue
		}
		shared[t.Name] = t
	}
	parsedNames := map[string]struct{}{}
	for _, d := range parsed {
		parsedNames[d.Name] = struct{}{}
	}

	out := make([]DiffEntry, 0, len(parsed))
	for _, d := range parsed {
		row, ok := shared[d.Name]
		switch {
		case !ok:
			out = append(out, DiffEntry{Name: d.Name, Action: DiffAdd, Detail: "new synced-only role"})
		case row.Scope == "builtin":
			if originOf(row) == "synced" && agenttmpl.SameContent(rowToDefinition(row), d) {
				out = append(out, DiffEntry{Name: d.Name, Action: DiffUnchanged})
			} else {
				out = append(out, DiffEntry{Name: d.Name, Action: DiffOverride, Detail: "overrides a builtin"})
			}
		case row.Scope == "global":
			if originOf(row) == "synced" {
				if agenttmpl.SameContent(rowToDefinition(row), d) {
					out = append(out, DiffEntry{Name: d.Name, Action: DiffUnchanged})
				} else {
					out = append(out, DiffEntry{Name: d.Name, Action: DiffOverride, Detail: "updates a synced global role"})
				}
			} else {
				out = append(out, DiffEntry{Name: d.Name, Action: DiffConflict, Detail: "collides with an admin global template"})
			}
		}
	}

	// Removals: any currently-synced shared row whose name left the fetched set.
	for _, t := range current {
		if originOf(t) != "synced" {
			continue
		}
		if _, present := parsedNames[t.Name]; present {
			continue
		}
		detail := "synced global role removed from source"
		if t.Scope == "builtin" {
			detail = "overridden builtin removed from source (reset to embedded)"
		}
		out = append(out, DiffEntry{Name: t.Name, Action: DiffRemove, Detail: detail})
	}
	return out
}

// originOf returns the row's origin string ("" when NULL).
func originOf(t store.AgentTemplate) string {
	if t.Origin.Valid {
		return t.Origin.String
	}
	return ""
}

// rowToDefinition maps a stored row onto the comparison type, decoding the jsonb
// tools column — the agentsource-local twin of the handler's templateToDefinition
// (that one is not importable here without a cycle).
func rowToDefinition(t store.AgentTemplate) agenttmpl.Definition {
	d := agenttmpl.Definition{
		Name:        t.Name,
		Description: t.Description,
		Tools:       decodeTools(t.Tools),
		PromptBody:  t.PromptBody,
	}
	if t.Model.Valid {
		d.Model = t.Model.String
	}
	return d
}

// decodeTools decodes the jsonb tools column ([]string), returning nil on an empty
// or malformed value (inherit-all), mirroring the handler's decodeTools semantics.
func decodeTools(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// countsFromStaged recomputes the {staged, changed, failed} triple from an already-
// persisted snapshot row so an unchanged (same-SHA) tick can report the counts of the
// set that is actually staged rather than zeros. A malformed/empty jsonb column decodes
// to an empty slice, yielding zeros — the safe degrade for a status panel.
func countsFromStaged(row store.AgentSourceStaged) (staged, changed, failed int) {
	var roles []StagedRole
	if len(row.Roles) > 0 {
		_ = json.Unmarshal(row.Roles, &roles)
	}
	var diff []DiffEntry
	if len(row.Diff) > 0 {
		_ = json.Unmarshal(row.Diff, &diff)
	}
	staged = 0
	for _, r := range roles {
		if r.OK {
			staged++
		}
	}
	return staged, countChanged(diff), countFailed(roles)
}

func countFailed(roles []StagedRole) int {
	n := 0
	for _, r := range roles {
		if !r.OK {
			n++
		}
	}
	return n
}

func countChanged(diff []DiffEntry) int {
	n := 0
	for _, d := range diff {
		if d.Action != DiffUnchanged {
			n++
		}
	}
	return n
}

// recordStatus writes the engine-managed last-sync status keys and invalidates the
// settings cache so the next read is fresh. Best-effort: a write failure is logged,
// never fatal (the worst case is a stale status panel). The error message is stored
// as-is — it was already PAT-scrubbed by FetchRoleFiles; recordStatus never receives
// raw token material.
func (r *Reconciler) recordStatus(ctx context.Context, status, sha, errMsg string, staged, changed, failed int) {
	counts, _ := json.Marshal(map[string]int{"staged": staged, "changed": changed, "failed": failed})
	writes := []store.UpsertAppSettingParams{
		{Key: settings.KeyAgentSourceLastSyncAt, Value: r.now().UTC().Format(time.RFC3339)},
		{Key: settings.KeyAgentSourceLastSyncStatus, Value: status},
		{Key: settings.KeyAgentSourceLastSyncSHA, Value: sha},
		{Key: settings.KeyAgentSourceLastSyncError, Value: errMsg},
		{Key: settings.KeyAgentSourceLastSyncCounts, Value: string(counts)},
	}
	for _, w := range writes {
		if _, err := r.store.UpsertAppSetting(ctx, w); err != nil {
			r.logger.Error("agentsource: persist last-sync status", "key", w.Key, "error", err)
		}
	}
	r.settings.Invalidate()
}

// Runner is the interval trigger. It sleeps AgentSourceInterval() and, when enabled,
// runs one Reconcile — recovering from any panic and logging (never crashing the
// process) on error. It is boot-safe: main.go starts it as a non-blocking background
// goroutine BEFORE the listener starts, and the first tick only fires after one
// interval, so a first reconcile against an unreachable source never delays boot.
type Runner struct {
	rec      *Reconciler
	settings SettingsReader
	logger   *slog.Logger
}

// NewRunner builds the interval trigger around a Reconciler.
func NewRunner(rec *Reconciler, set SettingsReader, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{rec: rec, settings: set, logger: logger}
}

// Start loops until ctx is cancelled: sleep the configured interval, then reconcile
// if enabled. Disabled is the default — the loop just idles and does nothing. It
// never panics the process (each tick's Reconcile is panic-recovered).
func (rn *Runner) Start(ctx context.Context) {
	for {
		interval, _ := rn.settings.AgentSourceInterval(ctx)
		if interval <= 0 {
			interval = time.Hour
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		rn.tick(ctx)
	}
}

// tick runs one reconcile with a panic guard so a bug in the parse/diff path can
// never take down the api process.
func (rn *Runner) tick(ctx context.Context) {
	defer func() {
		if p := recover(); p != nil {
			rn.logger.Error("agentsource: reconcile panic recovered", "panic", p)
		}
	}()
	enabled, _ := rn.settings.AgentSourceEnabled(ctx)
	if !enabled {
		return
	}
	if _, err := rn.rec.Reconcile(ctx); err != nil {
		rn.logger.Error("agentsource: reconcile", "error", err)
	}
}
