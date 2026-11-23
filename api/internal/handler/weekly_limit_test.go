package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/autoselect"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/weeklylimit"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// wlStore serves the reads the weekly-limit loader makes (and counts them). It embeds workersvc.Store
// nil, so any other read panics.
type wlStore struct {
	workersvc.Store
	cands   []store.ListAutoSelectCandidatesRow
	tokens  []store.ListReserveClaudeTokensRow
	workers []store.ListWorkersByUserRow
	reserve store.GetUserUsageReserveRow
	reads   atomic.Int64
}

func (s *wlStore) ListAutoSelectCandidates(context.Context, uuid.UUID) ([]store.ListAutoSelectCandidatesRow, error) {
	s.reads.Add(1)
	return s.cands, nil
}
func (s *wlStore) GetUserUsageReserve(context.Context, uuid.UUID) (store.GetUserUsageReserveRow, error) {
	s.reads.Add(1)
	return s.reserve, nil
}
func (s *wlStore) GetUserJudgeAnthropicBinding(context.Context, uuid.UUID) (store.GetUserJudgeAnthropicBindingRow, error) {
	s.reads.Add(1)
	return store.GetUserJudgeAnthropicBindingRow{JudgeAnthropicBindMode: "default"}, nil
}
func (s *wlStore) ListReserveClaudeTokens(context.Context, uuid.UUID) ([]store.ListReserveClaudeTokensRow, error) {
	s.reads.Add(1)
	return s.tokens, nil
}
func (s *wlStore) ListReserveCodexAliases(context.Context, uuid.UUID) ([]store.ListReserveCodexAliasesRow, error) {
	s.reads.Add(1)
	return nil, nil
}
func (s *wlStore) ListWorkersByUser(context.Context, uuid.UUID) ([]store.ListWorkersByUserRow, error) {
	s.reads.Add(1)
	return s.workers, nil
}
func (s *wlStore) CredentialSwitchHoldSettling(context.Context, store.CredentialSwitchHoldSettlingParams) (bool, error) {
	s.reads.Add(1)
	return false, nil
}

var (
	wlNow    = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	wlWorker = uuid.MustParse("00000000-0000-0000-0000-00000000f002")
	wlTokA   = uuid.MustParse("00000000-0000-0000-0000-0000000000a1")
	wlTokB   = uuid.MustParse("00000000-0000-0000-0000-0000000000a2")
)

func newWLStore(labelB string, owner uuid.UUID) *wlStore {
	s := &wlStore{}
	for _, t := range []struct {
		id    uuid.UUID
		label string
		pct   int16
	}{{wlTokA, "work", 96}, {wlTokB, labelB, 20}} {
		synced, reset := wlNow.Add(-time.Minute), wlNow.Add(72*time.Hour)
		s.cands = append(s.cands, store.ListAutoSelectCandidatesRow{
			UserSecretID: t.id, Label: t.label, AutoEligible: true,
			SevenDayPct: pgtype.Int2{Int16: t.pct, Valid: true}, SevenDayResetsAt: pgconv.Time(reset), SyncedAt: pgconv.Time(synced),
		})
		s.tokens = append(s.tokens, store.ListReserveClaudeTokensRow{
			UserSecretID: t.id, AutoEligible: true, HasReading: true,
			SevenDayPct: pgtype.Int2{Int16: t.pct, Valid: true}, SevenDayResetsAt: pgconv.Time(reset), SyncedAt: pgconv.Time(synced),
		})
	}
	s.workers = []store.ListWorkersByUserRow{{
		ID: wlWorker, UserID: owner, Name: "w1", AnthropicBindMode: "default", LastHeartbeatAt: pgconv.Time(wlNow.Add(-time.Second)),
	}}
	return s
}

// noRowsDB answers every query with no rows: GetRun resolves a pinned run's credential label through h.q,
// and the label is unneeded here (the weekly-limit block names its own credential).
type noRowsDB struct{}

type noRow struct{}

func (noRow) Scan(...any) error { return pgx.ErrNoRows }

func (noRowsDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, pgx.ErrNoRows
}
func (noRowsDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, pgx.ErrNoRows }
func (noRowsDB) QueryRow(context.Context, string, ...any) pgx.Row        { return noRow{} }

func wlRunsHandler(t *testing.T, rs *runsStore) *Handler {
	t.Helper()
	h := newRunsHandler(t, rs)
	h.q = store.New(noRowsDB{})
	return h
}

