package tfplan

import "testing"

func TestIsActionable(t *testing.T) {
	cases := []struct {
		actions []string
		want    bool
	}{
		{nil, false},
		{[]string{"no-op"}, false},
		{[]string{"read"}, false},
		{[]string{"update"}, true},
		{[]string{"create"}, true},
		{[]string{"delete"}, true},
		{[]string{"delete", "create"}, true}, // replace
		{[]string{"no-op", "read"}, false},
	}
	for _, c := range cases {
		if got := isActionable(c.actions); got != c.want {
			t.Errorf("isActionable(%v) = %v, want %v", c.actions, got, c.want)
		}
	}
}
