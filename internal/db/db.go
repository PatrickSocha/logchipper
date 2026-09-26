package db

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type DB struct {
	conn *sql.DB
}

type Event struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Source    string    `json:"source"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	Meta      string    `json:"meta,omitempty"`
}

func Open(path string) (*DB, error) {
	// _time_format=sqlite stores times as "2006-01-02 15:04:05.999999999-07:00",
	// which SQLite's date functions understand.
	conn, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_time_format=sqlite")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	conn.SetMaxOpenConns(1)

	d := &DB{conn: conn}
	if err := d.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return d, nil
}

// migrate applies embedded migrations/*.sql in filename order, once each,
// recording them in schema_migrations.
func (d *DB) migrate() error {
	if _, err := d.conn.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		name       TEXT PRIMARY KEY,
		applied_at DATETIME NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return err
	}

	files, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)

	for _, f := range files {
		name := path.Base(f)
		var n int
		if err := d.conn.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := migrationFiles.ReadFile(f)
		if err != nil {
			return err
		}
		tx, err := d.conn.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (name) VALUES (?)`, name); err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) Insert(e Event) (int64, error) {
	res, err := d.conn.Exec(
		`INSERT INTO events (created_at, source, level, message, meta) VALUES (?, ?, ?, ?, ?)`,
		e.CreatedAt.UTC(), e.Source, e.Level, e.Message, e.Meta,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// created_at is stored as text and compared as a string, so every time
// written or compared must be in the same zone: UTC.

type QueryParams struct {
	Source   string
	Level    string
	Search   string
	Since    time.Time
	Until    time.Time // exclusive
	Limit    int
	AfterID  int64
	BeforeID int64
	Asc      bool // oldest first; used to page forward from AfterID or Since
}

func (d *DB) Query(p QueryParams) ([]Event, error) {
	q := `SELECT id, created_at, source, level, message, meta FROM events WHERE 1=1`
	args := []any{}

	if p.Source != "" {
		q += ` AND source = ?`
		args = append(args, p.Source)
	}
	if p.Level != "" {
		q += ` AND level = ?`
		args = append(args, p.Level)
	}
	if p.Search != "" {
		q += ` AND (message LIKE ? OR source LIKE ? OR meta LIKE ?)`
		s := "%" + p.Search + "%"
		args = append(args, s, s, s)
	}
	if !p.Since.IsZero() {
		q += ` AND created_at >= ?`
		args = append(args, p.Since.UTC())
	}
	if !p.Until.IsZero() {
		q += ` AND created_at < ?`
		args = append(args, p.Until.UTC())
	}
	if p.AfterID > 0 {
		q += ` AND id > ?`
		args = append(args, p.AfterID)
	}

	if p.BeforeID > 0 {
		q += ` AND id < ?`
		args = append(args, p.BeforeID)
	}

	if p.Asc {
		q += ` ORDER BY id ASC`
	} else {
		q += ` ORDER BY id DESC`
	}
	if p.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, p.Limit)
	}

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.Source, &e.Level, &e.Message, &e.Meta); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

type Bucket struct {
	T     int64 `json:"t"` // bucket start, unix seconds
	Count int64 `json:"count"`
}

type HistogramParams struct {
	Source        string
	Level         string
	Search        string
	Since         time.Time
	BucketSeconds int
}

// Histogram returns event counts grouped into fixed-size time buckets. The
// GROUP BY runs inside SQLite, so memory use stays O(bucket count) — a few
// hundred rows at most — no matter how many raw events fall in the range.
func (d *DB) Histogram(p HistogramParams) ([]Bucket, error) {
	q := `SELECT (CAST(strftime('%s', created_at) AS INTEGER) / ?) * ? AS bucket, COUNT(*) AS cnt
	      FROM events WHERE created_at >= ?`
	args := []any{p.BucketSeconds, p.BucketSeconds, p.Since.UTC()}

	if p.Source != "" {
		q += ` AND source = ?`
		args = append(args, p.Source)
	}
	if p.Level != "" {
		q += ` AND level = ?`
		args = append(args, p.Level)
	}
	if p.Search != "" {
		q += ` AND (message LIKE ? OR source LIKE ? OR meta LIKE ?)`
		s := "%" + p.Search + "%"
		args = append(args, s, s, s)
	}
	q += ` GROUP BY bucket ORDER BY bucket`

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.T, &b.Count); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (d *DB) Sources() ([]string, error) {
	rows, err := d.conn.Query(`SELECT DISTINCT source FROM events ORDER BY source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *DB) Purge(olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	res, err := d.conn.Exec(`DELETE FROM events WHERE created_at < ?`, cutoff.UTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (d *DB) Close() error {
	return d.conn.Close()
}

// Count returns the total number of stored events.
func (d *DB) Count() (int64, error) {
	var n int64
	err := d.conn.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
	return n, err
}

// Size returns the logical database size in bytes (pages in use × page size).
func (d *DB) Size() (int64, error) {
	var pages, pageSize, free int64
	if err := d.conn.QueryRow(`SELECT page_count, page_size, freelist_count FROM pragma_page_count(), pragma_page_size(), pragma_freelist_count()`).Scan(&pages, &pageSize, &free); err != nil {
		return 0, err
	}
	return (pages - free) * pageSize, nil
}
