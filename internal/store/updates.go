package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
)

// UpdateCategories classify a project update. The title and summary carry
// the meaning; the category only helps scanning.
var UpdateCategories = []string{"new", "improved", "fixed", "changed", "important"}

const (
	maxUpdateTitle   = 200
	maxUpdateSummary = 4000
	maxUpdateDetails = 8000
	maxUpdateRef     = 200
)

// ProjectUpdate is a human-readable statement of what meaningfully changed in
// a project, written for the project owner rather than the next agent (that
// is a Handoff). Updates are never edited; a correction is a newer update.
type ProjectUpdate struct {
	ID            int64    `json:"id"`
	ProjectID     int64    `json:"project_id"`
	ProjectSlug   string   `json:"project_slug"`
	ProjectName   string   `json:"project_name"`
	Actor         ActorRef `json:"actor"`
	Title         string   `json:"title"`
	Summary       string   `json:"summary"`
	Details       string   `json:"details,omitempty"`
	Category      string   `json:"category"`
	TaskReference string   `json:"task_reference,omitempty"`

	// The handoff the update was published with, if any, and its git facts.
	HandoffID int64  `json:"handoff_id,omitempty"`
	Branch    string `json:"branch,omitempty"`
	CommitSHA string `json:"commit_sha,omitempty"`

	CreatedAt Time `json:"created_at"`
}

// UpdateContent is what an update says; UpdateInput adds who says it.
type UpdateContent struct {
	Title         string `json:"title"`
	Summary       string `json:"summary"`
	Details       string `json:"details"`
	Category      string `json:"category"`
	TaskReference string `json:"task_reference"`
}

type UpdateInput struct {
	Attribution
	UpdateContent
	IdempotencyKey string `json:"idempotency_key,omitempty"` // see ProjectInput
}

// update checks an update's fields; prefix names them when nested, e.g.
// "user_update." inside a handoff.
func (v *validator) update(prefix string, u *UpdateContent) {
	v.line(prefix+"title", &u.Title, maxUpdateTitle, true)
	v.text(prefix+"summary", &u.Summary, maxUpdateSummary, true)
	v.text(prefix+"details", &u.Details, maxUpdateDetails, false)
	u.Category = strings.ToLower(strings.TrimSpace(u.Category))
	v.oneOf(prefix+"category", u.Category, UpdateCategories)
	v.line(prefix+"task_reference", &u.TaskReference, maxUpdateRef, false)
}

const updateSelect = `SELECT u.id, u.project_id, p.slug, p.name, a.slug, a.display_name, a.kind,
	u.title, u.summary, u.details, u.category, u.task_reference,
	coalesce(u.handoff_id, 0), coalesce(h.branch, ''), coalesce(h.commit_sha, ''), u.created_at
FROM project_updates u
JOIN actors a ON a.id = u.actor_id
JOIN projects p ON p.id = u.project_id
LEFT JOIN handoffs h ON h.id = u.handoff_id`

func scanUpdate(sc scanner) (u ProjectUpdate, err error) {
	err = sc.Scan(&u.ID, &u.ProjectID, &u.ProjectSlug, &u.ProjectName, &u.Actor.Slug, &u.Actor.DisplayName, &u.Actor.Kind,
		&u.Title, &u.Summary, &u.Details, &u.Category, &u.TaskReference,
		&u.HandoffID, &u.Branch, &u.CommitSHA, &u.CreatedAt)
	return u, err
}

func queryUpdates(ctx context.Context, q querier, where string, args ...any) ([]ProjectUpdate, error) {
	rows, err := q.QueryContext(ctx, updateSelect+" "+where, args...)
	return collect(rows, err, scanUpdate)
}

