# The host contract

Everything `snowfarm apply` creates on the host, everything the supervisor
runs there, and the checks that prove a host still matches. Read it with
`stat`, `getfacl`, `lsattr`, `systemctl` and `sudo -l` against a running host:
every row is an assertion, not a description.

Paths are the roster's defaults. A roster that moves `farm.home_root`,
`farm.kanban_home`, `farm.claude_dir` or `farm.soul_dir` moves every row that
quotes them; the fields are in [`roster.md`](roster.md).

## Accounts

| Account | Made by | How |
|---|---|---|
| `farm-agents` (group) | `snowfarm apply` | `groupadd --system farm-agents` |
| `snowfarm` (user) | the host bootstrap | system user, home `/var/lib/snowfarm`, shell `/usr/sbin/nologin`, in `systemd-journal` and `farm-agents` |
| `farm-<agent>` (user) | `snowfarm apply` | `useradd --system --uid <uid> --home-dir /var/lib/farm/<agent> --create-home --shell /usr/sbin/nologin --user-group farm-<agent>`, then `usermod -aG farm-agents` and `loginctl enable-linger` |

An agent's uid is `farm.uid_base` plus a bounded FNV-1a hash of its **name**
(floor and default `6000`, span `1000`), so inserting, reordering or removing
an agent never moves another agent's uid. `useradd --uid` fixes it at the
first apply; two names that hash alike are a validation error, not a
collision on the host.

The Unix user is the boundary. There is no container runtime: every agent's
tools, `file` operations and Claude Code runs execute on the host as
`farm-<agent>`.

## Directories `snowfarm apply` creates

| Path | Mode | Owner:Group | What it holds |
|---|---|---|---|
| `/var/lib/snowfarm` | 0750 | `snowfarm:snowfarm` | the guard's state: ledger, channel map, profile hashes, event log |
| `/srv/snowfarm` | 0755 | `root:root` | the shared root, traversed by every agent |
| `/srv/snowfarm/kanban` | 2770 | `root:farm-agents` | the board: `kanban.db`, and Hermes' `kanban/` tree of workspaces, attachments and worker logs |
| `/srv/snowfarm/claude` | 0755 | `root:root` | the Claude Code tree, traversed by every agent |
| `/srv/snowfarm/claude/shared` | 0750 | `snowfarm:farm-agents` | the slot directory and the farm-wide limit marker; agents read, only the guard writes |
| `/srv/snowfarm/claude/shared/slots` | 0750 | `snowfarm:farm-agents` | one lock file per Claude Code slot |
| `/srv/snowfarm/claude/runs` | 0750 | `snowfarm:farm-agents` | one run ledger per agent |
| `/usr/local/lib/snowfarm` | 0755 | `root:root` | data `farm-unitctl` sources |
| `/usr/local/lib/snowfarm/limits` | 0755 | `root:root` | the per-agent run limits |
| `/var/lib/farm/<agent>` | 0750 | `farm-<agent>:farm-<agent>` | the agent's home: `.claude.json`, the Claude config directory, its work |
| `/var/lib/farm/<agent>/.hermes` | 0755 | `farm-<agent>:farm-<agent>` | **immutable** (`chattr +i`) |
| `/var/lib/farm/<agent>/.hermes/profiles` | 0755 | `farm-<agent>:farm-<agent>` | **immutable** (`chattr +i`) |
| `/var/lib/farm/<agent>/.hermes/profiles/<agent>` | 0750 | `farm-<agent>:farm-<agent>` | the profile: `config.yaml`, `SOUL.md`, `skills/`, `state.db` |
| `/var/lib/farm/<agent>/.hermes/profiles/<agent>/logs` | 0750 | `farm-<agent>:farm-<agent>` | `gateway.log`, which the guard's breaker tails |
| `/var/lib/farm/<manager>/.config/systemd/user` | 0750 | `farm-<manager>:farm-<manager>` | the gateway unit (managers only, with its two parents and `default.target.wants`) |

