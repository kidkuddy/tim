package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

// Record is one finished job in history.jsonl.
type Record struct {
	ID     string   `json:"id"`
	Pool   string   `json:"pool"`
	Cmd    []string `json:"cmd"`
	Cwd    string   `json:"cwd"`
	PeakMB int      `json:"peak_mb"`
	Secs   int64    `json:"secs"`
	Code   int      `json:"code"`
	Ended  int64    `json:"ended"`
}

// treeRSSMB sums the resident memory of pid and all its descendants.
// ponytail: `ps` every sample and RSS summed per process, so pages shared
// between processes count more than once and a spike between two samples is
// missed. Read cgroup memory.peak on Linux if exact peaks are needed.
func treeRSSMB(root int) int {
	out, err := exec.Command("ps", "-eo", "pid=,ppid=,rss=").Output()
	if err != nil {
		return 0
	}
	return sumTree(string(out), root) / 1024
}

// sumTree parses "pid ppid rssKB" lines and returns the KB of root's tree.
func sumTree(ps string, root int) int {
	kids, rss := map[int][]int{}, map[int]int{}
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		rss[pid], _ = strconv.Atoi(f[2])
		kids[ppid] = append(kids[ppid], pid)
	}
	total, todo, seen := 0, []int{root}, map[int]bool{}
	for len(todo) > 0 {
		p := todo[0]
		todo = todo[1:]
		if seen[p] {
			continue
		}
		seen[p] = true
		total += rss[p]
		todo = append(todo, kids[p]...)
	}
	return total
}

func readHistory() []Record {
	f, err := os.Open(filepath.Join(home(), "history.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var rs []Record
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 1<<20)
	for s.Scan() {
		var r Record
		// Jobs too short to be sampled have no peak and would drag the numbers down.
		if json.Unmarshal(s.Bytes(), &r) == nil && r.PeakMB > 0 {
			rs = append(rs, r)
		}
	}
	return rs
}

// p95 is the nearest-rank 95th percentile; 0 for no data.
func p95(v []int) int {
	if len(v) == 0 {
		return 0
	}
	s := append([]int(nil), v...)
	sort.Ints(s)
	return s[(len(s)*95+99)/100-1]
}

// peaksByPool returns each pool's peak-RSS samples from its last 200 jobs.
func peaksByPool(rs []Record) map[string][]int {
	m := map[string][]int{}
	for _, r := range rs {
		m[r.Pool] = append(m[r.Pool], r.PeakMB)
	}
	for k, v := range m {
		if len(v) > 200 {
			m[k] = v[len(v)-200:]
		}
	}
	return m
}

// cmdKey groups runs of the same command: its name and the next two arguments.
func cmdKey(argv []string) string {
	k := append([]string{filepath.Base(argv[0])}, argv[1:]...)
	if len(k) > 3 {
		k = k[:3]
	}
	return strings.Join(k, " ")
}

// cmdStats prints measured peak memory per pool and command, to tune mem_mb.
func cmdStats() error {
	cfg, err := loadConfig()
	type group struct {
		pool, cmd string
		peaks     []int
		secs      int64
	}
	groups, order := map[string]*group{}, []string{}
	for _, r := range readHistory() {
		if len(r.Cmd) == 0 {
			continue
		}
		k := r.Pool + "\x00" + cmdKey(r.Cmd)
		if groups[k] == nil {
			groups[k] = &group{pool: r.Pool, cmd: cmdKey(r.Cmd)}
			order = append(order, k)
		}
		groups[k].peaks = append(groups[k].peaks, r.PeakMB)
		groups[k].secs += r.Secs
	}
	if len(order) == 0 {
		fmt.Println("no finished jobs measured yet")
		return err
	}
	sort.Strings(order)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "POOL\tMB/JOB\tCOMMAND\tRUNS\tP95 MB\tMAX MB\tAVG TIME")
	for _, k := range order {
		g := groups[k]
		mx := 0
		for _, p := range g.peaks {
			mx = max(mx, p)
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%d\t%d\t%d\t%ds\n", g.pool, cfg.Pools[g.pool].MemMB, g.cmd, len(g.peaks), p95(g.peaks), mx, g.secs/int64(len(g.peaks)))
	}
	w.Flush()
	return err
}
