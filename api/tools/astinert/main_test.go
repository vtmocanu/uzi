package main

import "testing"

func TestInert(t *testing.T) {
	tests := []struct {
		name      string
		a         string
		b         string
		wantInert bool
		wantErr   bool
	}{
		{
			name: "comment line added is inert",
			a: `package p

func f() {
	x := 1
	_ = x
}
`,
			b: `package p

// a brand new prose comment line
func f() {
	x := 1
	_ = x
}
`,
			wantInert: true,
		},
		{
			name: "indentation and blank-line differences are inert",
			a: `package p

func f() {
	x := 1
	_ = x
}
`,
			b: `package p
func f() {
        x := 1
        _ = x
}
`,
			wantInert: true,
		},
		{
			name: "comment removed is inert",
			a: `package p

// leading doc comment
func f() { _ = 1 }
`,
			b: `package p

func f() { _ = 1 }
`,
			wantInert: true,
		},
		{
			name: "commented-out statement differs",
			a: `package p

func f(got, want int) {
	if got != want {
		panic("mismatch")
	}
}
`,
			b: `package p

func f(got, want int) {
	// if got != want {
	// 	panic("mismatch")
	// }
}
`,
			wantInert: false,
		},
		{
			name: "changed literal value differs",
			a: `package p

func f() int {
	x := 1
	return x
}
`,
			b: `package p

func f() int {
	x := 2
	return x
}
`,
			wantInert: false,
		},
		{
			name: "parse error is propagated",
			a: `package p

func f() { _ = 1 }
`,
			b:       `this is not valid go`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := inert("a.go", []byte(tt.a), "b.go", []byte(tt.b))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("inert() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("inert() unexpected error: %v", err)
			}
			if got != tt.wantInert {
				t.Errorf("inert() = %v, want %v", got, tt.wantInert)
			}
		})
	}
}
