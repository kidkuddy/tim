# tim

**T**ake **i**t easy, **m**an. A build queue for machines that several people or agents share.

Three `cargo build`s at once on a 16 GB box end with the kernel killing `rustc`. `tim` makes them
take turns:

```console
$ tim run -- cargo check
tim: job 455473 waiting for a build slot (cargo pool full (1/1); 0 ahead in cargo)
tim: job 455473 got a slot, starting
    Checking backend v0.1.0
```

One static binary, no dependencies, no root, Linux and macOS. A small per-user daemon starts by
itself the first time you run a job.

## Install

```sh
go install github.com/kidkuddy/tim@latest
```

## Use

```
tim run [--pool P] [--priority] -- <cmd...>     wait for a slot, run <cmd>, exit with its code
tim submit [--pool P] [--priority] -- <cmd...>  same, in the background; prints the job id
tim status [--json] [--all]        what is running and waiting, and why
tim wait <id>                      block until the job ends, exit with its code
tim logs [-f] <id>                 output of a submitted job
tim cancel <id>                    drop a waiting job or stop a running one
tim config                         config file path and effective values
tim daemon [stop]                  run or stop the daemon by hand
```

`tim run` runs the command itself, in your directory, with your environment and terminal. The
daemon only hands out slots. If `tim run` dies, its slot is freed.

```console
$ tim status
memory: 13859 MB available of 15783, budget 11837 MB, reserved 7000 MB

POOL     RUNNING  WAITING  MB/JOB
cargo    1/1      2        7000
default  0/4      0        1000
go       0/2      0        2000

ID      POOL   STATE    AGE  CWD            CMD
46db68  cargo  running  41s  ~/src/api      cargo check
91e6d6  cargo  waiting  30s  ~/src/api-2    cargo test  (cargo pool full (1/1))
63c8d9  cargo  waiting  12s  ~/src/worker   cargo check  (behind another cargo job)
```

## How a job gets a slot

Each pool is its own first-in-first-out queue. The pool comes from the command name (`cargo`,
`go`, anything else is `default`) or from `--pool`. A waiting job starts when all three hold:

1. its pool has a free slot;
2. the expected memory (`mem_mb`) of every running job plus its own fits `mem_budget_mb`;
3. the machine has at least its `mem_mb` available right now.

So a `go test` can run beside a `cargo build`, and two `cargo build`s cannot.

## CPU

Slots limit how many builds run, not how many cores one build takes. Two per-pool knobs cover that:

- `nice`: the command runs at this niceness (0-19). Everything else on the machine, such as a
  daemon that must answer a heartbeat, gets the CPU first. Needs no root.
- `env`: variables set for the command, e.g. `{"CARGO_BUILD_JOBS": "2"}` or `{"GOFLAGS": "-p=2"}`
  to cap the cores a build uses. They win over the caller's environment.

## Priority

`tim run --priority -- <cmd>` starts at once: no slot, budget or memory check, no `nice`, no
`env`. While it runs it still occupies its pool, so ordinary jobs wait for it. `tim status` marks
it `(PRIORITY)` and the daemon log records who started it.

It is for a person who decides their build cannot wait. tim cannot tell a person from an agent
running as the same user, so the rule lives in the agent's instructions: an agent passes
`--priority` only when the human prompting it asked for priority in their own words.

## Configure

`~/.tim/config.json` (or `$TIM_HOME/config.json`). Everything is optional; these are the defaults:

```json
{
  "mem_budget_mb": 0,
  "pools": {
    "cargo":   {"slots": 1, "mem_mb": 7000, "match": ["cargo"]},
    "go":      {"slots": 2, "mem_mb": 2000, "match": ["go"]},
    "default": {"slots": 4, "mem_mb": 1000}
  }
}
```

A pool in the file replaces the default of the same name. With the CPU knobs:

```json
{"pools": {"cargo": {"slots": 1, "mem_mb": 7000, "match": ["cargo"], "nice": 10,
                     "env": {"CARGO_BUILD_JOBS": "2"}}}}
```

`mem_budget_mb: 0` means 75% of RAM. `match` lists the command names routed to a pool. Add your
own pools (`"docker": {"slots": 1, "mem_mb": 4000, "match": ["docker"]}`). Changes apply on the
next scheduling pass.

## Limits

- Only commands started with `tim run` or `tim submit` are queued. A build started without it is
  not held back; tim only sees the memory it uses.
- `mem_mb` is your estimate. tim does not measure or cap what a job uses.
- Finished-job history is kept in memory and is lost when the daemon restarts.
- One daemon per user. Two users on one machine have two independent queues.

## For AI agents

Tell the agent: "run every command that compiles as `tim run -- <command>`; a `waiting for a build
slot` line is expected, wait for it; never add `--priority` unless I ask for it." `tim status --json` gives it the queue.
