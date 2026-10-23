package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type Blocker struct {
	ID          int64     `json:"id"`
	ProjectID   int64     `json:"project_id"`
	Actor       ActorRef  `json:"actor"` // who raised it
	Description string    `json:"description"`
	Resolved    bool      `json:"resolved"`
	Resolution  string    `json:"resolution"`
	ResolvedBy  *ActorRef `json:"resolved_by,omitempty"`
	ResolvedAt  Time      `json:"resolved_at,omitzero"`
	CreatedAt   Time      `json:"created_at"`
}

type BlockerInput struct {
	Attribution
	Description string `json:"description"`

	IdempotencyKey string `json:"idempotency_key,omitempty"` // see ProjectInput
}

// BlockerPatch resolves, reopens or edits a blocker. Only non-nil fields apply.
type BlockerPatch struct {
	Attribution
	Resolved    *bool   `json:"resolved"`
	Resolution  *string `json:"resolution"`
	Description *string `json:"description"`
}

const blockerSelect = `SELECT b.id, b.project_id, a.slug, a.display_name, a.kind, b.description, b.resolution,
	r.slug, r.display_name, r.kind, b.resolved_at, b.created_at
FROM blockers b JOIN actors a ON a.id = b.actor_id LEFT JOIN actors r ON r.id = b.resolved_by`

func scanBlocker(sc scanner) (b Blocker, err error) {
	var rSlug, rName, rKind sql.NullString
	err = sc.Scan(&b.ID, &b.ProjectID, &b.Actor.Slug, &b.Actor.DisplayName, &b.Actor.Kind, &b.Description, &b.Resolution,
		&rSlug, &rName, &rKind, &b.ResolvedAt, &b.CreatedAt)
	if rSlug.Valid {
		b.Resolved = true
		b.ResolvedBy = &ActorRef{rSlug.String, rName.String, rKind.String}
	}
	return b, err
}

func (s *Store) Blocker(ctx context.Context, id int64) (Blocker, error) {
	b, err := scanBlocker(s.r.QueryRowContext(ctx, blockerSelect+` WHERE b.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return b, fmt.Errorf("blocker %d %w", id, ErrNotFound)
	}
	return b, err
}

func (s *Store) AddBlocker(ctx context.Context, ref string, in BlockerInput) (Blocker, error) {
	var v validator
	v.text("description", &in.Description, maxText, true)
	v.idempotencyKey(&in.IdempotencyKey)
	if v.err != nil {
		return Blocker{}, v.err
	}
	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		actor, err := authorID(ctx, tx, in.Actor)
		if err != nil {
			return err
		}
		p, err := getProject(ctx, tx, ref)
		if err != nil {
			return err
		}
		op := opFor("raise_blocker", p.ID)
		if id, err = replayed(ctx, tx, actor, in.IdempotencyKey, op); err != nil || id != 0 {
			return err
		}
		now := Now()
		res, err := tx.ExecContext(ctx, `INSERT INTO blockers (project_id, actor_id, description, created_at) VALUES (?, ?, ?, ?)`,
			p.ID, actor, in.Description, now)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		if err := remember(ctx, tx, actor, in.IdempotencyKey, op, id, now); err != nil {
			return err
		}
		return logActivity(ctx, tx, event{project: p.ID, actor: actor, subject: id, via: in.Via, at: now,
			kind: "blocker.created", summary: "raised a blocker: " + Excerpt(in.Description, 120)})
	})
	if err != nil {
		return Blocker{}, err
	}
	return s.Blocker(ctx, id)
}

func (s *Store) UpdateBlocker(ctx context.Context, id int64, in BlockerPatch) (Blocker, error) {
	var v validator
	if in.Description != nil {
		v.text("description", in.Description, maxText, true)
	}
	if in.Resolution != nil {
		v.text("resolution", in.Resolution, maxText, false)
	}
	if v.err != nil {
		return Blocker{}, v.err
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		actor, err := authorID(ctx, tx, in.Actor)
		if err != nil {
			return err
		}
		var projectID int64
		var desc, resolution string
		var resolvedAt Time
		err = tx.QueryRowContext(ctx, `SELECT project_id, description, resolution, resolved_at FROM blockers WHERE id = ?`, id).
			Scan(&projectID, &desc, &resolution, &resolvedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("blocker %d %w", id, ErrNotFound)
		}
		if err != nil {
			return err
		}
		now := Now()
		ev := event{project: projectID, actor: actor, subject: id, via: in.Via, at: now}
		wasResolved := !resolvedAt.IsZero()

		if in.Description != nil && *in.Description != desc {
			desc = *in.Description
			ev.kind, ev.summary = "blocker.edited", "edited a blocker: "+Excerpt(desc, 120)
		}
		if in.Resolution != nil {
			resolution = *in.Resolution
		}
		var resolvedBy any
		switch {
		case in.Resolved != nil && *in.Resolved && !wasResolved:
			resolvedAt = now
			ev.kind, ev.summary = "blocker.resolved", "resolved a blocker: "+Excerpt(desc, 120)
		case in.Resolved != nil && !*in.Resolved && wasResolved:
			resolvedAt, resolution = Time{}, ""
			ev.kind, ev.summary = "blocker.reopened", "reopened a blocker: "+Excerpt(desc, 120)
		}
		if !resolvedAt.IsZero() {
			// Keep the original resolver unless this call is the one resolving it.
			if ev.kind == "blocker.resolved" {
				resolvedBy = actor
			} else if err := tx.QueryRowContext(ctx, `SELECT resolved_by FROM blockers WHERE id = ?`, id).Scan(&resolvedBy); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE blockers SET description = ?, resolution = ?, resolved_at = ?, resolved_by = ? WHERE id = ?`,
			desc, resolution, resolvedAt, resolvedBy, id); err != nil {
			return err
		}
		if ev.kind == "" {
			return nil // only the resolution note changed; not timeline-worthy
		}
		return logActivity(ctx, tx, ev)
	})
	if err != nil {
		return Blocker{}, err
	}
	return s.Blocker(ctx, id)
}

// Blockers lists a project's blockers, newest first. state is "open"
// (default), "resolved" or "all".
func (s *Store) Blockers(ctx context.Context, ref, state string, n int) ([]Blocker, error) {
	p, err := getProject(ctx, s.r, ref)
	if err != nil {
		return nil, err
	}
	return listBlockers(ctx, s.r, p.ID, state, n)
}

func listBlockers(ctx context.Context, q querier, projectID int64, state string, n int) ([]Blocker, error) {
	cond := ""
	switch state {
	case "", "open":
		cond = " AND b.resolved_at IS NULL"
	case "resolved":
		cond = " AND b.resolved_at IS NOT NULL"
	case "all":
	default:
		return nil, &ValidationError{"state", "must be one of: open, resolved, all"}
	}
	rows, err := q.QueryContext(ctx, blockerSelect+` WHERE b.project_id = ?`+cond+` ORDER BY b.id DESC LIMIT ?`, projectID, limit(n, 100))
	return collect(rows, err, scanBlocker)
}