## Files `snowfarm apply` installs

| Path | Mode | Owner:Group | Content |
|---|---|---|---|
| `/usr/local/sbin/farm-unitctl` | 0755 | `root:root` | rendered; the only privileged surface the guard has |
| `/etc/sudoers.d/snowfarm` | 0440 | `root:root` | `snowfarm ALL=(%farm-agents) NOPASSWD: /usr/local/sbin/farm-unitctl` |
| `/etc/tmpfiles.d/snowfarm.conf` | 0644 | `root:root` | `d /run/snowfarm 2750 snowfarm farm-agents -` |
| `/etc/systemd/system/snowfarm-guard.service` | 0644 | `root:root` | rendered from the roster's paths |
| `/etc/systemd/system/user-<uid>.slice.d/50-snowfarm.conf` | 0644 | `root:root` | `MemoryMax`, `MemoryHigh` (85% of it), `MemorySwapMax=0`, `CPUQuota` |
| `/usr/local/lib/snowfarm/limits/<agent>` | 0644 | `root:root` | `RUN_MEMORY_MAX`, `RUN_TASKS_MAX`, `RUN_MAX_ITERATIONS` (agents with `limits.run_mib`) |
| `/var/lib/farm/<agent>/.hermes/profiles/<agent>/config.yaml` | 0640 | `root:farm-<agent>` | rendered; **immutable** |
| `/var/lib/farm/<agent>/.hermes/profiles/<agent>/SOUL.md` | 0640 | `root:farm-<agent>` | rendered; **immutable** |
| `/var/lib/farm/<agent>/.hermes/profiles/<agent>/skills/<skill>/SKILL.md` | 0600 | `farm-<agent>:farm-<agent>` | rendered from the supervisor's own skill bodies |
| `/var/lib/farm/<manager>/.config/systemd/user/hermes-gateway.service` | 0644 | `farm-<manager>:farm-<manager>` | rendered; a unit still carrying `pending-reconcile` is installed and **not** started |
| `/var/lib/farm/<manager>/.config/systemd/user/default.target.wants/hermes-gateway.service` | symlink | `farm-<manager>:farm-<manager>` | enables the unit without `systemctl --user enable` |
| `/var/lib/snowfarm/profile-hashes.json` | 0640 | `snowfarm:snowfarm` | the hygiene sweep's baseline: the sha256 of each agent's `config.yaml` and `SOUL.md` |
| `/srv/snowfarm/claude/shared/slots/1` … `/N` | 0640 | `snowfarm:farm-agents` | empty; **N is `guard.max_claude_slots`** |
| `/srv/snowfarm/claude/runs/<agent>.jsonl` | 0640 | `farm-<agent>:farm-agents` | created empty and never rewritten; the wrapper appends |

Apply creates the slot and run-ledger files but owns no byte of their
content: it converges their existence, mode and owner only.

### Slots are a roster field, not an environment knob

There is **no** `FARM_CLAUDE_SLOTS` variable anywhere on the host. `snowfarm
apply` creates `<claude_dir>/shared/slots/1..N` from `guard.max_claude_slots`,
and `farm-claude` counts the files it finds there. Lowering the roster value
leaves the extra files behind; remove them by hand after confirming no run
holds one. A slots directory with no files is a loud error in the wrapper, not
a lock nobody can see.

### The run ledger is the one agent-writable path under `claude_dir`

`runs/` is 0750 `snowfarm:farm-agents` — group search, no group write — and
each `runs/<agent>.jsonl` is 0640 `farm-<agent>:farm-agents`. So an agent
appends to its own ledger and to nothing else: it cannot write another agent's
ledger, cannot create a ledger, and cannot write `shared/`. The guard reads
every ledger, counts the runs, and is the only writer of
`shared/limit-until`.

## What the operator installs, and apply never touches

