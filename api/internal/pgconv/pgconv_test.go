package pgconv

import (
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
)

func ptr[T any](v T) *T { return &v }

func TestText(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantValid bool
		wantStr   string
	}{
		{"empty is valid empty, not null", "", true, ""},
		{"normal", "hello", true, "hello"},
		{"whitespace is a normal non-empty string", "   ", true, "   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Text(tt.in)
			if got.Valid != tt.wantValid {
				t.Errorf("Text(%q).Valid = %v, want %v", tt.in, got.Valid, tt.wantValid)
			}
			if got.String != tt.wantStr {
				t.Errorf("Text(%q).String = %q, want %q", tt.in, got.String, tt.wantStr)
			}
		})
	}
}

func TestTextOrNull(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantValid bool
		wantStr   string
	}{
		{"empty is null", "", false, ""},
		{"normal", "hello", true, "hello"},
		{"whitespace is a normal non-empty string", "   ", true, "   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TextOrNull(tt.in)
			if got.Valid != tt.wantValid {
				t.Errorf("TextOrNull(%q).Valid = %v, want %v", tt.in, got.Valid, tt.wantValid)
			}
			if got.String != tt.wantStr {
				t.Errorf("TextOrNull(%q).String = %q, want %q", tt.in, got.String, tt.wantStr)
			}
		})
	}
}

func TestTextPtr(t *testing.T) {
	tests := []struct {
		name      string
		in        *string
		wantValid bool
		wantStr   string
	}{
		{"nil is null", nil, false, ""},
		{"ptr to empty is valid empty", ptr(""), true, ""},
		{"normal", ptr("hello"), true, "hello"},
		{"whitespace is a normal non-empty string", ptr("   "), true, "   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TextPtr(tt.in)
			if got.Valid != tt.wantValid {
				t.Errorf("TextPtr(%v).Valid = %v, want %v", tt.in, got.Valid, tt.wantValid)
			}
			if got.String != tt.wantStr {
				t.Errorf("TextPtr(%v).String = %q, want %q", tt.in, got.String, tt.wantStr)
			}
		})
	}
}

func TestTextPtrOrNull(t *testing.T) {
	tests := []struct {
		name      string
		in        *string
		wantValid bool
		wantStr   string
	}{
		{"nil is null", nil, false, ""},
		{"ptr to empty is null", ptr(""), false, ""},
		{"normal", ptr("hello"), true, "hello"},
		{"whitespace is a normal non-empty string", ptr("   "), true, "   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TextPtrOrNull(tt.in)
			if got.Valid != tt.wantValid {
				t.Errorf("TextPtrOrNull(%v).Valid = %v, want %v", tt.in, got.Valid, tt.wantValid)
			}
			if got.String != tt.wantStr {
				t.Errorf("TextPtrOrNull(%v).String = %q, want %q", tt.in, got.String, tt.wantStr)
			}
		})
	}
}

func TestUUID(t *testing.T) {
	some := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	tests := []struct {
		name      string
		in        uuid.UUID
		wantValid bool
		wantBytes uuid.UUID
	}{
		{"nil uuid is valid all-zero, not null", uuid.Nil, true, uuid.Nil},
		{"normal", some, true, some},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UUID(tt.in)
			if got.Valid != tt.wantValid {
				t.Errorf("UUID(%v).Valid = %v, want %v", tt.in, got.Valid, tt.wantValid)
			}
			if got.Bytes != [16]byte(tt.wantBytes) {
				t.Errorf("UUID(%v).Bytes = %v, want %v", tt.in, got.Bytes, tt.wantBytes)
			}
		})
	}
}

func TestUUIDOrNull(t *testing.T) {
	some := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	tests := []struct {
		name      string
		in        uuid.UUID
		wantValid bool
		wantBytes uuid.UUID
	}{
		{"nil uuid is null", uuid.Nil, false, uuid.Nil},
		{"normal", some, true, some},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UUIDOrNull(tt.in)
			if got.Valid != tt.wantValid {
				t.Errorf("UUIDOrNull(%v).Valid = %v, want %v", tt.in, got.Valid, tt.wantValid)
			}
			if got.Bytes != [16]byte(tt.wantBytes) {
				t.Errorf("UUIDOrNull(%v).Bytes = %v, want %v", tt.in, got.Bytes, tt.wantBytes)
			}
		})
	}
}

func TestUUIDPtr(t *testing.T) {
	some := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	tests := []struct {
		name      string
		in        *uuid.UUID
		wantValid bool
		wantBytes uuid.UUID
	}{
		{"nil is null", nil, false, uuid.Nil},
		{"ptr to nil uuid is valid all-zero", ptr(uuid.Nil), true, uuid.Nil},
		{"normal", ptr(some), true, some},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UUIDPtr(tt.in)
			if got.Valid != tt.wantValid {
				t.Errorf("UUIDPtr(%v).Valid = %v, want %v", tt.in, got.Valid, tt.wantValid)
			}
			if got.Bytes != [16]byte(tt.wantBytes) {
				t.Errorf("UUIDPtr(%v).Bytes = %v, want %v", tt.in, got.Bytes, tt.wantBytes)
			}
		})
	}
}

