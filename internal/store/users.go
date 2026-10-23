package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// UserRoles are the allowed values of User.Role. Admins manage users, actors
// and tokens; users have normal dashboard access.
var UserRoles = []string{"admin", "user"}

var (
	// ErrSetupDone is returned when first-run setup is attempted after an
	// account already exists.
	ErrSetupDone = errors.New("setup has already been completed")
	// ErrInvalidLogin is the only sign-in failure callers should show: it does
	// not reveal whether the username exists or the account is disabled.
	ErrInvalidLogin = errors.New("invalid username or password")
)

// User is a person who can sign in. Their writes are attributed to Actor.
type User struct {
	ID          int64    `json:"id"`
	Username    string   `json:"username"`
	Actor       ActorRef `json:"actor"`
	Role        string   `json:"role"`
	CreatedAt   Time     `json:"created_at"`
	LastLoginAt Time     `json:"last_login_at,omitzero"`
	DisabledAt  Time     `json:"disabled_at,omitzero"`
}

func (u User) IsAdmin() bool { return u.Role == "admin" }

type UserInput struct {
	Username    string
	DisplayName string // default: the username
	Password    string
	Role        string // default: "user"; the first account is always "admin"
}

const userSelect = `SELECT u.id, u.username, a.slug, a.display_name, a.kind, u.role, u.created_at, u.last_login_at, u.disabled_at
FROM users u JOIN actors a ON a.id = u.actor_id`

func scanUser(sc scanner) (u User, err error) {
	err = sc.Scan(&u.ID, &u.Username, &u.Actor.Slug, &u.Actor.DisplayName, &u.Actor.Kind, &u.Role, &u.CreatedAt, &u.LastLoginAt, &u.DisabledAt)
	return u, err
}

// UserCount returns the number of accounts; zero means first-run setup is due.
func (s *Store) UserCount(ctx context.Context) (int, error) {
	var n int
	err := s.r.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.r.QueryContext(ctx, userSelect+` ORDER BY u.disabled_at IS NOT NULL, u.username`)
	return collect(rows, err, scanUser)
}

