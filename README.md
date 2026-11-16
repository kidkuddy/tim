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
tim stats                          measured peak memory per pool and command
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
memory: 8194 MB available of 15783 MB
budget: 13283 MB for queued jobs (2500 MB kept in reserve for everything else), 9000 MB claimed by running jobs

POOL     RUNNING  WAITING  MB/JOB  MEASURED P95  RUNS
cargo    1/1      1        7000    6480 MB       37
default  0/4      0        1000    -             0
go       0/2      0        2000    910 MB        12
node     1/2      0        2000    1740 MB       85

ID      POOL   STATE    AGE  RSS      CWD           CMD
46db68  cargo  running  41s  3120 MB  ~/src/api     cargo check
91e6d6  cargo  waiting  30s  -        ~/src/api-2   cargo test  (cargo pool full (1/1))
63c8d9  node   running  12s  640 MB   ~/src/web     pnpm tsc --noEmit
```

## How a job gets a slot

Each pool is its own first-in-first-out queue. A waiting job starts when all three hold:

1. its pool has a free slot;
2. the expected memory (`mem_mb`) of every running job plus its own fits the budget;
3. the machine has at least its `mem_mb` available right now, after subtracting what running
   jobs are still expected to grow by (their `mem_mb` less their measured memory).

The budget is RAM minus `reserve_mb` (2500 by default): memory left alone for everything tim
does not queue, such as editors, agents and services.

So a `go test` can run beside a `cargo build`, and two `cargo build`s cannot.

## Which pool

`--pool` names it. Otherwise tim looks at the command:

| Pool | Commands |
|---|---|
| `cargo` | `cargo …` |
| `go` | `go …` |
| `node` | `tsc`, `vitest`, `jest`, `playwright`, `next build`, `vite build`, and the scripts `typecheck`, `build`, `test`, `lint` (also `test:unit` etc.) run through `pnpm`, `npm`, `yarn`, `npx` or `bun` |
| `default` | everything else, e.g. `pnpm install`, `make` |

Package-manager wrappers are looked through: `pnpm --filter web run build`, `npx playwright test`
and `pnpm exec vitest` all land in `node`.

## Measure, then tune

`tim run` samples the memory of the command's whole process tree every 2 seconds. The peak of
each finished job is kept in `~/.tim/history.jsonl`. `tim status` shows the 95th percentile per
pool next to `MB/JOB`, and `tim stats` breaks it down by command:

```console
$ tim stats
POOL   MB/JOB  COMMAND                 RUNS  P95 MB  MAX MB  AVG TIME
cargo  7000    cargo check --jobs 1    31    6480    6610    412s
node   2000    pnpm tsc --noEmit       58    1740    1905    96s
node   2000    pnpm test               27    1210    1388    64s
```

If a pool's P95 sits above its `mem_mb`, raise `mem_mb`; if far below, lower it and more jobs fit.
The numbers sum per-process resident memory, so shared pages count more than once and short spikes
between samples are missed: read them as an upper-leaning estimate.

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
  "reserve_mb": 2500,
  "pools": {
    "cargo":   {"slots": 1, "mem_mb": 7000, "match": ["cargo"]},
    "go":      {"slots": 2, "mem_mb": 2000, "match": ["go"]},
    "node":    {"slots": 2, "mem_mb": 2000,
                "match": ["tsc", "vitest", "jest", "playwright", "next build", "vite build"],
                "scripts": ["typecheck", "type-check", "build", "test", "lint"]},
    "default": {"slots": 4, "mem_mb": 1000}
  }
}
```

Set only what you want to change; a pool in the file overrides just the fields it names:

```json
{"reserve_mb": 4000,
 "pools": {"cargo": {"nice": 10, "env": {"CARGO_BUILD_JOBS": "2"}},
           "node":  {"mem_mb": 2500}}}
```

- `match`: a command name (`tsc`) or a name and its first argument (`next build`).
- `scripts`: package-manager script names (`pnpm test`); not matched for a bare command, so
  `/bin/test` is not caught.
- `mem_budget_mb`: set it to fix the budget yourself instead of RAM minus `reserve_mb`.
- Your own pools work the same: `"docker": {"slots": 1, "mem_mb": 4000, "match": ["docker"]}`.

Changes apply on the next scheduling pass.

## Limits

- Only commands started with `tim run` or `tim submit` are queued. A build started without it is
  not held back; tim only sees the memory it uses.
- `mem_mb` is your estimate. tim measures what a job uses but does not cap it.
- The job list of `tim status --all` is kept in memory and is lost when the daemon restarts;
  the measured peaks in `history.jsonl` stay.
- One daemon per user. Two users on one machine have two independent queues.

## For AI agents

Tell the agent: "run every command that compiles as `tim run -- <command>`; a `waiting for a build
slot` line is expected, wait for it; never add `--priority` unless I ask for it." `tim status --json` gives it the queue.
