# Board schema fixture

`schema.sql` stands in for Hermes' `kanban_db.SCHEMA_SQL` so `internal/board`
can build a real SQLite board in a temp directory and query it.

## Refreshing it from the pin

In a checkout of the pinned Hermes commit (`hermes-farm/pins.yaml`, `hermes.commit`):

```
python3 -c 'from hermes_cli import kanban_db; print(kanban_db.SCHEMA_SQL)' > schema.sql
```

Put the 40-hex commit in the header comment, then run `go test ./internal/board/...`.
Every query the reader issues is exercised by that package's tests, so a column
the pin names differently fails there rather than on the live board.

## State of the current file

The pin is chosen at F0 and no checkout of it exists in this repository yet, so
the committed `schema.sql` is **not** verbatim `SCHEMA_SQL`. It is transcribed
from the design's documented column set:

- `tasks` — the nine statuses, `assignee`, `created_by`, `created_at`,
  `priority`, model/provider/reasoning overrides, `max_runtime` (seconds),
  workspace kind, `parent_id`, `claim_lock`, `claim_expires_at`, `worker_pid`,
  `run_id`, `started_at`, `heartbeat_at`, `consecutive_failures`,
  `idempotency_key`.
- `task_runs` — one row per attempt, with its exit code.
- `task_events` — one row per transition; the terminal kinds are `completed`,
  `blocked`, `gave_up`, `crashed` and `timed_out`.
- `kanban_notify_subs` — the twelve columns of the pinned table, keyed
  `(task_id, platform, chat_id, thread_id)`. It carries no subscriber identity.

Tables the reader never touches (comments, attachments, labels, schedules) are
omitted rather than guessed at.

Column names and the storage shape of the timestamp and duration columns are
therefore unverified against the pin. The reader is written to survive the
likely variations — timestamps are parsed as RFC 3339, as SQLite's
`YYYY-MM-DD HH:MM:SS` text, or as an epoch number — but a renamed column is a
schema mismatch that only refreshing this file will surface.
