# snowfarm

`snowfarm` is the supervisor for a farm of Hermes agents running on one host:
it provisions each agent's Unix account and Hermes profile, dispatches the
shared Kanban board to the workers, guards the Discord-facing managers, and
exposes the farm's metrics. This repository also builds `farm-claude`, the
wrapper every agent calls to reach Claude Code.

Nothing here calls a model. The supervisor renders configuration, starts and
stops units, reads the board, and reports.

## Status

The supervisor is built: the roster schema, the renderers and the root
applier, the Discord reconciler and event log, the read-only board reader, the
dispatch loop and run termination, the schedules, the hygiene sweep, the
secrets socket, and the guard itself — the manager breaker, the drained
nightly restarts, the probes, `#farm-control` and the Prometheus endpoint. The
Claude Code run counters the wrapper writes are not read yet. Nothing has run
on the host.

## The roster

One `farm.yaml` declares the farm: the agents (name, tier, team, model and
reasoning level, toolsets, limits, MCP servers), the teams, the scheduled
cards and the guard's thresholds. `internal/roster` loads it, applies the
conservative defaults, and validates it — including the rules that keep an
agent able to reach Claude Code and a worker able to terminate its own card.

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

## The guard

`snowfarm guard` is the long-running half. It loads the roster, decrypts the
age files and binds the metrics address **before** it opens a Discord
connection, so a guard that cannot start costs no session from a bot's daily
budget. It then reconciles the guild, records the channel ids in
`<state>/channels.json` for `snowfarm apply` to read, and runs the dispatch
tick, the schedules, the hygiene sweep, the manager breaker, the nightly
drained restarts, the model-gateway, Google-token and Hermes-pin probes, the
secrets socket, and `/metrics` on the roster's `metrics_addr`.

The breaker reads each manager's `gateway.log`: past its turn or
operator-mention allowance the manager is paused for thirty minutes, a burst
of Discord 401/403/429 responses stops it until someone resumes it, and a
Hermes adapter circuit-breaker trip restarts it inside a restart budget and
only while that manager's own bot still has session starts to spend.

In `#farm-control`, and only from the operator's own account, the farm answers
`status`, `pause <manager>`, `resume <manager>`, `stop <task>` and `runs`.

`snowfarm reload` sends SIGHUP to the pid in the guard's pid file. The guard
re-reads `farm.yaml` and the age files and re-registers the schedules, keeping
its ledger; a `farm.yaml` that no longer validates is refused and the running
roster kept.

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

The agent is the uid the run is under, and the subscription is
`CLAUDE_CODE_OAUTH_TOKEN`; neither is a flag. It exits 3 when no slot came
free, 4 when the farm is already inside a limit window, 5 when the run itself
hit one, and 1 on any other failure.

## Development

```
CGO_ENABLED=0 go build ./... && go vet ./... && go test -race ./... && golangci-lint run
```

## License

MIT. See [LICENSE](LICENSE).
