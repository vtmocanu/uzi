package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3264 M2: cross_check_pins on PUT /api/me/model-settings. LiveDB only (skipped without
// UZI_TEST_DATABASE_URL; ./e2e/run-store-it.sh provides one).

var crossCheckCells = []struct{ stage, harness string }{
	{"plan", "claude"}, {"plan", "codex"}, {"code", "claude"}, {"code", "codex"},
}

// pinRows snapshots the fixture user's pin rows as "stage/harness" -> "model|effort" ("<null>" for NULL).
func (f *effFix) pinRows() map[string]string {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT stage, harness, COALESCE(model, '<null>'), COALESCE(effort, '<null>')
		FROM user_cross_check_pins WHERE user_id = $1`, f.user)
	if err != nil {
		f.t.Fatalf("query pins: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var stage, harness, model, effort string
		if err := rows.Scan(&stage, &harness, &model, &effort); err != nil {
			f.t.Fatalf("scan pin: %v", err)
		}
		out[stage+"/"+harness] = model + "|" + effort
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("pins rows: %v", err)
	}
	return out
}

// seedAllPins stores a distinct model and effort in every cell and returns the snapshot.
func (f *effFix) seedAllPins() map[string]string {
	f.t.Helper()
	f.exec(`DELETE FROM user_cross_check_pins WHERE user_id = $1`, f.user)
	f.exec(`INSERT INTO user_cross_check_pins (user_id, stage, harness, model, effort) VALUES
		($1,'plan','claude','sonnet','low'), ($1,'plan','codex','gpt-6-sol','medium'),
		($1,'code','claude','opus','xhigh'), ($1,'code','codex','gpt-6-astra','max')`, f.user)
	return f.pinRows()
}

func (f *effFix) wirePins() map[string]apitypes.CrossCheckPinDTO {
	f.t.Helper()
	var cells []apitypes.CrossCheckPinDTO
	if err := json.Unmarshal(f.wireSettings()["cross_check_pins"], &cells); err != nil {
		f.t.Fatalf("decode cross_check_pins: %v", err)
	}
	out := map[string]apitypes.CrossCheckPinDTO{}
	for _, c := range cells {
		out[c.Stage+"/"+c.Harness] = c
	}
	return out
}

func pinBody(stage, harness, members string) string {
	return fmt.Sprintf(`{"cross_check_pins":[{"stage":%q,"harness":%q,%s}]}`, stage, harness, members)
}

// Every cell can be set, edited one member at a time and reset, and each change touches only
// that cell's named members.
func TestPutMyModelSettingsCrossCheckSetAndResetEachCellLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	models := map[string][2]string{"claude": {"haiku", "fable"}, "codex": {"gpt-6-sol", "gpt-6.1-sol"}}
	for _, cell := range crossCheckCells {
		key := cell.stage + "/" + cell.harness
		t.Run(key, func(t *testing.T) {
			f.t = t
			seeded := f.seedAllPins()
			othersUnchanged := func(want map[string]string, step string) {
				t.Helper()
				got := f.pinRows()
				for k, v := range seeded {
					if k == key {
						continue
					}
					if got[k] != v {
						t.Fatalf("%s changed %s: %q -> %q", step, k, v, got[k])
					}
				}
				if len(got) != len(want) {
					t.Fatalf("%s: rows = %v", step, got)
				}
			}
			parts := strings.Split(seeded[key], "|")
			oldModel, oldEffort := parts[0], parts[1]

			// Model only: the effort member is preserved.
			if rec := f.putModel(pinBody(cell.stage, cell.harness, fmt.Sprintf(`"model":%q`, models[cell.harness][0]))); rec.Code != http.StatusOK {
				t.Fatalf("model-only = %d %s", rec.Code, rec.Body.String())
			}
			if got := f.pinRows()[key]; got != models[cell.harness][0]+"|"+oldEffort {
				t.Fatalf("after model-only %s = %q, want model %s with effort %s kept", key, got, models[cell.harness][0], oldEffort)
			}
			othersUnchanged(seeded, "model-only")

			// Effort only: the model member is preserved.
			if rec := f.putModel(pinBody(cell.stage, cell.harness, `"effort":"medium"`)); rec.Code != http.StatusOK {
				t.Fatalf("effort-only = %d %s", rec.Code, rec.Body.String())
			}
			if got := f.pinRows()[key]; got != models[cell.harness][0]+"|medium" {
				t.Fatalf("after effort-only %s = %q", key, got)
			}
			othersUnchanged(seeded, "effort-only")

			// A null member clears only that member.
			if rec := f.putModel(pinBody(cell.stage, cell.harness, `"model":null`)); rec.Code != http.StatusOK {
				t.Fatalf("model null = %d %s", rec.Code, rec.Body.String())
			}
			if got := f.pinRows()[key]; got != "<null>|medium" {
				t.Fatalf("after model null %s = %q", key, got)
			}

			// Reset (both members null) clears the cell and nothing else.
			if rec := f.putModel(pinBody(cell.stage, cell.harness, `"model":null,"effort":null`)); rec.Code != http.StatusOK {
				t.Fatalf("reset = %d %s", rec.Code, rec.Body.String())
			}
			if got := f.pinRows()[key]; got != "<null>|<null>" {
				t.Fatalf("after reset %s = %q, want both members NULL", key, got)
			}
			othersUnchanged(seeded, "reset")

			// Setting both members at once; the old values were distinct from these.
			if rec := f.putModel(pinBody(cell.stage, cell.harness, fmt.Sprintf(`"model":%q,"effort":"max"`, models[cell.harness][1]))); rec.Code != http.StatusOK {
				t.Fatalf("both = %d %s", rec.Code, rec.Body.String())
			}
			if got := f.pinRows()[key]; got != models[cell.harness][1]+"|max" {
				t.Fatalf("after both %s = %q (was %s|%s)", key, got, oldModel, oldEffort)
			}
			othersUnchanged(seeded, "set both")
		})
	}
}

// An empty list writes nothing, and a pin save without a scalar still answers the envelope with
// the pins in it.
func TestPutMyModelSettingsCrossCheckEmptyListAndEnvelopeLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	seeded := f.seedAllPins()
	rec := f.putModel(`{"cross_check_pins":[]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"cross_check_pins"`) {
		t.Fatalf("empty list = %d %s", rec.Code, rec.Body.String())
	}
	if got := f.pinRows(); !reflect.DeepEqual(got, seeded) {
		t.Fatalf("empty list changed pins: %v", got)
	}
	rec = f.putModel(pinBody("plan", "claude", `"effort":"high"`))
	var body struct {
		Settings struct {
			Pins []apitypes.CrossCheckPinDTO `json:"cross_check_pins"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("save = %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
	found := false
	for _, c := range body.Settings.Pins {
		if c.Stage == "plan" && c.Harness == "claude" {
			found = true
			if c.Effort == nil || *c.Effort != "high" || c.Model == nil || *c.Model != "sonnet" {
				t.Fatalf("response cell = %+v, want effort high and model sonnet kept", c)
			}
		}
	}
	if !found {
		t.Fatalf("response has no plan/claude cell: %s", rec.Body.String())
	}
}

// A malformed pins list is a 400 naming cross_check_pins, and nothing is written, not even a valid
// scalar sent in the same body. runtime is a worker-effort value and is refused for a checker.
func TestPutMyModelSettingsCrossCheckRejectsWithoutWritingLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	f.exec(`UPDATE users SET default_effort = 'low' WHERE id = $1`, f.user)
	seeded := f.seedAllPins()
	before := f.modelRow()

	for name, pins := range map[string]string{
		"top-level null":        `null`,
		"not an array":          `{"stage":"plan","harness":"claude","effort":"high"}`,
		"duplicate cells":       `[{"stage":"plan","harness":"claude","effort":"high"},{"stage":"plan","harness":"claude","model":"opus"}]`,
		"bad stage":             `[{"stage":"review","harness":"claude","effort":"high"}]`,
		"bad harness":           `[{"stage":"plan","harness":"gemini","effort":"high"}]`,
		"runtime effort claude": `[{"stage":"plan","harness":"claude","effort":"runtime"}]`,
		"runtime effort codex":  `[{"stage":"code","harness":"codex","effort":"runtime"}]`,
		"bad effort":            `[{"stage":"plan","harness":"claude","effort":"ultra"}]`,
		"codex model on claude": `[{"stage":"plan","harness":"claude","model":"gpt-6-sol"}]`,
		"claude model on codex": `[{"stage":"plan","harness":"codex","model":"sonnet"}]`,
		"model not a string":    `[{"stage":"plan","harness":"claude","model":7}]`,
		"unknown member":        `[{"stage":"plan","harness":"claude","effrot":"high"}]`,
		"five cells": `[{"stage":"plan","harness":"claude","effort":"low"},{"stage":"plan","harness":"codex","effort":"low"},
			{"stage":"code","harness":"claude","effort":"low"},{"stage":"code","harness":"codex","effort":"low"},
			{"stage":"code","harness":"codex","effort":"high"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			f.t = t
			rec := f.putModel(`{"default_effort":"max","cross_check_pins":` + pins + `}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("= %d %s, want 400", rec.Code, rec.Body.String())
			}
			if got := modelErrBody(t, rec); got["field"] != "cross_check_pins" || got["error"] == "" {
				t.Fatalf("body = %v, want field cross_check_pins with a message", got)
			}
			if after := f.modelRow(); !reflect.DeepEqual(before, after) {
				t.Fatalf("a rejected save wrote the user row:\n before %v\n after  %v", before, after)
			}
			if got := f.pinRows(); !reflect.DeepEqual(got, seeded) {
				t.Fatalf("a rejected save wrote pins: %v", got)
			}
		})
	}
	f.t = t

	// A scalar error is reported before the pins are looked at, so an invalid scalar next to
	// invalid pins names the scalar, as PutMySettings does.
	rec := f.putModel(`{"cross_check_pins":null,"default_effort":"bogus"}`)
	if got := modelErrBody(t, rec)["field"]; rec.Code != http.StatusBadRequest || got != "default_effort" {
		t.Fatalf("scalar + bad pins = %d field %q, want 400 default_effort", rec.Code, got)
	}
}

