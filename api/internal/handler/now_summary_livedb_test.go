package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// now_summary_livedb_test.go is the PRD #2603 api-seam gate for the model-written Now line: the
// effective setting (instance AND run owner), its delivery on the worker inputs poll, and the
// GetRun attachment rules. Skipped unless UZI_TEST_DATABASE_URL is set (./e2e/run-store-it.sh).

func withInstanceNowSummary(h *Handler, value string) {
	var rows []store.AppSetting
	if value != "" {
		rows = []store.AppSetting{{Key: settings.KeyNowSummaryEnabled, Value: value}}
	}
	h.settings = settings.New(&settingsStore{rows: rows}, time.Minute)
}

func setUserNowSummary(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, v *bool) {
	t.Helper()
	cliMustExec(t, pool, `UPDATE users SET now_summary_enabled = $2 WHERE id = $1`, userID, v)
}

func boolRef(b bool) *bool { return &b }

func TestEffectiveNowSummaryLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	t.Cleanup(func() { cliMustExec(t, pool, `DELETE FROM users WHERE id=$1`, owner) })
	cases := []struct {
		name     string
		instance string
		user     *bool
		want     bool
	}{
		{"default instance, unset user is on", "", nil, true},
		{"instance on, user explicitly on", "true", boolRef(true), true},
		{"instance on, user off", "true", boolRef(false), false},
		{"instance off, unset user", "false", nil, false},
		{"instance off, user on", "false", boolRef(true), false},
		{"junk instance value falls back to the default on", "junk", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withInstanceNowSummary(h, c.instance)
			setUserNowSummary(t, pool, owner, c.user)
			got, err := h.EffectiveNowSummary(t.Context(), owner)
			if err != nil || got != c.want {
				t.Fatalf("EffectiveNowSummary = %v, %v; want %v", got, err, c.want)
			}
		})
	}
	t.Run("an unknown user is off", func(t *testing.T) {
		withInstanceNowSummary(h, "")
		if got, err := h.EffectiveNowSummary(t.Context(), uuid.New()); err != nil || got {
			t.Fatalf("EffectiveNowSummary(unknown) = %v, %v; want false", got, err)
		}
	})
}

// seedNowRun makes a healthy executing issue run with a frozen milestone list, m1 done and the
// given in-progress ids, owned by owner and held by a fresh worker.
func seedNowRun(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, inProgress string) (runID, workerID uuid.UUID) {
	t.Helper()
	repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), time.Now().UnixNano()&0x3fffffff, true)
	runID, workerID = rmSeedRun(t, pool, owner, repo, "running"), uuid.New()
	cliMustExec(t, pool, `INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, $3, $4)`,
		workerID, owner, "now-"+workerID.String(), workerID[:])
	cliMustExec(t, pool, `UPDATE runs SET worker_id=$2, iteration_count=1, health='ok',
		milestones_frozen='[{"id":"m1","title":"One"},{"id":"m2","title":"Two"},{"id":"m3","title":"Three"}]',
		milestones_completed='["m1"]', milestones_in_progress=$3::jsonb WHERE id=$1`, runID, workerID, inProgress)
	return runID, workerID
}

