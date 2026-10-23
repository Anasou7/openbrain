// Package store owns the SQLite database: migrations, validation, and every
// read and write the application performs. The HTTP layer (and, later, an MCP
// layer) calls these semantic operations; nothing else touches SQL.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

// ValidationError reports a rejected input field.
type ValidationError struct{ Field, Problem string }

func (e *ValidationError) Error() string { return e.Field + " " + e.Problem }

// Store is safe for concurrent use. All writes share one connection: SQLite
// allows one writer at a time, and serialising writers in-process makes them
// queue for the connection instead of failing with SQLITE_BUSY. Reads use a
// small pool and run concurrently with writes thanks to WAL mode.
type Store struct {
	path string
	w, r *sql.DB

	// StaleAfter is how long a busy actor state (working, waiting, blocked)
	// may go without a report before reads flag it as stale.
	StaleAfter time.Duration
}

// Open opens the database at path, creating the file if needed. Call Migrate
// before using it.
func Open(path string) (*Store, error) {
	if path == "" || strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("invalid database path %q", path)
	}
	w, err := sql.Open("sqlite", path+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if err := w.Ping(); err != nil {
		w.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	r, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=query_only(1)")
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	return &Store{path: path, w: w, r: r, StaleAfter: time.Hour}, nil
}

func (s *Store) Close() error { return errors.Join(s.r.Close(), s.w.Close()) }

// SchemaVersion is the schema version this binary expects: the number of
// embedded migrations.
func SchemaVersion() int {
	names, _ := fs.Glob(migrationFS, "migrations/*.sql")
	return len(names)
}

// Migrate applies pending migrations, each in its own transaction together
// with the PRAGMA user_version bump that records it.
func (s *Store) Migrate(ctx context.Context) (from, to int, err error) {
	names, err := fs.Glob(migrationFS, "migrations/*.sql") // sorted
	if err != nil {
		return 0, 0, err
	}
	if err := s.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&from); err != nil {
		return 0, 0, err
	}
	if from > len(names) {
		return from, from, fmt.Errorf("database schema version %d is newer than this binary supports (%d); upgrade openbrain", from, len(names))
	}
	for i := from; i < len(names); i++ {
		if want := fmt.Sprintf("migrations/%04d_", i+1); !strings.HasPrefix(names[i], want) {
			return from, i, fmt.Errorf("migration %s is out of sequence (expected %s*)", names[i], want)
		}
		body, err := migrationFS.ReadFile(names[i])
		if err != nil {
			return from, i, err
		}
		err = s.write(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return fmt.Errorf("%s: %w", names[i], err)
			}
			_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1))
			return err
		})
		if err != nil {
			return from, i, err
		}
	}
	return from, len(names), nil
}

// Ready reports whether the database answers queries and its schema matches
// this binary.
func (s *Store) Ready(ctx context.Context) error {
	var v int
	if err := s.r.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v != SchemaVersion() {
		return fmt.Errorf("schema version is %d, want %d", v, SchemaVersion())
	}
	return nil
}

// write runs fn in an immediate write transaction.
func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// read runs fn in a read transaction, giving it a consistent snapshot.
func (s *Store) read(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.r.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(tx)
}

// Time is a UTC timestamp stored as fixed-width ISO-8601 text, which sorts
// correctly as a string. The zero Time is stored as NULL.
type Time struct{ time.Time }

const timeLayout = "2006-01-02T15:04:05.000Z"

// Now returns the current time at the precision the database stores.
func Now() Time { return Time{time.Now().UTC().Truncate(time.Millisecond)} }

func (t Time) Value() (driver.Value, error) {
	if t.IsZero() {
		return nil, nil
	}
	return t.UTC().Format(timeLayout), nil
}

func (t *Time) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*t = Time{}
		return nil
	case string:
		p, err := time.Parse(timeLayout, v)
		*t = Time{p}
		return err
	}
	return fmt.Errorf("cannot scan %T into store.Time", src)
}

// querier is satisfied by *sql.DB and *sql.Tx, so read helpers work both
// standalone and inside a snapshot.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type scanner interface{ Scan(dest ...any) error }

func collect[T any](rows *sql.Rows, err error, scan func(scanner) (T, error)) ([]T, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// limit applies a default and an upper bound to a caller-supplied page size.
func limit(n, def int) int {
	if n <= 0 {
		return def
	}
	return min(n, 500)
}

func isUnique(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
