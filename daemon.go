package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Pool is one queue: how many jobs may run at once and how much memory one
// job is expected to need at its peak.
type Pool struct {
	Slots int `json:"slots"`
	MemMB int `json:"mem_mb"`
	// Match routes a command here by its name ("tsc") or its name and first
	// argument ("next build"). Scripts does the same for package-manager
	// scripts only (`pnpm test`, `npm run build`), so `/bin/test` is not caught.
	Match   []string `json:"match,omitempty"`
	Scripts []string `json:"scripts,omitempty"`
	// CPU knobs, applied by `tim run` to the command it starts.
	Nice int               `json:"nice,omitempty"` // niceness added to the command (0-19)
	Env  map[string]string `json:"env,omitempty"`  // e.g. CARGO_BUILD_JOBS=2; wins over the caller's env
}

type Config struct {
	// ReserveMB is memory left for everything tim does not queue (agents,
	// editors, services). Budget = RAM - reserve unless MemBudgetMB is set.
	ReserveMB   int `json:"reserve_mb"`
	MemBudgetMB int `json:"mem_budget_mb"`
	// MaxMemPressure: while the kernel's memory pressure (Linux PSI "some avg10",
	// the % of time a task stalled on memory) is at or above it, no new job starts.
	// MemAvailable counts reclaimable cache and misses a box that is already
	// swapping; pressure does not. 0 turns the check off.
	MaxMemPressure float64         `json:"max_mem_pressure"`
	Pools          map[string]Pool `json:"pools"`
}

func defaultConfig() Config {
	return Config{ReserveMB: 2500, MaxMemPressure: 20, Pools: map[string]Pool{
		"cargo": {Slots: 1, MemMB: 7000, Match: []string{"cargo"}},
		"go":    {Slots: 2, MemMB: 2000, Match: []string{"go"}},
		"node": {Slots: 2, MemMB: 2000,
			Match:   []string{"tsc", "vitest", "jest", "playwright", "next build", "vite build"},
			Scripts: []string{"typecheck", "type-check", "build", "test", "lint"}},
		"default": {Slots: 4, MemMB: 1000},
	}}
}

// loadConfig merges ~/.tim/config.json over the defaults, field by field: a
// pool in the file overrides only the fields it sets.
func loadConfig() (Config, error) {
	cfg := defaultConfig()
	b, err := os.ReadFile(filepath.Join(home(), "config.json"))
	if err == nil {
		var f struct {
			ReserveMB      int                        `json:"reserve_mb"`
			MemBudgetMB    int                        `json:"mem_budget_mb"`
			MaxMemPressure *float64                   `json:"max_mem_pressure"`
			Pools          map[string]json.RawMessage `json:"pools"`
		}
		if err := json.Unmarshal(b, &f); err != nil {
			return defaultConfig().withBudget(), fmt.Errorf("config.json: %w", err)
		}
		if f.ReserveMB > 0 {
			cfg.ReserveMB = f.ReserveMB
		}
		cfg.MemBudgetMB = f.MemBudgetMB
		if f.MaxMemPressure != nil {
			cfg.MaxMemPressure = *f.MaxMemPressure
		}
		for name, raw := range f.Pools {
			p := cfg.Pools[name]
			if err := json.Unmarshal(raw, &p); err != nil {
				return defaultConfig().withBudget(), fmt.Errorf("config.json: pool %s: %w", name, err)
			}
			cfg.Pools[name] = p
		}
	}
	return cfg.withBudget(), nil
}

func (c Config) withBudget() Config {
	if c.MemBudgetMB == 0 {
		total, _ := memMB()
		c.MemBudgetMB = total - c.ReserveMB
	}
	return c
}

var pkgManagers = map[string]bool{"pnpm": true, "npm": true, "yarn": true, "npx": true, "bun": true, "bunx": true}

// Package-manager flags that take a value as the next argument.
var pmValueFlags = map[string]bool{"--filter": true, "-F": true, "-C": true, "--dir": true, "--prefix": true, "--cwd": true}

