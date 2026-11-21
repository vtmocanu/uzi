package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestPMCreateOutcome pins how pmCreateOutcome decides database ownership after a
// CREATE DATABASE attempt.
func TestPMCreateOutcome(t *testing.T) {
	pg := func(unlocalized string) error {
		return &pgconn.PgError{Severity: "ERREUR", SeverityUnlocalized: unlocalized, Code: "42P04"}
	}
	exec := func(err error) error { return &pmAdminErr{Stage: pmStageExec, Err: err} }
	cases := []struct {
		name                string
		err                 error
		wantCreated, wantAm bool
	}{
		{"nil", nil, true, false},
		{"connect stage", &pmAdminErr{Stage: pmStageConnect, Err: errors.New("refused")}, false, false},
		{"marker stage", &pmAdminErr{Stage: pmStageMarker, Err: errors.New("no marker")}, false, false},
		{"exec ERROR", exec(pg("ERROR")), false, false},
		{"exec FATAL", exec(pg("FATAL")), false, true},
		{"exec PANIC", exec(pg("PANIC")), false, true},
		{"exec plain", exec(errors.New("conn reset")), false, true},
		{"exec context canceled", exec(context.Canceled), false, true},
		{"unwrapped injected error", errors.New("simulated"), false, true},
		{"unwrapped PgError ERROR", pg("ERROR"), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created, amb := pmCreateOutcome(tc.err)
			if created != tc.wantCreated || amb != tc.wantAm {
				t.Fatalf("pmCreateOutcome = (created %v, ambiguous %v), want (%v, %v)", created, amb, tc.wantCreated, tc.wantAm)
			}
		})
	}
}

func pmGuardPrefix() string {
	return "pm_" + strings.ReplaceAll(uuid.New().String(), "-", "")
}