func (f *effFix) failTrigger(table, condition string) {
	f.t.Helper()
	fn := "uzi_t3264_pin_fail_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	f.exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF %s THEN RAISE EXCEPTION 'forced failure'; END IF;
			RETURN NEW;
		END $$`, fn, condition))
	f.exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT OR UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION %s()`, fn, table, fn))
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, fn, table))
		_, _ = f.pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
}

// The pin write is part of the route's transaction. A failing pin write (the LAST statement)
// rolls back the scalar writes, and a failing user-row write means no pin is written.
func TestPutMyModelSettingsCrossCheckRollsBackTogetherLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	f.exec(`UPDATE users SET default_effort = 'low' WHERE id = $1`, f.user)
	seeded := f.seedAllPins()
	before := f.modelRow()
	body := `{"default_effort":"high","default_claude_model":"opus",
		"cross_check_pins":[{"stage":"code","harness":"claude","model":"haiku"},{"stage":"plan","harness":"codex","effort":"low"}]}`

	// Last-write failure: a trigger on the pins table for this user.
	f.failTrigger("user_cross_check_pins", fmt.Sprintf(`NEW.user_id = '%s'::uuid`, f.user))
	rec := f.putModel(body)
	if rec.Code != http.StatusInternalServerError || modelErrBody(t, rec)["error"] != "internal error" {
		t.Fatalf("pin failure = %d %s, want a plain 500", rec.Code, rec.Body.String())
	}
	if after := f.modelRow(); !reflect.DeepEqual(before, after) {
		t.Fatalf("pin failure left the user row changed:\n before %v\n after  %v", before, after)
	}
	if got := f.pinRows(); !reflect.DeepEqual(got, seeded) {
		t.Fatalf("pin failure left pins changed: %v", got)
	}
}

