package dispatch

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// PassKind separates the two passes the guard issues. Only a spawn pass can
// claim a card, so only a spawn pass reserves capacity: a maintenance pass is
// issued for a worker that already holds a running card, which the running
// set counts already.
type PassKind string

const (
	PassSpawn       PassKind = "spawn"
	PassMaintenance PassKind = "maintenance"
)

type Pass struct {
	Agent string
	Unit  string
	Kind  PassKind
	At    time.Time
}

// Ledger is the guard's own record of what it asked the farm to do: the
// passes it issued, the cards those passes spawned, the runs it stopped, the
// file offsets its tailers reached and the advisories it has already posted.
// One connection carries every write, so the single-writer property the
// guard's goroutines assume is enforced rather than hoped for.
type Ledger struct{ db *sql.DB }

const ledgerSchema = `
CREATE TABLE IF NOT EXISTS passes (
	unit        TEXT PRIMARY KEY,
	agent       TEXT NOT NULL,
	kind        TEXT NOT NULL,
	issued_at   INTEGER NOT NULL,
	resolved_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_passes_agent ON passes(agent, issued_at);
CREATE TABLE IF NOT EXISTS spawns (
	task_id TEXT PRIMARY KEY,
	agent   TEXT NOT NULL,
	unit    TEXT NOT NULL,
	at      INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS stops (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT NOT NULL,
	unit    TEXT NOT NULL,
	reason  TEXT NOT NULL,
	at      INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS offsets (
	key    TEXT PRIMARY KEY,
	offset INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS notices (
	kind TEXT NOT NULL,
	key  TEXT NOT NULL,
	at   INTEGER NOT NULL,
	PRIMARY KEY (kind, key)
);
`

func OpenLedger(path string) (*Ledger, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open ledger %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(ledgerSchema); err != nil {
		return nil, errors.Join(fmt.Errorf("open ledger %s: %w", path, err), db.Close())
	}
	return &Ledger{db: db}, nil
}

func (l *Ledger) Close() error { return l.db.Close() }

func (l *Ledger) RecordPass(agent, unit string, kind PassKind, at time.Time) error {
	_, err := l.db.Exec(
		`INSERT INTO passes (unit, agent, kind, issued_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(unit) DO UPDATE SET agent = excluded.agent, kind = excluded.kind,
		 issued_at = excluded.issued_at, resolved_at = NULL`,
		unit, agent, string(kind), stamp(at))
	if err != nil {
		return fmt.Errorf("record %s pass for %s: %w", kind, agent, err)
	}
	return nil
}

// PendingPasses returns the agent's passes whose outcome the guard has not
// read yet, oldest first. The caller keeps only PassSpawn rows when it counts
// reserved capacity.
func (l *Ledger) PendingPasses(agent string) ([]Pass, error) {
	rows, err := l.db.Query(
		`SELECT unit, agent, kind, issued_at FROM passes
		 WHERE agent = ? AND resolved_at IS NULL ORDER BY issued_at, unit`, agent)
	if err != nil {
		return nil, fmt.Errorf("pending passes for %s: %w", agent, err)
	}
	defer func() { _ = rows.Close() }()
	var passes []Pass
	for rows.Next() {
		pass, err := scanPass(rows)
		if err != nil {
			return nil, fmt.Errorf("pending passes for %s: %w", agent, err)
		}
		passes = append(passes, pass)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pending passes for %s: %w", agent, err)
	}
	return passes, nil
}

// LastPass is the newest pass the agent was given, resolved or not, so the
// loop can leave a worker alone for one dispatch interval after its last one
// and give the next pass to whoever has waited longest.
func (l *Ledger) LastPass(agent string) (Pass, bool, error) {
	row := l.db.QueryRow(
		`SELECT unit, agent, kind, issued_at FROM passes
		 WHERE agent = ? ORDER BY issued_at DESC, unit DESC LIMIT 1`, agent)
	pass, err := scanPass(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Pass{}, false, nil
	case err != nil:
		return Pass{}, false, fmt.Errorf("last pass for %s: %w", agent, err)
	}
	return pass, true, nil
}