func TestTime(t *testing.T) {
	some := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		in        time.Time
		wantValid bool
		wantTime  time.Time
	}{
		{"zero time is valid present, not null", time.Time{}, true, time.Time{}},
		{"normal", some, true, some},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Time(tt.in)
			if got.Valid != tt.wantValid {
				t.Errorf("Time(%v).Valid = %v, want %v", tt.in, got.Valid, tt.wantValid)
			}
			if !got.Time.Equal(tt.wantTime) {
				t.Errorf("Time(%v).Time = %v, want %v", tt.in, got.Time, tt.wantTime)
			}
		})
	}
}

func TestTimePtr(t *testing.T) {
	some := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		in        *time.Time
		wantValid bool
		wantTime  time.Time
	}{
		{"nil is null", nil, false, time.Time{}},
		{"normal", ptr(some), true, some},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TimePtr(tt.in)
			if got.Valid != tt.wantValid {
				t.Errorf("TimePtr(%v).Valid = %v, want %v", tt.in, got.Valid, tt.wantValid)
			}
			if !got.Time.Equal(tt.wantTime) {
				t.Errorf("TimePtr(%v).Time = %v, want %v", tt.in, got.Time, tt.wantTime)
			}
		})
	}
}

func TestBoolPtr(t *testing.T) {
	tests := []struct {
		name      string
		in        *bool
		wantValid bool
		wantBool  bool
	}{
		{"nil is null", nil, false, false},
		{"false value is valid", ptr(false), true, false},
		{"true value is valid", ptr(true), true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BoolPtr(tt.in)
			if got.Valid != tt.wantValid {
				t.Errorf("BoolPtr(%v).Valid = %v, want %v", tt.in, got.Valid, tt.wantValid)
			}
			if got.Bool != tt.wantBool {
				t.Errorf("BoolPtr(%v).Bool = %v, want %v", tt.in, got.Bool, tt.wantBool)
			}
		})
	}
}

func TestInt4Ptr(t *testing.T) {
	tests := []struct {
		name      string
		in        *int
		wantValid bool
		wantInt   int32
	}{
		{"nil is null", nil, false, 0},
		{"zero value is valid", ptr(0), true, 0},
		{"normal", ptr(7), true, 7},
		{"max int32 kept", ptr(math.MaxInt32), true, math.MaxInt32},
		{"above max saturates", ptr(math.MaxInt32 + 1), true, math.MaxInt32},
		{"below min saturates", ptr(math.MinInt32 - 1), true, math.MinInt32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Int4Ptr(tt.in)
			if got.Valid != tt.wantValid {
				t.Errorf("Int4Ptr(%v).Valid = %v, want %v", tt.in, got.Valid, tt.wantValid)
			}
			if got.Int32 != tt.wantInt {
				t.Errorf("Int4Ptr(%v).Int32 = %v, want %v", tt.in, got.Int32, tt.wantInt)
			}
		})
	}
}

func TestInt4Ptr32(t *testing.T) {
	tests := []struct {
		name      string
		in        *int32
		wantValid bool
		wantInt   int32
	}{
		{"nil is null", nil, false, 0},
		{"zero value is valid", ptr(int32(0)), true, 0},
		{"normal", ptr(int32(7)), true, 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Int4Ptr32(tt.in)
			if got.Valid != tt.wantValid {
				t.Errorf("Int4Ptr32(%v).Valid = %v, want %v", tt.in, got.Valid, tt.wantValid)
			}
			if got.Int32 != tt.wantInt {
				t.Errorf("Int4Ptr32(%v).Int32 = %v, want %v", tt.in, got.Int32, tt.wantInt)
			}
		})
	}
}

func TestInt8Ptr(t *testing.T) {
	tests := []struct {
		name      string
		in        *int64
		wantValid bool
		wantInt   int64
	}{
		{"nil is null", nil, false, 0},
		{"zero value is valid", ptr(int64(0)), true, 0},
		{"normal", ptr(int64(9000000000)), true, 9000000000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Int8Ptr(tt.in)
			if got.Valid != tt.wantValid {
				t.Errorf("Int8Ptr(%v).Valid = %v, want %v", tt.in, got.Valid, tt.wantValid)
			}
			if got.Int64 != tt.wantInt {
				t.Errorf("Int8Ptr(%v).Int64 = %v, want %v", tt.in, got.Int64, tt.wantInt)
			}
		})
	}
}

func TestFloat4Ptr(t *testing.T) {
	tests := []struct {
		name      string
		in        *float64
		wantValid bool
		wantFloat float32
	}{
		{"nil is null", nil, false, 0},
		{"zero value is valid", ptr(0.0), true, 0},
		{"normal", ptr(1.5), true, 1.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Float4Ptr(tt.in)
			if got.Valid != tt.wantValid {
				t.Errorf("Float4Ptr(%v).Valid = %v, want %v", tt.in, got.Valid, tt.wantValid)
			}
			if got.Float32 != tt.wantFloat {
				t.Errorf("Float4Ptr(%v).Float32 = %v, want %v", tt.in, got.Float32, tt.wantFloat)
			}
		})
	}
}