func TestPutMyModelSettingsCrossCheckNoPinsWhenUserWriteFailsLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	f.exec(`UPDATE users SET default_effort = 'low' WHERE id = $1`, f.user)
	seeded := f.seedAllPins()
	before := f.modelRow()
	f.failTrigger("users", fmt.Sprintf(`NEW.id = '%s'::uuid AND NEW.default_effort IS DISTINCT FROM OLD.default_effort`, f.user))
	rec := f.putModel(`{"default_effort":"high","cross_check_pins":[{"stage":"plan","harness":"claude","model":"haiku"}]}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("user-row failure = %d %s, want 500", rec.Code, rec.Body.String())
	}
	if after := f.modelRow(); !reflect.DeepEqual(before, after) {
		t.Fatalf("user-row failure changed the row:\n before %v\n after  %v", before, after)
	}
	if got := f.pinRows(); !reflect.DeepEqual(got, seeded) {
		t.Fatalf("a failed user write still wrote pins: %v", got)
	}
}

// After a reset the cell follows the worker default: changing default_claude_model through the
// same route changes the cell's resolved model and source in GET /api/me/settings.
func TestPutMyModelSettingsCrossCheckResetFollowsWorkerDefaultLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	f.exec(`DELETE FROM user_cross_check_pins WHERE user_id = $1`, f.user)

	if rec := f.putModel(`{"default_claude_model":"sonnet",` + pinBody("plan", "claude", `"model":"opus"`)[1:]); rec.Code != http.StatusOK {
		t.Fatalf("pin = %d %s", rec.Code, rec.Body.String())
	}
	cell := f.wirePins()["plan/claude"]
	if cell.ResolvedModel == nil || *cell.ResolvedModel != "opus" || cell.ModelSource != "pin" {
		t.Fatalf("pinned cell = %+v, want opus from pin", cell)
	}

	if rec := f.putModel(pinBody("plan", "claude", `"model":null,"effort":null`)); rec.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body.String())
	}
	cell = f.wirePins()["plan/claude"]
	if cell.Model != nil || cell.ResolvedModel == nil || *cell.ResolvedModel != "sonnet" || cell.ModelSource != "worker default" {
		t.Fatalf("after reset = %+v, want sonnet from the worker default", cell)
	}

	if rec := f.putModel(`{"default_claude_model":"haiku"}`); rec.Code != http.StatusOK {
		t.Fatalf("change worker default = %d %s", rec.Code, rec.Body.String())
	}
	cell = f.wirePins()["plan/claude"]
	if cell.ResolvedModel == nil || *cell.ResolvedModel != "haiku" || cell.ModelSource != "worker default" {
		t.Fatalf("after the worker default changed = %+v, want haiku from the worker default", cell)
	}
	if got := f.pinRows()["plan/claude"]; got != "<null>|<null>" {
		t.Fatalf("stored cell = %q, want no copy of the default", got)
	}
}

// routeTxPids makes the route's transaction record its backend pid and fail loudly instead of
// hanging (lock_timeout 10s is above deadlock_timeout 1s). The first pid is sent on the channel.
func (f *effFix) routeTxPids(t *testing.T) <-chan int32 {
	t.Helper()
	pids := make(chan int32, 1)
	f.h.modelSettingsBeginTx = func(ctx context.Context) (pgx.Tx, error) {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		var pid int32
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
		select {
		case pids <- pid:
		default:
		}
		return tx, nil
	}
	t.Cleanup(func() { f.h.modelSettingsBeginTx = nil })
	return pids
}

// holdW opens connection W (a transaction with a 10s lock_timeout) and returns it with its pid.
// The caller's deferred rollback is registered here.
func (f *effFix) holdW(ctx context.Context, t *testing.T) (pgx.Tx, int32) {
	t.Helper()
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire W: %v", err)
	}
	t.Cleanup(conn.Release)
	wtx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin W: %v", err)
	}
	t.Cleanup(func() { _ = wtx.Rollback(context.WithoutCancel(ctx)) })
	if _, err := wtx.Exec(ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
		t.Fatalf("W lock_timeout: %v", err)
	}
	var wpid int32
	if err := wtx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&wpid); err != nil {
		t.Fatalf("W pid: %v", err)
	}
	return wtx, wpid
}

type routeResult struct {
	code int
	body string
}

// startRoute runs PutMyModelSettings in a goroutine. The returned exited channel closes when it
// has returned; a test waits on it (after releasing W) so no goroutine outlives the test.
func (f *effFix) startRoute(body string) (done <-chan routeResult, exited <-chan struct{}) {
	d := make(chan routeResult, 1)
	e := make(chan struct{})
	go func() {
		defer close(e)
		rec := httptest.NewRecorder()
		f.h.PutMyModelSettings(rec, userReq(http.MethodPut, "/api/me/model-settings", body, f.user, nil))
		d <- routeResult{rec.Code, rec.Body.String()}
	}()
	return d, e
}

// waitRouteBlockedBy polls until the route's backend (the first pid on pids) is waiting on a lock held
// by wpid, and returns that pid.
func waitRouteBlockedBy(ctx context.Context, t *testing.T, f *effFix, pids <-chan int32, done <-chan routeResult, wpid int32) int32 {
	t.Helper()
	var npid int32
	select {
	case npid = <-pids:
	case r := <-done:
		t.Fatalf("the route finished before opening its transaction: %d %s", r.code, r.body)
	case <-ctx.Done():
		t.Fatalf("the route never opened its transaction: %v", ctx.Err())
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for blocked := 0; blocked < 1; {
		select {
		case r := <-done:
			t.Fatalf("the route finished before blocking on W: %d %s", r.code, r.body)
		case <-ctx.Done():
			t.Fatalf("route backend %d never blocked behind W: %v", npid, ctx.Err())
		case <-ticker.C:
		}
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE pid = $1 AND wait_event_type = 'Lock' AND $2 = ANY(pg_blocking_pids(pid))`, npid, wpid).Scan(&blocked); err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
	}
	return npid
}

