package schedtmpl

import (
	"strings"
	"testing"
)

func TestParseCapacityRemoval(t *testing.T) {
	base := "---\nslug: test\nname: Test\ndescription: Test\ntarget: sweep\ncron: */10 * * * *\nlabels: on-deck\n"
	for _, tc := range []struct {
		name, fields string
		valid        bool
	}{
		{"absent", "", true},
		{"capacity", "capacity_limit: 4\ncapacity_room_needed: 2\n", true},
		{"removal", "remove_label_on_dispatch: true\n", true},
		{"false", "remove_label_on_dispatch: false\n", true},
		{"limit only", "capacity_limit: 4\n", false},
		{"room only", "capacity_room_needed: 2\n", false},
		{"zero", "capacity_limit: 4\ncapacity_room_needed: 0\n", false},
		{"negative", "capacity_limit: -1\ncapacity_room_needed: 1\n", false},
		{"large", "capacity_limit: 51\ncapacity_room_needed: 2\n", false},
		{"inverted", "capacity_limit: 1\ncapacity_room_needed: 2\n", false},
		{"float", "capacity_limit: 4.0\ncapacity_room_needed: 2\n", false},
		{"quoted", "capacity_limit: \"4\"\ncapacity_room_needed: 2\n", false},
		{"null", "capacity_limit: null\ncapacity_room_needed: 2\n", false},
		{"bool number", "remove_label_on_dispatch: 1\n", false},
		{"bool quoted", "remove_label_on_dispatch: \"true\"\n", false},
		{"bool capitalized", "remove_label_on_dispatch: True\n", false},
		{"multi capacity", "labels: a,b\ncapacity_limit: 50\ncapacity_room_needed: 50\n", true},
		{"multi removal", "labels: a,b\nremove_label_on_dispatch: true\n", false},
		{"assigned capacity", "labels: \nselector: assigned\ncapacity_limit: 4\ncapacity_room_needed: 2\n", false},
		{"assigned removal", "labels: \nselector: assigned\nremove_label_on_dispatch: true\n", false},
		{"prompt capacity", "target: prompt\ncapacity_limit: 4\ncapacity_room_needed: 2\n", false},
		{"prompt removal", "target: prompt\nremove_label_on_dispatch: true\n", false},
		{"self improve removal", "target: self_improve\nremove_label_on_dispatch: true\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rawBase := base
			if strings.Contains(tc.fields, "selector: assigned") {
				rawBase = strings.ReplaceAll(base, "labels: on-deck\n", "")
			}
			_, err := parse([]byte(rawBase + tc.fields + "---\n\nGuidance.\n"))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	for _, old := range Catalog() {
		if old.Slug != "ondeck-sweep" && (old.CapacityLimit != 0 || old.CapacityRoomNeeded != 0 || old.RemoveLabelOnDispatch) {
			t.Fatalf("existing catalog default changed: %+v", old)
		}
	}
	job, ok := BySlug("ondeck-sweep")
	if !ok || job.Cron != "*/10 * * * *" || job.Timezone != "UTC" || job.MaxIssues != 1 || job.CapacityLimit != 4 || job.CapacityRoomNeeded != 2 || !job.RemoveLabelOnDispatch || strings.Join(job.Labels, ",") != "on-deck" {
		t.Fatalf("job=%+v", job)
	}
}
