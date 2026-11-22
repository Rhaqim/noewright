package dsnenv

import "testing"

func TestGet(t *testing.T) {
	cases := []struct {
		name, cur, legacy, want string
	}{
		{"neither", "", "", ""},
		{"legacy only", "", "old", "old"},
		{"current only", "new", "", "new"},
		{"current wins", "new", "old", "new"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(Var, c.cur)
			t.Setenv(LegacyVar, c.legacy)
			if got := Get(); got != c.want {
				t.Fatalf("Get() = %q, want %q", got, c.want)
			}
		})
	}
}