| Path | Mode | Owner | Notes |
|---|---|---|---|
| `/usr/local/bin/snowfarm`, `/usr/local/bin/farm-claude` | 0755 | `root:root` | this repository's two binaries, verified against `SHA256SUMS` |
| `/usr/local/bin/hermes`, `/usr/local/bin/claude` | 0755 | `root:root` | the pinned Hermes and Claude Code |
| `/etc/snowfarm` | 0700 | `snowfarm` | everything the guard reads and no agent may |
| `/etc/snowfarm/farm.yaml` | — | `snowfarm` | the roster |
| `/etc/snowfarm/age.key` | 0400 | `snowfarm` | the identity that decrypts the secrets and the event log |
| `/etc/snowfarm/secrets` | 0700 | `snowfarm` | the per-agent age files (a 0400 directory has no search bit) |
| `/etc/snowfarm/secrets/<agent>.age` | 0400 | `snowfarm` | that agent's variables, encrypted |
| `/etc/snowfarm/secrets/supervisor.age` | 0400 | `snowfarm` | the guard's own credentials — the supervisor bot token and its probe key. It is not a roster agent and no uid is served it |
| `/etc/snowfarm/pins.yaml` | — | `snowfarm` | what the Hermes drift probe compares against |
| `/etc/snowfarm/soul` | 0755 | `root:root` | the persona directory, `farm.soul_dir` |
| `/etc/snowfarm/soul/<agent>.md` | 0644 | `root:root` | that agent's persona paragraphs |

### The `soul_dir` contract

Each agent's `SOUL.md` is the tier template, one blank line, then
`<soul_dir>/<agent>.md` verbatim. The operator copies the deployment
repository's `soul/` directory to `farm.soul_dir` **before** `snowfarm apply`.

- A persona file that is absent leaves the tier template alone, so a host
  where `soul/` has not been staged still plans and applies.
- A persona file that cannot be read, or is not a regular file (a symlink
  included), fails the render. Nothing is silently dropped.
- Editing one shows as `file …/SOUL.md: content differs` on the next
  `snowfarm plan`, and as a new baseline hash on the apply that follows.

## Runtime state, created by the guard rather than by apply

| Path | Mode | Owner:Group | Written by |
|---|---|---|---|
| `/run/snowfarm` | 2750 | `snowfarm:farm-agents` | `systemd-tmpfiles`, from the entry above |
| `/run/snowfarm/guard.sock` | 0660 | `snowfarm:farm-agents` | the guard; each agent asks it for its own variables and is answered by peer uid |
| `/run/snowfarm/guard.pid` | 0644 | `snowfarm:farm-agents` | the guard, for `snowfarm reload` |
| `/var/lib/snowfarm/ledger.db` | — | `snowfarm:farm-agents` | the guard's run and turn ledger |
| `/var/lib/snowfarm/channels.json` | 0640 | `snowfarm:farm-agents` | the reconciler; `snowfarm apply` reads it to fill each manager unit's channel ids |
| `/var/lib/snowfarm/log/events-<hour>-<seq>.jsonl.age` | 0600 (directory 0700) | `snowfarm:farm-agents` | the Discord event log, one complete age file per flush |
| `/srv/snowfarm/claude/shared/limit-until` | 0640 | `snowfarm:farm-agents` | the guard, from what the run ledgers report; removed when the window ends |
| `/srv/snowfarm/kanban/**` | — | `farm-<agent>:farm-agents` | Hermes, as each agent; the setgid board directory is what puts every file in the shared group |
| `/srv/snowfarm/kanban/kanban.db` | — | `farm-<agent>:farm-agents` | Hermes, on the first command that touches the board. Neither `snowfarm apply` nor the guard ever creates or writes it |

### The guard does not wait for the board

The bootstrap would deadlock if it did. A manager's gateway unit is installed
carrying `pending-reconcile` and is **not** started until an apply has read
the channel ids, and only a guard that has reconciled the guild writes them —
while `kanban.db` is Hermes' file, which no Hermes process has created on a
fresh host. So `snowfarm guard` starts without a board: it warns once with the
path it looked at, reconciles the guild, and every pass that reads the board —
the dispatch tick, the hygiene sweep, the board-size health probe, and the
`status` and `runs` controls — fails and says so until the file appears. No
restart is needed once it does.