// unwrap strips a package manager, its flags and run/exec, leaving what it
// starts: `pnpm --filter web run build` -> [build], viaPM.
func unwrap(argv []string) (rest []string, viaPM bool) {
	if !pkgManagers[filepath.Base(argv[0])] {
		return argv, false
	}
	for i := 1; i < len(argv); i++ {
		switch a := argv[i]; {
		case pmValueFlags[a]:
			i++
		case strings.HasPrefix(a, "-"), a == "run", a == "run-script", a == "exec", a == "dlx", a == "x":
		default:
			return argv[i:], true
		}
	}
	return nil, true
}

func (c Config) poolFor(argv []string) string {
	rest, viaPM := unwrap(argv)
	if len(rest) == 0 {
		return "default"
	}
	name := filepath.Base(rest[0])
	two := name
	if len(rest) > 1 {
		two = name + " " + rest[1]
	}
	script, _, _ := strings.Cut(name, ":") // test:unit counts as test
	names := make([]string, 0, len(c.Pools))
	for n := range c.Pools {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, m := range c.Pools[n].Match {
			if m == name || m == two {
				return n
			}
		}
		for _, s := range c.Pools[n].Scripts {
			if viaPM && (s == name || s == script) {
				return n
			}
		}
	}
	return "default"
}

type Job struct {
	ID        string   `json:"id"`
	Pool      string   `json:"pool"`
	Cmd       []string `json:"cmd"`
	Cwd       string   `json:"cwd"`
	PID       int      `json:"pid"`
	Log       string   `json:"log,omitempty"`
	State     string   `json:"state"` // waiting | running | done
	Reason    string   `json:"reason,omitempty"`
	Submitted int64    `json:"submitted"`
	Started   int64    `json:"started,omitempty"`
	Ended     int64    `json:"ended,omitempty"`
	Code      int      `json:"code"`
	Priority  bool     `json:"priority,omitempty"`
	RSSMB     int      `json:"rss_mb,omitempty"`  // process tree right now
	PeakMB    int      `json:"peak_mb,omitempty"` // highest seen

	conn     net.Conn
	finished chan struct{}
}

// Msg is every request, reply and event on the socket (one JSON per line).
type Msg struct {
	Op      string   `json:"op,omitempty"`    // acquire | rss | done | status | cancel | wait | stop
	Event   string   `json:"event,omitempty"` // queued | go | cancel
	ID      string   `json:"id,omitempty"`
	Pool    string   `json:"pool,omitempty"`
	Cmd     []string `json:"cmd,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	PID     int      `json:"pid,omitempty"`
	Log     string   `json:"log,omitempty"`
	Code    int      `json:"code"`
	Pos     int      `json:"pos,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	Prio    bool     `json:"priority,omitempty"`
	RSS     int      `json:"rss_mb,omitempty"`
	Err     string   `json:"err,omitempty"`
	Jobs    []*Job   `json:"jobs,omitempty"`
	AvailMB int      `json:"avail_mb,omitempty"`
	TotalMB int      `json:"total_mb,omitempty"`
	Config  *Config  `json:"config,omitempty"`
}