// finishW rolls W back and waits for the route goroutine, so a failure path leaves nothing running.
func finishW(ctx context.Context, wtx pgx.Tx, exited <-chan struct{}) {
	_ = wtx.Rollback(context.WithoutCancel(ctx))
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
	}
}

// Lock interleaving. The route holds the users row lock (FOR NO KEY UPDATE) while it waits for a
// pin cell that another transaction holds. That other transaction then inserts a different pin
// cell, whose foreign key takes a KEY SHARE lock on the users row. KEY SHARE does not conflict
// with FOR NO KEY UPDATE, so both finish. With a FOR UPDATE row lock the two wait for each other
// and Postgres aborts one with a deadlock.
//
// Deterministic, with no sleeps for correctness: connection W holds cell A (code/claude); the
// route's request (connection N) takes the users lock, writes default_effort and blocks on A; the
// test polls until N itself is seen waiting on W, then proves from a third connection that N holds
// the users lock in the right mode (FOR UPDATE NOWAIT is refused, FOR KEY SHARE NOWAIT succeeds);
// only then does W write the absent cell B (code/codex) and commit.
func TestModelSettingsUserLockAllowsConcurrentPinInsertLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	f.exec(`UPDATE users SET default_effort = 'low' WHERE id = $1`, f.user)
	f.exec(`DELETE FROM user_cross_check_pins WHERE user_id = $1`, f.user)
	f.exec(`INSERT INTO user_cross_check_pins (user_id, stage, harness, model) VALUES ($1,'code','claude','sonnet')`, f.user)

	ctx, cancel := context.WithTimeout(f.ctx, 40*time.Second)
	defer cancel()

	pids := f.routeTxPids(t)
	wtx, wpid := f.holdW(ctx, t)
	wq := store.New(wtx)
	if err := wq.PatchUserCrossCheckPin(ctx, store.PatchUserCrossCheckPinParams{UserID: f.user, Stage: "code", Harness: "claude",
		SetModel: true, Model: pgtype.Text{String: "haiku", Valid: true}}); err != nil {
		t.Fatalf("W patch A: %v", err)
	}

	done, exited := f.startRoute(`{"default_effort":"high","cross_check_pins":[{"stage":"code","harness":"claude","model":"opus"}]}`)
	defer finishW(ctx, wtx, exited)

	npid := waitRouteBlockedBy(ctx, t, f, pids, done, wpid)

	// N is waiting on W while holding a FOR NO KEY UPDATE-strength lock on its users row (the explicit
	// lock or its own earlier UPDATE). Lock ordering itself is pinned by the mirror-ordering test.
	_, err := f.pool.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE NOWAIT`, f.user)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("FOR UPDATE NOWAIT while N (pid %d) waits = %v, want SQLSTATE 55P03: N does not hold a row lock on users", npid, err)
	}
	if _, err := f.pool.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR KEY SHARE NOWAIT`, f.user); err != nil {
		t.Fatalf("FOR KEY SHARE NOWAIT while N (pid %d) waits = %v, want success: N's users lock is stronger than FOR NO KEY UPDATE", npid, err)
	}

	// W writes the absent cell B (FK KEY SHARE on the users row) and commits.
	checkPg := func(what string, err error) {
		t.Helper()
		if err == nil {
			return
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "55P03") {
			t.Fatalf("%s: SQLSTATE %s (%s): the users row lock conflicts with a pin insert", what, pgErr.Code, pgErr.Message)
		}
		t.Fatalf("%s: %v", what, err)
	}
	checkPg("W insert B", wq.PatchUserCrossCheckPin(ctx, store.PatchUserCrossCheckPinParams{UserID: f.user, Stage: "code", Harness: "codex",
		SetModel: true, Model: pgtype.Text{String: "gpt-6-sol", Valid: true}}))
	checkPg("W commit", wtx.Commit(ctx))

	select {
	case r := <-done:
		if r.code != http.StatusOK {
			t.Fatalf("the route = %d %s, want 200 (a 500 here is a lock failure)", r.code, r.body)
		}
	case <-ctx.Done():
		t.Fatalf("the route never finished: %v", ctx.Err())
	}

	pins := f.pinRows()
	if pins["code/claude"] != "opus|<null>" || pins["code/codex"] != "gpt-6-sol|<null>" {
		t.Fatalf("pins = %v, want A = opus (the route wins last) and B = gpt-6-sol", pins)
	}
	if got := f.modelRow()["default_effort"]; got != "high" {
		t.Fatalf("default_effort = %q, want high", got)
	}
}

