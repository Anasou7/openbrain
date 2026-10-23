package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ActorKinds are the allowed values of Actor.Kind.
var ActorKinds = []string{"human", "agent", "automation"}

// Actor is anyone or anything that writes to the brain: a person, an AI
// agent, or an automation. Agents are data, not schema.
type Actor struct {
	ID          int64           `json:"id"`
	Slug        string          `json:"slug"`
	DisplayName string          `json:"display_name"`
	Kind        string          `json:"kind"`
	Metadata    json.RawMessage `json:"metadata"`
	CreatedAt   Time            `json:"created_at"`
}

// ActorRef identifies the actor behind a record.
type ActorRef struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind"`
}

// Attribution says who is making a write and through which channel. Every
// write input embeds it.
type Attribution struct {
	Actor string `json:"actor"` // slug of a registered actor
	Via   string `json:"-"`     // set by the server, never the client: "api", "dashboard", ...
}

type ActorInput struct {
	Slug        string          `json:"slug"`
	DisplayName string          `json:"display_name"` // default: the slug
	Kind        string          `json:"kind"`         // default: "agent"
	Metadata    json.RawMessage `json:"metadata"`
}

func (s *Store) CreateActor(ctx context.Context, in ActorInput) (Actor, error) {
	if in.DisplayName == "" {
		in.DisplayName = in.Slug
	}
	if in.Kind == "" {
		in.Kind = "agent"
	}
	var v validator
	v.slug("slug", in.Slug)
	v.text("display_name", &in.DisplayName, maxShort, true)
	v.oneOf("kind", in.Kind, ActorKinds)
	meta := v.metadata("metadata", in.Metadata)
	if v.err != nil {
		return Actor{}, v.err
	}

	a := Actor{Slug: in.Slug, DisplayName: in.DisplayName, Kind: in.Kind, Metadata: json.RawMessage(meta), CreatedAt: Now()}
	res, err := s.w.ExecContext(ctx, `INSERT INTO actors (slug, display_name, kind, metadata, created_at) VALUES (?, ?, ?, ?, ?)`,
		a.Slug, a.DisplayName, a.Kind, meta, a.CreatedAt)
	if isUnique(err) {
		return Actor{}, fmt.Errorf("actor %q %w", a.Slug, ErrConflict)
	}
	if err != nil {
		return Actor{}, err
	}
	a.ID, err = res.LastInsertId()
	return a, err
}

// Actors lists all actors, humans first.
func (s *Store) Actors(ctx context.Context) ([]Actor, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id, slug, display_name, kind, metadata, created_at FROM actors ORDER BY kind <> 'human', slug`)
	return collect(rows, err, func(sc scanner) (a Actor, err error) {
		var meta string
		err = sc.Scan(&a.ID, &a.Slug, &a.DisplayName, &a.Kind, &meta, &a.CreatedAt)
		a.Metadata = json.RawMessage(meta)
		return a, err
	})
}

// Actor looks an actor up by slug.
func (s *Store) Actor(ctx context.Context, slug string) (Actor, error) {
	var a Actor
	var meta string
	err := s.r.QueryRowContext(ctx, `SELECT id, slug, display_name, kind, metadata, created_at FROM actors WHERE slug = ?`, slug).
		Scan(&a.ID, &a.Slug, &a.DisplayName, &a.Kind, &meta, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("actor %q %w", slug, ErrNotFound)
	}
	a.Metadata = json.RawMessage(meta)
	return a, err
}

// authorID resolves the actor making a write.
func authorID(ctx context.Context, tx *sql.Tx, slug string) (int64, error) {
	if slug == "" {
		return 0, &ValidationError{"actor", "is required"}
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM actors WHERE slug = ?`, slug).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, &ValidationError{"actor", fmt.Sprintf("%q is not a registered actor", slug)}
	}
	return id, err
}
