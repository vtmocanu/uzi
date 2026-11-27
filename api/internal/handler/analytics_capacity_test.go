package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Issue #4731: Capacity handler tests that need no database. The snapshot flow itself (savepoints,
// the historical statement, vault contention through the router) is in
// analytics_capacity_livedb_test.go.

func capacityHandler(poll time.Duration) *Handler {
	return &Handler{cfg: config.Config{UsagePollInterval: poll, CodexUsagePollInterval: poll}}
}

func capacityReq(t *testing.T, query string, admin bool) analyticsRequest {
	t.Helper()
	return mustParseAnalytics(t, query, admin, analytics.SectionCapacity, uuid.New())
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func TestCapacityEffectiveScopeNamesCarriedSelectors(t *testing.T) {
	repo := uuid.NewString()
	cases := []struct {
		query string
		want  []string
	}{
		{"", []string{}},
		{"harness=claude&window=7d", []string{}},
		{"repo=" + repo, []string{"repo"}},
		{"kind=delivery", []string{"kind"}},
		{"kind=job&repo=" + repo, []string{"repo", "kind"}},
	}
	for _, tc := range cases {
		req := capacityReq(t, tc.query, false)
		got := capacityEffectiveScope(req, capacityProviderCodex)
		if got.UnattributedSelectors == nil || !slices.Equal(got.UnattributedSelectors, tc.want) {
			t.Fatalf("%q: unattributed_selectors = %#v, want %v", tc.query, got.UnattributedSelectors, tc.want)
		}
		if (len(tc.want) > 0) != (got.Reason != nil) || (got.Reason != nil && *got.Reason != "shared_quota_not_attributable") {
			t.Fatalf("%q: reason = %v", tc.query, got.Reason)
		}
		if got.User != req.scopeUser.String() || got.Provider != "codex" {
			t.Fatalf("%q: scope = %+v", tc.query, got)
		}
	}
	if got := capacityEffectiveScope(capacityReq(t, "", true), "anthropic"); got.User != "all" {
		t.Fatalf("admin all-users scope user = %q", got.User)
	}
}

func TestCapacityProviderSelection(t *testing.T) {
	for harness, want := range map[string][2]bool{"": {true, true}, "harness=claude": {true, false}, "harness=codex": {false, true}} {
		req := capacityReq(t, harness, false)
		if req.providerSelected("anthropic") != want[0] || req.providerSelected("codex") != want[1] {
			t.Fatalf("%q selects anthropic=%v codex=%v, want %v", harness, req.providerSelected("anthropic"), req.providerSelected("codex"), want)
		}
	}
}

func TestMapAnalyticsCapacityEmptyAllTimeAndMidnight(t *testing.T) {
	// Empty all-time scope: no buckets, zero-duration range; every slice still non-null.
	now := time.Date(2031, 6, 15, 12, 0, 0, 0, time.UTC)
	empty, err := analytics.ComputeBounds(now, time.UTC, analytics.WindowAll, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := capacityReq(t, "window=all", false)
	zr := store.AnalyticsReasonZeroDurationRange
	dto := mapAnalyticsCapacity(req, empty, store.AnalyticsCapacityDoc{
		Occupancy: store.AnalyticsCapacityOccupancy{Window: store.AnalyticsCapacityOccupancyCell{Reason: &zr}},
	})
	body, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"buckets", "rolling", "not_tracked"} {
		if string(raw[k]) == "null" {
			t.Fatalf("%s serialized as null", k)
		}
	}
	if string(raw["buckets"]) != "[]" || dto.Occupancy.Window.AverageConcurrentRuns != nil || *dto.Occupancy.Window.Reason != "zero_duration_range" {
		t.Fatalf("empty all-time: buckets %s window %+v", raw["buckets"], dto.Occupancy.Window)
	}
	if dto.Occupancy.Buckets == nil || dto.UsageWait.Buckets == nil || dto.Occupancy.Limitations == nil || dto.Filters.Kinds == nil {
		t.Fatal("a capacity slice is nil, which would serialize as JSON null")
	}

	// Local midnight now: the final bucket is the zero-duration [now, now) one and keeps a reason.
	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	midnight := time.Date(2031, 6, 15, 0, 0, 0, 0, loc).UTC()
	b, err := analytics.ComputeBounds(midnight, loc, analytics.Window7d, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := analytics.ValidateBuckets(b); err != nil {
		t.Fatalf("bucket guard rejects the midnight calendar: %v", err)
	}
	last := len(b.Buckets) - 1
	zb := store.AnalyticsReasonZeroDurationBucket
	doc := store.AnalyticsCapacityDoc{}
	for i := range b.Buckets {
		cell := store.AnalyticsCapacityOccupancyCell{Index: i, DurationSeconds: 86400}
		if i == last {
			cell.DurationSeconds, cell.Reason = 0, &zb
		}
		doc.Occupancy.Buckets = append(doc.Occupancy.Buckets, cell)
		doc.UsageWait.Buckets = append(doc.UsageWait.Buckets, store.AnalyticsCapacityUsageWaitCell{Index: i})
	}
	mid := mapAnalyticsCapacity(capacityReq(t, "window=7d&tz=Europe/Bucharest", false), b, doc)
	if len(mid.Occupancy.Buckets) != len(b.Buckets) || mid.Occupancy.Buckets[last].Index != last {
		t.Fatalf("buckets = %d, want %d with index %d last", len(mid.Occupancy.Buckets), len(b.Buckets), last)
	}
	if got := mid.Occupancy.Buckets[last]; got.AverageConcurrentRuns != nil || got.Reason == nil || *got.Reason != "zero_duration_bucket" {
		t.Fatalf("midnight bucket = %+v", got)
	}
}

func TestMapAnalyticsCapacityFiltersAndExcludedKind(t *testing.T) {
	now := time.Date(2031, 6, 15, 12, 0, 0, 0, time.UTC)
	b, err := analytics.ComputeBounds(now, time.UTC, analytics.Window7d, nil)
	if err != nil {
		t.Fatal(err)
	}
	repo := uuid.New()
	dto := mapAnalyticsCapacity(capacityReq(t, "repo="+repo.String()+"&harness=codex&kind=chat", false), b, store.AnalyticsCapacityDoc{})
	if dto.ExcludedKindSelected == nil || *dto.ExcludedKindSelected != "chat" {
		t.Fatalf("excluded_kind_selected = %v, want chat", dto.ExcludedKindSelected)
	}
	f := dto.Filters
	if f.Repo == nil || *f.Repo != repo.String() || f.Harness == nil || *f.Harness != "codex" || f.Kinds == nil || len(f.Kinds) != 0 {
		t.Fatalf("filters = %+v", f)
	}
	plain := mapAnalyticsCapacity(capacityReq(t, "", false), b, store.AnalyticsCapacityDoc{})
	if plain.ExcludedKindSelected != nil || plain.Filters.Repo != nil || plain.Filters.Harness != nil || slices.Contains(plain.Filters.Kinds, "chat") || len(plain.Filters.Kinds) == 0 {
		t.Fatalf("unfiltered = excluded %v filters %+v", plain.ExcludedKindSelected, plain.Filters)
	}
	if plain.SelectedUser == "" || plain.Occupancy.TimeBasis != "interval_overlap" || plain.Occupancy.Unit != "average_concurrent_runs" ||
		plain.UsageWait.TimeBasis != "created_at" || plain.UsageWait.Cohort != "creation" || plain.UsageWait.Basis != "limit_wait_count" {
		t.Fatalf("labels = %+v / %+v", plain.Occupancy, plain.UsageWait)
	}
	if len(plain.NotTracked) != 4 {
		t.Fatalf("not_tracked = %+v", plain.NotTracked)
	}
	for _, n := range plain.NotTracked {
		if n.Value != nil || n.Reason != "not_tracked" {
			t.Fatalf("not_tracked entry %+v", n)
		}
	}
}

// Staleness is judged at the request's captured clock, not at time.Now: the same stored readings
// are fresh at one captured instant and stale three poll intervals later, for both providers.
func TestCapacityGaugeStalenessUsesCapturedClock(t *testing.T) {
	const poll = 5 * time.Minute
	h := capacityHandler(poll)
	synced := time.Date(2031, 6, 15, 12, 0, 0, 0, time.UTC)
	req := capacityReq(t, "", false)
	secret := uuid.New()

	anthropic := func(now time.Time) apitypes.RateLimitDTO {
		id := h.anthropicIdentity(secret, "tok", true, nil, nil, pgtype.Int2{Int16: 10, Valid: true}, pgtype.Timestamptz{},
			pgtype.Int2{Int16: 20, Valid: true}, pgtype.Timestamptz{}, pgtype.Text{String: "usage_endpoint", Valid: true}, ts(synced), now)
		return id.Limits
	}
	if l := anthropic(synced.Add(poll)); l.Status != "ok" || l.Stale == nil || *l.Stale {
		t.Fatalf("anthropic at +1 interval = %+v, want fresh", l)
	}
	if l := anthropic(synced.Add(3*poll + time.Second)); l.Stale == nil || !*l.Stale {
		t.Fatalf("anthropic at +3 intervals = %+v, want stale", l)
	}

	set := capacityCodexSet{selected: true, rows: []capacityCodexRow{{userID: req.scopeUser, accountID: uuid.New(), aliases: []string{"a"}, lastSuccessAt: ts(synced), lastAttemptAt: ts(synced)}}}
	codex := func(now time.Time) string {
		g := h.codexGauge(req, set, map[uuid.UUID]bool{req.scopeUser: false}, true, now)
		if g.Status != "available" || len(g.Identities) != 1 {
			t.Fatalf("codex gauge = %+v", g)
		}
		return g.Identities[0].Status
	}
	if got := codex(synced.Add(poll)); got != usagereserve.CodexMeterFresh {
		t.Fatalf("codex at +1 interval = %q, want fresh", got)
	}
	if got := codex(synced.Add(3*poll + time.Second)); got != usagereserve.CodexMeterStale {
		t.Fatalf("codex at +3 intervals = %q, want stale", got)
	}
}

func TestCapacityAnthropicMissedReadingIsUnavailable(t *testing.T) {
	h := capacityHandler(5 * time.Minute)
	id := h.anthropicIdentity(uuid.New(), "never-polled", false, nil, nil, pgtype.Int2{}, pgtype.Timestamptz{}, pgtype.Int2{}, pgtype.Timestamptz{}, pgtype.Text{}, pgtype.Timestamptz{}, time.Now())
	if id.Limits.Status != "unavailable" || id.Limits.SyncedAt != "" || id.Limits.Stale != nil || id.Limits.SevenDay != nil {
		t.Fatalf("missed reading = %+v, want status unavailable only", id.Limits)
	}
}

func TestCapacityGaugeStatesAndSetState(t *testing.T) {
	h := capacityHandler(time.Minute)
	req := capacityReq(t, "harness=claude", false)

	notSel := h.codexGauge(req, capacityCodexSet{selected: false}, nil, true, time.Now())
	if notSel.Status != "not_selected" || notSel.Reason == nil || *notSel.Reason != "not_selected" || notSel.SetState != nil || notSel.Identities == nil || notSel.VaultStateBasis != "live_observation" {
		t.Fatalf("not selected = %+v", notSel)
	}
	failed := h.codexGauge(req, capacityCodexSet{selected: true, failed: true}, nil, true, time.Now())
	if failed.Status != "unavailable" || *failed.Reason != "gauge_unavailable" || failed.SetState != nil {
		t.Fatalf("failed = %+v", failed)
	}
	vaultDown := h.codexGauge(req, capacityCodexSet{selected: true}, nil, false, time.Now())
	if vaultDown.Status != "unavailable" || *vaultDown.Reason != "vault_status_unavailable" || vaultDown.SetState != nil {
		t.Fatalf("vault down = %+v", vaultDown)
	}
	empty := h.codexGauge(req, capacityCodexSet{selected: true}, map[uuid.UUID]bool{}, true, time.Now())
	if empty.Status != "available" || empty.Reason != nil || *empty.SetState != "empty" || empty.Truncated || empty.Identities == nil {
		t.Fatalf("empty = %+v", empty)
	}
	row := capacityCodexRow{accountID: uuid.New(), aliases: []string{"x"}}
	complete := h.codexGauge(req, capacityCodexSet{selected: true, rows: []capacityCodexRow{row}}, map[uuid.UUID]bool{}, true, time.Now())
	if *complete.SetState != "complete" || complete.Identities[0].UserID != nil || complete.Identities[0].Email != nil {
		t.Fatalf("complete = %+v", complete)
	}
	trunc := h.codexGauge(req, capacityCodexSet{selected: true, rows: []capacityCodexRow{row}, truncated: true}, map[uuid.UUID]bool{}, true, time.Now())
	if *trunc.SetState != "truncated" || !trunc.Truncated {
		t.Fatalf("truncated = %+v", trunc)
	}
	all := h.codexGauge(capacityReq(t, "", true), capacityCodexSet{selected: true, rows: []capacityCodexRow{{userID: uuid.New(), email: "a@b", accountID: uuid.New()}}}, map[uuid.UUID]bool{}, true, time.Now())
	if all.Identities[0].UserID == nil || *all.Identities[0].Email != "a@b" {
		t.Fatalf("all-users identity lacks user fields: %+v", all.Identities[0])
	}
}

func TestCapGaugeKeepsTheOrderedPrefix(t *testing.T) {
	rows := make([]int, 201)
	for i := range rows {
		rows[i] = i
	}
	got, trunc := capGauge(rows)
	if !trunc || len(got) != 200 || got[199] != 199 {
		t.Fatalf("capGauge(201) = %d rows, truncated %v", len(got), trunc)
	}
	if got, trunc = capGauge(rows[:200]); trunc || len(got) != 200 {
		t.Fatalf("capGauge(200) = %d rows, truncated %v", len(got), trunc)
	}
}

// Owner route: the caller's own state is read with RequiresUnlock, as the self Codex endpoint does.
func TestCapacityOwnerVaultLockedMapsToStatus(t *testing.T) {
	owner := uuid.New()
	m := newMemVaultQ() // an unknown user is protected, with no cached key: locked
	req := capacityReq(t, "", false)
	req.scopeUser = owner
	row := capacityCodexRow{userID: owner, accountID: uuid.New(), aliases: []string{"a"}}
	for _, tc := range []struct {
		name string
		poll time.Duration
		want string
	}{
		{"polling on", time.Minute, usagereserve.CodexMeterVaultLocked},
		{"polling off", 0, usagereserve.CodexMeterPollingDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := capacityHandler(tc.poll)
			h.vault = newHandlerMemoryVault(newHandlerTestBox(t), m)
			locked, observed := h.observeCodexVault(context.Background(), &analyticsDeps{}, req, []capacityCodexRow{row})
			if !observed || !locked[owner] {
				t.Fatalf("observe = %v, %v, want locked", locked, observed)
			}
			g := h.codexGauge(req, capacityCodexSet{selected: true, rows: []capacityCodexRow{row}}, locked, observed, time.Now())
			if g.Status != "available" || g.Identities[0].Status != tc.want {
				t.Fatalf("status = %q / %q, want available / %q", g.Status, g.Identities[0].Status, tc.want)
			}
		})
	}

	// No identities, no vault read: an empty set never consults the vault.
	counter := &countingStateReader{memVaultQ: newMemVaultQ()}
	h := capacityHandler(time.Minute)
	h.vault = vault.New(newHandlerTestBox(t), counter, counter.transaction)
	if locked, observed := h.observeCodexVault(context.Background(), &analyticsDeps{}, req, nil); !observed || len(locked) != 0 {
		t.Fatalf("empty set observe = %v, %v", locked, observed)
	}
	if n := counter.reads.Load(); n != 0 {
		t.Fatalf("empty set read the vault %d times, want 0", n)
	}
	// Control: the same double does see the read when an identity is selected.
	h.observeCodexVault(context.Background(), &analyticsDeps{}, req, []capacityCodexRow{row})
	if counter.reads.Load() == 0 {
		t.Fatal("counting double never observed a read for a selected identity")
	}
}

// countingStateReader counts vault state reads.
type countingStateReader struct {
	*memVaultQ
	reads atomic.Int32
}

func (q *countingStateReader) GetUserVaultState(ctx context.Context, id uuid.UUID) (store.GetUserVaultStateRow, error) {
	q.reads.Add(1)
	return q.memVaultQ.GetUserVaultState(ctx, id)
}

func TestCapacityAdminVaultObservation(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	rows := []capacityCodexRow{
		{userID: a, accountID: uuid.New(), vaultMode: store.VaultModeUnprotected},
		{userID: a, accountID: uuid.New(), vaultMode: store.VaultModeUnprotected},
		{userID: b, accountID: uuid.New(), vaultMode: store.VaultModeProtected},
	}
	req := capacityReq(t, "", true)

	t.Run("locked state per distinct user", func(t *testing.T) {
		m := newMemVaultQ()
		m.states[a] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected}
		h := capacityHandler(time.Minute)
		h.vault = newHandlerMemoryVault(newHandlerTestBox(t), m)
		locked, observed := h.observeCodexVault(context.Background(), &analyticsDeps{}, req, rows)
		if !observed || locked[a] || !locked[b] {
			t.Fatalf("locked = %v observed %v, want a unlocked, b locked", locked, observed)
		}
	})
	t.Run("vault read error", func(t *testing.T) {
		m := newMemVaultQ()
		h := capacityHandler(time.Minute)
		h.vault = vault.New(newHandlerTestBox(t), &adminRateStatusReader{memVaultQ: m, fail: "state"}, m.transaction)
		if _, observed := h.observeCodexVault(context.Background(), &analyticsDeps{}, req, rows); observed {
			t.Fatal("a failing vault read must leave the state unobserved")
		}
		g := h.codexGauge(req, capacityCodexSet{selected: true, rows: rows}, nil, false, time.Now())
		if g.Status != "unavailable" || *g.Reason != "vault_status_unavailable" || len(g.Identities) != 0 {
			t.Fatalf("gauge = %+v", g)
		}
	})
	t.Run("budget expiry", func(t *testing.T) {
		m := newMemVaultQ()
		h := capacityHandler(time.Minute)
		h.vault = vault.New(newHandlerTestBox(t), &blockingStateReader{memVaultQ: m}, m.transaction)
		d := &analyticsDeps{vaultBudget: 30 * time.Millisecond}
		start := time.Now()
		if _, observed := h.observeCodexVault(context.Background(), d, req, rows); observed {
			t.Fatal("a read past the budget must leave the state unobserved")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("observation ran %v, the injected 30ms budget did not bound it", elapsed)
		}
	})
	t.Run("a lookup that succeeds after the budget expired is not accepted", func(t *testing.T) {
		m := newMemVaultQ()
		m.states[a] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected}
		h := capacityHandler(time.Minute)
		h.vault = vault.New(newHandlerTestBox(t), &lateSuccessStateReader{memVaultQ: m}, m.transaction)
		d := &analyticsDeps{vaultBudget: 30 * time.Millisecond}
		locked, observed := h.observeCodexVault(context.Background(), d, req, rows[:1])
		if observed || locked != nil {
			t.Fatalf("observe = %v, %v: a state returned after the budget expired must leave the state unobserved", locked, observed)
		}
		g := h.codexGauge(req, capacityCodexSet{selected: true, rows: rows[:1]}, locked, observed, time.Now())
		if g.Status != "unavailable" || *g.Reason != "vault_status_unavailable" {
			t.Fatalf("gauge = %+v, want unavailable / vault_status_unavailable", g)
		}
		// Control: the same double inside a generous budget is accepted, so the refusal above is
		// the post-lookup budget check and not the double failing.
		if locked, observed := h.observeCodexVault(context.Background(), &analyticsDeps{vaultBudget: time.Minute}, req, rows[:1]); !observed || locked[a] {
			t.Fatalf("control observe = %v, %v, want a observed unlocked", locked, observed)
		}
	})
	t.Run("nil vault keeps the conservative fallback", func(t *testing.T) {
		h := capacityHandler(time.Minute)
		locked, observed := h.observeCodexVault(context.Background(), &analyticsDeps{}, req, rows)
		if !observed || locked[a] || !locked[b] {
			t.Fatalf("nil-vault fallback = %v %v, want a (unprotected) open and b (protected) locked", locked, observed)
		}
	})
}