func (s *Store) User(ctx context.Context, id int64) (User, error) {
	u, err := scanUser(s.r.QueryRowContext(ctx, userSelect+` WHERE u.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return u, fmt.Errorf("user %d %w", id, ErrNotFound)
	}
	return u, err
}

func (s *Store) UserByName(ctx context.Context, username string) (User, error) {
	u, err := scanUser(s.r.QueryRowContext(ctx, userSelect+` WHERE u.username = ?`, strings.TrimSpace(username)))
	if errors.Is(err, sql.ErrNoRows) {
		return u, fmt.Errorf("user %q %w", username, ErrNotFound)
	}
	return u, err
}

// CreateFirstUser creates the first account, always as an administrator, and
// fails with ErrSetupDone if any account exists. The check and the insert run
// in one immediate transaction, so concurrent setups cannot both succeed.
func (s *Store) CreateFirstUser(ctx context.Context, in UserInput) (User, error) {
	return s.createUser(ctx, in, true)
}

// CreateUser adds an account. If no account exists yet it becomes an admin,
// so the CLI can bootstrap a headless install.
func (s *Store) CreateUser(ctx context.Context, in UserInput) (User, error) {
	return s.createUser(ctx, in, false)
}

func (s *Store) createUser(ctx context.Context, in UserInput, firstOnly bool) (User, error) {
	in.Username = strings.ToLower(strings.TrimSpace(in.Username))
	if strings.TrimSpace(in.DisplayName) == "" {
		in.DisplayName = in.Username
	}
	if in.Role == "" {
		in.Role = "user"
	}
	var v validator
	v.slug("username", in.Username)
	v.text("display_name", &in.DisplayName, 200, true)
	v.oneOf("role", in.Role, UserRoles)
	v.password("password", in.Password, in.Username)
	if v.err != nil {
		return User{}, v.err
	}
	hash := HashPassword(in.Password) // slow; done before taking the write lock

	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
			return err
		}
		if firstOnly && count > 0 {
			return ErrSetupDone
		}
		if count == 0 {
			in.Role = "admin"
		}
		actor, err := humanActorFor(ctx, tx, in.Username, in.DisplayName, count == 0)
		if err != nil {
			return err
		}
		now := Now()
		res, err := tx.ExecContext(ctx, `INSERT INTO users (actor_id, username, password_hash, role, created_at, updated_at, password_changed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, actor, in.Username, hash, in.Role, now, now, now)
		if isUnique(err) {
			return &ValidationError{"username", "is already taken"}
		}
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return User{}, err
	}
	return s.User(ctx, id)
}

// humanActorFor finds or creates the actor a new account writes as:
//   - an unclaimed human actor whose slug is the username is claimed;
//   - otherwise the first account claims the human actor "user" that the first
//     migration seeds (renamed to the username), so upgraded installs keep
//     their history under the new account;
//   - otherwise a new human actor is created.
func humanActorFor(ctx context.Context, tx *sql.Tx, username, displayName string, first bool) (int64, error) {
	var id int64
	var kind string
	var claimed bool
	err := tx.QueryRowContext(ctx, `SELECT a.id, a.kind, EXISTS (SELECT 1 FROM users u WHERE u.actor_id = a.id) FROM actors a WHERE a.slug = ?`, username).
		Scan(&id, &kind, &claimed)
	switch {
	case err == nil && kind == "human" && !claimed:
		_, err := tx.ExecContext(ctx, `UPDATE actors SET display_name = ? WHERE id = ?`, displayName, id)
		return id, err
	case err == nil:
		return 0, &ValidationError{"username", fmt.Sprintf("is already used by the %s actor %q; choose another", kind, username)}
	case !errors.Is(err, sql.ErrNoRows):
		return 0, err
	}

	if first {
		err := tx.QueryRowContext(ctx, `SELECT id FROM actors WHERE slug = 'user' AND kind = 'human'
			AND NOT EXISTS (SELECT 1 FROM users u WHERE u.actor_id = actors.id)`).Scan(&id)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE actors SET slug = ?, display_name = ? WHERE id = ?`, username, displayName, id)
			return id, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}

	res, err := tx.ExecContext(ctx, `INSERT INTO actors (slug, display_name, kind, created_at) VALUES (?, ?, 'human', ?)`, username, displayName, Now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// VerifyLogin checks a username and password. Every failure, including a
// disabled account, is ErrInvalidLogin, and takes about the same time.
func (s *Store) VerifyLogin(ctx context.Context, username, password string) (User, error) {
	username = strings.TrimSpace(username)
	var hash string
	var u User
	err := s.r.QueryRowContext(ctx, `SELECT u.id, u.password_hash, u.disabled_at FROM users u WHERE u.username = ?`, username).
		Scan(&u.ID, &hash, &u.DisabledAt)
	if errors.Is(err, sql.ErrNoRows) || username == "" || password == "" || len(password) > maxPassword {
		checkPassword(dummyHash(), password)
		return User{}, ErrInvalidLogin
	}
	if err != nil {
		return User{}, err
	}
	ok, rehash, err := checkPassword(hash, password)
	if err != nil {
		return User{}, err
	}
	if !ok || !u.DisabledAt.IsZero() {
		return User{}, ErrInvalidLogin
	}
	now := Now()
	if rehash {
		_, err = s.w.ExecContext(ctx, `UPDATE users SET password_hash = ?, last_login_at = ? WHERE id = ?`, HashPassword(password), now, u.ID)
	} else {
		_, err = s.w.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, now, u.ID)
	}
	if err != nil {
		return User{}, err
	}
	return s.User(ctx, u.ID)
}

// ChangePassword lets a user replace their own password. Their other sessions
// are signed out; keepSession (the caller's) stays.
func (s *Store) ChangePassword(ctx context.Context, userID int64, current, next string, keepSession int64) error {
	var username, hash string
	if err := s.r.QueryRowContext(ctx, `SELECT username, password_hash FROM users WHERE id = ?`, userID).Scan(&username, &hash); err != nil {
		return err
	}
	if ok, _, err := checkPassword(hash, current); err != nil || !ok {
		return &ValidationError{"current_password", "is not correct"}
	}
	return s.setPassword(ctx, userID, username, next, keepSession)
}

// SetPassword is an administrator's reset. All of the user's sessions end.
func (s *Store) SetPassword(ctx context.Context, userID int64, next string) error {
	u, err := s.User(ctx, userID)
	if err != nil {
		return err
	}
	return s.setPassword(ctx, userID, u.Username, next, 0)
}

func (s *Store) setPassword(ctx context.Context, userID int64, username, next string, keepSession int64) error {
	var v validator
	v.password("password", next, username)
	if v.err != nil {
		return v.err
	}
	hash := HashPassword(next)
	return s.write(ctx, func(tx *sql.Tx) error {
		now := Now()
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ?, password_changed_at = ?, updated_at = ? WHERE id = ?`, hash, now, now, userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE user_sessions SET revoked_at = ? WHERE user_id = ? AND id <> ? AND revoked_at IS NULL`, now, userID, keepSession)
		return err
	})
}

// SetDisabled disables or re-enables an account. Disabling ends its sessions
// at once. An admin cannot disable themselves, and the last active admin
// cannot be disabled, so the install can never be locked out from the web.
func (s *Store) SetDisabled(ctx context.Context, actingUserID, userID int64, disabled bool) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var role string
		if err := tx.QueryRowContext(ctx, `SELECT role FROM users WHERE id = ?`, userID).Scan(&role); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("user %d %w", userID, ErrNotFound)
		} else if err != nil {
			return err
		}
		now := Now()
		if !disabled {
			_, err := tx.ExecContext(ctx, `UPDATE users SET disabled_at = NULL, updated_at = ? WHERE id = ?`, now, userID)
			return err
		}
		if userID == actingUserID {
			return &ValidationError{"user", "cannot disable your own account"}
		}
		if role == "admin" {
			var admins int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role = 'admin' AND disabled_at IS NULL AND id <> ?`, userID).Scan(&admins); err != nil {
				return err
			}
			if admins == 0 {
				return &ValidationError{"user", "is the last active administrator"}
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET disabled_at = coalesce(disabled_at, ?), updated_at = ? WHERE id = ?`, now, now, userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE user_sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`, now, userID)
		return err
	})
}

