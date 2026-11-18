package config

import (
	"maps"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func loadWithEnv(t *testing.T, env map[string]string) (Config, error) {
	t.Helper()
	setWorkerEnv(t)
	t.Setenv("UZI_API_URL", "https://uzi.example.com")
	t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
	for k, v := range env {
		t.Setenv(k, v)
	}
	return Load()
}

// Every "none" spelling loads as nil for the new-placement fields too, and a valid
// value parses without touching the global fields.
func TestLoadParsesTheNewPlacement(t *testing.T) {
	for raw, want := range map[string]map[string]string{
		"":                      nil,
		"null":                  nil,
		"{}":                    nil,
		`{"pool":"new"}`:        {"pool": "new"},
		`{"pool":"new","z":""}`: {"pool": "new", "z": ""},
	} {
		t.Run("selector/"+raw, func(t *testing.T) {
			cfg, err := loadWithEnv(t, map[string]string{"UZI_WORKER_NEW_PLACEMENT_NODE_SELECTOR": raw})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := cfg.WorkerNewPlacementNodeSelector
			if !maps.Equal(got, want) || (want == nil) != (got == nil) {
				t.Errorf("WorkerNewPlacementNodeSelector = %#v, want %#v", got, want)
			}
			if cfg.WorkerNodeSelector != nil {
				t.Errorf("global WorkerNodeSelector = %#v, want nil", cfg.WorkerNodeSelector)
			}
		})
	}
	for raw, want := range map[string][]corev1.Toleration{
		"":     nil,
		"null": nil,
		"[]":   nil,
		`[{"key":"new","operator":"Equal","value":"true","effect":"NoSchedule"}]`: {
			{Key: "new", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule},
		},
	} {
		t.Run("tolerations/"+raw, func(t *testing.T) {
			cfg, err := loadWithEnv(t, map[string]string{"UZI_WORKER_NEW_PLACEMENT_TOLERATIONS": raw})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !reflect.DeepEqual(cfg.WorkerNewPlacementTolerations, want) {
				t.Errorf("WorkerNewPlacementTolerations = %#v, want %#v", cfg.WorkerNewPlacementTolerations, want)
			}
			if cfg.WorkerTolerations != nil {
				t.Errorf("global WorkerTolerations = %#v, want nil", cfg.WorkerTolerations)
			}
		})
	}
}

// Bad new-placement values fail at boot and the error names the NEW variable.
func TestLoadRejectsABadNewPlacement(t *testing.T) {
	const sel = "UZI_WORKER_NEW_PLACEMENT_NODE_SELECTOR"
	const tol = "UZI_WORKER_NEW_PLACEMENT_TOLERATIONS"
	for name, tc := range map[string]struct{ env, raw string }{
		"selector malformed":    {sel, `{"pool":"a"`},
		"selector array":        {sel, `["pool"]`},
		"selector null value":   {sel, `{"pool":null}`},
		"selector bad key":      {sel, `{"bad key!":"a"}`},
		"selector bad value":    {sel, `{"pool":"not a label value"}`},
		"tolerations malformed": {tol, `[{"key":"uzi"}`},
		"tolerations Lt":        {tol, `[{"key":"uzi","operator":"Lt","value":"5"}]`},
		"tolerations Gt":        {tol, `[{"key":"uzi","operator":"Gt","value":"5"}]`},
		"tolerations trailing":  {tol, `[{"key":"uzi"}] []`},
		"tolerations unknown":   {tol, `[{"key":"uzi","efect":"NoSchedule"}]`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadWithEnv(t, map[string]string{tc.env: tc.raw})
			if err == nil {
				t.Fatalf("%s=%s loaded without error", tc.env, tc.raw)
			}
			if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("error %q does not name %s", err, tc.env)
			}
		})
	}
}

// The global variables keep their original error text after the parsers were
// parameterised by name.
func TestGlobalPlacementErrorsKeepTheirText(t *testing.T) {
	_, err := parseNodeSelector("UZI_WORKER_NODE_SELECTOR", `{"a":null}`)
	want := `UZI_WORKER_NODE_SELECTOR: key "a" has a null value; give it a string value or remove the key`
	if err == nil || err.Error() != want {
		t.Errorf("selector error = %v, want %q", err, want)
	}
	_, err = parseTolerations("UZI_WORKER_TOLERATIONS", `[{"key":"uzi"}] []`)
	want = `UZI_WORKER_TOLERATIONS=` + `"[{\"key\":\"uzi\"}] []"` + ` has trailing data after the array`
	if err == nil || err.Error() != want {
		t.Errorf("tolerations error = %v, want %q", err, want)
	}
}