// insertUpdate writes an update and its activity entry inside a write
// transaction. handoff is 0 for a standalone update.
func insertUpdate(ctx context.Context, tx *sql.Tx, projectID, actor, handoff int64, c UpdateContent, via string, now Time) (int64, error) {
	var h any
	if handoff != 0 {
		h = handoff
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO project_updates
		(project_id, actor_id, handoff_id, title, summary, details, category, task_reference, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		projectID, actor, h, c.Title, c.Summary, c.Details, c.Category, c.TaskReference, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, logActivity(ctx, tx, event{project: projectID, actor: actor, subject: id, via: via, at: now,
		kind: "update.published", summary: "published an update: " + Excerpt(c.Title, 120)})
}

// AddUpdate publishes a project update attributed to in.Actor.
func (s *Store) AddUpdate(ctx context.Context, ref string, in UpdateInput) (ProjectUpdate, error) {
	var v validator
	v.update("", &in.UpdateContent)
	v.idempotencyKey(&in.IdempotencyKey)
	if v.err != nil {
		return ProjectUpdate{}, v.err
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
		op := opFor("publish_project_update", p.ID)
		if id, err = replayed(ctx, tx, actor, in.IdempotencyKey, op); err != nil || id != 0 {
			return err
		}
		now := Now()
		if id, err = insertUpdate(ctx, tx, p.ID, actor, 0, in.UpdateContent, in.Via, now); err != nil {
			return err
		}
		return remember(ctx, tx, actor, in.IdempotencyKey, op, id, now)
	})
	if err != nil {
		return ProjectUpdate{}, err
	}
	return s.oneUpdate(ctx, fmt.Sprintf("update %d", id), `WHERE u.id = ?`, id)
}

func (s *Store) oneUpdate(ctx context.Context, what, where string, args ...any) (ProjectUpdate, error) {
	us, err := queryUpdates(ctx, s.r, where, args...)
	if err == nil && len(us) == 0 {
		err = fmt.Errorf("%s %w", what, ErrNotFound)
	}
	if err != nil {
		return ProjectUpdate{}, err
	}
	return us[0], nil
}

// UpdateFilter narrows a project's updates.
type UpdateFilter struct {
	Actor    string // only this actor's (slug)
	Category string // only this category
	Before   int64  // only updates older than this ID, to page backwards
	Limit    int
}

// Updates returns a project's updates, newest first.
func (s *Store) Updates(ctx context.Context, ref string, f UpdateFilter) ([]ProjectUpdate, error) {
	p, err := getProject(ctx, s.r, ref)
	if err != nil {
		return nil, err
	}
	if f.Before <= 0 {
		f.Before = math.MaxInt64
	}
	where, args := `WHERE u.project_id = ? AND u.id < ?`, []any{p.ID, f.Before}
	if f.Actor != "" {
		where, args = where+` AND a.slug = ?`, append(args, f.Actor)
	}
	if f.Category != "" {
		where, args = where+` AND u.category = ?`, append(args, strings.ToLower(f.Category))
	}
	return queryUpdates(ctx, s.r, where+` ORDER BY u.id DESC LIMIT ?`, append(args, limit(f.Limit, 50))...)
}

// LatestUpdate returns a project's newest update, or ErrNotFound if it has
// none.
func (s *Store) LatestUpdate(ctx context.Context, ref string) (ProjectUpdate, error) {
	p, err := getProject(ctx, s.r, ref)
	if err != nil {
		return ProjectUpdate{}, err
	}
	return s.oneUpdate(ctx, "update on project "+p.Slug, `WHERE u.project_id = ? ORDER BY u.id DESC LIMIT 1`, p.ID)
}

// RecentUpdates returns the newest updates across projects that are not
// archived, optionally only those written by agents and automations.
func (s *Store) RecentUpdates(ctx context.Context, n int, agentsOnly bool) ([]ProjectUpdate, error) {
	where := `WHERE p.status <> 'archived'`
	if agentsOnly {
		where += ` AND a.kind <> 'human'`
	}
	return queryUpdates(ctx, s.r, where+` ORDER BY u.id DESC LIMIT ?`, limit(n, 20))
}

func listUpdates(ctx context.Context, q querier, projectID int64, n int) ([]ProjectUpdate, error) {
	return queryUpdates(ctx, q, `WHERE u.project_id = ? ORDER BY u.id DESC LIMIT ?`, projectID, limit(n, 10))
}
