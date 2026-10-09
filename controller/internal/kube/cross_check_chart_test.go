package kube

import (
	"bytes"
	"os"
	"regexp"
	"strconv"
	"testing"
	"text/template"

	"sigs.k8s.io/yaml"
)

// Exercise the chart's actual env stanza with Helm's quote semantics. Full chart
// rendering still belongs to the maintainer's Helm/k8s validation.
func TestCrossCheckChartAndComposeWiring(t *testing.T) {
	raw, err := os.ReadFile("../../../deploy/chart/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Workers struct {
			CrossCheckSlots   *int
			MaxConcurrentRuns int
		}
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	if values.Workers.CrossCheckSlots == nil || *values.Workers.CrossCheckSlots != 1 {
		t.Fatal("chart must explicitly default crossCheckSlots to 1")
	}
	if values.Workers.MaxConcurrentRuns != 1 {
		t.Fatal("run cap default changed")
	}
	raw, err = os.ReadFile("../../../deploy/chart/templates/controller-deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// A direct, unconditional stanza is required: with/if/default treat 0 as empty.
	re := regexp.MustCompile("(?m)^            - name: UZI_WORKER_CROSS_CHECK_SLOTS\n              value: (.+)$")
	match := re.FindStringSubmatch(string(raw))
	if len(match) != 2 {
		t.Fatal("missing direct cross-check slot env stanza")
	}
	if match[1] != "{{ .Values.workers.crossCheckSlots | quote }}" {
		t.Fatalf("slot expression %q must relay the value without defaulting zero", match[1])
	}
	tmpl, err := template.New("env").Funcs(template.FuncMap{
		"quote": func(v int) string { return strconv.Quote(strconv.Itoa(v)) },
	}).Parse(match[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, slots := range []int{1, 0, 16} {
		var out bytes.Buffer
		data := map[string]any{"Values": map[string]any{"workers": map[string]any{"crossCheckSlots": slots}}}
		if err := tmpl.Execute(&out, data); err != nil {
			t.Fatal(err)
		}
		var entries []struct {
			Name  string
			Value string
		}
		if err := yaml.Unmarshal(out.Bytes(), &entries); err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("rendered %d env entries, want one", len(entries))
		}
		env := entries[0]
		if env.Name != "UZI_WORKER_CROSS_CHECK_SLOTS" || env.Value != strconv.Itoa(slots) {
			t.Fatalf("rendered env = %+v, want slots %d", env, slots)
		}
	}
	raw, err = os.ReadFile("../../../docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]struct {
			Environment map[string]string
		}
	}
	if err := yaml.Unmarshal(raw, &compose); err != nil {
		t.Fatal(err)
	}
	if got := compose.Services["agent"].Environment["WORKER_CROSS_CHECK_SLOTS"]; got != "${WORKER_CROSS_CHECK_SLOTS:-1}" {
		t.Fatalf("compose slots = %q, want shell default 1 preserving explicit zero", got)
	}
}
