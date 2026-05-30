package queries

import "testing"

func TestWindowSeconds(t *testing.T) {
	cases := []struct {
		in   Window
		want float64
		ok   bool
	}{
		{"24h", 86400, true},
		{"7d", 604800, true},
		{"30d", 2592000, true},
		{"1w", 604800, true},
		{"5h", 18000, true},
		{"bogus", 0, false},
		{"1", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, err := c.in.Seconds()
		if c.ok && err != nil {
			t.Errorf("Window(%q).Seconds(): unexpected err %v", c.in, err)
			continue
		}
		if !c.ok && err == nil {
			t.Errorf("Window(%q).Seconds() = %v, expected error", c.in, got)
			continue
		}
		if c.ok && got != c.want {
			t.Errorf("Window(%q).Seconds() = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestValidateWindow(t *testing.T) {
	if err := ValidateWindow("7d"); err != nil {
		t.Errorf("7d should be valid: %v", err)
	}
	if err := ValidateWindow("garbage"); err == nil {
		t.Errorf("garbage should be invalid")
	}
}