func (l *Ledger) ResolvePass(unit string, at time.Time) error {
	if _, err := l.db.Exec(`UPDATE passes SET resolved_at = ? WHERE unit = ?`, stamp(at), unit); err != nil {
		return fmt.Errorf("resolve pass %s: %w", unit, err)
	}
	return nil
}

func (l *Ledger) RecordSpawn(agent, unit, taskID string, at time.Time) error {
	_, err := l.db.Exec(
		`INSERT INTO spawns (task_id, agent, unit, at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(task_id) DO UPDATE SET agent = excluded.agent, unit = excluded.unit,
		 at = excluded.at`,
		taskID, agent, unit, stamp(at))
	if err != nil {
		return fmt.Errorf("record spawn of %s by %s: %w", taskID, agent, err)
	}
	return nil
}

// UnitForTask names the transient unit whose pass spawned the card's worker,
// which is the unit the guard stops and reads an exit status from. A card the
// ledger never saw — a run from before the guard restarted — reports false,
// and so does a read that fails: either way the caller falls back to the
// worker's cgroup.
func (l *Ledger) UnitForTask(taskID string) (string, bool) {
	var unit string
	if err := l.db.QueryRow(`SELECT unit FROM spawns WHERE task_id = ?`, taskID).Scan(&unit); err != nil {
		return "", false
	}
	return unit, true
}

func (l *Ledger) RecordStop(taskID, unit, reason string, at time.Time) error {
	_, err := l.db.Exec(
		`INSERT INTO stops (task_id, unit, reason, at) VALUES (?, ?, ?, ?)`,
		taskID, unit, reason, stamp(at))
	if err != nil {
		return fmt.Errorf("record stop of %s: %w", taskID, err)
	}
	return nil
}

// Stopped reports whether the guard has already terminated this run. A card
// the guard has stopped stays in the board's running set until the reclaim
// lands, so without this a failed reclaim would re-stop, re-block and re-post
// the same run on every tick.
func (l *Ledger) Stopped(taskID, unit string) (bool, error) {
	var stops int
	err := l.db.QueryRow(
		`SELECT COUNT(*) FROM stops WHERE task_id = ? AND unit = ?`, taskID, unit).Scan(&stops)
	if err != nil {
		return false, fmt.Errorf("read stops of %s: %w", taskID, err)
	}
	return stops > 0, nil
}

func (l *Ledger) Offset(key string) (int64, error) {
	var offset int64
	err := l.db.QueryRow(`SELECT offset FROM offsets WHERE key = ?`, key).Scan(&offset)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("read offset %s: %w", key, err)
	}
	return offset, nil
}

func (l *Ledger) SetOffset(key string, off int64) error {
	_, err := l.db.Exec(
		`INSERT INTO offsets (key, offset) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET offset = excluded.offset`, key, off)
	if err != nil {
		return fmt.Errorf("write offset %s: %w", key, err)
	}
	return nil
}

// NoticeOnce reports whether this is the first time the guard has had this to
// say about this key, so an advisory about a card that stays as it is does not
// post on every tick.
func (l *Ledger) NoticeOnce(kind, key string, at time.Time) (bool, error) {
	result, err := l.db.Exec(
		`INSERT INTO notices (kind, key, at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
		kind, key, stamp(at))
	if err != nil {
		return false, fmt.Errorf("record notice %s/%s: %w", kind, key, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record notice %s/%s: %w", kind, key, err)
	}
	return affected > 0, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanPass(row scanner) (Pass, error) {
	var (
		pass  Pass
		kind  string
		issue int64
	)
	if err := row.Scan(&pass.Unit, &pass.Agent, &kind, &issue); err != nil {
		return Pass{}, err
	}
	pass.Kind = PassKind(kind)
	pass.At = time.Unix(0, issue).UTC()
	return pass, nil
}

func stamp(at time.Time) int64 { return at.UTC().UnixNano() }