// lateSuccessStateReader makes the lookup's last context-sensitive step (the policy read that
// follows the state read) take 200ms and succeed regardless of the context: the whole lookup
// succeeds, but only after a short budget has expired, and nothing inside it reports that.
type lateSuccessStateReader struct{ *memVaultQ }

func (q *lateSuccessStateReader) GetVaultPolicy(ctx context.Context) (string, error) {
	time.Sleep(200 * time.Millisecond)
	return q.memVaultQ.GetVaultPolicy(context.WithoutCancel(ctx))
}

type blockingStateReader struct{ *memVaultQ }

func (q *blockingStateReader) GetUserVaultState(ctx context.Context, _ uuid.UUID) (store.GetUserVaultStateRow, error) {
	<-ctx.Done()
	return store.GetUserVaultStateRow{}, ctx.Err()
}

// A snapshot-deadline expiry reaches analyticsFail as the context error Savepoint returns: 503 with
// Retry-After while the request context is live. The control shows why returning an ordinary
// gauge error unchanged would have been a 500.
func TestAnalyticsFailMapsSnapshotDeadlineTo503(t *testing.T) {
	req := analyticsGet(store.User{ID: uuid.New()}, "/api/analytics/capacity")
	rec := httptest.NewRecorder()
	analyticsFail(rec, req, context.DeadlineExceeded)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("deadline = %d Retry-After %q, want 503 and 1", rec.Code, rec.Header().Get("Retry-After"))
	}
	rec = httptest.NewRecorder()
	analyticsFail(rec, req, errors.New("gauge failed"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("control = %d, want 500", rec.Code)
	}
}
