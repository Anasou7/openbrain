package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ProjectStatuses are the allowed values of Project.Status.
var ProjectStatuses = []string{"active", "paused", "done", "archived"}

// Project is a unit of work tracked by the brain. Its identity is the numeric
// ID; the slug is a stable, readable handle that may be renamed.
type Project struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Status        string `json:"status"`
	CurrentFocus  string `json:"current_focus"`
	RepoURL       string `json:"repo_url"`
	DefaultBranch string `json:"default_branch"`
	CreatedAt     Time   `json:"created_at"`
	UpdatedAt     Time   `json:"updated_at"`
	ArchivedAt    Time   `json:"archived_at,omitzero"`

	// Derived on read.
	LastActivityAt Time `json:"last_activity_at,omitzero"`
	OpenBlockers   int  `json:"open_blockers"`
}

type ProjectInput struct {
	Attribution
	Slug          string `json:"slug"` // default: derived from name
	Name          string `json:"name"`
	Description   string `json:"description"`
	Status        string `json:"status"` // default: "active"
	CurrentFocus  string `json:"current_focus"`
	RepoURL       string `json:"repo_url"`
	DefaultBranch string `json:"default_branch"`

	// IdempotencyKey, if set, makes a retried call return the project the
	// first call created instead of failing or creating another.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// ProjectPatch changes only the fields that are non-nil.
type ProjectPatch struct {
	Attribution
	Slug          *string `json:"slug"`
	Name          *string `json:"name"`
	Description   *string `json:"description"`
	Status        *string `json:"status"`
	CurrentFocus  *string `json:"current_focus"`
	RepoURL       *string `json:"repo_url"`
	DefaultBranch *string `json:"default_branch"`
}

const projectSelect = `SELECT p.id, p.slug, p.name, p.description, p.status, p.current_focus,
	p.repo_url, p.default_branch, p.created_at, p.updated_at, p.archived_at,
	(SELECT created_at FROM activity WHERE project_id = p.id ORDER BY id DESC LIMIT 1) AS last_activity_at,
	(SELECT count(*) FROM blockers WHERE project_id = p.id AND resolved_at IS NULL)
FROM projects p`

func scanProject(sc scanner) (p Project, err error) {
	err = sc.Scan(&p.ID, &p.Slug, &p.Name, &p.Description, &p.Status, &p.CurrentFocus,
		&p.RepoURL, &p.DefaultBranch, &p.CreatedAt, &p.UpdatedAt, &p.ArchivedAt,
		&p.LastActivityAt, &p.OpenBlockers)
	return p, err
}

// Projects lists projects, most recently active first.
func (s *Store) Projects(ctx context.Context, includeArchived bool) ([]Project, error) {
	return listProjects(ctx, s.r, includeArchived)
}

func listProjects(ctx context.Context, q querier, includeArchived bool) ([]Project, error) {
	rows, err := q.QueryContext(ctx, projectSelect+` WHERE ? OR p.status <> 'archived' ORDER BY last_activity_at DESC, p.name`, includeArchived)
	return collect(rows, err, scanProject)
}

// Project looks a project up by numeric ID or slug.
func (s *Store) Project(ctx context.Context, ref string) (Project, error) {
	return getProject(ctx, s.r, ref)
}

func getProject(ctx context.Context, q querier, ref string) (Project, error) {
	where, arg := ` WHERE p.slug = ?`, any(ref)
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		where, arg = ` WHERE p.id = ?`, id
	}
	p, err := scanProject(q.QueryRowContext(ctx, projectSelect+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return p, fmt.Errorf("project %q %w", ref, ErrNotFound)
	}
	return p, err
}

// CreateProject creates a project. A slug that is already taken is an
// ErrConflict.
func (s *Store) CreateProject(ctx context.Context, in ProjectInput) (Project, error) {
	p, _, err := s.createProject(ctx, in, false)
	return p, err
}

// RegisterProject is CreateProject for agents that may not know whether the
// project exists: if a project already tracks the same repository (compared
// with RepoKey), that project is returned with created=false. The check and
// the insert share one transaction, so concurrent registrations of one
// repository cannot both create.
func (s *Store) RegisterProject(ctx context.Context, in ProjectInput) (p Project, created bool, err error) {
	return s.createProject(ctx, in, true)
}

func (s *Store) createProject(ctx context.Context, in ProjectInput, reuseRepo bool) (Project, bool, error) {
	if in.Slug == "" {
		in.Slug = Slugify(in.Name)
	}
	if in.Status == "" {
		in.Status = "active"
	}
	var v validator
	v.text("name", &in.Name, maxShort, true)
	v.slug("slug", in.Slug)
	v.oneOf("status", in.Status, ProjectStatuses)
	v.text("description", &in.Description, maxText, false)
	v.text("current_focus", &in.CurrentFocus, maxText, false)
	v.repoURL("repo_url", &in.RepoURL)
	v.token("default_branch", &in.DefaultBranch)
	v.idempotencyKey(&in.IdempotencyKey)
	if v.err != nil {
		return Project{}, false, v.err
	}

	var id int64
	created := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		actor, err := authorID(ctx, tx, in.Actor)
		if err != nil {
			return err
		}
		if id, err = replayed(ctx, tx, actor, in.IdempotencyKey, "create_project"); err != nil || id != 0 {
			return err
		}
		if reuseRepo && in.RepoURL != "" {
			all, err := listProjects(ctx, tx, true)
			if err != nil {
				return err
			}
			if existing := matchRepo(all, RepoKey(in.RepoURL)); len(existing) > 0 {
				id = existing[0].ID
				return nil
			}
		}
		now := Now()
		var archived Time
		if in.Status == "archived" {
			archived = now
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO projects
			(slug, name, description, status, current_focus, repo_url, default_branch, created_at, updated_at, archived_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.Slug, in.Name, in.Description, in.Status, in.CurrentFocus, in.RepoURL, in.DefaultBranch, now, now, archived)
		if isUnique(err) {
			return fmt.Errorf("project %q %w", in.Slug, ErrConflict)
		}
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		created = true
		if err := remember(ctx, tx, actor, in.IdempotencyKey, "create_project", id, now); err != nil {
			return err
		}
		return logActivity(ctx, tx, event{project: id, actor: actor, subject: id, via: in.Via, at: now,
			kind: "project.created", summary: "created the project"})
	})
	if err != nil {
		return Project{}, false, err
	}
	p, err := s.Project(ctx, strconv.FormatInt(id, 10))
	return p, created, err
}

