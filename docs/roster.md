# The roster

One `farm.yaml` declares the whole farm. `snowfarm plan`, `snowfarm apply` and
`snowfarm guard` read it and nothing else; `snowfarm reload` re-reads it into a
running guard.

Loading is strict. An unknown key is an error, not a warning, and every rule
below is checked at load: `plan`, `apply` and `guard` all refuse a roster that
does not validate, and a reload refuses the new file and keeps the running
one. Validation reports every failure at once rather than the first.

The paths and permissions the roster's values become are in
[`host-contract.md`](host-contract.md).

## An example

```yaml
farm:
  home_root: /var/lib/farm
  kanban_home: /srv/snowfarm/kanban
  claude_dir: /srv/snowfarm/claude
  hermes_bin: /usr/local/bin/hermes
  soul_dir: /etc/snowfarm/soul
  location: America/New_York
  uid_base: 6000
  metrics_addr: 100.64.0.2:9110
  modelgate: {api: https://gateway.example.invalid/v1}
  discord:
    guild_id: "100000000000000001"
    operator_user_id: "100000000000000002"
    supervisor_application_id: "100000000000000003"

teams:
  - {name: research, label: "TEAM · RESEARCH"}
  - {name: swe,      label: "TEAM · SOFTWARE ENGINEERING"}

agents:
  - name: alto
    tier: manager
    teams: [research, swe]
    model: gpt-5.6-sol
    reasoning: medium
    discord_application_id: "200000000000000001"
    toolsets: [kanban, web, file, memory, vision, cronjob, terminal, discord, skills]
    skills: [farm-claude-code, farm-subscribe]
    limits: {slice_mib: 2048, cpu_percent: 150}
    env: [CLAUDE_CODE_OAUTH_TOKEN, DISCORD_BOT_TOKEN, FARM_MODELGATE_KEY]
    max_iterations: 120
    context_length: 400000
    enabled: true
    modelgate_key_minted: "2026-09-10"

  - name: brio
    tier: worker
    teams: [swe]
    model: gpt-5.6-luna
    reasoning: xhigh
    toolsets: [kanban, terminal, file, web, memory, skills, mcp-notes]
    skills: [farm-claude-code]
    disabled_tools: [skill_manage, kanban_create, kanban_link]
    limits: {slice_mib: 2048, run_mib: 1792, cpu_percent: 150}
    env: [CLAUDE_CODE_OAUTH_TOKEN, FARM_MODELGATE_KEY, FARM_FORGE_TOKEN]
    allow_private_urls: true
    mcp_servers:
      - name: notes
        command: /usr/bin/node
        args: [/usr/local/lib/notes-mcp/build/index.js]
        env: {NOTES_CREDENTIALS: /var/lib/farm/brio/notes/client.json}
        version: "2.6.3"
        sha256: ""
    max_iterations: 80
    context_length: 400000
    enabled: true
    modelgate_key_minted: ""

schedules:
  - name: morning-check
    cron: "0 8 * * *"
    location: America/New_York
    title: "Morning check"
    body: "Read the overview dashboard and report anything anomalous since yesterday. Complete with a short summary; block with needs_input only if you need the operator."
    assignee: brio
    notifier: alto
    channel: "#swe-log"
    max_runtime: 2h
    priority: 5

guard:
  max_concurrent_runs: 1
  max_claude_slots: 2
  manager_turns_per_hour: 30
  operator_mentions_per_hour: 6
  burst_per_5m: 20
  restart_budget_per_6h: 6
  dispatch_interval: 30s
  default_max_runtime: 2h
  hygiene_interval: 5m
  restart_window: "04:00-05:00"
```

## `farm`

