package composeparity

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// Untyped decoding preserves scalar types and literal interpolation strings,
// while ignoring comments and mapping order. yaml.v3 rejects duplicate keys.
func parseCompose(data []byte) (map[string]any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var doc map[string]any
	if err := decoder.Decode(&doc); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("empty Compose document")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("expected one YAML document, trailing decode: %v", err)
	}
	return doc, nil
}

func readCompose(path string) (map[string]any, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: callers pass fixed repo paths or a t.TempDir path.
	if err != nil {
		return nil, err
	}
	doc, err := parseCompose(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return doc, nil
}

func documents(t *testing.T) (map[string]any, map[string]any) {
	t.Helper()
	root, err := readCompose("../../../docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	server, err := readCompose("../../../deploy/compose/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return root, server
}

// Only these differences are approved. Retain every mount other than the
// exact DinD socket mount, and every setting not explicitly removed.
func serverFromRoot(root map[string]any) map[string]any {
	services := root["services"].(map[string]any)
	for service, image := range map[string]string{"api": "api", "web": "web", "agent": "agent-base"} {
		config := services[service].(map[string]any)
		delete(config, "build")
		config["image"] = "ghcr.io/vtmocanu/uzi/" + image + ":${UZI_VERSION:?set UZI_VERSION in .env}"
	}
	delete(services, "dind-init")
	delete(services, "dind")
	agent := services["agent"].(map[string]any)
	delete(agent["environment"].(map[string]any), "UZI_DIND_SOCKET")
	mounts := agent["volumes"].([]any)
	kept := make([]any, 0, len(mounts))
	for _, mount := range mounts {
		if mount != "dindsock:/run/dind" {
			kept = append(kept, mount)
		}
	}
	agent["volumes"] = kept
	volumes := root["volumes"].(map[string]any)
	delete(volumes, "dindsock")
	delete(volumes, "dinddata")
	// A server comes back after a reboot; the laptop stack stays down.
	for _, service := range restartServices {
		services[service].(map[string]any)["restart"] = "unless-stopped"
	}
	return root
}

var restartServices = []string{"db", "api", "web", "agent"}

func parity(root, server map[string]any) error {
	expected := serverFromRoot(root)
	if !reflect.DeepEqual(expected, server) {
		return fmt.Errorf("server differs from exact root transformation\nwant: %#v\ngot:  %#v", expected, server)
	}
	return nil
}

func at(doc map[string]any, path ...string) any {
	var value any = doc
	for _, key := range path {
		mapping, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = mapping[key]
	}
	return value
}

func noBuild(value any) error {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "build" {
				return fmt.Errorf("build key present")
			}
			if err := noBuild(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := noBuild(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func invariants(doc map[string]any) error {
	if err := noBuild(doc); err != nil {
		return err
	}
	checks := []struct {
		path []string
		want any
	}{
		{[]string{"services", "api", "image"}, "ghcr.io/vtmocanu/uzi/api:${UZI_VERSION:?set UZI_VERSION in .env}"},
		{[]string{"services", "web", "image"}, "ghcr.io/vtmocanu/uzi/web:${UZI_VERSION:?set UZI_VERSION in .env}"},
		{[]string{"services", "agent", "image"}, "ghcr.io/vtmocanu/uzi/agent-base:${UZI_VERSION:?set UZI_VERSION in .env}"},
		{[]string{"services", "web", "ports"}, []any{"127.0.0.1:8080:8080"}},
		{[]string{"services", "agent", "profiles"}, []any{"agent"}},
		{[]string{"services", "agent", "cpus"}, "${AGENT_CPUS:-2}"},
		{[]string{"services", "agent", "mem_limit"}, "${AGENT_MEM_LIMIT:-4g}"},
		{[]string{"services", "db", "volumes"}, []any{"pgdata:/var/lib/postgresql/data"}},
		{[]string{"services", "agent", "volumes"}, []any{"agentdata:/data", "agentnix:/nix"}},
		{[]string{"volumes"}, map[string]any{"pgdata": nil, "agentdata": nil, "agentnix": nil}},
		{[]string{"services", "agent", "secrets"}, []any{"worker_token"}},
		{[]string{"services", "agent", "environment", "UZI_WORKER_TOKEN_FILE"}, "/run/secrets/worker_token"},
		{[]string{"secrets"}, map[string]any{"worker_token": map[string]any{"environment": "UZI_WORKER_TOKEN"}}},
		{[]string{"services", "agent", "security_opt"}, []any{"no-new-privileges:true"}},
		{[]string{"services", "agent", "cap_drop"}, []any{"ALL"}},
		{[]string{"services", "agent", "cap_add"}, []any{"SETUID", "SETGID", "SETPCAP", "CHOWN", "DAC_OVERRIDE"}},
	}
	for _, check := range checks {
		if got := at(doc, check.path...); !reflect.DeepEqual(got, check.want) {
			return fmt.Errorf("%v: got %#v, want %#v", check.path, got, check.want)
		}
	}
	for _, service := range restartServices {
		if got := at(doc, "services", service, "restart"); got != "unless-stopped" {
			return fmt.Errorf("%s restart: got %#v, want \"unless-stopped\"", service, got)
		}
	}
	for _, service := range []string{"api", "db"} {
		config, ok := at(doc, "services", service).(map[string]any)
		if !ok {
			return fmt.Errorf("missing service %s", service)
		}
		if _, exists := config["ports"]; exists {
			return fmt.Errorf("%s publishes ports", service)
		}
	}
	return nil
}

func TestServerComposeParity(t *testing.T) {
	root, server := documents(t)
	if err := parity(root, server); err != nil {
		t.Fatal(err)
	}
}

func TestServerComposeInvariants(t *testing.T) {
	_, server := documents(t)
	if err := invariants(server); err != nil {
		t.Fatal(err)
	}
}

func TestParityControls(t *testing.T) {
	cases := map[string]struct {
		path        []string
		key         string
		value       any // nil deletes the key
		independent bool
	}{
		"required env":         {[]string{"services", "api", "environment"}, "NEW_REQUIRED", "${NEW_REQUIRED:?required}", false},
		"default drift":        {[]string{"services", "api", "environment"}, "AUTH_TOKEN_TTL", "${AUTH_TOKEN_TTL:-1h}", false},
		"digest drift":         {[]string{"services", "db"}, "image", "postgres:17@sha256:changed", false},
		"missing mount":        {[]string{"services", "agent"}, "volumes", []any{"agentdata:/data"}, true},
		"missing healthcheck":  {[]string{"services", "api"}, "healthcheck", nil, false},
		"api port":             {[]string{"services", "api"}, "ports", []any{"8080:8080"}, true},
		"db port":              {[]string{"services", "db"}, "ports", []any{"5432:5432"}, true},
		"nested build":         {[]string{"services", "api", "environment"}, "build", "unexpected", true},
		"broken secret source": {[]string{"secrets", "worker_token"}, "environment", "WRONG_TOKEN", true},
		"broken secret mount":  {[]string{"services", "agent"}, "secrets", nil, true},
		"broken token path":    {[]string{"services", "agent", "environment"}, "UZI_WORKER_TOKEN_FILE", "/wrong", true},
		"unexpected service":   {[]string{"services"}, "extra", map[string]any{"image": "unexpected"}, false},
		"unexpected key":       {nil, "unexpected", true, false},
		"scalar type":          {[]string{"services", "db", "healthcheck"}, "retries", "10", false},
		"image version":        {[]string{"services", "web"}, "image", "ghcr.io/vtmocanu/uzi/web:latest", true},
		"public web":           {[]string{"services", "web"}, "ports", []any{"8080:8080"}, true},
		"profile":              {[]string{"services", "agent"}, "profiles", nil, true},
		"resource limit":       {[]string{"services", "agent"}, "mem_limit", nil, true},
		"hardening":            {[]string{"services", "agent"}, "cap_drop", nil, true},
		"missing restart":      {[]string{"services", "api"}, "restart", nil, true},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			root, server := documents(t)
			parent := at(server, change.path...).(map[string]any)
			if change.value == nil {
				delete(parent, change.key)
			} else {
				parent[change.key] = change.value
			}
			if err := parity(root, server); err == nil {
				t.Fatal("accepted server drift")
			}
			// Independent safety assertions also reject drift even if parity's
			// source of truth were changed to match it.
			if change.independent {
				if err := invariants(server); err == nil {
					t.Fatal("invariant accepted unsafe configuration")
				}
			}
		})
	}
	t.Run("new root setting must propagate", func(t *testing.T) {
		root, server := documents(t)
		at(root, "services", "api", "environment").(map[string]any)["NEW_REQUIRED"] = "${NEW_REQUIRED:?required}"
		if err := parity(root, server); err == nil {
			t.Fatal("accepted missing root setting")
		}
	})
	t.Run("identical updates accepted", func(t *testing.T) {
		root, server := documents(t)
		for _, doc := range []map[string]any{root, server} {
			at(doc, "services", "api", "environment").(map[string]any)["NEW_REQUIRED"] = "${NEW_REQUIRED:?required}"
		}
		if err := parity(root, server); err != nil {
			t.Fatal(err)
		}
	})
}

func TestComposeInputValidation(t *testing.T) {
	for name, input := range map[string]string{
		"malformed":                   "services: [",
		"duplicate":                   "services:\n  api: {}\n  api: {}\n",
		"trailing document":           "services: {}\n---\nvolumes: {}\n",
		"trailing empty document":     "services: {}\n---\n",
		"trailing malformed document": "services: {}\n---\n[",
		"empty":                       "",
		"non mapping":                 "- services\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseCompose([]byte(input)); err == nil {
				t.Fatal("accepted invalid YAML")
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		if _, err := readCompose(filepath.Join(t.TempDir(), "missing.yml")); err == nil {
			t.Fatal("accepted missing file")
		}
	})
	t.Run("types and literals", func(t *testing.T) {
		doc, err := parseCompose([]byte("integer: 10\nboolean: true\nnullable: null\nstring: '10'\nliteral: '${VALUE:?required}'\n"))
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"integer": 10, "boolean": true, "nullable": nil, "string": "10", "literal": "${VALUE:?required}"}
		if !reflect.DeepEqual(doc, want) {
			t.Fatalf("lost YAML types or interpolation: %#v", doc)
		}
	})
}
