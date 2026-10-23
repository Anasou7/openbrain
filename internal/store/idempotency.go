package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Keys are kept long enough to cover any realistic retry.
const idempotencyTTL = 7 * 24 * time.Hour

// replayed returns the ID of the record an earlier call with the same
// idempotency key created, or 0 if the key is empty or new. It runs inside
// the write transaction, and writes are serialised, so the check and the
// later remember cannot race.
func replayed(ctx context.Context, tx *sql.Tx, actor int64, key, op string) (int64, error) {
	if key == "" {
		return 0, nil
	}
	var prevOp string
	var subject int64
	err := tx.QueryRowContext(ctx, `SELECT operation, subject_id FROM idempotency_keys WHERE actor_id = ? AND key = ?`, actor, key).
		Scan(&prevOp, &subject)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if prevOp != op {
		return 0, &ValidationError{"idempotency_key", fmt.Sprintf("was already used for a different call (%s)", prevOp)}
	}
	return subject, nil
}

// remember records the record a keyed call created, and prunes expired keys.
func remember(ctx context.Context, tx *sql.Tx, actor int64, key, op string, subject int64, now Time) error {
	if key == "" {
		return nil
	}
	// ponytail: full scan of a small table on each keyed write; index
	// created_at if keyed writes ever reach thousands per day.
	if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE created_at < ?`, Time{now.Add(-idempotencyTTL)}); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys (actor_id, key, operation, subject_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		actor, key, op, subject, now)
	return err
}

func opFor(op string, projectID int64) string { return fmt.Sprintf("%s:%d", op, projectID) }
