// Package store persists vehicles, drive sessions, samples and DTCs in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS vehicles (
	vin        TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	protocol   TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	vin        TEXT NOT NULL REFERENCES vehicles(vin),
	started_at INTEGER NOT NULL,
	ended_at   INTEGER,
	protocol   TEXT NOT NULL DEFAULT '',
	readiness  TEXT NOT NULL DEFAULT '',
	notes      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS sessions_vin ON sessions(vin, started_at);
CREATE TABLE IF NOT EXISTS samples (
	session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	t          INTEGER NOT NULL, -- unix milliseconds
	pid        TEXT NOT NULL,
	value      REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS samples_session ON samples(session_id, t);
CREATE TABLE IF NOT EXISTS dtcs (
	session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	code       TEXT NOT NULL,
	kind       TEXT NOT NULL, -- stored | pending
	read_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS dtcs_session ON dtcs(session_id);
`

// ErrNotFound is returned when a lookup matches nothing.
var ErrNotFound = errors.New("store: not found")

// Store is a carsight database.
type Store struct{ db *sql.DB }

// Vehicle is a car identified by VIN.
type Vehicle struct {
	VIN      string
	Name     string
	Protocol string
}

// Session is one logged drive.
type Session struct {
	ID        int64
	VIN       string
	StartedAt time.Time
	EndedAt   time.Time // zero while logging
	Protocol  string
	Readiness string // JSON-encoded obd.Readiness
	Notes     string
}

// Sample is one PID reading.
type Sample struct {
	T     time.Time
	PID   string
	Value float64
}

// DTC is a trouble code read during a session.
type DTC struct {
	Code   string
	Kind   string
	ReadAt time.Time
}

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// UpsertVehicle records a vehicle, keeping an existing name if v.Name is empty.
func (s *Store) UpsertVehicle(ctx context.Context, v Vehicle) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO vehicles (vin, name, protocol, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(vin) DO UPDATE SET
			name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE vehicles.name END,
			protocol = excluded.protocol`,
		v.VIN, v.Name, v.Protocol, time.Now().UnixMilli())
	return err
}

// Vehicle looks up a vehicle by VIN.
func (s *Store) Vehicle(ctx context.Context, vin string) (Vehicle, error) {
	var v Vehicle
	err := s.db.QueryRowContext(ctx, `SELECT vin, name, protocol FROM vehicles WHERE vin = ?`, vin).
		Scan(&v.VIN, &v.Name, &v.Protocol)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}

// Vehicles lists all vehicles.
func (s *Store) Vehicles(ctx context.Context) ([]Vehicle, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT vin, name, protocol FROM vehicles ORDER BY name, vin`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Vehicle
	for rows.Next() {
		var v Vehicle
		if err := rows.Scan(&v.VIN, &v.Name, &v.Protocol); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// StartSession creates a session and returns its ID.
func (s *Store) StartSession(ctx context.Context, vin, protocol string, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO sessions (vin, started_at, protocol) VALUES (?, ?, ?)`,
		vin, at.UnixMilli(), protocol)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EndSession marks a session finished.
func (s *Store) EndSession(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET ended_at = ? WHERE id = ?`, at.UnixMilli(), id)
	return err
}

// SetReadiness stores the JSON-encoded monitor readiness for a session.
func (s *Store) SetReadiness(ctx context.Context, id int64, readinessJSON string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET readiness = ? WHERE id = ?`, readinessJSON, id)
	return err
}

// SetNotes stores free-form notes for a session (e.g. "cold start, commute").
func (s *Store) SetNotes(ctx context.Context, id int64, notes string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET notes = ? WHERE id = ?`, notes, id)
	return err
}

// AddSamples inserts samples in a single transaction.
func (s *Store) AddSamples(ctx context.Context, id int64, samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO samples (session_id, t, pid, value) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, sm := range samples {
		if _, err := stmt.ExecContext(ctx, id, sm.T.UnixMilli(), sm.PID, sm.Value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddDTCs records trouble codes of the given kind ("stored" or "pending").
func (s *Store) AddDTCs(ctx context.Context, id int64, kind string, codes []string, at time.Time) error {
	for _, c := range codes {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO dtcs (session_id, code, kind, read_at) VALUES (?, ?, ?, ?)`,
			id, c, kind, at.UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

const sessionCols = `id, vin, started_at, ended_at, protocol, readiness, notes`

func scanSession(sc interface{ Scan(...any) error }) (Session, error) {
	var ss Session
	var start int64
	var end sql.NullInt64
	if err := sc.Scan(&ss.ID, &ss.VIN, &start, &end, &ss.Protocol, &ss.Readiness, &ss.Notes); err != nil {
		return ss, err
	}
	ss.StartedAt = time.UnixMilli(start)
	if end.Valid {
		ss.EndedAt = time.UnixMilli(end.Int64)
	}
	return ss, nil
}

// Session looks up a session by ID.
func (s *Store) Session(ctx context.Context, id int64) (Session, error) {
	ss, err := scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ss, ErrNotFound
	}
	return ss, err
}

// Sessions lists sessions, newest first. An empty vin matches all vehicles;
// a zero since/until is unbounded.
func (s *Store) Sessions(ctx context.Context, vin string, since, until time.Time) ([]Session, error) {
	q := `SELECT ` + sessionCols + ` FROM sessions WHERE 1=1`
	var args []any
	if vin != "" {
		q += ` AND vin = ?`
		args = append(args, vin)
	}
	if !since.IsZero() {
		q += ` AND started_at >= ?`
		args = append(args, since.UnixMilli())
	}
	if !until.IsZero() {
		q += ` AND started_at < ?`
		args = append(args, until.UnixMilli())
	}
	q += ` ORDER BY started_at DESC, id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		ss, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

// Samples returns a session's samples in time order.
func (s *Store) Samples(ctx context.Context, id int64) ([]Sample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t, pid, value FROM samples WHERE session_id = ? ORDER BY t`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var sm Sample
		var t int64
		if err := rows.Scan(&t, &sm.PID, &sm.Value); err != nil {
			return nil, err
		}
		sm.T = time.UnixMilli(t)
		out = append(out, sm)
	}
	return out, rows.Err()
}

// DTCs returns the trouble codes recorded for a session.
func (s *Store) DTCs(ctx context.Context, id int64) ([]DTC, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT code, kind, read_at FROM dtcs WHERE session_id = ? ORDER BY kind DESC, code`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DTC
	for rows.Next() {
		var d DTC
		var t int64
		if err := rows.Scan(&d.Code, &d.Kind, &t); err != nil {
			return nil, err
		}
		d.ReadAt = time.UnixMilli(t)
		out = append(out, d)
	}
	return out, rows.Err()
}
