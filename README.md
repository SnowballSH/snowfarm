# snowfarm

`snowfarm` is the supervisor for a farm of Hermes agents running on one host:
it provisions each agent's Unix account and Hermes profile, dispatches the
shared Kanban board to the workers, guards the Discord-facing managers, and
exposes the farm's metrics. This repository also builds `farm-claude`, the
wrapper every agent calls to reach Claude Code.

Nothing here calls a model. The supervisor renders configuration, starts and
stops units, reads the board, and reports.

Two references go with it:

- [`docs/roster.md`](docs/roster.md) — every `farm.yaml` field and its
  validation rule.
- [`docs/host-contract.md`](docs/host-contract.md) — every path, permission
  and command the supervisor creates or runs on the host, as tables to read
  against a running host.

## Status

The supervisor is built: the roster schema, the renderers and the root
applier, the Discord reconciler and event log, the read-only board reader, the
dispatch loop and run termination, the schedules, the hygiene sweep, the
secrets socket, and the guard itself — the manager breaker, the drained
nightly restarts, the probes, `#farm-control`, the Prometheus endpoint, and
the Claude Code run and limit counters. Nothing has run on the host.

## The roster

One `farm.yaml` declares the farm: the agents, the teams, the scheduled cards
and the guard's thresholds. `internal/roster` loads it, applies the
conservative defaults, and validates it — strictly, so an unknown key is an
error, and every failure is reported at once.

```yaml
farm:
  location: America/New_York
  uid_base: 6000
  metrics_addr: 100.64.0.2:9110
  modelgate: {api: https://gateway.example.invalid/v1}
  discord:
    guild_id: "100000000000000001"
    operator_user_id: "100000000000000002"
    supervisor_application_id: "100000000000000003"

teams:
  - {name: swe, label: "TEAM · SOFTWARE ENGINEERING"}

agents:
  - name: alto
    tier: manager
    teams: [swe]
    model: gpt-5.6-sol
    reasoning: medium
    discord_application_id: "200000000000000001"
    toolsets: [kanban, web, file, memory, vision, cronjob, terminal, discord, skills]
    skills: [farm-claude-code, farm-subscribe]
    limits: {slice_mib: 2048, cpu_percent: 150}
    env: [CLAUDE_CODE_OAUTH_TOKEN, DISCORD_BOT_TOKEN, FARM_MODELGATE_KEY]
    context_length: 400000
    enabled: true

  - name: brio
    tier: worker
    teams: [swe]
    model: gpt-5.6-luna
    reasoning: xhigh
    toolsets: [kanban, terminal, file, web, memory, skills]
    skills: [farm-claude-code]
    disabled_tools: [skill_manage, kanban_create, kanban_link]
    limits: {slice_mib: 2048, run_mib: 1792, cpu_percent: 150}
    env: [CLAUDE_CODE_OAUTH_TOKEN, FARM_MODELGATE_KEY]
    context_length: 400000
    enabled: true

schedules:
  - name: morning-check
    cron: "0 8 * * *"
    title: "Morning check"
    body: "Read the overview dashboard and report anything anomalous since yesterday."
    assignee: brio
    notifier: alto
    channel: "#swe-log"
    max_runtime: 2h

guard:
  max_concurrent_runs: 1
  max_claude_slots: 2
  dispatch_interval: 30s
  restart_window: "04:00-05:00"
  drained_restarts: false
```

The rules that matter most are the ones that keep an agent able to reach
Claude Code and a worker able to terminate its own card: `terminal`, `skills`
and `kanban` in the toolsets, `farm-claude-code` in the skills,
`CLAUDE_CODE_OAUTH_TOKEN` in the environment names, and — on a worker — only
`skill_manage`, `kanban_create` and `kanban_link` given up. `env` lists names,
never values.

Each agent's `SOUL.md` is two parts: the tier template every manager or worker
shares, and `<soul_dir>/<agent>.md`, the agent-specific paragraphs the
deployment repository ships as `soul/`. The operator copies that directory to
`soul_dir` (default `/etc/snowfarm/soul`, the directory `0755 root:root` and
the files `0644`) before `snowfarm apply`. The render is the tier template,
one blank line, then the persona file verbatim; an agent the directory has no
file for gets the tier template alone, a persona the renderer cannot read
fails the render rather than dropping it, and editing one shows as an update
to that agent's `SOUL.md` on the next `snowfarm plan`.

`enabled` is the phase gate: the guard dispatches for, schedules for and
manages only the agents marked `enabled`, so an agent declared for a later
phase is never dispatched-as before its account exists. A running guard picks
up a change to it on `snowfarm reload` (SIGHUP), never on `apply` alone.

An agent's uid is `uid_base` plus a bounded hash of its **name**, so inserting
or reordering agents never moves an existing uid — `useradd --uid` fixes it at
the first apply.

## Commands

```
snowfarm plan|apply [--config PATH] [--only a,b,c] [--dry-run] [--start]
snowfarm guard [--config PATH] [--state DIR] [--secrets DIR] [--age-identity PATH]
               [--socket PATH] [--pins PATH] [--pidfile PATH]
snowfarm reload [--pidfile PATH]
snowfarm secret-env [--socket PATH]
snowfarm version
```

## The host contract

`snowfarm apply` converges the host from the roster, as root, idempotently.
[`docs/host-contract.md`](docs/host-contract.md) is the auditable version; in
outline:

- **Users.** One system group `farm-agents`, one `farm-<agent>` user per agent
  with a name-keyed uid, a nologin shell and linger enabled. The Unix user is
  the boundary — there is no container runtime, so every agent's tools and
  Claude Code runs execute on the host as that user.