func (s *Store) UpdateProject(ctx context.Context, ref string, in ProjectPatch) (Project, error) {
	var v validator
	if in.Name != nil {
		v.text("name", in.Name, maxShort, true)
	}
	if in.Slug != nil {
		v.slug("slug", *in.Slug)
	}
	if in.Status != nil {
		v.oneOf("status", *in.Status, ProjectStatuses)
	}
	if in.Description != nil {
		v.text("description", in.Description, maxText, false)
	}
	if in.CurrentFocus != nil {
		v.text("current_focus", in.CurrentFocus, maxText, false)
	}
	if in.RepoURL != nil {
		v.repoURL("repo_url", in.RepoURL)
	}
	if in.DefaultBranch != nil {
		v.token("default_branch", in.DefaultBranch)
	}
	if v.err != nil {
		return Project{}, v.err
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
		id = p.ID
		old := p
		var changed []string
		apply := func(field string, dst, src *string) {
			if src != nil && *src != *dst {
				*dst = *src
				changed = append(changed, field)
			}
		}
		apply("name", &p.Name, in.Name)
		apply("slug", &p.Slug, in.Slug)
		apply("status", &p.Status, in.Status)
		apply("description", &p.Description, in.Description)
		apply("current_focus", &p.CurrentFocus, in.CurrentFocus)
		apply("repo_url", &p.RepoURL, in.RepoURL)
		apply("default_branch", &p.DefaultBranch, in.DefaultBranch)
		if len(changed) == 0 {
			return nil
		}

		now := Now()
		if p.Status != old.Status {
			p.ArchivedAt = Time{}
			if p.Status == "archived" {
				p.ArchivedAt = now
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE projects SET slug = ?, name = ?, description = ?, status = ?,
			current_focus = ?, repo_url = ?, default_branch = ?, updated_at = ?, archived_at = ? WHERE id = ?`,
			p.Slug, p.Name, p.Description, p.Status, p.CurrentFocus, p.RepoURL, p.DefaultBranch, now, p.ArchivedAt, p.ID)
		if isUnique(err) {
			return fmt.Errorf("project %q %w", p.Slug, ErrConflict)
		}
		if err != nil {
			return err
		}

		// Status and focus changes get timeline entries of their own; other
		// edits are summarised in one.
		ev := event{project: p.ID, actor: actor, subject: p.ID, via: in.Via, at: now}
		var other []string
		for _, f := range changed {
			switch f {
			case "status":
				ev.kind, ev.summary = "project.status_changed", fmt.Sprintf("changed status from %s to %s", old.Status, p.Status)
			case "current_focus":
				ev.kind, ev.summary = "project.focus_changed", "cleared the focus"
				if p.CurrentFocus != "" {
					ev.summary = "set focus: " + Excerpt(p.CurrentFocus, 120)
				}
			default:
				other = append(other, f)
				continue
			}
			if err := logActivity(ctx, tx, ev); err != nil {
				return err
			}
		}
		if len(other) == 0 {
			return nil
		}
		ev.kind, ev.summary = "project.updated", "updated "+strings.Join(other, ", ")
		return logActivity(ctx, tx, ev)
	})
	if err != nil {
		return Project{}, err
	}
	return s.Project(ctx, strconv.FormatInt(id, 10))
}
