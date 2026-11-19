package main

import (
	"strings"
	"testing"
)

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

	// A cargo build that has not grown yet still owes its memory: with 9000 MB
	// available and 6800 MB owed, only one 2000 MB node job fits.
	cfg.MemBudgetMB = 11000
	young := []*Job{{ID: "c", Pool: "cargo", State: "running", RSSMB: 200}, j("a", "node", "waiting"), j("b", "node", "waiting")}
	if got := ids(admit(cfg, young, 9000)); got != "a" {
		t.Errorf("young cargo build: started %q, want a", got)
	}
	// Once it has grown, the OS figure already includes it.
	grown := []*Job{{ID: "c", Pool: "cargo", State: "running", RSSMB: 6800}, j("a", "node", "waiting"), j("b", "node", "waiting")}
	if got := ids(admit(cfg, grown, 9000)); got != "ab" {
		t.Errorf("grown cargo build: started %q, want ab", got)
	}

	// A job bigger than the whole budget still runs when nothing else does.
	cfg.MemBudgetMB = 5000
	if got := ids(admit(cfg, []*Job{j("a", "cargo", "waiting")}, 14000)); got != "a" {
		t.Errorf("oversized job alone: started %q, want a", got)
	}
}

func TestPoolFor(t *testing.T) {
	cfg := defaultConfig()
	for cmd, want := range map[string]string{
		"cargo check":                 "cargo",
		"/usr/bin/cargo test":         "cargo",
		"go test ./...":               "go",
		"tsc --noEmit":                "node",
		"pnpm tsc --noEmit":           "node",
		"pnpm test":                   "node",
		"pnpm run typecheck":          "node",
		"pnpm --filter web run build": "node",
		"pnpm -F web test:unit":       "node",
		"npm run lint":                "node",
		"yarn build":                  "node",
		"npx playwright test":         "node",
		"pnpm exec vitest run":        "node",
		"next build":                  "node",
		"pnpm vite build":             "node",
		"node_modules/.bin/jest --ci": "node",
		"pnpm install":                "default",
		"pnpm":                        "default",
		"pnpm dev":                    "default",
		"next dev":                    "default",
		"test -f x":                   "default",
		"build":                       "default",
		"prettier --write .":          "default",
		"pnpm exec eslint --fix .":    "default",
		"make lint":                   "default",
	} {
		if got := cfg.poolFor(strings.Fields(cmd)); got != want {
			t.Errorf("%q -> %s, want %s", cmd, got, want)
		}
	}
}

func TestStats(t *testing.T) {
	// tree of 1: 1 -> 2 -> 4, 1 -> 3; 9 is unrelated
	ps := "1 0 1000\n 2 1 2000\n 3 1 500\n 4 2 4000\n 9 0 99999\n"
	if got := sumTree(ps, 1); got != 7500 {
		t.Errorf("sumTree = %d, want 7500", got)
	}
	v := make([]int, 100)
	for i := range v {
		v[i] = i + 1
	}
	if p95(v) != 95 || p95([]int{7}) != 7 || p95(nil) != 0 || p95([]int{1, 2, 3}) != 3 {
		t.Errorf("p95 wrong: %d %d %d %d", p95(v), p95([]int{7}), p95(nil), p95([]int{1, 2, 3}))
	}
}

func TestAdmitNowPressure(t *testing.T) {
	cfg := defaultConfig()
	cfg.MemBudgetMB = 11000
	j := func(id string, prio bool) *Job { return &Job{ID: id, Pool: "go", State: "waiting", Priority: prio} }

	if got := ids(admitNow(cfg, []*Job{j("a", false)}, 14000, 5)); got != "a" {
		t.Fatalf("low pressure: got %q, want a", got)
	}
	if got := ids(admitNow(cfg, []*Job{j("a", false)}, 14000, -1)); got != "a" {
		t.Fatalf("no PSI (macOS): got %q, want a", got)
	}
	held := []*Job{j("a", false), {ID: "p", Pool: "cargo", State: "waiting", Priority: true}}
	if got := ids(admitNow(cfg, held, 14000, 60)); got != "p" {
		t.Fatalf("under pressure only priority starts: got %q, want p", got)
	}
	if !strings.HasPrefix(held[0].Reason, "memory pressure 60%") {
		t.Fatalf("reason = %q", held[0].Reason)
	}
	cfg.MaxMemPressure = 0
	if got := ids(admitNow(cfg, []*Job{j("a", false)}, 14000, 90)); got != "a" {
		t.Fatalf("gate off: got %q, want a", got)
	}
}
