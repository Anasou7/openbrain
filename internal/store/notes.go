package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type Note struct {
	ID        int64    `json:"id"`
	ProjectID int64    `json:"project_id"`
	Actor     ActorRef `json:"actor"`
	Kind      string   `json:"kind"`
	Content   string   `json:"content"`
	Pinned    bool     `json:"pinned"`
	CreatedAt Time     `json:"created_at"`
	UpdatedAt Time     `json:"updated_at"`
}

type NoteInput struct {
	Attribution
	Kind    string `json:"kind"` // free-form category such as "idea" or "warning"; default "note"
	Content string `json:"content"`
	Pinned  bool   `json:"pinned"`

	IdempotencyKey string `json:"idempotency_key,omitempty"` // see ProjectInput
}

// NotePatch changes only the fields that are non-nil. Only the note's author
// may change its content or kind; anyone may pin or unpin it.
type NotePatch struct {
	Attribution
	Kind    *string `json:"kind"`
	Content *string `json:"content"`
	Pinned  *bool   `json:"pinned"`
}

type NoteFilter struct {
	Actor      string // only notes by this actor slug
	HumanOnly  bool   // only notes by human actors
	PinnedOnly bool
	Before     int64 // only notes older than this ID, to page backwards
	Limit      int
}

const noteSelect = `SELECT n.id, n.project_id, a.slug, a.display_name, a.kind, n.kind, n.content, n.pinned, n.created_at, n.updated_at
FROM notes n JOIN actors a ON a.id = n.actor_id`

func scanNote(sc scanner) (n Note, err error) {
	err = sc.Scan(&n.ID, &n.ProjectID, &n.Actor.Slug, &n.Actor.DisplayName, &n.Actor.Kind,
		&n.Kind, &n.Content, &n.Pinned, &n.CreatedAt, &n.UpdatedAt)
	return n, err
}

func (s *Store) AddNote(ctx context.Context, ref string, in NoteInput) (Note, error) {
	if in.Kind == "" {
		in.Kind = "note"
	}
	var v validator
	v.text("content", &in.Content, maxText, true)
	v.slug("kind", in.Kind)
	v.idempotencyKey(&in.IdempotencyKey)
	if v.err != nil {
		return Note{}, v.err
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
		op := opFor("add_note", p.ID)
		if id, err = replayed(ctx, tx, actor, in.IdempotencyKey, op); err != nil || id != 0 {
			return err
		}
		now := Now()
		res, err := tx.ExecContext(ctx, `INSERT INTO notes (project_id, actor_id, kind, content, pinned, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, p.ID, actor, in.Kind, in.Content, in.Pinned, now, now)
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
			kind: "note.added", summary: "added a note: " + Excerpt(in.Content, 120)})
	})
	if err != nil {
		return Note{}, err
	}
	return s.Note(ctx, id)
}

func (s *Store) Note(ctx context.Context, id int64) (Note, error) {
	n, err := scanNote(s.r.QueryRowContext(ctx, noteSelect+` WHERE n.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return n, fmt.Errorf("note %d %w", id, ErrNotFound)
	}
	return n, err
}

func (s *Store) UpdateNote(ctx context.Context, id int64, in NotePatch) (Note, error) {
	var v validator
	if in.Content != nil {
		v.text("content", in.Content, maxText, true)
	}
	if in.Kind != nil {
		v.slug("kind", *in.Kind)
	}
	if v.err != nil {
		return Note{}, v.err
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		actor, err := authorID(ctx, tx, in.Actor)
		if err != nil {
			return err
		}
		type fields struct {
			kind, content string
			pinned        bool
		}
		var cur fields
		var projectID, authorID int64
		err = tx.QueryRowContext(ctx, `SELECT project_id, actor_id, kind, content, pinned FROM notes WHERE id = ?`, id).
			Scan(&projectID, &authorID, &cur.kind, &cur.content, &cur.pinned)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("note %d %w", id, ErrNotFound)
		}
		if err != nil {
			return err
		}
		next := cur
		if in.Kind != nil {
			next.kind = *in.Kind
		}
		if in.Content != nil {
			next.content = *in.Content
		}
		if in.Pinned != nil {
			next.pinned = *in.Pinned
		}
		if next == cur {
			return nil
		}
		textChanged := next.kind != cur.kind || next.content != cur.content
		if textChanged && actor != authorID {
			// Keeps attribution honest: a note's words are always its author's.
			return &ValidationError{"actor", "may not edit another actor's note; add a new note instead"}
		}
		now := Now()
		if _, err := tx.ExecContext(ctx, `UPDATE notes SET kind = ?, content = ?, pinned = ?, updated_at = ? WHERE id = ?`,
			next.kind, next.content, next.pinned, now, id); err != nil {
			return err
		}
		ev := event{project: projectID, actor: actor, subject: id, via: in.Via, at: now,
			kind: "note.edited", summary: "edited a note: " + Excerpt(next.content, 120)}
		if !textChanged {
			ev.kind, ev.summary = "note.pinned", "pinned a note: "+Excerpt(next.content, 120)
			if !next.pinned {
				ev.kind, ev.summary = "note.unpinned", "unpinned a note: "+Excerpt(next.content, 120)
			}
		}
		return logActivity(ctx, tx, ev)
	})
	if err != nil {
		return Note{}, err
	}
	return s.Note(ctx, id)
}

// Notes lists a project's notes, newest first.
func (s *Store) Notes(ctx context.Context, ref string, f NoteFilter) ([]Note, error) {
	p, err := getProject(ctx, s.r, ref)
	if err != nil {
		return nil, err
	}
	return listNotes(ctx, s.r, p.ID, f)
}

func listNotes(ctx context.Context, q querier, projectID int64, f NoteFilter) ([]Note, error) {
	where, args := []string{"n.project_id = ?"}, []any{projectID}
	if f.Actor != "" {
		where, args = append(where, "a.slug = ?"), append(args, f.Actor)
	}
	if f.HumanOnly {
		where = append(where, "a.kind = 'human'")
	}
	if f.Before > 0 {
		where, args = append(where, "n.id < ?"), append(args, f.Before)
	}
	if f.PinnedOnly {
		where = append(where, "n.pinned = 1")
	}
	args = append(args, limit(f.Limit, 50))
	rows, err := q.QueryContext(ctx, noteSelect+" WHERE "+strings.Join(where, " AND ")+" ORDER BY n.id DESC LIMIT ?", args...)
	return collect(rows, err, scanNote)
}
