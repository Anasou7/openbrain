package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// Backup writes a transactionally consistent, compacted copy of the database
// to dst (which must not exist) using VACUUM INTO, then verifies the copy.
// It is safe to run while the server is handling requests.
func (s *Store) Backup(ctx context.Context, dst string) error {
	if strings.ContainsAny(dst, "?#") {
		return fmt.Errorf("invalid backup path %q", dst)
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("backup target %s already exists", dst)
	}
	if _, err := s.w.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		os.Remove(dst)
		return fmt.Errorf("backup to %s: %w", dst, err)
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		return err
	}
	_, err := Verify(ctx, dst)
	return err
}

// Verify checks that path is an intact openbrain database and returns its
// schema version.
func Verify(ctx context.Context, path string) (int, error) {
	if strings.ContainsAny(path, "?#") {
		return 0, fmt.Errorf("invalid path %q", path)
	}
	if _, err := os.Stat(path); err != nil {
		return 0, err // never let sql.Open create an empty file
	}
	db, err := sql.Open("sqlite", path+"?_pragma=query_only(1)")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	if result != "ok" {
		return 0, fmt.Errorf("%s failed integrity check: %s", path, result)
	}
	var version, tables int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return 0, err
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'projects'`).Scan(&tables); err != nil {
		return 0, err
	}
	if version == 0 || tables == 0 {
		return 0, fmt.Errorf("%s is not a openbrain database", path)
	}
	return version, nil
}

// Restore replaces the database contents with the backup at src. The current
// contents are first saved next to the database file (returned path, empty if
// there was nothing to save). The copy uses SQLite's online backup API, so
// other connections see either the old or the new database, never a mix. Run
// Migrate afterwards in case the backup predates the current schema.
func (s *Store) Restore(ctx context.Context, src string) (saved string, err error) {
	version, err := Verify(ctx, src)
	if err != nil {
		return "", err
	}
	if version > SchemaVersion() {
		return "", fmt.Errorf("backup has schema version %d but this binary supports up to %d; upgrade openbrain first", version, SchemaVersion())
	}
	var current int
	if err := s.w.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&current); err != nil {
		return "", err
	}
	if current > 0 {
		saved = s.path + ".pre-restore-" + time.Now().UTC().Format("20060102T150405Z")
		if err := s.Backup(ctx, saved); err != nil {
			return "", fmt.Errorf("saving current database before restore: %w", err)
		}
	}

	conn, err := s.w.Conn(ctx)
	if err != nil {
		return saved, err
	}
	defer conn.Close()
	err = conn.Raw(func(dc any) error {
		r, ok := dc.(interface {
			NewRestore(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("sqlite driver does not support online restore")
		}
		b, err := r.NewRestore(src)
		if err != nil {
			return err
		}
		if _, err := b.Step(-1); err != nil {
			b.Finish()
			return err
		}
		return b.Finish()
	})
	if err != nil {
		return saved, fmt.Errorf("restore from %s: %w", src, err)
	}
	return saved, nil
}