// The users row lock is taken before the route reads the stored lane that feeds the legacy
// default_model mirror, so a save that waits on another writer reads that writer's committed
// value. W updates default_claude_model (uncommitted) and the route writes only the Codex lane for
// a user whose harness resolves to Claude: the mirror is the stored Claude lane, which must be
// W's value, read after the wait. Without the lock the lane is read before the wait and the mirror
// is the stale pre-W value.
func TestModelSettingsUserLockOrdersMirrorReadAfterConcurrentWriterLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	f.exec(`UPDATE users SET default_claude_model = 'sonnet', default_model = 'sonnet' WHERE id = $1`, f.user)

	ctx, cancel := context.WithTimeout(f.ctx, 40*time.Second)
	defer cancel()

	pids := f.routeTxPids(t)
	wtx, wpid := f.holdW(ctx, t)
	if _, err := wtx.Exec(ctx, `UPDATE users SET default_claude_model = 'opus' WHERE id = $1`, f.user); err != nil {
		t.Fatalf("W update: %v", err)
	}

	done, exited := f.startRoute(`{"default_codex_model":"gpt-6-sol"}`)
	defer finishW(ctx, wtx, exited)
	waitRouteBlockedBy(ctx, t, f, pids, done, wpid)
	if err := wtx.Commit(ctx); err != nil {
		t.Fatalf("W commit: %v", err)
	}

	select {
	case r := <-done:
		if r.code != http.StatusOK {
			t.Fatalf("the route = %d %s, want 200", r.code, r.body)
		}
	case <-ctx.Done():
		t.Fatalf("the route never finished: %v", ctx.Err())
	}
	row := f.modelRow()
	if row["default_claude_model"] != "opus" || row["default_codex_model"] != "gpt-6-sol" || row["default_model"] != "opus" {
		t.Fatalf("row = %v, want claude lane opus (W's write kept), codex lane gpt-6-sol and mirror opus read after the wait", row)
	}
}