| Field | Default | Rule |
|---|---|---|
| `home_root` | `/var/lib/farm` | each agent's home is `<home_root>/<agent>` |
| `kanban_home` | `/srv/snowfarm/kanban` | the board root every unit, mount and environment line cites. Hermes nests its tree under a `kanban/` subdirectory of it, so the workspaces and attachments roots are derived, not configured |
| `claude_dir` | `/srv/snowfarm/claude` | holds `shared/slots`, `shared/limit-until` and `runs/` |
| `hermes_bin` | `/usr/local/bin/hermes` | what apply and the pin-drift probe run |
| `soul_dir` | `/etc/snowfarm/soul` | must be an absolute path: apply reads each agent's persona from it |
| `location` | — | required, and must be an IANA zone the host's tzdata knows. It sets the manager report schedule and every schedule that names no location of its own |
| `uid_base` | `6000` | must be at least `6000`; below that an agent would keep the instance-role reach the host's egress rule drops by uid |
| `metrics_addr` | — | the address `snowfarm guard` binds `/metrics` on. A tailnet address; the guard binds it before it opens a Discord connection, so a bad address costs no bot session |
| `modelgate.api` | — | the OpenAI-compatible base URL every agent's provider block points at, and what the liveness probe requests |
| `discord.guild_id` | — | the guild the reconciler converges |
| `discord.operator_user_id` | — | **required**: a manager rendered without a human allow-list would act on bot mentions |
| `discord.supervisor_application_id` | — | the supervisor's own application, which posts status and serves `#farm-control` |

An agent's uid is `uid_base` plus a bounded FNV-1a hash of its **name** (span
`1000`), never its position in this file. Two agents whose names hash to the
same uid is a validation error naming both: rename one.

## `agents`

| Field | Default | Rule |
|---|---|---|
| `name` | — | matches `^[a-z][a-z0-9-]{1,15}$`, unique across the roster, and never `default`, which is Hermes' fallback profile. It is the profile name, the Unix user `farm-<name>`, and the uid |
| `tier` | — | `manager` or `worker` |
| `teams` | none | every entry must name a team declared in `teams` |
| `model` | — | required; the default model in the rendered profile |
| `reasoning` | — | one of `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max` |
| `discord_application_id` | — | required on a manager, and must be empty on a worker: workers hold no Discord application |
| `toolsets` | — | must list `terminal` and `skills`, or Claude Code is unreachable; must list `kanban`, without which there is no `kanban_complete` and no card can terminate. One `mcp-<name>` entry per `mcp_servers` entry |
| `disabled_toolsets` | none | must not list `terminal` or `skills`; on a worker must not list `kanban` |
| `disabled_tools` | none | a worker must list `skill_manage`, `kanban_create` and `kanban_link` — it keeps the kanban toolset and gives up only those three. Anything else here is added to the tier's own set |
| `skills` | — | must list `farm-claude-code`, or the skill body never reaches the model. Each entry must be a skill this supervisor ships: `farm-claude-code`, `farm-subscribe` |
| `limits.slice_mib` | — | positive; the agent's slice `MemoryMax`, with `MemoryHigh` at 85% of it |
| `limits.cpu_percent` | — | positive; the slice `CPUQuota` |
| `limits.run_mib` | — | positive on a worker; the `MemoryMax` on one transient run |
| `env` | — | **names, never values.** Each must start with `FARM_`, or be `CLAUDE_CODE_OAUTH_TOKEN` or `DISCORD_BOT_TOKEN`. The list must include `CLAUDE_CODE_OAUTH_TOKEN`, or Claude Code fails on authentication. The values live in `/etc/snowfarm/secrets/<agent>.age` and reach the agent over the guard's socket |
| `allow_private_urls` | `false` | lets this agent's `web` tools reach private addresses |
| `mcp_servers` | none | `name`, `command`, `args`, `env`, `version`, `sha256`. Each needs its `mcp-<name>` toolset. `version` and `sha256` are provenance the operator's install ceremony pins; the fetch check is the integrity gate |
| `max_iterations` | 120 manager, 80 worker | the gateway's `HERMES_MAX_ITERATIONS`, and a run's own ceiling |
| `context_length` | — | positive; Hermes bounds its own context to it |
| `enabled` | `false` | the phase gate. The guard dispatches for, schedules for and manages only enabled agents, and `apply` provisions only enabled agents, so an agent declared for a later phase is never dispatched-as before its account exists. A running guard picks up a change on `snowfarm reload`, never on `apply` alone |
| `modelgate_key_minted` | `""` | empty, or `YYYY-MM-DD`. It feeds `snowfarm_modelgate_key_age_seconds`; it is a record of the mint ceremony, not a credential |

