package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckRejectsUnsafeIdentifiers(t *testing.T) {
	for _, unsafe := range []string{"bad\x00id", "bad\u202eid", "bad\x1bid", "\tid", "id\n", strings.Repeat("a", 513)} {
		for _, field := range []string{"capability", "tool", "size", "milestone"} {
			t.Run(field+"/"+unsafe, func(t *testing.T) {
				c := PlanCrossCheckCandidate{Milestones: json.RawMessage("[]"), SizeClass: "s"}
				switch field {
				case "capability":
					c.RequiredCapabilities = []string{unsafe}
				case "tool":
					c.RequiredTools = []string{unsafe}
				case "size":
					c.SizeClass = unsafe
				case "milestone":
					c.Milestones, _ = json.Marshal([]map[string]string{{"id": unsafe, "title": "title"}})
				}
				if _, err := NormalizePlanCrossCheckCandidate(c); err == nil {
					t.Fatal("unsafe identifier accepted")
				}
			})
		}
		raw, _ := json.Marshal(map[string]any{"summary": "ok", "items": []map[string]string{{"file": unsafe, "severity": "error"}}})
		if _, err := NormalizeCrossCheckFindings("block", "block", raw); err == nil {
			t.Fatal("unsafe finding path accepted")
		}
	}
}

func TestPlanCrossCheckInvalidUTF8NativeIdentifiers(t *testing.T) {
	for _, field := range []string{"capability", "tool", "size"} {
		c := PlanCrossCheckCandidate{Milestones: json.RawMessage("[]"), SizeClass: "s"}
		invalid := string([]byte{0xff})
		switch field {
		case "capability":
			c.RequiredCapabilities = []string{invalid}
		case "tool":
			c.RequiredTools = []string{invalid}
		case "size":
			c.SizeClass = invalid
		}
		if _, err := NormalizePlanCrossCheckCandidate(c); err == nil {
			t.Fatalf("invalid UTF-8 accepted in %s", field)
		}
	}
}

func TestPlanCrossCheckIdentifierCountsAndLengths(t *testing.T) {
	c := PlanCrossCheckCandidate{Milestones: json.RawMessage("[]"), SizeClass: "s"}
	for i := 0; i < 64; i++ {
		c.RequiredCapabilities = append(c.RequiredCapabilities, strings.Repeat("a", 256))
		c.RequiredTools = append(c.RequiredTools, strings.Repeat("b", 256))
	}
	if _, err := NormalizePlanCrossCheckCandidate(c); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"capabilities", "tools"} {
		tooMany := c
		if field == "capabilities" {
			tooMany.RequiredCapabilities = append(append([]string{}, c.RequiredCapabilities...), "extra")
		} else {
			tooMany.RequiredTools = append(append([]string{}, c.RequiredTools...), "extra")
		}
		if _, err := NormalizePlanCrossCheckCandidate(tooMany); err == nil {
			t.Fatalf("65 %s accepted", field)
		}
	}
}

func TestPlanCrossCheckSafeIdentifierTrimAndProseScrub(t *testing.T) {
	token := "glpat-" + "abcdefghijklmnopqrst"
	c := PlanCrossCheckCandidate{PlanMd: "Plan\n\u202e" + token, PlanningDiff: "diff\n\x1b",
		Milestones:           json.RawMessage(`[{"id":"m1","title":"Title","metadata":{"id":12}}]`),
		RequiredCapabilities: []string{" go "}, RequiredTools: []string{" git "}, SizeClass: " s "}
	got, err := NormalizePlanCrossCheckCandidate(c)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequiredCapabilities[0] != "go" || got.RequiredTools[0] != "git" || got.SizeClass != "s" {
		t.Fatal("safe identifiers no longer trim")
	}
	if strings.Contains(got.PlanMd, token) || strings.Contains(got.PlanMd, "\u202e") ||
		!strings.Contains(got.PlanMd, "\n") || strings.Contains(got.PlanningDiff, "\x1b") {
		t.Fatal("prose scrub changed or leaked unsafe content")
	}
	for _, raw := range []string{`[{"id":12}]`, `[{"ID":"bad\u202eid"}]`, `["milestone"]`} {
		if _, err := NormalizePlanCrossCheckCandidate(PlanCrossCheckCandidate{Milestones: json.RawMessage(raw)}); err == nil {
			t.Fatalf("invalid actual milestone schema/ID accepted: %s", raw)
		}
	}
	raw := []byte(`{"summary":"ok","items":[{"file":" api/main.go ","severity":"info"}]}`)
	findings, err := NormalizeCrossCheckFindings("approve", "approve", raw)
	if err != nil || !strings.Contains(string(findings), `"file":"api/main.go"`) {
		t.Fatalf("safe finding path trim: %s %v", findings, err)
	}
}