func wlLoader(t *testing.T, s *wlStore) *weeklylimit.Loader {
	t.Helper()
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	svc := workersvc.New(s, box, workersvc.Params{
		Autoselect:        autoselect.Policy{Order: autoselect.OrderHeadroomFirst, MinHeadroom: 15, HeadroomTiePct: 5, MaxStaleness: 15 * time.Minute, InflightPenalty: 3},
		UsagePollInterval: 5 * time.Minute, CodexUsagePollInterval: 5 * time.Minute,
		RunLimitMaxWaits: 3, RunLimitMaxPark: 8 * 24 * time.Hour, WorkerHeartbeatStale: 45 * time.Second,
	})
	l := weeklylimit.NewLoader(svc)
	l.Now = func() time.Time { return wlNow }
	return l
}

func wlRun(owner uuid.UUID, limitType string) store.Run {
	return store.Run{
		ID: uuid.New(), UserID: owner, Kind: "issue", Harness: "claude", Status: "limit_wait",
		WorkerID: pgtype.UUID{Bytes: wlWorker, Valid: true}, WaitOnLimit: true, LimitWaitCount: 1,
		RateLimitType:     pgtype.Text{String: limitType, Valid: true},
		AnthropicSecretID: pgtype.UUID{Bytes: wlTokA, Valid: true}, LimitDeadSecretID: pgtype.UUID{Bytes: wlTokA, Valid: true},
		LimitResetsAt: pgconv.Time(wlNow.Add(72 * time.Hour)), RetryNotBefore: pgconv.Time(wlNow.Add(72 * time.Hour)),
		CredentialOverrideMode: pgtype.Text{String: "pinned", Valid: true}, CredentialOverrideSecretID: pgtype.UUID{Bytes: wlTokA, Valid: true},
	}
}

func getRunWeekly(t *testing.T, h *Handler, viewer store.User, id uuid.UUID) (int, *apitypes.WeeklyLimitDTO, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.GetRun(rec, runReq(viewer, id))
	var body struct {
		Run apitypes.RunDTO `json:"run"`
	}
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v: %s", err, rec.Body.String())
		}
	}
	return rec.Code, body.Run.WeeklyLimit, rec.Body.String()
}

func TestGetRunWeeklyLimitPinnedSevenDayPark(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	run := wlRun(owner.ID, "seven_day")
	h := wlRunsHandler(t, &runsStore{ownerID: owner.ID, run: run})
	h.SetWeeklyLimit(wlLoader(t, newWLStore("spare", owner.ID)))

	code, got, raw := getRunWeekly(t, h, owner, run.ID)
	if code != http.StatusOK || got == nil {
		t.Fatalf("GET run = %d weekly_limit=%v: %s", code, got, raw)
	}
	if got.Reason != "pinned" || got.Tier != apitypes.WeeklyLimitTierWarning || got.CredentialKey != "anthropic:"+wlTokA.String() {
		t.Fatalf("block = %+v", got)
	}
	var cmd string
	for _, f := range got.Fix {
		if f.Kind == weeklylimit.FixSetToken {
			cmd = f.Command
		}
	}
	if want := "uzi run set-token " + run.ID.String() + " -- spare"; cmd != want {
		t.Fatalf("set_token command = %q; want %q", cmd, want)
	}
}

func TestGetRunWeeklyLimitHostileLabelInDetail(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	run := wlRun(owner.ID, "seven_day")
	h := wlRunsHandler(t, &runsStore{ownerID: owner.ID, run: run})
	setToken := func(label string) *apitypes.WeeklyLimitFixDTO {
		t.Helper()
		h.SetWeeklyLimit(wlLoader(t, newWLStore(label, owner.ID)))
		_, got, raw := getRunWeekly(t, h, owner, run.ID)
		if got == nil {
			t.Fatalf("no block: %s", raw)
		}
		for i := range got.Fix {
			if got.Fix[i].Kind == weeklylimit.FixSetToken {
				return &got.Fix[i]
			}
		}
		t.Fatalf("no set_token fix: %s", raw)
		return nil
	}

	// Quotes and a command substitution are quoted, so the command still copies safely.
	st := setToken("x'" + "$" + "(id)" + "\"y\"")
	if want := "uzi run set-token " + run.ID.String() + " -- 'x'\\''$(id)\"y\"'"; st.Command != want {
		t.Fatalf("command = %q; want %q", st.Command, want)
	}

	// A backtick label gets no command (it would end the Slack code span), and says why.
	st = setToken("x" + "`y`")
	if st.Command != "" || st.CommandUnavailable != apitypes.WeeklyFixCommandLabel {
		t.Fatalf("backtick label: command %q unavailable %q; want none and %q", st.Command, st.CommandUnavailable, apitypes.WeeklyFixCommandLabel)
	}
}

