# Board schema fixture

`schema.sql` is Hermes' `kanban_db.SCHEMA_SQL` at the pinned commit, so
`internal/board` builds a real SQLite board in a temp directory and queries it
through the same statements it will issue against `/srv/snowfarm/kanban`.

## What is committed

Everything below the header comment is `SCHEMA_SQL` of Hermes `f6234d0`
verbatim — the commit the design names as the farm's floor (the CVE-2026-71963
fix, merged 2026-09-02). The header records the short commit and the SHA-256 of
the body so a drifted copy is visible:

```
shasum -a 256 <(tail -n +14 schema.sql)
093423a638606d3a96405c6b083d0cf2d06546f93cc89821d0765a087c6aa319
```

`SCHEMA_SQL` is what `init_db` executes on a fresh board. `init_db` then runs
additive `ALTER TABLE` migrations for legacy boards and four indexes that
cannot live in `SCHEMA_SQL` (`idx_tasks_tenant`, `idx_tasks_idempotency`,
`idx_tasks_session_id`, `idx_events_run`). Every column the reader names is in
this file, so the fixture is the whole surface the package queries.

## Refreshing it from the pin

In a checkout of the pinned Hermes commit (`hermes-farm/pins.yaml`,
`hermes.commit`):

```
python3 -c 'from hermes_cli import kanban_db; print(kanban_db.SCHEMA_SQL)' > body.sql
```

Put the body under the header comment, update the commit and the body hash in
that header, and run `go test ./internal/board/...`. Every query the reader
issues is exercised by that package's tests, so a column the pin renames fails
there rather than on the live board.

## What the pin fixes about storage

- Timestamps (`tasks.created_at`, `tasks.started_at`, `tasks.claim_expires`,
  `task_events.created_at`, `kanban_notify_subs.created_at`) are INTEGER epoch
  seconds — `_append_event` writes `int(time.time())`. The tests insert them
  that way.
- `tasks.max_runtime_seconds` is an INTEGER count of seconds and is NULL unless
  the card was created with `--max-runtime`.
- `tasks.current_run_id` is the INTEGER `task_runs.id` of the in-flight attempt
  (NULL when none), which is what `RunningCard.RunID` carries, rendered as its
  decimal string.
- `tasks.started_at` is the first time the task ever started, not the current
  attempt's start; `Running` takes `COALESCE(task_runs.started_at,
  tasks.started_at)` for the active run, the same expression Hermes'
  `enforce_max_runtime` uses.
- `kanban_notify_subs` carries no subscriber identity (S16): `user_id` is the
  Discord user, `notifier_profile` the profile that will deliver.

The reader's tolerant scalar decoding (`stampOf`, `durationOf`) is kept even
though the pin's affinities are now known: `_REBUILD_SPECS` in `kanban_db.py`
exists precisely because column types on older boards drift, and a scan failure
in `Running` would take out the dispatcher tick.
