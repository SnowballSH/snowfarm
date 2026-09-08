-- Hermes Kanban schema fixture for internal/board.
--
-- pinned commit: NOT YET PINNED. The farm's Hermes commit (>= f6234d0) is
-- chosen at F0 and recorded in hermes-farm/pins.yaml; no checkout of it exists
-- in this repository, so the statements below are transcribed from the design's
-- documented column set rather than copied from kanban_db.SCHEMA_SQL. Replace
-- this file, and this header, with the verbatim schema of the pin before the
-- reader is trusted against the live board (see README.md).

PRAGMA journal_mode=WAL;

CREATE TABLE IF NOT EXISTS tasks (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL DEFAULT '',
    body TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'triage' CHECK (status IN (
        'triage', 'todo', 'scheduled', 'ready', 'running',
        'blocked', 'review', 'done', 'archived')),
    assignee TEXT,
    created_by TEXT,
    priority INTEGER NOT NULL DEFAULT 0,
    model TEXT,
    provider TEXT,
    reasoning TEXT,
    toolsets TEXT,
    max_runtime INTEGER,
    workspace_kind TEXT,
    workspace TEXT,
    parent_id TEXT REFERENCES tasks(id),
    claim_lock TEXT,
    claim_expires_at TEXT,
    worker_pid INTEGER,
    run_id TEXT,
    started_at TEXT,
    heartbeat_at TEXT,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    block_kind TEXT,
    block_reason TEXT,
    idempotency_key TEXT UNIQUE,
    created_at TEXT NOT NULL,
    updated_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_assignee ON tasks(assignee);

CREATE TABLE IF NOT EXISTS task_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL UNIQUE,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    attempt INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL,
    worker_pid INTEGER,
    started_at TEXT,
    finished_at TEXT,
    exit_code INTEGER,
    log_path TEXT
);

CREATE INDEX IF NOT EXISTS idx_task_runs_task ON task_runs(task_id, id);

CREATE TABLE IF NOT EXISTS task_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    kind TEXT NOT NULL,
    actor TEXT,
    payload TEXT,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_task_events_task ON task_events(task_id, id);

CREATE TABLE IF NOT EXISTS kanban_notify_subs (
    task_id TEXT NOT NULL REFERENCES tasks(id),
    platform TEXT NOT NULL,
    chat_id TEXT NOT NULL,
    thread_id TEXT NOT NULL DEFAULT '',
    user_id TEXT,
    user_id_alt TEXT,
    chat_type TEXT,
    notifier_profile TEXT,
    delivery_mode TEXT NOT NULL DEFAULT 'notify',
    delivery_metadata TEXT,
    created_at TEXT NOT NULL,
    last_event_id INTEGER,
    PRIMARY KEY (task_id, platform, chat_id, thread_id)
);
