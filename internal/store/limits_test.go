package store

import "testing"

// TestClampLimit: an unset or negative limit is the list's default, and a larger one is clamped to its maximum, not reset to the default.
func TestClampLimit(t *testing.T) {
	for _, c := range []struct{ in, want int }{{0, 50}, {-3, 50}, {1, 1}, {200, 200}, {201, 200}, {1 << 30, 200}} {
		if got := clampLimit(c.in, 50, 200); got != c.want {
			t.Errorf("clampLimit(%d, 50, 200) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestIsUUID(t *testing.T) {
	for s, want := range map[string]bool{
		"0b5f3c2e-8f6a-4d3b-9c1e-2a7f4e6d8b90":  true,
		"0B5F3C2E-8F6A-4D3B-9C1E-2A7F4E6D8B90":  true,
		"":                                      false,
		"nope":                                  false,
		"0b5f3c2e8f6a4d3b9c1e2a7f4e6d8b90":      false,
		"0b5f3c2e-8f6a-4d3b-9c1e-2a7f4e6d8b9g":  false,
		"0b5f3c2e_8f6a-4d3b-9c1e-2a7f4e6d8b90":  false,
		"{0b5f3c2e-8f6a-4d3b-9c1e-2a7f4e6d8b9}": false,
	} {
		if got := IsUUID(s); got != want {
			t.Errorf("IsUUID(%q) = %v, want %v", s, got, want)
		}
	}
}