`--only` must name a subset of the **enabled** set: naming a disabled agent
asks for work `apply` will not do, and naming an unknown one is a typo.

### `SOUL.md`

An agent's persona is not in this file. `SOUL.md` is the tier template every
manager or worker shares, one blank line, then `<soul_dir>/<agent>.md`
verbatim. An agent the directory has no file for gets the tier template alone;
a persona the renderer cannot read fails the render rather than dropping it.
See the `soul_dir` contract in [`host-contract.md`](host-contract.md).

### Claude Code

Every agent reaches Claude Code through `farm-claude`, whose `--model`
defaults to `claude-opus-5`. `claude-fable-5-1` is the stronger and more
expensive model, asked for only when a task is genuinely hard to reason about
rather than merely long. Nothing in the roster changes that: the wrapper's
defaults and deny rules are compiled in.

## `teams`

| Field | Rule |
|---|---|
| `name` | the team's own name; agents reference it, and the reconciler builds `#<name>-general` and `#<name>-log` from it |
| `label` | the category label the Discord guild shows |

## `schedules`

A schedule creates one Kanban card on a cron, as the notifying manager, and
subscribes it to a channel so the outcome is reported there.

| Field | Default | Rule |
|---|---|---|
| `name` | — | the schedule's identity, and half of its idempotency key: the other half is the day it fired, in its own location, so a second fire of the same morning creates nothing |
| `cron` | — | a standard five-field expression |
| `location` | `farm.location` | must be an IANA zone |
| `title`, `body` | — | the card. The body is the worker's whole brief, so it says what to do, what to report, and how to terminate |
| `assignee` | — | must be a **worker** |
| `notifier` | — | must be a **manager**: it creates the card and owns the bot that reports the outcome |
| `channel` | — | must start with `#`. The guard resolves it to an id through the channel map its reconciler wrote |
| `max_runtime` | none | a Go duration passed to the card; a card without one is bounded by `guard.default_max_runtime` instead |
| `priority` | `0` | the card's priority |

## `guard`

| Field | Default | Rule |
|---|---|---|
| `max_concurrent_runs` | `1` | positive; how many worker runs the farm allows at once |
| `max_claude_slots` | `2` | positive; **`apply` creates exactly this many slot files**, and `farm-claude` counts them. There is no environment override |
| `manager_turns_per_hour` | `30` | positive; past it the manager is paused for thirty minutes |
| `operator_mentions_per_hour` | `6` | positive; the same, counted over operator mentions |
| `burst_per_5m` | `20` | positive; Discord 401/403/429 responses in five minutes that stop a manager until someone resumes it |
| `restart_budget_per_6h` | `6` | positive; restarts the guard may spend on adapter-trip recovery |
| `dispatch_interval` | `30s` | positive Go duration; the dispatch tick |
| `default_max_runtime` | `2h` | positive; what bounds a running card created without a `max_runtime` |
| `hygiene_interval` | `5m` | positive; the sweep that hashes profiles, sizes the board and removes forged notifier subscriptions |
| `housekeeping_passes` | `true` | an explicit `false` suspends the maintenance pass, and never the guard's own backstop |
| `restart_window` | `04:00-05:00` | the nightly drained-restart window. Setting it requires `turn_complete_pattern`: a turn-start line alone never proves the turn ended |
| `turn_log_pattern` | a placeholder | must compile; matches the gateway log line that opens a manager's turn |
| `turn_complete_pattern` | a placeholder | must compile; matches the line that closes one |

Both turn patterns ship as placeholders until a real `gateway.log` sample pins
them. The breaker counts turns with them, and the drained restarter is armed
only once they are right.
