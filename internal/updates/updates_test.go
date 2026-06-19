package updates

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.0.1", -1},
		{"1.0.0", "1.0.0", 0},
		{"1.2.0", "1.10.0", -1}, // numeric, not lexical
		{"2.0", "1.9.9", 1},
		{"1.0", "1.0.0", 0},
		{"1.0.0-rc1", "1.0.0", 0}, // pre-release suffix ignored in segment
	}
	for _, c := range cases {
		if got := compare(c.a, c.b); got != c.want {
			t.Errorf("compare(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"v1.2.3": "1.2.3", "V1.0": "1.0", " 1.0.0 ": "1.0.0", "1.0": "1.0",
	} {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q)=%q want %q", in, got, want)
		}
	}
}