func TestWorkerRunInputsCarriesNowSummaryLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	t.Cleanup(func() { cliMustExec(t, pool, `DELETE FROM users WHERE id=$1`, owner) })
	runID, workerID := seedNowRun(t, pool, owner, `["m2"]`)
	wkr := store.Worker{ID: workerID, UserID: owner}
	poll := func() map[string]json.RawMessage {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/worker/runs/x/inputs", bytes.NewReader(nil))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", runID.String())
		req = req.WithContext(context.WithValue(mw.ContextWithWorker(req.Context(), wkr), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.WorkerRunInputs(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	want := func(label string, body map[string]json.RawMessage, wantVal string) {
		t.Helper()
		got, present := body["now_summary"]
		if wantVal == "" && present {
			t.Fatalf("%s: now_summary = %s, want the field absent", label, got)
		}
		if wantVal != "" && string(got) != wantVal {
			t.Fatalf("%s: now_summary = %q, want %s", label, got, wantVal)
		}
	}

	withInstanceNowSummary(h, "")
	setUserNowSummary(t, pool, owner, nil)
	want("default on", poll(), "true")
	setUserNowSummary(t, pool, owner, boolRef(false))
	want("owner opted out", poll(), "false")
	setUserNowSummary(t, pool, owner, boolRef(true))
	withInstanceNowSummary(h, "false")
	want("admin switch off", poll(), "false")

	// The consume-nothing early returns leave the field out, which the worker reads as off.
	withInstanceNowSummary(h, "")
	cliMustExec(t, pool, `UPDATE runs SET claim_released_at = now() WHERE id = $1`, runID)
	want("released claim", poll(), "")
	cliMustExec(t, pool, `UPDATE runs SET claim_released_at = NULL, claim_generation = 4, credential_switch_requested_at = now(), credential_switch_generation = 4 WHERE id = $1`, runID)
	want("pending credential switch", poll(), "")
}

func getRunDTO(t *testing.T, h *Handler, viewer store.User, runID uuid.UUID) apitypes.RunDTO {
	t.Helper()
	rec := httptest.NewRecorder()
	h.GetRun(rec, runReq(viewer, runID))
	if rec.Code != http.StatusOK {
		t.Fatalf("GetRun status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Run apitypes.RunDTO `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Run
}

func insertNote(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, seq int, milestone, text string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"text": text, "milestone_id": milestone})
	cliMustExec(t, pool, `INSERT INTO run_messages (run_id, seq, kind, agent, payload) VALUES ($1, $2, 'progress_note', 'worker', $3::jsonb)`,
		runID, seq, string(payload))
}

func TestGetRunAttachesNewestNoteForActiveMilestoneLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	otherAdmin := cliSeedUser(t, pool, true)
	t.Cleanup(func() {
		cliMustExec(t, pool, `DELETE FROM users WHERE id=$1`, owner)
		cliMustExec(t, pool, `DELETE FROM users WHERE id=$1`, otherAdmin)
	})
	runID, _ := seedNowRun(t, pool, owner, `["m2"]`)
	ownerUser := store.User{ID: owner}
	withInstanceNowSummary(h, "")
	setUserNowSummary(t, pool, owner, nil)

	noteOf := func(viewer store.User) *apitypes.ProgressNote {
		t.Helper()
		dto := getRunDTO(t, h, viewer, runID)
		if dto.Progress == nil {
			t.Fatal("run has no progress block")
		}
		return dto.Progress.NowNote
	}

	if noteOf(ownerUser) != nil {
		t.Fatal("a run with no note must carry no now_note")
	}

	insertNote(t, pool, runID, 1, "m1", "Finished the first milestone work")
	insertNote(t, pool, runID, 2, "m2", "Reviewing the second milestone")
	insertNote(t, pool, runID, 3, "m2", "Running the tests for the second milestone")
	insertNote(t, pool, runID, 4, "m2", "") // a usage-only or clear note must not shadow the real one
	got := noteOf(ownerUser)
	if got == nil || got.Text != "Running the tests for the second milestone" || got.At.IsZero() {
		t.Fatalf("now_note = %+v, want the newest non-empty note for the active milestone m2", got)
	}

	t.Run("a note for an earlier milestone is not shown once the active milestone moves on", func(t *testing.T) {
		cliMustExec(t, pool, `UPDATE runs SET milestones_completed='["m1","m2"]', milestones_in_progress='["m3"]' WHERE id=$1`, runID)
		if n := noteOf(ownerUser); n != nil {
			t.Fatalf("now_note = %+v for milestone m3, want none", n)
		}
		cliMustExec(t, pool, `UPDATE runs SET milestones_completed='["m1"]', milestones_in_progress='["m2"]' WHERE id=$1`, runID)
	})

	t.Run("the run owner's setting decides, never the viewer's", func(t *testing.T) {
		adminViewer := store.User{ID: otherAdmin, IsAdmin: true}
		// The admin viewer has the setting off for themself; the owner has it on: shown.
		setUserNowSummary(t, pool, otherAdmin, boolRef(false))
		if n := noteOf(adminViewer); n == nil {
			t.Fatal("an admin viewing another user's run must see the note the owner enabled")
		}
		// The owner is off; the admin viewer is on: hidden, for the owner and the admin alike.
		setUserNowSummary(t, pool, owner, boolRef(false))
		setUserNowSummary(t, pool, otherAdmin, boolRef(true))
		if n := noteOf(adminViewer); n != nil {
			t.Fatalf("admin viewer saw %+v on a run whose owner opted out", n)
		}
		if n := noteOf(ownerUser); n != nil {
			t.Fatalf("owner saw %+v after opting out", n)
		}
		setUserNowSummary(t, pool, owner, nil)
	})

	t.Run("the admin switch hides it", func(t *testing.T) {
		withInstanceNowSummary(h, "false")
		if n := noteOf(ownerUser); n != nil {
			t.Fatalf("now_note = %+v with the instance switch off", n)
		}
		withInstanceNowSummary(h, "")
		if noteOf(ownerUser) == nil {
			t.Fatal("note must return once the instance switch is back on")
		}
	})

	// Held states: the same active milestone m2 and the same stored note, but progress.state is
	// not percent, so the note is hidden.
	for _, c := range []struct{ name, status, health string }{
		{"waiting for an answer", "awaiting_input", "ok"},
		{"waiting at the plan gate", "awaiting_approval", "ok"},
		{"parked on a usage limit", "limit_wait", "ok"},
		{"queued", "queued", "ok"},
		{"stalled", "running", "stalled"},
	} {
		t.Run("hidden when "+c.name, func(t *testing.T) {
			cliMustExec(t, pool, `UPDATE runs SET status=$2, health=$3 WHERE id=$1`, runID, c.status, c.health)
			dto := getRunDTO(t, h, ownerUser, runID)
			if dto.Progress != nil && dto.Progress.State == "percent" {
				t.Fatalf("setup: %s run still reports percent", c.name)
			}
			if dto.Progress != nil && dto.Progress.NowNote != nil {
				t.Fatalf("now_note = %+v on a %s run", dto.Progress.NowNote, c.name)
			}
			cliMustExec(t, pool, `UPDATE runs SET status='running', health='ok' WHERE id=$1`, runID)
		})
	}

	t.Run("the text is sanitised again on read", func(t *testing.T) {
		insertNote(t, pool, runID, 5, "m2", "\x1b[31mred\u202e alert\x07 done")
		n := noteOf(ownerUser)
		if n == nil || n.Text != "[31mred alert done" {
			t.Fatalf("now_note = %+v, want control and bidi runes stripped", n)
		}
	})
}

// A list read never attaches a note (and never runs the note query).
func TestListRunsNeverCarriesNowNoteLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	t.Cleanup(func() { cliMustExec(t, pool, `DELETE FROM users WHERE id=$1`, owner) })
	runID, _ := seedNowRun(t, pool, owner, `["m2"]`)
	withInstanceNowSummary(h, "")
	insertNote(t, pool, runID, 1, "m2", "Reviewing the second milestone")

	req := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: owner}))
	rec := httptest.NewRecorder()
	h.ListRuns(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ListRuns status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Runs []apitypes.RunListItemDTO `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, r := range resp.Runs {
		if r.ID != runID.String() {
			continue
		}
		seen = true
		if r.Progress == nil || r.Progress.State != "percent" {
			t.Fatalf("setup: list item progress = %+v, want percent", r.Progress)
		}
		if r.Progress.NowNote != nil {
			t.Fatalf("list item carried now_note %+v", r.Progress.NowNote)
		}
	}
	if !seen {
		t.Fatal("the seeded run is missing from the list")
	}
}

// TestPutMySettingsNowSummaryEnabledLiveDB: the per-user switch round-trips through PUT/GET with
// the PATCH semantics of mr_rework_enabled: absent leaves it, a bool sets it, null clears it
// back to the default-ON state, anything else is a 400.
func TestPutMySettingsNowSummaryEnabledLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	t.Cleanup(func() { cliMustExec(t, pool, `DELETE FROM users WHERE id=$1`, owner) })
	put := func(body string) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.PutMySettings(rec, userReq(http.MethodPut, "/api/me/settings", body, owner, nil))
		return rec.Code, rec.Body.String()
	}
	read := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.GetMySettings(rec, userReq(http.MethodGet, "/api/me/settings", "", owner, nil))
		var resp struct {
			Settings map[string]json.RawMessage `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		v, ok := resp.Settings["now_summary_enabled"]
		if !ok {
			t.Fatalf("settings carry no now_summary_enabled key: %s", rec.Body.String())
		}
		return string(v)
	}
	if got := read(); got != "null" {
		t.Fatalf("fresh user now_summary_enabled = %s, want null", got)
	}
	if code, body := put(`{"now_summary_enabled":false}`); code != http.StatusOK {
		t.Fatalf("PUT false: %d %s", code, body)
	}
	if got := read(); got != "false" {
		t.Fatalf("after PUT false = %s", got)
	}
	if code, _ := put(`{"summary_model":null}`); code != http.StatusOK {
		t.Fatal("unrelated PUT failed")
	}
	if got := read(); got != "false" {
		t.Fatalf("an absent field must leave the switch alone, got %s", got)
	}
	if code, body := put(`{"now_summary_enabled":"yes"}`); code != http.StatusBadRequest {
		t.Fatalf("PUT non-bool: %d %s, want 400", code, body)
	}
	if got := read(); got != "false" {
		t.Fatalf("a rejected PUT must write nothing, got %s", got)
	}
	if code, body := put(`{"now_summary_enabled":null}`); code != http.StatusOK {
		t.Fatalf("PUT null: %d %s", code, body)
	}
	if got := read(); got != "null" {
		t.Fatalf("after PUT null = %s, want null (inherit the default ON)", got)
	}
	if code, _ := put(`{"now_summary_enabled":true}`); code != http.StatusOK {
		t.Fatal("PUT true failed")
	}
	if got := read(); got != "true" {
		t.Fatalf("after PUT true = %s", got)
	}
}
