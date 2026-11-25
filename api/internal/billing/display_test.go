package billing

import "testing"

func TestDisplayNumber(t *testing.T) {
	cases := []struct {
		mode, fy string
		n        int32
		want     string
	}{
		{"ESTIMATE", "2083.084", 12, "E-2083/84-12"},
		{"TAX_INVOICE", "2083.084", 1, "2083/84-1"},
		{"TAX_INVOICE", "2099.100", 7, "2099/00-7"},
		{"ESTIMATE", "odd", 3, "E-odd-3"}, // unknown form passes through
	}
	for _, c := range cases {
		if got := displayNumber(c.mode, c.fy, c.n); got != c.want {
			t.Errorf("displayNumber(%s, %s, %d) = %q, want %q", c.mode, c.fy, c.n, got, c.want)
		}
	}
}