// admit returns the waiting jobs that may start now, in submit order, and
// sets Reason on the ones that may not. A job starts when its pool has a free
// slot, the expected memory of all running jobs plus its own fits the budget,
// and that much memory is available right now. "Available" is what the OS
// reports minus what running jobs are still expected to grow by (their mem_mb
// less their measured RSS): a build that started seconds ago has not taken
// its memory yet. FIFO inside a pool: a blocked
// job blocks the ones behind it in the same pool, not other pools.
// A priority job skips all three checks and starts at once; while it runs it
// still counts against its pool and the budget, so ordinary jobs wait for it.
// ponytail: no aging across pools; a stream of small jobs can hold the budget
// away from a big one. Add aging if a pool starves in practice.
func admit(cfg Config, jobs []*Job, availMB int) []*Job {
	running := map[string]int{}
	reserved, owed := 0, 0
	for _, j := range jobs {
		if j.State == "running" {
			running[j.Pool]++
			reserved += cfg.Pools[j.Pool].MemMB
			owed += max(0, cfg.Pools[j.Pool].MemMB-j.RSSMB)
		}
	}
	availMB -= owed
	var start []*Job
	blocked := map[string]bool{}
	for _, j := range jobs {
		if j.State != "waiting" {
			continue
		}
		p := cfg.Pools[j.Pool]
		switch {
		case j.Priority:
			j.Reason = ""
			running[j.Pool]++
			reserved += p.MemMB
			availMB -= p.MemMB
			start = append(start, j)
			continue
		case blocked[j.Pool]:
			j.Reason = "behind another " + j.Pool + " job"
		case running[j.Pool] >= p.Slots:
			j.Reason = fmt.Sprintf("%s pool full (%d/%d)", j.Pool, running[j.Pool], p.Slots)
		// reserved > 0: a job bigger than the whole budget still runs alone.
		case reserved > 0 && reserved+p.MemMB > cfg.MemBudgetMB:
			j.Reason = fmt.Sprintf("memory budget: %d MB reserved + %d MB > %d MB", reserved, p.MemMB, cfg.MemBudgetMB)
		case availMB < p.MemMB:
			j.Reason = fmt.Sprintf("low memory: %d MB free after running jobs reach their size, needs %d MB", availMB, p.MemMB)
		default:
			j.Reason = ""
			running[j.Pool]++
			reserved += p.MemMB
			availMB -= p.MemMB
			start = append(start, j)
			continue
		}
		blocked[j.Pool] = true
	}
	return start
}

// admitNow is admit plus the memory-pressure gate: under pressure only
// priority jobs start, and every other waiting job says why it waits.
// psi < 0 means the platform does not report pressure (macOS).
func admitNow(cfg Config, jobs []*Job, availMB int, psi float64) []*Job {
	if psi < 0 || cfg.MaxMemPressure <= 0 || psi < cfg.MaxMemPressure {
		return admit(cfg, jobs, availMB)
	}
	start := admit(cfg, jobs, -1<<30) // priority jobs skip the memory checks and still start
	for _, j := range jobs {
		if j.State == "waiting" && !j.Priority && strings.HasPrefix(j.Reason, "low memory") {
			j.Reason = fmt.Sprintf("memory pressure %.0f%% (limit %.0f%%)", psi, cfg.MaxMemPressure)
		}
	}
	return start
}

type daemon struct {
	mu   sync.Mutex
	jobs []*Job // waiting, running and the most recent done ones
}

// schedule starts every job that may run. Caller holds d.mu.
func (d *daemon) schedule() {
	cfg, err := loadConfig()
	if err != nil {
		log.Print(err, " (using defaults)")
	}
	_, avail := memMB()
	for _, j := range admitNow(cfg, d.jobs, avail, memPressure()) {
		j.State, j.Started = "running", time.Now().Unix()
		json.NewEncoder(j.conn).Encode(Msg{Event: "go"})
		log.Printf("start %s [%s] priority=%v pid=%d cwd=%s %v", j.ID, j.Pool, j.Priority, j.PID, j.Cwd, j.Cmd)
	}
}

