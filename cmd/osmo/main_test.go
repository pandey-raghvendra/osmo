package main

import (
	"testing"

	"github.com/pandey-raghvendra/osmo/internal/absorb"
	"github.com/pandey-raghvendra/osmo/internal/tfplan"
)

func addrs(drifts []tfplan.Drift) []string {
	out := make([]string, len(drifts))
	for i, d := range drifts {
		out[i] = d.Address
	}
	return out
}

func TestMatchesAny(t *testing.T) {
	cases := []struct {
		addr string
		sel  []string
		want bool
	}{
		{"aws_instance.web", []string{"aws_instance.web"}, true},
		{"aws_instance.web", []string{"aws_instance.db"}, false},
		{"aws_instance.web[0]", []string{"aws_instance.web"}, true},
		{"module.app.aws_instance.web", []string{"module.app"}, true},
		{"module.appendix.aws_instance.web", []string{"module.app"}, false}, // no false prefix match
		{"aws_instance.website", []string{"aws_instance.web"}, false},       // no substring match
		{"aws_instance.web", nil, false},
	}
	for _, c := range cases {
		if got := matchesAny(c.addr, c.sel); got != c.want {
			t.Errorf("matchesAny(%q, %v) = %v, want %v", c.addr, c.sel, got, c.want)
		}
	}
}

func TestFilterDrifts(t *testing.T) {
	drifts := []tfplan.Drift{
		{Address: "aws_instance.web"},
		{Address: "aws_instance.db"},
		{Address: "module.net.aws_vpc.main"},
	}

	t.Run("no selectors returns all", func(t *testing.T) {
		got := filterDrifts(drifts, nil, nil)
		if len(got) != 3 {
			t.Fatalf("want 3, got %v", addrs(got))
		}
	})

	t.Run("target narrows", func(t *testing.T) {
		got := filterDrifts(drifts, []string{"aws_instance.web", "module.net"}, nil)
		if len(got) != 2 || got[0].Address != "aws_instance.web" || got[1].Address != "module.net.aws_vpc.main" {
			t.Fatalf("unexpected: %v", addrs(got))
		}
	})

	t.Run("exclude wins over target", func(t *testing.T) {
		got := filterDrifts(drifts, []string{"aws_instance.web"}, []string{"aws_instance.web"})
		if len(got) != 0 {
			t.Fatalf("want 0, got %v", addrs(got))
		}
	})

	t.Run("exclude only", func(t *testing.T) {
		got := filterDrifts(drifts, nil, []string{"aws_instance.db"})
		if len(got) != 2 {
			t.Fatalf("want 2, got %v", addrs(got))
		}
		for _, d := range got {
			if d.Address == "aws_instance.db" {
				t.Fatalf("db should be excluded: %v", addrs(got))
			}
		}
	})

	t.Run("does not mutate input order", func(t *testing.T) {
		filterDrifts(drifts, []string{"aws_instance.db"}, nil)
		if drifts[0].Address != "aws_instance.web" {
			t.Fatalf("input slice was mutated: %v", addrs(drifts))
		}
	})
}

func TestAbsorbedAddresses(t *testing.T) {
	changes := []absorb.FileChange{
		{Path: "a.tf", Edits: []absorb.ResourceEdit{{Address: "aws_instance.web"}}},
		{Path: "b.tf", Edits: []absorb.ResourceEdit{{Address: "aws_instance.db"}, {Address: "aws_instance.web"}}},
	}
	got := absorbedAddresses(changes)
	if len(got) != 2 || !got["aws_instance.web"] || !got["aws_instance.db"] {
		t.Fatalf("unexpected absorbed set: %v", got)
	}
}
