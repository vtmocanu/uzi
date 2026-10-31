package kube

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const PodEpochAnnotation = "uzi.dev/pod-epoch-evidence"
const PodEpochMaxDocument = 8 * 1024
const PodEpochRuntimeEnv = "UZI_POD_EPOCH_RUNTIME"

type PodEpochContainer struct {
	Name         string `json:"name"`
	ContainerID  string `json:"containerID"`
	ExitCode     int32  `json:"exitCode"`
	RestartCount int32  `json:"restartCount"`
}
type PodEpochClaim struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	VolumeName string `json:"volumeName"`
}
type PodEpochRemint struct {
	OperationID string `json:"operationID"`
	OldClaimUID string `json:"oldClaimUID"`
	NewClaimUID string `json:"newClaimUID"`
}
type PodEpochEvidence struct {
	Version          int                 `json:"version"`
	Session          string              `json:"session"`
	Predecessor      string              `json:"predecessor"`
	Successor        string              `json:"successor"`
	Containers       []PodEpochContainer `json:"containers"`
	InitContainers   []PodEpochContainer `json:"initContainers"`
	Data             PodEpochClaim       `json:"data"`
	DinD             PodEpochClaim       `json:"dind"`
	Maintenance      *PodEpochRemint     `json:"maintenance,omitempty"`
	Runtime          string              `json:"runtime"`
	SessionStarted   time.Time           `json:"sessionStarted"`
	TerminalObserved time.Time           `json:"terminalObserved"`
	SuccessorAdded   time.Time           `json:"successorAdded"`
	// SuccessorContainers and Attestation are version-2 only: required at
	// version 2 and forbidden at version 1 (enforced by DecodePodEpochDocument,
	// because podEpochShape treats omitempty fields as optional).
	SuccessorContainers []PodEpochSuccessorContainer `json:"successorContainers,omitempty"`
	Attestation         *PodEpochAttestation         `json:"attestation,omitempty"`
}

// PodEpochSuccessorContainer is a successor container identity observed by the
// controller; exactly name and containerID.
type PodEpochSuccessorContainer struct {
	Name        string `json:"name"`
	ContainerID string `json:"containerID"`
}

// PodEpochAttestation is the controller's cross-check of runtime prefixes of the
// predecessor and successor container IDs against the declared runtime.
type PodEpochAttestation struct {
	Result string `json:"result"`
	Reason string `json:"reason"`
}
type PodEpochNegative struct {
	Handoff string `json:"handoff"`
	Reason  string `json:"reason"`
}

var podEpochUUID = regexp.MustCompile("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")

func podEpochRuntime(s string) bool { return s == "containerd-runc" || s == "crio-crun" }

