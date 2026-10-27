package preset

import "testing"

// Pins the exact resolved quantities of every shipped preset (issue #3383 added
// l-highmem). The s/m/l rows must stay unchanged by that addition.
func TestResolveReturnsTheApprovedQuantitiesPerPreset(t *testing.T) {
	r, err := NewResolver("harbor.example.com/uzi", "v1")
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	for _, tc := range []struct {
		size                     string
		cpuReq, cpuLim           string
		memReq, memLim, dataSize string
	}{
		{"s", "250m", "1", "2Gi", "4Gi", "5Gi"},
		{"m", "1", "4", "8Gi", "12Gi", "25Gi"},
		{"l", "1", "4", "14Gi", "20Gi", "50Gi"},
		{"l-highmem", "1", "4", "24Gi", "32Gi", "50Gi"},
	} {
		spec, err := r.Resolve("base", tc.size)
		if err != nil {
			t.Fatalf("Resolve(base, %q): %v", tc.size, err)
		}
		got := spec.Size
		for field, pair := range map[string][2]string{
			"CPURequest":    {got.CPURequest.String(), tc.cpuReq},
			"CPULimit":      {got.CPULimit.String(), tc.cpuLim},
			"MemoryRequest": {got.MemoryRequest.String(), tc.memReq},
			"MemoryLimit":   {got.MemoryLimit.String(), tc.memLim},
			"DataSize":      {got.DataSize.String(), tc.dataSize},
		} {
			if pair[0] != pair[1] {
				t.Errorf("size %q %s = %s, want %s", tc.size, field, pair[0], pair[1])
			}
		}
	}
}
