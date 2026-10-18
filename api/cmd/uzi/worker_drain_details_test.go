package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestWorkerDrainDetailsTerminalAndJSON(t *testing.T) {
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	id1 := "11111111-1111-4111-8111-111111111111"
	id2 := "33333333-3333-4333-8333-333333333333"
	detail := strings.Repeat("事实 ", 90) + "\u202e\x00\x1b\nFORGED\tROW " +
		id1 + " paused: submit input; " + id2 + " pool_wait: fund token; +7 omitted tail"
	second := "queued: wait for capacity"
	hidden := "DO-NOT-DISPLAY"
	workers := []apitypes.WorkerDTO{
		{ID: "worker-one", Kind: "hosted", DrainingSince: &at, UpgradeStatus: "up_to_date", UpgradeDetail: &detail},
		{ID: "worker-two", Kind: "hosted", DrainingSince: &at, UpgradeStatus: "unknown", UpgradeDetail: &second},
		{ID: "missing", Kind: "hosted", DrainingSince: &at},
		{ID: "not-draining", Kind: "hosted", UpgradeDetail: &hidden},
		{ID: "external", Kind: "external", DrainingSince: &at, UpgradeDetail: &hidden},
	}
	admin := make([]apitypes.AdminWorkerDTO, len(workers))
	for i, w := range workers {
		admin[i] = apitypes.AdminWorkerDTO{WorkerDTO: w, DiskPressureVolumes: []string{}}
	}
	for _, args := range [][]string{{"worker", "list"}, {"admin", "workers"}} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			fc := &uzicli.FakeClient{Workers: workers, AdminWorkers: admin}
			out, stderr, code := runCLI(t, fakeEnv(fc), args...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			for _, want := range []string{"Draining worker worker-one:", "Draining worker worker-two:", id1, id2, "paused: submit input;", "pool_wait: fund token;", "+7 omitted tail", second, "FORGED ROW"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q: %s", want, out)
				}
			}
			if strings.Contains(out, hidden) || strings.Count(out, "Draining worker ") != 2 || strings.Contains(out, "\nFORGED") {
				t.Fatalf("unexpected detail/forged row: %s", out)
			}
			for _, r := range out {
				if unicode.In(r, unicode.Cf) || (unicode.IsControl(r) && r != '\n' && r != '\t') {
					t.Fatalf("unsafe terminal rune %U", r)
				}
			}
			headers := strings.Fields(strings.Split(out, "\n")[0])
			if headers[len(headers)-1] != "OUTBOX" {
				t.Fatalf("header changed: %v", headers)
			}
			jsonArgs := append(append([]string{}, args...), "--json")
			out, _, code = runCLI(t, fakeEnv(fc), jsonArgs...)
			if code != 0 {
				t.Fatalf("JSON exit %d", code)
			}
			if args[0] == "worker" {
				var got []apitypes.WorkerDTO
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, workers) {
					t.Fatal("worker JSON changed")
				}
			} else {
				var got []apitypes.AdminWorkerDTO
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, admin) {
					t.Fatal("admin JSON changed")
				}
			}
		})
	}
}

func TestAdminHealthCapacitySplitsPathologicalWord(t *testing.T) {
	word := strings.Repeat("界", 401)
	check := apitypes.HealthCheckDTO{
		ID: "fleet.capacity", Scope: "owner", Severity: "warn",
		Evidence: []apitypes.HealthEvidenceDTO{{Label: "facts", Value: word + " tail"}},
	}
	out, stderr, code := runCLI(t, fakeEnv(&uzicli.FakeClient{AdminHealthDoc: healthDoc("warn", check)}), "admin", "health", "--no-color")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if strings.Count(out, "界") != 401 || !strings.Contains(out, "界 tail") || strings.Contains(out, "…") {
		t.Fatalf("pathological word lost text: %s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "界") && utf8.RuneCountInString(strings.TrimSpace(line)) > 200 {
			t.Fatalf("continuation exceeds 200 runes: %s", line)
		}
	}
}

func TestAdminHealthCapacityPreservesWholeEvidence(t *testing.T) {
	id1 := "11111111-1111-4111-8111-111111111111"
	id2 := "33333333-3333-4333-8333-333333333333"
	value := strings.Repeat("事实 ", 110) + "\u202e\x00\x1b\nFORGED\tROW " +
		id1 + " paused: submit input; " + id2 + " pool_wait: fund token; +9 omitted final-tail"
	check := apitypes.HealthCheckDTO{
		ID: "fleet.capacity", Scope: "owner", Severity: "danger",
		Evidence: []apitypes.HealthEvidenceDTO{{Label: "draining", Value: value}},
	}
	doc := healthDoc("danger", check)
	out, stderr, code := runCLI(t, fakeEnv(&uzicli.FakeClient{AdminHealthDoc: doc}), "admin", "health", "--no-color")
	if code != uzicli.ExitHealthDanger {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{id1, id2, "paused: submit input;", "pool_wait: fund token;", "+9 omitted final-tail", "FORGED ROW", "blocking: false (instance-wide)"} {
		if !strings.Contains(strings.Join(strings.Fields(out), " "), want) {
			t.Errorf("missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "…") || strings.Contains(out, "\nFORGED") {
		t.Fatalf("truncated/forged output: %s", out)
	}
	var evidence []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "DANGER") {
			start := strings.Index(line, "draining:")
			if start < 0 {
				t.Fatalf("missing label: %s", line)
			}
			line = line[start:]
		} else if line == "" || strings.HasPrefix(line, "SEVERITY") || strings.HasPrefix(line, "status:") || strings.HasPrefix(line, "blocking:") || strings.HasPrefix(line, "checks:") {
			continue
		}
		if utf8.RuneCountInString(line) > 200 {
			t.Fatalf("evidence row over cap: %s", line)
		}
		evidence = append(evidence, line)
	}
	want := "draining: " + strings.Repeat("事实 ", 110) + "FORGED ROW " + id1 + " paused: submit input; " + id2 + " pool_wait: fund token; +9 omitted final-tail"
	if strings.Join(evidence, " ") != want {
		t.Fatalf("evidence lost text: %q", strings.Join(evidence, " "))
	}
	for _, r := range out {
		if unicode.In(r, unicode.Cf) || (unicode.IsControl(r) && r != '\n' && r != '\t') {
			t.Fatalf("unsafe rune %U", r)
		}
	}
}