The guard's own files take the group its unit gives it, `farm-agents`; what
keeps them unreadable is `/var/lib/snowfarm` itself, 0750 `snowfarm:snowfarm`.

The event log is segmented on purpose: an age file cannot be appended to, so
every flush writes, closes and fsyncs its own complete
`events-YYYY-MM-DDTHH-<seq>.jsonl.age`. A day is read by concatenating that
day's segments in name order, and only the newest segment may have a
truncated payload tail.

## ACLs

The guard is in no per-agent group, so `apply` grants it exactly what it must
read and nothing else:

| Target | Entry |
|---|---|
| `<home>`, `<home>/.hermes`, `<home>/.hermes/profiles`, `<profile>`, `<profile>/logs` | `setfacl -m u:snowfarm:rx` |
| `<profile>/logs` | `setfacl -d -m u:snowfarm:rx`, so a log subdirectory inherits it |
| `<profile>/config.yaml`, `<profile>/SOUL.md` | `setfacl -m u:snowfarm:r` |

The read ACLs are set after the files are installed and before they are
frozen: a rename installs a new inode, and the kernel refuses `setxattr` on an
immutable one.

## The immutable set, and the drill that proves it

`snowfarm apply` clears the flags, writes, and restores them from a defer, so
an apply that dies part way through leaves no writable profile behind. Four
paths per agent carry `+i`:

```
/var/lib/farm/<agent>/.hermes
/var/lib/farm/<agent>/.hermes/profiles
/var/lib/farm/<agent>/.hermes/profiles/<agent>/config.yaml
/var/lib/farm/<agent>/.hermes/profiles/<agent>/SOUL.md
```

The flags, not the ownership, are the control: unlink and rename permission
comes from the containing directory, and both `<home>` and the profile
directory must stay agent-writable.

CI cannot prove this. `chattr +i` needs `CAP_LINUX_IMMUTABLE`, a tmpfs runner
returns `EOPNOTSUPP` for file flags, and no `farm-<agent>` user exists in the
runner. The unit tests assert that `Apply` issues the four `chattr +i` calls,
that a cleared flag is drift in the next `Plan`, and that a failed apply
restores every flag it cleared — and then a **host check replaces the unit
test**: run the drill once per host after the first apply, and after any
change to this code path.

Run it as the agent, from inside the profile directory, where the decoy
`other.yaml` is created first — that write must succeed — and removed
afterwards. Never stage the decoy in `/tmp`: on a tmpfs `mv` is not
`rename(2)` at all, and the step would fail for the wrong reason. All six
operations must fail:

| # | Operation | Must fail with | Because |
|---|---|---|---|
| 1 | `rm -f config.yaml` | `EPERM` | the flag on the file |
| 2 | `mv -f other.yaml config.yaml` (same-directory rename over it) | `EPERM` | the flag on the file |
| 3 | `echo x > config.yaml` | `EPERM`, or `EACCES` once the flag is cleared | the flag, then the 0640 `root:farm-<agent>` mode |
| 4 | `mv <home>/.hermes <home>/.hermes.bak` | `EPERM` | the flag on `.hermes` |
| 5 | `mv <profile> <profile>.old` | `EPERM` | the flag on `profiles` |
| 6 | `mkdir <home>/.hermes/profiles/intruder` | `EPERM` | an immutable directory admits no new entries |

Then nothing may be missing or renamed, and

```sh
lsattr -d /var/lib/farm/<agent>/.hermes \
          /var/lib/farm/<agent>/.hermes/profiles \
          /var/lib/farm/<agent>/.hermes/profiles/<agent>/config.yaml \
          /var/lib/farm/<agent>/.hermes/profiles/<agent>/SOUL.md
```

must show `i` on every line.