// The route's row lock is FOR NO KEY UPDATE, which is only the strength of a plain UPDATE if none
// of the columns the route writes is a unique-index column or a key a foreign key references.
// Read the catalogs to prove it.
func TestModelSettingsWrittenColumnsAreNotKeyColumnsLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	written := []string{"default_claude_model", "default_codex_model", "default_model", "default_harness",
		"default_effort", "default_codex_effort", "judge_model", "codex_judge_model", "judge_runs_on",
		"summary_model", "codex_summary_model"}
	rows, err := f.pool.Query(f.ctx, `SELECT a.attname FROM pg_attribute a
		WHERE a.attrelid = 'users'::regclass AND a.attnum > 0 AND NOT a.attisdropped AND (
			EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = a.attrelid AND i.indisunique AND a.attnum = ANY(i.indkey::int2[]))
			OR EXISTS (SELECT 1 FROM pg_constraint c WHERE c.contype = 'f' AND c.confrelid = a.attrelid AND a.attnum = ANY(c.confkey)))`)
	if err != nil {
		t.Fatalf("query key columns: %v", err)
	}
	defer rows.Close()
	keys := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		keys[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if !keys["id"] {
		t.Fatalf("key columns = %v, want at least id (the query is not seeing the primary key)", keys)
	}
	for _, c := range written {
		if keys[c] {
			t.Fatalf("%s is a unique or foreign-key-referenced column of users; FOR NO KEY UPDATE no longer matches the lock an UPDATE of it takes", c)
		}
	}
}