func TestGetRunWeeklyLimitAbsentCases(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	for _, lt := range []string{"five_hour", "seven_day_opus"} {
		run := wlRun(owner.ID, lt)
		s := newWLStore("spare", owner.ID)
		h := wlRunsHandler(t, &runsStore{ownerID: owner.ID, run: run})
		h.SetWeeklyLimit(wlLoader(t, s))
		code, got, raw := getRunWeekly(t, h, owner, run.ID)
		if code != http.StatusOK || got != nil || strings.Contains(raw, "weekly_limit") {
			t.Errorf("%s: weekly_limit present: %d %s", lt, code, raw)
		}
	}
	// A provider-bound run is structurally excluded, with no reads.
	run := wlRun(owner.ID, "seven_day")
	run.ModelProviderID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	s := newWLStore("spare", owner.ID)
	h := wlRunsHandler(t, &runsStore{ownerID: owner.ID, run: run})
	h.SetWeeklyLimit(wlLoader(t, s))
	if _, got, _ := getRunWeekly(t, h, owner, run.ID); got != nil || s.reads.Load() != 0 {
		t.Errorf("provider-bound run: block=%v reads=%d", got, s.reads.Load())
	}
	// No loader wired: no block.
	run = wlRun(owner.ID, "seven_day")
	h = wlRunsHandler(t, &runsStore{ownerID: owner.ID, run: run})
	if _, got, _ := getRunWeekly(t, h, owner, run.ID); got != nil {
		t.Errorf("nil loader produced a block: %+v", got)
	}
}

// spyWeekly fails the test if the handler asks it for any block.
type spyWeekly struct{ calls atomic.Int64 }

func (s *spyWeekly) ForRuns(context.Context, uuid.UUID, []store.Run) map[uuid.UUID]*apitypes.WeeklyLimitDTO {
	s.calls.Add(1)
	return nil
}

func TestWeeklyLimitNeverBuiltForAdminOnOthersRuns(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	run := wlRun(owner.ID, "seven_day")
	rs := &runsStore{ownerID: owner.ID, run: run,
		activeRuns: []store.ListActiveRunsAllRow{{Run: run, OwnerEmail: "o@example.com", PriorityClass: "normal"}}}
	spy := &spyWeekly{}
	h := wlRunsHandler(t, rs)
	h.SetWeeklyLimit(spy)

	rec := httptest.NewRecorder()
	h.GetRun(rec, runReq(admin, run.ID))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "weekly_limit") {
		t.Fatalf("admin GET run: %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/runs", nil)
	req = req.WithContext(mw.ContextWithUser(req.Context(), admin))
	rec = httptest.NewRecorder()
	h.AdminListRuns(rec, req)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "weekly_limit") {
		t.Fatalf("admin list: %d %s", rec.Code, rec.Body.String())
	}
	if spy.calls.Load() != 0 {
		t.Fatalf("weekly-limit source consulted %d times for an admin viewing another user's runs", spy.calls.Load())
	}
}

func TestListRunsWeeklyLimitSummaryOwnerOnly(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	parked, fiveHour := wlRun(owner.ID, "seven_day"), wlRun(owner.ID, "five_hour")
	rs := &runsStore{ownerID: owner.ID, run: parked, userRuns: []store.ListRunsForUserRow{
		{Run: parked, PriorityClass: "normal"}, {Run: fiveHour, PriorityClass: "normal"},
	}}
	h := wlRunsHandler(t, rs)
	// The default margin (15): with margin 0 the summary drops to notice
	// (TestWeeklyLimitMarginZeroDropsParkedOverviewDecision). Token A reads 50%, below
	// the warning line, so only the seven_day park carries a block: a five_hour park on a
	// near-limit token may carry an independent weekly warning, not a park explanation.
	s := nearStoreFor(owner.ID, 15)
	for i := range s.cands {
		if s.cands[i].UserSecretID == wlTokA {
			s.cands[i].SevenDayPct.Int16 = 50
		}
	}
	for i := range s.tokens {
		if s.tokens[i].UserSecretID == wlTokA {
			s.tokens[i].SevenDayPct.Int16 = 50
		}
	}
	h.SetWeeklyLimit(wlLoader(t, s))

	req := httptest.NewRequest(http.MethodGet, "/api/runs?view=summary", nil)
	req = req.WithContext(mw.ContextWithUser(req.Context(), owner))
	rec := httptest.NewRecorder()
	h.ListRuns(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Runs []apitypes.RunSummaryItemDTO `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	byID := map[string]*apitypes.WeeklyLimitSummaryDTO{}
	for _, r := range body.Runs {
		byID[r.ID] = r.WeeklyLimit
	}
	got := byID[parked.ID.String()]
	if got == nil || got.Tier != apitypes.WeeklyLimitTierWarning || got.CredentialKey != "anthropic:"+wlTokA.String() || got.CredentialLabel != "work" {
		t.Fatalf("parked summary = %+v", got)
	}
	if byID[fiveHour.ID.String()] != nil {
		t.Fatalf("five_hour run carried a summary: %+v", byID[fiveHour.ID.String()])
	}
}