The drill is falsifiable, which is why the two ancestor directories are
agent-owned rather than root-owned: with the flag cleared, operations 2 and 6
**must succeed**. The runnable script, the red side, the reason the decoy is
staged inside the profile rather than in `/tmp`, and what the drill does not
cover are in
[`profile-immutability-drill.md`](profile-immutability-drill.md).

The fallback runs whether or not the flags hold: the guard hashes both files
for every agent against `/var/lib/snowfarm/profile-hashes.json`, and a read or
hash *failure* is drift, not health.

## Claude Code

`farm-claude` is the only way an agent reaches Claude Code. Its directory, its
binary, its slot timeout and the deny rules are compiled in: an agent that
could point the wrapper elsewhere would take slots the farm does not count and
write runs nothing reads.

- The agent is the uid the run is under, never a flag or a variable; the
  subscription is `CLAUDE_CODE_OAUTH_TOKEN`, delivered by the secrets socket.
- `--model` defaults to **`claude-opus-5`**. `claude-fable-5-1` is the
  stronger and more expensive model, and an agent asks for it only when a task
  is genuinely hard to reason about rather than merely long.
- `--bare`, `--dangerously-skip-permissions`, `--permission-mode`,
  `--settings`, `--setting-sources`, `--mcp-config`, `--output-format`,
  `--resume` and `--continue` are the wrapper's to set; a caller that passes
  one is refused.
- The `--settings` object disables all hooks and carries deny rules that bind
  in every permission mode: `Read`/`Edit` of `/etc/snowfarm/**` and
  `/var/lib/snowfarm/**`, `Edit` of any agent's `config.yaml` or `SOUL.md`,
  `Bash(sudo *)` and `Bash(systemctl *)`.
- Exit 3 no slot came free within twenty minutes, 4 the farm is already inside
  a limit window, 5 this run hit one, 1 anything else.
- Every run appends one line to `runs/<agent>.jsonl`. The guard folds those
  into `snowfarm_claude_runs_total`, `snowfarm_claude_run_seconds_total` and
  `snowfarm_claude_limit_hits_total`, whose `kind` label is the limit kind
  lowercased. Seven kinds reach it: `session`, `weekly`, `opus` and `sonnet`
  are the limit message's own word; `fable` is the Fable notice, `credits` the
  `credits_required` error, and `unknown` a limit with no readable kind. The
  wrapper's `marker` kind — a run refused because the farm was already inside
  a window — is not a limit the run hit and is not counted.

## systemd

| Unit | Scope | Notes |
|---|---|---|
| `snowfarm-guard.service` | system | `Type=notify`, `User=snowfarm Group=farm-agents`, `SupplementaryGroups=systemd-journal`, `WatchdogSec=90`, `ProtectSystem=strict` with `ReadWritePaths=` covering `/var/lib/snowfarm /run/snowfarm /srv/snowfarm /var/lib/farm` and any roster path outside them. It carries no seccomp or namespace directive: `sudo` needs setuid, and such a directive makes the kernel drop the setuid bit. On a cold boot it waits up to **two minutes**, retrying every two seconds, for `farm.metrics_addr` to be assigned to an interface — `tailscaled` reports started before the tailnet address is up, and the unit's `StartLimitBurst=5` in `StartLimitIntervalSec=300` would otherwise stop the guard for good. Every other bind error still fails at once |
| `hermes-gateway.service` | user, per manager | `Type=simple`, `NoNewPrivileges=true`, `UMask=0007`, `Restart=on-failure`, and the `DISCORD_*` allow-list the roster fixes |
| `user-<uid>.slice` drop-in | system | the ceiling on everything the agent runs |
| `snowfarm-run-<agent>-<ns>.service` | user, transient | one dispatch or maintenance pass, started by `systemd-run --collect` with `ExitType=cgroup` and the agent's run limits |

