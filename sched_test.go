package main

import "testing"

func ids(js []*Job) string {
	s := ""
	for _, j := range js {
		s += j.ID
	}
	return s
}

func TestAdmit(t *testing.T) {
	cfg := defaultConfig()
	cfg.MemBudgetMB = 11000
	j := func(id, pool, state string) *Job { return &Job{ID: id, Pool: pool, State: state} }

	cases := []struct {
		name  string
		jobs  []*Job
		avail int
		want  string
	}{
		{"one cargo at a time, go runs beside it",
			[]*Job{j("a", "cargo", "waiting"), j("b", "cargo", "waiting"), j("c", "go", "waiting")}, 14000, "ac"},
		{"cargo slot taken",
			[]*Job{j("a", "cargo", "running"), j("b", "cargo", "waiting")}, 14000, ""},
		{"budget: cargo 7000 + go 2000 + go 2000 fits 11000, default 1000 does not",
			[]*Job{j("a", "cargo", "running"), j("b", "go", "waiting"), j("c", "go", "waiting"), j("d", "default", "waiting")}, 14000, "bc"},
		{"live memory too low for cargo, enough for go",
			[]*Job{j("a", "cargo", "waiting"), j("b", "go", "waiting")}, 3000, "b"},
		{"admitted jobs use up the live memory within one pass",
			[]*Job{j("a", "go", "waiting"), j("b", "go", "waiting")}, 3000, "a"},
		{"fifo inside a pool: blocked job holds back the one behind it",
			[]*Job{j("a", "go", "running"), j("b", "go", "running"), j("c", "go", "waiting"), j("d", "go", "waiting")}, 14000, ""},
		{"priority starts past a full pool, a spent budget and low memory",
			[]*Job{j("a", "cargo", "running"), j("b", "cargo", "waiting"), {ID: "p", Pool: "cargo", State: "waiting", Priority: true}}, 100, "p"},
		{"a running priority job still holds its slot against ordinary jobs",
			[]*Job{{ID: "p", Pool: "cargo", State: "running", Priority: true}, j("b", "cargo", "waiting")}, 14000, ""},
		{"done jobs hold nothing",
			[]*Job{j("a", "cargo", "done"), j("b", "cargo", "waiting")}, 14000, "b"},
	}
	for _, c := range cases {
		if got := ids(admit(cfg, c.jobs, c.avail)); got != c.want {
			t.Errorf("%s: started %q, want %q", c.name, got, c.want)
		}
	}

	// A job bigger than the whole budget still runs when nothing else does.
	cfg.MemBudgetMB = 5000
	if got := ids(admit(cfg, []*Job{j("a", "cargo", "waiting")}, 14000)); got != "a" {
		t.Errorf("oversized job alone: started %q, want a", got)
	}
}