// SetDisplayName changes the name shown for a user, which is their actor's.
func (s *Store) SetDisplayName(ctx context.Context, userID int64, name string) error {
	var v validator
	v.text("display_name", &name, 200, true)
	if v.err != nil {
		return v.err
	}
	_, err := s.w.ExecContext(ctx, `UPDATE actors SET display_name = ? WHERE id = (SELECT actor_id FROM users WHERE id = ?)`, name, userID)
	return err
}

// --- sessions ---

const (
	SessionIdle = 30 * 24 * time.Hour // expires after this long unused
	SessionMax  = 90 * 24 * time.Hour // and never lives longer than this
)

// Session is a signed-in browser. Its token exists only in the cookie.
type Session struct {
	ID         int64
	UserID     int64
	CreatedAt  Time
	ExpiresAt  Time
	LastSeenAt Time
}

// CreateSession starts a session and returns its secret token.
func (s *Store) CreateSession(ctx context.Context, userID int64) (string, Session, error) {
	buf := make([]byte, 32)
	rand.Read(buf)
	secret := base64.RawURLEncoding.EncodeToString(buf)
	now := Now()
	sess := Session{UserID: userID, CreatedAt: now, LastSeenAt: now, ExpiresAt: Time{now.Add(SessionIdle)}}
	err := s.write(ctx, func(tx *sql.Tx) error {
		// Housekeeping: drop sessions that ended over a week ago.
		cutoff := Time{now.Add(-7 * 24 * time.Hour)}
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_sessions WHERE expires_at < ? OR revoked_at < ?`, cutoff, cutoff); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO user_sessions (user_id, token_hash, created_at, expires_at, last_seen_at) VALUES (?, ?, ?, ?, ?)`,
			userID, hashToken(secret), sess.CreatedAt, sess.ExpiresAt, sess.LastSeenAt)
		if err != nil {
			return err
		}
		sess.ID, err = res.LastInsertId()
		return err
	})
	return secret, sess, err
}

// SessionUser resolves a session token to its user. Expired, revoked and
// disabled-account sessions are ErrInvalidToken. Use extends the expiry,
// written at most once an hour.
func (s *Store) SessionUser(ctx context.Context, secret string) (User, Session, error) {
	if secret == "" {
		return User{}, Session{}, ErrInvalidToken
	}
	now := Now()
	var sess Session
	err := s.r.QueryRowContext(ctx, `SELECT s.id, s.user_id, s.created_at, s.expires_at, s.last_seen_at
		FROM user_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.revoked_at IS NULL AND s.expires_at > ? AND u.disabled_at IS NULL`, hashToken(secret), now).
		Scan(&sess.ID, &sess.UserID, &sess.CreatedAt, &sess.ExpiresAt, &sess.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, Session{}, ErrInvalidToken
	}
	if err != nil {
		return User{}, Session{}, err
	}
	if now.Sub(sess.LastSeenAt.Time) > time.Hour {
		sess.LastSeenAt = now
		sess.ExpiresAt = Time{now.Add(SessionIdle)}
		if limit := sess.CreatedAt.Add(SessionMax); sess.ExpiresAt.After(limit) {
			sess.ExpiresAt = Time{limit}
		}
		if _, err := s.w.ExecContext(ctx, `UPDATE user_sessions SET last_seen_at = ?, expires_at = ? WHERE id = ?`, sess.LastSeenAt, sess.ExpiresAt, sess.ID); err != nil {
			return User{}, Session{}, err
		}
	}
	u, err := s.User(ctx, sess.UserID)
	return u, sess, err
}

// RevokeSession ends one session (sign-out).
func (s *Store) RevokeSession(ctx context.Context, secret string) error {
	_, err := s.w.ExecContext(ctx, `UPDATE user_sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL`, Now(), hashToken(secret))
	return err
}

// RevokeOtherSessions signs a user out everywhere except keepSession, and
// returns how many sessions ended.
func (s *Store) RevokeOtherSessions(ctx context.Context, userID, keepSession int64) (int64, error) {
	res, err := s.w.ExecContext(ctx, `UPDATE user_sessions SET revoked_at = ? WHERE user_id = ? AND id <> ? AND revoked_at IS NULL`, Now(), userID, keepSession)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ActiveSessions counts a user's live sessions.
func (s *Store) ActiveSessions(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.r.QueryRowContext(ctx, `SELECT count(*) FROM user_sessions WHERE user_id = ? AND revoked_at IS NULL AND expires_at > ?`, userID, Now()).Scan(&n)
	return n, err
}
