package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TokenPrefix starts every actor token, so tokens are recognisable in
// configuration files and by secret scanners. legacyTokenPrefix tokens were
// issued before the rename to OpenBrain and remain valid.
const (
	TokenPrefix       = "obt_"
	legacyTokenPrefix = "pbt_"
)

// ErrInvalidToken means a token is unknown or revoked. Callers should not tell
// the client which.
var ErrInvalidToken = errors.New("invalid or revoked token")

// Token is the metadata of an actor credential. The secret is never stored
// and never returned after creation.
type Token struct {
	ID         int64    `json:"id"`
	Actor      ActorRef `json:"actor"`
	Prefix     string   `json:"prefix"`
	Label      string   `json:"label"`
	CreatedAt  Time     `json:"created_at"`
	LastUsedAt Time     `json:"last_used_at,omitzero"`
	RevokedAt  Time     `json:"revoked_at,omitzero"`
}

// hashToken is all that is stored. A fast hash is appropriate here: tokens
// carry 256 random bits, so there is nothing to brute-force.
func hashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

const tokenSelect = `SELECT t.id, a.slug, a.display_name, a.kind, t.prefix, t.label, t.created_at, t.last_used_at, t.revoked_at
FROM actor_tokens t JOIN actors a ON a.id = t.actor_id`

func scanToken(sc scanner) (t Token, err error) {
	err = sc.Scan(&t.ID, &t.Actor.Slug, &t.Actor.DisplayName, &t.Actor.Kind, &t.Prefix, &t.Label, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt)
	return t, err
}

// CreateToken issues a credential for an actor and returns it with its
// secret. This is the only time the secret is available.
func (s *Store) CreateToken(ctx context.Context, actor, label string) (Token, string, error) {
	var v validator
	v.text("label", &label, 200, false)
	if v.err != nil {
		return Token{}, "", v.err
	}
	buf := make([]byte, 32)
	rand.Read(buf) // crypto/rand; never fails on supported platforms
	secret := TokenPrefix + base64.RawURLEncoding.EncodeToString(buf)

	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		actorID, err := authorID(ctx, tx, actor)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO actor_tokens (actor_id, token_hash, prefix, label, created_at) VALUES (?, ?, ?, ?, ?)`,
			actorID, hashToken(secret), secret[:len(TokenPrefix)+8], label, Now())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return Token{}, "", err
	}
	t, err := s.token(ctx, id)
	return t, secret, err
}

func (s *Store) token(ctx context.Context, id int64) (Token, error) {
	t, err := scanToken(s.r.QueryRowContext(ctx, tokenSelect+` WHERE t.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("token %d %w", id, ErrNotFound)
	}
	return t, err
}

// Tokens lists token metadata, grouped by actor.
func (s *Store) Tokens(ctx context.Context, includeRevoked bool) ([]Token, error) {
	rows, err := s.r.QueryContext(ctx, tokenSelect+` WHERE ? OR t.revoked_at IS NULL ORDER BY a.kind <> 'human', a.slug, t.id`, includeRevoked)
	return collect(rows, err, scanToken)
}

// RevokeToken disables a token immediately. Revoking twice is a no-op.
func (s *Store) RevokeToken(ctx context.Context, id int64) (Token, error) {
	if _, err := s.w.ExecContext(ctx, `UPDATE actor_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, Now(), id); err != nil {
		return Token{}, err
	}
	return s.token(ctx, id)
}

// Authenticate resolves a bearer secret to its token and actor.
func (s *Store) Authenticate(ctx context.Context, secret string) (Token, error) {
	if !strings.HasPrefix(secret, TokenPrefix) && !strings.HasPrefix(secret, legacyTokenPrefix) {
		return Token{}, ErrInvalidToken
	}
	t, err := scanToken(s.r.QueryRowContext(ctx, tokenSelect+` WHERE t.token_hash = ?`, hashToken(secret)))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !t.RevokedAt.IsZero()) {
		return Token{}, ErrInvalidToken
	}
	if err != nil {
		return Token{}, err
	}
	// Record use at most once a minute, so authenticated reads stay cheap.
	if time.Since(t.LastUsedAt.Time) > time.Minute {
		t.LastUsedAt = Now()
		if _, err := s.w.ExecContext(ctx, `UPDATE actor_tokens SET last_used_at = ? WHERE id = ?`, t.LastUsedAt, t.ID); err != nil {
			return Token{}, err
		}
	}
	return t, nil
}