// The transaction double exposes the public SetState validation boundary. It
// declines the final UPDATE, so these tests prove admissibility and fencing,
// without pretending to execute PostgreSQL's authoritative outcome CASE.
type crossCheckBoundaryTx struct {
	pgx.Tx
	t         *testing.T
	run       store.Run
	check     store.CrossCheck
	checkErr  error
	writes    int
	reason    string
	committed bool
	query     func(string, []any) pgx.Row
	exec      func(string, []any) (pgconn.CommandTag, error)
}

type crossCheckBoundaryRow struct {
	value any
	err   error
}

func (r crossCheckBoundaryRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	value := reflect.ValueOf(r.value)
	// Structs follow sqlc's SELECT * model order; scalar results use []any.
	var count int
	if value.Kind() == reflect.Struct {
		count = value.NumField()
	} else {
		count = value.Len()
	}
	if len(dest) != count {
		return fmt.Errorf("scan destinations=%d values=%d", len(dest), count)
	}
	for i, target := range dest {
		var field reflect.Value
		if value.Kind() == reflect.Struct {
			field = value.Field(i)
		} else {
			field = reflect.ValueOf(value.Index(i).Interface())
		}
		if field.Kind() == reflect.Interface {
			field = field.Elem()
		}
		reflect.ValueOf(target).Elem().Set(field)
	}
	return nil
}

func (tx *crossCheckBoundaryTx) Begin(context.Context) (pgx.Tx, error) { return tx, nil }
func (tx *crossCheckBoundaryTx) Commit(context.Context) error          { tx.committed = true; return nil }
func (tx *crossCheckBoundaryTx) Rollback(context.Context) error        { return nil }
func (tx *crossCheckBoundaryTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if tx.query != nil {
		return tx.query(sql, args)
	}
	switch {
	case strings.HasPrefix(sql, "-- name: GetRunOwnedByWorkerForUpdate"):
		return crossCheckBoundaryRow{value: tx.run}
	case strings.HasPrefix(sql, "-- name: GetPlanCrossCheck"):
		return crossCheckBoundaryRow{value: tx.check, err: tx.checkErr}
	default:
		tx.t.Fatalf("unexpected query: %s", strings.Split(sql, "\n")[0])
		return crossCheckBoundaryRow{err: errors.New("unexpected query")}
	}
}
func (tx *crossCheckBoundaryTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if tx.exec != nil {
		return tx.exec(sql, args)
	}
	if !strings.HasPrefix(sql, "-- name: SetRunAwaitingApproval") {
		tx.t.Fatalf("unexpected write: %s", strings.Split(sql, "\n")[0])
	}
	tx.writes++
	tx.reason = args[5].(pgtype.Text).String
	return pgconn.NewCommandTag("UPDATE 0"), nil
}

