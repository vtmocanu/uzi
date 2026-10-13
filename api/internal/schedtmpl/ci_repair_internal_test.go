package schedtmpl

import (
	"strings"
	"testing"
)

func TestCIRepairCatalogApproval(t *testing.T) {
	for _, job := range Catalog() {
		if job.Slug == "ci-repair" {
			if job.AutoApprove || job.Target != "ci_watch" || job.Cron != "0 * * * *" || job.Timezone != "UTC" || job.Prompt == "" {
				t.Fatalf("CI catalog entry = %+v", job)
			}
			for _, instruction := range []string{"untrusted", "Reproduce", "Classify", "smallest", "regression test", "CI-config", "publishing guard", "re-run", "report only"} {
				if !strings.Contains(job.Prompt, instruction) {
					t.Errorf("prompt missing %q", instruction)
				}
			}
		} else if !job.AutoApprove {
			t.Errorf("legacy job %s no longer auto-approves", job.Slug)
		}
	}
	if !AutoApprove || !WaitOnLimit {
		t.Fatal("migration constants changed")
	}
}

func TestParseAutoApprove(t *testing.T) {
	base := "---\nslug: approval\nname: Approval\ndescription: Test approval.\ntarget: ci_watch\ncron: 0 * * * *\n"
	for _, tc := range []struct {
		field   string
		want    bool
		invalid bool
	}{
		{"", true, false}, {"auto_approve: true\n", true, false},
		{"auto_approve: false\n", false, false}, {"auto_approve: invalid\n", false, true},
	} {
		job, err := parse([]byte(base + tc.field + "---\n\nRepair CI.\n"))
		if tc.invalid {
			if err == nil {
				t.Fatal("accepted invalid boolean")
			}
			continue
		}
		if err != nil || job.AutoApprove != tc.want || job.Prompt != "Repair CI." {
			t.Fatalf("parse %q: %+v, %v", tc.field, job, err)
		}
	}
}