// TestPoolMemoryHarnessDDLGuardsLiveDB proves against a real throwaway server that the harness
// DDL is marker-guarded and that clone creation and cleanup only ever touch databases the
// invocation itself created. It skips only when no database is configured.
func TestPoolMemoryHarnessDDLGuardsLiveDB(t *testing.T) {
	adminDSN := os.Getenv("UZI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	pmRequireThrowawayDB(ctx, t, adminDSN)

	// Both helpers report on the *testing.T they are given, so a subtest never fails its parent.
	dbExists := func(tt *testing.T, name string) bool {
		tt.Helper()
		c, err := pgx.Connect(ctx, adminDSN)
		if err != nil {
			tt.Fatalf("dbExists connect: %v", err)
		}
		defer func() { _ = c.Close(ctx) }()
		if err := pmCheckMarker(ctx, c); err != nil {
			tt.Fatalf("dbExists marker: %v", err)
		}
		var ok bool
		if err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&ok); err != nil {
			tt.Fatalf("dbExists query: %v", err)
		}
		return ok
	}
	// makeDB creates a fixture database directly from template0 and registers its exact-name drop
	// on tt.
	makeDB := func(tt *testing.T, name string) {
		tt.Helper()
		if err := pmAdminExec(ctx, adminDSN, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE template0"); err != nil {
			tt.Fatalf("create fixture %s: %v", name, err)
		}
		tt.Cleanup(func() {
			if err := pmDropDB(adminDSN, name); err != nil {
				tt.Errorf("drop fixture %s: %v", name, err)
			}
		})
	}

	outFile, err := os.Create(filepath.Join(t.TempDir(), "out.jsonl"))
	if err != nil {
		t.Fatalf("create out: %v", err)
	}
	defer func() { _ = outFile.Close() }()

	labPrefix := pmGuardPrefix()
	tplName, err := pmDBName(labPrefix, "_tpl")
	if err != nil {
		t.Fatalf("template name: %v", err)
	}
	makeDB(t, tplName)
	lab := &pmLab{t: t, out: &pmOut{f: outFile}, adminDSN: adminDSN, rawDir: t.TempDir(), jit: "default",
		prefix: labPrefix, templateName: tplName, exec: pmAdminExec}

	t.Run("unmarked connection", func(t *testing.T) {
		p := pmGuardPrefix()
		unmarked := p + "_unmarked"
		probe := p + "_probe"
		makeDB(t, unmarked)
		t.Cleanup(func() { // safety net if the guard ever fails: drop the probe and fail the test
			if !dbExists(t, probe) {
				return
			}
			t.Errorf("probe database %s exists; dropping it", probe)
			if err := pmDropDB(adminDSN, probe); err != nil {
				t.Errorf("drop probe %s: %v", probe, err)
			}
		})
		udsn := pmDSNForDB(t, adminDSN, unmarked)
		uc, err := pgx.Connect(ctx, udsn)
		if err != nil {
			t.Fatalf("connect unmarked: %v", err)
		}
		pre := pmCheckMarker(ctx, uc)
		_ = uc.Close(ctx)
		if pre == nil {
			t.Fatal("precondition: the unmarked database unexpectedly carries the marker")
		}
		err = pmAdminExec(ctx, udsn, "CREATE DATABASE "+pgx.Identifier{probe}.Sanitize())
		var ae *pmAdminErr
		if !errors.As(err, &ae) || ae.Stage != pmStageMarker {
			t.Fatalf("pmAdminExec on unmarked db = %v, want marker-stage pmAdminErr", err)
		}
		if dbExists(t, probe) {
			t.Fatalf("probe database %s was created despite the missing marker", probe)
		}
	})

	t.Run("distinct names", func(t *testing.T) {
		a, b := pmGuardPrefix(), pmGuardPrefix()
		if a == b {
			t.Fatal("two prefixes are identical")
		}
		if _, err := pmDBName(a, strings.Repeat("a", 40)); err == nil {
			t.Error("over-long name accepted")
		}
		if got, err := pmDBName(a, "_Mem_R1"); err != nil || got != a+"_mem_r1" {
			t.Errorf("uppercase suffix: got %q, %v", got, err)
		}
		for _, bad := range []string{"_a-b", `_a"b`, "_a b"} {
			if _, err := pmDBName(a, bad); err == nil {
				t.Errorf("suffix %q accepted", bad)
			}
		}
	})

	victim := labPrefix + "_victim"
	makeDB(t, victim)
	t.Run("collision", func(t *testing.T) {
		l := *lab
		l.t = t
		ran := false
		err := l.clone(ctx, "victim", func(string, string, *pmObserver) { ran = true })
		if err == nil {
			t.Fatal("clone over a pre-existing database returned nil")
		}
		if strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("a definite server ERROR was classified ambiguous: %v", err)
		}
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "42P04" {
			t.Fatalf("clone error = %v, want a *pgconn.PgError with code 42P04 (duplicate_database)", err)
		}
		if ran {
			t.Fatal("fn ran although the CREATE failed")
		}
	})
	if !dbExists(t, victim) {
		t.Fatalf("pre-existing database %s was dropped", victim)
	}

	foreign := "pm_" + strings.ReplaceAll(uuid.New().String(), "-", "") + "_foreign"
	makeDB(t, foreign)
	okName := labPrefix + "_ok"
	t.Cleanup(func() { // safety net: drop okName only if the clone's own cleanup left it behind
		if !dbExists(t, okName) {
			return
		}
		if err := pmDropDB(adminDSN, okName); err != nil {
			t.Errorf("drop %s: %v", okName, err)
		}
	})
	t.Run("owned-only cleanup", func(t *testing.T) {
		l := *lab
		l.t = t
		ran := false
		err := l.clone(ctx, "ok", func(name, _ string, _ *pmObserver) {
			ran = true
			if name != okName {
				t.Errorf("clone name %q, want %q", name, okName)
			}
			if !dbExists(t, name) {
				t.Errorf("clone %s does not exist inside fn", name)
			}
		})
		if err != nil {
			t.Fatalf("clone: %v", err)
		}
		if !ran {
			t.Fatal("fn did not run")
		}
	})
	if dbExists(t, okName) {
		t.Errorf("owned clone %s was not dropped", okName)
	}
	for _, keep := range []string{foreign, victim} {
		if !dbExists(t, keep) {
			t.Errorf("database %s that this lab did not create was dropped", keep)
		}
	}

	ambName := labPrefix + "_amb"
	t.Cleanup(func() {
		if err := pmDropDB(adminDSN, ambName); err != nil {
			t.Errorf("drop %s: %v", ambName, err)
		}
	})
	var recorded []string
	lab2 := *lab
	lab2.exec = func(ctx context.Context, dsn, sql string) error {
		recorded = append(recorded, sql)
		err := pmAdminExec(ctx, dsn, sql)
		if err == nil && strings.HasPrefix(sql, "CREATE DATABASE") {
			return errors.New("simulated lost acknowledgement")
		}
		return err
	}
	t.Run("ambiguous creation", func(t *testing.T) {
		lab2 := lab2
		lab2.t = t
		ran := false
		err := lab2.clone(ctx, "amb", func(string, string, *pmObserver) { ran = true })
		if err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "preserved") {
			t.Fatalf("clone error = %v, want ambiguous/preserved", err)
		}
		if ran {
			t.Fatal("fn ran although creation was ambiguous")
		}
	})
	for _, s := range recorded {
		if strings.Contains(s, "DROP") {
			t.Errorf("a DROP was issued after an ambiguous CREATE: %s", s)
		}
	}
	if !dbExists(t, ambName) {
		t.Errorf("database %s was not preserved after the ambiguous CREATE", ambName)
	}
}