func TestPlanCrossCheckRefusalStateBoundary(t *testing.T) {
	readFailure := errors.New("cross-check read failed")
	for _, tc := range []struct {
		name, reason, refusal string
		change                func(*store.Run, *StateRequest, *crossCheckBoundaryTx)
		want                  error
	}{
		{name: "candidate too large", reason: "candidate_refused", refusal: "candidate_too_large"},
		{name: "candidate invalid", reason: "candidate_refused", refusal: "candidate_invalid"},
		{name: "envelope too large", reason: "candidate_refused", refusal: "envelope_too_large"},
		{name: "submit failed", reason: "checker_failed", refusal: "submit_failed"},
		{name: "unbounded", reason: "candidate_refused", refusal: "anything", want: ErrInvalidState},
		{name: "missing classification", reason: "candidate_refused", want: ErrInvalidState},
		{name: "wrong classification", reason: "checker_failed", refusal: "candidate_invalid", want: ErrInvalidState},
		{name: "wrong reason", reason: "candidate_refused", refusal: "submit_failed", want: ErrInvalidState},
		{name: "classification without reason", refusal: "candidate_invalid", want: ErrInvalidState},
		{name: "other gate", reason: "planning_diff_refused", refusal: "candidate_invalid", want: ErrInvalidState},
		{name: "unknown gate", reason: "approve", refusal: "candidate_invalid", want: ErrInvalidState},
		{name: "not required", reason: "candidate_refused", refusal: "candidate_invalid", change: func(r *store.Run, _ *StateRequest, _ *crossCheckBoundaryTx) { r.PlanCrossCheckRequired = false }, want: ErrInvalidState},
		{name: "not auto", reason: "checker_failed", refusal: "submit_failed", change: func(r *store.Run, _ *StateRequest, _ *crossCheckBoundaryTx) { r.AutoApprove = false }, want: ErrInvalidState},
		{name: "codex lead", reason: "checker_failed", refusal: "submit_failed", change: func(r *store.Run, _ *StateRequest, _ *crossCheckBoundaryTx) { r.Harness = "codex" }, want: ErrInvalidState},
		{name: "other user", reason: "candidate_refused", refusal: "candidate_invalid", change: func(r *store.Run, _ *StateRequest, _ *crossCheckBoundaryTx) { r.UserID = uuid.New() }, want: ErrInvalidState},
		{name: "other worker", reason: "candidate_refused", refusal: "candidate_invalid", change: func(r *store.Run, _ *StateRequest, _ *crossCheckBoundaryTx) { r.WorkerID = pgconv.UUID(uuid.New()) }, want: ErrInvalidState},
		{name: "stale generation", reason: "candidate_refused", refusal: "candidate_invalid", change: func(_ *store.Run, r *StateRequest, _ *crossCheckBoundaryTx) {
			gen := int64(2)
			r.ClaimGeneration = &gen
		}, want: ErrStaleClaim},
		{name: "missing generation", reason: "candidate_refused", refusal: "candidate_invalid", change: func(_ *store.Run, r *StateRequest, _ *crossCheckBoundaryTx) { r.ClaimGeneration = nil }, want: ErrClaimGenerationRequired},
		{name: "released", reason: "candidate_refused", refusal: "candidate_invalid", change: func(r *store.Run, _ *StateRequest, _ *crossCheckBoundaryTx) { r.ClaimReleasedAt.Valid = true }, want: ErrStaleClaim},
		{name: "not writable", reason: "candidate_refused", refusal: "candidate_invalid", change: func(r *store.Run, _ *StateRequest, _ *crossCheckBoundaryTx) { r.Status = "awaiting_approval" }, want: ErrInvalidState},
		{name: "read failure", reason: "candidate_refused", refusal: "candidate_invalid", change: func(_ *store.Run, _ *StateRequest, tx *crossCheckBoundaryTx) { tx.checkErr = readFailure }, want: readFailure},
		{name: "stored dissent", reason: "candidate_refused", refusal: "candidate_invalid", change: func(_ *store.Run, _ *StateRequest, tx *crossCheckBoundaryTx) {
			tx.checkErr = nil
			tx.check.Verdict = "block"
		}},
		{name: "stored approve", reason: "checker_failed", refusal: "submit_failed", change: func(_ *store.Run, _ *StateRequest, tx *crossCheckBoundaryTx) {
			tx.checkErr = nil
			tx.check.Verdict = "approve"
		}},
		{name: "recovered generation", reason: "candidate_refused", refusal: "candidate_invalid", change: func(r *store.Run, req *StateRequest, _ *crossCheckBoundaryTx) {
			r.ClaimGeneration = 2
			gen := int64(2)
			req.ClaimGeneration = &gen
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := worker()
			r := store.Run{ID: uuid.New(), UserID: w.UserID, WorkerID: pgconv.UUID(w.ID), Kind: "issue",
				Status: "running", Harness: "claude", PlanCrossCheckRequired: true, AutoApprove: true, ClaimGeneration: 1}
			gen := int64(1)
			plan := "plan"
			req := StateRequest{State: "awaiting_approval", ClaimGeneration: &gen, PlanMd: &plan, PlanCrossCheckRefusal: tc.refusal}
			if tc.reason != "" {
				req.PlanCrossCheckGateReason = &tc.reason
			}
			tx := &crossCheckBoundaryTx{t: t, checkErr: pgx.ErrNoRows}
			if tc.change != nil {
				tc.change(&r, &req, tx)
			}
			tx.run = r
			fs := &fakeStore{runOwned: r}
			svc := New(fs, newBox(t), testParams())
			svc.SetTxBeginner(tx)
			_, applied, err := svc.SetState(context.Background(), w, r.ID, req)
			if applied || !errors.Is(err, tc.want) {
				t.Fatalf("applied=%v err=%v want=%v", applied, err, tc.want)
			}
			if tc.want != nil {
				if tx.writes != 0 || tx.committed {
					t.Fatal("invalid refusal mutated/committed")
				}
			} else if tx.writes != 1 || tx.reason != tc.reason || !tx.committed {
				t.Fatalf("park attempt: writes=%d reason=%s committed=%v", tx.writes, tx.reason, tx.committed)
			}
			if fs.setRunningParams != nil || fs.setAutopilotPlanParams != nil {
				t.Fatal("refusal granted plan authority")
			}
		})
	}
}
