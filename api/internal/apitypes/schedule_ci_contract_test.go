package apitypes

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestScheduleCIWorkflowsPresence(t *testing.T) {
	for _, tc := range []struct {
		body    string
		present bool
		value   []string
	}{
		{`{}`, false, nil},
		{`{"ci_workflows":null}`, true, nil},
		{`{"ci_workflows":[]}`, true, []string{}},
		{`{"ci_workflows":["nightly","123"]}`, true, []string{"nightly", "123"}},
	} {
		t.Run(tc.body, func(t *testing.T) {
			var request ScheduleRequest
			if err := json.Unmarshal([]byte(tc.body), &request); err != nil {
				t.Fatal(err)
			}
			if request.CIWorkflows.Present != tc.present || !reflect.DeepEqual(request.CIWorkflows.Value, tc.value) {
				t.Fatalf("presence/value = %+v", request.CIWorkflows)
			}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			_, present := fields["ci_workflows"]
			if present != tc.present {
				t.Fatalf("marshal presence = %t", present)
			}
		})
	}
	for _, body := range []string{`{"ci_workflows":42}`, `{"ci_workflows":["nightly",42]}`} {
		var request ScheduleRequest
		if err := json.Unmarshal([]byte(body), &request); err == nil {
			t.Fatalf("accepted invalid selector shape: %s", body)
		}
	}
}

func TestScheduleCIEpisodesOptional(t *testing.T) {
	raw, err := json.Marshal(ScheduleDTO{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["ci_episodes"]; present {
		t.Fatal("ordinary schedule carries episode page")
	}
	cursor := CIEpisodeCursor("nightly")
	raw, err = json.Marshal(CIEpisodePage{Episodes: []CIEpisodeDTO{}, NextCursor: &cursor})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"episodes":[],"next_cursor":"nightly"}` {
		t.Fatalf("page shape: %s", raw)
	}
}