A run's exit status comes only from the journal line `Main process exited,
code=exited, status=N`: the transient unit is `--collect`ed, so nothing
remains to ask afterwards. "Still active" is decided by the unit's presence in
`farm-unitctl run-list`, and by nothing else.

## Commands `snowfarm apply` runs, as root

| Command | When |
|---|---|
| `getent group farm-agents` | to decide whether the group exists |
| `groupadd --system farm-agents` | once |
| `useradd --system --uid … --home-dir … --create-home --shell /usr/sbin/nologin --user-group farm-<agent>` | per new agent |
| `usermod -aG farm-agents farm-<agent>`, `loginctl enable-linger farm-<agent>` | every apply, idempotent |
| `chown -h <owner>:<group> <path>` | per directory and file it converges |
| `setfacl -m …`, `setfacl -d -m …` | the ACL table above |
| `lsattr -d <path>` | to read the immutable flag, in `plan` and after `apply` |
| `chattr -i <path>` / `chattr +i <path>` | around every write into the frozen set |
| `systemctl daemon-reload` | when a system unit changed |
| `systemd-tmpfiles --create /etc/tmpfiles.d/snowfarm.conf` | when that file changed |
| `systemctl start user@<uid>.service`, `systemctl is-active --quiet user@<uid>.service` | to bound the wait for a newly lingering user manager |
| `systemctl --user -M farm-<agent>@ daemon-reload` | per agent it touched |
| `runuser -u farm-<agent> -- env HOME=… HERMES_HOME=… /usr/local/bin/hermes cron list` | to see whether the manager's `farm-report` job exists |
| `runuser -u farm-<agent> -- env … /usr/local/bin/hermes cron create …` | when it does not |
| `runuser -u farm-<agent> -- /usr/local/sbin/farm-unitctl gateway start` | `apply --start` only, and never for a unit still carrying `pending-reconcile` |

## Commands the guard runs, as `snowfarm`

Everything that touches an agent goes through one line of sudoers and one
wrapper:

```
sudo -n -u farm-<agent> -- /usr/local/sbin/farm-unitctl <verb> [args]
```

| Verb | Effect |
|---|---|
| `gateway start\|stop\|restart\|is-active\|show` | `systemctl --user … hermes-gateway.service` |
| `run-dispatch` | prints the transient unit name, then `systemd-run --user … hermes kanban dispatch --json --max 1` — a spawning pass |
| `run-maintenance` | the same with `--max 0`: `enforce_max_runtime`, reclaim and todo→ready promotion, spawning nothing. Housekeeping passes must use this verb |
| `run-stop <unit>` | `systemctl --user stop` on a `snowfarm-run-<agent>-*.service` and nothing else |
| `run-list` | `systemctl --user list-units --plain --no-legend --all 'snowfarm-run-<agent>-*.service'` |
| `kanban create\|notify-subscribe\|notify-unsubscribe\|block\|reclaim\|diagnostics\|list\|show\|stats` | `hermes kanban …` as that agent |

The wrapper derives the agent from the caller's own uid, refuses any other
verb with exit 2, exports the whole Hermes environment itself, and sets
`umask 0007`. The guard checks the same allow-lists before it calls, so a
defect shows as an error rather than an unread exit 2.

Beyond that the guard runs only:

| Command or request | Purpose |
|---|---|
| `journalctl --no-pager -o json --output-fields=MESSAGE,PRIORITY [--since @<epoch>] <user-unit matches>` | the exit status and output of one transient run unit |
| `<farm.hermes_bin> version` | the Hermes pin-drift probe |
| `GET <farm.modelgate.api>/models`, with the supervisor's own key | the model-gateway liveness probe |
| `POST https://oauth2.googleapis.com/token` | the weekly refresh-grant probe on a stored Google token |

It reads each manager's `gateway.log` and each agent's `runs/<agent>.jsonl` as
plain files, by offset, and it writes nothing inside an agent's home.

## Network

`snowfarm guard` binds `farm.metrics_addr` and serves `/metrics` there. The
address is a tailnet address; nothing in this repository opens a public port,
and the secrets socket is a Unix socket in the setgid `/run/snowfarm`,
authorised by peer credentials rather than by anything a caller sends.
