package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Pool is one queue: how many jobs may run at once and how much memory one
// job is expected to need at its peak.
type Pool struct {
	Slots int      `json:"slots"`
	MemMB int      `json:"mem_mb"`
	Match []string `json:"match,omitempty"` // command basenames routed here
	// CPU knobs, applied by `tim run` to the command it starts.
	Nice int               `json:"nice,omitempty"` // niceness added to the command (0-19)
	Env  map[string]string `json:"env,omitempty"`  // e.g. CARGO_BUILD_JOBS=2; wins over the caller's env
}

type Config struct {
	MemBudgetMB int             `json:"mem_budget_mb"` // 0 = 75% of RAM
	Pools       map[string]Pool `json:"pools"`
}

func defaultConfig() Config {
	return Config{Pools: map[string]Pool{
		"cargo":   {Slots: 1, MemMB: 7000, Match: []string{"cargo"}},
		"go":      {Slots: 2, MemMB: 2000, Match: []string{"go"}},
		"default": {Slots: 4, MemMB: 1000},
	}}
}

// loadConfig merges ~/.tim/config.json over the defaults. Pools named in the
// file replace the default of the same name; others stay.
func loadConfig() (Config, error) {
	cfg := defaultConfig()
	b, err := os.ReadFile(filepath.Join(home(), "config.json"))
	if err == nil {
		var f Config
		if err := json.Unmarshal(b, &f); err != nil {
			return cfg, fmt.Errorf("config.json: %w", err)
		}
		if f.MemBudgetMB > 0 {
			cfg.MemBudgetMB = f.MemBudgetMB
		}
		for name, p := range f.Pools {
			cfg.Pools[name] = p
		}
	}
	if cfg.MemBudgetMB == 0 {
		total, _ := memMB()
		cfg.MemBudgetMB = total * 3 / 4
	}
	return cfg, nil
}

func (c Config) poolFor(cmd string) string {
	base := filepath.Base(cmd)
	for name, p := range c.Pools {
		for _, m := range p.Match {
			if m == base {
				return name
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

	conn     net.Conn
	finished chan struct{}
}

// Msg is every request, reply and event on the socket (one JSON per line).
type Msg struct {
	Op      string   `json:"op,omitempty"`    // acquire | done | status | cancel | wait | stop
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
	Err     string   `json:"err,omitempty"`
	Jobs    []*Job   `json:"jobs,omitempty"`
	AvailMB int      `json:"avail_mb,omitempty"`
	TotalMB int      `json:"total_mb,omitempty"`
	Config  *Config  `json:"config,omitempty"`
}

// admit returns the waiting jobs that may start now, in submit order, and
// sets Reason on the ones that may not. A job starts when its pool has a free
// slot, the expected memory of all running jobs plus its own fits the budget,
// and that much memory is available right now. FIFO inside a pool: a blocked
// job blocks the ones behind it in the same pool, not other pools.
// A priority job skips all three checks and starts at once; while it runs it
// still counts against its pool and the budget, so ordinary jobs wait for it.
// ponytail: no aging across pools; a stream of small jobs can hold the budget
// away from a big one. Add aging if a pool starves in practice.
func admit(cfg Config, jobs []*Job, availMB int) []*Job {
	running := map[string]int{}
	reserved := 0
	for _, j := range jobs {
		if j.State == "running" {
			running[j.Pool]++
			reserved += cfg.Pools[j.Pool].MemMB
		}
	}
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
			j.Reason = fmt.Sprintf("low memory: %d MB available, needs %d MB", availMB, p.MemMB)
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
	for _, j := range admit(cfg, d.jobs, avail) {
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
		var fin Msg
		if dec.Decode(&fin) == nil && fin.Op == "done" {
			code = fin.Code
		}
		d.mu.Lock()
		j.State, j.Code, j.Ended, j.Reason = "done", code, time.Now().Unix(), ""
		close(j.finished)
		log.Printf("done %s code=%d", j.ID, code)
		d.trim()
		d.schedule()
		d.mu.Unlock()

	case "status":
		cfg, _ := loadConfig()
		total, avail := memMB()
		d.mu.Lock()
		admit(cfg, d.jobs, avail) // refresh the waiting reasons
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
