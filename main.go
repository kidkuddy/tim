// tim ("take it easy, man") queues heavy commands on a shared machine so they
// do not all run at once.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
)

const usage = `tim: take it easy, man. A build queue for shared machines.

  tim run [--pool P] [--priority] -- <cmd...>     wait for a slot, run <cmd>, exit with its code
  tim submit [--pool P] [--priority] -- <cmd...>  same, in the background; prints the job id
  tim status [--json] [--all]        what is running and waiting
  tim stats                          measured peak memory per pool and command
  tim wait <id>                      block until the job ends, exit with its code
  tim logs [-f] <id>                 output of a submitted job
  tim cancel <id>                    drop a waiting job or stop a running one
  tim config                         config file path and effective values
  tim daemon [stop]                  run or stop the daemon (run starts it by itself)

The pool is picked from the command: cargo, go, node (tsc, vitest, jest,
playwright, next/vite build, and pnpm/npm/yarn typecheck|build|test|lint),
else default.
--priority starts the job at once, past every slot, memory and CPU limit. It is
for a human who asks for it; an agent must not add it on its own.
State lives in ~/.tim (override with TIM_HOME).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "run":
		os.Exit(cmdRun(args))
	case "submit":
		err = cmdSubmit(args)
	case "status":
		err = cmdStatus(args)
	case "wait":
		if len(args) != 1 {
			die("usage: tim wait <id>")
		}
		r, e := request(Msg{Op: "wait", ID: args[0]})
		if e != nil {
			die(e.Error())
		}
		os.Exit(r.Code & 0xff)
	case "stats":
		err = cmdStats()
	case "logs":
		err = cmdLogs(args)
	case "cancel":
		if len(args) != 1 {
			die("usage: tim cancel <id>")
		}
		_, err = request(Msg{Op: "cancel", ID: args[0]})
	case "config":
		cfg, e := loadConfig()
		fmt.Println("# " + filepath.Join(home(), "config.json"))
		b, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(b))
		err = e
	case "daemon":
		if len(args) == 1 && args[0] == "stop" {
			if c, e := net.Dial("unix", sockPath()); e == nil {
				json.NewEncoder(c).Encode(Msg{Op: "stop"})
				io.ReadAll(c)
			}
			return
		}
		err = runDaemon()
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		die(err.Error())
	}
}

func die(msg string) {
	fmt.Fprintln(os.Stderr, "tim:", msg)
	os.Exit(1)
}

func home() string {
	h := os.Getenv("TIM_HOME")
	if h == "" {
		// The account's home from the user database, not $HOME: a build that
		// sets HOME for a test (Playwright's pwtest/home) would otherwise start
		// its own daemon with its own queue and skip every limit.
		u, err := user.Current()
		dir := ""
		if err == nil {
			dir = u.HomeDir
		}
		if dir == "" {
			dir, _ = os.UserHomeDir()
		}
		h = filepath.Join(dir, ".tim")
	}
	os.MkdirAll(filepath.Join(h, "jobs"), 0o700)
	return h
}

func sockPath() string { return filepath.Join(home(), "tim.sock") }

// dial connects to the daemon, starting it if it is not running.
func dial() (net.Conn, error) {
	c, err := net.Dial("unix", sockPath())
	if err == nil {
		return c, nil
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(filepath.Join(home(), "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	d := exec.Command(self, "daemon")
	d.Stdout, d.Stderr = logf, logf
	d.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := d.Start(); err != nil {
		return nil, err
	}
	go d.Wait() // a second daemon losing the lock race exits; reap it
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if c, err = net.Dial("unix", sockPath()); err == nil {
			return c, nil
		}
	}
	return nil, fmt.Errorf("daemon did not start, see %s", logf.Name())
}

// request does one request/reply exchange.
func request(m Msg) (Msg, error) {
	var r Msg
	c, err := dial()
	if err != nil {
		return r, err
	}
	defer c.Close()
	json.NewEncoder(c).Encode(m)
	if err := json.NewDecoder(c).Decode(&r); err != nil {
		return r, fmt.Errorf("daemon closed the connection: %w", err)
	}
	if r.Err != "" {
		return r, errors.New(r.Err)
	}
	return r, nil
}

func newID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// parseRun handles the flags shared by run and submit and resolves the pool.
func parseRun(name string, args []string) (id, pool string, detached, priority bool, cmd []string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&pool, "pool", "", "pool to queue in (default: from the command name)")
	fs.StringVar(&id, "id", "", "internal")
	fs.BoolVar(&detached, "detached", false, "internal")
	fs.BoolVar(&priority, "priority", false, "start now, past all limits (only when a human asked for it)")
	fs.Parse(args)
	cmd = fs.Args()
	if len(cmd) == 0 {
		die("usage: tim " + name + " [--pool P] -- <cmd...>")
	}
	cfg, err := loadConfig()
	if err != nil {
		die(err.Error())
	}
	if pool == "" {
		pool = cfg.poolFor(cmd)
	} else if _, ok := cfg.Pools[pool]; !ok {
		die("unknown pool " + pool)
	}
	if id == "" {
		id = newID()
	}
	return
}

func cmdRun(args []string) int {
	id, pool, detached, priority, argv := parseRun("run", args)
	cwd, _ := os.Getwd()
	logPath := ""
	if detached {
		logPath = filepath.Join(home(), "jobs", id+".log")
	}

	// Wait for a slot. If the daemon goes away while we wait, ask again.
	var conn net.Conn
	said := false
wait:
	for {
		c, err := dial()
		if err != nil {
			fmt.Fprintln(os.Stderr, "tim:", err)
			return 1
		}
		json.NewEncoder(c).Encode(Msg{Op: "acquire", ID: id, Pool: pool, Cmd: argv, Cwd: cwd, PID: os.Getpid(), Log: logPath, Prio: priority})
		dec := json.NewDecoder(c)
		for {
			var m Msg
			if dec.Decode(&m) != nil {
				c.Close()
				time.Sleep(time.Second)
				continue wait
			}
			switch {
			case m.Err != "":
				fmt.Fprintln(os.Stderr, "tim:", m.Err)
				return 1
			case m.Event == "queued" && !said:
				said = true
				fmt.Fprintf(os.Stderr, "tim: job %s waiting for a build slot (%s; %d ahead in %s)\n", id, m.Reason, m.Pos, pool)
			case m.Event == "cancel":
				fmt.Fprintf(os.Stderr, "tim: job %s cancelled while waiting\n", id)
				json.NewEncoder(c).Encode(Msg{Op: "done", Code: 130})
				return 130
			case m.Event == "go":
				conn = c
				break wait
			}
		}
	}
	defer conn.Close()
	var sendMu sync.Mutex
	send := func(m Msg) {
		sendMu.Lock()
		json.NewEncoder(conn).Encode(m)
		sendMu.Unlock()
	}
	if said {
		fmt.Fprintf(os.Stderr, "tim: job %s got a slot, starting\n", id)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	// The pool's CPU knobs. A priority job skips them like every other limit.
	if cfg, _ := loadConfig(); !priority {
		p := cfg.Pools[pool]
		if p.Nice > 0 {
			// Lower our own priority; the command and its children inherit it.
			syscall.Setpriority(syscall.PRIO_PROCESS, 0, p.Nice)
		}
		if len(p.Env) > 0 {
			cmd.Env = os.Environ()
			for k, v := range p.Env {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
		}
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Detached jobs get their own process group so cancel reaches the whole
	// tree (cargo and its rustc children). Foreground jobs stay in the
	// terminal's group so Ctrl-C works as usual.
	// ponytail: a foreground cancel signals only the direct child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: detached}
	code := 0
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "tim:", err)
		code = 127
	} else {
		kill := func(sig syscall.Signal) {
			pid := cmd.Process.Pid
			if detached {
				pid = -pid
			}
			syscall.Kill(pid, sig)
		}
		sigs := make(chan os.Signal, 4)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		cancelled := make(chan struct{})
		go func() {
			var m Msg
			if json.NewDecoder(conn).Decode(&m) == nil && m.Event == "cancel" {
				close(cancelled)
			}
		}()
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		// Report the process tree's memory: the daemon schedules on it and
		// records the peak.
		sample := time.NewTicker(2 * time.Second)
		defer sample.Stop()
		send(Msg{Op: "rss", RSS: treeRSSMB(cmd.Process.Pid)})
		wasCancelled := false
	loop:
		for {
			select {
			case <-sample.C:
				send(Msg{Op: "rss", RSS: treeRSSMB(cmd.Process.Pid)})
			case s := <-sigs:
				// In the foreground the terminal already sent Ctrl-C to the child.
				if s != syscall.SIGINT || detached {
					kill(s.(syscall.Signal))
				}
			case <-cancelled:
				cancelled, wasCancelled = nil, true
				kill(syscall.SIGTERM)
				time.AfterFunc(10*time.Second, func() { kill(syscall.SIGKILL) })
			case err := <-exited:
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					code = ee.ExitCode()
					if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
						code = 128 + int(ws.Signal())
					}
				} else if err != nil {
					code = 1
				}
				break loop
			}
		}
		if wasCancelled {
			code = 130
		}
	}
	send(Msg{Op: "done", Code: code})
	return code
}

func cmdSubmit(args []string) error {
	id, pool, _, priority, argv := parseRun("submit", args)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.Create(filepath.Join(home(), "jobs", id+".log"))
	if err != nil {
		return err
	}
	defer logf.Close()
	runArgs := []string{"run", "--detached", "--id", id, "--pool", pool}
	if priority {
		runArgs = append(runArgs, "--priority")
	}
	c := exec.Command(self, append(append(runArgs, "--"), argv...)...)
	c.Stdout, c.Stderr = logf, logf
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return err
	}
	// Return once the daemon knows the job, so `tim wait <id>` works at once.
	for i := 0; i < 50; i++ {
		if r, err := request(Msg{Op: "status"}); err == nil {
			for _, j := range r.Jobs {
				if j.ID == id {
					fmt.Println(id)
					return nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("job %s did not register, see %s", id, logf.Name())
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	all := fs.Bool("all", false, "also list finished jobs")
	fs.Parse(args)
	r, err := request(Msg{Op: "status"})
	if err != nil {
		return err
	}
	if *asJSON {
		if r.Jobs == nil {
			r.Jobs = []*Job{}
		}
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	running, waiting, reserved := map[string]int{}, map[string]int{}, 0
	for _, j := range r.Jobs {
		switch j.State {
		case "running":
			running[j.Pool]++
			reserved += r.Config.Pools[j.Pool].MemMB
		case "waiting":
			waiting[j.Pool]++
		}
	}
	fmt.Printf("memory: %d MB available of %d MB\n", r.AvailMB, r.TotalMB)
	fmt.Printf("budget: %d MB for queued jobs (%d MB kept in reserve for everything else), %d MB claimed by running jobs\n\n",
		r.Config.MemBudgetMB, r.TotalMB-r.Config.MemBudgetMB, reserved)
	peaks := peaksByPool(readHistory())
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "POOL\tRUNNING\tWAITING\tMB/JOB\tMEASURED P95\tRUNS")
	names := make([]string, 0, len(r.Config.Pools))
	for n := range r.Config.Pools {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := r.Config.Pools[n]
		measured := "-"
		if len(peaks[n]) > 0 {
			measured = fmt.Sprintf("%d MB", p95(peaks[n]))
		}
		fmt.Fprintf(w, "%s\t%d/%d\t%d\t%d\t%s\t%d\n", n, running[n], p.Slots, waiting[n], p.MemMB, measured, len(peaks[n]))
	}
	w.Flush()
	fmt.Println()
	userHome, _ := os.UserHomeDir()
	fmt.Fprintln(w, "ID\tPOOL\tSTATE\tAGE\tRSS\tCWD\tCMD")
	shown := 0
	for _, j := range r.Jobs {
		state, since := j.State, j.Submitted
		switch j.State {
		case "running":
			since = j.Started
		case "done":
			if !*all {
				continue
			}
			state, since = fmt.Sprintf("exit %d", j.Code), j.Ended
		}
		note := ""
		if j.Priority {
			note = "  (PRIORITY)"
		}
		if j.Reason != "" {
			note = "  (" + j.Reason + ")"
		}
		cwd := j.Cwd
		if userHome != "" && strings.HasPrefix(cwd, userHome) {
			cwd = "~" + cwd[len(userHome):]
		}
		age := time.Since(time.Unix(since, 0)).Round(time.Second)
		rss := "-"
		if j.State == "running" {
			rss = fmt.Sprintf("%d MB", j.RSSMB)
		} else if j.PeakMB > 0 {
			rss = fmt.Sprintf("peak %d MB", j.PeakMB)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s%s\n", j.ID, j.Pool, state, age, rss, cwd, strings.Join(j.Cmd, " "), note)
		shown++
	}
	if shown == 0 {
		fmt.Println("no jobs")
		return nil
	}
	return w.Flush()
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "follow until the job ends")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: tim logs [-f] <id>")
	}
	id := filepath.Base(fs.Arg(0))
	f, err := os.Open(filepath.Join(home(), "jobs", id+".log"))
	if err != nil {
		return fmt.Errorf("no log for job %s (only submitted jobs have one)", id)
	}
	defer f.Close()
	for {
		io.Copy(os.Stdout, f)
		if !*follow {
			return nil
		}
		active := false
		if r, err := request(Msg{Op: "status"}); err == nil {
			for _, j := range r.Jobs {
				active = active || (j.ID == id && j.State != "done")
			}
		}
		if !active {
			io.Copy(os.Stdout, f)
			return nil
		}
		time.Sleep(time.Second)
	}
}