// DecodePodEpochDocument is the worker wire contract for versions 1 and 2. Version 2
// adds required successorContainers and attestation, which version 1 must not carry.
// It rejects duplicate keys at every depth, nulls, unknown/missing fields, and
// trailing JSON. A decoded document alone grants no cleanup authority.
func DecodePodEpochDocument(b []byte) (*PodEpochEvidence, *PodEpochNegative, error) {
	if len(b) == 0 || len(b) > PodEpochMaxDocument || !utf8.Valid(b) {
		return nil, nil, errors.New("document size")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	if err := podEpochTokens(d); err != nil {
		return nil, nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, nil, errors.New("trailing JSON")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return nil, nil, err
	}
	if _, ok := fields["handoff"]; ok {
		var n PodEpochNegative
		if err := podEpochShape(b, reflect.TypeOf(n)); err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal(b, &n); err != nil {
			return nil, nil, err
		}
		if n.Handoff != "none" || strings.TrimSpace(n.Reason) == "" {
			return nil, nil, errors.New("negative document")
		}
		return nil, &n, nil
	}
	var e PodEpochEvidence
	if err := podEpochShape(b, reflect.TypeOf(e)); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, nil, err
	}
	if (e.Version != 1 && e.Version != 2) || !podEpochUUID.MatchString(e.Session) || !podEpochUUID.MatchString(e.Predecessor) || !podEpochUUID.MatchString(e.Successor) || e.Predecessor == e.Successor || !podEpochRuntime(e.Runtime) {
		return nil, nil, errors.New("identity or runtime")
	}
	if !podEpochValidClaim(e.Data) || !podEpochValidClaim(e.DinD) || e.Data.Name == e.DinD.Name {
		return nil, nil, errors.New("claims")
	}
	if !podEpochValidEntries(e.Containers, true) || !podEpochValidEntries(e.InitContainers, false) {
		return nil, nil, errors.New("containers")
	}
	if e.SessionStarted.IsZero() || e.TerminalObserved.Before(e.SessionStarted) || e.SuccessorAdded.Before(e.TerminalObserved) {
		return nil, nil, errors.New("times")
	}
	if m := e.Maintenance; m != nil {
		if strings.TrimSpace(m.OperationID) == "" || !podEpochUUID.MatchString(m.OldClaimUID) || !podEpochUUID.MatchString(m.NewClaimUID) || m.OldClaimUID == m.NewClaimUID || m.NewClaimUID != e.DinD.UID {
			return nil, nil, errors.New("maintenance")
		}
	}
	if e.Version == 1 {
		if e.SuccessorContainers != nil || e.Attestation != nil {
			return nil, nil, errors.New("version 1 carries version 2 fields")
		}
		return &e, nil, nil
	}
	if e.Attestation == nil || (e.Attestation.Result != "consistent" && e.Attestation.Result != "contradicted") || strings.TrimSpace(e.Attestation.Reason) == "" {
		return nil, nil, errors.New("attestation")
	}
	if len(e.SuccessorContainers) == 0 {
		return nil, nil, errors.New("successor containers")
	}
	seen := map[string]bool{}
	for _, c := range e.SuccessorContainers {
		if c.Name == "" || c.ContainerID == "" || seen[c.Name] {
			return nil, nil, errors.New("successor containers")
		}
		seen[c.Name] = true
	}
	return &e, nil, nil
}
func podEpochValidClaim(c PodEpochClaim) bool {
	return c.Name != "" && podEpochUUID.MatchString(c.UID) && c.VolumeName != ""
}
func podEpochValidEntries(es []PodEpochContainer, required bool) bool {
	if required && len(es) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, e := range es {
		if e.Name == "" || seen[e.Name] || e.ContainerID == "" || e.ExitCode < 0 || e.ExitCode == 137 || e.ExitCode == 255 || e.RestartCount < 0 {
			return false
		}
		seen[e.Name] = true
	}
	return true
}
func podEpochTokens(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	if t == nil {
		return errors.New("null")
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return err
			}
			s, ok := k.(string)
			if !ok || seen[s] {
				return errors.New("duplicate key")
			}
			seen[s] = true
			if err := podEpochTokens(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := podEpochTokens(d); err != nil {
				return err
			}
		}
	default:
		return errors.New("structure")
	}
	_, err = d.Token()
	return err
}
func podEpochShape(b []byte, t reflect.Type) error {
	if t.Kind() == reflect.Pointer {
		return podEpochShape(b, t.Elem())
	}
	if t == reflect.TypeFor[time.Time]() {
		return nil
	}
	if t.Kind() == reflect.Slice {
		var a []json.RawMessage
		if err := json.Unmarshal(b, &a); err != nil {
			return err
		}
		for _, v := range a {
			if err := podEpochShape(v, t.Elem()); err != nil {
				return err
			}
		}
		return nil
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")
		v, ok := m[tag[0]]
		if !ok {
			if len(tag) > 1 && tag[1] == "omitempty" {
				continue
			}
			return fmt.Errorf("missing %s", tag[0])
		}
		if err := podEpochShape(v, f.Type); err != nil {
			return err
		}
		delete(m, tag[0])
	}
	if len(m) != 0 {
		return errors.New("unknown field")
	}
	return nil
}

// EncodePodEpochEvidence never returns an oversize positive document.
func EncodePodEpochEvidence(e PodEpochEvidence) ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	_, _, err = DecodePodEpochDocument(b)
	if err != nil {
		return nil, err
	}
	return b, nil
}
