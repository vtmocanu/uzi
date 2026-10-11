package runprogress

import (
	"reflect"
	"testing"
)

func TestIssueRefs(t *testing.T) {
	cases := []struct {
		name  string
		texts []string
		self  int64
		want  []int64
	}{
		{"none", []string{"no refs here"}, 0, nil},
		{"empty input", nil, 0, nil},
		{"single", []string{"waiting on #2512 to land"}, 0, []int64{2512}},
		{"first-seen order across texts", []string{"#30 and #10", "then #20"}, 0, []int64{30, 10, 20}},
		{"dedupe", []string{"#7 #7 #8 #7"}, 0, []int64{7, 8}},
		{"drops self", []string{"is #5 done? and #6?"}, 5, []int64{6}},
		{"only self", []string{"my own #5"}, 5, nil},
		{"self zero keeps others", []string{"#5"}, 0, []int64{5}},
		{"drops zero", []string{"#0 #000 #1"}, 0, []int64{1}},
		{"nine digits kept", []string{"#999999999"}, 0, []int64{999999999}},
		{"ten digits dropped", []string{"#1000000000 #3"}, 0, []int64{3}},
		{"overlong digit run dropped whole", []string{"#99999999999999999999999 #4"}, 0, []int64{4}},
		{"hash without digits", []string{"# 12 #x #", "#"}, 0, nil},
		{"word char before hash counts", []string{"abc#12"}, 0, []int64{12}},
		{"trailing letters ignored", []string{"#12abc"}, 0, []int64{12}},
		{"leading zeros parse", []string{"#007"}, 0, []int64{7}},
		{"adjacent hashes", []string{"##9 #8#6"}, 0, []int64{9, 8, 6}},
		{"cap at five", []string{"#1 #2 #3 #4 #5 #6 #7"}, 0, []int64{1, 2, 3, 4, 5}},
		{"cap counts only kept values", []string{"#0 #1 #1 #2 #3 #4 #5 #6"}, 0, []int64{1, 2, 3, 4, 5}},
		{"non-ascii digits ignored", []string{"#١٢"}, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IssueRefs(tc.texts, tc.self)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("IssueRefs(%q, %d) = %v, want %v", tc.texts, tc.self, got, tc.want)
			}
		})
	}
}