func (d *daemon) find(id string) *Job {
	for _, j := range d.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

const keepDone = 100

// trim drops the oldest finished jobs beyond keepDone. Caller holds d.mu.
// ponytail: history is in memory only; a daemon restart forgets finished jobs.
func (d *daemon) trim() {
	done := 0
	for _, j := range d.jobs {
		if j.State == "done" {
			done++
		}
	}
	kept := d.jobs[:0]
	for _, j := range d.jobs {
		if j.State == "done" && done > keepDone {
			done--
			continue
		}
		kept = append(kept, j)
	}
	d.jobs = kept
}

func (d *daemon) handle(c net.Conn) {
	defer c.Close()
	dec, enc := json.NewDecoder(c), json.NewEncoder(c)
	var m Msg
	if dec.Decode(&m) != nil {
		return
	}
	switch m.Op {
	case "acquire":
		cfg, _ := loadConfig()
		if _, ok := cfg.Pools[m.Pool]; !ok {
			enc.Encode(Msg{Err: "unknown pool " + m.Pool})
			return
		}
		j := &Job{ID: m.ID, Pool: m.Pool, Cmd: m.Cmd, Cwd: m.Cwd, PID: m.PID, Log: m.Log, Priority: m.Prio,
			State: "waiting", Submitted: time.Now().Unix(), conn: c, finished: make(chan struct{})}
		d.mu.Lock()
		if d.find(j.ID) != nil {
			d.mu.Unlock()
			enc.Encode(Msg{Err: "job id " + j.ID + " already exists"})
			return
		}
		d.jobs = append(d.jobs, j)
		d.schedule()
		if j.State == "waiting" {
			pos := 0
			for _, o := range d.jobs {
				if o != j && o.State == "waiting" && o.Pool == j.Pool {
					pos++
				}
			}
			enc.Encode(Msg{Event: "queued", Pos: pos, Reason: j.Reason})
		}
		d.mu.Unlock()

		// The connection is the job's lease: "done" or a drop both end it.
		code := -1
		for {
			var in Msg
			if dec.Decode(&in) != nil {
				break
			}
			if in.Op == "done" {
				code = in.Code
				break
			}
			if in.Op == "rss" {
				d.mu.Lock()
				j.RSSMB, j.PeakMB = in.RSS, max(j.PeakMB, in.RSS)
				d.mu.Unlock()
			}
		}
		d.mu.Lock()
		j.State, j.Code, j.Ended, j.Reason, j.RSSMB = "done", code, time.Now().Unix(), "", 0
		if j.Started > 0 {
			record(j)
		}
		close(j.finished)
		log.Printf("done %s code=%d", j.ID, code)
		d.trim()
		d.schedule()
		d.mu.Unlock()

	case "status":
		cfg, _ := loadConfig()
		total, avail := memMB()
		d.mu.Lock()
		admitNow(cfg, d.jobs, avail, memPressure()) // refresh the waiting reasons
		enc.Encode(Msg{Jobs: d.jobs, AvailMB: avail, TotalMB: total, Config: &cfg})
		d.mu.Unlock()

	case "cancel":
		d.mu.Lock()
		j := d.find(m.ID)
		switch {
		case j == nil:
			enc.Encode(Msg{Err: "no such job " + m.ID})
		case j.State == "done":
			enc.Encode(Msg{Err: "job " + m.ID + " already finished"})
		default:
			json.NewEncoder(j.conn).Encode(Msg{Event: "cancel"})
			enc.Encode(Msg{})
		}
		d.mu.Unlock()

	case "wait":
		d.mu.Lock()
		j := d.find(m.ID)
		d.mu.Unlock()
		if j == nil {
			enc.Encode(Msg{Err: "no such job " + m.ID})
			return
		}
		<-j.finished
		enc.Encode(Msg{Code: j.Code})

	case "stop":
		enc.Encode(Msg{})
		log.Print("stopped")
		os.Exit(0)

	default:
		enc.Encode(Msg{Err: "unknown op " + m.Op})
	}
}

func runDaemon() error {
	lock, err := os.OpenFile(filepath.Join(home(), "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return fmt.Errorf("daemon already running")
	}
	// Only the lock holder may replace the socket.
	os.Remove(sockPath())
	l, err := net.Listen("unix", sockPath())
	if err != nil {
		return err
	}
	os.Chmod(sockPath(), 0o600)
	log.Printf("listening on %s", sockPath())
	d := &daemon{}
	go func() { // memory frees up without any event; re-check on a timer
		for range time.Tick(5 * time.Second) {
			d.mu.Lock()
			d.schedule()
			d.mu.Unlock()
		}
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go d.handle(c)
	}
}

// record appends a finished job to history.jsonl, the data `tim stats` and
// the P95 column of `tim status` are computed from.
// ponytail: the file only grows (~150 bytes a job); rotate if it gets large.
func record(j *Job) {
	f, err := os.OpenFile(filepath.Join(home(), "history.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Print(err)
		return
	}
	defer f.Close()
	json.NewEncoder(f).Encode(Record{ID: j.ID, Pool: j.Pool, Cmd: j.Cmd, Cwd: j.Cwd,
		PeakMB: j.PeakMB, Secs: j.Ended - j.Started, Code: j.Code, Ended: j.Ended})
}