- **Paths.** The guard's state in `/var/lib/snowfarm`, the shared board and
  Claude Code trees under `/srv/snowfarm`, one home per agent under
  `/var/lib/farm` with the rendered profile inside it. `config.yaml` and
  `SOUL.md` are `root:farm-<agent>` 0640 and immutable, as are
  `<home>/.hermes` and `<home>/.hermes/profiles`: the flags, not the
  ownership, are what stop an agent unlinking or renaming its way to a profile
  of its own. The host check that proves it is
  [`docs/profile-immutability-drill.md`](docs/profile-immutability-drill.md).
- **`farm-unitctl`.** One sudoers line lets the guard run one wrapper as any
  agent. The wrapper derives the agent from the caller's uid and allows a
  fixed verb list: the gateway verbs, a spawning `run-dispatch`, a
  spawn-nothing `run-maintenance`, `run-stop` on this agent's own run units,
  `run-list`, and a short list of `hermes kanban` subcommands. Everything else
  exits 2.
- **The socket.** The guard listens on `/run/snowfarm/guard.sock` in the
  setgid `/run/snowfarm`. An agent runs `snowfarm secret-env` and is served
  the variables of the agent its **peer uid** belongs to; it asks for nothing
  and carries no credential.
- **The wrapper.** `farm-claude` is how an agent reaches Claude Code, and its
  directory, slot budget, containment flags and deny rules are compiled in.

## The guard

`snowfarm guard` is the long-running half. It loads the roster, decrypts the
age files and binds the metrics address **before** it opens a Discord
connection, so a guard that cannot start costs no session from a bot's daily
budget. That bind waits, for up to two minutes, while the host says the
address is not assigned to any interface: on a cold boot the tailnet address
appears after `tailscaled` reports started, and the wait is what keeps the
unit's start limit from stopping the guard until an operator resets it. It does not wait for the board: `kanban.db` is Hermes' file and a fresh host
has none, while the gateway that would create one cannot start until an apply
has read the channel ids only a reconciled guard writes, so the guard warns
once and every board-reading pass fails until the file appears.
It then reconciles the guild, records the channel ids in
`<state>/channels.json` for `snowfarm apply` to read, and runs the dispatch
tick, the schedules, the hygiene sweep, the manager breaker, the nightly
drained restarts, the model-gateway, Google-token and Hermes-pin probes, the
Claude Code run tail, the secrets socket, and `/metrics` on the roster's
`metrics_addr`.

The breaker reads each manager's `gateway.log`: past its turn or
operator-mention allowance the manager is paused for thirty minutes, a burst
of Discord 401/403/429 responses stops it until someone resumes it, and a
Hermes adapter circuit-breaker trip restarts it inside a restart budget and
only while that manager's own bot still has session starts to spend. Its first
pass over a log it holds no offset for — and the pass after a log is rotated
in place — records where that log ends and acts on nothing already in it, so a
guard starting beside managers that have been running for months does not read
their history as a rate happening now.

A manager is safe to restart only between turns, and a turn is open from its
start line until its completion line. Those open turns are kept in the ledger
beside the log offset they were read from: a guard that restarts takes up the
set the one before it left rather than finding every manager quiet and
aborting a turn that is still running. Reading those two lines is what
`guard.turn_log_pattern` and `guard.turn_complete_pattern` do, and neither has
a default, so the nightly restart is armed by `guard.drained_restarts: true`
and a roster that arms it without both patterns does not load.

A dispatched run's exit status comes only from the journal line `Main process
exited, code=exited, status=N` — the transient unit is `--collect`ed, so
nothing remains to ask afterwards — and whether a run is still active is
decided by its presence in `farm-unitctl run-list`.

In `#farm-control`, and only from the operator's own account, the farm answers
`status`, `pause <manager>`, `resume <manager>`, `stop <task>` and `runs`.

`snowfarm reload` sends SIGHUP to the pid in the guard's pid file. The guard
re-reads `farm.yaml` and the age files and re-registers the schedules, keeping
its ledger; a `farm.yaml` that no longer validates is refused and the running
roster kept.

The Discord event log is kept encrypted at rest. An age file cannot be
appended to, so every flush writes its own complete
`events-YYYY-MM-DDTHH-<seq>.jsonl.age`, and a day is read by concatenating
that day's segments in name order.

## `farm-claude`

The wrapper every agent calls to reach Claude Code:

```
farm-claude -p PROMPT [--model M] [--effort E] [--max-turns N]
            [--add-dir D]... [--append-system-prompt S]
```

It takes one of the farm's Claude slots and waits up to twenty minutes for
one, refuses to start inside a limit window, and passes the containment flags
and the deny rules no caller can weaken. Every run appends a line to
`<claude_dir>/runs/<agent>.jsonl`, which is what the guard counts and what it
writes the farm-wide limit marker from.

`--model` defaults to `claude-opus-5`. `claude-fable-5-1` is the stronger and
more expensive model, and an agent asks for it only when a task is genuinely
hard to reason about rather than merely long.

How many slots there are is `guard.max_claude_slots`: `snowfarm apply` creates
that many files under `<claude_dir>/shared/slots`, and the wrapper counts
them. There is no environment variable for it.

The agent is the uid the run is under, and the subscription is
`CLAUDE_CODE_OAUTH_TOKEN`; neither is a flag. It exits 3 when no slot came
free, 4 when the farm is already inside a limit window, 5 when the run itself
hit one, and 1 on any other failure.

## Development

```
CGO_ENABLED=0 go build ./... && test -z "$(gofmt -l .)" && go vet ./... \
  && go test -race ./... && golangci-lint run && govulncheck ./...
```

Golden files under `internal/render/testdata/golden` are regenerated with
`go test ./internal/render -update`.

## License

MIT. See [LICENSE](LICENSE).
