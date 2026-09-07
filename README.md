# snowfarm

`snowfarm` is the supervisor for a farm of Hermes agents running on one host:
it provisions each agent's Unix account and Hermes profile, dispatches the
shared Kanban board to the workers, guards the Discord-facing managers, and
exposes the farm's metrics. This repository also builds `farm-claude`, the
wrapper every agent calls to reach Claude Code.

Nothing here calls a model. The supervisor renders configuration, starts and
stops units, reads the board, and reports.

## Status

Early scaffold. Today the repository holds the roster schema
(`internal/roster`) and the command dispatch of `cmd/snowfarm`; every
subcommand but `version` returns `not implemented`, and `farm-claude` is not
built yet.

## The roster

One `farm.yaml` declares the farm: the agents (name, tier, team, model and
reasoning level, toolsets, limits, MCP servers), the teams, the scheduled
cards and the guard's thresholds. `internal/roster` loads it, applies the
conservative defaults, and validates it — including the rules that keep an
agent able to reach Claude Code and a worker able to terminate its own card.

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
snowfarm guard
snowfarm reload
snowfarm secret-env
snowfarm version
```

## Development

```
CGO_ENABLED=0 go build ./... && go vet ./... && go test -race ./... && golangci-lint run
```

## License

MIT. See [LICENSE](LICENSE).
