package ids

import "testing"

func TestIsUUID(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"550e8400-e29b-41d4-a716-446655440000", "550E8400-E29B-41D4-A716-446655440000"} {
		if !IsUUID(s) {
			t.Errorf("IsUUID(%q) = false", s)
		}
	}
	for _, s := range []string{"not-a-uuid", "", "550e8400-e29b-41d4-a716", "550e8400-e29b-41d4-a716-446655440000\n", " 550e8400-e29b-41d4-a716-446655440000"} {
		if IsUUID(s) {
			t.Errorf("IsUUID(%q) = true", s)
		}
	}
}

func TestNewUUID(t *testing.T) {
	t.Parallel()
	a, b := NewUUID(), NewUUID()
	if !IsUUID(a) || a == b || a[14] != '4' {
		t.Errorf("NewUUID() = %q, %q", a, b)
	}
}
